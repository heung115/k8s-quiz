package session

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/k8s-quiz/backend/pkg/middleware"
	"github.com/k8s-quiz/backend/pkg/models"
)

func setupSessionRouter(svc *Service) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	authStub := func(c *gin.Context) {
		c.Set(middleware.UserContextKey, &models.User{ID: "user-1", Username: "tester", Role: models.RoleUser})
		c.Next()
	}
	protected := r.Group("/api", authStub)
	NewHandler(svc).RegisterRoutes(protected)
	return r
}

func TestGetCurrentNoSession(t *testing.T) {
	svc, _, _ := newTestService()
	r := setupSessionRouter(svc)

	req := httptest.NewRequest("GET", "/api/sessions/current", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestGetCurrentWithSession(t *testing.T) {
	svc, _, _ := newTestService()
	svc.sessions["user-1"] = &Session{
		ID:        "sess-1",
		UserID:    "user-1",
		ProblemID: "p1",
		Status:    StatusReady,
		TimeoutAt: time.Now().Add(30 * time.Minute),
	}
	r := setupSessionRouter(svc)

	req := httptest.NewRequest("GET", "/api/sessions/current", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var body struct {
		SessionID string `json:"session_id"`
		ProblemID string `json:"problem_id"`
		Status    string `json:"status"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.SessionID != "sess-1" || body.ProblemID != "p1" || body.Status != "ready" {
		t.Errorf("unexpected body: %+v", body)
	}
}

func TestEndCurrentRemovesSession(t *testing.T) {
	svc, mgr, _ := newTestService()
	svc.sessions["user-1"] = &Session{
		ID:          "sess-1",
		UserID:      "user-1",
		ProblemID:   "p1",
		ContainerID: "container-123",
		Status:      StatusReady,
	}
	r := setupSessionRouter(svc)

	req := httptest.NewRequest("DELETE", "/api/sessions/current", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", w.Code)
	}
	if svc.GetSession("user-1") != nil {
		t.Error("expected session to be ended")
	}
	mgr.mu.Lock()
	removed := len(mgr.removed)
	mgr.mu.Unlock()
	if removed != 1 {
		t.Errorf("expected container removal, got %d", removed)
	}
}

func TestEndCurrentNoSession(t *testing.T) {
	svc, _, _ := newTestService()
	r := setupSessionRouter(svc)

	req := httptest.NewRequest("DELETE", "/api/sessions/current", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204 even without session, got %d", w.Code)
	}
}
