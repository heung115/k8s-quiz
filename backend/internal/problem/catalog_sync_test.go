package problem

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/k8s-quiz/backend/internal/runner"
	"github.com/k8s-quiz/backend/pkg/models"
)

type memoryCatalogRepository struct {
	mu              sync.Mutex
	leaseMu         sync.Mutex
	active          map[string]models.Problem
	head            *CatalogHead
	publications    map[uint64][]CatalogEntry
	identities      map[CatalogPublicationRef]uint64
	replaceErr      error
	replaceApply    bool
	supersede       bool
	replaces        int
	legacyBindErr   error
	legacyBindApply bool
	legacyBinds     int
}

type commitFailingCatalogActivation struct {
	inner catalogActivation
	err   error
}

func (a *commitFailingCatalogActivation) Commit() error { return a.err }
func (a *commitFailingCatalogActivation) Abort() error  { return a.inner.Abort() }

type blockingArtifactStore struct {
	inner   ArtifactStore
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

type failingArtifactStore struct {
	inner ArtifactStore

	mu          sync.Mutex
	ensureErr   error
	getErr      error
	ensureCalls int
	getCalls    int
}

func (s *blockingArtifactStore) Ensure(ctx context.Context, ref ArtifactRef, data []byte) error {
	s.once.Do(func() { close(s.started) })
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.release:
	}
	return s.inner.Ensure(ctx, ref, data)
}

func (s *blockingArtifactStore) Get(ctx context.Context, ref ArtifactRef) ([]byte, error) {
	return s.inner.Get(ctx, ref)
}

func (s *failingArtifactStore) Ensure(ctx context.Context, ref ArtifactRef, data []byte) error {
	s.mu.Lock()
	s.ensureCalls++
	err := s.ensureErr
	s.mu.Unlock()
	if err != nil {
		return err
	}
	return s.inner.Ensure(ctx, ref, data)
}

func (s *failingArtifactStore) Get(ctx context.Context, ref ArtifactRef) ([]byte, error) {
	s.mu.Lock()
	s.getCalls++
	err := s.getErr
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return s.inner.Get(ctx, ref)
}

func (s *failingArtifactStore) calls() (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ensureCalls, s.getCalls
}

func newMemoryCatalogRepository() *memoryCatalogRepository {
	return &memoryCatalogRepository{
		active:       make(map[string]models.Problem),
		publications: make(map[uint64][]CatalogEntry),
		identities:   make(map[CatalogPublicationRef]uint64),
	}
}

func (r *memoryCatalogRepository) FindActiveCatalogProblem(_ context.Context, id string) (CatalogHead, *models.Problem, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	problem, found := r.active[id]
	if !found || r.head == nil {
		return CatalogHead{}, nil, errors.New("problem not active")
	}
	copy := cloneProblem(problem)
	return *r.head, &copy, nil
}

func (r *memoryCatalogRepository) FindCatalogArtifact(_ context.Context, ref runner.ProblemRef) (CatalogEntry, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for generation := uint64(1); generation <= uint64(len(r.publications)); generation++ {
		for _, entry := range r.publications[generation] {
			if entry.Problem.ID == ref.ID && entry.Problem.Revision == ref.Revision {
				return cloneCatalogEntry(entry), nil
			}
		}
	}
	return CatalogEntry{}, runner.ErrInvalidRevision
}

func (r *memoryCatalogRepository) AcquireCatalogPublicationLease(context.Context) (catalogPublicationLease, error) {
	r.leaseMu.Lock()
	return &memoryCatalogPublicationLease{repo: r}, nil
}

type memoryCatalogPublicationLease struct {
	repo      *memoryCatalogRepository
	closeOnce sync.Once
}

func (l *memoryCatalogPublicationLease) Close(context.Context) error {
	l.closeOnce.Do(func() { l.repo.leaseMu.Unlock() })
	return nil
}

func (l *memoryCatalogPublicationLease) Discard(context.Context) error {
	return l.Close(context.Background())
}

func (l *memoryCatalogPublicationLease) Head(context.Context) (*CatalogHead, error) {
	l.repo.mu.Lock()
	defer l.repo.mu.Unlock()
	if l.repo.head == nil {
		return nil, nil
	}
	head := *l.repo.head
	return &head, nil
}

func (l *memoryCatalogPublicationLease) ReadPublication(_ context.Context, generation uint64) (CatalogHead, []CatalogEntry, error) {
	l.repo.mu.Lock()
	defer l.repo.mu.Unlock()
	entries, found := l.repo.publications[generation]
	if !found {
		return CatalogHead{}, nil, ErrCatalogPublicationMissing
	}
	var ref CatalogPublicationRef
	for candidate, storedGeneration := range l.repo.identities {
		if storedGeneration == generation {
			ref = candidate
			break
		}
	}
	for _, entry := range entries {
		if validateArtifactRef(entry.Artifact) != nil || entry.SourceTrust != BundleSourceDevelopmentCheckout {
			return CatalogHead{}, nil, ErrCatalogArtifactBindingMissing
		}
	}
	return CatalogHead{Generation: generation, Ref: ref, EntryCount: len(entries)}, cloneCatalogEntries(entries), nil
}

func (l *memoryCatalogPublicationLease) ReadLegacyPublication(_ context.Context, generation uint64) (CatalogHead, []models.Problem, error) {
	l.repo.mu.Lock()
	defer l.repo.mu.Unlock()
	entries, found := l.repo.publications[generation]
	if !found {
		return CatalogHead{}, nil, ErrCatalogPublicationMissing
	}
	var ref CatalogPublicationRef
	for candidate, storedGeneration := range l.repo.identities {
		if storedGeneration == generation {
			ref = candidate
			break
		}
	}
	return CatalogHead{Generation: generation, Ref: ref, EntryCount: len(entries)}, catalogProblems(entries), nil
}

func (l *memoryCatalogPublicationLease) BindLegacyArtifacts(_ context.Context, expected CatalogHead, entries []CatalogEntry) error {
	l.repo.mu.Lock()
	defer l.repo.mu.Unlock()
	l.repo.legacyBinds++
	bindErr := l.repo.legacyBindErr
	l.repo.legacyBindErr = nil
	if bindErr != nil && !l.repo.legacyBindApply {
		return bindErr
	}
	if !sameCatalogHead(l.repo.head, &expected) {
		return ErrCatalogHeadConflict
	}
	stored, found := l.repo.publications[expected.Generation]
	if !found || !sameCatalogProjection(catalogProblems(stored), catalogProblems(entries)) {
		return ErrCatalogStartupMismatch
	}
	l.repo.publications[expected.Generation] = cloneCatalogEntries(entries)
	return bindErr
}

