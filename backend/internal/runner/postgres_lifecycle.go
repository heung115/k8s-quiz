package runner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

// MarkCreateSucceeded records the accepted provider allocation before setup is
// launched. The exact reserved allocation identity must be returned unchanged.
func (s *PostgresStore) MarkCreateSucceeded(ctx context.Context, ref AllocationRef) error {
	return s.markCreateSucceeded(ctx, ref, nil)
}

// MarkClaimedCreateSucceeded completes only the currently leased replacement
// create. It prevents a late worker from committing after its lease expired.
func (s *PostgresStore) MarkClaimedCreateSucceeded(ctx context.Context, claim CreateWorkClaim) error {
	if err := validateCreateClaim(claim); err != nil {
		return err
	}
	return s.markCreateSucceeded(ctx, claim.Reservation.Allocation.Ref, &claim)
}

func (s *PostgresStore) markCreateSucceeded(ctx context.Context, ref AllocationRef, claim *CreateWorkClaim) error {
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("begin create success transition: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.lockControllerEpochTx(ctx, tx); err != nil {
		return err
	}

	providerID, err := lockExactAllocation(ctx, tx, ref)
	if err != nil {
		return err
	}
	if err := s.requireProvider(providerID); err != nil {
		return err
	}
	var operationID, operationState string
	var leaseToken *string
	var leaseActive bool
	if err := tx.QueryRow(ctx, `
		SELECT id,state,lease_token::text,COALESCE(lease_expires_at>clock_timestamp(),false) FROM runner_operations
		WHERE allocation_id=$1 AND provider_id=$2 AND kind='create' FOR UPDATE`,
		ref.ID, providerID).Scan(&operationID, &operationState, &leaseToken, &leaseActive); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: durable create operation is missing", ErrLifecycleConflict)
		}
		return fmt.Errorf("lock durable create operation: %w", err)
	}
	if claim != nil {
		if operationID != claim.OperationID || operationState != "running" || leaseToken == nil || *leaseToken != claim.LeaseToken || !leaseActive {
			return fmt.Errorf("%w: create work claim is no longer current", ErrLifecycleConflict)
		}
	} else {
		if operationState == "succeeded" {
			return commitLifecycleTx(ctx, tx, "create success replay")
		}
		if leaseActive {
			return fmt.Errorf("%w: create operation is leased by a worker", ErrLifecycleConflict)
		}
	}
	if operationState != "pending" && operationState != "running" {
		return fmt.Errorf("%w: create operation is %s", ErrLifecycleConflict, operationState)
	}

	var currentGeneration int64
	var sessionState, sessionDesired, allocationDesired, allocationObserved string
	if err := tx.QueryRow(ctx, `
		SELECT current_generation,state,desired_state FROM sessions WHERE id=$1 FOR UPDATE`,
		ref.Session.SessionID).Scan(&currentGeneration, &sessionState, &sessionDesired); err != nil {
		return fmt.Errorf("lock durable session create success: %w", err)
	}
	if err := tx.QueryRow(ctx, `SELECT desired_state,observed_state FROM runner_allocations WHERE id=$1`, ref.ID).
		Scan(&allocationDesired, &allocationObserved); err != nil {
		return fmt.Errorf("read durable allocation create success: %w", err)
	}
	if currentGeneration != int64(ref.Session.Generation) || sessionDesired != "active" || allocationDesired != "active" {
		return ErrGenerationStale
	}
	switch sessionState {
	case "queued", "provisioning", "booting", "setting_up", "ready":
	default:
		return fmt.Errorf("%w: session cannot accept create success from %s", ErrLifecycleConflict, sessionState)
	}
	if allocationObserved != "unknown" && allocationObserved != "provisioning" && allocationObserved != "running" {
		return fmt.Errorf("%w: allocation cannot accept create success from %s", ErrLifecycleConflict, allocationObserved)
	}
	tag, err := tx.Exec(ctx, `
		UPDATE runner_operations
		SET state='succeeded', completed_at=NOW(), error_code=NULL, error_message=NULL,
		    lease_token=NULL,lease_owner=NULL,lease_expires_at=NULL,next_attempt_at=NULL,updated_at=NOW()
		WHERE allocation_id=$1 AND provider_id=$2 AND kind='create'
		  AND state IN ('pending','running')`, ref.ID, providerID)
	if err != nil {
		return fmt.Errorf("complete durable create operation: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return errors.New("durable create operation is not completable")
	}
	if _, err := tx.Exec(ctx, `
		UPDATE runner_allocations
		SET observed_state=CASE WHEN observed_state='unknown' THEN 'provisioning' ELSE observed_state END,
		    failure_code=NULL, failure_message=NULL,
		    last_observed_at=NOW(), updated_at=NOW(), lock_version=lock_version+1
		WHERE id=$1 AND desired_state='active'`, ref.ID); err != nil {
		return fmt.Errorf("record durable allocation provisioning: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE sessions
		SET state=CASE WHEN state IN ('queued','provisioning') THEN 'booting' ELSE state END,
		    started_at=COALESCE(started_at,NOW()), updated_at=NOW(), lock_version=lock_version+1
		WHERE id=$1 AND current_generation=$2 AND desired_state='active'`, ref.Session.SessionID, int64(ref.Session.Generation)); err != nil {
		return fmt.Errorf("record durable session booting: %w", err)
	}
	if _, err := ensureEventTx(ctx, tx, ref.ID, "vm_created", "provider_created", "Execution environment was created."); err != nil {
		return err
	}
	return commitLifecycleTx(ctx, tx, "create success transition")
}

func (s *PostgresStore) MarkReady(ctx context.Context, ref AllocationRef) error {
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("begin ready transition: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.lockControllerEpochTx(ctx, tx); err != nil {
		return err
	}
	providerID, err := lockExactAllocation(ctx, tx, ref)
	if err != nil {
		return err
	}
	if err := s.requireProvider(providerID); err != nil {
		return err
	}
	var state, desired, allocationDesired, createState string
	if err := tx.QueryRow(ctx, `
		SELECT state,desired_state FROM sessions
		WHERE id=$1 AND current_generation=$2 FOR UPDATE`,
		ref.Session.SessionID, int64(ref.Session.Generation)).Scan(&state, &desired); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrGenerationStale
		}
		return fmt.Errorf("lock durable session ready transition: %w", err)
	}
	if desired != "active" {
		return ErrGenerationStale
	}
	if err := tx.QueryRow(ctx, `SELECT desired_state FROM runner_allocations WHERE id=$1`, ref.ID).Scan(&allocationDesired); err != nil {
		return fmt.Errorf("read durable allocation ready transition: %w", err)
	}
	if allocationDesired != "active" {
		return ErrGenerationStale
	}
	if err := tx.QueryRow(ctx, `SELECT state FROM runner_operations WHERE allocation_id=$1 AND kind='create' FOR UPDATE`, ref.ID).Scan(&createState); err != nil {
		return fmt.Errorf("lock durable create operation for ready transition: %w", err)
	}
	if createState != "succeeded" {
		return fmt.Errorf("%w: create operation is %s before ready", ErrLifecycleConflict, createState)
	}
	if state == "ready" {
		return commitLifecycleTx(ctx, tx, "ready replay")
	}
	if state != "booting" && state != "setting_up" && state != "provisioning" {
		return fmt.Errorf("durable session cannot become ready from %q", state)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE sessions SET state='ready',updated_at=NOW(),lock_version=lock_version+1 WHERE id=$1`, ref.Session.SessionID); err != nil {
		return fmt.Errorf("record durable session ready: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE runner_allocations
		SET observed_state='running',last_observed_at=NOW(),updated_at=NOW(),lock_version=lock_version+1
		WHERE id=$1`, ref.ID); err != nil {
		return fmt.Errorf("record durable allocation running: %w", err)
	}
	if _, err := ensureEventTx(ctx, tx, ref.ID, "ready", "setup_complete", "Environment ready."); err != nil {
		return err
	}
	return commitLifecycleTx(ctx, tx, "ready transition")
}

// MarkSettingUp publishes the setup boundary in the same transaction as the
// authoritative session state. It is safe to replay after setup has advanced.
func (s *PostgresStore) MarkSettingUp(ctx context.Context, ref AllocationRef) error {
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("begin setting up transition: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.lockControllerEpochTx(ctx, tx); err != nil {
		return err
	}
	providerID, err := lockExactAllocation(ctx, tx, ref)
	if err != nil {
		return err
	}
	if err := s.requireProvider(providerID); err != nil {
		return err
	}

	var currentGeneration int64
	var state, sessionDesired, allocationDesired, createState string
	if err := tx.QueryRow(ctx, `
		SELECT current_generation,state,desired_state FROM sessions
		WHERE id=$1 FOR UPDATE`, ref.Session.SessionID).
		Scan(&currentGeneration, &state, &sessionDesired); err != nil {
		return fmt.Errorf("lock durable session setting up transition: %w", err)
	}
	if currentGeneration != int64(ref.Session.Generation) {
		return ErrGenerationStale
	}
	if err := tx.QueryRow(ctx, `SELECT desired_state FROM runner_allocations WHERE id=$1`, ref.ID).
		Scan(&allocationDesired); err != nil {
		return fmt.Errorf("read durable allocation setting up transition: %w", err)
	}
	if sessionDesired != "active" || allocationDesired != "active" {
		return ErrGenerationStale
	}
	if err := tx.QueryRow(ctx, `
		SELECT state FROM runner_operations
		WHERE allocation_id=$1 AND provider_id=$2 AND kind='create' FOR UPDATE`, ref.ID, providerID).
		Scan(&createState); err != nil {
		return fmt.Errorf("lock durable create operation for setting up transition: %w", err)
	}
	if createState != "succeeded" {
		return fmt.Errorf("%w: create operation is %s before setup", ErrLifecycleConflict, createState)
	}
	advanced := false
	switch state {
	case "setting_up":
		// Exact replay: the singleton event check below is authoritative.
	case "ready", "verifying":
		// A late exact replay may succeed only if its event was already committed;
		// never append setup_running after ready and invert lifecycle ordering.
		advanced = true
	case "booting", "provisioning":
		if _, err := tx.Exec(ctx, `
			UPDATE sessions SET state='setting_up',updated_at=NOW(),lock_version=lock_version+1
			WHERE id=$1`, ref.Session.SessionID); err != nil {
			return fmt.Errorf("record durable session setting up: %w", err)
		}
	default:
		return fmt.Errorf("%w: session cannot start setup from %s", ErrLifecycleConflict, state)
	}
	inserted, err := ensureEventTx(ctx, tx, ref.ID, "setup_running", "setup_started", "Environment setup started.")
	if err != nil {
		return err
	}
	if advanced && inserted {
		return fmt.Errorf("%w: setup event was missing after session advanced to %s", ErrLifecycleConflict, state)
	}
	return commitLifecycleTx(ctx, tx, "setting up transition")
}

// LookupVerify resolves an exact client operation without creating it. This is
// deliberately usable after the allocation has been cleaned up, so a lost HTTP
// response can replay its durable grade without executing the verifier again.
func (s *PostgresStore) LookupVerify(ctx context.Context, userID, operationID string) (VerifyDecision, bool, error) {
	return s.lookupVerify(ctx, userID, operationID, true)
}

// lookupVerify is also used to reconcile an ambiguous commit. Browser-facing
// replays require the operation to belong to the current active generation;
// commit reconciliation deliberately disables only that moving-current gate
// and still proves authenticated owner, exact key, request hash, and result.
func (s *PostgresStore) lookupVerify(ctx context.Context, userID, operationID string, requireCurrent bool) (VerifyDecision, bool, error) {
	key, err := verifyIdempotencyKey(userID, operationID)
	if err != nil {
		return VerifyDecision{}, false, err
	}
	var ref AllocationRef
	var kind, state, errorCode, activeSessionID string
	var activeGeneration int64
	var storedHash, storedResult []byte
	err = s.db.QueryRow(ctx, `
		SELECT a.id,a.session_id,a.generation,a.provider_kind,o.kind,o.request_hash,o.state,
		       o.result_metadata,COALESCE(o.error_code,''),
		       COALESCE(current_active.id::text,''),COALESCE(current_active.current_generation,0)
		FROM runner_operations o
		JOIN runner_allocations a ON a.id=o.allocation_id AND a.provider_id=o.provider_id
		JOIN sessions sess ON sess.id=a.session_id
		LEFT JOIN LATERAL (
			SELECT current_session.id,current_session.current_generation
			FROM sessions current_session
			WHERE current_session.user_id=$3 AND current_session.desired_state='active'
			ORDER BY current_session.queued_at DESC,current_session.created_at DESC,current_session.id DESC
			LIMIT 1
		) current_active ON TRUE
		WHERE o.provider_id=$1 AND o.idempotency_key=$2 AND sess.user_id=$3`,
		s.fence.ProviderID, key, userID,
	).Scan(&ref.ID, &ref.Session.SessionID, &ref.Session.Generation, &ref.Provider,
		&kind, &storedHash, &state, &storedResult, &errorCode, &activeSessionID, &activeGeneration)
	if errors.Is(err, pgx.ErrNoRows) {
		return VerifyDecision{}, false, nil
	}
	if err != nil {
		return VerifyDecision{}, false, fmt.Errorf("load durable verify operation: %w", err)
	}
	identity, err := newVerifyIdentity(ref, userID, operationID)
	if err != nil {
		return VerifyDecision{}, false, err
	}
	if kind != "verify" || !bytes.Equal(storedHash, identity.requestHash[:]) {
		return VerifyDecision{}, false, ErrIdempotencyConflict
	}
	if requireCurrent && activeSessionID != "" && (activeSessionID != ref.Session.SessionID || activeGeneration != int64(ref.Session.Generation)) {
		return VerifyDecision{}, false, ErrIdempotencyConflict
	}
	decision, err := storedVerifyDecision(ref.Session, state, errorCode, storedResult)
	if err != nil {
		return VerifyDecision{}, false, err
	}
	return decision, true, nil
}

// BeginVerify atomically creates the verify operation, publishes verifying,
// and returns VerifyExecute. Exact retries return their typed durable state and
// never authorize another provider execution.
func (s *PostgresStore) BeginVerify(ctx context.Context, userID string, ref AllocationRef, operationID string) (VerifyDecision, error) {
	identity, err := newVerifyIdentity(ref, userID, operationID)
	if err != nil {
		return VerifyDecision{}, err
	}
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return VerifyDecision{}, fmt.Errorf("begin verify start transition: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.lockControllerEpochTx(ctx, tx); err != nil {
		return VerifyDecision{}, err
	}
	providerID, err := lockExactAllocation(ctx, tx, ref)
	if err != nil {
		return VerifyDecision{}, err
	}
	if err := s.requireProvider(providerID); err != nil {
		return VerifyDecision{}, err
	}

	var currentGeneration int64
	var state, sessionDesired, allocationDesired, allocationObserved, storedUserID string
	if err := tx.QueryRow(ctx, `
		SELECT current_generation,state,desired_state,user_id FROM sessions
		WHERE id=$1 FOR UPDATE`, ref.Session.SessionID).
		Scan(&currentGeneration, &state, &sessionDesired, &storedUserID); err != nil {
		return VerifyDecision{}, fmt.Errorf("lock durable session verify start: %w", err)
	}
	if storedUserID != userID {
		return VerifyDecision{}, ErrIdempotencyConflict
	}
	if currentGeneration != int64(ref.Session.Generation) {
		return VerifyDecision{}, ErrGenerationStale
	}
	if err := tx.QueryRow(ctx, `
		SELECT desired_state,observed_state FROM runner_allocations WHERE id=$1`, ref.ID).
		Scan(&allocationDesired, &allocationObserved); err != nil {
		return VerifyDecision{}, fmt.Errorf("read durable allocation verify start: %w", err)
	}

	_, found, err := ensureVerifyOperationTx(ctx, tx, ref, providerID, identity)
	if err != nil {
		return VerifyDecision{}, err
	}
	if found {
		decision, err := loadVerifyDecisionTx(ctx, tx, ref, providerID, identity)
		if err != nil {
			return VerifyDecision{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return s.reconcileVerifyBeginCommit(userID, operationID, decision, true, err)
		}
		return decision, nil
	}
	if sessionDesired != "active" || allocationDesired != "active" {
		return VerifyDecision{}, ErrGenerationStale
	}
	if allocationObserved != "running" {
		return VerifyDecision{}, fmt.Errorf("%w: allocation is %s before verify", ErrLifecycleConflict, allocationObserved)
	}
	if state != "ready" {
		return VerifyDecision{}, fmt.Errorf("%w: session cannot start verify from %s", ErrLifecycleConflict, state)
	}
	if attemptState, err := attemptStatusTx(ctx, tx, ref.Session); err != nil {
		return VerifyDecision{}, err
	} else if attemptState != "in_progress" {
		return VerifyDecision{}, fmt.Errorf("%w: attempt is %s before verify", ErrLifecycleConflict, attemptState)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE sessions SET state='verifying',updated_at=NOW(),lock_version=lock_version+1
		WHERE id=$1`, ref.Session.SessionID); err != nil {
		return VerifyDecision{}, fmt.Errorf("record durable session verifying: %w", err)
	}
	payload, err := boundedEventPayload(struct {
		OperationID string `json:"operation_id"`
	}{OperationID: operationID})
	if err != nil {
		return VerifyDecision{}, err
	}
	if _, err := ensureKeyedEventTx(ctx, tx, ref.ID, identity.startedEventKey(), "verify_started", "verify_requested", "Verification started.", payload); err != nil {
		return VerifyDecision{}, err
	}
	decision := VerifyDecision{Kind: VerifyExecute, Session: ref.Session}
	if err := tx.Commit(ctx); err != nil {
		return s.reconcileVerifyBeginCommit(userID, operationID, decision, false, err)
	}
	return decision, nil
}

// RecordVerifyFinished atomically records the grade and its lifecycle event.
// A passing grade also records terminal cleanup intent in event order.
func (s *PostgresStore) RecordVerifyFinished(ctx context.Context, ref AllocationRef, operationID string, success bool, verifyLog string) error {
	userID, err := s.verifyUserID(ctx, ref)
	if err != nil {
		return err
	}
	identity, err := newVerifyIdentity(ref, userID, operationID)
	if err != nil {
		return err
	}
	verifyLog = boundedDurableMessage(verifyLog)
	result := verifyResultMetadata{Success: &success, Log: &verifyLog}
	resultJSON, err := boundedEventPayload(result)
	if err != nil {
		return err
	}
	return s.recordVerifyResult(ctx, ref, userID, identity, result, resultJSON)
}

// RecordVerifyInfrastructureFailure records only a caller-supplied safe code;
// provider error text must never enter durable browser-visible lifecycle data.
func (s *PostgresStore) RecordVerifyInfrastructureFailure(ctx context.Context, ref AllocationRef, operationID, code string) error {
	userID, err := s.verifyUserID(ctx, ref)
	if err != nil {
		return err
	}
	identity, err := newVerifyIdentity(ref, userID, operationID)
	if err != nil {
		return err
	}
	code = safeVerifyInfrastructureCode(code)
	result := verifyResultMetadata{InfrastructureError: true, Code: code}
	resultJSON, err := boundedEventPayload(result)
	if err != nil {
		return err
	}
	return s.recordVerifyResult(ctx, ref, userID, identity, result, resultJSON)
}

type verifyIdentity struct {
	operationID    string
	idempotencyKey string
	requestHash    [sha256.Size]byte
}

type verifyResultMetadata struct {
	Success             *bool   `json:"success,omitempty"`
	Log                 *string `json:"log,omitempty"`
	InfrastructureError bool    `json:"infrastructure_error,omitempty"`
	Code                string  `json:"code,omitempty"`
}

func newVerifyIdentity(ref AllocationRef, userID, operationID string) (verifyIdentity, error) {
	if userID == "" || len(userID) > 128 {
		return verifyIdentity{}, errors.New("verify user id is invalid")
	}
	if len(operationID) < 8 || len(operationID) > 128 {
		return verifyIdentity{}, errors.New("verify operation id must be between 8 and 128 characters")
	}
	for _, c := range operationID {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.') {
			if c == ':' {
				continue
			}
			return verifyIdentity{}, errors.New("verify operation id contains an unsafe character")
		}
	}
	if ref.ID == "" || ref.Session.SessionID == "" || ref.Session.Generation == 0 || ref.Provider == "" {
		return verifyIdentity{}, ErrAllocationNotFound
	}
	key, err := verifyIdempotencyKey(userID, operationID)
	if err != nil {
		return verifyIdentity{}, err
	}
	hash := sha256.Sum256([]byte(fmt.Sprintf("k8s-quiz/verify/v2\x00%s\x00%s\x00%d\x00%s\x00%s\x00%s", userID, ref.ID, ref.Session.Generation, ref.Session.SessionID, ref.Provider, operationID)))
	return verifyIdentity{operationID: operationID, idempotencyKey: key, requestHash: hash}, nil
}

func verifyIdempotencyKey(userID, operationID string) (string, error) {
	if userID == "" || len(userID) > 128 {
		return "", errors.New("verify user id is invalid")
	}
	if len(operationID) < 8 || len(operationID) > 128 {
		return "", errors.New("verify operation id must be between 8 and 128 characters")
	}
	for _, c := range userID + operationID {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.') {
			if c == ':' {
				continue
			}
			return "", errors.New("verify identity contains an unsafe character")
		}
	}
	key := "verify-user:" + userID + ":" + operationID
	if err := ValidateProviderOperationKey(key); err != nil {
		return "", errors.New("verify idempotency key exceeds the provider bound")
	}
	return key, nil
}

