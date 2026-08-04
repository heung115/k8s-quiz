package runner

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/k8s-quiz/backend/pkg/dbmigrate"
)

const runnerTestDatabaseEnv = "RUNNER_TEST_DATABASE_URL"

var runnerTestSchemaSequence atomic.Uint64

func openRunnerTestStore(t *testing.T) (*PostgresStore, *pgxpool.Pool) {
	t.Helper()
	databaseURL := os.Getenv(runnerTestDatabaseEnv)
	if databaseURL == "" {
		t.Skip(runnerTestDatabaseEnv + " is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	admin, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open runner integration database: %v", err)
	}
	if err := admin.Ping(ctx); err != nil {
		admin.Close()
		t.Fatalf("ping runner integration database: %v", err)
	}

	sequence := runnerTestSchemaSequence.Add(1)
	schema := fmt.Sprintf("runner_%d_%d", time.Now().UnixNano(), sequence)
	identifier := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+identifier); err != nil {
		admin.Close()
		t.Fatalf("create isolated runner schema: %v", err)
	}
	parsed, err := url.Parse(databaseURL)
	if err != nil {
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+identifier+" CASCADE")
		admin.Close()
		t.Fatalf("parse runner integration database URL: %v", err)
	}
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	isolatedURL := parsed.String()
	if err := dbmigrate.UpIsolatedTestSchema(isolatedURL); err != nil {
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+identifier+" CASCADE")
		admin.Close()
		t.Fatalf("migrate isolated runner schema: %v", err)
	}
	pool, err := pgxpool.New(ctx, isolatedURL)
	if err != nil {
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+identifier+" CASCADE")
		admin.Close()
		t.Fatalf("open isolated runner schema: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+identifier+" CASCADE")
		admin.Close()
		t.Fatalf("ping isolated runner schema: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer dropCancel()
		if _, err := admin.Exec(dropCtx, "DROP SCHEMA "+identifier+" CASCADE"); err != nil {
			t.Errorf("drop isolated runner schema: %v", err)
		}
		admin.Close()
	})

	lease, err := acquireControllerLease(ctx, pool, "local-docker:integration", 20*time.Millisecond, func(loss error) {
		t.Errorf("integration store controller lease lost: %v", loss)
	})
	if err != nil {
		t.Fatalf("acquire integration store controller lease: %v", err)
	}
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := lease.Close(closeCtx); err != nil {
			t.Errorf("close integration store controller lease: %v", err)
		}
	})
	store, err := NewPostgresStore(pool, lease.Fence())
	if err != nil {
		t.Fatal(err)
	}
	return store, pool
}

func resetRunnerTestData(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		t.Fatalf("begin runner integration reset: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `DELETE FROM runner_controller_proof_consumptions`); err != nil {
		t.Fatalf("delete runner controller proof consumptions: %v", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM runner_controller_proofs`); err != nil {
		t.Fatalf("delete runner controller proofs: %v", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM session_events`); err != nil {
		t.Fatalf("delete runner integration events: %v", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM runner_operations`); err != nil {
		t.Fatalf("delete runner integration operations: %v", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM attempts`); err != nil {
		t.Fatalf("delete runner integration attempts: %v", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM runner_allocations`); err != nil {
		t.Fatalf("delete runner integration allocations: %v", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM sessions`); err != nil {
		t.Fatalf("delete runner integration sessions: %v", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM users`); err != nil {
		t.Fatalf("delete runner integration users: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("reset runner integration data: %v", err)
	}
}

func seedRunnerTestIdentity(t *testing.T, pool *pgxpool.Pool, userID, problemID, revision string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx,
		`INSERT INTO users (id,github_id,username) VALUES ($1,$2,$3)
		 ON CONFLICT (id) DO NOTHING`,
		userID, int64(9001), "runner-test",
	); err != nil {
		t.Fatalf("seed runner integration user: %v", err)
	}
	var headCount int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM problem_catalog_head`).Scan(&headCount); err != nil {
		t.Fatalf("read runner integration catalog head: %v", err)
	}
	if headCount == 0 {
		digest := sha256.Sum256([]byte(problemID + ":" + revision))
		artifactDigest := sha256.Sum256([]byte("runner-test-artifact:" + problemID + ":" + revision))
		tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
		if err != nil {
			t.Fatalf("begin runner integration catalog seed: %v", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx,
			`INSERT INTO problems (id,title,revision) VALUES ($1,$2,$3)`,
			problemID, "Runner test", revision,
		); err != nil {
			t.Fatalf("seed runner integration problem: %v", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO problem_catalog_publications
				(generation,previous_generation,digest_schema,candidate_digest,entry_count)
			VALUES (1,NULL,1,$1,1)`, digest[:]); err != nil {
			t.Fatalf("seed runner integration publication: %v", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO problem_artifacts (
				problem_id,problem_revision,artifact_digest_schema,artifact_digest,
				artifact_media_type,artifact_size,source_trust
			) VALUES (
				$1,$2,1,$3,'application/vnd.k8s-quiz.problem-runtime.v1',1024,
				'development_checkout'
			)`, problemID, revision, artifactDigest[:]); err != nil {
			t.Fatalf("seed runner integration artifact binding: %v", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO problem_catalog_entries
				(catalog_generation,problem_id,problem_revision,title,category,difficulty,type,timeout_minutes,verify_type)
			VALUES (1,$1,$2,'Runner test','pod','easy','fix',30,'script')`, problemID, revision); err != nil {
			t.Fatalf("seed runner integration catalog entry: %v", err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO problem_catalog_head (singleton,catalog_generation) VALUES (TRUE,1)`); err != nil {
			t.Fatalf("seed runner integration catalog head: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit runner integration catalog seed: %v", err)
		}
		return
	}
	var storedRevision string
	if err := pool.QueryRow(ctx, `
		SELECT e.problem_revision
		FROM problem_catalog_head h
		JOIN problem_catalog_entries e ON e.catalog_generation=h.catalog_generation
		WHERE h.singleton=TRUE AND e.problem_id=$1`, problemID).Scan(&storedRevision); err != nil {
		t.Fatalf("reuse runner integration catalog problem: %v", err)
	}
	if storedRevision != revision {
		t.Fatalf("runner integration catalog revision=%q want=%q", storedRevision, revision)
	}
}

func integrationReservation(userID, problemID, revision, sessionID, key string) ReserveSessionParams {
	return ReserveSessionParams{
		SessionID: sessionID, UserID: userID,
		Selection: CatalogSelection{Generation: 1, Problem: ProblemRef{ID: problemID, Revision: revision}},
		Provider:  ProviderLocalDocker, ProviderID: "local-docker:integration",
		ResourceProfile: DefaultResourceProfile,
		ExpiresAt:       time.Now().UTC().Add(time.Hour),
		IdempotencyKey:  key,
	}
}

func TestPostgresStoreReservationIsAtomicAndReplayable(t *testing.T) {
	store, pool := openRunnerTestStore(t)
	resetRunnerTestData(t, pool)
	const (
		userID    = "11111111-1111-4111-8111-111111111111"
		problemID = "runner-test"
		revision  = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		sessionID = "22222222-2222-4222-8222-222222222222"
	)
	seedRunnerTestIdentity(t, pool, userID, problemID, revision)
	params := integrationReservation(userID, problemID, revision, sessionID, "create-integration-1")

	first, err := store.ReserveSession(context.Background(), params)
	if err != nil {
		t.Fatalf("reserve session: %v", err)
	}
	second, err := store.ReserveSession(context.Background(), params)
	if err != nil {
		t.Fatalf("replay reservation: %v", err)
	}
	if first.Session.ID != second.Session.ID || first.Allocation.Ref != second.Allocation.Ref ||
		first.Operation.ID != second.Operation.ID || first.Operation.RequestHash != second.Operation.RequestHash {
		t.Fatalf("reservation replay changed identity: first=%+v second=%+v", first, second)
	}
	delayed := params
	delayed.ExpiresAt = params.ExpiresAt.Add(37 * time.Second)
	if _, err := store.ReserveSession(context.Background(), delayed); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("changed-expiry replay error = %v, want ErrIdempotencyConflict", err)
	}
	third, found, err := store.FindReservation(context.Background(), params.ProviderID, params.IdempotencyKey)
	if err != nil || !found {
		t.Fatalf("find exact reservation replay: found=%v err=%v", found, err)
	}
	if !third.Session.ExpiresAt.Equal(first.Session.ExpiresAt) || !third.Allocation.ExpiresAt.Equal(first.Allocation.ExpiresAt) {
		t.Fatalf("read-only replay changed stored expiry: first=%v/%v replay=%v/%v",
			first.Session.ExpiresAt, first.Allocation.ExpiresAt, third.Session.ExpiresAt, third.Allocation.ExpiresAt)
	}

	var sessions, allocations, operations, events, attempts int
	if err := pool.QueryRow(context.Background(), `SELECT
		(SELECT COUNT(*) FROM sessions),
		(SELECT COUNT(*) FROM runner_allocations),
		(SELECT COUNT(*) FROM runner_operations),
		(SELECT COUNT(*) FROM session_events),
		(SELECT COUNT(*) FROM attempts WHERE session_id IS NOT NULL)`).Scan(
		&sessions, &allocations, &operations, &events, &attempts,
	); err != nil {
		t.Fatal(err)
	}
	if sessions != 1 || allocations != 1 || operations != 1 || events != 1 || attempts != 1 {
		t.Fatalf("atomic reservation row counts = %d/%d/%d/%d/%d", sessions, allocations, operations, events, attempts)
	}

	conflict := params
	conflict.Selection.Problem.Revision = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	if _, err := store.ReserveSession(context.Background(), conflict); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("conflicting replay error = %v, want ErrIdempotencyConflict", err)
	}
}

func TestPostgresActiveReservationSnapshotAndGenerationOneClaim(t *testing.T) {
	store, pool := openRunnerTestStore(t)
	resetRunnerTestData(t, pool)
	const (
		userID    = "71717171-7171-4717-8717-717171717171"
		problemID = "runner-startup-hydration"
		revision  = "7171717171717171717171717171717171717171717171717171717171717171"
		sessionID = "72727272-7272-4727-8727-727272727272"
	)
	seedRunnerTestIdentity(t, pool, userID, problemID, revision)
	reservation, err := store.ReserveSession(context.Background(), integrationReservation(
		userID, problemID, revision, sessionID, "create-startup-hydration",
	))
	if err != nil {
		t.Fatal(err)
	}

	active, err := store.ListActiveReservations(context.Background(), "local-docker:integration")
	if err != nil {
		t.Fatalf("list active reservations: %v", err)
	}
	if len(active) != 1 || active[0].Allocation.Ref != reservation.Allocation.Ref ||
		active[0].Operation.ID != reservation.Operation.ID || active[0].AttemptStatus != "in_progress" {
		t.Fatalf("active reservation snapshot = %+v, want %+v", active, reservation)
	}

	claim, found, err := store.ClaimProvisionableCreate(
		context.Background(), "local-docker:integration", reservation.Operation.ID,
		"startup-generation-one", 5*time.Second,
	)
	if err != nil {
		t.Fatalf("claim generation-one create: %v", err)
	}
	if !found || claim.Reservation.Allocation.Ref != reservation.Allocation.Ref ||
		claim.Reservation.Allocation.Ref.Session.Generation != 1 || claim.LeaseExpiresAt.IsZero() {
		t.Fatalf("generation-one create claim = %+v", claim)
	}
	if _, found, err := store.ClaimProvisionableReset(
		context.Background(), "local-docker:integration", reservation.Operation.ID,
		"reset-only-must-skip-generation-one", 5*time.Second,
	); err != nil || found {
		t.Fatalf("reset-only claim selected generation one: found=%v err=%v", found, err)
	}
	if err := store.MarkClaimedCreateSucceeded(context.Background(), claim); err != nil {
		t.Fatalf("complete generation-one create claim: %v", err)
	}

	active, err = store.ListActiveReservations(context.Background(), "local-docker:integration")
	if err != nil {
		t.Fatalf("list adopted active reservation: %v", err)
	}
	if len(active) != 1 || active[0].Operation.State != "succeeded" || active[0].Session.State != "booting" {
		t.Fatalf("adopted active reservation = %+v", active)
	}
}

func TestPostgresCreateClaimLeaseStartsAfterEpochLockWait(t *testing.T) {
	store, pool := openRunnerTestStore(t)
	resetRunnerTestData(t, pool)
	const (
		userID    = "73737373-7373-4737-8737-737373737373"
		problemID = "runner-claim-clock"
		revision  = "7373737373737373737373737373737373737373737373737373737373737373"
		sessionID = "74747474-7474-4747-8747-747474747474"
	)
	seedRunnerTestIdentity(t, pool, userID, problemID, revision)
	reservation, err := store.ReserveSession(context.Background(), integrationReservation(
		userID, problemID, revision, sessionID, "create-claim-clock",
	))
	if err != nil {
		t.Fatal(err)
	}

	lockTx, err := pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lockTx.Rollback(context.Background()) }()
	if _, err := lockTx.Exec(context.Background(), `
		SELECT epoch FROM runner_controller_epochs
		WHERE provider_id=$1 FOR UPDATE`, "local-docker:integration"); err != nil {
		t.Fatalf("lock controller epoch row: %v", err)
	}

	type claimResult struct {
		claim CreateWorkClaim
		found bool
		err   error
	}
	result := make(chan claimResult, 1)
	go func() {
		claim, found, claimErr := store.ClaimProvisionableCreate(
			context.Background(), "local-docker:integration", reservation.Operation.ID,
			"clock-wait-worker", time.Second,
		)
		result <- claimResult{claim: claim, found: found, err: claimErr}
	}()
	// Exceed the complete requested lease while the claim transaction is
	// blocked behind the epoch row. transaction_timestamp()-based code would
	// return an already expired claim after the lock is released.
	time.Sleep(1100 * time.Millisecond)
	if err := lockTx.Commit(context.Background()); err != nil {
		t.Fatalf("release controller epoch row: %v", err)
	}
	claimed := <-result
	if claimed.err != nil || !claimed.found {
		t.Fatalf("claim after epoch lock wait: found=%v err=%v", claimed.found, claimed.err)
	}
	remaining := time.Until(claimed.claim.LeaseExpiresAt)
	if remaining < 800*time.Millisecond {
		t.Fatalf("claim returned with only %v usable lease, want nearly one second", remaining)
	}
}

func TestPostgresStoreSerializesConcurrentActiveSessionsPerUser(t *testing.T) {
	store, pool := openRunnerTestStore(t)
	resetRunnerTestData(t, pool)
	const (
		userID    = "33333333-3333-4333-8333-333333333333"
		problemID = "runner-concurrent"
		revision  = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	)
	seedRunnerTestIdentity(t, pool, userID, problemID, revision)

	params := []ReserveSessionParams{
		integrationReservation(userID, problemID, revision, "44444444-4444-4444-8444-444444444444", "create-concurrent-a"),
		integrationReservation(userID, problemID, revision, "55555555-5555-4555-8555-555555555555", "create-concurrent-b"),
	}
	start := make(chan struct{})
	errs := make([]error, len(params))
	var wg sync.WaitGroup
	for i := range params {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-start
			_, errs[index] = store.ReserveSession(context.Background(), params[index])
		}(i)
	}
	close(start)
	wg.Wait()

	succeeded, activeConflict := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, ErrActiveSession):
			activeConflict++
		default:
			t.Fatalf("unexpected concurrent reservation error: %v", err)
		}
	}
	if succeeded != 1 || activeConflict != 1 {
		t.Fatalf("concurrent reservation results success=%d active-conflict=%d errors=%v", succeeded, activeConflict, errs)
	}
}

