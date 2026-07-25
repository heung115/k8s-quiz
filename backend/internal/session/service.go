package session

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/k8s-quiz/backend/internal/container"
	"github.com/k8s-quiz/backend/pkg/models"
)

type Status string

const (
	StatusCreating  Status = "creating"
	StatusBooting   Status = "booting"
	StatusSettingUp Status = "setting_up"
	StatusReady     Status = "ready"
	StatusVerifying Status = "verifying"
	StatusCompleted Status = "completed"
	StatusFailed    Status = "failed"
	StatusTimeout   Status = "timeout"
)

// Sentinel errors mapped to HTTP 429 by the problem handler (SESS-3).
var (
	ErrTooManySessions = errors.New("too many concurrent sessions")
	ErrStartCooldown   = errors.New("start cooldown active")
	ErrVerifyTooFast   = errors.New("verify cooldown active") // SEC3-4
)

// startCooldown throttles per-user session starts (SESS-3); verifyThrottle
// bounds how often a user may start verification (SEC3-4: verify.sh execs
// are expensive).
const (
	defaultStartCooldown  = 3 * time.Second
	defaultVerifyThrottle = 2 * time.Second
)

type Session struct {
	ID          string
	UserID      string
	ProblemID   string
	AttemptID   string
	ContainerID string
	Status      Status
	StartedAt   time.Time
	TimeoutAt   time.Time
	// timeoutWarned makes timeout_warning fire ONCE per session (WS-3);
	// reset whenever the environment is (re)started.
	timeoutWarned bool
	mu            sync.Mutex
}

type StageCallback func(userID, stage, message string)
type VerifyCallback func(userID string, success bool, log string)

type ProblemStore interface {
	FindByID(ctx context.Context, id string) (*models.Problem, error)
	CreateAttempt(ctx context.Context, a *models.Attempt) error
	UpdateAttempt(ctx context.Context, a *models.Attempt) error
	GetAttempt(ctx context.Context, id string) (*models.Attempt, error)
}

type ScriptProvider interface {
	HasSetupScript(problemID string) bool
	HasVerifyScript(problemID string) bool
	GetSetupScript(problemID string) (string, error)
	GetVerifyScript(problemID string) (string, error)
}

type Grader interface {
	Grade(ctx context.Context, rubric, evidence string) (bool, string, error)
}

type Service struct {
	containerMgr   container.Manager
	problemStore   ProblemStore
	scripts        ScriptProvider
	sessions       map[string]*Session
	mu             sync.RWMutex
	userMu         sync.Map             // userID -> *sync.Mutex, serializes start/reset/end per user
	maxSessions    int                  // SESS-3: 0 = unlimited
	startCooldown  time.Duration        // SESS-3/SEC3-5: per-user start+reset throttle
	lastStart      map[string]time.Time // SESS-3: last successful start/reset per user
	pendingStarts  int                  // T3: in-flight starts reserved against the cap
	verifyThrottle time.Duration        // SEC3-4: min gap between verify starts
	lastVerify     map[string]time.Time // SEC3-4: last verify start per user
	onStage        StageCallback
	onTimeout      func(userID string)
	onTimeoutWarn  func(userID string, remainingSeconds int)
	onVerify       VerifyCallback
	onCrash        func(userID string)
	onReset        func(userID string)
	grader         Grader
	pool           *container.Pool
}

func NewService(mgr container.Manager, store ProblemStore, scripts ScriptProvider) *Service {
	s := &Service{
		containerMgr:   mgr,
		problemStore:   store,
		scripts:        scripts,
		sessions:       make(map[string]*Session),
		startCooldown:  defaultStartCooldown,
		lastStart:      make(map[string]time.Time),
		verifyThrottle: defaultVerifyThrottle,
		lastVerify:     make(map[string]time.Time),
	}
	go s.timeoutWatcher()
	go s.crashWatcher()
	return s
}

func (s *Service) SetStageCallback(cb StageCallback) {
	s.onStage = cb
}

func (s *Service) SetTimeoutCallback(cb func(string)) {
	s.onTimeout = cb
}

func (s *Service) SetTimeoutWarningCallback(cb func(string, int)) {
	s.onTimeoutWarn = cb
}

func (s *Service) SetVerifyCallback(cb VerifyCallback) {
	s.onVerify = cb
}

func (s *Service) SetCrashCallback(cb func(string)) {
	s.onCrash = cb
}

// SetResetCallback wires the session_ended{reason:"reset"} WS event (WS-4).
func (s *Service) SetResetCallback(cb func(string)) {
	s.onReset = cb
}

