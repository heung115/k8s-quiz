package session

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/k8s-quiz/backend/internal/runner"
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

func TestEndCurrentWaitsForBackgroundWorkerOwningExactCleanup(t *testing.T) {
	svc, runtime, _, durable := newDurableTestService(t)
	svc.SetStartCooldown(0)
	if _, err := svc.StartProblemOperation(context.Background(), "user-1", "p1", "start:p1:end-worker-race"); err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, svc, "user-1", StatusReady)
	sess := svc.GetSession("user-1")
	sess.mu.Lock()
	ref := sess.Allocation
	sess.mu.Unlock()
	operationID := "end:worker-race-0001"

	if decision, err := durable.RequestEnd(context.Background(), "user-1", ref.Session, operationID); err != nil {
		t.Fatalf("RequestEnd: %v", err)
	} else if decision.State != runner.EndPending {
		t.Fatalf("initial end decision = %+v", decision)
	}
	runtime.destroyEntered = make(chan runner.AllocationRef, 1)
	runtime.destroyGate = make(chan struct{})
	workerDone := make(chan error, 1)
	go func() {
		_, err := svc.processDurableWorkOnce(context.Background())
		workerDone <- err
	}()
	select {
	case got := <-runtime.destroyEntered:
		if got != ref {
			t.Fatalf("worker targeted %+v, want %+v", got, ref)
		}
	case <-time.After(time.Second):
		t.Fatal("background worker did not enter destroy")
	}

	r := setupSessionRouter(svc)
	requestBody := []byte(`{"session_id":"` + ref.Session.SessionID + `","generation":1}`)
	req := httptest.NewRequest("POST", "/api/sessions/end", bytes.NewReader(requestBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", operationID)
	w := httptest.NewRecorder()
	handlerDone := make(chan struct{})
	go func() {
		r.ServeHTTP(w, req)
		close(handlerDone)
	}()
	select {
	case <-handlerDone:
		t.Fatalf("end request completed while the exact cleanup worker still held the user transition lock: status=%d body=%s", w.Code, w.Body.String())
	case <-time.After(100 * time.Millisecond):
	}
	if svc.GetSession("user-1") == nil {
		t.Fatal("in-flight cleanup must retain the exact session until absence is committed")
	}

	close(runtime.destroyGate)
	if err := <-workerDone; err != nil {
		t.Fatalf("background destroy: %v", err)
	}
	select {
	case <-handlerDone:
	case <-time.After(time.Second):
		t.Fatal("end request did not resume after the exact cleanup worker released the user transition lock")
	}
	if w.Code != http.StatusNoContent {
		t.Fatalf("end replay after worker cleanup expected 204, got %d body=%s", w.Code, w.Body.String())
	}
	runtime.mu.Lock()
	removed := append([]runner.AllocationRef(nil), runtime.removed...)
	runtime.mu.Unlock()
	if len(removed) != 1 || removed[0] != ref {
		t.Fatalf("provider destroy effects = %+v, want exactly [%+v]", removed, ref)
	}

	w = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/api/sessions/end", bytes.NewReader([]byte(`{"session_id":"`+ref.Session.SessionID+`","generation":1}`)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", operationID)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("completed cleanup replay expected 204, got %d body=%s", w.Code, w.Body.String())
	}
	if svc.GetSession("user-1") != nil {
		t.Fatal("completed cleanup replay did not evict session")
	}
}

func TestEndCurrentRejectsStaleGenerationWithoutMutation(t *testing.T) {
	svc, runtime, _, _ := newDurableTestService(t)
	svc.SetStartCooldown(0)
	if _, err := svc.StartProblemOperation(context.Background(), "user-1", "p1", "start:p1:end-stale"); err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, svc, "user-1", StatusReady)
	sess := svc.GetSession("user-1")
	sess.mu.Lock()
	ref := sess.Allocation
	sess.mu.Unlock()

	req := httptest.NewRequest("POST", "/api/sessions/end", bytes.NewReader([]byte(`{"session_id":"`+ref.Session.SessionID+`","generation":2}`)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "end:stale-generation-0001")
	w := httptest.NewRecorder()
	setupSessionRouter(svc).ServeHTTP(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("stale end expected 409, got %d body=%s", w.Code, w.Body.String())
	}
	if current := svc.GetSession("user-1"); current == nil {
		t.Fatal("stale end removed the current session")
	}
	runtime.mu.Lock()
	removed := len(runtime.removed)
	runtime.mu.Unlock()
	if removed != 0 {
		t.Fatalf("stale end called provider %d time(s)", removed)
	}
}

func TestEndCurrentRequiresExactIdentityAndOperationKey(t *testing.T) {
	svc, _, _, _ := newDurableTestService(t)
	r := setupSessionRouter(svc)
	for name, request := range map[string]*http.Request{
		"missing key": httptest.NewRequest("POST", "/api/sessions/end", bytes.NewReader([]byte(`{"session_id":"s1","generation":1}`))),
		"missing identity": func() *http.Request {
			req := httptest.NewRequest("POST", "/api/sessions/end", bytes.NewReader([]byte(`{}`)))
			req.Header.Set("Idempotency-Key", "end:missing-identity")
			return req
		}(),
	} {
		request.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, request)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%s expected 400, got %d body=%s", name, w.Code, w.Body.String())
		}
	}
}

func TestLegacyDeleteCurrentIsNotRegistered(t *testing.T) {
	svc, _, _, _ := newDurableTestService(t)
	w := httptest.NewRecorder()
	setupSessionRouter(svc).ServeHTTP(w, httptest.NewRequest("DELETE", "/api/sessions/current", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("legacy unscoped delete expected 404, got %d", w.Code)
	}
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
		ID:         "sess-1",
		UserID:     "user-1",
		ProblemID:  "p1",
		Generation: 1,
		Status:     StatusReady,
		TimeoutAt:  time.Now().Add(30 * time.Minute),
	}
	r := setupSessionRouter(svc)

	req := httptest.NewRequest("GET", "/api/sessions/current", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var body struct {
		SessionID  string `json:"session_id"`
		ProblemID  string `json:"problem_id"`
		Generation uint64 `json:"generation"`
		Status     string `json:"status"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.SessionID != "sess-1" || body.ProblemID != "p1" || body.Generation != 1 || body.Status != "ready" {
		t.Errorf("unexpected body: %+v", body)
	}
}
