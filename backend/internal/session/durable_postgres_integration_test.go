package session

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/k8s-quiz/backend/internal/runner"
	"github.com/k8s-quiz/backend/pkg/dbmigrate"
	"github.com/k8s-quiz/backend/pkg/models"
)

const sessionRecoveryTestDatabaseEnv = "SESSION_RECOVERY_TEST_DATABASE_URL"

const sessionRecoveryRevision = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

var sessionRecoverySchemaSequence atomic.Uint64

type postgresRecoveryFixture struct {
	t          *testing.T
	pool       *pgxpool.Pool
	providerID string
	problem    *mockProblemStore
}

func openPostgresRecoveryFixture(t *testing.T) *postgresRecoveryFixture {
	t.Helper()
	databaseURL := os.Getenv(sessionRecoveryTestDatabaseEnv)
	if databaseURL == "" {
		t.Skip(sessionRecoveryTestDatabaseEnv + " is not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	admin, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open session recovery integration database: %v", err)
	}
	if err := admin.Ping(ctx); err != nil {
		admin.Close()
		t.Fatalf("ping session recovery integration database: %v", err)
	}

	sequence := sessionRecoverySchemaSequence.Add(1)
	schema := fmt.Sprintf("session_recovery_%d_%d", time.Now().UnixNano(), sequence)
	identifier := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+identifier); err != nil {
		admin.Close()
		t.Fatalf("create isolated session recovery schema: %v", err)
	}

	parsed, err := url.Parse(databaseURL)
	if err != nil {
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+identifier+" CASCADE")
		admin.Close()
		t.Fatalf("parse session recovery database URL: %v", err)
	}
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	isolatedURL := parsed.String()
	if err := dbmigrate.UpIsolatedTestSchema(isolatedURL); err != nil {
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+identifier+" CASCADE")
		admin.Close()
		t.Fatalf("migrate isolated session recovery schema: %v", err)
	}

	pool, err := pgxpool.New(ctx, isolatedURL)
	if err != nil {
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+identifier+" CASCADE")
		admin.Close()
		t.Fatalf("open isolated session recovery schema: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+identifier+" CASCADE")
		admin.Close()
		t.Fatalf("ping isolated session recovery schema: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer dropCancel()
		if _, err := admin.Exec(dropCtx, "DROP SCHEMA "+identifier+" CASCADE"); err != nil {
			t.Errorf("drop isolated session recovery schema: %v", err)
		}
		admin.Close()
	})

	problemStore := newMockProblemStore()
	problemStore.problems["p1"] = &models.Problem{
		ID: "p1", Revision: sessionRecoveryRevision, TimeoutMinutes: 30, VerifyType: "script",
	}
	fixture := &postgresRecoveryFixture{
		t:          t,
		pool:       pool,
		providerID: "local-docker:session-recovery:" + schema,
		problem:    problemStore,
	}
	fixture.seedIdentity()
	return fixture
}

