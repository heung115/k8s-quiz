package session

import (
	"strings"
	"context"
	"fmt"
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

type Session struct {
	ID          string
	UserID      string
	ProblemID   string
	AttemptID   string
	ContainerID string
	Status      Status
	StartedAt   time.Time
	TimeoutAt   time.Time
	mu          sync.Mutex
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
	containerMgr container.Manager
	problemStore ProblemStore
	scripts      ScriptProvider
	sessions     map[string]*Session
	mu           sync.RWMutex
	userMu       sync.Map // userID -> *sync.Mutex, serializes start/reset/end per user
	onStage      StageCallback
	onTimeout    func(userID string)
	onTimeoutWarn func(userID string, remainingSeconds int)
	onVerify     VerifyCallback
	onCrash      func(userID string)
	grader       Grader
	pool         *container.Pool
}

func NewService(mgr container.Manager, store ProblemStore, scripts ScriptProvider) *Service {
	s := &Service{
		containerMgr: mgr,
		problemStore: store,
		scripts:      scripts,
		sessions:     make(map[string]*Session),
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

	s.mu.Lock()
	existing := s.sessions[userID]
	s.mu.Unlock()
	if existing != nil {
		s.endSessionLocked(context.Background(), userID)
	}

	p, err := s.problemStore.FindByID(ctx, problemID)
	if err != nil {
		return "", fmt.Errorf("problem not found: %w", err)
	}

	attempt := &models.Attempt{
		UserID:    userID,
		ProblemID: problemID,
		Status:    "in_progress",
		StartedAt: time.Now(),
	}
	if err := s.problemStore.CreateAttempt(ctx, attempt); err != nil {
		return "", fmt.Errorf("create attempt: %w", err)
	}

	image := p.BaseImage
	if p.Image != "" {
		image = p.Image
	}

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
	s.mu.Unlock()

	go s.setupEnvironment(sess, p, attempt, preWarmed)

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

	s.containerMgr.Remove(ctx, sess.ContainerID)

	p, err := s.problemStore.FindByID(ctx, sess.ProblemID)
	if err != nil {
		return err
	}

	image := p.BaseImage
	if p.Image != "" {
		image = p.Image
	}

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
	sess.mu.Unlock()

	attempt, _ := s.problemStore.GetAttempt(ctx, sess.AttemptID)
	go s.setupEnvironment(sess, p, attempt, false)

	return nil
}

func (s *Service) userLock(userID string) *sync.Mutex {
	v, _ := s.userMu.LoadOrStore(userID, &sync.Mutex{})
	return v.(*sync.Mutex)
}

func (s *Service) EndSession(ctx context.Context, userID string) {
	ul := s.userLock(userID)
	ul.Lock()
	defer ul.Unlock()
	s.endSessionLocked(ctx, userID)
}

// endSessionLocked tears down a session; the caller must hold the per-user lock.
func (s *Service) endSessionLocked(ctx context.Context, userID string) {
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
			attempt.Status = "failed"
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

func (s *Service) timeoutWatcher() {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		s.mu.RLock()
		var expired []string
		for userID, sess := range s.sessions {
			sess.mu.Lock()
			if time.Now().After(sess.TimeoutAt) && sess.Status != StatusCompleted {
				sess.Status = StatusTimeout
				expired = append(expired, userID)
			}
			remaining := time.Until(sess.TimeoutAt)
			if remaining > 0 && remaining < 5*time.Minute && sess.Status == StatusReady {
				if s.onTimeoutWarn != nil {
					s.onTimeoutWarn(userID, int(remaining.Seconds()))
				}
			}
			sess.mu.Unlock()
		}
		s.mu.RUnlock()

		for _, userID := range expired {
			s.EndSession(context.Background(), userID)
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
