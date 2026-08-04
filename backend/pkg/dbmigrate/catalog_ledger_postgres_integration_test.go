package dbmigrate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	pgxmigrate "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/k8s-quiz/backend/migrations"
)

const catalogLedgerMigrationTestDatabaseEnv = "MIGRATION_TEST_DATABASE_URL"

type catalogLedgerMigrationFixture struct {
	pool     *pgxpool.Pool
	migrator *migrate.Migrate
}

func openCatalogLedgerMigrationFixture(t *testing.T) *catalogLedgerMigrationFixture {
	t.Helper()
	databaseURL := os.Getenv(catalogLedgerMigrationTestDatabaseEnv)
	if databaseURL == "" {
		t.Skip(catalogLedgerMigrationTestDatabaseEnv + " is not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	admin, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open catalog ledger migration database: %v", err)
	}
	if err := admin.Ping(ctx); err != nil {
		admin.Close()
		t.Fatalf("ping catalog ledger migration database: %v", err)
	}
	prepareCatalogLedgerTestExtensions(t, ctx, admin)

	schema := fmt.Sprintf("catalog_ledger_migration_%d_%d", os.Getpid(), time.Now().UnixNano())
	identifier := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+identifier); err != nil {
		admin.Close()
		t.Fatalf("create isolated catalog ledger migration schema: %v", err)
	}

	parsed, err := url.Parse(databaseURL)
	if err != nil {
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+identifier+" CASCADE")
		admin.Close()
		t.Fatalf("parse catalog ledger migration database URL: %v", err)
	}
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	isolatedURL := parsed.String()

	pool, err := pgxpool.New(ctx, isolatedURL)
	if err != nil {
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+identifier+" CASCADE")
		admin.Close()
		t.Fatalf("open isolated catalog ledger migration schema: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+identifier+" CASCADE")
		admin.Close()
		t.Fatalf("ping isolated catalog ledger migration schema: %v", err)
	}

	source, err := iofs.New(migrations.FS, ".")
	if err != nil {
		pool.Close()
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+identifier+" CASCADE")
		admin.Close()
		t.Fatalf("open embedded catalog ledger migrations: %v", err)
	}
	sqlDB, err := sql.Open("pgx", isolatedURL)
	if err != nil {
		pool.Close()
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+identifier+" CASCADE")
		admin.Close()
		t.Fatalf("open catalog ledger migration driver database: %v", err)
	}
	driver, err := pgxmigrate.WithInstance(sqlDB, &pgxmigrate.Config{})
	if err != nil {
		_ = sqlDB.Close()
		pool.Close()
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+identifier+" CASCADE")
		admin.Close()
		t.Fatalf("construct catalog ledger migration driver: %v", err)
	}
	migrator, err := migrate.NewWithInstance("iofs", source, "pgx", driver)
	if err != nil {
		_ = sqlDB.Close()
		pool.Close()
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+identifier+" CASCADE")
		admin.Close()
		t.Fatalf("construct catalog ledger migrator: %v", err)
	}

	fixture := &catalogLedgerMigrationFixture{pool: pool, migrator: migrator}
	t.Cleanup(func() {
		_, _ = migrator.Close()
		pool.Close()
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer dropCancel()
		if _, err := admin.Exec(dropCtx, "DROP SCHEMA "+identifier+" CASCADE"); err != nil {
			t.Errorf("drop isolated catalog ledger migration schema: %v", err)
		}
		admin.Close()
	})
	return fixture
}

func prepareCatalogLedgerTestExtensions(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire extension setup connection: %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(hashtext('k8s-quiz.integration-test-extensions'))`); err != nil {
		t.Fatalf("lock extension setup: %v", err)
	}
	defer func() {
		var unlocked bool
		if err := conn.QueryRow(context.Background(), `SELECT pg_advisory_unlock(hashtext('k8s-quiz.integration-test-extensions'))`).Scan(&unlocked); err != nil || !unlocked {
			t.Errorf("unlock extension setup: unlocked=%v err=%v", unlocked, err)
		}
	}()
	if _, err := conn.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS pgcrypto`); err != nil {
		t.Fatalf("prepare pgcrypto extension: %v", err)
	}
}

