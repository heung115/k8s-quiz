package runner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

type endIdentity struct {
	idempotencyKey string
	requestHash    [sha256.Size]byte
}

func newEndIdentity(userID string, expected SessionRef, operationID string) (endIdentity, error) {
	if expected.SessionID == "" || expected.Generation == 0 {
		return endIdentity{}, errors.New("end session identity is invalid")
	}
	key, err := verifyIdempotencyKey(userID, operationID)
	if err != nil {
		return endIdentity{}, err
	}
	hash := sha256.Sum256([]byte(fmt.Sprintf(
		"k8s-quiz/end/v1\x00%s\x00%s\x00%d\x00%s",
		userID, expected.SessionID, expected.Generation, operationID,
	)))
	return endIdentity{idempotencyKey: key, requestHash: hash}, nil
}

// RequestEnd creates or replays one exact logical-session end operation. The
// first transaction marks every generation desired-absent and reserves every
// destroy operation. Replays complete the end operation only after every
// allocation is physically absent and every destroy operation succeeded.
func (s *PostgresStore) RequestEnd(ctx context.Context, userID string, expected SessionRef, operationID string) (EndDecision, error) {
	identity, err := newEndIdentity(userID, expected, operationID)
	if err != nil {
		return EndDecision{}, err
	}
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return EndDecision{}, fmt.Errorf("begin end operation: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.lockControllerEpochTx(ctx, tx); err != nil {
		return EndDecision{}, err
	}

	var lockedUser string
	if err := tx.QueryRow(ctx, `SELECT id FROM users WHERE id=$1 FOR UPDATE`, userID).Scan(&lockedUser); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return EndDecision{}, ErrAllocationNotFound
		}
		return EndDecision{}, fmt.Errorf("lock end operation user: %w", err)
	}

	operationIDDB, found, err := loadEndOperationTx(ctx, tx, s.fence.ProviderID, userID, expected, identity)
	if err != nil {
		return EndDecision{}, err
	}
	if !found {
		operationIDDB, err = createEndOperationTx(ctx, tx, s.fence.ProviderID, userID, expected, identity)
		if err != nil {
			return EndDecision{}, err
		}
	}
	decision, err := reconcileEndOperationTx(ctx, tx, s.fence.ProviderID, operationIDDB, expected)
	if err != nil {
		return EndDecision{}, err
	}
	if err := commitLifecycleTx(ctx, tx, "end operation"); err != nil {
		return s.reconcileEndCommit(userID, expected, identity, decision, err)
	}
	return decision, nil
}

func (s *PostgresStore) reconcileEndCommit(
	userID string,
	expected SessionRef,
	identity endIdentity,
	expectedDecision EndDecision,
	commitErr error,
) (EndDecision, error) {
	reconcileCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	decision, found, lookupErr := s.lookupEndOperation(reconcileCtx, userID, expected, identity)
	if lookupErr != nil {
		return EndDecision{}, errors.Join(ErrOperationOutcomeUnknown, commitErr, lookupErr)
	}
	if !found {
		return EndDecision{}, commitErr
	}
	if decision.Session == expected && decision.State == expectedDecision.State &&
		sameAllocationRefs(decision.Pending, expectedDecision.Pending) {
		return decision, nil
	}
	return EndDecision{}, errors.Join(ErrOperationOutcomeUnknown, commitErr,
		fmt.Errorf("durable end commit reconciled as %s with %d pending allocations", decision.State, len(decision.Pending)))
}

