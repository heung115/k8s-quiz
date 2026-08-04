package runner

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func reserveReadyVerifySession(t *testing.T, store *PostgresStore, pool *pgxpool.Pool, userID, problemID, revision, sessionID string) AllocationRef {
	t.Helper()
	seedRunnerTestIdentity(t, pool, userID, problemID, revision)
	reservation, err := store.ReserveSession(context.Background(), integrationReservation(
		userID, problemID, revision, sessionID, "create-"+sessionID,
	))
	if err != nil {
		t.Fatalf("reserve verify integration session: %v", err)
	}
	ref := reservation.Allocation.Ref
	if err := store.MarkCreateSucceeded(context.Background(), ref); err != nil {
		t.Fatalf("mark verify integration create succeeded: %v", err)
	}
	if err := store.MarkSettingUp(context.Background(), ref); err != nil {
		t.Fatalf("mark verify integration setting up: %v", err)
	}
	if err := store.MarkSettingUp(context.Background(), ref); err != nil {
		t.Fatalf("replay verify integration setting up: %v", err)
	}
	if err := store.MarkReady(context.Background(), ref); err != nil {
		t.Fatalf("mark verify integration ready: %v", err)
	}
	assertLifecycleEventCount(t, pool, ref.ID, "setup_running", 1)
	return ref
}

