package runner

import (
	"context"
	"errors"
	"testing"
)

func TestPostgresLifecycleBootstrapReplayOwnershipAndNoLeakage(t *testing.T) {
	store, pool := openRunnerTestStore(t)
	resetRunnerTestData(t, pool)
	const (
		ownerID  = "10101010-1010-4010-8010-101010101010"
		otherID  = "20202020-2020-4020-8020-202020202020"
		problem  = "lifecycle-reader-bootstrap"
		revision = "1010101010101010101010101010101010101010101010101010101010101010"
		session  = "30303030-3030-4030-8030-303030303030"
	)
	seedRunnerTestIdentity(t, pool, ownerID, problem, revision)
	if _, err := pool.Exec(context.Background(), `INSERT INTO users (id,github_id,username) VALUES ($1,$2,$3)`, otherID, int64(9002), "other-reader"); err != nil {
		t.Fatal(err)
	}
	reservation, err := store.ReserveSession(context.Background(), integrationReservation(ownerID, problem, revision, session, "create-lifecycle-reader"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkCreateSucceeded(context.Background(), reservation.Allocation.Ref); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkReady(context.Background(), reservation.Allocation.Ref); err != nil {
		t.Fatal(err)
	}

	bootstrap, err := store.BootstrapLifecycle(context.Background(), ownerID, &LifecycleCursor{
		SessionID: session, Generation: 1, EventSequence: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if bootstrap.Resync || bootstrap.Snapshot == nil || bootstrap.Snapshot.Status != "ready" || bootstrap.Snapshot.EventSequence != 3 {
		t.Fatalf("bootstrap = %+v", bootstrap)
	}
	if len(bootstrap.Events) != 2 || bootstrap.Events[0].Sequence != 2 || bootstrap.Events[1].Sequence != 3 {
		t.Fatalf("bootstrap replay = %+v", bootstrap.Events)
	}
	for _, event := range bootstrap.Events {
		if event.AllocationID != "" {
			t.Fatalf("event leaks allocation id %q", event.AllocationID)
		}
	}
	if bootstrap.Snapshot.OperationID == "" {
		t.Fatal("snapshot omitted create operation identity")
	}

	foreign, err := store.BootstrapLifecycle(context.Background(), otherID, &LifecycleCursor{
		SessionID: session, Generation: 1, EventSequence: 1,
	})
	if !errors.Is(err, ErrLifecycleNotFoundOrForbidden) {
		t.Fatalf("foreign bootstrap error = %v, result=%+v", err, foreign)
	}
	if foreign.Snapshot != nil || len(foreign.Events) != 0 {
		t.Fatalf("foreign bootstrap leaked state: %+v", foreign)
	}
	foreignDelta, err := store.ReadLifecycleDelta(context.Background(), otherID, LifecycleCursor{
		SessionID: session, Generation: 1, EventSequence: 1,
	}, 16)
	if !errors.Is(err, ErrLifecycleNotFoundOrForbidden) {
		t.Fatalf("foreign delta error = %v, result=%+v", err, foreignDelta)
	}
}

func TestPostgresLifecycleReaderDetectsMissingCursorAndInternalGap(t *testing.T) {
	store, pool := openRunnerTestStore(t)
	resetRunnerTestData(t, pool)
	const (
		userID   = "40404040-4040-4040-8040-404040404040"
		problem  = "lifecycle-reader-gap"
		revision = "4040404040404040404040404040404040404040404040404040404040404040"
		session  = "50505050-5050-4050-8050-505050505050"
	)
	seedRunnerTestIdentity(t, pool, userID, problem, revision)
	reservation, err := store.ReserveSession(context.Background(), integrationReservation(userID, problem, revision, session, "create-lifecycle-gap"))
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 4; index++ {
		if _, err := store.AppendEvent(context.Background(), reservation.Allocation.Ref.ID, "booting", "progress", "progress", nil); err != nil {
			t.Fatal(err)
		}
	}
	cursor := LifecycleCursor{SessionID: session, Generation: 1, EventSequence: 2}
	if _, err := pool.Exec(context.Background(), `DELETE FROM session_events WHERE allocation_id=$1 AND sequence=2`, reservation.Allocation.Ref.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReadLifecycleDelta(context.Background(), userID, cursor, 16); !errors.Is(err, ErrLifecycleCursorUnavailable) {
		t.Fatalf("missing cursor delta error = %v", err)
	}
	missingBootstrap, err := store.BootstrapLifecycle(context.Background(), userID, &cursor)
	if err != nil {
		t.Fatal(err)
	}
	if !missingBootstrap.Resync || missingBootstrap.Snapshot == nil || len(missingBootstrap.Events) != 0 {
		t.Fatalf("missing cursor bootstrap = %+v", missingBootstrap)
	}

	if _, err := pool.Exec(context.Background(), `
		INSERT INTO session_events (allocation_id,sequence,event_type,reason_code,message,sanitized_payload)
		VALUES ($1,2,'booting','restored','restored','{}'::jsonb)`, reservation.Allocation.Ref.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `DELETE FROM session_events WHERE allocation_id=$1 AND sequence=4`, reservation.Allocation.Ref.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReadLifecycleDelta(context.Background(), userID, cursor, 16); !errors.Is(err, ErrLifecycleCursorUnavailable) {
		t.Fatalf("internal gap delta error = %v", err)
	}
}

func TestPostgresLifecycleBootstrapResyncsFutureAndOversizedCursor(t *testing.T) {
	store, pool := openRunnerTestStore(t)
	resetRunnerTestData(t, pool)
	const (
		userID   = "80808080-8080-4080-8080-808080808080"
		problem  = "lifecycle-reader-resync"
		revision = "8080808080808080808080808080808080808080808080808080808080808080"
		session  = "90909090-9090-4090-8090-909090909090"
	)
	seedRunnerTestIdentity(t, pool, userID, problem, revision)
	reservation, err := store.ReserveSession(context.Background(), integrationReservation(userID, problem, revision, session, "create-lifecycle-resync"))
	if err != nil {
		t.Fatal(err)
	}

	future, err := store.BootstrapLifecycle(context.Background(), userID, &LifecycleCursor{
		SessionID: session, Generation: 1, EventSequence: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !future.Resync || future.Snapshot == nil || future.Snapshot.EventSequence != 1 || len(future.Events) != 0 {
		t.Fatalf("future cursor bootstrap = %+v", future)
	}

	for index := 0; index <= maxLifecycleBootstrapReplay; index++ {
		if _, err := store.AppendEvent(context.Background(), reservation.Allocation.Ref.ID, "booting", "progress", "progress", nil); err != nil {
			t.Fatal(err)
		}
	}
	oversized, err := store.BootstrapLifecycle(context.Background(), userID, &LifecycleCursor{
		SessionID: session, Generation: 1, EventSequence: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !oversized.Resync || oversized.Snapshot == nil || oversized.Snapshot.EventSequence != maxLifecycleBootstrapReplay+2 || len(oversized.Events) != 0 {
		t.Fatalf("oversized cursor bootstrap = snapshot=%+v resync=%v events=%d", oversized.Snapshot, oversized.Resync, len(oversized.Events))
	}
}

func TestPostgresLifecycleGenerationSwitchAndTerminalNoSession(t *testing.T) {
	store, pool := openRunnerTestStore(t)
	resetRunnerTestData(t, pool)
	const (
		userID   = "60606060-6060-4060-8060-606060606060"
		problem  = "lifecycle-reader-switch"
		revision = "6060606060606060606060606060606060606060606060606060606060606060"
		session  = "70707070-7070-4070-8070-707070707070"
	)
	seedRunnerTestIdentity(t, pool, userID, problem, revision)
	params := integrationReservation(userID, problem, revision, session, "create-lifecycle-switch")
	start, err := store.ReserveSession(context.Background(), params)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkCreateSucceeded(context.Background(), start.Allocation.Ref); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkReady(context.Background(), start.Allocation.Ref); err != nil {
		t.Fatal(err)
	}
	oldCursor := LifecycleCursor{SessionID: session, Generation: 1, EventSequence: 3}
	reset, err := store.ReserveReset(context.Background(), ReserveResetParams{
		Expected: start.Allocation.Ref.Session, UserID: userID, Provider: ProviderLocalDocker,
		ProviderID: params.ProviderID, IdempotencyKey: "reset-lifecycle-switch",
	})
	if err != nil {
		t.Fatal(err)
	}
	delta, err := store.ReadLifecycleDelta(context.Background(), userID, oldCursor, 16)
	if err != nil {
		t.Fatal(err)
	}
	if !delta.SnapshotChanged || delta.Snapshot == nil || delta.Snapshot.Generation != 2 || delta.Snapshot.EventSequence != 2 || !delta.Snapshot.CleanupPending {
		t.Fatalf("generation switch delta = %+v", delta)
	}
	if err := store.MarkDestroyed(context.Background(), reset.Old); err != nil {
		t.Fatal(err)
	}
	cleanupDelta, err := store.ReadLifecycleDelta(context.Background(), userID, delta.Snapshot.Cursor(), 16)
	if err != nil {
		t.Fatal(err)
	}
	if len(cleanupDelta.Events) != 1 || cleanupDelta.Events[0].Type != "predecessor_destroyed" ||
		cleanupDelta.Events[0].Sequence != 3 {
		t.Fatalf("ordered predecessor cleanup delta = %+v", cleanupDelta)
	}
	postCleanup, err := store.BootstrapLifecycle(context.Background(), userID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if postCleanup.Snapshot == nil || postCleanup.Snapshot.EventSequence != 3 || postCleanup.Snapshot.CleanupPending {
		t.Fatalf("post-cleanup snapshot = %+v", postCleanup)
	}
	if err := store.RequestDestroy(context.Background(), reset.New.Allocation.Ref, "failed", "failed", "replacement failed"); err != nil {
		t.Fatal(err)
	}
	terminalBootstrap, err := store.BootstrapLifecycle(context.Background(), userID, &LifecycleCursor{
		SessionID: session, Generation: 2, EventSequence: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if terminalBootstrap.Snapshot == nil || terminalBootstrap.Snapshot.TerminalReason != "failed" {
		t.Fatalf("terminal snapshot = %+v", terminalBootstrap)
	}
	terminalCursor := terminalBootstrap.Snapshot.Cursor()
	pendingCleanup, err := store.ReadLifecycleDelta(context.Background(), userID, terminalCursor, 16)
	if err != nil {
		t.Fatal(err)
	}
	if pendingCleanup.SnapshotChanged || pendingCleanup.Snapshot != nil || len(pendingCleanup.Events) != 0 {
		t.Fatalf("destroy intent must retain the pending snapshot, delta = %+v", pendingCleanup)
	}
	if err := store.MarkDestroyed(context.Background(), reset.New.Allocation.Ref); err != nil {
		t.Fatal(err)
	}
	destroyedDelta, err := store.ReadLifecycleDelta(context.Background(), userID, terminalCursor, 16)
	if err != nil {
		t.Fatal(err)
	}
	if len(destroyedDelta.Events) == 0 || destroyedDelta.Events[len(destroyedDelta.Events)-1].Type != "destroyed" {
		t.Fatalf("destroyed delta = %+v", destroyedDelta)
	}
	noSession, err := store.ReadLifecycleDelta(context.Background(), userID, LifecycleCursor{
		SessionID: session, Generation: 2,
		EventSequence: destroyedDelta.Events[len(destroyedDelta.Events)-1].Sequence,
	}, 16)
	if err != nil {
		t.Fatal(err)
	}
	if !noSession.SnapshotChanged || noSession.Snapshot != nil || len(noSession.Events) != 0 {
		t.Fatalf("terminal no-session delta = %+v", noSession)
	}
}