func (v verifyIdentity) startedEventKey() string  { return v.idempotencyKey + ":started" }
func (v verifyIdentity) finishedEventKey() string { return v.idempotencyKey + ":finished" }

func ensureVerifyOperationTx(ctx context.Context, tx pgx.Tx, ref AllocationRef, providerID string, identity verifyIdentity) (string, bool, error) {
	var operationState string
	err := tx.QueryRow(ctx, `
		INSERT INTO runner_operations (
			allocation_id,provider_id,kind,idempotency_key,request_hash,state,started_at
		) VALUES ($1,$2,'verify',$3,$4,'running',NOW())
		ON CONFLICT (provider_id,idempotency_key) DO NOTHING
		RETURNING state`, ref.ID, providerID, identity.idempotencyKey, identity.requestHash[:]).Scan(&operationState)
	if err == nil {
		return operationState, false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", false, fmt.Errorf("create durable verify operation: %w", err)
	}
	var storedAllocation, kind string
	var storedHash []byte
	if err := tx.QueryRow(ctx, `
		SELECT allocation_id,kind,request_hash,state FROM runner_operations
		WHERE provider_id=$1 AND idempotency_key=$2 FOR UPDATE`, providerID, identity.idempotencyKey).
		Scan(&storedAllocation, &kind, &storedHash, &operationState); err != nil {
		return "", false, fmt.Errorf("load durable verify operation replay: %w", err)
	}
	if storedAllocation != ref.ID || kind != "verify" || !bytes.Equal(storedHash, identity.requestHash[:]) {
		return "", false, ErrIdempotencyConflict
	}
	return operationState, true, nil
}

