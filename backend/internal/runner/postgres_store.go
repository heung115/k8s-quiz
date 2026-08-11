package runner

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const initialGeneration uint64 = 1

const maxDurableJSONBytes = 16 * 1024

const catalogSharedTransactionSQL = `SELECT pg_advisory_xact_lock_shared(hashtext('k8s-quiz.problem-catalog'))`

type DurableSession struct {
	ID                string
	UserID            string
	Selection         CatalogSelection
	CurrentGeneration uint64
	State             string
	DesiredState      string
	QueuedAt          time.Time
	ExpiresAt         time.Time
}

type DurableAllocation struct {
	Ref               AllocationRef
	ProviderID        string
	ResourceProfile   string
	DesiredState      string
	ObservedState     string
	ExpiresAt         time.Time
	LastEventSequence uint64
}

type DurableOperation struct {
	ID             string
	AllocationID   string
	ProviderID     string
	Kind           string
	IdempotencyKey string
	RequestHash    [sha256.Size]byte
	State          string
}

type DurableEvent struct {
	AllocationID string
	Sequence     uint64
	Type         string
	ReasonCode   string
	Message      string
	Payload      json.RawMessage
	CreatedAt    time.Time
}

type SessionReservation struct {
	Session    DurableSession
	Allocation DurableAllocation
	Operation  DurableOperation
	Event      DurableEvent
	AttemptID  string
	// AttemptStatus is loaded for recovery validation. Recovery may adopt only
	// the current generation's single in-progress attempt.
	AttemptStatus string
}

type ReserveSessionParams struct {
	SessionID       string
	UserID          string
	Selection       CatalogSelection
	Provider        ProviderKind
	ProviderID      string
	ResourceProfile string
	ExpiresAt       time.Time
	IdempotencyKey  string
}

// PostgresStore is the desired-state authority for Runner lifecycle data.
// Provider calls must happen only after its reservation transaction commits.
type PostgresStore struct {
	db    *pgxpool.Pool
	fence ControllerFence
}

func NewPostgresStore(db *pgxpool.Pool, fence ControllerFence) (*PostgresStore, error) {
	if db == nil {
		return nil, errors.New("runner postgres store requires a database pool")
	}
	if fence.ProviderID == "" || len(fence.ProviderID) > 128 || fence.Epoch == 0 || fence.Epoch > MaxDurableValue {
		return nil, errors.New("runner postgres store requires a valid controller fence")
	}
	return &PostgresStore{db: db, fence: fence}, nil
}

// lockControllerEpochTx is deliberately the first lock in every durable
// mutation transaction. The row lock makes a new lease's epoch increment wait
// for old-epoch transactions to finish, so recovery cannot snapshot before an
// older controller's late commit. Transactions starting after the increment
// see the mismatch and fail closed.
func (s *PostgresStore) lockControllerEpochTx(ctx context.Context, tx pgx.Tx) error {
	var epoch int64
	if err := tx.QueryRow(ctx, `
		SELECT epoch FROM runner_controller_epochs
		WHERE provider_id=$1 FOR UPDATE`, s.fence.ProviderID).Scan(&epoch); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrControllerFenced
		}
		return fmt.Errorf("lock runner controller epoch: %w", err)
	}
	if epoch <= 0 || uint64(epoch) != s.fence.Epoch {
		return ErrControllerFenced
	}
	return nil
}

func (s *PostgresStore) requireProvider(providerID string) error {
	if providerID != s.fence.ProviderID {
		return fmt.Errorf("%w: store provider is %q, request provider is %q", ErrControllerFenced, s.fence.ProviderID, providerID)
	}
	return nil
}