func TestPostgresVerifyGradeLifecycleIsAtomicReplayableAndOrdered(t *testing.T) {
	store, pool := openRunnerTestStore(t)
	resetRunnerTestData(t, pool)
	const (
		userID    = "81818181-8181-4181-8181-818181818181"
		problemID = "runner-verify-grade"
		revision  = "8181818181818181818181818181818181818181818181818181818181818181"
		sessionID = "82828282-8282-4282-8282-828282828282"
	)
	ref := reserveReadyVerifySession(t, store, pool, userID, problemID, revision, sessionID)

	const failedOperation = "verify-failed-0001"
	decision, err := store.BeginVerify(context.Background(), userID, ref, failedOperation)
	if err != nil || decision.Kind != VerifyExecute || decision.Session != ref.Session {
		t.Fatalf("begin failed verify decision=%+v err=%v", decision, err)
	}
	decision, err = store.BeginVerify(context.Background(), userID, ref, failedOperation)
	if err != nil || decision.Kind != VerifyResume || decision.Session != ref.Session {
		t.Fatalf("resume failed verify decision=%+v err=%v", decision, err)
	}
	if err := store.RecordVerifyFinished(context.Background(), ref, failedOperation, false, "workload still broken"); err != nil {
		t.Fatalf("record failed grade: %v", err)
	}
	if err := store.RecordVerifyFinished(context.Background(), ref, failedOperation, false, "workload still broken"); err != nil {
		t.Fatalf("replay failed grade: %v", err)
	}
	if err := store.RecordVerifyFinished(context.Background(), ref, failedOperation, true, "changed result"); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("conflicting failed grade replay error = %v, want ErrIdempotencyConflict", err)
	}

	var sessionState, sessionDesired, attemptState, attemptLog, operationState string
	var failedResult []byte
	if err := pool.QueryRow(context.Background(), `SELECT
		(SELECT state FROM sessions WHERE id=$1),
		(SELECT desired_state FROM sessions WHERE id=$1),
		(SELECT status FROM attempts WHERE session_id=$1 AND generation=1),
		(SELECT verify_log FROM attempts WHERE session_id=$1 AND generation=1),
		(SELECT state FROM runner_operations WHERE allocation_id=$2 AND kind='verify' AND idempotency_key LIKE '%verify-failed-0001'),
		(SELECT result_metadata FROM runner_operations WHERE allocation_id=$2 AND kind='verify' AND idempotency_key LIKE '%verify-failed-0001')`,
		sessionID, ref.ID).Scan(&sessionState, &sessionDesired, &attemptState, &attemptLog, &operationState, &failedResult); err != nil {
		t.Fatal(err)
	}
	if sessionState != "ready" || sessionDesired != "active" || attemptState != "in_progress" ||
		attemptLog != "workload still broken" || operationState != "succeeded" {
		t.Fatalf("failed grade state = session %s/%s attempt %s/%q operation %s",
			sessionState, sessionDesired, attemptState, attemptLog, operationState)
	}
	assertVerifyGradeJSON(t, failedResult, false, "workload still broken")
	assertLifecycleEventCount(t, pool, ref.ID, "verify_started", 1)
	assertLifecycleEventCount(t, pool, ref.ID, "verify_finished", 1)
	assertLifecycleEventCount(t, pool, ref.ID, "destroying", 0)
	decision, found, err := store.LookupVerify(context.Background(), userID, failedOperation)
	if err != nil || !found || decision.Kind != VerifyGradeReplay || decision.Success || decision.Log != "workload still broken" {
		t.Fatalf("failed grade lookup found=%v decision=%+v err=%v", found, decision, err)
	}
	bootstrap, err := store.BootstrapLifecycle(context.Background(), userID, nil)
	if err != nil || bootstrap.Snapshot == nil || bootstrap.Snapshot.LatestVerifyResult == nil ||
		bootstrap.Snapshot.LatestVerifyResult.Success || bootstrap.Snapshot.LatestVerifyResult.Log != "workload still broken" {
		t.Fatalf("failed grade lifecycle snapshot=%+v err=%v", bootstrap.Snapshot, err)
	}

	const passedOperation = "verify-passed-0002"
	longLog := strings.Repeat("가", 600)
	decision, err = store.BeginVerify(context.Background(), userID, ref, passedOperation)
	if err != nil || decision.Kind != VerifyExecute || decision.Session != ref.Session {
		t.Fatalf("begin passed verify decision=%+v err=%v", decision, err)
	}
	if err := store.RecordVerifyFinished(context.Background(), ref, passedOperation, true, longLog); err != nil {
		t.Fatalf("record passed grade: %v", err)
	}
	if err := store.RecordVerifyFinished(context.Background(), ref, passedOperation, true, longLog); err != nil {
		t.Fatalf("replay passed grade: %v", err)
	}

	var allocationDesired, allocationObserved, passedAttemptState, passedAttemptLog, passedOperationState, destroyState string
	var passedResult []byte
	if err := pool.QueryRow(context.Background(), `SELECT
		(SELECT state FROM sessions WHERE id=$1),
		(SELECT desired_state FROM sessions WHERE id=$1),
		(SELECT desired_state FROM runner_allocations WHERE id=$2),
		(SELECT observed_state FROM runner_allocations WHERE id=$2),
		(SELECT status FROM attempts WHERE session_id=$1 AND generation=1),
		(SELECT verify_log FROM attempts WHERE session_id=$1 AND generation=1),
		(SELECT state FROM runner_operations WHERE allocation_id=$2 AND kind='verify' AND idempotency_key LIKE '%verify-passed-0002'),
		(SELECT result_metadata FROM runner_operations WHERE allocation_id=$2 AND kind='verify' AND idempotency_key LIKE '%verify-passed-0002'),
		(SELECT state FROM runner_operations WHERE allocation_id=$2 AND kind='destroy')`,
		sessionID, ref.ID).Scan(&sessionState, &sessionDesired, &allocationDesired, &allocationObserved,
		&passedAttemptState, &passedAttemptLog, &passedOperationState, &passedResult, &destroyState); err != nil {
		t.Fatal(err)
	}
	if sessionState != "completed" || sessionDesired != "absent" || allocationDesired != "absent" ||
		allocationObserved != "deleting" || passedAttemptState != "success" || passedOperationState != "succeeded" || destroyState != "pending" {
		t.Fatalf("passed grade state = session %s/%s allocation %s/%s attempt %s operation %s destroy %s",
			sessionState, sessionDesired, allocationDesired, allocationObserved, passedAttemptState, passedOperationState, destroyState)
	}
	if len(passedAttemptLog) > 1024 || !strings.HasPrefix(longLog, passedAttemptLog) {
		t.Fatalf("bounded verify log length/content = %d/%q", len(passedAttemptLog), passedAttemptLog)
	}
	assertVerifyGradeJSON(t, passedResult, true, passedAttemptLog)
	assertLifecycleEventCount(t, pool, ref.ID, "verify_started", 2)
	assertLifecycleEventCount(t, pool, ref.ID, "verify_finished", 2)
	assertLifecycleEventCount(t, pool, ref.ID, "destroying", 1)
	var finishedSequence, destroyingSequence int64
	if err := pool.QueryRow(context.Background(), `SELECT
		(SELECT MAX(sequence) FROM session_events WHERE allocation_id=$1 AND event_type='verify_finished'),
		(SELECT sequence FROM session_events WHERE allocation_id=$1 AND event_type='destroying')`, ref.ID).
		Scan(&finishedSequence, &destroyingSequence); err != nil {
		t.Fatal(err)
	}
	if finishedSequence >= destroyingSequence {
		t.Fatalf("verify_finished sequence %d must precede destroying %d", finishedSequence, destroyingSequence)
	}
	if err := store.RequestDestroy(context.Background(), ref, "completed", "success", passedAttemptLog); err != nil {
		t.Fatalf("completed RequestDestroy replay after verify result: %v", err)
	}
	decision, found, err = store.LookupVerify(context.Background(), userID, passedOperation)
	if err != nil || !found || decision.Kind != VerifyGradeReplay || !decision.Success || decision.Log != passedAttemptLog {
		t.Fatalf("passed grade lookup found=%v decision=%+v err=%v", found, decision, err)
	}
}