func (s *PostgresStore) verifyUserID(ctx context.Context, ref AllocationRef) (string, error) {
	if ref.ID == "" || ref.Session.SessionID == "" || ref.Session.Generation == 0 || ref.Provider == "" {
		return "", ErrAllocationNotFound
	}
	var userID string
	err := s.db.QueryRow(ctx, `
		SELECT sess.user_id
		FROM runner_allocations a
		JOIN sessions sess ON sess.id=a.session_id
		WHERE a.id=$1 AND a.session_id=$2 AND a.generation=$3 AND a.provider_kind=$4`,
		ref.ID, ref.Session.SessionID, int64(ref.Session.Generation), ref.Provider,
	).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrAllocationNotFound
	}
	if err != nil {
		return "", fmt.Errorf("resolve durable verify user: %w", err)
	}
	return userID, nil
}

func loadVerifyDecisionTx(ctx context.Context, tx pgx.Tx, ref AllocationRef, providerID string, identity verifyIdentity) (VerifyDecision, error) {
	var state, errorCode string
	var storedAllocation, kind string
	var storedHash, storedResult []byte
	if err := tx.QueryRow(ctx, `
		SELECT state,allocation_id,kind,request_hash,result_metadata,COALESCE(error_code,'')
		FROM runner_operations
		WHERE provider_id=$1 AND idempotency_key=$2 FOR UPDATE`, providerID, identity.idempotencyKey).
		Scan(&state, &storedAllocation, &kind, &storedHash, &storedResult, &errorCode); err != nil {
		return VerifyDecision{}, fmt.Errorf("load durable verify decision: %w", err)
	}
	if storedAllocation != ref.ID || kind != "verify" || !bytes.Equal(storedHash, identity.requestHash[:]) {
		return VerifyDecision{}, ErrIdempotencyConflict
	}
	return storedVerifyDecision(ref.Session, state, errorCode, storedResult)
}

func storedVerifyDecision(session SessionRef, state, errorCode string, storedResult []byte) (VerifyDecision, error) {
	decision := VerifyDecision{Session: session}
	switch state {
	case "running":
		decision.Kind = VerifyResume
		return decision, nil
	case "succeeded":
		var result verifyResultMetadata
		if err := json.Unmarshal(storedResult, &result); err != nil || result.Success == nil || result.Log == nil || result.InfrastructureError {
			return VerifyDecision{}, fmt.Errorf("%w: durable verify grade result is invalid", ErrLifecycleConflict)
		}
		decision.Kind = VerifyGradeReplay
		decision.Success = *result.Success
		decision.Log = boundedDurableMessage(*result.Log)
		return decision, nil
	case "failed":
		var result verifyResultMetadata
		if err := json.Unmarshal(storedResult, &result); err != nil || !result.InfrastructureError || result.Success != nil || result.Code == "" {
			return VerifyDecision{}, fmt.Errorf("%w: durable verify infrastructure result is invalid", ErrLifecycleConflict)
		}
		code := safeVerifyInfrastructureCode(result.Code)
		if code != result.Code || errorCode != code {
			return VerifyDecision{}, fmt.Errorf("%w: durable verify infrastructure code is inconsistent", ErrLifecycleConflict)
		}
		decision.Kind = VerifyInfrastructureReplay
		decision.ErrorCode = code
		return decision, nil
	default:
		return VerifyDecision{}, fmt.Errorf("%w: verify operation is %s", ErrLifecycleConflict, state)
	}
}

