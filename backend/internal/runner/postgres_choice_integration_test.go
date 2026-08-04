package runner

import (
	"context"
	"errors"
	"testing"
)

func TestPostgresChoiceGradeIsAnswerBoundAtomicAndReplayable(t *testing.T) {
	store, pool := openRunnerTestStore(t)
	resetRunnerTestData(t, pool)
	const (
		userID    = "71717171-7171-4171-8171-717171717171"
		problemID = "runner-choice-grade"
		revision  = "7171717171717171717171717171717171717171717171717171717171717171"
		sessionID = "72727272-7272-4272-8272-727272727272"
	)
	ref := reserveReadyVerifySession(t, store, pool, userID, problemID, revision, sessionID)

	failed := ChoiceSubmission{
		OperationID: "choice-failed-0001",
		ProblemID:   problemID,
		Session:     ref.Session,
		ChoiceID:    "a",
	}
	decision, err := store.RecordChoice(context.Background(), userID, ref, failed, false, "")
	if err != nil || decision.Kind != VerifyGradeReplay || decision.Success || decision.Session != ref.Session {
		t.Fatalf("failed choice decision=%+v err=%v", decision, err)
	}
	decision, err = store.RecordChoice(context.Background(), userID, ref, failed, false, "")
	if err != nil || decision.Kind != VerifyGradeReplay || decision.Success {
		t.Fatalf("failed choice replay decision=%+v err=%v", decision, err)
	}
	if _, err := store.BeginVerify(context.Background(), userID, ref, failed.OperationID); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("choice operation reused as script verify error=%v, want idempotency conflict", err)
	}
	changedAnswer := failed
	changedAnswer.ChoiceID = "b"
	if _, err := store.RecordChoice(context.Background(), userID, ref, changedAnswer, true, ""); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("changed answer error=%v, want idempotency conflict", err)
	}
	changedProblem := failed
	changedProblem.ProblemID = "another-problem"
	if _, _, err := store.LookupChoice(context.Background(), userID, changedProblem); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("changed problem lookup error=%v, want idempotency conflict", err)
	}

	var state, desired, attemptState string
	var verifyOperations, destroyOperations int
	if err := pool.QueryRow(context.Background(), `SELECT
		(SELECT state FROM sessions WHERE id=$1),
		(SELECT desired_state FROM sessions WHERE id=$1),
		(SELECT status FROM attempts WHERE session_id=$1 AND generation=1),
		(SELECT COUNT(*) FROM runner_operations WHERE allocation_id=$2 AND kind='verify'),
		(SELECT COUNT(*) FROM runner_operations WHERE allocation_id=$2 AND kind='destroy')`,
		sessionID, ref.ID).Scan(&state, &desired, &attemptState, &verifyOperations, &destroyOperations); err != nil {
		t.Fatal(err)
	}
	if state != "ready" || desired != "active" || attemptState != "in_progress" || verifyOperations != 1 || destroyOperations != 0 {
		t.Fatalf("failed choice state=%s/%s attempt=%s verify=%d destroy=%d",
			state, desired, attemptState, verifyOperations, destroyOperations)
	}
	assertLifecycleEventCount(t, pool, ref.ID, "verify_started", 1)
	assertLifecycleEventCount(t, pool, ref.ID, "verify_finished", 1)

	passed := ChoiceSubmission{
		OperationID: "choice-passed-0002",
		ProblemID:   problemID,
		Session:     ref.Session,
		ChoiceID:    "b",
	}
	decision, err = store.RecordChoice(context.Background(), userID, ref, passed, true, "")
	if err != nil || decision.Kind != VerifyGradeReplay || !decision.Success {
		t.Fatalf("passed choice decision=%+v err=%v", decision, err)
	}
	decision, found, err := store.LookupChoice(context.Background(), userID, passed)
	if err != nil || !found || decision.Kind != VerifyGradeReplay || !decision.Success {
		t.Fatalf("passed choice lookup found=%v decision=%+v err=%v", found, decision, err)
	}

	var allocationDesired, operationState, destroyState string
	if err := pool.QueryRow(context.Background(), `SELECT
		(SELECT state FROM sessions WHERE id=$1),
		(SELECT desired_state FROM sessions WHERE id=$1),
		(SELECT desired_state FROM runner_allocations WHERE id=$2),
		(SELECT status FROM attempts WHERE session_id=$1 AND generation=1),
		(SELECT state FROM runner_operations WHERE allocation_id=$2 AND kind='verify' AND idempotency_key LIKE '%choice-passed-0002'),
		(SELECT state FROM runner_operations WHERE allocation_id=$2 AND kind='destroy')`,
		sessionID, ref.ID).Scan(&state, &desired, &allocationDesired, &attemptState, &operationState, &destroyState); err != nil {
		t.Fatal(err)
	}
	if state != "completed" || desired != "absent" || allocationDesired != "absent" ||
		attemptState != "success" || operationState != "succeeded" || destroyState != "pending" {
		t.Fatalf("passed choice state=%s/%s allocation=%s attempt=%s operation=%s destroy=%s",
			state, desired, allocationDesired, attemptState, operationState, destroyState)
	}
	assertLifecycleEventCount(t, pool, ref.ID, "verify_started", 2)
	assertLifecycleEventCount(t, pool, ref.ID, "verify_finished", 2)
	assertLifecycleEventCount(t, pool, ref.ID, "destroying", 1)
	if err := store.RequestDestroy(context.Background(), ref, "completed", "success", ""); err != nil {
		t.Fatalf("choice cleanup intent replay: %v", err)
	}
	assertLifecycleEventCount(t, pool, ref.ID, "destroying", 1)
}

