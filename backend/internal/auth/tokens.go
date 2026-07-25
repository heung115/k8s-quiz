package auth

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// RefreshTokenRecord is a stored refresh token row (token itself is only ever
// stored as a sha256 hash; see hashToken).
type RefreshTokenRecord struct {
	UserID    string
	FamilyID  string
	Used      bool
	ExpiresAt time.Time
}

// RefreshTokenStore abstracts refresh-token persistence so rotation/reuse
// logic (AUTH-5) is unit-testable without a database.
type RefreshTokenStore interface {
	// CreateRefreshToken stores a hashed token. An empty familyID starts a new
	// family; the (new or given) family id is returned.
	CreateRefreshToken(ctx context.Context, userID, tokenHash string, expiresAt time.Time, familyID string) (string, error)
	FindRefreshToken(ctx context.Context, tokenHash string) (*RefreshTokenRecord, error)
	MarkRefreshTokenUsed(ctx context.Context, tokenHash string) error
	DeleteRefreshToken(ctx context.Context, tokenHash string) error
	DeleteRefreshTokenFamily(ctx context.Context, familyID string) error
}

// pgRefreshTokenStore is the Postgres-backed RefreshTokenStore. Schema:
// migration 004_refresh_token_families (family_id UUID, used BOOLEAN).
type pgRefreshTokenStore struct {
	db *pgxpool.Pool
}

func (r *pgRefreshTokenStore) CreateRefreshToken(ctx context.Context, userID, tokenHash string, expiresAt time.Time, familyID string) (string, error) {
	if familyID == "" {
		err := r.db.QueryRow(ctx,
			`INSERT INTO refresh_tokens (user_id, token_hash, expires_at) VALUES ($1, $2, $3) RETURNING family_id`,
			userID, tokenHash, expiresAt,
		).Scan(&familyID)
		return familyID, err
	}
	_, err := r.db.Exec(ctx,
		`INSERT INTO refresh_tokens (user_id, token_hash, expires_at, family_id) VALUES ($1, $2, $3, $4)`,
		userID, tokenHash, expiresAt, familyID,
	)
	return familyID, err
}

func (r *pgRefreshTokenStore) FindRefreshToken(ctx context.Context, tokenHash string) (*RefreshTokenRecord, error) {
	rec := &RefreshTokenRecord{}
	err := r.db.QueryRow(ctx,
		`SELECT user_id, family_id, used, expires_at FROM refresh_tokens WHERE token_hash = $1`, tokenHash,
	).Scan(&rec.UserID, &rec.FamilyID, &rec.Used, &rec.ExpiresAt)
	if err != nil {
		return nil, err
	}
	return rec, nil
}

func (r *pgRefreshTokenStore) MarkRefreshTokenUsed(ctx context.Context, tokenHash string) error {
	_, err := r.db.Exec(ctx, `UPDATE refresh_tokens SET used = true WHERE token_hash = $1`, tokenHash)
	return err
}

func (r *pgRefreshTokenStore) DeleteRefreshToken(ctx context.Context, tokenHash string) error {
	_, err := r.db.Exec(ctx, `DELETE FROM refresh_tokens WHERE token_hash = $1`, tokenHash)
	return err
}

func (r *pgRefreshTokenStore) DeleteRefreshTokenFamily(ctx context.Context, familyID string) error {
	_, err := r.db.Exec(ctx, `DELETE FROM refresh_tokens WHERE family_id = $1`, familyID)
	return err
}
