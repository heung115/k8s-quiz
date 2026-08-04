package session

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/k8s-quiz/backend/internal/runner"
	"github.com/k8s-quiz/backend/pkg/models"
)

type mockRunner struct {
	mu                   sync.Mutex
	created              []runner.CreateSessionRequest
	removed              []runner.AllocationRef
	verifyResult         runner.VerifyResult
	verifyResultMutator  func(*runner.VerifyResult)
	verifyErr            error
	waitReadyErr         error
	setupErr             error
	setupGates           map[uint64]chan struct{}
	setupEntered         chan runner.AllocationRef
	setupRequests        []runner.SetupSessionRequest
	setupResults         map[string]error
	setupEffects         int
	createEntered        chan runner.CreateSessionRequest
	running              bool
	createGate           chan struct{} // if set, CreateSession blocks until closed (T3)
	createIgnoresContext bool
	observeEntered       chan runner.AllocationRef
	observeGate          chan struct{}
	verifyEntered        chan runner.AllocationRef
	verifyGate           chan struct{}
	verifyKeys           []string
	verifyRequests       []runner.VerifyRequest
	destroyEntered       chan runner.AllocationRef
	destroyGate          chan struct{}
	destroyErr           error
	createErr            error
	createRef            *runner.AllocationRef
	nextID               int
}

func (m *mockRunner) Kind() runner.ProviderKind { return runner.ProviderLocalDocker }

func (m *mockRunner) CreateSession(ctx context.Context, req runner.CreateSessionRequest) (runner.AllocationRef, error) {
	if m.createEntered != nil {
		select {
		case m.createEntered <- req:
		default:
		}
	}
	if m.createGate != nil {
		if m.createIgnoresContext {
			<-m.createGate
		} else {
			select {
			case <-m.createGate:
			case <-ctx.Done():
				return runner.AllocationRef{}, ctx.Err()
			}
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.created = append(m.created, req)
	if m.createErr != nil {
		err := m.createErr
		m.createErr = nil
		return runner.AllocationRef{}, err
	}
	if m.createRef != nil {
		return *m.createRef, nil
	}
	m.nextID++
	return runner.AllocationRef{
		ID:       req.AllocationID,
		Session:  req.Session,
		Provider: runner.ProviderLocalDocker,
	}, nil
}

func (m *mockRunner) WaitReady(context.Context, runner.AllocationRef, time.Duration) error {
	return m.waitReadyErr
}

func (m *mockRunner) SetupSession(ctx context.Context, request runner.SetupSessionRequest) error {
	ref := request.Allocation
	m.mu.Lock()
	if m.setupResults == nil {
		m.setupResults = make(map[string]error)
	}
	if prior, replay := m.setupResults[request.IdempotencyKey]; replay {
		m.setupRequests = append(m.setupRequests, request)
		m.mu.Unlock()
		return prior
	}
	err := m.setupErr
	gate := m.setupGates[ref.Session.Generation]
	entered := m.setupEntered
	m.setupRequests = append(m.setupRequests, request)
	m.setupEffects++
	m.setupResults[request.IdempotencyKey] = err
	m.mu.Unlock()
	if entered != nil {
		select {
		case entered <- ref:
		default:
		}
	}
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return err
}

func (m *mockRunner) OpenTerminal(context.Context, runner.OpenTerminalRequest) (runner.TerminalSession, error) {
	return nil, io.EOF
}

func (m *mockRunner) VerifySession(ctx context.Context, request runner.VerifyRequest) (runner.VerifyResult, error) {
	m.mu.Lock()
	entered := m.verifyEntered
	gate := m.verifyGate
	m.verifyKeys = append(m.verifyKeys, request.IdempotencyKey)
	m.verifyRequests = append(m.verifyRequests, request)
	m.mu.Unlock()
	if entered != nil {
		select {
		case entered <- request.Allocation:
		default:
		}
	}
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return runner.VerifyResult{}, ctx.Err()
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.verifyErr != nil {
		return runner.VerifyResult{}, m.verifyErr
	}
	result := mockVerificationResult(ctx, request, m.verifyResult)
	if m.verifyResultMutator != nil {
		m.verifyResultMutator(&result)
	}
	return result, nil
}

func mockVerificationResult(ctx context.Context, request runner.VerifyRequest, result runner.VerifyResult) runner.VerifyResult {
	feedback, err := runner.PublicFeedbackForStatus(result.Status)
	if err != nil {
		return result
	}
	finishedAt := time.Now().UTC()
	startedAt := finishedAt.Add(-time.Millisecond)
	fence, _ := runner.ControllerFenceFromContext(ctx)
	result.Feedback = feedback
	result.Receipt = runner.VerificationReceipt{
		Schema: runner.VerificationReceiptSchema, OperationID: request.IdempotencyKey,
		Allocation: request.Allocation, Problem: request.Problem, Deadline: request.Deadline,
		Status: result.Status, Feedback: feedback,
		EvidenceDigest:         runner.VerifyEvidenceDigest(result.Evidence),
		VerifierArtifactDigest: "sha256:" + strings.Repeat("a", 64),
		VerifierExecutionID:    "mock-verifier-execution", StartedAt: startedAt, FinishedAt: finishedAt,
		Assurance: runner.VerifyAssuranceDevelopmentGuest, ControllerFence: fence,
	}
	return result
}

func (m *mockRunner) GetSession(ctx context.Context, ref runner.AllocationRef) (runner.Observation, error) {
	m.mu.Lock()
	entered := m.observeEntered
	gate := m.observeGate
	running := m.running
	m.mu.Unlock()
	if entered != nil {
		select {
		case entered <- ref:
		default:
		}
	}
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return runner.Observation{}, ctx.Err()
		}
	}
	state := runner.ObservedStopped
	if running {
		state = runner.ObservedRunning
	}
	return runner.Observation{Allocation: ref, State: state}, nil
}

