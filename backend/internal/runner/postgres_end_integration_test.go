package runner

import (
	"context"
	"errors"
	"testing"
)

func TestPostgresEndOperationRequiresWholeLogicalSessionAbsence(t *testing.T) {
	store, pool := openRunnerTestStore(t)
	resetRunnerTestData(t, pool)
	const (
		userID       = "81818181-8181-4181-8181-818181818181"
		problemID    = "durable-end-chain"
		revision     = "8181818181818181818181818181818181818181818181818181818181818181"
		sessionID    = "82828282-8282-4282-8282-828282828282"
		endOperation = "end:whole-chain-0001"
	)
	seedRunnerTestIdentity(t, pool, userID, problemID, revision)
	params := integrationReservation(userID, problemID, revision, sessionID, "create-end-chain")
	first, err := store.ReserveSession(context.Background(), params)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkCreateSucceeded(context.Background(), first.Allocation.Ref); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkReady(context.Background(), first.Allocation.Ref); err != nil {
		t.Fatal(err)
	}
	reset, err := store.ReserveReset(context.Background(), ReserveResetParams{
		Expected: first.Allocation.Ref.Session, UserID: userID,
		Provider: ProviderLocalDocker, ProviderID: params.ProviderID,
		IdempotencyKey: "reset-end-chain-0001",
	})
	if err != nil {
		t.Fatal(err)
	}

	expected := reset.New.Allocation.Ref.Session
	decision, err := store.RequestEnd(context.Background(), userID, expected, endOperation)
	if err != nil {
		t.Fatal(err)
	}
	if decision.State != EndPending || len(decision.Pending) != 2 {
		t.Fatalf("initial end decision = %+v, want two pending generations", decision)
	}
	if _, err := store.RequestEnd(context.Background(), userID,
		SessionRef{SessionID: expected.SessionID, Generation: expected.Generation + 1}, endOperation,
	); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("changed end identity error=%v, want idempotency conflict", err)
	}

	if err := store.MarkDestroyed(context.Background(), reset.New.Allocation.Ref); err != nil {
		t.Fatal(err)
	}
	decision, err = store.RequestEnd(context.Background(), userID, expected, endOperation)
	if err != nil {
		t.Fatal(err)
	}
	if decision.State != EndPending || len(decision.Pending) != 1 || decision.Pending[0] != reset.Old {
		t.Fatalf("one-generation pending decision = %+v", decision)
	}

	if err := store.MarkDestroyed(context.Background(), reset.Old); err != nil {
		t.Fatal(err)
	}
	decision, err = store.RequestEnd(context.Background(), userID, expected, endOperation)
	if err != nil {
		t.Fatal(err)
	}
	if decision.State != EndCompleted || len(decision.Pending) != 0 {
		t.Fatalf("completed end decision = %+v", decision)
	}
	if replay, err := store.RequestEnd(context.Background(), userID, expected, endOperation); err != nil {
		t.Fatal(err)
	} else if replay.State != EndCompleted {
		t.Fatalf("completed replay = %+v", replay)
	}

	var outstanding, unfinishedDestroy, endSucceeded int
	if err := pool.QueryRow(context.Background(), `
		SELECT
			COUNT(*) FILTER (WHERE a.desired_state<>'absent' OR a.observed_state<>'absent'),
			COUNT(*) FILTER (WHERE d.kind='destroy' AND d.state<>'succeeded'),
			COUNT(*) FILTER (WHERE d.kind='end' AND d.state='succeeded')
		FROM runner_allocations a
		JOIN runner_operations d ON d.allocation_id=a.id AND d.provider_id=a.provider_id
		WHERE a.session_id=$1`, sessionID).Scan(&outstanding, &unfinishedDestroy, &endSucceeded); err != nil {
		t.Fatal(err)
	}
	if outstanding != 0 || unfinishedDestroy != 0 || endSucceeded != 1 {
		t.Fatalf("end ledger outstanding=%d unfinished_destroy=%d end_succeeded=%d", outstanding, unfinishedDestroy, endSucceeded)
	}
}

func TestPostgresEndCommitReconciliationAcceptsCommittedLedger(t *testing.T) {
	store, pool := openRunnerTestStore(t)
	resetRunnerTestData(t, pool)
	const (
		userID      = "91919191-9191-4191-8191-919191919191"
		problemID   = "end-reconcile-committed"
		revision    = "9191919191919191919191919191919191919191919191919191919191919191"
		sessionID   = "92929292-9292-4292-8292-929292929292"
		operationID = "end:reconcile-committed-0001"
	)
	seedRunnerTestIdentity(t, pool, userID, problemID, revision)
	reservation, err := store.ReserveSession(context.Background(), integrationReservation(
		userID, problemID, revision, sessionID, "create-end-reconcile-committed",
	))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkCreateSucceeded(context.Background(), reservation.Allocation.Ref); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkReady(context.Background(), reservation.Allocation.Ref); err != nil {
		t.Fatal(err)
	}
	decision, err := store.RequestEnd(context.Background(), userID, reservation.Allocation.Ref.Session, operationID)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := newEndIdentity(userID, reservation.Allocation.Ref.Session, operationID)
	if err != nil {
		t.Fatal(err)
	}
	commitErr := errors.New("simulated response loss")
	reconciled, err := store.reconcileEndCommit(userID, reservation.Allocation.Ref.Session, identity, decision, commitErr)
	if err != nil || reconciled.State != decision.State || !sameAllocationRefs(reconciled.Pending, decision.Pending) {
		t.Fatalf("committed reconciliation decision=%+v err=%v, want %+v", reconciled, err, decision)
	}
}