func (s *PostgresStore) reconcileVerifyBeginCommit(userID, operationID string, expected VerifyDecision, replay bool, commitErr error) (VerifyDecision, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	actual, found, lookupErr := s.lookupVerify(ctx, userID, operationID, false)
	if lookupErr != nil {
		return VerifyDecision{}, errors.Join(ErrOperationOutcomeUnknown, commitErr, lookupErr)
	}
	if !found {
		return VerifyDecision{}, commitErr
	}
	if replay {
		return actual, nil
	}
	if actual.Kind != VerifyResume || actual.Session != expected.Session {
		return VerifyDecision{}, errors.Join(ErrOperationOutcomeUnknown, commitErr,
			fmt.Errorf("durable verify begin reconciled as %s", actual.Kind))
	}
	return expected, nil
}

func (s *PostgresStore) recordVerifyResult(ctx context.Context, ref AllocationRef, userID string, identity verifyIdentity, result verifyResultMetadata, resultJSON json.RawMessage) error {
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("begin verify result transition: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.lockControllerEpochTx(ctx, tx); err != nil {
		return err
	}
	providerID, err := lockExactAllocation(ctx, tx, ref)
	if err != nil {
		return err
	}
	if err := s.requireProvider(providerID); err != nil {
		return err
	}
	var currentGeneration int64
	var sessionState, sessionDesired string
	if err := tx.QueryRow(ctx, `
		SELECT current_generation,state,desired_state FROM sessions
		WHERE id=$1 FOR UPDATE`, ref.Session.SessionID).
		Scan(&currentGeneration, &sessionState, &sessionDesired); err != nil {
		return fmt.Errorf("lock durable session verify result: %w", err)
	}
	if currentGeneration != int64(ref.Session.Generation) {
		return ErrGenerationStale
	}

	var operationState, storedAllocation, kind string
	var storedHash, storedResult []byte
	if err := tx.QueryRow(ctx, `
		SELECT state,allocation_id,kind,request_hash,result_metadata
		FROM runner_operations
		WHERE provider_id=$1 AND idempotency_key=$2 FOR UPDATE`, providerID, identity.idempotencyKey).
		Scan(&operationState, &storedAllocation, &kind, &storedHash, &storedResult); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: durable verify operation is missing", ErrLifecycleConflict)
		}
		return fmt.Errorf("lock durable verify operation result: %w", err)
	}
	if storedAllocation != ref.ID || kind != "verify" || !bytes.Equal(storedHash, identity.requestHash[:]) {
		return ErrIdempotencyConflict
	}
	terminalState := "succeeded"
	if result.InfrastructureError {
		terminalState = "failed"
	}
	if operationState == terminalState {
		if !equalJSON(storedResult, resultJSON) {
			return ErrIdempotencyConflict
		}
		if err := tx.Commit(ctx); err != nil {
			return s.reconcileVerifyResultCommit(userID, identity.operationID, result, err)
		}
		return nil
	}
	if operationState != "running" {
		return fmt.Errorf("%w: verify operation is already %s", ErrLifecycleConflict, operationState)
	}
	if sessionState != "verifying" || sessionDesired != "active" {
		return fmt.Errorf("%w: session cannot finish verify from %s/%s", ErrLifecycleConflict, sessionState, sessionDesired)
	}
	if attemptState, err := attemptStatusTx(ctx, tx, ref.Session); err != nil {
		return err
	} else if attemptState != "in_progress" {
		return fmt.Errorf("%w: attempt is %s before verify result", ErrLifecycleConflict, attemptState)
	}

	if result.InfrastructureError {
		if _, err := tx.Exec(ctx, `
			UPDATE runner_operations
			SET state='failed',completed_at=NOW(),error_code=$3,error_message=NULL,
			    result_metadata=$4::jsonb,updated_at=NOW()
			WHERE provider_id=$1 AND idempotency_key=$2 AND state='running'`,
			providerID, identity.idempotencyKey, result.Code, string(resultJSON)); err != nil {
			return fmt.Errorf("record durable verify infrastructure failure: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE sessions SET state='ready',updated_at=NOW(),lock_version=lock_version+1
			WHERE id=$1`, ref.Session.SessionID); err != nil {
			return fmt.Errorf("restore durable session after verify infrastructure failure: %w", err)
		}
	} else {
		if _, err := tx.Exec(ctx, `
			UPDATE runner_operations
			SET state='succeeded',completed_at=NOW(),error_code=NULL,error_message=NULL,
			    result_metadata=$3::jsonb,updated_at=NOW()
			WHERE provider_id=$1 AND idempotency_key=$2 AND state='running'`,
			providerID, identity.idempotencyKey, string(resultJSON)); err != nil {
			return fmt.Errorf("complete durable verify operation: %w", err)
		}
		if result.gradeSucceeded() {
			if _, err := tx.Exec(ctx, `
				UPDATE attempts
				SET status='success',finished_at=NOW(),
				    duration_seconds=GREATEST(0,EXTRACT(EPOCH FROM (NOW()-started_at))::int),verify_log=$3
				WHERE session_id=$1 AND generation=$2 AND status='in_progress'`,
				ref.Session.SessionID, int64(ref.Session.Generation), result.verifyLog()); err != nil {
				return fmt.Errorf("record successful durable verify attempt: %w", err)
			}
			if _, err := tx.Exec(ctx, `
				UPDATE sessions
				SET state='completed',desired_state='absent',finished_at=NOW(),updated_at=NOW(),lock_version=lock_version+1
				WHERE id=$1`, ref.Session.SessionID); err != nil {
				return fmt.Errorf("record completed durable session: %w", err)
			}
			if _, err := tx.Exec(ctx, `
				UPDATE runner_allocations
				SET desired_state='absent',observed_state=CASE WHEN observed_state='absent' THEN 'absent' ELSE 'deleting' END,
				    updated_at=NOW(),lock_version=lock_version+1 WHERE id=$1`, ref.ID); err != nil {
				return fmt.Errorf("record completed durable allocation cleanup intent: %w", err)
			}
			if _, err := ensureDestroyOperationTx(ctx, tx, ref.ID, providerID); err != nil {
				return err
			}
		} else {
			if _, err := tx.Exec(ctx, `
				UPDATE attempts SET verify_log=$3
				WHERE session_id=$1 AND generation=$2 AND status='in_progress'`,
				ref.Session.SessionID, int64(ref.Session.Generation), result.verifyLog()); err != nil {
				return fmt.Errorf("record failed grade durable attempt log: %w", err)
			}
			if _, err := tx.Exec(ctx, `
				UPDATE sessions SET state='ready',updated_at=NOW(),lock_version=lock_version+1
				WHERE id=$1`, ref.Session.SessionID); err != nil {
				return fmt.Errorf("restore durable session after failed grade: %w", err)
			}
		}
	}

	reason := "grade_failed"
	message := "Verification finished."
	if result.InfrastructureError {
		reason = result.Code
		message = "Verification could not be completed."
	} else if result.gradeSucceeded() {
		reason = "grade_passed"
	}
	if _, err := ensureKeyedEventTx(ctx, tx, ref.ID, identity.finishedEventKey(), "verify_finished", reason, message, resultJSON); err != nil {
		return err
	}
	if result.gradeSucceeded() {
		if _, err := ensureEventTx(ctx, tx, ref.ID, "destroying", "completed", "Environment cleanup started."); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return s.reconcileVerifyResultCommit(userID, identity.operationID, result, err)
	}
	return nil
}

func (s *PostgresStore) reconcileVerifyResultCommit(userID, operationID string, expected verifyResultMetadata, commitErr error) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	actual, found, lookupErr := s.lookupVerify(ctx, userID, operationID, false)
	if lookupErr != nil {
		return errors.Join(ErrOperationOutcomeUnknown, commitErr, lookupErr)
	}
	if !found {
		return commitErr
	}
	if expected.InfrastructureError {
		if actual.Kind == VerifyInfrastructureReplay && actual.ErrorCode == expected.Code {
			return nil
		}
	} else if expected.Success != nil && expected.Log != nil && actual.Kind == VerifyGradeReplay &&
		actual.Success == *expected.Success && actual.Log == boundedDurableMessage(*expected.Log) {
		return nil
	}
	return errors.Join(ErrOperationOutcomeUnknown, commitErr,
		fmt.Errorf("durable verify result reconciled as %s", actual.Kind))
}

func (r verifyResultMetadata) gradeSucceeded() bool {
	return r.Success != nil && *r.Success
}

func (r verifyResultMetadata) verifyLog() string {
	if r.Log == nil {
		return ""
	}
	return *r.Log
}

func safeVerifyInfrastructureCode(code string) string {
	if code == "" || len(code) > 64 {
		return "verify_infrastructure_failure"
	}
	for _, c := range code {
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_' || c == '-' || c == '.') {
			return "verify_infrastructure_failure"
		}
	}
	return code
}

func boundedEventPayload(value any) (json.RawMessage, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode durable lifecycle event payload: %w", err)
	}
	if len(payload) > maxDurableJSONBytes {
		return nil, errors.New("durable lifecycle event payload exceeds the persisted bound")
	}
	return payload, nil
}

func equalJSON(left, right []byte) bool {
	var leftValue, rightValue any
	if json.Unmarshal(left, &leftValue) != nil || json.Unmarshal(right, &rightValue) != nil {
		return false
	}
	leftCanonical, leftErr := json.Marshal(leftValue)
	rightCanonical, rightErr := json.Marshal(rightValue)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftCanonical, rightCanonical)
}

// MarkCreateFailed preserves cleanup intent when create failed or its outcome
// is ambiguous. Scoped reconciliation must prove absence before destroyed.
func (s *PostgresStore) MarkCreateFailed(ctx context.Context, ref AllocationRef, code, message string) error {
	return s.markCreateFailed(ctx, ref, code, message, nil)
}

func (s *PostgresStore) MarkClaimedCreateFailed(ctx context.Context, claim CreateWorkClaim, code, message string) error {
	if err := validateCreateClaim(claim); err != nil {
		return err
	}
	return s.markCreateFailed(ctx, claim.Reservation.Allocation.Ref, code, message, &claim)
}

func (s *PostgresStore) markCreateFailed(ctx context.Context, ref AllocationRef, code, message string, claim *CreateWorkClaim) error {
	if code == "" {
		code = "provider_create_failed"
	}
	code = boundedDurableCode(code)
	message = boundedDurableMessage(message)
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("begin create failure transition: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.lockControllerEpochTx(ctx, tx); err != nil {
		return err
	}
	providerID, err := lockExactAllocation(ctx, tx, ref)
	if err != nil {
		return err
	}
	if err := s.requireProvider(providerID); err != nil {
		return err
	}
	var operationID, operationState string
	var leaseToken *string
	var leaseActive bool
	if err := tx.QueryRow(ctx, `
		SELECT id,state,lease_token::text,COALESCE(lease_expires_at>clock_timestamp(),false) FROM runner_operations
		WHERE allocation_id=$1 AND provider_id=$2 AND kind='create' FOR UPDATE`,
		ref.ID, providerID).Scan(&operationID, &operationState, &leaseToken, &leaseActive); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: durable create operation is missing", ErrLifecycleConflict)
		}
		return fmt.Errorf("lock durable create failure operation: %w", err)
	}
	if claim != nil {
		if operationID != claim.OperationID || operationState != "running" || leaseToken == nil || *leaseToken != claim.LeaseToken || !leaseActive {
			return fmt.Errorf("%w: create work claim is no longer current", ErrLifecycleConflict)
		}
	} else {
		if operationState == "cleanup_required" {
			return commitLifecycleTx(ctx, tx, "create failure replay")
		}
		if leaseActive {
			return fmt.Errorf("%w: create operation is leased by a worker", ErrLifecycleConflict)
		}
	}
	if operationState != "pending" && operationState != "running" {
		return fmt.Errorf("%w: create operation is %s", ErrLifecycleConflict, operationState)
	}
	var currentGeneration int64
	var sessionDesired, allocationDesired string
	if err := tx.QueryRow(ctx, `SELECT current_generation,desired_state FROM sessions WHERE id=$1 FOR UPDATE`, ref.Session.SessionID).
		Scan(&currentGeneration, &sessionDesired); err != nil {
		return fmt.Errorf("lock durable session create failure: %w", err)
	}
	if err := tx.QueryRow(ctx, `SELECT desired_state FROM runner_allocations WHERE id=$1`, ref.ID).Scan(&allocationDesired); err != nil {
		return fmt.Errorf("read durable allocation create failure: %w", err)
	}
	if currentGeneration != int64(ref.Session.Generation) || sessionDesired != "active" || allocationDesired != "active" {
		return ErrGenerationStale
	}
	tag, err := tx.Exec(ctx, `
		UPDATE runner_operations
		SET state='cleanup_required',error_code=$3,error_message=$4,
		    completed_at=NULL,lease_token=NULL,lease_owner=NULL,lease_expires_at=NULL,
		    next_attempt_at=NULL,updated_at=NOW()
		WHERE allocation_id=$1 AND provider_id=$2 AND kind='create'
		  AND state IN ('pending','running')`, ref.ID, providerID, code, message)
	if err != nil {
		return fmt.Errorf("record durable create cleanup intent: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("%w: durable create failure lost transition authority", ErrLifecycleConflict)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE runner_allocations
		SET desired_state='absent',observed_state='error',failure_code=$2,failure_message=$3,
		    last_observed_at=NOW(),updated_at=NOW(),lock_version=lock_version+1
		WHERE id=$1`, ref.ID, code, message); err != nil {
		return fmt.Errorf("record durable failed allocation: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE sessions
		SET state='failed',desired_state='absent',finished_at=COALESCE(finished_at,NOW()),
		    updated_at=NOW(),lock_version=lock_version+1
		WHERE id=$1 AND current_generation=$2`, ref.Session.SessionID, int64(ref.Session.Generation)); err != nil {
		return fmt.Errorf("record durable failed session: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE attempts
		SET status='failed',finished_at=COALESCE(finished_at,NOW()),
		    duration_seconds=COALESCE(duration_seconds,GREATEST(0,EXTRACT(EPOCH FROM (NOW()-started_at))::int)),
		    verify_log=CASE WHEN verify_log='' THEN $3 ELSE verify_log END
		WHERE session_id=$1 AND generation=$2 AND status='in_progress'`,
		ref.Session.SessionID, int64(ref.Session.Generation), message); err != nil {
		return fmt.Errorf("record durable failed attempt: %w", err)
	}
	if _, err := ensureDestroyOperationTx(ctx, tx, ref.ID, providerID); err != nil {
		return err
	}
	if _, err := ensureEventTx(ctx, tx, ref.ID, "failed", code, "Environment creation failed."); err != nil {
		return err
	}
	if _, err := ensureEventTx(ctx, tx, ref.ID, "cleanup_required", code, "Environment cleanup is pending."); err != nil {
		return err
	}
	return commitLifecycleTx(ctx, tx, "create failure transition")
}

// RequestDestroy records the first terminal outcome and an idempotent exact
// destroy operation. Older generations remain cleanable without mutating a
// newer current generation.
func (s *PostgresStore) RequestDestroy(ctx context.Context, ref AllocationRef, outcome, attemptStatus, verifyLog string) error {
	canonical, err := canonicalTerminalOutcome(outcome, attemptStatus)
	if err != nil {
		return err
	}
	verifyLog = boundedDurableMessage(verifyLog)
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("begin destroy request transition: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.lockControllerEpochTx(ctx, tx); err != nil {
		return err
	}
	providerID, err := lockExactAllocation(ctx, tx, ref)
	if err != nil {
		return err
	}
	if err := s.requireProvider(providerID); err != nil {
		return err
	}
	var desired, observed string
	if err := tx.QueryRow(ctx, `SELECT desired_state,observed_state FROM runner_allocations WHERE id=$1 FOR UPDATE`, ref.ID).Scan(&desired, &observed); err != nil {
		return fmt.Errorf("lock durable destroy target: %w", err)
	}
	var currentGeneration int64
	var sessionState, sessionDesired string
	if err := tx.QueryRow(ctx, `SELECT current_generation,state,desired_state FROM sessions WHERE id=$1 FOR UPDATE`, ref.Session.SessionID).
		Scan(&currentGeneration, &sessionState, &sessionDesired); err != nil {
		return fmt.Errorf("lock durable session destroy request: %w", err)
	}
	isCurrent := currentGeneration == int64(ref.Session.Generation)
	existingOutcome, hasOutcome, err := destroyOutcomeTx(ctx, tx, ref.ID)
	if err != nil {
		return err
	}
	if hasOutcome && existingOutcome != canonical {
		return fmt.Errorf("%w: terminal outcome is already %s", ErrLifecycleConflict, existingOutcome)
	}
	if isCurrent {
		if !hasOutcome {
			switch sessionState {
			case "completed", "failed", "timed_out", "provider_lost":
				if sessionState != canonical {
					return fmt.Errorf("%w: session outcome is already %s", ErrLifecycleConflict, sessionState)
				}
			case "destroying", "destroyed":
				return fmt.Errorf("%w: terminal outcome evidence is missing", ErrLifecycleConflict)
			}
		}
		attemptState, err := attemptStatusTx(ctx, tx, ref.Session)
		if err != nil {
			return err
		}
		if attemptState != "in_progress" && attemptState != attemptStatus {
			return fmt.Errorf("%w: attempt outcome is already %s", ErrLifecycleConflict, attemptState)
		}
		if attemptState == "in_progress" {
			tag, err := tx.Exec(ctx, `
				UPDATE attempts
				SET status=$3,finished_at=COALESCE(finished_at,NOW()),
				    duration_seconds=COALESCE(duration_seconds,GREATEST(0,EXTRACT(EPOCH FROM (NOW()-started_at))::int)),
				    verify_log=CASE WHEN verify_log='' THEN $4 ELSE verify_log END
				WHERE session_id=$1 AND generation=$2 AND status='in_progress'`,
				ref.Session.SessionID, int64(ref.Session.Generation), attemptStatus, verifyLog)
			if err != nil {
				return fmt.Errorf("record durable terminal attempt: %w", err)
			}
			if tag.RowsAffected() != 1 {
				return fmt.Errorf("%w: terminal attempt lost transition authority", ErrLifecycleConflict)
			}
		}
		if !hasOutcome {
			tag, err := tx.Exec(ctx, `
				UPDATE sessions
				SET state=$3,desired_state='absent',finished_at=COALESCE(finished_at,NOW()),
				    updated_at=NOW(),lock_version=lock_version+1
				WHERE id=$1 AND current_generation=$2`,
				ref.Session.SessionID, int64(ref.Session.Generation), canonical)
			if err != nil {
				return fmt.Errorf("record durable terminal session outcome: %w", err)
			}
			if tag.RowsAffected() != 1 {
				return ErrGenerationStale
			}
		} else if sessionDesired == "active" {
			return fmt.Errorf("%w: terminal event exists while session remains active", ErrLifecycleConflict)
		}
	}
	if _, err := tx.Exec(ctx, `
		UPDATE runner_allocations
		SET desired_state='absent',observed_state=CASE WHEN observed_state='absent' THEN 'absent' ELSE 'deleting' END,
		    updated_at=NOW(),lock_version=lock_version+1 WHERE id=$1 AND desired_state<>'absent'`, ref.ID); err != nil {
		return fmt.Errorf("record durable allocation desired absent: %w", err)
	}
	if _, err := ensureDestroyOperationTx(ctx, tx, ref.ID, providerID); err != nil {
		return err
	}
	if isCurrent {
		if terminalEvent := terminalEventType(canonical); terminalEvent != "" {
			if _, err := ensureEventTx(ctx, tx, ref.ID, terminalEvent, canonical, "Environment ended."); err != nil {
				return err
			}
		}
	}
	if _, err := ensureEventTx(ctx, tx, ref.ID, "destroying", canonical, "Environment cleanup started."); err != nil {
		return err
	}
	return commitLifecycleTx(ctx, tx, "destroy request transition")
}

func (s *PostgresStore) MarkDestroyed(ctx context.Context, ref AllocationRef) error {
	return s.markDestroyed(ctx, ref, nil)
}

func (s *PostgresStore) markDestroyed(ctx context.Context, ref AllocationRef, claim *DestroyWorkClaim) error {
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("begin destroyed transition: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.lockControllerEpochTx(ctx, tx); err != nil {
		return err
	}
	providerID, err := lockExactAllocation(ctx, tx, ref)
	if err != nil {
		return err
	}
	if err := s.requireProvider(providerID); err != nil {
		return err
	}
	var desired, observed string
	if err := tx.QueryRow(ctx, `SELECT desired_state,observed_state FROM runner_allocations WHERE id=$1 FOR UPDATE`, ref.ID).Scan(&desired, &observed); err != nil {
		return fmt.Errorf("lock durable destroyed target: %w", err)
	}
	if desired != "absent" {
		return fmt.Errorf("%w: destroy was not requested", ErrLifecycleConflict)
	}
	var destroyOperationID, destroyState string
	var leaseToken *string
	var leaseActive bool
	if err := tx.QueryRow(ctx, `
		SELECT id,state,lease_token::text,COALESCE(lease_expires_at>clock_timestamp(),false) FROM runner_operations
		WHERE allocation_id=$1 AND provider_id=$2 AND kind='destroy' FOR UPDATE`, ref.ID, providerID).
		Scan(&destroyOperationID, &destroyState, &leaseToken, &leaseActive); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: durable destroy operation is missing", ErrLifecycleConflict)
		}
		return fmt.Errorf("lock durable destroy operation: %w", err)
	}
	if destroyState != "pending" && destroyState != "running" && destroyState != "cleanup_required" && destroyState != "succeeded" {
		return fmt.Errorf("%w: destroy operation is %s", ErrLifecycleConflict, destroyState)
	}
	if claim == nil && leaseActive {
		return fmt.Errorf("%w: destroy operation is leased by a worker", ErrLifecycleConflict)
	}
	if claim != nil {
		if destroyOperationID != claim.OperationID || destroyState != "running" || leaseToken == nil || *leaseToken != claim.LeaseToken || !leaseActive {
			return fmt.Errorf("%w: destroy work claim is no longer current", ErrLifecycleConflict)
		}
	}
	if observed != "absent" {
		if _, err := tx.Exec(ctx, `
		UPDATE runner_allocations
		SET desired_state='absent',observed_state='absent',last_observed_at=NOW(),
		    failure_code=NULL,failure_message=NULL,updated_at=NOW(),lock_version=lock_version+1
		WHERE id=$1`, ref.ID); err != nil {
			return fmt.Errorf("record durable allocation absent: %w", err)
		}
	}
	// Absence is the final fence for an ambiguous or interrupted create. A
	// provider cannot still own a usable allocation after exact deletion has
	// been proven, so no create operation or lease may remain claimable. Keep a
	// recorded successful create intact: it is valid history followed by a
	// successful destroy.
	if _, err := tx.Exec(ctx, `
		UPDATE runner_operations
		SET state='failed',completed_at=COALESCE(completed_at,NOW()),
		    error_code=COALESCE(error_code,'allocation_destroyed'),
		    error_message=COALESCE(error_message,'allocation was destroyed before create completed'),
		    lease_token=NULL,lease_owner=NULL,lease_expires_at=NULL,next_attempt_at=NULL,updated_at=NOW()
		WHERE allocation_id=$1 AND provider_id=$2 AND kind='create'
		  AND state IN ('pending','running','cleanup_required')`, ref.ID, providerID); err != nil {
		return fmt.Errorf("terminalize unfinished durable create operation: %w", err)
	}
	tag, err := tx.Exec(ctx, `
		UPDATE runner_operations
		SET state='succeeded',completed_at=COALESCE(completed_at,NOW()),error_code=NULL,error_message=NULL,
		    lease_token=NULL,lease_owner=NULL,lease_expires_at=NULL,next_attempt_at=NULL,updated_at=NOW()
		WHERE allocation_id=$1 AND provider_id=$2 AND kind='destroy'
		  AND state IN ('pending','running','cleanup_required','succeeded')`, ref.ID, providerID)
	if err != nil {
		return fmt.Errorf("complete durable destroy operation: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("%w: durable destroy operation lost transition authority", ErrLifecycleConflict)
	}
	var currentGeneration int64
	var sessionDesired string
	if err := tx.QueryRow(ctx, `
		SELECT current_generation,desired_state FROM sessions WHERE id=$1 FOR UPDATE`,
		ref.Session.SessionID).Scan(&currentGeneration, &sessionDesired); err != nil {
		return fmt.Errorf("lock durable session after allocation absence: %w", err)
	}
	if currentGeneration < int64(ref.Session.Generation) {
		return fmt.Errorf("%w: destroyed generation is newer than the session", ErrLifecycleConflict)
	}
	// cleanup_pending is derived from every older generation. Make an older
	// allocation's disappearance part of the current generation's ordered
	// stream so equal-watermark snapshots can never disagree about it.
	if currentGeneration > int64(ref.Session.Generation) {
		var currentAllocationID string
		if err := tx.QueryRow(ctx, `
			SELECT id FROM runner_allocations
			WHERE session_id=$1 AND generation=$2`,
			ref.Session.SessionID, currentGeneration).Scan(&currentAllocationID); err != nil {
			return fmt.Errorf("resolve current allocation after predecessor cleanup: %w", err)
		}
		var cleanupPending bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM runner_allocations pending
				WHERE pending.session_id=$1
				  AND ($3='absent' OR pending.generation<$2)
				  AND (pending.desired_state<>'absent' OR pending.observed_state<>'absent')
			)`, ref.Session.SessionID, currentGeneration, sessionDesired).Scan(&cleanupPending); err != nil {
			return fmt.Errorf("derive cleanup state after predecessor cleanup: %w", err)
		}
		payload, err := boundedEventPayload(struct {
			CleanupPending        bool   `json:"cleanup_pending"`
			PredecessorGeneration uint64 `json:"predecessor_generation"`
		}{
			CleanupPending:        cleanupPending,
			PredecessorGeneration: ref.Session.Generation,
		})
		if err != nil {
			return err
		}
		if _, err := ensureKeyedEventTx(
			ctx, tx, currentAllocationID, "predecessor-destroyed:"+ref.ID,
			"predecessor_destroyed", "absence_proven", "Previous environment cleanup completed.", payload,
		); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `
		UPDATE sessions SET state='destroyed',desired_state='absent',finished_at=COALESCE(finished_at,NOW()),updated_at=NOW(),lock_version=lock_version+1
		WHERE id=$1 AND current_generation=$2`, ref.Session.SessionID, int64(ref.Session.Generation)); err != nil {
		return fmt.Errorf("record durable session destroyed: %w", err)
	}
	if _, err := ensureEventTx(ctx, tx, ref.ID, "destroyed", "absence_proven", "Environment cleanup completed."); err != nil {
		return err
	}
	return commitLifecycleTx(ctx, tx, "destroyed transition")
}

// SnapshotLocalRecovery freezes the durable work that existed before scoped
// provider cleanup. Startup must call this under the controller lease before
// Local Docker Reconcile(Apply=true), then pass the exact result to
// ConvergeLocalRestartSnapshot only after provider absence is proven.
func (s *PostgresStore) SnapshotLocalRecovery(ctx context.Context, providerID string) ([]LocalRecoveryAllocation, error) {
	if providerID == "" {
		return nil, errors.New("local restart recovery requires a provider id")
	}
	if err := s.requireProvider(providerID); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(ctx, `
		SELECT a.id,a.session_id,a.generation,a.provider_kind,
		       s.catalog_generation,s.problem_id,s.problem_revision,a.resource_profile
		FROM runner_allocations a
		JOIN sessions s ON s.id=a.session_id
		WHERE a.provider_id=$1 AND (a.desired_state<>'absent' OR a.observed_state<>'absent')
		ORDER BY a.created_at,a.id`, providerID)
	if err != nil {
		return nil, fmt.Errorf("list local restart recovery work: %w", err)
	}
	var snapshot []LocalRecoveryAllocation
	for rows.Next() {
		var recovery LocalRecoveryAllocation
		var generation int64
		var catalogGeneration sql.NullInt64
		if err := rows.Scan(
			&recovery.Ref.ID, &recovery.Ref.Session.SessionID, &generation, &recovery.Ref.Provider,
			&catalogGeneration, &recovery.Selection.Problem.ID, &recovery.Selection.Problem.Revision,
			&recovery.ResourceProfile,
		); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan local restart recovery work: %w", err)
		}
		if generation <= 0 || !catalogGeneration.Valid || catalogGeneration.Int64 <= 0 ||
			recovery.Ref.ID == "" || recovery.Ref.Session.SessionID == "" ||
			recovery.Selection.Problem.ID == "" || recovery.Selection.Problem.Revision == "" ||
			recovery.ResourceProfile == "" {
			rows.Close()
			return nil, fmt.Errorf("local restart allocation %q lacks trusted catalog or resource provenance", recovery.Ref.ID)
		}
		recovery.Ref.Session.Generation = uint64(generation)
		recovery.Selection.Generation = uint64(catalogGeneration.Int64)
		snapshot = append(snapshot, recovery)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("iterate local restart recovery work: %w", err)
	}
	rows.Close()
	return snapshot, nil
}

// ConvergeLocalRestartSnapshot records absence only for the pre-cleanup
// snapshot. It refuses provider mismatches and verifies every row again inside
// its transaction, so later or foreign allocations cannot be swept into the
// startup result.
func (s *PostgresStore) ConvergeLocalRestartSnapshot(ctx context.Context, providerID string, snapshot []LocalRecoveryAllocation) (int, error) {
	if err := s.requireProvider(providerID); err != nil {
		return 0, err
	}
	var errs []error
	converged := 0
	for _, recovery := range snapshot {
		if recovery.Ref.Provider != ProviderLocalDocker {
			errs = append(errs, fmt.Errorf("refuse local restart convergence for provider %q allocation %s", recovery.Ref.Provider, recovery.Ref.ID))
			continue
		}
		if err := s.convergeOneLocalRestart(ctx, providerID, recovery); err != nil {
			errs = append(errs, err)
			continue
		}
		converged++
	}
	return converged, errors.Join(errs...)
}

func (s *PostgresStore) convergeOneLocalRestart(ctx context.Context, expectedProviderID string, recovery LocalRecoveryAllocation) error {
	ref := recovery.Ref
	if err := s.requireProvider(expectedProviderID); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.lockControllerEpochTx(ctx, tx); err != nil {
		return err
	}
	providerID, err := lockExactAllocation(ctx, tx, ref)
	if err != nil {
		return err
	}
	if providerID != expectedProviderID {
		return fmt.Errorf("%w: allocation belongs to provider %q, not %q", ErrLifecycleConflict, providerID, expectedProviderID)
	}
	if err := s.requireProvider(providerID); err != nil {
		return err
	}
	var observed, desired, resourceProfile string
	if err := tx.QueryRow(ctx, `SELECT observed_state,desired_state,resource_profile FROM runner_allocations WHERE id=$1 FOR UPDATE`, ref.ID).Scan(&observed, &desired, &resourceProfile); err != nil {
		return err
	}
	var currentGeneration int64
	var catalogGeneration sql.NullInt64
	var problemID, problemRevision string
	if err := tx.QueryRow(ctx, `
		SELECT current_generation,catalog_generation,problem_id,problem_revision
		FROM sessions WHERE id=$1 FOR UPDATE`, ref.Session.SessionID).Scan(
		&currentGeneration, &catalogGeneration, &problemID, &problemRevision,
	); err != nil {
		return err
	}
	if !catalogGeneration.Valid || catalogGeneration.Int64 <= 0 ||
		uint64(catalogGeneration.Int64) != recovery.Selection.Generation ||
		problemID != recovery.Selection.Problem.ID || problemRevision != recovery.Selection.Problem.Revision ||
		resourceProfile != recovery.ResourceProfile {
		return fmt.Errorf("%w: local restart provenance changed for allocation %s", ErrLifecycleConflict, ref.ID)
	}
	if observed == "absent" && desired == "absent" {
		return commitLifecycleTx(ctx, tx, "local restart replay")
	}
	isCurrent := currentGeneration == int64(ref.Session.Generation)
	causedFailure := false
	if isCurrent {
		attemptState, err := attemptStatusTx(ctx, tx, ref.Session)
		if err != nil {
			return err
		}
		causedFailure = attemptState == "in_progress"
	}
	if _, err := tx.Exec(ctx, `
		UPDATE runner_allocations
		SET desired_state='absent',observed_state='absent',last_observed_at=NOW(),
		    failure_code=NULL,failure_message=NULL,updated_at=NOW(),lock_version=lock_version+1
		WHERE id=$1`, ref.ID); err != nil {
		return err
	}
	if isCurrent {
		if _, err := tx.Exec(ctx, `
		UPDATE sessions
		SET state='destroyed',desired_state='absent',finished_at=COALESCE(finished_at,NOW()),updated_at=NOW(),lock_version=lock_version+1
		WHERE id=$1 AND current_generation=$2`, ref.Session.SessionID, int64(ref.Session.Generation)); err != nil {
			return err
		}
	}
	if causedFailure {
		if _, err := tx.Exec(ctx, `
		UPDATE attempts
		SET status='failed',finished_at=COALESCE(finished_at,NOW()),
		    duration_seconds=COALESCE(duration_seconds,GREATEST(0,EXTRACT(EPOCH FROM (NOW()-started_at))::int)),
		    verify_log=CASE WHEN verify_log='' THEN 'control plane restarted; local development environment was removed' ELSE verify_log END
		WHERE session_id=$1 AND generation=$2 AND status='in_progress'`, ref.Session.SessionID, int64(ref.Session.Generation)); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `
		UPDATE runner_operations
		SET state=CASE WHEN kind='destroy' THEN 'succeeded' ELSE 'failed' END,
		    error_code=CASE WHEN kind='destroy' THEN NULL ELSE 'control_plane_restarted' END,
		    error_message=CASE WHEN kind='destroy' THEN NULL ELSE 'local development allocation removed during restart recovery' END,
		    completed_at=COALESCE(completed_at,NOW()),lease_token=NULL,lease_owner=NULL,lease_expires_at=NULL,updated_at=NOW()
		WHERE allocation_id=$1 AND provider_id=$2 AND state IN ('pending','running','cleanup_required')`, ref.ID, providerID); err != nil {
		return err
	}
	if _, err := ensureDestroyOperationTx(ctx, tx, ref.ID, providerID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE runner_operations
		SET state='succeeded',completed_at=COALESCE(completed_at,NOW()),error_code=NULL,error_message=NULL,
		    lease_token=NULL,lease_owner=NULL,lease_expires_at=NULL,updated_at=NOW()
		WHERE allocation_id=$1 AND provider_id=$2 AND kind='destroy'
		  AND state IN ('pending','running','cleanup_required','succeeded')`, ref.ID, providerID); err != nil {
		return err
	}
	if causedFailure {
		if _, err := ensureEventTx(ctx, tx, ref.ID, "failed", "control_plane_restarted", "Local development environment ended during server restart."); err != nil {
			return err
		}
	}
	if _, err := ensureEventTx(ctx, tx, ref.ID, "destroyed", "scoped_reconcile_absent", "Local development environment cleanup completed."); err != nil {
		return err
	}
	return commitLifecycleTx(ctx, tx, "local restart convergence")
}

