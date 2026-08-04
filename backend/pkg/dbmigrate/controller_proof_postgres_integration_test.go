package dbmigrate

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	controllerProofV1FunctionIdentity = "consume_runner_controller_proof(text,bigint,uuid,uuid,text,bytea,timestamp with time zone,timestamp with time zone,timestamp with time zone,text,text,bytea)"
	controllerProofV2FunctionIdentity = "consume_runner_controller_proof_v2(text,bigint,uuid,uuid,text,bytea,timestamp with time zone,timestamp with time zone,timestamp with time zone,text,text,bytea)"
)

func TestControllerProofMigrationInstallsSchemaBoundSecurityDefiner(t *testing.T) {
	fixture := openCatalogLedgerMigrationFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := fixture.migrator.Migrate(13); err != nil {
		t.Fatalf("migrate isolated schema to v13: %v", err)
	}
	if err := fixture.migrator.Migrate(14); err != nil {
		t.Fatalf("migrate isolated schema to v14: %v", err)
	}

	var schema, definition string
	var securityDefiner bool
	var settings []string
	var publicCanExecute bool
	if err := fixture.pool.QueryRow(ctx, `
		SELECT namespace.nspname,
		       procedure.prosecdef,
		       COALESCE(procedure.proconfig,ARRAY[]::TEXT[]),
		       pg_catalog.pg_get_functiondef(procedure.oid),
		       pg_catalog.has_function_privilege(
		           'public', procedure.oid, 'EXECUTE'
		       )
		FROM pg_catalog.pg_proc procedure
		JOIN pg_catalog.pg_namespace namespace ON namespace.oid=procedure.pronamespace
		WHERE procedure.oid=$1::REGPROCEDURE`, controllerProofV1FunctionIdentity).Scan(
		&schema, &securityDefiner, &settings, &definition, &publicCanExecute,
	); err != nil {
		t.Fatalf("inspect controller proof function: %v", err)
	}
	var currentSchema string
	if err := fixture.pool.QueryRow(ctx, `SELECT current_schema()`).Scan(&currentSchema); err != nil {
		t.Fatal(err)
	}
	if schema != currentSchema || !securityDefiner || publicCanExecute {
		t.Fatalf("proof function metadata schema=%q current=%q security_definer=%v public_execute=%v",
			schema, currentSchema, securityDefiner, publicCanExecute)
	}
	if len(settings) != 1 || settings[0] != "search_path=pg_catalog" {
		t.Fatalf("proof function settings=%v, want exact pg_catalog search_path", settings)
	}
	for _, relation := range []string{
		"runner_controller_epochs",
		"runner_controller_proofs",
		"runner_controller_proof_consumptions",
	} {
		quoted := `"` + currentSchema + `".` + relation
		unquoted := currentSchema + `.` + relation
		if !strings.Contains(definition, quoted) && !strings.Contains(definition, unquoted) {
			t.Fatalf("proof function does not bind %s to isolated schema", relation)
		}
	}
	if strings.Contains(definition, "public.runner_controller_") || strings.Contains(definition, "public.digest") {
		t.Fatal("proof function definition depends on public-schema authority objects")
	}
}

func TestControllerProofDeploymentPinRejectsNonPublicSchema(t *testing.T) {
	fixture := openCatalogLedgerMigrationFixture(t)
	if err := fixture.migrator.Migrate(14); err != nil {
		t.Fatalf("migrate isolated schema to v14: %v", err)
	}
	err := fixture.migrator.Migrate(15)
	if err == nil || !strings.Contains(err.Error(), "requires fixed public schema") {
		t.Fatalf("non-public v15 migration error=%v", err)
	}
}

