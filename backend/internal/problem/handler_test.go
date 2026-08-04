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
	"github.com/k8s-quiz/backend/internal/runner"
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

func (m *mockRepo) ListAll(ctx context.Context) ([]models.Problem, error) {
	return m.List(ctx, "", "", "")
}

func (m *mockRepo) FindByID(ctx context.Context, id string) (*models.Problem, error) {
	p, ok := m.problems[id]
	if !ok {
		return nil, errors.New("not found")
	}
	return p, nil
}

func (m *mockRepo) FindAnyByID(ctx context.Context, id string) (*models.Problem, error) {
	return m.FindByID(ctx, id)
}

func (m *mockRepo) Upsert(ctx context.Context, p *models.Problem) error {
	m.problems[p.ID] = p
	return nil
}

func (m *mockRepo) Delete(ctx context.Context, id string) error {
	if problem, found := m.problems[id]; found && problem.Revision != "" {
		return ErrCatalogManagedProblem
	}
	delete(m.problems, id)
	return nil
}

type mockLoader struct {
	problems []models.Problem
	err      error
	healthy  *bool
}

func (m *mockLoader) Sync(ctx context.Context) (int, error) {
	return len(m.problems), m.err
}

func (m *mockLoader) Healthy() bool {
	return m.healthy == nil || *m.healthy
}

type mockSession struct {
	startResult   string
	startErr      error
	startOp       string
	startUser     string
	startProblem  string
	verifyOK      bool
	verifyLog     string
	verifyErr     error
	verifyOp      string
	choiceErr     error
	choiceUser    string
	choiceProblem string
	choiceExpect  runner.SessionRef
	choiceOp      string
	choiceID      string
	resetErr      error
	resetResult   *session.CurrentSession
	resetOp       string
	resetProblem  string
	resetExpected runner.SessionRef
	current       *session.CurrentSession
}

func (m *mockSession) StartProblem(ctx context.Context, userID, problemID string) (string, error) {
	return m.startResult, m.startErr
}
func (m *mockSession) StartProblemOperation(ctx context.Context, userID, problemID, operationID string) (*session.CurrentSession, error) {
	m.startUser, m.startProblem, m.startOp = userID, problemID, operationID
	return m.current, m.startErr
}
func (m *mockSession) GetCurrentSession(userID string) *session.CurrentSession {
	return m.current
}
func (m *mockSession) Verify(ctx context.Context, userID, operationID string) (bool, string, error) {
	m.verifyOp = operationID
	return m.verifyOK, m.verifyLog, m.verifyErr
}
func (m *mockSession) SubmitChoice(_ context.Context, userID, problemID string, expected runner.SessionRef, operationID, choiceID string) (bool, error) {
	m.choiceUser, m.choiceProblem, m.choiceExpect = userID, problemID, expected
	m.choiceOp, m.choiceID = operationID, choiceID
	return choiceID == "b", m.choiceErr
}
func (m *mockSession) ResetEnvironment(ctx context.Context, userID string) error {
	return m.resetErr
}
func (m *mockSession) ResetEnvironmentOperation(ctx context.Context, userID, problemID string, expected runner.SessionRef, operationID string) (*session.CurrentSession, error) {
	m.resetProblem, m.resetExpected, m.resetOp = problemID, expected, operationID
	return m.resetResult, m.resetErr
}

func authStub() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Set(middleware.UserContextKey, &models.User{ID: "user-1", Username: "tester", Role: models.RoleUser})
		c.Next()
	}
}

func setupTestRouter(repo ProblemRepository, syncer ProblemSyncer, sess SessionService) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := NewHandler(repo, syncer, sess)
	h.RegisterRoutes(r.Group("/api", authStub()))
	return r
}

func setupAdminRouter(repo ProblemRepository, syncer ProblemSyncer, sess SessionService) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := NewHandler(repo, syncer, sess)
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

