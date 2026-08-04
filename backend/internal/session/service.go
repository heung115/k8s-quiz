package session

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log"
	"reflect"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/k8s-quiz/backend/internal/runner"
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
	ErrTooManySessions      = errors.New("too many concurrent sessions")
	ErrStartCooldown        = errors.New("start cooldown active")
	ErrVerifyTooFast        = errors.New("verify cooldown active") // SEC3-4
	ErrVerifyInfrastructure = errors.New("verification infrastructure failure")
	ErrServiceStopping      = errors.New("session service is stopping")
	// ErrTransitionBusy bounds lifecycle admission to one running transition
	// per user and no server-side wait queue. The caller may retry the same
	// idempotency key after the current transition finishes.
	ErrTransitionBusy = errors.New("session transition already in progress")
	// ErrCleanupPending means durable desired-absent intent is committed and an
	// exact idempotent destroy is already running or queued. It is accepted by
	// the HTTP end-session path, but is not absence proof for reset/replacement.
	ErrCleanupPending = errors.New("session cleanup is pending")
)

// startCooldown throttles per-user session starts (SESS-3); verifyThrottle
// bounds how often a user may start verification (SEC3-4: verify.sh execs
// are expensive).
const (
	defaultStartCooldown   = 3 * time.Second
	defaultVerifyThrottle  = 2 * time.Second
	verifyOperationTTL     = 30 * time.Minute
	verifyOperationLimit   = 2048
	durableDestroyLease    = 30 * time.Second
	durableDestroyTimeout  = 20 * time.Second
	durableCreateLease     = 2 * time.Minute
	durableCreateTimeout   = 90 * time.Second
	durableLeaseCommitGap  = 5 * time.Second
	durableWorkBatch       = 16
	shutdownDestroyWorkers = 4
)

type verifyOperation struct {
	sessionID  string
	generation uint64
	done       chan struct{}
	success    bool
	log        string
	err        error
	completed  time.Time
}

type Session struct {
	ID            string
	UserID        string
	ProblemID     string
	Selection     runner.CatalogSelection
	VerifyType    string
	CorrectChoice string
	// GradingPrompt is captured with the approved problem revision so text
	// verification cannot mix create-time evidence with a later edited rubric.
	GradingPrompt string
	AttemptID     string
	OperationID   string
	Generation    uint64
	Allocation    runner.AllocationRef
	Status        Status
	StartedAt     time.Time
	TimeoutAt     time.Time
	// Terminal intent is retained until persistence and exact allocation
	// cleanup both succeed. The cleanup watcher retries this state machine.
	terminalAttemptStatus string
	terminalVerifyLog     string
	// terminalIneligible is an in-process authority fence. Durable desired-
	// absent may commit before provider cleanup or local eviction completes;
	// terminal attaches must fail closed throughout that interval without
	// reclassifying an already-durable terminal outcome.
	terminalIneligible bool
	// timeoutWarned makes timeout_warning fire ONCE per session (WS-3);
	// reset whenever the environment is (re)started.
	timeoutWarned bool
	// resetNotifiedGeneration prevents the request path and the durable worker
	// from publishing the same process-local generation advance twice. A
	// replacement generation is announced before any of its setup stages.
	resetNotifiedGeneration uint64
	setupCancel             context.CancelFunc
	mu                      sync.Mutex
}

type StageCallback func(userID string, session runner.SessionRef, stage, message string)
type VerifyCallback func(userID string, session runner.SessionRef, success bool, log string)
type SessionCallback func(userID string, session runner.SessionRef)
type EndedCallback func(userID string, session runner.SessionRef, reason string)

type ProblemStore interface {
	FindByID(ctx context.Context, id string) (*models.Problem, error)
	CreateAttempt(ctx context.Context, a *models.Attempt) error
	UpdateAttempt(ctx context.Context, a *models.Attempt) error
	GetAttempt(ctx context.Context, id string) (*models.Attempt, error)
}

type Grader interface {
	Grade(ctx context.Context, rubric, evidence string) (bool, string, error)
}

// TrustedProblemCatalog resolves grading metadata from the same immutable
// problem bundle whose revision is passed to the Runner. The database is a
// query/index mirror and is not authoritative for answers or rubrics.
type TrustedProblemCatalog interface {
	ResolveProblem(context.Context, runner.ProblemRef) (models.Problem, error)
}

// ActiveProblemCatalog serializes a fresh start against runtime catalog
// publication. The caller must hold release through durable reservation commit.
// Historical replay/reset/recovery continues through TrustedProblemCatalog.
type ActiveProblemCatalog interface {
	AcquireActiveProblem(context.Context, string) (models.Problem, runner.CatalogSelection, func(), error)
}

type DurableProblemCatalog interface {
	TrustedProblemCatalog
	ActiveProblemCatalog
}

// DurableLifecycleStore is the PostgreSQL desired-state authority used by the
// real HTTP start path. Provider calls happen only after ReserveSession commits.
// The legacy StartProblem method remains for isolated unit tests and is never
// wired by the server.
type DurableLifecycleStore interface {
	BootstrapLifecycle(context.Context, string, *runner.LifecycleCursor) (runner.LifecycleBootstrap, error)
	FindReservation(context.Context, string, string) (runner.SessionReservation, bool, error)
	ListActiveReservations(context.Context, string) ([]runner.SessionReservation, error)
	PrepareRecoveryWork(context.Context, string) error
	ReserveSession(context.Context, runner.ReserveSessionParams) (runner.SessionReservation, error)
	ReserveReset(context.Context, runner.ReserveResetParams) (runner.ResetReservation, error)
	MarkCreateSucceeded(context.Context, runner.AllocationRef) error
	MarkClaimedCreateSucceeded(context.Context, runner.CreateWorkClaim) error
	MarkSettingUp(context.Context, runner.AllocationRef) error
	MarkReady(context.Context, runner.AllocationRef) error
	LookupVerify(context.Context, string, string) (runner.VerifyDecision, bool, error)
	BeginVerify(context.Context, string, runner.AllocationRef, string) (runner.VerifyDecision, error)
	RecordVerifyFinished(context.Context, runner.AllocationRef, string, bool, string) error
	RecordVerifyInfrastructureFailure(context.Context, runner.AllocationRef, string, string) error
	LookupChoice(context.Context, string, runner.ChoiceSubmission) (runner.VerifyDecision, bool, error)
	RecordChoice(context.Context, string, runner.AllocationRef, runner.ChoiceSubmission, bool, string) (runner.VerifyDecision, error)
	RequestEnd(context.Context, string, runner.SessionRef, string) (runner.EndDecision, error)
	MarkCreateFailed(context.Context, runner.AllocationRef, string, string) error
	MarkClaimedCreateFailed(context.Context, runner.CreateWorkClaim, string, string) error
	RequestDestroy(context.Context, runner.AllocationRef, string, string, string) error
	MarkDestroyed(context.Context, runner.AllocationRef) error
	ClaimDestroyWork(context.Context, string, string, string, time.Duration) (runner.DestroyWorkClaim, bool, error)
	RetryDestroyWork(context.Context, runner.DestroyWorkClaim, error, time.Duration) error
	MarkClaimedDestroyed(context.Context, runner.DestroyWorkClaim) error
	FindProvisionableReplacement(context.Context, runner.AllocationRef) (runner.SessionReservation, bool, error)
	ClaimProvisionableCreate(context.Context, string, string, string, time.Duration) (runner.CreateWorkClaim, bool, error)
	ClaimProvisionableReset(context.Context, string, string, string, time.Duration) (runner.CreateWorkClaim, bool, error)
}

// userTransitionLock has Mutex-compatible Lock/Unlock plus a cancellable wait
// used by bounded shutdown and workers. Provider calls may still take time,
// but a cleanup deadline is no longer defeated before it acquires the lock.
type userTransitionLock struct {
	token chan struct{}
}

func newUserTransitionLock() *userTransitionLock {
	lock := &userTransitionLock{token: make(chan struct{}, 1)}
	lock.token <- struct{}{}
	return lock
}

func (l *userTransitionLock) Lock() {
	<-l.token
}

