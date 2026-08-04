package problem

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/k8s-quiz/backend/internal/runner"
	"github.com/k8s-quiz/backend/pkg/models"
	"golang.org/x/sync/semaphore"
)

var ErrCatalogAdmissionUnavailable = errors.New("problem catalog admission is unavailable")

// A reader acquires one unit and a publisher acquires the entire gate. The
// capacity is intentionally far above any realistic number of concurrent
// starts while remaining safely representable by semaphore.Weighted.
const catalogAdmissionCapacity int64 = 1 << 62

// Reconciliation must outlive a canceled HTTP request, but it must not hold
// the global PostgreSQL publication lease indefinitely.
const catalogCommitReconcileTimeout = 5 * time.Second

const catalogStartupSnapshotAttempts = 3

// ActiveCatalogRepository is the transactional database projection used by
// catalog publication. FindByID must return active rows only.
type ActiveCatalogRepository interface {
	CatalogLedger
}

type catalogActivation interface {
	Commit() error
	Abort() error
}

type catalogActivationPreparer func(*CatalogCandidate, CatalogHead) (catalogActivation, error)

// CatalogCoordinator linearizes the database projection and in-process bundle
// publication. It is provider-neutral: the same boundary applies to Local
// Docker, Proxmox, and future cloud runners.
type CatalogCoordinator struct {
	loader *GitLoader
	repo   ActiveCatalogRepository

	prepareActivation catalogActivationPreparer
	syncGate          *semaphore.Weighted
	admissionGate     *semaphore.Weighted
	healthMu          sync.RWMutex
	poisoned          error
}

func NewCatalogCoordinator(loader *GitLoader, repo ActiveCatalogRepository) (*CatalogCoordinator, error) {
	if loader == nil || !loader.strictRuntime {
		return nil, errors.New("catalog coordinator requires a strict runtime loader")
	}
	if loader.artifactStore == nil {
		return nil, errors.New("catalog coordinator requires a persistent artifact store")
	}
	if repo == nil {
		return nil, errors.New("catalog coordinator requires a repository")
	}
	return &CatalogCoordinator{
		loader: loader,
		repo:   repo,
		prepareActivation: func(candidate *CatalogCandidate, target CatalogHead) (catalogActivation, error) {
			return loader.prepareActivationAt(candidate, target)
		},
		syncGate:      semaphore.NewWeighted(1),
		admissionGate: semaphore.NewWeighted(catalogAdmissionCapacity),
	}, nil
}

// Startup bootstraps an empty ledger or reconstructs the persisted head from
// PostgreSQL and the artifact CAS without rewriting it. Mutable checkout
// differences never become an implicit rollback or publication.
func (c *CatalogCoordinator) Startup(ctx context.Context) (int, error) {
	if err := c.syncGate.Acquire(ctx, 1); err != nil {
		return 0, err
	}
	defer c.syncGate.Release(1)
	if err := c.healthError(); err != nil {
		return 0, err
	}

	// Every filesystem/image operation is prepared outside both global fences.
	// A short lease snapshots immutable DB rows; the final fenced phase proves
	// that the head and complete inventory are still exact before activation.
	for attempt := 0; attempt < catalogStartupSnapshotAttempts; attempt++ {
		snapshot, err := c.readStartupSnapshot(ctx)
		if err != nil {
			return 0, err
		}
		if snapshot == nil {
			candidate, err := c.loader.BuildCandidate(ctx)
			if err != nil {
				return 0, err
			}
			if err := candidate.ensureArtifacts(ctx, c.loader.artifactStore); err != nil {
				return 0, err
			}
			count, retry, err := c.finalizeEmptyStartup(ctx, candidate)
			if err != nil || !retry {
				return count, err
			}
			continue
		}

		candidate, err := c.prepareStartupSnapshot(ctx, snapshot)
		if err != nil {
			return 0, err
		}
		count, retry, err := c.finalizeStartupSnapshot(ctx, snapshot, candidate)
		if err != nil || !retry {
			return count, err
		}
	}
	return 0, ErrCatalogHeadConflict
}

type catalogStartupSnapshot struct {
	head           CatalogHead
	entries        []CatalogEntry
	legacyProblems []models.Problem
	legacy         bool
}

