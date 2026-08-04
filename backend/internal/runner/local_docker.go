package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	containerapi "github.com/k8s-quiz/backend/internal/container"
)

const (
	DefaultResourceProfile = "standard"
	maxRunnerOutputBytes   = 64 * 1024
	defaultOperationTTL    = 30 * time.Minute
	defaultOperationLimit  = 2048
	lateCreateCleanupLimit = 20 * time.Second
)

// LocalDockerRuntime is adapter-private mechanism configuration returned by
// a trusted catalog. Browser and session-domain inputs can never construct it.
type LocalDockerRuntime struct {
	Revision     string
	Image        string
	SetupScript  string
	VerifyScript string
	VerifyType   string
}

type LocalDockerRuntimeCatalog interface {
	ResolveRuntime(context.Context, ProblemRef) (LocalDockerRuntime, error)
}

// LocalDockerVerifier is a provider-private capability. Keeping it separate
// from container.Manager prevents lifecycle code from silently treating an
// arbitrary learner exec as a trusted grade. The only built-in implementation
// is explicitly named DevelopmentGuestVerifier and cannot produce trusted
// assurance.
type LocalDockerVerifier interface {
	Verify(context.Context, LocalDockerVerifyRequest) (VerifyResult, error)
}

type LocalDockerVerifyRequest struct {
	Request     VerifyRequest
	ContainerID string
	Runtime     LocalDockerRuntime
}

type localAllocation struct {
	ref        AllocationRef
	request    CreateSessionRequest
	runtime    LocalDockerRuntime
	target     containerapi.OwnedAllocationTarget
	destroying bool
	setup      *setupRecord
	leases     map[string]*managedTerminal
}

type createRecord struct {
	requestHash      string
	allocation       AllocationRef
	done             chan struct{}
	err              error
	destroyRequested bool
	destroyErr       error
	completedAt      time.Time
}

type verifyRecord struct {
	request     VerifyRequest
	done        chan struct{}
	result      VerifyResult
	err         error
	completedAt time.Time
}

type setupRecord struct {
	idempotencyKey string
	done           chan struct{}
	err            error
}

type localOperationKind string

const (
	localOperationCreate localOperationKind = "create"
	localOperationSetup  localOperationKind = "setup"
	localOperationVerify localOperationKind = "verify"
)

type localOperationKey struct {
	kind        localOperationKind
	requestHash string
	allocation  AllocationRef
	completedAt time.Time
}

// LocalDockerRunner is a development-only adapter. It intentionally keeps
// provider IDs and all arbitrary execution mechanism inside this file.
type LocalDockerRunner struct {
	mgr      containerapi.Manager
	owned    containerapi.OwnedAllocationManager
	catalog  LocalDockerRuntimeCatalog
	verifier LocalDockerVerifier
	scope    string

	mu             sync.RWMutex
	allocations    map[string]*localAllocation
	active         map[string]AllocationRef
	creates        map[string]*createRecord
	pending        map[string]*createRecord
	generations    map[string]uint64
	absentThrough  map[string]uint64
	verifies       map[string]*verifyRecord
	operationKeys  map[string]*localOperationKey
	operationTTL   time.Duration
	operationLimit int
	now            func() time.Time
}

func NewLocalDockerRunner(mgr containerapi.Manager, catalog LocalDockerRuntimeCatalog, verifier LocalDockerVerifier, scope string) (*LocalDockerRunner, error) {
	if mgr == nil {
		return nil, errors.New("local Docker runner requires a container manager")
	}
	if catalog == nil {
		return nil, errors.New("local Docker runner requires a trusted runtime catalog")
	}
	if verifier == nil {
		return nil, errors.New("local Docker runner requires an explicit verifier capability")
	}
	owned, ok := mgr.(containerapi.OwnedAllocationManager)
	if !ok {
		return nil, errors.New("local Docker runner requires exact owned-allocation capability")
	}
	scope = strings.TrimSpace(scope)
	if scope == "" {
		return nil, errors.New("local Docker runner requires a non-empty controller scope")
	}
	return &LocalDockerRunner{
		mgr:            mgr,
		owned:          owned,
		catalog:        catalog,
		verifier:       verifier,
		scope:          scope,
		allocations:    make(map[string]*localAllocation),
		active:         make(map[string]AllocationRef),
		creates:        make(map[string]*createRecord),
		pending:        make(map[string]*createRecord),
		generations:    make(map[string]uint64),
		absentThrough:  make(map[string]uint64),
		verifies:       make(map[string]*verifyRecord),
		operationKeys:  make(map[string]*localOperationKey),
		operationTTL:   defaultOperationTTL,
		operationLimit: defaultOperationLimit,
		now:            time.Now,
	}, nil
}

func (r *LocalDockerRunner) Kind() ProviderKind { return ProviderLocalDocker }

