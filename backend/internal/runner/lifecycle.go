package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

const maxLifecycleBootstrapReplay = 256

var (
	// ErrLifecycleNotFoundOrForbidden deliberately combines ownership failure
	// and absence so a browser-supplied cursor cannot be used as an ID oracle.
	ErrLifecycleNotFoundOrForbidden = errors.New("lifecycle session not found or forbidden")
	// ErrLifecycleCursorUnavailable tells the transport to bootstrap a fresh
	// authoritative snapshot instead of advancing over a missing event.
	ErrLifecycleCursorUnavailable = errors.New("lifecycle cursor unavailable")
)

// LifecycleCursor is the public, generation-scoped position in the durable
// lifecycle stream. Allocation and provider identities remain store-private.
type LifecycleCursor struct {
	SessionID     string `json:"session_id"`
	Generation    uint64 `json:"generation"`
	EventSequence uint64 `json:"event_sequence"`
}

// LifecycleSnapshot is the browser-safe durable projection at EventSequence.
// TerminalReason remains available after cleanup changes Status to destroyed.
type LifecycleSnapshot struct {
	SessionID          string                 `json:"session_id"`
	ProblemID          string                 `json:"problem_id"`
	Generation         uint64                 `json:"generation"`
	OperationID        string                 `json:"operation_id"`
	Status             string                 `json:"status"`
	TimeoutAt          time.Time              `json:"timeout_at"`
	CleanupPending     bool                   `json:"cleanup_pending"`
	TerminalReason     string                 `json:"terminal_reason,omitempty"`
	EventSequence      uint64                 `json:"event_sequence"`
	LatestVerifyResult *LifecycleVerifyResult `json:"latest_verify_result,omitempty"`
}

type LifecycleVerifyResult struct {
	Success bool   `json:"success"`
	Log     string `json:"log"`
}

func (s LifecycleSnapshot) Cursor() LifecycleCursor {
	return LifecycleCursor{
		SessionID:     s.SessionID,
		Generation:    s.Generation,
		EventSequence: s.EventSequence,
	}
}

// LifecycleBootstrap always carries an authoritative snapshot decision. A nil
// Snapshot means the authenticated user has no current or resumed session.
type LifecycleBootstrap struct {
	Snapshot *LifecycleSnapshot
	Events   []DurableEvent
	Resync   bool
}

// LifecycleDelta distinguishes an empty same-generation tail from an
// authoritative session switch. SnapshotChanged may be true with Snapshot nil.
type LifecycleDelta struct {
	Snapshot        *LifecycleSnapshot
	SnapshotChanged bool
	Events          []DurableEvent
}

type lifecycleSnapshotRecord struct {
	snapshot     LifecycleSnapshot
	desiredState string
}

