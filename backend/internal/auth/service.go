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
	"log"
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
	cfg          *config.Config
	db           *pgxpool.Pool
	userRepo     UserRepository
	refreshStore RefreshTokenStore
	oauthConf    *oauth2.Config
}

func NewService(cfg *config.Config, db *pgxpool.Pool, userRepo UserRepository) *Service {
	return &Service{
		cfg:          cfg,
		db:           db,
		userRepo:     userRepo,
		refreshStore: &pgRefreshTokenStore{db: db},
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

	accessToken, refreshToken, err = s.IssueTokens(ctx, u)
	if err != nil {
		return "", "", nil, err
	}

	return accessToken, refreshToken, u, nil
}

// IssueTokens generates a fresh access token and a refresh token that starts
// a new token family (AUTH-5).
func (s *Service) IssueTokens(ctx context.Context, u *models.User) (accessToken, refreshToken string, err error) {
	accessToken, err = s.generateAccessToken(u)
	if err != nil {
		return "", "", err
	}
	refreshToken, err = s.generateRefreshToken(ctx, u, "")
	if err != nil {
		return "", "", err
	}
	return accessToken, refreshToken, nil
}

// RefreshAccessToken rotates a refresh token (AUTH-5): the presented token is
// marked used and a new token is issued in the SAME family. Presenting an
// already-used token is replay/theft → the whole family is revoked.
func (s *Service) RefreshAccessToken(ctx context.Context, refreshToken string) (string, string, *models.User, error) {
	tokenHash := hashToken(refreshToken)

	// SEC3-2: the claim is a single atomic UPDATE (used=false → true), so two
	// concurrent refreshes of the same token cannot both succeed; the loser
	// sees Used=true and takes the reuse path.
	rec, err := s.refreshStore.ClaimRefreshToken(ctx, tokenHash)
	if err != nil {
		return "", "", nil, ErrInvalidRefresh
	}

	if rec.Used {
		// Token reuse: either a buggy client replaying or a stolen token being
		// played after the legitimate client rotated. Revoke the whole family.
		s.refreshStore.DeleteRefreshTokenFamily(ctx, rec.FamilyID)
		log.Printf("SECURITY: refresh token reuse detected (user=%s family=%s); token family revoked", rec.UserID, rec.FamilyID)
		return "", "", nil, ErrInvalidRefresh
	}

	if time.Now().After(rec.ExpiresAt) {
		s.refreshStore.DeleteRefreshToken(ctx, tokenHash)
		return "", "", nil, ErrExpiredToken
	}

	u, err := s.userRepo.FindByID(ctx, rec.UserID)
	if err != nil {
		return "", "", nil, ErrInvalidRefresh
	}

	accessToken, err := s.generateAccessToken(u)
	if err != nil {
		return "", "", nil, err
	}

	newRefresh, err := s.generateRefreshToken(ctx, u, rec.FamilyID)
	if err != nil {
		return "", "", nil, err
	}

	return accessToken, newRefresh, u, nil
}

// RevokeRefresh revokes the whole refresh-token family that the presented
// token belongs to (server-side logout). A missing/unknown token is not an
// error.
func (s *Service) RevokeRefresh(ctx context.Context, refreshToken string) {
	if refreshToken == "" {
		return
	}
	tokenHash := hashToken(refreshToken)
	rec, err := s.refreshStore.FindRefreshToken(ctx, tokenHash)
	if err != nil {
		// Unknown token: best-effort delete by hash (pre-family rows).
		s.refreshStore.DeleteRefreshToken(ctx, tokenHash)
		return
	}
	s.refreshStore.DeleteRefreshTokenFamily(ctx, rec.FamilyID)
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

// generateRefreshToken mints a raw refresh token and stores its hash. An
// empty familyID starts a new family (login); a given familyID joins that
// family (rotation, AUTH-5).
func (s *Service) generateRefreshToken(ctx context.Context, u *models.User, familyID string) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	rawToken := hex.EncodeToString(b)
	tokenHash := hashToken(rawToken)
	expiresAt := time.Now().Add(7 * 24 * time.Hour)

	if _, err := s.refreshStore.CreateRefreshToken(ctx, u.ID, tokenHash, expiresAt, familyID); err != nil {
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
