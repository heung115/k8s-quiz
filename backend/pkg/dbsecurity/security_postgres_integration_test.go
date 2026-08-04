package dbsecurity_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	stdruntime "runtime"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/k8s-quiz/backend/internal/auth"
	"github.com/k8s-quiz/backend/internal/problem"
	"github.com/k8s-quiz/backend/internal/runner"
	"github.com/k8s-quiz/backend/internal/user"
	"github.com/k8s-quiz/backend/migrations"
	appconfig "github.com/k8s-quiz/backend/pkg/config"
	"github.com/k8s-quiz/backend/pkg/dbsecurity"
	"github.com/k8s-quiz/backend/pkg/models"
)

const strictDatabaseEnv = "CONTROL_PLANE_DB_SECURITY_TEST_DATABASE_URL"

const (
	strictDatabase      = "kq_control_plane_security"
	strictOwner         = "kq_app_owner"
	strictMigrator      = "kq_migrator"
	strictRuntime       = "kq_runtime"
	strictValidator     = "kq_validator"
	strictMigratorPass  = "Migrator_Strict_42!"
	strictRuntimePass   = "Runtime_Strict_42!"
	strictValidatorPass = "Validator_Strict_42!"
)

var strictConfig = dbsecurity.Config{
	OwnerRole: strictOwner, MigratorRole: strictMigrator,
	RuntimeRole: strictRuntime, ValidatorRole: strictValidator,
	DedicatedCluster: true,
}

// TestPostgresStrictControlPlaneBoundary is intentionally destructive to the
// supplied PostgreSQL cluster: hardening changes cluster-wide pg_catalog ACLs
// and closes CONNECT on every non-target database. The environment variable
// must therefore point only at a fresh, disposable PostgreSQL 16 cluster.
func TestPostgresStrictControlPlaneBoundary(t *testing.T) {
	adminURL := os.Getenv(strictDatabaseEnv)
	if adminURL == "" {
		t.Skip(strictDatabaseEnv + " is not set (requires a fresh disposable PostgreSQL 16 cluster)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	admin := openUnconfiguredPool(t, ctx, adminURL)
	defer admin.Close()
	assertFreshPostgres16(t, ctx, admin)
	strictConfig.CatalogOwnerRole = currentUser(t, ctx, admin)

	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{strictDatabase}.Sanitize()); err != nil {
		t.Fatalf("create strict Control Plane database: %v", err)
	}
	targetAdminURL := databaseURL(t, adminURL, strictDatabase, "", "")
	runBootstrap(t, ctx, targetAdminURL)
	setRolePassword(t, ctx, admin, strictMigrator, strictMigratorPass)
	setRolePassword(t, ctx, admin, strictRuntime, strictRuntimePass)
	setRolePassword(t, ctx, admin, strictValidator, strictValidatorPass)

	// Bootstrap is both idempotent and corrective for the role attributes it
	// owns. Deliberately contaminate one role before the second run.
	if _, err := admin.Exec(ctx, "ALTER ROLE "+pgx.Identifier{strictRuntime}.Sanitize()+" INHERIT CREATEDB"); err != nil {
		t.Fatalf("contaminate runtime role before bootstrap replay: %v", err)
	}
	runBootstrap(t, ctx, targetAdminURL)
	assertRoleNormalized(t, ctx, admin, strictRuntime)

	targetAdmin := openUnconfiguredPool(t, ctx, targetAdminURL)
	defer targetAdmin.Close()
	if _, err := targetAdmin.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS pgcrypto WITH SCHEMA public`); err != nil {
		t.Fatalf("preinstall pgcrypto as provisioner: %v", err)
	}
	assertOwnerHasNoDatabaseCreate(t, ctx, targetAdmin)

	migratorURL := databaseURL(t, adminURL, strictDatabase, strictMigrator, strictMigratorPass)
	if err := dbsecurity.Migrate(ctx, migrations.FS, ".", migratorURL, strictOwner); err != nil {
		t.Fatalf("migrate through SET ROLE owner: %v", err)
	}
	if err := dbsecurity.Harden(ctx, targetAdmin, strictConfig); err != nil {
		t.Fatalf("harden strict Control Plane database: %v", err)
	}

	runtimeURL := databaseURL(t, adminURL, strictDatabase, strictRuntime, strictRuntimePass)
	validatorURL := databaseURL(t, adminURL, strictDatabase, strictValidator, strictValidatorPass)
	runtimePool := openRuntimePool(t, ctx, runtimeURL, strictConfig, false)
	defer runtimePool.Close()
	validatorPool := openRuntimePool(t, ctx, validatorURL, strictConfig, true)
	defer validatorPool.Close()

	if err := dbsecurity.VerifyRuntime(ctx, runtimePool, strictConfig); err != nil {
		t.Fatalf("runtime-only security attestation: %v", err)
	}
	if err := dbsecurity.Verify(ctx, runtimePool, validatorPool, strictConfig); err != nil {
		t.Fatalf("runtime and validator security attestation: %v", err)
	}
	assertNonTargetDatabaseClosed(t, ctx, admin, adminURL)
	assertPoolRejectsSearchPathContamination(t, ctx, runtimePool)
	assertEffectiveDenials(t, ctx, runtimePool, validatorPool)

	runtimeSmoke(t, ctx, runtimePool, validatorPool, targetAdmin, strictConfig)
	assertDriftIsRejected(t, ctx, runtimePool, validatorPool, targetAdmin, strictConfig)
}

func assertFreshPostgres16(t *testing.T, ctx context.Context, admin *pgxpool.Pool) {
	t.Helper()
	var version int
	var super bool
	if err := admin.QueryRow(ctx, `SELECT current_setting('server_version_num')::int,
		(SELECT rolsuper FROM pg_catalog.pg_roles WHERE rolname=current_user)`).Scan(&version, &super); err != nil {
		t.Fatalf("inspect strict PostgreSQL fixture: %v", err)
	}
	if version < 160000 || version >= 170000 || !super {
		t.Fatalf("strict fixture requires PostgreSQL 16 superuser, got version=%d superuser=%v", version, super)
	}
	var databaseExists bool
	var roleCount int
	if err := admin.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_catalog.pg_database WHERE datname=$1),
		(SELECT COUNT(*) FROM pg_catalog.pg_roles WHERE rolname=ANY($2::text[]))`, strictDatabase,
		[]string{strictOwner, strictMigrator, strictRuntime, strictValidator}).Scan(&databaseExists, &roleCount); err != nil {
		t.Fatalf("inspect disposable fixture inventory: %v", err)
	}
	if databaseExists || roleCount != 0 {
		t.Fatalf("strict fixture is not fresh: target database exists=%v configured roles=%d", databaseExists, roleCount)
	}
}

