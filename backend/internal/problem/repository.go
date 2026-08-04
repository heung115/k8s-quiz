package problem

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/k8s-quiz/backend/pkg/models"
)

var (
	ErrCatalogManagedProblem = errors.New("problem is managed by the runtime catalog")
	ErrCatalogCommitUnknown  = errors.New("problem catalog commit outcome is unknown")
)

const catalogAdvisoryLockSQL = `SELECT pg_advisory_xact_lock(hashtext('k8s-quiz.problem-catalog'))`

type Repository struct {
	db *pgxpool.Pool
}

func NewRepository(db *pgxpool.Pool) *Repository {
	return &Repository{db: db}
}

func (r *Repository) List(ctx context.Context, category, difficulty, ptype string) ([]models.Problem, error) {
	query := legacyCatalogEntrySelect + `
		JOIN problem_catalog_head h ON h.catalog_generation=e.catalog_generation
		JOIN problem_artifacts a
		  ON a.problem_id=e.problem_id AND a.problem_revision=e.problem_revision
		WHERE h.singleton=TRUE`
	args := []any{}
	argIdx := 1
	if category != "" {
		query += fmt.Sprintf(" AND e.category = $%d", argIdx)
		args = append(args, category)
		argIdx++
	}
	if difficulty != "" {
		query += fmt.Sprintf(" AND e.difficulty = $%d", argIdx)
		args = append(args, difficulty)
		argIdx++
	}
	if ptype != "" {
		query += fmt.Sprintf(" AND e.type = $%d", argIdx)
		args = append(args, ptype)
	}
	query += " ORDER BY e.created_at DESC"

	rows, err := r.db.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanCatalogProblems(rows)
}

// ListAll includes inactive identity rows and metadata-only drafts for the
// admin API. An inactive identity row is not revision history: catalog sync
// overwrites the row's latest projection. Public List reads the immutable
// catalog head and never trusts this mutable compatibility projection.
func (r *Repository) ListAll(ctx context.Context) ([]models.Problem, error) {
	return r.listIdentityRows(ctx)
}