func waitForStatus(t *testing.T, svc *Service, userID string, want Status) *Session {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		sess := svc.GetSession(userID)
		if sess != nil {
			sess.mu.Lock()
			status := sess.Status
			sess.mu.Unlock()
			if status == want {
				return sess
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("session %s did not reach status %s", userID, want)
	return nil
}

func (m *mockRunner) DestroySession(ctx context.Context, ref runner.AllocationRef) error {
	if m.destroyEntered != nil {
		select {
		case m.destroyEntered <- ref:
		default:
		}
	}
	if m.destroyGate != nil {
		select {
		case <-m.destroyGate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.removed = append(m.removed, ref)
	if m.destroyErr != nil {
		err := m.destroyErr
		m.destroyErr = nil
		return err
	}
	return nil
}

func (m *mockRunner) Reconcile(context.Context, runner.ReconcileRequest) (runner.ReconcileResult, error) {
	return runner.ReconcileResult{}, nil
}

type mockProblemStore struct {
	mu        sync.Mutex
	problems  map[string]*models.Problem
	attempts  map[string]*models.Attempt
	updateErr error
	nextID    int
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
	if p.Revision == "" {
		p.Revision = "revision-" + id
	}
	return p, nil
}

func (m *mockProblemStore) ResolveProblem(ctx context.Context, ref runner.ProblemRef) (models.Problem, error) {
	p, err := m.FindByID(ctx, ref.ID)
	if err != nil {
		return models.Problem{}, err
	}
	if ref.Revision == "" || p.Revision != ref.Revision {
		return models.Problem{}, runner.ErrInvalidRevision
	}
	return *p, nil
}

func (m *mockProblemStore) AcquireActiveProblem(ctx context.Context, id string) (models.Problem, runner.CatalogSelection, func(), error) {
	p, err := m.FindByID(ctx, id)
	if err != nil {
		return models.Problem{}, runner.CatalogSelection{}, nil, err
	}
	copy := *p
	return copy, runner.CatalogSelection{
		Generation: 1,
		Problem:    runner.ProblemRef{ID: copy.ID, Revision: copy.Revision},
	}, func() {}, nil
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
	if m.updateErr != nil {
		err := m.updateErr
		m.updateErr = nil
		return err
	}
	copy := *a
	m.attempts[a.ID] = &copy
	return nil
}

func (m *mockProblemStore) GetAttempt(ctx context.Context, id string) (*models.Attempt, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.attempts[id]
	if !ok {
		return nil, context.DeadlineExceeded
	}
	copy := *a
	return &copy, nil
}

func newTestService() (*Service, *mockRunner, *mockProblemStore) {
	mgr := &mockRunner{verifyResult: runner.VerifyResult{Status: runner.VerifyPassed}, running: true}
	store := newMockProblemStore()
	svc := NewService(mgr, store, store)
	// Tests start back-to-back; disable the SESS-3/SEC3-5 cooldown and the
	// SEC3-4 verify throttle unless a test re-enables them explicitly.
	svc.SetStartCooldown(0)
	svc.SetVerifyThrottle(0)
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

func TestPausedServiceRejectsAdmissionUntilActivated(t *testing.T) {
	mgr := &mockRunner{verifyResult: runner.VerifyResult{Status: runner.VerifyPassed}, running: true}
	store := newMockProblemStore()
	store.problems["p1"] = &models.Problem{ID: "p1", TimeoutMinutes: 30, VerifyType: "script"}
	svc := NewPausedService(mgr, store, store)

	if _, err := svc.StartProblem(context.Background(), "user-1", "p1"); !errors.Is(err, ErrServiceStopping) {
		t.Fatalf("paused start error = %v, want ErrServiceStopping", err)
	}
	if err := svc.ResetEnvironment(context.Background(), "user-1"); !errors.Is(err, ErrServiceStopping) {
		t.Fatalf("paused reset error = %v, want ErrServiceStopping", err)
	}
	if err := svc.Activate(); err != nil {
		t.Fatalf("activate paused service: %v", err)
	}
	if _, err := svc.StartProblem(context.Background(), "user-1", "p1"); err != nil {
		t.Fatalf("start after activation: %v", err)
	}
	svc.BeginShutdown()
	waitCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := svc.WaitForWatchers(waitCtx); err != nil {
		t.Fatalf("stop activated paused service: %v", err)
	}
}

func TestPausedServiceCannotActivateAfterShutdown(t *testing.T) {
	mgr := &mockRunner{verifyResult: runner.VerifyResult{Status: runner.VerifyPassed}, running: true}
	store := newMockProblemStore()
	svc := NewPausedService(mgr, store, store)
	svc.BeginShutdown()
	if err := svc.Activate(); !errors.Is(err, ErrServiceStopping) {
		t.Fatalf("activation after shutdown error = %v, want ErrServiceStopping", err)
	}
}

func TestAuthorityFenceRejectsAllSessionMutationsAndProviderCleanup(t *testing.T) {
	mgr := &mockRunner{verifyResult: runner.VerifyResult{Status: runner.VerifyPassed}, running: true}
	store := newMockProblemStore()
	store.problems["p1"] = &models.Problem{ID: "p1", Revision: "revision-p1", TimeoutMinutes: 30, VerifyType: "choice", CorrectChoice: "b"}
	gate := runner.NewAuthorityGate()
	authorized, err := runner.NewAuthorityRunner(mgr, gate, runner.ControllerFence{ProviderID: "local-docker:test", Epoch: 1})
	if err != nil {
		t.Fatal(err)
	}
	svc := NewPausedService(authorized, store, store)
	if err := svc.SetAuthorityGate(gate); err != nil {
		t.Fatal(err)
	}
	if err := gate.Activate(); err != nil {
		t.Fatal(err)
	}
	if err := svc.Activate(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		svc.BeginShutdown()
		waitCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = svc.WaitForWatchers(waitCtx)
	})
	ref := runner.AllocationRef{
		ID: "allocation-1", Session: runner.SessionRef{SessionID: "session-1", Generation: 1},
		Provider: runner.ProviderLocalDocker,
	}
	store.attempts["attempt-1"] = &models.Attempt{
		ID: "attempt-1", UserID: "user-1", ProblemID: "p1", Status: "in_progress", StartedAt: time.Now(),
	}
	svc.sessions["user-1"] = &Session{
		ID: "session-1", UserID: "user-1", ProblemID: "p1",
		Selection:  runner.CatalogSelection{Generation: 1, Problem: runner.ProblemRef{ID: "p1", Revision: "revision-p1"}},
		VerifyType: "choice", CorrectChoice: "b", AttemptID: "attempt-1", Generation: 1,
		Allocation: ref, Status: StatusReady, TimeoutAt: time.Now().Add(time.Hour),
	}

	gate.Fence(errors.New("lease lost"))
	if _, err := svc.StartProblem(context.Background(), "user-2", "p1"); !errors.Is(err, ErrServiceStopping) {
		t.Fatalf("start after fence error = %v", err)
	}
	if err := svc.ResetEnvironment(context.Background(), "user-1"); !errors.Is(err, ErrServiceStopping) {
		t.Fatalf("reset after fence error = %v", err)
	}
	if _, _, err := svc.Verify(context.Background(), "user-1", "verify-after-fence"); !errors.Is(err, ErrServiceStopping) {
		t.Fatalf("verify after fence error = %v", err)
	}
	if _, err := svc.SubmitChoice(context.Background(), "user-1", "p1", ref.Session, "choice-after-fence", "b"); !errors.Is(err, ErrServiceStopping) {
		t.Fatalf("choice after fence error = %v", err)
	}
	if err := svc.EndSession(context.Background(), "user-1"); !errors.Is(err, ErrServiceStopping) {
		t.Fatalf("end after fence error = %v", err)
	}
	if _, ok := svc.GetTerminalTarget("user-1", runner.SessionRef{SessionID: "session-1", Generation: 1}); ok {
		t.Fatal("terminal target remained available after fence")
	}
	if err := svc.CleanupAll(context.Background()); !errors.Is(err, runner.ErrControllerAuthorityUnavailable) {
		t.Fatalf("cleanup after fence error = %v", err)
	}
	mgr.mu.Lock()
	creates, destroys, verifies := len(mgr.created), len(mgr.removed), len(mgr.verifyKeys)
	mgr.mu.Unlock()
	if creates != 0 || destroys != 0 || verifies != 0 {
		t.Fatalf("provider mutated after fence: creates=%d destroys=%d verifies=%d", creates, destroys, verifies)
	}
}

func TestStartProblemNotFound(t *testing.T) {
	svc, _, _ := newTestService()
	_, err := svc.StartProblem(context.Background(), "user-1", "nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent problem")
	}
}

func TestAcquireTerminalCommitLinearizesUserTransition(t *testing.T) {
	svc, _, _ := newTestService()
	ref := runner.AllocationRef{
		ID:       "allocation-terminal-commit",
		Session:  runner.SessionRef{SessionID: "session-terminal-commit", Generation: 3},
		Provider: runner.ProviderLocalDocker,
	}
	svc.mu.Lock()
	svc.sessions["user-1"] = &Session{
		ID:         ref.Session.SessionID,
		UserID:     "user-1",
		Generation: ref.Session.Generation,
		Allocation: ref,
		Status:     StatusReady,
		TimeoutAt:  time.Now().Add(time.Hour),
	}
	svc.mu.Unlock()

	release, ok := svc.AcquireTerminalCommit(context.Background(), "user-1", ref)
	if !ok || release == nil {
		t.Fatal("exact ready allocation did not acquire a terminal commit lease")
	}
	lock := svc.userLock("user-1")
	if lock.TryLock() {
		lock.Unlock()
		t.Fatal("terminal commit lease did not hold the user transition lock")
	}

	transitionStarted := make(chan struct{})
	transitionAcquired := make(chan struct{})
	transitionRelease := make(chan struct{})
	transitionDone := make(chan struct{})
	go func() {
		defer close(transitionDone)
		close(transitionStarted)
		lock.Lock()
		close(transitionAcquired)
		<-transitionRelease
		lock.Unlock()
	}()
	<-transitionStarted
	select {
	case <-transitionAcquired:
		t.Fatal("user transition crossed a held terminal commit lease")
	default:
	}

	// Release is safe even when deferred cleanup and an explicit error path race.
	var releases sync.WaitGroup
	releases.Add(2)
	go func() { defer releases.Done(); release() }()
	go func() { defer releases.Done(); release() }()
	releases.Wait()
	select {
	case <-transitionAcquired:
	case <-time.After(time.Second):
		t.Fatal("user transition did not proceed after terminal commit release")
	}
	close(transitionRelease)
	select {
	case <-transitionDone:
	case <-time.After(time.Second):
		t.Fatal("user transition did not release the transition lock")
	}

	// A double release must not add a second lock token.
	if !lock.TryLock() {
		t.Fatal("transition lock was not available after release")
	}
	if lock.TryLock() {
		lock.Unlock()
		t.Fatal("idempotent release added more than one transition-lock token")
	}
	lock.Unlock()
}

func TestAcquireTerminalCommitRejectsInvalidAuthority(t *testing.T) {
	newReadyService := func(t *testing.T) (*Service, runner.AllocationRef) {
		t.Helper()
		svc, _, _ := newTestService()
		ref := runner.AllocationRef{
			ID:       "allocation-terminal-authority",
			Session:  runner.SessionRef{SessionID: "session-terminal-authority", Generation: 2},
			Provider: runner.ProviderLocalDocker,
		}
		svc.mu.Lock()
		svc.sessions["user-1"] = &Session{
			ID:         ref.Session.SessionID,
			UserID:     "user-1",
			Generation: ref.Session.Generation,
			Allocation: ref,
			Status:     StatusReady,
			TimeoutAt:  time.Now().Add(time.Hour),
		}
		svc.mu.Unlock()
		return svc, ref
	}

	t.Run("cancelled context", func(t *testing.T) {
		svc, ref := newReadyService(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if release, ok := svc.AcquireTerminalCommit(ctx, "user-1", ref); ok || release != nil {
			t.Fatal("cancelled terminal commit acquired authority")
		}
	})
	t.Run("stale allocation", func(t *testing.T) {
		svc, ref := newReadyService(t)
		ref.ID = "stale-allocation"
		if release, ok := svc.AcquireTerminalCommit(context.Background(), "user-1", ref); ok || release != nil {
			t.Fatal("stale allocation acquired terminal commit authority")
		}
	})
	t.Run("non-ready status", func(t *testing.T) {
		svc, ref := newReadyService(t)
		sess := svc.GetSession("user-1")
		sess.mu.Lock()
		sess.Status = StatusVerifying
		sess.mu.Unlock()
		if release, ok := svc.AcquireTerminalCommit(context.Background(), "user-1", ref); ok || release != nil {
			t.Fatal("non-ready allocation acquired terminal commit authority")
		}
	})
	t.Run("expired allocation", func(t *testing.T) {
		svc, ref := newReadyService(t)
		sess := svc.GetSession("user-1")
		sess.mu.Lock()
		sess.TimeoutAt = time.Now()
		sess.mu.Unlock()
		if _, ok := svc.GetTerminalTarget("user-1", ref.Session); ok {
			t.Fatal("expired allocation remained a terminal target")
		}
		if release, ok := svc.AcquireTerminalCommit(context.Background(), "user-1", ref); ok || release != nil {
			t.Fatal("expired allocation acquired terminal commit authority")
		}
	})
	t.Run("terminal intent", func(t *testing.T) {
		svc, ref := newReadyService(t)
		sess := svc.GetSession("user-1")
		svc.setTerminalIntent(sess, StatusFailed, "failed", "")
		if release, ok := svc.AcquireTerminalCommit(context.Background(), "user-1", ref); ok || release != nil {
			t.Fatal("terminal-intent allocation acquired terminal commit authority")
		}
	})
	t.Run("service stopping", func(t *testing.T) {
		svc, ref := newReadyService(t)
		svc.BeginShutdown()
		if release, ok := svc.AcquireTerminalCommit(context.Background(), "user-1", ref); ok || release != nil {
			t.Fatal("stopping service acquired terminal commit authority")
		}
	})
	t.Run("empty identity", func(t *testing.T) {
		svc, _ := newReadyService(t)
		if release, ok := svc.AcquireTerminalCommit(context.Background(), "", runner.AllocationRef{}); ok || release != nil {
			t.Fatal("empty terminal identity acquired commit authority")
		}
	})
}

func TestEndCommitFenceDoesNotTouchReplacementSession(t *testing.T) {
	svc, _, _ := newTestService()
	replacement := runner.AllocationRef{
		ID:       "allocation-replacement",
		Session:  runner.SessionRef{SessionID: "session-replacement", Generation: 1},
		Provider: runner.ProviderLocalDocker,
	}
	svc.mu.Lock()
	svc.sessions["user-1"] = &Session{
		ID:         replacement.Session.SessionID,
		UserID:     "user-1",
		Generation: replacement.Session.Generation,
		Allocation: replacement,
		Status:     StatusReady,
		TimeoutAt:  time.Now().Add(time.Hour),
	}
	svc.mu.Unlock()

	svc.markEndCommittedLocked("user-1", runner.SessionRef{SessionID: "historical-session", Generation: 3})
	if release, ok := svc.AcquireTerminalCommit(context.Background(), "user-1", replacement); !ok || release == nil {
		t.Fatal("historical end replay fenced the replacement session")
	} else {
		release()
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
	mgr.verifyResult = runner.VerifyResult{Status: runner.VerifyPassed}

	svc.StartProblem(context.Background(), "user-1", "p1")
	time.Sleep(100 * time.Millisecond)

	success, log, err := svc.Verify(context.Background(), "user-1", "operation-success")
	if err != nil {
		t.Fatalf("Verify failed: %v", err)
	}
	if !success {
		t.Error("expected verify success")
	}
	if log != "Verification passed." {
		t.Errorf("expected approved public feedback, got %s", log)
	}
}

func TestVerifySameOperationReplaysAfterSuccessfulSessionCleanup(t *testing.T) {
	svc, mgr, store := newTestService()
	store.problems["p1"] = &models.Problem{ID: "p1", TimeoutMinutes: 30, VerifyType: "script"}
	mgr.verifyResult = runner.VerifyResult{Status: runner.VerifyPassed}
	if _, err := svc.StartProblem(context.Background(), "user-1", "p1"); err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, svc, "user-1", StatusReady)

	firstSuccess, firstLog, err := svc.Verify(context.Background(), "user-1", "operation-response-loss")
	if err != nil {
		t.Fatal(err)
	}
	if svc.GetSession("user-1") != nil {
		t.Fatal("successful verification must clean up the session")
	}
	secondSuccess, secondLog, err := svc.Verify(context.Background(), "user-1", "operation-response-loss")
	if err != nil {
		t.Fatalf("same operation replay after cleanup: %v", err)
	}
	if !firstSuccess || !secondSuccess || firstLog != secondLog {
		t.Fatalf("replayed result mismatch: first=(%v,%q) second=(%v,%q)", firstSuccess, firstLog, secondSuccess, secondLog)
	}
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	if len(mgr.verifyKeys) != 1 {
		t.Fatalf("same verify operation reached Runner %d times", len(mgr.verifyKeys))
	}
	if !strings.HasSuffix(mgr.verifyKeys[0], ":operation-response-loss") {
		t.Fatalf("operation id was not propagated to Runner: %q", mgr.verifyKeys[0])
	}
}

func TestVerifyRejectsSameUserWaitQueueWhileTransitionRuns(t *testing.T) {
	svc, mgr, _ := newTestService()
	lock := svc.userLock("user-1")
	lock.Lock()
	defer lock.Unlock()

	const requests = 100
	start := make(chan struct{})
	results := make(chan error, requests)
	for i := 0; i < requests; i++ {
		go func(i int) {
			<-start
			_, _, err := svc.Verify(context.Background(), "user-1", fmt.Sprintf("verify-queued-%03d", i))
			results <- err
		}(i)
	}
	close(start)

	deadline := time.After(time.Second)
	for i := 0; i < requests; i++ {
		select {
		case err := <-results:
			if !errors.Is(err, ErrTransitionBusy) {
				t.Fatalf("verify contention error = %v, want ErrTransitionBusy", err)
			}
		case <-deadline:
			t.Fatalf("verify request %d joined a blocking same-user queue", i+1)
		}
	}
	mgr.mu.Lock()
	verifyCalls := len(mgr.verifyKeys)
	mgr.mu.Unlock()
	if verifyCalls != 0 {
		t.Fatalf("rejected verify contention reached Runner %d times", verifyCalls)
	}
}

func TestVerifyOperationKeyConflictsAfterResetGeneration(t *testing.T) {
	svc, mgr, store := newTestService()
	store.problems["p1"] = &models.Problem{ID: "p1", TimeoutMinutes: 30, VerifyType: "script"}
	mgr.verifyResult = runner.VerifyResult{Status: runner.VerifyFailed}
	if _, err := svc.StartProblem(context.Background(), "user-1", "p1"); err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, svc, "user-1", StatusReady)
	if _, _, err := svc.Verify(context.Background(), "user-1", "operation-generation-bound"); err != nil {
		t.Fatal(err)
	}
	if err := svc.ResetEnvironment(context.Background(), "user-1"); err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, svc, "user-1", StatusReady)

	if _, _, err := svc.Verify(context.Background(), "user-1", "operation-generation-bound"); !errors.Is(err, runner.ErrIdempotencyConflict) {
		t.Fatalf("same key on reset generation error = %v, want ErrIdempotencyConflict", err)
	}
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	if len(mgr.verifyKeys) != 1 {
		t.Fatalf("conflicting generation reached Runner %d times, want 1", len(mgr.verifyKeys))
	}
}

func TestVerifyOperationKeyConflictsWithNewSession(t *testing.T) {
	svc, _, store := newTestService()
	store.problems["p1"] = &models.Problem{ID: "p1", TimeoutMinutes: 30, VerifyType: "script"}
	store.problems["p2"] = &models.Problem{ID: "p2", TimeoutMinutes: 30, VerifyType: "script"}
	if _, err := svc.StartProblem(context.Background(), "user-1", "p1"); err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, svc, "user-1", StatusReady)
	if success, _, err := svc.Verify(context.Background(), "user-1", "operation-session-bound"); err != nil || !success {
		t.Fatalf("first verify success=%v err=%v", success, err)
	}
	if _, err := svc.StartProblem(context.Background(), "user-1", "p2"); err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, svc, "user-1", StatusReady)

	if _, _, err := svc.Verify(context.Background(), "user-1", "operation-session-bound"); !errors.Is(err, runner.ErrIdempotencyConflict) {
		t.Fatalf("same key on new session error = %v, want ErrIdempotencyConflict", err)
	}
}

