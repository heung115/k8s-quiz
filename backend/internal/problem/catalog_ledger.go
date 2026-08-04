package problem

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/k8s-quiz/backend/internal/runner"
	"github.com/k8s-quiz/backend/pkg/models"
)

var (
	ErrCatalogHeadConflict           = errors.New("problem catalog head changed")
	ErrCatalogRollback               = errors.New("problem catalog rollback is not allowed")
	ErrCatalogStartupMismatch        = errors.New("runtime problem catalog does not match the persisted head")
	ErrCatalogLegacyActive           = errors.New("legacy active sessions must be reconciled before catalog bootstrap")
	ErrCatalogPublicationMissing     = errors.New("problem catalog publication is missing")
	ErrCatalogArtifactBindingMissing = errors.New("problem catalog artifact binding is missing")
	ErrCatalogCommitNotApplied       = errors.New("problem catalog commit was confirmed not applied")
)

const (
	catalogSessionLockSQL       = `SELECT pg_advisory_lock(hashtext('k8s-quiz.problem-catalog'))`
	catalogSessionUnlockSQL     = `SELECT pg_advisory_unlock(hashtext('k8s-quiz.problem-catalog'))`
	catalogSharedTransactionSQL = `SELECT pg_advisory_xact_lock_shared(hashtext('k8s-quiz.problem-catalog'))`
)

// CatalogHead is the globally durable publication identity. Generation is
// monotonic in PostgreSQL; Ref identifies the exact candidate inventory.
type CatalogHead struct {
	Generation uint64
	Ref        CatalogPublicationRef
	EntryCount int
}

type catalogPublicationLease interface {
	Head(context.Context) (*CatalogHead, error)
	ReadPublication(context.Context, uint64) (CatalogHead, []CatalogEntry, error)
	ReadLegacyPublication(context.Context, uint64) (CatalogHead, []models.Problem, error)
	BindLegacyArtifacts(context.Context, CatalogHead, []CatalogEntry) error
	FindPublication(context.Context, CatalogPublicationRef) (*CatalogHead, error)
	Publish(context.Context, *CatalogHead, CatalogPublicationRef, []CatalogEntry) (CatalogHead, error)
	Close(context.Context) error
	Discard(context.Context) error
}

type CatalogLedger interface {
	AcquireCatalogPublicationLease(context.Context) (catalogPublicationLease, error)
	FindActiveCatalogProblem(context.Context, string) (CatalogHead, *models.Problem, error)
	FindCatalogArtifact(context.Context, runner.ProblemRef) (CatalogEntry, error)
}

type postgresCatalogPublicationLease struct {
	conn      *pgxpool.Conn
	closeOnce sync.Once
	closeErr  error
}

func (r *Repository) AcquireCatalogPublicationLease(ctx context.Context) (catalogPublicationLease, error) {
	conn, err := r.db.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire problem catalog publication connection: %w", err)
	}
	if _, err := conn.Exec(ctx, catalogSessionLockSQL); err != nil {
		conn.Release()
		return nil, fmt.Errorf("acquire problem catalog publication lease: %w", err)
	}
	return &postgresCatalogPublicationLease{conn: conn}, nil
}

func (l *postgresCatalogPublicationLease) Close(ctx context.Context) error {
	if l == nil {
		return nil
	}
	l.closeOnce.Do(func() {
		if l.conn == nil {
			return
		}
		var unlocked bool
		if err := l.conn.QueryRow(ctx, catalogSessionUnlockSQL).Scan(&unlocked); err != nil || !unlocked {
			if err == nil {
				err = errors.New("publication lease was not held")
			}
			l.closeErr = fmt.Errorf("release problem catalog publication lease: %w", err)
			raw := l.conn.Hijack()
			_ = raw.Close(context.Background())
			l.conn = nil
			return
		}
		l.conn.Release()
		l.conn = nil
	})
	return l.closeErr
}

// Discard closes an ambiguous-commit connection without issuing another SQL
// command on it. Closing the PostgreSQL session releases its advisory lock;
// reconciliation then reacquires that lock through a fresh pool connection.
func (l *postgresCatalogPublicationLease) Discard(ctx context.Context) error {
	if l == nil {
		return nil
	}
	l.closeOnce.Do(func() {
		if l.conn == nil {
			return
		}
		raw := l.conn.Hijack()
		l.conn = nil
		if err := raw.Close(ctx); err != nil {
			l.closeErr = fmt.Errorf("discard ambiguous problem catalog connection: %w", err)
		}
	})
	return l.closeErr
}

