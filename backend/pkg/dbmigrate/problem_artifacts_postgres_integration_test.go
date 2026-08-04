package dbmigrate

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	problemArtifactTestID        = "artifact-problem"
	problemArtifactTestRevision  = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	problemArtifactUnmappedID    = "artifact-unmapped"
	problemArtifactUnmappedRev   = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	problemArtifactTestMediaType = "application/vnd.k8s-quiz.problem-runtime.v1"
)

var (
	problemArtifactCatalogDigest = bytes.Repeat([]byte{0x11}, 32)
	problemArtifactDigest        = bytes.Repeat([]byte{0x22}, 32)
)

type problemArtifactV11Snapshot struct {
	Generation         int64
	PreviousGeneration sql.NullInt64
	DigestSchema       int16
	CandidateDigest    []byte
	EntryCount         int
	PublicationCreated time.Time
	ProblemID          string
	ProblemRevision    string
	Title              string
	EntryCreated       time.Time
	HeadGeneration     int64
}

func TestProblemArtifactMigrationPreservesLegacyPublicationAndGatesSessions(t *testing.T) {
	fixture := openCatalogLedgerMigrationFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	problemArtifactMigrateAndSeedV11(t, ctx, fixture)
	want := problemArtifactReadV11Snapshot(t, ctx, fixture.pool)

	if err := fixture.migrator.Migrate(12); err != nil {
		t.Fatalf("migrate artifact fixture to v12: %v", err)
	}
	got := problemArtifactReadV11Snapshot(t, ctx, fixture.pool)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("v11 publication changed during v12 migration:\n got: %#v\nwant: %#v", got, want)
	}

	var artifactCount int
	if err := fixture.pool.QueryRow(ctx, `SELECT COUNT(*) FROM problem_artifacts`).Scan(&artifactCount); err != nil {
		t.Fatalf("count initial artifact bindings: %v", err)
	}
	if artifactCount != 0 {
		t.Fatalf("migration synthesized %d artifact bindings for legacy publication, want 0", artifactCount)
	}

	problemArtifactAssertSessionRejectedWithoutBinding(t, ctx, fixture.pool)
	problemArtifactInsertBinding(t, ctx, fixture.pool, problemArtifactDigest)
	problemArtifactInsertSessionAndAllocation(t, ctx, fixture.pool)
}

