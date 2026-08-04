package runner

import (
	"context"
	"errors"
	"io"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	containerapi "github.com/k8s-quiz/backend/internal/container"
)

type fakeCatalog struct {
	mu       sync.Mutex
	runtime  LocalDockerRuntime
	resolve  func(ProblemRef) (LocalDockerRuntime, error)
	requests []ProblemRef
	started  chan struct{}
	block    <-chan struct{}
}

func (c *fakeCatalog) ResolveRuntime(_ context.Context, ref ProblemRef) (LocalDockerRuntime, error) {
	c.mu.Lock()
	c.requests = append(c.requests, ref)
	runtime := c.runtime
	resolve := c.resolve
	started := c.started
	block := c.block
	c.mu.Unlock()
	if started != nil {
		select {
		case started <- struct{}{}:
		default:
		}
	}
	if block != nil {
		<-block
	}
	if resolve != nil {
		return resolve(ref)
	}
	return runtime, nil
}

type fakeTerminal struct{}

type fakeLocalDockerVerifier struct {
	mu     sync.Mutex
	calls  []LocalDockerVerifyRequest
	verify func(context.Context, LocalDockerVerifyRequest) (VerifyResult, error)
}

func (v *fakeLocalDockerVerifier) Verify(ctx context.Context, request LocalDockerVerifyRequest) (VerifyResult, error) {
	v.mu.Lock()
	v.calls = append(v.calls, request)
	verify := v.verify
	v.mu.Unlock()
	if verify == nil {
		return VerifyResult{}, errors.New("fake verifier result is not configured")
	}
	return verify(ctx, request)
}

func (v *fakeLocalDockerVerifier) recordedCalls() []LocalDockerVerifyRequest {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]LocalDockerVerifyRequest(nil), v.calls...)
}

type observedTerminal struct {
	closeOnce sync.Once
	closed    chan struct{}
}

func newObservedTerminal() *observedTerminal {
	return &observedTerminal{closed: make(chan struct{})}
}

func (*observedTerminal) Read([]byte) (int, error)    { return 0, io.EOF }
func (*observedTerminal) Write(p []byte) (int, error) { return len(p), nil }
func (t *observedTerminal) Close() error {
	t.closeOnce.Do(func() { close(t.closed) })
	return nil
}
func (*observedTerminal) Resize(cols, rows int) error { return nil }

type fakeContainerManager struct {
	mu sync.Mutex

	created             []containerapi.CreateOpts
	createEntered       chan containerapi.CreateOpts
	createGate          chan struct{}
	createIgnoresCancel bool
	execCommands        [][]string
	terminalCmds        [][]string
	removed             [][2]string
	removeCalls         int
	removeErr           error
	ownedInspections    []containerapi.OwnedAllocationObservation
	ownedInspectErr     error
	ownedRemoveResult   containerapi.OwnedAllocationRemoval
	ownedRemoveErr      error
	ownedInspectCalls   int
	ownedRemoveCalls    int
	ownedNames          []string
	ownedLabels         []map[string]string
	execResult          containerapi.ExecResult
	execErr             error
	execEntered         chan []string
	execGate            chan struct{}
	running             bool
	terminal            containerapi.TerminalSession
	terminalErr         error
	terminals           []*observedTerminal
}

func (m *fakeContainerManager) Create(ctx context.Context, opts containerapi.CreateOpts) (string, error) {
	m.mu.Lock()
	m.created = append(m.created, opts)
	entered, gate, ignoresCancel := m.createEntered, m.createGate, m.createIgnoresCancel
	m.mu.Unlock()
	if entered != nil {
		select {
		case entered <- opts:
		default:
		}
	}
	if gate != nil {
		if ignoresCancel {
			<-gate
		} else {
			select {
			case <-gate:
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
	}
	return "container-" + opts.Labels["k8s-quiz.allocation"], nil
}

func (m *fakeContainerManager) CreateOwnedAllocation(ctx context.Context, opts containerapi.CreateOpts) (containerapi.OwnedAllocationHandle, error) {
	containerID, err := m.Create(ctx, opts)
	if err != nil {
		return containerapi.OwnedAllocationHandle{}, err
	}
	return containerapi.OwnedAllocationHandle{
		ContainerID: containerID,
		NetworkID:   "network-" + opts.Labels["k8s-quiz.allocation"],
	}, nil
}

func (m *fakeContainerManager) Exec(ctx context.Context, _ string, cmd []string) (containerapi.ExecResult, error) {
	m.mu.Lock()
	m.execCommands = append(m.execCommands, append([]string(nil), cmd...))
	entered, gate := m.execEntered, m.execGate
	result, err := m.execResult, m.execErr
	m.mu.Unlock()
	if entered != nil {
		select {
		case entered <- append([]string(nil), cmd...):
		default:
		}
	}
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return containerapi.ExecResult{}, ctx.Err()
		}
	}
	return result, err
}

func (m *fakeContainerManager) ExecInteractive(_ context.Context, _ string, cmd []string) (containerapi.TerminalSession, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.terminalCmds = append(m.terminalCmds, append([]string(nil), cmd...))
	if m.terminalErr != nil {
		return nil, m.terminalErr
	}
	if m.terminal == nil {
		terminal := newObservedTerminal()
		m.terminals = append(m.terminals, terminal)
		return terminal, nil
	}
	return m.terminal, nil
}

func (m *fakeContainerManager) Remove(ctx context.Context, id string) error {
	return m.RemoveAllocation(ctx, id, "")
}

func (m *fakeContainerManager) RemoveAllocation(_ context.Context, containerID, networkName string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.removeCalls++
	m.removed = append(m.removed, [2]string{containerID, networkName})
	if m.removeErr != nil {
		err := m.removeErr
		m.removeErr = nil
		return err
	}
	return nil
}

func (m *fakeContainerManager) Logs(context.Context, string) (string, error) { return "", nil }

func (m *fakeContainerManager) WaitReady(_ context.Context, _ string, check func() bool, _ time.Duration) error {
	if !check() {
		return errors.New("not ready")
	}
	return nil
}

func (m *fakeContainerManager) IsRunning(context.Context, string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.running, nil
}

func (m *fakeContainerManager) InspectOwnedAllocation(_ context.Context, target containerapi.OwnedAllocationTarget) (containerapi.OwnedAllocationObservation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ownedInspectCalls++
	m.ownedNames = append(m.ownedNames, target.Create.Name)
	m.ownedLabels = append(m.ownedLabels, cloneLabels(target.Create.Labels))
	if m.ownedInspectErr != nil {
		return containerapi.OwnedAllocationObservation{}, m.ownedInspectErr
	}
	if len(m.ownedInspections) == 0 {
		return containerapi.OwnedAllocationObservation{OwnershipComplete: true}, nil
	}
	result := m.ownedInspections[0]
	m.ownedInspections = m.ownedInspections[1:]
	return result, nil
}

func (m *fakeContainerManager) RemoveOwnedAllocation(_ context.Context, target containerapi.OwnedAllocationTarget) (containerapi.OwnedAllocationRemoval, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.removeCalls++
	m.ownedRemoveCalls++
	m.ownedNames = append(m.ownedNames, target.Create.Name)
	m.ownedLabels = append(m.ownedLabels, cloneLabels(target.Create.Labels))
	m.removed = append(m.removed, [2]string{target.Handle.ContainerID, target.Create.NetworkMode})
	if m.removeErr != nil {
		err := m.removeErr
		m.removeErr = nil
		return containerapi.OwnedAllocationRemoval{}, err
	}
	if m.ownedRemoveErr != nil {
		return m.ownedRemoveResult, m.ownedRemoveErr
	}
	if !m.ownedRemoveResult.Before.OwnershipComplete && !m.ownedRemoveResult.Before.ContainerPresent &&
		!m.ownedRemoveResult.Before.NetworkPresent && !m.ownedRemoveResult.After.OwnershipComplete &&
		!m.ownedRemoveResult.After.ContainerPresent && !m.ownedRemoveResult.After.NetworkPresent {
		return containerapi.OwnedAllocationRemoval{
			Before: containerapi.OwnedAllocationObservation{
				ContainerPresent: true, NetworkPresent: true, OwnershipComplete: true,
			},
			After: containerapi.OwnedAllocationObservation{OwnershipComplete: true},
		}, nil
	}
	return m.ownedRemoveResult, nil
}