func (c *CatalogCoordinator) readStartupSnapshot(ctx context.Context) (*catalogStartupSnapshot, error) {
	lease, err := c.repo.AcquireCatalogPublicationLease(ctx)
	if err != nil {
		return nil, err
	}
	head, readErr := lease.Head(ctx)
	if readErr != nil {
		readErr = fmt.Errorf("read persisted problem catalog head: %w", readErr)
	} else if head == nil {
		if closeErr := closeCatalogPublicationLease(ctx, lease); closeErr != nil {
			return nil, closeErr
		}
		return nil, nil
	}
	if readErr == nil {
		var storedHead CatalogHead
		var entries []CatalogEntry
		storedHead, entries, readErr = lease.ReadPublication(ctx, head.Generation)
		if readErr == nil {
			if storedHead != *head {
				readErr = ErrCatalogStartupMismatch
			} else {
				snapshot := &catalogStartupSnapshot{head: *head, entries: cloneStartupCatalogEntries(entries)}
				if closeErr := closeCatalogPublicationLease(ctx, lease); closeErr != nil {
					return nil, closeErr
				}
				return snapshot, nil
			}
		} else if errors.Is(readErr, ErrCatalogArtifactBindingMissing) && head.Ref.DigestSchema == legacyCatalogDigestSchema {
			var legacyHead CatalogHead
			var problems []models.Problem
			legacyHead, problems, readErr = lease.ReadLegacyPublication(ctx, head.Generation)
			if readErr == nil {
				if legacyHead != *head {
					readErr = ErrCatalogStartupMismatch
				} else {
					snapshot := &catalogStartupSnapshot{head: *head, legacyProblems: cloneStartupCatalogProblems(problems), legacy: true}
					if closeErr := closeCatalogPublicationLease(ctx, lease); closeErr != nil {
						return nil, closeErr
					}
					return snapshot, nil
				}
			}
		}
	}
	closeErr := closeCatalogPublicationLease(ctx, lease)
	return nil, errors.Join(readErr, closeErr)
}

func (c *CatalogCoordinator) prepareStartupSnapshot(ctx context.Context, snapshot *catalogStartupSnapshot) (*CatalogCandidate, error) {
	if snapshot == nil {
		return nil, ErrCatalogHeadConflict
	}
	local := c.loader.catalogHead()
	if local != nil && !sameCatalogHead(local, &snapshot.head) {
		return nil, c.poisonedError("local and persisted catalog heads differ", nil)
	}
	if snapshot.legacy {
		candidate, err := c.loader.BuildCandidate(ctx)
		if err != nil {
			return nil, fmt.Errorf("capture legacy catalog for artifact binding: %w", err)
		}
		if err := candidate.ensureArtifacts(ctx, c.loader.artifactStore); err != nil {
			return nil, err
		}
		legacyIdentity, err := candidate.legacyIdentity()
		if err != nil || legacyIdentity != snapshot.head.Ref ||
			!sameCatalogProjection(snapshot.legacyProblems, candidate.Problems()) {
			return nil, ErrCatalogStartupMismatch
		}
		return candidate, nil
	}

	candidate, err := c.loader.candidateFromEntries(ctx, snapshot.entries, c.loader.artifactStore)
	if err != nil {
		if local != nil && shouldPoisonCatalogValidation(err) {
			return nil, c.poisonedError("revalidate active problem catalog artifacts", err)
		}
		return nil, err
	}
	identity, identityErr := candidate.Identity()
	identityMatches := identityErr == nil && identity == snapshot.head.Ref
	if !identityMatches && snapshot.head.Ref.DigestSchema == legacyCatalogDigestSchema {
		legacy, legacyErr := candidate.legacyIdentity()
		identityMatches = legacyErr == nil && legacy == snapshot.head.Ref
	}
	if !identityMatches || !sameCatalogEntries(snapshot.entries, candidate.Entries()) {
		if local != nil {
			return nil, c.poisonedError("revalidate active problem catalog identity", ErrCatalogStartupMismatch)
		}
		return nil, ErrCatalogStartupMismatch
	}
	return candidate, nil
}

