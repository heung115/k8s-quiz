package auth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/k8s-quiz/backend/pkg/config"
)

func newTestHandler() *Handler {
	cfg := &config.Config{FrontendURL: "http://localhost:5173"}
	return NewHandler(NewService(cfg, nil, nil))
}

func setupAuthRouter(h *Handler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h.RegisterRoutes(r.Group("/api"))
	return r
}

func TestGithubCallbackInvalidState(t *testing.T) {
	r := setupAuthRouter(newTestHandler())

	req := httptest.NewRequest("GET", "/api/auth/github/callback?state=abc&code=xyz", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusTemporaryRedirect {
		t.Fatalf("expected 307 redirect, got %d", w.Code)
	}
	if loc := w.Header().Get("Location"); loc != "http://localhost:5173/login?error=invalid_state" {
		t.Errorf("unexpected redirect location: %s", loc)
	}
}

func TestGithubCallbackMissingCode(t *testing.T) {
	r := setupAuthRouter(newTestHandler())

	req := httptest.NewRequest("GET", "/api/auth/github/callback?state=abc", nil)
	req.AddCookie(&http.Cookie{Name: "oauth_state", Value: "abc"})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusTemporaryRedirect {
		t.Fatalf("expected 307 redirect, got %d", w.Code)
	}
	if loc := w.Header().Get("Location"); loc != "http://localhost:5173/login?error=missing_code" {
		t.Errorf("unexpected redirect location: %s", loc)
	}
}

func TestGithubLoginRedirectsToGithub(t *testing.T) {
	r := setupAuthRouter(newTestHandler())

	req := httptest.NewRequest("GET", "/api/auth/github", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusTemporaryRedirect {
		t.Fatalf("expected 307 redirect, got %d", w.Code)
	}
	loc := w.Header().Get("Location")
	if !strings.Contains(loc, "github.com/login/oauth/authorize") {
		t.Errorf("expected redirect to GitHub authorize URL, got %q", loc)
	}
	if w.Header().Get("Set-Cookie") == "" {
		t.Error("expected oauth_state cookie to be set")
	}
}
