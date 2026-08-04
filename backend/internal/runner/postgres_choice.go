package runner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

// LookupChoice resolves an exact choice operation without creating or
// mutating it. It remains usable after successful grading made the session
// desired-absent, which lets an HTTP response lost after COMMIT replay the
// durable result without grading a newer session.
func (s *PostgresStore) LookupChoice(ctx context.Context, userID string, submission ChoiceSubmission) (VerifyDecision, bool, error) {
	return s.lookupChoice(ctx, userID, submission, true)
}

func (s *PostgresStore) lookupChoice(
	ctx context.Context,
	userID string,
	submission ChoiceSubmission,
	requireCurrent bool,
) (VerifyDecision, bool, error) {
	identityKey, err := verifyIdempotencyKey(userID, submission.OperationID)
	if err != nil {
		return VerifyDecision{}, false, err
	}
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return VerifyDecision{}, false, fmt.Errorf("begin durable choice lookup: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var lockedUser string
	if err := tx.QueryRow(ctx, `SELECT id FROM users WHERE id=$1 FOR UPDATE`, userID).Scan(&lockedUser); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return VerifyDecision{}, false, nil
		}
		return VerifyDecision{}, false, fmt.Errorf("lock durable choice user: %w", err)
	}

	var ref AllocationRef
	var storedProblemID, kind, state, errorCode string
	var storedHash, storedResult []byte
	err = tx.QueryRow(ctx, `
		SELECT a.id,a.session_id,a.generation,a.provider_kind,sess.problem_id,
		       o.kind,o.request_hash,o.state,o.result_metadata,COALESCE(o.error_code,'')
		FROM runner_operations o
		JOIN runner_allocations a ON a.id=o.allocation_id AND a.provider_id=o.provider_id
		JOIN sessions sess ON sess.id=a.session_id
		WHERE o.provider_id=$1 AND o.idempotency_key=$2 AND sess.user_id=$3`,
		s.fence.ProviderID, identityKey, userID,
	).Scan(&ref.ID, &ref.Session.SessionID, &ref.Session.Generation, &ref.Provider,
		&storedProblemID, &kind, &storedHash, &state, &storedResult, &errorCode)
	if errors.Is(err, pgx.ErrNoRows) {
		return VerifyDecision{}, false, nil
	}
	if err != nil {
		return VerifyDecision{}, false, fmt.Errorf("load durable choice operation: %w", err)
	}
	if ref.Session != submission.Session || storedProblemID != submission.ProblemID || kind != "verify" {
		return VerifyDecision{}, false, ErrIdempotencyConflict
	}
	identity, err := newChoiceIdentity(ref, userID, submission)
	if err != nil {
		return VerifyDecision{}, false, err
	}
	if !bytes.Equal(storedHash, identity.requestHash[:]) {
		return VerifyDecision{}, false, ErrIdempotencyConflict
	}
	decision, err := storedVerifyDecision(ref.Session, state, errorCode, storedResult)
	if err != nil {
		return VerifyDecision{}, false, err
	}
	if requireCurrent {
		if err := rejectChoiceReplayAfterSessionAdvance(ctx, tx, userID, ref.Session); err != nil {
			return VerifyDecision{}, false, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return VerifyDecision{}, false, fmt.Errorf("commit durable choice lookup: %w", err)
	}
	return decision, true, nil
}