func (c *CatalogCoordinator) finalizeEmptyStartup(ctx context.Context, candidate *CatalogCandidate) (int, bool, error) {
	if err := c.admissionGate.Acquire(ctx, catalogAdmissionCapacity); err != nil {
		return 0, false, err
	}
	defer c.admissionGate.Release(catalogAdmissionCapacity)
	lease, err := c.repo.AcquireCatalogPublicationLease(ctx)
	if err != nil {
		return 0, false, err
	}
	defer func() { _ = closeCatalogPublicationLease(ctx, lease) }()
	head, err := lease.Head(ctx)
	if err != nil {
		return 0, false, fmt.Errorf("recheck persisted problem catalog head: %w", err)
	}
	if head != nil {
		return 0, true, nil
	}
	count, err := c.publishCandidate(ctx, lease, nil, candidate)
	return count, false, err
}

func (c *CatalogCoordinator) finalizeStartupSnapshot(ctx context.Context, snapshot *catalogStartupSnapshot, candidate *CatalogCandidate) (int, bool, error) {
	if err := c.admissionGate.Acquire(ctx, catalogAdmissionCapacity); err != nil {
		return 0, false, err
	}
	defer c.admissionGate.Release(catalogAdmissionCapacity)
	lease, err := c.repo.AcquireCatalogPublicationLease(ctx)
	if err != nil {
		return 0, false, err
	}
	defer func() { _ = closeCatalogPublicationLease(ctx, lease) }()
	current, err := lease.Head(ctx)
	if err != nil {
		return 0, false, fmt.Errorf("recheck persisted problem catalog head: %w", err)
	}
	if !sameCatalogHead(current, &snapshot.head) {
		if c.loader.catalogHead() != nil {
			return 0, false, c.poisonedError("persisted catalog head changed while revalidating active artifacts", nil)
		}
		return 0, true, nil
	}

	if snapshot.legacy {
		storedHead, problems, err := lease.ReadLegacyPublication(ctx, snapshot.head.Generation)
		if err != nil || storedHead != snapshot.head || !sameCatalogProjection(problems, snapshot.legacyProblems) {
			return 0, false, ErrCatalogStartupMismatch
		}
		handled, err := c.bindLegacyArtifacts(ctx, lease, snapshot.head, candidate, true,
			"adopt artifact-bound legacy problem catalog")
		if err != nil {
			return 0, false, err
		}
		if !handled {
			if err := c.activateCandidate(candidate, snapshot.head, "adopt artifact-bound legacy problem catalog"); err != nil {
				return 0, false, err
			}
		}
		return len(candidate.ids), false, nil
	}

	storedHead, entries, err := lease.ReadPublication(ctx, snapshot.head.Generation)
	if err != nil || storedHead != snapshot.head || !sameCatalogEntries(entries, snapshot.entries) {
		return 0, false, ErrCatalogStartupMismatch
	}
	if c.loader.catalogHead() != nil {
		return len(entries), false, nil
	}
	if err := c.activateCandidate(candidate, snapshot.head, "adopt persisted problem catalog"); err != nil {
		return 0, false, err
	}
	return len(entries), false, nil
}

func shouldPoisonCatalogValidation(err error) bool {
	return err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
}

func cloneStartupCatalogProblems(problems []models.Problem) []models.Problem {
	result := make([]models.Problem, len(problems))
	for i := range problems {
		result[i] = cloneProblem(problems[i])
	}
	return result
}

func cloneStartupCatalogEntries(entries []CatalogEntry) []CatalogEntry {
	result := make([]CatalogEntry, len(entries))
	for i := range entries {
		result[i] = entries[i]
		result[i].Problem = cloneProblem(entries[i].Problem)
	}
	return result
}

func closeCatalogPublicationLease(ctx context.Context, lease catalogPublicationLease) error {
	closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), catalogCommitReconcileTimeout)
	defer cancel()
	if err := lease.Close(closeCtx); err != nil {
		return fmt.Errorf("close problem catalog publication lease: %w", err)
	}
	return nil
}