func TestPostgresEndCommitReconciliationReturnsOriginalErrorWhenLedgerAbsent(t *testing.T) {
	store, pool := openRunnerTestStore(t)
	resetRunnerTestData(t, pool)
	const (
		userID      = "93939393-9393-4393-8393-939393939393"
		problemID   = "end-reconcile-absent"
		revision    = "9393939393939393939393939393939393939393939393939393939393939393"
		sessionID   = "94949494-9494-4494-8494-949494949494"
		operationID = "end:reconcile-absent-0001"
	)
	seedRunnerTestIdentity(t, pool, userID, problemID, revision)
	reservation, err := store.ReserveSession(context.Background(), integrationReservation(
		userID, problemID, revision, sessionID, "create-end-reconcile-absent",
	))
	if err != nil {
		t.Fatal(err)
	}
	identity, err := newEndIdentity(userID, reservation.Allocation.Ref.Session, operationID)
	if err != nil {
		t.Fatal(err)
	}
	commitErr := errors.New("simulated commit failure")
	_, err = store.reconcileEndCommit(userID, reservation.Allocation.Ref.Session, identity,
		EndDecision{Session: reservation.Allocation.Ref.Session, State: EndPending}, commitErr)
	if !errors.Is(err, commitErr) || errors.Is(err, ErrOperationOutcomeUnknown) {
		t.Fatalf("absent ledger reconciliation error=%v, want original failure only", err)
	}
	var sessionDesired, allocationDesired string
	if err := pool.QueryRow(context.Background(), `
		SELECT s.desired_state,a.desired_state
		FROM sessions s JOIN runner_allocations a ON a.session_id=s.id
		WHERE s.id=$1 AND a.id=$2`, sessionID, reservation.Allocation.Ref.ID).
		Scan(&sessionDesired, &allocationDesired); err != nil {
		t.Fatal(err)
	}
	if sessionDesired != "active" || allocationDesired != "active" {
		t.Fatalf("absent reconciliation mutated desired state: session=%s allocation=%s", sessionDesired, allocationDesired)
	}
}

func TestPostgresEndCommitReconciliationReportsContradictionAsUnknown(t *testing.T) {
	store, pool := openRunnerTestStore(t)
	resetRunnerTestData(t, pool)
	const (
		userID      = "95959595-9595-4595-8595-959595959595"
		problemID   = "end-reconcile-conflict"
		revision    = "9595959595959595959595959595959595959595959595959595959595959595"
		sessionID   = "96969696-9696-4696-8696-969696969696"
		operationID = "end:reconcile-conflict-0001"
	)
	seedRunnerTestIdentity(t, pool, userID, problemID, revision)
	reservation, err := store.ReserveSession(context.Background(), integrationReservation(
		userID, problemID, revision, sessionID, "create-end-reconcile-conflict",
	))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.RequestEnd(context.Background(), userID, reservation.Allocation.Ref.Session, operationID); err != nil {
		t.Fatal(err)
	}
	identity, err := newEndIdentity(userID, reservation.Allocation.Ref.Session, operationID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.reconcileEndCommit(userID, reservation.Allocation.Ref.Session, identity,
		EndDecision{Session: reservation.Allocation.Ref.Session, State: EndCompleted}, errors.New("simulated response loss"))
	if !errors.Is(err, ErrOperationOutcomeUnknown) {
		t.Fatalf("contradictory reconciliation error=%v, want ErrOperationOutcomeUnknown", err)
	}
}

func TestPostgresEndOperationRejectsStaleCurrentWithoutMutation(t *testing.T) {
	store, pool := openRunnerTestStore(t)
	resetRunnerTestData(t, pool)
	const (
		userID    = "83838383-8383-4383-8383-838383838383"
		problemID = "durable-end-stale"
		revision  = "8383838383838383838383838383838383838383838383838383838383838383"
		sessionID = "84848484-8484-4484-8484-848484848484"
	)
	seedRunnerTestIdentity(t, pool, userID, problemID, revision)
	params := integrationReservation(userID, problemID, revision, sessionID, "create-end-stale")
	reservation, err := store.ReserveSession(context.Background(), params)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.RequestEnd(context.Background(), userID,
		SessionRef{SessionID: sessionID, Generation: 2}, "end:stale-current-0001",
	); !errors.Is(err, ErrGenerationStale) {
		t.Fatalf("stale end error=%v, want generation stale", err)
	}
	var desired, createState string
	var endCount, destroyCount int
	if err := pool.QueryRow(context.Background(), `
		SELECT a.desired_state,c.state,
		       COUNT(*) FILTER (WHERE o.kind='end') OVER (),
		       COUNT(*) FILTER (WHERE o.kind='destroy') OVER ()
		FROM runner_allocations a
		JOIN runner_operations c ON c.allocation_id=a.id AND c.kind='create'
		JOIN runner_operations o ON o.allocation_id=a.id
		WHERE a.id=$1 LIMIT 1`, reservation.Allocation.Ref.ID).
		Scan(&desired, &createState, &endCount, &destroyCount); err != nil {
		t.Fatal(err)
	}
	if desired != "active" || createState != "pending" || endCount != 0 || destroyCount != 0 {
		t.Fatalf("stale end mutated desired=%s create=%s end=%d destroy=%d", desired, createState, endCount, destroyCount)
	}
}