func (s *PostgresStore) lookupEndOperation(
	ctx context.Context,
	userID string,
	expected SessionRef,
	identity endIdentity,
) (EndDecision, bool, error) {
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return EndDecision{}, false, fmt.Errorf("begin end reconciliation: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	operationID, found, err := loadEndOperationTx(ctx, tx, s.fence.ProviderID, userID, expected, identity)
	if err != nil || !found {
		return EndDecision{}, found, err
	}
	decision, err := readEndOperationTx(ctx, tx, s.fence.ProviderID, operationID, expected)
	if err != nil {
		return EndDecision{}, false, err
	}
	return decision, true, nil
}

func sameAllocationRefs(left, right []AllocationRef) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func loadEndOperationTx(
	ctx context.Context,
	tx pgx.Tx,
	providerID, userID string,
	expected SessionRef,
	identity endIdentity,
) (string, bool, error) {
	var operationID, allocationID, sessionID, kind, ownerID string
	var generation int64
	var storedHash []byte
	err := tx.QueryRow(ctx, `
		SELECT o.id,o.allocation_id,a.session_id,a.generation,o.kind,o.request_hash,s.user_id
		FROM runner_operations o
		JOIN runner_allocations a ON a.id=o.allocation_id AND a.provider_id=o.provider_id
		JOIN sessions s ON s.id=a.session_id
		WHERE o.provider_id=$1 AND o.idempotency_key=$2
		FOR UPDATE OF o,a,s`, providerID, identity.idempotencyKey).Scan(
		&operationID, &allocationID, &sessionID, &generation, &kind, &storedHash, &ownerID,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("load durable end replay: %w", err)
	}
	if ownerID != userID || sessionID != expected.SessionID || generation != int64(expected.Generation) ||
		kind != "end" || !bytes.Equal(storedHash, identity.requestHash[:]) {
		return "", false, ErrIdempotencyConflict
	}
	return operationID, true, nil
}

func createEndOperationTx(
	ctx context.Context,
	tx pgx.Tx,
	providerID, userID string,
	expected SessionRef,
	identity endIdentity,
) (string, error) {
	var currentGeneration int64
	var sessionDesired string
	if err := tx.QueryRow(ctx, `
		SELECT current_generation,desired_state FROM sessions
		WHERE id=$1 AND user_id=$2 FOR UPDATE`, expected.SessionID, userID).
		Scan(&currentGeneration, &sessionDesired); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrAllocationNotFound
		}
		return "", fmt.Errorf("lock exact end session: %w", err)
	}
	if currentGeneration != int64(expected.Generation) || sessionDesired != "active" {
		return "", ErrGenerationStale
	}

	var currentAllocationID string
	var currentProviderID string
	if err := tx.QueryRow(ctx, `
		SELECT id,provider_id FROM runner_allocations
		WHERE session_id=$1 AND generation=$2 FOR UPDATE`,
		expected.SessionID, int64(expected.Generation)).Scan(&currentAllocationID, &currentProviderID); err != nil {
		return "", fmt.Errorf("lock exact end allocation: %w", err)
	}
	if currentProviderID != providerID {
		return "", fmt.Errorf("%w: end allocation provider changed", ErrLifecycleConflict)
	}

	var endOperationID string
	if err := tx.QueryRow(ctx, `
		INSERT INTO runner_operations (
			allocation_id,provider_id,kind,idempotency_key,request_hash,state
		) VALUES ($1,$2,'end',$3,$4,'pending') RETURNING id`,
		currentAllocationID, providerID, identity.idempotencyKey, identity.requestHash[:],
	).Scan(&endOperationID); err != nil {
		return "", mapReservationError(err)
	}

	if _, err := tx.Exec(ctx, `
		UPDATE attempts
		SET status='failed',finished_at=COALESCE(finished_at,NOW()),
		    duration_seconds=COALESCE(duration_seconds,GREATEST(0,EXTRACT(EPOCH FROM (NOW()-started_at))::int)),
		    verify_log=CASE WHEN verify_log='' THEN 'environment ended by user' ELSE verify_log END
		WHERE session_id=$1 AND generation=$2 AND status='in_progress'`,
		expected.SessionID, int64(expected.Generation)); err != nil {
		return "", fmt.Errorf("terminalize exact end attempt: %w", err)
	}
	if tag, err := tx.Exec(ctx, `
		UPDATE sessions
		SET state='failed',desired_state='absent',finished_at=COALESCE(finished_at,NOW()),
		    updated_at=NOW(),lock_version=lock_version+1
		WHERE id=$1 AND user_id=$2 AND current_generation=$3 AND desired_state='active'`,
		expected.SessionID, userID, int64(expected.Generation)); err != nil {
		return "", fmt.Errorf("record exact end session intent: %w", err)
	} else if tag.RowsAffected() != 1 {
		return "", ErrGenerationStale
	}

	rows, err := tx.Query(ctx, `
		SELECT id,generation,provider_kind,provider_id,observed_state
		FROM runner_allocations WHERE session_id=$1
		ORDER BY generation FOR UPDATE`, expected.SessionID)
	if err != nil {
		return "", fmt.Errorf("lock end allocation chain: %w", err)
	}
	type endAllocation struct {
		ref        AllocationRef
		providerID string
		observed   string
	}
	allocations := make([]endAllocation, 0)
	for rows.Next() {
		var item endAllocation
		var generation int64
		if err := rows.Scan(&item.ref.ID, &generation, &item.ref.Provider, &item.providerID, &item.observed); err != nil {
			rows.Close()
			return "", fmt.Errorf("scan end allocation chain: %w", err)
		}
		if generation <= 0 || item.providerID != providerID {
			rows.Close()
			return "", fmt.Errorf("%w: end allocation chain changed provider", ErrLifecycleConflict)
		}
		item.ref.Session = SessionRef{SessionID: expected.SessionID, Generation: uint64(generation)}
		allocations = append(allocations, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return "", fmt.Errorf("iterate end allocation chain: %w", err)
	}
	rows.Close()
	if len(allocations) == 0 {
		return "", fmt.Errorf("%w: end session has no allocations", ErrLifecycleConflict)
	}

	if _, err := ensureEventTx(ctx, tx, currentAllocationID, "failed", "failed", "Environment ended."); err != nil {
		return "", err
	}
	for _, allocation := range allocations {
		if _, err := tx.Exec(ctx, `
			UPDATE runner_allocations
			SET desired_state='absent',
			    observed_state=CASE WHEN observed_state='absent' THEN 'absent' ELSE 'deleting' END,
			    updated_at=NOW(),lock_version=lock_version+1
			WHERE id=$1`, allocation.ref.ID); err != nil {
			return "", fmt.Errorf("record allocation end intent: %w", err)
		}
		destroyOperationID, err := ensureDestroyOperationTx(ctx, tx, allocation.ref.ID, providerID)
		if err != nil {
			return "", err
		}
		if allocation.observed == "absent" {
			if _, err := tx.Exec(ctx, `
				UPDATE runner_operations
				SET state='succeeded',completed_at=COALESCE(completed_at,NOW()),
				    lease_token=NULL,lease_owner=NULL,lease_expires_at=NULL,next_attempt_at=NULL,
				    error_code=NULL,error_message=NULL,updated_at=NOW()
				WHERE id=$1`, destroyOperationID); err != nil {
				return "", fmt.Errorf("complete already-absent destroy operation: %w", err)
			}
		}
		if _, err := ensureEventTx(ctx, tx, allocation.ref.ID, "destroying", "failed", "Environment cleanup started."); err != nil {
			return "", err
		}
	}
	return endOperationID, nil
}