// SetMaxConcurrentSessions caps total active sessions; 0 = unlimited (SESS-3).
func (s *Service) SetMaxConcurrentSessions(n int) {
	s.mu.Lock()
	s.maxSessions = n
	s.mu.Unlock()
}

// SetStartCooldown overrides the per-user start/reset throttle (SESS-3; tests).
func (s *Service) SetStartCooldown(d time.Duration) {
	s.mu.Lock()
	s.startCooldown = d
	s.mu.Unlock()
}

// SetVerifyThrottle overrides the per-user verify throttle (SEC3-4; tests).
func (s *Service) SetVerifyThrottle(d time.Duration) {
	s.mu.Lock()
	s.verifyThrottle = d
	s.mu.Unlock()
}

func (s *Service) SetGrader(g Grader) {
	s.grader = g
}

func (s *Service) SetPool(p *container.Pool) {
	s.pool = p
}

func (s *Service) StartProblem(ctx context.Context, userID, problemID string) (string, error) {
	ul := s.userLock(userID)
	ul.Lock()
	defer ul.Unlock()

	// SESS-3: per-user start cooldown, then global concurrency cap. The
	// user's own existing session does not count against the cap (a start
	// replaces it, keeping the total unchanged).
	now := time.Now()
	reserved := false
	s.mu.Lock()
	if last, ok := s.lastStart[userID]; ok && now.Sub(last) < s.startCooldown {
		s.mu.Unlock()
		return "", ErrStartCooldown
	}
	if s.maxSessions > 0 {
		// Count in-flight starts (pendingStarts) too, otherwise concurrent
		// starts can all pass the check before any session lands in the map
		// (TOCTOU → cap exceeded). The user's own existing session does not
		// count: a start replaces it, keeping the total unchanged.
		active := len(s.sessions) + s.pendingStarts
		if _, hasOwn := s.sessions[userID]; hasOwn {
			active--
		}
		if active >= s.maxSessions {
			s.mu.Unlock()
			return "", ErrTooManySessions
		}
		s.pendingStarts++
		reserved = true
	}
	s.mu.Unlock()
	releaseReservation := func() {
		if reserved {
			s.mu.Lock()
			s.pendingStarts--
			s.mu.Unlock()
		}
	}

	s.mu.Lock()
	existing := s.sessions[userID]
	s.mu.Unlock()
	if existing != nil {
		s.endSessionLocked(context.Background(), userID, "failed")
	}

	p, err := s.problemStore.FindByID(ctx, problemID)
	if err != nil {
		releaseReservation()
		return "", fmt.Errorf("problem not found: %w", err)
	}

	attempt := &models.Attempt{
		UserID:    userID,
		ProblemID: problemID,
		Status:    "in_progress",
		StartedAt: time.Now(),
	}
	if err := s.problemStore.CreateAttempt(ctx, attempt); err != nil {
		releaseReservation()
		return "", fmt.Errorf("create attempt: %w", err)
	}

	image := p.EffectiveImage()

	s.emitStage(userID, "container_created", "Creating container...")

	var containerID string
	preWarmed := false
	if s.pool != nil {
		if id, ok := s.pool.Acquire(); ok {
			containerID = id
			preWarmed = true
		}
	}
	if containerID == "" {
		var err error
		containerID, err = s.containerMgr.Create(ctx, container.CreateOpts{
			Image: image,
			Labels: map[string]string{
				"k8s-quiz":         "true",
				"k8s-quiz.user":    userID,
				"k8s-quiz.problem": problemID,
			},
			CPULimit:    1_000_000_000,
			MemoryLimit: 1073741824,
			Privileged:  true,
			NetworkMode: "k8s-quiz-" + userID,
		})
		if err != nil {
			s.failAttempt(ctx, attempt, "container creation failed: "+err.Error())
			releaseReservation()
			return "", fmt.Errorf("create container: %w", err)
		}
	}

	timeout := time.Duration(p.TimeoutMinutes) * time.Minute
	sess := &Session{
		ID:          attempt.ID,
		UserID:      userID,
		ProblemID:   problemID,
		AttemptID:   attempt.ID,
		ContainerID: containerID,
		Status:      StatusBooting,
		StartedAt:   time.Now(),
		TimeoutAt:   time.Now().Add(timeout),
	}

	s.mu.Lock()
	s.sessions[userID] = sess
	if reserved {
		s.pendingStarts-- // reservation becomes the real session
	}
	s.mu.Unlock()

	go s.setupEnvironment(sess, p, attempt, preWarmed)

	s.mu.Lock()
	s.lastStart[userID] = time.Now()
	s.mu.Unlock()

	return sess.ID, nil
}