func TestPostgresStoreConcurrentEventsAreContiguousAndReplayable(t *testing.T) {
	store, pool := openRunnerTestStore(t)
	resetRunnerTestData(t, pool)
	const (
		userID    = "66666666-6666-4666-8666-666666666666"
		problemID = "runner-events"
		revision  = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
		sessionID = "77777777-7777-4777-8777-777777777777"
	)
	seedRunnerTestIdentity(t, pool, userID, problemID, revision)
	reservation, err := store.ReserveSession(context.Background(), integrationReservation(
		userID, problemID, revision, sessionID, "create-events",
	))
	if err != nil {
		t.Fatal(err)
	}

	const appends = 24
	start := make(chan struct{})
	errs := make(chan error, appends)
	var wg sync.WaitGroup
	for i := 0; i < appends; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-start
			_, appendErr := store.AppendEvent(
				context.Background(), reservation.Allocation.Ref.ID, "booting", "integration",
				fmt.Sprintf("event-%02d", index), nil,
			)
			errs <- appendErr
		}(i)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("append event: %v", err)
		}
	}

	events, err := store.ReadEvents(context.Background(), reservation.Allocation.Ref.ID, 0, appends+1)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != appends+1 {
		t.Fatalf("event count = %d, want %d", len(events), appends+1)
	}
	for index, event := range events {
		want := uint64(index + 1)
		if event.Sequence != want {
			t.Fatalf("event sequence at %d = %d, want %d", index, event.Sequence, want)
		}
	}
	replayed, err := store.ReadEvents(context.Background(), reservation.Allocation.Ref.ID, 20, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(replayed) != 5 || replayed[0].Sequence != 21 || replayed[4].Sequence != 25 {
		t.Fatalf("event replay after 20 = %+v", replayed)
	}
}

func TestPostgresLifecycleCreateReadyIsIdempotentAndRejectsLateFailure(t *testing.T) {
	store, pool := openRunnerTestStore(t)
	resetRunnerTestData(t, pool)
	const (
		userID    = "88888888-8888-4888-8888-888888888888"
		problemID = "runner-lifecycle-ready"
		revision  = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
		sessionID = "99999999-9999-4999-8999-999999999999"
	)
	seedRunnerTestIdentity(t, pool, userID, problemID, revision)
	reservation, err := store.ReserveSession(context.Background(), integrationReservation(
		userID, problemID, revision, sessionID, "create-lifecycle-ready",
	))
	if err != nil {
		t.Fatal(err)
	}
	ref := reservation.Allocation.Ref

	if err := store.MarkCreateSucceeded(context.Background(), ref); err != nil {
		t.Fatalf("mark create succeeded: %v", err)
	}
	if err := store.MarkCreateSucceeded(context.Background(), ref); err != nil {
		t.Fatalf("replay create succeeded: %v", err)
	}
	if err := store.MarkReady(context.Background(), ref); err != nil {
		t.Fatalf("mark ready: %v", err)
	}
	if err := store.MarkReady(context.Background(), ref); err != nil {
		t.Fatalf("replay ready: %v", err)
	}
	if err := store.MarkCreateSucceeded(context.Background(), ref); err != nil {
		t.Fatalf("replay create after ready: %v", err)
	}
	if err := store.MarkCreateFailed(context.Background(), ref, "late_failure", "late provider error"); !errors.Is(err, ErrLifecycleConflict) {
		t.Fatalf("late create failure error = %v, want ErrLifecycleConflict", err)
	}

	var sessionState, sessionDesired, allocationState, operationState, attemptState string
	if err := pool.QueryRow(context.Background(), `SELECT
		(SELECT state FROM sessions WHERE id=$1),
		(SELECT desired_state FROM sessions WHERE id=$1),
		(SELECT observed_state FROM runner_allocations WHERE id=$2),
		(SELECT state FROM runner_operations WHERE allocation_id=$2 AND kind='create'),
		(SELECT status FROM attempts WHERE session_id=$1 AND generation=1)`,
		sessionID, ref.ID,
	).Scan(&sessionState, &sessionDesired, &allocationState, &operationState, &attemptState); err != nil {
		t.Fatal(err)
	}
	if sessionState != "ready" || sessionDesired != "active" || allocationState != "running" ||
		operationState != "succeeded" || attemptState != "in_progress" {
		t.Fatalf("ready durable state = session %s/%s allocation %s operation %s attempt %s",
			sessionState, sessionDesired, allocationState, operationState, attemptState)
	}
	assertLifecycleEventCount(t, pool, ref.ID, "vm_created", 1)
	assertLifecycleEventCount(t, pool, ref.ID, "ready", 1)
	assertLifecycleEventCount(t, pool, ref.ID, "failed", 0)
}