func TestVerifyFailure(t *testing.T) {
	svc, mgr, store := newTestService()
	store.problems["p1"] = &models.Problem{ID: "p1", TimeoutMinutes: 30, BaseImage: "k3s-base:latest", VerifyType: "script"}
	mgr.verifyResult = runner.VerifyResult{Status: runner.VerifyFailed}

	svc.StartProblem(context.Background(), "user-1", "p1")
	time.Sleep(100 * time.Millisecond)

	success, _, err := svc.Verify(context.Background(), "user-1", "operation-failure")
	if err != nil {
		t.Fatalf("Verify failed: %v", err)
	}
	if success {
		t.Error("expected verify failure")
	}
}

func TestVerifyNoSession(t *testing.T) {
	svc, _, _ := newTestService()
	_, _, err := svc.Verify(context.Background(), "user-1", "operation-no-session")
	if err == nil {
		t.Fatal("expected error for no session")
	}
}

func TestVerifyCallback(t *testing.T) {
	svc, mgr, store := newTestService()
	store.problems["p1"] = &models.Problem{ID: "p1", TimeoutMinutes: 30, BaseImage: "k3s-base:latest", VerifyType: "script"}
	mgr.verifyResult = runner.VerifyResult{Status: runner.VerifyPassed}

	var callbackCalled bool
	var callbackSuccess bool
	svc.SetVerifyCallback(func(userID string, sessionRef runner.SessionRef, success bool, log string) {
		callbackCalled = true
		callbackSuccess = success
	})

	svc.StartProblem(context.Background(), "user-1", "p1")
	time.Sleep(100 * time.Millisecond)
	svc.Verify(context.Background(), "user-1", "operation-callback")

	if !callbackCalled {
		t.Error("expected verify callback to be called")
	}
	if !callbackSuccess {
		t.Error("expected callback success=true")
	}
}

