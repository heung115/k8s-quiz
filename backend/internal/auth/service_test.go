package auth

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/k8s-quiz/backend/pkg/config"
	"github.com/k8s-quiz/backend/pkg/models"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func githubClient(respond func(*http.Request) (int, string, string)) *http.Client {
	return &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		status, contentType, body := respond(req)
		return &http.Response{
			StatusCode: status,
			Header:     http.Header{"Content-Type": []string{contentType}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    req,
		}, nil
	})}
}

type accessTokenUserRepository struct{ user *models.User }

func (*accessTokenUserRepository) FindByGithubID(context.Context, int64) (*models.User, error) {
	return nil, errors.New("not implemented")
}
func (r *accessTokenUserRepository) FindByID(context.Context, string) (*models.User, error) {
	return r.user, nil
}
func (*accessTokenUserRepository) Create(context.Context, *models.User) error { return nil }
func (*accessTokenUserRepository) Update(context.Context, *models.User) error { return nil }

func TestGenerateAndValidateAccessToken(t *testing.T) {
	cfg := &config.Config{
		JWTSecret: "test-secret",
	}
	svc := &Service{cfg: cfg}

	user := &models.User{
		ID:       "user-123",
		Username: "testuser",
		Role:     models.RoleUser,
	}

	token, err := svc.generateAccessToken(user)
	if err != nil {
		t.Fatalf("generateAccessToken failed: %v", err)
	}
	if token == "" {
		t.Fatal("expected non-empty token")
	}

	parsed, err := jwt.Parse(token, func(tok *jwt.Token) (interface{}, error) {
		return []byte("test-secret"), nil
	})
	if err != nil {
		t.Fatalf("failed to parse token: %v", err)
	}
	if !parsed.Valid {
		t.Fatal("expected valid token")
	}

	claims, ok := parsed.Claims.(jwt.MapClaims)
	if !ok {
		t.Fatal("expected MapClaims")
	}
	if claims["sub"] != "user-123" {
		t.Errorf("expected sub=user-123, got %v", claims["sub"])
	}
	if claims["username"] != "testuser" {
		t.Errorf("expected username=testuser, got %v", claims["username"])
	}
	if claims["role"] != "user" {
		t.Errorf("expected role=user, got %v", claims["role"])
	}
}

func TestAccessTokenExpiry(t *testing.T) {
	cfg := &config.Config{JWTSecret: "test-secret"}
	svc := &Service{cfg: cfg}

	user := &models.User{ID: "u1", Username: "test", Role: models.RoleUser}
	token, err := svc.generateAccessToken(user)
	if err != nil {
		t.Fatalf("generateAccessToken failed: %v", err)
	}

	parsed, err := jwt.Parse(token, func(tok *jwt.Token) (interface{}, error) {
		return []byte("test-secret"), nil
	})
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}

	claims := parsed.Claims.(jwt.MapClaims)
	exp, err := claims.GetExpirationTime()
	if err != nil {
		t.Fatalf("get exp failed: %v", err)
	}

	expectedExpiry := time.Now().Add(15 * time.Minute)
	diff := exp.Time.Sub(expectedExpiry)
	if diff > 5*time.Second || diff < -5*time.Second {
		t.Errorf("token expiry not ~15min from now: %v", exp.Time)
	}
}

func TestValidateAccessTokenWithExpiryReturnsSignedExpiry(t *testing.T) {
	user := &models.User{ID: "u1", Username: "test", Role: models.RoleUser}
	svc := &Service{
		cfg:      &config.Config{JWTSecret: "test-secret"},
		userRepo: &accessTokenUserRepository{user: user},
	}
	token, err := svc.generateAccessToken(user)
	if err != nil {
		t.Fatal(err)
	}
	gotUser, expiresAt, err := svc.ValidateAccessTokenWithExpiry(token)
	if err != nil {
		t.Fatal(err)
	}
	if gotUser != user {
		t.Fatalf("validated user = %p, want %p", gotUser, user)
	}
	want := time.Now().Add(15 * time.Minute)
	if delta := expiresAt.Sub(want); delta < -2*time.Second || delta > 2*time.Second {
		t.Fatalf("verified expiry = %v, want near %v", expiresAt, want)
	}
}

