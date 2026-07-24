package session

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/k8s-quiz/backend/internal/container"
	"github.com/k8s-quiz/backend/pkg/models"
)

type mockContainerManager struct {
	mu         sync.Mutex
	created    []container.CreateOpts
	removed    []string
	execResult container.ExecResult
	execErr    error
	readyAfter int
	readyCalls int
	running    bool
}

func (m *mockContainerManager) Create(ctx context.Context, opts container.CreateOpts) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.created = append(m.created, opts)
	return "container-123", nil
}

func (m *mockContainerManager) Exec(ctx context.Context, containerID string, cmd []string) (container.ExecResult, error) {
	return m.execResult, m.execErr
}

func (m *mockContainerManager) ExecInteractive(ctx context.Context, containerID string, cmd []string) (container.TerminalSession, error) {
	return nil, nil
}

func (m *mockContainerManager) Remove(ctx context.Context, containerID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.removed = append(m.removed, containerID)
	return nil
}

func (m *mockContainerManager) Logs(ctx context.Context, containerID string) (string, error) {
	return "", nil
}

func (m *mockContainerManager) WaitReady(ctx context.Context, containerID string, check func() bool, timeout time.Duration) error {
	return nil
}

func (m *mockContainerManager) IsRunning(ctx context.Context, containerID string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.running, nil
}

type mockProblemStore struct {
	mu       sync.Mutex
	problems map[string]*models.Problem
	attempts map[string]*models.Attempt
	nextID   int
}

func newMockProblemStore() *mockProblemStore {
	return &mockProblemStore{
		problems: make(map[string]*models.Problem),
		attempts: make(map[string]*models.Attempt),
		nextID:   1,
	}
}

func (m *mockProblemStore) FindByID(ctx context.Context, id string) (*models.Problem, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.problems[id]
	if !ok {
		return nil, context.DeadlineExceeded
	}
	return p, nil
}

func (m *mockProblemStore) CreateAttempt(ctx context.Context, a *models.Attempt) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	a.ID = "attempt-" + string(rune('0'+m.nextID))
	m.nextID++
	m.attempts[a.ID] = a
	return nil
}

func (m *mockProblemStore) UpdateAttempt(ctx context.Context, a *models.Attempt) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.attempts[a.ID] = a
	return nil
}

func (m *mockProblemStore) GetAttempt(ctx context.Context, id string) (*models.Attempt, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.attempts[id]
	if !ok {
		return nil, context.DeadlineExceeded
	}
	return a, nil
}

type mockScriptProvider struct{}

func (m *mockScriptProvider) HasSetupScript(problemID string) bool            { return false }
func (m *mockScriptProvider) HasVerifyScript(problemID string) bool           { return true }
func (m *mockScriptProvider) GetSetupScript(problemID string) (string, error) { return "", nil }
func (m *mockScriptProvider) GetVerifyScript(problemID string) (string, error) {
	return "echo ok", nil
}

func newTestService() (*Service, *mockContainerManager, *mockProblemStore) {
	mgr := &mockContainerManager{execResult: container.ExecResult{ExitCode: 0, Stdout: "ok"}, running: true}
	store := newMockProblemStore()
	svc := NewService(mgr, store, &mockScriptProvider{})
	return svc, mgr, store
}

func TestStartProblem(t *testing.T) {
	svc, mgr, store := newTestService()
	store.problems["test-problem"] = &models.Problem{
		ID:             "test-problem",
		Title:          "Test",
		TimeoutMinutes: 30,
		BaseImage:      "k3s-base:latest",
		VerifyType:     "script",
	}

	sessionID, err := svc.StartProblem(context.Background(), "user-1", "test-problem")
	if err != nil {
		t.Fatalf("StartProblem failed: %v", err)
	}
	if sessionID == "" {
		t.Fatal("expected non-empty session ID")
	}

	time.Sleep(100 * time.Millisecond)

	sess := svc.GetSession("user-1")
	if sess == nil {
		t.Fatal("expected active session")
	}
	if sess.ProblemID != "test-problem" {
		t.Errorf("expected problem ID test-problem, got %s", sess.ProblemID)
	}

	mgr.mu.Lock()
	if len(mgr.created) != 1 {
		t.Errorf("expected 1 container created, got %d", len(mgr.created))
	}
	mgr.mu.Unlock()
}

func TestStartProblemNotFound(t *testing.T) {
	svc, _, _ := newTestService()
	_, err := svc.StartProblem(context.Background(), "user-1", "nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent problem")
	}
}