func cloneLabels(labels map[string]string) map[string]string {
	cloned := make(map[string]string, len(labels))
	for key, value := range labels {
		cloned[key] = value
	}
	return cloned
}

func newLocalRunnerWithVerifier(t *testing.T, verifyType string, verifier LocalDockerVerifier) (*LocalDockerRunner, *fakeContainerManager, *fakeCatalog) {
	t.Helper()
	mgr := &fakeContainerManager{
		execResult: containerapi.ExecResult{ExitCode: 0, Stdout: "True"},
		running:    true,
	}
	catalog := &fakeCatalog{runtime: LocalDockerRuntime{
		Revision:     "revision-1",
		Image:        "sha256:" + strings.Repeat("a", 64),
		SetupScript:  "echo setup",
		VerifyScript: "echo verify",
		VerifyType:   verifyType,
	}}
	if verifier == nil {
		var err error
		verifier, err = NewDevelopmentGuestVerifier(mgr)
		if err != nil {
			t.Fatalf("NewDevelopmentGuestVerifier: %v", err)
		}
	}
	r, err := NewLocalDockerRunner(mgr, catalog, verifier, "test-scope")
	if err != nil {
		t.Fatalf("NewLocalDockerRunner: %v", err)
	}
	return r, mgr, catalog
}

func newLocalRunner(t *testing.T, verifyType string) (*LocalDockerRunner, *fakeContainerManager, *fakeCatalog) {
	t.Helper()
	return newLocalRunnerWithVerifier(t, verifyType, nil)
}

func createRequest(generation uint64, key string) CreateSessionRequest {
	session := SessionRef{SessionID: "session-1", Generation: generation}
	return CreateSessionRequest{
		AllocationID:    AllocationIDForSession(session),
		Session:         session,
		UserID:          "user-1",
		Selection:       CatalogSelection{Generation: 1, Problem: ProblemRef{ID: "problem-1", Revision: "revision-1"}},
		ResourceProfile: DefaultResourceProfile,
		ExpiresAt:       time.Now().Add(time.Hour).UTC(),
		IdempotencyKey:  key,
	}
}

var localDockerTestFence = ControllerFence{ProviderID: "local-docker:test-scope", Epoch: 1}

func localDockerVerifyContext() context.Context {
	return withControllerFence(context.Background(), localDockerTestFence)
}

func localDockerVerifyRequest(ref AllocationRef, key string) VerifyRequest {
	return VerifyRequest{
		Allocation:     ref,
		Problem:        ProblemRef{ID: "problem-1", Revision: "revision-1"},
		IdempotencyKey: key,
		Deadline:       time.Now().Add(time.Minute).UTC(),
	}
}

func validLocalDockerVerifyResult(request VerifyRequest) VerifyResult {
	feedback, err := PublicFeedbackForStatus(VerifyPassed)
	if err != nil {
		panic(err)
	}
	evidence := "private verifier evidence"
	startedAt := time.Now().Add(-time.Millisecond).UTC()
	finishedAt := time.Now().UTC()
	return VerifyResult{
		Status:   VerifyPassed,
		Feedback: feedback,
		Evidence: evidence,
		Receipt: VerificationReceipt{
			Schema:                 VerificationReceiptSchema,
			OperationID:            request.IdempotencyKey,
			Allocation:             request.Allocation,
			Problem:                request.Problem,
			Deadline:               request.Deadline,
			Status:                 VerifyPassed,
			Feedback:               feedback,
			EvidenceDigest:         VerifyEvidenceDigest(evidence),
			VerifierArtifactDigest: "sha256:" + strings.Repeat("b", 64),
			VerifierExecutionID:    "fake-verifier-execution-1",
			StartedAt:              startedAt,
			FinishedAt:             finishedAt,
			Assurance:              VerifyAssuranceDevelopmentGuest,
			ControllerFence:        localDockerTestFence,
		},
	}
}

func TestCreateRequestHasNoProviderMechanismFields(t *testing.T) {
	typ := reflect.TypeOf(CreateSessionRequest{})
	forbidden := []string{"image", "template", "command", "script", "mount", "path", "env", "privilege", "capability", "network", "proxmox", "cloud", "container"}
	for i := 0; i < typ.NumField(); i++ {
		name := strings.ToLower(typ.Field(i).Name)
		for _, word := range forbidden {
			if strings.Contains(name, word) {
				t.Fatalf("provider mechanism %q leaked into CreateSessionRequest field %q", word, typ.Field(i).Name)
			}
		}
	}
}

func TestLocalDockerCreateIsIdempotentAndUsesTrustedRuntime(t *testing.T) {
	r, mgr, catalog := newLocalRunner(t, "script")
	req := createRequest(1, "create-1")

	first, err := r.CreateSession(context.Background(), req)
	if err != nil {
		t.Fatalf("first create: %v", err)
	}
	second, err := r.CreateSession(context.Background(), req)
	if err != nil {
		t.Fatalf("duplicate create: %v", err)
	}
	if first != second {
		t.Fatalf("duplicate create returned different refs: %+v != %+v", first, second)
	}

	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	if len(mgr.created) != 1 {
		t.Fatalf("duplicate create made %d resources", len(mgr.created))
	}
	opts := mgr.created[0]
	if opts.Image != catalog.runtime.Image || !opts.Privileged || opts.CPULimit == 0 || opts.MemoryLimit == 0 {
		t.Fatalf("adapter did not use immutable trusted runtime/profile: %+v", opts)
	}
	if opts.NetworkMode != "k8s-quiz-test-scope-"+first.ID || opts.Name != opts.NetworkMode || opts.Labels["k8s-quiz.allocation"] != first.ID || opts.Labels["k8s-quiz.scope"] != "test-scope" {
		t.Fatalf("allocation ownership/network mismatch: %+v", opts)
	}
	if first.ID == "" || strings.Contains(first.ID, "container-") {
		t.Fatalf("allocation ID leaked/reused provider ID: %q", first.ID)
	}
	if first.ID != req.AllocationID {
		t.Fatalf("adapter changed reserved allocation ID: got %q want %q", first.ID, req.AllocationID)
	}
}

func TestLocalDockerRejectsUnreservedAllocationIdentity(t *testing.T) {
	r, mgr, _ := newLocalRunner(t, "script")
	req := createRequest(1, "create-bad-allocation")
	req.AllocationID = "d1bf68ca-7375-4b24-b87b-699b4f5b8fa0"

	if _, err := r.CreateSession(context.Background(), req); err == nil || !strings.Contains(err.Error(), "allocation id") {
		t.Fatalf("mismatched allocation id error = %v", err)
	}
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	if len(mgr.created) != 0 {
		t.Fatalf("invalid allocation reached provider: %+v", mgr.created)
	}
}

