package problem

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/k8s-quiz/backend/internal/runner"
	"github.com/k8s-quiz/backend/pkg/dbmigrate"
	"github.com/k8s-quiz/backend/pkg/models"
)

const problemCatalogTestDatabaseEnv = "PROBLEM_CATALOG_TEST_DATABASE_URL"

var problemCatalogSchemaSequence atomic.Uint64

func openProblemCatalogTestRepository(t *testing.T) (*Repository, *pgxpool.Pool) {
	t.Helper()
	databaseURL := os.Getenv(problemCatalogTestDatabaseEnv)
	if databaseURL == "" {
		t.Skip(problemCatalogTestDatabaseEnv + " is not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	admin, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open problem catalog integration database: %v", err)
	}
	if err := admin.Ping(ctx); err != nil {
		admin.Close()
		t.Fatalf("ping problem catalog integration database: %v", err)
	}
	prepareProblemCatalogTestExtensions(t, ctx, admin)

	sequence := problemCatalogSchemaSequence.Add(1)
	schema := fmt.Sprintf("problem_catalog_%d_%d", time.Now().UnixNano(), sequence)
	identifier := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+identifier); err != nil {
		admin.Close()
		t.Fatalf("create isolated problem catalog schema: %v", err)
	}

	parsed, err := url.Parse(databaseURL)
	if err != nil {
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+identifier+" CASCADE")
		admin.Close()
		t.Fatalf("parse problem catalog database URL: %v", err)
	}
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	isolatedURL := parsed.String()
	if err := dbmigrate.UpIsolatedTestSchema(isolatedURL); err != nil {
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+identifier+" CASCADE")
		admin.Close()
		t.Fatalf("migrate isolated problem catalog schema: %v", err)
	}

	pool, err := pgxpool.New(ctx, isolatedURL)
	if err != nil {
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+identifier+" CASCADE")
		admin.Close()
		t.Fatalf("open isolated problem catalog schema: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+identifier+" CASCADE")
		admin.Close()
		t.Fatalf("ping isolated problem catalog schema: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer dropCancel()
		if _, err := admin.Exec(dropCtx, "DROP SCHEMA "+identifier+" CASCADE"); err != nil {
			t.Errorf("drop isolated problem catalog schema: %v", err)
		}
		admin.Close()
	})
	return NewRepository(pool), pool
}

func prepareProblemCatalogTestExtensions(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
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

func catalogIntegrationProblem(id, revision, title string) models.Problem {
	return models.Problem{
		ID: id, Revision: revision, Title: title, Description: title,
		Category: "pod", Difficulty: "easy", Type: "fix",
		TimeoutMinutes: 30, VerifyType: "script", BaseImage: "k3s-base:latest",
	}
}

func catalogIntegrationRef(label string) CatalogPublicationRef {
	return CatalogPublicationRef{
		DigestSchema:    1,
		CandidateDigest: sha256.Sum256([]byte("k8s-quiz-test-catalog:" + label)),
	}
}

func catalogIntegrationEntries(problems ...models.Problem) []CatalogEntry {
	entries := make([]CatalogEntry, len(problems))
	for i := range problems {
		digest := sha256.Sum256([]byte("k8s-quiz-test-artifact\x00" + problems[i].ID + "\x00" + problems[i].Revision))
		entries[i] = CatalogEntry{
			Problem: cloneProblem(problems[i]),
			Artifact: ArtifactRef{
				DigestSchema: ArtifactDigestSchemaV1,
				Digest:       digest,
				MediaType:    RuntimeArtifactMediaTypeV1,
				Size:         1,
			},
			SourceTrust: BundleSourceDevelopmentCheckout,
		}
	}
	return entries
}

func publishCatalogIntegration(t *testing.T, repo *Repository, problems []models.Problem) CatalogHead {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	lease, err := repo.AcquireCatalogPublicationLease(ctx)
	if err != nil {
		t.Fatalf("acquire catalog publication lease: %v", err)
	}
	defer func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer closeCancel()
		if err := lease.Close(closeCtx); err != nil {
			t.Errorf("close catalog publication lease: %v", err)
		}
	}()
	expected, err := lease.Head(ctx)
	if err != nil {
		t.Fatalf("read catalog head before publish: %v", err)
	}
	ref := catalogIntegrationRef(fmt.Sprintf("generation-%d", func() uint64 {
		if expected == nil {
			return 1
		}
		return expected.Generation + 1
	}()))
	head, err := lease.Publish(ctx, expected, ref, catalogIntegrationEntries(problems...))
	if err != nil {
		t.Fatalf("publish catalog generation: %v", err)
	}
	return head
}

