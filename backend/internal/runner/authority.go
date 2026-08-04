package runner

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

var ErrControllerAuthorityUnavailable = errors.New("runner controller authority is unavailable")

type AuthorityState string

const (
	AuthorityRecovering AuthorityState = "recovering"
	AuthorityReady      AuthorityState = "ready"
	AuthorityDraining   AuthorityState = "draining"
	AuthorityFenced     AuthorityState = "fenced"
)

// AuthorityGate is the process-local mirror of the provider-scoped controller
// lease. State transitions are monotonic: recovering -> ready -> draining, or
// any non-final state -> fenced. Its context is cancelled before either
// draining or fenced becomes observable to callers.
type AuthorityGate struct {
	mu              sync.RWMutex
	state           AuthorityState
	cause           error
	workCtx         context.Context
	cancelWork      context.CancelFunc
	authorityCtx    context.Context
	cancelAuthority context.CancelFunc
}

func NewAuthorityGate() *AuthorityGate {
	workCtx, cancelWork := context.WithCancel(context.Background())
	authorityCtx, cancelAuthority := context.WithCancel(context.Background())
	return &AuthorityGate{
		state:   AuthorityRecovering,
		workCtx: workCtx, cancelWork: cancelWork,
		authorityCtx: authorityCtx, cancelAuthority: cancelAuthority,
	}
}