func lockExactAllocation(ctx context.Context, tx pgx.Tx, ref AllocationRef) (string, error) {
	if ref.ID == "" || ref.Session.SessionID == "" || ref.Session.Generation == 0 || ref.Provider == "" {
		return "", ErrAllocationNotFound
	}
	var sessionID, providerID string
	var generation int64
	var provider ProviderKind
	err := tx.QueryRow(ctx, `
		SELECT session_id,generation,provider_kind,provider_id
		FROM runner_allocations WHERE id=$1 FOR UPDATE`, ref.ID).Scan(&sessionID, &generation, &provider, &providerID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrAllocationNotFound
	}
	if err != nil {
		return "", fmt.Errorf("lock exact durable allocation: %w", err)
	}
	if sessionID != ref.Session.SessionID || generation != int64(ref.Session.Generation) || provider != ref.Provider {
		return "", ErrAllocationNotFound
	}
	return providerID, nil
}

func appendEventTx(ctx context.Context, tx pgx.Tx, allocationID, eventType, reasonCode, message string) (uint64, error) {
	reasonCode = boundedDurableCode(reasonCode)
	message = boundedDurableMessage(message)
	var sequence int64
	if err := tx.QueryRow(ctx, `
		UPDATE runner_allocations SET last_event_sequence=last_event_sequence+1,updated_at=NOW()
		WHERE id=$1 RETURNING last_event_sequence`, allocationID).Scan(&sequence); err != nil {
		return 0, fmt.Errorf("reserve durable lifecycle event sequence: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO session_events (allocation_id,sequence,event_type,reason_code,message,sanitized_payload)
		VALUES ($1,$2,$3,NULLIF($4,''),$5,'{}'::jsonb)`, allocationID, sequence, eventType, reasonCode, message); err != nil {
		return 0, fmt.Errorf("append durable lifecycle event: %w", err)
	}
	return uint64(sequence), nil
}

func ensureEventTx(ctx context.Context, tx pgx.Tx, allocationID, eventType, reasonCode, message string) (bool, error) {
	var exists bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS(SELECT 1 FROM session_events WHERE allocation_id=$1 AND event_type=$2)`,
		allocationID, eventType).Scan(&exists); err != nil {
		return false, fmt.Errorf("check durable lifecycle event: %w", err)
	}
	if exists {
		return false, nil
	}
	if _, err := appendEventTx(ctx, tx, allocationID, eventType, reasonCode, message); err != nil {
		return false, err
	}
	return true, nil
}

func ensureKeyedEventTx(ctx context.Context, tx pgx.Tx, allocationID, eventKey, eventType, reasonCode, message string, payload json.RawMessage) (bool, error) {
	if eventKey == "" || len(eventKey) > 255 {
		return false, errors.New("durable lifecycle event key is invalid")
	}
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	if !json.Valid(payload) || len(payload) > maxDurableJSONBytes {
		return false, errors.New("durable lifecycle event payload is invalid or exceeds the persisted bound")
	}
	var storedType string
	var storedPayload []byte
	err := tx.QueryRow(ctx, `
		SELECT event_type,sanitized_payload FROM session_events
		WHERE allocation_id=$1 AND event_key=$2`, allocationID, eventKey).Scan(&storedType, &storedPayload)
	if err == nil {
		if storedType != eventType || !equalJSON(storedPayload, payload) {
			return false, ErrIdempotencyConflict
		}
		return false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("check keyed durable lifecycle event: %w", err)
	}
	reasonCode = boundedDurableCode(reasonCode)
	message = boundedDurableMessage(message)
	var sequence int64
	if err := tx.QueryRow(ctx, `
		UPDATE runner_allocations SET last_event_sequence=last_event_sequence+1,updated_at=NOW()
		WHERE id=$1 RETURNING last_event_sequence`, allocationID).Scan(&sequence); err != nil {
		return false, fmt.Errorf("reserve keyed durable lifecycle event sequence: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO session_events (
			allocation_id,sequence,event_type,reason_code,message,sanitized_payload,event_key
		) VALUES ($1,$2,$3,NULLIF($4,''),$5,$6::jsonb,$7)`,
		allocationID, sequence, eventType, reasonCode, message, string(payload), eventKey); err != nil {
		return false, fmt.Errorf("append keyed durable lifecycle event: %w", err)
	}
	return true, nil
}

func destroyOutcomeTx(ctx context.Context, tx pgx.Tx, allocationID string) (string, bool, error) {
	var outcome string
	err := tx.QueryRow(ctx, `
		SELECT COALESCE(reason_code,'') FROM session_events
		WHERE allocation_id=$1 AND event_type='destroying'
		ORDER BY sequence LIMIT 1`, allocationID).Scan(&outcome)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("load durable terminal outcome: %w", err)
	}
	return outcome, true, nil
}

func attemptStatusTx(ctx context.Context, tx pgx.Tx, ref SessionRef) (string, error) {
	var status string
	if err := tx.QueryRow(ctx, `SELECT status FROM attempts WHERE session_id=$1 AND generation=$2 FOR UPDATE`,
		ref.SessionID, int64(ref.Generation)).Scan(&status); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", fmt.Errorf("%w: durable attempt is missing", ErrLifecycleConflict)
		}
		return "", fmt.Errorf("lock durable attempt: %w", err)
	}
	return status, nil
}

func ensureDestroyOperationTx(ctx context.Context, tx pgx.Tx, allocationID, providerID string) (string, error) {
	key := "destroy:" + allocationID
	hash := sha256.Sum256([]byte("k8s-quiz/destroy/v1\x00" + allocationID + "\x00" + providerID))
	var operationID string
	err := tx.QueryRow(ctx, `
		INSERT INTO runner_operations (allocation_id,provider_id,kind,idempotency_key,request_hash,state)
		VALUES ($1,$2,'destroy',$3,$4,'pending')
		ON CONFLICT (provider_id,idempotency_key) DO NOTHING
		RETURNING id`, allocationID, providerID, key, hash[:]).Scan(&operationID)
	if err == nil {
		return operationID, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("create durable destroy operation: %w", err)
	}
	var storedAllocation, kind string
	var storedHash []byte
	if err := tx.QueryRow(ctx, `
		SELECT id,allocation_id,kind,request_hash FROM runner_operations
		WHERE provider_id=$1 AND idempotency_key=$2`, providerID, key).Scan(&operationID, &storedAllocation, &kind, &storedHash); err != nil {
		return "", fmt.Errorf("load durable destroy operation replay: %w", err)
	}
	if storedAllocation != allocationID || kind != "destroy" || !bytes.Equal(storedHash, hash[:]) {
		return "", ErrIdempotencyConflict
	}
	return operationID, nil
}

func canonicalTerminalOutcome(outcome, attemptStatus string) (string, error) {
	switch outcome {
	case "completed":
		if attemptStatus != "success" {
			return "", errors.New("completed session requires success attempt status")
		}
	case "failed":
		if attemptStatus != "failed" {
			return "", errors.New("failed session requires failed attempt status")
		}
	case "timed_out":
		if attemptStatus != "timeout" {
			return "", errors.New("timed out session requires timeout attempt status")
		}
	case "provider_lost":
		if attemptStatus != "failed" {
			return "", errors.New("provider lost session requires failed attempt status")
		}
	default:
		return "", fmt.Errorf("invalid durable terminal outcome %q", outcome)
	}
	return outcome, nil
}

func terminalEventType(outcome string) string {
	switch outcome {
	case "failed", "timed_out", "provider_lost":
		return outcome
	default:
		return ""
	}
}

func boundedDurableMessage(value string) string {
	return boundedUTF8(value, 1024)
}

func boundedDurableCode(value string) string {
	return boundedUTF8(value, 64)
}

func boundedUTF8(value string, limit int) string {
	value = strings.ToValidUTF8(value, "\uFFFD")
	if len(value) <= limit {
		return value
	}
	end := limit
	for end > 0 && !utf8.ValidString(value[:end]) {
		end--
	}
	return value[:end]
}

func commitLifecycleTx(ctx context.Context, tx pgx.Tx, operation string) error {
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit durable %s: %w", operation, err)
	}
	return nil
}