func (l *postgresCatalogPublicationLease) Head(ctx context.Context) (*CatalogHead, error) {
	return scanCatalogHead(l.conn.QueryRow(ctx, `
		SELECT p.generation,p.digest_schema,p.candidate_digest,p.entry_count
		FROM problem_catalog_head h
		JOIN problem_catalog_publications p ON p.generation=h.catalog_generation
		WHERE h.singleton=TRUE`))
}

func scanCatalogHead(row pgx.Row) (*CatalogHead, error) {
	var generation int64
	var digestSchema int16
	var digest []byte
	var entryCount int
	if err := row.Scan(&generation, &digestSchema, &digest, &entryCount); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	if generation <= 0 || digestSchema <= 0 || len(digest) != sha256.Size || entryCount <= 0 {
		return nil, errors.New("persisted problem catalog head is invalid")
	}
	head := &CatalogHead{Generation: uint64(generation), EntryCount: entryCount}
	head.Ref.DigestSchema = uint16(digestSchema)
	copy(head.Ref.CandidateDigest[:], digest)
	return head, nil
}

func (l *postgresCatalogPublicationLease) ReadPublication(ctx context.Context, generation uint64) (CatalogHead, []CatalogEntry, error) {
	if generation == 0 {
		return CatalogHead{}, nil, ErrCatalogPublicationMissing
	}
	head, err := scanCatalogHead(l.conn.QueryRow(ctx, `
		SELECT generation,digest_schema,candidate_digest,entry_count
		FROM problem_catalog_publications WHERE generation=$1`, int64(generation)))
	if err != nil {
		return CatalogHead{}, nil, fmt.Errorf("read problem catalog publication: %w", err)
	}
	if head == nil {
		return CatalogHead{}, nil, ErrCatalogPublicationMissing
	}
	rows, err := l.conn.Query(ctx, catalogEntrySelect+`
		WHERE e.catalog_generation=$1 ORDER BY e.problem_id`, int64(generation))
	if err != nil {
		return CatalogHead{}, nil, fmt.Errorf("read problem catalog entries: %w", err)
	}
	defer rows.Close()
	entries, err := scanCatalogEntries(rows)
	if err != nil {
		return CatalogHead{}, nil, err
	}
	if len(entries) != head.EntryCount {
		return CatalogHead{}, nil, errors.New("persisted problem catalog entry count is inconsistent")
	}
	return *head, entries, nil
}

// ReadLegacyPublication is deliberately metadata-only and may be used only by
// the explicit version-11 upgrade path. Normal startup uses ReadPublication,
// which rejects every missing artifact binding.
func (l *postgresCatalogPublicationLease) ReadLegacyPublication(ctx context.Context, generation uint64) (CatalogHead, []models.Problem, error) {
	if generation == 0 {
		return CatalogHead{}, nil, ErrCatalogPublicationMissing
	}
	head, err := scanCatalogHead(l.conn.QueryRow(ctx, `
		SELECT generation,digest_schema,candidate_digest,entry_count
		FROM problem_catalog_publications WHERE generation=$1`, int64(generation)))
	if err != nil {
		return CatalogHead{}, nil, fmt.Errorf("read legacy problem catalog publication: %w", err)
	}
	if head == nil {
		return CatalogHead{}, nil, ErrCatalogPublicationMissing
	}
	rows, err := l.conn.Query(ctx, legacyCatalogEntrySelect+`
		WHERE e.catalog_generation=$1 ORDER BY e.problem_id`, int64(generation))
	if err != nil {
		return CatalogHead{}, nil, fmt.Errorf("read legacy problem catalog entries: %w", err)
	}
	defer rows.Close()
	problems, err := scanCatalogProblems(rows)
	if err != nil {
		return CatalogHead{}, nil, err
	}
	if len(problems) != head.EntryCount {
		return CatalogHead{}, nil, errors.New("persisted legacy problem catalog entry count is inconsistent")
	}
	return *head, problems, nil
}

