package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/k8s-quiz/backend/pkg/config"
	"github.com/k8s-quiz/backend/pkg/models"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/github"
)

var (
	ErrInvalidToken   = errors.New("invalid token")
	ErrExpiredToken   = errors.New("token expired")
	ErrInvalidRefresh = errors.New("invalid refresh token")
)

type UserRepository interface {
	FindByGithubID(ctx context.Context, githubID int64) (*models.User, error)
	FindByID(ctx context.Context, id string) (*models.User, error)
	Create(ctx context.Context, u *models.User) error
	Update(ctx context.Context, u *models.User) error
}

type Service struct {
	cfg       *config.Config
	db        *pgxpool.Pool
	userRepo  UserRepository
	oauthConf *oauth2.Config
}

func NewService(cfg *config.Config, db *pgxpool.Pool, userRepo UserRepository) *Service {
	return &Service{
		cfg:      cfg,
		db:       db,
		userRepo: userRepo,
		oauthConf: &oauth2.Config{
			ClientID:     cfg.GithubClientID,
			ClientSecret: cfg.GithubClientSecret,
			Scopes:       []string{"read:user", "user:email"},
			Endpoint:     github.Endpoint,
		},
	}
}

func (s *Service) GetAuthURL(state string) string {
	return s.oauthConf.AuthCodeURL(state)
}

type githubUser struct {
	ID        int64  `json:"id"`
	Login     string `json:"login"`
	Email     string `json:"email"`
	AvatarURL string `json:"avatar_url"`
}

func (s *Service) HandleCallback(ctx context.Context, code string) (accessToken string, refreshToken string, u *models.User, err error) {
	token, err := s.oauthConf.Exchange(ctx, code)
	if err != nil {
		return "", "", nil, fmt.Errorf("oauth exchange: %w", err)
	}

	client := s.oauthConf.Client(ctx, token)
	ghUser, err := fetchGithubUser(client)
	if err != nil {
		return "", "", nil, fmt.Errorf("fetch github user: %w", err)
	}

	u, err = s.userRepo.FindByGithubID(ctx, ghUser.ID)
	if err != nil {
		u = &models.User{
			GithubID:  ghUser.ID,
			Username:  ghUser.Login,
			Email:     ghUser.Email,
			AvatarURL: ghUser.AvatarURL,
			Role:      models.RoleUser,
		}
		if err := s.userRepo.Create(ctx, u); err != nil {
			return "", "", nil, fmt.Errorf("create user: %w", err)
		}
	} else {
		u.Username = ghUser.Login
		u.Email = ghUser.Email
		u.AvatarURL = ghUser.AvatarURL
		if err := s.userRepo.Update(ctx, u); err != nil {
			return "", "", nil, fmt.Errorf("update user: %w", err)
		}
	}

	accessToken, err = s.generateAccessToken(u)
	if err != nil {
		return "", "", nil, err
	}

	refreshToken, err = s.generateRefreshToken(ctx, u)
	if err != nil {
		return "", "", nil, err
	}

	return accessToken, refreshToken, u, nil
}

func (s *Service) RefreshAccessToken(ctx context.Context, refreshToken string) (string, string, error) {
	tokenHash := hashToken(refreshToken)

	var userID string
	var expiresAt time.Time
	err := s.db.QueryRow(ctx,
		`SELECT user_id, expires_at FROM refresh_tokens WHERE token_hash = $1`, tokenHash,
	).Scan(&userID, &expiresAt)
	if err != nil {
		return "", "", ErrInvalidRefresh
	}

	if time.Now().After(expiresAt) {
		s.db.Exec(ctx, `DELETE FROM refresh_tokens WHERE token_hash = $1`, tokenHash)
		return "", "", ErrExpiredToken
	}

	u, err := s.userRepo.FindByID(ctx, userID)
	if err != nil {
		return "", "", ErrInvalidRefresh
	}

	s.db.Exec(ctx, `DELETE FROM refresh_tokens WHERE token_hash = $1`, tokenHash)

	accessToken, err := s.generateAccessToken(u)
	if err != nil {
		return "", "", err
	}

	newRefresh, err := s.generateRefreshToken(ctx, u)
	if err != nil {
		return "", "", err
	}

	return accessToken, newRefresh, nil
}

func (s *Service) ValidateAccessToken(tokenString string) (*models.User, error) {
	token, err := jwt.Parse(tokenString, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, ErrInvalidToken
		}
		return []byte(s.cfg.JWTSecret), nil
	})
	if err != nil {
		return nil, ErrInvalidToken
	}

	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok || !token.Valid {
		return nil, ErrInvalidToken
	}

	userID, ok := claims["sub"].(string)
	if !ok {
		return nil, ErrInvalidToken
	}

	u, err := s.userRepo.FindByID(context.Background(), userID)
	if err != nil {
		return nil, ErrInvalidToken
	}

	return u, nil
}

func (s *Service) generateAccessToken(u *models.User) (string, error) {
	claims := jwt.MapClaims{
		"sub":      u.ID,
		"username": u.Username,
		"role":     string(u.Role),
		"exp":      time.Now().Add(15 * time.Minute).Unix(),
		"iat":      time.Now().Unix(),
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString([]byte(s.cfg.JWTSecret))
}

func (s *Service) generateRefreshToken(ctx context.Context, u *models.User) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	rawToken := hex.EncodeToString(b)
	tokenHash := hashToken(rawToken)
	expiresAt := time.Now().Add(7 * 24 * time.Hour)

	_, err := s.db.Exec(ctx,
		`INSERT INTO refresh_tokens (user_id, token_hash, expires_at) VALUES ($1, $2, $3)`,
		u.ID, tokenHash, expiresAt,
	)
	if err != nil {
		return "", err
	}

	return rawToken, nil
}

func hashToken(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

func fetchGithubUser(client *http.Client) (*githubUser, error) {
	resp, err := client.Get("https://api.github.com/user")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var ghUser githubUser
	if err := json.Unmarshal(body, &ghUser); err != nil {
		return nil, err
	}

	if ghUser.Email == "" {
		resp2, err := client.Get("https://api.github.com/user/emails")
		if err == nil {
			defer resp2.Body.Close()
			body2, _ := io.ReadAll(resp2.Body)
			var emails []struct {
				Email   string `json:"email"`
				Primary bool   `json:"primary"`
			}
			if json.Unmarshal(body2, &emails) == nil {
				for _, e := range emails {
					if e.Primary {
						ghUser.Email = e.Email
						break
					}
				}
			}
		}
	}

	return &ghUser, nil
}
