package runner

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

type authorityTestTerminal struct {
	closeOnce sync.Once
	closed    chan struct{}
}

func newAuthorityTestTerminal() *authorityTestTerminal {
	return &authorityTestTerminal{closed: make(chan struct{})}
}

func (*authorityTestTerminal) Read([]byte) (int, error)    { return 0, io.EOF }
func (*authorityTestTerminal) Write(p []byte) (int, error) { return len(p), nil }
func (*authorityTestTerminal) Resize(int, int) error       { return nil }
func (t *authorityTestTerminal) Close() error {
	t.closeOnce.Do(func() { close(t.closed) })
	return nil
}

type authorityTestRunner struct {
	mu            sync.Mutex
	provider      ProviderKind
	creates       int
	destroys      int
	verifies      int
	terminalOpens int
	terminal      *authorityTestTerminal
	openEntered   chan struct{}
	openGate      chan struct{}
	ignoreOpenCtx bool
	createCtxDone chan struct{}
	lateEntered   chan string
	lateGate      chan struct{}
	verifyMutate  func(*VerifyResult)
}

func (r *authorityTestRunner) Kind() ProviderKind {
	if r.provider == "" {
		return ProviderLocalDocker
	}
	return r.provider
}
func (r *authorityTestRunner) CreateSession(ctx context.Context, request CreateSessionRequest) (AllocationRef, error) {
	r.mu.Lock()
	r.creates++
	done := r.createCtxDone
	r.mu.Unlock()
	if done != nil {
		<-ctx.Done()
		close(done)
		return AllocationRef{}, ctx.Err()
	}
	r.waitLate("create")
	return AllocationRef{ID: request.AllocationID, Session: request.Session, Provider: ProviderLocalDocker}, nil
}

func (r *authorityTestRunner) waitLate(operation string) {
	r.mu.Lock()
	entered, gate := r.lateEntered, r.lateGate
	r.mu.Unlock()
	if entered != nil {
		entered <- operation
	}
	if gate != nil {
		<-gate
	}
}