func requirePostgresCode(t *testing.T, err error, codes ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("operation unexpectedly succeeded; want one of SQLSTATE %v", codes)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("error = %v, want PostgreSQL error", err)
	}
	for _, code := range codes {
		if pgErr.Code == code {
			return
		}
	}
	t.Fatalf("SQLSTATE = %s, want one of %v: %v", pgErr.Code, codes, err)
}

func TestPostgresReplaceActiveCatalogIsAtomicAndRetainsIdentityRows(t *testing.T) {
	repo, pool := openProblemCatalogTestRepository(t)
	ctx := context.Background()
	revisionA := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	revisionB := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	revisionC := "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"

	first := []models.Problem{
		catalogIntegrationProblem("catalog-a", revisionA, "A version one"),
		catalogIntegrationProblem("catalog-retired", revisionA, "Retired identity"),
	}
	if err := repo.ReplaceActiveCatalog(ctx, first); err != nil {
		t.Fatalf("publish first catalog: %v", err)
	}
	active, err := repo.ListAll(ctx)
	if err != nil || len(active) != 2 {
		t.Fatalf("first active catalog = %+v, err=%v", active, err)
	}

	var retiredUpdatedAt time.Time
	if err := pool.QueryRow(ctx, `SELECT updated_at FROM problems WHERE id='catalog-retired'`).Scan(&retiredUpdatedAt); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Millisecond)
	if err := repo.ReplaceActiveCatalog(ctx, []models.Problem{
		catalogIntegrationProblem("catalog-a", revisionB, "A version two"),
	}); err != nil {
		t.Fatalf("publish replacement catalog: %v", err)
	}

	current, err := repo.FindAnyByID(ctx, "catalog-a")
	if err != nil || current.Revision != revisionB || current.Title != "A version two" || !current.CatalogActive {
		t.Fatalf("replacement active problem = %+v, err=%v", current, err)
	}
	retired, err := repo.FindAnyByID(ctx, "catalog-retired")
	if err != nil || retired.CatalogActive || retired.Revision != revisionA || !retired.UpdatedAt.After(retiredUpdatedAt) {
		t.Fatalf("retained inactive identity = %+v, err=%v, previous updated_at=%v", retired, err, retiredUpdatedAt)
	}

	// The second entry is deliberately invalid after the first upsert. The
	// transaction must roll back both that upsert and the initial deactivation.
	err = repo.ReplaceActiveCatalog(ctx, []models.Problem{
		catalogIntegrationProblem("catalog-a", revisionC, "must roll back"),
		catalogIntegrationProblem("catalog-invalid", "", "missing revision"),
	})
	if err == nil {
		t.Fatal("invalid replacement unexpectedly committed")
	}
	current, err = repo.FindAnyByID(ctx, "catalog-a")
	if err != nil || current.Revision != revisionB || current.Title != "A version two" || !current.CatalogActive {
		t.Fatalf("failed replacement changed active catalog = %+v, err=%v", current, err)
	}
	if _, err := repo.FindAnyByID(ctx, "catalog-invalid"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("failed replacement left partial row, err=%v", err)
	}
	all, err := repo.ListAll(ctx)
	if err != nil || len(all) != 2 {
		t.Fatalf("identity rows after rollback = %+v, err=%v", all, err)
	}
}