func currentUser(t *testing.T, ctx context.Context, pool *pgxpool.Pool) string {
	t.Helper()
	var name string
	if err := pool.QueryRow(ctx, `SELECT current_user`).Scan(&name); err != nil {
		t.Fatal(err)
	}
	return name
}

func runBootstrap(t *testing.T, ctx context.Context, targetAdminURL string) {
	t.Helper()
	_, file, _, ok := stdruntime.Caller(0)
	if !ok {
		t.Fatal("locate dbsecurity integration test")
	}
	script := filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", "..", "docker", "postgres", "bootstrap-control-plane-roles.sql"))
	command := exec.CommandContext(ctx, "psql", targetAdminURL,
		"--set", "ON_ERROR_STOP=1",
		"--set", "owner_role="+strictOwner,
		"--set", "migrator_role="+strictMigrator,
		"--set", "runtime_role="+strictRuntime,
		"--set", "validator_role="+strictValidator,
		"--set", "database_name="+strictDatabase,
		"--file", script,
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("run PostgreSQL role bootstrap: %v\n%s", err, output)
	}
}

func setRolePassword(t *testing.T, ctx context.Context, admin *pgxpool.Pool, role, password string) {
	t.Helper()
	var statement string
	if err := admin.QueryRow(ctx, `SELECT pg_catalog.format('ALTER ROLE %I PASSWORD %L',$1::text,$2::text)`, role, password).Scan(&statement); err != nil {
		t.Fatalf("format password statement for %s: %v", role, err)
	}
	if _, err := admin.Exec(ctx, statement); err != nil {
		t.Fatalf("set password for %s: %v", role, err)
	}
}

