package runner

import (
	"context"
	"errors"
	"testing"
	"time"

	protocol "github.com/heung115/k8s-quiz/runnerprotocol/v1"
)

type fakePrivateVMLifecycleClient struct {
	activateRequest protocol.ActivateRequest
	createRequest   protocol.CreateRequest
	getRequest      protocol.GetRequest
	destroyRequest  protocol.DestroyRequest
	activateCalls   int
	createCalls     int
	getCalls        int
	destroyCalls    int
	activateErr     error
	createErr       error
	getErr          error
	destroyErr      error
	createView      protocol.AllocationView
	getView         protocol.AllocationView
	destroyView     protocol.AllocationView
}

func (f *fakePrivateVMLifecycleClient) ActivateController(_ context.Context, request protocol.ActivateRequest) error {
	f.activateCalls++
	f.activateRequest = request
	return f.activateErr
}

func (f *fakePrivateVMLifecycleClient) Create(_ context.Context, request protocol.CreateRequest) (protocol.AllocationView, error) {
	f.createCalls++
	f.createRequest = request
	return f.createView, f.createErr
}

func (f *fakePrivateVMLifecycleClient) Get(_ context.Context, request protocol.GetRequest) (protocol.AllocationView, error) {
	f.getCalls++
	f.getRequest = request
	return f.getView, f.getErr
}

func (f *fakePrivateVMLifecycleClient) Destroy(_ context.Context, request protocol.DestroyRequest) (protocol.AllocationView, error) {
	f.destroyCalls++
	f.destroyRequest = request
	return f.destroyView, f.destroyErr
}

func privateVMFixture(t *testing.T) (*PrivateVMLifecycle, *fakePrivateVMLifecycleClient, ControllerFence, CreateSessionRequest) {
	t.Helper()
	now := time.Date(2026, 8, 1, 12, 0, 0, 123000000, time.UTC)
	session := SessionRef{SessionID: "11111111-1111-4111-8111-111111111111", Generation: 3}
	request := CreateSessionRequest{
		AllocationID: AllocationIDForSession(session), Session: session,
		UserID: "22222222-2222-4222-8222-222222222222",
		Selection: CatalogSelection{Generation: 9, Problem: ProblemRef{
			ID: "pod-crash", Revision: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		}},
		ResourceProfile: "standard", ExpiresAt: now.Add(time.Hour),
		IdempotencyKey: "create-session-3",
	}
	client := &fakePrivateVMLifecycleClient{}
	lifecycle, err := NewPrivateVMLifecycle(client, "home-proxmox:test", 30*time.Second)
	if err != nil {
		t.Fatalf("NewPrivateVMLifecycle: %v", err)
	}
	lifecycle.now = func() time.Time { return now }
	return lifecycle, client, ControllerFence{ProviderID: "home-proxmox:test", Epoch: 7}, request
}

func privateVMView(request CreateSessionRequest, desired, phase, observed string) protocol.AllocationView {
	return protocol.AllocationView{
		AllocationID: request.AllocationID, SessionID: request.Session.SessionID,
		Generation: request.Session.Generation, CatalogGeneration: request.Selection.Generation,
		DesiredState: desired, VMPhase: phase, VMObservedState: observed,
		ExpiresAt: request.ExpiresAt.Format(time.RFC3339Nano), LifecycleScope: protocol.LifecycleScopeVMOnly,
	}
}

func TestPrivateVMLifecycleCreateBindsExactDomainRequest(t *testing.T) {
	lifecycle, client, fence, request := privateVMFixture(t)
	client.createView = privateVMView(request, "active", "vm_running", "vm_running")

	state, err := lifecycle.Create(context.Background(), fence, request)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if client.createCalls != 1 || state.Allocation.Provider != ProviderHomeProxmox ||
		state.Phase != PrivateVMRunning || state.Observed != PrivateVMObservedRunning {
		t.Fatalf("create calls=%d state=%+v", client.createCalls, state)
	}
	wire := client.createRequest
	if wire.Schema != protocol.Schema || wire.ControllerEpoch != fence.Epoch ||
		wire.AllocationID != request.AllocationID || wire.SessionID != request.Session.SessionID ||
		wire.Generation != request.Session.Generation || wire.UserID != request.UserID ||
		wire.CatalogGeneration != request.Selection.Generation || wire.ProblemID != request.Selection.Problem.ID ||
		wire.ProblemRevision != request.Selection.Problem.Revision || wire.ResourceProfile != request.ResourceProfile ||
		wire.IdempotencyKey != request.IdempotencyKey || wire.ExpiresAt != request.ExpiresAt.Format(time.RFC3339Nano) ||
		wire.EffectDeadline != "2026-08-01T12:00:30.123Z" {
		t.Fatalf("wire create request is not exact: %+v", wire)
	}
}