func TestDuplicateSessionReplacesOld(t *testing.T) {
	svc, mgr, store := newTestService()
	store.problems["p1"] = &models.Problem{ID: "p1", TimeoutMinutes: 30, BaseImage: "k3s-base:latest", VerifyType: "script"}
	store.problems["p2"] = &models.Problem{ID: "p2", TimeoutMinutes: 30, BaseImage: "k3s-base:latest", VerifyType: "script"}

	svc.StartProblem(context.Background(), "user-1", "p1")
	time.Sleep(50 * time.Millisecond)

	svc.StartProblem(context.Background(), "user-1", "p2")
	time.Sleep(50 * time.Millisecond)

	sess := svc.GetSession("user-1")
	if sess == nil {
		t.Fatal("expected active session")
	}
	if sess.ProblemID != "p2" {
		t.Errorf("expected problem p2, got %s", sess.ProblemID)
	}

	mgr.mu.Lock()
	if len(mgr.removed) < 1 {
		t.Error("expected old container to be removed")
	}
	mgr.mu.Unlock()
}

func TestEndSession(t *testing.T) {
	svc, mgr, store := newTestService()
	store.problems["p1"] = &models.Problem{ID: "p1", TimeoutMinutes: 30, BaseImage: "k3s-base:latest", VerifyType: "script"}

	svc.StartProblem(context.Background(), "user-1", "p1")
	time.Sleep(50 * time.Millisecond)

	svc.EndSession(context.Background(), "user-1")

	if svc.GetSession("user-1") != nil {
		t.Error("expected no session after end")
	}

	mgr.mu.Lock()
	if len(mgr.removed) != 1 {
		t.Errorf("expected 1 removal, got %d", len(mgr.removed))
	}
	mgr.mu.Unlock()
}

func TestVerifySuccess(t *testing.T) {
	svc, mgr, store := newTestService()
	store.problems["p1"] = &models.Problem{ID: "p1", TimeoutMinutes: 30, BaseImage: "k3s-base:latest", VerifyType: "script"}
	mgr.execResult = container.ExecResult{ExitCode: 0, Stdout: "SUCCESS"}

	svc.StartProblem(context.Background(), "user-1", "p1")
	time.Sleep(100 * time.Millisecond)

	success, log, err := svc.Verify(context.Background(), "user-1")
	if err != nil {
		t.Fatalf("Verify failed: %v", err)
	}
	if !success {
		t.Error("expected verify success")
	}
	if log != "SUCCESS" {
		t.Errorf("expected log SUCCESS, got %s", log)
	}
}

func TestVerifyFailure(t *testing.T) {
	svc, mgr, store := newTestService()
	store.problems["p1"] = &models.Problem{ID: "p1", TimeoutMinutes: 30, BaseImage: "k3s-base:latest", VerifyType: "script"}
	mgr.execResult = container.ExecResult{ExitCode: 1, Stdout: "FAIL"}

	svc.StartProblem(context.Background(), "user-1", "p1")
	time.Sleep(100 * time.Millisecond)

	success, _, err := svc.Verify(context.Background(), "user-1")
	if err != nil {
		t.Fatalf("Verify failed: %v", err)
	}
	if success {
		t.Error("expected verify failure")
	}
}

func TestVerifyNoSession(t *testing.T) {
	svc, _, _ := newTestService()
	_, _, err := svc.Verify(context.Background(), "user-1")
	if err == nil {
		t.Fatal("expected error for no session")
	}
}

func TestVerifyCallback(t *testing.T) {
	svc, mgr, store := newTestService()
	store.problems["p1"] = &models.Problem{ID: "p1", TimeoutMinutes: 30, BaseImage: "k3s-base:latest", VerifyType: "script"}
	mgr.execResult = container.ExecResult{ExitCode: 0, Stdout: "ok"}

	var callbackCalled bool
	var callbackSuccess bool
	svc.SetVerifyCallback(func(userID string, success bool, log string) {
		callbackCalled = true
		callbackSuccess = success
	})

	svc.StartProblem(context.Background(), "user-1", "p1")
	time.Sleep(100 * time.Millisecond)
	svc.Verify(context.Background(), "user-1")

	if !callbackCalled {
		t.Error("expected verify callback to be called")
	}
	if !callbackSuccess {
		t.Error("expected callback success=true")
	}
}