func TestPostgresCatalogActiveConstraintRejectsRevisionlessRows(t *testing.T) {
	_, pool := openProblemCatalogTestRepository(t)
	_, err := pool.Exec(context.Background(), `
		INSERT INTO problems (id,title,revision,catalog_active)
		VALUES ('invalid-active','Invalid','',TRUE)`)
	if err == nil {
		t.Fatal("migration 010 allowed an active problem without a revision")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
		t.Fatalf("active revision constraint error = %v, want SQLSTATE 23514", err)
	}
}

func TestPostgresCatalogLedgerRetainsHistoricalSelectionAcrossPublication(t *testing.T) {
	repo, pool := openProblemCatalogTestRepository(t)
	ctx := context.Background()
	const (
		problemID = "ledger-history"
		revisionA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		revisionB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		userID    = "11111111-1111-4111-8111-111111111111"
		sessionID = "22222222-2222-4222-8222-222222222222"
		allocID   = "33333333-3333-4333-8333-333333333333"
	)

	headA := publishCatalogIntegration(t, repo, []models.Problem{
		catalogIntegrationProblem(problemID, revisionA, "Version A"),
	})
	if headA.Generation != 1 || headA.EntryCount != 1 {
		t.Fatalf("first catalog head = %+v", headA)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO users (id,github_id,username) VALUES ($1,10001,'ledger-history-user')`, userID); err != nil {
		t.Fatal(err)
	}
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `
		INSERT INTO sessions (
			id,user_id,problem_id,problem_revision,catalog_generation,current_generation,
			state,desired_state,queued_at,expires_at
		) VALUES ($1,$2,$3,$4,1,1,'queued','active',NOW(),NOW()+INTERVAL '1 hour')`,
		sessionID, userID, problemID, revisionA); err != nil {
		t.Fatalf("insert generation-one session: %v", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO runner_allocations (
			id,session_id,generation,provider_kind,provider_id,resource_profile,
			desired_state,observed_state,expires_at
		) VALUES ($1,$2,1,'local-docker','local-docker:ledger-test','small','active','unknown',NOW()+INTERVAL '1 hour')`,
		allocID, sessionID); err != nil {
		t.Fatalf("insert generation-one allocation: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit generation-one session: %v", err)
	}

	headB := publishCatalogIntegration(t, repo, []models.Problem{
		catalogIntegrationProblem(problemID, revisionB, "Version B"),
	})
	if headB.Generation != 2 || headB.EntryCount != 1 {
		t.Fatalf("second catalog head = %+v", headB)
	}
	var entryA, entryB, boundSessions int
	if err := pool.QueryRow(ctx, `SELECT
		(SELECT COUNT(*) FROM problem_catalog_entries WHERE catalog_generation=1 AND problem_id=$1 AND problem_revision=$2),
		(SELECT COUNT(*) FROM problem_catalog_entries WHERE catalog_generation=2 AND problem_id=$1 AND problem_revision=$3),
		(SELECT COUNT(*) FROM sessions WHERE id=$4 AND catalog_generation=1 AND problem_revision=$2)`,
		problemID, revisionA, revisionB, sessionID).Scan(&entryA, &entryB, &boundSessions); err != nil {
		t.Fatal(err)
	}
	if entryA != 1 || entryB != 1 || boundSessions != 1 {
		t.Fatalf("historical ledger counts A=%d B=%d session=%d", entryA, entryB, boundSessions)
	}
	activeHead, active, err := repo.FindActiveCatalogProblem(ctx, problemID)
	if err != nil {
		t.Fatal(err)
	}
	if activeHead.Generation != 2 || active.Revision != revisionB || active.Title != "Version B" {
		t.Fatalf("active catalog = head=%+v problem=%+v", activeHead, active)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE problems SET revision=$2,catalog_active=FALSE,title='forged mutable projection'
		WHERE id=$1`, problemID, revisionA); err != nil {
		t.Fatalf("mutate compatibility projection: %v", err)
	}
	publicProblem, err := repo.FindByID(ctx, problemID)
	if err != nil || publicProblem.Revision != revisionB || publicProblem.Title != "Version B" || !publicProblem.CatalogActive {
		t.Fatalf("public lookup trusted mutable identity projection: problem=%+v err=%v", publicProblem, err)
	}
	publicList, err := repo.List(ctx, "pod", "easy", "fix")
	if err != nil || len(publicList) != 1 || publicList[0].Revision != revisionB || publicList[0].Title != "Version B" {
		t.Fatalf("public list trusted mutable identity projection: problems=%+v err=%v", publicList, err)
	}
	for name, mutate := range map[string]func() error{
		"update": func() error {
			return repo.Upsert(ctx, &models.Problem{
				ID: problemID, Title: "admin overwrite", Category: "pod",
				Difficulty: "easy", Type: "fix", TimeoutMinutes: 30, VerifyType: "script",
			})
		},
		"delete": func() error { return repo.Delete(ctx, problemID) },
	} {
		if err := mutate(); !errors.Is(err, ErrCatalogManagedProblem) {
			t.Fatalf("admin %s after projection corruption error=%v, want ErrCatalogManagedProblem", name, err)
		}
	}

	_, err = pool.Exec(ctx, `
		INSERT INTO sessions (
			id,user_id,problem_id,problem_revision,catalog_generation,current_generation,
			state,desired_state,queued_at,expires_at
		) VALUES ('44444444-4444-4444-8444-444444444444',$1,$2,$3,1,1,
			'queued','absent',NOW(),NOW()+INTERVAL '1 hour')`, userID, problemID, revisionA)
	requirePostgresCode(t, err, "23503")
}

func TestPostgresCatalogLedgerSealsPublicationAndEntries(t *testing.T) {
	repo, pool := openProblemCatalogTestRepository(t)
	ctx := context.Background()
	const revision = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	publishCatalogIntegration(t, repo, []models.Problem{
		catalogIntegrationProblem("sealed-problem", revision, "Sealed"),
	})
	if _, err := pool.Exec(ctx, `INSERT INTO problems (id,title,revision) VALUES ('late-entry','Late',$1)`, revision); err != nil {
		t.Fatal(err)
	}

	_, err := pool.Exec(ctx, `
		INSERT INTO problem_catalog_entries (
			catalog_generation,problem_id,problem_revision,title,category,difficulty,type,timeout_minutes,verify_type
		) VALUES (1,'late-entry',$1,'Late','pod','easy','fix',30,'script')`, revision)
	requirePostgresCode(t, err, "55000")
	_, err = pool.Exec(ctx, `UPDATE problem_catalog_entries SET title='mutated' WHERE catalog_generation=1`)
	requirePostgresCode(t, err, "55000")
	_, err = pool.Exec(ctx, `DELETE FROM problem_catalog_entries WHERE catalog_generation=1`)
	requirePostgresCode(t, err, "55000")
	_, err = pool.Exec(ctx, `TRUNCATE problem_catalog_entries`)
	requirePostgresCode(t, err, "55000", "0A000")

	_, err = pool.Exec(ctx, `UPDATE problem_catalog_publications SET entry_count=2 WHERE generation=1`)
	requirePostgresCode(t, err, "55000")
	_, err = pool.Exec(ctx, `DELETE FROM problem_catalog_publications WHERE generation=1`)
	requirePostgresCode(t, err, "55000")
	_, err = pool.Exec(ctx, `TRUNCATE problem_catalog_publications`)
	requirePostgresCode(t, err, "55000", "0A000")
}

func TestPostgresCatalogLedgerRejectsOrphanPublicationAndNewNullSession(t *testing.T) {
	repo, pool := openProblemCatalogTestRepository(t)
	ctx := context.Background()
	const (
		revisionA = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
		revisionB = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	)
	publishCatalogIntegration(t, repo, []models.Problem{
		catalogIntegrationProblem("orphan-base", revisionA, "Base"),
	})
	if _, err := pool.Exec(ctx, `INSERT INTO problems (id,title,revision) VALUES ('orphan-next','Next',$1)`, revisionB); err != nil {
		t.Fatal(err)
	}
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		t.Fatal(err)
	}
	orphanRef := catalogIntegrationRef("orphan")
	orphanArtifactDigest := sha256.Sum256([]byte("orphan-next:" + revisionB))
	if _, err := tx.Exec(ctx, `
		INSERT INTO problem_catalog_publications
			(generation,previous_generation,digest_schema,candidate_digest,entry_count)
		VALUES (2,1,1,$1,1)`, orphanRef.CandidateDigest[:]); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("insert pending orphan publication: %v", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO problem_artifacts (
			problem_id,problem_revision,artifact_digest_schema,artifact_digest,
			artifact_media_type,artifact_size,source_trust
		) VALUES ('orphan-next',$1,1,$2,$3,1,'development_checkout')`,
		revisionB, orphanArtifactDigest[:], RuntimeArtifactMediaTypeV1); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("insert pending orphan artifact: %v", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO problem_catalog_entries (
			catalog_generation,problem_id,problem_revision,title,category,difficulty,type,timeout_minutes,verify_type
		) VALUES (2,'orphan-next',$1,'Next','pod','easy','fix',30,'script')`, revisionB); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("insert pending orphan entry: %v", err)
	}
	err = tx.Commit(ctx)
	requirePostgresCode(t, err, "23514")
	var publications int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM problem_catalog_publications`).Scan(&publications); err != nil {
		t.Fatal(err)
	}
	if publications != 1 {
		t.Fatalf("orphan publication left %d rows, want 1", publications)
	}

	if _, err := pool.Exec(ctx, `
		INSERT INTO users (id,github_id,username)
		VALUES ('55555555-5555-4555-8555-555555555555',10002,'null-selection-user')`); err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(ctx, `
		INSERT INTO sessions (
			id,user_id,problem_id,problem_revision,catalog_generation,current_generation,
			state,desired_state,queued_at,expires_at
		) VALUES ('66666666-6666-4666-8666-666666666666',
			'55555555-5555-4555-8555-555555555555','orphan-base',$1,NULL,1,
			'queued','active',NOW(),NOW()+INTERVAL '1 hour')`, revisionA)
	requirePostgresCode(t, err, "23514")
}