func TestPrivateVMLifecycleForwardsExpiredExactCreateForDurableReplay(t *testing.T) {
	lifecycle, client, fence, request := privateVMFixture(t)
	request.ExpiresAt = lifecycle.now().Add(-time.Minute)
	client.createView = privateVMView(request, "active", "vm_running", "vm_running")

	if _, err := lifecycle.Create(context.Background(), fence, request); err != nil {
		t.Fatalf("expired exact replay was blocked before durable provider: %v", err)
	}
	if client.createCalls != 1 || client.createRequest.ExpiresAt != request.ExpiresAt.Format(time.RFC3339Nano) {
		t.Fatalf("expired replay did not reach exact provider request: calls=%d request=%+v", client.createCalls, client.createRequest)
	}
}

func TestPrivateVMLifecycleRejectsFenceAndUntrustedCreateBeforeTransport(t *testing.T) {
	lifecycle, client, fence, request := privateVMFixture(t)
	wrongFence := fence
	wrongFence.ProviderID = "home-proxmox:other"
	if _, err := lifecycle.Create(context.Background(), wrongFence, request); !errors.Is(err, ErrControllerFenced) {
		t.Fatalf("wrong fence error=%v", err)
	}
	request.Selection.Problem.Revision = "development-revision"
	if _, err := lifecycle.Create(context.Background(), fence, request); !errors.Is(err, ErrProviderRequest) {
		t.Fatalf("untrusted revision error=%v", err)
	}
	if client.createCalls != 0 {
		t.Fatalf("invalid calls reached private transport %d time(s)", client.createCalls)
	}
}

func TestPrivateVMLifecyclePreservesUnknownMutationOutcome(t *testing.T) {
	lifecycle, client, fence, request := privateVMFixture(t)
	ref := AllocationRef{ID: request.AllocationID, Session: request.Session, Provider: ProviderHomeProxmox}
	client.destroyErr = errors.Join(protocol.ErrOutcomeUnknown, protocol.ErrProtocolViolation)

	_, err := lifecycle.Destroy(context.Background(), fence, ref)
	if !errors.Is(err, ErrOperationOutcomeUnknown) || !errors.Is(err, ErrProviderProtocol) {
		t.Fatalf("destroy error=%v, want outcome unknown and protocol error", err)
	}
	if client.destroyCalls != 1 {
		t.Fatalf("destroy calls=%d, want 1", client.destroyCalls)
	}
}

func TestPrivateVMLifecycleRejectsSemanticallyMismatchedSuccess(t *testing.T) {
	lifecycle, client, fence, request := privateVMFixture(t)
	client.createView = privateVMView(request, "active", "vm_running", "vm_running")
	client.createView.CatalogGeneration++

	if _, err := lifecycle.Create(context.Background(), fence, request); !errors.Is(err, ErrProviderProtocol) ||
		!errors.Is(err, ErrOperationOutcomeUnknown) {
		t.Fatalf("mismatched success error=%v", err)
	}
}

func TestPrivateVMLifecycleDestroySemanticMismatchIsOutcomeUnknown(t *testing.T) {
	lifecycle, client, fence, request := privateVMFixture(t)
	ref := AllocationRef{ID: request.AllocationID, Session: request.Session, Provider: ProviderHomeProxmox}
	client.destroyView = privateVMView(request, "absent", "vm_absent", "vm_absent")
	client.destroyView.AllocationID = "33333333-3333-4333-8333-333333333333"

	if _, err := lifecycle.Destroy(context.Background(), fence, ref); !errors.Is(err, ErrProviderProtocol) ||
		!errors.Is(err, ErrOperationOutcomeUnknown) {
		t.Fatalf("mismatched destroy success error=%v", err)
	}
}

func TestPrivateVMAbsenceRemainsVMOnlyEvidence(t *testing.T) {
	lifecycle, client, fence, request := privateVMFixture(t)
	ref := AllocationRef{ID: request.AllocationID, Session: request.Session, Provider: ProviderHomeProxmox}
	client.destroyView = privateVMView(request, "absent", "vm_absent", "vm_absent")

	state, err := lifecycle.Destroy(context.Background(), fence, ref)
	if err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if state.Phase != PrivateVMAbsent || state.Observed != PrivateVMObservedAbsent ||
		state.Allocation != ref {
		t.Fatalf("unexpected VM-only absence: %+v", state)
	}
	if _, implementsFullRunner := any(lifecycle).(Runner); implementsFullRunner {
		t.Fatal("VM-only lifecycle must not implement the full Runner contract")
	}
}