func TestProblemArtifactBindingsAreAppendOnlyAndRequiredForNewEntries(t *testing.T) {
	fixture := openCatalogLedgerMigrationFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	problemArtifactMigrateAndSeedV11(t, ctx, fixture)
	if err := fixture.migrator.Migrate(12); err != nil {
		t.Fatalf("migrate append-only fixture to v12: %v", err)
	}
	problemArtifactInsertBinding(t, ctx, fixture.pool, problemArtifactDigest)

	mutationCases := []struct {
		name    string
		sql     string
		code    string
		message string
	}{
		{
			name:    "update",
			sql:     `UPDATE problem_artifacts SET artifact_size=artifact_size+1 WHERE problem_id='artifact-problem'`,
			code:    "55000",
			message: "problem_artifacts is append-only",
		},
		{
			name:    "delete",
			sql:     `DELETE FROM problem_artifacts WHERE problem_id='artifact-problem'`,
			code:    "55000",
			message: "problem_artifacts is append-only",
		},
		{
			name:    "truncate",
			sql:     `TRUNCATE TABLE problem_artifacts`,
			code:    "0A000",
			message: "cannot truncate a table referenced in a foreign key constraint",
		},
	}
	for _, tc := range mutationCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := fixture.pool.Exec(ctx, tc.sql)
			problemArtifactRequirePostgresError(t, err, tc.code, tc.message, "")
		})
	}

	_, err := fixture.pool.Exec(ctx, `
		INSERT INTO problem_artifacts (
			problem_id,problem_revision,artifact_digest_schema,artifact_digest,
			artifact_media_type,artifact_size,source_trust
		) VALUES ($1,$2,1,$3,$4,2048,'development_checkout')`,
		problemArtifactTestID, problemArtifactTestRevision,
		bytes.Repeat([]byte{0x33}, 32), problemArtifactTestMediaType)
	problemArtifactRequirePostgresError(t, err, "23505", "", "problem_artifacts_pkey")

	publicationTx, err := fixture.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin unmapped publication: %v", err)
	}
	defer func() { _ = publicationTx.Rollback(context.Background()) }()
	if _, err := publicationTx.Exec(ctx, `
		INSERT INTO problem_catalog_publications (
			generation,previous_generation,digest_schema,candidate_digest,entry_count
		) VALUES (2,1,2,$1,1)`, bytes.Repeat([]byte{0x44}, 32)); err != nil {
		t.Fatalf("insert pending publication for unmapped entry: %v", err)
	}
	_, err = publicationTx.Exec(ctx, `
		INSERT INTO problem_catalog_entries (
			catalog_generation,problem_id,problem_revision,title,category,difficulty,
			type,timeout_minutes,verify_type
		) VALUES (2,$1,$2,'Unmapped artifact','pod','easy','fix',30,'script')`,
		problemArtifactUnmappedID, problemArtifactUnmappedRev)
	problemArtifactRequirePostgresError(t, err, "23503", "", "problem_catalog_entries_artifact_fk")
	if err := publicationTx.Rollback(ctx); err != nil {
		t.Fatalf("roll back rejected unmapped publication: %v", err)
	}

	var artifactCount int
	if err := fixture.pool.QueryRow(ctx, `SELECT COUNT(*) FROM problem_artifacts`).Scan(&artifactCount); err != nil {
		t.Fatalf("count bindings after rejected mutations: %v", err)
	}
	if artifactCount != 1 {
		t.Fatalf("artifact binding count after rejected mutations = %d, want 1", artifactCount)
	}
	var generationTwoCount int
	if err := fixture.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM problem_catalog_publications WHERE generation=2`).Scan(&generationTwoCount); err != nil {
		t.Fatalf("count rolled-back generation 2 publication: %v", err)
	}
	if generationTwoCount != 0 {
		t.Fatalf("rejected unmapped publication persisted %d generation 2 rows", generationTwoCount)
	}
}

func TestProblemArtifactMigrationDownSucceedsOnlyWhenBindingsAreEmpty(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		fixture := openCatalogLedgerMigrationFixture(t)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()

		problemArtifactMigrateAndSeedV11(t, ctx, fixture)
		want := problemArtifactReadV11Snapshot(t, ctx, fixture.pool)
		if err := fixture.migrator.Migrate(12); err != nil {
			t.Fatalf("migrate empty downgrade fixture to v12: %v", err)
		}
		if err := fixture.migrator.Steps(-1); err != nil {
			t.Fatalf("downgrade empty artifact bindings to v11: %v", err)
		}

		version, dirty, err := fixture.migrator.Version()
		if err != nil {
			t.Fatalf("read migration version after safe artifact downgrade: %v", err)
		}
		if version != 11 || dirty {
			t.Fatalf("migration state after safe artifact downgrade = v%d dirty=%v, want v11 clean", version, dirty)
		}
		var artifactTable sql.NullString
		if err := fixture.pool.QueryRow(ctx, `SELECT to_regclass('problem_artifacts')::text`).Scan(&artifactTable); err != nil {
			t.Fatalf("inspect artifact table after safe downgrade: %v", err)
		}
		if artifactTable.Valid {
			t.Fatalf("artifact table remains after safe downgrade: %q", artifactTable.String)
		}
		got := problemArtifactReadV11Snapshot(t, ctx, fixture.pool)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("legacy publication changed during safe v12 down migration:\n got: %#v\nwant: %#v", got, want)
		}
	})

	t.Run("non-empty", func(t *testing.T) {
		fixture := openCatalogLedgerMigrationFixture(t)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()

		problemArtifactMigrateAndSeedV11(t, ctx, fixture)
		if err := fixture.migrator.Migrate(12); err != nil {
			t.Fatalf("migrate non-empty downgrade fixture to v12: %v", err)
		}
		problemArtifactInsertBinding(t, ctx, fixture.pool, problemArtifactDigest)

		err := fixture.migrator.Steps(-1)
		if err == nil || !strings.Contains(err.Error(), "cannot remove problem artifact bindings after artifacts have been published") {
			t.Fatalf("non-empty artifact downgrade error = %v, want fail-closed refusal", err)
		}
		var artifactCount int
		if err := fixture.pool.QueryRow(ctx, `SELECT COUNT(*) FROM problem_artifacts`).Scan(&artifactCount); err != nil {
			t.Fatalf("count bindings after rejected downgrade: %v", err)
		}
		if artifactCount != 1 {
			t.Fatalf("artifact bindings after rejected downgrade = %d, want 1", artifactCount)
		}
	})
}

func TestProblemArtifactDowngradeWaitsForConcurrentBinding(t *testing.T) {
	fixture := openCatalogLedgerMigrationFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	problemArtifactMigrateAndSeedV11(t, ctx, fixture)
	if err := fixture.migrator.Migrate(12); err != nil {
		t.Fatalf("migrate concurrent downgrade fixture to v12: %v", err)
	}

	bindingTx, err := fixture.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin concurrent artifact binding: %v", err)
	}
	defer func() { _ = bindingTx.Rollback(context.Background()) }()
	if _, err := bindingTx.Exec(ctx, `
		INSERT INTO problem_artifacts (
			problem_id,problem_revision,artifact_digest_schema,artifact_digest,
			artifact_media_type,artifact_size,source_trust
		) VALUES ($1,$2,1,$3,$4,1024,'development_checkout')`,
		problemArtifactTestID, problemArtifactTestRevision,
		problemArtifactDigest, problemArtifactTestMediaType); err != nil {
		t.Fatalf("insert uncommitted artifact binding: %v", err)
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
				  AND relation='problem_artifacts'::regclass
				  AND mode='AccessExclusiveLock'
				  AND NOT granted
			)`).Scan(&waiting); err != nil {
			t.Fatalf("observe artifact downgrade table lock: %v", err)
		}
		if waiting {
			break
		}
		select {
		case err := <-downResult:
			t.Fatalf("artifact downgrade did not wait for concurrent binding: %v", err)
		default:
		}
		if time.Now().After(waitDeadline) {
			t.Fatal("artifact downgrade did not request an exclusive artifact-table lock")
		}
		time.Sleep(10 * time.Millisecond)
	}

	if err := bindingTx.Commit(ctx); err != nil {
		t.Fatalf("commit concurrent artifact binding: %v", err)
	}
	select {
	case err := <-downResult:
		if err == nil || !strings.Contains(err.Error(), "cannot remove problem artifact bindings after artifacts have been published") {
			t.Fatalf("concurrent artifact downgrade error = %v, want fail-closed refusal", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("artifact downgrade did not resume after concurrent binding commit")
	}

	var artifactCount int
	if err := fixture.pool.QueryRow(ctx, `SELECT COUNT(*) FROM problem_artifacts`).Scan(&artifactCount); err != nil {
		t.Fatalf("count concurrent binding after rejected downgrade: %v", err)
	}
	if artifactCount != 1 {
		t.Fatalf("concurrent binding count after rejected downgrade = %d, want 1", artifactCount)
	}
}

func problemArtifactMigrateAndSeedV11(t *testing.T, ctx context.Context, fixture *catalogLedgerMigrationFixture) {
	t.Helper()
	if err := fixture.migrator.Migrate(11); err != nil {
		t.Fatalf("migrate artifact fixture to v11: %v", err)
	}

	tx, err := fixture.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin v11 artifact fixture seed: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `
		INSERT INTO users (id,github_id,username) VALUES
			('a1000000-0000-4000-8000-000000000001',9501,'artifact-session-user')`); err != nil {
		t.Fatalf("seed v11 artifact user: %v", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO problems (id,title,revision,catalog_active) VALUES
			($1,'Artifact problem',$2,TRUE),
			($3,'Unmapped artifact problem',$4,FALSE)`,
		problemArtifactTestID, problemArtifactTestRevision,
		problemArtifactUnmappedID, problemArtifactUnmappedRev); err != nil {
		t.Fatalf("seed v11 artifact identities: %v", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO problem_catalog_publications (
			generation,previous_generation,digest_schema,candidate_digest,entry_count
		) VALUES (1,NULL,1,$1,1)`, problemArtifactCatalogDigest); err != nil {
		t.Fatalf("seed v11 artifact publication identity: %v", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO problem_catalog_entries (
			catalog_generation,problem_id,problem_revision,title,category,difficulty,
			type,timeout_minutes,verify_type
		) VALUES (1,$1,$2,'Artifact problem','pod','easy','fix',30,'script')`,
		problemArtifactTestID, problemArtifactTestRevision); err != nil {
		t.Fatalf("seed v11 artifact publication entry: %v", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO problem_catalog_head (singleton,catalog_generation)
		VALUES (TRUE,1)`); err != nil {
		t.Fatalf("seed v11 artifact publication head: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit v11 artifact fixture seed: %v", err)
	}
}

func problemArtifactReadV11Snapshot(t *testing.T, ctx context.Context, pool *pgxpool.Pool) problemArtifactV11Snapshot {
	t.Helper()
	var snapshot problemArtifactV11Snapshot
	if err := pool.QueryRow(ctx, `
		SELECT p.generation,p.previous_generation,p.digest_schema,p.candidate_digest,
		       p.entry_count,p.created_at,e.problem_id,e.problem_revision,e.title,e.created_at,
		       h.catalog_generation
		FROM problem_catalog_publications p
		JOIN problem_catalog_entries e ON e.catalog_generation=p.generation
		JOIN problem_catalog_head h ON h.catalog_generation=p.generation
		WHERE p.generation=1 AND e.problem_id=$1`, problemArtifactTestID).Scan(
		&snapshot.Generation, &snapshot.PreviousGeneration, &snapshot.DigestSchema,
		&snapshot.CandidateDigest, &snapshot.EntryCount, &snapshot.PublicationCreated,
		&snapshot.ProblemID, &snapshot.ProblemRevision, &snapshot.Title,
		&snapshot.EntryCreated, &snapshot.HeadGeneration,
	); err != nil {
		t.Fatalf("read v11 artifact publication snapshot: %v", err)
	}
	return snapshot
}

func problemArtifactAssertSessionRejectedWithoutBinding(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin unmapped artifact session: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	_, err = tx.Exec(ctx, `
		INSERT INTO sessions (
			id,user_id,problem_id,problem_revision,catalog_generation,current_generation,
			state,desired_state,queued_at,expires_at
		) VALUES (
			'a2000000-0000-4000-8000-000000000001',
			'a1000000-0000-4000-8000-000000000001',$1,$2,1,1,
			'queued','active',NOW(),NOW()+INTERVAL '1 hour'
		)`, problemArtifactTestID, problemArtifactTestRevision)
	problemArtifactRequirePostgresError(
		t, err, "23503",
		"new durable session catalog selection is not the current artifact-bound head", "",
	)
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("roll back rejected unmapped artifact session: %v", err)
	}

	var sessionCount int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM sessions WHERE id='a2000000-0000-4000-8000-000000000001'`).Scan(&sessionCount); err != nil {
		t.Fatalf("count rejected unmapped artifact session: %v", err)
	}
	if sessionCount != 0 {
		t.Fatalf("rejected unmapped artifact session persisted %d rows", sessionCount)
	}
}

func problemArtifactInsertBinding(t *testing.T, ctx context.Context, pool *pgxpool.Pool, digest []byte) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
		INSERT INTO problem_artifacts (
			problem_id,problem_revision,artifact_digest_schema,artifact_digest,
			artifact_media_type,artifact_size,source_trust
		) VALUES ($1,$2,1,$3,$4,1024,'development_checkout')`,
		problemArtifactTestID, problemArtifactTestRevision, digest, problemArtifactTestMediaType); err != nil {
		t.Fatalf("insert exact artifact binding: %v", err)
	}
}