func (s *Service) setupEnvironment(sess *Session, p *models.Problem, attempt *models.Attempt, preWarmed bool) {
	ctx := context.Background()

	if !preWarmed {
		s.emitStage(sess.UserID, "k3s_booting", "Waiting for k3s to boot...")

		err := s.containerMgr.WaitReady(ctx, sess.ContainerID, func() bool {
			result, err := s.containerMgr.Exec(ctx, sess.ContainerID, []string{"kubectl", "get", "nodes", "-o", `jsonpath={.items[0].status.conditions[?(@.type=="Ready")].status}`})
			return err == nil && result.ExitCode == 0 && strings.TrimSpace(result.Stdout) == "True"
		}, 120*time.Second)
		if err != nil {
			sess.mu.Lock()
			sess.Status = StatusFailed
			sess.mu.Unlock()
			s.failAttempt(ctx, attempt, "k3s node did not become Ready")
			return
		}
	}

	if s.scripts.HasSetupScript(p.ID) {
		s.emitStage(sess.UserID, "setup_running", "Setting up problem environment...")
		sess.mu.Lock()
		sess.Status = StatusSettingUp
		sess.mu.Unlock()

		script, err := s.scripts.GetSetupScript(p.ID)
		if err == nil {
			result, err := s.containerMgr.Exec(ctx, sess.ContainerID, []string{"/bin/sh", "-c", script})
			if err != nil || result.ExitCode != 0 {
				sess.mu.Lock()
				sess.Status = StatusFailed
				sess.mu.Unlock()
				errMsg := "setup failed"
				if err != nil {
					errMsg = err.Error()
				} else {
					errMsg = result.Stderr
				}
				s.failAttempt(ctx, attempt, errMsg)
				return
			}
		}
	}

	sess.mu.Lock()
	sess.Status = StatusReady
	sess.mu.Unlock()
	s.emitStage(sess.UserID, "ready", "Environment ready. Good luck!")
}

func (s *Service) Verify(ctx context.Context, userID string) (bool, string, error) {
	sess := s.GetSession(userID)
	if sess == nil {
		return false, "", fmt.Errorf("no active session")
	}

	// SEC3-4: at most one verify start per user per throttle window
	// (verify.sh execs are expensive; this is a DoS guard, not a lock —
	// concurrent verifies are still serialized by the status flip below).
	now := time.Now()
	s.mu.Lock()
	if last, ok := s.lastVerify[userID]; ok && now.Sub(last) < s.verifyThrottle {
		s.mu.Unlock()
		return false, "", ErrVerifyTooFast
	}
	s.lastVerify[userID] = now
	s.mu.Unlock()

	sess.mu.Lock()
	if sess.Status != StatusReady {
		sess.mu.Unlock()
		return false, "", fmt.Errorf("session not ready")
	}
	sess.Status = StatusVerifying
	sess.mu.Unlock()

	p, err := s.problemStore.FindByID(ctx, sess.ProblemID)
	if err != nil {
		return false, "", err
	}

	var success bool
	var verifyLog string

	// Bound the verification exec so a hanging verify.sh (or text grader)
	// cannot tie up the handler indefinitely (DoS guard).
	vctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	if p.VerifyType == "script" {
		script, err := s.scripts.GetVerifyScript(p.ID)
		if err != nil {
			return false, "", fmt.Errorf("verify script not found")
		}
		result, err := s.containerMgr.Exec(vctx, sess.ContainerID, []string{"/bin/sh", "-c", script})
		if err != nil {
			return false, "", fmt.Errorf("verify exec failed: %w", err)
		}
		success = result.ExitCode == 0
		verifyLog = result.Stdout + result.Stderr
	} else if p.VerifyType == "text" {
		if s.grader == nil {
			return false, "", fmt.Errorf("text grading is not configured")
		}
		evidence, err := s.collectEvidence(vctx, sess.ContainerID)
		if err != nil {
			return false, "", fmt.Errorf("collect evidence: %w", err)
		}
		graded, reason, err := s.grader.Grade(ctx, p.GradingPrompt, evidence)
		if err != nil {
			return false, "", fmt.Errorf("grading failed: %w", err)
		}
		success = graded
		verifyLog = reason
	}

	sess.mu.Lock()
	if success {
		sess.Status = StatusCompleted
	} else {
		sess.Status = StatusReady
	}
	sess.mu.Unlock()

	attempt, _ := s.problemStore.GetAttempt(ctx, sess.AttemptID)
	if attempt != nil {
		now := time.Now()
		duration := int(now.Sub(attempt.StartedAt).Seconds())
		attempt.VerifyLog = verifyLog
		if success {
			attempt.Status = "success"
			attempt.FinishedAt = &now
			attempt.DurationSeconds = &duration
		}
		s.problemStore.UpdateAttempt(ctx, attempt)
	}

	if s.onVerify != nil {
		s.onVerify(userID, success, verifyLog)
	}

	return success, verifyLog, nil
}

