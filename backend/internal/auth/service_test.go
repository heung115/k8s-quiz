package auth

import (
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/k8s-quiz/backend/pkg/config"
	"github.com/k8s-quiz/backend/pkg/models"
)

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