func TestPostgresVerifyInfrastructureFailureIsSafeAndRetryable(t *testing.T) {
	store, pool := openRunnerTestStore(t)
	resetRunnerTestData(t, pool)
	const (
		userID    = "83838383-8383-4383-8383-838383838383"
		problemID = "runner-verify-infrastructure"
		revision  = "8383838383838383838383838383838383838383838383838383838383838383"
		sessionID = "84848484-8484-4484-8484-848484848484"
		operation = "verify-infra-0001"
	)
	ref := reserveReadyVerifySession(t, store, pool, userID, problemID, revision, sessionID)
	decision, err := store.BeginVerify(context.Background(), userID, ref, operation)
	if err != nil || decision.Kind != VerifyExecute || decision.Session != ref.Session {
		t.Fatalf("begin infrastructure verify decision=%+v err=%v", decision, err)
	}
	if err := store.RecordVerifyInfrastructureFailure(context.Background(), ref, operation, "provider raw: secret=bad"); err != nil {
		t.Fatalf("record verify infrastructure failure: %v", err)
	}
	if err := store.RecordVerifyInfrastructureFailure(context.Background(), ref, operation, "provider raw: secret=bad"); err != nil {
		t.Fatalf("replay verify infrastructure failure: %v", err)
	}

	var sessionState, attemptState, attemptLog, operationState, errorCode, errorMessage string
	var completed bool
	var result, payload []byte
	if err := pool.QueryRow(context.Background(), `SELECT
		(SELECT state FROM sessions WHERE id=$1),
		(SELECT status FROM attempts WHERE session_id=$1 AND generation=1),
		(SELECT verify_log FROM attempts WHERE session_id=$1 AND generation=1),
		(SELECT state FROM runner_operations WHERE allocation_id=$2 AND kind='verify'),
		(SELECT COALESCE(error_code,'') FROM runner_operations WHERE allocation_id=$2 AND kind='verify'),
		(SELECT COALESCE(error_message,'') FROM runner_operations WHERE allocation_id=$2 AND kind='verify'),
		(SELECT completed_at IS NOT NULL FROM runner_operations WHERE allocation_id=$2 AND kind='verify'),
		(SELECT result_metadata FROM runner_operations WHERE allocation_id=$2 AND kind='verify'),
		(SELECT sanitized_payload FROM session_events WHERE allocation_id=$2 AND event_type='verify_finished')`,
		sessionID, ref.ID).Scan(&sessionState, &attemptState, &attemptLog, &operationState, &errorCode, &errorMessage, &completed, &result, &payload); err != nil {
		t.Fatal(err)
	}
	if sessionState != "ready" || attemptState != "in_progress" || attemptLog != "" ||
		operationState != "failed" || !completed || errorCode != "verify_infrastructure_failure" || errorMessage != "" {
		t.Fatalf("infrastructure result = session %s attempt %s/%q operation %s completed=%v code=%q message=%q",
			sessionState, attemptState, attemptLog, operationState, completed, errorCode, errorMessage)
	}
	for _, raw := range [][]byte{result, payload} {
		if strings.Contains(string(raw), "secret") || strings.Contains(string(raw), "provider raw") {
			t.Fatalf("unsafe provider detail persisted: %s", raw)
		}
		var decoded map[string]any
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatal(err)
		}
		if decoded["infrastructure_error"] != true || decoded["code"] != "verify_infrastructure_failure" {
			t.Fatalf("infrastructure payload = %v", decoded)
		}
	}
	assertLifecycleEventCount(t, pool, ref.ID, "verify_started", 1)
	assertLifecycleEventCount(t, pool, ref.ID, "verify_finished", 1)
	assertLifecycleEventCount(t, pool, ref.ID, "destroying", 0)
	decision, found, err := store.LookupVerify(context.Background(), userID, operation)
	if err != nil || !found || decision.Kind != VerifyInfrastructureReplay || decision.ErrorCode != "verify_infrastructure_failure" {
		t.Fatalf("infrastructure lookup found=%v decision=%+v err=%v", found, decision, err)
	}
	bootstrap, err := store.BootstrapLifecycle(context.Background(), userID, nil)
	if err != nil || bootstrap.Snapshot == nil || bootstrap.Snapshot.LatestVerifyResult != nil {
		t.Fatalf("infrastructure failure leaked as grade snapshot=%+v err=%v", bootstrap.Snapshot, err)
	}

	decision, err = store.BeginVerify(context.Background(), userID, ref, "verify-after-infra-0002")
	if err != nil || decision.Kind != VerifyExecute || decision.Session != ref.Session {
		t.Fatalf("new verify after infrastructure failure decision=%+v err=%v", decision, err)
	}
}