func TestPostgresLifecycleCreateFailureCleanupIsReplayable(t *testing.T) {
	store, pool := openRunnerTestStore(t)
	resetRunnerTestData(t, pool)
	const (
		userID    = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
		problemID = "runner-lifecycle-failed"
		revision  = "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
		sessionID = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	)
	seedRunnerTestIdentity(t, pool, userID, problemID, revision)
	reservation, err := store.ReserveSession(context.Background(), integrationReservation(
		userID, problemID, revision, sessionID, "create-lifecycle-failed",
	))
	if err != nil {
		t.Fatal(err)
	}
	ref := reservation.Allocation.Ref
	longUTF8Message := string(make([]byte, 0))
	for i := 0; i < 600; i++ {
		longUTF8Message += "실"
	}
	if err := store.MarkCreateFailed(context.Background(), ref, "provider_create_failed", longUTF8Message); err != nil {
		t.Fatalf("mark create failed: %v", err)
	}
	if err := store.MarkCreateFailed(context.Background(), ref, "provider_create_failed", longUTF8Message); err != nil {
		t.Fatalf("replay create failed: %v", err)
	}

	var sessionState, sessionDesired, allocationDesired, allocationObserved, createState, destroyState, attemptState, failureMessage string
	if err := pool.QueryRow(context.Background(), `SELECT
		(SELECT state FROM sessions WHERE id=$1),
		(SELECT desired_state FROM sessions WHERE id=$1),
		(SELECT desired_state FROM runner_allocations WHERE id=$2),
		(SELECT observed_state FROM runner_allocations WHERE id=$2),
		(SELECT state FROM runner_operations WHERE allocation_id=$2 AND kind='create'),
		(SELECT state FROM runner_operations WHERE allocation_id=$2 AND kind='destroy'),
		(SELECT status FROM attempts WHERE session_id=$1 AND generation=1),
		(SELECT failure_message FROM runner_allocations WHERE id=$2)`, sessionID, ref.ID,
	).Scan(&sessionState, &sessionDesired, &allocationDesired, &allocationObserved,
		&createState, &destroyState, &attemptState, &failureMessage); err != nil {
		t.Fatal(err)
	}
	if sessionState != "failed" || sessionDesired != "absent" || allocationDesired != "absent" ||
		allocationObserved != "error" || createState != "cleanup_required" || destroyState != "pending" || attemptState != "failed" {
		t.Fatalf("failed durable state = session %s/%s allocation %s/%s create %s destroy %s attempt %s",
			sessionState, sessionDesired, allocationDesired, allocationObserved, createState, destroyState, attemptState)
	}
	if len(failureMessage) > 1024 {
		t.Fatalf("bounded failure message has %d bytes", len(failureMessage))
	}
	assertLifecycleEventCount(t, pool, ref.ID, "failed", 1)
	assertLifecycleEventCount(t, pool, ref.ID, "cleanup_required", 1)

	if err := store.MarkDestroyed(context.Background(), ref); err != nil {
		t.Fatalf("mark failed allocation destroyed: %v", err)
	}
	if err := store.MarkDestroyed(context.Background(), ref); err != nil {
		t.Fatalf("replay failed allocation destroyed: %v", err)
	}
	if err := pool.QueryRow(context.Background(), `SELECT
		(SELECT observed_state FROM runner_allocations WHERE id=$1),
		(SELECT state FROM runner_operations WHERE allocation_id=$1 AND kind='create'),
		(SELECT state FROM runner_operations WHERE allocation_id=$1 AND kind='destroy'),
		(SELECT state FROM sessions WHERE id=$2)`, ref.ID, sessionID,
	).Scan(&allocationObserved, &createState, &destroyState, &sessionState); err != nil {
		t.Fatal(err)
	}
	if allocationObserved != "absent" || createState != "failed" || destroyState != "succeeded" || sessionState != "destroyed" {
		t.Fatalf("destroyed state = allocation %s create %s destroy %s session %s",
			allocationObserved, createState, destroyState, sessionState)
	}
	assertLifecycleEventCount(t, pool, ref.ID, "destroyed", 1)
}

func TestPostgresLifecycleDestroyTerminalizesUnfinishedCreate(t *testing.T) {
	store, pool := openRunnerTestStore(t)
	resetRunnerTestData(t, pool)
	const (
		userID    = "12121212-1212-4121-8121-121212121212"
		problemID = "runner-lifecycle-aborted-create"
		revision  = "abababababababababababababababababababababababababababababababab"
		sessionID = "34343434-3434-4343-8343-343434343434"
	)
	seedRunnerTestIdentity(t, pool, userID, problemID, revision)
	reservation, err := store.ReserveSession(context.Background(), integrationReservation(
		userID, problemID, revision, sessionID, "create-lifecycle-aborted-before-persist",
	))
	if err != nil {
		t.Fatal(err)
	}
	ref := reservation.Allocation.Ref
	if err := store.RequestDestroy(context.Background(), ref, "failed", "failed", "activation failed before create acknowledgement"); err != nil {
		t.Fatalf("request destroy before create acknowledgement: %v", err)
	}
	if err := store.MarkDestroyed(context.Background(), ref); err != nil {
		t.Fatalf("mark aborted create allocation destroyed: %v", err)
	}

	var createState, createCode, destroyState, sessionState, allocationObserved string
	if err := pool.QueryRow(context.Background(), `SELECT
		(SELECT state FROM runner_operations WHERE allocation_id=$1 AND kind='create'),
		(SELECT COALESCE(error_code,'') FROM runner_operations WHERE allocation_id=$1 AND kind='create'),
		(SELECT state FROM runner_operations WHERE allocation_id=$1 AND kind='destroy'),
		(SELECT state FROM sessions WHERE id=$2),
		(SELECT observed_state FROM runner_allocations WHERE id=$1)`, ref.ID, sessionID,
	).Scan(&createState, &createCode, &destroyState, &sessionState, &allocationObserved); err != nil {
		t.Fatal(err)
	}
	if createState != "failed" || createCode == "" || destroyState != "succeeded" ||
		sessionState != "destroyed" || allocationObserved != "absent" {
		t.Fatalf("aborted create convergence = create %s/%s destroy %s session %s allocation %s",
			createState, createCode, destroyState, sessionState, allocationObserved)
	}
}

func TestPostgresLifecycleTerminalFirstWriterWins(t *testing.T) {
	store, pool := openRunnerTestStore(t)
	resetRunnerTestData(t, pool)
	const (
		userID    = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
		problemID = "runner-lifecycle-terminal"
		revision  = "1111111111111111111111111111111111111111111111111111111111111111"
		sessionID = "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
	)
	seedRunnerTestIdentity(t, pool, userID, problemID, revision)
	reservation, err := store.ReserveSession(context.Background(), integrationReservation(
		userID, problemID, revision, sessionID, "create-lifecycle-terminal",
	))
	if err != nil {
		t.Fatal(err)
	}
	ref := reservation.Allocation.Ref
	if err := store.MarkCreateSucceeded(context.Background(), ref); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkReady(context.Background(), ref); err != nil {
		t.Fatal(err)
	}
	if err := store.RequestDestroy(context.Background(), ref, "completed", "success", "verified"); err != nil {
		t.Fatalf("request completed destroy: %v", err)
	}
	if err := store.RequestDestroy(context.Background(), ref, "completed", "success", "verified"); err != nil {
		t.Fatalf("replay completed destroy: %v", err)
	}
	if err := store.RequestDestroy(context.Background(), ref, "failed", "failed", "late failure"); !errors.Is(err, ErrLifecycleConflict) {
		t.Fatalf("conflicting terminal outcome error = %v, want ErrLifecycleConflict", err)
	}

	var sessionState, sessionDesired, allocationDesired, attemptState, verifyLog string
	if err := pool.QueryRow(context.Background(), `SELECT
		(SELECT state FROM sessions WHERE id=$1),
		(SELECT desired_state FROM sessions WHERE id=$1),
		(SELECT desired_state FROM runner_allocations WHERE id=$2),
		(SELECT status FROM attempts WHERE session_id=$1 AND generation=1),
		(SELECT verify_log FROM attempts WHERE session_id=$1 AND generation=1)`, sessionID, ref.ID,
	).Scan(&sessionState, &sessionDesired, &allocationDesired, &attemptState, &verifyLog); err != nil {
		t.Fatal(err)
	}
	if sessionState != "completed" || sessionDesired != "absent" || allocationDesired != "absent" ||
		attemptState != "success" || verifyLog != "verified" {
		t.Fatalf("terminal state = session %s/%s allocation %s attempt %s log %q",
			sessionState, sessionDesired, allocationDesired, attemptState, verifyLog)
	}
	assertLifecycleEventCount(t, pool, ref.ID, "destroying", 1)
	assertLifecycleEventCount(t, pool, ref.ID, "failed", 0)
}