func TestPostgresChoiceReplayRejectsAdvancedGenerationWithoutMutation(t *testing.T) {
	store, pool := openRunnerTestStore(t)
	resetRunnerTestData(t, pool)
	const (
		userID    = "73737373-7373-4373-8373-737373737373"
		problemID = "runner-choice-stale"
		revision  = "7373737373737373737373737373737373737373737373737373737373737373"
		sessionID = "74747474-7474-4474-8474-747474747474"
	)
	ref := reserveReadyVerifySession(t, store, pool, userID, problemID, revision, sessionID)
	submission := ChoiceSubmission{
		OperationID: "choice-stale-0001",
		ProblemID:   problemID,
		Session:     ref.Session,
		ChoiceID:    "a",
	}
	if decision, err := store.RecordChoice(context.Background(), userID, ref, submission, false, ""); err != nil || decision.Success {
		t.Fatalf("seed old choice decision=%+v err=%v", decision, err)
	}

	reset, err := store.ReserveReset(context.Background(), ReserveResetParams{
		Expected: ref.Session, UserID: userID, Provider: ProviderLocalDocker,
		ProviderID: "local-docker:integration", IdempotencyKey: "reset-choice-stale-0001",
	})
	if err != nil {
		t.Fatalf("advance choice session generation: %v", err)
	}
	if reset.New.Allocation.Ref.Session.Generation != 2 {
		t.Fatalf("reset generation=%d, want 2", reset.New.Allocation.Ref.Session.Generation)
	}
	var baselineOperations, baselineEvents int
	var baselineAttemptStatus string
	if err := pool.QueryRow(context.Background(), `SELECT
		(SELECT COUNT(*) FROM runner_operations WHERE allocation_id=$1),
		(SELECT COUNT(*) FROM session_events WHERE allocation_id=$1),
		(SELECT status FROM attempts WHERE session_id=$2 AND generation=1)`,
		ref.ID, sessionID).Scan(&baselineOperations, &baselineEvents, &baselineAttemptStatus); err != nil {
		t.Fatal(err)
	}

	if _, _, err := store.LookupChoice(context.Background(), userID, submission); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("stale choice lookup error=%v, want idempotency conflict", err)
	}
	if _, err := store.RecordChoice(context.Background(), userID, ref, submission, false, ""); !errors.Is(err, ErrGenerationStale) && !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("stale choice record error=%v, want stale/conflict", err)
	}

	var operationsAfter, eventsAfter int
	var attemptAfter string
	if err := pool.QueryRow(context.Background(), `SELECT
		(SELECT COUNT(*) FROM runner_operations WHERE allocation_id=$1),
		(SELECT COUNT(*) FROM session_events WHERE allocation_id=$1),
		(SELECT status FROM attempts WHERE session_id=$2 AND generation=1)`,
		ref.ID, sessionID).Scan(&operationsAfter, &eventsAfter, &attemptAfter); err != nil {
		t.Fatal(err)
	}
	if operationsAfter != baselineOperations || eventsAfter != baselineEvents || attemptAfter != baselineAttemptStatus {
		t.Fatalf("stale choice mutated old allocation operations=%d/%d events=%d/%d attempt=%s/%s",
			operationsAfter, baselineOperations, eventsAfter, baselineEvents, attemptAfter, baselineAttemptStatus)
	}
}