func assertRoleNormalized(t *testing.T, ctx context.Context, admin *pgxpool.Pool, role string) {
	t.Helper()
	var login, inherit, super, createDB, createRole, replication, bypass bool
	if err := admin.QueryRow(ctx, `SELECT rolcanlogin,rolinherit,rolsuper,rolcreatedb,
		rolcreaterole,rolreplication,rolbypassrls FROM pg_catalog.pg_roles WHERE rolname=$1`, role).
		Scan(&login, &inherit, &super, &createDB, &createRole, &replication, &bypass); err != nil {
		t.Fatalf("inspect normalized role %s: %v", role, err)
	}
	if !login || inherit || super || createDB || createRole || replication || bypass {
		t.Fatalf("bootstrap did not normalize role %s", role)
	}
}

func assertOwnerHasNoDatabaseCreate(t *testing.T, ctx context.Context, admin *pgxpool.Pool) {
	t.Helper()
	var allowed bool
	if err := admin.QueryRow(ctx, `SELECT pg_catalog.has_database_privilege($1,current_database(),'CREATE')`, strictOwner).Scan(&allowed); err != nil {
		t.Fatal(err)
	}
	if allowed {
		t.Fatal("NOLOGIN application owner unexpectedly has database CREATE")
	}
}

func openUnconfiguredPool(t *testing.T, ctx context.Context, rawURL string) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(ctx, rawURL)
	if err != nil {
		t.Fatalf("open PostgreSQL pool: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("ping PostgreSQL pool: %v", err)
	}
	return pool
}

func openRuntimePool(t *testing.T, ctx context.Context, rawURL string, security dbsecurity.Config, validator bool) *pgxpool.Pool {
	t.Helper()
	poolConfig, err := pgxpool.ParseConfig(rawURL)
	if err != nil {
		t.Fatalf("parse least-privilege URL: %v", err)
	}
	poolConfig.MaxConns = 8
	if validator {
		err = dbsecurity.ConfigureValidatorPool(poolConfig, security)
	} else {
		err = dbsecurity.ConfigureRuntimePool(poolConfig, security)
	}
	if err != nil {
		t.Fatalf("configure least-privilege pool: %v", err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		t.Fatalf("open least-privilege pool: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("ping least-privilege pool: %v", err)
	}
	return pool
}

func databaseURL(t *testing.T, raw, database, username, password string) string {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse PostgreSQL URL: %v", err)
	}
	parsed.Path = "/" + database
	if username != "" {
		parsed.User = url.UserPassword(username, password)
	}
	return parsed.String()
}

func assertNonTargetDatabaseClosed(t *testing.T, ctx context.Context, admin *pgxpool.Pool, adminURL string) {
	t.Helper()
	var publicConnect, migratorConnect, runtimeConnect, validatorConnect bool
	if err := admin.QueryRow(ctx, `SELECT
		pg_catalog.has_database_privilege('public','postgres','CONNECT'),
		pg_catalog.has_database_privilege($1,'postgres','CONNECT'),
		pg_catalog.has_database_privilege($2,'postgres','CONNECT'),
		pg_catalog.has_database_privilege($3,'postgres','CONNECT')`,
		strictMigrator, strictRuntime, strictValidator).Scan(
		&publicConnect, &migratorConnect, &runtimeConnect, &validatorConnect,
	); err != nil {
		t.Fatalf("inspect non-target CONNECT boundary: %v", err)
	}
	if publicConnect || migratorConnect || runtimeConnect || validatorConnect {
		t.Fatalf("non-target postgres database remains reachable: public=%v migrator=%v runtime=%v validator=%v",
			publicConnect, migratorConnect, runtimeConnect, validatorConnect)
	}
	nonTargetRuntime := databaseURL(t, adminURL, "postgres", strictRuntime, strictRuntimePass)
	conn, err := pgx.Connect(ctx, nonTargetRuntime)
	if err == nil {
		_ = conn.Close(ctx)
		t.Fatal("runtime login connected to non-target postgres database")
	}
}

func assertPoolRejectsSearchPathContamination(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var contaminatedPID int32
	if err := conn.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&contaminatedPID); err != nil {
		conn.Release()
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `SET search_path TO public`); err != nil {
		conn.Release()
		t.Fatal(err)
	}
	conn.Release()

	replacement, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire after search_path contamination: %v", err)
	}
	defer replacement.Release()
	var replacementPID int32
	var path string
	if err := replacement.QueryRow(ctx, `SELECT pg_backend_pid(),current_setting('search_path')`).Scan(&replacementPID, &path); err != nil {
		t.Fatal(err)
	}
	if replacementPID == contaminatedPID || path != "pg_catalog, public, pg_temp" {
		t.Fatalf("contaminated pooled connection was reused: old=%d new=%d path=%q", contaminatedPID, replacementPID, path)
	}
}