func (r *LocalDockerRunner) CreateSession(ctx context.Context, req CreateSessionRequest) (AllocationRef, error) {
	hash, err := hashCreateRequest(req)
	if err != nil {
		return AllocationRef{}, err
	}
	// Replay is evaluated before time-sensitive validation. Once this key has
	// completed, the same immutable request returns the recorded outcome even
	// after ExpiresAt passes or the allocation is destroyed.
	if req.IdempotencyKey != "" {
		r.mu.Lock()
		r.pruneOperationsLocked(r.now())
		if err := r.checkOperationKeyLocked(req.IdempotencyKey, localOperationCreate, hash); err != nil {
			r.mu.Unlock()
			return AllocationRef{}, err
		}
		prior := r.creates[req.IdempotencyKey]
		if prior != nil && prior.requestHash != hash {
			r.mu.Unlock()
			return AllocationRef{}, ErrIdempotencyConflict
		}
		if prior != nil {
			done := prior.done
			r.mu.Unlock()
			select {
			case <-done:
				if prior.err != nil {
					return AllocationRef{}, prior.err
				}
				return prior.allocation, nil
			case <-ctx.Done():
				return AllocationRef{}, ctx.Err()
			}
		}
		r.mu.Unlock()
	}
	if err := validateCreateRequestAt(req, r.now()); err != nil {
		return AllocationRef{}, err
	}
	createCtx, cancelCreate := context.WithDeadline(ctx, req.ExpiresAt)
	defer cancelCreate()

	var record *createRecord
	var ref AllocationRef
	for {
		r.mu.Lock()
		r.pruneOperationsLocked(r.now())
		if err := r.checkOperationKeyLocked(req.IdempotencyKey, localOperationCreate, hash); err != nil {
			r.mu.Unlock()
			return AllocationRef{}, err
		}
		if prior, ok := r.creates[req.IdempotencyKey]; ok {
			if prior.requestHash != hash {
				r.mu.Unlock()
				return AllocationRef{}, ErrIdempotencyConflict
			}
			done := prior.done
			r.mu.Unlock()
			select {
			case <-done:
				if prior.err != nil {
					return AllocationRef{}, prior.err
				}
				return prior.allocation, nil
			case <-ctx.Done():
				return AllocationRef{}, ctx.Err()
			}
		}
		if inFlight := r.pending[req.Session.SessionID]; inFlight != nil {
			if inFlight.allocation.Session.Generation >= req.Session.Generation {
				r.mu.Unlock()
				return AllocationRef{}, ErrGenerationStale
			}
			done := inFlight.done
			r.mu.Unlock()
			select {
			case <-done:
				continue
			case <-ctx.Done():
				return AllocationRef{}, ctx.Err()
			}
		}
		if latest := r.generations[req.Session.SessionID]; latest >= req.Session.Generation {
			r.mu.Unlock()
			return AllocationRef{}, ErrGenerationStale
		}
		if absent := r.absentThrough[req.Session.SessionID]; absent >= req.Session.Generation {
			r.mu.Unlock()
			return AllocationRef{}, ErrGenerationStale
		}
		ref = AllocationRef{ID: req.AllocationID, Session: req.Session, Provider: ProviderLocalDocker}
		if err := r.reserveOperationKeyLocked(req.IdempotencyKey, localOperationCreate, hash, ref); err != nil {
			r.mu.Unlock()
			return AllocationRef{}, err
		}
		record = &createRecord{requestHash: hash, allocation: ref, done: make(chan struct{})}
		r.creates[req.IdempotencyKey] = record
		r.pending[req.Session.SessionID] = record
		r.generations[req.Session.SessionID] = req.Session.Generation
		r.mu.Unlock()
		break
	}

	finishCreate := func(createErr error) {
		r.mu.Lock()
		record.err = createErr
		record.completedAt = r.now()
		r.completeOperationKeyLocked(req.IdempotencyKey, record.completedAt)
		if r.pending[req.Session.SessionID] == record {
			delete(r.pending, req.Session.SessionID)
		}
		close(record.done)
		r.mu.Unlock()
	}

	runtime, err := r.catalog.ResolveRuntime(createCtx, req.Selection.Problem)
	if err != nil {
		if !req.ExpiresAt.After(r.now()) {
			err = ErrSessionExpired
		}
		err = fmt.Errorf("resolve approved runtime: %w", err)
		finishCreate(err)
		return AllocationRef{}, err
	}
	if runtime.Revision == "" || runtime.Revision != req.Selection.Problem.Revision {
		finishCreate(ErrInvalidRevision)
		return AllocationRef{}, ErrInvalidRevision
	}
	if runtime.Image == "" {
		err = errors.New("approved runtime has no image")
		finishCreate(err)
		return AllocationRef{}, err
	}
	if !validLocalImageContentID(runtime.Image) {
		err = fmt.Errorf("approved runtime image is not an immutable sha256 content ID: %q", runtime.Image)
		finishCreate(err)
		return AllocationRef{}, err
	}
	if !req.ExpiresAt.After(r.now()) {
		finishCreate(ErrSessionExpired)
		return AllocationRef{}, ErrSessionExpired
	}
	createOpts, err := localDockerCreateOpts(r.scope, ref, runtime, req.ResourceProfile)
	if err != nil {
		finishCreate(err)
		return AllocationRef{}, err
	}
	handle, err := r.owned.CreateOwnedAllocation(createCtx, createOpts)
	if err != nil {
		if !req.ExpiresAt.After(r.now()) {
			err = ErrSessionExpired
		}
		err = fmt.Errorf("create local Docker allocation: %w", err)
		finishCreate(err)
		return AllocationRef{}, err
	}

	r.mu.Lock()
	cleanupCause := error(nil)
	if record.destroyRequested {
		cleanupCause = ErrGenerationStale
	} else if !req.ExpiresAt.After(r.now()) {
		cleanupCause = ErrSessionExpired
	}
	if cleanupCause != nil {
		r.mu.Unlock()
		cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), lateCreateCleanupLimit)
		target := containerapi.OwnedAllocationTarget{Handle: handle, Create: createOpts}
		cleanupErr := r.removeLocalAllocation(cleanupCtx, target)
		cancelCleanup()
		if cleanupErr != nil {
			cleanupErr = fmt.Errorf("compensate late local Docker allocation: %w", cleanupErr)
		}
		createErr := errors.Join(cleanupCause, cleanupErr)
		r.mu.Lock()
		if cleanupErr != nil {
			// Retain the exact physical cleanup identity without making it
			// active. The durable destroy operation received this error and its
			// next retry must see and remove the late allocation rather than
			// falsely treating it as absent.
			r.allocations[ref.ID] = &localAllocation{
				ref: ref, request: req, runtime: runtime, target: target, destroying: true,
				leases: make(map[string]*managedTerminal),
			}
		}
		record.err = createErr
		record.destroyErr = cleanupErr
		record.completedAt = r.now()
		r.completeOperationKeyLocked(req.IdempotencyKey, record.completedAt)
		if r.pending[req.Session.SessionID] == record {
			delete(r.pending, req.Session.SessionID)
		}
		close(record.done)
		r.mu.Unlock()
		return AllocationRef{}, createErr
	}
	r.allocations[ref.ID] = &localAllocation{
		ref: ref, request: req, runtime: runtime,
		target: containerapi.OwnedAllocationTarget{Handle: handle, Create: createOpts},
		leases: make(map[string]*managedTerminal),
	}
	if r.generations[req.Session.SessionID] == req.Session.Generation {
		r.active[req.Session.SessionID] = ref
	}
	record.completedAt = r.now()
	r.completeOperationKeyLocked(req.IdempotencyKey, record.completedAt)
	if r.pending[req.Session.SessionID] == record {
		delete(r.pending, req.Session.SessionID)
	}
	close(record.done)
	r.mu.Unlock()
	return ref, nil
}