// bindLegacyArtifacts handles an ambiguous COMMIT without trusting the
// connection that returned it. Under a fresh advisory lease it either observes
// the complete exact binding or retries the idempotent binding transaction.
// When activate is true, activation happens while the proving lease is held.
func (c *CatalogCoordinator) bindLegacyArtifacts(
	ctx context.Context,
	lease catalogPublicationLease,
	expected CatalogHead,
	candidate *CatalogCandidate,
	activate bool,
	operation string,
) (bool, error) {
	entries := candidate.Entries()
	err := lease.BindLegacyArtifacts(ctx, expected, entries)
	if err == nil {
		return false, nil
	}
	if !errors.Is(err, ErrCatalogCommitUnknown) {
		return false, err
	}

	reconcileCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), catalogCommitReconcileTimeout)
	defer cancel()
	currentLease := lease
	for {
		if err := currentLease.Discard(reconcileCtx); err != nil {
			return false, c.poisonedError("discard ambiguous legacy artifact binding connection", err)
		}
		fresh, err := c.repo.AcquireCatalogPublicationLease(reconcileCtx)
		if err != nil {
			return false, c.poisonedError("reacquire publication lease after ambiguous legacy binding", err)
		}
		currentLease = fresh

		current, headErr := currentLease.Head(reconcileCtx)
		if headErr != nil || !sameCatalogHead(current, &expected) {
			_ = currentLease.Close(reconcileCtx)
			return false, c.poisonedError("reconcile ambiguous legacy artifact binding head", headErr)
		}
		storedHead, storedEntries, readErr := currentLease.ReadPublication(reconcileCtx, expected.Generation)
		if readErr == nil {
			if storedHead != expected || !sameCatalogEntries(storedEntries, entries) {
				_ = currentLease.Close(reconcileCtx)
				return false, c.poisonedError("ambiguous legacy artifact binding diverged", nil)
			}
			if activate {
				if err := c.activateCandidate(candidate, expected, operation); err != nil {
					_ = currentLease.Close(reconcileCtx)
					return false, err
				}
			}
			if err := currentLease.Close(reconcileCtx); err != nil {
				return false, c.poisonedError("close reconciled legacy artifact binding lease", err)
			}
			return true, nil
		}
		if !errors.Is(readErr, ErrCatalogArtifactBindingMissing) {
			_ = currentLease.Close(reconcileCtx)
			return false, c.poisonedError("read ambiguous legacy artifact binding", readErr)
		}

		err = currentLease.BindLegacyArtifacts(reconcileCtx, expected, entries)
		if err == nil {
			if activate {
				if err := c.activateCandidate(candidate, expected, operation); err != nil {
					_ = currentLease.Close(reconcileCtx)
					return false, err
				}
			}
			if err := currentLease.Close(reconcileCtx); err != nil {
				return false, c.poisonedError("close retried legacy artifact binding lease", err)
			}
			return true, nil
		}
		if !errors.Is(err, ErrCatalogCommitUnknown) {
			_ = currentLease.Close(reconcileCtx)
			return false, err
		}
		// The fresh connection is now ambiguous too. Discard it at the top of
		// the loop and prove the transaction outcome through another session.
	}
}

// Sync is the explicit administrative publication path. It appends head+1;
// synchronizing an unchanged current checkout is an idempotent no-op.
func (c *CatalogCoordinator) Sync(ctx context.Context) (int, error) {
	if err := c.syncGate.Acquire(ctx, 1); err != nil {
		return 0, err
	}
	defer c.syncGate.Release(1)
	if err := c.healthError(); err != nil {
		return 0, err
	}
	candidate, err := c.loader.BuildCandidate(ctx)
	if err != nil {
		return 0, err
	}
	if err := candidate.ensureArtifacts(ctx, c.loader.artifactStore); err != nil {
		return 0, err
	}
	if err := c.admissionGate.Acquire(ctx, catalogAdmissionCapacity); err != nil {
		return 0, err
	}
	defer c.admissionGate.Release(catalogAdmissionCapacity)
	lease, err := c.repo.AcquireCatalogPublicationLease(ctx)
	if err != nil {
		return 0, err
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), catalogCommitReconcileTimeout)
		defer cancel()
		_ = lease.Close(closeCtx)
	}()
	return c.syncCandidateUnderLease(ctx, lease, candidate)
}