func TestLocalDockerCreateReplaySurvivesExpiryWithinRetention(t *testing.T) {
	r, mgr, _ := newLocalRunner(t, "script")
	req := createRequest(1, "create-expiring-replay")
	req.ExpiresAt = time.Now().Add(30 * time.Millisecond)
	first, err := r.CreateSession(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(40 * time.Millisecond)
	second, err := r.CreateSession(context.Background(), req)
	if err != nil {
		t.Fatalf("replay after request expiry: %v", err)
	}
	if first != second {
		t.Fatalf("expiry replay returned a different allocation: first=%+v second=%+v", first, second)
	}
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	if len(mgr.created) != 1 {
		t.Fatalf("expiry replay created %d provider resources", len(mgr.created))
	}
}

func TestLocalDockerConcurrentCreateSharesOneProviderOperation(t *testing.T) {
	r, mgr, catalog := newLocalRunner(t, "script")
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	catalog.mu.Lock()
	catalog.started = started
	catalog.block = release
	catalog.mu.Unlock()

	req := createRequest(1, "create-concurrent")
	type outcome struct {
		ref AllocationRef
		err error
	}
	results := make(chan outcome, 2)
	go func() {
		ref, err := r.CreateSession(context.Background(), req)
		results <- outcome{ref: ref, err: err}
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first create did not reach runtime resolution")
	}
	go func() {
		ref, err := r.CreateSession(context.Background(), req)
		results <- outcome{ref: ref, err: err}
	}()
	close(release)

	first := <-results
	second := <-results
	if first.err != nil || second.err != nil || first.ref != second.ref {
		t.Fatalf("concurrent duplicate result mismatch: first=%+v second=%+v", first, second)
	}
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	if len(mgr.created) != 1 {
		t.Fatalf("concurrent duplicate created %d provider resources", len(mgr.created))
	}
}

func TestLocalDockerCreateCompensatesSuccessReturnedAfterExpiresAt(t *testing.T) {
	r, mgr, _ := newLocalRunner(t, "script")
	mgr.mu.Lock()
	mgr.createEntered = make(chan containerapi.CreateOpts, 1)
	mgr.createGate = make(chan struct{})
	mgr.createIgnoresCancel = true
	entered, release := mgr.createEntered, mgr.createGate
	mgr.mu.Unlock()
	req := createRequest(1, "create-expired-after-provider")
	req.ExpiresAt = time.Now().Add(40 * time.Millisecond)

	result := make(chan error, 1)
	go func() {
		_, err := r.CreateSession(context.Background(), req)
		result <- err
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("create did not reach context-ignoring provider")
	}
	time.Sleep(60 * time.Millisecond)
	close(release)
	if err := <-result; !errors.Is(err, ErrSessionExpired) {
		t.Fatalf("late provider success error=%v, want ErrSessionExpired", err)
	}

	r.mu.RLock()
	_, allocated := r.allocations[req.AllocationID]
	_, active := r.active[req.Session.SessionID]
	r.mu.RUnlock()
	if allocated || active {
		t.Fatalf("expired provider success was published: allocated=%v active=%v", allocated, active)
	}
	mgr.mu.Lock()
	created, removed := len(mgr.created), len(mgr.removed)
	mgr.mu.Unlock()
	if created != 1 || removed != 1 {
		t.Fatalf("expired create effects created=%d removed=%d, want 1/1", created, removed)
	}
	if _, err := r.CreateSession(context.Background(), req); !errors.Is(err, ErrSessionExpired) {
		t.Fatalf("expired create replay error=%v", err)
	}
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	if len(mgr.created) != 1 || len(mgr.removed) != 1 {
		t.Fatalf("expired replay repeated provider effects: create=%d remove=%d", len(mgr.created), len(mgr.removed))
	}
}

func TestLocalDockerExpiredCreateCleanupFailureRemainsRetryable(t *testing.T) {
	r, mgr, _ := newLocalRunner(t, "script")
	mgr.mu.Lock()
	mgr.createEntered = make(chan containerapi.CreateOpts, 1)
	mgr.createGate = make(chan struct{})
	mgr.createIgnoresCancel = true
	mgr.removeErr = errors.New("cleanup unavailable")
	entered, release := mgr.createEntered, mgr.createGate
	mgr.mu.Unlock()
	req := createRequest(1, "create-expired-cleanup-retry")
	req.ExpiresAt = time.Now().Add(40 * time.Millisecond)
	ref := AllocationRef{ID: req.AllocationID, Session: req.Session, Provider: ProviderLocalDocker}

	result := make(chan error, 1)
	go func() {
		_, err := r.CreateSession(context.Background(), req)
		result <- err
	}()
	<-entered
	time.Sleep(60 * time.Millisecond)
	close(release)
	if err := <-result; !errors.Is(err, ErrSessionExpired) || !strings.Contains(err.Error(), "cleanup unavailable") {
		t.Fatalf("expired cleanup error=%v", err)
	}
	r.mu.RLock()
	retained := r.allocations[ref.ID]
	r.mu.RUnlock()
	if retained == nil || !retained.destroying {
		t.Fatal("failed expiry compensation did not retain exact cleanup identity")
	}
	if err := r.DestroySession(context.Background(), ref); err != nil {
		t.Fatalf("retry expired allocation cleanup: %v", err)
	}
}

func TestLocalDockerDestroyWaitsForLateCreateCompensation(t *testing.T) {
	r, mgr, _ := newLocalRunner(t, "script")
	mgr.mu.Lock()
	mgr.createEntered = make(chan containerapi.CreateOpts, 1)
	mgr.createGate = make(chan struct{})
	mgr.createIgnoresCancel = true
	entered, release := mgr.createEntered, mgr.createGate
	mgr.mu.Unlock()

	req := createRequest(1, "create-late-after-destroy")
	ref := AllocationRef{ID: req.AllocationID, Session: req.Session, Provider: ProviderLocalDocker}
	createCtx, cancelCreate := context.WithCancel(context.Background())
	createResult := make(chan error, 1)
	go func() {
		_, err := r.CreateSession(createCtx, req)
		createResult <- err
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("create did not reach context-ignoring provider")
	}

	destroyResult := make(chan error, 1)
	go func() { destroyResult <- r.DestroySession(context.Background(), ref) }()
	select {
	case err := <-destroyResult:
		t.Fatalf("destroy proved absence before in-flight create resolved: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	cancelCreate()
	close(release)
	if err := <-createResult; !errors.Is(err, ErrGenerationStale) {
		t.Fatalf("late create error = %v, want ErrGenerationStale", err)
	}
	if err := <-destroyResult; err != nil {
		t.Fatalf("destroy after late-create compensation: %v", err)
	}

	observation, err := r.GetSession(context.Background(), ref)
	if err != nil || observation.State != ObservedAbsent {
		t.Fatalf("late allocation observation = %+v, err=%v", observation, err)
	}
	r.mu.RLock()
	_, allocated := r.allocations[ref.ID]
	_, active := r.active[ref.Session.SessionID]
	r.mu.RUnlock()
	if allocated || active {
		t.Fatalf("late allocation was published: allocated=%v active=%v", allocated, active)
	}
	mgr.mu.Lock()
	created, removed := len(mgr.created), append([][2]string(nil), mgr.removed...)
	mgr.mu.Unlock()
	if created != 1 || len(removed) != 1 || !strings.Contains(removed[0][0], ref.ID) {
		t.Fatalf("late-create compensation effects: created=%d removed=%v", created, removed)
	}
}

func TestLocalDockerDestroyTombstoneRejectsFutureStaleCreateOnly(t *testing.T) {
	r, mgr, _ := newLocalRunner(t, "script")
	staleReq := createRequest(1, "create-after-absence")
	staleRef := AllocationRef{ID: staleReq.AllocationID, Session: staleReq.Session, Provider: ProviderLocalDocker}
	if err := r.DestroySession(context.Background(), staleRef); err != nil {
		t.Fatalf("record absent generation: %v", err)
	}
	if _, err := r.CreateSession(context.Background(), staleReq); !errors.Is(err, ErrGenerationStale) {
		t.Fatalf("create after exact absence error = %v, want ErrGenerationStale", err)
	}
	newerReq := createRequest(2, "create-newer-after-absence")
	if _, err := r.CreateSession(context.Background(), newerReq); err != nil {
		t.Fatalf("absence tombstone blocked newer generation: %v", err)
	}
	mgr.mu.Lock()
	creates := len(mgr.created)
	mgr.mu.Unlock()
	if creates != 1 {
		t.Fatalf("provider creates=%d, want only generation two", creates)
	}
}

func TestLocalDockerLateCreateCleanupFailureRemainsRetryable(t *testing.T) {
	r, mgr, _ := newLocalRunner(t, "script")
	mgr.mu.Lock()
	mgr.createEntered = make(chan containerapi.CreateOpts, 1)
	mgr.createGate = make(chan struct{})
	mgr.createIgnoresCancel = true
	mgr.removeErr = errors.New("provider cleanup temporarily unavailable")
	entered, release := mgr.createEntered, mgr.createGate
	mgr.mu.Unlock()

	req := createRequest(1, "create-late-cleanup-retry")
	ref := AllocationRef{ID: req.AllocationID, Session: req.Session, Provider: ProviderLocalDocker}
	createResult := make(chan error, 1)
	go func() {
		_, err := r.CreateSession(context.Background(), req)
		createResult <- err
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("create did not reach provider")
	}
	destroyResult := make(chan error, 1)
	go func() { destroyResult <- r.DestroySession(context.Background(), ref) }()
	deadline := time.Now().Add(time.Second)
	for {
		r.mu.RLock()
		pending := r.pending[ref.Session.SessionID]
		destroyRequested := pending != nil && pending.destroyRequested
		r.mu.RUnlock()
		if destroyRequested {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("destroy did not fence the in-flight create")
		}
		runtime.Gosched()
	}
	close(release)
	if err := <-createResult; !errors.Is(err, ErrGenerationStale) || !strings.Contains(err.Error(), "temporarily unavailable") {
		t.Fatalf("late create cleanup failure = %v", err)
	}
	if err := <-destroyResult; err == nil || !strings.Contains(err.Error(), "temporarily unavailable") {
		t.Fatalf("destroy must retain cleanup failure, got %v", err)
	}
	r.mu.RLock()
	retained := r.allocations[ref.ID]
	_, active := r.active[ref.Session.SessionID]
	r.mu.RUnlock()
	if retained == nil || !retained.destroying || active {
		t.Fatalf("retry identity retained=%+v active=%v", retained, active)
	}
	if err := r.DestroySession(context.Background(), ref); err != nil {
		t.Fatalf("retry exact late cleanup: %v", err)
	}
	observation, err := r.GetSession(context.Background(), ref)
	if err != nil || observation.State != ObservedAbsent {
		t.Fatalf("post-retry observation=%+v err=%v", observation, err)
	}
	mgr.mu.Lock()
	removeCalls := mgr.removeCalls
	mgr.mu.Unlock()
	if removeCalls != 2 {
		t.Fatalf("cleanup attempts=%d, want failed compensation plus durable retry", removeCalls)
	}
}

func TestLocalDockerSerializesGenerationsForSameSession(t *testing.T) {
	r, _, catalog := newLocalRunner(t, "script")
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	catalog.mu.Lock()
	catalog.started = started
	catalog.block = release
	catalog.mu.Unlock()

	type outcome struct {
		ref AllocationRef
		err error
	}
	oldResult := make(chan outcome, 1)
	newResult := make(chan outcome, 1)
	go func() {
		ref, err := r.CreateSession(context.Background(), createRequest(1, "create-old"))
		oldResult <- outcome{ref: ref, err: err}
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("old generation did not begin")
	}
	go func() {
		ref, err := r.CreateSession(context.Background(), createRequest(2, "create-new"))
		newResult <- outcome{ref: ref, err: err}
	}()
	select {
	case got := <-newResult:
		t.Fatalf("new generation bypassed in-flight old generation: %+v", got)
	case <-time.After(25 * time.Millisecond):
	}
	close(release)
	old := <-oldResult
	newer := <-newResult
	if old.err != nil || newer.err != nil {
		t.Fatalf("serialized create failed: old=%v new=%v", old.err, newer.err)
	}
	if _, err := r.OpenTerminal(context.Background(), OpenTerminalRequest{Allocation: old.ref, UserID: "user-1", LeaseID: "old"}); !errors.Is(err, ErrGenerationStale) {
		t.Fatalf("old generation remained active: %v", err)
	}
	terminal, err := r.OpenTerminal(context.Background(), OpenTerminalRequest{Allocation: newer.ref, UserID: "user-1", LeaseID: "new"})
	if err != nil {
		t.Fatalf("new generation is not active: %v", err)
	}
	_ = terminal.Close()
}

func TestLocalDockerUsesCatalogContentIDForEveryAllocation(t *testing.T) {
	r, mgr, _ := newLocalRunner(t, "script")
	if _, err := r.CreateSession(context.Background(), createRequest(1, "create-one")); err != nil {
		t.Fatal(err)
	}
	second := createRequest(1, "create-two")
	second.Session.SessionID = "session-2"
	second.AllocationID = AllocationIDForSession(second.Session)
	if _, err := r.CreateSession(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	want := "sha256:" + strings.Repeat("a", 64)
	if len(mgr.created) != 2 || mgr.created[0].Image != want || mgr.created[1].Image != want {
		t.Fatalf("allocations did not use catalog content ID: %+v", mgr.created)
	}
}

func TestLocalDockerRejectsRuntimeImageThatIsNotContentAddressed(t *testing.T) {
	r, mgr, catalog := newLocalRunner(t, "script")
	catalog.runtime.Image = "k3s-base:latest"
	if _, err := r.CreateSession(context.Background(), createRequest(1, "create-mutable-image")); err == nil || !strings.Contains(err.Error(), "not an immutable sha256 content ID") {
		t.Fatalf("mutable runtime image error = %v", err)
	}
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	if len(mgr.created) != 0 {
		t.Fatalf("mutable runtime image reached provider create: %+v", mgr.created)
	}
}

func TestLocalDockerCreateRejectsIdempotencyConflictAndRevisionMismatch(t *testing.T) {
	r, _, catalog := newLocalRunner(t, "script")
	req := createRequest(1, "create-1")
	if _, err := r.CreateSession(context.Background(), req); err != nil {
		t.Fatalf("create: %v", err)
	}
	conflict := req
	conflict.UserID = "different-user"
	if _, err := r.CreateSession(context.Background(), conflict); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("conflict error = %v, want ErrIdempotencyConflict", err)
	}

	r2, _, _ := newLocalRunner(t, "script")
	r2.catalog = &fakeCatalog{runtime: catalog.runtime}
	badRevision := createRequest(1, "bad-revision")
	badRevision.Selection.Problem.Revision = "unexpected"
	if _, err := r2.CreateSession(context.Background(), badRevision); !errors.Is(err, ErrInvalidRevision) {
		t.Fatalf("revision error = %v, want ErrInvalidRevision", err)
	}
}

func TestLocalDockerIdempotencyKeysConflictAcrossOperationKinds(t *testing.T) {
	t.Run("create key cannot be reused by verify", func(t *testing.T) {
		r, mgr, _ := newLocalRunner(t, "script")
		const sharedKey = "cross-kind-create-verify"
		ref, err := r.CreateSession(context.Background(), createRequest(1, sharedKey))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.VerifySession(localDockerVerifyContext(), localDockerVerifyRequest(ref, sharedKey)); !errors.Is(err, ErrIdempotencyConflict) {
			t.Fatalf("create-to-verify key reuse error=%v, want ErrIdempotencyConflict", err)
		}
		mgr.mu.Lock()
		defer mgr.mu.Unlock()
		if len(mgr.execCommands) != 0 {
			t.Fatalf("cross-kind verify reached provider: %+v", mgr.execCommands)
		}
	})

	t.Run("verify key cannot be reused by create", func(t *testing.T) {
		r, mgr, _ := newLocalRunner(t, "script")
		ref, err := r.CreateSession(context.Background(), createRequest(1, "create-before-cross-kind-verify"))
		if err != nil {
			t.Fatal(err)
		}
		const sharedKey = "cross-kind-verify-create"
		if _, err := r.VerifySession(localDockerVerifyContext(), localDockerVerifyRequest(ref, sharedKey)); err != nil {
			t.Fatal(err)
		}
		request := createRequest(1, sharedKey)
		request.Session.SessionID = "session-cross-kind-create"
		request.AllocationID = AllocationIDForSession(request.Session)
		if _, err := r.CreateSession(context.Background(), request); !errors.Is(err, ErrIdempotencyConflict) {
			t.Fatalf("verify-to-create key reuse error=%v, want ErrIdempotencyConflict", err)
		}
		mgr.mu.Lock()
		defer mgr.mu.Unlock()
		if len(mgr.created) != 1 {
			t.Fatalf("cross-kind create reached provider %d time(s)", len(mgr.created)-1)
		}
	})

	t.Run("create key cannot be reused by canonical setup", func(t *testing.T) {
		r, mgr, _ := newLocalRunner(t, "script")
		request := createRequest(1, "temporary")
		ref := AllocationRef{ID: request.AllocationID, Session: request.Session, Provider: ProviderLocalDocker}
		request.IdempotencyKey = SetupIdempotencyKey(ref)
		created, err := r.CreateSession(context.Background(), request)
		if err != nil {
			t.Fatal(err)
		}
		if err := r.SetupSession(context.Background(), SetupSessionRequest{
			Allocation: created, IdempotencyKey: SetupIdempotencyKey(created),
		}); !errors.Is(err, ErrIdempotencyConflict) {
			t.Fatalf("create-to-setup key reuse error=%v, want ErrIdempotencyConflict", err)
		}
		mgr.mu.Lock()
		defer mgr.mu.Unlock()
		if len(mgr.execCommands) != 0 {
			t.Fatalf("cross-kind setup reached provider: %+v", mgr.execCommands)
		}
	})
}

func TestLocalDockerSetupIsIdempotentAcrossSequentialAndConcurrentReplay(t *testing.T) {
	r, mgr, _ := newLocalRunner(t, "script")
	ref, err := r.CreateSession(context.Background(), createRequest(1, "create-setup-idempotent"))
	if err != nil {
		t.Fatal(err)
	}
	request := SetupSessionRequest{Allocation: ref, IdempotencyKey: SetupIdempotencyKey(ref)}
	mgr.mu.Lock()
	mgr.execEntered = make(chan []string, 1)
	mgr.execGate = make(chan struct{})
	mgr.mu.Unlock()

	results := make(chan error, 2)
	go func() { results <- r.SetupSession(context.Background(), request) }()
	select {
	case command := <-mgr.execEntered:
		if !reflect.DeepEqual(command, []string{"/bin/sh", "-c", "echo setup"}) {
			t.Fatalf("setup command = %v", command)
		}
	case <-time.After(time.Second):
		t.Fatal("first setup did not reach provider")
	}
	go func() { results <- r.SetupSession(context.Background(), request) }()
	time.Sleep(20 * time.Millisecond)
	mgr.mu.Lock()
	executionsBeforeRelease := len(mgr.execCommands)
	close(mgr.execGate)
	mgr.mu.Unlock()
	if executionsBeforeRelease != 1 {
		t.Fatalf("concurrent setup executed %d times before release", executionsBeforeRelease)
	}
	if err := <-results; err != nil {
		t.Fatalf("first setup: %v", err)
	}
	if err := <-results; err != nil {
		t.Fatalf("concurrent setup replay: %v", err)
	}
	if err := r.SetupSession(context.Background(), request); err != nil {
		t.Fatalf("sequential setup replay: %v", err)
	}
	mgr.mu.Lock()
	executions := len(mgr.execCommands)
	mgr.mu.Unlock()
	if executions != 1 {
		t.Fatalf("setup replay executed approved script %d times, want 1", executions)
	}
}

func TestLocalDockerSetupRejectsUntrustedRetryIdentityBeforeExecution(t *testing.T) {
	r, mgr, _ := newLocalRunner(t, "script")
	ref, err := r.CreateSession(context.Background(), createRequest(1, "create-setup-conflict"))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.SetupSession(context.Background(), SetupSessionRequest{
		Allocation: ref, IdempotencyKey: "setup:browser-selected",
	}); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("unsafe setup key error = %v, want ErrIdempotencyConflict", err)
	}
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	if len(mgr.execCommands) != 0 {
		t.Fatalf("unsafe setup key reached provider: %+v", mgr.execCommands)
	}
}

func TestLocalDockerStaleGenerationRejectedButExactDestroyAllowed(t *testing.T) {
	r, mgr, _ := newLocalRunner(t, "script")
	oldRef, err := r.CreateSession(context.Background(), createRequest(1, "create-1"))
	if err != nil {
		t.Fatal(err)
	}
	newRef, err := r.CreateSession(context.Background(), createRequest(2, "create-2"))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := r.OpenTerminal(context.Background(), OpenTerminalRequest{Allocation: oldRef, UserID: "user-1", LeaseID: "lease-old"}); !errors.Is(err, ErrGenerationStale) {
		t.Fatalf("old terminal error = %v, want ErrGenerationStale", err)
	}
	if _, err := r.VerifySession(localDockerVerifyContext(), localDockerVerifyRequest(oldRef, "verify-old")); !errors.Is(err, ErrGenerationStale) {
		t.Fatalf("old verify error = %v, want ErrGenerationStale", err)
	}
	if err := r.DestroySession(context.Background(), oldRef); err != nil {
		t.Fatalf("exact stale destroy: %v", err)
	}
	if _, err := r.VerifySession(localDockerVerifyContext(), localDockerVerifyRequest(newRef, "verify-new")); err != nil {
		t.Fatalf("stale destroy mutated new generation: %v", err)
	}
	mgr.mu.Lock()
	if len(mgr.removed) != 1 || !strings.Contains(mgr.removed[0][1], oldRef.ID) {
		t.Errorf("destroy did not target exact old allocation: %+v", mgr.removed)
	}
	mgr.mu.Unlock()
}

func TestLocalDockerTerminalUsesFixedShellAndAuthority(t *testing.T) {
	r, mgr, _ := newLocalRunner(t, "script")
	ref, err := r.CreateSession(context.Background(), createRequest(1, "create-1"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.OpenTerminal(context.Background(), OpenTerminalRequest{Allocation: ref, UserID: "wrong", LeaseID: "lease"}); err == nil {
		t.Fatal("expected mismatched user to be rejected")
	}
	if _, err := r.OpenTerminal(context.Background(), OpenTerminalRequest{Allocation: ref, UserID: "user-1", LeaseID: "lease"}); err != nil {
		t.Fatalf("open terminal: %v", err)
	}
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	if len(mgr.terminalCmds) != 1 || !reflect.DeepEqual(mgr.terminalCmds[0], []string{"/bin/sh"}) {
		t.Fatalf("terminal command is not fixed: %+v", mgr.terminalCmds)
	}
}

func TestLocalDockerTerminalLeaseCloseExpiryAndDestroy(t *testing.T) {
	t.Run("exact prior lease permits one prepared replacement", func(t *testing.T) {
		r, mgr, _ := newLocalRunner(t, "script")
		ref, err := r.CreateSession(context.Background(), createRequest(1, "create-lease"))
		if err != nil {
			t.Fatal(err)
		}
		first, err := r.OpenTerminal(context.Background(), OpenTerminalRequest{Allocation: ref, UserID: "user-1", LeaseID: "lease-1"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.OpenTerminal(context.Background(), OpenTerminalRequest{Allocation: ref, UserID: "user-1", LeaseID: "lease-2"}); !errors.Is(err, ErrTerminalLease) {
			t.Fatalf("second lease error = %v, want ErrTerminalLease", err)
		}
		if _, err := r.OpenTerminal(context.Background(), OpenTerminalRequest{
			Allocation: ref, UserID: "user-1", LeaseID: "lease-2", ReplacesLeaseID: "wrong-lease",
		}); !errors.Is(err, ErrTerminalLease) {
			t.Fatalf("wrong replacement lease error = %v, want ErrTerminalLease", err)
		}
		second, err := r.OpenTerminal(context.Background(), OpenTerminalRequest{
			Allocation: ref, UserID: "user-1", LeaseID: "lease-2", ReplacesLeaseID: "lease-1",
		})
		if err != nil {
			t.Fatalf("prepare replacement lease: %v", err)
		}
		if _, err := r.OpenTerminal(context.Background(), OpenTerminalRequest{
			Allocation: ref, UserID: "user-1", LeaseID: "lease-3", ReplacesLeaseID: "lease-1",
		}); !errors.Is(err, ErrTerminalLease) {
			t.Fatalf("third overlapping lease error = %v, want ErrTerminalLease", err)
		}
		if err := first.Close(); err != nil {
			t.Fatal(err)
		}
		_ = second.Close()
		mgr.mu.Lock()
		defer mgr.mu.Unlock()
		if len(mgr.terminals) != 2 {
			t.Fatalf("opened terminals = %d, want 2", len(mgr.terminals))
		}
	})

	t.Run("failed replacement preparation preserves prior lease", func(t *testing.T) {
		r, mgr, _ := newLocalRunner(t, "script")
		ref, err := r.CreateSession(context.Background(), createRequest(1, "create-failed-replacement"))
		if err != nil {
			t.Fatal(err)
		}
		first, err := r.OpenTerminal(context.Background(), OpenTerminalRequest{
			Allocation: ref, UserID: "user-1", LeaseID: "lease-1",
		})
		if err != nil {
			t.Fatal(err)
		}
		mgr.mu.Lock()
		priorTerminal := mgr.terminals[0]
		mgr.terminalErr = errors.New("provider open failed")
		mgr.mu.Unlock()
		if _, err := r.OpenTerminal(context.Background(), OpenTerminalRequest{
			Allocation: ref, UserID: "user-1", LeaseID: "lease-2", ReplacesLeaseID: "lease-1",
		}); err == nil {
			t.Fatal("expected replacement preparation failure")
		}
		select {
		case <-priorTerminal.closed:
			t.Fatal("failed replacement closed the prior provider terminal")
		default:
		}
		mgr.mu.Lock()
		mgr.terminalErr = nil
		mgr.mu.Unlock()
		replacement, err := r.OpenTerminal(context.Background(), OpenTerminalRequest{
			Allocation: ref, UserID: "user-1", LeaseID: "lease-2", ReplacesLeaseID: "lease-1",
		})
		if err != nil {
			t.Fatalf("failed candidate reservation leaked: %v", err)
		}
		_ = first.Close()
		_ = replacement.Close()
	})

	t.Run("expiry closes provider terminal", func(t *testing.T) {
		r, mgr, _ := newLocalRunner(t, "script")
		req := createRequest(1, "create-expiry")
		req.ExpiresAt = time.Now().Add(40 * time.Millisecond)
		ref, err := r.CreateSession(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.OpenTerminal(context.Background(), OpenTerminalRequest{Allocation: ref, UserID: "user-1", LeaseID: "lease-expiry"}); err != nil {
			t.Fatal(err)
		}
		mgr.mu.Lock()
		providerTerminal := mgr.terminals[0]
		mgr.mu.Unlock()
		select {
		case <-providerTerminal.closed:
		case <-time.After(time.Second):
			t.Fatal("terminal was not closed at session expiry")
		}
	})

	t.Run("destroy closes provider terminal", func(t *testing.T) {
		r, mgr, _ := newLocalRunner(t, "script")
		ref, err := r.CreateSession(context.Background(), createRequest(1, "create-destroy-terminal"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.OpenTerminal(context.Background(), OpenTerminalRequest{Allocation: ref, UserID: "user-1", LeaseID: "lease-destroy"}); err != nil {
			t.Fatal(err)
		}
		mgr.mu.Lock()
		providerTerminal := mgr.terminals[0]
		mgr.mu.Unlock()
		if err := r.DestroySession(context.Background(), ref); err != nil {
			t.Fatal(err)
		}
		select {
		case <-providerTerminal.closed:
		case <-time.After(time.Second):
			t.Fatal("destroy did not close active terminal")
		}
	})

	t.Run("context cancellation closes lease and permits replacement", func(t *testing.T) {
		r, mgr, _ := newLocalRunner(t, "script")
		ref, err := r.CreateSession(context.Background(), createRequest(1, "create-context-terminal"))
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		if _, err := r.OpenTerminal(ctx, OpenTerminalRequest{Allocation: ref, UserID: "user-1", LeaseID: "lease-context"}); err != nil {
			t.Fatal(err)
		}
		mgr.mu.Lock()
		providerTerminal := mgr.terminals[0]
		mgr.mu.Unlock()
		cancel()
		select {
		case <-providerTerminal.closed:
		case <-time.After(time.Second):
			t.Fatal("context cancellation did not close provider terminal")
		}
		replacement, err := r.OpenTerminal(context.Background(), OpenTerminalRequest{Allocation: ref, UserID: "user-1", LeaseID: "lease-replacement"})
		if err != nil {
			t.Fatalf("cancelled lease was not released: %v", err)
		}
		_ = replacement.Close()
	})
}

func TestLocalDockerVerifyDistinguishesFailureFromInfrastructureError(t *testing.T) {
	r, mgr, _ := newLocalRunner(t, "script")
	ref, err := r.CreateSession(context.Background(), createRequest(1, "create-1"))
	if err != nil {
		t.Fatal(err)
	}
	mgr.mu.Lock()
	mgr.execResult = containerapi.ExecResult{ExitCode: 1, Stdout: "not fixed"}
	mgr.mu.Unlock()
	result, err := r.VerifySession(localDockerVerifyContext(), localDockerVerifyRequest(ref, "verify-1"))
	if err != nil || result.Status != VerifyFailed || result.Feedback.Message != "Verification did not pass." || result.Evidence != "not fixed" {
		t.Fatalf("legitimate verify failure misclassified: result=%+v err=%v", result, err)
	}

	mgr.mu.Lock()
	mgr.execErr = errors.New("transport lost")
	mgr.mu.Unlock()
	if _, err := r.VerifySession(localDockerVerifyContext(), localDockerVerifyRequest(ref, "verify-2")); err == nil {
		t.Fatal("expected infrastructure error")
	}
}

func TestLocalDockerVerifyUsesInjectedCapabilityAndValidatesItsReceipt(t *testing.T) {
	t.Run("delegates once without direct learner exec", func(t *testing.T) {
		verifier := &fakeLocalDockerVerifier{}
		verifier.verify = func(_ context.Context, target LocalDockerVerifyRequest) (VerifyResult, error) {
			return validLocalDockerVerifyResult(target.Request), nil
		}
		r, mgr, catalog := newLocalRunnerWithVerifier(t, "script", verifier)
		ref, err := r.CreateSession(context.Background(), createRequest(1, "create-verifier-seam"))
		if err != nil {
			t.Fatal(err)
		}
		request := localDockerVerifyRequest(ref, "verify-verifier-seam")
		first, err := r.VerifySession(localDockerVerifyContext(), request)
		if err != nil {
			t.Fatalf("first verify: %v", err)
		}
		second, err := r.VerifySession(localDockerVerifyContext(), request)
		if err != nil {
			t.Fatalf("verify replay: %v", err)
		}
		if first != second {
			t.Fatalf("verify replay changed result: first=%+v second=%+v", first, second)
		}
		calls := verifier.recordedCalls()
		if len(calls) != 1 {
			t.Fatalf("injected verifier calls = %d, want 1", len(calls))
		}
		if calls[0].Request != request || calls[0].ContainerID == "" || calls[0].Runtime != catalog.runtime {
			t.Fatalf("verifier received wrong exact target: %+v", calls[0])
		}
		mgr.mu.Lock()
		defer mgr.mu.Unlock()
		if len(mgr.execCommands) != 0 {
			t.Fatalf("runner bypassed verifier capability with learner exec: %+v", mgr.execCommands)
		}
	})

	t.Run("rejects wrong problem revision before delegation", func(t *testing.T) {
		verifier := &fakeLocalDockerVerifier{verify: func(_ context.Context, target LocalDockerVerifyRequest) (VerifyResult, error) {
			return validLocalDockerVerifyResult(target.Request), nil
		}}
		r, _, _ := newLocalRunnerWithVerifier(t, "script", verifier)
		ref, err := r.CreateSession(context.Background(), createRequest(1, "create-wrong-revision"))
		if err != nil {
			t.Fatal(err)
		}
		request := localDockerVerifyRequest(ref, "verify-wrong-revision")
		request.Problem.Revision = "revision-2"
		if _, err := r.VerifySession(localDockerVerifyContext(), request); !errors.Is(err, ErrInvalidRevision) {
			t.Fatalf("wrong revision error = %v, want ErrInvalidRevision", err)
		}
		if calls := verifier.recordedCalls(); len(calls) != 0 {
			t.Fatalf("wrong revision reached verifier: %+v", calls)
		}
	})

	t.Run("rejects malformed verifier receipt", func(t *testing.T) {
		verifier := &fakeLocalDockerVerifier{verify: func(_ context.Context, target LocalDockerVerifyRequest) (VerifyResult, error) {
			result := validLocalDockerVerifyResult(target.Request)
			result.Receipt.Schema = "malformed"
			return result, nil
		}}
		r, mgr, _ := newLocalRunnerWithVerifier(t, "script", verifier)
		ref, err := r.CreateSession(context.Background(), createRequest(1, "create-malformed-receipt"))
		if err != nil {
			t.Fatal(err)
		}
		request := localDockerVerifyRequest(ref, "verify-malformed-receipt")
		if _, err := r.VerifySession(localDockerVerifyContext(), request); err == nil || !strings.Contains(err.Error(), "verify receipt") {
			t.Fatalf("malformed receipt error = %v", err)
		}
		if calls := verifier.recordedCalls(); len(calls) != 1 {
			t.Fatalf("malformed receipt verifier calls = %d, want 1", len(calls))
		}
		mgr.mu.Lock()
		defer mgr.mu.Unlock()
		if len(mgr.execCommands) != 0 {
			t.Fatalf("runner used learner exec after malformed receipt: %+v", mgr.execCommands)
		}
	})
}

func TestLocalDockerVerifyIsIdempotentAndConflictsAcrossAllocations(t *testing.T) {
	r, mgr, _ := newLocalRunner(t, "script")
	first, err := r.CreateSession(context.Background(), createRequest(1, "create-1"))
	if err != nil {
		t.Fatal(err)
	}
	request := localDockerVerifyRequest(first, "verify-idempotent")
	if _, err := r.VerifySession(localDockerVerifyContext(), request); err != nil {
		t.Fatalf("first verify: %v", err)
	}
	if _, err := r.VerifySession(localDockerVerifyContext(), request); err != nil {
		t.Fatalf("duplicate verify: %v", err)
	}
	changedProblem := request
	changedProblem.Problem.Revision = "different-revision"
	if _, err := r.VerifySession(localDockerVerifyContext(), changedProblem); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("same-key changed-problem error = %v, want ErrIdempotencyConflict", err)
	}
	changedDeadline := request
	changedDeadline.Deadline = changedDeadline.Deadline.Add(time.Second)
	if _, err := r.VerifySession(localDockerVerifyContext(), changedDeadline); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("same-key changed-deadline error = %v, want ErrIdempotencyConflict", err)
	}
	mgr.mu.Lock()
	if len(mgr.execCommands) != 1 {
		t.Fatalf("duplicate verify executed %d times", len(mgr.execCommands))
	}
	mgr.mu.Unlock()

	second, err := r.CreateSession(context.Background(), createRequest(2, "create-2"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.VerifySession(localDockerVerifyContext(), localDockerVerifyRequest(second, "verify-idempotent")); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("cross-allocation verify key error = %v, want ErrIdempotencyConflict", err)
	}
}

func TestLocalDockerVerifyReplaySurvivesDestroyWithinRetention(t *testing.T) {
	r, mgr, _ := newLocalRunner(t, "script")
	ref, err := r.CreateSession(context.Background(), createRequest(1, "create-verify-replay"))
	if err != nil {
		t.Fatal(err)
	}
	request := localDockerVerifyRequest(ref, "verify-replay")
	first, err := r.VerifySession(localDockerVerifyContext(), request)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.DestroySession(context.Background(), ref); err != nil {
		t.Fatal(err)
	}
	second, err := r.VerifySession(localDockerVerifyContext(), request)
	if err != nil {
		t.Fatalf("verify replay after destroy: %v", err)
	}
	if first != second {
		t.Fatalf("verify replay changed result: first=%+v second=%+v", first, second)
	}
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	if len(mgr.execCommands) != 1 {
		t.Fatalf("verify replay executed verifier %d times", len(mgr.execCommands))
	}
}

func TestLocalDockerCreateReplayRetentionFollowsExactActiveAllocation(t *testing.T) {
	r, mgr, _ := newLocalRunner(t, "script")
	now := time.Unix(1_700_000_000, 0)
	r.now = func() time.Time { return now }
	r.operationTTL = time.Minute
	r.operationLimit = 1

	requestForSession := func(sessionID, key string) CreateSessionRequest {
		req := createRequest(1, key)
		req.Session.SessionID = sessionID
		req.AllocationID = AllocationIDForSession(req.Session)
		return req
	}
	firstRequest := requestForSession("session-retention-one", "create-retention-one")
	first, err := r.CreateSession(context.Background(), firstRequest)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute) // Exceeds TTL.
	if _, err := r.CreateSession(context.Background(), requestForSession("session-retention-two", "create-retention-two")); err != nil {
		t.Fatal(err)
	}
	if replay, err := r.CreateSession(context.Background(), firstRequest); err != nil || replay != first {
		t.Fatalf("active create replay = %+v, %v; want %+v, nil", replay, err, first)
	}

	r.operationTTL = 0 // Isolate capacity pressure from the TTL assertion above.
	if _, err := r.CreateSession(context.Background(), requestForSession("session-retention-three", "create-retention-three")); err != nil {
		t.Fatal(err)
	}
	if replay, err := r.CreateSession(context.Background(), firstRequest); err != nil || replay != first {
		t.Fatalf("capacity-pruned active create replay = %+v, %v; want %+v, nil", replay, err, first)
	}
	mgr.mu.Lock()
	created := len(mgr.created)
	mgr.mu.Unlock()
	if created != 3 {
		t.Fatalf("active create replay executed provider %d times, want 3", created)
	}

	if err := r.DestroySession(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	if _, err := r.CreateSession(context.Background(), requestForSession("session-retention-four", "create-retention-four")); err != nil {
		t.Fatal(err)
	}
	r.mu.RLock()
	_, retained := r.creates[firstRequest.IdempotencyKey]
	r.mu.RUnlock()
	if retained {
		t.Fatal("destroyed allocation's create record survived bounded retention")
	}
}

func TestLocalDockerVerifyReplayRetentionFollowsExactActiveAllocation(t *testing.T) {
	r, _, _ := newLocalRunner(t, "script")
	now := time.Unix(1_700_000_000, 0)
	r.now = func() time.Time { return now }
	r.operationTTL = time.Minute
	r.operationLimit = 1

	ref, err := r.CreateSession(context.Background(), createRequest(1, "create-verify-retention"))
	if err != nil {
		t.Fatal(err)
	}
	firstRequest := localDockerVerifyRequest(ref, "verify-retention-one")
	if _, err := r.VerifySession(localDockerVerifyContext(), firstRequest); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute) // Exceeds TTL.
	if _, err := r.VerifySession(localDockerVerifyContext(), localDockerVerifyRequest(ref, "verify-retention-two")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.VerifySession(localDockerVerifyContext(), firstRequest); err != nil {
		t.Fatalf("active verify replay: %v", err)
	}

	r.operationTTL = 0 // Isolate capacity pressure from the TTL assertion above.
	if _, err := r.VerifySession(localDockerVerifyContext(), localDockerVerifyRequest(ref, "verify-retention-three")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.VerifySession(localDockerVerifyContext(), firstRequest); err != nil {
		t.Fatalf("capacity-pruned active verify replay: %v", err)
	}
	r.mu.RLock()
	verifyCount := len(r.verifies)
	r.mu.RUnlock()
	if verifyCount != 3 {
		t.Fatalf("active verify records=%d, want 3 retained replay keys", verifyCount)
	}

	if err := r.DestroySession(context.Background(), ref); err != nil {
		t.Fatal(err)
	}
	if _, err := r.VerifySession(localDockerVerifyContext(), localDockerVerifyRequest(ref, "verify-after-destroy")); !errors.Is(err, ErrAllocationNotFound) {
		t.Fatalf("verify after destroy error = %v, want ErrAllocationNotFound", err)
	}
	r.mu.RLock()
	_, retained := r.verifies[firstRequest.IdempotencyKey]
	r.mu.RUnlock()
	if retained {
		t.Fatal("destroyed allocation's verify record survived bounded retention")
	}
}

func TestLocalDockerDestroyRetriesAndIsIdempotent(t *testing.T) {
	r, mgr, _ := newLocalRunner(t, "script")
	ref, err := r.CreateSession(context.Background(), createRequest(1, "create-1"))
	if err != nil {
		t.Fatal(err)
	}
	mgr.mu.Lock()
	mgr.removeErr = errors.New("network busy")
	mgr.mu.Unlock()
	if err := r.DestroySession(context.Background(), ref); err == nil {
		t.Fatal("expected first destroy to retain cleanup state")
	}
	if err := r.DestroySession(context.Background(), ref); err != nil {
		t.Fatalf("destroy retry: %v", err)
	}
	if err := r.DestroySession(context.Background(), ref); err != nil {
		t.Fatalf("duplicate destroy must succeed: %v", err)
	}
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	if mgr.removeCalls != 2 {
		t.Fatalf("expected one failed and one successful provider removal, got %d", mgr.removeCalls)
	}
	for _, target := range mgr.removed {
		if target[1] != "k8s-quiz-test-scope-"+ref.ID {
			t.Fatalf("destroy retry lost deterministic network target: %+v", mgr.removed)
		}
	}
}

func TestLocalDockerReconcileIsExplicitAndErrorVisible(t *testing.T) {
	r, mgr, _ := newLocalRunner(t, "script")
	result, err := r.Reconcile(context.Background(), ReconcileRequest{})
	if err != nil || result.Mode != ReconcileModeReportOnly || len(result.Findings) != 1 ||
		result.Findings[0].Kind != ReconcileFindingExpectationsRequired {
		t.Fatalf("zero-value reconcile = %+v, %v", result, err)
	}
	mgr.mu.Lock()
	if mgr.ownedInspectCalls != 0 || mgr.ownedRemoveCalls != 0 {
		t.Fatalf("zero-value reconcile mutated or inspected provider: inspect=%d remove=%d", mgr.ownedInspectCalls, mgr.ownedRemoveCalls)
	}
	mgr.mu.Unlock()

	result, err = r.Reconcile(context.Background(), ReconcileRequest{Apply: true})
	if err != nil || result.Mode != ReconcileModeApply || len(result.Findings) != 1 ||
		result.Findings[0].Kind != ReconcileFindingExpectationsRequired {
		t.Fatalf("empty apply reconcile = %+v, %v", result, err)
	}
	mgr.mu.Lock()
	if mgr.ownedRemoveCalls != 0 {
		t.Fatalf("empty apply mutated provider %d time(s)", mgr.ownedRemoveCalls)
	}
	mgr.mu.Unlock()
}

func TestLocalDockerReconcileExactOwnershipReportAndApply(t *testing.T) {
	r, mgr, _ := newLocalRunner(t, "script")
	request := createRequest(1, "create-reconcile-exact")
	ownership := ReconcileOwnership{
		Provider: ProviderLocalDocker, Scope: "test-scope", AllocationID: request.AllocationID,
		SessionID: request.Session.SessionID, Generation: request.Session.Generation,
	}
	expectation := ReconcileExpectation{
		Ownership: ownership, Desired: ReconcileDesiredAbsent,
		Selection: request.Selection, ResourceProfile: request.ResourceProfile,
	}
	present := containerapi.OwnedAllocationObservation{ContainerPresent: true, NetworkPresent: true, OwnershipComplete: true}
	absent := containerapi.OwnedAllocationObservation{OwnershipComplete: true}
	mgr.mu.Lock()
	mgr.ownedInspections = []containerapi.OwnedAllocationObservation{present}
	mgr.mu.Unlock()

	report, err := r.Reconcile(context.Background(), ReconcileRequest{Expectations: []ReconcileExpectation{expectation}})
	if err != nil || report.Mode != ReconcileModeReportOnly || len(report.Actions) != 1 ||
		report.Actions[0].Kind != ReconcileActionRemove || report.Actions[0].Applied {
		t.Fatalf("report-only exact reconcile = %+v, %v", report, err)
	}
	mgr.mu.Lock()
	if mgr.ownedRemoveCalls != 0 {
		t.Fatalf("report-only exact reconcile removed %d allocation(s)", mgr.ownedRemoveCalls)
	}
	mgr.ownedInspections = []containerapi.OwnedAllocationObservation{present}
	mgr.ownedRemoveResult = containerapi.OwnedAllocationRemoval{Before: present, After: absent}
	mgr.mu.Unlock()

	applied, err := r.Reconcile(context.Background(), ReconcileRequest{
		Mode: ReconcileModeApply, Expectations: []ReconcileExpectation{expectation},
	})
	if err != nil || len(applied.Actions) != 1 || applied.Actions[0].Kind != ReconcileActionDestroyed || !applied.Actions[0].Applied {
		t.Fatalf("apply exact reconcile = %+v, %v", applied, err)
	}
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	if mgr.ownedRemoveCalls != 1 || mgr.ownedInspectCalls != 2 {
		t.Fatalf("exact reconcile inspect/remove calls = %d/%d, want 2/1", mgr.ownedInspectCalls, mgr.ownedRemoveCalls)
	}
	wantLabels := reconcileOwnershipLabels(ownership)
	if len(mgr.ownedLabels) != 3 || !reflect.DeepEqual(mgr.ownedLabels[len(mgr.ownedLabels)-1], wantLabels) {
		t.Fatalf("exact reconcile labels = %+v", mgr.ownedLabels)
	}
}

func TestLocalDockerReconcileAmbiguousOwnershipNeverMutates(t *testing.T) {
	r, mgr, _ := newLocalRunner(t, "script")
	request := createRequest(1, "create-reconcile-ambiguous")
	expectation := ReconcileExpectation{
		Ownership: ReconcileOwnership{
			Provider: ProviderLocalDocker, Scope: "test-scope", AllocationID: request.AllocationID,
			SessionID: request.Session.SessionID, Generation: request.Session.Generation,
		},
		Desired: ReconcileDesiredAbsent, Selection: request.Selection, ResourceProfile: request.ResourceProfile,
	}
	mgr.mu.Lock()
	mgr.ownedInspections = []containerapi.OwnedAllocationObservation{{ContainerPresent: true, OwnershipComplete: false}}
	mgr.mu.Unlock()

	result, err := r.Reconcile(context.Background(), ReconcileRequest{
		Mode: ReconcileModeApply, Expectations: []ReconcileExpectation{expectation},
	})
	if err != nil || len(result.Findings) != 1 || result.Findings[0].Kind != ReconcileFindingAmbiguousOwnership {
		t.Fatalf("ambiguous reconcile = %+v, %v", result, err)
	}
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	if mgr.ownedRemoveCalls != 0 {
		t.Fatalf("ambiguous ownership removed %d allocation(s)", mgr.ownedRemoveCalls)
	}
}

func TestLocalDockerReconcileSecondRuntimePreflightFailureHasZeroProviderEffects(t *testing.T) {
	r, mgr, catalog := newLocalRunner(t, "script")
	firstRequest := createRequest(1, "create-reconcile-preflight-first")
	secondSession := SessionRef{SessionID: "session-2", Generation: 1}
	expectations := []ReconcileExpectation{
		{
			Ownership: ReconcileOwnership{
				Provider: ProviderLocalDocker, Scope: "test-scope", AllocationID: firstRequest.AllocationID,
				SessionID: firstRequest.Session.SessionID, Generation: firstRequest.Session.Generation,
			},
			Desired: ReconcileDesiredAbsent, Selection: firstRequest.Selection,
			ResourceProfile: firstRequest.ResourceProfile,
		},
		{
			Ownership: ReconcileOwnership{
				Provider: ProviderLocalDocker, Scope: "test-scope", AllocationID: AllocationIDForSession(secondSession),
				SessionID: secondSession.SessionID, Generation: secondSession.Generation,
			},
			Desired:         ReconcileDesiredAbsent,
			Selection:       CatalogSelection{Generation: 1, Problem: ProblemRef{ID: "missing-problem", Revision: "missing-revision"}},
			ResourceProfile: DefaultResourceProfile,
		},
	}
	catalog.mu.Lock()
	catalog.resolve = func(ref ProblemRef) (LocalDockerRuntime, error) {
		if ref.ID == "missing-problem" {
			return LocalDockerRuntime{}, errors.New("historical artifact missing")
		}
		return catalog.runtime, nil
	}
	catalog.mu.Unlock()

	result, err := r.Reconcile(context.Background(), ReconcileRequest{
		Mode: ReconcileModeApply, Expectations: expectations,
	})
	if err == nil || len(result.Findings) != 1 || result.Findings[0].Ownership != expectations[1].Ownership {
		t.Fatalf("preflight result = %+v, err=%v", result, err)
	}
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	if mgr.ownedInspectCalls != 0 || mgr.ownedRemoveCalls != 0 {
		t.Fatalf("failed batch preflight reached provider: inspect=%d remove=%d", mgr.ownedInspectCalls, mgr.ownedRemoveCalls)
	}
}