// RecordChoice atomically admits and grades a choice submission. A fresh
// operation, verify_started/verify_finished events, attempt update, and (for a
// correct answer) desired-absent cleanup intent are one PostgreSQL transaction.
// Exact replays return the stored result; changed payloads conflict.
func (s *PostgresStore) RecordChoice(
	ctx context.Context,
	userID string,
	ref AllocationRef,
	submission ChoiceSubmission,
	success bool,
	verifyLog string,
) (VerifyDecision, error) {
	if submission.Session != ref.Session {
		return VerifyDecision{}, ErrIdempotencyConflict
	}
	identity, err := newChoiceIdentity(ref, userID, submission)
	if err != nil {
		return VerifyDecision{}, err
	}
	verifyLog = boundedDurableMessage(verifyLog)
	result := verifyResultMetadata{Success: &success, Log: &verifyLog}
	resultJSON, err := boundedEventPayload(result)
	if err != nil {
		return VerifyDecision{}, err
	}

	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return VerifyDecision{}, fmt.Errorf("begin choice grade transition: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.lockControllerEpochTx(ctx, tx); err != nil {
		return VerifyDecision{}, err
	}
	var lockedUser string
	if err := tx.QueryRow(ctx, `SELECT id FROM users WHERE id=$1 FOR UPDATE`, userID).Scan(&lockedUser); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return VerifyDecision{}, ErrIdempotencyConflict
		}
		return VerifyDecision{}, fmt.Errorf("lock durable choice user: %w", err)
	}

	var storedUserID, storedProblemID, sessionState, sessionDesired string
	var currentGeneration int64
	if err := tx.QueryRow(ctx, `
		SELECT user_id,problem_id,current_generation,state,desired_state
		FROM sessions WHERE id=$1 FOR UPDATE`, submission.Session.SessionID).
		Scan(&storedUserID, &storedProblemID, &currentGeneration, &sessionState, &sessionDesired); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return VerifyDecision{}, ErrGenerationStale
		}
		return VerifyDecision{}, fmt.Errorf("lock durable choice session: %w", err)
	}
	if storedUserID != userID || storedProblemID != submission.ProblemID {
		return VerifyDecision{}, ErrIdempotencyConflict
	}
	if currentGeneration != int64(submission.Session.Generation) {
		return VerifyDecision{}, ErrGenerationStale
	}

	var storedRef AllocationRef
	var providerID, allocationDesired, allocationObserved string
	if err := tx.QueryRow(ctx, `
		SELECT id,provider_kind,provider_id,desired_state,observed_state
		FROM runner_allocations
		WHERE session_id=$1 AND generation=$2 FOR UPDATE`,
		submission.Session.SessionID, int64(submission.Session.Generation)).
		Scan(&storedRef.ID, &storedRef.Provider, &providerID, &allocationDesired, &allocationObserved); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return VerifyDecision{}, ErrGenerationStale
		}
		return VerifyDecision{}, fmt.Errorf("lock durable choice allocation: %w", err)
	}
	storedRef.Session = submission.Session
	if storedRef != ref {
		return VerifyDecision{}, ErrIdempotencyConflict
	}
	if err := s.requireProvider(providerID); err != nil {
		return VerifyDecision{}, err
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
		if err := rejectChoiceReplayAfterSessionAdvance(ctx, tx, userID, ref.Session); err != nil {
			return VerifyDecision{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return s.reconcileChoiceCommit(userID, submission, decision, err)
		}
		return decision, nil
	}

	if sessionDesired != "active" || allocationDesired != "active" {
		return VerifyDecision{}, ErrGenerationStale
	}
	if sessionState != "ready" || allocationObserved != "running" {
		return VerifyDecision{}, fmt.Errorf("%w: session cannot grade choice from %s/%s", ErrLifecycleConflict, sessionState, allocationObserved)
	}
	if attemptState, err := attemptStatusTx(ctx, tx, submission.Session); err != nil {
		return VerifyDecision{}, err
	} else if attemptState != "in_progress" {
		return VerifyDecision{}, fmt.Errorf("%w: attempt is %s before choice grade", ErrLifecycleConflict, attemptState)
	}

	startedPayload, err := boundedEventPayload(struct {
		OperationID string `json:"operation_id"`
	}{OperationID: submission.OperationID})
	if err != nil {
		return VerifyDecision{}, err
	}
	if _, err := ensureKeyedEventTx(ctx, tx, ref.ID, identity.startedEventKey(), "verify_started", "choice_submitted", "Verification started.", startedPayload); err != nil {
		return VerifyDecision{}, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE runner_operations
		SET state='succeeded',completed_at=NOW(),error_code=NULL,error_message=NULL,
		    result_metadata=$3::jsonb,updated_at=NOW()
		WHERE provider_id=$1 AND idempotency_key=$2 AND state='running'`,
		providerID, identity.idempotencyKey, string(resultJSON)); err != nil {
		return VerifyDecision{}, fmt.Errorf("complete durable choice operation: %w", err)
	}

	if success {
		if _, err := tx.Exec(ctx, `
			UPDATE attempts
			SET status='success',finished_at=NOW(),
			    duration_seconds=GREATEST(0,EXTRACT(EPOCH FROM (NOW()-started_at))::int),verify_log=$3
			WHERE session_id=$1 AND generation=$2 AND status='in_progress'`,
			submission.Session.SessionID, int64(submission.Session.Generation), verifyLog); err != nil {
			return VerifyDecision{}, fmt.Errorf("record successful durable choice attempt: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE sessions
			SET state='completed',desired_state='absent',finished_at=NOW(),updated_at=NOW(),lock_version=lock_version+1
			WHERE id=$1`, submission.Session.SessionID); err != nil {
			return VerifyDecision{}, fmt.Errorf("record completed durable choice session: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE runner_allocations
			SET desired_state='absent',observed_state=CASE WHEN observed_state='absent' THEN 'absent' ELSE 'deleting' END,
			    updated_at=NOW(),lock_version=lock_version+1 WHERE id=$1`, ref.ID); err != nil {
			return VerifyDecision{}, fmt.Errorf("record durable choice cleanup intent: %w", err)
		}
		if _, err := ensureDestroyOperationTx(ctx, tx, ref.ID, providerID); err != nil {
			return VerifyDecision{}, err
		}
	} else {
		if _, err := tx.Exec(ctx, `
			UPDATE attempts SET verify_log=$3
			WHERE session_id=$1 AND generation=$2 AND status='in_progress'`,
			submission.Session.SessionID, int64(submission.Session.Generation), verifyLog); err != nil {
			return VerifyDecision{}, fmt.Errorf("record failed durable choice attempt log: %w", err)
		}
	}

	reason := "grade_failed"
	if success {
		reason = "grade_passed"
	}
	if _, err := ensureKeyedEventTx(ctx, tx, ref.ID, identity.finishedEventKey(), "verify_finished", reason, "Verification finished.", resultJSON); err != nil {
		return VerifyDecision{}, err
	}
	if success {
		if _, err := ensureEventTx(ctx, tx, ref.ID, "destroying", "completed", "Environment cleanup started."); err != nil {
			return VerifyDecision{}, err
		}
	}

	expected := VerifyDecision{Kind: VerifyGradeReplay, Session: submission.Session, Success: success, Log: verifyLog}
	if err := tx.Commit(ctx); err != nil {
		return s.reconcileChoiceCommit(userID, submission, expected, err)
	}
	return expected, nil
}

type choiceRequestHash struct {
	Schema       string       `json:"schema"`
	UserID       string       `json:"user_id"`
	AllocationID string       `json:"allocation_id"`
	Provider     ProviderKind `json:"provider"`
	ProblemID    string       `json:"problem_id"`
	SessionID    string       `json:"session_id"`
	Generation   uint64       `json:"generation"`
	Kind         string       `json:"kind"`
	OperationID  string       `json:"operation_id"`
	ChoiceID     string       `json:"choice_id"`
}

func newChoiceIdentity(ref AllocationRef, userID string, submission ChoiceSubmission) (verifyIdentity, error) {
	if ref.ID == "" || ref.Session.SessionID == "" || ref.Session.Generation == 0 || ref.Provider == "" {
		return verifyIdentity{}, ErrAllocationNotFound
	}
	if submission.Session != ref.Session || submission.ProblemID == "" || len(submission.ProblemID) > 255 {
		return verifyIdentity{}, ErrIdempotencyConflict
	}
	if submission.ChoiceID == "" || len(submission.ChoiceID) > 128 || !utf8.ValidString(submission.ChoiceID) {
		return verifyIdentity{}, errors.New("choice id is invalid")
	}
	key, err := verifyIdempotencyKey(userID, submission.OperationID)
	if err != nil {
		return verifyIdentity{}, err
	}
	payload, err := json.Marshal(choiceRequestHash{
		Schema: "k8s-quiz/choice-verification/v1", UserID: userID,
		AllocationID: ref.ID, Provider: ref.Provider, ProblemID: submission.ProblemID,
		SessionID: submission.Session.SessionID, Generation: submission.Session.Generation,
		Kind: "choice", OperationID: submission.OperationID, ChoiceID: submission.ChoiceID,
	})
	if err != nil {
		return verifyIdentity{}, fmt.Errorf("encode choice verification identity: %w", err)
	}
	hash := sha256.Sum256(payload)
	return verifyIdentity{operationID: submission.OperationID, idempotencyKey: key, requestHash: hash}, nil
}

func rejectChoiceReplayAfterSessionAdvance(ctx context.Context, tx pgx.Tx, userID string, expected SessionRef) error {
	var activeSessionID string
	var activeGeneration int64
	err := tx.QueryRow(ctx, `
		SELECT id,current_generation
		FROM sessions
		WHERE user_id=$1 AND desired_state='active'`, userID).
		Scan(&activeSessionID, &activeGeneration)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("resolve active session for choice replay: %w", err)
	}
	if activeSessionID != expected.SessionID || activeGeneration != int64(expected.Generation) {
		return ErrIdempotencyConflict
	}
	return nil
}

func (s *PostgresStore) reconcileChoiceCommit(
	userID string,
	submission ChoiceSubmission,
	expected VerifyDecision,
	commitErr error,
) (VerifyDecision, error) {
	reconcileCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	actual, found, lookupErr := s.lookupChoice(reconcileCtx, userID, submission, false)
	if lookupErr != nil {
		return VerifyDecision{}, errors.Join(ErrOperationOutcomeUnknown, commitErr, lookupErr)
	}
	if !found {
		return VerifyDecision{}, commitErr
	}
	if actual.Kind == expected.Kind && actual.Session == expected.Session &&
		actual.Success == expected.Success && actual.Log == expected.Log {
		return actual, nil
	}
	return VerifyDecision{}, errors.Join(ErrOperationOutcomeUnknown, commitErr,
		fmt.Errorf("durable choice commit reconciled as %s", actual.Kind))
}