func TestProblemCatalogLedgerMigratesLegacySessionsAndRejectsUnsafeDowngrade(t *testing.T) {
	fixture := openCatalogLedgerMigrationFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if err := fixture.migrator.Migrate(10); err != nil {
		t.Fatalf("migrate isolated schema to v10: %v", err)
	}
	seedCatalogLedgerV10Data(t, ctx, fixture.pool)

	if err := fixture.migrator.Migrate(11); err != nil {
		t.Fatalf("migrate dirty v10 schema to v11: %v", err)
	}

	var legacyGeneration sql.NullInt64
	if err := fixture.pool.QueryRow(ctx, `
		SELECT catalog_generation FROM sessions
		WHERE id='11111111-1111-4111-8111-111111111111'`).Scan(&legacyGeneration); err != nil {
		t.Fatalf("read migrated legacy catalog generation: %v", err)
	}
	if legacyGeneration.Valid {
		t.Fatalf("legacy session catalog generation = %d, want NULL", legacyGeneration.Int64)
	}

	bootstrapCatalogLedgerV11(t, ctx, fixture.pool)
	assertNewNullCatalogGenerationRejected(t, ctx, fixture.pool)
	seedGenerationBoundV11Session(t, ctx, fixture.pool)

	err := fixture.migrator.Steps(-1)
	if err == nil {
		t.Fatal("migration 011 down succeeded with a generation-bound session")
	}
	if !strings.Contains(err.Error(), "cannot remove problem catalog ledger while generation-bound sessions exist") {
		t.Fatalf("migration 011 down error = %v, want generation-bound session refusal", err)
	}

	var boundGeneration int64
	if err := fixture.pool.QueryRow(ctx, `
		SELECT catalog_generation FROM sessions
		WHERE id='33333333-3333-4333-8333-333333333333'`).Scan(&boundGeneration); err != nil {
		t.Fatalf("read generation-bound session after rejected down migration: %v", err)
	}
	if boundGeneration != 1 {
		t.Fatalf("generation-bound session changed after rejected down migration: got %d", boundGeneration)
	}
}