func assertEffectiveDenials(t *testing.T, ctx context.Context, runtimePool, validatorPool *pgxpool.Pool) {
	t.Helper()
	var version int
	if err := runtimePool.QueryRow(ctx, `SELECT version FROM public.schema_migrations`).Scan(&version); err != nil || version != 17 {
		t.Fatalf("runtime cannot read migration version 17: version=%d err=%v", version, err)
	}
	expectDeniedTx(t, ctx, runtimePool, `UPDATE public.schema_migrations SET dirty=TRUE`)
	expectDeniedTx(t, ctx, runtimePool, `CREATE TABLE public.runtime_must_not_create(id int)`)
	expectDeniedTx(t, ctx, runtimePool, "SET ROLE "+pgx.Identifier{strictOwner}.Sanitize())
	expectDeniedTx(t, ctx, runtimePool, `SELECT pg_catalog.pg_advisory_lock(1,2)`)

	expectDeniedTx(t, ctx, runtimePool, `SELECT * FROM public.runner_controller_proof_consumptions`)
	expectDeniedTx(t, ctx, runtimePool, `INSERT INTO public.runner_controller_proof_consumptions(provider_id,proof_id) VALUES ('x','00000000-0000-4000-8000-000000000001')`)
	expectDeniedTx(t, ctx, runtimePool, `UPDATE public.runner_controller_proof_consumptions SET consumed_at=NOW()`)
	expectDeniedTx(t, ctx, runtimePool, `DELETE FROM public.runner_controller_proof_consumptions`)
	expectDeniedTx(t, ctx, runtimePool, `TRUNCATE public.runner_controller_proof_consumptions`)
	expectDeniedTx(t, ctx, runtimePool, `SELECT token_hash FROM public.runner_controller_proofs`)
	expectDeniedTx(t, ctx, validatorPool, `SELECT * FROM public.users`)
}

func expectDeniedTx(t *testing.T, ctx context.Context, pool *pgxpool.Pool, statement string, args ...any) {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, execErr := tx.Exec(ctx, statement, args...)
	_ = tx.Rollback(ctx)
	var pgErr *pgconn.PgError
	if !errors.As(execErr, &pgErr) || pgErr.Code != "42501" {
		t.Fatalf("statement was not denied with insufficient_privilege: %q err=%v", statement, execErr)
	}
}