func (l *userTransitionLock) LockContext(ctx context.Context) error {
	select {
	case <-l.token:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// TryLock lets background sweeps skip a contended user and continue serving
// unrelated users. Request and shutdown paths use LockContext when they must
// wait for a specific transition.
func (l *userTransitionLock) TryLock() bool {
	select {
	case <-l.token:
		return true
	default:
		return false
	}
}

func (l *userTransitionLock) Unlock() {
	l.token <- struct{}{}
}

type Service struct {
	runner          runner.Runner
	problemStore    ProblemStore
	problemCatalog  TrustedProblemCatalog
	activeCatalog   ActiveProblemCatalog
	durableStore    DurableLifecycleStore
	authority       *runner.AuthorityGate
	providerID      string
	workerOwner     string
	sessions        map[string]*Session
	mu              sync.RWMutex
	userMu          sync.Map             // userID -> *userTransitionLock, serializes start/reset/end per user
	maxSessions     int                  // SESS-3: 0 = unlimited
	startCooldown   time.Duration        // SESS-3/SEC3-5: per-user start+reset throttle
	lastStart       map[string]time.Time // SESS-3: last successful start/reset per user
	pendingStarts   int                  // T3: in-flight starts reserved against the cap
	verifyThrottle  time.Duration        // SEC3-4: min gap between verify starts
	lastVerify      map[string]time.Time // SEC3-4: last verify start per user
	verifyOps       map[string]*verifyOperation
	onStage         StageCallback
	onTimeout       SessionCallback
	onTimeoutWarn   func(userID string, session runner.SessionRef, remainingSeconds int)
	onVerify        VerifyCallback
	onCrash         SessionCallback
	onReset         SessionCallback
	onEnded         EndedCallback
	grader          Grader
	accepting       bool
	provisioningWG  sync.WaitGroup
	lifecycleCtx    context.Context
	lifecycleCancel context.CancelFunc
	setupWG         sync.WaitGroup
	watcherWG       sync.WaitGroup
	watchersStarted bool
}

func NewService(r runner.Runner, store ProblemStore, catalog TrustedProblemCatalog) *Service {
	s := NewPausedService(r, store, catalog)
	if err := s.Activate(); err != nil {
		panic(err)
	}
	return s
}

// NewPausedService constructs the lifecycle state without admitting work or
// starting watchers. Server startup uses it until the controller lease and
// provider recovery gate have both completed.
func NewPausedService(r runner.Runner, store ProblemStore, catalog TrustedProblemCatalog) *Service {
	lifecycleCtx, lifecycleCancel := context.WithCancel(context.Background())
	s := &Service{
		runner:          r,
		problemStore:    store,
		problemCatalog:  catalog,
		sessions:        make(map[string]*Session),
		startCooldown:   defaultStartCooldown,
		lastStart:       make(map[string]time.Time),
		verifyThrottle:  defaultVerifyThrottle,
		lastVerify:      make(map[string]time.Time),
		verifyOps:       make(map[string]*verifyOperation),
		lifecycleCtx:    lifecycleCtx,
		lifecycleCancel: lifecycleCancel,
	}
	if active, ok := catalog.(ActiveProblemCatalog); ok {
		s.activeCatalog = active
	}
	return s

}

// NewDurablePausedService constructs the production service. Admission stays
// closed until startup recovery has completed under the controller lease.
func NewDurablePausedService(r runner.Runner, store ProblemStore, catalog DurableProblemCatalog, durable DurableLifecycleStore, providerID string) (*Service, error) {
	if isNilInterface(catalog) {
		return nil, errors.New("durable session service requires an active problem catalog")
	}
	if durable == nil {
		return nil, errors.New("durable session service requires a lifecycle store")
	}
	if providerID == "" {
		return nil, errors.New("durable session service requires a provider id")
	}
	s := NewPausedService(r, store, catalog)
	s.activeCatalog = catalog
	s.durableStore = durable
	s.providerID = providerID
	ownerDigest := sha256.Sum256([]byte(providerID))
	s.workerOwner = fmt.Sprintf("cleanup-%x", ownerDigest[:12])
	return s, nil
}

func isNilInterface(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

// Activate opens admission and starts lifecycle watchers exactly once. It
// must be called only after startup recovery has converged under the controller
// lease.
func (s *Service) Activate() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lifecycleCtx.Err() != nil {
		return ErrServiceStopping
	}
	if s.authority != nil && !s.authority.Ready() {
		return ErrServiceStopping
	}
	if s.watchersStarted {
		s.accepting = true
		return nil
	}
	s.accepting = true
	s.watchersStarted = true
	s.watcherWG.Add(3)
	go s.timeoutWatcher(s.lifecycleCtx)
	go s.crashWatcher(s.lifecycleCtx)
	go s.cleanupWatcher(s.lifecycleCtx)
	return nil
}

func (s *Service) SetStageCallback(cb StageCallback) {
	s.onStage = cb
}

func (s *Service) SetTimeoutCallback(cb SessionCallback) {
	s.onTimeout = cb
}

func (s *Service) SetTimeoutWarningCallback(cb func(string, runner.SessionRef, int)) {
	s.onTimeoutWarn = cb
}

func (s *Service) SetVerifyCallback(cb VerifyCallback) {
	s.onVerify = cb
}

func (s *Service) SetCrashCallback(cb SessionCallback) {
	s.onCrash = cb
}

// SetResetCallback retains the legacy in-process callback seam for tests and
// compatibility. Production lifecycle delivery uses the durable event stream.
func (s *Service) SetResetCallback(cb SessionCallback) {
	s.onReset = cb
}

func (s *Service) SetEndedCallback(cb EndedCallback) {
	s.onEnded = cb
}

func (s *Service) announceResetGeneration(sess *Session, ref runner.SessionRef) {
	if ref.Generation <= 1 {
		return
	}
	sess.mu.Lock()
	if sess.resetNotifiedGeneration >= ref.Generation {
		sess.mu.Unlock()
		return
	}
	sess.resetNotifiedGeneration = ref.Generation
	sess.mu.Unlock()
	if s.onReset != nil {
		s.onReset(sess.UserID, ref)
	}
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

// SetAuthorityGate binds public admission to the same monotonic authority
// state enforced at the Runner boundary. It must be set before Activate.
func (s *Service) SetAuthorityGate(gate *runner.AuthorityGate) error {
	if gate == nil {
		return errors.New("session authority gate is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.watchersStarted || s.accepting {
		return errors.New("session authority gate must be set before activation")
	}
	s.authority = gate
	return nil
}

// RecoverDurableState reconstructs and converges the current provider scope
// while both HTTP/session admission and the ordinary Runner boundary remain
// closed. Every provider call uses the recovery-only authority capability.
// A contradiction or ambiguous provider observation fails startup closed.
func (s *Service) RecoverDurableState(ctx context.Context) error {
	if s.durableStore == nil || s.providerID == "" {
		return errors.New("durable startup recovery is not configured")
	}
	recoveryRunner, ok := s.runner.(runner.StartupRecoveryRunner)
	if !ok {
		return errors.New("runner does not expose controller-fenced startup recovery")
	}
	s.mu.RLock()
	paused := !s.accepting && !s.watchersStarted
	authority := s.authority
	s.mu.RUnlock()
	if !paused || authority == nil || authority.State() != runner.AuthorityRecovering {
		return errors.New("durable startup recovery requires a paused recovering service")
	}
	if err := s.durableStore.PrepareRecoveryWork(ctx, s.providerID); err != nil {
		return fmt.Errorf("prepare durable startup work: %w", err)
	}
	if err := s.hydrateActiveReservations(ctx); err != nil {
		return err
	}

	for processed := 0; processed < 4096; processed++ {
		worked, err := s.processRecoveryWorkOnce(ctx, recoveryRunner)
		if err != nil {
			return err
		}
		if !worked {
			break
		}
		if processed == 4095 {
			return errors.New("durable startup recovery exceeded the work bound")
		}
	}
	if err := s.reconcileHydratedReservations(ctx, recoveryRunner); err != nil {
		return err
	}
	for processed := 0; processed < 4096; processed++ {
		worked, err := s.processRecoveryWorkOnce(ctx, recoveryRunner)
		if err != nil {
			return err
		}
		if !worked {
			break
		}
		if processed == 4095 {
			return errors.New("durable startup cleanup exceeded the work bound")
		}
	}
	return s.validateRecoveredFixedPoint(ctx, recoveryRunner)
}

func (s *Service) hydrateActiveReservations(ctx context.Context) error {
	reservations, err := s.durableStore.ListActiveReservations(ctx, s.providerID)
	if err != nil {
		return fmt.Errorf("list active durable sessions for recovery: %w", err)
	}
	for _, reservation := range reservations {
		trustedProblem, err := s.problemCatalog.ResolveProblem(ctx, reservation.Session.Selection.Problem)
		if err != nil {
			return fmt.Errorf("resolve recovery problem %s@%s: %w",
				reservation.Session.Selection.Problem.ID, reservation.Session.Selection.Problem.Revision, err)
		}
		if _, err := s.hydrateReservation(reservation, trustedProblem); err != nil {
			return fmt.Errorf("hydrate session %s generation %d: %w",
				reservation.Session.ID, reservation.Session.CurrentGeneration, err)
		}
	}
	return nil
}

func (s *Service) processRecoveryWorkOnce(ctx context.Context, recoveryRunner runner.StartupRecoveryRunner) (bool, error) {
	destroyClaim, foundDestroy, err := s.durableStore.ClaimDestroyWork(
		ctx, s.providerID, "", s.workerOwner+"-startup", durableDestroyLease,
	)
	if err != nil {
		return false, fmt.Errorf("claim startup destroy work: %w", err)
	}
	if foundDestroy {
		if destroyClaim.Pending {
			return true, fmt.Errorf("%w: startup destroy is already owned by another worker", ErrCleanupPending)
		}
		if destroyClaim.Completed {
			return true, nil
		}
		callCtx, cancel := context.WithTimeout(ctx, durableDestroyTimeout)
		destroyErr := recoveryRunner.DestroySessionRecovery(callCtx, destroyClaim.Ref)
		cancel()
		if destroyErr != nil {
			retryErr := s.durableStore.RetryDestroyWork(ctx, destroyClaim, destroyErr, durableRetryDelay(destroyClaim.Attempt))
			return true, errors.Join(fmt.Errorf("startup destroy allocation %s: %w", destroyClaim.Ref.ID, destroyErr), retryErr)
		}
		if err := s.durableStore.MarkClaimedDestroyed(ctx, destroyClaim); err != nil {
			return true, fmt.Errorf("record startup allocation %s absent: %w", destroyClaim.Ref.ID, err)
		}
		s.evictAllocation(destroyClaim.Ref)
		return true, nil
	}

	claim, foundCreate, err := s.durableStore.ClaimProvisionableCreate(
		ctx, s.providerID, "", s.workerOwner+"-startup", durableCreateLease,
	)
	if err != nil {
		return false, fmt.Errorf("claim startup create work: %w", err)
	}
	if !foundCreate {
		return false, nil
	}
	if err := s.resumeClaimedCreateRecovery(ctx, recoveryRunner, claim); err != nil {
		return true, err
	}
	return true, nil
}

func (s *Service) resumeClaimedCreateRecovery(ctx context.Context, recoveryRunner runner.StartupRecoveryRunner, claim runner.CreateWorkClaim) error {
	reservation := claim.Reservation
	trustedProblem, err := s.problemCatalog.ResolveProblem(ctx, reservation.Session.Selection.Problem)
	if err != nil {
		return s.failClaimedCreateRecovery(ctx, recoveryRunner, claim, "invalid_revision", err)
	}
	current := s.GetSession(reservation.Session.UserID)
	if current == nil {
		current, err = s.hydrateReservation(reservation, trustedProblem)
		if err != nil {
			return s.failClaimedCreateRecovery(ctx, recoveryRunner, claim, "recovery_hydration_failed", err)
		}
	}

	createCtx, cancel, err := claimedCreateContext(ctx, claim)
	if err != nil {
		return err
	}
	request := runner.CreateSessionRequest{
		AllocationID:    reservation.Allocation.Ref.ID,
		Session:         reservation.Allocation.Ref.Session,
		UserID:          reservation.Session.UserID,
		Selection:       reservation.Session.Selection,
		ResourceProfile: reservation.Allocation.ResourceProfile,
		ExpiresAt:       reservation.Allocation.ExpiresAt,
		IdempotencyKey:  reservation.Operation.IdempotencyKey,
	}
	createdRef, createErr := recoveryRunner.CreateSessionRecovery(createCtx, request)
	cancel()
	if createErr != nil {
		return s.failClaimedCreateRecovery(ctx, recoveryRunner, claim, "provider_create_failed", createErr)
	}
	if createdRef != reservation.Allocation.Ref {
		mismatch := fmt.Errorf("runner returned allocation %+v, want reserved %+v", createdRef, reservation.Allocation.Ref)
		return s.failClaimedCreateRecovery(ctx, recoveryRunner, claim, "provider_contract_violation", mismatch)
	}
	if err := s.durableStore.MarkClaimedCreateSucceeded(ctx, claim); err != nil {
		return fmt.Errorf("persist startup claimed create: %w", err)
	}
	current.mu.Lock()
	current.Status = StatusBooting
	current.mu.Unlock()
	return s.completeRecoveredSetup(ctx, recoveryRunner, current, reservation.Allocation.Ref)
}

func (s *Service) failClaimedCreateRecovery(ctx context.Context, recoveryRunner runner.StartupRecoveryRunner, claim runner.CreateWorkClaim, code string, cause error) error {
	ref := claim.Reservation.Allocation.Ref
	transitionErr := s.durableStore.MarkClaimedCreateFailed(ctx, claim, code, cause.Error())
	if transitionErr != nil {
		return errors.Join(cause, transitionErr)
	}
	destroyClaim, found, claimErr := s.durableStore.ClaimDestroyWork(
		ctx, s.providerID, ref.ID, s.workerOwner+"-startup-create-failure", durableDestroyLease,
	)
	if claimErr != nil || !found {
		if claimErr == nil {
			claimErr = fmt.Errorf("%w: startup create cleanup is not claimable", runner.ErrLifecycleConflict)
		}
		return errors.Join(cause, claimErr)
	}
	if destroyClaim.Pending {
		return errors.Join(cause, fmt.Errorf("%w: startup create cleanup is already owned", ErrCleanupPending))
	}
	if destroyClaim.Completed {
		s.evictSession(claim.Reservation.Session.UserID, s.GetSession(claim.Reservation.Session.UserID))
		return cause
	}
	callCtx, cancel := context.WithTimeout(ctx, durableDestroyTimeout)
	destroyErr := recoveryRunner.DestroySessionRecovery(callCtx, ref)
	cancel()
	if destroyErr != nil {
		retryErr := s.durableStore.RetryDestroyWork(ctx, destroyClaim, destroyErr, durableRetryDelay(destroyClaim.Attempt))
		return errors.Join(cause, destroyErr, retryErr)
	}
	if err := s.durableStore.MarkClaimedDestroyed(ctx, destroyClaim); err != nil {
		return errors.Join(cause, err)
	}
	s.evictSession(claim.Reservation.Session.UserID, s.GetSession(claim.Reservation.Session.UserID))
	return cause
}

func (s *Service) completeRecoveredSetup(ctx context.Context, recoveryRunner runner.StartupRecoveryRunner, sess *Session, ref runner.AllocationRef) error {
	waitCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
	err := recoveryRunner.WaitReadyRecovery(waitCtx, ref, 120*time.Second)
	cancel()
	if err != nil {
		return s.failRecoveredEnvironment(ctx, recoveryRunner, sess, ref, "boot_timeout", err)
	}
	if err := s.durableStore.MarkSettingUp(ctx, ref); err != nil {
		return fmt.Errorf("persist recovered setting-up state: %w", err)
	}
	sess.mu.Lock()
	sess.Status = StatusSettingUp
	sess.mu.Unlock()
	setupCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	err = recoveryRunner.SetupSessionRecovery(setupCtx, runner.SetupSessionRequest{
		Allocation: ref, IdempotencyKey: runner.SetupIdempotencyKey(ref),
	})
	cancel()
	if err != nil {
		return s.failRecoveredEnvironment(ctx, recoveryRunner, sess, ref, "setup_failed", err)
	}
	if err := s.durableStore.MarkReady(ctx, ref); err != nil {
		return fmt.Errorf("persist recovered ready state: %w", err)
	}
	sess.mu.Lock()
	sess.Status = StatusReady
	sess.mu.Unlock()
	return nil
}

func (s *Service) failRecoveredEnvironment(ctx context.Context, recoveryRunner runner.StartupRecoveryRunner, sess *Session, ref runner.AllocationRef, code string, cause error) error {
	if err := s.durableStore.RequestDestroy(ctx, ref, "failed", "failed", code+": "+cause.Error()); err != nil {
		return errors.Join(cause, err)
	}
	claim, found, err := s.durableStore.ClaimDestroyWork(
		ctx, s.providerID, ref.ID, s.workerOwner+"-startup-environment-failure", durableDestroyLease,
	)
	if err != nil || !found {
		if err == nil {
			err = fmt.Errorf("%w: recovered environment cleanup is not claimable", runner.ErrLifecycleConflict)
		}
		return errors.Join(cause, err)
	}
	if claim.Pending {
		return errors.Join(cause, fmt.Errorf("%w: recovered environment cleanup is already owned", ErrCleanupPending))
	}
	if claim.Completed {
		s.evictSession(sess.UserID, sess)
		return cause
	}
	callCtx, cancel := context.WithTimeout(ctx, durableDestroyTimeout)
	destroyErr := recoveryRunner.DestroySessionRecovery(callCtx, ref)
	cancel()
	if destroyErr != nil {
		retryErr := s.durableStore.RetryDestroyWork(ctx, claim, destroyErr, durableRetryDelay(claim.Attempt))
		return errors.Join(cause, destroyErr, retryErr)
	}
	if err := s.durableStore.MarkClaimedDestroyed(ctx, claim); err != nil {
		return errors.Join(cause, err)
	}
	s.evictSession(sess.UserID, sess)
	return cause
}

func (s *Service) reconcileHydratedReservations(ctx context.Context, recoveryRunner runner.StartupRecoveryRunner) error {
	reservations, err := s.durableStore.ListActiveReservations(ctx, s.providerID)
	if err != nil {
		return fmt.Errorf("list hydrated recovery sessions: %w", err)
	}
	for _, reservation := range reservations {
		if reservation.Operation.State != "succeeded" {
			return fmt.Errorf("%w: active create %s remains %s after startup drain",
				runner.ErrLifecycleConflict, reservation.Operation.ID, reservation.Operation.State)
		}
		sess := s.GetSession(reservation.Session.UserID)
		if sess == nil {
			return fmt.Errorf("%w: active session %s was not hydrated", runner.ErrLifecycleConflict, reservation.Session.ID)
		}
		ref := reservation.Allocation.Ref
		observation, err := recoveryRunner.GetSessionRecovery(ctx, ref)
		if err != nil {
			return fmt.Errorf("observe recovered allocation %s: %w", ref.ID, err)
		}
		if observation.Allocation != ref {
			return fmt.Errorf("%w: provider observation changed allocation identity", runner.ErrLifecycleConflict)
		}
		if !reservation.Allocation.ExpiresAt.After(time.Now()) {
			if err := s.durableStore.RequestDestroy(ctx, ref, "timed_out", "timeout", "environment expired during controller restart"); err != nil {
				return err
			}
			continue
		}
		switch observation.State {
		case runner.ObservedRunning:
			if reservation.Session.State != "ready" {
				if err := s.completeRecoveredSetup(ctx, recoveryRunner, sess, ref); err != nil {
					return err
				}
			}
		case runner.ObservedAbsent, runner.ObservedStopped:
			if err := s.durableStore.RequestDestroy(ctx, ref, "provider_lost", "failed", "environment was missing during controller restart"); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%w: unsupported provider observation %q", runner.ErrLifecycleConflict, observation.State)
		}
	}
	return nil
}

func (s *Service) validateRecoveredFixedPoint(ctx context.Context, recoveryRunner runner.StartupRecoveryRunner) error {
	reservations, err := s.durableStore.ListActiveReservations(ctx, s.providerID)
	if err != nil {
		return err
	}
	for _, reservation := range reservations {
		if reservation.Operation.State != "succeeded" || reservation.Session.State != "ready" ||
			reservation.Allocation.ObservedState != "running" {
			return fmt.Errorf("%w: recovery did not converge session %s generation %d (%s/%s/%s)",
				runner.ErrLifecycleConflict, reservation.Session.ID, reservation.Session.CurrentGeneration,
				reservation.Operation.State, reservation.Session.State, reservation.Allocation.ObservedState)
		}
		observation, err := recoveryRunner.GetSessionRecovery(ctx, reservation.Allocation.Ref)
		if err != nil {
			return err
		}
		if observation.Allocation != reservation.Allocation.Ref || observation.State != runner.ObservedRunning {
			return fmt.Errorf("%w: final provider observation does not match durable ready allocation", runner.ErrLifecycleConflict)
		}
	}
	return nil
}

func (s *Service) StartProblem(ctx context.Context, userID, problemID string) (string, error) {
	if !s.beginProvisioning() {
		return "", ErrServiceStopping
	}
	defer s.provisioningWG.Done()
	provisioningCtx, cancelProvisioning := s.operationContext(ctx)
	defer cancelProvisioning()

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

	p, err := s.problemStore.FindByID(provisioningCtx, problemID)
	if err != nil {
		releaseReservation()
		return "", fmt.Errorf("problem not found: %w", err)
	}
	if p.Revision == "" {
		releaseReservation()
		return "", fmt.Errorf("problem %q has no approved runtime revision", problemID)
	}
	problemRef := runner.ProblemRef{ID: problemID, Revision: p.Revision}
	selection := runner.CatalogSelection{Generation: 1, Problem: problemRef}
	if s.problemCatalog == nil {
		releaseReservation()
		return "", errors.New("trusted problem catalog is not configured")
	}
	trustedProblem, err := s.problemCatalog.ResolveProblem(provisioningCtx, problemRef)
	if err != nil {
		releaseReservation()
		return "", fmt.Errorf("resolve approved problem revision: %w", err)
	}

	s.mu.RLock()
	existing := s.sessions[userID]
	s.mu.RUnlock()
	if existing != nil {
		if err := s.endSessionLocked(provisioningCtx, userID, "failed"); err != nil {
			releaseReservation()
			return "", fmt.Errorf("replace existing session: %w", err)
		}
	}

	attempt := &models.Attempt{
		UserID:    userID,
		ProblemID: problemID,
		Status:    "in_progress",
		StartedAt: time.Now(),
	}
	if err := s.problemStore.CreateAttempt(provisioningCtx, attempt); err != nil {
		releaseReservation()
		return "", fmt.Errorf("create attempt: %w", err)
	}

	s.emitStage(userID, runner.SessionRef{SessionID: attempt.ID, Generation: 1}, "container_created", "Creating container...")
	timeout := time.Duration(trustedProblem.TimeoutMinutes) * time.Minute
	expiresAt := time.Now().Add(timeout)
	createSession := runner.SessionRef{
		SessionID:  attempt.ID,
		Generation: 1,
	}
	ref, err := s.runner.CreateSession(provisioningCtx, runner.CreateSessionRequest{
		AllocationID:    runner.AllocationIDForSession(createSession),
		Session:         createSession,
		UserID:          userID,
		Selection:       selection,
		ResourceProfile: runner.DefaultResourceProfile,
		ExpiresAt:       expiresAt,
		IdempotencyKey:  fmt.Sprintf("create:%s:%d", attempt.ID, 1),
	})
	if err != nil {
		s.failAttempt(provisioningCtx, attempt, "environment creation failed: "+err.Error())
		releaseReservation()
		if s.isStopping() {
			return "", errors.Join(ErrServiceStopping, fmt.Errorf("create environment: %w", err))
		}
		return "", fmt.Errorf("create environment: %w", err)
	}
	sess := &Session{
		ID:            attempt.ID,
		UserID:        userID,
		ProblemID:     problemID,
		Selection:     selection,
		VerifyType:    trustedProblem.VerifyType,
		CorrectChoice: trustedProblem.CorrectChoice,
		GradingPrompt: trustedProblem.GradingPrompt,
		AttemptID:     attempt.ID,
		Generation:    1,
		Allocation:    ref,
		Status:        StatusBooting,
		StartedAt:     time.Now(),
		TimeoutAt:     expiresAt,
	}

	s.mu.Lock()
	s.sessions[userID] = sess
	stopping := !s.accepting || provisioningCtx.Err() != nil
	if reserved {
		s.pendingStarts-- // reservation becomes the real session
	}
	s.mu.Unlock()
	if stopping {
		s.setTerminalIntent(sess, StatusFailed, "failed", "server stopped while creating the environment")
		cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), 10*time.Second)
		finalizeErr := s.finalizeTerminalLocked(cleanupCtx, userID, sess, ref)
		cancelCleanup()
		if finalizeErr != nil {
			log.Printf("late start allocation cleanup for %s failed; shutdown cleanup will retry: %v", userID, finalizeErr)
		}
		return "", ErrServiceStopping
	}

	s.launchSetup(sess, attempt, ref)

	s.mu.Lock()
	s.lastStart[userID] = time.Now()
	s.mu.Unlock()

	return sess.ID, nil
}

// StartProblemOperation is the durable, API-facing start path. The client
// operation key is converted to a deterministic logical session ID, and the
// reservation transaction commits before any provider mutation. Exact replay
// returns the same operation/session/generation and stored expiry.
func (s *Service) StartProblemOperation(ctx context.Context, userID, problemID, operationID string) (*CurrentSession, error) {
	if s.durableStore == nil {
		return nil, errors.New("durable session lifecycle is not configured")
	}
	if operationID == "" {
		return nil, errors.New("start operation id is required")
	}
	if !s.beginProvisioning() {
		return nil, ErrServiceStopping
	}
	defer s.provisioningWG.Done()
	provisioningCtx, cancelProvisioning := s.operationContext(ctx)
	defer cancelProvisioning()

	sessionID := runner.SessionIDForOperation(userID, operationID)
	providerKey := "start:" + sessionID
	// A projected generation-one session with the deterministic ID derived from
	// this exact client operation is a read-only replay. Serve it before
	// transition admission so setup's short state lock cannot create a spurious
	// 429, while unrelated keys are rejected before adding database load.
	s.mu.RLock()
	existingReplay := s.sessions[userID]
	s.mu.RUnlock()
	if existingReplay != nil {
		existingReplay.mu.Lock()
		sameReplay := existingReplay.ID == sessionID && existingReplay.Generation == 1 && existingReplay.ProblemID == problemID
		snapshot := currentSessionSnapshot(existingReplay)
		existingReplay.mu.Unlock()
		if sameReplay {
			return s.authoritativeOperationSnapshot(provisioningCtx, userID, snapshot)
		}
	}

	ul := s.userLock(userID)
	if !ul.TryLock() {
		return nil, ErrTransitionBusy
	}
	defer ul.Unlock()

	// Exact operation replay is bound to the persisted problem revision. It
	// remains valid after that problem is updated or retired from new starts.
	prior, replay, err := s.durableStore.FindReservation(provisioningCtx, s.providerID, providerKey)
	if err != nil {
		return nil, fmt.Errorf("load durable start replay: %w", err)
	}
	if replay {
		if prior.Session.ID != sessionID || prior.Session.UserID != userID ||
			prior.Session.Selection.Problem.ID != problemID || prior.Operation.Kind != "create" {
			return nil, runner.ErrIdempotencyConflict
		}
		if s.problemCatalog == nil {
			return nil, errors.New("trusted problem catalog is not configured")
		}
		trustedProblem, err := s.problemCatalog.ResolveProblem(provisioningCtx, prior.Session.Selection.Problem)
		if err != nil {
			return nil, fmt.Errorf("resolve durable replay problem revision: %w", err)
		}
		params := runner.ReserveSessionParams{
			SessionID: sessionID, UserID: userID, Selection: prior.Session.Selection,
			Provider: s.runner.Kind(), ProviderID: s.providerID,
			ResourceProfile: prior.Allocation.ResourceProfile,
			ExpiresAt:       prior.Session.ExpiresAt,
			IdempotencyKey:  providerKey,
		}
		reservation, err := s.durableStore.ReserveSession(provisioningCtx, params)
		if err != nil {
			return nil, fmt.Errorf("validate durable start replay: %w", err)
		}
		if reservation.Operation.ID != prior.Operation.ID {
			return nil, runner.ErrIdempotencyConflict
		}
		return s.resumeDurableReservation(provisioningCtx, userID, trustedProblem, reservation)
	}

	if s.activeCatalog == nil {
		return nil, errors.New("active problem catalog is not configured")
	}
	trustedProblem, selection, releaseCatalog, err := s.activeCatalog.AcquireActiveProblem(provisioningCtx, problemID)
	if err != nil {
		return nil, fmt.Errorf("resolve active problem: %w", err)
	}
	catalogReleased := false
	releaseActiveCatalog := func() {
		if !catalogReleased {
			releaseCatalog()
			catalogReleased = true
		}
	}
	defer releaseActiveCatalog()
	if selection.Problem != (runner.ProblemRef{ID: problemID, Revision: trustedProblem.Revision}) || selection.Generation == 0 {
		releaseActiveCatalog()
		return nil, fmt.Errorf("%w: active catalog returned a contradictory selection", runner.ErrLifecycleConflict)
	}

	params := runner.ReserveSessionParams{
		SessionID: sessionID, UserID: userID, Selection: selection,
		Provider: s.runner.Kind(), ProviderID: s.providerID,
		ResourceProfile: runner.DefaultResourceProfile,
		ExpiresAt:       time.Now().Add(time.Duration(trustedProblem.TimeoutMinutes) * time.Minute),
		IdempotencyKey:  providerKey,
	}

	now := time.Now()
	reservedCap := false
	s.mu.Lock()
	if last, ok := s.lastStart[userID]; ok && now.Sub(last) < s.startCooldown {
		s.mu.Unlock()
		return nil, ErrStartCooldown
	}
	if s.maxSessions > 0 {
		active := len(s.sessions) + s.pendingStarts
		if _, hasOwn := s.sessions[userID]; hasOwn {
			active--
		}
		if active >= s.maxSessions {
			s.mu.Unlock()
			return nil, ErrTooManySessions
		}
		s.pendingStarts++
		reservedCap = true
	}
	s.mu.Unlock()
	releaseCap := func() {
		if reservedCap {
			s.mu.Lock()
			s.pendingStarts--
			s.mu.Unlock()
			reservedCap = false
		}
	}
	defer releaseCap()

	// A new start operation never replaces an active durable session. Reset is
	// a separate generation transaction; silently tearing down here would make
	// a failed reservation lose the user's current environment.
	s.mu.RLock()
	existing := s.sessions[userID]
	s.mu.RUnlock()
	if existing != nil {
		return nil, runner.ErrActiveSession
	}

	reservation, err := s.durableStore.ReserveSession(provisioningCtx, params)
	releaseActiveCatalog()
	if err != nil {
		return nil, fmt.Errorf("reserve durable session: %w", err)
	}
	claim, claimed, err := s.durableStore.ClaimProvisionableCreate(
		provisioningCtx, s.providerID, reservation.Operation.ID, s.workerOwner+"-start-request", durableCreateLease,
	)
	if err != nil {
		return nil, fmt.Errorf("claim durable start create: %w", err)
	}
	if !claimed {
		return durableReservationSnapshot(reservation), nil
	}
	return s.provisionDurableReservationWithClaim(provisioningCtx, userID, trustedProblem, reservation, &claim)
}

func (s *Service) resumeDurableReservation(ctx context.Context, userID string, trustedProblem models.Problem, reservation runner.SessionReservation) (*CurrentSession, error) {
	if reservation.Session.UserID != userID || reservation.Session.Selection.Problem.ID != trustedProblem.ID {
		return nil, runner.ErrIdempotencyConflict
	}
	if reservation.Session.DesiredState != "active" || reservation.Allocation.DesiredState != "active" {
		return nil, fmt.Errorf("%w: start operation is already terminal", runner.ErrLifecycleConflict)
	}
	s.mu.RLock()
	existing := s.sessions[userID]
	s.mu.RUnlock()
	if existing != nil {
		existing.mu.Lock()
		same := existing.ID == reservation.Session.ID && existing.Generation == reservation.Session.CurrentGeneration &&
			existing.Allocation == reservation.Allocation.Ref
		snapshot := currentSessionSnapshot(existing)
		existing.mu.Unlock()
		if same {
			return s.authoritativeOperationSnapshot(ctx, userID, snapshot)
		}
		return nil, runner.ErrActiveSession
	}
	if reservation.Operation.State == "pending" || reservation.Operation.State == "running" {
		claim, claimed, err := s.durableStore.ClaimProvisionableCreate(
			ctx, s.providerID, reservation.Operation.ID, s.workerOwner+"-start-replay", durableCreateLease,
		)
		if err != nil {
			return nil, fmt.Errorf("claim durable start replay: %w", err)
		}
		if !claimed {
			return durableReservationSnapshot(reservation), nil
		}
		return s.provisionDurableReservationWithClaim(ctx, userID, trustedProblem, reservation, &claim)
	}
	return s.provisionDurableReservation(ctx, userID, trustedProblem, reservation)
}

func (s *Service) provisionDurableReservation(ctx context.Context, userID string, trustedProblem models.Problem, reservation runner.SessionReservation) (*CurrentSession, error) {
	return s.provisionDurableReservationWithClaim(ctx, userID, trustedProblem, reservation, nil)
}

func (s *Service) provisionDurableReservationWithClaim(ctx context.Context, userID string, trustedProblem models.Problem, reservation runner.SessionReservation, claim *runner.CreateWorkClaim) (*CurrentSession, error) {
	ref := reservation.Allocation.Ref
	createRequest := runner.CreateSessionRequest{
		AllocationID: ref.ID, Session: ref.Session, UserID: userID,
		Selection: reservation.Session.Selection, ResourceProfile: reservation.Allocation.ResourceProfile,
		ExpiresAt: reservation.Allocation.ExpiresAt, IdempotencyKey: reservation.Operation.IdempotencyKey,
	}
	createCtx := ctx
	createCancel := func() {}
	if claim != nil {
		var deadlineErr error
		createCtx, createCancel, deadlineErr = claimedCreateContext(ctx, *claim)
		if deadlineErr != nil {
			return nil, deadlineErr
		}
	}
	defer createCancel()
	createdRef, err := s.runner.CreateSession(createCtx, createRequest)
	if err != nil {
		cleanupErr := s.failReservedCreateClaim(ref, claim, err)
		return nil, errors.Join(fmt.Errorf("create environment: %w", err), cleanupErr)
	}
	if createdRef != ref {
		mismatch := fmt.Errorf("runner returned allocation %+v, want reserved %+v", createdRef, ref)
		// The provider response is untrusted identity evidence. Never destroy
		// createdRef: it may identify another allocation. Fence the reserved
		// create as failed and clean only the exact durable allocation.
		cleanupErr := s.failReservedCreateClaimWithCode(ref, claim, "provider_contract_violation", mismatch)
		return nil, errors.Join(mismatch, cleanupErr)
	}
	var markCreatedErr error
	if claim != nil {
		markCreatedErr = s.durableStore.MarkClaimedCreateSucceeded(ctx, *claim)
	} else {
		markCreatedErr = s.durableStore.MarkCreateSucceeded(ctx, ref)
	}
	if markCreatedErr != nil {
		// A replacement worker whose operation lease expired no longer owns
		// cleanup. The successor may already be creating the same idempotent
		// allocation, so the stale worker must not delete it.
		if claim != nil && errors.Is(markCreatedErr, runner.ErrLifecycleConflict) {
			return nil, fmt.Errorf("persist claimed environment creation: %w", markCreatedErr)
		}
		cleanupErr := s.failProvisionedReservation(ref, markCreatedErr)
		return nil, errors.Join(fmt.Errorf("persist environment creation: %w", markCreatedErr), cleanupErr)
	}

	status := StatusBooting
	if reservation.Session.State == "ready" {
		status = StatusReady
	}
	sess := &Session{
		ID: reservation.Session.ID, UserID: userID,
		ProblemID: trustedProblem.ID, Selection: reservation.Session.Selection,
		VerifyType: trustedProblem.VerifyType, CorrectChoice: trustedProblem.CorrectChoice,
		GradingPrompt: trustedProblem.GradingPrompt, AttemptID: reservation.AttemptID,
		OperationID: reservation.Operation.ID, Generation: ref.Session.Generation,
		Allocation: ref, Status: status, StartedAt: reservation.Session.QueuedAt,
		TimeoutAt: reservation.Session.ExpiresAt,
	}

	s.mu.Lock()
	if current := s.sessions[userID]; current != nil && current.ID != sess.ID {
		s.mu.Unlock()
		cleanupErr := s.failProvisionedReservation(ref, runner.ErrActiveSession)
		return nil, errors.Join(runner.ErrActiveSession, cleanupErr)
	}
	s.sessions[userID] = sess
	stopping := !s.accepting || ctx.Err() != nil
	s.mu.Unlock()

	if stopping {
		s.setTerminalIntent(sess, StatusFailed, "failed", "server stopped while creating the environment")
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return nil, errors.Join(ErrServiceStopping, s.finalizeTerminalLocked(cleanupCtx, userID, sess, ref))
	}

	if status != StatusReady {
		attempt := &models.Attempt{ID: sess.AttemptID, UserID: userID, ProblemID: sess.ProblemID, Status: "in_progress", StartedAt: sess.StartedAt}
		s.launchSetup(sess, attempt, ref)
	}
	s.mu.Lock()
	s.lastStart[userID] = time.Now()
	s.mu.Unlock()
	sess.mu.Lock()
	snapshot := currentSessionSnapshot(sess)
	sess.mu.Unlock()
	return s.authoritativeOperationSnapshot(ctx, userID, snapshot)
}

func (s *Service) failReservedCreate(ref runner.AllocationRef, createErr error) error {
	return s.failReservedCreateClaim(ref, nil, createErr)
}

func (s *Service) failReservedCreateClaim(ref runner.AllocationRef, claim *runner.CreateWorkClaim, createErr error) error {
	return s.failReservedCreateClaimWithCode(ref, claim, "provider_create_failed", createErr)
}

func (s *Service) failReservedCreateClaimWithCode(ref runner.AllocationRef, claim *runner.CreateWorkClaim, code string, createErr error) error {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var transitionErr error
	if claim != nil {
		transitionErr = s.durableStore.MarkClaimedCreateFailed(cleanupCtx, *claim, code, createErr.Error())
	} else {
		transitionErr = s.durableStore.MarkCreateFailed(cleanupCtx, ref, code, createErr.Error())
	}
	// Only the current create claim may publish cleanup intent. Once that
	// transition commits, deletion itself is owned by a destroy-operation claim.
	var cleanupErr error
	if transitionErr == nil {
		cleanupErr = s.executeClaimedDestroy(cleanupCtx, ref, s.workerOwner+"-create-failure")
	}
	return errors.Join(transitionErr, cleanupErr)
}

func (s *Service) failProvisionedReservation(ref runner.AllocationRef, cause error) error {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	requestErr := s.durableStore.RequestDestroy(cleanupCtx, ref, "failed", "failed", "environment activation failed: "+cause.Error())
	var cleanupErr error
	if requestErr == nil {
		cleanupErr = s.executeClaimedDestroy(cleanupCtx, ref, s.workerOwner+"-activation-failure")
	}
	return errors.Join(requestErr, cleanupErr)
}

func (s *Service) launchSetup(sess *Session, attempt *models.Attempt, ref runner.AllocationRef) {
	setupCtx, setupCancel := context.WithCancel(s.lifecycleCtx)
	sess.mu.Lock()
	sess.setupCancel = setupCancel
	sess.mu.Unlock()
	s.setupWG.Add(1)
	go func() {
		defer s.setupWG.Done()
		defer setupCancel()
		s.setupEnvironment(setupCtx, sess, attempt, ref)
	}()
}

func (s *Service) setupEnvironment(ctx context.Context, sess *Session, attempt *models.Attempt, ref runner.AllocationRef) {
	s.emitStage(sess.UserID, ref.Session, "k3s_booting", "Waiting for k3s to boot...")
	if err := s.runner.WaitReady(ctx, ref, 120*time.Second); err != nil {
		if ctx.Err() != nil {
			return
		}
		if !s.isCurrent(sess.UserID, sess, ref) {
			return
		}
		s.failEnvironment(ctx, sess, attempt, ref, "k3s node did not become Ready: "+err.Error())
		return
	}
	ul := s.userLock(sess.UserID)
	ul.Lock()
	if !s.isCurrent(sess.UserID, sess, ref) {
		ul.Unlock()
		return
	}
	if s.durableStore != nil && sess.OperationID != "" {
		if err := s.durableStore.MarkSettingUp(ctx, ref); err != nil {
			ul.Unlock()
			s.failEnvironment(ctx, sess, attempt, ref, "persist setting-up state: "+err.Error())
			return
		}
	}
	sess.mu.Lock()
	sess.Status = StatusSettingUp
	sess.mu.Unlock()
	s.emitStage(sess.UserID, ref.Session, "setup_running", "Setting up problem environment...")
	ul.Unlock()

	if err := s.runner.SetupSession(ctx, runner.SetupSessionRequest{
		Allocation: ref, IdempotencyKey: runner.SetupIdempotencyKey(ref),
	}); err != nil {
		if ctx.Err() != nil {
			return
		}
		if !s.isCurrent(sess.UserID, sess, ref) {
			return
		}
		s.failEnvironment(ctx, sess, attempt, ref, "setup failed: "+err.Error())
		return
	}
	ul.Lock()
	if !s.isCurrent(sess.UserID, sess, ref) {
		ul.Unlock()
		return
	}
	if s.durableStore != nil && sess.OperationID != "" {
		if err := s.durableStore.MarkReady(ctx, ref); err != nil {
			ul.Unlock()
			s.failEnvironment(ctx, sess, attempt, ref, "persist ready state: "+err.Error())
			return
		}
	}

	sess.mu.Lock()
	sess.Status = StatusReady
	sess.mu.Unlock()
	s.emitStage(sess.UserID, ref.Session, "ready", "Environment ready. Good luck!")
	ul.Unlock()
}

func (s *Service) Verify(ctx context.Context, userID, operationID string) (success bool, verifyLog string, retErr error) {
	if !s.isAccepting() {
		return false, "", ErrServiceStopping
	}
	if operationID == "" {
		return false, "", errors.New("verify operation id is required")
	}
	operationKey := userID + "\x00" + operationID
	// Exact completed retries are read-only and may bypass transition
	// admission. An in-flight retry is rejected instead of creating an
	// unbounded waiter set on the operation's done channel.
	s.mu.Lock()
	s.pruneVerifyOperationsLocked(time.Now())
	prior := s.verifyOps[operationKey]
	if prior != nil && prior.completed.IsZero() {
		s.mu.Unlock()
		return false, "", ErrTransitionBusy
	}
	s.mu.Unlock()
	if prior != nil {
		if s.durableStore != nil {
			return s.replayCachedDurableVerify(ctx, userID, operationID)
		}
		return s.replayVerifyOperation(ctx, userID, prior)
	}

	durable := s.durableStore != nil
	var durableDecision runner.VerifyDecision
	var durableFound bool
	if durable {
		var err error
		durableDecision, durableFound, err = s.durableStore.LookupVerify(ctx, userID, operationID)
		if err != nil {
			return false, "", fmt.Errorf("load durable verify operation: %w", err)
		}
		if handled, replaySuccess, replayLog, replayErr := durableVerifyReplay(durableDecision, durableFound); handled {
			return replaySuccess, replayLog, replayErr
		}
	}

	ul := s.userLock(userID)
	if !ul.TryLock() {
		return false, "", ErrTransitionBusy
	}
	defer ul.Unlock()

	// A concurrent duplicate may have completed between the initial lookup and
	// transition admission. Replay it before inspecting session state.
	s.mu.Lock()
	s.pruneVerifyOperationsLocked(time.Now())
	if prior := s.verifyOps[operationKey]; prior != nil {
		s.mu.Unlock()
		if durable {
			return s.replayCachedDurableVerify(ctx, userID, operationID)
		}
		return s.replayVerifyOperation(ctx, userID, prior)
	}
	s.mu.Unlock()
	if durable {
		var err error
		durableDecision, durableFound, err = s.durableStore.LookupVerify(ctx, userID, operationID)
		if err != nil {
			return false, "", fmt.Errorf("reload durable verify operation: %w", err)
		}
		if handled, replaySuccess, replayLog, replayErr := durableVerifyReplay(durableDecision, durableFound); handled {
			return replaySuccess, replayLog, replayErr
		}
	}

	sess := s.GetSession(userID)
	if sess == nil {
		return false, "", fmt.Errorf("no active session")
	}

	sess.mu.Lock()
	durable = durable && sess.OperationID != ""
	if sess.Status != StatusReady {
		sess.mu.Unlock()
		return false, "", fmt.Errorf("session not ready")
	}
	ref := sess.Allocation
	gradingPrompt := sess.GradingPrompt
	attemptID := sess.AttemptID
	sess.mu.Unlock()

	if durable {
		if durableFound {
			return false, "", fmt.Errorf("%w: durable verify admission was not replayed", runner.ErrLifecycleConflict)
		} else {
			if err := s.admitVerifyThrottle(userID); err != nil {
				return false, "", err
			}
			decision, err := s.durableStore.BeginVerify(ctx, userID, ref, operationID)
			if err != nil {
				s.rollbackVerifyThrottle(userID)
				return false, "", fmt.Errorf("persist verify start: %w", err)
			}
			if handled, replaySuccess, replayLog, replayErr := durableVerifyReplay(decision, true); handled {
				s.rollbackVerifyThrottle(userID)
				return replaySuccess, replayLog, replayErr
			}
			if !decision.AllowsProviderCall() || decision.Session != ref.Session {
				return false, "", fmt.Errorf("%w: invalid durable verify admission", runner.ErrLifecycleConflict)
			}
		}
	} else if err := s.admitVerifyThrottle(userID); err != nil {
		return false, "", err
	}
	// PostgreSQL is the publication authority for durable sessions. Expose the
	// process-local state only after the state+event transaction commits.
	sess.mu.Lock()
	sess.Status = StatusVerifying
	sess.mu.Unlock()

	op := &verifyOperation{
		sessionID:  sess.ID,
		generation: ref.Session.Generation,
		done:       make(chan struct{}),
	}
	s.mu.Lock()
	s.verifyOps[operationKey] = op
	s.mu.Unlock()
	durableTerminalPersisted := false
	defer func() {
		s.mu.Lock()
		op.success = success
		op.log = verifyLog
		op.err = retErr
		op.completed = time.Now()
		close(op.done)
		if retErr != nil && (!durable || !durableTerminalPersisted) {
			delete(s.verifyOps, operationKey)
		}
		s.pruneVerifyOperationsLocked(op.completed)
		s.mu.Unlock()
	}()

	// Bound the verification exec so a hanging verify.sh (or text grader)
	// cannot tie up the handler indefinitely (DoS guard).
	vctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	applied := false
	providerOutcomeReady := false
	defer func() {
		if applied || providerOutcomeReady || retErr == nil || !s.isCurrent(userID, sess, ref) {
			return
		}
		if durable {
			persistCtx, cancelPersist := context.WithTimeout(context.Background(), 5*time.Second)
			persistErr := s.durableStore.RecordVerifyInfrastructureFailure(
				persistCtx, ref, operationID, "verification_infrastructure_error",
			)
			cancelPersist()
			if persistErr != nil {
				retErr = errors.Join(retErr, fmt.Errorf("persist verify infrastructure failure: %w", persistErr))
				return
			}
			durableTerminalPersisted = true
			retErr = errors.Join(ErrVerifyInfrastructure, retErr)
		}
		sess.mu.Lock()
		if sess.Status == StatusVerifying {
			sess.Status = StatusReady
		}
		sess.mu.Unlock()
	}()

	deadline, ok := vctx.Deadline()
	if !ok {
		return false, "", errors.New("verify deadline is unavailable")
	}
	verifyRequest := runner.VerifyRequest{
		Allocation: ref, Problem: sess.Selection.Problem,
		IdempotencyKey: "verify:" + sess.ID + ":" + fmt.Sprint(ref.Session.Generation) + ":" + operationID,
		Deadline:       deadline.UTC(),
	}
	result, err := s.runner.VerifySession(vctx, verifyRequest)
	if err != nil {
		return false, "", fmt.Errorf("verify environment: %w", err)
	}
	if err := runner.ValidateVerifyResult(verifyRequest, result); err != nil {
		return false, "", fmt.Errorf("validate verify result: %w", err)
	}
	switch result.Status {
	case runner.VerifyPassed:
		success = true
		verifyLog = result.Feedback.Message
	case runner.VerifyFailed:
		verifyLog = result.Feedback.Message
	case runner.VerifyNeedsGrading:
		if s.grader == nil {
			return false, "", fmt.Errorf("text grading is not configured")
		}
		graded, _, err := s.grader.Grade(vctx, gradingPrompt, result.Evidence)
		if err != nil {
			return false, "", fmt.Errorf("grading failed: %w", err)
		}
		success = graded
		finalStatus := runner.VerifyFailed
		if success {
			finalStatus = runner.VerifyPassed
		}
		feedback, err := runner.PublicFeedbackForStatus(finalStatus)
		if err != nil {
			return false, "", fmt.Errorf("build text grading feedback: %w", err)
		}
		verifyLog = feedback.Message
	default:
		return false, "", fmt.Errorf("runner returned unknown verify status %q", result.Status)
	}
	verifyLog = boundedVerifyLog(verifyLog)
	providerOutcomeReady = true
	if !s.isCurrent(userID, sess, ref) {
		return false, "", runner.ErrGenerationStale
	}
	if durable {
		if err := s.durableStore.RecordVerifyFinished(ctx, ref, operationID, success, verifyLog); err != nil {
			return false, "", fmt.Errorf("persist verify result: %w", err)
		}
		durableTerminalPersisted = true
	}

	if success {
		ref, _ = s.setTerminalIntent(sess, StatusCompleted, "success", verifyLog)
	} else {
		sess.mu.Lock()
		sess.Status = StatusReady
		sess.mu.Unlock()
	}
	applied = true

	if !success && !durable {
		attempt, _ := s.problemStore.GetAttempt(ctx, attemptID)
		if attempt != nil {
			attempt.VerifyLog = verifyLog
			if err := s.problemStore.UpdateAttempt(ctx, attempt); err != nil {
				log.Printf("persist failed verification log for attempt %s: %v", attemptID, err)
			}
		}
	}

	if s.onVerify != nil {
		s.onVerify(userID, ref.Session, success, verifyLog)
	}
	if success {
		if err := s.finalizeTerminalLocked(ctx, userID, sess, ref); err != nil {
			log.Printf("completed session finalization for %s failed; retry scheduled: %v", userID, err)
		}
		if s.onEnded != nil {
			s.onEnded(userID, ref.Session, "completed")
		}
	}

	return success, verifyLog, nil
}

func (s *Service) admitVerifyThrottle(userID string) error {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if last, ok := s.lastVerify[userID]; ok && now.Sub(last) < s.verifyThrottle {
		return ErrVerifyTooFast
	}
	s.lastVerify[userID] = now
	return nil
}

// rollbackVerifyThrottle releases a reservation that did not reach an
// executable durable verify operation. Verify holds the per-user transition
// lock while calling this helper, so it cannot erase a newer admission.
func (s *Service) rollbackVerifyThrottle(userID string) {
	s.mu.Lock()
	delete(s.lastVerify, userID)
	s.mu.Unlock()
}

func durableVerifyReplay(decision runner.VerifyDecision, found bool) (bool, bool, string, error) {
	if !found {
		return false, false, "", nil
	}
	switch decision.Kind {
	case runner.VerifyGradeReplay:
		return true, decision.Success, decision.Log, nil
	case runner.VerifyInfrastructureReplay:
		return true, false, "", fmt.Errorf("%w: %s", ErrVerifyInfrastructure, decision.ErrorCode)
	case runner.VerifyExecute:
		return false, false, "", nil
	case runner.VerifyResume:
		return true, false, "", ErrTransitionBusy
	default:
		return true, false, "", fmt.Errorf("%w: invalid stored verify decision %q", runner.ErrLifecycleConflict, decision.Kind)
	}
}

func (s *Service) replayCachedDurableVerify(ctx context.Context, userID, operationID string) (bool, string, error) {
	decision, found, err := s.durableStore.LookupVerify(ctx, userID, operationID)
	if err != nil {
		return false, "", fmt.Errorf("authorize cached durable verify replay: %w", err)
	}
	if handled, success, verifyLog, replayErr := durableVerifyReplay(decision, found); handled {
		return success, verifyLog, replayErr
	}
	return false, "", fmt.Errorf("%w: cached durable verify result is not terminal", runner.ErrLifecycleConflict)
}

func boundedVerifyLog(value string) string {
	const limit = 1024
	value = strings.ToValidUTF8(value, "\uFFFD")
	if len(value) <= limit {
		return value
	}
	end := limit
	for end > 0 && !utf8.ValidString(value[:end]) {
		end--
	}
	return value[:end]
}

func (s *Service) replayVerifyOperation(ctx context.Context, userID string, prior *verifyOperation) (bool, string, error) {
	if current := s.GetSession(userID); current != nil {
		current.mu.Lock()
		sameGeneration := current.ID == prior.sessionID && current.Generation == prior.generation
		current.mu.Unlock()
		if !sameGeneration {
			return false, "", runner.ErrIdempotencyConflict
		}
	}
	select {
	case <-prior.done:
		return prior.success, prior.log, prior.err
	case <-ctx.Done():
		return false, "", ctx.Err()
	}
}

func (s *Service) persistTerminalAttempt(ctx context.Context, sess *Session) error {
	sess.mu.Lock()
	attemptID := sess.AttemptID
	desiredStatus := sess.terminalAttemptStatus
	verifyLog := sess.terminalVerifyLog
	sess.mu.Unlock()
	if desiredStatus == "" {
		return errors.New("terminal session has no desired attempt status")
	}
	attempt, err := s.problemStore.GetAttempt(ctx, attemptID)
	if err != nil {
		return fmt.Errorf("load attempt %s: %w", attemptID, err)
	}
	if attempt == nil {
		return fmt.Errorf("attempt %s is missing", attemptID)
	}
	if attempt.Status == desiredStatus {
		return nil
	}
	if attempt.Status != "in_progress" {
		return fmt.Errorf("attempt %s already terminal as %s, cannot converge to %s", attemptID, attempt.Status, desiredStatus)
	}
	now := time.Now()
	duration := int(now.Sub(attempt.StartedAt).Seconds())
	attempt.Status = desiredStatus
	attempt.FinishedAt = &now
	attempt.DurationSeconds = &duration
	attempt.VerifyLog = verifyLog
	if err := s.problemStore.UpdateAttempt(ctx, attempt); err != nil {
		return fmt.Errorf("persist attempt %s as %s: %w", attemptID, desiredStatus, err)
	}
	return nil
}

func (s *Service) finalizeTerminalLocked(ctx context.Context, userID string, sess *Session, ref runner.AllocationRef) error {
	if !s.ownsAllocation(userID, sess, ref) {
		return runner.ErrGenerationStale
	}
	sess.mu.Lock()
	if sess.setupCancel != nil {
		sess.setupCancel()
	}
	sess.mu.Unlock()
	// Desired-absent and the first terminal outcome are committed before the
	// provider mutation. A transient destroy failure leaves durable recovery
	// work and the exact ref in cache for retry.
	var persistErr error
	if s.durableStore != nil && sess.OperationID != "" {
		sess.mu.Lock()
		attemptStatus := sess.terminalAttemptStatus
		verifyLog := sess.terminalVerifyLog
		status := sess.Status
		sess.mu.Unlock()
		outcome := durableOutcome(status)
		persistErr = s.durableStore.RequestDestroy(ctx, ref, outcome, attemptStatus, verifyLog)
	} else {
		persistErr = s.persistTerminalAttempt(ctx, sess)
	}
	durable := s.durableStore != nil && sess.OperationID != ""
	var cleanupErr error
	if ref.ID != "" {
		if durable && persistErr == nil {
			cleanupErr = s.executeClaimedDestroy(ctx, ref, s.workerOwner+"-terminal")
		} else if !durable {
			cleanupErr = s.runner.DestroySession(ctx, ref)
		}
	}
	if err := errors.Join(persistErr, cleanupErr); err != nil {
		return err
	}
	s.mu.Lock()
	if s.sessions[userID] == sess {
		delete(s.sessions, userID)
	}
	s.mu.Unlock()
	return nil
}

func durableOutcome(status Status) string {
	switch status {
	case StatusCompleted:
		return "completed"
	case StatusTimeout:
		return "timed_out"
	default:
		return "failed"
	}
}

// setTerminalIntent records the first terminal outcome only. Callers may race
// with retries or later user mutations, but success/timeout/failure is a
// write-once fact and must not be reclassified while persistence or exact
// provider cleanup is pending.
func (s *Service) setTerminalIntent(sess *Session, status Status, attemptStatus, verifyLog string) (runner.AllocationRef, bool) {
	sess.mu.Lock()
	set := sess.terminalAttemptStatus == ""
	if set {
		sess.Status = status
		sess.terminalAttemptStatus = attemptStatus
		sess.terminalVerifyLog = verifyLog
	}
	sess.terminalIneligible = true
	ref := sess.Allocation
	sess.mu.Unlock()
	return ref, set
}

func (s *Service) pruneVerifyOperationsLocked(now time.Time) {
	cutoff := now.Add(-verifyOperationTTL)
	for key, op := range s.verifyOps {
		if !op.completed.IsZero() && op.completed.Before(cutoff) {
			delete(s.verifyOps, key)
		}
	}
	for len(s.verifyOps) >= verifyOperationLimit {
		var oldestKey string
		var oldest time.Time
		for key, op := range s.verifyOps {
			if op.completed.IsZero() {
				continue
			}
			if oldestKey == "" || op.completed.Before(oldest) {
				oldestKey, oldest = key, op.completed
			}
		}
		if oldestKey == "" {
			return
		}
		delete(s.verifyOps, oldestKey)
	}
}

func (s *Service) SubmitChoice(
	ctx context.Context,
	userID, problemID string,
	expected runner.SessionRef,
	operationID, choiceID string,
) (bool, error) {
	if !s.isAccepting() {
		return false, ErrServiceStopping
	}
	submission := runner.ChoiceSubmission{
		OperationID: operationID,
		ProblemID:   problemID,
		Session:     expected,
		ChoiceID:    choiceID,
	}
	if s.durableStore != nil {
		decision, found, err := s.durableStore.LookupChoice(ctx, userID, submission)
		if err != nil {
			return false, fmt.Errorf("load durable choice operation: %w", err)
		}
		if handled, success, _, replayErr := durableVerifyReplay(decision, found); handled {
			return success, replayErr
		}
	}

	ul := s.userLock(userID)
	if !ul.TryLock() {
		return false, ErrTransitionBusy
	}
	defer ul.Unlock()
	if s.durableStore != nil {
		decision, found, err := s.durableStore.LookupChoice(ctx, userID, submission)
		if err != nil {
			return false, fmt.Errorf("reload durable choice operation: %w", err)
		}
		if handled, success, _, replayErr := durableVerifyReplay(decision, found); handled {
			return success, replayErr
		}
	}

	sess := s.GetSession(userID)
	if sess == nil {
		return false, runner.ErrGenerationStale
	}

	sess.mu.Lock()
	ref := sess.Allocation
	durable := s.durableStore != nil && sess.OperationID != ""
	if sess.ProblemID != problemID {
		sess.mu.Unlock()
		return false, runner.ErrIdempotencyConflict
	}
	if sess.ID != expected.SessionID || sess.Generation != expected.Generation || ref.Session != expected {
		sess.mu.Unlock()
		return false, runner.ErrGenerationStale
	}
	if sess.Status != StatusReady {
		sess.mu.Unlock()
		return false, fmt.Errorf("%w: session not ready", runner.ErrLifecycleConflict)
	}
	if sess.VerifyType != "choice" {
		sess.mu.Unlock()
		return false, fmt.Errorf("%w: session is not a choice problem", runner.ErrLifecycleConflict)
	}
	success := sess.CorrectChoice == choiceID
	sess.mu.Unlock()

	if durable {
		if err := s.admitVerifyThrottle(userID); err != nil {
			return false, err
		}
		decision, err := s.durableStore.RecordChoice(ctx, userID, ref, submission, success, "")
		if err != nil {
			s.rollbackVerifyThrottle(userID)
			return false, fmt.Errorf("persist choice result: %w", err)
		}
		if decision.Kind != runner.VerifyGradeReplay || decision.Session != expected || decision.Success != success {
			return false, fmt.Errorf("%w: invalid durable choice result", runner.ErrLifecycleConflict)
		}
	}

	if success {
		ref, _ = s.setTerminalIntent(sess, StatusCompleted, "success", "")
		if err := s.finalizeTerminalLocked(ctx, userID, sess, ref); err != nil {
			log.Printf("completed choice finalization for %s failed; retry scheduled: %v", userID, err)
		}
		if s.onVerify != nil {
			s.onVerify(userID, ref.Session, true, "")
		}
		if s.onEnded != nil {
			s.onEnded(userID, ref.Session, "completed")
		}
	} else if s.onVerify != nil {
		s.onVerify(userID, ref.Session, false, "")
	}

	return success, nil
}

func (s *Service) ResetEnvironment(ctx context.Context, userID string) error {
	if !s.beginProvisioning() {
		return ErrServiceStopping
	}
	defer s.provisioningWG.Done()
	provisioningCtx, cancelProvisioning := s.operationContext(ctx)
	defer cancelProvisioning()

	ul := s.userLock(userID)
	ul.Lock()
	defer ul.Unlock()

	sess := s.GetSession(userID)
	if sess == nil {
		return fmt.Errorf("no active session")
	}
	sess.mu.Lock()
	terminalPending := sess.terminalAttemptStatus != ""
	pendingRef := sess.Allocation
	sess.mu.Unlock()
	if terminalPending {
		if err := s.finalizeTerminalLocked(provisioningCtx, userID, sess, pendingRef); err != nil {
			return fmt.Errorf("session terminal cleanup is still pending: %w", err)
		}
		return errors.New("session already ended while cleanup was pending")
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

	sess.mu.Lock()
	oldRef := sess.Allocation
	selection := sess.Selection
	nextGeneration := sess.Generation + 1
	if sess.setupCancel != nil {
		sess.setupCancel()
	}
	sess.Status = StatusCreating
	sess.mu.Unlock()
	if err := s.runner.DestroySession(provisioningCtx, oldRef); err != nil {
		_, newlyTerminal := s.setTerminalIntent(sess, StatusFailed, "failed", "reset failed while destroying the previous environment: "+err.Error())
		if persistErr := s.persistTerminalAttempt(context.Background(), sess); persistErr != nil {
			log.Printf("persist reset destroy failure for %s failed; retry scheduled: %v", userID, persistErr)
		}
		if newlyTerminal && s.onEnded != nil {
			s.onEnded(userID, oldRef.Session, "environment_failed")
		}
		return fmt.Errorf("destroy previous environment: %w", err)
	}
	sess.mu.Lock()
	// Reset holds the per-user transition lock, so cancelled generation-1
	// setup cannot commit while destroy is running. Advance only after exact
	// old-allocation cleanup succeeds; otherwise cleanup retry retains a
	// generation-consistent target.
	sess.Generation = nextGeneration
	sess.Allocation = runner.AllocationRef{}
	sess.mu.Unlock()

	createSession := runner.SessionRef{SessionID: sess.ID, Generation: nextGeneration}
	ref, err := s.runner.CreateSession(provisioningCtx, runner.CreateSessionRequest{
		AllocationID:    runner.AllocationIDForSession(createSession),
		Session:         createSession,
		UserID:          userID,
		Selection:       selection,
		ResourceProfile: runner.DefaultResourceProfile,
		ExpiresAt:       sess.TimeoutAt,
		IdempotencyKey:  fmt.Sprintf("create:%s:%d", sess.ID, nextGeneration),
	})
	if err != nil {
		_, newlyTerminal := s.setTerminalIntent(sess, StatusFailed, "failed", "reset failed while creating the replacement environment: "+err.Error())
		if finalizeErr := s.finalizeTerminalLocked(context.Background(), userID, sess, runner.AllocationRef{}); finalizeErr != nil {
			log.Printf("finalize reset create failure for %s failed; retry scheduled: %v", userID, finalizeErr)
		}
		if newlyTerminal && s.onEnded != nil {
			// No successful reset event was sent, so the browser still owns the
			// previous generation identity. Terminate that visible session.
			s.onEnded(userID, oldRef.Session, "environment_failed")
		}
		return fmt.Errorf("recreate environment: %w", err)
	}

	sess.mu.Lock()
	sess.Allocation = ref
	stopping := s.isStopping() || provisioningCtx.Err() != nil
	if !stopping {
		sess.Status = StatusBooting
	}
	sess.timeoutWarned = false // WS-3: warn once per environment
	sess.mu.Unlock()
	if stopping {
		s.setTerminalIntent(sess, StatusFailed, "failed", "server stopped while recreating the environment")
		cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), 10*time.Second)
		finalizeErr := s.finalizeTerminalLocked(cleanupCtx, userID, sess, ref)
		cancelCleanup()
		if finalizeErr != nil {
			log.Printf("late reset allocation cleanup for %s failed; shutdown cleanup will retry: %v", userID, finalizeErr)
		}
		return ErrServiceStopping
	}
	s.announceResetGeneration(sess, ref.Session)

	attempt, _ := s.problemStore.GetAttempt(provisioningCtx, sess.AttemptID)
	s.launchSetup(sess, attempt, ref)

	s.mu.Lock()
	s.lastStart[userID] = time.Now()
	s.mu.Unlock()

	return nil
}

// ResetEnvironmentOperation durably advances exactly one generation for one
// idempotent client operation. The old allocation is destroyed and proven
// absent before any provider mutation for the replacement generation.
func (s *Service) ResetEnvironmentOperation(ctx context.Context, userID, expectedProblemID string, expected runner.SessionRef, operationID string) (*CurrentSession, error) {
	if s.durableStore == nil {
		return nil, errors.New("durable session lifecycle is not configured")
	}
	if expectedProblemID == "" {
		return nil, errors.New("reset problem id is required")
	}
	if expected.SessionID == "" || expected.Generation == 0 || expected.Generation == ^uint64(0) {
		return nil, errors.New("reset source session is invalid")
	}
	if operationID == "" {
		return nil, errors.New("reset operation id is required")
	}
	if !s.beginProvisioning() {
		return nil, ErrServiceStopping
	}
	defer s.provisioningWG.Done()
	provisioningCtx, cancelProvisioning := s.operationContext(ctx)
	defer cancelProvisioning()

	ul := s.userLock(userID)
	if !ul.TryLock() {
		return nil, ErrTransitionBusy
	}
	defer ul.Unlock()

	providerKey := "reset:" + runner.SessionIDForOperation(userID, operationID)
	prior, replay, err := s.durableStore.FindReservation(provisioningCtx, s.providerID, providerKey)
	if err != nil {
		return nil, fmt.Errorf("load durable reset replay: %w", err)
	}

	if replay {
		if prior.Session.UserID != userID || prior.Session.ID != expected.SessionID ||
			prior.Session.Selection.Problem.ID != expectedProblemID ||
			prior.Allocation.Ref.Session.SessionID != expected.SessionID ||
			prior.Allocation.Ref.Session.Generation != expected.Generation+1 {
			return nil, runner.ErrIdempotencyConflict
		}
	} else {
		sess := s.GetSession(userID)
		if sess == nil {
			return nil, errors.New("no active session")
		}
		sess.mu.Lock()
		if sess.terminalAttemptStatus != "" {
			sess.mu.Unlock()
			return nil, fmt.Errorf("%w: session cleanup is pending", runner.ErrLifecycleConflict)
		}
		if sess.Status != StatusReady {
			sess.mu.Unlock()
			return nil, fmt.Errorf("%w: reset source is %s", runner.ErrLifecycleConflict, sess.Status)
		}
		if sess.ProblemID != expectedProblemID {
			sess.mu.Unlock()
			return nil, fmt.Errorf("%w: reset problem does not match active session", runner.ErrLifecycleConflict)
		}
		if sess.Allocation.Session != expected {
			sess.mu.Unlock()
			return nil, runner.ErrGenerationStale
		}
		sess.mu.Unlock()

		now := time.Now()
		s.mu.Lock()
		if last, ok := s.lastStart[userID]; ok && now.Sub(last) < s.startCooldown {
			s.mu.Unlock()
			return nil, ErrStartCooldown
		}
		s.mu.Unlock()
	}

	reset, err := s.durableStore.ReserveReset(provisioningCtx, runner.ReserveResetParams{
		Expected: expected, UserID: userID, Provider: s.runner.Kind(),
		ProviderID: s.providerID, IdempotencyKey: providerKey,
	})
	if err != nil {
		return nil, fmt.Errorf("reserve durable reset: %w", err)
	}
	if reset.New.Session.Selection.Problem.ID != expectedProblemID {
		return nil, runner.ErrIdempotencyConflict
	}
	if replay && reset.New.Operation.ID != prior.Operation.ID {
		return nil, runner.ErrIdempotencyConflict
	}

	current := s.GetSession(userID)
	if current == nil || current.ID != reset.New.Session.ID {
		return nil, runner.ErrActiveSession
	}
	target := reset.New.Allocation.Ref.Session
	current.mu.Lock()
	currentGeneration := current.Generation
	sameTarget := current.Generation == target.Generation &&
		current.Allocation == reset.New.Allocation.Ref && current.OperationID == reset.New.Operation.ID
	if replay && reset.New.Operation.State == "succeeded" && sameTarget {
		snapshot := currentSessionSnapshot(current)
		current.mu.Unlock()
		return s.authoritativeOperationSnapshot(provisioningCtx, userID, snapshot)
	}
	current.mu.Unlock()

	// Exact historical replays remain valid acknowledgements for their own
	// operation, but cannot roll the process-local current generation back.
	if currentGeneration > target.Generation {
		if replay {
			return durableReservationSnapshot(reset.New), nil
		}
		return nil, runner.ErrGenerationStale
	}
	if currentGeneration != expected.Generation && !sameTarget {
		return nil, runner.ErrGenerationStale
	}
	current.mu.Lock()
	if current.setupCancel != nil {
		current.setupCancel()
	}
	current.Generation = reset.New.Allocation.Ref.Session.Generation
	current.Allocation = reset.New.Allocation.Ref
	current.AttemptID = reset.New.AttemptID
	current.OperationID = reset.New.Operation.ID
	current.Status = StatusCreating
	current.timeoutWarned = false
	current.mu.Unlock()
	// ReserveReset has already committed generation N+1. Publish that durable
	// identity before destroy/create/setup can emit any N+1 progress, otherwise
	// the browser correctly rejects those stages as future-generation frames.
	s.announceResetGeneration(current, reset.New.Allocation.Ref.Session)

	// ReserveReset already committed old desired-absent and its destroy
	// operation. Absence is idempotent; a retry repeats these exact steps.
	if err := s.executeClaimedDestroy(provisioningCtx, reset.Old, s.workerOwner+"-reset-request"); err != nil {
		if errors.Is(err, ErrCleanupPending) {
			snapshot, snapshotErr := s.GetCurrentSessionSnapshot(provisioningCtx, userID)
			if snapshotErr != nil {
				return nil, errors.Join(err, snapshotErr)
			}
			if snapshot == nil || snapshot.SessionID != reset.New.Session.ID ||
				snapshot.Generation != reset.New.Allocation.Ref.Session.Generation ||
				snapshot.OperationID != reset.New.Operation.ID {
				return nil, fmt.Errorf("%w: reset replacement lifecycle identity changed", runner.ErrLifecycleConflict)
			}
			snapshot.CleanupPending = true
			return snapshot, ErrCleanupPending
		}
		return nil, fmt.Errorf("destroy reset source environment: %w", err)
	}
	createClaim, claimed, err := s.durableStore.ClaimProvisionableCreate(
		provisioningCtx, s.providerID, reset.New.Operation.ID, s.workerOwner+"-request", durableCreateLease,
	)
	if err != nil {
		return nil, fmt.Errorf("claim reset replacement create: %w", err)
	}
	if !claimed {
		// A background worker can win the claim between absence commit and
		// this request. It will resume after this per-user lock is released.
		return s.GetCurrentSessionSnapshot(provisioningCtx, userID)
	}
	if createClaim.OperationID != reset.New.Operation.ID || createClaim.Reservation.Allocation.Ref != reset.New.Allocation.Ref {
		return nil, fmt.Errorf("%w: claimed a different reset replacement", runner.ErrLifecycleConflict)
	}

	trustedProblem, err := s.problemCatalog.ResolveProblem(provisioningCtx, reset.New.Session.Selection.Problem)
	if err != nil {
		cleanupErr := s.failReservedCreateClaim(reset.New.Allocation.Ref, &createClaim, err)
		s.evictSession(userID, current)
		return nil, errors.Join(fmt.Errorf("resolve approved reset problem revision: %w", err), cleanupErr)
	}
	snapshot, err := s.provisionDurableReservationWithClaim(provisioningCtx, userID, trustedProblem, reset.New, &createClaim)
	if err != nil {
		// A stale request claim means a successor worker owns the same exact
		// idempotent create. Preserve the hydrated generation for that worker.
		if !errors.Is(err, runner.ErrLifecycleConflict) {
			s.evictSession(userID, current)
		}
		return nil, err
	}
	s.mu.Lock()
	s.lastStart[userID] = time.Now()
	s.mu.Unlock()
	return snapshot, nil
}

func (s *Service) evictSession(userID string, expected *Session) {
	s.mu.Lock()
	if s.sessions[userID] == expected {
		delete(s.sessions, userID)
	}
	s.mu.Unlock()
}

func (s *Service) evictAllocation(ref runner.AllocationRef) {
	s.mu.Lock()
	for userID, sess := range s.sessions {
		sess.mu.Lock()
		matches := sess.Allocation == ref
		sess.mu.Unlock()
		if matches {
			delete(s.sessions, userID)
			break
		}
	}
	s.mu.Unlock()
}

func (s *Service) userLock(userID string) *userTransitionLock {
	v, _ := s.userMu.LoadOrStore(userID, newUserTransitionLock())
	return v.(*userTransitionLock)
}

func (s *Service) EndSession(ctx context.Context, userID string) error {
	if !s.isAccepting() {
		return ErrServiceStopping
	}
	return s.endSession(ctx, userID, "failed")
}

// EndSessionOperation binds a browser end request to one exact logical
// session generation. The durable store marks the complete generation chain
// desired-absent before any provider call and returns completed only after
// every allocation has authoritative absence proof.
func (s *Service) EndSessionOperation(
	ctx context.Context,
	userID string,
	expected runner.SessionRef,
	operationID string,
) (runner.EndDecision, error) {
	if !s.isAccepting() {
		return runner.EndDecision{}, ErrServiceStopping
	}
	if s.durableStore == nil || s.providerID == "" {
		return runner.EndDecision{}, errors.New("durable end operation is not configured")
	}
	if expected.SessionID == "" || expected.Generation == 0 || operationID == "" {
		return runner.EndDecision{}, errors.New("end operation identity is invalid")
	}
	ul := s.userLock(userID)
	if err := ul.LockContext(ctx); err != nil {
		return runner.EndDecision{}, err
	}
	defer ul.Unlock()

	decision, err := s.durableStore.RequestEnd(ctx, userID, expected, operationID)
	if err != nil {
		if errors.Is(err, runner.ErrOperationOutcomeUnknown) {
			// The commit may have made the generation desired-absent. Fence the
			// exact local allocation before any reconciliation or provider work.
			s.markEndCommittedLocked(userID, expected)
		}
		return runner.EndDecision{}, err
	}
	// RequestEnd has committed desired-absent. Fence only the exact local
	// generation while the same per-user transition lock is still held, before
	// cancellation or slow provider cleanup can create a terminal-commit gap.
	s.markEndCommittedLocked(userID, expected)
	for _, ref := range decision.Pending {
		err := s.executeClaimedDestroy(ctx, ref, s.workerOwner+"-end-request")
		if err != nil && !errors.Is(err, ErrCleanupPending) {
			return runner.EndDecision{}, err
		}
	}
	decision, err = s.durableStore.RequestEnd(ctx, userID, expected, operationID)
	if err != nil {
		return runner.EndDecision{}, err
	}
	if decision.State == runner.EndCompleted {
		s.evictLogicalSession(expected.SessionID)
	}
	return decision, nil
}

// markEndCommittedLocked makes a successful durable end commit immediately
// terminal-ineligible in this process. The caller holds userID's transition
// lock. A historical replay must never fence a newer replacement generation.
func (s *Service) markEndCommittedLocked(userID string, expected runner.SessionRef) {
	s.mu.RLock()
	sess := s.sessions[userID]
	s.mu.RUnlock()
	if sess == nil {
		return
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.ID != expected.SessionID || sess.Generation != expected.Generation ||
		sess.Allocation.Session != expected {
		return
	}
	sess.terminalIneligible = true
	if sess.setupCancel != nil {
		sess.setupCancel()
	}
}

func (s *Service) evictLogicalSession(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for userID, sess := range s.sessions {
		sess.mu.Lock()
		matches := sess.ID == sessionID
		sess.mu.Unlock()
		if matches {
			delete(s.sessions, userID)
		}
	}
}

// endSession tears down the user's session, recording attemptStatus on the
// in-progress attempt: "failed" for user-initiated end / start-replacement,
// "timeout" when the timeout watcher expires the session. The caller must NOT
// hold the per-user lock.
func (s *Service) endSession(ctx context.Context, userID, attemptStatus string) error {
	ul := s.userLock(userID)
	if err := ul.LockContext(ctx); err != nil {
		return err
	}
	defer ul.Unlock()
	return s.endSessionLocked(ctx, userID, attemptStatus)
}

// endSessionLocked tears down a session; the caller must hold the per-user lock.
func (s *Service) endSessionLocked(ctx context.Context, userID, attemptStatus string) error {
	s.mu.RLock()
	sess := s.sessions[userID]
	s.mu.RUnlock()
	if sess != nil {
		status := StatusFailed
		if attemptStatus == "timeout" {
			status = StatusTimeout
		}
		ref, _ := s.setTerminalIntent(sess, status, attemptStatus, "")
		if err := s.finalizeTerminalLocked(ctx, userID, sess, ref); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) GetSession(userID string) *Session {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.sessions[userID]
}

// CurrentSession is the user-facing snapshot of an active session. It is the
// response shape of GET /api/sessions/current and POST /api/problems/:id/start.
type CurrentSession struct {
	RequestID     string    `json:"request_id,omitempty"`
	OperationID   string    `json:"operation_id,omitempty"`
	SessionID     string    `json:"session_id"`
	ProblemID     string    `json:"problem_id"`
	Generation    uint64    `json:"generation"`
	Status        Status    `json:"status"`
	TimeoutAt     time.Time `json:"timeout_at"`
	EventSequence uint64    `json:"event_sequence"`
	// CleanupPending is set only on a reset HTTP 202: this generation is
	// durably reserved while the lifecycle worker removes its predecessor.
	CleanupPending bool    `json:"cleanup_pending"`
	TerminalReason *string `json:"terminal_reason"`
}

// GetCurrentSessionSnapshot returns the PostgreSQL-authoritative REST view.
// Status and cursor are read from one repeatable-read lifecycle snapshot, so a
// client never receives a newer watermark paired with an older process-local
// status. Legacy in-memory services use the local projection only in tests.
func (s *Service) GetCurrentSessionSnapshot(ctx context.Context, userID string) (*CurrentSession, error) {
	if s.durableStore == nil {
		return s.GetCurrentSession(userID), nil
	}
	bootstrap, err := s.durableStore.BootstrapLifecycle(ctx, userID, nil)
	if err != nil {
		return nil, fmt.Errorf("load authoritative current session: %w", err)
	}
	if bootstrap.Snapshot == nil {
		return nil, nil
	}
	return currentSessionFromLifecycle(*bootstrap.Snapshot), nil
}

func (s *Service) authoritativeOperationSnapshot(ctx context.Context, userID string, expected *CurrentSession) (*CurrentSession, error) {
	if expected == nil {
		return nil, errors.New("expected operation snapshot is missing")
	}
	if s.durableStore == nil {
		return expected, nil
	}
	current, err := s.GetCurrentSessionSnapshot(ctx, userID)
	if err != nil {
		return nil, err
	}
	if current == nil || current.SessionID != expected.SessionID || current.ProblemID != expected.ProblemID ||
		current.Generation != expected.Generation || current.OperationID != expected.OperationID {
		return nil, fmt.Errorf("%w: authoritative lifecycle identity changed", runner.ErrLifecycleConflict)
	}
	return current, nil
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
		OperationID: sess.OperationID,
		SessionID:   sess.ID,
		ProblemID:   sess.ProblemID,
		Generation:  sess.Generation,
		Status:      sess.Status,
		TimeoutAt:   sess.TimeoutAt,
	}
}

func currentSessionSnapshot(sess *Session) *CurrentSession {
	return &CurrentSession{
		OperationID: sess.OperationID,
		SessionID:   sess.ID,
		ProblemID:   sess.ProblemID,
		Generation:  sess.Generation,
		Status:      sess.Status,
		TimeoutAt:   sess.TimeoutAt,
	}
}

func currentSessionFromLifecycle(snapshot runner.LifecycleSnapshot) *CurrentSession {
	var terminalReason *string
	if snapshot.TerminalReason != "" {
		reason := snapshot.TerminalReason
		terminalReason = &reason
	}
	return &CurrentSession{
		OperationID: snapshot.OperationID, SessionID: snapshot.SessionID,
		ProblemID: snapshot.ProblemID, Generation: snapshot.Generation,
		Status: Status(snapshot.Status), TimeoutAt: snapshot.TimeoutAt,
		EventSequence: snapshot.EventSequence, CleanupPending: snapshot.CleanupPending,
		TerminalReason: terminalReason,
	}
}

func durableReservationSnapshot(reservation runner.SessionReservation) *CurrentSession {
	status := StatusCreating
	switch reservation.Operation.State {
	case "succeeded":
		status = StatusBooting
	case "failed":
		status = StatusFailed
	}
	return &CurrentSession{
		OperationID:   reservation.Operation.ID,
		SessionID:     reservation.Allocation.Ref.Session.SessionID,
		ProblemID:     reservation.Session.Selection.Problem.ID,
		Generation:    reservation.Allocation.Ref.Session.Generation,
		Status:        status,
		TimeoutAt:     reservation.Allocation.ExpiresAt,
		EventSequence: reservation.Allocation.LastEventSequence,
	}
}

// hydrateReservation reconstructs the process-local projection of one
// authoritative current generation. It is intentionally strict: the cache is
// never allowed to paper over contradictory durable identity/state, and states
// without implemented replay semantics keep startup closed.
func (s *Service) hydrateReservation(reservation runner.SessionReservation, trustedProblem models.Problem) (*Session, error) {
	ref := reservation.Allocation.Ref
	if reservation.Session.ID == "" || reservation.Session.UserID == "" ||
		reservation.Session.Selection.Generation == 0 ||
		reservation.Session.Selection.Problem.ID == "" || reservation.Session.Selection.Problem.Revision == "" ||
		reservation.Session.CurrentGeneration == 0 || ref.ID == "" ||
		ref.Session.SessionID != reservation.Session.ID ||
		ref.Session.Generation != reservation.Session.CurrentGeneration ||
		reservation.Operation.ID == "" || reservation.Operation.AllocationID != ref.ID ||
		reservation.AttemptID == "" || reservation.Session.DesiredState != "active" ||
		reservation.Allocation.DesiredState != "active" {
		return nil, fmt.Errorf("%w: durable recovery reservation is contradictory", runner.ErrLifecycleConflict)
	}
	if trustedProblem.ID != reservation.Session.Selection.Problem.ID || trustedProblem.Revision != reservation.Session.Selection.Problem.Revision {
		return nil, fmt.Errorf("%w: recovery problem revision does not match reservation", runner.ErrInvalidRevision)
	}
	if reservation.AttemptStatus != "" && reservation.AttemptStatus != "in_progress" {
		return nil, fmt.Errorf("%w: recovery attempt is %s", runner.ErrLifecycleConflict, reservation.AttemptStatus)
	}

	status := StatusCreating
	switch reservation.Operation.State {
	case "pending", "running":
		if reservation.Session.State != "queued" && reservation.Session.State != "provisioning" {
			return nil, fmt.Errorf("%w: unfinished create has session state %s", runner.ErrLifecycleConflict, reservation.Session.State)
		}
	case "succeeded":
		switch reservation.Session.State {
		case "queued", "provisioning", "booting":
			status = StatusBooting
		case "setting_up":
			status = StatusSettingUp
		case "ready":
			status = StatusReady
		case "verifying":
			return nil, fmt.Errorf("%w: durable verification recovery is not implemented", runner.ErrLifecycleConflict)
		default:
			return nil, fmt.Errorf("%w: succeeded create has session state %s", runner.ErrLifecycleConflict, reservation.Session.State)
		}
	default:
		return nil, fmt.Errorf("%w: active recovery create operation is %s", runner.ErrLifecycleConflict, reservation.Operation.State)
	}

	sess := &Session{
		ID: reservation.Session.ID, UserID: reservation.Session.UserID,
		ProblemID: trustedProblem.ID, Selection: reservation.Session.Selection,
		VerifyType: trustedProblem.VerifyType, CorrectChoice: trustedProblem.CorrectChoice,
		GradingPrompt: trustedProblem.GradingPrompt, AttemptID: reservation.AttemptID,
		OperationID: reservation.Operation.ID, Generation: ref.Session.Generation,
		Allocation: ref, Status: status, StartedAt: reservation.Session.QueuedAt,
		TimeoutAt: reservation.Allocation.ExpiresAt,
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.accepting || s.watchersStarted {
		return nil, errors.New("durable recovery hydration requires a paused service")
	}
	if current := s.sessions[sess.UserID]; current != nil {
		current.mu.Lock()
		same := current.ID == sess.ID && current.Generation == sess.Generation && current.Allocation == sess.Allocation
		current.mu.Unlock()
		if same {
			return current, nil
		}
		return nil, fmt.Errorf("%w: multiple current sessions for one user during recovery", runner.ErrActiveSession)
	}
	s.sessions[sess.UserID] = sess
	return sess, nil
}

func (s *Service) CleanupAll(ctx context.Context) error {
	if s.durableStore != nil {
		return s.cleanupAllDurable(ctx)
	}
	s.mu.RLock()
	userIDs := make([]string, 0, len(s.sessions))
	for userID := range s.sessions {
		userIDs = append(userIDs, userID)
	}
	s.mu.RUnlock()
	var errs []error
	for _, userID := range userIDs {
		if err := s.endSession(ctx, userID, "failed"); err != nil {
			errs = append(errs, fmt.Errorf("cleanup user %s: %w", userID, err))
		}
	}
	return errors.Join(errs...)
}

// cleanupAllDurable has a deliberate two-phase shutdown contract:
//
//  1. snapshot every DB-authoritative active allocation and persist its
//     desired-absent intent without making any provider call;
//  2. only after the complete snapshot was attempted, drain destroy work with
//     bounded concurrency. Replacement creates are never run during shutdown.
//
// A provider timeout may therefore leave physical cleanup pending, but it
// cannot leave an unrecorded active allocation merely because an earlier
// destroy consumed the shutdown deadline. The next fenced controller resumes
// the retained destroy operations.
func (s *Service) cleanupAllDurable(ctx context.Context) error {
	reservations, err := s.durableStore.ListActiveReservations(ctx, s.providerID)
	if err != nil {
		return fmt.Errorf("snapshot durable shutdown cleanup: %w", err)
	}

	var intentErrs []error
	for _, reservation := range reservations {
		if ctx.Err() != nil {
			intentErrs = append(intentErrs, ctx.Err())
			break
		}
		ref := reservation.Allocation.Ref
		outcome, attemptStatus, verifyLog := "failed", "failed", "server shutdown"
		if sess := s.GetSession(reservation.Session.UserID); sess != nil {
			sess.mu.Lock()
			if sess.Allocation == ref {
				if sess.terminalAttemptStatus == "" {
					sess.Status = StatusFailed
					sess.terminalAttemptStatus = "failed"
					sess.terminalVerifyLog = "server shutdown"
				}
				outcome = durableOutcome(sess.Status)
				attemptStatus = sess.terminalAttemptStatus
				verifyLog = sess.terminalVerifyLog
			}
			sess.mu.Unlock()
		}
		if err := s.durableStore.RequestDestroy(ctx, ref, outcome, attemptStatus, verifyLog); err != nil {
			intentErrs = append(intentErrs, fmt.Errorf("persist shutdown cleanup for allocation %s: %w", ref.ID, err))
		}
	}

	_, drainErr := s.drainDestroyWork(ctx, shutdownDestroyWorkers)
	return errors.Join(append(intentErrs, drainErr)...)
}

type sessionEvent struct {
	userID  string
	session runner.SessionRef
}

func (s *Service) checkCrashes() []sessionEvent {
	return s.checkCrashesContext(context.Background())
}

func (s *Service) checkCrashesContext(ctx context.Context) []sessionEvent {
	s.mu.RLock()
	snapshot := make(map[string]*Session, len(s.sessions))
	for userID, sess := range s.sessions {
		snapshot[userID] = sess
	}
	s.mu.RUnlock()
	var crashed []sessionEvent
	for userID, sess := range snapshot {
		sess.mu.Lock()
		active := sess.Status == StatusReady || sess.Status == StatusBooting || sess.Status == StatusSettingUp || sess.Status == StatusVerifying
		ref := sess.Allocation
		sess.mu.Unlock()
		if !active {
			continue
		}
		observation, err := s.runner.GetSession(ctx, ref)
		if err != nil || (observation.State != runner.ObservedStopped && observation.State != runner.ObservedAbsent) {
			continue
		}
		ul := s.userLock(userID)
		if !ul.TryLock() {
			continue
		}
		if s.isCurrent(userID, sess, ref) {
			sess.mu.Lock()
			stillActive := sess.Status == StatusReady || sess.Status == StatusBooting || sess.Status == StatusSettingUp || sess.Status == StatusVerifying
			sess.mu.Unlock()
			if stillActive {
				_, newlyTerminal := s.setTerminalIntent(sess, StatusFailed, "failed", "provider allocation stopped unexpectedly")
				if err := s.finalizeTerminalLocked(ctx, userID, sess, ref); err != nil {
					log.Printf("crashed session finalization for %s failed; retry scheduled: %v", userID, err)
				}
				if newlyTerminal {
					crashed = append(crashed, sessionEvent{userID: userID, session: ref.Session})
				}
			}
		}
		ul.Unlock()
	}
	return crashed
}

func (s *Service) crashWatcher(ctx context.Context) {
	defer s.watcherWG.Done()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for _, event := range s.checkCrashesContext(ctx) {
				s.emitStage(event.userID, event.session, "provider_lost", "The environment stopped unexpectedly and was removed.")
				if s.onCrash != nil {
					s.onCrash(event.userID, event.session)
				}
			}
		}
	}
}

// checkTimeouts expires every session past its deadline, persisting attempt
// status "timeout" and tearing the container down. It returns the expired
// userIDs so the watcher can fire the session_ended{reason:"timeout"} callback.
func (s *Service) checkTimeouts() []sessionEvent {
	return s.checkTimeoutsContext(context.Background())
}

func (s *Service) checkTimeoutsContext(ctx context.Context) []sessionEvent {
	type candidate struct {
		userID string
		sess   *Session
		ref    runner.AllocationRef
	}
	s.mu.RLock()
	var candidates []candidate
	var warnings []struct {
		userID    string
		session   runner.SessionRef
		remaining int
	}
	for userID, sess := range s.sessions {
		sess.mu.Lock()
		if time.Now().After(sess.TimeoutAt) && sess.Status != StatusCompleted && sess.Status != StatusTimeout {
			candidates = append(candidates, candidate{userID: userID, sess: sess, ref: sess.Allocation})
		}
		remaining := time.Until(sess.TimeoutAt)
		if remaining > 0 && remaining < 5*time.Minute && sess.Status == StatusReady && !sess.timeoutWarned {
			sess.timeoutWarned = true // WS-3: fire ONCE per session
			warnings = append(warnings, struct {
				userID    string
				session   runner.SessionRef
				remaining int
			}{userID: userID, session: sess.Allocation.Session, remaining: int(remaining.Seconds())})
		}
		sess.mu.Unlock()
	}
	s.mu.RUnlock()

	for _, warning := range warnings {
		if s.onTimeoutWarn != nil {
			s.onTimeoutWarn(warning.userID, warning.session, warning.remaining)
		}
	}

	var expired []sessionEvent
	for _, candidate := range candidates {
		ul := s.userLock(candidate.userID)
		if !ul.TryLock() {
			continue
		}
		if s.isCurrent(candidate.userID, candidate.sess, candidate.ref) {
			candidate.sess.mu.Lock()
			shouldExpire := time.Now().After(candidate.sess.TimeoutAt) && candidate.sess.Status != StatusCompleted && candidate.sess.Status != StatusTimeout
			candidate.sess.mu.Unlock()
			if shouldExpire {
				_, newlyTerminal := s.setTerminalIntent(candidate.sess, StatusTimeout, "timeout", "")
				if err := s.finalizeTerminalLocked(ctx, candidate.userID, candidate.sess, candidate.ref); err != nil {
					log.Printf("timeout finalization for %s failed; retry scheduled: %v", candidate.userID, err)
				}
				if newlyTerminal {
					expired = append(expired, sessionEvent{userID: candidate.userID, session: candidate.ref.Session})
				}
			}
		}
		ul.Unlock()
	}
	return expired
}

func (s *Service) timeoutWatcher(ctx context.Context) {
	defer s.watcherWG.Done()
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for _, event := range s.checkTimeoutsContext(ctx) {
				if s.onTimeout != nil {
					s.onTimeout(event.userID, event.session)
				}
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

func (s *Service) emitStage(userID string, session runner.SessionRef, stage, message string) {
	if s.onStage != nil {
		s.onStage(userID, session, stage, message)
	}
}

// GetTerminalTarget returns only the exact generation-bound Runner allocation
// requested by the screen and only while it is still the current ready session.
func (s *Service) GetTerminalTarget(userID string, expected runner.SessionRef) (runner.AllocationRef, bool) {
	if !s.isAccepting() {
		return runner.AllocationRef{}, false
	}
	if expected.SessionID == "" || expected.Generation == 0 {
		return runner.AllocationRef{}, false
	}
	sess := s.GetSession(userID)
	if sess == nil {
		return runner.AllocationRef{}, false
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	now := time.Now()
	if sess.Status != StatusReady || sess.terminalIneligible || sess.Allocation.Session != expected ||
		sess.Generation != expected.Generation || sess.TimeoutAt.IsZero() || !now.Before(sess.TimeoutAt) {
		return runner.AllocationRef{}, false
	}
	return sess.Allocation, sess.Allocation.ID != ""
}

// AcquireTerminalCommit linearizes terminal Hub ownership changes with every
// lifecycle transition for the same user. The caller must hold the returned
// lease only around the in-process Hub compare-and-swap and release it
// immediately afterwards. Reset, end, timeout, and verification paths use the
// same per-user transition lock, so none can invalidate the allocation between
// this exact authority check and the terminal commit point. Controller shutdown
// is fenced independently by accepting/authority state and socket cancellation.
func (s *Service) AcquireTerminalCommit(ctx context.Context, userID string, allocation runner.AllocationRef) (func(), bool) {
	if ctx == nil || userID == "" || allocation.ID == "" ||
		allocation.Session.SessionID == "" || allocation.Session.Generation == 0 {
		return nil, false
	}
	ul := s.userLock(userID)
	if err := ul.LockContext(ctx); err != nil {
		return nil, false
	}

	valid := s.isAccepting()
	if valid {
		s.mu.RLock()
		sess := s.sessions[userID]
		s.mu.RUnlock()
		if sess == nil {
			valid = false
		} else {
			sess.mu.Lock()
			now := time.Now()
			valid = sess.Status == StatusReady &&
				!sess.terminalIneligible &&
				sess.ID == allocation.Session.SessionID &&
				sess.Generation == allocation.Session.Generation &&
				sess.Allocation == allocation &&
				!sess.TimeoutAt.IsZero() && now.Before(sess.TimeoutAt)
			sess.mu.Unlock()
		}
	}
	if valid && s.durableStore != nil {
		bootstrap, err := s.durableStore.BootstrapLifecycle(ctx, userID, nil)
		now := time.Now()
		valid = err == nil && bootstrap.Snapshot != nil &&
			bootstrap.Snapshot.SessionID == allocation.Session.SessionID &&
			bootstrap.Snapshot.Generation == allocation.Session.Generation &&
			bootstrap.Snapshot.Status == string(StatusReady) &&
			!bootstrap.Snapshot.TimeoutAt.IsZero() && now.Before(bootstrap.Snapshot.TimeoutAt)
	}
	if !valid || ctx.Err() != nil {
		ul.Unlock()
		return nil, false
	}

	var once sync.Once
	return func() {
		once.Do(ul.Unlock)
	}, true
}

func (s *Service) isCurrent(userID string, sess *Session, ref runner.AllocationRef) bool {
	s.mu.RLock()
	current := s.sessions[userID]
	s.mu.RUnlock()
	if current != sess {
		return false
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	return sess.Allocation == ref && sess.Generation == ref.Session.Generation
}

func (s *Service) ownsAllocation(userID string, sess *Session, ref runner.AllocationRef) bool {
	s.mu.RLock()
	current := s.sessions[userID]
	s.mu.RUnlock()
	if current != sess {
		return false
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	return sess.Allocation == ref
}

func (s *Service) failEnvironment(ctx context.Context, sess *Session, attempt *models.Attempt, ref runner.AllocationRef, message string) {
	ul := s.userLock(sess.UserID)
	ul.Lock()
	defer ul.Unlock()

	if !s.isCurrent(sess.UserID, sess, ref) {
		return
	}
	_, newlyTerminal := s.setTerminalIntent(sess, StatusFailed, "failed", message)
	// finalizeTerminalLocked cancels the session setup context before it
	// persists and destroys. Use an independent bounded context so setup cannot
	// cancel its own cleanup operation.
	cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelCleanup()
	if err := s.finalizeTerminalLocked(cleanupCtx, sess.UserID, sess, ref); err != nil {
		log.Printf("failed environment finalization %s generation %d failed; retry scheduled: %v", ref.Session.SessionID, ref.Session.Generation, err)
	}
	if newlyTerminal && s.onEnded != nil {
		s.onEnded(sess.UserID, ref.Session, "environment_failed")
	}
}

func (s *Service) retryTerminalCleanup() {
	s.retryTerminalCleanupContext(context.Background())
}

func (s *Service) retryTerminalCleanupContext(ctx context.Context) {
	type target struct {
		userID string
		sess   *Session
		ref    runner.AllocationRef
	}
	s.mu.RLock()
	var targets []target
	for userID, sess := range s.sessions {
		sess.mu.Lock()
		terminal := sess.Status == StatusCompleted || sess.Status == StatusFailed || sess.Status == StatusTimeout
		if terminal && sess.terminalAttemptStatus == "" {
			switch sess.Status {
			case StatusCompleted:
				sess.terminalAttemptStatus = "success"
			case StatusTimeout:
				sess.terminalAttemptStatus = "timeout"
			default:
				sess.terminalAttemptStatus = "failed"
			}
		}
		ref := sess.Allocation
		sess.mu.Unlock()
		if terminal {
			targets = append(targets, target{userID: userID, sess: sess, ref: ref})
		}
	}
	s.mu.RUnlock()
	for _, target := range targets {
		if ctx.Err() != nil {
			return
		}
		ul := s.userLock(target.userID)
		if !ul.TryLock() {
			continue
		}
		if s.ownsAllocation(target.userID, target.sess, target.ref) {
			if err := s.finalizeTerminalLocked(ctx, target.userID, target.sess, target.ref); err != nil {
				log.Printf("retry cleanup for %s failed: %v", target.userID, err)
			}
		}
		ul.Unlock()
	}
}

func (s *Service) cleanupWatcher(ctx context.Context) {
	defer s.watcherWG.Done()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.retryTerminalCleanupContext(ctx)
			if _, err := s.drainDurableWork(ctx, durableWorkBatch); err != nil && ctx.Err() == nil {
				log.Printf("durable lifecycle worker failed: %v", err)
			}
		}
	}
}

// processDurableWorkOnce executes at most one destroy claim and one eligible
// reset replacement claim. It is intentionally callable from focused tests;
// the watcher remains only a wakeup loop over PostgreSQL authority.
func (s *Service) processDurableWorkOnce(ctx context.Context) (bool, error) {
	if s.durableStore == nil || s.workerOwner == "" {
		return false, nil
	}
	worked := false
	var workErrs []error

	destroyWorked, destroyErr := s.processDestroyWorkOnce(ctx, s.workerOwner)
	if destroyErr != nil {
		workErrs = append(workErrs, destroyErr)
	}
	if destroyWorked {
		worked = true
	}

	createWorked, err := s.processProvisionableCreateOnce(ctx)
	if err != nil {
		workErrs = append(workErrs, err)
	}
	if createWorked {
		worked = true
	}
	return worked, errors.Join(workErrs...)
}

func (s *Service) processDestroyWorkOnce(ctx context.Context, owner string) (bool, error) {
	destroyClaim, found, err := s.durableStore.ClaimDestroyWork(ctx, s.providerID, "", owner, durableDestroyLease)
	if err != nil {
		return false, fmt.Errorf("claim destroy work: %w", err)
	}
	if !found {
		return false, nil
	}
	if destroyClaim.Pending || destroyClaim.Completed {
		return true, fmt.Errorf("%w: unscoped destroy claim returned an observation", runner.ErrLifecycleConflict)
	}
	claimCtx, cancel, err := claimedDestroyContext(ctx, destroyClaim)
	if err != nil {
		return true, err
	}
	defer cancel()
	// An unscoped claim has no user identity. Snapshot an exact local owner
	// without waiting on any user lock, then acquire that user's transition
	// lock and revalidate. Only an allocation that still intersects local
	// current state holds the lock across provider mutation and eviction.
	if userID, sess, local := s.localAllocationOwner(destroyClaim.Ref); local {
		ul := s.userLock(userID)
		if err := ul.LockContext(claimCtx); err != nil {
			return true, err
		}
		if s.localAllocationCurrent(userID, sess, destroyClaim.Ref) {
			sess.mu.Lock()
			sess.terminalIneligible = true
			if sess.setupCancel != nil {
				sess.setupCancel()
			}
			sess.mu.Unlock()
			err := s.executeDestroyClaimMutation(claimCtx, ctx, destroyClaim)
			if err == nil {
				s.evictSession(userID, sess)
			}
			ul.Unlock()
			return true, err
		}
		ul.Unlock()
	}
	return true, s.executeDestroyClaimMutation(claimCtx, ctx, destroyClaim)
}

func (s *Service) localAllocationOwner(ref runner.AllocationRef) (string, *Session, bool) {
	s.mu.RLock()
	snapshot := make(map[string]*Session, len(s.sessions))
	for userID, sess := range s.sessions {
		snapshot[userID] = sess
	}
	s.mu.RUnlock()
	for userID, sess := range snapshot {
		sess.mu.Lock()
		matches := sess.Allocation == ref
		sess.mu.Unlock()
		if matches {
			return userID, sess, true
		}
	}
	return "", nil, false
}

func (s *Service) localAllocationCurrent(userID string, expected *Session, ref runner.AllocationRef) bool {
	s.mu.RLock()
	current := s.sessions[userID]
	s.mu.RUnlock()
	if current != expected || current == nil {
		return false
	}
	current.mu.Lock()
	matches := current.Allocation == ref
	current.mu.Unlock()
	return matches
}

func (s *Service) executeDestroyClaimMutation(claimCtx, persistenceCtx context.Context, destroyClaim runner.DestroyWorkClaim) error {
	destroyErr := s.runner.DestroySession(claimCtx, destroyClaim.Ref)
	if destroyErr != nil {
		retryDelay := durableRetryDelay(destroyClaim.Attempt)
		// The provider context intentionally expires before the database lease.
		// Use the still-live parent to release/reschedule the exact claim during
		// that safety margin instead of losing the retry to ctx cancellation.
		retryErr := s.durableStore.RetryDestroyWork(persistenceCtx, destroyClaim, destroyErr, retryDelay)
		return errors.Join(fmt.Errorf("destroy allocation %s: %w", destroyClaim.Ref.ID, destroyErr), retryErr)
	}
	if err := s.durableStore.MarkClaimedDestroyed(claimCtx, destroyClaim); err != nil {
		return fmt.Errorf("record claimed allocation %s absent: %w", destroyClaim.Ref.ID, err)
	}
	return nil
}

func claimedDestroyContext(parent context.Context, claim runner.DestroyWorkClaim) (context.Context, context.CancelFunc, error) {
	if claim.LeaseExpiresAt.IsZero() {
		return nil, nil, fmt.Errorf("%w: destroy claim has no expiry", runner.ErrLifecycleConflict)
	}
	latestDeadline := claim.LeaseExpiresAt.Add(-durableLeaseCommitGap)
	if !time.Now().Before(latestDeadline) {
		return nil, nil, fmt.Errorf("%w: destroy claim has insufficient lease remaining", runner.ErrLifecycleConflict)
	}
	timeoutDeadline := time.Now().Add(durableDestroyTimeout)
	if timeoutDeadline.Before(latestDeadline) {
		latestDeadline = timeoutDeadline
	}
	ctx, cancel := context.WithDeadline(parent, latestDeadline)
	return ctx, cancel, nil
}

func (s *Service) drainDestroyWork(ctx context.Context, workers int) (int, error) {
	if workers <= 0 {
		return 0, nil
	}
	type result struct {
		processed int
		err       error
	}
	results := make(chan result, workers)
	for worker := 0; worker < workers; worker++ {
		owner := fmt.Sprintf("%s-shutdown-%d", s.workerOwner, worker)
		go func() {
			processed := 0
			var errs []error
			for ctx.Err() == nil {
				worked, err := s.processDestroyWorkOnce(ctx, owner)
				if err != nil {
					errs = append(errs, err)
				}
				if !worked {
					break
				}
				processed++
			}
			results <- result{processed: processed, err: errors.Join(errs...)}
		}()
	}
	processed := 0
	var errs []error
	for worker := 0; worker < workers; worker++ {
		select {
		case result := <-results:
			processed += result.processed
			if result.err != nil {
				errs = append(errs, result.err)
			}
		case <-ctx.Done():
			errs = append(errs, ctx.Err())
			return processed, errors.Join(errs...)
		}
	}
	return processed, errors.Join(errs...)
}

func (s *Service) executeClaimedDestroy(ctx context.Context, ref runner.AllocationRef, owner string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	claim, found, err := s.durableStore.ClaimDestroyWork(ctx, s.providerID, ref.ID, owner, durableDestroyLease)
	if err != nil {
		return fmt.Errorf("claim destroy operation: %w", err)
	}
	if !found {
		return fmt.Errorf("%w: destroy operation is already leased or not claimable", runner.ErrLifecycleConflict)
	}
	if claim.Ref != ref {
		return fmt.Errorf("%w: destroy claim changed allocation identity", runner.ErrLifecycleConflict)
	}
	if claim.Pending {
		return ErrCleanupPending
	}
	if claim.Completed {
		return nil
	}
	claimCtx, cancel, err := claimedDestroyContext(ctx, claim)
	if err != nil {
		return err
	}
	defer cancel()
	destroyErr := s.runner.DestroySession(claimCtx, ref)
	if destroyErr != nil {
		retryErr := s.durableStore.RetryDestroyWork(ctx, claim, destroyErr, durableRetryDelay(claim.Attempt))
		if retryErr == nil {
			return ErrCleanupPending
		}
		return errors.Join(fmt.Errorf("destroy allocation %s: %w", ref.ID, destroyErr), retryErr)
	}
	if err := s.durableStore.MarkClaimedDestroyed(claimCtx, claim); err != nil {
		return fmt.Errorf("record allocation %s absent: %w", ref.ID, err)
	}
	return nil
}

func (s *Service) drainDurableWork(ctx context.Context, limit int) (int, error) {
	if limit <= 0 {
		return 0, nil
	}
	processed := 0
	var errs []error
	for processed < limit && ctx.Err() == nil {
		worked, err := s.processDurableWorkOnce(ctx)
		if err != nil {
			errs = append(errs, err)
		}
		if !worked {
			break
		}
		processed++
	}
	return processed, errors.Join(errs...)
}

func (s *Service) processProvisionableCreateOnce(ctx context.Context) (bool, error) {
	reservations, err := s.durableStore.ListActiveReservations(ctx, s.providerID)
	if err != nil {
		return false, fmt.Errorf("list provisionable creates: %w", err)
	}
	for _, reservation := range reservations {
		if reservation.Operation.State != "pending" && reservation.Operation.State != "running" {
			continue
		}
		userID := reservation.Session.UserID
		ul := s.userLock(userID)
		if !ul.TryLock() {
			continue
		}
		claim, found, claimErr := s.durableStore.ClaimProvisionableCreate(
			ctx, s.providerID, reservation.Operation.ID, s.workerOwner, durableCreateLease,
		)
		if claimErr != nil {
			ul.Unlock()
			return false, fmt.Errorf("claim durable create: %w", claimErr)
		}
		if !found {
			ul.Unlock()
			continue
		}
		err := s.resumeClaimedCreateLocked(ctx, claim)
		ul.Unlock()
		return true, err
	}
	return false, nil
}

func (s *Service) resumeClaimedCreateLocked(ctx context.Context, claim runner.CreateWorkClaim) error {
	reservation := claim.Reservation
	userID := reservation.Session.UserID
	if userID == "" || claim.OperationID == "" || claim.OperationID != reservation.Operation.ID {
		return errors.New("claimed create reservation is invalid")
	}

	// Refresh after acquiring the user lock and the exact operation claim.
	latest, found, err := s.durableStore.FindReservation(ctx, s.providerID, reservation.Operation.IdempotencyKey)
	if err != nil {
		return fmt.Errorf("refresh claimed create: %w", err)
	}
	if !found || latest.Operation.ID != claim.OperationID {
		return fmt.Errorf("%w: claimed replacement changed identity", runner.ErrLifecycleConflict)
	}
	if latest.Operation.State == "succeeded" || latest.Operation.State == "cleanup_required" || latest.Operation.State == "failed" {
		return nil
	}

	current := s.GetSession(userID)
	if current == nil {
		trustedProblem, resolveErr := s.problemCatalog.ResolveProblem(ctx, latest.Session.Selection.Problem)
		if resolveErr != nil {
			cleanupErr := s.failReservedCreateClaim(latest.Allocation.Ref, &claim, resolveErr)
			return errors.Join(fmt.Errorf("resolve approved recovery revision: %w", resolveErr), cleanupErr)
		}
		current, err = s.hydrateReservation(latest, trustedProblem)
		if err != nil {
			cleanupErr := s.failReservedCreateClaim(latest.Allocation.Ref, &claim, err)
			return errors.Join(err, cleanupErr)
		}
	}
	current.mu.Lock()
	matches := current.ID == latest.Session.ID && current.Generation == latest.Allocation.Ref.Session.Generation &&
		current.Allocation == latest.Allocation.Ref && current.OperationID == latest.Operation.ID
	status := current.Status
	current.mu.Unlock()
	if !matches {
		return runner.ErrGenerationStale
	}
	if status != StatusCreating {
		return nil
	}
	// A background worker may own the replacement after the HTTP request
	// returned cleanup_pending, or after process recovery. Announce the durable
	// generation before provisionDurableReservationWithClaim can launch setup.
	s.announceResetGeneration(current, latest.Allocation.Ref.Session)

	trustedProblem, err := s.problemCatalog.ResolveProblem(ctx, latest.Session.Selection.Problem)
	if err != nil {
		cleanupErr := s.failReservedCreateClaim(latest.Allocation.Ref, &claim, err)
		s.evictSession(userID, current)
		return errors.Join(fmt.Errorf("resolve approved replacement revision: %w", err), cleanupErr)
	}
	_, err = s.provisionDurableReservationWithClaim(ctx, userID, trustedProblem, latest, &claim)
	if err != nil {
		// A stale lease means a successor owns the same idempotent create.
		// Preserve the hydrated generation so that successor can converge it.
		if !errors.Is(err, runner.ErrLifecycleConflict) {
			s.evictSession(userID, current)
		}
		return fmt.Errorf("resume durable create: %w", err)
	}
	s.mu.Lock()
	s.lastStart[userID] = time.Now()
	s.mu.Unlock()
	return nil
}

func claimedCreateContext(parent context.Context, claim runner.CreateWorkClaim) (context.Context, context.CancelFunc, error) {
	if claim.LeaseExpiresAt.IsZero() {
		return nil, nil, fmt.Errorf("%w: create claim has no expiry", runner.ErrLifecycleConflict)
	}
	latestDeadline := claim.LeaseExpiresAt.Add(-durableLeaseCommitGap)
	if time.Until(latestDeadline) <= 0 {
		return nil, nil, fmt.Errorf("%w: create claim has insufficient lease remaining", runner.ErrLifecycleConflict)
	}
	timeoutDeadline := time.Now().Add(durableCreateTimeout)
	if timeoutDeadline.Before(latestDeadline) {
		latestDeadline = timeoutDeadline
	}
	ctx, cancel := context.WithDeadline(parent, latestDeadline)
	return ctx, cancel, nil
}

func durableRetryDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	shift := attempt - 1
	if shift > 6 {
		shift = 6
	}
	return time.Second * time.Duration(1<<shift)
}

func (s *Service) beginProvisioning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.accepting || (s.authority != nil && !s.authority.Ready()) {
		return false
	}
	s.provisioningWG.Add(1)
	return true
}

func (s *Service) BeginShutdown() {
	s.mu.Lock()
	s.accepting = false
	s.mu.Unlock()
	s.lifecycleCancel()
}

func (s *Service) operationContext(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	stopLifecycleCancel := context.AfterFunc(s.lifecycleCtx, cancel)
	return ctx, func() {
		stopLifecycleCancel()
		cancel()
	}
}

func (s *Service) isStopping() bool {
	return !s.isAccepting()
}

func (s *Service) isAccepting() bool {
	s.mu.RLock()
	accepting := s.accepting
	authority := s.authority
	s.mu.RUnlock()
	return accepting && (authority == nil || authority.Ready())
}

func (s *Service) IsAccepting() bool {
	return s.isAccepting()
}

func (s *Service) WaitForProvisioning(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		s.provisioningWG.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Service) WaitForWatchers(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		s.watcherWG.Wait()
		s.setupWG.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Service) isSessionRef(sess *Session, ref runner.AllocationRef) bool {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	return sess.Allocation == ref && sess.Generation == ref.Session.Generation
}

func (s *Service) ownsSessionRef(sess *Session, ref runner.AllocationRef) bool {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	return sess.Allocation == ref
}