func TestProblemCatalogLedgerDowngradeWaitsForAndRejectsConcurrentReservation(t *testing.T) {
	fixture := openCatalogLedgerMigrationFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if err := fixture.migrator.Migrate(10); err != nil {
		t.Fatalf("migrate isolated schema to v10: %v", err)
	}
	seedCatalogLedgerV10Data(t, ctx, fixture.pool)
	if err := fixture.migrator.Migrate(11); err != nil {
		t.Fatalf("migrate isolated schema to v11: %v", err)
	}
	bootstrapCatalogLedgerV11(t, ctx, fixture.pool)

	reservationTx, err := fixture.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin concurrent generation-bound reservation: %v", err)
	}
	defer func() { _ = reservationTx.Rollback(ctx) }()
	if _, err := reservationTx.Exec(ctx, `
		INSERT INTO sessions (
			id,user_id,problem_id,problem_revision,catalog_generation,current_generation,
			state,desired_state,queued_at,expires_at
		) VALUES (
			'44444444-4444-4444-8444-444444444444',
			'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa3',
			'ledger-problem',repeat('a',64),1,1,
			'queued','active',NOW(),NOW()+INTERVAL '1 hour'
		)`); err != nil {
		t.Fatalf("insert uncommitted generation-bound session: %v", err)
	}
	if _, err := reservationTx.Exec(ctx, `
		INSERT INTO runner_allocations (
			id,session_id,generation,provider_kind,provider_id,resource_profile,
			desired_state,observed_state,expires_at
		) VALUES (
			'44444444-4444-4444-8444-444444444445',
			'44444444-4444-4444-8444-444444444444',1,
			'local-docker','migration-v11-concurrent','default','active','unknown',NOW()+INTERVAL '1 hour'
		)`); err != nil {
		t.Fatalf("insert uncommitted generation-bound allocation: %v", err)
	}

	downResult := make(chan error, 1)
	go func() { downResult <- fixture.migrator.Steps(-1) }()

	waitDeadline := time.Now().Add(5 * time.Second)
	for {
		var waiting bool
		if err := fixture.pool.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1
				FROM pg_locks
				WHERE database=(SELECT oid FROM pg_database WHERE datname=current_database())
				  AND relation='sessions'::regclass
				  AND mode='AccessExclusiveLock'
				  AND NOT granted
			)`).Scan(&waiting); err != nil {
			t.Fatalf("observe downgrade session lock: %v", err)
		}
		if waiting {
			break
		}
		select {
		case err := <-downResult:
			t.Fatalf("downgrade did not wait for concurrent reservation: %v", err)
		default:
		}
		if time.Now().After(waitDeadline) {
			t.Fatal("downgrade did not request an exclusive sessions lock")
		}
		time.Sleep(10 * time.Millisecond)
	}

	if err := reservationTx.Commit(ctx); err != nil {
		t.Fatalf("commit concurrent generation-bound reservation: %v", err)
	}
	select {
	case err := <-downResult:
		if err == nil || !strings.Contains(err.Error(), "cannot remove problem catalog ledger while generation-bound sessions exist") {
			t.Fatalf("concurrent downgrade error = %v, want generation-bound session refusal", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("downgrade did not resume after reservation commit")
	}

	var generation int64
	if err := fixture.pool.QueryRow(ctx, `
		SELECT catalog_generation FROM sessions
		WHERE id='44444444-4444-4444-8444-444444444444'`).Scan(&generation); err != nil {
		t.Fatalf("read concurrent reservation after rejected downgrade: %v", err)
	}
	if generation != 1 {
		t.Fatalf("concurrent reservation generation = %d, want 1", generation)
	}
}

func seedCatalogLedgerV10Data(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
		INSERT INTO users (id,github_id,username) VALUES
			('aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa1',9401,'legacy-user'),
			('aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa2',9402,'null-rejection-user'),
			('aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa3',9403,'bound-user');
		INSERT INTO problems (id,title,revision,catalog_active)
		VALUES ('ledger-problem','Ledger problem',repeat('a',64),TRUE)`); err != nil {
		t.Fatalf("seed v10 catalog identities: %v", err)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin v10 legacy session seed: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `
		INSERT INTO sessions (
			id,user_id,problem_id,problem_revision,current_generation,state,
			desired_state,queued_at,expires_at
		) VALUES (
			'11111111-1111-4111-8111-111111111111',
			'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa1',
			'ledger-problem',repeat('a',64),1,'ready','active',NOW(),NOW()+INTERVAL '1 hour'
		)`); err != nil {
		t.Fatalf("insert v10 legacy session: %v", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO runner_allocations (
			id,session_id,generation,provider_kind,provider_id,resource_profile,
			desired_state,observed_state,expires_at
		) VALUES (
			'11111111-1111-4111-8111-111111111112',
			'11111111-1111-4111-8111-111111111111',1,
			'local-docker','migration-v10-legacy','default','active','running',NOW()+INTERVAL '1 hour'
		)`); err != nil {
		t.Fatalf("insert v10 legacy allocation: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit v10 legacy session seed: %v", err)
	}
}

func bootstrapCatalogLedgerV11(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin v11 catalog bootstrap: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `
		INSERT INTO problem_catalog_publications (
			generation,previous_generation,digest_schema,candidate_digest,entry_count
		) VALUES (1,NULL,1,decode(repeat('11',32),'hex'),1)`); err != nil {
		t.Fatalf("insert v11 catalog publication: %v", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO problem_catalog_entries (
			catalog_generation,problem_id,problem_revision,title,category,difficulty,
			type,timeout_minutes,verify_type
		) VALUES (
			1,'ledger-problem',repeat('a',64),'Ledger problem','pod','easy','fix',30,'script'
		)`); err != nil {
		t.Fatalf("insert v11 catalog entry: %v", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO problem_catalog_head (singleton,catalog_generation)
		VALUES (TRUE,1)`); err != nil {
		t.Fatalf("insert v11 catalog head: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit v11 catalog bootstrap: %v", err)
	}
}

func assertNewNullCatalogGenerationRejected(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	_, err := pool.Exec(ctx, `
		INSERT INTO sessions (
			id,user_id,problem_id,problem_revision,catalog_generation,current_generation,
			state,desired_state,queued_at,expires_at
		) VALUES (
			'22222222-2222-4222-8222-222222222222',
			'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa2',
			'ledger-problem',repeat('a',64),NULL,1,
			'queued','active',NOW(),NOW()+INTERVAL '1 hour'
		)`)
	if err == nil {
		t.Fatal("v11 accepted a new session with NULL catalog generation")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
		t.Fatalf("new NULL catalog generation error = %v, want SQLSTATE 23514", err)
	}
	var count int
	if scanErr := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM sessions
		WHERE id='22222222-2222-4222-8222-222222222222'`).Scan(&count); scanErr != nil {
		t.Fatalf("count rejected NULL-generation session: %v", scanErr)
	}
	if count != 0 {
		t.Fatalf("rejected NULL-generation session persisted %d rows", count)
	}
}

func seedGenerationBoundV11Session(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin generation-bound session seed: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `
		INSERT INTO sessions (
			id,user_id,problem_id,problem_revision,catalog_generation,current_generation,
			state,desired_state,queued_at,expires_at
		) VALUES (
			'33333333-3333-4333-8333-333333333333',
			'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa3',
			'ledger-problem',repeat('a',64),1,1,
			'ready','active',NOW(),NOW()+INTERVAL '1 hour'
		)`); err != nil {
		t.Fatalf("insert generation-bound v11 session: %v", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO runner_allocations (
			id,session_id,generation,provider_kind,provider_id,resource_profile,
			desired_state,observed_state,expires_at
		) VALUES (
			'33333333-3333-4333-8333-333333333334',
			'33333333-3333-4333-8333-333333333333',1,
			'local-docker','migration-v11-bound','default','active','running',NOW()+INTERVAL '1 hour'
		)`); err != nil {
		t.Fatalf("insert generation-bound v11 allocation: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit generation-bound v11 session seed: %v", err)
	}
}