func (s *Service) SubmitChoice(ctx context.Context, userID, choiceID string) (bool, error) {
	sess := s.GetSession(userID)
	if sess == nil {
		return false, fmt.Errorf("no active session")
	}

	p, err := s.problemStore.FindByID(ctx, sess.ProblemID)
	if err != nil {
		return false, err
	}

	success := p.CorrectChoice == choiceID

	if success {
		attempt, _ := s.problemStore.GetAttempt(ctx, sess.AttemptID)
		if attempt != nil {
			now := time.Now()
			duration := int(now.Sub(attempt.StartedAt).Seconds())
			attempt.Status = "success"
			attempt.FinishedAt = &now
			attempt.DurationSeconds = &duration
			s.problemStore.UpdateAttempt(ctx, attempt)
		}
		sess.mu.Lock()
		sess.Status = StatusCompleted
		sess.mu.Unlock()
	}

	return success, nil
}

func (s *Service) ResetEnvironment(ctx context.Context, userID string) error {
	ul := s.userLock(userID)
	ul.Lock()
	defer ul.Unlock()

	sess := s.GetSession(userID)
	if sess == nil {
		return fmt.Errorf("no active session")
	}

	// SEC3-5: a reset recreates the environment, so it honors the same
	// per-user cooldown as start.
	now := time.Now()
	s.mu.Lock()
	if last, ok := s.lastStart[userID]; ok && now.Sub(last) < s.startCooldown {
		s.mu.Unlock()
		return ErrStartCooldown
	}
	s.mu.Unlock()

	// WS-4: tell the client the current environment is going away BEFORE the
	// new container's boot stages start (the session itself stays active).
	if s.onReset != nil {
		s.onReset(userID)
	}

	s.containerMgr.Remove(ctx, sess.ContainerID)

	p, err := s.problemStore.FindByID(ctx, sess.ProblemID)
	if err != nil {
		return err
	}

	image := p.EffectiveImage()

	containerID, err := s.containerMgr.Create(ctx, container.CreateOpts{
		Image: image,
		Labels: map[string]string{
			"k8s-quiz":         "true",
			"k8s-quiz.user":    userID,
			"k8s-quiz.problem": sess.ProblemID,
		},
		CPULimit:    1_000_000_000,
		MemoryLimit: 1073741824,
		Privileged:  true,
		NetworkMode: "k8s-quiz-" + userID,
	})
	if err != nil {
		return fmt.Errorf("recreate container: %w", err)
	}

	sess.mu.Lock()
	sess.ContainerID = containerID
	sess.Status = StatusBooting
	sess.timeoutWarned = false // WS-3: warn once per environment
	sess.mu.Unlock()

	attempt, _ := s.problemStore.GetAttempt(ctx, sess.AttemptID)
	go s.setupEnvironment(sess, p, attempt, false)

	s.mu.Lock()
	s.lastStart[userID] = time.Now()
	s.mu.Unlock()

	return nil
}

func (s *Service) userLock(userID string) *sync.Mutex {
	v, _ := s.userMu.LoadOrStore(userID, &sync.Mutex{})
	return v.(*sync.Mutex)
}

func (s *Service) EndSession(ctx context.Context, userID string) {
	s.endSession(ctx, userID, "failed")
}

// endSession tears down the user's session, recording attemptStatus on the
// in-progress attempt: "failed" for user-initiated end / start-replacement,
// "timeout" when the timeout watcher expires the session. The caller must NOT
// hold the per-user lock.
func (s *Service) endSession(ctx context.Context, userID, attemptStatus string) {
	ul := s.userLock(userID)
	ul.Lock()
	defer ul.Unlock()
	s.endSessionLocked(ctx, userID, attemptStatus)
}

// endSessionLocked tears down a session; the caller must hold the per-user lock.
func (s *Service) endSessionLocked(ctx context.Context, userID, attemptStatus string) {
	s.mu.Lock()
	sess, ok := s.sessions[userID]
	if ok {
		delete(s.sessions, userID)
	}
	s.mu.Unlock()

	if sess != nil {
		s.containerMgr.Remove(ctx, sess.ContainerID)
		attempt, _ := s.problemStore.GetAttempt(ctx, sess.AttemptID)
		if attempt != nil && attempt.Status == "in_progress" {
			now := time.Now()
			duration := int(now.Sub(attempt.StartedAt).Seconds())
			attempt.Status = attemptStatus
			attempt.FinishedAt = &now
			attempt.DurationSeconds = &duration
			s.problemStore.UpdateAttempt(ctx, attempt)
		}
	}
}

