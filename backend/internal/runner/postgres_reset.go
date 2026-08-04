package runner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

type ReserveResetParams struct {
	Expected       SessionRef
	UserID         string
	Provider       ProviderKind
	ProviderID     string
	IdempotencyKey string
}

type ResetReservation struct {
	Old AllocationRef
	New SessionReservation
}

// ReserveReset atomically records old-generation cleanup and reserves the
// replacement generation. Provider calls happen only after this transaction
// commits, and replacement create must wait until Old is proven absent.
func (s *PostgresStore) ReserveReset(ctx context.Context, params ReserveResetParams) (ResetReservation, error) {
	if err := s.requireProvider(params.ProviderID); err != nil {
		return ResetReservation{}, err
	}
	if err := validateResetReservation(params); err != nil {
		return ResetReservation{}, err
	}
	nextSession := SessionRef{SessionID: params.Expected.SessionID, Generation: params.Expected.Generation + 1}
	nextAllocationID := AllocationIDForSession(nextSession)

	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return ResetReservation{}, fmt.Errorf("begin runner reset reservation: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.lockControllerEpochTx(ctx, tx); err != nil {
		return ResetReservation{}, err
	}

	var lockedUser string
	if err := tx.QueryRow(ctx, `SELECT id FROM users WHERE id=$1 FOR UPDATE`, params.UserID).Scan(&lockedUser); err != nil {
		return ResetReservation{}, fmt.Errorf("lock runner reset user: %w", err)
	}

	if replay, found, err := loadReservationByOperation(ctx, tx, params.ProviderID, params.IdempotencyKey); err != nil {
		return ResetReservation{}, err
	} else if found {
		requestHash, hashErr := hashResetReservation(params, nextAllocationID, replay.Session.Selection)
		if hashErr != nil {
			return ResetReservation{}, fmt.Errorf("hash runner reset replay: %w", hashErr)
		}
		if replay.Session.ID != params.Expected.SessionID || replay.Allocation.Ref.ID != nextAllocationID ||
			replay.Allocation.Ref.Session != nextSession || replay.Operation.Kind != "create" ||
			!bytes.Equal(replay.Operation.RequestHash[:], requestHash[:]) {
			return ResetReservation{}, ErrIdempotencyConflict
		}
		if err := tx.Commit(ctx); err != nil {
			return ResetReservation{}, fmt.Errorf("commit runner reset replay: %w", err)
		}
		return ResetReservation{
			Old: AllocationRef{ID: AllocationIDForSession(params.Expected), Session: params.Expected, Provider: params.Provider},
			New: replay,
		}, nil
	}

	var selection CatalogSelection
	var catalogGeneration *int64
	var currentGeneration int64
	var sessionState, sessionDesired string
	var queuedAt, expiresAt time.Time
	if err := tx.QueryRow(ctx, `
		SELECT problem_id,problem_revision,catalog_generation,current_generation,state,desired_state,queued_at,expires_at
		FROM sessions WHERE id=$1 AND user_id=$2 FOR UPDATE`,
		params.Expected.SessionID, params.UserID,
	).Scan(&selection.Problem.ID, &selection.Problem.Revision, &catalogGeneration, &currentGeneration, &sessionState, &sessionDesired, &queuedAt, &expiresAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ResetReservation{}, ErrAllocationNotFound
		}
		return ResetReservation{}, fmt.Errorf("lock durable session reset: %w", err)
	}
	if catalogGeneration == nil || *catalogGeneration <= 0 || selection.Problem.ID == "" || selection.Problem.Revision == "" {
		return ResetReservation{}, fmt.Errorf("%w: reset source has no verified catalog selection", ErrLifecycleConflict)
	}
	selection.Generation = uint64(*catalogGeneration)
	requestHash, err := hashResetReservation(params, nextAllocationID, selection)
	if err != nil {
		return ResetReservation{}, fmt.Errorf("hash runner reset reservation: %w", err)
	}
	if currentGeneration != int64(params.Expected.Generation) || sessionDesired != "active" {
		return ResetReservation{}, ErrGenerationStale
	}
	if sessionState != "ready" {
		return ResetReservation{}, fmt.Errorf("%w: reset source session is %s", ErrLifecycleConflict, sessionState)
	}
	var databaseNow time.Time
	if err := tx.QueryRow(ctx, `SELECT NOW()`).Scan(&databaseNow); err != nil {
		return ResetReservation{}, fmt.Errorf("read reset database clock: %w", err)
	}
	if !expiresAt.After(databaseNow) {
		return ResetReservation{}, ErrSessionExpired
	}

	oldRef := AllocationRef{ID: AllocationIDForSession(params.Expected), Session: params.Expected, Provider: params.Provider}
	providerID, err := lockExactAllocation(ctx, tx, oldRef)
	if err != nil {
		return ResetReservation{}, err
	}
	if providerID != params.ProviderID {
		return ResetReservation{}, fmt.Errorf("%w: reset provider id changed", ErrLifecycleConflict)
	}
	var resourceProfile, oldDesired, oldObserved string
	if err := tx.QueryRow(ctx, `SELECT resource_profile,desired_state,observed_state FROM runner_allocations WHERE id=$1`, oldRef.ID).
		Scan(&resourceProfile, &oldDesired, &oldObserved); err != nil {
		return ResetReservation{}, fmt.Errorf("read durable reset source: %w", err)
	}
	if oldDesired != "active" {
		return ResetReservation{}, fmt.Errorf("%w: reset source is already desired absent", ErrLifecycleConflict)
	}
	if oldObserved != "running" {
		return ResetReservation{}, fmt.Errorf("%w: reset source allocation is %s", ErrLifecycleConflict, oldObserved)
	}
	var sourceCreateState string
	if err := tx.QueryRow(ctx, `
		SELECT state FROM runner_operations
		WHERE allocation_id=$1 AND provider_id=$2 AND kind='create' FOR UPDATE`,
		oldRef.ID, providerID,
	).Scan(&sourceCreateState); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ResetReservation{}, fmt.Errorf("%w: reset source create operation is missing", ErrLifecycleConflict)
		}
		return ResetReservation{}, fmt.Errorf("lock reset source create operation: %w", err)
	}
	if sourceCreateState != "succeeded" {
		return ResetReservation{}, fmt.Errorf("%w: reset source create operation is %s", ErrLifecycleConflict, sourceCreateState)
	}
	var olderNotAbsent int
	if err := tx.QueryRow(ctx, `
		SELECT COUNT(*) FROM runner_allocations
		WHERE session_id=$1 AND generation<$2
		  AND (desired_state<>'absent' OR observed_state<>'absent')`,
		params.Expected.SessionID, int64(params.Expected.Generation),
	).Scan(&olderNotAbsent); err != nil {
		return ResetReservation{}, fmt.Errorf("check older reset allocations: %w", err)
	}
	if olderNotAbsent != 0 {
		return ResetReservation{}, fmt.Errorf("%w: %d older allocation(s) are not absent", ErrLifecycleConflict, olderNotAbsent)
	}

	oldAttemptStatus, err := attemptStatusTx(ctx, tx, params.Expected)
	if err != nil {
		return ResetReservation{}, err
	}
	if oldAttemptStatus != "in_progress" {
		return ResetReservation{}, fmt.Errorf("%w: reset source attempt is already %s", ErrLifecycleConflict, oldAttemptStatus)
	}
	if tag, err := tx.Exec(ctx, `
		UPDATE attempts
		SET status='failed',finished_at=NOW(),
		    duration_seconds=GREATEST(0,EXTRACT(EPOCH FROM (NOW()-started_at))::int),
		    verify_log=CASE WHEN verify_log='' THEN 'environment reset by user' ELSE verify_log END
		WHERE session_id=$1 AND generation=$2 AND status='in_progress'`,
		params.Expected.SessionID, int64(params.Expected.Generation)); err != nil {
		return ResetReservation{}, fmt.Errorf("terminalize reset source attempt: %w", err)
	} else if tag.RowsAffected() != 1 {
		return ResetReservation{}, fmt.Errorf("%w: reset source attempt lost transition authority", ErrLifecycleConflict)
	}
	if tag, err := tx.Exec(ctx, `
		UPDATE runner_allocations
		SET desired_state='absent',
		    observed_state=CASE WHEN observed_state='absent' THEN 'absent' ELSE 'deleting' END,
		    updated_at=NOW(),lock_version=lock_version+1
		WHERE id=$1 AND desired_state='active'`, oldRef.ID); err != nil {
		return ResetReservation{}, fmt.Errorf("record reset source desired absent: %w", err)
	} else if tag.RowsAffected() != 1 {
		return ResetReservation{}, fmt.Errorf("%w: reset source lost transition authority", ErrLifecycleConflict)
	}
	if _, err := ensureDestroyOperationTx(ctx, tx, oldRef.ID, providerID); err != nil {
		return ResetReservation{}, err
	}
	if _, err := ensureEventTx(ctx, tx, oldRef.ID, "reset_requested", "user_reset", "Replacement environment requested."); err != nil {
		return ResetReservation{}, err
	}
	if _, err := ensureEventTx(ctx, tx, oldRef.ID, "destroying", "reset", "Previous environment cleanup started."); err != nil {
		return ResetReservation{}, err
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO runner_allocations (
			id,session_id,generation,provider_kind,provider_id,resource_profile,
			desired_state,observed_state,expires_at,last_event_sequence
		) VALUES ($1,$2,$3,$4,$5,$6,'active','unknown',$7,2)`,
		nextAllocationID, params.Expected.SessionID, int64(nextSession.Generation), params.Provider,
		params.ProviderID, resourceProfile, expiresAt,
	); err != nil {
		return ResetReservation{}, mapReservationError(err)
	}
	var operationID string
	if err := tx.QueryRow(ctx, `
		INSERT INTO runner_operations (
			allocation_id,provider_id,kind,idempotency_key,request_hash,state
		) VALUES ($1,$2,'create',$3,$4,'pending') RETURNING id`,
		nextAllocationID, params.ProviderID, params.IdempotencyKey, requestHash[:],
	).Scan(&operationID); err != nil {
		return ResetReservation{}, mapReservationError(err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO session_events (
			allocation_id,sequence,event_type,reason_code,message,sanitized_payload,created_at
		) VALUES
			($1,1,'allocation_reserved','reset_replacement','Replacement environment reserved.','{}'::jsonb,$2),
			($1,2,'reset_requested','user_reset','Replacement environment requested.','{}'::jsonb,$2)`,
		nextAllocationID, databaseNow,
	); err != nil {
		return ResetReservation{}, fmt.Errorf("append reset replacement events: %w", err)
	}
	if tag, err := tx.Exec(ctx, `
		UPDATE sessions
		SET current_generation=$2,state='queued',desired_state='active',
		    started_at=NULL,finished_at=NULL,updated_at=NOW(),lock_version=lock_version+1
		WHERE id=$1 AND current_generation=$3 AND desired_state='active'`,
		params.Expected.SessionID, int64(nextSession.Generation), int64(params.Expected.Generation),
	); err != nil {
		return ResetReservation{}, fmt.Errorf("advance durable reset generation: %w", err)
	} else if tag.RowsAffected() != 1 {
		return ResetReservation{}, ErrGenerationStale
	}
	var attemptID string
	if err := tx.QueryRow(ctx, `
		INSERT INTO attempts (user_id,problem_id,status,started_at,session_id,generation)
		VALUES ($1,$2,'in_progress',$3,$4,$5) RETURNING id`,
		params.UserID, selection.Problem.ID, databaseNow, params.Expected.SessionID, int64(nextSession.Generation),
	).Scan(&attemptID); err != nil {
		return ResetReservation{}, mapReservationError(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return ResetReservation{}, mapReservationError(err)
	}

	newReservation := SessionReservation{
		Session: DurableSession{
			ID: params.Expected.SessionID, UserID: params.UserID, Selection: selection,
			CurrentGeneration: nextSession.Generation, State: "queued", DesiredState: "active",
			QueuedAt: queuedAt, ExpiresAt: expiresAt,
		},
		Allocation: DurableAllocation{
			Ref:        AllocationRef{ID: nextAllocationID, Session: nextSession, Provider: params.Provider},
			ProviderID: params.ProviderID, ResourceProfile: resourceProfile,
			DesiredState: "active", ObservedState: "unknown", ExpiresAt: expiresAt,
			LastEventSequence: 2,
		},
		Operation: DurableOperation{
			ID: operationID, AllocationID: nextAllocationID, ProviderID: params.ProviderID,
			Kind: "create", IdempotencyKey: params.IdempotencyKey, RequestHash: requestHash, State: "pending",
		},
		Event: DurableEvent{
			AllocationID: nextAllocationID, Sequence: 1, Type: "allocation_reserved",
			ReasonCode: "reset_replacement", Message: "Replacement environment reserved.",
			Payload: json.RawMessage(`{}`), CreatedAt: databaseNow,
		},
		AttemptID:     attemptID,
		AttemptStatus: "in_progress",
	}
	return ResetReservation{Old: oldRef, New: newReservation}, nil
}