func TestPublicProblemReadsReturn503WhenCatalogIsUnhealthy(t *testing.T) {
	healthy := false
	repo := newMockRepo()
	repo.problems["p1"] = &models.Problem{ID: "p1", Title: "One"}
	syncer := &mockLoader{healthy: &healthy}
	r := setupTestRouter(repo, syncer, &mockSession{})

	for _, path := range []string{"/api/problems", "/api/problems/p1"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("GET %s status=%d body=%s, want 503", path, w.Code, w.Body.String())
		}
	}
}

func TestAdminProblemListRemainsAvailableWhenCatalogIsUnhealthy(t *testing.T) {
	healthy := false
	repo := newMockRepo()
	repo.problems["draft"] = &models.Problem{ID: "draft", Title: "Draft"}
	r := setupAdminRouter(repo, &mockLoader{healthy: &healthy}, &mockSession{})

	req := httptest.NewRequest(http.MethodGet, "/api/admin/problems", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("admin list status=%d body=%s, want 200", w.Code, w.Body.String())
	}
}

func TestStartProblem(t *testing.T) {
	repo := newMockRepo()
	timeoutAt := time.Now().Add(30 * time.Minute).Truncate(time.Second)
	sess := &mockSession{
		startResult: "session-abc",
		current: &session.CurrentSession{
			OperationID:   "operation-12345678",
			SessionID:     "session-abc",
			ProblemID:     "p1",
			Generation:    1,
			Status:        session.StatusBooting,
			TimeoutAt:     timeoutAt,
			EventSequence: 2,
		},
	}
	r := setupTestRouter(repo, &mockLoader{}, sess)

	req := httptest.NewRequest("POST", "/api/problems/p1/start", nil)
	req.Header.Set("Idempotency-Key", "operation-12345678")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	// Start must return the same shape as GET /api/sessions/current.
	var body struct {
		RequestID     string    `json:"request_id"`
		OperationID   string    `json:"operation_id"`
		SessionID     string    `json:"session_id"`
		ProblemID     string    `json:"problem_id"`
		Generation    uint64    `json:"generation"`
		Status        string    `json:"status"`
		TimeoutAt     time.Time `json:"timeout_at"`
		EventSequence uint64    `json:"event_sequence"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.RequestID != "operation-12345678" || body.OperationID != "operation-12345678" || body.SessionID != "session-abc" || body.Generation != 1 || body.EventSequence != 2 {
		t.Errorf("expected session-abc, got %s", body.SessionID)
	}
	if sess.startUser != "user-1" || sess.startProblem != "p1" || sess.startOp != "operation-12345678" {
		t.Fatalf("start operation was not forwarded exactly: user=%q problem=%q op=%q", sess.startUser, sess.startProblem, sess.startOp)
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
	req.Header.Set("Idempotency-Key", "operation-12345678")
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
	req.Header.Set("Idempotency-Key", "operation-12345678")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d", w.Code)
	}
}

func TestStartProblemRequiresValidIdempotencyKeyBeforeServiceMutation(t *testing.T) {
	for _, key := range []string{"", "short", "contains space"} {
		t.Run(key, func(t *testing.T) {
			sess := &mockSession{current: &session.CurrentSession{SessionID: "must-not-return"}}
			r := setupTestRouter(newMockRepo(), &mockLoader{}, sess)
			req := httptest.NewRequest(http.MethodPost, "/api/problems/p1/start", nil)
			if key != "" {
				req.Header.Set("Idempotency-Key", key)
			}
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("key %q status=%d, want 400", key, w.Code)
			}
			if sess.startOp != "" {
				t.Fatalf("key %q reached service as %q", key, sess.startOp)
			}
		})
	}
}

func TestVerify(t *testing.T) {
	sess := &mockSession{verifyOK: true, verifyLog: "SUCCESS"}
	r := setupTestRouter(newMockRepo(), &mockLoader{}, sess)

	req := httptest.NewRequest("POST", "/api/problems/p1/verify", nil)
	req.Header.Set("Idempotency-Key", "verify-operation-1")
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
	if sess.verifyOp != "verify-operation-1" {
		t.Errorf("handler passed operation id %q", sess.verifyOp)
	}
}

func TestVerifyError(t *testing.T) {
	sess := &mockSession{verifyErr: errors.New("no session")}
	r := setupTestRouter(newMockRepo(), &mockLoader{}, sess)

	req := httptest.NewRequest("POST", "/api/problems/p1/verify", nil)
	req.Header.Set("Idempotency-Key", "verify-operation-2")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestSubmitChoice(t *testing.T) {
	sess := &mockSession{}
	r := setupTestRouter(newMockRepo(), &mockLoader{}, sess)

	req := httptest.NewRequest("POST", "/api/problems/p1/submit", strings.NewReader(`{"session_id":"session-1","generation":3,"choice_id":"b"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "choice-operation-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var body struct {
		RequestID  string `json:"request_id"`
		ProblemID  string `json:"problem_id"`
		SessionID  string `json:"session_id"`
		Generation uint64 `json:"generation"`
		Success    bool   `json:"success"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !body.Success {
		t.Error("expected correct choice success")
	}
	if body.RequestID != "choice-operation-1" || body.ProblemID != "p1" ||
		body.SessionID != "session-1" || body.Generation != 3 {
		t.Fatalf("unexpected choice response identity: %+v", body)
	}
	if sess.choiceUser != "user-1" || sess.choiceProblem != "p1" ||
		sess.choiceExpect != (runner.SessionRef{SessionID: "session-1", Generation: 3}) ||
		sess.choiceOp != "choice-operation-1" || sess.choiceID != "b" {
		t.Fatalf("handler passed incomplete choice identity: %+v", sess)
	}
}

func TestSubmitChoiceMissingID(t *testing.T) {
	r := setupTestRouter(newMockRepo(), &mockLoader{}, &mockSession{})
	req := httptest.NewRequest("POST", "/api/problems/p1/submit", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "choice-operation-2")
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

func approvedRuntimeProblem() *models.Problem {
	return &models.Problem{
		ID:             "approved",
		Revision:       "trusted-revision",
		Title:          "Original title",
		TimeoutMinutes: 25,
		VerifyType:     "choice",
		BaseImage:      "trusted-base:v1",
		Image:          "trusted-image:v1",
		Choices:        []models.Choice{{ID: "a", Text: "A"}, {ID: "b", Text: "B"}},
		CorrectChoice:  "b",
		GradingPrompt:  "trusted rubric",
	}
}

func assertApprovedRuntimePreserved(t *testing.T, p *models.Problem) {
	t.Helper()
	if p.Revision != "trusted-revision" || p.TimeoutMinutes != 25 || p.VerifyType != "choice" ||
		p.BaseImage != "trusted-base:v1" || p.Image != "trusted-image:v1" ||
		len(p.Choices) != 2 || p.CorrectChoice != "b" || p.GradingPrompt != "trusted rubric" {
		t.Fatalf("approved runtime fields changed: %+v", p)
	}
}

func TestAdminCreateCannotOverwriteApprovedRuntimeFields(t *testing.T) {
	repo := newMockRepo()
	repo.problems["approved"] = approvedRuntimeProblem()
	r := setupAdminRouter(repo, &mockLoader{}, &mockSession{})
	body := `{
		"id":"approved","title":"Editable metadata","timeout_minutes":99,
		"verify_type":"script","base_image":"attacker-base:latest","image":"attacker:latest",
		"choices":[{"id":"x","text":"X"},{"id":"y","text":"Y"}],
		"correct_choice":"x","grading_prompt":"attacker rubric"
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/admin/problems", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	assertApprovedRuntimePreserved(t, repo.problems["approved"])
	if repo.problems["approved"].Title != "Original title" {
		t.Fatalf("catalog-managed metadata changed: %+v", repo.problems["approved"])
	}
}

func TestAdminUpdateCannotOverwriteApprovedRuntimeFields(t *testing.T) {
	repo := newMockRepo()
	repo.problems["approved"] = approvedRuntimeProblem()
	r := setupAdminRouter(repo, &mockLoader{}, &mockSession{})
	body := `{
		"title":"Updated title","timeout_minutes":99,
		"verify_type":"script","base_image":"attacker-base:latest","image":"attacker:latest",
		"choices":[{"id":"x","text":"X"},{"id":"y","text":"Y"}],
		"correct_choice":"x","grading_prompt":"attacker rubric"
	}`
	req := httptest.NewRequest(http.MethodPut, "/api/admin/problems/approved", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	assertApprovedRuntimePreserved(t, repo.problems["approved"])
	if repo.problems["approved"].Title != "Original title" {
		t.Fatalf("catalog-managed metadata changed: %+v", repo.problems["approved"])
	}
}

func TestAdminDeleteRejectsCatalogManagedProblem(t *testing.T) {
	repo := newMockRepo()
	repo.problems["approved"] = approvedRuntimeProblem()
	r := setupAdminRouter(repo, &mockLoader{}, &mockSession{})
	req := httptest.NewRequest(http.MethodDelete, "/api/admin/problems/approved", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	if _, found := repo.problems["approved"]; !found {
		t.Fatal("catalog-managed problem was deleted")
	}
}

// SESS-3: the Start handler maps session sentinel errors to 429.
func TestStartProblemCapacity429(t *testing.T) {
	sess := &mockSession{startErr: session.ErrTooManySessions}
	r := setupTestRouter(newMockRepo(), &mockLoader{}, sess)

	req := httptest.NewRequest("POST", "/api/problems/p1/start", nil)
	req.Header.Set("Idempotency-Key", "operation-capacity-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 at capacity, got %d", w.Code)
	}
	var body map[string]string
	json.Unmarshal(w.Body.Bytes(), &body)
	if body["error"] != "server is at capacity; try again later" {
		t.Errorf("unexpected error message: %q", body["error"])
	}
}

func TestStartProblemBusy429(t *testing.T) {
	sess := &mockSession{startErr: session.ErrTransitionBusy}
	r := setupTestRouter(newMockRepo(), &mockLoader{}, sess)
	req := httptest.NewRequest(http.MethodPost, "/api/problems/p1/start", nil)
	req.Header.Set("Idempotency-Key", "operation-busy-12345678")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("busy start status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestStartProblemCatalogAdmissionUnavailable503(t *testing.T) {
	sess := &mockSession{startErr: errors.Join(errors.New("resolve active problem"), ErrCatalogAdmissionUnavailable)}
	r := setupTestRouter(newMockRepo(), &mockLoader{}, sess)
	req := httptest.NewRequest(http.MethodPost, "/api/problems/p1/start", nil)
	req.Header.Set("Idempotency-Key", "operation-catalog-unavailable")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("catalog admission status=%d body=%s, want 503", w.Code, w.Body.String())
	}
}

func TestStartProblemCooldown429(t *testing.T) {
	sess := &mockSession{startErr: session.ErrStartCooldown}
	r := setupTestRouter(newMockRepo(), &mockLoader{}, sess)

	req := httptest.NewRequest("POST", "/api/problems/p1/start", nil)
	req.Header.Set("Idempotency-Key", "operation-cooldown-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 during cooldown, got %d", w.Code)
	}
	var body map[string]string
	json.Unmarshal(w.Body.Bytes(), &body)
	if body["error"] != "starting too fast; wait a few seconds and retry" {
		t.Errorf("unexpected error message: %q", body["error"])
	}
}

// SEC3-4: verify throttle maps to 429.
func TestVerifyTooFast429(t *testing.T) {
	sess := &mockSession{verifyErr: session.ErrVerifyTooFast}
	r := setupTestRouter(newMockRepo(), &mockLoader{}, sess)

	req := httptest.NewRequest("POST", "/api/problems/p1/verify", nil)
	req.Header.Set("Idempotency-Key", "verify-operation-3")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d", w.Code)
	}
	var body map[string]string
	json.Unmarshal(w.Body.Bytes(), &body)
	if body["error"] != "verifying too fast" {
		t.Errorf("unexpected error message: %q", body["error"])
	}
}

func TestVerifyTransitionBusy429(t *testing.T) {
	sess := &mockSession{verifyErr: session.ErrTransitionBusy}
	r := setupTestRouter(newMockRepo(), &mockLoader{}, sess)
	req := httptest.NewRequest(http.MethodPost, "/api/problems/p1/verify", nil)
	req.Header.Set("Idempotency-Key", "verify-operation-busy")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d", w.Code)
	}
}

func TestVerifyInfrastructureFailureAcceptsColonKeyAndReturns503(t *testing.T) {
	const operationID = "verify:user-1:operation-4"
	sess := &mockSession{verifyErr: session.ErrVerifyInfrastructure}
	r := setupTestRouter(newMockRepo(), &mockLoader{}, sess)
	req := httptest.NewRequest(http.MethodPost, "/api/problems/p1/verify", nil)
	req.Header.Set("Idempotency-Key", operationID)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("infrastructure verify status=%d body=%s", w.Code, w.Body.String())
	}
	if sess.verifyOp != operationID {
		t.Fatalf("colon operation key reached service as %q", sess.verifyOp)
	}
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["error"] != "verification service was unavailable; start a new verification" {
		t.Fatalf("unexpected infrastructure error body: %q", body["error"])
	}
}