func TestPostgresLifecycleDestroyRequiresDurableRequest(t *testing.T) {
	store, pool := openRunnerTestStore(t)
	resetRunnerTestData(t, pool)
	const (
		userID    = "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"
		problemID = "runner-lifecycle-destroy-gate"
		revision  = "2222222222222222222222222222222222222222222222222222222222222222"
		sessionID = "ffffffff-ffff-4fff-8fff-ffffffffffff"
	)
	seedRunnerTestIdentity(t, pool, userID, problemID, revision)
	reservation, err := store.ReserveSession(context.Background(), integrationReservation(
		userID, problemID, revision, sessionID, "create-lifecycle-destroy-gate",
	))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkDestroyed(context.Background(), reservation.Allocation.Ref); !errors.Is(err, ErrLifecycleConflict) {
		t.Fatalf("destroy without request error = %v, want ErrLifecycleConflict", err)
	}
	assertLifecycleEventCount(t, pool, reservation.Allocation.Ref.ID, "destroyed", 0)
}

func TestPostgresLifecycleStaleCreateCannotReviveOldGeneration(t *testing.T) {
	store, pool := openRunnerTestStore(t)
	resetRunnerTestData(t, pool)
	const (
		userID    = "12345678-1234-4234-8234-1234567890ab"
		problemID = "runner-lifecycle-stale"
		revision  = "3333333333333333333333333333333333333333333333333333333333333333"
		sessionID = "23456789-2345-4345-8345-234567890abc"
	)
	seedRunnerTestIdentity(t, pool, userID, problemID, revision)
	reservation, err := store.ReserveSession(context.Background(), integrationReservation(
		userID, problemID, revision, sessionID, "create-lifecycle-stale",
	))
	if err != nil {
		t.Fatal(err)
	}
	oldRef := reservation.Allocation.Ref
	newRef := AllocationRef{
		ID:      AllocationIDForSession(SessionRef{SessionID: sessionID, Generation: 2}),
		Session: SessionRef{SessionID: sessionID, Generation: 2}, Provider: ProviderLocalDocker,
	}
	tx, err := pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(context.Background(), `UPDATE runner_allocations SET desired_state='absent' WHERE id=$1`, oldRef.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(context.Background(), `
		INSERT INTO runner_allocations
		(id,session_id,generation,provider_kind,provider_id,resource_profile,desired_state,observed_state,expires_at,last_event_sequence)
		SELECT $1,session_id,2,provider_kind,provider_id,resource_profile,'active','unknown',expires_at,0
		FROM runner_allocations WHERE id=$2`, newRef.ID, oldRef.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(context.Background(), `UPDATE sessions SET current_generation=2,state='queued' WHERE id=$1`, sessionID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}

	if err := store.MarkCreateSucceeded(context.Background(), oldRef); !errors.Is(err, ErrGenerationStale) {
		t.Fatalf("stale create success error = %v, want ErrGenerationStale", err)
	}
	var oldObserved, oldOperation string
	if err := pool.QueryRow(context.Background(), `SELECT
		(SELECT observed_state FROM runner_allocations WHERE id=$1),
		(SELECT state FROM runner_operations WHERE allocation_id=$1 AND kind='create')`, oldRef.ID,
	).Scan(&oldObserved, &oldOperation); err != nil {
		t.Fatal(err)
	}
	if oldObserved != "unknown" || oldOperation != "pending" {
		t.Fatalf("stale allocation changed to observed=%s operation=%s", oldObserved, oldOperation)
	}
}

func TestPostgresLifecycleLocalRestartPreservesTerminalOutcome(t *testing.T) {
	store, pool := openRunnerTestStore(t)
	resetRunnerTestData(t, pool)
	const (
		userID    = "34567890-3456-4456-8456-34567890abcd"
		problemID = "runner-lifecycle-restart"
		revision  = "4444444444444444444444444444444444444444444444444444444444444444"
		sessionID = "45678901-4567-4567-8567-4567890abcde"
	)
	seedRunnerTestIdentity(t, pool, userID, problemID, revision)
	params := integrationReservation(userID, problemID, revision, sessionID, "create-lifecycle-restart")
	reservation, err := store.ReserveSession(context.Background(), params)
	if err != nil {
		t.Fatal(err)
	}
	ref := reservation.Allocation.Ref
	if err := store.MarkCreateSucceeded(context.Background(), ref); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkReady(context.Background(), ref); err != nil {
		t.Fatal(err)
	}
	if err := store.RequestDestroy(context.Background(), ref, "completed", "success", "verified before restart"); err != nil {
		t.Fatal(err)
	}

	snapshot, err := store.SnapshotLocalRecovery(context.Background(), params.ProviderID)
	if err != nil || len(snapshot) != 1 {
		t.Fatalf("snapshot local restart: %+v, %v", snapshot, err)
	}
	converged, err := store.ConvergeLocalRestartSnapshot(context.Background(), params.ProviderID, snapshot)
	if err != nil {
		t.Fatalf("converge local restart: %v", err)
	}
	if converged != 1 {
		t.Fatalf("converged allocations = %d, want 1", converged)
	}
	snapshot, err = store.SnapshotLocalRecovery(context.Background(), params.ProviderID)
	if err != nil || len(snapshot) != 0 {
		t.Fatalf("post-convergence snapshot = %+v, %v; want empty", snapshot, err)
	}

	var attemptState, verifyLog, allocationObserved, destroyState string
	if err := pool.QueryRow(context.Background(), `SELECT
		(SELECT status FROM attempts WHERE session_id=$1 AND generation=1),
		(SELECT verify_log FROM attempts WHERE session_id=$1 AND generation=1),
		(SELECT observed_state FROM runner_allocations WHERE id=$2),
		(SELECT state FROM runner_operations WHERE allocation_id=$2 AND kind='destroy')`, sessionID, ref.ID,
	).Scan(&attemptState, &verifyLog, &allocationObserved, &destroyState); err != nil {
		t.Fatal(err)
	}
	if attemptState != "success" || verifyLog != "verified before restart" || allocationObserved != "absent" || destroyState != "succeeded" {
		t.Fatalf("restart convergence changed terminal outcome: attempt=%s log=%q allocation=%s destroy=%s",
			attemptState, verifyLog, allocationObserved, destroyState)
	}
	assertLifecycleEventCount(t, pool, ref.ID, "failed", 0)
	assertLifecycleEventCount(t, pool, ref.ID, "destroyed", 1)
}

func TestPostgresLifecycleLocalRestartPreservesInterruptedVerifyReplay(t *testing.T) {
	store, pool := openRunnerTestStore(t)
	resetRunnerTestData(t, pool)
	const (
		userID    = "46567890-4656-4456-8456-46567890abcd"
		problemID = "runner-lifecycle-verify-restart"
		revision  = "4646464646464646464646464646464646464646464646464646464646464646"
		sessionID = "47678901-4767-4567-8567-4767890abcde"
		operation = "verify-local-restart-0001"
	)
	ref := reserveReadyVerifySession(t, store, pool, userID, problemID, revision, sessionID)
	decision, err := store.BeginVerify(context.Background(), userID, ref, operation)
	if err != nil || decision.Kind != VerifyExecute {
		t.Fatalf("begin local interrupted verify decision=%+v err=%v", decision, err)
	}
	refs, err := store.SnapshotLocalRecovery(context.Background(), "local-docker:integration")
	wantRecovery := LocalRecoveryAllocation{
		Ref: ref,
		Selection: CatalogSelection{
			Generation: 1,
			Problem:    ProblemRef{ID: problemID, Revision: revision},
		},
		ResourceProfile: DefaultResourceProfile,
	}
	if err != nil || len(refs) != 1 || refs[0] != wantRecovery {
		t.Fatalf("local recovery snapshot=%+v err=%v", refs, err)
	}
	if err := store.PrepareRecoveryWork(context.Background(), "local-docker:integration"); err != nil {
		t.Fatalf("terminalize local interrupted verify: %v", err)
	}
	converged, err := store.ConvergeLocalRestartSnapshot(
		context.Background(), "local-docker:integration", refs,
	)
	if err != nil || converged != 1 {
		t.Fatalf("converge local interrupted verify=%d err=%v", converged, err)
	}

	decision, found, err := store.LookupVerify(context.Background(), userID, operation)
	if err != nil || !found || decision.Kind != VerifyInfrastructureReplay || decision.ErrorCode != "controller_restarted" {
		t.Fatalf("local interrupted verify replay found=%v decision=%+v err=%v", found, decision, err)
	}
	assertLifecycleEventCount(t, pool, ref.ID, "verify_started", 1)
	assertLifecycleEventCount(t, pool, ref.ID, "verify_finished", 1)
	assertLifecycleEventCount(t, pool, ref.ID, "destroyed", 1)
}

func TestPostgresLocalRestartConvergenceRejectsChangedProvenance(t *testing.T) {
	store, pool := openRunnerTestStore(t)
	resetRunnerTestData(t, pool)
	const (
		userID    = "48567890-4856-4456-8456-48567890abcd"
		problemID = "runner-recovery-provenance"
		revision  = "4848484848484848484848484848484848484848484848484848484848484848"
		sessionID = "49678901-4967-4567-8567-4967890abcde"
	)
	seedRunnerTestIdentity(t, pool, userID, problemID, revision)
	reservation, err := store.ReserveSession(context.Background(), integrationReservation(
		userID, problemID, revision, sessionID, "create-recovery-provenance",
	))
	if err != nil {
		t.Fatal(err)
	}
	ref := reservation.Allocation.Ref
	snapshot, err := store.SnapshotLocalRecovery(context.Background(), "local-docker:integration")
	if err != nil || len(snapshot) != 1 {
		t.Fatalf("snapshot=%+v err=%v", snapshot, err)
	}
	if _, err := pool.Exec(context.Background(), `
		UPDATE runner_allocations SET resource_profile='changed-after-snapshot' WHERE id=$1`, ref.ID); err != nil {
		t.Fatal(err)
	}
	converged, err := store.ConvergeLocalRestartSnapshot(
		context.Background(), "local-docker:integration", snapshot,
	)
	if converged != 0 || !errors.Is(err, ErrLifecycleConflict) {
		t.Fatalf("changed provenance convergence=%d err=%v, want 0 lifecycle conflict", converged, err)
	}
	var desired, observed string
	if err := pool.QueryRow(context.Background(), `
		SELECT desired_state,observed_state FROM runner_allocations WHERE id=$1`, ref.ID,
	).Scan(&desired, &observed); err != nil {
		t.Fatal(err)
	}
	if desired == "absent" || observed == "absent" {
		t.Fatalf("changed provenance was converged: desired=%s observed=%s", desired, observed)
	}
}

func TestPostgresLocalRestartConvergenceRejectsEveryMismatchedCatalogField(t *testing.T) {
	store, pool := openRunnerTestStore(t)
	resetRunnerTestData(t, pool)
	const (
		userID    = "50567890-5056-4456-8456-50567890abcd"
		problemID = "runner-recovery-catalog-fields"
		revision  = "5050505050505050505050505050505050505050505050505050505050505050"
		sessionID = "50678901-5067-4567-8567-5067890abcde"
	)
	seedRunnerTestIdentity(t, pool, userID, problemID, revision)
	reservation, err := store.ReserveSession(context.Background(), integrationReservation(
		userID, problemID, revision, sessionID, "create-recovery-catalog-fields",
	))
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.SnapshotLocalRecovery(context.Background(), "local-docker:integration")
	if err != nil || len(snapshot) != 1 {
		t.Fatalf("snapshot=%+v err=%v", snapshot, err)
	}

	tests := []struct {
		name   string
		mutate func(*LocalRecoveryAllocation)
	}{
		{name: "catalog generation", mutate: func(recovery *LocalRecoveryAllocation) { recovery.Selection.Generation++ }},
		{name: "problem id", mutate: func(recovery *LocalRecoveryAllocation) { recovery.Selection.Problem.ID = "different-problem" }},
		{name: "problem revision", mutate: func(recovery *LocalRecoveryAllocation) { recovery.Selection.Problem.Revision = strings.Repeat("f", 64) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			changed := snapshot[0]
			tt.mutate(&changed)
			converged, err := store.ConvergeLocalRestartSnapshot(
				context.Background(), "local-docker:integration", []LocalRecoveryAllocation{changed},
			)
			if converged != 0 || !errors.Is(err, ErrLifecycleConflict) {
				t.Fatalf("convergence=%d err=%v, want 0 lifecycle conflict", converged, err)
			}
		})
	}

	var desired, observed string
	if err := pool.QueryRow(context.Background(), `
		SELECT desired_state,observed_state FROM runner_allocations WHERE id=$1`, reservation.Allocation.Ref.ID,
	).Scan(&desired, &observed); err != nil {
		t.Fatal(err)
	}
	if desired == "absent" || observed == "absent" {
		t.Fatalf("mismatched catalog provenance was converged: desired=%s observed=%s", desired, observed)
	}
}

func TestPostgresLocalRestartSnapshotRejectsLegacyNullCatalogProvenanceWithoutMutation(t *testing.T) {
	store, pool := openRunnerTestStore(t)
	resetRunnerTestData(t, pool)
	const (
		userID    = "51567890-5156-4456-8456-51567890abcd"
		problemID = "runner-recovery-null-catalog"
		revision  = "5151515151515151515151515151515151515151515151515151515151515151"
		sessionID = "51678901-5167-4567-8567-5167890abcde"
	)
	seedRunnerTestIdentity(t, pool, userID, problemID, revision)
	reservation, err := store.ReserveSession(context.Background(), integrationReservation(
		userID, problemID, revision, sessionID, "create-recovery-null-catalog",
	))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `ALTER TABLE sessions DISABLE TRIGGER sessions_catalog_selection_guard`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), `ALTER TABLE sessions ENABLE TRIGGER sessions_catalog_selection_guard`); err != nil {
			t.Errorf("restore session catalog guard: %v", err)
		}
	})
	if _, err := pool.Exec(context.Background(), `UPDATE sessions SET catalog_generation=NULL WHERE id=$1`, sessionID); err != nil {
		t.Fatal(err)
	}

	if snapshot, err := store.SnapshotLocalRecovery(context.Background(), "local-docker:integration"); err == nil || len(snapshot) != 0 {
		t.Fatalf("legacy NULL provenance snapshot=%+v err=%v, want fail-closed", snapshot, err)
	}
	var desired, observed string
	if err := pool.QueryRow(context.Background(), `
		SELECT desired_state,observed_state FROM runner_allocations WHERE id=$1`, reservation.Allocation.Ref.ID,
	).Scan(&desired, &observed); err != nil {
		t.Fatal(err)
	}
	if desired == "absent" || observed == "absent" {
		t.Fatalf("legacy NULL snapshot mutated allocation: desired=%s observed=%s", desired, observed)
	}
}