func problemArtifactInsertSessionAndAllocation(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin artifact-bound session: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `
		INSERT INTO sessions (
			id,user_id,problem_id,problem_revision,catalog_generation,current_generation,
			state,desired_state,queued_at,expires_at
		) VALUES (
			'a2000000-0000-4000-8000-000000000001',
			'a1000000-0000-4000-8000-000000000001',$1,$2,1,1,
			'queued','active',NOW(),NOW()+INTERVAL '1 hour'
		)`, problemArtifactTestID, problemArtifactTestRevision); err != nil {
		t.Fatalf("insert artifact-bound session: %v", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO runner_allocations (
			id,session_id,generation,provider_kind,provider_id,resource_profile,
			desired_state,observed_state,expires_at
		) VALUES (
			'a3000000-0000-4000-8000-000000000001',
			'a2000000-0000-4000-8000-000000000001',1,
			'local-docker','migration-v12-artifact','default','active','unknown',NOW()+INTERVAL '1 hour'
		)`); err != nil {
		t.Fatalf("insert artifact-bound allocation: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit artifact-bound session: %v", err)
	}

	var generation int64
	if err := pool.QueryRow(ctx, `
		SELECT catalog_generation FROM sessions
		WHERE id='a2000000-0000-4000-8000-000000000001'`).Scan(&generation); err != nil {
		t.Fatalf("read admitted artifact-bound session: %v", err)
	}
	if generation != 1 {
		t.Fatalf("admitted artifact-bound session generation = %d, want 1", generation)
	}
}

func problemArtifactRequirePostgresError(t *testing.T, err error, code, message, constraint string) {
	t.Helper()
	if err == nil {
		t.Fatalf("database operation succeeded, want SQLSTATE %s", code)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("database operation error = %v, want PostgreSQL error", err)
	}
	if pgErr.Code != code {
		t.Fatalf("database operation SQLSTATE = %s (%v), want %s", pgErr.Code, err, code)
	}
	if message != "" && !strings.Contains(pgErr.Message, message) {
		t.Fatalf("database operation message = %q, want substring %q", pgErr.Message, message)
	}
	if constraint != "" && pgErr.ConstraintName != constraint {
		t.Fatalf("database operation constraint = %q (%v), want %q", pgErr.ConstraintName, err, constraint)
	}
}