func reconcileEndOperationTx(
	ctx context.Context,
	tx pgx.Tx,
	providerID, endOperationID string,
	expected SessionRef,
) (EndDecision, error) {
	var state string
	if err := tx.QueryRow(ctx, `
		SELECT state FROM runner_operations
		WHERE id=$1 AND provider_id=$2 AND kind='end' FOR UPDATE`,
		endOperationID, providerID).Scan(&state); err != nil {
		return EndDecision{}, fmt.Errorf("lock durable end operation: %w", err)
	}
	if state != "pending" && state != "succeeded" {
		return EndDecision{}, fmt.Errorf("%w: end operation is %s", ErrLifecycleConflict, state)
	}

	rows, err := tx.Query(ctx, `
		SELECT id,generation,provider_kind,provider_id,desired_state,observed_state
		FROM runner_allocations
		WHERE session_id=$1
		ORDER BY generation FOR UPDATE`, expected.SessionID)
	if err != nil {
		return EndDecision{}, fmt.Errorf("inspect end allocation chain: %w", err)
	}
	pending := make([]AllocationRef, 0)
	complete := true
	for rows.Next() {
		var ref AllocationRef
		var generation int64
		var allocationProviderID, desired, observed string
		if err := rows.Scan(&ref.ID, &generation, &ref.Provider, &allocationProviderID, &desired, &observed); err != nil {
			rows.Close()
			return EndDecision{}, fmt.Errorf("scan end allocation status: %w", err)
		}
		if generation <= 0 || allocationProviderID != providerID || desired != "absent" {
			rows.Close()
			return EndDecision{}, fmt.Errorf("%w: end allocation chain is incomplete", ErrLifecycleConflict)
		}
		ref.Session = SessionRef{SessionID: expected.SessionID, Generation: uint64(generation)}
		pending = append(pending, ref)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return EndDecision{}, fmt.Errorf("iterate end allocation status: %w", err)
	}
	rows.Close()
	if len(pending) == 0 {
		return EndDecision{}, fmt.Errorf("%w: end session has no allocations", ErrLifecycleConflict)
	}
	remaining := pending[:0]
	for _, ref := range pending {
		var observed, destroyState string
		if err := tx.QueryRow(ctx, `
			SELECT a.observed_state,d.state
			FROM runner_allocations a
			JOIN runner_operations d
			  ON d.allocation_id=a.id AND d.provider_id=a.provider_id AND d.kind='destroy'
			WHERE a.id=$1 AND a.provider_id=$2
			FOR UPDATE OF d`, ref.ID, providerID).Scan(&observed, &destroyState); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return EndDecision{}, fmt.Errorf("%w: end destroy operation is missing", ErrLifecycleConflict)
			}
			return EndDecision{}, fmt.Errorf("lock end destroy operation: %w", err)
		}
		if observed != "absent" || destroyState != "succeeded" {
			complete = false
			remaining = append(remaining, ref)
		}
	}
	pending = remaining

	decision := EndDecision{Session: expected, State: EndPending, Pending: pending}
	if complete {
		if state == "pending" {
			if tag, err := tx.Exec(ctx, `
				UPDATE runner_operations
				SET state='succeeded',completed_at=NOW(),updated_at=NOW()
				WHERE id=$1 AND state='pending'`, endOperationID); err != nil {
				return EndDecision{}, fmt.Errorf("complete durable end operation: %w", err)
			} else if tag.RowsAffected() != 1 {
				return EndDecision{}, fmt.Errorf("%w: end completion lost authority", ErrLifecycleConflict)
			}
		}
		decision.State = EndCompleted
		decision.Pending = nil
	} else if state == "succeeded" {
		return EndDecision{}, fmt.Errorf("%w: completed end operation has outstanding allocations", ErrLifecycleConflict)
	}
	return decision, nil
}

