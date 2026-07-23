package user

import (
	"context"
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

func (r *Repository) FindByGithubID(ctx context.Context, githubID int64) (*models.User, error) {
	u := &models.User{}
	err := r.db.QueryRow(ctx,
		`SELECT id, github_id, username, COALESCE(email, ''), COALESCE(avatar_url, ''), role, created_at, updated_at FROM users WHERE github_id = $1`,
		githubID,
	).Scan(&u.ID, &u.GithubID, &u.Username, &u.Email, &u.AvatarURL, &u.Role, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return u, nil
}

func (r *Repository) FindByID(ctx context.Context, id string) (*models.User, error) {
	u := &models.User{}
	err := r.db.QueryRow(ctx,
		`SELECT id, github_id, username, COALESCE(email, ''), COALESCE(avatar_url, ''), role, created_at, updated_at FROM users WHERE id = $1`,
		id,
	).Scan(&u.ID, &u.GithubID, &u.Username, &u.Email, &u.AvatarURL, &u.Role, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return u, nil
}

func (r *Repository) Create(ctx context.Context, u *models.User) error {
	now := time.Now()
	u.CreatedAt = now
	u.UpdatedAt = now
	if u.Role == "" {
		u.Role = models.RoleUser
	}
	return r.db.QueryRow(ctx,
		`INSERT INTO users (github_id, username, email, avatar_url, role, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id`,
		u.GithubID, u.Username, u.Email, u.AvatarURL, u.Role, u.CreatedAt, u.UpdatedAt,
	).Scan(&u.ID)
}

func (r *Repository) Update(ctx context.Context, u *models.User) error {
	u.UpdatedAt = time.Now()
	_, err := r.db.Exec(ctx,
		`UPDATE users SET username=$1, email=$2, avatar_url=$3, role=$4, updated_at=$5 WHERE id=$6`,
		u.Username, u.Email, u.AvatarURL, u.Role, u.UpdatedAt, u.ID,
	)
	return err
}

func (r *Repository) List(ctx context.Context) ([]models.User, error) {
	rows, err := r.db.Query(ctx,
		`SELECT id, github_id, username, COALESCE(email, ''), COALESCE(avatar_url, ''), role, created_at, updated_at FROM users ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var users []models.User
	for rows.Next() {
		var u models.User
		if err := rows.Scan(&u.ID, &u.GithubID, &u.Username, &u.Email, &u.AvatarURL, &u.Role, &u.CreatedAt, &u.UpdatedAt); err != nil {
			return nil, err
		}
		users = append(users, u)
	}
	return users, nil
}

func (r *Repository) UpdateRole(ctx context.Context, id string, role models.Role) error {
	_, err := r.db.Exec(ctx, `UPDATE users SET role=$1, updated_at=NOW() WHERE id=$2`, role, id)
	return err
}

func (r *Repository) Leaderboard(ctx context.Context, limit int) ([]models.LeaderboardEntry, error) {
	rows, err := r.db.Query(ctx, `
		SELECT u.id, u.username, u.avatar_url,
		       COUNT(DISTINCT CASE WHEN a.status = 'success' THEN a.problem_id END) AS solved_count,
		       COUNT(a.id) AS total_attempts
		FROM users u
		LEFT JOIN attempts a ON u.id = a.user_id
		GROUP BY u.id, u.username, u.avatar_url
		HAVING COUNT(DISTINCT CASE WHEN a.status = 'success' THEN a.problem_id END) > 0
		ORDER BY solved_count DESC, total_attempts ASC
		LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	entries := []models.LeaderboardEntry{}
	rank := 1
	for rows.Next() {
		var e models.LeaderboardEntry
		if err := rows.Scan(&e.UserID, &e.Username, &e.AvatarURL, &e.SolvedCount, &e.TotalAttempts); err != nil {
			return nil, err
		}
		e.Rank = rank
		rank++
		entries = append(entries, e)
	}
	return entries, nil
}
