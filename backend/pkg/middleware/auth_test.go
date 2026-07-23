package middleware

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/k8s-quiz/backend/pkg/models"
)

type mockValidator struct {
	user *models.User
	err  error
}

func (m *mockValidator) ValidateAccessToken(tokenString string) (*models.User, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.user, nil
}

func setupRouter(validator TokenValidator) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/protected", Auth(validator), func(c *gin.Context) {
		u := GetUser(c)
		c.JSON(200, gin.H{"user": u.Username})
	})
	r.GET("/admin", Auth(validator), AdminOnly(), func(c *gin.Context) {
		c.JSON(200, gin.H{"admin": true})
	})
	return r
}

func TestAuthMiddleware_NoHeader(t *testing.T) {
	r := setupRouter(&mockValidator{})
	req := httptest.NewRequest("GET", "/protected", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

func TestAuthMiddleware_InvalidFormat(t *testing.T) {
	r := setupRouter(&mockValidator{})
	req := httptest.NewRequest("GET", "/protected", nil)
	req.Header.Set("Authorization", "Basic abc123")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

func TestAuthMiddleware_ValidToken(t *testing.T) {
	validator := &mockValidator{user: &models.User{ID: "u1", Username: "testuser", Role: models.RoleUser}}
	r := setupRouter(validator)
	req := httptest.NewRequest("GET", "/protected", nil)
	req.Header.Set("Authorization", "Bearer valid-token")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

func TestAuthMiddleware_InvalidToken(t *testing.T) {
	validator := &mockValidator{err: errors.New("invalid token")}
	r := setupRouter(validator)
	req := httptest.NewRequest("GET", "/protected", nil)
	req.Header.Set("Authorization", "Bearer bad-token")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

func TestAdminOnly_AdminUser(t *testing.T) {
	validator := &mockValidator{user: &models.User{ID: "u1", Username: "admin", Role: models.RoleAdmin}}
	r := setupRouter(validator)
	req := httptest.NewRequest("GET", "/admin", nil)
	req.Header.Set("Authorization", "Bearer admin-token")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

func TestAdminOnly_RegularUser(t *testing.T) {
	validator := &mockValidator{user: &models.User{ID: "u1", Username: "user", Role: models.RoleUser}}
	r := setupRouter(validator)
	req := httptest.NewRequest("GET", "/admin", nil)
	req.Header.Set("Authorization", "Bearer user-token")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403, got %d", w.Code)
	}
}