func TestPostgresCatalogLedgerSerializesPublishersFromSameHead(t *testing.T) {
	repo, pool := openProblemCatalogTestRepository(t)
	ctx := context.Background()
	const (
		revisionA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		revisionB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		revisionC = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	)
	headA := publishCatalogIntegration(t, repo, []models.Problem{
		catalogIntegrationProblem("publisher-base", revisionA, "Base"),
	})
	for _, candidate := range []struct {
		id       string
		revision string
	}{
		{id: "publisher-b", revision: revisionB},
		{id: "publisher-c", revision: revisionC},
	} {
		if _, err := pool.Exec(ctx, `INSERT INTO problems (id,title,revision) VALUES ($1,$1,$2)`, candidate.id, candidate.revision); err != nil {
			t.Fatalf("seed publisher candidate %q: %v", candidate.id, err)
		}
	}

	type publishResult struct {
		id  string
		err error
	}
	ready := make(chan struct{}, 2)
	start := make(chan struct{})
	results := make(chan publishResult, 2)
	candidates := []struct {
		id       string
		revision string
		ref      CatalogPublicationRef
	}{
		{id: "publisher-b", revision: revisionB, ref: catalogIntegrationRef("publisher-b")},
		{id: "publisher-c", revision: revisionC, ref: catalogIntegrationRef("publisher-c")},
	}
	var publishers sync.WaitGroup
	for _, candidate := range candidates {
		candidate := candidate
		publishers.Add(1)
		go func() {
			defer publishers.Done()
			tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
			if err != nil {
				results <- publishResult{id: candidate.id, err: err}
				return
			}
			defer func() { _ = tx.Rollback(ctx) }()
			var observed int64
			if err := tx.QueryRow(ctx, `SELECT catalog_generation FROM problem_catalog_head WHERE singleton=TRUE`).Scan(&observed); err != nil {
				results <- publishResult{id: candidate.id, err: err}
				return
			}
			if observed != int64(headA.Generation) {
				results <- publishResult{id: candidate.id, err: fmt.Errorf("observed head %d, want %d", observed, headA.Generation)}
				return
			}
			ready <- struct{}{}
			<-start
			if _, err := tx.Exec(ctx, `
				INSERT INTO problem_catalog_publications
					(generation,previous_generation,digest_schema,candidate_digest,entry_count)
				VALUES (2,1,1,$1,1)`, candidate.ref.CandidateDigest[:]); err != nil {
				results <- publishResult{id: candidate.id, err: err}
				return
			}
			artifactDigest := sha256.Sum256([]byte(candidate.id + ":" + candidate.revision))
			if _, err := tx.Exec(ctx, `
				INSERT INTO problem_artifacts (
					problem_id,problem_revision,artifact_digest_schema,artifact_digest,
					artifact_media_type,artifact_size,source_trust
				) VALUES ($1,$2,1,$3,$4,1,'development_checkout')`,
				candidate.id, candidate.revision, artifactDigest[:], RuntimeArtifactMediaTypeV1); err != nil {
				results <- publishResult{id: candidate.id, err: err}
				return
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO problem_catalog_entries (
					catalog_generation,problem_id,problem_revision,title,category,difficulty,type,timeout_minutes,verify_type
				) VALUES (2,$1,$2,$1,'pod','easy','fix',30,'script')`, candidate.id, candidate.revision); err != nil {
				results <- publishResult{id: candidate.id, err: err}
				return
			}
			if _, err := tx.Exec(ctx, `UPDATE problem_catalog_head SET catalog_generation=2 WHERE singleton=TRUE AND catalog_generation=1`); err != nil {
				results <- publishResult{id: candidate.id, err: err}
				return
			}
			results <- publishResult{id: candidate.id, err: tx.Commit(ctx)}
		}()
	}
	<-ready
	<-ready
	close(start)
	publishers.Wait()
	close(results)

	winner := ""
	losers := 0
	for result := range results {
		if result.err == nil {
			if winner != "" {
				t.Fatalf("multiple publishers committed: %q and %q", winner, result.id)
			}
			winner = result.id
			continue
		}
		requirePostgresCode(t, result.err, "23514", "23505")
		losers++
	}
	if winner == "" || losers != 1 {
		t.Fatalf("publisher outcome winner=%q losers=%d, want exactly one of each", winner, losers)
	}
	var headGeneration int64
	var publicationCount, generationTwoEntries int
	if err := pool.QueryRow(ctx, `SELECT
		(SELECT catalog_generation FROM problem_catalog_head WHERE singleton=TRUE),
		(SELECT COUNT(*) FROM problem_catalog_publications),
		(SELECT COUNT(*) FROM problem_catalog_entries WHERE catalog_generation=2)`).Scan(
		&headGeneration, &publicationCount, &generationTwoEntries,
	); err != nil {
		t.Fatal(err)
	}
	if headGeneration != 2 || publicationCount != 2 || generationTwoEntries != 1 {
		t.Fatalf("serialized ledger head=%d publications=%d generation-two entries=%d", headGeneration, publicationCount, generationTwoEntries)
	}
}

func TestPostgresCatalogFencePreservesReservationBeforePublication(t *testing.T) {
	repo, pool := openProblemCatalogTestRepository(t)
	ctx := context.Background()
	const (
		problemID = "reservation-before-publication"
		revisionA = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
		revisionB = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
		userID    = "71111111-1111-4111-8111-111111111111"
		sessionID = "72222222-2222-4222-8222-222222222222"
		allocID   = "73333333-3333-4333-8333-333333333333"
	)
	publishCatalogIntegration(t, repo, []models.Problem{
		catalogIntegrationProblem(problemID, revisionA, "Version A"),
	})
	if _, err := pool.Exec(ctx, `INSERT INTO users (id,github_id,username) VALUES ($1,11001,'reservation-before-user')`, userID); err != nil {
		t.Fatal(err)
	}

	reservationTx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reservationTx.Rollback(ctx) }()
	if _, err := reservationTx.Exec(ctx, catalogSharedTransactionSQL); err != nil {
		t.Fatalf("acquire shared catalog fence: %v", err)
	}
	if _, err := reservationTx.Exec(ctx, `
		INSERT INTO sessions (
			id,user_id,problem_id,problem_revision,catalog_generation,current_generation,
			state,desired_state,queued_at,expires_at
		) VALUES ($1,$2,$3,$4,1,1,'queued','active',NOW(),NOW()+INTERVAL '1 hour')`,
		sessionID, userID, problemID, revisionA); err != nil {
		t.Fatalf("insert generation-A reservation: %v", err)
	}
	if _, err := reservationTx.Exec(ctx, `
		INSERT INTO runner_allocations (
			id,session_id,generation,provider_kind,provider_id,resource_profile,
			desired_state,observed_state,expires_at
		) VALUES ($1,$2,1,'local-docker','local-docker:catalog-fence-a','standard',
			'active','unknown',NOW()+INTERVAL '1 hour')`, allocID, sessionID); err != nil {
		t.Fatalf("insert generation-A allocation: %v", err)
	}

	publisherPID := make(chan int32, 1)
	published := make(chan error, 1)
	go func() {
		conn, err := pool.Acquire(context.Background())
		if err != nil {
			published <- err
			return
		}
		var pid int32
		if err := conn.QueryRow(context.Background(), `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
			conn.Release()
			published <- err
			return
		}
		publisherPID <- pid
		if _, err := conn.Exec(context.Background(), catalogSessionLockSQL); err != nil {
			conn.Release()
			published <- err
			return
		}
		lease := &postgresCatalogPublicationLease{conn: conn}
		expected, err := lease.Head(context.Background())
		if err == nil {
			_, err = lease.Publish(context.Background(), expected, catalogIntegrationRef("reservation-after-a"), catalogIntegrationEntries(
				catalogIntegrationProblem(problemID, revisionB, "Version B"),
			))
		}
		closeErr := lease.Close(context.Background())
		if err == nil {
			err = closeErr
		}
		published <- err
	}()

	pid := <-publisherPID
	waitDeadline := time.Now().Add(5 * time.Second)
	for {
		var waitEventType, waitEvent *string
		if err := pool.QueryRow(ctx, `
			SELECT wait_event_type,wait_event FROM pg_stat_activity WHERE pid=$1`, pid).Scan(&waitEventType, &waitEvent); err != nil {
			t.Fatalf("observe publication advisory wait: %v", err)
		}
		if waitEventType != nil && *waitEventType == "Lock" && waitEvent != nil && *waitEvent == "advisory" {
			break
		}
		select {
		case err := <-published:
			t.Fatalf("publication crossed an uncommitted shared reservation fence: %v", err)
		default:
		}
		if time.Now().After(waitDeadline) {
			t.Fatalf("publisher backend %d did not block on the catalog advisory fence", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := reservationTx.Commit(ctx); err != nil {
		t.Fatalf("commit generation-A reservation: %v", err)
	}
	select {
	case err := <-published:
		if err != nil {
			t.Fatalf("publish generation B after reservation commit: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("publication did not resume after reservation commit")
	}

	var sessionGeneration int64
	var sessionRevision string
	var headGeneration int64
	if err := pool.QueryRow(ctx, `SELECT s.catalog_generation,s.problem_revision,h.catalog_generation
		FROM sessions s CROSS JOIN problem_catalog_head h
		WHERE s.id=$1 AND h.singleton=TRUE`, sessionID).Scan(&sessionGeneration, &sessionRevision, &headGeneration); err != nil {
		t.Fatal(err)
	}
	if sessionGeneration != 1 || sessionRevision != revisionA || headGeneration != 2 {
		t.Fatalf("reservation/head selection=(%d,%q) head=%d, want immutable A under head B", sessionGeneration, sessionRevision, headGeneration)
	}
}

func TestPostgresCatalogFenceRejectsStaleReservationAfterPublication(t *testing.T) {
	repo, pool := openProblemCatalogTestRepository(t)
	ctx := context.Background()
	const (
		problemID = "publication-before-reservation"
		revisionA = "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
		revisionB = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
		userID    = "81111111-1111-4111-8111-111111111111"
		sessionID = "82222222-2222-4222-8222-222222222222"
		provider  = "local-docker:catalog-fence-b"
	)
	headA := publishCatalogIntegration(t, repo, []models.Problem{
		catalogIntegrationProblem(problemID, revisionA, "Version A"),
	})
	if _, err := pool.Exec(ctx, `INSERT INTO users (id,github_id,username) VALUES ($1,12001,'publication-before-user')`, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO runner_controller_epochs (provider_id,epoch) VALUES ($1,1)`, provider); err != nil {
		t.Fatal(err)
	}
	store, err := runner.NewPostgresStore(pool, runner.ControllerFence{ProviderID: provider, Epoch: 1})
	if err != nil {
		t.Fatal(err)
	}

	lease, err := repo.AcquireCatalogPublicationLease(ctx)
	if err != nil {
		t.Fatal(err)
	}
	params := runner.ReserveSessionParams{
		SessionID: sessionID,
		UserID:    userID,
		Selection: runner.CatalogSelection{
			Generation: headA.Generation,
			Problem:    runner.ProblemRef{ID: problemID, Revision: revisionA},
		},
		Provider:        runner.ProviderLocalDocker,
		ProviderID:      provider,
		ResourceProfile: runner.DefaultResourceProfile,
		ExpiresAt:       time.Now().UTC().Add(time.Hour),
		IdempotencyKey:  "catalog-fence-stale-a",
	}
	reserved := make(chan error, 1)
	go func() {
		_, err := store.ReserveSession(context.Background(), params)
		reserved <- err
	}()

	headB, err := lease.Publish(ctx, &headA, catalogIntegrationRef("publication-before-reservation-b"), catalogIntegrationEntries(
		catalogIntegrationProblem(problemID, revisionB, "Version B"),
	))
	if err != nil {
		_ = lease.Close(ctx)
		t.Fatalf("publish generation B while reservation waits: %v", err)
	}
	if err := lease.Close(ctx); err != nil {
		t.Fatalf("release publication fence: %v", err)
	}
	select {
	case err := <-reserved:
		if !errors.Is(err, runner.ErrCatalogSelectionStale) {
			t.Fatalf("generation-A reservation error=%v, want ErrCatalogSelectionStale", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stale reservation did not resume after publication fence released")
	}

	params.Selection = runner.CatalogSelection{
		Generation: headB.Generation,
		Problem:    runner.ProblemRef{ID: problemID, Revision: revisionB},
	}
	params.IdempotencyKey = "catalog-fence-current-b"
	reservation, err := store.ReserveSession(ctx, params)
	if err != nil {
		t.Fatalf("reserve current generation B: %v", err)
	}
	if reservation.Session.Selection != params.Selection {
		t.Fatalf("durable selection=%+v want=%+v", reservation.Session.Selection, params.Selection)
	}
	var storedGeneration int64
	var storedRevision string
	if err := pool.QueryRow(ctx, `SELECT catalog_generation,problem_revision FROM sessions WHERE id=$1`, sessionID).Scan(&storedGeneration, &storedRevision); err != nil {
		t.Fatal(err)
	}
	if storedGeneration != int64(headB.Generation) || storedRevision != revisionB {
		t.Fatalf("stored selection=(%d,%q), want generation B", storedGeneration, storedRevision)
	}
}
