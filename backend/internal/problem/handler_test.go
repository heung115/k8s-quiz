package problem

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/k8s-quiz/backend/internal/session"
	"github.com/k8s-quiz/backend/pkg/middleware"
	"github.com/k8s-quiz/backend/pkg/models"
)

type mockRepo struct {
	problems map[string]*models.Problem
	listErr  error
}

func newMockRepo() *mockRepo {
	return &mockRepo{problems: make(map[string]*models.Problem)}
}

func (m *mockRepo) List(ctx context.Context, category, difficulty, ptype string) ([]models.Problem, error) {
	if m.listErr != nil {
		return nil, m.listErr
	}
	var out []models.Problem
	for _, p := range m.problems {
		if category != "" && p.Category != category {
			continue
		}
		if difficulty != "" && p.Difficulty != difficulty {
			continue
		}
		if ptype != "" && p.Type != ptype {
			continue
		}
		out = append(out, *p)
	}
	return out, nil
}

func (m *mockRepo) FindByID(ctx context.Context, id string) (*models.Problem, error) {
	p, ok := m.problems[id]
	if !ok {
		return nil, errors.New("not found")
	}
	return p, nil
}

func (m *mockRepo) Upsert(ctx context.Context, p *models.Problem) error {
	m.problems[p.ID] = p
	return nil
}

func (m *mockRepo) Delete(ctx context.Context, id string) error {
	delete(m.problems, id)
	return nil
}

type mockLoader struct {
	problems []models.Problem
	err      error
}

func (m *mockLoader) LoadAll(ctx context.Context) ([]models.Problem, error) {
	return m.problems, m.err
}

type mockSession struct {
	startResult string
	startErr    error
	verifyOK    bool
	verifyLog   string
	verifyErr   error
	current     *session.CurrentSession
}

func (m *mockSession) StartProblem(ctx context.Context, userID, problemID string) (string, error) {
	return m.startResult, m.startErr
}
func (m *mockSession) GetCurrentSession(userID string) *session.CurrentSession {
	return m.current
}
func (m *mockSession) Verify(ctx context.Context, userID string) (bool, string, error) {
	return m.verifyOK, m.verifyLog, m.verifyErr
}
func (m *mockSession) SubmitChoice(ctx context.Context, userID, choiceID string) (bool, error) {
	return choiceID == "b", nil
}
func (m *mockSession) ResetEnvironment(ctx context.Context, userID string) error {
	return nil
}

func authStub() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Set(middleware.UserContextKey, &models.User{ID: "user-1", Username: "tester", Role: models.RoleUser})
		c.Next()
	}
}

func setupTestRouter(repo ProblemRepository, loader ProblemLoader, sess SessionService) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := NewHandler(repo, loader, sess)
	h.RegisterRoutes(r.Group("/api", authStub()))
	return r
}

func setupAdminRouter(repo ProblemRepository, loader ProblemLoader, sess SessionService) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := NewHandler(repo, loader, sess)
	h.RegisterAdminRoutes(r.Group("/api", authStub()))
	return r
}