func validLocalImageContentID(value string) bool {
	const prefix = "sha256:"
	if !strings.HasPrefix(value, prefix) || len(value) != len(prefix)+sha256.Size*2 {
		return false
	}
	if value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value[len(prefix):])
	return err == nil
}

func (r *LocalDockerRunner) WaitReady(ctx context.Context, ref AllocationRef, timeout time.Duration) error {
	a, err := r.activeAllocation(ref)
	if err != nil {
		return err
	}
	return r.mgr.WaitReady(ctx, a.target.Handle.ContainerID, func() bool {
		result, err := r.mgr.Exec(ctx, a.target.Handle.ContainerID, []string{
			"kubectl", "get", "nodes", "-o",
			`jsonpath={.items[0].status.conditions[?(@.type=="Ready")].status}`,
		})
		return err == nil && result.ExitCode == 0 && strings.TrimSpace(result.Stdout) == "True"
	}, timeout)
}

func (r *LocalDockerRunner) SetupSession(ctx context.Context, request SetupSessionRequest) error {
	ref := request.Allocation
	if request.IdempotencyKey == "" {
		return errors.New("setup idempotency key is required")
	}
	if request.IdempotencyKey != SetupIdempotencyKey(ref) {
		return ErrIdempotencyConflict
	}
	requestHash, err := hashLocalOperationRequest(request)
	if err != nil {
		return err
	}

	r.mu.Lock()
	r.pruneOperationsLocked(r.now())
	a, ok := r.allocations[ref.ID]
	current := r.active[ref.Session.SessionID]
	if ref.Provider != ProviderLocalDocker || ref.ID == "" || ref.Session.SessionID == "" ||
		ref.Session.Generation == 0 || !ok || a.ref != ref {
		r.mu.Unlock()
		return ErrAllocationNotFound
	}
	if current != ref || a.destroying {
		r.mu.Unlock()
		return ErrGenerationStale
	}
	if prior := a.setup; prior != nil {
		if prior.idempotencyKey != request.IdempotencyKey {
			r.mu.Unlock()
			return ErrIdempotencyConflict
		}
		done := prior.done
		r.mu.Unlock()
		select {
		case <-done:
			return prior.err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if err := r.reserveOperationKeyLocked(request.IdempotencyKey, localOperationSetup, requestHash, ref); err != nil {
		r.mu.Unlock()
		return err
	}
	record := &setupRecord{idempotencyKey: request.IdempotencyKey, done: make(chan struct{})}
	a.setup = record
	r.mu.Unlock()

	var setupErr error
	if a.runtime.SetupScript != "" {
		result, err := r.mgr.Exec(ctx, a.target.Handle.ContainerID, []string{"/bin/sh", "-c", a.runtime.SetupScript})
		switch {
		case err != nil:
			setupErr = fmt.Errorf("run approved setup: %w", err)
		case result.ExitCode != 0:
			setupErr = fmt.Errorf("approved setup failed: %s", boundedOutput(result.Stdout+result.Stderr))
		}
	}
	r.mu.Lock()
	record.err = setupErr
	r.completeOperationKeyLocked(request.IdempotencyKey, r.now())
	close(record.done)
	r.mu.Unlock()
	return setupErr
}

func (r *LocalDockerRunner) OpenTerminal(ctx context.Context, req OpenTerminalRequest) (TerminalSession, error) {
	a, err := r.activeAllocation(req.Allocation)
	if err != nil {
		return nil, err
	}
	if req.UserID == "" || req.UserID != a.request.UserID || req.LeaseID == "" {
		return nil, errors.New("terminal authority does not match allocation")
	}
	if !a.request.ExpiresAt.After(time.Now()) {
		return nil, ErrSessionExpired
	}
	r.mu.Lock()
	if a.destroying || req.LeaseID == req.ReplacesLeaseID {
		r.mu.Unlock()
		return nil, ErrTerminalLease
	}
	if _, exists := a.leases[req.LeaseID]; exists {
		r.mu.Unlock()
		return nil, ErrTerminalLease
	}
	// A reconnect may prepare one candidate PTY while the exact previous lease
	// remains usable. The Control Plane commits ownership only after this open
	// succeeds; a failed candidate therefore cannot evict the healthy terminal.
	// Any other overlap is rejected so concurrent candidates cannot fan out
	// terminal authority.
	switch len(a.leases) {
	case 0:
		// The previous socket may have closed between the Control Plane snapshot
		// and this reservation. Opening the candidate is still safe.
	case 1:
		previous, exists := a.leases[req.ReplacesLeaseID]
		if req.ReplacesLeaseID == "" || !exists || previous == nil {
			r.mu.Unlock()
			return nil, ErrTerminalLease
		}
	default:
		r.mu.Unlock()
		return nil, ErrTerminalLease
	}
	// Reserve before the provider call so concurrent opens cannot race.
	a.leases[req.LeaseID] = nil
	r.mu.Unlock()
	// The shell is fixed by the trusted adapter; callers cannot select a
	// command through either the HTTP/WS or Runner contract.
	terminal, err := r.mgr.ExecInteractive(ctx, a.target.Handle.ContainerID, []string{"/bin/sh"})
	if err != nil {
		r.mu.Lock()
		if current, ok := r.allocations[req.Allocation.ID]; ok && current == a {
			delete(a.leases, req.LeaseID)
		}
		r.mu.Unlock()
		return nil, err
	}
	ttl := time.Until(a.request.ExpiresAt)
	if ttl <= 0 {
		_ = terminal.Close()
		r.mu.Lock()
		if current, ok := r.allocations[req.Allocation.ID]; ok && current == a {
			delete(a.leases, req.LeaseID)
		}
		r.mu.Unlock()
		return nil, ErrSessionExpired
	}

	r.mu.Lock()
	current, allocated := r.allocations[req.Allocation.ID]
	active := r.active[req.Allocation.Session.SessionID]
	reserved, leaseReserved := a.leases[req.LeaseID]
	if !allocated || current != a || a.destroying || active != req.Allocation || !leaseReserved || reserved != nil {
		if allocated && current == a {
			delete(a.leases, req.LeaseID)
		}
		r.mu.Unlock()
		_ = terminal.Close()
		return nil, ErrGenerationStale
	}
	managed := newManagedTerminal(ctx, terminal, ttl, func() {
		r.mu.Lock()
		if current, ok := r.allocations[req.Allocation.ID]; ok && current == a {
			delete(a.leases, req.LeaseID)
		}
		r.mu.Unlock()
	})
	a.leases[req.LeaseID] = managed
	r.mu.Unlock()
	return managed, nil
}

func (r *LocalDockerRunner) VerifySession(ctx context.Context, request VerifyRequest) (VerifyResult, error) {
	if err := ValidateVerifyRequest(request); err != nil {
		return VerifyResult{}, err
	}
	ref := request.Allocation
	idempotencyKey := request.IdempotencyKey
	requestHash, err := hashLocalOperationRequest(request)
	if err != nil {
		return VerifyResult{}, err
	}
	r.mu.Lock()
	r.pruneOperationsLocked(r.now())
	if err := r.checkOperationKeyLocked(idempotencyKey, localOperationVerify, requestHash); err != nil {
		r.mu.Unlock()
		return VerifyResult{}, err
	}
	if prior, ok := r.verifies[idempotencyKey]; ok {
		if !sameVerifyRequest(prior.request, request) {
			r.mu.Unlock()
			return VerifyResult{}, ErrIdempotencyConflict
		}
		done := prior.done
		r.mu.Unlock()
		select {
		case <-done:
			return prior.result, prior.err
		case <-ctx.Done():
			return VerifyResult{}, ctx.Err()
		}
	}
	r.mu.Unlock()

	a, err := r.activeAllocation(ref)
	if err != nil {
		return VerifyResult{}, err
	}
	if request.Problem != a.request.Selection.Problem {
		return VerifyResult{}, ErrInvalidRevision
	}

	r.mu.Lock()
	r.pruneOperationsLocked(r.now())
	if err := r.checkOperationKeyLocked(idempotencyKey, localOperationVerify, requestHash); err != nil {
		r.mu.Unlock()
		return VerifyResult{}, err
	}
	// Another caller may have installed the same operation while current
	// allocation authority was checked. Reuse it instead of executing twice.
	if prior, ok := r.verifies[idempotencyKey]; ok {
		if !sameVerifyRequest(prior.request, request) {
			r.mu.Unlock()
			return VerifyResult{}, ErrIdempotencyConflict
		}
		done := prior.done
		r.mu.Unlock()
		select {
		case <-done:
			return prior.result, prior.err
		case <-ctx.Done():
			return VerifyResult{}, ctx.Err()
		}
	}
	if err := r.reserveOperationKeyLocked(idempotencyKey, localOperationVerify, requestHash, ref); err != nil {
		r.mu.Unlock()
		return VerifyResult{}, err
	}
	record := &verifyRecord{request: request, done: make(chan struct{})}
	r.verifies[idempotencyKey] = record
	r.mu.Unlock()

	result, verifyErr := r.verifier.Verify(ctx, LocalDockerVerifyRequest{
		Request: request, ContainerID: a.target.Handle.ContainerID, Runtime: a.runtime,
	})
	if verifyErr == nil {
		fence, ok := ControllerFenceFromContext(ctx)
		if !ok {
			verifyErr = errors.New("local verifier requires controller fence")
		} else if err := ValidateVerifyAuthority(request, result, fence); err != nil {
			verifyErr = fmt.Errorf("validate local verify receipt: %w", err)
		}
	}
	r.mu.Lock()
	record.result = result
	record.err = verifyErr
	record.completedAt = r.now()
	r.completeOperationKeyLocked(idempotencyKey, record.completedAt)
	close(record.done)
	r.mu.Unlock()
	return result, verifyErr
}

func sameVerifyRequest(left, right VerifyRequest) bool {
	return left.Allocation == right.Allocation &&
		left.Problem == right.Problem &&
		left.IdempotencyKey == right.IdempotencyKey &&
		left.Deadline.Equal(right.Deadline)
}

func (r *LocalDockerRunner) GetSession(ctx context.Context, ref AllocationRef) (Observation, error) {
	a, err := r.allocation(ref)
	if errors.Is(err, ErrAllocationNotFound) {
		return Observation{Allocation: ref, State: ObservedAbsent}, nil
	}
	if err != nil {
		return Observation{}, err
	}
	running, err := r.mgr.IsRunning(ctx, a.target.Handle.ContainerID)
	if err != nil {
		return Observation{}, err
	}
	state := ObservedStopped
	if running {
		state = ObservedRunning
	}
	return Observation{Allocation: ref, State: state}, nil
}

func (r *LocalDockerRunner) DestroySession(ctx context.Context, ref AllocationRef) error {
	if ref.Provider != ProviderLocalDocker || ref.ID == "" || ref.Session.SessionID == "" ||
		ref.Session.Generation == 0 || ref.ID != AllocationIDForSession(ref.Session) {
		return ErrAllocationNotFound
	}
	r.mu.Lock()
	if r.absentThrough[ref.Session.SessionID] < ref.Session.Generation {
		r.absentThrough[ref.Session.SessionID] = ref.Session.Generation
	}
	a, ok := r.allocations[ref.ID]
	if !ok {
		if pending := r.pending[ref.Session.SessionID]; pending != nil && pending.allocation == ref {
			pending.destroyRequested = true
			done := pending.done
			r.mu.Unlock()
			select {
			case <-done:
				r.mu.RLock()
				destroyErr := pending.destroyErr
				r.mu.RUnlock()
				return destroyErr
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		r.mu.Unlock()
		return nil // absent is success; destroy is idempotent
	}
	if a.ref != ref {
		r.mu.Unlock()
		return ErrAllocationNotFound
	}
	a.destroying = true
	terminals := make([]*managedTerminal, 0, len(a.leases))
	for _, terminal := range a.leases {
		if terminal != nil {
			terminals = append(terminals, terminal)
		}
	}
	r.mu.Unlock()
	for _, terminal := range terminals {
		_ = terminal.Close()
	}
	destroyErr := r.removeLocalAllocation(ctx, a.target)
	if destroyErr != nil {
		return fmt.Errorf("destroy local Docker allocation: %w", destroyErr)
	}
	r.mu.Lock()
	delete(r.allocations, ref.ID)
	if current, ok := r.active[ref.Session.SessionID]; ok && current == ref {
		delete(r.active, ref.Session.SessionID)
	}
	r.mu.Unlock()
	return nil
}

func (r *LocalDockerRunner) removeLocalAllocation(ctx context.Context, target containerapi.OwnedAllocationTarget) error {
	_, err := r.owned.RemoveOwnedAllocation(ctx, target)
	return err
}

func (r *LocalDockerRunner) Reconcile(ctx context.Context, req ReconcileRequest) (ReconcileResult, error) {
	mode, err := effectiveReconcileMode(req)
	if err != nil {
		return ReconcileResult{}, err
	}
	result := ReconcileResult{Mode: mode}
	if len(req.Expectations) == 0 {
		result.Findings = append(result.Findings, ReconcileFinding{
			Kind: ReconcileFindingExpectationsRequired, Message: "exact reconciliation expectations are required",
		})
		result.Actions = append(result.Actions, ReconcileAction{Kind: ReconcileActionManualReview})
		return result, nil
	}

	// Validate the complete batch before touching provider inventory. A single
	// incomplete or duplicate expectation fences the preflight so a valid-looking
	// sibling cannot be deleted from an ambiguous request. Once exact apply starts,
	// Docker deletions are not atomic; the caller must require complete batch
	// absence proof before durable convergence.
	seen := make(map[ReconcileOwnership]struct{}, len(req.Expectations))
	invalid := false
	for _, expectation := range req.Expectations {
		validationErr := r.validateReconcileExpectation(expectation)
		if _, duplicate := seen[expectation.Ownership]; duplicate {
			validationErr = errors.New("duplicate reconciliation ownership")
		}
		seen[expectation.Ownership] = struct{}{}
		if validationErr == nil {
			continue
		}
		invalid = true
		result.Findings = append(result.Findings, ReconcileFinding{
			Kind: ReconcileFindingIncompleteExpectation, Ownership: expectation.Ownership,
			Desired: expectation.Desired, Message: validationErr.Error(),
		})
		result.Actions = append(result.Actions, ReconcileAction{
			Kind: ReconcileActionManualReview, Ownership: expectation.Ownership,
		})
	}
	if invalid {
		return result, nil
	}

	type inspectedExpectation struct {
		expectation ReconcileExpectation
		target      containerapi.OwnedAllocationTarget
		observation containerapi.OwnedAllocationObservation
	}
	inspected := make([]inspectedExpectation, 0, len(req.Expectations))
	// Resolve every approved runtime and build every trusted create spec before
	// provider inventory is inspected. One missing historical artifact or
	// provenance mismatch therefore blocks the complete batch with zero provider
	// effects instead of authorizing deletion from names or self-asserted labels.
	var preflightErrors []error
	for _, expectation := range req.Expectations {
		runtime, resolveErr := r.catalog.ResolveRuntime(ctx, expectation.Selection.Problem)
		if resolveErr == nil && runtime.Revision != expectation.Selection.Problem.Revision {
			resolveErr = ErrInvalidRevision
		}
		if resolveErr == nil && !validLocalImageContentID(runtime.Image) {
			resolveErr = fmt.Errorf("approved runtime image is not an immutable sha256 content ID: %q", runtime.Image)
		}
		ref := AllocationRef{
			ID: expectation.Ownership.AllocationID,
			Session: SessionRef{
				SessionID:  expectation.Ownership.SessionID,
				Generation: expectation.Ownership.Generation,
			},
			Provider: expectation.Ownership.Provider,
		}
		var opts containerapi.CreateOpts
		if resolveErr == nil {
			opts, resolveErr = localDockerCreateOpts(r.scope, ref, runtime, expectation.ResourceProfile)
		}
		if resolveErr != nil {
			preflightErrors = append(preflightErrors, fmt.Errorf("resolve trusted local Docker allocation %s: %w", expectation.Ownership.AllocationID, resolveErr))
			result.Findings = append(result.Findings, ReconcileFinding{
				Kind: ReconcileFindingIncompleteExpectation, Ownership: expectation.Ownership,
				Desired: expectation.Desired, Message: "trusted runtime provenance could not be reconstructed",
			})
			result.Actions = append(result.Actions, ReconcileAction{
				Kind: ReconcileActionManualReview, Ownership: expectation.Ownership,
			})
			continue
		}
		inspected = append(inspected, inspectedExpectation{
			expectation: expectation,
			target:      containerapi.OwnedAllocationTarget{Create: opts},
		})
	}
	if len(preflightErrors) != 0 {
		return result, errors.Join(preflightErrors...)
	}

	blocked := false
	var inspectErrors []error
	for index := range inspected {
		item := &inspected[index]
		expectation := item.expectation
		observation, inspectErr := r.owned.InspectOwnedAllocation(ctx, item.target)
		item.observation = observation
		if inspectErr != nil {
			blocked = true
			inspectErrors = append(inspectErrors, fmt.Errorf("inspect local Docker allocation %s: %w", expectation.Ownership.AllocationID, inspectErr))
			result.Findings = append(result.Findings, ReconcileFinding{
				Kind: ReconcileFindingInventoryUnavailable, Ownership: expectation.Ownership,
				Desired: expectation.Desired, Message: "exact provider inventory inspection failed",
			})
			result.Actions = append(result.Actions, ReconcileAction{
				Kind: ReconcileActionManualReview, Ownership: expectation.Ownership,
			})
			continue
		}
		if !observation.OwnershipComplete {
			blocked = true
			result.Findings = append(result.Findings, ReconcileFinding{
				Kind: ReconcileFindingAmbiguousOwnership, Ownership: expectation.Ownership,
				Desired: expectation.Desired, Message: "provider resources do not prove exact immutable ownership",
			})
			result.Actions = append(result.Actions, ReconcileAction{
				Kind: ReconcileActionManualReview, Ownership: expectation.Ownership,
			})
		}
	}
	if blocked {
		return result, errors.Join(inspectErrors...)
	}

	for _, item := range inspected {
		present := item.observation.ContainerPresent || item.observation.NetworkPresent
		switch item.expectation.Desired {
		case ReconcileDesiredActive:
			if !item.observation.ContainerPresent || !item.observation.NetworkPresent {
				result.Findings = append(result.Findings, ReconcileFinding{
					Kind: ReconcileFindingMissing, Ownership: item.expectation.Ownership,
					Desired: item.expectation.Desired, Message: "active allocation is missing a container or network",
				})
				result.Actions = append(result.Actions, ReconcileAction{
					Kind: ReconcileActionManualReview, Ownership: item.expectation.Ownership,
				})
			} else {
				result.Findings = append(result.Findings, ReconcileFinding{
					Kind: ReconcileFindingAdopted, Ownership: item.expectation.Ownership,
					Desired: item.expectation.Desired, Message: "exact owned allocation remains present",
				})
				result.Actions = append(result.Actions, ReconcileAction{
					Kind: ReconcileActionRetained, Ownership: item.expectation.Ownership,
				})
			}
		case ReconcileDesiredAbsent:
			if !present {
				result.Findings = append(result.Findings, ReconcileFinding{
					Kind: ReconcileFindingMissing, Ownership: item.expectation.Ownership,
					Desired: item.expectation.Desired, Message: "exact owned allocation is absent",
				})
				result.Actions = append(result.Actions, ReconcileAction{
					Kind: ReconcileActionObserved, Ownership: item.expectation.Ownership,
				})
				continue
			}
			result.Findings = append(result.Findings, ReconcileFinding{
				Kind: ReconcileFindingCleanupRequired, Ownership: item.expectation.Ownership,
				Desired: item.expectation.Desired, Message: "exact owned allocation remains present",
			})
			if mode == ReconcileModeReportOnly {
				result.Actions = append(result.Actions, ReconcileAction{
					Kind: ReconcileActionRemove, Ownership: item.expectation.Ownership,
				})
				continue
			}
			removed, removeErr := r.owned.RemoveOwnedAllocation(ctx, item.target)
			if removeErr != nil || !removed.Before.OwnershipComplete {
				if !removed.Before.OwnershipComplete {
					result.Findings = append(result.Findings, ReconcileFinding{
						Kind: ReconcileFindingAmbiguousOwnership, Ownership: item.expectation.Ownership,
						Desired: item.expectation.Desired, Message: "ownership became ambiguous before removal",
					})
					result.Actions = append(result.Actions, ReconcileAction{
						Kind: ReconcileActionManualReview, Ownership: item.expectation.Ownership,
					})
				}
				if removeErr == nil {
					removeErr = errors.New("exact ownership was not proven at removal")
				}
				return result, fmt.Errorf("remove local Docker allocation %s: %w", item.expectation.Ownership.AllocationID, removeErr)
			}
			if !removed.After.OwnershipComplete || removed.After.ContainerPresent || removed.After.NetworkPresent {
				result.Findings = append(result.Findings, ReconcileFinding{
					Kind: ReconcileFindingCleanupRequired, Ownership: item.expectation.Ownership,
					Desired: item.expectation.Desired, Message: "provider did not prove complete allocation absence after removal",
				})
				return result, fmt.Errorf("local Docker allocation %s remains present or ambiguous after cleanup", item.expectation.Ownership.AllocationID)
			}
			result.Actions = append(result.Actions, ReconcileAction{
				Kind: ReconcileActionDestroyed, Ownership: item.expectation.Ownership,
				Applied: removed.Before.ContainerPresent || removed.Before.NetworkPresent,
			})
		}
	}
	return result, nil
}

func effectiveReconcileMode(req ReconcileRequest) (ReconcileMode, error) {
	if req.Mode == "" {
		if req.Apply {
			return ReconcileModeApply, nil
		}
		return ReconcileModeReportOnly, nil
	}
	if req.Apply && req.Mode != ReconcileModeApply {
		return "", errors.New("legacy reconcile apply conflicts with explicit report-only mode")
	}
	switch req.Mode {
	case ReconcileModeReportOnly, ReconcileModeApply:
		return req.Mode, nil
	default:
		return "", fmt.Errorf("invalid reconcile mode %q", req.Mode)
	}
}

func (r *LocalDockerRunner) validateReconcileExpectation(expectation ReconcileExpectation) error {
	ownership := expectation.Ownership
	session := SessionRef{SessionID: ownership.SessionID, Generation: ownership.Generation}
	switch {
	case ownership.Provider != ProviderLocalDocker:
		return errors.New("ownership provider does not match local Docker")
	case ownership.Scope == "" || ownership.Scope != r.scope:
		return errors.New("ownership scope does not match the configured provider scope")
	case ownership.AllocationID == "" || ownership.SessionID == "" || ownership.Generation == 0:
		return errors.New("ownership allocation, session, and generation are required")
	case ownership.AllocationID != AllocationIDForSession(session):
		return errors.New("ownership allocation does not match session generation")
	case expectation.Selection.Generation == 0:
		return errors.New("catalog generation is required for reconciliation")
	case expectation.Selection.Problem.ID == "" || expectation.Selection.Problem.Revision == "":
		return errors.New("problem id and revision are required for reconciliation")
	case expectation.ResourceProfile != DefaultResourceProfile:
		return fmt.Errorf("resource profile %q is not approved for reconciliation", expectation.ResourceProfile)
	case expectation.Desired != ReconcileDesiredActive && expectation.Desired != ReconcileDesiredAbsent:
		return errors.New("reconciliation desired state is invalid")
	default:
		return nil
	}
}

func reconcileOwnershipLabels(ownership ReconcileOwnership) map[string]string {
	return map[string]string{
		"k8s-quiz":            "true",
		"k8s-quiz.scope":      ownership.Scope,
		"k8s-quiz.provider":   string(ownership.Provider),
		"k8s-quiz.allocation": ownership.AllocationID,
		"k8s-quiz.session":    ownership.SessionID,
		"k8s-quiz.generation": strconv.FormatUint(ownership.Generation, 10),
	}
}

func (r *LocalDockerRunner) activeAllocation(ref AllocationRef) (*localAllocation, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.allocations[ref.ID]
	if ref.Provider != ProviderLocalDocker || ref.ID == "" || ref.Session.SessionID == "" || ref.Session.Generation == 0 || !ok || a.ref != ref {
		return nil, ErrAllocationNotFound
	}
	current, ok := r.active[ref.Session.SessionID]
	if !ok || current != ref {
		return nil, ErrGenerationStale
	}
	if a.destroying {
		return nil, ErrGenerationStale
	}
	return a, nil
}

func (r *LocalDockerRunner) allocation(ref AllocationRef) (*localAllocation, error) {
	if ref.Provider != ProviderLocalDocker || ref.ID == "" || ref.Session.SessionID == "" || ref.Session.Generation == 0 {
		return nil, ErrAllocationNotFound
	}
	r.mu.RLock()
	a, ok := r.allocations[ref.ID]
	r.mu.RUnlock()
	if !ok || a.ref != ref {
		return nil, ErrAllocationNotFound
	}
	return a, nil
}

func validateCreateRequestAt(req CreateSessionRequest, now time.Time) error {
	switch {
	case req.AllocationID == "":
		return errors.New("allocation id is required")
	case req.AllocationID != AllocationIDForSession(req.Session):
		return errors.New("allocation id does not match session generation")
	case req.Session.SessionID == "":
		return errors.New("session id is required")
	case req.Session.Generation == 0:
		return errors.New("generation must be positive")
	case req.UserID == "":
		return errors.New("user id is required")
	case req.Selection.Generation == 0:
		return errors.New("create request catalog generation is required")
	case req.Selection.Problem.ID == "" || req.Selection.Problem.Revision == "":
		return errors.New("approved problem id and revision are required")
	case req.ResourceProfile != DefaultResourceProfile:
		return fmt.Errorf("resource profile %q is not approved", req.ResourceProfile)
	case req.ExpiresAt.IsZero() || !req.ExpiresAt.After(now):
		return errors.New("expiry must be in the future")
	case ValidateProviderOperationKey(req.IdempotencyKey) != nil:
		return errors.New("idempotency key is required")
	default:
		return nil
	}
}

func hashCreateRequest(req CreateSessionRequest) (string, error) {
	return hashLocalOperationRequest(req)
}

func hashLocalOperationRequest(request any) (string, error) {
	b, err := json.Marshal(request)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

func boundedOutput(s string) string {
	if len(s) <= maxRunnerOutputBytes {
		return s
	}
	return s[:maxRunnerOutputBytes]
}

func dockerResourceName(scope, allocationID string) string {
	safeScope := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			return r
		}
		return '-'
	}, scope)
	return "k8s-quiz-" + safeScope + "-" + allocationID
}

func localDockerCreateOpts(scope string, ref AllocationRef, runtime LocalDockerRuntime, resourceProfile string) (containerapi.CreateOpts, error) {
	if strings.TrimSpace(scope) == "" {
		return containerapi.CreateOpts{}, errors.New("local Docker scope is required")
	}
	if ref.Provider != ProviderLocalDocker || ref.ID == "" || ref.Session.SessionID == "" || ref.Session.Generation == 0 ||
		ref.ID != AllocationIDForSession(ref.Session) {
		return containerapi.CreateOpts{}, errors.New("local Docker allocation identity is invalid")
	}
	if resourceProfile != DefaultResourceProfile {
		return containerapi.CreateOpts{}, fmt.Errorf("resource profile %q is not approved", resourceProfile)
	}
	if !validLocalImageContentID(runtime.Image) {
		return containerapi.CreateOpts{}, fmt.Errorf("approved runtime image is not an immutable sha256 content ID: %q", runtime.Image)
	}
	name := dockerResourceName(scope, ref.ID)
	ownership := ReconcileOwnership{
		Provider: ref.Provider, Scope: scope, AllocationID: ref.ID,
		SessionID: ref.Session.SessionID, Generation: ref.Session.Generation,
	}
	return containerapi.CreateOpts{
		Name: name, Image: runtime.Image, Labels: reconcileOwnershipLabels(ownership),
		CPULimit: 1_000_000_000, MemoryLimit: 1_073_741_824,
		NetworkMode: name, Privileged: true,
	}, nil
}

func (r *LocalDockerRunner) pruneOperationsLocked(now time.Time) {
	if r.operationTTL > 0 {
		cutoff := now.Add(-r.operationTTL)
		for key, record := range r.operationKeys {
			if !record.completedAt.IsZero() && record.completedAt.Before(cutoff) && !r.hasCurrentAllocationLocked(record.allocation) {
				r.deleteOperationKeyLocked(key, record)
			}
		}
	}
	for r.operationLimit > 0 && len(r.operationKeys) >= r.operationLimit {
		var oldestKey string
		var oldest time.Time
		for key, record := range r.operationKeys {
			if record.completedAt.IsZero() || r.hasCurrentAllocationLocked(record.allocation) {
				continue
			}
			if oldestKey == "" || record.completedAt.Before(oldest) {
				oldestKey, oldest = key, record.completedAt
			}
		}
		if oldestKey == "" {
			break
		}
		r.deleteOperationKeyLocked(oldestKey, r.operationKeys[oldestKey])
	}
}

func (r *LocalDockerRunner) checkOperationKeyLocked(key string, kind localOperationKind, requestHash string) error {
	record := r.operationKeys[key]
	if record != nil && (record.kind != kind || record.requestHash != requestHash) {
		return ErrIdempotencyConflict
	}
	return nil
}

func (r *LocalDockerRunner) reserveOperationKeyLocked(key string, kind localOperationKind, requestHash string, allocation AllocationRef) error {
	if key == "" || requestHash == "" {
		return ErrIdempotencyConflict
	}
	if err := r.checkOperationKeyLocked(key, kind, requestHash); err != nil {
		return err
	}
	if r.operationKeys[key] == nil {
		r.operationKeys[key] = &localOperationKey{kind: kind, requestHash: requestHash, allocation: allocation}
	}
	return nil
}

func (r *LocalDockerRunner) completeOperationKeyLocked(key string, completedAt time.Time) {
	if record := r.operationKeys[key]; record != nil {
		record.completedAt = completedAt
	}
}

func (r *LocalDockerRunner) deleteOperationKeyLocked(key string, record *localOperationKey) {
	delete(r.operationKeys, key)
	switch record.kind {
	case localOperationCreate:
		delete(r.creates, key)
	case localOperationVerify:
		delete(r.verifies, key)
	case localOperationSetup:
		if allocation := r.allocations[record.allocation.ID]; allocation != nil && allocation.ref == record.allocation &&
			allocation.setup != nil && allocation.setup.idempotencyKey == key {
			allocation.setup = nil
		}
	}
}

// hasCurrentAllocationLocked identifies the exact active allocation rather
// than merely matching a session. A reset can reuse the session ID with a new
// generation, whose records must not pin the old generation indefinitely.
func (r *LocalDockerRunner) hasCurrentAllocationLocked(ref AllocationRef) bool {
	allocation, ok := r.allocations[ref.ID]
	if !ok || allocation.ref != ref {
		return false
	}
	current, ok := r.active[ref.Session.SessionID]
	return ok && current == ref
}

type managedTerminal struct {
	TerminalSession
	once    sync.Once
	timer   *time.Timer
	done    chan struct{}
	onClose func()
}

func newManagedTerminal(ctx context.Context, terminal TerminalSession, ttl time.Duration, onClose func()) *managedTerminal {
	m := &managedTerminal{TerminalSession: terminal, done: make(chan struct{}), onClose: onClose}
	if ttl <= 0 {
		_ = m.Close()
		return m
	}
	m.timer = time.NewTimer(ttl)
	go func() {
		select {
		case <-m.timer.C:
			_ = m.Close()
		case <-ctx.Done():
			_ = m.Close()
		case <-m.done:
		}
	}()
	return m
}

func (m *managedTerminal) Close() error {
	var err error
	m.once.Do(func() {
		if m.timer != nil {
			m.timer.Stop()
		}
		close(m.done)
		err = m.TerminalSession.Close()
		if m.onClose != nil {
			m.onClose()
		}
	})
	return err
}