func TestPostgresResetReservationIsAtomicAndReplayable(t *testing.T) {
	store, pool := openRunnerTestStore(t)
	resetRunnerTestData(t, pool)
	const (
		userID    = "56789012-5678-4678-8678-567890abcdef"
		problemID = "runner-reset-atomic"
		revision  = "5555555555555555555555555555555555555555555555555555555555555555"
		sessionID = "67890123-6789-4789-8789-67890abcdef0"
	)
	seedRunnerTestIdentity(t, pool, userID, problemID, revision)
	startParams := integrationReservation(userID, problemID, revision, sessionID, "create-reset-atomic")
	start, err := store.ReserveSession(context.Background(), startParams)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkCreateSucceeded(context.Background(), start.Allocation.Ref); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkReady(context.Background(), start.Allocation.Ref); err != nil {
		t.Fatal(err)
	}

	params := ReserveResetParams{
		Expected: start.Allocation.Ref.Session, UserID: userID,
		Provider: ProviderLocalDocker, ProviderID: startParams.ProviderID,
		IdempotencyKey: "reset:" + sessionID + ":1:atomic",
	}
	first, err := store.ReserveReset(context.Background(), params)
	if err != nil {
		t.Fatalf("reserve reset: %v", err)
	}
	second, err := store.ReserveReset(context.Background(), params)
	if err != nil {
		t.Fatalf("replay reset: %v", err)
	}
	if first.Old != start.Allocation.Ref || first.New.Allocation.Ref != second.New.Allocation.Ref ||
		first.New.Operation.ID != second.New.Operation.ID || first.New.AttemptID != second.New.AttemptID {
		t.Fatalf("reset replay changed identity: first=%+v second=%+v", first, second)
	}
	if first.New.Allocation.Ref.Session.Generation != 2 || first.New.Session.CurrentGeneration != 2 {
		t.Fatalf("reset replacement generation = %+v", first.New)
	}

	var currentGeneration int64
	var sessionState, sessionDesired, oldDesired, oldObserved, newDesired, newObserved string
	var oldAttempt, newAttempt, destroyOperation, createOperation string
	if err := pool.QueryRow(context.Background(), `SELECT
		(SELECT current_generation FROM sessions WHERE id=$1),
		(SELECT state FROM sessions WHERE id=$1),
		(SELECT desired_state FROM sessions WHERE id=$1),
		(SELECT desired_state FROM runner_allocations WHERE session_id=$1 AND generation=1),
		(SELECT observed_state FROM runner_allocations WHERE session_id=$1 AND generation=1),
		(SELECT desired_state FROM runner_allocations WHERE session_id=$1 AND generation=2),
		(SELECT observed_state FROM runner_allocations WHERE session_id=$1 AND generation=2),
		(SELECT status FROM attempts WHERE session_id=$1 AND generation=1),
		(SELECT status FROM attempts WHERE session_id=$1 AND generation=2),
		(SELECT state FROM runner_operations WHERE allocation_id=$2 AND kind='destroy'),
		(SELECT state FROM runner_operations WHERE allocation_id=$3 AND kind='create')`,
		sessionID, first.Old.ID, first.New.Allocation.Ref.ID,
	).Scan(&currentGeneration, &sessionState, &sessionDesired, &oldDesired, &oldObserved,
		&newDesired, &newObserved, &oldAttempt, &newAttempt, &destroyOperation, &createOperation); err != nil {
		t.Fatal(err)
	}
	if currentGeneration != 2 || sessionState != "queued" || sessionDesired != "active" ||
		oldDesired != "absent" || oldObserved != "deleting" || newDesired != "active" || newObserved != "unknown" ||
		oldAttempt != "failed" || newAttempt != "in_progress" || destroyOperation != "pending" || createOperation != "pending" {
		t.Fatalf("atomic reset state generation=%d session=%s/%s old=%s/%s new=%s/%s attempts=%s/%s operations=%s/%s",
			currentGeneration, sessionState, sessionDesired, oldDesired, oldObserved, newDesired, newObserved,
			oldAttempt, newAttempt, destroyOperation, createOperation)
	}
	assertLifecycleEventCount(t, pool, first.Old.ID, "reset_requested", 1)
	assertLifecycleEventCount(t, pool, first.Old.ID, "destroying", 1)
	assertLifecycleEventCount(t, pool, first.New.Allocation.Ref.ID, "allocation_reserved", 1)
	assertLifecycleEventCount(t, pool, first.New.Allocation.Ref.ID, "reset_requested", 1)
}