func TestSubmitChoice(t *testing.T) {
	svc, mgr, store := newTestService()
	store.problems["p1"] = &models.Problem{
		ID:             "p1",
		TimeoutMinutes: 30,
		BaseImage:      "k3s-base:latest",
		VerifyType:     "choice",
		CorrectChoice:  "b",
	}

	svc.StartProblem(context.Background(), "user-1", "p1")
	time.Sleep(50 * time.Millisecond)

	current := svc.GetCurrentSession("user-1")
	if current == nil {
		t.Fatal("choice session is missing")
	}
	success, err := svc.SubmitChoice(context.Background(), "user-1", "p1", runner.SessionRef{
		SessionID: current.SessionID, Generation: current.Generation,
	}, "choice-correct-operation", "b")
	if err != nil {
		t.Fatalf("SubmitChoice failed: %v", err)
	}
	if !success {
		t.Error("expected correct choice")
	}
	if svc.GetSession("user-1") != nil {
		t.Fatal("successful choice must release the active session")
	}
	mgr.mu.Lock()
	removed := len(mgr.removed)
	mgr.mu.Unlock()
	if removed != 1 {
		t.Fatalf("successful choice must destroy one allocation, got %d", removed)
	}
}

func TestSubmitIncorrectChoiceKeepsSessionReady(t *testing.T) {
	svc, _, store := newTestService()
	store.problems["p1"] = &models.Problem{
		ID: "p1", TimeoutMinutes: 30, VerifyType: "choice", CorrectChoice: "b",
	}
	if _, err := svc.StartProblem(context.Background(), "user-1", "p1"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	current := svc.GetCurrentSession("user-1")
	if current == nil {
		t.Fatal("choice session is missing")
	}
	success, err := svc.SubmitChoice(context.Background(), "user-1", "p1", runner.SessionRef{
		SessionID: current.SessionID, Generation: current.Generation,
	}, "choice-incorrect-operation", "a")
	if err != nil {
		t.Fatalf("SubmitChoice failed: %v", err)
	}
	if success {
		t.Fatal("expected incorrect choice")
	}
	if sess := svc.GetSession("user-1"); sess == nil {
		t.Fatal("incorrect choice must keep the session active")
	}
}

func TestSubmitChoiceRejectsSameUserWaitQueueWhileTransitionRuns(t *testing.T) {
	svc, _, _ := newTestService()
	lock := svc.userLock("user-1")
	lock.Lock()
	defer lock.Unlock()

	const requests = 100
	start := make(chan struct{})
	results := make(chan error, requests)
	for i := 0; i < requests; i++ {
		go func() {
			<-start
			_, err := svc.SubmitChoice(context.Background(), "user-1", "p1", runner.SessionRef{
				SessionID: "session-1", Generation: 1,
			}, "choice-busy-operation", "b")
			results <- err
		}()
	}
	close(start)

	deadline := time.After(time.Second)
	for i := 0; i < requests; i++ {
		select {
		case err := <-results:
			if !errors.Is(err, ErrTransitionBusy) {
				t.Fatalf("choice contention error = %v, want ErrTransitionBusy", err)
			}
		case <-deadline:
			t.Fatalf("choice request %d joined a blocking same-user queue", i+1)
		}
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
	if len(crashed) != 1 || crashed[0].userID != "user-1" || crashed[0].session.Generation != 1 {
		t.Fatalf("expected user-1 crashed, got %v", crashed)
	}

	if svc.GetSession("user-1") != nil {
		t.Fatal("crashed session must be removed after cleanup")
	}
	mgr.mu.Lock()
	removed := len(mgr.removed)
	mgr.mu.Unlock()
	if removed != 1 {
		t.Fatalf("crash cleanup removed %d allocations, want 1", removed)
	}
}

func TestReadinessFailurePersistsDestroysAndEmitsIdentity(t *testing.T) {
	svc, mgr, store := newTestService()
	store.problems["p1"] = &models.Problem{ID: "p1", TimeoutMinutes: 30, VerifyType: "script"}
	mgr.waitReadyErr = errors.New("k3s boot failed")
	ended := make(chan struct {
		ref    runner.SessionRef
		reason string
	}, 1)
	svc.SetEndedCallback(func(_ string, ref runner.SessionRef, reason string) {
		ended <- struct {
			ref    runner.SessionRef
			reason string
		}{ref: ref, reason: reason}
	})

	sessionID, err := svc.StartProblem(context.Background(), "user-1", "p1")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-ended:
		if event.ref != (runner.SessionRef{SessionID: sessionID, Generation: 1}) || event.reason != "environment_failed" {
			t.Fatalf("ended event = %+v/%q", event.ref, event.reason)
		}
	case <-time.After(time.Second):
		t.Fatal("readiness failure did not emit terminal identity")
	}
	if got := attemptStatus(t, store, sessionID); got != "failed" {
		t.Fatalf("attempt status = %q, want failed", got)
	}
	if svc.GetSession("user-1") != nil {
		t.Fatal("failed readiness session remained active")
	}
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	if len(mgr.removed) != 1 || mgr.removed[0].Session != (runner.SessionRef{SessionID: sessionID, Generation: 1}) {
		t.Fatalf("readiness cleanup targets = %+v", mgr.removed)
	}
}

func TestSetupFailureRetriesExactDestroy(t *testing.T) {
	svc, mgr, store := newTestService()
	store.problems["p1"] = &models.Problem{ID: "p1", TimeoutMinutes: 30, VerifyType: "script"}
	mgr.setupErr = errors.New("setup script failed")
	mgr.destroyErr = errors.New("provider busy")
	ended := make(chan runner.SessionRef, 1)
	svc.SetEndedCallback(func(_ string, ref runner.SessionRef, _ string) { ended <- ref })

	sessionID, err := svc.StartProblem(context.Background(), "user-1", "p1")
	if err != nil {
		t.Fatal(err)
	}
	var failedRef runner.SessionRef
	select {
	case failedRef = <-ended:
	case <-time.After(time.Second):
		t.Fatal("setup failure did not emit terminal event")
	}
	if failedRef != (runner.SessionRef{SessionID: sessionID, Generation: 1}) {
		t.Fatalf("failed identity = %+v", failedRef)
	}
	if svc.GetSession("user-1") == nil {
		t.Fatal("destroy failure discarded exact cleanup target")
	}
	svc.retryTerminalCleanup()
	if svc.GetSession("user-1") != nil {
		t.Fatal("setup failure cleanup retry did not converge")
	}
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	if len(mgr.removed) != 2 || mgr.removed[0] != mgr.removed[1] || mgr.removed[0].Session != failedRef {
		t.Fatalf("setup failure did not retry exact allocation: %+v", mgr.removed)
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
	mgr.verifyResult = runner.VerifyResult{Status: runner.VerifyNeedsGrading, Evidence: "pod running"}
	svc.SetGrader(&mockGrader{success: true, log: "SECRET_TOKEN=grader-must-not-publish"})

	svc.StartProblem(context.Background(), "user-1", "p1")
	time.Sleep(100 * time.Millisecond)

	success, log, err := svc.Verify(context.Background(), "user-1", "operation-text-grade")
	if err != nil {
		t.Fatalf("Verify failed: %v", err)
	}
	if !success {
		t.Error("expected text grading success")
	}
	if log != "Verification passed." || strings.Contains(log, "SECRET_TOKEN") {
		t.Errorf("expected allowlisted feedback without grader reason, got %s", log)
	}
}

func TestVerifyTextFailureDoesNotPublishGraderReason(t *testing.T) {
	svc, mgr, store := newTestService()
	store.problems["p1"] = &models.Problem{ID: "p1", TimeoutMinutes: 30, VerifyType: "text", GradingPrompt: "check"}
	mgr.verifyResult = runner.VerifyResult{Status: runner.VerifyNeedsGrading, Evidence: "private evidence"}
	svc.SetGrader(&mockGrader{success: false, log: "-----BEGIN PRIVATE KEY-----"})

	if _, err := svc.StartProblem(context.Background(), "user-1", "p1"); err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, svc, "user-1", StatusReady)
	success, log, err := svc.Verify(context.Background(), "user-1", "operation-text-grade-failed")
	if err != nil || success {
		t.Fatalf("text failure success=%v log=%q err=%v", success, log, err)
	}
	if log != "Verification did not pass." || strings.Contains(log, "PRIVATE KEY") {
		t.Fatalf("grader reason leaked through public feedback: %q", log)
	}
}

func TestVerifyTextNoGrader(t *testing.T) {
	svc, mgr, store := newTestService()
	store.problems["p1"] = &models.Problem{ID: "p1", TimeoutMinutes: 30, BaseImage: "k3s-base:latest", VerifyType: "text", GradingPrompt: "check"}
	mgr.verifyResult = runner.VerifyResult{Status: runner.VerifyNeedsGrading, Evidence: "pod running"}

	svc.StartProblem(context.Background(), "user-1", "p1")
	time.Sleep(100 * time.Millisecond)

	_, _, err := svc.Verify(context.Background(), "user-1", "operation-no-grader")
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
	if len(expired) != 1 || expired[0].userID != "user-1" || expired[0].session.Generation != 1 {
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

func TestCleanupRetryRemovesCompletedSession(t *testing.T) {
	svc, _, store := newTestService()
	store.problems["p1"] = &models.Problem{ID: "p1", TimeoutMinutes: 30, BaseImage: "k3s-base:latest", VerifyType: "script"}

	svc.StartProblem(context.Background(), "user-1", "p1")
	time.Sleep(100 * time.Millisecond)

	sess := svc.GetSession("user-1")
	sess.mu.Lock()
	sess.TimeoutAt = time.Now().Add(-time.Second)
	sess.Status = StatusCompleted
	sess.mu.Unlock()

	svc.retryTerminalCleanup()
	if svc.GetSession("user-1") != nil {
		t.Error("completed session must be removed by cleanup retry")
	}
}

func TestTerminalFinalizationDestroysDespiteAttemptPersistenceFailure(t *testing.T) {
	svc, mgr, store := newTestService()
	store.problems["p1"] = &models.Problem{ID: "p1", TimeoutMinutes: 30, VerifyType: "script"}
	if _, err := svc.StartProblem(context.Background(), "user-1", "p1"); err != nil {
		t.Fatal(err)
	}
	sess := waitForStatus(t, svc, "user-1", StatusReady)
	sess.mu.Lock()
	attemptID := sess.AttemptID
	sess.mu.Unlock()

	store.mu.Lock()
	store.updateErr = errors.New("database unavailable")
	store.mu.Unlock()
	if success, _, err := svc.Verify(context.Background(), "user-1", "operation-persist-retry"); err != nil || !success {
		t.Fatalf("verify result success=%v err=%v", success, err)
	}
	if svc.GetSession("user-1") == nil {
		t.Fatal("session was forgotten before terminal persistence converged")
	}
	mgr.mu.Lock()
	removedBeforeRetry := len(mgr.removed)
	mgr.mu.Unlock()
	if removedBeforeRetry != 1 {
		t.Fatalf("database failure blocked exact allocation destroy, removals=%d", removedBeforeRetry)
	}

	svc.retryTerminalCleanup()
	if svc.GetSession("user-1") != nil {
		t.Fatal("retry did not remove converged terminal session")
	}
	if got := attemptStatus(t, store, attemptID); got != "success" {
		t.Fatalf("attempt status = %s, want success", got)
	}
	mgr.mu.Lock()
	removedAfterRetry := len(mgr.removed)
	mgr.mu.Unlock()
	if removedAfterRetry != 2 {
		t.Fatalf("retry made %d idempotent exact destroy attempts, want 2", removedAfterRetry)
	}
}

func TestCompletedIntentSurvivesEndWhileCleanupPending(t *testing.T) {
	svc, mgr, store := newTestService()
	store.problems["p1"] = &models.Problem{ID: "p1", TimeoutMinutes: 30, VerifyType: "script"}
	if _, err := svc.StartProblem(context.Background(), "user-1", "p1"); err != nil {
		t.Fatal(err)
	}
	sess := waitForStatus(t, svc, "user-1", StatusReady)
	sess.mu.Lock()
	attemptID := sess.AttemptID
	ref := sess.Allocation
	sess.mu.Unlock()
	mgr.mu.Lock()
	mgr.destroyErr = errors.New("provider busy")
	mgr.mu.Unlock()

	if success, _, err := svc.Verify(context.Background(), "user-1", "operation-success-pending"); err != nil || !success {
		t.Fatalf("verify success=%v err=%v", success, err)
	}
	if svc.GetSession("user-1") == nil {
		t.Fatal("destroy failure discarded cleanup-pending session")
	}
	if err := svc.EndSession(context.Background(), "user-1"); err != nil {
		t.Fatalf("end should retry the existing terminal intent: %v", err)
	}
	if got := attemptStatus(t, store, attemptID); got != "success" {
		t.Fatalf("completed intent was reclassified as %q", got)
	}
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	if len(mgr.removed) != 2 || mgr.removed[0] != ref || mgr.removed[1] != ref {
		t.Fatalf("end did not retry the exact completed allocation: %+v", mgr.removed)
	}
}

func TestTimeoutIntentSurvivesEndWhileCleanupPending(t *testing.T) {
	svc, mgr, store := newTestService()
	store.problems["p1"] = &models.Problem{ID: "p1", TimeoutMinutes: 30, VerifyType: "script"}
	if _, err := svc.StartProblem(context.Background(), "user-1", "p1"); err != nil {
		t.Fatal(err)
	}
	sess := waitForStatus(t, svc, "user-1", StatusReady)
	sess.mu.Lock()
	sess.TimeoutAt = time.Now().Add(-time.Second)
	attemptID := sess.AttemptID
	ref := sess.Allocation
	sess.mu.Unlock()
	mgr.mu.Lock()
	mgr.destroyErr = errors.New("provider busy")
	mgr.mu.Unlock()

	if expired := svc.checkTimeouts(); len(expired) != 1 {
		t.Fatalf("expired events = %v, want one", expired)
	}
	if err := svc.EndSession(context.Background(), "user-1"); err != nil {
		t.Fatalf("end should retry the existing timeout intent: %v", err)
	}
	if got := attemptStatus(t, store, attemptID); got != "timeout" {
		t.Fatalf("timeout intent was reclassified as %q", got)
	}
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	if len(mgr.removed) != 2 || mgr.removed[0] != ref || mgr.removed[1] != ref {
		t.Fatalf("end did not retry the exact timed-out allocation: %+v", mgr.removed)
	}
}

func TestStartRetriesCompletedCleanupWithoutReclassifyingOldAttempt(t *testing.T) {
	svc, mgr, store := newTestService()
	store.problems["p1"] = &models.Problem{ID: "p1", TimeoutMinutes: 30, VerifyType: "script"}
	store.problems["p2"] = &models.Problem{ID: "p2", TimeoutMinutes: 30, VerifyType: "script"}
	if _, err := svc.StartProblem(context.Background(), "user-1", "p1"); err != nil {
		t.Fatal(err)
	}
	sess := waitForStatus(t, svc, "user-1", StatusReady)
	sess.mu.Lock()
	oldAttemptID := sess.AttemptID
	sess.mu.Unlock()
	mgr.mu.Lock()
	mgr.destroyErr = errors.New("provider busy")
	mgr.mu.Unlock()
	if success, _, err := svc.Verify(context.Background(), "user-1", "operation-success-replace"); err != nil || !success {
		t.Fatalf("verify success=%v err=%v", success, err)
	}

	if _, err := svc.StartProblem(context.Background(), "user-1", "p2"); err != nil {
		t.Fatalf("start should converge old cleanup before replacement: %v", err)
	}
	if got := attemptStatus(t, store, oldAttemptID); got != "success" {
		t.Fatalf("replacement reclassified completed attempt as %q", got)
	}
	current := svc.GetSession("user-1")
	if current == nil || current.ProblemID != "p2" {
		t.Fatalf("replacement session = %+v, want p2", current)
	}
}

func TestTerminalFinalizationRetriesExactAllocationAfterDestroyFailure(t *testing.T) {
	svc, mgr, store := newTestService()
	store.problems["p1"] = &models.Problem{ID: "p1", TimeoutMinutes: 30, VerifyType: "script"}
	if _, err := svc.StartProblem(context.Background(), "user-1", "p1"); err != nil {
		t.Fatal(err)
	}
	sess := waitForStatus(t, svc, "user-1", StatusReady)
	sess.mu.Lock()
	attemptID := sess.AttemptID
	ref := sess.Allocation
	sess.mu.Unlock()
	mgr.mu.Lock()
	mgr.destroyErr = errors.New("provider busy")
	mgr.mu.Unlock()

	if err := svc.EndSession(context.Background(), "user-1"); err == nil {
		t.Fatal("expected first destroy failure")
	}
	if got := attemptStatus(t, store, attemptID); got != "failed" {
		t.Fatalf("attempt status = %s, want failed before destroy retry", got)
	}
	if svc.GetSession("user-1") == nil {
		t.Fatal("destroy failure discarded exact cleanup target")
	}

	svc.retryTerminalCleanup()
	if svc.GetSession("user-1") != nil {
		t.Fatal("destroy retry did not remove session")
	}
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	if len(mgr.removed) != 2 || mgr.removed[0] != ref || mgr.removed[1] != ref {
		t.Fatalf("destroy retry did not preserve exact allocation: %+v", mgr.removed)
	}
}

func TestResetDestroyFailureRetainsExactCleanupAndTerminatesVisibleGeneration(t *testing.T) {
	svc, mgr, store := newTestService()
	store.problems["p1"] = &models.Problem{ID: "p1", TimeoutMinutes: 30, VerifyType: "script"}
	if _, err := svc.StartProblem(context.Background(), "user-1", "p1"); err != nil {
		t.Fatal(err)
	}
	sess := waitForStatus(t, svc, "user-1", StatusReady)
	sess.mu.Lock()
	oldRef := sess.Allocation
	attemptID := sess.AttemptID
	sess.mu.Unlock()
	mgr.mu.Lock()
	mgr.destroyErr = errors.New("provider busy")
	mgr.mu.Unlock()
	var ended runner.SessionRef
	var reason string
	svc.SetEndedCallback(func(_ string, ref runner.SessionRef, gotReason string) {
		ended, reason = ref, gotReason
	})

	if err := svc.ResetEnvironment(context.Background(), "user-1"); err == nil {
		t.Fatal("expected reset destroy failure")
	}
	if ended != oldRef.Session || reason != "environment_failed" {
		t.Fatalf("ended event = %+v/%q, want old generation environment_failed", ended, reason)
	}
	if got := attemptStatus(t, store, attemptID); got != "failed" {
		t.Fatalf("attempt status = %s, want failed", got)
	}
	svc.retryTerminalCleanup()
	if svc.GetSession("user-1") != nil {
		t.Fatal("reset destroy retry did not remove terminal session")
	}
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	if len(mgr.removed) != 2 || mgr.removed[0] != oldRef || mgr.removed[1] != oldRef {
		t.Fatalf("reset cleanup did not retry exact old allocation: %+v", mgr.removed)
	}
}

func TestResetCreateFailureConvergesWithoutPhantomAllocation(t *testing.T) {
	svc, mgr, store := newTestService()
	store.problems["p1"] = &models.Problem{ID: "p1", TimeoutMinutes: 30, VerifyType: "script"}
	if _, err := svc.StartProblem(context.Background(), "user-1", "p1"); err != nil {
		t.Fatal(err)
	}
	sess := waitForStatus(t, svc, "user-1", StatusReady)
	sess.mu.Lock()
	oldRef := sess.Allocation
	attemptID := sess.AttemptID
	sess.mu.Unlock()
	mgr.mu.Lock()
	mgr.createErr = errors.New("provider create failed")
	mgr.mu.Unlock()
	var ended runner.SessionRef
	svc.SetEndedCallback(func(_ string, ref runner.SessionRef, _ string) { ended = ref })

	if err := svc.ResetEnvironment(context.Background(), "user-1"); err == nil {
		t.Fatal("expected replacement create failure")
	}
	if ended != oldRef.Session {
		t.Fatalf("visible terminal event identity = %+v, want %+v", ended, oldRef.Session)
	}
	if svc.GetSession("user-1") != nil {
		t.Fatal("allocation-free failed reset session was not removed")
	}
	if got := attemptStatus(t, store, attemptID); got != "failed" {
		t.Fatalf("attempt status = %s, want failed", got)
	}
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	if len(mgr.removed) != 1 || mgr.removed[0] != oldRef {
		t.Fatalf("replacement create failure targeted a phantom allocation: %+v", mgr.removed)
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

// SESS-3: global concurrency cap → ErrTooManySessions (own replacement exempt).
func TestStartProblemCapacityLimit(t *testing.T) {
	svc, _, store := newTestService()
	store.problems["p1"] = &models.Problem{ID: "p1", TimeoutMinutes: 30, BaseImage: "k3s-base:latest", VerifyType: "script"}
	store.problems["p2"] = &models.Problem{ID: "p2", TimeoutMinutes: 30, BaseImage: "k3s-base:latest", VerifyType: "script"}
	svc.SetMaxConcurrentSessions(1)

	if _, err := svc.StartProblem(context.Background(), "user-1", "p1"); err != nil {
		t.Fatalf("first start failed: %v", err)
	}
	time.Sleep(50 * time.Millisecond)

	// Another user hits the cap.
	if _, err := svc.StartProblem(context.Background(), "user-2", "p2"); !errors.Is(err, ErrTooManySessions) {
		t.Errorf("expected ErrTooManySessions, got %v", err)
	}

	// The same user replacing their own session is allowed (total unchanged).
	if _, err := svc.StartProblem(context.Background(), "user-1", "p2"); err != nil {
		t.Errorf("own-session replacement must not hit the cap: %v", err)
	}
}

// SESS-3: per-user start cooldown → ErrStartCooldown until it elapses.
func TestStartProblemCooldown(t *testing.T) {
	svc, _, store := newTestService()
	store.problems["p1"] = &models.Problem{ID: "p1", TimeoutMinutes: 30, BaseImage: "k3s-base:latest", VerifyType: "script"}
	svc.SetStartCooldown(80 * time.Millisecond)

	if _, err := svc.StartProblem(context.Background(), "user-1", "p1"); err != nil {
		t.Fatalf("first start failed: %v", err)
	}
	if _, err := svc.StartProblem(context.Background(), "user-1", "p1"); !errors.Is(err, ErrStartCooldown) {
		t.Errorf("expected ErrStartCooldown immediately after start, got %v", err)
	}

	time.Sleep(100 * time.Millisecond)
	if _, err := svc.StartProblem(context.Background(), "user-1", "p1"); err != nil {
		t.Errorf("expected start to succeed after cooldown, got %v", err)
	}
}

// WS-3: timeout_warning fires ONCE per session until reset.
func TestTimeoutWarningOncePerSession(t *testing.T) {
	svc, _, store := newTestService()
	store.problems["p1"] = &models.Problem{ID: "p1", TimeoutMinutes: 30, BaseImage: "k3s-base:latest", VerifyType: "script"}

	var warns int
	var warnMu sync.Mutex
	svc.SetTimeoutWarningCallback(func(userID string, sessionRef runner.SessionRef, remaining int) {
		warnMu.Lock()
		warns++
		warnMu.Unlock()
	})

	svc.StartProblem(context.Background(), "user-1", "p1")
	time.Sleep(100 * time.Millisecond)

	sess := svc.GetSession("user-1")
	sess.mu.Lock()
	sess.TimeoutAt = time.Now().Add(2 * time.Minute) // under 5min, still > 0
	sess.mu.Unlock()

	svc.checkTimeouts()
	svc.checkTimeouts()
	svc.checkTimeouts()

	warnMu.Lock()
	got := warns
	warnMu.Unlock()
	if got != 1 {
		t.Fatalf("expected exactly 1 warning, got %d", got)
	}

	// Reset re-arms the warning for the new environment.
	if err := svc.ResetEnvironment(context.Background(), "user-1"); err != nil {
		t.Fatalf("reset failed: %v", err)
	}
	time.Sleep(100 * time.Millisecond)

	sess = svc.GetSession("user-1")
	sess.mu.Lock()
	sess.TimeoutAt = time.Now().Add(2 * time.Minute)
	sess.mu.Unlock()

	svc.checkTimeouts()
	warnMu.Lock()
	got = warns
	warnMu.Unlock()
	if got != 2 {
		t.Errorf("expected warning re-armed after reset (2 total), got %d", got)
	}
}

// WS-4: ResetEnvironment emits the reset event before new boot stages.
func TestResetEmitsResetCallback(t *testing.T) {
	svc, _, store := newTestService()
	store.problems["p1"] = &models.Problem{ID: "p1", TimeoutMinutes: 30, BaseImage: "k3s-base:latest", VerifyType: "script"}

	var resetFor string
	svc.SetResetCallback(func(userID string, sessionRef runner.SessionRef) { resetFor = userID })

	svc.StartProblem(context.Background(), "user-1", "p1")
	time.Sleep(50 * time.Millisecond)

	if err := svc.ResetEnvironment(context.Background(), "user-1"); err != nil {
		t.Fatalf("reset failed: %v", err)
	}
	if resetFor != "user-1" {
		t.Errorf("expected reset callback for user-1, got %q", resetFor)
	}
	if svc.GetSession("user-1") == nil {
		t.Error("session must stay active across reset")
	}
}

func TestResetIgnoresStaleSetupCompletion(t *testing.T) {
	svc, mgr, store := newTestService()
	store.problems["p1"] = &models.Problem{ID: "p1", TimeoutMinutes: 30, BaseImage: "k3s-base:latest", VerifyType: "script"}
	oldSetupRelease := make(chan struct{})
	mgr.setupGates = map[uint64]chan struct{}{1: oldSetupRelease}
	mgr.setupEntered = make(chan runner.AllocationRef, 2)

	if _, err := svc.StartProblem(context.Background(), "user-1", "p1"); err != nil {
		t.Fatalf("start: %v", err)
	}
	select {
	case ref := <-mgr.setupEntered:
		if ref.Session.Generation != 1 {
			t.Fatalf("first setup generation = %d, want 1", ref.Session.Generation)
		}
	case <-time.After(time.Second):
		t.Fatal("generation 1 setup did not start")
	}

	if err := svc.ResetEnvironment(context.Background(), "user-1"); err != nil {
		t.Fatalf("reset: %v", err)
	}
	close(oldSetupRelease)

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		sess := svc.GetSession("user-1")
		if sess != nil {
			sess.mu.Lock()
			generation, status, refGeneration := sess.Generation, sess.Status, sess.Allocation.Session.Generation
			sess.mu.Unlock()
			if generation == 2 && refGeneration == 2 && status == StatusReady {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("generation 2 did not remain the ready current environment")
}

func TestVerifyInfrastructureErrorRestoresReady(t *testing.T) {
	svc, mgr, store := newTestService()
	store.problems["p1"] = &models.Problem{ID: "p1", TimeoutMinutes: 30, VerifyType: "script"}
	if _, err := svc.StartProblem(context.Background(), "user-1", "p1"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	mgr.verifyErr = errors.New("runner unavailable")
	if _, _, err := svc.Verify(context.Background(), "user-1", "operation-infra-error"); err == nil {
		t.Fatal("expected verify infrastructure error")
	}
	sess := svc.GetSession("user-1")
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.Status != StatusReady {
		t.Fatalf("verify error left status %s, want ready", sess.Status)
	}
}

// The session service sends only a problem identity/revision across the
// Runner boundary; provider image selection stays inside the adapter catalog.
func TestStartProblemUsesSafeRunnerRequest(t *testing.T) {
	svc, mgr, store := newTestService()
	store.problems["p1"] = &models.Problem{ID: "p1", TimeoutMinutes: 30, Image: "custom:v2", BaseImage: "k3s-base:latest", VerifyType: "script"}

	if _, err := svc.StartProblem(context.Background(), "user-1", "p1"); err != nil {
		t.Fatalf("start failed: %v", err)
	}
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	if len(mgr.created) != 1 {
		t.Fatalf("expected one Runner create request, got %+v", mgr.created)
	}
	req := mgr.created[0]
	if req.Selection.Problem.ID != "p1" || req.Selection.Problem.Revision != "revision-p1" || req.Selection.Generation != 1 || req.Session.Generation != 1 {
		t.Errorf("unexpected safe Runner request: %+v", req)
	}
	if req.AllocationID != runner.AllocationIDForSession(req.Session) {
		t.Errorf("create did not use reserved stable allocation identity: %+v", req)
	}
	if req.ResourceProfile != runner.DefaultResourceProfile || req.IdempotencyKey == "" || req.ExpiresAt.IsZero() {
		t.Errorf("incomplete safe Runner request: %+v", req)
	}
}

// SEC3-4: verifies are throttled per user (min gap between starts).
func TestVerifyThrottle(t *testing.T) {
	svc, mgr, store := newTestService()
	store.problems["p1"] = &models.Problem{ID: "p1", TimeoutMinutes: 30, BaseImage: "k3s-base:latest", VerifyType: "script"}
	// A FAILING verify keeps the session ready, so the throttle (not the
	// session status) is what gates the second/third attempts.
	mgr.verifyResult = runner.VerifyResult{Status: runner.VerifyFailed}
	svc.SetVerifyThrottle(100 * time.Millisecond)

	svc.StartProblem(context.Background(), "user-1", "p1")
	time.Sleep(100 * time.Millisecond)

	if _, _, err := svc.Verify(context.Background(), "user-1", "operation-throttle-a"); err != nil {
		t.Fatalf("first verify failed: %v", err)
	}
	if _, _, err := svc.Verify(context.Background(), "user-1", "operation-throttle-b"); !errors.Is(err, ErrVerifyTooFast) {
		t.Fatalf("expected ErrVerifyTooFast, got %v", err)
	}
	time.Sleep(120 * time.Millisecond)
	if _, _, err := svc.Verify(context.Background(), "user-1", "operation-throttle-c"); err != nil {
		t.Errorf("expected verify allowed after throttle window, got %v", err)
	}
}

// SEC3-5: reset honors the same per-user cooldown as start.
func TestResetHonorsCooldown(t *testing.T) {
	svc, _, store := newTestService()
	store.problems["p1"] = &models.Problem{ID: "p1", TimeoutMinutes: 30, BaseImage: "k3s-base:latest", VerifyType: "script"}
	svc.SetStartCooldown(100 * time.Millisecond)

	svc.StartProblem(context.Background(), "user-1", "p1")
	time.Sleep(50 * time.Millisecond)

	if err := svc.ResetEnvironment(context.Background(), "user-1"); !errors.Is(err, ErrStartCooldown) {
		t.Fatalf("expected ErrStartCooldown on immediate reset, got %v", err)
	}
	time.Sleep(120 * time.Millisecond)
	if err := svc.ResetEnvironment(context.Background(), "user-1"); err != nil {
		t.Errorf("expected reset allowed after cooldown, got %v", err)
	}
	// The reset itself re-arms the cooldown.
	if err := svc.ResetEnvironment(context.Background(), "user-1"); !errors.Is(err, ErrStartCooldown) {
		t.Errorf("expected cooldown re-armed by reset, got %v", err)
	}
}

// T3: the global cap must hold under concurrent starts (TOCTOU regression).
// Create blocks on a gate so all starts overlap before any session lands in
// the map; without the pendingStarts reservation every start would pass the
// check and the cap would be exceeded.
func TestStartProblemCapUnderConcurrency(t *testing.T) {
	mgr := &mockRunner{verifyResult: runner.VerifyResult{Status: runner.VerifyPassed}, running: true}
	gate := make(chan struct{})
	mgr.createGate = gate
	store := newMockProblemStore()
	svc := NewService(mgr, store, store)
	svc.SetStartCooldown(0)
	svc.SetMaxConcurrentSessions(2)
	store.problems["p1"] = &models.Problem{ID: "p1", TimeoutMinutes: 30, BaseImage: "k3s-base:latest", VerifyType: "script"}

	const users = 5
	errs := make([]error, users)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < users; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, errs[i] = svc.StartProblem(context.Background(), "user-"+string(rune('a'+i)), "p1")
		}(i)
	}
	close(start)
	time.Sleep(100 * time.Millisecond) // let all goroutines reach the gated Create
	close(gate)                        // release container creation
	wg.Wait()

	capErrs, ok := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, ErrTooManySessions):
			capErrs++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if ok != 2 || capErrs != 3 {
		t.Fatalf("expected 2 starts ok + 3 ErrTooManySessions, got ok=%d cap=%d", ok, capErrs)
	}
	time.Sleep(50 * time.Millisecond)
	svc.mu.RLock()
	active, pending := len(svc.sessions), svc.pendingStarts
	svc.mu.RUnlock()
	if active > 2 {
		t.Errorf("cap exceeded: %d active sessions", active)
	}
	if pending != 0 {
		t.Errorf("leaked %d pending reservations", pending)
	}
}

// The reservation is released when start fails (no slot leak).
func TestStartProblemCapReservationReleasedOnFailure(t *testing.T) {
	svc, _, store := newTestService()
	svc.SetMaxConcurrentSessions(1)

	// Unknown problem → start fails; the reservation must be released so the
	// next (valid) start is not falsely rejected.
	if _, err := svc.StartProblem(context.Background(), "user-1", "ghost"); err == nil {
		t.Fatal("expected error for unknown problem")
	}
	store.problems["p1"] = &models.Problem{ID: "p1", TimeoutMinutes: 30, BaseImage: "k3s-base:latest", VerifyType: "script"}
	if _, err := svc.StartProblem(context.Background(), "user-2", "p1"); err != nil {
		t.Errorf("expected slot released after failed start, got %v", err)
	}
}

func TestShutdownRejectsNewStartsAndWaitsForInflightProvisioning(t *testing.T) {
	mgr := &mockRunner{
		verifyResult:  runner.VerifyResult{Status: runner.VerifyPassed},
		running:       true,
		createGate:    make(chan struct{}),
		createEntered: make(chan runner.CreateSessionRequest, 1),
	}
	store := newMockProblemStore()
	store.problems["p1"] = &models.Problem{ID: "p1", TimeoutMinutes: 30, VerifyType: "script"}
	svc := NewService(mgr, store, store)
	svc.SetStartCooldown(0)

	startResult := make(chan error, 1)
	go func() {
		_, err := svc.StartProblem(context.Background(), "user-1", "p1")
		startResult <- err
	}()
	select {
	case <-mgr.createEntered:
	case <-time.After(time.Second):
		t.Fatal("in-flight start did not reach Runner")
	}

	svc.BeginShutdown()
	if _, err := svc.StartProblem(context.Background(), "user-2", "p1"); !errors.Is(err, ErrServiceStopping) {
		t.Fatalf("start after shutdown error = %v, want ErrServiceStopping", err)
	}
	waitCtx, waitCancel := context.WithTimeout(context.Background(), time.Second)
	defer waitCancel()
	if err := svc.WaitForProvisioning(waitCtx); err != nil {
		t.Fatalf("shutdown did not cancel admitted start: %v", err)
	}
	if err := <-startResult; !errors.Is(err, ErrServiceStopping) {
		t.Fatalf("cancelled admitted start error = %v, want ErrServiceStopping", err)
	}
	if err := svc.WaitForWatchers(waitCtx); err != nil {
		t.Fatalf("wait for watcher shutdown: %v", err)
	}
	if err := svc.CleanupAll(waitCtx); err != nil {
		t.Fatalf("cleanup after admitted start: %v", err)
	}
	mgr.mu.Lock()
	removed := len(mgr.removed)
	mgr.mu.Unlock()
	if removed != 0 {
		t.Fatalf("cancelled pre-commit start removed %d allocations, want 0", removed)
	}
}

func TestShutdownRejectsNewResetAndWaitsForInflightReset(t *testing.T) {
	svc, mgr, store := newTestService()
	store.problems["p1"] = &models.Problem{ID: "p1", TimeoutMinutes: 30, VerifyType: "script"}
	if _, err := svc.StartProblem(context.Background(), "user-1", "p1"); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitForStatus(t, svc, "user-1", StatusReady)

	mgr.mu.Lock()
	mgr.createGate = make(chan struct{})
	mgr.createEntered = make(chan runner.CreateSessionRequest, 1)
	createGate := mgr.createGate
	createEntered := mgr.createEntered
	mgr.mu.Unlock()
	resetResult := make(chan error, 1)
	go func() {
		resetResult <- svc.ResetEnvironment(context.Background(), "user-1")
	}()
	select {
	case req := <-createEntered:
		if req.Session.Generation != 2 {
			t.Fatalf("replacement generation = %d, want 2", req.Session.Generation)
		}
	case <-time.After(time.Second):
		t.Fatal("in-flight reset did not reach Runner create")
	}

	svc.BeginShutdown()
	if err := svc.ResetEnvironment(context.Background(), "user-1"); !errors.Is(err, ErrServiceStopping) {
		t.Fatalf("reset after shutdown error = %v, want ErrServiceStopping", err)
	}
	waitCtx, waitCancel := context.WithTimeout(context.Background(), time.Second)
	defer waitCancel()
	if err := svc.WaitForProvisioning(waitCtx); err != nil {
		t.Fatalf("shutdown did not cancel admitted reset: %v", err)
	}
	if err := <-resetResult; err == nil {
		t.Fatal("cancelled admitted reset unexpectedly succeeded")
	}
	if err := svc.WaitForWatchers(waitCtx); err != nil {
		t.Fatalf("wait for watcher shutdown: %v", err)
	}
	if err := svc.CleanupAll(waitCtx); err != nil {
		t.Fatalf("cleanup after admitted reset: %v", err)
	}
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	if len(mgr.removed) != 1 {
		t.Fatalf("cancelled reset removed %d allocations, want old generation only", len(mgr.removed))
	}
	if len(mgr.created) != 1 {
		t.Fatalf("created %d allocations, want initial generation only", len(mgr.created))
	}
	close(createGate)
}

func TestShutdownCompensatesLateCreateSuccessFromContextIgnoringProvider(t *testing.T) {
	mgr := &mockRunner{
		verifyResult:         runner.VerifyResult{Status: runner.VerifyPassed},
		running:              true,
		createGate:           make(chan struct{}),
		createEntered:        make(chan runner.CreateSessionRequest, 1),
		createIgnoresContext: true,
	}
	store := newMockProblemStore()
	store.problems["p1"] = &models.Problem{ID: "p1", TimeoutMinutes: 30, VerifyType: "script"}
	svc := NewService(mgr, store, store)
	svc.SetStartCooldown(0)

	result := make(chan error, 1)
	go func() {
		_, err := svc.StartProblem(context.Background(), "user-1", "p1")
		result <- err
	}()
	select {
	case <-mgr.createEntered:
	case <-time.After(time.Second):
		t.Fatal("start did not reach context-ignoring provider")
	}
	svc.BeginShutdown()
	shortCtx, shortCancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer shortCancel()
	if err := svc.WaitForProvisioning(shortCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("context-ignoring create was not tracked: %v", err)
	}
	close(mgr.createGate)
	waitCtx, waitCancel := context.WithTimeout(context.Background(), time.Second)
	defer waitCancel()
	if err := svc.WaitForProvisioning(waitCtx); err != nil {
		t.Fatalf("wait for late create compensation: %v", err)
	}
	if err := <-result; !errors.Is(err, ErrServiceStopping) {
		t.Fatalf("late create result = %v, want ErrServiceStopping", err)
	}
	if svc.GetSession("user-1") != nil {
		t.Fatal("late successful allocation remained visible after compensation")
	}
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	if len(mgr.created) != 1 || len(mgr.removed) != 1 {
		t.Fatalf("late create convergence created/removed = %d/%d, want 1/1", len(mgr.created), len(mgr.removed))
	}
	if mgr.removed[0].Session != mgr.created[0].Session {
		t.Fatalf("compensation removed wrong allocation: created=%+v removed=%+v", mgr.created[0], mgr.removed[0])
	}
}

func TestShutdownCancelsAndJoinsBlockedSetup(t *testing.T) {
	svc, mgr, store := newTestService()
	store.problems["p1"] = &models.Problem{ID: "p1", TimeoutMinutes: 30, VerifyType: "script"}
	mgr.setupGates = map[uint64]chan struct{}{1: make(chan struct{})}
	mgr.setupEntered = make(chan runner.AllocationRef, 1)
	if _, err := svc.StartProblem(context.Background(), "user-1", "p1"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-mgr.setupEntered:
	case <-time.After(time.Second):
		t.Fatal("setup did not block in Runner")
	}

	svc.BeginShutdown()
	waitCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := svc.WaitForWatchers(waitCtx); err != nil {
		t.Fatalf("shutdown did not cancel and join setup: %v", err)
	}
	if err := svc.CleanupAll(waitCtx); err != nil {
		t.Fatalf("cleanup after setup cancellation: %v", err)
	}
}

func TestStaleCrashObservationCannotRemoveResetGeneration(t *testing.T) {
	svc, mgr, store := newTestService()
	store.problems["p1"] = &models.Problem{ID: "p1", TimeoutMinutes: 30, VerifyType: "script"}
	if _, err := svc.StartProblem(context.Background(), "user-1", "p1"); err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, svc, "user-1", StatusReady)

	mgr.mu.Lock()
	mgr.running = false
	mgr.observeEntered = make(chan runner.AllocationRef, 1)
	mgr.observeGate = make(chan struct{})
	observeEntered := mgr.observeEntered
	observeGate := mgr.observeGate
	mgr.mu.Unlock()
	crashResult := make(chan []sessionEvent, 1)
	go func() { crashResult <- svc.checkCrashes() }()
	var observed runner.AllocationRef
	select {
	case observed = <-observeEntered:
	case <-time.After(time.Second):
		t.Fatal("crash observation did not start")
	}
	if observed.Session.Generation != 1 {
		t.Fatalf("observed generation = %d, want 1", observed.Session.Generation)
	}

	if err := svc.ResetEnvironment(context.Background(), "user-1"); err != nil {
		t.Fatalf("reset while old observation pending: %v", err)
	}
	mgr.mu.Lock()
	mgr.running = true
	mgr.mu.Unlock()
	close(observeGate)
	if crashed := <-crashResult; len(crashed) != 0 {
		t.Fatalf("stale observation reported current session crashed: %v", crashed)
	}
	sess := waitForStatus(t, svc, "user-1", StatusReady)
	sess.mu.Lock()
	generation := sess.Generation
	allocationGeneration := sess.Allocation.Session.Generation
	sess.mu.Unlock()
	if generation != 2 || allocationGeneration != 2 {
		t.Fatalf("stale crash changed reset generation: session=%d allocation=%d", generation, allocationGeneration)
	}
}

func TestVerifySuccessSerializesBeforeTimeout(t *testing.T) {
	svc, mgr, store := newTestService()
	store.problems["p1"] = &models.Problem{ID: "p1", TimeoutMinutes: 30, VerifyType: "script"}
	if _, err := svc.StartProblem(context.Background(), "user-1", "p1"); err != nil {
		t.Fatal(err)
	}
	sess := waitForStatus(t, svc, "user-1", StatusReady)
	sess.mu.Lock()
	sess.TimeoutAt = time.Now().Add(-time.Second)
	attemptID := sess.AttemptID
	sess.mu.Unlock()

	mgr.verifyEntered = make(chan runner.AllocationRef, 1)
	mgr.verifyGate = make(chan struct{})
	verifyResult := make(chan error, 1)
	go func() {
		success, _, err := svc.Verify(context.Background(), "user-1", "operation-timeout-race")
		if err == nil && !success {
			err = errors.New("verify unexpectedly failed")
		}
		verifyResult <- err
	}()
	select {
	case <-mgr.verifyEntered:
	case <-time.After(time.Second):
		t.Fatal("verify did not reach Runner")
	}

	timeoutResult := make(chan []sessionEvent, 1)
	go func() { timeoutResult <- svc.checkTimeouts() }()
	// Verify holds the per-user transition lock. Give checkTimeouts enough time
	// to capture its stale candidate and block on that same lock.
	time.Sleep(20 * time.Millisecond)
	close(mgr.verifyGate)
	if err := <-verifyResult; err != nil {
		t.Fatalf("verify success: %v", err)
	}
	if expired := <-timeoutResult; len(expired) != 0 {
		t.Fatalf("timeout overrode completed verify: %v", expired)
	}
	if got := attemptStatus(t, store, attemptID); got != "success" {
		t.Fatalf("attempt status = %s, want success", got)
	}
	if svc.GetSession("user-1") != nil {
		t.Fatal("completed session must be removed")
	}
}

func TestTerminalCleanupSkipsContendedUserAndServesNextUser(t *testing.T) {
	svc, mgr, store := newTestService()
	store.problems["p1"] = &models.Problem{ID: "p1", TimeoutMinutes: 30, VerifyType: "script"}
	for _, userID := range []string{"user-a", "user-b"} {
		if _, err := svc.StartProblem(context.Background(), userID, "p1"); err != nil {
			t.Fatalf("start %s: %v", userID, err)
		}
		waitForStatus(t, svc, userID, StatusReady)
		sess := svc.GetSession(userID)
		svc.setTerminalIntent(sess, StatusFailed, "failed", "cleanup fairness test")
	}

	aLock := svc.userLock("user-a")
	aLock.Lock()
	svc.retryTerminalCleanupContext(context.Background())
	aLock.Unlock()

	if svc.GetSession("user-a") == nil {
		t.Fatal("contended user-a should remain pending for the next sweep")
	}
	if svc.GetSession("user-b") != nil {
		t.Fatal("uncontended user-b cleanup was blocked behind user-a")
	}
	mgr.mu.Lock()
	removed := append([]runner.AllocationRef(nil), mgr.removed...)
	mgr.mu.Unlock()
	if len(removed) != 1 || removed[0].Session.SessionID == svc.GetSession("user-a").ID {
		t.Fatalf("cleanup effects = %+v, want only user-b", removed)
	}

	svc.retryTerminalCleanupContext(context.Background())
	if svc.GetSession("user-a") != nil {
		t.Fatal("user-a did not converge after its lock was released")
	}
}
