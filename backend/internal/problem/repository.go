package problem

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/k8s-quiz/backend/pkg/models"
)

type Repository struct {
	db *pgxpool.Pool
}

func NewRepository(db *pgxpool.Pool) *Repository {
	return &Repository{db: db}
}

func (r *Repository) List(ctx context.Context, category, difficulty, ptype string) ([]models.Problem, error) {
	query := `SELECT id, title, description, category, difficulty, type, timeout_minutes, verify_type, base_image, image, choices, correct_choice, hint, grading_prompt, created_at, updated_at FROM problems WHERE 1=1`
	args := []interface{}{}
	argIdx := 1

	if category != "" {
		query += fmt.Sprintf(" AND category = $%d", argIdx)
		args = append(args, category)
		argIdx++
	}
	if difficulty != "" {
		query += fmt.Sprintf(" AND difficulty = $%d", argIdx)
		args = append(args, difficulty)
		argIdx++
	}
	if ptype != "" {
		query += fmt.Sprintf(" AND type = $%d", argIdx)
		args = append(args, ptype)
		argIdx++
	}
	query += " ORDER BY created_at DESC"

	rows, err := r.db.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var problems []models.Problem
	for rows.Next() {
		var p models.Problem
		var choicesJSON []byte
		if err := rows.Scan(&p.ID, &p.Title, &p.Description, &p.Category, &p.Difficulty, &p.Type, &p.TimeoutMinutes, &p.VerifyType, &p.BaseImage, &p.Image, &choicesJSON, &p.CorrectChoice, &p.Hint, &p.GradingPrompt, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		if len(choicesJSON) > 0 {
			json.Unmarshal(choicesJSON, &p.Choices)
		}
		problems = append(problems, p)
	}
	return problems, nil
}

func (r *Repository) FindByID(ctx context.Context, id string) (*models.Problem, error) {
	p := &models.Problem{}
	var choicesJSON []byte
	err := r.db.QueryRow(ctx,
		`SELECT id, title, description, category, difficulty, type, timeout_minutes, verify_type, base_image, image, choices, correct_choice, hint, grading_prompt, created_at, updated_at FROM problems WHERE id = $1`, id,
	).Scan(&p.ID, &p.Title, &p.Description, &p.Category, &p.Difficulty, &p.Type, &p.TimeoutMinutes, &p.VerifyType, &p.BaseImage, &p.Image, &choicesJSON, &p.CorrectChoice, &p.Hint, &p.GradingPrompt, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return nil, err
	}
	if len(choicesJSON) > 0 {
		json.Unmarshal(choicesJSON, &p.Choices)
	}
	return p, nil
}

func (r *Repository) Upsert(ctx context.Context, p *models.Problem) error {
	choicesJSON, _ := json.Marshal(p.Choices)
	now := time.Now()
	_, err := r.db.Exec(ctx,
		`INSERT INTO problems (id, title, description, category, difficulty, type, timeout_minutes, verify_type, base_image, image, choices, correct_choice, hint, grading_prompt, created_at, updated_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)
		 ON CONFLICT (id) DO UPDATE SET title=$2, description=$3, category=$4, difficulty=$5, type=$6, timeout_minutes=$7, verify_type=$8, base_image=$9, image=$10, choices=$11, correct_choice=$12, hint=$13, grading_prompt=$14, updated_at=$16`,
		p.ID, p.Title, p.Description, p.Category, p.Difficulty, p.Type, p.TimeoutMinutes, p.VerifyType, p.BaseImage, p.Image, choicesJSON, p.CorrectChoice, p.Hint, p.GradingPrompt, now, now,
	)
	return err
}

func (r *Repository) Delete(ctx context.Context, id string) error {
	_, err := r.db.Exec(ctx, `DELETE FROM problems WHERE id = $1`, id)
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
