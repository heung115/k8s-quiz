package runner

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	minDestroyLease = time.Second
	maxDestroyLease = 5 * time.Minute
	maxDestroyRetry = 15 * time.Minute
)

// DestroyWorkClaim is a short-lived, database-backed right to execute one
// exact idempotent DestroySession call. LeaseToken, rather than LeaseOwner,
// identifies the claim: a worker whose lease expired cannot complete or
// reschedule work after another worker has reclaimed it.
type DestroyWorkClaim struct {
	OperationID    string
	LeaseToken     string
	LeaseOwner     string
	Ref            AllocationRef
	Attempt        int
	LeaseExpiresAt time.Time
	// Pending is an exact, non-authoritative observation that matching destroy
	// work is already leased or durably queued for retry. It is returned only
	// to allocation-scoped callers and never grants provider mutation rights.
	Pending   bool
	Completed bool
}

// CreateWorkClaim is the short-lived right to execute a current-generation
// create. Generation one and reset replacements share the same protocol;
// replacements additionally require every older allocation to be absent.
type CreateWorkClaim struct {
	OperationID    string
	LeaseToken     string
	LeaseOwner     string
	Reservation    SessionReservation
	Attempt        int
	LeaseExpiresAt time.Time
}

// PrepareRecoveryWork releases operation leases left by the previous
// controller epoch. The caller has already acquired the provider advisory
// lease and incremented the epoch, so no current-epoch worker can own these
// rows yet. Retry delay is cleared so startup can reach a fixed point before
// admission rather than waiting behind process-local timers from a dead owner.
func (s *PostgresStore) PrepareRecoveryWork(ctx context.Context, providerID string) error {
	if err := s.requireProvider(providerID); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("begin runner recovery work preparation: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.lockControllerEpochTx(ctx, tx); err != nil {
		return err
	}
	if err := convergeInterruptedVerificationsTx(ctx, tx, providerID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE runner_operations
		SET state=CASE
		      WHEN kind='destroy' AND state='running' THEN 'cleanup_required'
		      WHEN kind='create' AND state='running' THEN 'pending'
		      ELSE state
		    END,
		    lease_token=NULL,lease_owner=NULL,lease_expires_at=NULL,
		    next_attempt_at=CASE WHEN kind IN ('create','destroy') THEN NULL ELSE next_attempt_at END,
		    updated_at=clock_timestamp()
		WHERE provider_id=$1 AND kind IN ('create','destroy')
		  AND state IN ('pending','running','cleanup_required')`, providerID); err != nil {
		return fmt.Errorf("release previous controller operation leases: %w", err)
	}
	return commitLifecycleTx(ctx, tx, "runner recovery work preparation")
}

type interruptedVerify struct {
	operationID       string
	idempotencyKey    string
	allocationID      string
	sessionID         string
	generation        int64
	sessionState      string
	sessionDesired    string
	allocationDesired string
}

// convergeInterruptedVerificationsTx turns a controller-lost provider call
// into one safe terminal infrastructure result. The verifier is not restarted
// speculatively during boot; the browser may start a new operation after the
// ready snapshot is published.
func convergeInterruptedVerificationsTx(ctx context.Context, tx pgx.Tx, providerID string) error {
	rows, err := tx.Query(ctx, `
		SELECT o.id,o.idempotency_key,a.id,a.session_id,a.generation,
		       s.state,s.desired_state,a.desired_state
		FROM runner_operations o
		JOIN runner_allocations a ON a.id=o.allocation_id AND a.provider_id=o.provider_id
		JOIN sessions s ON s.id=a.session_id
		WHERE o.provider_id=$1 AND o.kind='verify' AND o.state='running'
		ORDER BY o.created_at,o.id
		FOR UPDATE OF o,a,s`, providerID)
	if err != nil {
		return fmt.Errorf("lock interrupted verify operations: %w", err)
	}
	var interrupted []interruptedVerify
	for rows.Next() {
		var item interruptedVerify
		if err := rows.Scan(
			&item.operationID, &item.idempotencyKey, &item.allocationID, &item.sessionID,
			&item.generation, &item.sessionState, &item.sessionDesired, &item.allocationDesired,
		); err != nil {
			rows.Close()
			return fmt.Errorf("scan interrupted verify operation: %w", err)
		}
		interrupted = append(interrupted, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("iterate interrupted verify operations: %w", err)
	}
	rows.Close()

	runningByAllocation := make(map[string]string, len(interrupted))
	for _, item := range interrupted {
		if existing, ok := runningByAllocation[item.allocationID]; ok {
			return fmt.Errorf("%w: allocation %s has multiple running verify operations %s and %s",
				ErrLifecycleConflict, item.allocationID, existing, item.operationID)
		}
		runningByAllocation[item.allocationID] = item.operationID
	}

	result := verifyResultMetadata{InfrastructureError: true, Code: "controller_restarted"}
	payload, err := boundedEventPayload(result)
	if err != nil {
		return err
	}
	for _, item := range interrupted {
		if item.generation <= 0 || item.sessionState != "verifying" || item.sessionDesired != "active" || item.allocationDesired != "active" {
			return fmt.Errorf("%w: interrupted verify %s has contradictory lifecycle %s/%s/%s",
				ErrLifecycleConflict, item.operationID, item.sessionState, item.sessionDesired, item.allocationDesired)
		}
		if len(item.idempotencyKey)+len(":finished") > 255 {
			return fmt.Errorf("%w: interrupted verify event key exceeds bound", ErrLifecycleConflict)
		}
		tag, err := tx.Exec(ctx, `
			UPDATE runner_operations
			SET state='failed',completed_at=NOW(),error_code='controller_restarted',error_message=NULL,
			    result_metadata=$2::jsonb,updated_at=NOW()
			WHERE id=$1 AND state='running'`, item.operationID, string(payload))
		if err != nil {
			return fmt.Errorf("terminalize interrupted verify operation: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("%w: interrupted verify lost transition authority", ErrLifecycleConflict)
		}
		tag, err = tx.Exec(ctx, `
			UPDATE sessions SET state='ready',updated_at=NOW(),lock_version=lock_version+1
			WHERE id=$1 AND current_generation=$2 AND state='verifying' AND desired_state='active'`,
			item.sessionID, item.generation)
		if err != nil {
			return fmt.Errorf("restore session after interrupted verify: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("%w: interrupted verify session lost transition authority", ErrLifecycleConflict)
		}
		if _, err := ensureKeyedEventTx(ctx, tx, item.allocationID, item.idempotencyKey+":finished",
			"verify_finished", "controller_restarted", "Verification was interrupted by a controller restart.", payload); err != nil {
			return err
		}
	}
	return nil
}

// ClaimDestroyWork leases the oldest eligible destroy operation for this
// provider. PostgreSQL is the work authority; process-local wakeups are only
// an optimization. An expired running claim is safe to reclaim because
// DestroySession is required to be idempotent and absence is success.
func (s *PostgresStore) ClaimDestroyWork(ctx context.Context, providerID, allocationID, owner string, lease time.Duration) (DestroyWorkClaim, bool, error) {
	if err := s.requireProvider(providerID); err != nil {
		return DestroyWorkClaim{}, false, err
	}
	if owner == "" || len(owner) > 128 {
		return DestroyWorkClaim{}, false, errors.New("destroy work owner is invalid")
	}
	if len(allocationID) > 128 {
		return DestroyWorkClaim{}, false, errors.New("destroy work allocation id is invalid")
	}
	if lease < minDestroyLease || lease > maxDestroyLease {
		return DestroyWorkClaim{}, false, errors.New("destroy work lease is outside the allowed bound")
	}

	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return DestroyWorkClaim{}, false, fmt.Errorf("begin destroy work claim: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.lockControllerEpochTx(ctx, tx); err != nil {
		return DestroyWorkClaim{}, false, err
	}

	var claim DestroyWorkClaim
	var generation int64
	err = tx.QueryRow(ctx, `
		WITH candidate AS (
			SELECT o.id
			FROM runner_operations o
			JOIN runner_allocations a
			  ON a.id=o.allocation_id AND a.provider_id=o.provider_id
			WHERE o.provider_id=$1
			  AND o.kind='destroy'
			  AND ($4='' OR a.id=$4::uuid)
			  AND o.state IN ('pending','running','cleanup_required')
			  AND (o.next_attempt_at IS NULL OR o.next_attempt_at<=clock_timestamp())
			  AND (o.lease_expires_at IS NULL OR o.lease_expires_at<=clock_timestamp())
			  AND a.desired_state='absent'
			ORDER BY COALESCE(o.next_attempt_at,o.created_at),o.created_at,o.id
			FOR UPDATE OF o SKIP LOCKED
			LIMIT 1
		), claimed AS (
			UPDATE runner_operations o
			SET state='running',lease_token=gen_random_uuid(),lease_owner=$2,
			    lease_expires_at=clock_timestamp()+($3::bigint*INTERVAL '1 millisecond'),
			    attempt_count=o.attempt_count+1,started_at=COALESCE(o.started_at,clock_timestamp()),
			    next_attempt_at=NULL,error_code=NULL,error_message=NULL,updated_at=clock_timestamp()
			FROM candidate c
			WHERE o.id=c.id
			RETURNING o.id,o.allocation_id,o.provider_id,o.lease_token,o.lease_owner,o.attempt_count,o.lease_expires_at
		)
		SELECT c.id,c.lease_token::text,c.lease_owner,c.attempt_count,c.lease_expires_at,
		       a.id,a.session_id,a.generation,a.provider_kind
		FROM claimed c
		JOIN runner_allocations a
		  ON a.id=c.allocation_id AND a.provider_id=c.provider_id`,
		providerID, owner, lease.Milliseconds(), allocationID,
	).Scan(
		&claim.OperationID, &claim.LeaseToken, &claim.LeaseOwner, &claim.Attempt, &claim.LeaseExpiresAt,
		&claim.Ref.ID, &claim.Ref.Session.SessionID, &generation, &claim.Ref.Provider,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		// An exact caller needs to distinguish completed replay from work
		// currently leased by another worker (or delayed by backoff). Do not
		// expose completed rows to the unscoped background scan.
		if allocationID != "" {
			var operationID string
			var ref AllocationRef
			var generation int64
			var operationState, desired, observed string
			replayErr := tx.QueryRow(ctx, `
				SELECT o.id,o.state,a.id,a.session_id,a.generation,a.provider_kind,
				       a.desired_state,a.observed_state
				FROM runner_operations o
				JOIN runner_allocations a
				  ON a.id=o.allocation_id AND a.provider_id=o.provider_id
				WHERE o.provider_id=$1 AND o.allocation_id=$2::uuid AND o.kind='destroy'`,
				providerID, allocationID,
			).Scan(&operationID, &operationState, &ref.ID, &ref.Session.SessionID,
				&generation, &ref.Provider, &desired, &observed)
			if replayErr != nil && !errors.Is(replayErr, pgx.ErrNoRows) {
				return DestroyWorkClaim{}, false, fmt.Errorf("load destroy work replay: %w", replayErr)
			}
			if replayErr == nil && generation > 0 && operationState == "succeeded" && desired == "absent" && observed == "absent" {
				ref.Session.Generation = uint64(generation)
				if err := tx.Commit(ctx); err != nil {
					return DestroyWorkClaim{}, false, fmt.Errorf("commit completed destroy work replay: %w", err)
				}
				return DestroyWorkClaim{OperationID: operationID, Ref: ref, Completed: true}, true, nil
			}
			if replayErr == nil && generation > 0 && desired == "absent" && observed != "absent" &&
				(operationState == "pending" || operationState == "running" || operationState == "cleanup_required") {
				ref.Session.Generation = uint64(generation)
				if err := tx.Commit(ctx); err != nil {
					return DestroyWorkClaim{}, false, fmt.Errorf("commit pending destroy work observation: %w", err)
				}
				return DestroyWorkClaim{OperationID: operationID, Ref: ref, Pending: true}, true, nil
			}
		}
		if err := tx.Commit(ctx); err != nil {
			return DestroyWorkClaim{}, false, fmt.Errorf("commit empty destroy work claim: %w", err)
		}
		return DestroyWorkClaim{}, false, nil
	}
	if err != nil {
		return DestroyWorkClaim{}, false, fmt.Errorf("claim durable destroy operation: %w", err)
	}
	if generation <= 0 {
		return DestroyWorkClaim{}, false, errors.New("claimed destroy operation has invalid generation")
	}
	claim.Ref.Session.Generation = uint64(generation)
	if err := tx.Commit(ctx); err != nil {
		return DestroyWorkClaim{}, false, fmt.Errorf("commit destroy work claim: %w", err)
	}
	return claim, true, nil
}

// RetryDestroyWork releases only the caller's current claim and schedules a
// bounded retry. A late worker cannot overwrite a successor claim.
func (s *PostgresStore) RetryDestroyWork(ctx context.Context, claim DestroyWorkClaim, cause error, retryAfter time.Duration) error {
	if err := validateDestroyClaim(claim); err != nil {
		return err
	}
	if cause == nil {
		return errors.New("destroy work retry requires a cause")
	}
	if retryAfter < 0 || retryAfter > maxDestroyRetry {
		return errors.New("destroy work retry delay is outside the allowed bound")
	}

	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("begin destroy work retry: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.lockControllerEpochTx(ctx, tx); err != nil {
		return err
	}
	providerID, err := lockExactAllocation(ctx, tx, claim.Ref)
	if err != nil {
		return err
	}
	if err := s.requireProvider(providerID); err != nil {
		return err
	}

	tag, err := tx.Exec(ctx, `
		UPDATE runner_operations
		SET state='cleanup_required',error_code='provider_destroy_failed',error_message=$4,
		    next_attempt_at=clock_timestamp()+($5::bigint*INTERVAL '1 millisecond'),
		    lease_token=NULL,lease_owner=NULL,lease_expires_at=NULL,updated_at=clock_timestamp()
		WHERE id=$1 AND allocation_id=$2 AND provider_id=$3 AND kind='destroy'
		  AND state='running' AND lease_token=$6::uuid AND lease_expires_at>clock_timestamp()`,
		claim.OperationID, claim.Ref.ID, providerID, boundedDurableMessage(cause.Error()),
		retryAfter.Milliseconds(), claim.LeaseToken,
	)
	if err != nil {
		return fmt.Errorf("reschedule durable destroy operation: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("%w: destroy work claim is no longer current", ErrLifecycleConflict)
	}
	if _, err := ensureEventTx(ctx, tx, claim.Ref.ID, "cleanup_required", "provider_destroy_failed", "Environment cleanup will be retried."); err != nil {
		return err
	}
	return commitLifecycleTx(ctx, tx, "destroy work retry")
}

// MarkClaimedDestroyed records provider absence only while the supplied lease
// token is still current. Provider deletion may have succeeded after a lease
// expired; in that case the next idempotent claimant records absence.
func (s *PostgresStore) MarkClaimedDestroyed(ctx context.Context, claim DestroyWorkClaim) error {
	if err := validateDestroyClaim(claim); err != nil {
		return err
	}
	return s.markDestroyed(ctx, claim.Ref, &claim)
}

// FindProvisionableReplacement returns the immediate reset replacement only
// after every older allocation is durably proven absent. It is a read path;
// CreateSession idempotency plus the service's per-user lock serialize the
// provider mutation.
func (s *PostgresStore) FindProvisionableReplacement(ctx context.Context, destroyed AllocationRef) (SessionReservation, bool, error) {
	if destroyed.ID == "" || destroyed.Session.SessionID == "" || destroyed.Session.Generation == 0 || destroyed.Provider == "" {
		return SessionReservation{}, false, ErrAllocationNotFound
	}
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadOnly})
	if err != nil {
		return SessionReservation{}, false, fmt.Errorf("begin replacement lookup: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var providerID, createKey string
	var oldGeneration, currentGeneration int64
	err = tx.QueryRow(ctx, `
		SELECT old.provider_id,old.generation,s.current_generation,create_op.idempotency_key
		FROM runner_allocations old
		JOIN sessions s ON s.id=old.session_id
		JOIN runner_allocations next
		  ON next.session_id=old.session_id AND next.generation=old.generation+1
		JOIN runner_operations create_op
		  ON create_op.allocation_id=next.id AND create_op.provider_id=next.provider_id
		 AND create_op.kind='create'
		WHERE old.id=$1 AND old.session_id=$2 AND old.generation=$3 AND old.provider_kind=$4
		  AND old.desired_state='absent' AND old.observed_state='absent'
		  AND s.current_generation=old.generation+1 AND s.desired_state='active'
		  AND next.provider_id=old.provider_id AND next.desired_state='active'
		  AND next.observed_state IN ('unknown','provisioning')
		  AND create_op.state='pending'
		  AND NOT EXISTS (
			SELECT 1 FROM runner_allocations prior
			WHERE prior.session_id=old.session_id AND prior.generation<next.generation
			  AND (prior.desired_state<>'absent' OR prior.observed_state<>'absent')
		  )`,
		destroyed.ID, destroyed.Session.SessionID, int64(destroyed.Session.Generation), destroyed.Provider,
	).Scan(&providerID, &oldGeneration, &currentGeneration, &createKey)
	if errors.Is(err, pgx.ErrNoRows) {
		if err := tx.Commit(ctx); err != nil {
			return SessionReservation{}, false, fmt.Errorf("commit empty replacement lookup: %w", err)
		}
		return SessionReservation{}, false, nil
	}
	if err != nil {
		return SessionReservation{}, false, fmt.Errorf("find reset replacement: %w", err)
	}
	if err := s.requireProvider(providerID); err != nil {
		return SessionReservation{}, false, err
	}
	if oldGeneration != int64(destroyed.Session.Generation) || currentGeneration != oldGeneration+1 {
		return SessionReservation{}, false, ErrGenerationStale
	}
	replacement, found, err := loadReservationByOperation(ctx, tx, providerID, createKey)
	if err != nil {
		return SessionReservation{}, false, err
	}
	if !found {
		return SessionReservation{}, false, fmt.Errorf("%w: replacement reservation disappeared", ErrLifecycleConflict)
	}
	if err := tx.Commit(ctx); err != nil {
		return SessionReservation{}, false, fmt.Errorf("commit replacement lookup: %w", err)
	}
	return replacement, true, nil
}

// ClaimProvisionableCreate leases a current-generation create whose complete
// older-generation chain is absent. Generation one uses the same durable work
// protocol as reset replacements, closing the reservation-to-provider crash
// window instead of relying on the original HTTP request to survive.
func (s *PostgresStore) ClaimProvisionableCreate(ctx context.Context, providerID, operationID, owner string, lease time.Duration) (CreateWorkClaim, bool, error) {
	return s.claimProvisionableCreate(ctx, providerID, operationID, owner, lease, false)
}

func (s *PostgresStore) claimProvisionableCreate(ctx context.Context, providerID, operationID, owner string, lease time.Duration, resetOnly bool) (CreateWorkClaim, bool, error) {
	if err := s.requireProvider(providerID); err != nil {
		return CreateWorkClaim{}, false, err
	}
	if owner == "" || len(owner) > 128 {
		return CreateWorkClaim{}, false, errors.New("create work owner is invalid")
	}
	if len(operationID) > 128 {
		return CreateWorkClaim{}, false, errors.New("create work operation id is invalid")
	}
	if lease < minDestroyLease || lease > maxDestroyLease {
		return CreateWorkClaim{}, false, errors.New("create work lease is outside the allowed bound")
	}

	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return CreateWorkClaim{}, false, fmt.Errorf("begin create work claim: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.lockControllerEpochTx(ctx, tx); err != nil {
		return CreateWorkClaim{}, false, err
	}

	var claim CreateWorkClaim
	var key string
	minimumGeneration := int64(0)
	if resetOnly {
		minimumGeneration = 1
	}
	err = tx.QueryRow(ctx, `
		WITH candidate AS (
			SELECT o.id
			FROM runner_operations o
			JOIN runner_allocations a
			  ON a.id=o.allocation_id AND a.provider_id=o.provider_id
			JOIN sessions s ON s.id=a.session_id
			WHERE o.provider_id=$1 AND o.kind='create'
			  AND ($4='' OR o.id=$4::uuid)
			  AND a.generation>$5
			  AND o.state IN ('pending','running')
			  AND (o.lease_expires_at IS NULL OR o.lease_expires_at<=clock_timestamp())
			  AND a.desired_state='active'
			  AND a.observed_state IN ('unknown','provisioning')
			  AND s.current_generation=a.generation AND s.desired_state='active'
			  AND NOT EXISTS (
				SELECT 1 FROM runner_allocations prior
				WHERE prior.session_id=a.session_id AND prior.generation<a.generation
				  AND (prior.desired_state<>'absent' OR prior.observed_state<>'absent')
			  )
			ORDER BY o.created_at,o.id
			FOR UPDATE OF o SKIP LOCKED
			LIMIT 1
		), claimed AS (
			UPDATE runner_operations o
			SET state='running',lease_token=gen_random_uuid(),lease_owner=$2,
			    lease_expires_at=clock_timestamp()+($3::bigint*INTERVAL '1 millisecond'),
			    attempt_count=o.attempt_count+1,started_at=COALESCE(o.started_at,clock_timestamp()),
			    next_attempt_at=NULL,error_code=NULL,error_message=NULL,updated_at=clock_timestamp()
			FROM candidate c
			WHERE o.id=c.id
			RETURNING o.id,o.idempotency_key,o.lease_token,o.lease_owner,o.attempt_count,o.lease_expires_at
		)
		SELECT id,idempotency_key,lease_token::text,lease_owner,attempt_count,lease_expires_at FROM claimed`,
		providerID, owner, lease.Milliseconds(), operationID, minimumGeneration,
	).Scan(&claim.OperationID, &key, &claim.LeaseToken, &claim.LeaseOwner, &claim.Attempt, &claim.LeaseExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		if err := tx.Commit(ctx); err != nil {
			return CreateWorkClaim{}, false, fmt.Errorf("commit empty reset create work claim: %w", err)
		}
		return CreateWorkClaim{}, false, nil
	}
	if err != nil {
		return CreateWorkClaim{}, false, fmt.Errorf("claim durable create: %w", err)
	}
	reservation, found, err := loadReservationByOperation(ctx, tx, providerID, key)
	if err != nil {
		return CreateWorkClaim{}, false, err
	}
	if !found || reservation.Operation.ID != claim.OperationID {
		return CreateWorkClaim{}, false, fmt.Errorf("%w: claimed create reservation is missing", ErrLifecycleConflict)
	}
	claim.Reservation = reservation
	if err := tx.Commit(ctx); err != nil {
		return CreateWorkClaim{}, false, fmt.Errorf("commit reset create work claim: %w", err)
	}
	return claim, true, nil
}

// ClaimProvisionableReset is retained as a compatibility alias for callers
// that intentionally scan only reset work. New lifecycle code uses the
// generation-neutral ClaimProvisionableCreate operation.
func (s *PostgresStore) ClaimProvisionableReset(ctx context.Context, providerID, operationID, owner string, lease time.Duration) (CreateWorkClaim, bool, error) {
	return s.claimProvisionableCreate(ctx, providerID, operationID, owner, lease, true)
}

func validateDestroyClaim(claim DestroyWorkClaim) error {
	if claim.Pending {
		return errors.New("pending destroy work observation is not a mutation claim")
	}
	if claim.Completed {
		if claim.OperationID == "" || claim.Ref.ID == "" || claim.Ref.Session.SessionID == "" ||
			claim.Ref.Session.Generation == 0 || claim.Ref.Provider == "" {
			return errors.New("completed destroy work replay is invalid")
		}
		return nil
	}
	if claim.OperationID == "" || claim.LeaseToken == "" || claim.LeaseOwner == "" || claim.LeaseExpiresAt.IsZero() ||
		claim.Ref.ID == "" || claim.Ref.Session.SessionID == "" ||
		claim.Ref.Session.Generation == 0 || claim.Ref.Provider == "" || claim.Attempt <= 0 {
		return errors.New("destroy work claim is invalid")
	}
	return nil
}

func validateCreateClaim(claim CreateWorkClaim) error {
	if claim.OperationID == "" || claim.LeaseToken == "" || claim.LeaseOwner == "" || claim.LeaseExpiresAt.IsZero() || claim.Attempt <= 0 ||
		claim.Reservation.Operation.ID != claim.OperationID ||
		claim.Reservation.Allocation.Ref.ID == "" ||
		claim.Reservation.Allocation.Ref.Session.SessionID == "" ||
		claim.Reservation.Allocation.Ref.Session.Generation == 0 ||
		claim.Reservation.Allocation.Ref.Provider == "" {
		return errors.New("create work claim is invalid")
	}
	return nil
}