// The validator must authorize at the time it has acquired all serialization
// locks, not at function entry. This regression test deliberately starts a
// consume while the epoch row is locked, waits past proof expiry, and proves
// that migration 016 rejects the stale capability without recording use.
func TestControllerProofExpiresWhileWaitingForAuthorityRowLock(t *testing.T) {
	databaseURL := os.Getenv("MIGRATION_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("MIGRATION_TEST_DATABASE_URL is not set")
	}
	if err := Up(databaseURL); err != nil {
		t.Fatalf("apply current migrations: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open migration test database: %v", err)
	}
	defer pool.Close()

	leaseConnection, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire lease connection: %v", err)
	}
	defer leaseConnection.Release()
	providerID := fmt.Sprintf("migration-lock-expiry-%d", time.Now().UnixNano())
	leaseID := "10000000-0000-4000-8000-000000000001"
	proofID := "20000000-0000-4000-8000-000000000002"
	advisoryKey := time.Now().UnixNano() & 0x7fffffffffffffff
	if _, err := leaseConnection.Exec(ctx, `SELECT pg_catalog.pg_advisory_lock($1)`, advisoryKey); err != nil {
		t.Fatalf("hold test controller advisory lease: %v", err)
	}
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		_, _ = leaseConnection.Exec(cleanupCtx, `SELECT pg_catalog.pg_advisory_unlock($1)`, advisoryKey)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM public.runner_controller_proof_consumptions WHERE provider_id=$1`, providerID)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM public.runner_controller_proofs WHERE provider_id=$1`, providerID)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM public.runner_controller_epochs WHERE provider_id=$1`, providerID)
	}()

	var backendPID int32
	var backendStart time.Time
	if err := leaseConnection.QueryRow(ctx, `SELECT pg_backend_pid(),backend_start
		FROM pg_catalog.pg_stat_activity WHERE pid=pg_backend_pid()`).Scan(&backendPID, &backendStart); err != nil {
		t.Fatalf("read test lease backend identity: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO public.runner_controller_epochs
		(provider_id,epoch,lease_id,lease_backend_pid,lease_backend_start,lease_advisory_key)
		VALUES ($1,1,$2::uuid,$3,$4,$5)`, providerID, leaseID, backendPID, backendStart, advisoryKey); err != nil {
		t.Fatalf("seed test controller epoch: %v", err)
	}
	digest := sha256.Sum256([]byte("post-lock-expiry-digest"))
	tokenHash := sha256.Sum256([]byte("post-lock-expiry-token"))
	var issuedAt, expiresAt, effectDeadline time.Time
	if err := pool.QueryRow(ctx, `WITH authority_time AS MATERIALIZED (
		SELECT clock_timestamp() AS issued_at
	), inserted AS (
		INSERT INTO public.runner_controller_proofs
		(provider_id,epoch,lease_id,proof_id,operation,request_digest,effect_deadline,
		 issuer_uri,audience_uri,token_hash,issued_at,expires_at)
		SELECT $1,1,$2::uuid,$3::uuid,'create',$4,
		       issued_at+interval '4 seconds','spiffe://test/control-plane',
		       'spiffe://test/private-runner',$5,issued_at,issued_at+interval '2 seconds'
		FROM authority_time
		RETURNING issued_at,expires_at,effect_deadline)
		SELECT issued_at,expires_at,effect_deadline FROM inserted`,
		providerID, leaseID, proofID, digest[:], tokenHash[:]).Scan(&issuedAt, &expiresAt, &effectDeadline); err != nil {
		t.Fatalf("seed expiring controller proof: %v", err)
	}

	lockTx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin authority row lock: %v", err)
	}
	defer func() { _ = lockTx.Rollback(context.Background()) }()
	if _, err := lockTx.Exec(ctx, `UPDATE public.runner_controller_epochs
		SET updated_at=updated_at WHERE provider_id=$1`, providerID); err != nil {
		t.Fatalf("lock controller epoch row: %v", err)
	}

	result := make(chan struct {
		status string
		err    error
	}, 1)
	go func() {
		var status string
		err := pool.QueryRow(context.Background(), `SELECT public.consume_runner_controller_proof_v2(
			$1,1,$2::uuid,$3::uuid,'create',$4,$5,$6,$7,
			'spiffe://test/control-plane','spiffe://test/private-runner',$8)`,
			providerID, leaseID, proofID, digest[:], effectDeadline, issuedAt, expiresAt, tokenHash[:]).Scan(&status)
		result <- struct {
			status string
			err    error
		}{status: status, err: err}
	}()

	// Prove the consumer is actually waiting on a relation lock before letting
	// the capability expire; this avoids a timing-only regression assertion.
	deadline := time.Now().Add(3 * time.Second)
	waiting := false
	for time.Now().Before(deadline) {
		if err := pool.QueryRow(ctx, `SELECT EXISTS (
			SELECT 1 FROM pg_catalog.pg_stat_activity
			WHERE datname=current_database() AND wait_event_type='Lock'
			  AND query LIKE 'SELECT public.consume_runner_controller_proof_v2%')`).Scan(&waiting); err != nil {
			t.Fatalf("inspect proof consumer lock wait: %v", err)
		}
		if waiting {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !waiting {
		t.Fatal("proof consumer did not block on the authority row lock")
	}
	if wait := time.Until(expiresAt.Add(250 * time.Millisecond)); wait > 0 {
		time.Sleep(wait)
	}
	if err := lockTx.Commit(ctx); err != nil {
		t.Fatalf("release authority row lock: %v", err)
	}
	got := <-result
	if got.err != nil || got.status != "unauthorized" {
		t.Fatalf("post-lock expired proof status=%q err=%v, want unauthorized", got.status, got.err)
	}
	var consumed int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM public.runner_controller_proof_consumptions
		WHERE provider_id=$1 AND proof_id=$2::uuid`, providerID, proofID).Scan(&consumed); err != nil {
		t.Fatalf("count expired proof consumptions: %v", err)
	}
	if consumed != 0 {
		t.Fatalf("expired proof wrote %d consumption rows", consumed)
	}
}

func TestControllerProofMigrationCleanDowngradeIsAllowed(t *testing.T) {
	fixture := openCatalogLedgerMigrationFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := fixture.migrator.Migrate(14); err != nil {
		t.Fatalf("migrate isolated schema to v14: %v", err)
	}
	if err := fixture.migrator.Steps(-1); err != nil {
		t.Fatalf("clean inactive v14 downgrade: %v", err)
	}
	version, dirty, err := fixture.migrator.Version()
	if err != nil {
		t.Fatalf("read migration version after clean downgrade: %v", err)
	}
	if version != 13 || dirty {
		t.Fatalf("clean downgrade version=%d dirty=%v, want v13 clean", version, dirty)
	}
	var leaseColumnCount int
	if err := fixture.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM information_schema.columns
		WHERE table_schema=current_schema()
		  AND table_name='runner_controller_epochs'
		  AND column_name='lease_id'`).Scan(&leaseColumnCount); err != nil {
		t.Fatal(err)
	}
	if leaseColumnCount != 0 {
		t.Fatal("clean downgrade retained controller proof lease columns")
	}
}