func (c *CatalogCoordinator) syncCandidateUnderLease(ctx context.Context, lease catalogPublicationLease, candidate *CatalogCandidate) (int, error) {
	persistedHead, err := lease.Head(ctx)
	if err != nil {
		return 0, fmt.Errorf("read persisted problem catalog head: %w", err)
	}
	localHead := c.loader.catalogHead()
	if localHead != nil && !sameCatalogHead(localHead, persistedHead) {
		return 0, c.poisonedError("local and persisted catalog heads differ", nil)
	}
	identity, err := candidate.Identity()
	if err != nil {
		return 0, err
	}
	if persistedHead != nil && persistedHead.Ref == identity {
		storedHead, stored, err := lease.ReadPublication(ctx, persistedHead.Generation)
		if err != nil || storedHead != *persistedHead || !sameCatalogEntries(stored, candidate.Entries()) {
			return 0, ErrCatalogStartupMismatch
		}
		if localHead == nil {
			if err := c.activateCandidate(candidate, storedHead, "adopt unchanged problem catalog"); err != nil {
				return 0, err
			}
		}
		return len(stored), nil
	}
	legacy, legacyErr := candidate.legacyIdentity()
	if persistedHead != nil && legacyErr == nil && persistedHead.Ref == legacy {
		storedHead, stored, err := lease.ReadLegacyPublication(ctx, persistedHead.Generation)
		if err != nil || storedHead != *persistedHead || !sameCatalogProjection(stored, candidate.Problems()) {
			return 0, ErrCatalogStartupMismatch
		}
		handled, err := c.bindLegacyArtifacts(ctx, lease, *persistedHead, candidate, localHead == nil,
			"adopt upgraded legacy problem catalog")
		if err != nil {
			return 0, err
		}
		if localHead == nil && !handled {
			if err := c.activateCandidate(candidate, *persistedHead, "adopt upgraded legacy problem catalog"); err != nil {
				return 0, err
			}
		}
		return len(stored), nil
	}
	if prior, err := lease.FindPublication(ctx, identity); err != nil {
		return 0, err
	} else if prior != nil {
		return 0, ErrCatalogRollback
	}
	if legacyErr == nil {
		if prior, err := lease.FindPublication(ctx, legacy); err != nil {
			return 0, err
		} else if prior != nil {
			return 0, ErrCatalogRollback
		}
	}
	return c.publishCandidate(ctx, lease, persistedHead, candidate)
}

func (c *CatalogCoordinator) publishCandidate(ctx context.Context, lease catalogPublicationLease, expected *CatalogHead, candidate *CatalogCandidate) (int, error) {
	identity, err := candidate.Identity()
	if err != nil {
		return 0, err
	}
	entries := candidate.Entries()
	target := CatalogHead{Generation: 1, Ref: identity, EntryCount: len(entries)}
	if expected != nil {
		target.Generation = expected.Generation + 1
	}
	activation, err := c.prepareActivation(candidate, target)
	if err != nil {
		return 0, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = activation.Abort()
		}
	}()

	published, err := lease.Publish(ctx, expected, identity, entries)
	if err == nil {
		if published != target {
			return 0, c.poisonedError("persisted publication identity changed", nil)
		}
		if err := activation.Commit(); err != nil {
			return 0, c.poisonedError("publish committed problem catalog", err)
		}
		committed = true
		return len(entries), nil
	}
	if !errors.Is(err, ErrCatalogCommitUnknown) {
		return 0, err
	}
	ambiguousErr := err
	reconcileCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), catalogCommitReconcileTimeout)
	defer cancel()
	if discardErr := lease.Discard(reconcileCtx); discardErr != nil {
		return 0, c.poisonedError("discard ambiguous publication connection", discardErr)
	}

	// Reconciliation never uses the session that returned the ambiguous COMMIT
	// result. Reacquiring the advisory lease also fences later publishers while
	// the exact publication and CAS-bound entries are observed.
	reconcileLease, err := c.repo.AcquireCatalogPublicationLease(reconcileCtx)
	if err != nil {
		return 0, c.poisonedError("reacquire publication lease after ambiguous commit", err)
	}
	defer func() { _ = reconcileLease.Close(reconcileCtx) }()
	current, headErr := reconcileLease.Head(reconcileCtx)
	candidateHead, findErr := reconcileLease.FindPublication(reconcileCtx, identity)
	if headErr != nil || findErr != nil {
		return 0, c.poisonedError("reconcile ambiguous problem catalog commit",
			fmt.Errorf("head=%v publication=%v", headErr, findErr))
	}
	if candidateHead == nil {
		if !sameCatalogHead(current, expected) {
			return 0, c.poisonedError("ambiguous catalog commit was not recorded but the head changed", nil)
		}
		return 0, fmt.Errorf("%w: %w", ErrCatalogCommitNotApplied, ambiguousErr)
	}
	storedHead, storedEntries, readErr := reconcileLease.ReadPublication(reconcileCtx, candidateHead.Generation)
	if readErr != nil || storedHead != target || *candidateHead != target ||
		!sameCatalogEntries(storedEntries, entries) || !sameCatalogHead(current, &target) {
		return 0, c.poisonedError("ambiguous catalog commit produced a divergent or superseded publication", readErr)
	}
	if err := activation.Commit(); err != nil {
		return 0, c.poisonedError("activate reconciled problem catalog", err)
	}
	committed = true
	return len(entries), nil
}