func TestPostgresResetCompetingKeysAdvanceOneGeneration(t *testing.T) {
	store, pool := openRunnerTestStore(t)
	resetRunnerTestData(t, pool)
	const (
		userID    = "78901234-7890-4890-8890-7890abcdef01"
		problemID = "runner-reset-race"
		revision  = "6666666666666666666666666666666666666666666666666666666666666666"
		sessionID = "89012345-8901-4901-8901-890abcdef012"
	)
	seedRunnerTestIdentity(t, pool, userID, problemID, revision)
	startParams := integrationReservation(userID, problemID, revision, sessionID, "create-reset-race")
	start, err := store.ReserveSession(context.Background(), startParams)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkCreateSucceeded(context.Background(), start.Allocation.Ref); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkReady(context.Background(), start.Allocation.Ref); err != nil {
		t.Fatal(err)
	}

	params := []ReserveResetParams{
		{Expected: start.Allocation.Ref.Session, UserID: userID, Provider: ProviderLocalDocker, ProviderID: startParams.ProviderID, IdempotencyKey: "reset-race-a"},
		{Expected: start.Allocation.Ref.Session, UserID: userID, Provider: ProviderLocalDocker, ProviderID: startParams.ProviderID, IdempotencyKey: "reset-race-b"},
	}
	startRace := make(chan struct{})
	errs := make([]error, len(params))
	var wg sync.WaitGroup
	for index := range params {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-startRace
			_, errs[i] = store.ReserveReset(context.Background(), params[i])
		}(index)
	}
	close(startRace)
	wg.Wait()
	success, stale := 0, 0
	for _, resetErr := range errs {
		switch {
		case resetErr == nil:
			success++
		case errors.Is(resetErr, ErrGenerationStale), errors.Is(resetErr, ErrLifecycleConflict):
			stale++
		default:
			t.Fatalf("unexpected reset race error: %v", resetErr)
		}
	}
	if success != 1 || stale != 1 {
		t.Fatalf("reset race results success=%d stale=%d errors=%v", success, stale, errs)
	}
	var currentGeneration, allocations, attempts int
	if err := pool.QueryRow(context.Background(), `SELECT
		(SELECT current_generation FROM sessions WHERE id=$1),
		(SELECT COUNT(*) FROM runner_allocations WHERE session_id=$1),
		(SELECT COUNT(*) FROM attempts WHERE session_id=$1)`, sessionID,
	).Scan(&currentGeneration, &allocations, &attempts); err != nil {
		t.Fatal(err)
	}
	if currentGeneration != 2 || allocations != 2 || attempts != 2 {
		t.Fatalf("reset race durable rows generation=%d allocations=%d attempts=%d", currentGeneration, allocations, attempts)
	}
}

func TestPostgresResetRejectsSourceWhoseCreateIsPending(t *testing.T) {
	store, pool := openRunnerTestStore(t)
	resetRunnerTestData(t, pool)
	const (
		userID    = "90123456-9012-4012-8012-9012abcdef01"
		problemID = "runner-reset-pending"
		revision  = "7777777777777777777777777777777777777777777777777777777777777777"
		sessionID = "01234567-0123-4123-8123-012345abcdef"
	)
	seedRunnerTestIdentity(t, pool, userID, problemID, revision)
	startParams := integrationReservation(userID, problemID, revision, sessionID, "create-reset-pending")
	start, err := store.ReserveSession(context.Background(), startParams)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.ReserveReset(context.Background(), ReserveResetParams{
		Expected: start.Allocation.Ref.Session, UserID: userID,
		Provider: ProviderLocalDocker, ProviderID: startParams.ProviderID,
		IdempotencyKey: "reset-source-pending",
	})
	if !errors.Is(err, ErrLifecycleConflict) {
		t.Fatalf("pending source reset error = %v, want lifecycle conflict", err)
	}
	var generation, allocations, attempts int
	if err := pool.QueryRow(context.Background(), `SELECT
		(SELECT current_generation FROM sessions WHERE id=$1),
		(SELECT COUNT(*) FROM runner_allocations WHERE session_id=$1),
		(SELECT COUNT(*) FROM attempts WHERE session_id=$1)`, sessionID,
	).Scan(&generation, &allocations, &attempts); err != nil {
		t.Fatal(err)
	}
	if generation != 1 || allocations != 1 || attempts != 1 {
		t.Fatalf("pending source mutated reset state generation=%d allocations=%d attempts=%d", generation, allocations, attempts)
	}
}