func (r *authorityTestRunner) WaitReady(context.Context, AllocationRef, time.Duration) error {
	r.waitLate("wait_ready")
	return nil
}
func (r *authorityTestRunner) SetupSession(context.Context, SetupSessionRequest) error {
	r.waitLate("setup")
	return nil
}
func (r *authorityTestRunner) OpenTerminal(ctx context.Context, _ OpenTerminalRequest) (TerminalSession, error) {
	r.mu.Lock()
	r.terminalOpens++
	entered, gate, ignoreCtx, terminal := r.openEntered, r.openGate, r.ignoreOpenCtx, r.terminal
	r.mu.Unlock()
	if entered != nil {
		select {
		case <-entered:
		default:
			close(entered)
		}
	}
	if gate != nil {
		if ignoreCtx {
			<-gate
		} else {
			select {
			case <-gate:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	}
	return terminal, nil
}
func (r *authorityTestRunner) VerifySession(ctx context.Context, request VerifyRequest) (VerifyResult, error) {
	r.mu.Lock()
	r.verifies++
	mutate := r.verifyMutate
	r.mu.Unlock()
	r.waitLate("verify")
	feedback, err := PublicFeedbackForStatus(VerifyFailed)
	if err != nil {
		return VerifyResult{}, err
	}
	evidence := "authority test evidence"
	fence, _ := ControllerFenceFromContext(ctx)
	assurance := VerifyAssuranceDevelopmentGuest
	attestation := ""
	if r.Kind() != ProviderLocalDocker {
		assurance = VerifyAssuranceTrusted
		attestation = "signed-test-attestation"
	}
	result := VerifyResult{
		Status:   VerifyFailed,
		Feedback: feedback,
		Evidence: evidence,
		Receipt: VerificationReceipt{
			Schema:                 VerificationReceiptSchema,
			OperationID:            request.IdempotencyKey,
			Allocation:             request.Allocation,
			Problem:                request.Problem,
			Deadline:               request.Deadline,
			Status:                 VerifyFailed,
			Feedback:               feedback,
			EvidenceDigest:         VerifyEvidenceDigest(evidence),
			VerifierArtifactDigest: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			VerifierExecutionID:    "authority-test-execution",
			StartedAt:              request.Deadline.Add(-2 * time.Second),
			FinishedAt:             request.Deadline.Add(-time.Second),
			Assurance:              assurance,
			ControllerFence:        fence,
			Attestation:            attestation,
		},
	}
	if mutate != nil {
		mutate(&result)
	}
	return result, nil
}
func (r *authorityTestRunner) GetSession(context.Context, AllocationRef) (Observation, error) {
	r.waitLate("get")
	return Observation{State: ObservedRunning}, nil
}
func (r *authorityTestRunner) DestroySession(context.Context, AllocationRef) error {
	r.mu.Lock()
	r.destroys++
	r.mu.Unlock()
	r.waitLate("destroy")
	return nil
}
func (r *authorityTestRunner) Reconcile(context.Context, ReconcileRequest) (ReconcileResult, error) {
	r.waitLate("reconcile")
	return ReconcileResult{}, nil
}

var authorityTestFence = ControllerFence{ProviderID: "local-docker:test", Epoch: 1}

type authorityTestAttestationVerifier struct {
	err      error
	requests []VerifyRequest
	receipts []VerificationReceipt
}

func (v *authorityTestAttestationVerifier) VerifyAttestation(_ context.Context, request VerifyRequest, receipt VerificationReceipt) error {
	v.requests = append(v.requests, request)
	v.receipts = append(v.receipts, receipt)
	return v.err
}

func authorityVerifyRequest(ref AllocationRef, operationID string) VerifyRequest {
	return VerifyRequest{
		Allocation:     ref,
		Problem:        ProblemRef{ID: "problem-1", Revision: "sha256:problem-revision"},
		IdempotencyKey: operationID,
		Deadline:       time.Now().Add(time.Minute),
	}
}

func TestAuthorityGateStateIsMonotonic(t *testing.T) {
	gate := NewAuthorityGate()
	if gate.State() != AuthorityRecovering || gate.Ready() {
		t.Fatalf("initial authority state = %s ready=%v", gate.State(), gate.Ready())
	}
	if err := gate.Activate(); err != nil {
		t.Fatal(err)
	}
	gate.BeginDrain()
	if gate.State() != AuthorityDraining || gate.Ready() {
		t.Fatalf("draining authority state = %s ready=%v", gate.State(), gate.Ready())
	}
	if err := gate.Activate(); !errors.Is(err, ErrControllerAuthorityUnavailable) {
		t.Fatalf("reactivate draining gate error = %v", err)
	}
	loss := errors.New("lease lost")
	gate.Fence(loss)
	gate.BeginDrain()
	if gate.State() != AuthorityFenced || !errors.Is(gate.Cause(), loss) {
		t.Fatalf("fenced authority state=%s cause=%v", gate.State(), gate.Cause())
	}
}

func TestAuthorityRunnerAllowsOnlyDestroyWhileDrainingAndNothingWhenFenced(t *testing.T) {
	gate := NewAuthorityGate()
	inner := &authorityTestRunner{terminal: newAuthorityTestTerminal()}
	authorized, err := NewAuthorityRunner(inner, gate, authorityTestFence)
	if err != nil {
		t.Fatal(err)
	}
	ref := AllocationRef{ID: "allocation-1", Session: SessionRef{SessionID: "session-1", Generation: 1}, Provider: ProviderLocalDocker}
	request := CreateSessionRequest{AllocationID: ref.ID, Session: ref.Session}
	if _, err := authorized.CreateSession(context.Background(), request); !errors.Is(err, ErrControllerAuthorityUnavailable) {
		t.Fatalf("create while recovering error = %v", err)
	}
	if err := gate.Activate(); err != nil {
		t.Fatal(err)
	}
	if _, err := authorized.CreateSession(context.Background(), request); err != nil {
		t.Fatalf("create while ready: %v", err)
	}
	gate.BeginDrain()
	if _, err := authorized.VerifySession(context.Background(), authorityVerifyRequest(ref, "verify-1")); !errors.Is(err, ErrControllerAuthorityUnavailable) {
		t.Fatalf("verify while draining error = %v", err)
	}
	if err := authorized.DestroySession(context.Background(), ref); err != nil {
		t.Fatalf("destroy while draining: %v", err)
	}
	gate.Fence(errors.New("lost"))
	if err := authorized.DestroySession(context.Background(), ref); !errors.Is(err, ErrControllerAuthorityUnavailable) {
		t.Fatalf("destroy while fenced error = %v", err)
	}
	inner.mu.Lock()
	creates, verifies, destroys := inner.creates, inner.verifies, inner.destroys
	inner.mu.Unlock()
	if creates != 1 || verifies != 0 || destroys != 1 {
		t.Fatalf("provider calls create=%d verify=%d destroy=%d", creates, verifies, destroys)
	}
}

func TestAuthorityFenceCancelsActiveWorkAndClosesTerminal(t *testing.T) {
	gate := NewAuthorityGate()
	if err := gate.Activate(); err != nil {
		t.Fatal(err)
	}
	terminal := newAuthorityTestTerminal()
	inner := &authorityTestRunner{terminal: terminal}
	authorized, _ := NewAuthorityRunner(inner, gate, authorityTestFence)
	ref := AllocationRef{ID: "allocation-1", Session: SessionRef{SessionID: "session-1", Generation: 1}, Provider: ProviderLocalDocker}
	opened, err := authorized.OpenTerminal(context.Background(), OpenTerminalRequest{Allocation: ref, UserID: "user-1", LeaseID: "lease-1"})
	if err != nil {
		t.Fatal(err)
	}
	gate.Fence(errors.New("lost"))
	select {
	case <-terminal.closed:
	case <-time.After(time.Second):
		t.Fatal("authority fence did not close active terminal")
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestAuthorityFenceClosesTerminalReturnedAfterCancellation(t *testing.T) {
	gate := NewAuthorityGate()
	if err := gate.Activate(); err != nil {
		t.Fatal(err)
	}
	terminal := newAuthorityTestTerminal()
	inner := &authorityTestRunner{
		terminal: terminal, openEntered: make(chan struct{}), openGate: make(chan struct{}), ignoreOpenCtx: true,
	}
	authorized, _ := NewAuthorityRunner(inner, gate, authorityTestFence)
	result := make(chan error, 1)
	go func() {
		_, err := authorized.OpenTerminal(context.Background(), OpenTerminalRequest{})
		result <- err
	}()
	<-inner.openEntered
	gate.Fence(errors.New("lost"))
	close(inner.openGate)
	if err := <-result; !errors.Is(err, ErrControllerAuthorityUnavailable) {
		t.Fatalf("late terminal open error = %v", err)
	}
	select {
	case <-terminal.closed:
	case <-time.After(time.Second):
		t.Fatal("late terminal result was not closed")
	}
}

func TestAuthorityFenceCancelsActiveCreate(t *testing.T) {
	gate := NewAuthorityGate()
	if err := gate.Activate(); err != nil {
		t.Fatal(err)
	}
	createCtxDone := make(chan struct{})
	inner := &authorityTestRunner{createCtxDone: createCtxDone}
	authorized, err := NewAuthorityRunner(inner, gate, authorityTestFence)
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, err := authorized.CreateSession(context.Background(), CreateSessionRequest{})
		result <- err
	}()

	inner.mu.Lock()
	creates := inner.creates
	inner.mu.Unlock()
	deadline := time.After(time.Second)
	for creates == 0 {
		select {
		case <-deadline:
			t.Fatal("provider create did not start")
		default:
			time.Sleep(time.Millisecond)
			inner.mu.Lock()
			creates = inner.creates
			inner.mu.Unlock()
		}
	}

	gate.Fence(errors.New("lost"))
	select {
	case <-createCtxDone:
	case <-time.After(time.Second):
		t.Fatal("authority fence did not cancel active create")
	}
	if err := <-result; !errors.Is(err, ErrControllerAuthorityUnavailable) {
		t.Fatalf("create after fence error = %v, want authority unavailable", err)
	}
}

func TestAuthorityRunnerRejectsContextIgnoringLateResultsAfterFence(t *testing.T) {
	operations := []struct {
		name string
		call func(*AuthorityRunner) error
	}{
		{name: "create", call: func(r *AuthorityRunner) error {
			_, err := r.CreateSession(context.Background(), CreateSessionRequest{})
			return err
		}},
		{name: "wait_ready", call: func(r *AuthorityRunner) error {
			return r.WaitReady(context.Background(), AllocationRef{}, time.Second)
		}},
		{name: "setup", call: func(r *AuthorityRunner) error {
			return r.SetupSession(context.Background(), SetupSessionRequest{})
		}},
		{name: "verify", call: func(r *AuthorityRunner) error {
			ref := AllocationRef{ID: "allocation-late", Session: SessionRef{SessionID: "session-late", Generation: 1}, Provider: ProviderLocalDocker}
			_, err := r.VerifySession(context.Background(), authorityVerifyRequest(ref, "verify-key"))
			return err
		}},
		{name: "get", call: func(r *AuthorityRunner) error {
			_, err := r.GetSession(context.Background(), AllocationRef{})
			return err
		}},
		{name: "reconcile", call: func(r *AuthorityRunner) error {
			_, err := r.Reconcile(context.Background(), ReconcileRequest{})
			return err
		}},
		{name: "destroy", call: func(r *AuthorityRunner) error {
			return r.DestroySession(context.Background(), AllocationRef{})
		}},
	}
	for _, test := range operations {
		t.Run(test.name, func(t *testing.T) {
			gate := NewAuthorityGate()
			if err := gate.Activate(); err != nil {
				t.Fatal(err)
			}
			inner := &authorityTestRunner{lateEntered: make(chan string, 1), lateGate: make(chan struct{})}
			authorized, err := NewAuthorityRunner(inner, gate, authorityTestFence)
			if err != nil {
				t.Fatal(err)
			}
			result := make(chan error, 1)
			go func() { result <- test.call(authorized) }()
			if entered := <-inner.lateEntered; entered != test.name {
				t.Fatalf("provider entered %q, want %q", entered, test.name)
			}
			gate.Fence(errors.New("lease lost"))
			close(inner.lateGate)
			if err := <-result; !errors.Is(err, ErrControllerAuthorityUnavailable) {
				t.Fatalf("late %s result error = %v, want authority unavailable", test.name, err)
			}
		})
	}
}

func TestAuthorityRecoveryRejectsContextIgnoringLateReconcile(t *testing.T) {
	gate := NewAuthorityGate()
	inner := &authorityTestRunner{lateEntered: make(chan string, 1), lateGate: make(chan struct{})}
	authorized, err := NewAuthorityRunner(inner, gate, authorityTestFence)
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, reconcileErr := authorized.ReconcileRecovery(context.Background(), ReconcileRequest{Apply: true})
		result <- reconcileErr
	}()
	if entered := <-inner.lateEntered; entered != "reconcile" {
		t.Fatalf("provider entered %q", entered)
	}
	gate.Fence(errors.New("lease lost"))
	close(inner.lateGate)
	if err := <-result; !errors.Is(err, ErrControllerAuthorityUnavailable) {
		t.Fatalf("late recovery result error = %v, want authority unavailable", err)
	}
}

func TestAuthorityRunnerAllowsRecoveryOperationsOnlyWhileRecovering(t *testing.T) {
	gate := NewAuthorityGate()
	inner := &authorityTestRunner{}
	authorized, err := NewAuthorityRunner(inner, gate, authorityTestFence)
	if err != nil {
		t.Fatal(err)
	}
	ref := AllocationRef{
		ID:       "allocation-recovery-1",
		Session:  SessionRef{SessionID: "session-recovery-1", Generation: 1},
		Provider: ProviderLocalDocker,
	}
	request := CreateSessionRequest{AllocationID: ref.ID, Session: ref.Session}

	created, err := authorized.CreateSessionRecovery(context.Background(), request)
	if err != nil {
		t.Fatalf("recovery create while recovering: %v", err)
	}
	if created != ref {
		t.Fatalf("recovery create ref = %+v, want %+v", created, ref)
	}
	if err := authorized.WaitReadyRecovery(context.Background(), ref, time.Second); err != nil {
		t.Fatalf("recovery wait while recovering: %v", err)
	}
	if err := authorized.SetupSessionRecovery(context.Background(), SetupSessionRequest{Allocation: ref, IdempotencyKey: SetupIdempotencyKey(ref)}); err != nil {
		t.Fatalf("recovery setup while recovering: %v", err)
	}
	if _, err := authorized.GetSessionRecovery(context.Background(), ref); err != nil {
		t.Fatalf("recovery get while recovering: %v", err)
	}
	if err := authorized.DestroySessionRecovery(context.Background(), ref); err != nil {
		t.Fatalf("recovery destroy while recovering: %v", err)
	}
	if _, err := authorized.ReconcileRecovery(context.Background(), ReconcileRequest{Apply: true}); err != nil {
		t.Fatalf("recovery reconcile while recovering: %v", err)
	}

	inner.mu.Lock()
	createsBeforeReady, destroysBeforeReady := inner.creates, inner.destroys
	inner.mu.Unlock()
	if err := gate.Activate(); err != nil {
		t.Fatal(err)
	}

	readyCalls := []struct {
		name string
		call func() error
	}{
		{name: "create", call: func() error {
			_, err := authorized.CreateSessionRecovery(context.Background(), request)
			return err
		}},
		{name: "wait_ready", call: func() error {
			return authorized.WaitReadyRecovery(context.Background(), ref, time.Second)
		}},
		{name: "setup", call: func() error {
			return authorized.SetupSessionRecovery(context.Background(), SetupSessionRequest{Allocation: ref, IdempotencyKey: SetupIdempotencyKey(ref)})
		}},
		{name: "get", call: func() error {
			_, err := authorized.GetSessionRecovery(context.Background(), ref)
			return err
		}},
		{name: "destroy", call: func() error {
			return authorized.DestroySessionRecovery(context.Background(), ref)
		}},
		{name: "reconcile", call: func() error {
			_, err := authorized.ReconcileRecovery(context.Background(), ReconcileRequest{Apply: true})
			return err
		}},
	}
	for _, test := range readyCalls {
		t.Run("ready_rejects_"+test.name, func(t *testing.T) {
			if err := test.call(); !errors.Is(err, ErrControllerAuthorityUnavailable) {
				t.Fatalf("recovery %s after activation error = %v", test.name, err)
			}
		})
	}
	inner.mu.Lock()
	createsAfterReady, destroysAfterReady := inner.creates, inner.destroys
	inner.mu.Unlock()
	if createsAfterReady != createsBeforeReady || destroysAfterReady != destroysBeforeReady {
		t.Fatalf("recovery calls reached provider after activation: creates %d->%d destroys %d->%d",
			createsBeforeReady, createsAfterReady, destroysBeforeReady, destroysAfterReady)
	}
}

func TestAuthorityRunnerRejectsContextIgnoringRecoveryResultsAfterFence(t *testing.T) {
	operations := []struct {
		name string
		call func(*AuthorityRunner) error
	}{
		{name: "create", call: func(r *AuthorityRunner) error {
			_, err := r.CreateSessionRecovery(context.Background(), CreateSessionRequest{})
			return err
		}},
		{name: "wait_ready", call: func(r *AuthorityRunner) error {
			return r.WaitReadyRecovery(context.Background(), AllocationRef{}, time.Second)
		}},
		{name: "setup", call: func(r *AuthorityRunner) error {
			return r.SetupSessionRecovery(context.Background(), SetupSessionRequest{})
		}},
		{name: "get", call: func(r *AuthorityRunner) error {
			_, err := r.GetSessionRecovery(context.Background(), AllocationRef{})
			return err
		}},
		{name: "destroy", call: func(r *AuthorityRunner) error {
			return r.DestroySessionRecovery(context.Background(), AllocationRef{})
		}},
		{name: "reconcile", call: func(r *AuthorityRunner) error {
			_, err := r.ReconcileRecovery(context.Background(), ReconcileRequest{Apply: true})
			return err
		}},
	}
	for _, test := range operations {
		t.Run(test.name, func(t *testing.T) {
			gate := NewAuthorityGate()
			inner := &authorityTestRunner{lateEntered: make(chan string, 1), lateGate: make(chan struct{})}
			authorized, err := NewAuthorityRunner(inner, gate, authorityTestFence)
			if err != nil {
				t.Fatal(err)
			}
			result := make(chan error, 1)
			go func() { result <- test.call(authorized) }()
			if entered := <-inner.lateEntered; entered != test.name {
				t.Fatalf("provider entered %q, want %q", entered, test.name)
			}
			gate.Fence(errors.New("lease lost"))
			close(inner.lateGate)
			if err := <-result; !errors.Is(err, ErrControllerAuthorityUnavailable) {
				t.Fatalf("late recovery %s result error = %v, want authority unavailable", test.name, err)
			}
		})
	}
}

func TestAuthorityRunnerPropagatesControllerFenceDuringRecovery(t *testing.T) {
	gate := NewAuthorityGate()
	seen := make(chan ControllerFence, 1)
	inner := &fenceInspectRunner{seen: seen}
	authorized, err := NewAuthorityRunner(inner, gate, authorityTestFence)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := authorized.CreateSessionRecovery(context.Background(), CreateSessionRequest{}); err != nil {
		t.Fatal(err)
	}
	if got := <-seen; got != authorityTestFence {
		t.Fatalf("recovery provider fence = %+v, want %+v", got, authorityTestFence)
	}
}

func TestAuthorityRunnerPropagatesControllerFenceToProvider(t *testing.T) {
	gate := NewAuthorityGate()
	if err := gate.Activate(); err != nil {
		t.Fatal(err)
	}
	seen := make(chan ControllerFence, 1)
	inner := &fenceInspectRunner{seen: seen}
	authorized, err := NewAuthorityRunner(inner, gate, authorityTestFence)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := authorized.CreateSession(context.Background(), CreateSessionRequest{}); err != nil {
		t.Fatal(err)
	}
	if got := <-seen; got != authorityTestFence {
		t.Fatalf("provider fence = %+v, want %+v", got, authorityTestFence)
	}
}

func TestAuthorityRunnerRejectsVerifyReceiptMismatch(t *testing.T) {
	gate := NewAuthorityGate()
	if err := gate.Activate(); err != nil {
		t.Fatal(err)
	}
	inner := &authorityTestRunner{verifyMutate: func(result *VerifyResult) {
		result.Receipt.Allocation.ID = "different-allocation"
	}}
	authorized, err := NewAuthorityRunner(inner, gate, authorityTestFence)
	if err != nil {
		t.Fatal(err)
	}
	ref := AllocationRef{ID: "allocation-verify", Session: SessionRef{SessionID: "session-verify", Generation: 1}, Provider: ProviderLocalDocker}
	_, err = authorized.VerifySession(context.Background(), authorityVerifyRequest(ref, "verify-receipt-mismatch"))
	if err == nil || !strings.Contains(err.Error(), "verify receipt allocation does not match request") {
		t.Fatalf("receipt mismatch error = %v", err)
	}
}

func TestAuthorityRunnerRejectsVerifyReceiptFenceMismatch(t *testing.T) {
	gate := NewAuthorityGate()
	if err := gate.Activate(); err != nil {
		t.Fatal(err)
	}
	inner := &authorityTestRunner{verifyMutate: func(result *VerifyResult) {
		result.Receipt.ControllerFence.Epoch++
	}}
	authorized, err := NewAuthorityRunner(inner, gate, authorityTestFence)
	if err != nil {
		t.Fatal(err)
	}
	ref := AllocationRef{ID: "allocation-verify", Session: SessionRef{SessionID: "session-verify", Generation: 1}, Provider: ProviderLocalDocker}
	_, err = authorized.VerifySession(context.Background(), authorityVerifyRequest(ref, "verify-fence-mismatch"))
	if err == nil || !strings.Contains(err.Error(), "verify receipt controller fence does not match authority") {
		t.Fatalf("receipt fence mismatch error = %v", err)
	}
}

func TestAuthorityRunnerRequiresCryptographicAttestationForNonLocalProvider(t *testing.T) {
	gate := NewAuthorityGate()
	inner := &authorityTestRunner{provider: ProviderHomeProxmox}
	fence := ControllerFence{ProviderID: "home-proxmox:test", Epoch: 1}

	if _, err := NewAuthorityRunner(inner, gate, fence); err == nil || !strings.Contains(err.Error(), "cryptographic attestation verification") {
		t.Fatalf("non-local authority without attestation verifier error = %v", err)
	}
	if _, err := NewTrustedAuthorityRunner(inner, gate, fence, nil); err == nil || !strings.Contains(err.Error(), "requires an attestation verifier") {
		t.Fatalf("trusted authority with nil verifier error = %v", err)
	}
	if _, err := NewTrustedAuthorityRunner(&authorityTestRunner{}, gate, authorityTestFence, &authorityTestAttestationVerifier{}); err == nil || !strings.Contains(err.Error(), "local Docker") {
		t.Fatalf("local authority accepted trusted verifier: %v", err)
	}
}

func TestAuthorityRunnerRejectsUnauthenticatedTrustedReceipt(t *testing.T) {
	gate := NewAuthorityGate()
	if err := gate.Activate(); err != nil {
		t.Fatal(err)
	}
	inner := &authorityTestRunner{provider: ProviderHomeProxmox}
	verifier := &authorityTestAttestationVerifier{err: errors.New("signature mismatch")}
	fence := ControllerFence{ProviderID: "home-proxmox:test", Epoch: 1}
	authorized, err := NewTrustedAuthorityRunner(inner, gate, fence, verifier)
	if err != nil {
		t.Fatal(err)
	}
	ref := AllocationRef{
		ID: "allocation-trusted", Session: SessionRef{SessionID: "session-trusted", Generation: 1},
		Provider: ProviderHomeProxmox,
	}
	request := authorityVerifyRequest(ref, "verify-trusted-attestation")
	if _, err := authorized.VerifySession(context.Background(), request); err == nil || !strings.Contains(err.Error(), "authenticate verify receipt: signature mismatch") {
		t.Fatalf("unauthenticated trusted receipt error = %v", err)
	}
	if len(verifier.requests) != 1 || verifier.requests[0] != request || len(verifier.receipts) != 1 {
		t.Fatalf("attestation verifier calls requests=%+v receipts=%+v", verifier.requests, verifier.receipts)
	}
}

func TestAuthorityRunnerAcceptsAuthenticatedTrustedReceipt(t *testing.T) {
	gate := NewAuthorityGate()
	if err := gate.Activate(); err != nil {
		t.Fatal(err)
	}
	inner := &authorityTestRunner{provider: ProviderCloud}
	verifier := &authorityTestAttestationVerifier{}
	fence := ControllerFence{ProviderID: "cloud:test", Epoch: 1}
	authorized, err := NewTrustedAuthorityRunner(inner, gate, fence, verifier)
	if err != nil {
		t.Fatal(err)
	}
	ref := AllocationRef{
		ID: "allocation-cloud", Session: SessionRef{SessionID: "session-cloud", Generation: 1},
		Provider: ProviderCloud,
	}
	request := authorityVerifyRequest(ref, "verify-cloud-attestation")
	if _, err := authorized.VerifySession(context.Background(), request); err != nil {
		t.Fatalf("authenticated trusted receipt rejected: %v", err)
	}
	if len(verifier.requests) != 1 || verifier.requests[0] != request {
		t.Fatalf("attestation verifier calls = %+v", verifier.requests)
	}
}

type fenceInspectRunner struct{ seen chan ControllerFence }

func (*fenceInspectRunner) Kind() ProviderKind { return ProviderLocalDocker }
func (r *fenceInspectRunner) CreateSession(ctx context.Context, request CreateSessionRequest) (AllocationRef, error) {
	fence, _ := ControllerFenceFromContext(ctx)
	r.seen <- fence
	return AllocationRef{}, nil
}
func (*fenceInspectRunner) WaitReady(context.Context, AllocationRef, time.Duration) error { return nil }
func (*fenceInspectRunner) SetupSession(context.Context, SetupSessionRequest) error       { return nil }
func (*fenceInspectRunner) OpenTerminal(context.Context, OpenTerminalRequest) (TerminalSession, error) {
	return nil, ErrUnsupported
}
func (*fenceInspectRunner) VerifySession(context.Context, VerifyRequest) (VerifyResult, error) {
	return VerifyResult{}, nil
}
func (*fenceInspectRunner) GetSession(context.Context, AllocationRef) (Observation, error) {
	return Observation{}, nil
}
func (*fenceInspectRunner) DestroySession(context.Context, AllocationRef) error { return nil }
func (*fenceInspectRunner) Reconcile(context.Context, ReconcileRequest) (ReconcileResult, error) {
	return ReconcileResult{}, nil
}