func validateResetReservation(params ReserveResetParams) error {
	switch {
	case params.Expected.SessionID == "" || params.Expected.Generation == 0 || params.Expected.Generation >= MaxDurableValue:
		return errors.New("runner reset expected session is invalid")
	case params.UserID == "":
		return errors.New("runner reset user id is required")
	case params.Provider == "":
		return errors.New("runner reset provider kind is required")
	case params.ProviderID == "" || len(params.ProviderID) > 128:
		return errors.New("runner reset provider id is invalid")
	case ValidateProviderOperationKey(params.IdempotencyKey) != nil:
		return errors.New("runner reset idempotency key is invalid")
	default:
		return nil
	}
}

func hashResetReservation(params ReserveResetParams, nextAllocationID string, selection CatalogSelection) ([sha256.Size]byte, error) {
	payload, err := json.Marshal(struct {
		Schema           string           `json:"schema"`
		SessionID        string           `json:"session_id"`
		SourceGeneration uint64           `json:"source_generation"`
		NextAllocationID string           `json:"next_allocation_id"`
		UserID           string           `json:"user_id"`
		Selection        CatalogSelection `json:"selection"`
		Provider         ProviderKind     `json:"provider"`
		ProviderID       string           `json:"provider_id"`
	}{
		Schema: "k8s-quiz.runner-reset/v2", SessionID: params.Expected.SessionID,
		SourceGeneration: params.Expected.Generation, NextAllocationID: nextAllocationID,
		UserID: params.UserID, Selection: selection, Provider: params.Provider, ProviderID: params.ProviderID,
	})
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	return sha256.Sum256(payload), nil
}