func (f *postgresRecoveryFixture) seedIdentity() {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO users (id,github_id,username)
		VALUES ('11111111-1111-4111-8111-111111111111',9101,'session-recovery-test')`); err != nil {
		f.t.Fatalf("seed session recovery user: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO problems (id,title,revision,timeout_minutes,verify_type)
		VALUES ('p1','Session recovery test',$1,30,'script')`, sessionRecoveryRevision); err != nil {
		f.t.Fatalf("seed session recovery problem: %v", err)
	}
	tx, err := f.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		f.t.Fatalf("begin session recovery catalog seed: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `
		INSERT INTO problem_catalog_publications
			(generation,previous_generation,digest_schema,candidate_digest,entry_count)
		VALUES (1,NULL,1,decode(repeat('01',32),'hex'),1)`); err != nil {
		f.t.Fatalf("seed session recovery publication: %v", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO problem_artifacts (
			problem_id,problem_revision,artifact_digest_schema,artifact_digest,
			artifact_media_type,artifact_size,source_trust
		) VALUES (
			'p1',$1,1,decode(repeat('02',32),'hex'),
			'application/vnd.k8s-quiz.problem-runtime.v1',1024,'development_checkout'
		)`, sessionRecoveryRevision); err != nil {
		f.t.Fatalf("seed session recovery artifact binding: %v", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO problem_catalog_entries
			(catalog_generation,problem_id,problem_revision,title,category,difficulty,type,timeout_minutes,verify_type)
		VALUES (1,'p1',$1,'Session recovery test','pod','easy','fix',30,'script')`, sessionRecoveryRevision); err != nil {
		f.t.Fatalf("seed session recovery catalog entry: %v", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO problem_catalog_head (singleton,catalog_generation) VALUES (TRUE,1)`); err != nil {
		f.t.Fatalf("seed session recovery catalog head: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		f.t.Fatalf("commit session recovery catalog seed: %v", err)
	}
}

func (f *postgresRecoveryFixture) acquireStore(owner string) (*runner.PostgresStore, *runner.ControllerLease) {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	lease, err := runner.AcquireControllerLease(ctx, f.pool, f.providerID, func(loss error) {
		f.t.Errorf("%s controller lease lost: %v", owner, loss)
	})
	if err != nil {
		f.t.Fatalf("acquire %s controller lease: %v", owner, err)
	}
	store, err := runner.NewPostgresStore(f.pool, lease.Fence())
	if err != nil {
		f.closeLease(lease)
		f.t.Fatalf("construct %s postgres store: %v", owner, err)
	}
	return store, lease
}

func (f *postgresRecoveryFixture) closeLease(lease *runner.ControllerLease) {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := lease.Close(ctx); err != nil {
		f.t.Fatalf("close session recovery controller lease: %v", err)
	}
}

func (f *postgresRecoveryFixture) reserveGenerationOne(store *runner.PostgresStore, sessionID, operationKey string) runner.SessionReservation {
	f.t.Helper()
	reservation, err := store.ReserveSession(context.Background(), runner.ReserveSessionParams{
		SessionID:       sessionID,
		UserID:          "11111111-1111-4111-8111-111111111111",
		Selection:       runner.CatalogSelection{Generation: 1, Problem: runner.ProblemRef{ID: "p1", Revision: sessionRecoveryRevision}},
		Provider:        runner.ProviderLocalDocker,
		ProviderID:      f.providerID,
		ResourceProfile: runner.DefaultResourceProfile,
		ExpiresAt:       time.Now().UTC().Add(time.Hour),
		IdempotencyKey:  operationKey,
	})
	if err != nil {
		f.t.Fatalf("reserve generation one: %v", err)
	}
	return reservation
}

func (f *postgresRecoveryFixture) recoverWithNewController(store *runner.PostgresStore, lease *runner.ControllerLease, runtime *mockRunner) (*Service, *runner.AuthorityGate) {
	f.t.Helper()
	gate := runner.NewAuthorityGate()
	authorized, err := runner.NewAuthorityRunner(runtime, gate, lease.Fence())
	if err != nil {
		f.t.Fatalf("construct recovery authority runner: %v", err)
	}
	svc, err := NewDurablePausedService(authorized, f.problem, f.problem, store, f.providerID)
	if err != nil {
		f.t.Fatalf("construct recovered service: %v", err)
	}
	if err := svc.SetAuthorityGate(gate); err != nil {
		f.t.Fatalf("bind recovered service authority: %v", err)
	}
	if err := svc.RecoverDurableState(context.Background()); err != nil {
		f.t.Fatalf("recover durable service: %v", err)
	}
	return svc, gate
}

func assertRecoveredReservation(t *testing.T, store *runner.PostgresStore, providerID string, want runner.AllocationRef) {
	t.Helper()
	active, err := store.ListActiveReservations(context.Background(), providerID)
	if err != nil {
		t.Fatalf("list recovered reservations: %v", err)
	}
	if len(active) != 1 {
		t.Fatalf("active recovered reservations = %d, want 1: %+v", len(active), active)
	}
	got := active[0]
	if got.Allocation.Ref != want || got.Operation.State != "succeeded" ||
		got.Session.State != "ready" || got.Allocation.ObservedState != "running" ||
		got.AttemptStatus != "in_progress" {
		t.Fatalf("recovered durable state = %+v, want ready/running/succeeded for %+v", got, want)
	}
}

func TestPostgresDurableRecoveryResumesGenerationOneInNewService(t *testing.T) {
	fixture := openPostgresRecoveryFixture(t)
	storeA, leaseA := fixture.acquireStore("process-a")
	reservation := fixture.reserveGenerationOne(
		storeA,
		"22222222-2222-4222-8222-222222222222",
		"start:postgres-recovery-generation-one",
	)
	fixture.closeLease(leaseA)

	storeB, leaseB := fixture.acquireStore("process-b")
	runtime := &mockRunner{running: true, setupGates: make(map[uint64]chan struct{})}
	svc, gate := fixture.recoverWithNewController(storeB, leaseB, runtime)

	current := svc.GetCurrentSession(reservation.Session.UserID)
	if current == nil || current.SessionID != reservation.Session.ID || current.Generation != 1 || current.Status != StatusReady {
		t.Fatalf("new service current session = %+v", current)
	}
	runtime.mu.Lock()
	created := append([]runner.CreateSessionRequest(nil), runtime.created...)
	runtime.mu.Unlock()
	if len(created) != 1 || created[0].AllocationID != reservation.Allocation.Ref.ID ||
		created[0].IdempotencyKey != reservation.Operation.IdempotencyKey {
		t.Fatalf("new controller provider creates = %+v", created)
	}
	assertRecoveredReservation(t, storeB, fixture.providerID, reservation.Allocation.Ref)
	if gate.State() != runner.AuthorityRecovering || svc.IsAccepting() {
		t.Fatalf("recovery opened admission before activation: gate=%s accepting=%v", gate.State(), svc.IsAccepting())
	}
	if err := gate.Activate(); err != nil {
		t.Fatal(err)
	}
	if err := svc.Activate(); err != nil {
		t.Fatalf("activate recovered service: %v", err)
	}
	if !svc.IsAccepting() {
		t.Fatal("recovered service did not open admission after explicit activation")
	}
	svc.BeginShutdown()
	watchCtx, watchCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer watchCancel()
	if err := svc.WaitForWatchers(watchCtx); err != nil {
		t.Fatalf("stop recovered service watchers: %v", err)
	}
	gate.BeginDrain()
	fixture.closeLease(leaseB)
}

func TestPostgresDurableRecoveryDestroysOldGenerationBeforeResetReplacement(t *testing.T) {
	fixture := openPostgresRecoveryFixture(t)
	storeA, leaseA := fixture.acquireStore("process-a-reset")
	start := fixture.reserveGenerationOne(
		storeA,
		"33333333-3333-4333-8333-333333333333",
		"start:postgres-recovery-reset",
	)
	if err := storeA.MarkCreateSucceeded(context.Background(), start.Allocation.Ref); err != nil {
		t.Fatalf("mark reset source created: %v", err)
	}
	if err := storeA.MarkReady(context.Background(), start.Allocation.Ref); err != nil {
		t.Fatalf("mark reset source ready: %v", err)
	}
	reset, err := storeA.ReserveReset(context.Background(), runner.ReserveResetParams{
		Expected:       start.Allocation.Ref.Session,
		UserID:         start.Session.UserID,
		Provider:       runner.ProviderLocalDocker,
		ProviderID:     fixture.providerID,
		IdempotencyKey: "reset:postgres-recovery-generation-two",
	})
	if err != nil {
		t.Fatalf("reserve reset before restart: %v", err)
	}
	fixture.closeLease(leaseA)

	storeB, leaseB := fixture.acquireStore("process-b-reset")
	runtime := &mockRunner{running: true, setupGates: make(map[uint64]chan struct{})}
	svc, gate := fixture.recoverWithNewController(storeB, leaseB, runtime)

	current := svc.GetCurrentSession(start.Session.UserID)
	if current == nil || current.SessionID != start.Session.ID || current.Generation != 2 || current.Status != StatusReady {
		t.Fatalf("new service reset current session = %+v", current)
	}
	runtime.mu.Lock()
	created := append([]runner.CreateSessionRequest(nil), runtime.created...)
	removed := append([]runner.AllocationRef(nil), runtime.removed...)
	runtime.mu.Unlock()
	if len(removed) != 1 || removed[0] != reset.Old {
		t.Fatalf("old generation cleanup calls = %+v, want exactly %+v", removed, reset.Old)
	}
	if len(created) != 1 || created[0].AllocationID != reset.New.Allocation.Ref.ID || created[0].Session.Generation != 2 {
		t.Fatalf("replacement create calls = %+v", created)
	}
	assertRecoveredReservation(t, storeB, fixture.providerID, reset.New.Allocation.Ref)

	var oldObserved, oldDestroyState string
	if err := fixture.pool.QueryRow(context.Background(), `
		SELECT a.observed_state,o.state
		FROM runner_allocations a
		JOIN runner_operations o ON o.allocation_id=a.id AND o.provider_id=a.provider_id AND o.kind='destroy'
		WHERE a.id=$1`, reset.Old.ID).Scan(&oldObserved, &oldDestroyState); err != nil {
		t.Fatalf("read recovered reset source: %v", err)
	}
	if oldObserved != "absent" || oldDestroyState != "succeeded" {
		t.Fatalf("reset source durable cleanup = observed %s destroy %s", oldObserved, oldDestroyState)
	}
	gate.BeginDrain()
	svc.BeginShutdown()
	fixture.closeLease(leaseB)
}