func TestPostgresInterruptedVerifyRecoveryIsTerminalAndIdempotent(t *testing.T) {
	store, pool := openRunnerTestStore(t)
	resetRunnerTestData(t, pool)
	const (
		userID    = "89898989-8989-4989-8989-898989898989"
		problemID = "runner-verify-recovery"
		revision  = "8989898989898989898989898989898989898989898989898989898989898989"
		sessionID = "90909090-9090-4090-8090-909090909090"
		operation = "verify-recovery-0001"
	)
	ref := reserveReadyVerifySession(t, store, pool, userID, problemID, revision, sessionID)
	decision, err := store.BeginVerify(context.Background(), userID, ref, operation)
	if err != nil || decision.Kind != VerifyExecute {
		t.Fatalf("begin interrupted verify decision=%+v err=%v", decision, err)
	}

	if err := store.PrepareRecoveryWork(context.Background(), "local-docker:integration"); err != nil {
		t.Fatalf("prepare interrupted verify recovery: %v", err)
	}
	if err := store.PrepareRecoveryWork(context.Background(), "local-docker:integration"); err != nil {
		t.Fatalf("replay interrupted verify recovery: %v", err)
	}

	var sessionState, operationState, errorCode string
	if err := pool.QueryRow(context.Background(), `SELECT
		(SELECT state FROM sessions WHERE id=$1),
		(SELECT state FROM runner_operations WHERE allocation_id=$2 AND kind='verify'),
		(SELECT COALESCE(error_code,'') FROM runner_operations WHERE allocation_id=$2 AND kind='verify')`,
		sessionID, ref.ID).Scan(&sessionState, &operationState, &errorCode); err != nil {
		t.Fatal(err)
	}
	if sessionState != "ready" || operationState != "failed" || errorCode != "controller_restarted" {
		t.Fatalf("interrupted verify recovery session=%s operation=%s code=%s", sessionState, operationState, errorCode)
	}
	decision, found, err := store.LookupVerify(context.Background(), userID, operation)
	if err != nil || !found || decision.Kind != VerifyInfrastructureReplay || decision.ErrorCode != "controller_restarted" {
		t.Fatalf("interrupted verify lookup found=%v decision=%+v err=%v", found, decision, err)
	}
	assertLifecycleEventCount(t, pool, ref.ID, "verify_started", 1)
	assertLifecycleEventCount(t, pool, ref.ID, "verify_finished", 1)
}