func TestValidateAccessTokenWithExpiryRejectsMissingExpiry(t *testing.T) {
	user := &models.User{ID: "u1", Username: "test", Role: models.RoleUser}
	svc := &Service{
		cfg:      &config.Config{JWTSecret: "test-secret"},
		userRepo: &accessTokenUserRepository{user: user},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"sub": user.ID})
	raw, err := token.SignedString([]byte("test-secret"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.ValidateAccessTokenWithExpiry(raw); err == nil {
		t.Fatal("access token without exp was accepted for a long-lived transport")
	}
}

func TestValidateAccessToken_WrongSecret(t *testing.T) {
	cfg := &config.Config{JWTSecret: "secret-a"}
	svc := &Service{cfg: cfg}

	user := &models.User{ID: "u1", Username: "test", Role: models.RoleUser}
	token, _ := svc.generateAccessToken(user)

	svc2 := &Service{cfg: &config.Config{JWTSecret: "secret-b"}}
	_, err := svc2.ValidateAccessToken(token)
	if err == nil {
		t.Fatal("expected error for wrong secret")
	}
}

func TestValidateAccessToken_MalformedToken(t *testing.T) {
	cfg := &config.Config{JWTSecret: "test-secret"}
	svc := &Service{cfg: cfg}

	_, err := svc.ValidateAccessToken("not-a-valid-token")
	if err == nil {
		t.Fatal("expected error for malformed token")
	}
}

func TestHashToken(t *testing.T) {
	hash1 := hashToken("token-a")
	hash2 := hashToken("token-a")
	hash3 := hashToken("token-b")

	if hash1 != hash2 {
		t.Error("same input should produce same hash")
	}
	if hash1 == hash3 {
		t.Error("different input should produce different hash")
	}
	if len(hash1) != 64 {
		t.Errorf("expected 64 char hex hash, got %d", len(hash1))
	}
}

func TestFetchGithubUserAcceptsValidatedUserAndPrimaryEmail(t *testing.T) {
	client := githubClient(func(req *http.Request) (int, string, string) {
		switch req.URL.Path {
		case "/user":
			return http.StatusOK, "application/json; charset=utf-8", `{"id":42,"login":" octocat ","email":""}`
		case "/user/emails":
			return http.StatusOK, "application/vnd.github+json", `[{"email":" octocat@example.com ","primary":true}]`
		default:
			t.Fatalf("unexpected GitHub path %q", req.URL.Path)
			return 0, "", ""
		}
	})

	got, err := fetchGithubUser(client)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != 42 || got.Login != "octocat" || got.Email != "octocat@example.com" {
		t.Fatalf("github user = %+v", got)
	}
}

func TestFetchGithubUserRejectsInvalidUserResponses(t *testing.T) {
	tests := []struct {
		name        string
		status      int
		contentType string
		body        string
	}{
		{name: "non-success status", status: http.StatusUnauthorized, contentType: "application/json", body: `{}`},
		{name: "wrong media type", status: http.StatusOK, contentType: "text/html", body: `{"id":1,"login":"octocat"}`},
		{name: "oversized body", status: http.StatusOK, contentType: "application/json", body: strings.Repeat(" ", int(maxGithubResponseBytes)+1)},
		{name: "malformed JSON", status: http.StatusOK, contentType: "application/json", body: `{"id":`},
		{name: "trailing JSON", status: http.StatusOK, contentType: "application/json", body: `{"id":1,"login":"octocat"}{}`},
		{name: "zero ID", status: http.StatusOK, contentType: "application/json", body: `{"id":0,"login":"octocat"}`},
		{name: "empty login", status: http.StatusOK, contentType: "application/json", body: `{"id":1,"login":"  "}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := githubClient(func(*http.Request) (int, string, string) {
				return tt.status, tt.contentType, tt.body
			})
			if _, err := fetchGithubUser(client); err == nil {
				t.Fatal("invalid GitHub user response was accepted")
			}
		})
	}
}

func TestFetchGithubUserRejectsInvalidEmailFallback(t *testing.T) {
	tests := []struct {
		name        string
		status      int
		contentType string
		body        string
	}{
		{name: "non-success status", status: http.StatusForbidden, contentType: "application/json", body: `[]`},
		{name: "wrong media type", status: http.StatusOK, contentType: "text/plain", body: `[]`},
		{name: "oversized body", status: http.StatusOK, contentType: "application/json", body: strings.Repeat(" ", int(maxGithubResponseBytes)+1)},
		{name: "malformed JSON", status: http.StatusOK, contentType: "application/json", body: `[`},
		{name: "empty primary email", status: http.StatusOK, contentType: "application/json", body: `[{"email":"  ","primary":true}]`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := githubClient(func(req *http.Request) (int, string, string) {
				if req.URL.Path == "/user" {
					return http.StatusOK, "application/json", `{"id":1,"login":"octocat"}`
				}
				return tt.status, tt.contentType, tt.body
			})
			if _, err := fetchGithubUser(client); err == nil {
				t.Fatal("invalid GitHub email response was accepted")
			}
		})
	}
}