func TestControllerProofMigrationRejectsDowngradeWithLiveLease(t *testing.T) {
	fixture := openCatalogLedgerMigrationFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := fixture.migrator.Migrate(14); err != nil {
		t.Fatalf("migrate isolated schema to v14: %v", err)
	}
	if _, err := fixture.pool.Exec(ctx, `
		INSERT INTO runner_controller_epochs (
			provider_id,epoch,lease_id,lease_backend_pid,lease_backend_start,lease_advisory_key
		) VALUES (
			'home-proxmox:downgrade-live',1,
			'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa',pg_backend_pid(),
			(SELECT backend_start FROM pg_catalog.pg_stat_activity WHERE pid=pg_backend_pid()),
			42
		)`); err != nil {
		t.Fatalf("seed live controller lease: %v", err)
	}

	err := fixture.migrator.Steps(-1)
	if err == nil || !strings.Contains(err.Error(), "cannot remove controller proof boundary while a controller lease is active") {
		t.Fatalf("live-lease downgrade error=%v", err)
	}
	assertControllerProofDowngradeFailedClosed(t, fixture, "home-proxmox:downgrade-live")
}

func TestControllerProofMigrationRejectsDowngradeWithProofLedger(t *testing.T) {
	fixture := openCatalogLedgerMigrationFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := fixture.migrator.Migrate(14); err != nil {
		t.Fatalf("migrate isolated schema to v14: %v", err)
	}
	if _, err := fixture.pool.Exec(ctx, `
		INSERT INTO runner_controller_epochs (provider_id,epoch)
		VALUES ('home-proxmox:downgrade-ledger',1);
		INSERT INTO runner_controller_proofs (
			provider_id,epoch,lease_id,proof_id,operation,request_digest,
			effect_deadline,issuer_uri,audience_uri,token_hash,issued_at,expires_at
		) VALUES (
			'home-proxmox:downgrade-ledger',1,
			'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa',
			'bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb','get',
			decode(repeat('11',32),'hex'),clock_timestamp()+INTERVAL '30 seconds',
			'spiffe://k8s-quiz.test/control-plane',
			'spiffe://k8s-quiz.test/private-runner',
			decode(repeat('22',32),'hex'),clock_timestamp(),
			clock_timestamp()+INTERVAL '5 seconds'
		)`); err != nil {
		t.Fatalf("seed controller proof ledger: %v", err)
	}

	err := fixture.migrator.Steps(-1)
	if err == nil || !strings.Contains(err.Error(), "cannot remove nonempty controller proof ledger") {
		t.Fatalf("nonempty-ledger downgrade error=%v", err)
	}
	assertControllerProofDowngradeFailedClosed(t, fixture, "home-proxmox:downgrade-ledger")
}

func assertControllerProofDowngradeFailedClosed(
	t *testing.T,
	fixture *catalogLedgerMigrationFixture,
	providerID string,
) {
	t.Helper()
	version, dirty, err := fixture.migrator.Version()
	if err != nil {
		t.Fatalf("read migration version after rejected downgrade: %v", err)
	}
	// golang-migrate writes the target version and dirty flag before running a
	// down migration. The transactional SQL refusal preserves every v14 object
	// and row, while dirty bookkeeping prevents automatic startup or further
	// migration until an operator inspects and explicitly repairs it.
	if version != 13 || !dirty {
		t.Fatalf("rejected downgrade version=%d dirty=%v, want v13 dirty fail-closed bookkeeping", version, dirty)
	}
	var epoch int64
	if err := fixture.pool.QueryRow(context.Background(), `
		SELECT epoch FROM runner_controller_epochs WHERE provider_id=$1`, providerID).Scan(&epoch); err != nil {
		if err == sql.ErrNoRows {
			t.Fatal("rejected downgrade removed controller authority row")
		}
		t.Fatalf("read authority row after rejected downgrade: %v", err)
	}
	if epoch != 1 {
		t.Fatalf("authority epoch after rejected downgrade=%d", epoch)
	}
	var functionExists bool
	if err := fixture.pool.QueryRow(context.Background(), `
		SELECT to_regprocedure($1) IS NOT NULL`, controllerProofV1FunctionIdentity).Scan(&functionExists); err != nil {
		t.Fatalf("inspect proof function after rejected downgrade: %v", err)
	}
	if !functionExists {
		t.Fatal("rejected downgrade removed the v14 proof function")
	}
}