func TestListProblems(t *testing.T) {
	repo := newMockRepo()
	repo.problems["p1"] = &models.Problem{ID: "p1", Title: "One", Category: "pod", Difficulty: "easy", Type: "fix"}
	repo.problems["p2"] = &models.Problem{ID: "p2", Title: "Two", Category: "network", Difficulty: "hard", Type: "find"}

	r := setupTestRouter(repo, &mockLoader{}, &mockSession{})
	req := httptest.NewRequest("GET", "/api/problems", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var body struct {
		Problems []models.Problem `json:"problems"`
	}
	json.Unmarshal(w.Body.Bytes(), &body)
	if len(body.Problems) != 2 {
		t.Errorf("expected 2 problems, got %d", len(body.Problems))
	}
}

func TestListProblemsFiltered(t *testing.T) {
	repo := newMockRepo()
	repo.problems["p1"] = &models.Problem{ID: "p1", Category: "pod", Difficulty: "easy", Type: "fix"}
	repo.problems["p2"] = &models.Problem{ID: "p2", Category: "network", Difficulty: "hard", Type: "find"}

	r := setupTestRouter(repo, &mockLoader{}, &mockSession{})
	req := httptest.NewRequest("GET", "/api/problems?category=pod", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	var body struct {
		Problems []models.Problem `json:"problems"`
	}
	json.Unmarshal(w.Body.Bytes(), &body)
	if len(body.Problems) != 1 {
		t.Errorf("expected 1 filtered problem, got %d", len(body.Problems))
	}
	if len(body.Problems) > 0 && body.Problems[0].ID != "p1" {
		t.Errorf("expected p1, got %s", body.Problems[0].ID)
	}
}

func TestGetProblem(t *testing.T) {
	repo := newMockRepo()
	repo.problems["p1"] = &models.Problem{ID: "p1", Title: "One"}

	r := setupTestRouter(repo, &mockLoader{}, &mockSession{})

	req := httptest.NewRequest("GET", "/api/problems/p1", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("expected 200 for existing, got %d", w.Code)
	}

	req = httptest.NewRequest("GET", "/api/problems/missing", nil)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 for missing, got %d", w.Code)
	}
}

func TestStartProblem(t *testing.T) {
	repo := newMockRepo()
	timeoutAt := time.Now().Add(30 * time.Minute).Truncate(time.Second)
	sess := &mockSession{
		startResult: "session-abc",
		current: &session.CurrentSession{
			SessionID: "session-abc",
			ProblemID: "p1",
			Status:    session.StatusBooting,
			TimeoutAt: timeoutAt,
		},
	}
	r := setupTestRouter(repo, &mockLoader{}, sess)

	req := httptest.NewRequest("POST", "/api/problems/p1/start", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	// Start must return the same shape as GET /api/sessions/current.
	var body struct {
		SessionID string    `json:"session_id"`
		ProblemID string    `json:"problem_id"`
		Status    string    `json:"status"`
		TimeoutAt time.Time `json:"timeout_at"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.SessionID != "session-abc" {
		t.Errorf("expected session-abc, got %s", body.SessionID)
	}
	if body.ProblemID != "p1" || body.Status != "booting" {
		t.Errorf("unexpected session shape: %+v", body)
	}
	if !body.TimeoutAt.Equal(timeoutAt) {
		t.Errorf("expected timeout_at %v, got %v", timeoutAt, body.TimeoutAt)
	}
}

func TestStartProblemNoSessionAfterStart(t *testing.T) {
	sess := &mockSession{startResult: "session-abc"} // current stays nil
	r := setupTestRouter(newMockRepo(), &mockLoader{}, sess)

	req := httptest.NewRequest("POST", "/api/problems/p1/start", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500 when session snapshot is missing, got %d", w.Code)
	}
}

func TestStartProblemError(t *testing.T) {
	sess := &mockSession{startErr: errors.New("boom")}
	r := setupTestRouter(newMockRepo(), &mockLoader{}, sess)

	req := httptest.NewRequest("POST", "/api/problems/p1/start", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d", w.Code)
	}
}

func TestVerify(t *testing.T) {
	sess := &mockSession{verifyOK: true, verifyLog: "SUCCESS"}
	r := setupTestRouter(newMockRepo(), &mockLoader{}, sess)

	req := httptest.NewRequest("POST", "/api/problems/p1/verify", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var body struct {
		Success bool   `json:"success"`
		Log     string `json:"log"`
	}
	json.Unmarshal(w.Body.Bytes(), &body)
	if !body.Success {
		t.Error("expected success true")
	}
	if body.Log != "SUCCESS" {
		t.Errorf("expected log SUCCESS, got %s", body.Log)
	}
}

func TestVerifyError(t *testing.T) {
	sess := &mockSession{verifyErr: errors.New("no session")}
	r := setupTestRouter(newMockRepo(), &mockLoader{}, sess)

	req := httptest.NewRequest("POST", "/api/problems/p1/verify", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestSubmitChoice(t *testing.T) {
	sess := &mockSession{}
	r := setupTestRouter(newMockRepo(), &mockLoader{}, sess)

	req := httptest.NewRequest("POST", "/api/problems/p1/submit", strings.NewReader(`{"choice_id":"b"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var body struct {
		Success bool `json:"success"`
	}
	json.Unmarshal(w.Body.Bytes(), &body)
	if !body.Success {
		t.Error("expected correct choice success")
	}
}

func TestSubmitChoiceMissingID(t *testing.T) {
	r := setupTestRouter(newMockRepo(), &mockLoader{}, &mockSession{})
	req := httptest.NewRequest("POST", "/api/problems/p1/submit", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func assertNoAnswerLeak(t *testing.T, body string) {
	t.Helper()
	if strings.Contains(body, "correct_choice") || strings.Contains(body, "grading_prompt") ||
		strings.Contains(body, "secret rubric") || strings.Contains(body, "answer-b") {
		t.Errorf("user-facing response leaks answer fields: %s", body)
	}
}

func secretProblem() *models.Problem {
	return &models.Problem{
		ID:            "p1",
		Title:         "One",
		VerifyType:    "choice",
		Choices:       []models.Choice{{ID: "a", Text: "A"}, {ID: "b", Text: "B"}},
		CorrectChoice: "answer-b",
		GradingPrompt: "secret rubric",
	}
}

func TestListProblemsHidesAnswers(t *testing.T) {
	repo := newMockRepo()
	repo.problems["p1"] = secretProblem()
	r := setupTestRouter(repo, &mockLoader{}, &mockSession{})

	req := httptest.NewRequest("GET", "/api/problems", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	assertNoAnswerLeak(t, w.Body.String())
	// choices must stay visible (users need the option list)
	var body struct {
		Problems []struct {
			ID      string          `json:"id"`
			Choices []models.Choice `json:"choices"`
		} `json:"problems"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(body.Problems) != 1 || len(body.Problems[0].Choices) != 2 {
		t.Errorf("expected choices to stay visible, got: %s", w.Body.String())
	}
}

func TestGetProblemHidesAnswers(t *testing.T) {
	repo := newMockRepo()
	repo.problems["p1"] = secretProblem()
	r := setupTestRouter(repo, &mockLoader{}, &mockSession{})

	req := httptest.NewRequest("GET", "/api/problems/p1", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	assertNoAnswerLeak(t, w.Body.String())
	if !strings.Contains(w.Body.String(), `"choices"`) {
		t.Errorf("expected choices to stay visible, got: %s", w.Body.String())
	}
}

func TestAdminListKeepsAnswers(t *testing.T) {
	repo := newMockRepo()
	repo.problems["p1"] = secretProblem()
	r := setupAdminRouter(repo, &mockLoader{}, &mockSession{})

	req := httptest.NewRequest("GET", "/api/admin/problems", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var body struct {
		Problems []models.Problem `json:"problems"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(body.Problems) != 1 {
		t.Fatalf("expected 1 problem, got %d", len(body.Problems))
	}
	if body.Problems[0].CorrectChoice != "answer-b" || body.Problems[0].GradingPrompt != "secret rubric" {
		t.Errorf("admin response must keep full model, got: %+v", body.Problems[0])
	}
}