func runtimeSmoke(
	t *testing.T,
	ctx context.Context,
	runtimePool, validatorPool, admin *pgxpool.Pool,
	security dbsecurity.Config,
) {
	t.Helper()
	userRepo := user.NewRepository(runtimePool)
	principal := &models.User{
		GithubID: 730001, Username: "strict-runtime", Email: "strict@example.invalid",
		AvatarURL: "https://example.invalid/avatar", Role: models.RoleUser,
	}
	if err := userRepo.Create(ctx, principal); err != nil {
		t.Fatalf("runtime user create with RETURNING/default UUID: %v", err)
	}
	if _, err := userRepo.FindByGithubID(ctx, principal.GithubID); err != nil {
		t.Fatalf("runtime user find by GitHub id: %v", err)
	}
	principal.Username = "strict-runtime-updated"
	if err := userRepo.Update(ctx, principal); err != nil {
		t.Fatalf("runtime user update: %v", err)
	}
	if _, err := userRepo.List(ctx); err != nil {
		t.Fatalf("runtime user list: %v", err)
	}
	if _, err := userRepo.CountAdmins(ctx); err != nil {
		t.Fatalf("runtime admin count: %v", err)
	}
	if err := userRepo.UpdateRole(ctx, principal.ID, models.RoleAdmin); err != nil {
		t.Fatalf("runtime admin role update: %v", err)
	}
	if err := userRepo.UpdateRole(ctx, principal.ID, models.RoleUser); err != nil {
		t.Fatalf("runtime role restore: %v", err)
	}

	authService := auth.NewService(&appconfig.Config{
		JWTSecret: "strict-integration-jwt-secret", JWTRefreshSecret: "strict-integration-refresh-secret",
	}, runtimePool, userRepo)
	_, refresh, err := authService.IssueTokens(ctx, principal)
	if err != nil {
		t.Fatalf("runtime refresh issue: %v", err)
	}
	_, rotated, _, err := authService.RefreshAccessToken(ctx, refresh)
	if err != nil {
		t.Fatalf("runtime refresh claim/rotation: %v", err)
	}
	authService.RevokeRefresh(ctx, rotated)
	var refreshCount int
	if err := runtimePool.QueryRow(ctx, `SELECT COUNT(*) FROM public.refresh_tokens WHERE user_id=$1`, principal.ID).Scan(&refreshCount); err != nil || refreshCount != 0 {
		t.Fatalf("runtime refresh family revoke: count=%d err=%v", refreshCount, err)
	}

	problemRepo := problem.NewRepository(runtimePool)
	draft := &models.Problem{
		ID: "strict-draft", Title: "Strict draft", Description: "draft", Category: "pod",
		Difficulty: "easy", Type: "fix", TimeoutMinutes: 30, VerifyType: "script",
		BaseImage: "k3s-base:latest",
	}
	if err := problemRepo.Upsert(ctx, draft); err != nil {
		t.Fatalf("runtime problem draft upsert: %v", err)
	}
	if _, err := problemRepo.FindAnyByID(ctx, draft.ID); err != nil {
		t.Fatalf("runtime problem draft find: %v", err)
	}
	if err := problemRepo.Delete(ctx, draft.ID); err != nil {
		t.Fatalf("runtime problem draft delete: %v", err)
	}

	const revision = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	published := models.Problem{
		ID: "strict-published", Revision: revision, Title: "Strict published", Description: "published",
		Category: "pod", Difficulty: "easy", Type: "fix", TimeoutMinutes: 30,
		VerifyType: "script", BaseImage: "k3s-base:latest",
	}
	publicationRef := problem.CatalogPublicationRef{
		DigestSchema: 1, CandidateDigest: sha256.Sum256([]byte("strict-catalog-publication")),
	}
	artifactDigest := sha256.Sum256([]byte("strict-runtime-artifact"))
	entries := []problem.CatalogEntry{{
		Problem: published,
		Artifact: problem.ArtifactRef{
			DigestSchema: problem.ArtifactDigestSchemaV1, Digest: artifactDigest,
			MediaType: problem.RuntimeArtifactMediaTypeV1, Size: 1,
		},
		SourceTrust: problem.BundleSourceDevelopmentCheckout,
	}}
	publicationLease, err := problemRepo.AcquireCatalogPublicationLease(ctx)
	if err != nil {
		t.Fatalf("runtime catalog publication lease: %v", err)
	}
	head, err := publicationLease.Publish(ctx, nil, publicationRef, entries)
	if closeErr := publicationLease.Close(ctx); err == nil && closeErr != nil {
		err = closeErr
	}
	if err != nil {
		t.Fatalf("runtime catalog bootstrap publication: %v", err)
	}
	if _, err := problemRepo.FindByID(ctx, published.ID); err != nil {
		t.Fatalf("runtime active catalog lookup: %v", err)
	}

	providerID := "local-docker:strict-dbsecurity"
	controllerLease, err := runner.AcquireControllerLease(ctx, runtimePool, providerID, func(loss error) {
		t.Errorf("strict controller lease lost: %v", loss)
	})
	if err != nil {
		t.Fatalf("runtime controller lease acquire: %v", err)
	}
	store, err := runner.NewPostgresStore(runtimePool, controllerLease.Fence())
	if err != nil {
		t.Fatalf("construct runtime store: %v", err)
	}
	if err := store.PrepareRecoveryWork(ctx, providerID); err != nil {
		t.Fatalf("runtime recovery preparation: %v", err)
	}

	reservation, err := store.ReserveSession(ctx, runner.ReserveSessionParams{
		SessionID: "11111111-1111-4111-8111-111111111111", UserID: principal.ID,
		Selection: runner.CatalogSelection{
			Generation: head.Generation, Problem: runner.ProblemRef{ID: published.ID, Revision: revision},
		},
		Provider: runner.ProviderLocalDocker, ProviderID: providerID,
		ResourceProfile: runner.DefaultResourceProfile, ExpiresAt: time.Now().UTC().Add(time.Hour),
		IdempotencyKey: "strict-create-generation-one",
	})
	if err != nil {
		t.Fatalf("runtime session reserve: %v", err)
	}
	if err := store.MarkCreateSucceeded(ctx, reservation.Allocation.Ref); err != nil {
		t.Fatalf("runtime create success: %v", err)
	}
	if err := store.MarkSettingUp(ctx, reservation.Allocation.Ref); err != nil {
		t.Fatalf("runtime setup transition: %v", err)
	}
	if err := store.MarkReady(ctx, reservation.Allocation.Ref); err != nil {
		t.Fatalf("runtime ready transition: %v", err)
	}

	reset, err := store.ReserveReset(ctx, runner.ReserveResetParams{
		Expected: reservation.Allocation.Ref.Session, UserID: principal.ID,
		Provider: runner.ProviderLocalDocker, ProviderID: providerID,
		IdempotencyKey: "strict-reset-generation-two",
	})
	if err != nil {
		t.Fatalf("runtime reset reserve: %v", err)
	}
	if err := store.MarkDestroyed(ctx, reset.Old); err != nil {
		t.Fatalf("runtime predecessor cleanup: %v", err)
	}
	if err := store.MarkCreateSucceeded(ctx, reset.New.Allocation.Ref); err != nil {
		t.Fatalf("runtime replacement create success: %v", err)
	}
	if err := store.MarkSettingUp(ctx, reset.New.Allocation.Ref); err != nil {
		t.Fatalf("runtime replacement setup: %v", err)
	}
	if err := store.MarkReady(ctx, reset.New.Allocation.Ref); err != nil {
		t.Fatalf("runtime replacement ready: %v", err)
	}
	decision, err := store.BeginVerify(ctx, principal.ID, reset.New.Allocation.Ref, "strict-verify-operation")
	if err != nil || decision.Kind != runner.VerifyExecute {
		t.Fatalf("runtime verify begin: decision=%+v err=%v", decision, err)
	}
	if err := store.RecordVerifyFinished(ctx, reset.New.Allocation.Ref, "strict-verify-operation", false, "expected retry"); err != nil {
		t.Fatalf("runtime verify result: %v", err)
	}
	if _, err := store.BootstrapLifecycle(ctx, principal.ID, nil); err != nil {
		t.Fatalf("runtime lifecycle bootstrap: %v", err)
	}
	if _, err := userRepo.Leaderboard(ctx, 10); err != nil {
		t.Fatalf("runtime leaderboard: %v", err)
	}
	end, err := store.RequestEnd(ctx, principal.ID, reset.New.Allocation.Ref.Session, "strict-end-operation")
	if err != nil || len(end.Pending) == 0 {
		t.Fatalf("runtime end request: decision=%+v err=%v", end, err)
	}
	if err := store.MarkDestroyed(ctx, reset.New.Allocation.Ref); err != nil {
		t.Fatalf("runtime final cleanup: %v", err)
	}

	assertProofBoundary(t, ctx, runtimePool, validatorPool, admin, controllerLease, security)
	if err := controllerLease.Close(ctx); err != nil {
		t.Fatalf("close runtime controller lease: %v", err)
	}
}