// FindReservation resolves an operation replay without applying cooldown or
// capacity policy. Callers must still invoke ReserveSession with the current
// immutable request so its hash is checked before trusting the replay.
func (s *PostgresStore) FindReservation(ctx context.Context, providerID, idempotencyKey string) (SessionReservation, bool, error) {
	if providerID == "" || idempotencyKey == "" {
		return SessionReservation{}, false, errors.New("runner reservation lookup requires provider and idempotency key")
	}
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadOnly})
	if err != nil {
		return SessionReservation{}, false, fmt.Errorf("begin runner reservation lookup: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	reservation, found, err := loadReservationByOperation(ctx, tx, providerID, idempotencyKey)
	if err != nil {
		return SessionReservation{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return SessionReservation{}, false, fmt.Errorf("commit runner reservation lookup: %w", err)
	}
	return reservation, found, nil
}

// ListActiveReservations returns the authoritative current generation for
// every active logical session in this provider scope. It deliberately fails
// on contradictory rows instead of omitting them: startup recovery must not
// open admission with an active session it could not hydrate.
func (s *PostgresStore) ListActiveReservations(ctx context.Context, providerID string) ([]SessionReservation, error) {
	if err := s.requireProvider(providerID); err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, fmt.Errorf("begin active runner reservation snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx, `
		SELECT o.idempotency_key
		FROM sessions s
		JOIN runner_allocations a
		  ON a.session_id=s.id AND a.generation=s.current_generation
		JOIN runner_operations o
		  ON o.allocation_id=a.id AND o.provider_id=a.provider_id AND o.kind='create'
		WHERE a.provider_id=$1 AND s.desired_state='active'
		ORDER BY s.queued_at,s.id`, providerID)
	if err != nil {
		return nil, fmt.Errorf("list active runner reservation keys: %w", err)
	}
	var keys []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan active runner reservation key: %w", err)
		}
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("iterate active runner reservation keys: %w", err)
	}
	rows.Close()

	reservations := make([]SessionReservation, 0, len(keys))
	for _, key := range keys {
		reservation, found, err := loadReservationByOperation(ctx, tx, providerID, key)
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, fmt.Errorf("%w: active reservation %q disappeared from snapshot", ErrLifecycleConflict, key)
		}
		if err := validateActiveRecoveryReservation(reservation, providerID); err != nil {
			return nil, err
		}
		reservations = append(reservations, reservation)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit active runner reservation snapshot: %w", err)
	}
	return reservations, nil
}

// ReserveSession atomically creates generation 1 and its allocation, create
// operation, first event, and attempt. Replaying the exact provider/key/hash
// returns the same reservation; a conflicting replay fails closed.
func (s *PostgresStore) ReserveSession(ctx context.Context, params ReserveSessionParams) (SessionReservation, error) {
	if err := s.requireProvider(params.ProviderID); err != nil {
		return SessionReservation{}, err
	}
	if params.SessionID == "" {
		id, err := newUUID()
		if err != nil {
			return SessionReservation{}, err
		}
		params.SessionID = id
	}
	if params.IdempotencyKey == "" {
		params.IdempotencyKey = fmt.Sprintf("create:%s:%d", params.SessionID, initialGeneration)
	}
	if err := validateReservation(params); err != nil {
		return SessionReservation{}, err
	}
	// PostgreSQL TIMESTAMPTZ persists microseconds. Canonicalize before hashing
	// and returning the reservation so exact replays observe the same value as
	// the durable row instead of differing only in discarded nanoseconds.
	params.ExpiresAt = params.ExpiresAt.UTC().Truncate(time.Microsecond)

	sessionRef := SessionRef{SessionID: params.SessionID, Generation: initialGeneration}
	allocationID := AllocationIDForSession(sessionRef)
	requestHash, err := hashReservation(params, allocationID)
	if err != nil {
		return SessionReservation{}, fmt.Errorf("hash runner reservation: %w", err)
	}

	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return SessionReservation{}, fmt.Errorf("begin runner reservation: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.lockControllerEpochTx(ctx, tx); err != nil {
		return SessionReservation{}, err
	}

	// The user row is the cross-process transition lock. The partial unique
	// indexes remain the final database backstop for active-session races.
	var lockedUser string
	if err := tx.QueryRow(ctx, `SELECT id FROM users WHERE id=$1 FOR UPDATE`, params.UserID).Scan(&lockedUser); err != nil {
		return SessionReservation{}, fmt.Errorf("lock runner session user: %w", err)
	}

	if replay, found, err := loadReservationByOperation(ctx, tx, params.ProviderID, params.IdempotencyKey); err != nil {
		return SessionReservation{}, err
	} else if found {
		if replay.Allocation.Ref.ID != allocationID || replay.Operation.Kind != "create" ||
			!bytes.Equal(replay.Operation.RequestHash[:], requestHash[:]) {
			return SessionReservation{}, ErrIdempotencyConflict
		}
		if err := tx.Commit(ctx); err != nil {
			return SessionReservation{}, fmt.Errorf("commit runner reservation replay: %w", err)
		}
		return replay, nil
	}
	// Exact replay above intentionally does not consult the current head: a
	// retired problem must remain replayable. Only a new reservation participates
	// in the cross-process publication fence and must select the current head.
	if _, err := tx.Exec(ctx, catalogSharedTransactionSQL); err != nil {
		return SessionReservation{}, fmt.Errorf("lock problem catalog for reservation: %w", err)
	}
	if err := validateCatalogSelectionTx(ctx, tx, params.Selection); err != nil {
		return SessionReservation{}, err
	}

	var queuedAt time.Time
	if err := tx.QueryRow(ctx, `SELECT NOW()`).Scan(&queuedAt); err != nil {
		return SessionReservation{}, fmt.Errorf("read database clock: %w", err)
	}
	expiresAt := params.ExpiresAt.UTC()
	if !expiresAt.After(queuedAt) {
		return SessionReservation{}, errors.New("runner reservation expiry must be in the future")
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO sessions (
			id,user_id,problem_id,problem_revision,catalog_generation,current_generation,state,
			desired_state,queued_at,expires_at
		) VALUES ($1,$2,$3,$4,$5,$6,'queued','active',$7,$8)`,
		params.SessionID, params.UserID, params.Selection.Problem.ID, params.Selection.Problem.Revision,
		int64(params.Selection.Generation), int64(initialGeneration), queuedAt, expiresAt,
	)
	if err != nil {
		return SessionReservation{}, mapReservationError(err)
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO runner_allocations (
			id,session_id,generation,provider_kind,provider_id,resource_profile,desired_state,
			observed_state,expires_at,last_event_sequence
		) VALUES ($1,$2,$3,$4,$5,$6,'active','unknown',$7,1)`,
		allocationID, params.SessionID, int64(initialGeneration), params.Provider,
		params.ProviderID, params.ResourceProfile, expiresAt,
	)
	if err != nil {
		return SessionReservation{}, mapReservationError(err)
	}

	var operationID string
	if err := tx.QueryRow(ctx, `
		INSERT INTO runner_operations (
			allocation_id,provider_id,kind,idempotency_key,request_hash,state
		) VALUES ($1,$2,'create',$3,$4,'pending') RETURNING id`,
		allocationID, params.ProviderID, params.IdempotencyKey, requestHash[:],
	).Scan(&operationID); err != nil {
		return SessionReservation{}, mapReservationError(err)
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO session_events (
			allocation_id,sequence,event_type,reason_code,message,sanitized_payload,created_at
		) VALUES ($1,1,'allocation_reserved','create_requested','Environment reservation accepted.','{}'::jsonb,$2)`,
		allocationID, queuedAt,
	)
	if err != nil {
		return SessionReservation{}, fmt.Errorf("append allocation reservation event: %w", err)
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO attempts (
			id,user_id,problem_id,status,started_at,session_id,generation
		) VALUES ($1,$2,$3,'in_progress',$4,$1,$5)`,
		params.SessionID, params.UserID, params.Selection.Problem.ID, queuedAt, int64(initialGeneration),
	)
	if err != nil {
		return SessionReservation{}, mapReservationError(err)
	}

	if err := tx.Commit(ctx); err != nil {
		return SessionReservation{}, mapReservationError(err)
	}
	return reservationValue(params, allocationID, operationID, requestHash, queuedAt), nil
}

// AppendEvent serializes sequence allocation on the allocation row. A failed
// insert rolls the counter increment back, so committed sequences are gapless.
func (s *PostgresStore) AppendEvent(ctx context.Context, allocationID, eventType, reasonCode, message string, payload json.RawMessage) (DurableEvent, error) {
	if allocationID == "" || eventType == "" {
		return DurableEvent{}, errors.New("allocation id and event type are required")
	}
	if len(reasonCode) > 64 || len(message) > 1024 {
		return DurableEvent{}, errors.New("runner event text exceeds the persisted bound")
	}
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	if !json.Valid(payload) {
		return DurableEvent{}, errors.New("runner event payload is not valid JSON")
	}
	if len(payload) > maxDurableJSONBytes {
		return DurableEvent{}, errors.New("runner event payload exceeds the persisted bound")
	}

	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return DurableEvent{}, fmt.Errorf("begin runner event append: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.lockControllerEpochTx(ctx, tx); err != nil {
		return DurableEvent{}, err
	}
	var providerID string
	if err := tx.QueryRow(ctx, `SELECT provider_id FROM runner_allocations WHERE id=$1`, allocationID).Scan(&providerID); err != nil {
		return DurableEvent{}, fmt.Errorf("load runner event provider: %w", err)
	}
	if err := s.requireProvider(providerID); err != nil {
		return DurableEvent{}, err
	}

	var sequence int64
	if err := tx.QueryRow(ctx, `
		UPDATE runner_allocations
		SET last_event_sequence=last_event_sequence+1, updated_at=NOW()
		WHERE id=$1
		RETURNING last_event_sequence`, allocationID).Scan(&sequence); err != nil {
		return DurableEvent{}, fmt.Errorf("reserve runner event sequence: %w", err)
	}
	var createdAt time.Time
	if err := tx.QueryRow(ctx, `
		INSERT INTO session_events (
			allocation_id,sequence,event_type,reason_code,message,sanitized_payload
		) VALUES ($1,$2,$3,NULLIF($4,''),$5,$6::jsonb)
		RETURNING created_at`, allocationID, sequence, eventType, reasonCode, message, string(payload)).Scan(&createdAt); err != nil {
		return DurableEvent{}, fmt.Errorf("append runner event: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return DurableEvent{}, fmt.Errorf("commit runner event: %w", err)
	}
	return DurableEvent{
		AllocationID: allocationID, Sequence: uint64(sequence), Type: eventType,
		ReasonCode: reasonCode, Message: message, Payload: cloneJSON(payload), CreatedAt: createdAt,
	}, nil
}

func (s *PostgresStore) ReadEvents(ctx context.Context, allocationID string, after uint64, limit int) ([]DurableEvent, error) {
	if allocationID == "" || limit <= 0 || limit > 1000 {
		return nil, errors.New("invalid runner event replay request")
	}
	rows, err := s.db.Query(ctx, `
		SELECT allocation_id,sequence,event_type,COALESCE(reason_code,''),message,sanitized_payload,created_at
		FROM session_events
		WHERE allocation_id=$1 AND sequence>$2
		ORDER BY sequence ASC LIMIT $3`, allocationID, int64(after), limit)
	if err != nil {
		return nil, fmt.Errorf("read runner events: %w", err)
	}
	defer rows.Close()

	events := make([]DurableEvent, 0)
	for rows.Next() {
		var event DurableEvent
		var sequence int64
		if err := rows.Scan(
			&event.AllocationID, &sequence, &event.Type, &event.ReasonCode,
			&event.Message, &event.Payload, &event.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan runner event: %w", err)
		}
		event.Sequence = uint64(sequence)
		event.Payload = cloneJSON(event.Payload)
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate runner events: %w", err)
	}
	return events, nil
}

func validateReservation(params ReserveSessionParams) error {
	switch {
	case params.UserID == "":
		return errors.New("runner reservation user id is required")
	case params.Selection.Generation == 0 || params.Selection.Generation > MaxDurableValue:
		return errors.New("runner reservation catalog generation is required")
	case params.Selection.Problem.ID == "" || params.Selection.Problem.Revision == "":
		return errors.New("runner reservation approved problem is required")
	case params.Provider == "":
		return errors.New("runner reservation provider kind is required")
	case params.ProviderID == "" || len(params.ProviderID) > 128:
		return errors.New("runner reservation provider id is invalid")
	case params.ResourceProfile == "" || len(params.ResourceProfile) > 64:
		return errors.New("runner reservation resource profile is invalid")
	case params.ExpiresAt.IsZero():
		return errors.New("runner reservation expiry is required")
	case ValidateProviderOperationKey(params.IdempotencyKey) != nil:
		return errors.New("runner reservation idempotency key is invalid")
	default:
		return nil
	}
}

func hashReservation(params ReserveSessionParams, allocationID string) ([sha256.Size]byte, error) {
	payload, err := json.Marshal(struct {
		Schema          string           `json:"schema"`
		AllocationID    string           `json:"allocation_id"`
		SessionID       string           `json:"session_id"`
		Generation      uint64           `json:"generation"`
		UserID          string           `json:"user_id"`
		Selection       CatalogSelection `json:"selection"`
		Provider        ProviderKind     `json:"provider"`
		ProviderID      string           `json:"provider_id"`
		ResourceProfile string           `json:"resource_profile"`
		ExpiresAtUnixNS int64            `json:"expires_at_unix_ns"`
	}{
		Schema: "k8s-quiz.runner-create/v2", AllocationID: allocationID,
		SessionID: params.SessionID, Generation: initialGeneration, UserID: params.UserID,
		Selection: params.Selection, Provider: params.Provider, ProviderID: params.ProviderID,
		ResourceProfile: params.ResourceProfile,
		ExpiresAtUnixNS: params.ExpiresAt.UTC().UnixNano(),
	})
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	return sha256.Sum256(payload), nil
}

func loadReservationByOperation(ctx context.Context, tx pgx.Tx, providerID, key string) (SessionReservation, bool, error) {
	var reservation SessionReservation
	var catalogGeneration *int64
	var currentGeneration, allocationGeneration, allocationLastSequence, eventSequence int64
	var requestHash []byte
	var payload []byte
	err := tx.QueryRow(ctx, `
		SELECT
			s.id,s.user_id,s.problem_id,s.problem_revision,s.catalog_generation,s.current_generation,s.state,s.desired_state,s.queued_at,s.expires_at,
			a.id,a.generation,a.provider_kind,a.provider_id,a.resource_profile,a.desired_state,a.observed_state,a.expires_at,a.last_event_sequence,
			o.id,o.allocation_id,o.provider_id,o.kind,o.idempotency_key,o.request_hash,o.state,
			e.allocation_id,e.sequence,e.event_type,COALESCE(e.reason_code,''),e.message,e.sanitized_payload,e.created_at,
			at.id,at.status
		FROM runner_operations o
		JOIN runner_allocations a ON a.id=o.allocation_id AND a.provider_id=o.provider_id
		JOIN sessions s ON s.id=a.session_id
		JOIN session_events e ON e.allocation_id=a.id AND e.sequence=1
		JOIN attempts at ON at.session_id=a.session_id AND at.generation=a.generation
		WHERE o.provider_id=$1 AND o.idempotency_key=$2`, providerID, key).Scan(
		&reservation.Session.ID, &reservation.Session.UserID,
		&reservation.Session.Selection.Problem.ID, &reservation.Session.Selection.Problem.Revision,
		&catalogGeneration, &currentGeneration, &reservation.Session.State, &reservation.Session.DesiredState,
		&reservation.Session.QueuedAt, &reservation.Session.ExpiresAt,
		&reservation.Allocation.Ref.ID, &allocationGeneration, &reservation.Allocation.Ref.Provider,
		&reservation.Allocation.ProviderID,
		&reservation.Allocation.ResourceProfile, &reservation.Allocation.DesiredState,
		&reservation.Allocation.ObservedState, &reservation.Allocation.ExpiresAt, &allocationLastSequence,
		&reservation.Operation.ID, &reservation.Operation.AllocationID, &reservation.Operation.ProviderID,
		&reservation.Operation.Kind, &reservation.Operation.IdempotencyKey, &requestHash, &reservation.Operation.State,
		&reservation.Event.AllocationID, &eventSequence, &reservation.Event.Type,
		&reservation.Event.ReasonCode, &reservation.Event.Message, &payload, &reservation.Event.CreatedAt,
		&reservation.AttemptID, &reservation.AttemptStatus,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return SessionReservation{}, false, nil
	}
	if err != nil {
		return SessionReservation{}, false, fmt.Errorf("load runner reservation replay: %w", err)
	}
	if len(requestHash) != sha256.Size {
		return SessionReservation{}, false, errors.New("stored runner request hash has invalid length")
	}
	if catalogGeneration == nil || *catalogGeneration <= 0 {
		return SessionReservation{}, false, fmt.Errorf("%w: stored runner reservation has no verified catalog selection", ErrLifecycleConflict)
	}
	if currentGeneration <= 0 || allocationGeneration <= 0 || allocationLastSequence <= 0 || eventSequence != 1 {
		return SessionReservation{}, false, errors.New("stored runner reservation has invalid generation or event sequence")
	}
	reservation.Session.Selection.Generation = uint64(*catalogGeneration)
	reservation.Session.CurrentGeneration = uint64(currentGeneration)
	reservation.Allocation.Ref.Session = SessionRef{SessionID: reservation.Session.ID, Generation: uint64(allocationGeneration)}
	reservation.Allocation.LastEventSequence = uint64(allocationLastSequence)
	reservation.Event.Sequence = uint64(eventSequence)
	reservation.Event.Payload = cloneJSON(payload)
	copy(reservation.Operation.RequestHash[:], requestHash)
	return reservation, true, nil
}

func reservationValue(params ReserveSessionParams, allocationID, operationID string, requestHash [sha256.Size]byte, queuedAt time.Time) SessionReservation {
	sessionRef := SessionRef{SessionID: params.SessionID, Generation: initialGeneration}
	return SessionReservation{
		Session: DurableSession{
			ID: params.SessionID, UserID: params.UserID, Selection: params.Selection,
			CurrentGeneration: initialGeneration, State: "queued", DesiredState: "active",
			QueuedAt: queuedAt, ExpiresAt: params.ExpiresAt.UTC(),
		},
		Allocation: DurableAllocation{
			Ref:        AllocationRef{ID: allocationID, Session: sessionRef, Provider: params.Provider},
			ProviderID: params.ProviderID, ResourceProfile: params.ResourceProfile,
			DesiredState: "active", ObservedState: "unknown", ExpiresAt: params.ExpiresAt.UTC(),
			LastEventSequence: 1,
		},
		Operation: DurableOperation{
			ID: operationID, AllocationID: allocationID, ProviderID: params.ProviderID,
			Kind: "create", IdempotencyKey: params.IdempotencyKey, RequestHash: requestHash, State: "pending",
		},
		Event: DurableEvent{
			AllocationID: allocationID, Sequence: 1, Type: "allocation_reserved",
			ReasonCode: "create_requested", Message: "Environment reservation accepted.",
			Payload: json.RawMessage(`{}`), CreatedAt: queuedAt,
		},
		AttemptID:     params.SessionID,
		AttemptStatus: "in_progress",
	}
}

func validateActiveRecoveryReservation(reservation SessionReservation, providerID string) error {
	ref := reservation.Allocation.Ref
	if reservation.Session.ID == "" || reservation.Session.UserID == "" ||
		reservation.Session.Selection.Generation == 0 || reservation.Session.Selection.Problem.ID == "" ||
		reservation.Session.Selection.Problem.Revision == "" ||
		reservation.Session.CurrentGeneration == 0 || ref.Session.SessionID != reservation.Session.ID ||
		ref.Session.Generation != reservation.Session.CurrentGeneration ||
		reservation.Allocation.ProviderID != providerID || reservation.Operation.ProviderID != providerID ||
		reservation.Operation.AllocationID != ref.ID || reservation.Operation.Kind != "create" ||
		reservation.Session.DesiredState != "active" || reservation.Allocation.DesiredState != "active" ||
		reservation.AttemptID == "" || reservation.AttemptStatus != "in_progress" {
		return fmt.Errorf("%w: active recovery reservation has contradictory identity or desired state", ErrLifecycleConflict)
	}
	switch reservation.Session.State {
	case "queued", "provisioning", "booting", "setting_up", "ready", "verifying":
	default:
		return fmt.Errorf("%w: active recovery session is %s", ErrLifecycleConflict, reservation.Session.State)
	}
	switch reservation.Operation.State {
	case "pending", "running", "succeeded":
	default:
		return fmt.Errorf("%w: active recovery create operation is %s", ErrLifecycleConflict, reservation.Operation.State)
	}
	return nil
}

func validateCatalogSelectionTx(ctx context.Context, tx pgx.Tx, selection CatalogSelection) error {
	var exists bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM problem_catalog_head h
			JOIN problem_catalog_entries e
			  ON e.catalog_generation=h.catalog_generation
			WHERE h.singleton=TRUE
			  AND h.catalog_generation=$1
			  AND e.problem_id=$2
			  AND e.problem_revision=$3
		)`, int64(selection.Generation), selection.Problem.ID, selection.Problem.Revision).Scan(&exists); err != nil {
		return fmt.Errorf("validate runner catalog selection: %w", err)
	}
	if !exists {
		return ErrCatalogSelectionStale
	}
	return nil
}

func mapReservationError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.ConstraintName {
		case "sessions_one_active_per_user", "attempts_one_active_durable_per_user":
			return ErrActiveSession
		case "runner_operations_provider_id_idempotency_key_key":
			return ErrIdempotencyConflict
		}
	}
	return fmt.Errorf("persist runner reservation: %w", err)
}

func newUUID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate runner UUID: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(b)
	return fmt.Sprintf("%s-%s-%s-%s-%s", encoded[:8], encoded[8:12], encoded[12:16], encoded[16:20], encoded[20:]), nil
}

func cloneJSON(value []byte) json.RawMessage {
	return append(json.RawMessage(nil), value...)
}