func (l *memoryCatalogPublicationLease) FindPublication(_ context.Context, ref CatalogPublicationRef) (*CatalogHead, error) {
	l.repo.mu.Lock()
	defer l.repo.mu.Unlock()
	generation, found := l.repo.identities[ref]
	if !found {
		return nil, nil
	}
	head := CatalogHead{Generation: generation, Ref: ref, EntryCount: len(l.repo.publications[generation])}
	return &head, nil
}

func (l *memoryCatalogPublicationLease) Publish(_ context.Context, expected *CatalogHead, ref CatalogPublicationRef, entries []CatalogEntry) (CatalogHead, error) {
	r := l.repo
	r.mu.Lock()
	defer r.mu.Unlock()
	r.replaces++
	if r.replaceErr != nil && !r.replaceApply {
		return CatalogHead{}, r.replaceErr
	}
	if !sameCatalogHead(r.head, expected) {
		return CatalogHead{}, ErrCatalogHeadConflict
	}
	if _, found := r.identities[ref]; found {
		return CatalogHead{}, ErrCatalogRollback
	}
	generation := uint64(1)
	if expected != nil {
		generation = expected.Generation + 1
	}
	next := make(map[string]models.Problem, len(entries))
	for _, entry := range entries {
		problem := entry.Problem
		problem.CatalogActive = true
		next[problem.ID] = cloneProblem(problem)
	}
	r.active = next
	r.publications[generation] = cloneCatalogEntries(entries)
	r.identities[ref] = generation
	head := CatalogHead{Generation: generation, Ref: ref, EntryCount: len(entries)}
	r.head = &head
	if r.replaceErr != nil {
		if r.supersede {
			supersedingRef := ref
			supersedingRef.CandidateDigest[0] ^= 0xff
			supersedingGeneration := generation + 1
			r.publications[supersedingGeneration] = cloneCatalogEntries(entries)
			r.identities[supersedingRef] = supersedingGeneration
			supersedingHead := CatalogHead{
				Generation: supersedingGeneration,
				Ref:        supersedingRef,
				EntryCount: len(entries),
			}
			r.head = &supersedingHead
		}
		return CatalogHead{}, r.replaceErr
	}
	return head, nil
}

func cloneCatalogProblems(problems []models.Problem) []models.Problem {
	result := make([]models.Problem, len(problems))
	for i := range problems {
		result[i] = cloneProblem(problems[i])
	}
	return result
}

func cloneCatalogEntry(entry CatalogEntry) CatalogEntry {
	entry.Problem = cloneProblem(entry.Problem)
	return entry
}

func cloneCatalogEntries(entries []CatalogEntry) []CatalogEntry {
	result := make([]CatalogEntry, len(entries))
	for i := range entries {
		result[i] = cloneCatalogEntry(entries[i])
	}
	return result
}

func TestCatalogCoordinatorStartupBootstrapsThenAdoptsWithoutPublishing(t *testing.T) {
	dir := setupTestProblems(t)
	repo := newMemoryCatalogRepository()

	firstLoader := newSnapshotRuntimeLoader(t, dir)
	first, err := NewCatalogCoordinator(firstLoader, repo)
	if err != nil {
		t.Fatal(err)
	}
	firstCount, err := first.Startup(context.Background())
	if err != nil {
		t.Fatalf("bootstrap startup: %v", err)
	}
	if firstCount == 0 {
		t.Fatal("bootstrap published an empty catalog")
	}
	repo.mu.Lock()
	bootstrapWrites := repo.replaces
	bootstrapHead := *repo.head
	repo.mu.Unlock()
	if bootstrapWrites != 1 || bootstrapHead.Generation != 1 || bootstrapHead.EntryCount != firstCount {
		t.Fatalf("bootstrap writes=%d head=%+v count=%d", bootstrapWrites, bootstrapHead, firstCount)
	}

	// A process restart builds a fresh in-memory loader but must adopt the
	// persisted head without appending another publication.
	secondLoader := newSnapshotRuntimeLoader(t, dir)
	second, err := NewCatalogCoordinator(secondLoader, repo)
	if err != nil {
		t.Fatal(err)
	}
	secondCount, err := second.Startup(context.Background())
	if err != nil {
		t.Fatalf("adopt startup: %v", err)
	}
	if secondCount != firstCount {
		t.Fatalf("adopt count=%d want=%d", secondCount, firstCount)
	}
	repo.mu.Lock()
	adoptWrites := repo.replaces
	adoptHead := *repo.head
	repo.mu.Unlock()
	if adoptWrites != bootstrapWrites || adoptHead != bootstrapHead {
		t.Fatalf("startup adoption mutated ledger: writes=%d/%d head=%+v/%+v", adoptWrites, bootstrapWrites, adoptHead, bootstrapHead)
	}
	if got := secondLoader.catalogHead(); got == nil || *got != bootstrapHead {
		t.Fatalf("adopted loader head=%+v want=%+v", got, bootstrapHead)
	}
	_, selection, release, err := second.AcquireActiveProblem(context.Background(), "test-problem")
	if err != nil {
		t.Fatalf("adopted catalog admission: %v", err)
	}
	release()
	if selection.Generation != bootstrapHead.Generation {
		t.Fatalf("adopted selection generation=%d want=%d", selection.Generation, bootstrapHead.Generation)
	}
}