func readEndOperationTx(
	ctx context.Context,
	tx pgx.Tx,
	providerID, endOperationID string,
	expected SessionRef,
) (EndDecision, error) {
	var state string
	if err := tx.QueryRow(ctx, `
		SELECT state FROM runner_operations
		WHERE id=$1 AND provider_id=$2 AND kind='end'`, endOperationID, providerID).Scan(&state); err != nil {
		return EndDecision{}, fmt.Errorf("read durable end operation: %w", err)
	}
	if state != "pending" && state != "succeeded" {
		return EndDecision{}, fmt.Errorf("%w: end operation is %s", ErrLifecycleConflict, state)
	}
	rows, err := tx.Query(ctx, `
		SELECT a.id,a.generation,a.provider_kind,a.provider_id,a.desired_state,a.observed_state,
		       COALESCE(d.state,'')
		FROM runner_allocations a
		LEFT JOIN runner_operations d
		  ON d.allocation_id=a.id AND d.provider_id=a.provider_id AND d.kind='destroy'
		WHERE a.session_id=$1
		ORDER BY a.generation`, expected.SessionID)
	if err != nil {
		return EndDecision{}, fmt.Errorf("read end allocation chain: %w", err)
	}
	defer rows.Close()
	decision := EndDecision{Session: expected, State: EndPending}
	count := 0
	for rows.Next() {
		count++
		var ref AllocationRef
		var generation int64
		var allocationProviderID, desired, observed, destroyState string
		if err := rows.Scan(&ref.ID, &generation, &ref.Provider, &allocationProviderID, &desired, &observed, &destroyState); err != nil {
			return EndDecision{}, fmt.Errorf("scan end reconciliation allocation: %w", err)
		}
		if generation <= 0 || allocationProviderID != providerID || desired != "absent" || destroyState == "" {
			return EndDecision{}, fmt.Errorf("%w: end reconciliation chain is incomplete", ErrLifecycleConflict)
		}
		ref.Session = SessionRef{SessionID: expected.SessionID, Generation: uint64(generation)}
		if observed != "absent" || destroyState != "succeeded" {
			decision.Pending = append(decision.Pending, ref)
		}
	}
	if err := rows.Err(); err != nil {
		return EndDecision{}, fmt.Errorf("iterate end reconciliation allocations: %w", err)
	}
	if count == 0 {
		return EndDecision{}, fmt.Errorf("%w: end session has no allocations", ErrLifecycleConflict)
	}
	if len(decision.Pending) == 0 {
		if state != "succeeded" {
			return EndDecision{}, fmt.Errorf("%w: end completion is not committed", ErrLifecycleConflict)
		}
		decision.State = EndCompleted
	} else if state == "succeeded" {
		return EndDecision{}, fmt.Errorf("%w: completed end operation has outstanding allocations", ErrLifecycleConflict)
	}
	return decision, nil
}