func (l *postgresCatalogPublicationLease) BindLegacyArtifacts(ctx context.Context, expected CatalogHead, entries []CatalogEntry) error {
	if expected.Generation == 0 || expected.Ref.DigestSchema != legacyCatalogDigestSchema || len(entries) != expected.EntryCount {
		return ErrCatalogStartupMismatch
	}
	tx, err := l.conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("begin legacy artifact binding: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	current, err := lockCatalogHead(ctx, tx)
	if err != nil {
		return err
	}
	if !sameCatalogHead(current, &expected) {
		return ErrCatalogHeadConflict
	}
	rows, err := tx.Query(ctx, legacyCatalogEntrySelect+`
		WHERE e.catalog_generation=$1 ORDER BY e.problem_id`, int64(expected.Generation))
	if err != nil {
		return fmt.Errorf("read legacy publication before artifact binding: %w", err)
	}
	stored, err := scanCatalogProblems(rows)
	rows.Close()
	if err != nil {
		return err
	}
	wanted := catalogProblems(entries)
	if !sameCatalogProjection(stored, wanted) {
		return ErrCatalogStartupMismatch
	}
	for i := range entries {
		if err := insertArtifactBinding(ctx, tx, &entries[i]); err != nil {
			return fmt.Errorf("bind legacy problem artifact %q: %w", entries[i].Problem.ID, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return classifyCatalogCommitError(err)
	}
	return nil
}

func (l *postgresCatalogPublicationLease) FindPublication(ctx context.Context, ref CatalogPublicationRef) (*CatalogHead, error) {
	return scanCatalogHead(l.conn.QueryRow(ctx, `
		SELECT generation,digest_schema,candidate_digest,entry_count
		FROM problem_catalog_publications
		WHERE digest_schema=$1 AND candidate_digest=$2`, int16(ref.DigestSchema), ref.CandidateDigest[:]))
}

func (l *postgresCatalogPublicationLease) Publish(ctx context.Context, expected *CatalogHead, ref CatalogPublicationRef, entries []CatalogEntry) (CatalogHead, error) {
	if ref.DigestSchema == 0 || len(entries) == 0 {
		return CatalogHead{}, errors.New("problem catalog publication is empty or has no identity")
	}
	tx, err := l.conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return CatalogHead{}, fmt.Errorf("begin problem catalog publication: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	current, err := lockCatalogHead(ctx, tx)
	if err != nil {
		return CatalogHead{}, err
	}
	if !sameCatalogHead(current, expected) {
		return CatalogHead{}, ErrCatalogHeadConflict
	}
	if expected == nil {
		var legacyActive int
		if err := tx.QueryRow(ctx, `
			SELECT COUNT(*) FROM sessions
			WHERE desired_state='active' AND catalog_generation IS NULL`).Scan(&legacyActive); err != nil {
			return CatalogHead{}, fmt.Errorf("check legacy active sessions before catalog bootstrap: %w", err)
		}
		if legacyActive != 0 {
			return CatalogHead{}, ErrCatalogLegacyActive
		}
	}
	if existing, err := findPublicationTx(ctx, tx, ref); err != nil {
		return CatalogHead{}, err
	} else if existing != nil {
		return CatalogHead{}, ErrCatalogRollback
	}

	nextGeneration := uint64(1)
	var previous any
	if expected != nil {
		nextGeneration = expected.Generation + 1
		previous = int64(expected.Generation)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO problem_catalog_publications
			(generation,previous_generation,digest_schema,candidate_digest,entry_count)
		VALUES ($1,$2,$3,$4,$5)`,
		int64(nextGeneration), previous, int16(ref.DigestSchema), ref.CandidateDigest[:], len(entries)); err != nil {
		return CatalogHead{}, fmt.Errorf("insert problem catalog publication: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE problems SET catalog_active=FALSE,updated_at=NOW() WHERE catalog_active=TRUE`); err != nil {
		return CatalogHead{}, fmt.Errorf("deactivate previous problem catalog projection: %w", err)
	}
	for i := range entries {
		entry := &entries[i]
		if entry.Problem.ID == "" || entry.Problem.Revision == "" {
			return CatalogHead{}, fmt.Errorf("problem catalog entry %d has incomplete identity", i)
		}
		if err := upsertCatalogProblem(ctx, tx, &entry.Problem); err != nil {
			return CatalogHead{}, fmt.Errorf("update problem catalog identity %q: %w", entry.Problem.ID, err)
		}
		if err := insertArtifactBinding(ctx, tx, entry); err != nil {
			return CatalogHead{}, fmt.Errorf("bind problem artifact %q: %w", entry.Problem.ID, err)
		}
		if err := insertCatalogEntry(ctx, tx, nextGeneration, &entry.Problem); err != nil {
			return CatalogHead{}, fmt.Errorf("insert problem catalog entry %q: %w", entry.Problem.ID, err)
		}
	}
	if expected == nil {
		_, err = tx.Exec(ctx, `INSERT INTO problem_catalog_head (singleton,catalog_generation) VALUES (TRUE,$1)`, int64(nextGeneration))
	} else {
		var tag pgconnCommandTag
		tag, err = execCatalogHeadCAS(ctx, tx, expected.Generation, nextGeneration)
		if err == nil && tag.RowsAffected() != 1 {
			err = ErrCatalogHeadConflict
		}
	}
	if err != nil {
		return CatalogHead{}, fmt.Errorf("advance problem catalog head: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return CatalogHead{}, classifyCatalogCommitError(err)
	}
	return CatalogHead{Generation: nextGeneration, Ref: ref, EntryCount: len(entries)}, nil
}

type pgconnCommandTag interface{ RowsAffected() int64 }

func execCatalogHeadCAS(ctx context.Context, tx pgx.Tx, expected, next uint64) (pgconnCommandTag, error) {
	return tx.Exec(ctx, `
		UPDATE problem_catalog_head SET catalog_generation=$1
		WHERE singleton=TRUE AND catalog_generation=$2`, int64(next), int64(expected))
}

func lockCatalogHead(ctx context.Context, tx pgx.Tx) (*CatalogHead, error) {
	return scanCatalogHead(tx.QueryRow(ctx, `
		SELECT p.generation,p.digest_schema,p.candidate_digest,p.entry_count
		FROM problem_catalog_head h
		JOIN problem_catalog_publications p ON p.generation=h.catalog_generation
		WHERE h.singleton=TRUE FOR UPDATE OF h`))
}

func findPublicationTx(ctx context.Context, tx pgx.Tx, ref CatalogPublicationRef) (*CatalogHead, error) {
	return scanCatalogHead(tx.QueryRow(ctx, `
		SELECT generation,digest_schema,candidate_digest,entry_count
		FROM problem_catalog_publications
		WHERE digest_schema=$1 AND candidate_digest=$2`, int16(ref.DigestSchema), ref.CandidateDigest[:]))
}

func sameCatalogHead(left, right *CatalogHead) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func insertCatalogEntry(ctx context.Context, tx pgx.Tx, generation uint64, p *models.Problem) error {
	choices, err := json.Marshal(p.Choices)
	if err != nil {
		return fmt.Errorf("encode choices: %w", err)
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO problem_catalog_entries (
			catalog_generation,problem_id,problem_revision,title,description,category,
			difficulty,type,timeout_minutes,verify_type,base_image,image,choices,
			correct_choice,hint,grading_prompt
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)`,
		int64(generation), p.ID, p.Revision, p.Title, p.Description, p.Category,
		p.Difficulty, p.Type, p.TimeoutMinutes, p.VerifyType, p.BaseImage, p.Image,
		choices, p.CorrectChoice, p.Hint, p.GradingPrompt)
	return err
}

func insertArtifactBinding(ctx context.Context, tx pgx.Tx, entry *CatalogEntry) error {
	if entry == nil || entry.Problem.ID == "" || entry.Problem.Revision == "" {
		return errors.New("problem artifact binding has incomplete identity")
	}
	if entry.SourceTrust != BundleSourceDevelopmentCheckout {
		return fmt.Errorf("unsupported problem artifact source trust %q", entry.SourceTrust)
	}
	if err := validateArtifactRef(entry.Artifact); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO problem_artifacts (
			problem_id,problem_revision,artifact_digest_schema,artifact_digest,
			artifact_media_type,artifact_size,source_trust
		) VALUES ($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT (problem_id,problem_revision) DO NOTHING`,
		entry.Problem.ID, entry.Problem.Revision, int16(entry.Artifact.DigestSchema),
		entry.Artifact.Digest[:], entry.Artifact.MediaType, entry.Artifact.Size,
		string(entry.SourceTrust)); err != nil {
		return err
	}
	var digestSchema int16
	var digest []byte
	var mediaType string
	var size int64
	var sourceTrust string
	if err := tx.QueryRow(ctx, `
		SELECT artifact_digest_schema,artifact_digest,artifact_media_type,artifact_size,source_trust
		FROM problem_artifacts WHERE problem_id=$1 AND problem_revision=$2`,
		entry.Problem.ID, entry.Problem.Revision).Scan(
		&digestSchema, &digest, &mediaType, &size, &sourceTrust,
	); err != nil {
		return err
	}
	if digestSchema != int16(entry.Artifact.DigestSchema) || len(digest) != sha256.Size ||
		!equalDigest(digest, entry.Artifact.Digest) || mediaType != entry.Artifact.MediaType ||
		size != entry.Artifact.Size || sourceTrust != string(entry.SourceTrust) {
		return fmt.Errorf("problem revision %s/%s is already bound to a different artifact", entry.Problem.ID, entry.Problem.Revision)
	}
	return nil
}

func equalDigest(raw []byte, expected [sha256.Size]byte) bool {
	if len(raw) != len(expected) {
		return false
	}
	for i := range expected {
		if raw[i] != expected[i] {
			return false
		}
	}
	return true
}

const catalogEntrySelect = `SELECT
	e.problem_id,e.problem_revision,TRUE,e.title,e.description,e.category,e.difficulty,
	e.type,e.timeout_minutes,e.verify_type,e.base_image,e.image,e.choices,
	e.correct_choice,e.hint,e.grading_prompt,e.created_at,e.created_at,
	COALESCE(a.artifact_digest_schema,0),COALESCE(a.artifact_digest,''::bytea),
	COALESCE(a.artifact_media_type,''),COALESCE(a.artifact_size,0),COALESCE(a.source_trust,'')
	FROM problem_catalog_entries e
	LEFT JOIN problem_artifacts a
	  ON a.problem_id=e.problem_id AND a.problem_revision=e.problem_revision `

const legacyCatalogEntrySelect = `SELECT
	e.problem_id,e.problem_revision,TRUE,e.title,e.description,e.category,e.difficulty,
	e.type,e.timeout_minutes,e.verify_type,e.base_image,e.image,e.choices,
	e.correct_choice,e.hint,e.grading_prompt,e.created_at,e.created_at
	FROM problem_catalog_entries e `

type catalogRows interface {
	Next() bool
	Scan(...any) error
	Err() error
}

func scanCatalogProblems(rows catalogRows) ([]models.Problem, error) {
	var problems []models.Problem
	for rows.Next() {
		var p models.Problem
		var choices []byte
		if err := rows.Scan(
			&p.ID, &p.Revision, &p.CatalogActive, &p.Title, &p.Description,
			&p.Category, &p.Difficulty, &p.Type, &p.TimeoutMinutes, &p.VerifyType,
			&p.BaseImage, &p.Image, &choices, &p.CorrectChoice, &p.Hint,
			&p.GradingPrompt, &p.CreatedAt, &p.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan problem catalog entry: %w", err)
		}
		if len(choices) != 0 {
			if err := json.Unmarshal(choices, &p.Choices); err != nil {
				return nil, fmt.Errorf("decode problem %q choices: %w", p.ID, err)
			}
		}
		problems = append(problems, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate problem catalog entries: %w", err)
	}
	return problems, nil
}

func scanCatalogEntries(rows catalogRows) ([]CatalogEntry, error) {
	var entries []CatalogEntry
	for rows.Next() {
		var entry CatalogEntry
		var choices []byte
		var digestSchema int16
		var digest []byte
		var sourceTrust string
		if err := rows.Scan(
			&entry.Problem.ID, &entry.Problem.Revision, &entry.Problem.CatalogActive,
			&entry.Problem.Title, &entry.Problem.Description, &entry.Problem.Category,
			&entry.Problem.Difficulty, &entry.Problem.Type, &entry.Problem.TimeoutMinutes,
			&entry.Problem.VerifyType, &entry.Problem.BaseImage, &entry.Problem.Image,
			&choices, &entry.Problem.CorrectChoice, &entry.Problem.Hint,
			&entry.Problem.GradingPrompt, &entry.Problem.CreatedAt, &entry.Problem.UpdatedAt,
			&digestSchema, &digest, &entry.Artifact.MediaType, &entry.Artifact.Size, &sourceTrust,
		); err != nil {
			return nil, fmt.Errorf("scan artifact-bound problem catalog entry: %w", err)
		}
		if digestSchema <= 0 || len(digest) != sha256.Size || entry.Artifact.MediaType == "" ||
			entry.Artifact.Size <= 0 || sourceTrust == "" {
			return nil, fmt.Errorf("%w: %s/%s", ErrCatalogArtifactBindingMissing, entry.Problem.ID, entry.Problem.Revision)
		}
		entry.Artifact.DigestSchema = uint16(digestSchema)
		copy(entry.Artifact.Digest[:], digest)
		entry.SourceTrust = BundleSourceTrust(sourceTrust)
		if err := validateArtifactRef(entry.Artifact); err != nil {
			return nil, fmt.Errorf("invalid artifact binding for %s/%s: %w", entry.Problem.ID, entry.Problem.Revision, err)
		}
		if entry.SourceTrust != BundleSourceDevelopmentCheckout {
			return nil, fmt.Errorf("invalid source trust for %s/%s", entry.Problem.ID, entry.Problem.Revision)
		}
		if len(choices) != 0 {
			if err := json.Unmarshal(choices, &entry.Problem.Choices); err != nil {
				return nil, fmt.Errorf("decode problem %q choices: %w", entry.Problem.ID, err)
			}
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate artifact-bound problem catalog entries: %w", err)
	}
	return entries, nil
}

func catalogProblems(entries []CatalogEntry) []models.Problem {
	problems := make([]models.Problem, len(entries))
	for i := range entries {
		problems[i] = cloneProblem(entries[i].Problem)
	}
	return problems
}

func (r *Repository) FindActiveCatalogProblem(ctx context.Context, id string) (CatalogHead, *models.Problem, error) {
	var head CatalogHead
	var generation int64
	var digestSchema int16
	var digest []byte
	var choices []byte
	p := &models.Problem{}
	err := r.db.QueryRow(ctx, `
		SELECT pub.generation,pub.digest_schema,pub.candidate_digest,pub.entry_count,
			e.problem_id,e.problem_revision,TRUE,e.title,e.description,e.category,e.difficulty,
			e.type,e.timeout_minutes,e.verify_type,e.base_image,e.image,e.choices,
			e.correct_choice,e.hint,e.grading_prompt,e.created_at,e.created_at
		FROM problem_catalog_head h
		JOIN problem_catalog_publications pub ON pub.generation=h.catalog_generation
		JOIN problem_catalog_entries e ON e.catalog_generation=h.catalog_generation
		JOIN problem_artifacts a
		  ON a.problem_id=e.problem_id AND a.problem_revision=e.problem_revision
		WHERE h.singleton=TRUE AND e.problem_id=$1`, id).Scan(
		&generation, &digestSchema, &digest, &head.EntryCount,
		&p.ID, &p.Revision, &p.CatalogActive, &p.Title, &p.Description, &p.Category,
		&p.Difficulty, &p.Type, &p.TimeoutMinutes, &p.VerifyType, &p.BaseImage,
		&p.Image, &choices, &p.CorrectChoice, &p.Hint, &p.GradingPrompt,
		&p.CreatedAt, &p.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return CatalogHead{}, nil, runner.ErrInvalidRevision
		}
		return CatalogHead{}, nil, err
	}
	if generation <= 0 || digestSchema <= 0 || len(digest) != sha256.Size {
		return CatalogHead{}, nil, errors.New("active problem catalog head is invalid")
	}
	head.Generation = uint64(generation)
	head.Ref.DigestSchema = uint16(digestSchema)
	copy(head.Ref.CandidateDigest[:], digest)
	if len(choices) != 0 {
		if err := json.Unmarshal(choices, &p.Choices); err != nil {
			return CatalogHead{}, nil, fmt.Errorf("decode active problem %q choices: %w", id, err)
		}
	}
	return head, p, nil
}

func (r *Repository) FindCatalogArtifact(ctx context.Context, ref runner.ProblemRef) (CatalogEntry, error) {
	if !validProblemID(ref.ID) || ref.Revision == "" {
		return CatalogEntry{}, runner.ErrInvalidRevision
	}
	rows, err := r.db.Query(ctx, catalogEntrySelect+`
		WHERE e.problem_id=$1 AND e.problem_revision=$2
		ORDER BY e.catalog_generation DESC LIMIT 1`, ref.ID, ref.Revision)
	if err != nil {
		return CatalogEntry{}, err
	}
	defer rows.Close()
	entries, err := scanCatalogEntries(rows)
	if err != nil {
		return CatalogEntry{}, err
	}
	if len(entries) != 1 {
		return CatalogEntry{}, runner.ErrInvalidRevision
	}
	return entries[0], nil
}