// BootstrapLifecycle resolves a browser resume cursor under authenticated
// ownership and freezes snapshot, watermark, and bounded replay in one
// repeatable-read transaction.
func (s *PostgresStore) BootstrapLifecycle(ctx context.Context, userID string, resume *LifecycleCursor) (LifecycleBootstrap, error) {
	if !validLifecycleUUID(userID) {
		return LifecycleBootstrap{}, errors.New("lifecycle reader requires a valid user id")
	}
	if resume != nil && !validLifecycleCursorIdentity(*resume) {
		return LifecycleBootstrap{}, ErrLifecycleNotFoundOrForbidden
	}

	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return LifecycleBootstrap{}, fmt.Errorf("begin lifecycle bootstrap: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var resumedGeneration uint64
	if resume != nil {
		resumedGeneration, err = lifecycleOwnedGeneration(ctx, tx, userID, resume.SessionID)
		if err != nil {
			return LifecycleBootstrap{}, err
		}
	}

	currentSessionID, found, err := lifecycleActiveSessionID(ctx, tx, userID)
	if err != nil {
		return LifecycleBootstrap{}, err
	}
	if !found && resume == nil {
		if err := tx.Commit(ctx); err != nil {
			return LifecycleBootstrap{}, fmt.Errorf("commit empty lifecycle bootstrap: %w", err)
		}
		return LifecycleBootstrap{}, nil
	}

	targetSessionID := currentSessionID
	if !found {
		targetSessionID = resume.SessionID
	}
	record, err := loadLifecycleSnapshot(ctx, tx, userID, targetSessionID)
	if err != nil {
		return LifecycleBootstrap{}, err
	}
	result := LifecycleBootstrap{Snapshot: &record.snapshot}

	if resume != nil {
		exactCurrent := resume.SessionID == record.snapshot.SessionID &&
			resume.Generation == record.snapshot.Generation &&
			resumedGeneration == record.snapshot.Generation
		if !exactCurrent {
			result.Resync = true
		} else {
			result.Events, result.Resync, err = lifecycleReplayToWatermark(
				ctx, tx, userID, record.snapshot, resume.EventSequence,
			)
			if err != nil {
				return LifecycleBootstrap{}, err
			}
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return LifecycleBootstrap{}, fmt.Errorf("commit lifecycle bootstrap: %w", err)
	}
	return result, nil
}

// ReadLifecycleDelta rechecks ownership and the user's durable current logical
// session on every call. Same-generation reads return only contiguous events;
// a logical-session or generation change returns an authoritative snapshot.
func (s *PostgresStore) ReadLifecycleDelta(ctx context.Context, userID string, cursor LifecycleCursor, limit int) (LifecycleDelta, error) {
	if !validLifecycleUUID(userID) {
		return LifecycleDelta{}, errors.New("lifecycle reader requires a valid user id")
	}
	if !validLifecycleCursorIdentity(cursor) {
		return LifecycleDelta{}, ErrLifecycleNotFoundOrForbidden
	}
	if cursor.EventSequence == 0 || limit <= 0 || limit > maxLifecycleBootstrapReplay {
		return LifecycleDelta{}, ErrLifecycleCursorUnavailable
	}

	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return LifecycleDelta{}, fmt.Errorf("begin lifecycle delta: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	ownedGeneration, err := lifecycleOwnedGeneration(ctx, tx, userID, cursor.SessionID)
	if err != nil {
		return LifecycleDelta{}, err
	}

	activeSessionID, found, err := lifecycleActiveSessionID(ctx, tx, userID)
	if err != nil {
		return LifecycleDelta{}, err
	}
	if found && activeSessionID != cursor.SessionID {
		record, err := loadLifecycleSnapshot(ctx, tx, userID, activeSessionID)
		if err != nil {
			return LifecycleDelta{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return LifecycleDelta{}, fmt.Errorf("commit lifecycle session switch: %w", err)
		}
		return LifecycleDelta{Snapshot: &record.snapshot, SnapshotChanged: true}, nil
	}

	record, err := loadLifecycleSnapshot(ctx, tx, userID, cursor.SessionID)
	if err != nil {
		return LifecycleDelta{}, err
	}
	if ownedGeneration != record.snapshot.Generation || cursor.Generation != record.snapshot.Generation {
		if err := tx.Commit(ctx); err != nil {
			return LifecycleDelta{}, fmt.Errorf("commit lifecycle generation switch: %w", err)
		}
		return LifecycleDelta{Snapshot: &record.snapshot, SnapshotChanged: true}, nil
	}
	if cursor.EventSequence > record.snapshot.EventSequence {
		return LifecycleDelta{}, ErrLifecycleCursorUnavailable
	}
	cursorExists, err := lifecycleEventExists(
		ctx, tx, userID, record.snapshot.SessionID, record.snapshot.Generation, cursor.EventSequence,
	)
	if err != nil {
		return LifecycleDelta{}, err
	}
	if !cursorExists {
		return LifecycleDelta{}, ErrLifecycleCursorUnavailable
	}

	events, err := readOwnedLifecycleEvents(
		ctx, tx, userID, record.snapshot.SessionID, record.snapshot.Generation,
		cursor.EventSequence, record.snapshot.EventSequence, limit,
	)
	if err != nil {
		return LifecycleDelta{}, err
	}
	expectedSequence := cursor.EventSequence + 1
	for _, event := range events {
		if event.Sequence != expectedSequence {
			return LifecycleDelta{}, ErrLifecycleCursorUnavailable
		}
		expectedSequence++
	}
	if len(events) == 0 && cursor.EventSequence < record.snapshot.EventSequence {
		return LifecycleDelta{}, ErrLifecycleCursorUnavailable
	}
	// Desired-absent is only an intent. Keep the authoritative logical-session
	// snapshot visible while any generation still has provider work to clean
	// up, and publish the null tombstone only after physical absence is proven.
	if len(events) == 0 && record.desiredState == "absent" && !record.snapshot.CleanupPending {
		if err := tx.Commit(ctx); err != nil {
			return LifecycleDelta{}, fmt.Errorf("commit lifecycle no-session snapshot: %w", err)
		}
		return LifecycleDelta{SnapshotChanged: true}, nil
	}

	if err := tx.Commit(ctx); err != nil {
		return LifecycleDelta{}, fmt.Errorf("commit lifecycle delta: %w", err)
	}
	return LifecycleDelta{Events: events}, nil
}

func lifecycleOwnedGeneration(ctx context.Context, tx pgx.Tx, userID, sessionID string) (uint64, error) {
	var generation int64
	err := tx.QueryRow(ctx, `
		SELECT s.current_generation
		FROM sessions s
		WHERE s.id=$1 AND s.user_id=$2`, sessionID, userID).Scan(&generation)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrLifecycleNotFoundOrForbidden
	}
	if err != nil {
		return 0, fmt.Errorf("authorize lifecycle session: %w", err)
	}
	if generation <= 0 {
		return 0, fmt.Errorf("%w: invalid current generation", ErrLifecycleCursorUnavailable)
	}
	return uint64(generation), nil
}

func lifecycleActiveSessionID(ctx context.Context, tx pgx.Tx, userID string) (string, bool, error) {
	var sessionID string
	err := tx.QueryRow(ctx, `
		SELECT s.id
		FROM sessions s
		WHERE s.user_id=$1
		  AND EXISTS (
			SELECT 1 FROM runner_allocations outstanding
			WHERE outstanding.session_id=s.id
			  AND (outstanding.desired_state<>'absent' OR outstanding.observed_state<>'absent')
		  )
		ORDER BY s.queued_at DESC,s.created_at DESC,s.id DESC
		LIMIT 1`, userID).Scan(&sessionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("resolve active lifecycle session: %w", err)
	}
	return sessionID, true, nil
}

func loadLifecycleSnapshot(ctx context.Context, tx pgx.Tx, userID, sessionID string) (lifecycleSnapshotRecord, error) {
	var record lifecycleSnapshotRecord
	var generation, watermark int64
	var createOperationCount int64
	var verifyResult []byte
	err := tx.QueryRow(ctx, `
		SELECT
			s.id,s.problem_id,a.generation,o.id,s.state,s.desired_state,s.expires_at,
			EXISTS (
				SELECT 1 FROM runner_allocations pending
				WHERE pending.session_id=s.id
				  AND (s.desired_state='absent' OR pending.generation<a.generation)
				  AND (pending.desired_state<>'absent' OR pending.observed_state<>'absent')
			),
			COALESCE(terminal.reason,''),a.last_event_sequence,COUNT(*) OVER (),
			COALESCE(latest_verify.result_metadata,'{}'::jsonb)
		FROM sessions s
		JOIN runner_allocations a
		  ON a.session_id=s.id AND a.generation=s.current_generation
		JOIN runner_operations o
		  ON o.allocation_id=a.id AND o.kind='create'
		LEFT JOIN LATERAL (
			SELECT CASE
				WHEN e.event_type IN ('failed','timed_out','provider_lost') THEN e.event_type
				ELSE COALESCE(e.reason_code,'')
			END AS reason
			FROM session_events e
			WHERE e.allocation_id=a.id
			  AND e.event_type IN ('failed','timed_out','provider_lost','destroying')
			ORDER BY e.sequence ASC
			LIMIT 1
		) terminal ON TRUE
		LEFT JOIN LATERAL (
			SELECT verify.result_metadata
			FROM runner_operations verify
			WHERE verify.allocation_id=a.id AND verify.provider_id=a.provider_id
			  AND verify.kind='verify' AND verify.state='succeeded'
			ORDER BY verify.completed_at DESC,verify.created_at DESC,verify.id DESC
			LIMIT 1
		) latest_verify ON TRUE
		WHERE s.id=$1 AND s.user_id=$2
		ORDER BY o.created_at,o.id
		LIMIT 1`, sessionID, userID).Scan(
		&record.snapshot.SessionID,
		&record.snapshot.ProblemID,
		&generation,
		&record.snapshot.OperationID,
		&record.snapshot.Status,
		&record.desiredState,
		&record.snapshot.TimeoutAt,
		&record.snapshot.CleanupPending,
		&record.snapshot.TerminalReason,
		&watermark,
		&createOperationCount,
		&verifyResult,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return lifecycleSnapshotRecord{}, fmt.Errorf("%w: lifecycle snapshot is incomplete", ErrLifecycleCursorUnavailable)
	}
	if err != nil {
		return lifecycleSnapshotRecord{}, fmt.Errorf("load lifecycle snapshot: %w", err)
	}
	if generation <= 0 || watermark < 0 || createOperationCount != 1 {
		return lifecycleSnapshotRecord{}, fmt.Errorf("%w: contradictory lifecycle snapshot", ErrLifecycleCursorUnavailable)
	}
	record.snapshot.Generation = uint64(generation)
	record.snapshot.EventSequence = uint64(watermark)
	if len(verifyResult) > 0 && string(verifyResult) != "{}" {
		var result verifyResultMetadata
		if err := json.Unmarshal(verifyResult, &result); err != nil || result.Success == nil || result.Log == nil || result.InfrastructureError {
			return lifecycleSnapshotRecord{}, fmt.Errorf("%w: latest lifecycle verify result is invalid", ErrLifecycleCursorUnavailable)
		}
		record.snapshot.LatestVerifyResult = &LifecycleVerifyResult{
			Success: *result.Success,
			Log:     boundedDurableMessage(*result.Log),
		}
	}
	return record, nil
}

func lifecycleReplayToWatermark(ctx context.Context, tx pgx.Tx, userID string, snapshot LifecycleSnapshot, after uint64) ([]DurableEvent, bool, error) {
	watermark := snapshot.EventSequence
	if after == 0 || after > watermark || watermark-after > maxLifecycleBootstrapReplay {
		return nil, true, nil
	}

	cursorExists, err := lifecycleEventExists(ctx, tx, userID, snapshot.SessionID, snapshot.Generation, after)
	if err != nil {
		return nil, false, err
	}
	if !cursorExists {
		return nil, true, nil
	}

	events, err := readOwnedLifecycleEvents(
		ctx, tx, userID, snapshot.SessionID, snapshot.Generation,
		after, watermark, maxLifecycleBootstrapReplay,
	)
	if err != nil {
		return nil, false, err
	}
	expectedSequence := after + 1
	for _, event := range events {
		if event.Sequence != expectedSequence {
			return nil, true, nil
		}
		expectedSequence++
	}
	if uint64(len(events)) != watermark-after {
		return nil, true, nil
	}
	return events, false, nil
}

func lifecycleEventExists(ctx context.Context, tx pgx.Tx, userID, sessionID string, generation, sequence uint64) (bool, error) {
	var exists bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM session_events e
			JOIN runner_allocations a ON a.id=e.allocation_id
			JOIN sessions s ON s.id=a.session_id AND s.user_id=$1
			WHERE s.id=$2 AND a.generation=$3 AND e.sequence=$4
		)`, userID, sessionID, int64(generation), int64(sequence)).Scan(&exists); err != nil {
		return false, fmt.Errorf("validate lifecycle cursor event: %w", err)
	}
	return exists, nil
}

func readOwnedLifecycleEvents(ctx context.Context, tx pgx.Tx, userID, sessionID string, generation, after, through uint64, limit int) ([]DurableEvent, error) {
	rows, err := tx.Query(ctx, `
		SELECT e.sequence,e.event_type,COALESCE(e.reason_code,''),e.message,e.sanitized_payload,e.created_at
		FROM session_events e
		JOIN runner_allocations a ON a.id=e.allocation_id
		JOIN sessions s ON s.id=a.session_id AND s.user_id=$1
		WHERE s.id=$2 AND a.generation=$3 AND e.sequence>$4 AND e.sequence<=$5
		ORDER BY e.sequence ASC
		LIMIT $6`, userID, sessionID, int64(generation), int64(after), int64(through), limit)
	if err != nil {
		return nil, fmt.Errorf("read owned lifecycle events: %w", err)
	}
	defer rows.Close()

	events := make([]DurableEvent, 0)
	for rows.Next() {
		var event DurableEvent
		var sequence int64
		if err := rows.Scan(
			&sequence, &event.Type, &event.ReasonCode, &event.Message,
			&event.Payload, &event.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan owned lifecycle event: %w", err)
		}
		if sequence <= 0 {
			return nil, fmt.Errorf("%w: invalid durable event sequence", ErrLifecycleCursorUnavailable)
		}
		event.Sequence = uint64(sequence)
		event.Payload = cloneJSON(event.Payload)
		// AllocationID is intentionally never selected into the public reader.
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate owned lifecycle events: %w", err)
	}
	return events, nil
}

func validLifecycleCursorIdentity(cursor LifecycleCursor) bool {
	return validLifecycleUUID(cursor.SessionID) && cursor.Generation > 0
}

func validLifecycleUUID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for index := range value {
		switch index {
		case 8, 13, 18, 23:
			if value[index] != '-' {
				return false
			}
		default:
			character := value[index]
			if !((character >= '0' && character <= '9') ||
				(character >= 'a' && character <= 'f') ||
				(character >= 'A' && character <= 'F')) {
				return false
			}
		}
	}
	return true
}