func (r *Repository) listIdentityRows(ctx context.Context) ([]models.Problem, error) {
	query := `SELECT id, revision, catalog_active, title, description, category, difficulty, type, timeout_minutes, verify_type, COALESCE(base_image,''), COALESCE(image,''), choices, COALESCE(correct_choice,''), COALESCE(hint,''), COALESCE(grading_prompt,''), created_at, updated_at FROM problems WHERE 1=1`
	query += " ORDER BY created_at DESC"

	rows, err := r.db.Query(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var problems []models.Problem
	for rows.Next() {
		var p models.Problem
		var choicesJSON []byte
		if err := rows.Scan(&p.ID, &p.Revision, &p.CatalogActive, &p.Title, &p.Description, &p.Category, &p.Difficulty, &p.Type, &p.TimeoutMinutes, &p.VerifyType, &p.BaseImage, &p.Image, &choicesJSON, &p.CorrectChoice, &p.Hint, &p.GradingPrompt, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		if len(choicesJSON) > 0 {
			if err := json.Unmarshal(choicesJSON, &p.Choices); err != nil {
				return nil, fmt.Errorf("decode problem %q choices: %w", p.ID, err)
			}
		}
		problems = append(problems, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return problems, nil
}

func (r *Repository) FindByID(ctx context.Context, id string) (*models.Problem, error) {
	rows, err := r.db.Query(ctx, legacyCatalogEntrySelect+`
		JOIN problem_catalog_head h ON h.catalog_generation=e.catalog_generation
		JOIN problem_artifacts a
		  ON a.problem_id=e.problem_id AND a.problem_revision=e.problem_revision
		WHERE h.singleton=TRUE AND e.problem_id=$1`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	problems, err := scanCatalogProblems(rows)
	if err != nil {
		return nil, err
	}
	if len(problems) != 1 {
		return nil, pgx.ErrNoRows
	}
	return &problems[0], nil
}

// FindAnyByID includes inactive identity rows for admin inspection. New session
// admission uses CatalogCoordinator.AcquireActiveProblem; public reads use the
// immutable head-backed FindByID above.
func (r *Repository) FindAnyByID(ctx context.Context, id string) (*models.Problem, error) {
	return r.findIdentityRowByID(ctx, id)
}

func (r *Repository) findIdentityRowByID(ctx context.Context, id string) (*models.Problem, error) {
	p := &models.Problem{}
	var choicesJSON []byte
	query := `SELECT id, revision, catalog_active, title, description, category, difficulty, type, timeout_minutes, verify_type, COALESCE(base_image,''), COALESCE(image,''), choices, COALESCE(correct_choice,''), COALESCE(hint,''), COALESCE(grading_prompt,''), created_at, updated_at FROM problems WHERE id = $1`
	err := r.db.QueryRow(ctx, query, id).Scan(&p.ID, &p.Revision, &p.CatalogActive, &p.Title, &p.Description, &p.Category, &p.Difficulty, &p.Type, &p.TimeoutMinutes, &p.VerifyType, &p.BaseImage, &p.Image, &choicesJSON, &p.CorrectChoice, &p.Hint, &p.GradingPrompt, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return nil, err
	}
	if len(choicesJSON) > 0 {
		if err := json.Unmarshal(choicesJSON, &p.Choices); err != nil {
			return nil, fmt.Errorf("decode problem %q choices: %w", p.ID, err)
		}
	}
	return p, nil
}

// Upsert creates or edits only a metadata draft. Once an ID appears in the
// append-only catalog ledger it remains managed even if its mutable identity
// projection is later retired or corrupted.
func (r *Repository) Upsert(ctx context.Context, p *models.Problem) error {
	choicesJSON, err := json.Marshal(p.Choices)
	if err != nil {
		return fmt.Errorf("encode problem choices: %w", err)
	}
	now := time.Now()
	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, catalogAdvisoryLockSQL); err != nil {
		return fmt.Errorf("lock problem catalog: %w", err)
	}
	var managed bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM problem_catalog_entries WHERE problem_id=$1)`, p.ID,
	).Scan(&managed); err != nil {
		return fmt.Errorf("check problem catalog ownership: %w", err)
	}
	if managed {
		return ErrCatalogManagedProblem
	}
	var id string
	err = tx.QueryRow(ctx,
		`INSERT INTO problems (id, revision, catalog_active, title, description, category, difficulty, type, timeout_minutes, verify_type, base_image, image, choices, correct_choice, hint, grading_prompt, created_at, updated_at)
		 VALUES ($1,'',FALSE,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$15)
		 ON CONFLICT (id) DO UPDATE SET
		   revision='', catalog_active=FALSE,
		   title=EXCLUDED.title, description=EXCLUDED.description, category=EXCLUDED.category,
		   difficulty=EXCLUDED.difficulty, type=EXCLUDED.type, timeout_minutes=EXCLUDED.timeout_minutes,
		   verify_type=EXCLUDED.verify_type, base_image=EXCLUDED.base_image, image=EXCLUDED.image,
		   choices=EXCLUDED.choices, correct_choice=EXCLUDED.correct_choice,
		   hint=EXCLUDED.hint, grading_prompt=EXCLUDED.grading_prompt, updated_at=EXCLUDED.updated_at
		 RETURNING id`,
		p.ID, p.Title, p.Description, p.Category, p.Difficulty, p.Type, p.TimeoutMinutes,
		p.VerifyType, p.BaseImage, p.Image, choicesJSON, p.CorrectChoice, p.Hint, p.GradingPrompt, now,
	).Scan(&id)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *Repository) Delete(ctx context.Context, id string) error {
	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, catalogAdvisoryLockSQL); err != nil {
		return fmt.Errorf("lock problem catalog: %w", err)
	}
	var rowID string
	if err := tx.QueryRow(ctx, `SELECT id FROM problems WHERE id=$1 FOR UPDATE`, id).Scan(&rowID); err != nil {
		return err
	}
	var managed bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM problem_catalog_entries WHERE problem_id=$1)`, id,
	).Scan(&managed); err != nil {
		return fmt.Errorf("check problem catalog ownership: %w", err)
	}
	if managed {
		return ErrCatalogManagedProblem
	}
	if _, err := tx.Exec(ctx, `DELETE FROM problems WHERE id=$1`, id); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ReplaceActiveCatalog updates the complete public catalog in one transaction.
// Missing problem IDs are retained as inactive identity anchors so attempts and
// durable session foreign keys remain intact. This legacy projection writer is
// retained for draft/repository compatibility tests; production runtime
// publication goes through CatalogCoordinator and the append-only catalog
// ledger. Runtime artifact bytes still require a persistent CAS.
func (r *Repository) ReplaceActiveCatalog(ctx context.Context, problems []models.Problem) error {
	// The transaction-scoped advisory lock serializes every catalog writer.
	// ReadCommitted avoids unnecessary serialization failures from a snapshot
	// taken before a contending writer obtains that lock.
	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("begin problem catalog replacement: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, catalogAdvisoryLockSQL); err != nil {
		return fmt.Errorf("lock problem catalog: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE problems SET catalog_active=FALSE, updated_at=NOW() WHERE catalog_active=TRUE`); err != nil {
		return fmt.Errorf("deactivate previous problem catalog: %w", err)
	}
	for i := range problems {
		if problems[i].Revision == "" {
			return fmt.Errorf("problem %q has no runtime revision", problems[i].ID)
		}
		if err := upsertCatalogProblem(ctx, tx, &problems[i]); err != nil {
			return fmt.Errorf("replace catalog problem %q: %w", problems[i].ID, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return classifyCatalogCommitError(err)
	}
	return nil
}

// classifyCatalogCommitError distinguishes a confirmed non-commit from a
// lost acknowledgement. Only the latter can leave the database projection
// ahead of the in-memory catalog and therefore requires admission poisoning.
func classifyCatalogCommitError(err error) error {
	if errors.Is(err, pgx.ErrTxCommitRollback) {
		return fmt.Errorf("commit problem catalog replacement rolled back: %w", err)
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && (pgErr.Code == "40001" || pgErr.Code == "40P01") {
		return fmt.Errorf("commit problem catalog replacement rolled back: %w", err)
	}
	// SafeToRetry is true only when pgconn guarantees that no bytes were sent
	// to PostgreSQL, so COMMIT could not have taken effect.
	if pgconn.SafeToRetry(err) {
		return fmt.Errorf("commit problem catalog replacement was not sent: %w", err)
	}
	return fmt.Errorf("%w: %v", ErrCatalogCommitUnknown, err)
}

func upsertCatalogProblem(ctx context.Context, tx pgx.Tx, p *models.Problem) error {
	choicesJSON, err := json.Marshal(p.Choices)
	if err != nil {
		return fmt.Errorf("encode choices: %w", err)
	}
	now := time.Now()
	_, err = tx.Exec(ctx,
		`INSERT INTO problems (id, revision, catalog_active, title, description, category, difficulty, type, timeout_minutes, verify_type, base_image, image, choices, correct_choice, hint, grading_prompt, created_at, updated_at)
		 VALUES ($1,$2,TRUE,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$16)
		 ON CONFLICT (id) DO UPDATE SET
		   revision=EXCLUDED.revision, catalog_active=TRUE, title=EXCLUDED.title,
		   description=EXCLUDED.description, category=EXCLUDED.category,
		   difficulty=EXCLUDED.difficulty, type=EXCLUDED.type,
		   timeout_minutes=EXCLUDED.timeout_minutes, verify_type=EXCLUDED.verify_type,
		   base_image=EXCLUDED.base_image, image=EXCLUDED.image, choices=EXCLUDED.choices,
		   correct_choice=EXCLUDED.correct_choice, hint=EXCLUDED.hint,
		   grading_prompt=EXCLUDED.grading_prompt, updated_at=EXCLUDED.updated_at`,
		p.ID, p.Revision, p.Title, p.Description, p.Category, p.Difficulty, p.Type,
		p.TimeoutMinutes, p.VerifyType, p.BaseImage, p.Image, choicesJSON,
		p.CorrectChoice, p.Hint, p.GradingPrompt, now,
	)
	return err
}

func (r *Repository) CreateAttempt(ctx context.Context, a *models.Attempt) error {
	return r.db.QueryRow(ctx,
		`INSERT INTO attempts (user_id, problem_id, status, container_id, started_at) VALUES ($1,$2,$3,$4,$5) RETURNING id`,
		a.UserID, a.ProblemID, a.Status, a.ContainerID, a.StartedAt,
	).Scan(&a.ID)
}

func (r *Repository) UpdateAttempt(ctx context.Context, a *models.Attempt) error {
	_, err := r.db.Exec(ctx,
		`UPDATE attempts SET status=$1, finished_at=$2, duration_seconds=$3, verify_log=$4 WHERE id=$5`,
		a.Status, a.FinishedAt, a.DurationSeconds, a.VerifyLog, a.ID,
	)
	return err
}

func (r *Repository) GetAttempt(ctx context.Context, id string) (*models.Attempt, error) {
	a := &models.Attempt{}
	err := r.db.QueryRow(ctx,
		`SELECT id, user_id, problem_id, status, container_id, started_at, finished_at, duration_seconds, verify_log, created_at FROM attempts WHERE id=$1`, id,
	).Scan(&a.ID, &a.UserID, &a.ProblemID, &a.Status, &a.ContainerID, &a.StartedAt, &a.FinishedAt, &a.DurationSeconds, &a.VerifyLog, &a.CreatedAt)
	if err != nil {
		return nil, err
	}
	return a, nil
}

func (r *Repository) GetActiveAttempt(ctx context.Context, userID string) (*models.Attempt, error) {
	a := &models.Attempt{}
	err := r.db.QueryRow(ctx,
		`SELECT id, user_id, problem_id, status, container_id, started_at, finished_at, duration_seconds, verify_log, created_at FROM attempts WHERE user_id=$1 AND status='in_progress' ORDER BY started_at DESC LIMIT 1`, userID,
	).Scan(&a.ID, &a.UserID, &a.ProblemID, &a.Status, &a.ContainerID, &a.StartedAt, &a.FinishedAt, &a.DurationSeconds, &a.VerifyLog, &a.CreatedAt)
	if err != nil {
		return nil, err
	}
	return a, nil
}

func (r *Repository) ListAttemptsByUser(ctx context.Context, userID string) ([]models.Attempt, error) {
	rows, err := r.db.Query(ctx,
		`SELECT id, user_id, problem_id, status, container_id, started_at, finished_at, duration_seconds, verify_log, created_at FROM attempts WHERE user_id=$1 ORDER BY started_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanAttempts(rows)
}

func (r *Repository) ListAllAttempts(ctx context.Context) ([]models.Attempt, error) {
	rows, err := r.db.Query(ctx,
		`SELECT id, user_id, problem_id, status, container_id, started_at, finished_at, duration_seconds, verify_log, created_at FROM attempts ORDER BY started_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanAttempts(rows)
}

type pgxRows interface {
	Next() bool
	Scan(dest ...any) error
}

func scanAttempts(rows pgxRows) ([]models.Attempt, error) {
	var attempts []models.Attempt
	for rows.Next() {
		var a models.Attempt
		if err := rows.Scan(&a.ID, &a.UserID, &a.ProblemID, &a.Status, &a.ContainerID, &a.StartedAt, &a.FinishedAt, &a.DurationSeconds, &a.VerifyLog, &a.CreatedAt); err != nil {
			return nil, err
		}
		attempts = append(attempts, a)
	}
	return attempts, nil
}