func (c *CatalogCoordinator) activateCandidate(candidate *CatalogCandidate, target CatalogHead, operation string) error {
	activation, err := c.prepareActivation(candidate, target)
	if err != nil {
		return err
	}
	if err := activation.Commit(); err != nil {
		abortErr := activation.Abort()
		if abortErr != nil && !errors.Is(abortErr, ErrCatalogActivationConsumed) {
			err = fmt.Errorf("%w; abort activation: %v", err, abortErr)
		}
		return c.poisonedError(operation, err)
	}
	return nil
}

func (c *CatalogCoordinator) poisonedError(operation string, cause error) error {
	var err error
	if cause == nil {
		err = fmt.Errorf("%w: %s", ErrCatalogAdmissionUnavailable, operation)
	} else {
		err = fmt.Errorf("%w: %s: %w", ErrCatalogAdmissionUnavailable, operation, cause)
	}
	c.poison(err)
	return err
}

func sameCatalogEntries(left, right []CatalogEntry) bool {
	if len(left) != len(right) {
		return false
	}
	leftByID := make(map[string]CatalogEntry, len(left))
	for _, entry := range left {
		if _, duplicate := leftByID[entry.Problem.ID]; duplicate {
			return false
		}
		leftByID[entry.Problem.ID] = entry
	}
	for _, want := range right {
		got, ok := leftByID[want.Problem.ID]
		if !ok || got.Artifact != want.Artifact || got.SourceTrust != want.SourceTrust ||
			!sameCatalogProjection([]models.Problem{got.Problem}, []models.Problem{want.Problem}) {
			return false
		}
	}
	return true
}

func sameCatalogProjection(left, right []models.Problem) bool {
	if len(left) != len(right) {
		return false
	}
	normalize := func(values []models.Problem) map[string]models.Problem {
		result := make(map[string]models.Problem, len(values))
		for _, value := range values {
			value.CatalogActive = true
			value.CreatedAt = value.CreatedAt.UTC().Truncate(0)
			value.UpdatedAt = value.CreatedAt
			result[value.ID] = value
		}
		return result
	}
	leftMap, rightMap := normalize(left), normalize(right)
	for id, want := range rightMap {
		got, ok := leftMap[id]
		if !ok {
			return false
		}
		got.CreatedAt, got.UpdatedAt = want.CreatedAt, want.UpdatedAt
		if !reflect.DeepEqual(got, want) {
			return false
		}
	}
	return true
}

// AcquireActiveProblem holds a read admission lease until release. A fresh
// durable reservation must commit while this lease is held, making it wholly
// precede or follow a catalog publication.
func (c *CatalogCoordinator) AcquireActiveProblem(ctx context.Context, problemID string) (models.Problem, runner.CatalogSelection, func(), error) {
	if err := ctx.Err(); err != nil {
		return models.Problem{}, runner.CatalogSelection{}, nil, err
	}
	if err := c.admissionGate.Acquire(ctx, 1); err != nil {
		return models.Problem{}, runner.CatalogSelection{}, nil, err
	}
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() { c.admissionGate.Release(1) })
	}
	if err := c.healthError(); err != nil {
		release()
		return models.Problem{}, runner.CatalogSelection{}, nil, err
	}
	head, row, err := c.repo.FindActiveCatalogProblem(ctx, problemID)
	if err != nil {
		release()
		return models.Problem{}, runner.CatalogSelection{}, nil, err
	}
	if row.Revision == "" || !row.CatalogActive {
		release()
		return models.Problem{}, runner.CatalogSelection{}, nil, runner.ErrInvalidRevision
	}
	trusted, err := c.loader.ResolveActiveProblem(ctx, runner.ProblemRef{ID: problemID, Revision: row.Revision})
	if err != nil {
		release()
		return models.Problem{}, runner.CatalogSelection{}, nil, err
	}
	// Existing durable work can continue from the immutable in-memory snapshot,
	// but a fresh session must prove that its recovery source still exists and
	// authenticates against the PostgreSQL artifact binding. Live CAS loss or
	// corruption therefore closes readiness before accepting unrecoverable work.
	ref := runner.ProblemRef{ID: problemID, Revision: row.Revision}
	validated, err := c.loadHistoricalBundle(ctx, ref)
	if err != nil || !sameRuntimeProblemProjection(validated.Problem, trusted) {
		release()
		if err == nil {
			err = ErrCatalogStartupMismatch
		}
		return models.Problem{}, runner.CatalogSelection{}, nil,
			c.poisonedError("validate active problem artifact before admission", err)
	}
	selection := runner.CatalogSelection{
		Generation: head.Generation,
		Problem:    ref,
	}
	return trusted, selection, release, nil
}