func TestCatalogCoordinatorBootstrapPreparesCASBeforeFencesAndAdoptsConcurrentWinner(t *testing.T) {
	dir := setupTestProblems(t)
	repo := newMemoryCatalogRepository()

	root := filepath.Join(canonicalTempDir(t), "blocked-artifact-store")
	filesystemStore, err := NewFilesystemArtifactStore(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = filesystemStore.Close() })
	blockedStore := &blockingArtifactStore{
		inner:   filesystemStore,
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	blockedLoader, err := NewPersistentRuntimeGitLoader(dir, &mutableImageResolver{id: imageID('a')}, blockedStore)
	if err != nil {
		t.Fatal(err)
	}
	blockedCoordinator, err := NewCatalogCoordinator(blockedLoader, repo)
	if err != nil {
		t.Fatal(err)
	}

	blockedResult := make(chan error, 1)
	go func() {
		_, err := blockedCoordinator.Startup(context.Background())
		blockedResult <- err
	}()
	select {
	case <-blockedStore.started:
	case <-time.After(time.Second):
		t.Fatal("bootstrap did not reach CAS preparation")
	}

	// CAS preparation must not hold this process's admission fence.
	admissionCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := blockedCoordinator.admissionGate.Acquire(admissionCtx, catalogAdmissionCapacity); err != nil {
		t.Fatalf("CAS preparation held admission fence: %v", err)
	}
	blockedCoordinator.admissionGate.Release(catalogAdmissionCapacity)

	// It must not hold the global publication lease either. A second process
	// can win bootstrap while the first process is still writing its CAS.
	winnerLoader := newSnapshotRuntimeLoader(t, dir)
	winner, err := NewCatalogCoordinator(winnerLoader, repo)
	if err != nil {
		t.Fatal(err)
	}
	winnerCount, err := winner.Startup(context.Background())
	if err != nil {
		t.Fatalf("concurrent winner bootstrap: %v", err)
	}
	close(blockedStore.release)
	select {
	case err := <-blockedResult:
		if err != nil {
			t.Fatalf("bootstrap loser did not adopt winner: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("bootstrap loser did not finish after CAS release")
	}

	repo.mu.Lock()
	writes := repo.replaces
	head := *repo.head
	repo.mu.Unlock()
	if writes != 1 || head.Generation != 1 || head.EntryCount != winnerCount {
		t.Fatalf("bootstrap race writes=%d head=%+v winner_count=%d", writes, head, winnerCount)
	}
	if adopted := blockedLoader.catalogHead(); adopted == nil || *adopted != head {
		t.Fatalf("bootstrap loser head=%+v want=%+v", adopted, head)
	}
}

func TestCatalogCoordinatorLegacyBindingReconcilesAmbiguousCommit(t *testing.T) {
	for _, test := range []struct {
		name        string
		applyBefore bool
		wantBinds   int
	}{
		{name: "commit applied response lost", applyBefore: true, wantBinds: 1},
		{name: "commit not applied response lost", applyBefore: false, wantBinds: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := setupTestProblems(t)
			loader := newSnapshotRuntimeLoader(t, dir)
			candidate, err := loader.BuildCandidate(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			legacyRef, err := candidate.legacyIdentity()
			if err != nil {
				t.Fatal(err)
			}
			legacyEntries := candidate.Entries()
			active := make(map[string]models.Problem, len(legacyEntries))
			for i := range legacyEntries {
				legacyEntries[i].Artifact = ArtifactRef{}
				legacyEntries[i].SourceTrust = ""
				problem := cloneProblem(legacyEntries[i].Problem)
				problem.CatalogActive = true
				active[problem.ID] = problem
			}
			repo := newMemoryCatalogRepository()
			head := CatalogHead{Generation: 1, Ref: legacyRef, EntryCount: len(legacyEntries)}
			repo.head = &head
			repo.active = active
			repo.publications[1] = cloneCatalogEntries(legacyEntries)
			repo.identities[legacyRef] = 1
			repo.legacyBindErr = ErrCatalogCommitUnknown
			repo.legacyBindApply = test.applyBefore

			coordinator, err := NewCatalogCoordinator(loader, repo)
			if err != nil {
				t.Fatal(err)
			}
			count, err := coordinator.Startup(context.Background())
			if err != nil {
				t.Fatalf("reconcile legacy binding: %v", err)
			}
			if count != len(legacyEntries) || !coordinator.Healthy() {
				t.Fatalf("legacy startup count=%d healthy=%v", count, coordinator.Healthy())
			}
			repo.mu.Lock()
			binds := repo.legacyBinds
			bound := cloneCatalogEntries(repo.publications[1])
			repo.mu.Unlock()
			if binds != test.wantBinds || !sameCatalogEntries(bound, candidate.Entries()) {
				t.Fatalf("legacy binds=%d want=%d entries_match=%v", binds, test.wantBinds, sameCatalogEntries(bound, candidate.Entries()))
			}
			if got := loader.catalogHead(); got == nil || *got != head {
				t.Fatalf("legacy reconciled loader head=%+v want=%+v", got, head)
			}
		})
	}
}

func TestCatalogCoordinatorRestartAdoptsCurrentHeadWithoutCheckout(t *testing.T) {
	dir := setupTestProblems(t)
	repo := newMemoryCatalogRepository()
	firstLoader := newSnapshotRuntimeLoader(t, dir)
	first, err := NewCatalogCoordinator(firstLoader, repo)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Startup(context.Background()); err != nil {
		t.Fatalf("bootstrap catalog: %v", err)
	}
	repo.mu.Lock()
	head := *repo.head
	entries := cloneCatalogEntries(repo.publications[head.Generation])
	repo.mu.Unlock()

	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("remove mutable checkout: %v", err)
	}
	restartedLoader := newSnapshotRuntimeLoader(t, dir)
	restarted, err := NewCatalogCoordinator(restartedLoader, repo)
	if err != nil {
		t.Fatal(err)
	}
	if count, err := restarted.Startup(context.Background()); err != nil || count != len(entries) {
		t.Fatalf("checkout-free startup count=%d err=%v", count, err)
	}
	for _, entry := range entries {
		ref := runner.ProblemRef{ID: entry.Problem.ID, Revision: entry.Problem.Revision}
		problem, err := restarted.ResolveProblem(context.Background(), ref)
		if err != nil || problem.Title != entry.Problem.Title {
			t.Fatalf("resolve checkout-free problem %+v: problem=%+v err=%v", ref, problem, err)
		}
		runtime, err := restarted.ResolveRuntime(context.Background(), ref)
		if err != nil || runtime.Revision != ref.Revision || runtime.Image != imageID('a') {
			t.Fatalf("resolve checkout-free runtime %+v: runtime=%+v err=%v", ref, runtime, err)
		}
	}
}

func TestCatalogCoordinatorRestartResolvesRetiredRevisionFromCAS(t *testing.T) {
	dir := setupTestProblems(t)
	repo := newMemoryCatalogRepository()
	loader := newSnapshotRuntimeLoader(t, dir)
	coordinator, err := NewCatalogCoordinator(loader, repo)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.Startup(context.Background()); err != nil {
		t.Fatal(err)
	}
	oldProblem, _, release, err := coordinator.AcquireActiveProblem(context.Background(), "test-problem")
	if err != nil {
		t.Fatal(err)
	}
	release()
	oldRef := runner.ProblemRef{ID: oldProblem.ID, Revision: oldProblem.Revision}
	oldRuntime, err := coordinator.ResolveRuntime(context.Background(), oldRef)
	if err != nil {
		t.Fatal(err)
	}

	mutateSnapshotProblem(t, dir)
	if _, err := coordinator.Sync(context.Background()); err != nil {
		t.Fatalf("publish generation B: %v", err)
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	restartedLoader := newSnapshotRuntimeLoader(t, dir)
	restarted, err := NewCatalogCoordinator(restartedLoader, repo)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.Startup(context.Background()); err != nil {
		t.Fatalf("restart on generation B: %v", err)
	}

	resolvedProblem, err := restarted.ResolveProblem(context.Background(), oldRef)
	if err != nil {
		t.Fatalf("resolve retired problem A: %v", err)
	}
	resolvedRuntime, err := restarted.ResolveRuntime(context.Background(), oldRef)
	if err != nil {
		t.Fatalf("resolve retired runtime A: %v", err)
	}
	if resolvedProblem.Title != oldProblem.Title || resolvedProblem.Title == "Changed Problem" {
		t.Fatalf("retired problem A changed after restart: %+v", resolvedProblem)
	}
	assertSameRuntimeSnapshot(t, resolvedRuntime, oldRuntime)
	if _, found := restartedLoader.resolveCatalogBundle(oldRef); !found {
		t.Fatal("historical CAS resolution was not installed in the process cache")
	}
}

func TestCatalogCoordinatorRestartRejectsMissingCurrentArtifact(t *testing.T) {
	dir := setupTestProblems(t)
	repo := newMemoryCatalogRepository()
	loader := newSnapshotRuntimeLoader(t, dir)
	coordinator, err := NewCatalogCoordinator(loader, repo)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.Startup(context.Background()); err != nil {
		t.Fatal(err)
	}
	repo.mu.Lock()
	head := *repo.head
	missing := repo.publications[head.Generation][0].Artifact
	repo.mu.Unlock()
	store := loader.artifactStore.(*FilesystemArtifactStore)
	if err := os.Remove(testArtifactPath(store.root, missing)); err != nil {
		t.Fatalf("remove current artifact: %v", err)
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}

	restartedLoader := newSnapshotRuntimeLoader(t, dir)
	restarted, err := NewCatalogCoordinator(restartedLoader, repo)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.Startup(context.Background()); !errors.Is(err, ErrArtifactNotFound) {
		t.Fatalf("missing artifact startup error=%v want ErrArtifactNotFound", err)
	}
	if head := restartedLoader.catalogHead(); head != nil {
		t.Fatalf("missing artifact activated catalog head %+v", head)
	}
}

func TestCatalogCoordinatorRestartRejectsSameSizeCorruptCurrentArtifact(t *testing.T) {
	dir := setupTestProblems(t)
	repo := newMemoryCatalogRepository()
	loader := newSnapshotRuntimeLoader(t, dir)
	coordinator, err := NewCatalogCoordinator(loader, repo)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.Startup(context.Background()); err != nil {
		t.Fatalf("bootstrap catalog: %v", err)
	}
	repo.mu.Lock()
	head := *repo.head
	corrupt := repo.publications[head.Generation][0].Artifact
	repo.mu.Unlock()

	store := loader.artifactStore.(*FilesystemArtifactStore)
	objectPath := testArtifactPath(store.root, corrupt)
	data, err := os.ReadFile(objectPath)
	if err != nil {
		t.Fatalf("read current artifact: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("current artifact is unexpectedly empty")
	}
	data[len(data)/2] ^= 0xff
	if err := os.WriteFile(objectPath, data, artifactStoreFileMode); err != nil {
		t.Fatalf("corrupt current artifact in place: %v", err)
	}
	info, err := os.Stat(objectPath)
	if err != nil {
		t.Fatalf("stat corrupted current artifact: %v", err)
	}
	if info.Size() != corrupt.Size {
		t.Fatalf("corrupted current artifact size=%d want=%d", info.Size(), corrupt.Size)
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("remove mutable checkout: %v", err)
	}

	restartedLoader := newSnapshotRuntimeLoader(t, dir)
	restarted, err := NewCatalogCoordinator(restartedLoader, repo)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.Startup(context.Background()); !errors.Is(err, ErrArtifactIntegrity) {
		t.Fatalf("corrupt artifact startup error=%v want ErrArtifactIntegrity", err)
	}
	if got := restartedLoader.catalogHead(); got != nil {
		t.Fatalf("corrupt artifact activated catalog head %+v", got)
	}
}

func TestCatalogCoordinatorLiveArtifactCorruptionPoisonsFreshAdmission(t *testing.T) {
	dir := setupTestProblems(t)
	repo := newMemoryCatalogRepository()
	loader := newSnapshotRuntimeLoader(t, dir)
	coordinator, err := NewCatalogCoordinator(loader, repo)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.Startup(context.Background()); err != nil {
		t.Fatal(err)
	}

	active, _, release, err := coordinator.AcquireActiveProblem(context.Background(), "test-problem")
	if err != nil {
		t.Fatalf("initial active admission: %v", err)
	}
	release()
	repo.mu.Lock()
	var entry CatalogEntry
	for _, candidate := range repo.publications[repo.head.Generation] {
		if candidate.Problem.ID == active.ID {
			entry = cloneCatalogEntry(candidate)
			break
		}
	}
	repo.mu.Unlock()
	if entry.Problem.ID == "" {
		t.Fatal("active artifact binding was not found")
	}
	store := loader.artifactStore.(*FilesystemArtifactStore)
	objectPath := testArtifactPath(store.root, entry.Artifact)
	data, err := os.ReadFile(objectPath)
	if err != nil {
		t.Fatalf("read active artifact: %v", err)
	}
	data[len(data)/2] ^= 0xff
	if err := os.WriteFile(objectPath, data, artifactStoreFileMode); err != nil {
		t.Fatalf("corrupt active artifact: %v", err)
	}

	if _, _, release, err := coordinator.AcquireActiveProblem(context.Background(), active.ID); !errors.Is(err, ErrCatalogAdmissionUnavailable) || !errors.Is(err, ErrArtifactIntegrity) {
		if release != nil {
			release()
		}
		t.Fatalf("fresh admission after live corruption error=%v, want admission and integrity errors", err)
	}
	if coordinator.Healthy() {
		t.Fatal("live artifact corruption did not close readiness")
	}
	if _, err := coordinator.ResolveProblem(context.Background(), runner.ProblemRef{ID: active.ID, Revision: active.Revision}); err != nil {
		t.Fatalf("live corruption blocked existing immutable in-memory revision: %v", err)
	}
}

func TestCatalogCoordinatorRepeatedStartupRevalidatesActiveArtifacts(t *testing.T) {
	dir := setupTestProblems(t)
	repo := newMemoryCatalogRepository()
	loader := newSnapshotRuntimeLoader(t, dir)
	coordinator, err := NewCatalogCoordinator(loader, repo)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.Startup(context.Background()); err != nil {
		t.Fatal(err)
	}
	repo.mu.Lock()
	entry := cloneCatalogEntry(repo.publications[repo.head.Generation][0])
	repo.mu.Unlock()
	store := loader.artifactStore.(*FilesystemArtifactStore)
	if err := os.Remove(testArtifactPath(store.root, entry.Artifact)); err != nil {
		t.Fatalf("remove active artifact: %v", err)
	}

	if _, err := coordinator.Startup(context.Background()); !errors.Is(err, ErrCatalogAdmissionUnavailable) || !errors.Is(err, ErrArtifactNotFound) {
		t.Fatalf("repeated startup after artifact loss error=%v, want admission and not-found errors", err)
	}
	if coordinator.Healthy() {
		t.Fatal("repeated startup artifact loss did not close readiness")
	}
}

func TestCatalogCoordinatorArtifactPreparationFailureDoesNotPublishOrActivate(t *testing.T) {
	for _, test := range []struct {
		name        string
		ensureErr   error
		getErr      error
		wantEnsures int
		wantGets    int
	}{
		{
			name:        "ensure failure",
			ensureErr:   artifactStoreError(ArtifactStoreErrorUnavailable, "injected ensure", errors.New("ensure failed")),
			wantEnsures: 1,
		},
		{
			name:        "read-back failure",
			getErr:      artifactStoreError(ArtifactStoreErrorUnavailable, "injected get", errors.New("get failed")),
			wantEnsures: 1,
			wantGets:    1,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := setupTestProblems(t)
			filesystemStore, err := NewFilesystemArtifactStore(filepath.Join(canonicalTempDir(t), "artifacts"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = filesystemStore.Close() })
			store := &failingArtifactStore{
				inner:     filesystemStore,
				ensureErr: test.ensureErr,
				getErr:    test.getErr,
			}
			loader, err := NewPersistentRuntimeGitLoader(dir, &mutableImageResolver{id: imageID('a')}, store)
			if err != nil {
				t.Fatal(err)
			}
			repo := newMemoryCatalogRepository()
			coordinator, err := NewCatalogCoordinator(loader, repo)
			if err != nil {
				t.Fatal(err)
			}

			if _, err := coordinator.Sync(context.Background()); !errors.Is(err, ErrArtifactUnavailable) {
				t.Fatalf("artifact preparation error=%v want ErrArtifactUnavailable", err)
			}
			ensures, gets := store.calls()
			if ensures != test.wantEnsures || gets != test.wantGets {
				t.Fatalf("artifact calls ensure=%d/%d get=%d/%d", ensures, test.wantEnsures, gets, test.wantGets)
			}
			repo.mu.Lock()
			replaces := repo.replaces
			head := repo.head
			publicationCount := len(repo.publications)
			identityCount := len(repo.identities)
			activeCount := len(repo.active)
			repo.mu.Unlock()
			if replaces != 0 || head != nil || publicationCount != 0 || identityCount != 0 || activeCount != 0 {
				t.Fatalf("artifact failure mutated publication: writes=%d head=%+v publications=%d identities=%d active=%d",
					replaces, head, publicationCount, identityCount, activeCount)
			}
			if got := loader.catalogHead(); got != nil {
				t.Fatalf("artifact failure activated loader head %+v", got)
			}
			if !coordinator.Healthy() {
				t.Fatal("pre-publication artifact failure poisoned catalog admission")
			}
		})
	}
}

func TestCatalogCoordinatorKnownPublishFailureLeavesReusableUnreferencedArtifacts(t *testing.T) {
	dir := setupTestProblems(t)
	store, err := NewFilesystemArtifactStore(filepath.Join(canonicalTempDir(t), "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	loader, err := NewPersistentRuntimeGitLoader(dir, &mutableImageResolver{id: imageID('a')}, store)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := loader.BuildCandidate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	wantEntries := candidate.Entries()
	wantIdentity, err := candidate.Identity()
	if err != nil {
		t.Fatal(err)
	}

	repo := newMemoryCatalogRepository()
	publishErr := errors.New("known transaction rollback")
	repo.replaceErr = publishErr
	coordinator, err := NewCatalogCoordinator(loader, repo)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.Sync(context.Background()); !errors.Is(err, publishErr) {
		t.Fatalf("known publish failure error=%v want=%v", err, publishErr)
	}

	repo.mu.Lock()
	failedWrites := repo.replaces
	failedHead := repo.head
	failedPublications := len(repo.publications)
	failedIdentities := len(repo.identities)
	failedActive := len(repo.active)
	repo.mu.Unlock()
	if failedWrites != 1 || failedHead != nil || failedPublications != 0 || failedIdentities != 0 || failedActive != 0 {
		t.Fatalf("known failure state writes=%d head=%+v publications=%d identities=%d active=%d",
			failedWrites, failedHead, failedPublications, failedIdentities, failedActive)
	}
	if got := loader.catalogHead(); got != nil {
		t.Fatalf("known publish failure activated loader head %+v", got)
	}
	if !coordinator.Healthy() {
		t.Fatal("known publish rollback poisoned catalog admission")
	}
	for _, entry := range wantEntries {
		data, err := store.Get(context.Background(), entry.Artifact)
		if err != nil {
			t.Fatalf("read unreferenced artifact %q: %v", entry.Problem.ID, err)
		}
		_, bundle, err := DecodeRuntimeArtifact(entry.Artifact, data)
		if err != nil {
			t.Fatalf("decode unreferenced artifact %q: %v", entry.Problem.ID, err)
		}
		if bundle.Problem.ID != entry.Problem.ID || bundle.Revision != entry.Problem.Revision {
			t.Fatalf("unreferenced artifact %q decoded as problem=%q revision=%q",
				entry.Problem.ID, bundle.Problem.ID, bundle.Revision)
		}
		if _, err := repo.FindCatalogArtifact(context.Background(), runner.ProblemRef{
			ID: entry.Problem.ID, Revision: entry.Problem.Revision,
		}); !errors.Is(err, runner.ErrInvalidRevision) {
			t.Fatalf("failed publication referenced artifact %q: %v", entry.Problem.ID, err)
		}
	}

	repo.mu.Lock()
	repo.replaceErr = nil
	repo.mu.Unlock()
	count, err := coordinator.Sync(context.Background())
	if err != nil {
		t.Fatalf("retry known publish failure: %v", err)
	}
	repo.mu.Lock()
	publishedHead := *repo.head
	publishedEntries := cloneCatalogEntries(repo.publications[publishedHead.Generation])
	publishedWrites := repo.replaces
	repo.mu.Unlock()
	wantHead := CatalogHead{Generation: 1, Ref: wantIdentity, EntryCount: len(wantEntries)}
	if count != len(wantEntries) || publishedHead != wantHead || publishedWrites != 2 || !sameCatalogEntries(publishedEntries, wantEntries) {
		t.Fatalf("retry publication count=%d/%d head=%+v/%+v writes=%d entries_match=%v",
			count, len(wantEntries), publishedHead, wantHead, publishedWrites, sameCatalogEntries(publishedEntries, wantEntries))
	}
	if got := loader.catalogHead(); got == nil || *got != wantHead {
		t.Fatalf("retry activated loader head=%+v want=%+v", got, wantHead)
	}
}

func TestCatalogCoordinatorStartupIgnoresChangedCheckoutAndAdoptsCAS(t *testing.T) {
	dir := setupTestProblems(t)
	repo := newMemoryCatalogRepository()
	firstLoader := newSnapshotRuntimeLoader(t, dir)
	first, err := NewCatalogCoordinator(firstLoader, repo)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Startup(context.Background()); err != nil {
		t.Fatal(err)
	}
	repo.mu.Lock()
	wantWrites := repo.replaces
	wantHead := *repo.head
	repo.mu.Unlock()

	mutateSnapshotProblem(t, dir)
	mismatchedLoader := newSnapshotRuntimeLoader(t, dir)
	mismatched, err := NewCatalogCoordinator(mismatchedLoader, repo)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mismatched.Startup(context.Background()); err != nil {
		t.Fatalf("CAS startup with changed checkout: %v", err)
	}
	repo.mu.Lock()
	gotWrites := repo.replaces
	gotHead := *repo.head
	repo.mu.Unlock()
	if gotWrites != wantWrites || gotHead != wantHead {
		t.Fatalf("mismatched startup mutated ledger: writes=%d/%d head=%+v/%+v", gotWrites, wantWrites, gotHead, wantHead)
	}
	if got := mismatchedLoader.catalogHead(); got == nil || *got != wantHead {
		t.Fatalf("CAS startup activated head=%+v want=%+v", got, wantHead)
	}
	problem, err := mismatched.ResolveProblem(context.Background(), runner.ProblemRef{
		ID: "test-problem", Revision: repo.publications[wantHead.Generation][1].Problem.Revision,
	})
	if err != nil {
		t.Fatalf("resolve CAS-adopted problem: %v", err)
	}
	if problem.Title == "Changed Problem" {
		t.Fatal("startup adopted the changed checkout instead of the persisted CAS head")
	}
}

func TestCatalogCoordinatorStartupProjectionMismatchDoesNotActivate(t *testing.T) {
	dir := setupTestProblems(t)
	repo := newMemoryCatalogRepository()
	firstLoader := newSnapshotRuntimeLoader(t, dir)
	first, err := NewCatalogCoordinator(firstLoader, repo)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Startup(context.Background()); err != nil {
		t.Fatal(err)
	}
	repo.mu.Lock()
	repo.publications[1][0].Problem.Title = "tampered persisted projection"
	wantWrites := repo.replaces
	repo.mu.Unlock()

	loader := newSnapshotRuntimeLoader(t, dir)
	coordinator, err := NewCatalogCoordinator(loader, repo)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.Startup(context.Background()); !errors.Is(err, ErrCatalogStartupMismatch) {
		t.Fatalf("projection mismatch error=%v want ErrCatalogStartupMismatch", err)
	}
	repo.mu.Lock()
	gotWrites := repo.replaces
	repo.mu.Unlock()
	if gotWrites != wantWrites {
		t.Fatalf("projection mismatch mutated ledger: writes=%d want=%d", gotWrites, wantWrites)
	}
	if got := loader.catalogHead(); got != nil {
		t.Fatalf("projection mismatch activated loader head %+v", got)
	}
}

func TestCatalogCoordinatorStartupActivationFailureReleasesLoaderLock(t *testing.T) {
	dir := setupTestProblems(t)
	repo := newMemoryCatalogRepository()
	firstLoader := newSnapshotRuntimeLoader(t, dir)
	first, err := NewCatalogCoordinator(firstLoader, repo)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Startup(context.Background()); err != nil {
		t.Fatal(err)
	}

	loader := newSnapshotRuntimeLoader(t, dir)
	coordinator, err := NewCatalogCoordinator(loader, repo)
	if err != nil {
		t.Fatal(err)
	}
	prepare := coordinator.prepareActivation
	commitErr := errors.New("injected startup activation failure")
	coordinator.prepareActivation = func(candidate *CatalogCandidate, target CatalogHead) (catalogActivation, error) {
		activation, err := prepare(candidate, target)
		if err != nil {
			return nil, err
		}
		return &commitFailingCatalogActivation{inner: activation, err: commitErr}, nil
	}
	if _, err := coordinator.Startup(context.Background()); !errors.Is(err, ErrCatalogAdmissionUnavailable) {
		t.Fatalf("startup activation error=%v want ErrCatalogAdmissionUnavailable", err)
	}
	if coordinator.Healthy() {
		t.Fatal("startup activation failure did not poison admission")
	}

	candidate, err := loader.BuildCandidate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ref := mustCatalogIdentity(t, candidate)
	target := CatalogHead{Generation: 1, Ref: ref, EntryCount: len(candidate.Problems())}
	lockResult := make(chan error, 1)
	go func() {
		activation, err := loader.prepareActivationAt(candidate, target)
		if err == nil {
			err = activation.Abort()
		}
		lockResult <- err
	}()
	select {
	case err := <-lockResult:
		if err != nil {
			t.Fatalf("startup activation left publication lock unusable: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("startup activation failure left publication lock held")
	}
}

func TestCatalogCoordinatorSyncIsIdempotentAndRejectsPublishedRollback(t *testing.T) {
	dirA := setupTestProblems(t)
	repo := newMemoryCatalogRepository()
	loaderA := newSnapshotRuntimeLoader(t, dirA)
	coordinatorA, err := NewCatalogCoordinator(loaderA, repo)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coordinatorA.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	repo.mu.Lock()
	firstWrites := repo.replaces
	repo.mu.Unlock()
	if _, err := coordinatorA.Sync(context.Background()); err != nil {
		t.Fatalf("unchanged administrative sync: %v", err)
	}
	repo.mu.Lock()
	unchangedWrites := repo.replaces
	repo.mu.Unlock()
	if unchangedWrites != firstWrites {
		t.Fatalf("unchanged sync appended a publication: writes=%d want=%d", unchangedWrites, firstWrites)
	}

	mutateSnapshotProblem(t, dirA)
	if _, err := coordinatorA.Sync(context.Background()); err != nil {
		t.Fatalf("publish second generation: %v", err)
	}
	repo.mu.Lock()
	headAfterSecond := *repo.head
	writesAfterSecond := repo.replaces
	repo.mu.Unlock()
	if headAfterSecond.Generation != 2 {
		t.Fatalf("second publication head=%+v", headAfterSecond)
	}

	// A new process using the original checkout must not republish generation
	// one as generation three.
	dirOriginal := setupTestProblems(t)
	rollbackLoader := newSnapshotRuntimeLoader(t, dirOriginal)
	rollback, err := NewCatalogCoordinator(rollbackLoader, repo)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rollback.Sync(context.Background()); !errors.Is(err, ErrCatalogRollback) {
		t.Fatalf("rollback sync error=%v want ErrCatalogRollback", err)
	}
	repo.mu.Lock()
	gotHead := *repo.head
	gotWrites := repo.replaces
	repo.mu.Unlock()
	if gotHead != headAfterSecond || gotWrites != writesAfterSecond {
		t.Fatalf("rollback attempt mutated ledger: head=%+v/%+v writes=%d/%d", gotHead, headAfterSecond, gotWrites, writesAfterSecond)
	}
	if got := rollbackLoader.catalogHead(); got != nil {
		t.Fatalf("rollback attempt activated loader head %+v", got)
	}
}

func TestCatalogCandidateIsInvisibleUntilActivation(t *testing.T) {
	dir := setupTestProblems(t)
	loader := newSnapshotRuntimeLoader(t, dir)
	candidate, err := loader.BuildCandidate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	problems := candidate.Problems()
	if len(problems) == 0 {
		t.Fatal("candidate contains no problems")
	}
	ref := runner.ProblemRef{ID: problems[0].ID, Revision: problems[0].Revision}
	if _, err := loader.ResolveProblem(context.Background(), ref); !errors.Is(err, runner.ErrInvalidRevision) {
		t.Fatalf("unpublished candidate resolution error = %v, want ErrInvalidRevision", err)
	}
	if _, err := loader.ResolveActiveProblem(context.Background(), ref); !errors.Is(err, runner.ErrInvalidRevision) {
		t.Fatalf("unpublished active candidate error = %v, want ErrInvalidRevision", err)
	}

	returned := candidate.Problems()
	returned[0].Title = "caller mutation"
	if candidate.Problems()[0].Title == "caller mutation" {
		t.Fatal("caller mutated immutable candidate projection")
	}
	if err := activateCatalogCandidateForTest(loader, candidate); err != nil {
		t.Fatal(err)
	}
	if _, err := loader.ResolveActiveProblem(context.Background(), ref); err != nil {
		t.Fatalf("activated candidate did not resolve: %v", err)
	}
	if err := activateCatalogCandidateForTest(loader, candidate); err == nil {
		t.Fatal("consumed candidate was activated twice")
	}
}

func TestCatalogCandidateRejectsCrossLoaderAndStaleActivation(t *testing.T) {
	dir := setupTestProblems(t)
	first := newSnapshotRuntimeLoader(t, dir)
	second := newSnapshotRuntimeLoader(t, dir)
	candidate, err := first.BuildCandidate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := activateCatalogCandidateForTest(second, candidate); err == nil {
		t.Fatal("cross-loader candidate was accepted")
	}

	newer, err := first.BuildCandidate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := activateCatalogCandidateForTest(first, newer); err != nil {
		t.Fatal(err)
	}
	if err := activateCatalogCandidateForTest(first, candidate); err == nil {
		t.Fatal("stale candidate rolled catalog generation back")
	}
}

func TestCatalogCoordinatorKnownFailureKeepsPreviousGeneration(t *testing.T) {
	dir := setupTestProblems(t)
	loader := newSnapshotRuntimeLoader(t, dir)
	repo := newMemoryCatalogRepository()
	coordinator, err := NewCatalogCoordinator(loader, repo)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	first, firstSelection, release, err := coordinator.AcquireActiveProblem(context.Background(), "test-problem")
	if err != nil {
		t.Fatal(err)
	}
	release()
	if firstSelection.Generation != 1 || firstSelection.Problem != (runner.ProblemRef{ID: first.ID, Revision: first.Revision}) {
		t.Fatalf("first active selection = %+v", firstSelection)
	}

	mutateSnapshotProblem(t, dir)
	repo.replaceErr = errors.New("known transaction rollback")
	if _, err := coordinator.Sync(context.Background()); err == nil {
		t.Fatal("known database failure unexpectedly synchronized")
	}
	if !coordinator.Healthy() {
		t.Fatal("known rollback poisoned catalog admission")
	}
	current, currentSelection, release, err := coordinator.AcquireActiveProblem(context.Background(), "test-problem")
	if err != nil {
		t.Fatalf("old active catalog unavailable after rollback: %v", err)
	}
	release()
	if current.Revision != first.Revision || current.Title != first.Title || currentSelection != firstSelection {
		t.Fatalf("known rollback exposed new generation: got=%+v want=%+v", current, first)
	}
}

func TestCatalogCoordinatorAdmissionReleaseIsIdempotent(t *testing.T) {
	dir := setupTestProblems(t)
	loader := newSnapshotRuntimeLoader(t, dir)
	repo := newMemoryCatalogRepository()
	coordinator, err := NewCatalogCoordinator(loader, repo)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}

	_, _, release, err := coordinator.AcquireActiveProblem(context.Background(), "test-problem")
	if err != nil {
		t.Fatal(err)
	}
	release()
	release()

	_, _, release, err = coordinator.AcquireActiveProblem(context.Background(), "test-problem")
	if err != nil {
		t.Fatal(err)
	}
	var releases sync.WaitGroup
	for range 32 {
		releases.Add(1)
		go func() {
			defer releases.Done()
			release()
		}()
	}
	releases.Wait()

	if err := coordinator.admissionGate.Acquire(context.Background(), catalogAdmissionCapacity); err != nil {
		t.Fatal(err)
	}
	coordinator.admissionGate.Release(catalogAdmissionCapacity)
}

func TestCatalogCoordinatorAdmissionWaitHonorsContextCancellation(t *testing.T) {
	dir := setupTestProblems(t)
	loader := newSnapshotRuntimeLoader(t, dir)
	repo := newMemoryCatalogRepository()
	coordinator, err := NewCatalogCoordinator(loader, repo)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}

	if err := coordinator.admissionGate.Acquire(context.Background(), catalogAdmissionCapacity); err != nil {
		t.Fatal(err)
	}
	defer coordinator.admissionGate.Release(catalogAdmissionCapacity)

	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		close(started)
		_, _, release, err := coordinator.AcquireActiveProblem(ctx, "test-problem")
		if release != nil {
			release()
		}
		result <- err
	}()
	<-started
	select {
	case err := <-result:
		t.Fatalf("admission returned before cancellation: %v", err)
	default:
	}
	cancel()

	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("admission cancellation error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("admission did not return after context cancellation")
	}
}

func TestCatalogCoordinatorSyncWaitHonorsContextCancellation(t *testing.T) {
	dir := setupTestProblems(t)
	loader := newSnapshotRuntimeLoader(t, dir)
	repo := newMemoryCatalogRepository()
	coordinator, err := NewCatalogCoordinator(loader, repo)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}

	_, _, release, err := coordinator.AcquireActiveProblem(context.Background(), "test-problem")
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		close(started)
		_, err := coordinator.Sync(ctx)
		result <- err
	}()
	<-started
	cancel()

	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("sync cancellation error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("sync did not return after context cancellation")
	}
}

func TestCatalogCoordinatorUnknownCommitConfirmedNotAppliedKeepsAdmissionHealthy(t *testing.T) {
	dir := setupTestProblems(t)
	loader := newSnapshotRuntimeLoader(t, dir)
	repo := newMemoryCatalogRepository()
	coordinator, err := NewCatalogCoordinator(loader, repo)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	active, _, release, err := coordinator.AcquireActiveProblem(context.Background(), "test-problem")
	if err != nil {
		t.Fatal(err)
	}
	release()
	historicalRef := runner.ProblemRef{ID: active.ID, Revision: active.Revision}

	mutateSnapshotProblem(t, dir)
	repo.replaceErr = ErrCatalogCommitUnknown
	if _, err := coordinator.Sync(context.Background()); !errors.Is(err, ErrCatalogCommitUnknown) || !errors.Is(err, ErrCatalogCommitNotApplied) {
		t.Fatalf("sync error = %v, want ErrCatalogCommitUnknown and ErrCatalogCommitNotApplied", err)
	}
	if !coordinator.Healthy() {
		t.Fatal("confirmed rollback poisoned catalog admission")
	}
	current, selection, release, err := coordinator.AcquireActiveProblem(context.Background(), "test-problem")
	if err != nil {
		t.Fatalf("old catalog admission failed after confirmed rollback: %v", err)
	}
	release()
	if current.Revision != active.Revision || selection.Generation != 1 {
		t.Fatalf("confirmed rollback changed active catalog: problem=%+v selection=%+v", current, selection)
	}
	if _, err := coordinator.ResolveProblem(context.Background(), historicalRef); err != nil {
		t.Fatalf("confirmed rollback blocked historical session resolution: %v", err)
	}
}

func TestCatalogCoordinatorUnknownCommitAppliedExactlyActivatesCandidate(t *testing.T) {
	dir := setupTestProblems(t)
	loader := newSnapshotRuntimeLoader(t, dir)
	repo := newMemoryCatalogRepository()
	coordinator, err := NewCatalogCoordinator(loader, repo)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	mutateSnapshotProblem(t, dir)
	repo.replaceErr = ErrCatalogCommitUnknown
	repo.replaceApply = true

	count, err := coordinator.Sync(context.Background())
	if err != nil {
		t.Fatalf("reconciled sync: %v", err)
	}
	repo.mu.Lock()
	wantCount := len(repo.active)
	repo.mu.Unlock()
	if count != wantCount {
		t.Fatalf("reconciled sync count=%d want=%d", count, wantCount)
	}
	if !coordinator.Healthy() {
		t.Fatal("exactly reconciled commit poisoned admission")
	}
	current, selection, release, err := coordinator.AcquireActiveProblem(context.Background(), "test-problem")
	if err != nil {
		t.Fatal(err)
	}
	release()
	if selection.Generation != 2 || selection.Problem.Revision != current.Revision {
		t.Fatalf("reconciled selection = %+v, problem=%+v", selection, current)
	}
}

func TestCatalogCoordinatorUnknownCommitSupersededPoisonsAdmission(t *testing.T) {
	dir := setupTestProblems(t)
	loader := newSnapshotRuntimeLoader(t, dir)
	repo := newMemoryCatalogRepository()
	coordinator, err := NewCatalogCoordinator(loader, repo)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	mutateSnapshotProblem(t, dir)
	repo.replaceErr = ErrCatalogCommitUnknown
	repo.replaceApply = true
	repo.supersede = true

	if _, err := coordinator.Sync(context.Background()); !errors.Is(err, ErrCatalogAdmissionUnavailable) {
		t.Fatalf("superseded reconciliation error = %v, want ErrCatalogAdmissionUnavailable", err)
	}
	if coordinator.Healthy() {
		t.Fatal("superseded ambiguous commit did not poison admission")
	}
}

func TestCatalogCoordinatorActivationFailureAfterDBCommitPoisonsAdmission(t *testing.T) {
	dir := setupTestProblems(t)
	loader := newSnapshotRuntimeLoader(t, dir)
	repo := newMemoryCatalogRepository()
	coordinator, err := NewCatalogCoordinator(loader, repo)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	active, _, release, err := coordinator.AcquireActiveProblem(context.Background(), "test-problem")
	if err != nil {
		t.Fatal(err)
	}
	release()
	historicalRef := runner.ProblemRef{ID: active.ID, Revision: active.Revision}

	mutateSnapshotProblem(t, dir)
	prepare := coordinator.prepareActivation
	commitErr := errors.New("injected memory publication failure")
	coordinator.prepareActivation = func(candidate *CatalogCandidate, target CatalogHead) (catalogActivation, error) {
		activation, err := prepare(candidate, target)
		if err != nil {
			return nil, err
		}
		return &commitFailingCatalogActivation{inner: activation, err: commitErr}, nil
	}

	if _, err := coordinator.Sync(context.Background()); !errors.Is(err, ErrCatalogAdmissionUnavailable) {
		t.Fatalf("sync error = %v, want ErrCatalogAdmissionUnavailable", err)
	}
	if coordinator.Healthy() {
		t.Fatal("memory publication failure did not poison catalog admission")
	}
	if _, _, release, err := coordinator.AcquireActiveProblem(context.Background(), "test-problem"); !errors.Is(err, ErrCatalogAdmissionUnavailable) {
		if release != nil {
			release()
		}
		t.Fatalf("poisoned new admission error = %v, want ErrCatalogAdmissionUnavailable", err)
	}
	if _, err := coordinator.ResolveProblem(context.Background(), historicalRef); err != nil {
		t.Fatalf("memory publication failure blocked historical resolution: %v", err)
	}

	lockResult := make(chan error, 1)
	go func() {
		candidate, err := loader.BuildCandidate(context.Background())
		if err != nil {
			lockResult <- err
			return
		}
		activation, err := loader.prepareActivation(candidate)
		if err == nil {
			err = activation.Abort()
		}
		lockResult <- err
	}()
	select {
	case err := <-lockResult:
		if err != nil {
			t.Fatalf("publication lock was not reusable after failed commit: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("publication lock remained held after failed commit")
	}
}

func activateCatalogCandidateForTest(loader *GitLoader, candidate *CatalogCandidate) error {
	activation, err := loader.prepareActivation(candidate)
	if err != nil {
		return err
	}
	return activation.Commit()
}

func TestStrictCatalogRejectsMissingOrEmptyScriptVerifier(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(string) error
	}{
		{name: "missing", edit: os.Remove},
		{name: "empty", edit: func(path string) error { return os.WriteFile(path, nil, 0755) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := setupTestProblems(t)
			if err := test.edit(filepath.Join(dir, "test-problem", "verify.sh")); err != nil {
				t.Fatal(err)
			}
			loader := newSnapshotRuntimeLoader(t, dir)
			if _, err := loader.BuildCandidate(context.Background()); err == nil {
				t.Fatal("strict catalog accepted an ungradable script problem")
			}
		})
	}
}