func (s *Service) GetSession(userID string) *Session {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.sessions[userID]
}

// CurrentSession is the user-facing snapshot of an active session. It is the
// response shape of GET /api/sessions/current and POST /api/problems/:id/start.
type CurrentSession struct {
	SessionID string    `json:"session_id"`
	ProblemID string    `json:"problem_id"`
	Status    Status    `json:"status"`
	TimeoutAt time.Time `json:"timeout_at"`
}

// GetCurrentSession snapshots the user's active session, or nil if there is
// none. It takes only the session lock, so it is safe to call right after
// StartProblem returns (the per-user lock is released by then).
func (s *Service) GetCurrentSession(userID string) *CurrentSession {
	sess := s.GetSession(userID)
	if sess == nil {
		return nil
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	return &CurrentSession{
		SessionID: sess.ID,
		ProblemID: sess.ProblemID,
		Status:    sess.Status,
		TimeoutAt: sess.TimeoutAt,
	}
}

func (s *Service) CleanupAll(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for userID, sess := range s.sessions {
		s.containerMgr.Remove(ctx, sess.ContainerID)
		delete(s.sessions, userID)
	}
}

func (s *Service) collectEvidence(ctx context.Context, containerID string) (string, error) {
	result, err := s.containerMgr.Exec(ctx, containerID, []string{"kubectl", "get", "all", "--all-namespaces"})
	if err != nil {
		return "", err
	}
	return result.Stdout + result.Stderr, nil
}

func (s *Service) checkCrashes() []string {
	s.mu.RLock()
	var crashed []string
	for userID, sess := range s.sessions {
		sess.mu.Lock()
		active := sess.Status == StatusReady || sess.Status == StatusBooting || sess.Status == StatusSettingUp || sess.Status == StatusVerifying
		if active {
			running, err := s.containerMgr.IsRunning(context.Background(), sess.ContainerID)
			if err == nil && !running {
				sess.Status = StatusFailed
				crashed = append(crashed, userID)
			}
		}
		sess.mu.Unlock()
	}
	s.mu.RUnlock()
	return crashed
}

func (s *Service) crashWatcher() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		for _, userID := range s.checkCrashes() {
			s.emitStage(userID, "container_crashed", "Container stopped unexpectedly. Reset the environment to continue.")
			if s.onCrash != nil {
				s.onCrash(userID)
			}
		}
	}
}

// checkTimeouts expires every session past its deadline, persisting attempt
// status "timeout" and tearing the container down. It returns the expired
// userIDs so the watcher can fire the session_ended{reason:"timeout"} callback.
func (s *Service) checkTimeouts() []string {
	s.mu.RLock()
	var expired []string
	for userID, sess := range s.sessions {
		sess.mu.Lock()
		if time.Now().After(sess.TimeoutAt) && sess.Status != StatusCompleted {
			sess.Status = StatusTimeout
			expired = append(expired, userID)
		}
		remaining := time.Until(sess.TimeoutAt)
		if remaining > 0 && remaining < 5*time.Minute && sess.Status == StatusReady && !sess.timeoutWarned {
			sess.timeoutWarned = true // WS-3: fire ONCE per session
			if s.onTimeoutWarn != nil {
				s.onTimeoutWarn(userID, int(remaining.Seconds()))
			}
		}
		sess.mu.Unlock()
	}
	s.mu.RUnlock()

	for _, userID := range expired {
		s.endSession(context.Background(), userID, "timeout")
	}
	return expired
}

func (s *Service) timeoutWatcher() {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		for _, userID := range s.checkTimeouts() {
			if s.onTimeout != nil {
				s.onTimeout(userID)
			}
		}
	}
}

func (s *Service) failAttempt(ctx context.Context, attempt *models.Attempt, log string) {
	now := time.Now()
	duration := int(now.Sub(attempt.StartedAt).Seconds())
	attempt.Status = "failed"
	attempt.FinishedAt = &now
	attempt.DurationSeconds = &duration
	attempt.VerifyLog = log
	s.problemStore.UpdateAttempt(ctx, attempt)
}

func (s *Service) emitStage(userID, stage, message string) {
	if s.onStage != nil {
		s.onStage(userID, stage, message)
	}
}