// ResolveProblem preserves exact historical resolution for durable replay,
// reset, verification, and controller recovery.
func (c *CatalogCoordinator) ResolveProblem(ctx context.Context, ref runner.ProblemRef) (models.Problem, error) {
	problem, err := c.loader.ResolveProblem(ctx, ref)
	if err == nil || !errors.Is(err, runner.ErrInvalidRevision) {
		return problem, err
	}
	bundle, err := c.loadHistoricalBundle(ctx, ref)
	if err != nil {
		return models.Problem{}, err
	}
	return cloneProblem(bundle.Problem), nil
}

// ResolveRuntime implements runner.LocalDockerRuntimeCatalog from the same
// verified ledger/CAS boundary used for grading metadata. Providers therefore
// cannot bypass publication or resolve bytes directly from the checkout.
func (c *CatalogCoordinator) ResolveRuntime(ctx context.Context, ref runner.ProblemRef) (runner.LocalDockerRuntime, error) {
	runtime, err := c.loader.ResolveRuntime(ctx, ref)
	if err == nil || !errors.Is(err, runner.ErrInvalidRevision) {
		return runtime, err
	}
	bundle, err := c.loadHistoricalBundle(ctx, ref)
	if err != nil {
		return runner.LocalDockerRuntime{}, err
	}
	return localRuntimeFromBundle(bundle), nil
}

func (c *CatalogCoordinator) loadHistoricalBundle(ctx context.Context, ref runner.ProblemRef) (RuntimeBundle, error) {
	if err := ctx.Err(); err != nil {
		return RuntimeBundle{}, err
	}
	entry, err := c.repo.FindCatalogArtifact(ctx, ref)
	if err != nil {
		return RuntimeBundle{}, err
	}
	data, err := c.loader.artifactStore.Get(ctx, entry.Artifact)
	if err != nil {
		return RuntimeBundle{}, fmt.Errorf("load historical runtime artifact %s/%s: %w", ref.ID, ref.Revision, err)
	}
	_, bundle, err := DecodeRuntimeArtifact(entry.Artifact, data)
	if err != nil {
		return RuntimeBundle{}, fmt.Errorf("decode historical runtime artifact %s/%s: %w", ref.ID, ref.Revision, err)
	}
	if bundle.Problem.ID != ref.ID || bundle.Revision != ref.Revision ||
		bundle.SourceTrust != entry.SourceTrust || !sameRuntimeProblemProjection(bundle.Problem, entry.Problem) {
		return RuntimeBundle{}, runner.ErrInvalidRevision
	}
	if err := c.loader.installHistoricalBundle(bundle); err != nil {
		return RuntimeBundle{}, err
	}
	return cloneRuntimeBundle(bundle), nil
}

func (c *CatalogCoordinator) Healthy() bool { return c.healthError() == nil }

func (c *CatalogCoordinator) healthError() error {
	c.healthMu.RLock()
	defer c.healthMu.RUnlock()
	return c.poisoned
}

func (c *CatalogCoordinator) poison(err error) {
	c.healthMu.Lock()
	defer c.healthMu.Unlock()
	if c.poisoned == nil {
		c.poisoned = err
	}
}