func assertProofBoundary(
	t *testing.T,
	ctx context.Context,
	runtimePool, validatorPool, admin *pgxpool.Pool,
	lease *runner.ControllerLease,
	security dbsecurity.Config,
) {
	t.Helper()
	subject := func(label string) runner.ControllerProofSubject {
		return runner.ControllerProofSubject{
			Operation: runner.ControllerProofCreate, RequestDigest: sha256.Sum256([]byte(label)),
			EffectDeadline: time.Now().UTC().Add(30 * time.Second).Truncate(time.Microsecond),
			IssuerURI:      "spiffe://control.example/controller", AudienceURI: "spiffe://runner.example/private-api",
		}
	}
	proof, err := lease.IssueProof(ctx, subject("accepted-and-replayed"))
	if err != nil {
		t.Fatalf("issue accepted controller proof: %v", err)
	}
	if status, err := consumeProof(ctx, validatorPool, proof); err != nil || status != "accepted" {
		t.Fatalf("consume controller proof: status=%q err=%v", status, err)
	}
	if status, err := consumeProof(ctx, validatorPool, proof); err != nil || status != "replayed" {
		t.Fatalf("replay controller proof: status=%q err=%v", status, err)
	}

	statsProof, err := lease.IssueProof(ctx, subject("stats-membership"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "REVOKE pg_read_all_stats FROM "+pgx.Identifier{strictOwner}.Sanitize()); err != nil {
		t.Fatalf("remove stats visibility for proof test: %v", err)
	}
	if status, err := consumeProof(ctx, validatorPool, statsProof); err != nil || status != "fenced" {
		t.Fatalf("proof without pg_read_all_stats: status=%q err=%v", status, err)
	}
	if _, err := admin.Exec(ctx, "GRANT pg_read_all_stats TO "+pgx.Identifier{strictOwner}.Sanitize()+" WITH INHERIT TRUE, SET FALSE"); err != nil {
		t.Fatalf("restore stats visibility: %v", err)
	}
	if status, err := consumeProof(ctx, validatorPool, statsProof); err != nil || status != "accepted" {
		t.Fatalf("proof after stats restore: status=%q err=%v", status, err)
	}
	if err := dbsecurity.Verify(ctx, runtimePool, validatorPool, security); err != nil {
		t.Fatalf("security verification after stats restore: %v", err)
	}
	assertProofExpiresWhileWaitingForEpochLock(t, ctx, validatorPool, admin, lease)

	fencedProof, err := lease.IssueProof(ctx, subject("normal-close-fence"))
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.Close(ctx); err != nil {
		t.Fatalf("close controller lease before fenced consume: %v", err)
	}
	if status, err := consumeProof(ctx, validatorPool, fencedProof); err != nil || status != "fenced" {
		t.Fatalf("proof after lease close: status=%q err=%v", status, err)
	}
}

func assertProofExpiresWhileWaitingForEpochLock(
	t *testing.T,
	ctx context.Context,
	validatorPool, admin *pgxpool.Pool,
	lease *runner.ControllerLease,
) {
	t.Helper()
	proof, err := lease.IssueProof(ctx, runner.ControllerProofSubject{
		Operation:      runner.ControllerProofCreate,
		RequestDigest:  sha256.Sum256([]byte("expires-behind-epoch-row-lock")),
		EffectDeadline: time.Now().UTC().Add(30 * time.Second).Truncate(time.Microsecond),
		IssuerURI:      "spiffe://control.example/controller",
		AudienceURI:    "spiffe://runner.example/private-api",
	})
	if err != nil {
		t.Fatalf("issue proof for post-lock expiry regression: %v", err)
	}

	lockTx, err := admin.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin epoch row-lock transaction: %v", err)
	}
	defer func() { _ = lockTx.Rollback(context.Background()) }()
	if tag, err := lockTx.Exec(ctx, `UPDATE public.runner_controller_epochs
		SET updated_at=updated_at WHERE provider_id=$1`, proof.ProviderID); err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("lock controller epoch row: rows=%d err=%v", tag.RowsAffected(), err)
	}

	validator, err := validatorPool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire dedicated validator connection: %v", err)
	}
	defer validator.Release()
	var validatorPID int32
	if err := validator.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&validatorPID); err != nil {
		t.Fatalf("read dedicated validator backend pid: %v", err)
	}
	type result struct {
		status string
		err    error
	}
	resultCh := make(chan result, 1)
	go func() {
		status, consumeErr := consumeProof(ctx, validator, proof)
		resultCh <- result{status: status, err: consumeErr}
	}()

	waitDeadline := time.Now().Add(3 * time.Second)
	blocked := false
	for time.Now().Before(waitDeadline) {
		var waiting bool
		if err := admin.QueryRow(ctx, `SELECT COALESCE((
			SELECT wait_event_type='Lock' FROM pg_catalog.pg_stat_activity WHERE pid=$1
		),FALSE)`, validatorPID).Scan(&waiting); err != nil {
			t.Fatalf("inspect blocked proof consumer: %v", err)
		}
		if waiting {
			blocked = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !blocked {
		t.Fatal("proof consumer did not block behind the controller epoch row lock")
	}

	if remaining := time.Until(proof.ExpiresAt.Add(250 * time.Millisecond)); remaining > 0 {
		timer := time.NewTimer(remaining)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			t.Fatalf("wait for proof expiry: %v", ctx.Err())
		}
	}
	if err := lockTx.Commit(ctx); err != nil {
		t.Fatalf("release controller epoch row lock: %v", err)
	}
	select {
	case got := <-resultCh:
		if got.err != nil || got.status != "unauthorized" {
			t.Fatalf("post-lock expired proof: status=%q err=%v", got.status, got.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("post-lock expired proof consumer did not return")
	}
	var consumptionCount int
	if err := admin.QueryRow(ctx, `SELECT COUNT(*) FROM public.runner_controller_proof_consumptions
		WHERE provider_id=$1 AND proof_id=$2::uuid`, proof.ProviderID, proof.ProofID).Scan(&consumptionCount); err != nil {
		t.Fatalf("inspect post-lock expired proof consumption: %v", err)
	}
	if consumptionCount != 0 {
		t.Fatalf("post-lock expired proof created %d consumption rows", consumptionCount)
	}
}

type proofQueryer interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func consumeProof(ctx context.Context, validator proofQueryer, proof runner.ControllerAuthorityProof) (string, error) {
	token := proof.Token()
	tokenHash := sha256.Sum256(token[:])
	clear(token[:])
	var status string
	err := validator.QueryRow(ctx, `SELECT public.consume_runner_controller_proof_v2(
		$1,$2,$3::uuid,$4::uuid,$5,$6,$7,$8,$9,$10,$11,$12)`,
		proof.ProviderID, int64(proof.Epoch), proof.LeaseID, proof.ProofID,
		string(proof.Operation), proof.RequestDigest[:], proof.EffectDeadline,
		proof.IssuedAt, proof.ExpiresAt, proof.IssuerURI, proof.AudienceURI, tokenHash[:],
	).Scan(&status)
	return status, err
}

func assertDriftIsRejected(
	t *testing.T,
	ctx context.Context,
	runtimePool, validatorPool, admin *pgxpool.Pool,
	security dbsecurity.Config,
) {
	t.Helper()
	if _, err := admin.Exec(ctx, "GRANT SELECT ON public.runner_controller_proof_consumptions TO "+pgx.Identifier{strictRuntime}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	if err := dbsecurity.VerifyRuntime(ctx, runtimePool, security); err == nil {
		t.Fatal("VerifyRuntime accepted excess runtime ledger grant")
	}
	if _, err := admin.Exec(ctx, "REVOKE SELECT ON public.runner_controller_proof_consumptions FROM "+pgx.Identifier{strictRuntime}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	if err := dbsecurity.VerifyRuntime(ctx, runtimePool, security); err != nil {
		t.Fatalf("VerifyRuntime did not recover after runtime grant removal: %v", err)
	}

	if _, err := admin.Exec(ctx, "GRANT SELECT ON public.users TO "+pgx.Identifier{strictValidator}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	if err := dbsecurity.Verify(ctx, runtimePool, validatorPool, security); err == nil {
		t.Fatal("Verify accepted excess validator table grant")
	}
	if _, err := admin.Exec(ctx, "REVOKE SELECT ON public.users FROM "+pgx.Identifier{strictValidator}.Sanitize()); err != nil {
		t.Fatal(err)
	}

	if _, err := admin.Exec(ctx, "GRANT pg_monitor TO "+pgx.Identifier{strictRuntime}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	if err := dbsecurity.VerifyRuntime(ctx, runtimePool, security); err == nil || !strings.Contains(err.Error(), "membership graph") {
		t.Fatalf("VerifyRuntime accepted unexpected role membership: %v", err)
	}
	if _, err := admin.Exec(ctx, "REVOKE pg_monitor FROM "+pgx.Identifier{strictRuntime}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	if err := dbsecurity.Verify(ctx, runtimePool, validatorPool, security); err != nil {
		t.Fatalf("security verification did not recover after drift cleanup: %v", err)
	}
}