func TestPostgresResetRejectsWhenOlderAllocationIsNotAbsent(t *testing.T) {
	store, pool := openRunnerTestStore(t)
	resetRunnerTestData(t, pool)
	const (
		userID    = "12345678-90ab-4cde-8f01-234567890abc"
		problemID = "runner-reset-older-live"
		revision  = "8888888888888888888888888888888888888888888888888888888888888888"
		sessionID = "23456789-0abc-4def-8012-34567890abcd"
	)
	seedRunnerTestIdentity(t, pool, userID, problemID, revision)
	startParams := integrationReservation(userID, problemID, revision, sessionID, "create-reset-older-live")
	start, err := store.ReserveSession(context.Background(), startParams)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkCreateSucceeded(context.Background(), start.Allocation.Ref); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkReady(context.Background(), start.Allocation.Ref); err != nil {
		t.Fatal(err)
	}
	first, err := store.ReserveReset(context.Background(), ReserveResetParams{
		Expected: start.Allocation.Ref.Session, UserID: userID,
		Provider: ProviderLocalDocker, ProviderID: startParams.ProviderID,
		IdempotencyKey: "reset-leaves-older-live",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Simulate an unsafe controller that advanced the replacement before the
	// old provider allocation was proven absent. ReserveReset must still fence
	// the next generation from durable state alone.
	if err := store.MarkCreateSucceeded(context.Background(), first.New.Allocation.Ref); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkReady(context.Background(), first.New.Allocation.Ref); err != nil {
		t.Fatal(err)
	}
	_, err = store.ReserveReset(context.Background(), ReserveResetParams{
		Expected: first.New.Allocation.Ref.Session, UserID: userID,
		Provider: ProviderLocalDocker, ProviderID: startParams.ProviderID,
		IdempotencyKey: "reset-must-see-older-live",
	})
	if !errors.Is(err, ErrLifecycleConflict) {
		t.Fatalf("older non-absent reset error = %v, want lifecycle conflict", err)
	}
	var generation, allocations, attempts int
	if err := pool.QueryRow(context.Background(), `SELECT
		(SELECT current_generation FROM sessions WHERE id=$1),
		(SELECT COUNT(*) FROM runner_allocations WHERE session_id=$1),
		(SELECT COUNT(*) FROM attempts WHERE session_id=$1)`, sessionID,
	).Scan(&generation, &allocations, &attempts); err != nil {
		t.Fatal(err)
	}
	if generation != 2 || allocations != 2 || attempts != 2 {
		t.Fatalf("older allocation fence mutated state generation=%d allocations=%d attempts=%d", generation, allocations, attempts)
	}
}

func TestPostgresDestroyWorkerClaimsOnceRejectsExpiredOwnerAndUnlocksReplacement(t *testing.T) {
	store, pool := openRunnerTestStore(t)
	resetRunnerTestData(t, pool)
	const (
		userID    = "34567890-abcd-4ef0-8123-456789abcdef"
		problemID = "runner-reset-worker"
		revision  = "9999999999999999999999999999999999999999999999999999999999999999"
		sessionID = "45678901-bcde-4f01-8234-56789abcdef0"
	)
	seedRunnerTestIdentity(t, pool, userID, problemID, revision)
	startParams := integrationReservation(userID, problemID, revision, sessionID, "create-reset-worker")
	start, err := store.ReserveSession(context.Background(), startParams)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkCreateSucceeded(context.Background(), start.Allocation.Ref); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkReady(context.Background(), start.Allocation.Ref); err != nil {
		t.Fatal(err)
	}
	reset, err := store.ReserveReset(context.Background(), ReserveResetParams{
		Expected: start.Allocation.Ref.Session, UserID: userID,
		Provider: ProviderLocalDocker, ProviderID: startParams.ProviderID,
		IdempotencyKey: "reset-worker-recovery",
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, found, err := store.ClaimProvisionableReset(context.Background(), startParams.ProviderID, "", "create-worker-before-absence", 30*time.Second); err != nil {
		t.Fatal(err)
	} else if found {
		t.Fatal("replacement create became claimable before old absence")
	}

	type destroyResult struct {
		claim DestroyWorkClaim
		found bool
		err   error
	}
	begin := make(chan struct{})
	results := make(chan destroyResult, 2)
	for _, owner := range []string{"destroy-worker-a", "destroy-worker-b"} {
		owner := owner
		go func() {
			<-begin
			claim, found, claimErr := store.ClaimDestroyWork(context.Background(), startParams.ProviderID, reset.Old.ID, owner, 30*time.Second)
			results <- destroyResult{claim: claim, found: found, err: claimErr}
		}()
	}
	close(begin)
	var first, concurrentPending DestroyWorkClaim
	claimed, pending := 0, 0
	for range 2 {
		result := <-results
		if result.err != nil {
			t.Fatalf("concurrent destroy claim: %v", result.err)
		}
		if !result.found {
			t.Fatal("exact concurrent destroy lookup returned neither a claim nor pending observation")
		}
		if result.claim.Pending {
			pending++
			concurrentPending = result.claim
		} else {
			claimed++
			first = result.claim
		}
	}
	if claimed != 1 || pending != 1 || first.Ref != reset.Old || first.Attempt != 1 ||
		first.LeaseToken == "" || concurrentPending.Ref != reset.Old || concurrentPending.LeaseToken != "" {
		t.Fatalf("concurrent destroy results claimed=%d pending=%d first=%+v observation=%+v",
			claimed, pending, first, concurrentPending)
	}
	if err := store.MarkDestroyed(context.Background(), first.Ref); !errors.Is(err, ErrLifecycleConflict) {
		t.Fatalf("unclaimed completion during active destroy lease error=%v, want lifecycle conflict", err)
	}
	pendingReplay, found, err := store.ClaimDestroyWork(
		context.Background(), startParams.ProviderID, reset.Old.ID, "destroy-pending-observer", 30*time.Second,
	)
	if err != nil || !found {
		t.Fatalf("load pending destroy observation found=%v err=%v", found, err)
	}
	if !pendingReplay.Pending || pendingReplay.Completed || pendingReplay.Ref != reset.Old ||
		pendingReplay.LeaseToken != "" || pendingReplay.LeaseOwner != "" || !pendingReplay.LeaseExpiresAt.IsZero() {
		t.Fatalf("pending destroy observation = %+v", pendingReplay)
	}
	if err := store.MarkClaimedDestroyed(context.Background(), pendingReplay); err == nil {
		t.Fatal("pending observation was accepted as a destroy mutation claim")
	}

	if _, err := pool.Exec(context.Background(), `
		UPDATE runner_operations SET lease_expires_at=NOW()-INTERVAL '1 second'
		WHERE id=$1 AND lease_token=$2::uuid`, first.OperationID, first.LeaseToken); err != nil {
		t.Fatal(err)
	}
	second, found, err := store.ClaimDestroyWork(context.Background(), startParams.ProviderID, reset.Old.ID, "destroy-worker-successor", 30*time.Second)
	if err != nil || !found {
		t.Fatalf("reclaim expired destroy work found=%v err=%v", found, err)
	}
	if second.OperationID != first.OperationID || second.LeaseToken == first.LeaseToken || second.Attempt != 2 {
		t.Fatalf("expired destroy claim was not replaced: first=%+v second=%+v", first, second)
	}
	if err := store.MarkClaimedDestroyed(context.Background(), first); !errors.Is(err, ErrLifecycleConflict) {
		t.Fatalf("expired worker completion error=%v, want lifecycle conflict", err)
	}
	if err := store.RetryDestroyWork(context.Background(), first, errors.New("late failure"), 0); !errors.Is(err, ErrLifecycleConflict) {
		t.Fatalf("expired worker retry error=%v, want lifecycle conflict", err)
	}
	if err := store.MarkClaimedDestroyed(context.Background(), second); err != nil {
		t.Fatalf("complete successor destroy claim: %v", err)
	}
	destroyReplay, found, err := store.ClaimDestroyWork(
		context.Background(), startParams.ProviderID, reset.Old.ID, "destroy-replay", 30*time.Second,
	)
	if err != nil || !found {
		t.Fatalf("load completed destroy replay found=%v err=%v", found, err)
	}
	if !destroyReplay.Completed || destroyReplay.Ref != reset.Old || destroyReplay.LeaseToken != "" {
		t.Fatalf("completed destroy replay = %+v", destroyReplay)
	}

	createClaim, found, err := store.ClaimProvisionableReset(context.Background(), startParams.ProviderID, reset.New.Operation.ID, "create-worker-a", 30*time.Second)
	if err != nil || !found {
		t.Fatalf("claim reset replacement after absence found=%v err=%v", found, err)
	}
	if createClaim.Reservation.Allocation.Ref != reset.New.Allocation.Ref || createClaim.OperationID != reset.New.Operation.ID || createClaim.Attempt != 1 {
		t.Fatalf("claimed wrong reset replacement: %+v", createClaim)
	}
	if _, found, err := store.ClaimProvisionableReset(context.Background(), startParams.ProviderID, "", "create-worker-b", 30*time.Second); err != nil {
		t.Fatal(err)
	} else if found {
		t.Fatal("second worker claimed an actively leased replacement")
	}
	if _, err := pool.Exec(context.Background(), `
		UPDATE runner_operations SET lease_expires_at=NOW()-INTERVAL '1 second'
		WHERE id=$1 AND lease_token=$2::uuid`, createClaim.OperationID, createClaim.LeaseToken); err != nil {
		t.Fatal(err)
	}
	createSuccessor, found, err := store.ClaimProvisionableReset(
		context.Background(), startParams.ProviderID, reset.New.Operation.ID, "create-worker-successor", 30*time.Second,
	)
	if err != nil || !found {
		t.Fatalf("reclaim expired create work found=%v err=%v", found, err)
	}
	if createSuccessor.OperationID != createClaim.OperationID || createSuccessor.LeaseToken == createClaim.LeaseToken || createSuccessor.Attempt != 2 {
		t.Fatalf("expired create claim was not replaced: first=%+v second=%+v", createClaim, createSuccessor)
	}
	if err := store.MarkClaimedCreateSucceeded(context.Background(), createClaim); !errors.Is(err, ErrLifecycleConflict) {
		t.Fatalf("expired create worker completion error=%v, want lifecycle conflict", err)
	}
	if err := store.MarkCreateSucceeded(context.Background(), createSuccessor.Reservation.Allocation.Ref); !errors.Is(err, ErrLifecycleConflict) {
		t.Fatalf("unclaimed completion during active lease error=%v, want lifecycle conflict", err)
	}
	if err := store.MarkClaimedCreateSucceeded(context.Background(), createSuccessor); err != nil {
		t.Fatalf("complete claimed replacement create: %v", err)
	}
	if err := store.MarkClaimedCreateSucceeded(context.Background(), createClaim); !errors.Is(err, ErrLifecycleConflict) {
		t.Fatalf("expired create worker terminal replay error=%v, want lifecycle conflict", err)
	}
	if err := store.MarkClaimedCreateFailed(context.Background(), createClaim, "late_failure", "late stale failure"); !errors.Is(err, ErrLifecycleConflict) {
		t.Fatalf("expired create worker failure replay error=%v, want lifecycle conflict", err)
	}

	var oldObserved, destroyState, createState string
	var destroyLease, createLease *string
	if err := pool.QueryRow(context.Background(), `SELECT
		(SELECT observed_state FROM runner_allocations WHERE id=$1),
		(SELECT state FROM runner_operations WHERE allocation_id=$1 AND kind='destroy'),
		(SELECT lease_token::text FROM runner_operations WHERE allocation_id=$1 AND kind='destroy'),
		(SELECT state FROM runner_operations WHERE allocation_id=$2 AND kind='create'),
		(SELECT lease_token::text FROM runner_operations WHERE allocation_id=$2 AND kind='create')`,
		reset.Old.ID, reset.New.Allocation.Ref.ID,
	).Scan(&oldObserved, &destroyState, &destroyLease, &createState, &createLease); err != nil {
		t.Fatal(err)
	}
	if oldObserved != "absent" || destroyState != "succeeded" || destroyLease != nil || createState != "succeeded" || createLease != nil {
		t.Fatalf("worker convergence old=%s destroy=%s/%v create=%s/%v",
			oldObserved, destroyState, destroyLease, createState, createLease)
	}
}

func assertLifecycleEventCount(t *testing.T, pool *pgxpool.Pool, allocationID, eventType string, want int) {
	t.Helper()
	var got int
	if err := pool.QueryRow(context.Background(), `
		SELECT COUNT(*) FROM session_events WHERE allocation_id=$1 AND event_type=$2`,
		allocationID, eventType).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("%s event count = %d, want %d", eventType, got, want)
	}
}

func TestControllerLeaseAllowsOneOwnerPerProviderInstance(t *testing.T) {
	_, pool := openRunnerTestStore(t)
	providerID := "local-docker:lease-" + newTestIdentity(t)

	first, err := acquireControllerLease(context.Background(), pool, providerID, 20*time.Millisecond, func(error) {})
	if err != nil {
		t.Fatalf("acquire first controller lease: %v", err)
	}
	second, err := acquireControllerLease(context.Background(), pool, providerID, 20*time.Millisecond, func(error) {})
	if !errors.Is(err, ErrControllerLeaseHeld) {
		if second != nil {
			_ = second.Close(context.Background())
		}
		t.Fatalf("second controller lease error = %v, want ErrControllerLeaseHeld", err)
	}

	closeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := first.Close(closeCtx); err != nil {
		t.Fatalf("release first controller lease: %v", err)
	}
	select {
	case loss := <-first.Lost():
		t.Fatalf("normal controller lease close reported loss: %v", loss)
	default:
	}
	third, err := acquireControllerLease(context.Background(), pool, providerID, 20*time.Millisecond, func(error) {})
	if err != nil {
		t.Fatalf("reacquire controller lease after release: %v", err)
	}
	if err := third.Close(closeCtx); err != nil {
		t.Fatalf("release reacquired controller lease: %v", err)
	}
}

func TestControllerLeaseConcurrentCloseSharesResult(t *testing.T) {
	_, pool := openRunnerTestStore(t)
	providerID := "local-docker:concurrent-close-" + newTestIdentity(t)
	lease, err := acquireControllerLease(context.Background(), pool, providerID, time.Second, func(error) {
		t.Error("normal concurrent close invoked loss fence")
	})
	if err != nil {
		t.Fatal(err)
	}

	const callers = 8
	results := make(chan error, callers)
	start := make(chan struct{})
	for i := 0; i < callers; i++ {
		go func() {
			<-start
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			results <- lease.Close(ctx)
		}()
	}
	close(start)
	for i := 0; i < callers; i++ {
		if err := <-results; err != nil {
			t.Fatalf("concurrent close result %d: %v", i, err)
		}
	}
	select {
	case loss := <-lease.Lost():
		t.Fatalf("normal concurrent close reported loss: %v", loss)
	default:
	}

	replacement, err := acquireControllerLease(context.Background(), pool, providerID, time.Second, func(error) {})
	if err != nil {
		t.Fatalf("reacquire after concurrent close: %v", err)
	}
	if err := replacement.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestControllerLeaseUnlockFailureDiscardsConnectionWithoutLossSignal(t *testing.T) {
	_, pool := openRunnerTestStore(t)
	providerID := "local-docker:unlock-failure-" + newTestIdentity(t)
	lossFenceCalls := 0
	lease, err := acquireControllerLease(context.Background(), pool, providerID, time.Hour, func(error) {
		lossFenceCalls++
	})
	if err != nil {
		t.Fatal(err)
	}
	backendPID := lease.backendPID

	var terminated bool
	if err := pool.QueryRow(context.Background(), `SELECT pg_terminate_backend($1)`, backendPID).Scan(&terminated); err != nil {
		t.Fatal(err)
	}
	if !terminated {
		t.Fatalf("lease backend %d was not terminated", backendPID)
	}
	closeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := lease.Close(closeCtx); err == nil || errors.Is(err, ErrControllerLeaseLost) {
		t.Fatalf("close after unlock failure error = %v", err)
	}
	if lossFenceCalls != 0 {
		t.Fatalf("normal close unlock failure invoked loss fence %d times", lossFenceCalls)
	}
	select {
	case loss := <-lease.Lost():
		t.Fatalf("normal close unlock failure reported loss: %v", loss)
	default:
	}

	replacement, err := acquireControllerLease(context.Background(), pool, providerID, time.Second, func(error) {})
	if err != nil {
		t.Fatalf("reacquire after unlock failure: %v", err)
	}
	if err := replacement.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestControllerLeaseLossFencesBeforeNotificationAndAllowsReacquire(t *testing.T) {
	_, pool := openRunnerTestStore(t)
	providerID := "local-docker:forced-loss-" + newTestIdentity(t)
	fenced := make(chan error, 1)
	lease, err := acquireControllerLease(context.Background(), pool, providerID, 20*time.Millisecond, func(loss error) {
		fenced <- loss
	})
	if err != nil {
		t.Fatal(err)
	}
	backendPID := lease.backendPID
	var terminated bool
	if err := pool.QueryRow(context.Background(), `SELECT pg_terminate_backend($1)`, backendPID).Scan(&terminated); err != nil {
		t.Fatal(err)
	}
	if !terminated {
		t.Fatalf("lease backend %d was not terminated", backendPID)
	}

	select {
	case loss := <-lease.Lost():
		if !errors.Is(loss, ErrControllerLeaseLost) {
			t.Fatalf("lost error = %v", loss)
		}
		select {
		case fenceLoss := <-fenced:
			if !errors.Is(fenceLoss, ErrControllerLeaseLost) {
				t.Fatalf("fence error = %v", fenceLoss)
			}
		default:
			t.Fatal("lease loss became observable before synchronous fence callback")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("forced lease loss was not detected")
	}

	replacement, err := acquireControllerLease(context.Background(), pool, providerID, 20*time.Millisecond, func(error) {})
	if err != nil {
		t.Fatalf("reacquire after forced loss: %v", err)
	}
	closeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := replacement.Close(closeCtx); err != nil {
		t.Fatalf("close replacement lease: %v", err)
	}
	if err := lease.Close(closeCtx); !errors.Is(err, ErrControllerLeaseLost) {
		t.Fatalf("close lost lease error = %v", err)
	}

	for i := 0; i < 5; i++ {
		conn, err := pool.Acquire(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		var pid int32
		err = conn.QueryRow(context.Background(), `SELECT pg_backend_pid()`).Scan(&pid)
		conn.Release()
		if err != nil {
			t.Fatal(err)
		}
		if pid == backendPID {
			t.Fatalf("terminated lease backend %d returned to pool", backendPID)
		}
	}
}

func TestControllerLeaseLossFencePanicStillDiscardsAndNotifies(t *testing.T) {
	_, pool := openRunnerTestStore(t)
	providerID := "local-docker:panic-fence-" + newTestIdentity(t)
	lease, err := acquireControllerLease(context.Background(), pool, providerID, 20*time.Millisecond, func(error) {
		panic("test fence panic")
	})
	if err != nil {
		t.Fatal(err)
	}
	backendPID := lease.backendPID
	var terminated bool
	if err := pool.QueryRow(context.Background(), `SELECT pg_terminate_backend($1)`, backendPID).Scan(&terminated); err != nil {
		t.Fatal(err)
	}
	if !terminated {
		t.Fatalf("lease backend %d was not terminated", backendPID)
	}

	select {
	case loss := <-lease.Lost():
		if !errors.Is(loss, ErrControllerLeaseLost) || !strings.Contains(loss.Error(), "loss fence panicked") {
			t.Fatalf("panic fence loss error = %v", loss)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("forced lease loss with panic fence was not detected")
	}

	replacement, err := acquireControllerLease(context.Background(), pool, providerID, time.Second, func(error) {})
	if err != nil {
		t.Fatalf("reacquire after panic fence: %v", err)
	}
	if err := replacement.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestControllerEpochSerializesTakeoverAndRejectsStaleStore(t *testing.T) {
	_, pool := openRunnerTestStore(t)
	resetRunnerTestData(t, pool)
	providerID := "local-docker:epoch-" + newTestIdentity(t)
	oldLease, err := acquireControllerLease(context.Background(), pool, providerID, time.Hour, func(error) {})
	if err != nil {
		t.Fatal(err)
	}
	oldStore, err := NewPostgresStore(pool, oldLease.Fence())
	if err != nil {
		t.Fatal(err)
	}

	// Model a lifecycle mutation that has entered under the old epoch. Holding
	// this row lock means a successor cannot issue its epoch (and therefore
	// cannot snapshot recovery state) until the old transaction ends.
	oldTx, err := pool.BeginTx(context.Background(), pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = oldTx.Rollback(context.Background()) }()
	if err := oldStore.lockControllerEpochTx(context.Background(), oldTx); err != nil {
		t.Fatalf("lock old controller epoch: %v", err)
	}

	closeResult := make(chan error, 1)
	go func() {
		closeCtx, cancelClose := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelClose()
		closeResult <- oldLease.Close(closeCtx)
	}()
	select {
	case err := <-closeResult:
		t.Fatalf("old lease closed before its epoch transaction drained: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	if err := oldTx.Commit(context.Background()); err != nil {
		t.Fatalf("commit old epoch transaction: %v", err)
	}
	if err := <-closeResult; err != nil {
		t.Fatalf("release old advisory lease after drain: %v", err)
	}

	type acquireResult struct {
		lease *ControllerLease
		err   error
	}
	takeover := make(chan acquireResult, 1)
	go func() {
		lease, acquireErr := acquireControllerLease(context.Background(), pool, providerID, time.Hour, func(error) {})
		takeover <- acquireResult{lease: lease, err: acquireErr}
	}()
	result := <-takeover
	if result.err != nil {
		t.Fatalf("acquire successor controller: %v", result.err)
	}
	newLease := result.lease
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := newLease.Close(ctx); err != nil {
			t.Errorf("close successor controller: %v", err)
		}
	})
	if newLease.Fence().Epoch != oldLease.Fence().Epoch+1 {
		t.Fatalf("successor epoch=%d old=%d", newLease.Fence().Epoch, oldLease.Fence().Epoch)
	}

	const (
		userID    = "c1111111-1111-4111-8111-111111111111"
		problemID = "epoch-test"
		revision  = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
		sessionID = "c2222222-2222-4222-8222-222222222222"
	)
	seedRunnerTestIdentity(t, pool, userID, problemID, revision)
	params := integrationReservation(userID, problemID, revision, sessionID, "create-stale-epoch")
	params.ProviderID = providerID
	if _, err := oldStore.ReserveSession(context.Background(), params); !errors.Is(err, ErrControllerFenced) {
		t.Fatalf("stale store mutation error=%v, want ErrControllerFenced", err)
	}
	var sessions int
	if err := pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM sessions WHERE id=$1`, sessionID).Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if sessions != 0 {
		t.Fatalf("stale store committed %d session rows", sessions)
	}

	newStore, err := NewPostgresStore(pool, newLease.Fence())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := newStore.ReserveSession(context.Background(), params); err != nil {
		t.Fatalf("successor store mutation: %v", err)
	}
}

func newTestIdentity(t *testing.T) string {
	t.Helper()
	id, err := newUUID()
	if err != nil {
		t.Fatal(err)
	}
	return id
}