func TestSubmitChoiceTransitionBusy429(t *testing.T) {
	sess := &mockSession{choiceErr: session.ErrTransitionBusy}
	r := setupTestRouter(newMockRepo(), &mockLoader{}, sess)
	req := httptest.NewRequest(http.MethodPost, "/api/problems/p1/submit", strings.NewReader(`{"session_id":"session-1","generation":1,"choice_id":"b"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "choice-operation-busy")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d", w.Code)
	}
}

// SEC3-5: reset cooldown maps to 429.
func TestResetTooFast429(t *testing.T) {
	sess := &mockSession{resetErr: session.ErrStartCooldown}
	r := setupTestRouter(newMockRepo(), &mockLoader{}, sess)

	req := httptest.NewRequest("POST", "/api/problems/p1/reset", strings.NewReader(`{"session_id":"session-abc","source_generation":1}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "reset-operation-cooldown")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d", w.Code)
	}
}

func TestResetCleanupPendingReturnsAcceptedOperationSnapshot(t *testing.T) {
	timeoutAt := time.Now().Add(20 * time.Minute).Truncate(time.Second)
	sess := &mockSession{
		resetErr: session.ErrCleanupPending,
		resetResult: &session.CurrentSession{
			OperationID: "durable-reset-operation", SessionID: "session-abc", ProblemID: "p1",
			Generation: 2, Status: session.StatusCreating, TimeoutAt: timeoutAt, CleanupPending: true,
		},
	}
	r := setupTestRouter(newMockRepo(), &mockLoader{}, sess)
	req := httptest.NewRequest(http.MethodPost, "/api/problems/p1/reset", strings.NewReader(`{"session_id":"session-abc","source_generation":1}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "reset-operation-pending-12345678")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("pending reset status=%d body=%s", w.Code, w.Body.String())
	}
	var body session.CurrentSession
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.RequestID != "reset-operation-pending-12345678" || body.OperationID != "durable-reset-operation" ||
		body.SessionID != "session-abc" || body.Generation != 2 || body.Status != session.StatusCreating ||
		!body.CleanupPending || !body.TimeoutAt.Equal(timeoutAt) {
		t.Fatalf("pending reset response=%+v", body)
	}
}

func TestResetDuringShutdown503(t *testing.T) {
	sess := &mockSession{resetErr: session.ErrServiceStopping}
	r := setupTestRouter(newMockRepo(), &mockLoader{}, sess)

	req := httptest.NewRequest("POST", "/api/problems/p1/reset", strings.NewReader(`{"session_id":"session-abc","source_generation":1}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "reset-operation-shutdown")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 during shutdown, got %d", w.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["error"] != "server is shutting down" {
		t.Fatalf("unexpected error message: %q", body["error"])
	}
}

