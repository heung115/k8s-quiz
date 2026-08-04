package runner

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	protocol "github.com/heung115/k8s-quiz/runnerprotocol/v1"
)

const maxPrivateVMOperationTimeout = 2 * time.Minute

var (
	privateVMUUIDPattern     = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	privateVMProblemPattern  = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	privateVMRevisionPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
	privateVMProfilePattern  = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?$`)
)

// PrivateVMLifecycleClient is the VM-only private Runner transport. It is
// deliberately smaller than Runner: VM running is not guest readiness and VM
// absence is not proof that disks, NICs, firewall state, credentials, agent
// leases, and terminal leases are absent.
type PrivateVMLifecycleClient interface {
	ActivateController(context.Context, protocol.ActivateRequest) error
	Create(context.Context, protocol.CreateRequest) (protocol.AllocationView, error)
	Get(context.Context, protocol.GetRequest) (protocol.AllocationView, error)
	Destroy(context.Context, protocol.DestroyRequest) (protocol.AllocationView, error)
}

type PrivateVMDesiredState string

const (
	PrivateVMDesiredActive PrivateVMDesiredState = "active"
	PrivateVMDesiredAbsent PrivateVMDesiredState = "absent"
)

type PrivateVMPhase string

const (
	PrivateVMProvisioning    PrivateVMPhase = "vm_provisioning"
	PrivateVMConfiguring     PrivateVMPhase = "vm_configuring"
	PrivateVMStarting        PrivateVMPhase = "vm_starting"
	PrivateVMRunning         PrivateVMPhase = "vm_running"
	PrivateVMDestroying      PrivateVMPhase = "vm_destroying"
	PrivateVMAbsent          PrivateVMPhase = "vm_absent"
	PrivateVMCleanupRequired PrivateVMPhase = "vm_cleanup_required"
	PrivateVMProviderLost    PrivateVMPhase = "vm_provider_lost"
)

type PrivateVMObservedState string

const (
	PrivateVMObservedUnknown  PrivateVMObservedState = "vm_unknown"
	PrivateVMObservedStopped  PrivateVMObservedState = "vm_stopped"
	PrivateVMObservedRunning  PrivateVMObservedState = "vm_running"
	PrivateVMObservedDeleting PrivateVMObservedState = "vm_deleting"
	PrivateVMObservedAbsent   PrivateVMObservedState = "vm_absent"
	PrivateVMObservedError    PrivateVMObservedState = "vm_error"
)

// PrivateVMState is an explicitly incomplete provider observation. In
// particular, Phase==PrivateVMAbsent must never be passed to MarkDestroyed or
// exposed as full session cleanup evidence.
type PrivateVMState struct {
	Allocation        AllocationRef
	CatalogGeneration uint64
	Desired           PrivateVMDesiredState
	Phase             PrivateVMPhase
	Observed          PrivateVMObservedState
	ExpiresAt         time.Time
	ErrorCode         protocol.ErrorCode
}

// PrivateVMLifecycle adapts the shared mTLS protocol to Control Plane domain
// identities without pretending that the VM-only API implements Runner.
type PrivateVMLifecycle struct {
	client           PrivateVMLifecycleClient
	providerID       string
	operationTimeout time.Duration
	now              func() time.Time
}

func NewPrivateVMLifecycle(client PrivateVMLifecycleClient, providerID string, operationTimeout time.Duration) (*PrivateVMLifecycle, error) {
	if client == nil || !protocol.ValidProviderID(providerID) {
		return nil, errors.New("private VM lifecycle requires a client and canonical provider id")
	}
	if operationTimeout <= 0 || operationTimeout > maxPrivateVMOperationTimeout {
		return nil, errors.New("private VM lifecycle operation timeout must be between zero and two minutes")
	}
	return &PrivateVMLifecycle{
		client: client, providerID: providerID, operationTimeout: operationTimeout, now: time.Now,
	}, nil
}

func (l *PrivateVMLifecycle) ActivateController(ctx context.Context, fence ControllerFence) error {
	deadline, err := l.effectDeadline(ctx, fence)
	if err != nil {
		return err
	}
	err = l.client.ActivateController(ctx, protocol.ActivateRequest{
		Schema: protocol.Schema, ControllerEpoch: fence.Epoch, EffectDeadline: deadline,
	})
	return mapPrivateVMError(err)
}

func (l *PrivateVMLifecycle) Create(ctx context.Context, fence ControllerFence, request CreateSessionRequest) (PrivateVMState, error) {
	deadline, err := l.effectDeadline(ctx, fence)
	if err != nil {
		return PrivateVMState{}, err
	}
	expiresAt, err := validatePrivateVMCreate(request)
	if err != nil {
		return PrivateVMState{}, err
	}
	view, callErr := l.client.Create(ctx, protocol.CreateRequest{
		Schema: protocol.Schema, ControllerEpoch: fence.Epoch, EffectDeadline: deadline,
		AllocationID: request.AllocationID, SessionID: request.Session.SessionID,
		Generation: request.Session.Generation, UserID: request.UserID,
		CatalogGeneration: request.Selection.Generation, ProblemID: request.Selection.Problem.ID,
		ProblemRevision: request.Selection.Problem.Revision, ResourceProfile: request.ResourceProfile,
		ExpiresAt: expiresAt, IdempotencyKey: request.IdempotencyKey,
	})
	if callErr != nil {
		return PrivateVMState{}, mapPrivateVMError(callErr)
	}
	state, err := projectPrivateVMState(view, AllocationRef{
		ID: request.AllocationID, Session: request.Session, Provider: ProviderHomeProxmox,
	})
	if err != nil || state.CatalogGeneration != request.Selection.Generation || !state.ExpiresAt.Equal(request.ExpiresAt) {
		return PrivateVMState{}, privateVMMutationResponseError()
	}
	return state, nil
}

func (l *PrivateVMLifecycle) Get(ctx context.Context, fence ControllerFence, ref AllocationRef) (PrivateVMState, error) {
	deadline, err := l.effectDeadline(ctx, fence)
	if err != nil {
		return PrivateVMState{}, err
	}
	if err := validatePrivateVMRef(ref); err != nil {
		return PrivateVMState{}, err
	}
	view, callErr := l.client.Get(ctx, protocol.GetRequest{
		Schema: protocol.Schema, ControllerEpoch: fence.Epoch, EffectDeadline: deadline,
		AllocationID: ref.ID, SessionID: ref.Session.SessionID, Generation: ref.Session.Generation,
	})
	if callErr != nil {
		return PrivateVMState{}, mapPrivateVMError(callErr)
	}
	state, err := projectPrivateVMState(view, ref)
	if err != nil {
		return PrivateVMState{}, ErrProviderProtocol
	}
	return state, nil
}

func (l *PrivateVMLifecycle) Destroy(ctx context.Context, fence ControllerFence, ref AllocationRef) (PrivateVMState, error) {
	deadline, err := l.effectDeadline(ctx, fence)
	if err != nil {
		return PrivateVMState{}, err
	}
	if err := validatePrivateVMRef(ref); err != nil {
		return PrivateVMState{}, err
	}
	view, callErr := l.client.Destroy(ctx, protocol.DestroyRequest{
		Schema: protocol.Schema, ControllerEpoch: fence.Epoch, EffectDeadline: deadline,
		AllocationID: ref.ID, SessionID: ref.Session.SessionID, Generation: ref.Session.Generation,
	})
	if callErr != nil {
		return PrivateVMState{}, mapPrivateVMError(callErr)
	}
	state, err := projectPrivateVMState(view, ref)
	if err != nil {
		return PrivateVMState{}, privateVMMutationResponseError()
	}
	return state, nil
}

func (l *PrivateVMLifecycle) effectDeadline(ctx context.Context, fence ControllerFence) (string, error) {
	if l == nil || l.client == nil || l.now == nil || fence.ProviderID != l.providerID ||
		fence.Epoch == 0 || fence.Epoch > MaxDurableValue {
		return "", ErrControllerFenced
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	now := l.now().UTC().Truncate(time.Microsecond)
	deadline := now.Add(l.operationTimeout)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline.UTC().Truncate(time.Microsecond)
	}
	if !deadline.After(now) {
		return "", context.DeadlineExceeded
	}
	encoded, err := protocol.FormatCanonicalDatabaseUTC(deadline)
	if err != nil {
		return "", ErrProviderRequest
	}
	return encoded, nil
}

func validatePrivateVMCreate(request CreateSessionRequest) (string, error) {
	if err := validatePrivateVMRef(AllocationRef{
		ID: request.AllocationID, Session: request.Session, Provider: ProviderHomeProxmox,
	}); err != nil {
		return "", err
	}
	if !privateVMUUIDPattern.MatchString(request.UserID) || request.Selection.Generation == 0 ||
		request.Selection.Generation > MaxDurableValue || !privateVMProblemPattern.MatchString(request.Selection.Problem.ID) ||
		!privateVMRevisionPattern.MatchString(request.Selection.Problem.Revision) ||
		!privateVMProfilePattern.MatchString(request.ResourceProfile) ||
		ValidateProviderOperationKey(request.IdempotencyKey) != nil || strings.HasPrefix(request.IdempotencyKey, "destroy:") {
		return "", ErrProviderRequest
	}
	// Do not reject a past expiry here. The durable private provider must see
	// an exact replay after response loss before it can distinguish it from new
	// expired work. It rejects new expired requests before any provider effect.
	expiresAt, err := protocol.FormatCanonicalDatabaseUTC(request.ExpiresAt.UTC())
	if err != nil {
		return "", ErrProviderRequest
	}
	return expiresAt, nil
}

func validatePrivateVMRef(ref AllocationRef) error {
	if ref.Provider != ProviderHomeProxmox || !privateVMUUIDPattern.MatchString(ref.ID) ||
		!privateVMUUIDPattern.MatchString(ref.Session.SessionID) || ref.Session.Generation == 0 ||
		ref.Session.Generation > MaxDurableValue || ref.ID != AllocationIDForSession(ref.Session) {
		return ErrProviderRequest
	}
	return nil
}

func projectPrivateVMState(view protocol.AllocationView, expected AllocationRef) (PrivateVMState, error) {
	if view.AllocationID != expected.ID || view.SessionID != expected.Session.SessionID ||
		view.Generation != expected.Session.Generation || view.CatalogGeneration == 0 ||
		view.CatalogGeneration > MaxDurableValue || view.LifecycleScope != protocol.LifecycleScopeVMOnly {
		return PrivateVMState{}, ErrProviderProtocol
	}
	expiresAt, err := protocol.ParseCanonicalDatabaseUTC(view.ExpiresAt)
	if err != nil {
		return PrivateVMState{}, ErrProviderProtocol
	}
	state := PrivateVMState{
		Allocation: expected, CatalogGeneration: view.CatalogGeneration,
		Desired: PrivateVMDesiredState(view.DesiredState), Phase: PrivateVMPhase(view.VMPhase),
		Observed: PrivateVMObservedState(view.VMObservedState), ExpiresAt: expiresAt,
		ErrorCode: protocol.ErrorCode(view.ErrorCode),
	}
	if !validPrivateVMState(state) {
		return PrivateVMState{}, ErrProviderProtocol
	}
	return state, nil
}

func validPrivateVMState(state PrivateVMState) bool {
	if state.Desired != PrivateVMDesiredActive && state.Desired != PrivateVMDesiredAbsent {
		return false
	}
	switch state.Phase {
	case PrivateVMProvisioning, PrivateVMConfiguring, PrivateVMStarting, PrivateVMRunning,
		PrivateVMDestroying, PrivateVMAbsent, PrivateVMCleanupRequired, PrivateVMProviderLost:
	default:
		return false
	}
	switch state.Observed {
	case PrivateVMObservedUnknown, PrivateVMObservedStopped, PrivateVMObservedRunning,
		PrivateVMObservedDeleting, PrivateVMObservedAbsent, PrivateVMObservedError:
	default:
		return false
	}
	if state.ErrorCode != "" && state.ErrorCode != protocol.CodeCleanupRequired &&
		state.ErrorCode != protocol.CodeProviderUnavailable {
		return false
	}
	return true
}

func mapPrivateVMError(err error) error {
	if err == nil {
		return nil
	}
	var mapped []error
	if errors.Is(err, context.Canceled) {
		mapped = append(mapped, context.Canceled)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		mapped = append(mapped, context.DeadlineExceeded)
	}
	if errors.Is(err, protocol.ErrOutcomeUnknown) {
		mapped = append(mapped, ErrOperationOutcomeUnknown)
	}
	if errors.Is(err, protocol.ErrProtocolViolation) {
		mapped = append(mapped, ErrProviderProtocol)
	}
	switch {
	case errors.Is(err, protocol.ErrControllerFenced), errors.Is(err, protocol.ErrUnauthorized):
		mapped = append(mapped, ErrControllerFenced)
	case errors.Is(err, protocol.ErrIdempotencyConflict):
		mapped = append(mapped, ErrIdempotencyConflict)
	case errors.Is(err, protocol.ErrGenerationStale):
		mapped = append(mapped, ErrGenerationStale)
	case errors.Is(err, protocol.ErrAllocationNotFound):
		mapped = append(mapped, ErrAllocationNotFound)
	case errors.Is(err, protocol.ErrCleanupRequired):
		mapped = append(mapped, ErrProviderCleanupRequired)
	case errors.Is(err, protocol.ErrProviderUnavailable), errors.Is(err, protocol.ErrDeadlineExceeded):
		mapped = append(mapped, ErrProviderUnavailable)
	case errors.Is(err, protocol.ErrUnsupported):
		mapped = append(mapped, ErrUnsupported)
	case errors.Is(err, protocol.ErrInvalidRequest):
		mapped = append(mapped, ErrProviderRequest)
	}
	if len(mapped) == 0 {
		mapped = append(mapped, ErrProviderUnavailable)
	}
	return fmt.Errorf("private VM lifecycle: %w", errors.Join(mapped...))
}

func privateVMMutationResponseError() error {
	// The authenticated peer may already have applied the mutation before it
	// returned a success body that failed this adapter's semantic binding.
	return errors.Join(ErrOperationOutcomeUnknown, ErrProviderProtocol)
}

var _ PrivateVMLifecycleClient = (*protocol.Client)(nil)