func (g *AuthorityGate) Activate() error {
	if g == nil {
		return errors.New("runner authority gate is required")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	switch g.state {
	case AuthorityRecovering:
		g.state = AuthorityReady
		return nil
	case AuthorityReady:
		return nil
	default:
		return ErrControllerAuthorityUnavailable
	}
}

// BeginDrain closes public/provider admission while retaining only the exact
// destroy authority needed for bounded graceful cleanup before lease release.
func (g *AuthorityGate) BeginDrain() {
	if g == nil {
		return
	}
	g.mu.Lock()
	if g.state == AuthorityRecovering || g.state == AuthorityReady {
		g.state = AuthorityDraining
		g.cancelWork()
	}
	g.mu.Unlock()
}

// Fence records lease loss synchronously. No provider operation, including
// destroy, is authorized after this transition; the next controller must
// reconcile durable cleanup intent under a newly acquired lease.
func (g *AuthorityGate) Fence(cause error) {
	if g == nil {
		return
	}
	g.mu.Lock()
	if g.state != AuthorityFenced {
		g.state = AuthorityFenced
		g.cause = cause
		g.cancelWork()
		g.cancelAuthority()
	}
	g.mu.Unlock()
}

func (g *AuthorityGate) State() AuthorityState {
	if g == nil {
		return AuthorityFenced
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.state
}

func (g *AuthorityGate) Cause() error {
	if g == nil {
		return ErrControllerAuthorityUnavailable
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.cause
}

func (g *AuthorityGate) Ready() bool {
	return g != nil && g.State() == AuthorityReady
}

func (g *AuthorityGate) Context() context.Context {
	if g == nil {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		return ctx
	}
	return g.workCtx
}

func (g *AuthorityGate) authorityContext() context.Context {
	if g == nil {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		return ctx
	}
	return g.authorityCtx
}

func (g *AuthorityGate) checkRecovery() error {
	if g == nil {
		return ErrControllerAuthorityUnavailable
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	if g.state != AuthorityRecovering {
		return ErrControllerAuthorityUnavailable
	}
	return nil
}

// RecoveryContext binds startup snapshot/reconcile/convergence to the same
// lease-lifetime cancellation boundary as runtime provider work. Recovery is
// authorized only before Activate and never while draining or fenced.
func (g *AuthorityGate) RecoveryContext(parent context.Context) (context.Context, context.CancelFunc, error) {
	if err := g.checkRecovery(); err != nil {
		return nil, nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	stop := context.AfterFunc(g.authorityContext(), cancel)
	if err := g.checkRecovery(); err != nil {
		stop()
		cancel()
		return nil, nil, err
	}
	return ctx, func() {
		stop()
		cancel()
	}, nil
}

func (g *AuthorityGate) check(allowDrain bool) error {
	if g == nil {
		return ErrControllerAuthorityUnavailable
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	if g.state == AuthorityReady || (allowDrain && g.state == AuthorityDraining) {
		return nil
	}
	return ErrControllerAuthorityUnavailable
}

// AuthorityRunner applies the same lease authority immediately before every
// provider boundary call. The unwrapped Runner is used only for startup
// recovery while no public/session service admission exists.
type AuthorityRunner struct {
	inner               Runner
	gate                *AuthorityGate
	fence               ControllerFence
	attestationVerifier VerifyAttestationVerifier
}

// VerifyAttestationVerifier authenticates a trusted verifier receipt against
// provider-specific trust configuration. Structural receipt validation is not
// authentication: public-capable providers must supply this capability before
// AuthorityRunner can be constructed.
type VerifyAttestationVerifier interface {
	VerifyAttestation(context.Context, VerifyRequest, VerificationReceipt) error
}

func NewAuthorityRunner(inner Runner, gate *AuthorityGate, fence ControllerFence) (*AuthorityRunner, error) {
	return newAuthorityRunner(inner, gate, fence, nil)
}

// NewTrustedAuthorityRunner is the only construction path for non-local
// providers. The verifier must cryptographically authenticate the attestation
// over the canonical receipt subject using trust material unavailable to the
// learner and provider workload.
func NewTrustedAuthorityRunner(inner Runner, gate *AuthorityGate, fence ControllerFence, verifier VerifyAttestationVerifier) (*AuthorityRunner, error) {
	if verifier == nil {
		return nil, errors.New("trusted authority runner requires an attestation verifier")
	}
	return newAuthorityRunner(inner, gate, fence, verifier)
}

func newAuthorityRunner(inner Runner, gate *AuthorityGate, fence ControllerFence, verifier VerifyAttestationVerifier) (*AuthorityRunner, error) {
	if inner == nil || gate == nil {
		return nil, errors.New("authority runner requires runner and gate")
	}
	if fence.ProviderID == "" || fence.Epoch == 0 || fence.Epoch > MaxDurableValue {
		return nil, errors.New("authority runner requires a valid controller fence")
	}
	if inner.Kind() == ProviderLocalDocker {
		if verifier != nil {
			return nil, errors.New("local Docker authority runner cannot accept a trusted attestation verifier")
		}
	} else if verifier == nil {
		return nil, errors.New("non-local authority runner requires cryptographic attestation verification")
	}
	return &AuthorityRunner{inner: inner, gate: gate, fence: fence, attestationVerifier: verifier}, nil
}

func (r *AuthorityRunner) Kind() ProviderKind { return r.inner.Kind() }

func (r *AuthorityRunner) activeContext(parent context.Context) (context.Context, context.CancelFunc, error) {
	if err := r.gate.check(false); err != nil {
		return nil, nil, err
	}
	ctx, cancel := context.WithCancel(withControllerFence(parent, r.fence))
	stop := context.AfterFunc(r.gate.Context(), cancel)
	if err := r.gate.check(false); err != nil {
		stop()
		cancel()
		return nil, nil, err
	}
	return ctx, func() {
		stop()
		cancel()
	}, nil
}

func (r *AuthorityRunner) destroyContext(parent context.Context) (context.Context, context.CancelFunc, error) {
	if err := r.gate.check(true); err != nil {
		return nil, nil, err
	}
	ctx, cancel := context.WithCancel(withControllerFence(parent, r.fence))
	stop := context.AfterFunc(r.gate.authorityContext(), cancel)
	if err := r.gate.check(true); err != nil {
		stop()
		cancel()
		return nil, nil, err
	}
	return ctx, func() {
		stop()
		cancel()
	}, nil
}

func (r *AuthorityRunner) recoveryContext(parent context.Context) (context.Context, context.CancelFunc, error) {
	ctx, cancel, err := r.gate.RecoveryContext(withControllerFence(parent, r.fence))
	if err != nil {
		return nil, nil, err
	}
	return ctx, cancel, nil
}

func (r *AuthorityRunner) postActive(callCtx context.Context) error {
	if err := r.gate.check(false); err != nil {
		return err
	}
	return callCtx.Err()
}

func (r *AuthorityRunner) postDestroy(callCtx context.Context) error {
	if err := r.gate.check(true); err != nil {
		return err
	}
	return callCtx.Err()
}

func (r *AuthorityRunner) CreateSession(ctx context.Context, request CreateSessionRequest) (AllocationRef, error) {
	callCtx, cancel, err := r.activeContext(ctx)
	if err != nil {
		return AllocationRef{}, err
	}
	defer cancel()
	ref, callErr := r.inner.CreateSession(callCtx, request)
	if err := r.postActive(callCtx); err != nil {
		return AllocationRef{}, err
	}
	return ref, callErr
}

func (r *AuthorityRunner) WaitReady(ctx context.Context, ref AllocationRef, timeout time.Duration) error {
	callCtx, cancel, err := r.activeContext(ctx)
	if err != nil {
		return err
	}
	defer cancel()
	callErr := r.inner.WaitReady(callCtx, ref, timeout)
	if err := r.postActive(callCtx); err != nil {
		return err
	}
	return callErr
}

func (r *AuthorityRunner) SetupSession(ctx context.Context, request SetupSessionRequest) error {
	callCtx, cancel, err := r.activeContext(ctx)
	if err != nil {
		return err
	}
	defer cancel()
	callErr := r.inner.SetupSession(callCtx, request)
	if err := r.postActive(callCtx); err != nil {
		return err
	}
	return callErr
}

func (r *AuthorityRunner) OpenTerminal(ctx context.Context, request OpenTerminalRequest) (TerminalSession, error) {
	callCtx, cancel, err := r.activeContext(ctx)
	if err != nil {
		return nil, err
	}
	terminal, err := r.inner.OpenTerminal(callCtx, request)
	if err != nil {
		cancel()
		return nil, err
	}
	if r.postActive(callCtx) != nil {
		_ = terminal.Close()
		cancel()
		return nil, ErrControllerAuthorityUnavailable
	}
	wrapped := &authorityTerminal{TerminalSession: terminal, cancel: cancel, done: make(chan struct{})}
	go func() {
		select {
		case <-callCtx.Done():
			_ = wrapped.Close()
		case <-wrapped.done:
		}
	}()
	return wrapped, nil
}

func (r *AuthorityRunner) VerifySession(ctx context.Context, request VerifyRequest) (VerifyResult, error) {
	if err := ValidateVerifyRequest(request); err != nil {
		return VerifyResult{}, err
	}
	if request.Allocation.Provider != r.inner.Kind() {
		return VerifyResult{}, ErrAllocationNotFound
	}
	callCtx, cancel, err := r.activeContext(ctx)
	if err != nil {
		return VerifyResult{}, err
	}
	defer cancel()
	result, callErr := r.inner.VerifySession(callCtx, request)
	if err := r.postActive(callCtx); err != nil {
		return VerifyResult{}, err
	}
	if callErr == nil {
		if err := ValidateVerifyAuthority(request, result, r.fence); err != nil {
			return VerifyResult{}, fmt.Errorf("validate verify receipt: %w", err)
		}
		if r.inner.Kind() != ProviderLocalDocker {
			if r.attestationVerifier == nil {
				return VerifyResult{}, errors.New("trusted verify attestation verifier is unavailable")
			}
			if err := r.attestationVerifier.VerifyAttestation(callCtx, request, result.Receipt); err != nil {
				return VerifyResult{}, fmt.Errorf("authenticate verify receipt: %w", err)
			}
			if err := r.postActive(callCtx); err != nil {
				return VerifyResult{}, err
			}
		}
	}
	return result, callErr
}

func (r *AuthorityRunner) GetSession(ctx context.Context, ref AllocationRef) (Observation, error) {
	callCtx, cancel, err := r.activeContext(ctx)
	if err != nil {
		return Observation{}, err
	}
	defer cancel()
	observation, callErr := r.inner.GetSession(callCtx, ref)
	if err := r.postActive(callCtx); err != nil {
		return Observation{}, err
	}
	return observation, callErr
}

func (r *AuthorityRunner) DestroySession(ctx context.Context, ref AllocationRef) error {
	callCtx, cancel, err := r.destroyContext(ctx)
	if err != nil {
		return err
	}
	defer cancel()
	// Graceful draining intentionally cancelled the active-work context. Exact
	// destroy uses the caller's bounded cleanup context while the lease remains
	// held. A fenced gate fails the state check above.
	callErr := r.inner.DestroySession(callCtx, ref)
	if err := r.postDestroy(callCtx); err != nil {
		return err
	}
	return callErr
}

type authorityTerminal struct {
	TerminalSession
	cancel    context.CancelFunc
	closeOnce sync.Once
	done      chan struct{}
	err       error
}

func (t *authorityTerminal) Close() error {
	t.closeOnce.Do(func() {
		t.cancel()
		t.err = t.TerminalSession.Close()
		close(t.done)
	})
	return t.err
}

func (r *AuthorityRunner) Reconcile(ctx context.Context, request ReconcileRequest) (ReconcileResult, error) {
	callCtx, cancel, err := r.activeContext(ctx)
	if err != nil {
		return ReconcileResult{}, err
	}
	defer cancel()
	result, callErr := r.inner.Reconcile(callCtx, request)
	if err := r.postActive(callCtx); err != nil {
		return ReconcileResult{}, err
	}
	return result, callErr
}

// ReconcileRecovery is the only provider mutation allowed while the gate is in
// recovering. It performs a post-call authority check so a provider that
// ignores cancellation cannot return a stale success for durable convergence.
func (r *AuthorityRunner) ReconcileRecovery(ctx context.Context, request ReconcileRequest) (ReconcileResult, error) {
	callCtx, cancel, err := r.recoveryContext(ctx)
	if err != nil {
		return ReconcileResult{}, err
	}
	defer cancel()
	result, callErr := r.inner.Reconcile(callCtx, request)
	if err := r.gate.checkRecovery(); err != nil {
		return ReconcileResult{}, err
	}
	if err := callCtx.Err(); err != nil {
		return ReconcileResult{}, err
	}
	return result, callErr
}

func (r *AuthorityRunner) CreateSessionRecovery(ctx context.Context, request CreateSessionRequest) (AllocationRef, error) {
	callCtx, cancel, err := r.recoveryContext(ctx)
	if err != nil {
		return AllocationRef{}, err
	}
	defer cancel()
	ref, callErr := r.inner.CreateSession(callCtx, request)
	if err := r.postRecovery(callCtx); err != nil {
		return AllocationRef{}, err
	}
	return ref, callErr
}

func (r *AuthorityRunner) WaitReadyRecovery(ctx context.Context, ref AllocationRef, timeout time.Duration) error {
	callCtx, cancel, err := r.recoveryContext(ctx)
	if err != nil {
		return err
	}
	defer cancel()
	callErr := r.inner.WaitReady(callCtx, ref, timeout)
	if err := r.postRecovery(callCtx); err != nil {
		return err
	}
	return callErr
}

func (r *AuthorityRunner) SetupSessionRecovery(ctx context.Context, request SetupSessionRequest) error {
	callCtx, cancel, err := r.recoveryContext(ctx)
	if err != nil {
		return err
	}
	defer cancel()
	callErr := r.inner.SetupSession(callCtx, request)
	if err := r.postRecovery(callCtx); err != nil {
		return err
	}
	return callErr
}

func (r *AuthorityRunner) GetSessionRecovery(ctx context.Context, ref AllocationRef) (Observation, error) {
	callCtx, cancel, err := r.recoveryContext(ctx)
	if err != nil {
		return Observation{}, err
	}
	defer cancel()
	observation, callErr := r.inner.GetSession(callCtx, ref)
	if err := r.postRecovery(callCtx); err != nil {
		return Observation{}, err
	}
	return observation, callErr
}

func (r *AuthorityRunner) DestroySessionRecovery(ctx context.Context, ref AllocationRef) error {
	callCtx, cancel, err := r.recoveryContext(ctx)
	if err != nil {
		return err
	}
	defer cancel()
	callErr := r.inner.DestroySession(callCtx, ref)
	if err := r.postRecovery(callCtx); err != nil {
		return err
	}
	return callErr
}

func (r *AuthorityRunner) postRecovery(callCtx context.Context) error {
	if err := r.gate.checkRecovery(); err != nil {
		return err
	}
	return callCtx.Err()
}