func TestPostgresInterruptedVerifyRecoveryDiagnosesDuplicateRunningOperations(t *testing.T) {
	store, pool := openRunnerTestStore(t)
	resetRunnerTestData(t, pool)
	const (
		userID             = "94949494-9494-4494-8494-949494949494"
		problemID          = "runner-verify-duplicate-recovery"
		revision           = "9494949494949494949494949494949494949494949494949494949494949494"
		sessionID          = "95959595-9595-4595-8595-959595959595"
		operation          = "verify-recovery-primary-0001"
		duplicateOperation = "verify-recovery-duplicate-0002"
		indexName          = "runner_operations_one_running_verify_per_allocation"
	)
	ref := reserveReadyVerifySession(t, store, pool, userID, problemID, revision, sessionID)
	if decision, err := store.BeginVerify(context.Background(), userID, ref, operation); err != nil || decision.Kind != VerifyExecute {
		t.Fatalf("begin primary interrupted verify decision=%+v err=%v", decision, err)
	}

	// Migration 009 prevents this state. Temporarily remove the guard in the
	// disposable integration database to prove recovery fails with a precise,
	// atomic diagnosis if legacy/corrupt data already contains it.
	if _, err := pool.Exec(context.Background(), `DROP INDEX `+indexName); err != nil {
		t.Fatalf("drop running verify guard for corruption fixture: %v", err)
	}
	indexDropped := true
	t.Cleanup(func() {
		if !indexDropped {
			return
		}
		_, _ = pool.Exec(context.Background(), `DELETE FROM runner_operations WHERE idempotency_key=$1`, duplicateOperation)
		if _, err := pool.Exec(context.Background(), `
			CREATE UNIQUE INDEX IF NOT EXISTS runner_operations_one_running_verify_per_allocation
			ON runner_operations (allocation_id)
			WHERE kind='verify' AND state='running'`); err != nil {
			t.Errorf("restore running verify guard: %v", err)
		}
	})
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO runner_operations (
			allocation_id,provider_id,kind,idempotency_key,request_hash,state,started_at
		) VALUES ($1,$2,'verify',$3,decode(repeat('ab',32),'hex'),'running',NOW())`,
		ref.ID, "local-docker:integration", duplicateOperation); err != nil {
		t.Fatalf("seed duplicate running verify: %v", err)
	}

	err := store.PrepareRecoveryWork(context.Background(), "local-docker:integration")
	if !errors.Is(err, ErrLifecycleConflict) || !strings.Contains(err.Error(), "multiple running verify operations") {
		t.Fatalf("duplicate recovery error=%v, want precise lifecycle conflict", err)
	}
	var sessionState string
	var running int
	if err := pool.QueryRow(context.Background(), `SELECT
		(SELECT state FROM sessions WHERE id=$1),
		(SELECT COUNT(*) FROM runner_operations WHERE allocation_id=$2 AND kind='verify' AND state='running')`,
		sessionID, ref.ID).Scan(&sessionState, &running); err != nil {
		t.Fatal(err)
	}
	if sessionState != "verifying" || running != 2 {
		t.Fatalf("duplicate diagnosis was not atomic: session=%s running=%d", sessionState, running)
	}

	if _, err := pool.Exec(context.Background(), `DELETE FROM runner_operations WHERE idempotency_key=$1`, duplicateOperation); err != nil {
		t.Fatalf("remove duplicate recovery fixture: %v", err)
	}
	if _, err := pool.Exec(context.Background(), `
		CREATE UNIQUE INDEX runner_operations_one_running_verify_per_allocation
		ON runner_operations (allocation_id)
		WHERE kind='verify' AND state='running'`); err != nil {
		t.Fatalf("restore running verify guard: %v", err)
	}
	indexDropped = false
	if err := store.PrepareRecoveryWork(context.Background(), "local-docker:integration"); err != nil {
		t.Fatalf("recover after duplicate is repaired: %v", err)
	}
	if err := pool.QueryRow(context.Background(), `SELECT state FROM sessions WHERE id=$1`, sessionID).Scan(&sessionState); err != nil {
		t.Fatal(err)
	}
	if sessionState != "ready" {
		t.Fatalf("repaired recovery session=%s, want ready", sessionState)
	}
}

func TestPostgresVerifyCommitReconciliationIgnoresNewActiveSession(t *testing.T) {
	store, pool := openRunnerTestStore(t)
	resetRunnerTestData(t, pool)
	const (
		userID       = "91919191-9191-4191-8191-919191919191"
		problemID    = "runner-verify-commit-reconcile"
		revision     = "9191919191919191919191919191919191919191919191919191919191919191"
		oldSessionID = "92929292-9292-4292-8292-929292929292"
		newSessionID = "93939393-9393-4393-8393-939393939393"
		operation    = "verify-commit-reconcile-0001"
	)
	oldRef := reserveReadyVerifySession(t, store, pool, userID, problemID, revision, oldSessionID)
	if decision, err := store.BeginVerify(context.Background(), userID, oldRef, operation); err != nil || decision.Kind != VerifyExecute {
		t.Fatalf("begin old verify decision=%+v err=%v", decision, err)
	}
	if err := store.RecordVerifyFinished(context.Background(), oldRef, operation, true, "fixed"); err != nil {
		t.Fatalf("record old verify grade: %v", err)
	}
	if _, err := store.ReserveSession(context.Background(), integrationReservation(
		userID, problemID, revision, newSessionID, "create-after-verify-commit",
	)); err != nil {
		t.Fatalf("reserve new active session: %v", err)
	}

	if _, _, err := store.LookupVerify(context.Background(), userID, operation); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("public old verify replay error=%v, want idempotency conflict", err)
	}
	decision, found, err := store.lookupVerify(context.Background(), userID, operation, false)
	if err != nil || !found || decision.Kind != VerifyGradeReplay || !decision.Success || decision.Log != "fixed" {
		t.Fatalf("exact reconciliation lookup found=%v decision=%+v err=%v", found, decision, err)
	}
}

func TestPostgresVerifyRejectsStaleGenerationAndRollsBackEventFailure(t *testing.T) {
	store, pool := openRunnerTestStore(t)
	resetRunnerTestData(t, pool)
	const (
		userID    = "85858585-8585-4585-8585-858585858585"
		problemID = "runner-verify-stale"
		revision  = "8585858585858585858585858585858585858585858585858585858585858585"
		sessionID = "86868686-8686-4686-8686-868686868686"
	)
	oldRef := reserveReadyVerifySession(t, store, pool, userID, problemID, revision, sessionID)
	if _, err := store.ReserveReset(context.Background(), ReserveResetParams{
		Expected: oldRef.Session, UserID: userID, Provider: oldRef.Provider,
		ProviderID: "local-docker:integration", IdempotencyKey: "reset-verify-stale-0001",
	}); err != nil {
		t.Fatalf("reserve replacement generation: %v", err)
	}
	if _, err := store.BeginVerify(context.Background(), userID, oldRef, "verify-stale-0001"); !errors.Is(err, ErrGenerationStale) {
		t.Fatalf("stale verify start error = %v, want ErrGenerationStale", err)
	}
	if err := store.MarkSettingUp(context.Background(), oldRef); !errors.Is(err, ErrGenerationStale) {
		t.Fatalf("stale setup error = %v, want ErrGenerationStale", err)
	}
	var staleOperations int
	if err := pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM runner_operations WHERE allocation_id=$1 AND kind='verify'`, oldRef.ID).
		Scan(&staleOperations); err != nil {
		t.Fatal(err)
	}
	if staleOperations != 0 {
		t.Fatalf("stale verify persisted %d operation(s)", staleOperations)
	}

	resetRunnerTestData(t, pool)
	const (
		rollbackUser    = "87878787-8787-4787-8787-878787878787"
		rollbackSession = "88888888-8888-4888-8888-888888888888"
		rollbackOp      = "verify-rollback-0001"
	)
	ref := reserveReadyVerifySession(t, store, pool, rollbackUser, problemID, revision, rollbackSession)
	identity, err := newVerifyIdentity(ref, rollbackUser, rollbackOp)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `WITH next_event AS (
		UPDATE runner_allocations SET last_event_sequence=last_event_sequence+1 WHERE id=$1
		RETURNING last_event_sequence
	) INSERT INTO session_events (
		allocation_id,sequence,event_type,reason_code,message,sanitized_payload,event_key
	) SELECT $1,last_event_sequence,'verify_started','tampered','tampered','{"operation_id":"different"}'::jsonb,$2
	FROM next_event`, ref.ID, identity.startedEventKey()); err != nil {
		t.Fatalf("seed conflicting keyed event: %v", err)
	}
	if _, err := store.BeginVerify(context.Background(), rollbackUser, ref, rollbackOp); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("conflicting event start error = %v, want ErrIdempotencyConflict", err)
	}
	var state string
	var operationCount int
	if err := pool.QueryRow(context.Background(), `SELECT
		(SELECT state FROM sessions WHERE id=$1),
		(SELECT COUNT(*) FROM runner_operations WHERE allocation_id=$2 AND kind='verify')`, rollbackSession, ref.ID).
		Scan(&state, &operationCount); err != nil {
		t.Fatal(err)
	}
	if state != "ready" || operationCount != 0 {
		t.Fatalf("failed keyed event was not atomic: session=%s verify_operations=%d", state, operationCount)
	}
}

func assertVerifyGradeJSON(t *testing.T, raw []byte, success bool, log string) {
	t.Helper()
	var result struct {
		Success *bool   `json:"success"`
		Log     *string `json:"log"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("decode verify result metadata: %v", err)
	}
	if result.Success == nil || *result.Success != success || result.Log == nil || *result.Log != log {
		t.Fatalf("verify result metadata = %s, want success=%v log=%q", raw, success, log)
	}
}