func TestSubmitChoice(t *testing.T) {
	svc, _, store := newTestService()
	store.problems["p1"] = &models.Problem{
		ID:             "p1",
		TimeoutMinutes: 30,
		BaseImage:      "k3s-base:latest",
		VerifyType:     "choice",
		CorrectChoice:  "b",
	}

	svc.StartProblem(context.Background(), "user-1", "p1")
	time.Sleep(50 * time.Millisecond)

	success, err := svc.SubmitChoice(context.Background(), "user-1", "b")
	if err != nil {
		t.Fatalf("SubmitChoice failed: %v", err)
	}
	if !success {
		t.Error("expected correct choice")
	}

	success, err = svc.SubmitChoice(context.Background(), "user-1", "a")
	if err != nil {
		t.Fatalf("SubmitChoice failed: %v", err)
	}
	if success {
		t.Error("expected incorrect choice")
	}
}

func TestCleanupAll(t *testing.T) {
	svc, mgr, store := newTestService()
	store.problems["p1"] = &models.Problem{ID: "p1", TimeoutMinutes: 30, BaseImage: "k3s-base:latest", VerifyType: "script"}
	store.problems["p2"] = &models.Problem{ID: "p2", TimeoutMinutes: 30, BaseImage: "k3s-base:latest", VerifyType: "script"}

	svc.StartProblem(context.Background(), "user-1", "p1")
	svc.StartProblem(context.Background(), "user-2", "p2")
	time.Sleep(50 * time.Millisecond)

	svc.CleanupAll(context.Background())

	if svc.GetSession("user-1") != nil || svc.GetSession("user-2") != nil {
		t.Error("expected all sessions cleaned up")
	}

	mgr.mu.Lock()
	if len(mgr.removed) < 2 {
		t.Errorf("expected at least 2 removals, got %d", len(mgr.removed))
	}
	mgr.mu.Unlock()
}

func TestCheckCrashes(t *testing.T) {
	svc, mgr, store := newTestService()
	store.problems["p1"] = &models.Problem{ID: "p1", TimeoutMinutes: 30, BaseImage: "k3s-base:latest", VerifyType: "script"}

	svc.StartProblem(context.Background(), "user-1", "p1")
	time.Sleep(100 * time.Millisecond)

	if crashed := svc.checkCrashes(); len(crashed) != 0 {
		t.Errorf("expected no crash while running, got %v", crashed)
	}

	mgr.mu.Lock()
	mgr.running = false
	mgr.mu.Unlock()

	crashed := svc.checkCrashes()
	if len(crashed) != 1 || crashed[0] != "user-1" {
		t.Fatalf("expected user-1 crashed, got %v", crashed)
	}

	sess := svc.GetSession("user-1")
	sess.mu.Lock()
	status := sess.Status
	sess.mu.Unlock()
	if status != StatusFailed {
		t.Errorf("expected status failed after crash, got %s", status)
	}
}

type mockGrader struct {
	success bool
	log     string
	err     error
}

func (m *mockGrader) Grade(ctx context.Context, rubric, evidence string) (bool, string, error) {
	return m.success, m.log, m.err
}

func TestVerifyTextGrading(t *testing.T) {
	svc, mgr, store := newTestService()
	store.problems["p1"] = &models.Problem{ID: "p1", TimeoutMinutes: 30, BaseImage: "k3s-base:latest", VerifyType: "text", GradingPrompt: "check pods are running"}
	mgr.execResult = container.ExecResult{ExitCode: 0, Stdout: "pod running"}
	svc.SetGrader(&mockGrader{success: true, log: "PASS: solved"})

	svc.StartProblem(context.Background(), "user-1", "p1")
	time.Sleep(100 * time.Millisecond)

	success, log, err := svc.Verify(context.Background(), "user-1")
	if err != nil {
		t.Fatalf("Verify failed: %v", err)
	}
	if !success {
		t.Error("expected text grading success")
	}
	if log != "PASS: solved" {
		t.Errorf("expected grader log, got %s", log)
	}
}

func TestVerifyTextNoGrader(t *testing.T) {
	svc, _, store := newTestService()
	store.problems["p1"] = &models.Problem{ID: "p1", TimeoutMinutes: 30, BaseImage: "k3s-base:latest", VerifyType: "text", GradingPrompt: "check"}

	svc.StartProblem(context.Background(), "user-1", "p1")
	time.Sleep(100 * time.Millisecond)

	_, _, err := svc.Verify(context.Background(), "user-1")
	if err == nil {
		t.Fatal("expected error when grader not configured")
	}
}

func attemptStatus(t *testing.T, store *mockProblemStore, id string) string {
	t.Helper()
	store.mu.Lock()
	defer store.mu.Unlock()
	a, ok := store.attempts[id]
	if !ok || a == nil {
		t.Fatalf("attempt %s missing", id)
	}
	return a.Status
}