func TestResetRequiresValidIdempotencyKeyBeforeServiceMutation(t *testing.T) {
	for _, key := range []string{"", "short", "contains space"} {
		t.Run(key, func(t *testing.T) {
			sess := &mockSession{resetResult: &session.CurrentSession{SessionID: "must-not-return"}}
			r := setupTestRouter(newMockRepo(), &mockLoader{}, sess)
			req := httptest.NewRequest(http.MethodPost, "/api/problems/p1/reset", strings.NewReader(`{"session_id":"session-abc","source_generation":1}`))
			req.Header.Set("Content-Type", "application/json")
			if key != "" {
				req.Header.Set("Idempotency-Key", key)
			}
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("key %q status=%d, want 400", key, w.Code)
			}
			if sess.resetOp != "" {
				t.Fatalf("key %q reached reset service as %q", key, sess.resetOp)
			}
		})
	}
}

func TestResetReturnsOperationBoundReplacementSnapshot(t *testing.T) {
	timeoutAt := time.Now().Add(20 * time.Minute).Truncate(time.Second)
	sess := &mockSession{resetResult: &session.CurrentSession{
		OperationID: "reset-operation-12345678", SessionID: "session-abc", ProblemID: "p1",
		Generation: 2, Status: session.StatusBooting, TimeoutAt: timeoutAt,
	}}
	r := setupTestRouter(newMockRepo(), &mockLoader{}, sess)
	req := httptest.NewRequest(http.MethodPost, "/api/problems/p1/reset", strings.NewReader(`{"session_id":"session-abc","source_generation":1}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "reset-operation-12345678")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("reset status=%d body=%s", w.Code, w.Body.String())
	}
	var body session.CurrentSession
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.RequestID != "reset-operation-12345678" || body.OperationID != "reset-operation-12345678" || body.SessionID != "session-abc" || body.Generation != 2 ||
		body.ProblemID != "p1" || !body.TimeoutAt.Equal(timeoutAt) {
		t.Fatalf("reset snapshot=%+v", body)
	}
	if sess.resetOp != "reset-operation-12345678" {
		t.Fatalf("reset operation forwarded as %q", sess.resetOp)
	}
	if sess.resetProblem != "p1" {
		t.Fatalf("reset problem forwarded as %q", sess.resetProblem)
	}
	if sess.resetExpected != (runner.SessionRef{SessionID: "session-abc", Generation: 1}) {
		t.Fatalf("reset precondition forwarded as %+v", sess.resetExpected)
	}
}

func TestResetRequiresSourcePreconditionBeforeServiceMutation(t *testing.T) {
	for _, body := range []string{"", `{}`, `{"session_id":"session-abc"}`, `{"source_generation":1}`, `{"session_id":"session-abc","source_generation":0}`} {
		t.Run(body, func(t *testing.T) {
			sess := &mockSession{}
			r := setupTestRouter(newMockRepo(), &mockLoader{}, sess)
			req := httptest.NewRequest(http.MethodPost, "/api/problems/p1/reset", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Idempotency-Key", "reset-operation-12345678")
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("body %q status=%d, want 400", body, w.Code)
			}
			if sess.resetOp != "" {
				t.Fatalf("body %q reached reset service", body)
			}
		})
	}
}