// STATE-1: the timeout path must persist attempt status "timeout", not "failed".
func TestCheckTimeoutsWritesTimeoutAttempt(t *testing.T) {
	svc, mgr, store := newTestService()
	store.problems["p1"] = &models.Problem{ID: "p1", TimeoutMinutes: 30, BaseImage: "k3s-base:latest", VerifyType: "script"}

	svc.StartProblem(context.Background(), "user-1", "p1")
	time.Sleep(100 * time.Millisecond)

	sess := svc.GetSession("user-1")
	if sess == nil {
		t.Fatal("expected active session")
	}
	sess.mu.Lock()
	sess.TimeoutAt = time.Now().Add(-time.Second)
	attemptID := sess.AttemptID
	sess.mu.Unlock()

	expired := svc.checkTimeouts()
	if len(expired) != 1 || expired[0] != "user-1" {
		t.Fatalf("expected user-1 expired, got %v", expired)
	}
	if svc.GetSession("user-1") != nil {
		t.Error("expected session removed after timeout")
	}
	if got := attemptStatus(t, store, attemptID); got != "timeout" {
		t.Errorf("expected attempt status timeout, got %s", got)
	}
	mgr.mu.Lock()
	removed := len(mgr.removed)
	mgr.mu.Unlock()
	if removed != 1 {
		t.Errorf("expected 1 container removal, got %d", removed)
	}
}

func TestCheckTimeoutsSkipsCompleted(t *testing.T) {
	svc, _, store := newTestService()
	store.problems["p1"] = &models.Problem{ID: "p1", TimeoutMinutes: 30, BaseImage: "k3s-base:latest", VerifyType: "script"}

	svc.StartProblem(context.Background(), "user-1", "p1")
	time.Sleep(100 * time.Millisecond)

	sess := svc.GetSession("user-1")
	sess.mu.Lock()
	sess.TimeoutAt = time.Now().Add(-time.Second)
	sess.Status = StatusCompleted
	sess.mu.Unlock()

	if expired := svc.checkTimeouts(); len(expired) != 0 {
		t.Errorf("completed session must not be expired, got %v", expired)
	}
	if svc.GetSession("user-1") == nil {
		t.Error("completed session must survive the timeout scan")
	}
}

// User-initiated end keeps the "failed" semantics.
func TestEndSessionWritesFailedAttempt(t *testing.T) {
	svc, _, store := newTestService()
	store.problems["p1"] = &models.Problem{ID: "p1", TimeoutMinutes: 30, BaseImage: "k3s-base:latest", VerifyType: "script"}

	svc.StartProblem(context.Background(), "user-1", "p1")
	time.Sleep(100 * time.Millisecond)

	attemptID := svc.GetSession("user-1").AttemptID
	svc.EndSession(context.Background(), "user-1")

	if got := attemptStatus(t, store, attemptID); got != "failed" {
		t.Errorf("expected attempt status failed after user end, got %s", got)
	}
}

// Start-replacement of an existing session keeps the "failed" semantics.
func TestStartReplacementMarksOldAttemptFailed(t *testing.T) {
	svc, _, store := newTestService()
	store.problems["p1"] = &models.Problem{ID: "p1", TimeoutMinutes: 30, BaseImage: "k3s-base:latest", VerifyType: "script"}
	store.problems["p2"] = &models.Problem{ID: "p2", TimeoutMinutes: 30, BaseImage: "k3s-base:latest", VerifyType: "script"}

	svc.StartProblem(context.Background(), "user-1", "p1")
	time.Sleep(50 * time.Millisecond)
	firstAttemptID := svc.GetSession("user-1").AttemptID

	svc.StartProblem(context.Background(), "user-1", "p2")
	time.Sleep(50 * time.Millisecond)

	if got := attemptStatus(t, store, firstAttemptID); got != "failed" {
		t.Errorf("expected replaced attempt status failed, got %s", got)
	}
}

// GetCurrentSession returns the 4-field snapshot used by /sessions/current and
// /problems/:id/start.
func TestGetCurrentSession(t *testing.T) {
	svc, _, store := newTestService()
	store.problems["p1"] = &models.Problem{ID: "p1", TimeoutMinutes: 30, BaseImage: "k3s-base:latest", VerifyType: "script"}

	if cur := svc.GetCurrentSession("user-1"); cur != nil {
		t.Fatalf("expected nil without session, got %+v", cur)
	}

	svc.StartProblem(context.Background(), "user-1", "p1")
	time.Sleep(50 * time.Millisecond)

	cur := svc.GetCurrentSession("user-1")
	if cur == nil {
		t.Fatal("expected session snapshot")
	}
	if cur.SessionID == "" || cur.ProblemID != "p1" || cur.Status == "" || cur.TimeoutAt.IsZero() {
		t.Errorf("incomplete snapshot: %+v", cur)
	}
}
