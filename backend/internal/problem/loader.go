package problem

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/k8s-quiz/backend/internal/runner"
	"github.com/k8s-quiz/backend/pkg/models"
	"golang.org/x/sys/unix"
	"gopkg.in/yaml.v3"
)

type Loader interface {
	LoadAll(ctx context.Context) ([]models.Problem, error)
	GetProblemDir(problemID string) string
}

type GitLoader struct {
	repoPath      string
	clean         string
	imageResolver ImageResolver
	artifactStore ArtifactStore
	strictRuntime bool

	// Strict runtime catalogs are published only after every candidate bundle
	// has been captured and validated twice with the same content identities.
	// history is append-only for the process lifetime so active sessions can
	// resolve an exact old revision after an explicit catalog reload.
	publicationMu sync.Mutex
	catalogMu     sync.RWMutex
	generation    uint64
	activeHead    *CatalogHead
	active        map[string]string
	history       map[string]map[string]RuntimeBundle
}

// CatalogCandidate is a validated, immutable runtime catalog that has not yet
// been published. Its contents remain private so callers can persist only the
// Problems projection and cannot alter the bundles later installed in history.
type CatalogCandidate struct {
	owner          *GitLoader
	baseGeneration uint64
	ids            []string
	bundles        map[string]RuntimeBundle
	artifacts      map[string]CapturedRuntimeArtifact
	artifactBytes  map[string][]byte
	artifactRefs   map[string]ArtifactRef
}

// CatalogPublicationRef is the provider-neutral identity of one exact catalog
// inventory. It deliberately excludes process-local generation: a later
// persistent publisher assigns the global generation while retaining this
// immutable digest as the candidate subject.
type CatalogPublicationRef struct {
	DigestSchema    uint16
	CandidateDigest [sha256.Size]byte
}

// DigestString returns the algorithm-qualified representation used in logs,
// artifacts, and future publication reconciliation messages.
func (r CatalogPublicationRef) DigestString() string {
	return "sha256:" + hex.EncodeToString(r.CandidateDigest[:])
}

// catalogActivationToken reserves the loader's publication sequence. Preparing an
// activation performs every check that can fail before the database commit;
// Commit then publishes the already-validated candidate without filesystem IO.
// It is intentionally package-private: runtime publication must go through
// CatalogCoordinator so the database and admission gate participate.
type catalogActivationToken struct {
	state *catalogActivationState
}

type catalogActivationState struct {
	mu        sync.Mutex
	loader    *GitLoader
	candidate *CatalogCandidate
	target    CatalogHead
	consumed  bool
}

var (
	ErrCatalogActivationConsumed = errors.New("catalog activation already consumed")
	ErrInvalidCatalogActivation  = errors.New("invalid catalog activation")
	ErrRuntimeCatalogCoordinator = errors.New("strict runtime catalogs must be published through CatalogCoordinator")
)

// ImageResolver maps an authored image reference (which may be a mutable tag)
// to the immutable local content ID that the provider will execute.
type ImageResolver interface {
	ResolveImage(context.Context, string) (string, error)
}

// RuntimeBundle is a single, immutable read of the files that can influence
// a problem environment. The scripts and revision are derived from the same
// bytes so a checkout change cannot mix create-time and verify-time content.
type RuntimeBundle struct {
	Problem      models.Problem
	SetupScript  string
	VerifyScript string
	Revision     string
	RuntimeImage string
	SourceTrust  BundleSourceTrust
}

// BundleSourceTrust separates content identity from approval authority. A
// development checkout can provide a stable digest and immutable in-process
// snapshot, but it is not signed provenance and is never public-eligible.
type BundleSourceTrust string

const BundleSourceDevelopmentCheckout BundleSourceTrust = "development_checkout"

const (
	catalogCandidateDigestSchema uint16 = 2
	catalogCandidateDigestDomain        = "k8s-quiz.problem-catalog\x00"
	legacyCatalogDigestSchema    uint16 = 1
)

const (
	maxProblemManifestBytes = 256 << 10
	maxProblemScriptBytes   = 1 << 20
	maxProblemHintBytes     = 256 << 10
	maxImageLockBytes       = 1 << 20
	// maxRuntimeArtifactBytes is persisted in problem_artifacts.artifact_size.
	// Keep the canonical codec, filesystem CAS, and PostgreSQL constraint equal.
	maxRuntimeArtifactBytes = 8 << 20
	maxRuntimeCatalogBytes  = 32 << 20
	maxRuntimeCatalogCount  = 1024
)

var workloadImagePatterns = []*regexp.Regexp{
	regexp.MustCompile(`^[\t ]*image:[\t ]*["']?([A-Za-z0-9][A-Za-z0-9._/:+@-]*)`),
	regexp.MustCompile(`(?:^|[\t ])--image(?:=|[\t ]+)["']?([A-Za-z0-9][A-Za-z0-9._/:+@-]*)`),
	regexp.MustCompile(`["']image["'][\t ]*:[\t ]*["']([^"']+)["']`),
	regexp.MustCompile(`^[\t ]*[A-Za-z_][A-Za-z0-9_]*IMAGE[A-Za-z0-9_]*=["']?([A-Za-z0-9][A-Za-z0-9._/:+@-]*)`),
}

var (
	imageDigestReference = regexp.MustCompile(`[A-Za-z0-9][A-Za-z0-9._/:+-]*@sha256:[0-9A-Fa-f]+`)
	unsupportedImageOps  = []*regexp.Regexp{
		regexp.MustCompile(`(?i)\bkubectl\s+set\s+image\b`),
		regexp.MustCompile(`(?i)(^|[;&|[:space:]])helm([[:space:]]|$)`),
		regexp.MustCompile(`(?i)(^|[;&|[:space:]])kustomize([[:space:]]|$)`),
		regexp.MustCompile(`(?i)\bkubectl\s+(apply|create|replace)\b[^\n]*\s-f[=[:space:]]+(https?://|\$|\x60)`),
		regexp.MustCompile(`(?i)^[\t ]*image:[\t ]*["']?(\$|\x60)`),
		regexp.MustCompile(`(?i)--image(?:=|[\t ]+)["']?(\$|\x60)`),
		regexp.MustCompile(`(?i)["']image["'][\t ]*:[\t ]*["']?(\$|\x60)`),
		regexp.MustCompile(`(?i)^[\t ]*[A-Za-z_][A-Za-z0-9_]*IMAGE[A-Za-z0-9_]*=["']?(\$|\x60)`),
	}
)

func NewGitLoader(repoPath string) *GitLoader {
	return &GitLoader{repoPath: repoPath, clean: filepath.Clean(repoPath)}
}

// NewRuntimeGitLoader constructs the fail-closed loader used by a runtime
// server. Unlike the file-only loader used by authoring tools and unit tests,
// it binds the provider-resolved image content ID into every problem revision.
func NewRuntimeGitLoader(repoPath string, resolver ImageResolver) (*GitLoader, error) {
	if resolver == nil {
		return nil, errors.New("runtime problem loader requires an image resolver")
	}
	return &GitLoader{
		repoPath:      repoPath,
		clean:         filepath.Clean(repoPath),
		imageResolver: resolver,
		strictRuntime: true,
		active:        make(map[string]string),
		history:       make(map[string]map[string]RuntimeBundle),
	}, nil
}

// NewPersistentRuntimeGitLoader constructs the server runtime loader. Unlike
// NewRuntimeGitLoader, which remains useful for isolated authoring and loader
// tests, this constructor cannot publish without a durable artifact store.
func NewPersistentRuntimeGitLoader(repoPath string, resolver ImageResolver, store ArtifactStore) (*GitLoader, error) {
	loader, err := NewRuntimeGitLoader(repoPath, resolver)
	if err != nil {
		return nil, err
	}
	if store == nil || reflect.ValueOf(store).Kind() == reflect.Ptr && reflect.ValueOf(store).IsNil() {
		return nil, errors.New("persistent runtime problem loader requires an artifact store")
	}
	loader.artifactStore = store
	return loader, nil
}

// safeJoin resolves repoPath/problemID/file and guarantees the result stays
// inside repoPath, defeating path-traversal via a crafted problemID (e.g.
// "../../etc"). Without this, GetSetupScript could read & execute an
// arbitrary host file.
func (l *GitLoader) safeJoin(problemID, file string) (string, error) {
	full := filepath.Clean(filepath.Join(l.repoPath, problemID, file))
	if full != l.clean && !strings.HasPrefix(full, l.clean+string(os.PathSeparator)) {
		return "", fmt.Errorf("problem path escapes repo root")
	}
	return full, nil
}

func (l *GitLoader) LoadAll(ctx context.Context) ([]models.Problem, error) {
	if l.strictRuntime {
		return nil, ErrRuntimeCatalogCoordinator
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(l.repoPath)
	if err != nil {
		return nil, fmt.Errorf("read problems dir: %w", err)
	}

	var problems []models.Problem
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		// Repositories may contain tooling directories such as hack/. Only a
		// directory with a problem.yaml is a problem candidate. Once a
		// manifest exists, strict runtime loading fails closed on every defect.
		manifestPath := filepath.Join(l.clean, entry.Name(), "problem.yaml")
		manifestInfo, err := os.Lstat(manifestPath)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			if l.strictRuntime {
				return nil, fmt.Errorf("inspect runtime problem %q manifest: %w", entry.Name(), err)
			}
			continue
		}
		if !manifestInfo.Mode().IsRegular() {
			if l.strictRuntime {
				return nil, fmt.Errorf("runtime problem %q manifest must be a regular file", entry.Name())
			}
			continue
		}
		// Directory names become problem ids; reject anything that could
		// escape the root or break URLs/paths.
		if !validProblemID(entry.Name()) {
			if l.strictRuntime {
				return nil, fmt.Errorf("runtime problem directory %q is not a valid problem id", entry.Name())
			}
			continue
		}
		bundle, err := l.getRuntimeBundle(ctx, entry.Name())
		if err != nil {
			if l.strictRuntime {
				return nil, fmt.Errorf("load runtime problem %q: %w", entry.Name(), err)
			}
			continue
		}
		p := bundle.Problem
		if p.ID != entry.Name() {
			if l.strictRuntime {
				return nil, fmt.Errorf("runtime problem directory %q contains id %q", entry.Name(), p.ID)
			}
			log.Printf("skipping invalid problem %q: manifest id %q does not match directory", entry.Name(), p.ID)
			continue
		}

		// PROB-12: skip invalid problems (bad id/enums/choice rules) with a
		// server log in authoring mode. Runtime mode fails closed so one bad
		// approved candidate cannot silently disappear from the catalog.
		if msg := validateLoadedProblem(&p); msg != "" {
			if l.strictRuntime {
				return nil, fmt.Errorf("runtime problem %q is invalid: %s", entry.Name(), msg)
			}
			log.Printf("skipping invalid problem %q: %s", entry.Name(), msg)
			continue
		}

		p.Hint = bundle.Problem.Hint

		problems = append(problems, p)
	}

	return problems, nil
}

// BuildCandidate builds a complete candidate without touching active or
// historical catalog state. A second independent capture must yield identical
// bundles. This detects concurrent checkout mutation; it does not claim the
// filesystem is a signed or transactionally atomic source.
func (l *GitLoader) BuildCandidate(ctx context.Context) (*CatalogCandidate, error) {
	if !l.strictRuntime {
		return nil, errors.New("catalog candidates require a strict runtime loader")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Bind the candidate to the generation that existed before any source IO.
	// If another publisher commits while this capture is in progress, activation
	// rejects this candidate rather than allowing an older snapshot to publish
	// after the newer generation.
	l.catalogMu.RLock()
	baseGeneration := l.generation
	l.catalogMu.RUnlock()
	first, err := l.captureRuntimeCatalog(ctx)
	if err != nil {
		return nil, err
	}
	second, err := l.captureRuntimeCatalog(ctx)
	if err != nil {
		return nil, err
	}
	if !reflect.DeepEqual(first, second) {
		return nil, errors.New("runtime problem catalog changed while it was being captured")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	ids := make([]string, 0, len(first))
	for id := range first {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	bundles := make(map[string]RuntimeBundle, len(first))
	artifacts := make(map[string]CapturedRuntimeArtifact, len(first))
	artifactBytes := make(map[string][]byte, len(first))
	artifactRefs := make(map[string]ArtifactRef, len(first))
	for _, id := range ids {
		data, ref, err := EncodeRuntimeArtifact(first[id])
		if err != nil {
			return nil, fmt.Errorf("encode runtime artifact for %q: %w", id, err)
		}
		decoded, bundle, err := DecodeRuntimeArtifact(ref, data)
		if err != nil {
			return nil, fmt.Errorf("verify runtime artifact for %q: %w", id, err)
		}
		artifacts[id] = decoded
		bundles[id] = bundle
		artifactBytes[id] = append([]byte(nil), data...)
		artifactRefs[id] = ref
	}
	return &CatalogCandidate{
		owner:          l,
		baseGeneration: baseGeneration,
		ids:            append([]string(nil), ids...),
		bundles:        bundles,
		artifacts:      artifacts,
		artifactBytes:  artifactBytes,
		artifactRefs:   artifactRefs,
	}, nil
}

// Problems returns a deep copy of the database projection for this candidate.
func (c *CatalogCandidate) Problems() []models.Problem {
	if c == nil {
		return nil
	}
	problems := make([]models.Problem, 0, len(c.ids))
	for _, id := range c.ids {
		problems = append(problems, cloneProblem(c.bundles[id].Problem))
	}
	return problems
}

// CatalogEntry binds the immutable metadata projection to the exact canonical
// runtime artifact. PostgreSQL persists this whole value for publication,
// startup adoption, ambiguous-commit reconciliation, and historical lookup.
type CatalogEntry struct {
	Problem     models.Problem
	Artifact    ArtifactRef
	SourceTrust BundleSourceTrust
}

// Entries returns a deep copy of the artifact-bearing publication inventory.
func (c *CatalogCandidate) Entries() []CatalogEntry {
	if c == nil {
		return nil
	}
	entries := make([]CatalogEntry, 0, len(c.ids))
	for _, id := range c.ids {
		entries = append(entries, CatalogEntry{
			Problem:     cloneProblem(c.bundles[id].Problem),
			Artifact:    c.artifactRefs[id],
			SourceTrust: c.bundles[id].SourceTrust,
		})
	}
	return entries
}

// ensureArtifacts durably installs and reads back every canonical object. It
// intentionally runs before the admission gate and PostgreSQL publication
// lease: a later failure may leave only harmless unreferenced CAS objects.
func (c *CatalogCandidate) ensureArtifacts(ctx context.Context, store ArtifactStore) error {
	if c == nil || store == nil {
		return errors.New("runtime catalog candidate requires an artifact store")
	}
	for _, id := range c.ids {
		if err := ctx.Err(); err != nil {
			return err
		}
		ref, ok := c.artifactRefs[id]
		data, dataOK := c.artifactBytes[id]
		want, bundleOK := c.bundles[id]
		if !ok || !dataOK || !bundleOK {
			return fmt.Errorf("runtime catalog candidate has no canonical artifact for %q", id)
		}
		if err := store.Ensure(ctx, ref, data); err != nil {
			return fmt.Errorf("persist runtime artifact for %q: %w", id, err)
		}
		stored, err := store.Get(ctx, ref)
		if err != nil {
			return fmt.Errorf("read back runtime artifact for %q: %w", id, err)
		}
		_, got, err := DecodeRuntimeArtifact(ref, stored)
		if err != nil {
			return fmt.Errorf("decode persisted runtime artifact for %q: %w", id, err)
		}
		if !reflect.DeepEqual(got, want) {
			return fmt.Errorf("persisted runtime artifact for %q does not match the candidate", id)
		}
	}
	return nil
}

// candidateFromEntries reconstructs a complete activation candidate from the
// persisted ledger and verified CAS. It never reads the mutable checkout or
// resolves an image tag, so ordinary restart adoption is reproducible even
// when the checkout is absent or has changed.
func (l *GitLoader) candidateFromEntries(ctx context.Context, entries []CatalogEntry, store ArtifactStore) (*CatalogCandidate, error) {
	if !l.strictRuntime || store == nil {
		return nil, errors.New("persisted catalog adoption requires a strict loader and artifact store")
	}
	if len(entries) == 0 || len(entries) > maxRuntimeCatalogCount {
		return nil, errors.New("persisted catalog has an invalid entry count")
	}
	l.catalogMu.RLock()
	baseGeneration := l.generation
	l.catalogMu.RUnlock()

	ids := make([]string, 0, len(entries))
	bundles := make(map[string]RuntimeBundle, len(entries))
	artifacts := make(map[string]CapturedRuntimeArtifact, len(entries))
	artifactBytes := make(map[string][]byte, len(entries))
	artifactRefs := make(map[string]ArtifactRef, len(entries))
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		id := entry.Problem.ID
		if !validProblemID(id) || entry.Problem.Revision == "" || entry.SourceTrust != BundleSourceDevelopmentCheckout {
			return nil, fmt.Errorf("persisted catalog contains an invalid entry for %q", id)
		}
		if _, duplicate := bundles[id]; duplicate {
			return nil, fmt.Errorf("persisted catalog contains duplicate problem %q", id)
		}
		data, err := store.Get(ctx, entry.Artifact)
		if err != nil {
			return nil, fmt.Errorf("load persisted runtime artifact for %q: %w", id, err)
		}
		artifact, bundle, err := DecodeRuntimeArtifact(entry.Artifact, data)
		if err != nil {
			return nil, fmt.Errorf("decode persisted runtime artifact for %q: %w", id, err)
		}
		if bundle.Problem.ID != id || bundle.Revision != entry.Problem.Revision ||
			bundle.SourceTrust != entry.SourceTrust || !sameRuntimeProblemProjection(bundle.Problem, entry.Problem) {
			return nil, fmt.Errorf("%w: persisted runtime artifact for %q does not match its ledger entry", ErrCatalogStartupMismatch, id)
		}
		ids = append(ids, id)
		bundles[id] = bundle
		artifacts[id] = artifact
		artifactBytes[id] = append([]byte(nil), data...)
		artifactRefs[id] = entry.Artifact
	}
	sort.Strings(ids)
	return &CatalogCandidate{
		owner:          l,
		baseGeneration: baseGeneration,
		ids:            ids,
		bundles:        bundles,
		artifacts:      artifacts,
		artifactBytes:  artifactBytes,
		artifactRefs:   artifactRefs,
	}, nil
}

func sameRuntimeProblemProjection(runtime, persisted models.Problem) bool {
	persisted.CatalogActive = false
	persisted.CreatedAt = runtime.CreatedAt
	persisted.UpdatedAt = runtime.UpdatedAt
	return reflect.DeepEqual(runtime, persisted)
}

// Identity returns a canonical digest of the exact problem/revision membership
// in this candidate. The encoding is domain-separated, schema-versioned,
// sorted, counted, and length-prefixed so neither map iteration nor delimiter
// ambiguity can change or collide the inventory representation.
func (c *CatalogCandidate) Identity() (CatalogPublicationRef, error) {
	if c == nil {
		return CatalogPublicationRef{}, errors.New("catalog candidate is nil")
	}
	if len(c.ids) != len(c.bundles) {
		return CatalogPublicationRef{}, errors.New("catalog candidate inventory is inconsistent")
	}

	ids := append([]string(nil), c.ids...)
	sort.Strings(ids)
	h := sha256.New()
	_, _ = h.Write([]byte(catalogCandidateDigestDomain))
	writeCatalogUint16(h, catalogCandidateDigestSchema)
	writeCatalogUint64(h, uint64(len(ids)))
	for index, id := range ids {
		if index > 0 && ids[index-1] == id {
			return CatalogPublicationRef{}, fmt.Errorf("catalog candidate contains duplicate problem %q", id)
		}
		bundle, found := c.bundles[id]
		if !found || bundle.Problem.ID != id || bundle.Revision == "" || bundle.Problem.Revision != bundle.Revision {
			return CatalogPublicationRef{}, fmt.Errorf("catalog candidate contains an invalid bundle for %q", id)
		}
		writeCatalogString(h, id)
		writeCatalogString(h, bundle.Revision)
		artifactRef, found := c.artifactRefs[id]
		if !found || artifactRef.DigestSchema == 0 || artifactRef.MediaType == "" || artifactRef.Size <= 0 {
			return CatalogPublicationRef{}, fmt.Errorf("catalog candidate contains no artifact reference for %q", id)
		}
		writeCatalogUint16(h, artifactRef.DigestSchema)
		_, _ = h.Write(artifactRef.Digest[:])
		writeCatalogString(h, artifactRef.MediaType)
		writeCatalogUint64(h, uint64(artifactRef.Size))
		writeCatalogString(h, string(bundle.SourceTrust))
	}

	ref := CatalogPublicationRef{DigestSchema: catalogCandidateDigestSchema}
	copy(ref.CandidateDigest[:], h.Sum(nil))
	return ref, nil
}

// legacyIdentity reproduces the version-11 metadata-only catalog digest. It is
// used only to recognize an unmapped legacy head for an exact, verified
// upgrade; all new publications use Identity's artifact-bearing schema v2.
func (c *CatalogCandidate) legacyIdentity() (CatalogPublicationRef, error) {
	if c == nil || len(c.ids) != len(c.bundles) {
		return CatalogPublicationRef{}, errors.New("catalog candidate inventory is inconsistent")
	}
	ids := append([]string(nil), c.ids...)
	sort.Strings(ids)
	h := sha256.New()
	_, _ = h.Write([]byte(catalogCandidateDigestDomain))
	writeCatalogUint16(h, legacyCatalogDigestSchema)
	writeCatalogUint64(h, uint64(len(ids)))
	for index, id := range ids {
		if index > 0 && ids[index-1] == id {
			return CatalogPublicationRef{}, fmt.Errorf("catalog candidate contains duplicate problem %q", id)
		}
		bundle, found := c.bundles[id]
		if !found || bundle.Problem.ID != id || bundle.Revision == "" || bundle.Problem.Revision != bundle.Revision {
			return CatalogPublicationRef{}, fmt.Errorf("catalog candidate contains an invalid bundle for %q", id)
		}
		writeCatalogString(h, id)
		writeCatalogString(h, bundle.Revision)
	}
	ref := CatalogPublicationRef{DigestSchema: legacyCatalogDigestSchema}
	copy(ref.CandidateDigest[:], h.Sum(nil))
	return ref, nil
}

func writeCatalogString(w io.Writer, value string) {
	writeCatalogUint64(w, uint64(len(value)))
	_, _ = io.WriteString(w, value)
}

func writeCatalogUint16(w io.Writer, value uint16) {
	var encoded [2]byte
	binary.BigEndian.PutUint16(encoded[:], value)
	_, _ = w.Write(encoded[:])
}

func writeCatalogUint64(w io.Writer, value uint64) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], value)
	_, _ = w.Write(encoded[:])
}

// prepareActivation serializes publishers and validates candidate ownership,
// freshness, and revision bindings. It intentionally does not take catalogMu
// for the lifetime of a database transaction, so existing exact-revision
// resolutions continue while new-start admission is paused by the coordinator.
func (l *GitLoader) prepareActivation(candidate *CatalogCandidate) (*catalogActivationToken, error) {
	if candidate == nil {
		return nil, errors.New("runtime catalog candidate is nil")
	}
	ref, err := candidate.Identity()
	if err != nil {
		return nil, err
	}
	return l.prepareActivationAt(candidate, CatalogHead{
		Generation: candidate.baseGeneration + 1,
		Ref:        ref,
		EntryCount: len(candidate.ids),
	})
}

// prepareActivationAt installs the exact PostgreSQL publication generation.
// Startup adoption may therefore restore generation N without replaying N
// process-local commits. Callers must hold the global publication lease.
func (l *GitLoader) prepareActivationAt(candidate *CatalogCandidate, target CatalogHead) (*catalogActivationToken, error) {
	if !l.strictRuntime {
		return nil, errors.New("catalog activation requires a strict runtime loader")
	}
	if candidate == nil || candidate.owner != l {
		return nil, errors.New("runtime catalog candidate belongs to a different loader")
	}
	identity, err := candidate.Identity()
	if err != nil {
		return nil, err
	}
	identityMatches := target.Ref == identity
	if !identityMatches && target.Ref.DigestSchema == legacyCatalogDigestSchema {
		legacy, legacyErr := candidate.legacyIdentity()
		identityMatches = legacyErr == nil && target.Ref == legacy
	}
	if target.Generation == 0 || !identityMatches || target.EntryCount != len(candidate.ids) {
		return nil, errors.New("runtime catalog activation target does not match candidate")
	}
	l.publicationMu.Lock()
	l.catalogMu.RLock()
	defer l.catalogMu.RUnlock()
	if candidate.baseGeneration != l.generation {
		l.publicationMu.Unlock()
		return nil, errors.New("runtime catalog candidate is stale")
	}
	for _, id := range candidate.ids {
		bundle := candidate.bundles[id]
		if bundle.Problem.ID != id || bundle.Revision == "" || bundle.Problem.Revision != bundle.Revision {
			l.publicationMu.Unlock()
			return nil, fmt.Errorf("runtime catalog candidate contains an invalid bundle for %q", id)
		}
		if existing, found := l.history[id][bundle.Revision]; found && !reflect.DeepEqual(existing, bundle) {
			l.publicationMu.Unlock()
			return nil, fmt.Errorf("runtime revision %s for problem %s is already bound to different content", bundle.Revision, id)
		}
	}
	return &catalogActivationToken{state: &catalogActivationState{loader: l, candidate: candidate, target: target}}, nil
}

// Commit atomically publishes the prepared candidate in memory. All
// fallible validation happened in prepareActivation.
func (a *catalogActivationToken) Commit() error {
	if a == nil || a.state == nil {
		return ErrInvalidCatalogActivation
	}
	state := a.state
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.loader == nil || state.candidate == nil {
		return ErrInvalidCatalogActivation
	}
	if state.consumed {
		return ErrCatalogActivationConsumed
	}
	state.consumed = true

	l := state.loader
	candidate := state.candidate
	l.catalogMu.Lock()
	active := make(map[string]string, len(candidate.ids))
	for _, id := range candidate.ids {
		bundle := cloneRuntimeBundle(candidate.bundles[id])
		if l.history[id] == nil {
			l.history[id] = make(map[string]RuntimeBundle)
		}
		l.history[id][bundle.Revision] = bundle
		active[id] = bundle.Revision
	}
	l.active = active
	l.generation = state.target.Generation
	head := state.target
	l.activeHead = &head
	l.catalogMu.Unlock()
	l.publicationMu.Unlock()
	return nil
}

func (l *GitLoader) catalogHead() *CatalogHead {
	l.catalogMu.RLock()
	defer l.catalogMu.RUnlock()
	if l.activeHead == nil {
		return nil
	}
	head := *l.activeHead
	return &head
}

// Abort releases a prepared publication after a database rollback.
func (a *catalogActivationToken) Abort() error {
	if a == nil || a.state == nil {
		return ErrInvalidCatalogActivation
	}
	state := a.state
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.loader == nil {
		return ErrInvalidCatalogActivation
	}
	if state.consumed {
		return ErrCatalogActivationConsumed
	}
	state.consumed = true
	state.loader.publicationMu.Unlock()
	return nil
}

// GetRuntimeBundle reads and hashes the approved problem files. File names
// and explicit missing-file markers are included in the digest so adding or
// removing an optional script or hint always changes the revision.
func (l *GitLoader) GetRuntimeBundle(problemID string) (RuntimeBundle, error) {
	return l.getRuntimeBundle(context.Background(), problemID)
}

func (l *GitLoader) getRuntimeBundle(ctx context.Context, problemID string) (RuntimeBundle, error) {
	if err := ctx.Err(); err != nil {
		return RuntimeBundle{}, err
	}
	if !validProblemID(problemID) {
		return RuntimeBundle{}, fmt.Errorf("invalid problem id %q", problemID)
	}

	if l.strictRuntime {
		return l.captureRuntimeBundle(ctx, problemID)
	}
	files, err := l.readAuthoringProblemFiles(problemID)
	if err != nil {
		return RuntimeBundle{}, err
	}
	return l.buildRuntimeBundle(ctx, problemID, files, nil, approvedImagePolicy{})
}

func (l *GitLoader) readAuthoringProblemFiles(problemID string) (map[string][]byte, error) {
	files := make(map[string][]byte, 4)
	for _, name := range []string{"problem.yaml", "setup.sh", "verify.sh", "hint.md"} {
		path, err := l.safeJoin(problemID, name)
		if err != nil {
			return nil, err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) && name != "problem.yaml" {
				files[name] = nil
				continue
			}
			return nil, fmt.Errorf("read %s for problem %s: %w", name, problemID, err)
		}
		files[name] = data
	}
	return files, nil
}

func (l *GitLoader) buildRuntimeBundle(ctx context.Context, problemID string, files map[string][]byte, imageLock []byte, imagePolicy approvedImagePolicy) (RuntimeBundle, error) {

	var py models.ProblemYAML
	if err := yaml.Unmarshal(files["problem.yaml"], &py); err != nil {
		return RuntimeBundle{}, fmt.Errorf("parse problem.yaml for %s: %w", problemID, err)
	}
	p := models.Problem{
		ID:             py.ID,
		Title:          py.Title,
		Description:    py.Description,
		Category:       py.Category,
		Difficulty:     py.Difficulty,
		Type:           py.Type,
		TimeoutMinutes: py.TimeoutMinutes,
		VerifyType:     py.VerifyType,
		BaseImage:      py.BaseImage,
		Image:          py.Image,
		Choices:        py.Choices,
		CorrectChoice:  py.CorrectChoice,
		GradingPrompt:  py.GradingPrompt,
		Hint:           string(files["hint.md"]),
	}
	if p.TimeoutMinutes == 0 {
		p.TimeoutMinutes = 30
	}
	if p.BaseImage == "" && p.Image == "" {
		p.BaseImage = models.DefaultBaseImage
	}
	if l.strictRuntime {
		if err := validateApprovedImagePolicy(files, imagePolicy); err != nil {
			return RuntimeBundle{}, fmt.Errorf("validate workload images for problem %s: %w", problemID, err)
		}
		if p.VerifyType == "script" && strings.TrimSpace(string(files["verify.sh"])) == "" {
			return RuntimeBundle{}, fmt.Errorf("script problem %s requires a non-empty verify.sh", problemID)
		}
	}

	runtimeImage := ""
	if l.imageResolver != nil {
		resolved, err := l.imageResolver.ResolveImage(ctx, p.EffectiveImage())
		if err != nil {
			return RuntimeBundle{}, fmt.Errorf("resolve image %q for problem %s: %w", p.EffectiveImage(), problemID, err)
		}
		if !validImageContentID(resolved) {
			return RuntimeBundle{}, fmt.Errorf("resolve image %q for problem %s: non-content-addressed ID %q", p.EffectiveImage(), problemID, resolved)
		}
		runtimeImage = resolved
	}

	h := sha256.New()
	for _, name := range []string{"problem.yaml", "setup.sh", "verify.sh", "hint.md"} {
		data, present := files[name]
		presence := byte(0)
		if present && data != nil {
			presence = 1
		}
		fmt.Fprintf(h, "%s\x00%d\x00%d\x00", name, presence, len(data))
		if present && data != nil {
			h.Write(data)
		}
		h.Write([]byte{0})
	}
	if imageLock != nil {
		fmt.Fprintf(h, "images.lock\x001\x00%d\x00", len(imageLock))
		h.Write(imageLock)
		h.Write([]byte{0})
	}
	if runtimeImage != "" {
		fmt.Fprintf(h, "runtime-image-content-id\x001\x00%d\x00", len(runtimeImage))
		h.Write([]byte(runtimeImage))
		h.Write([]byte{0})
	}
	revision := hex.EncodeToString(h.Sum(nil))
	p.Revision = revision

	return RuntimeBundle{
		Problem:      p,
		SetupScript:  string(files["setup.sh"]),
		VerifyScript: string(files["verify.sh"]),
		Revision:     revision,
		RuntimeImage: runtimeImage,
		SourceTrust:  BundleSourceDevelopmentCheckout,
	}, nil
}

func (l *GitLoader) captureRuntimeCatalog(ctx context.Context) (map[string]CapturedRuntimeArtifact, error) {
	root, err := openRuntimeDirectory(l.clean)
	if err != nil {
		return nil, fmt.Errorf("open runtime problem catalog: %w", err)
	}
	defer root.Close()

	imageLock, err := readRuntimeRegularFile(int(root.Fd()), "images.lock", true, maxImageLockBytes)
	if err != nil {
		return nil, fmt.Errorf("read runtime images.lock: %w", err)
	}
	imagePolicy, err := parseApprovedImagePolicy(imageLock)
	if err != nil {
		return nil, fmt.Errorf("parse images.lock: %w", err)
	}

	entries, err := root.ReadDir(-1)
	if err != nil {
		return nil, fmt.Errorf("read runtime catalog directory: %w", err)
	}
	artifacts := make(map[string]CapturedRuntimeArtifact)
	totalBytes := len(imageLock)
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		name := entry.Name()
		info, err := entry.Info()
		if err != nil {
			return nil, fmt.Errorf("inspect runtime catalog entry %q: %w", name, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("runtime catalog entry %q must not be a symbolic link", name)
		}
		if strings.HasPrefix(name, ".") || !info.IsDir() {
			if !info.IsDir() && !info.Mode().IsRegular() {
				return nil, fmt.Errorf("runtime catalog entry %q has unsupported file type %s", name, info.Mode().Type())
			}
			continue
		}

		problemDir, err := openRuntimeDirectoryAt(int(root.Fd()), name)
		if err != nil {
			return nil, fmt.Errorf("open runtime catalog directory %q: %w", name, err)
		}
		manifest, manifestErr := readRuntimeRegularFile(int(problemDir.Fd()), "problem.yaml", false, maxProblemManifestBytes)
		if manifestErr != nil {
			problemDir.Close()
			return nil, fmt.Errorf("inspect runtime problem %q manifest: %w", name, manifestErr)
		}
		if manifest == nil {
			problemDir.Close()
			continue
		}
		if !validProblemID(name) {
			problemDir.Close()
			return nil, fmt.Errorf("runtime problem directory %q is not a valid problem id", name)
		}
		files, err := readRuntimeProblemFiles(problemDir, manifest)
		problemDir.Close()
		if err != nil {
			return nil, fmt.Errorf("read runtime problem %q: %w", name, err)
		}
		for _, data := range files {
			totalBytes += len(data)
			if totalBytes > maxRuntimeCatalogBytes {
				return nil, fmt.Errorf("runtime problem catalog exceeds %d bytes", maxRuntimeCatalogBytes)
			}
		}
		bundle, err := l.buildRuntimeBundle(ctx, name, files, imageLock, imagePolicy)
		if err != nil {
			return nil, fmt.Errorf("load runtime problem %q: %w", name, err)
		}
		if bundle.Problem.ID != name {
			return nil, fmt.Errorf("runtime problem directory %q contains id %q", name, bundle.Problem.ID)
		}
		if msg := validateLoadedProblem(&bundle.Problem); msg != "" {
			return nil, fmt.Errorf("runtime problem %q is invalid: %s", name, msg)
		}
		artifact, err := NewCapturedRuntimeArtifact(files, imageLock, bundle)
		if err != nil {
			return nil, fmt.Errorf("capture runtime artifact %q: %w", name, err)
		}
		if _, duplicate := artifacts[name]; duplicate {
			return nil, fmt.Errorf("runtime problem %q appears more than once", name)
		}
		artifacts[name] = artifact
		if len(artifacts) > maxRuntimeCatalogCount {
			return nil, fmt.Errorf("runtime problem catalog exceeds %d problems", maxRuntimeCatalogCount)
		}
	}
	return artifacts, nil
}

func (l *GitLoader) captureRuntimeBundle(ctx context.Context, problemID string) (RuntimeBundle, error) {
	root, err := openRuntimeDirectory(l.clean)
	if err != nil {
		return RuntimeBundle{}, fmt.Errorf("open runtime problem catalog: %w", err)
	}
	defer root.Close()
	imageLock, err := readRuntimeRegularFile(int(root.Fd()), "images.lock", true, maxImageLockBytes)
	if err != nil {
		return RuntimeBundle{}, fmt.Errorf("read runtime images.lock: %w", err)
	}
	policy, err := parseApprovedImagePolicy(imageLock)
	if err != nil {
		return RuntimeBundle{}, fmt.Errorf("parse images.lock: %w", err)
	}
	problemDir, err := openRuntimeDirectoryAt(int(root.Fd()), problemID)
	if err != nil {
		return RuntimeBundle{}, fmt.Errorf("open runtime problem %q: %w", problemID, err)
	}
	defer problemDir.Close()
	manifest, err := readRuntimeRegularFile(int(problemDir.Fd()), "problem.yaml", true, maxProblemManifestBytes)
	if err != nil {
		return RuntimeBundle{}, fmt.Errorf("read problem.yaml for problem %s: %w", problemID, err)
	}
	files, err := readRuntimeProblemFiles(problemDir, manifest)
	if err != nil {
		return RuntimeBundle{}, err
	}
	return l.buildRuntimeBundle(ctx, problemID, files, imageLock, policy)
}

func readRuntimeProblemFiles(dir *os.File, manifest []byte) (map[string][]byte, error) {
	files := map[string][]byte{"problem.yaml": append([]byte(nil), manifest...)}
	limits := map[string]int64{
		"setup.sh": maxProblemScriptBytes, "verify.sh": maxProblemScriptBytes, "hint.md": maxProblemHintBytes,
	}
	for _, name := range []string{"setup.sh", "verify.sh", "hint.md"} {
		data, err := readRuntimeRegularFile(int(dir.Fd()), name, false, limits[name])
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", name, err)
		}
		files[name] = data
	}
	return files, nil
}

func openRuntimeDirectory(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}

func openRuntimeDirectoryAt(parentFD int, name string) (*os.File, error) {
	if name == "" || name == "." || name == ".." || filepath.Base(name) != name {
		return nil, errors.New("runtime directory name is not canonical")
	}
	fd, err := unix.Openat(parentFD, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), name), nil
}

func readRuntimeRegularFile(parentFD int, name string, required bool, maxBytes int64) ([]byte, error) {
	if name == "" || name == "." || name == ".." || filepath.Base(name) != name {
		return nil, errors.New("runtime file name is not canonical")
	}
	fd, err := unix.Openat(parentFD, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		if !required && errors.Is(err, unix.ENOENT) {
			return nil, nil
		}
		return nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	defer file.Close()
	var before unix.Stat_t
	if err := unix.Fstat(fd, &before); err != nil {
		return nil, err
	}
	if before.Mode&unix.S_IFMT != unix.S_IFREG {
		return nil, fmt.Errorf("%s must be a regular file", name)
	}
	if before.Nlink != 1 {
		return nil, fmt.Errorf("%s must not be a hard-linked file", name)
	}
	if before.Size < 0 || before.Size > maxBytes {
		return nil, fmt.Errorf("%s exceeds the %d byte limit", name, maxBytes)
	}
	data, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("%s exceeds the %d byte limit", name, maxBytes)
	}
	var after unix.Stat_t
	if err := unix.Fstat(fd, &after); err != nil {
		return nil, err
	}
	if before.Dev != after.Dev || before.Ino != after.Ino || before.Mode != after.Mode || before.Nlink != after.Nlink ||
		before.Size != after.Size || before.Mtim != after.Mtim || before.Ctim != after.Ctim {
		return nil, fmt.Errorf("%s changed while it was being read", name)
	}
	return data, nil
}

func cloneProblem(problem models.Problem) models.Problem {
	copy := problem
	copy.Choices = append([]models.Choice(nil), problem.Choices...)
	return copy
}

func cloneRuntimeBundle(bundle RuntimeBundle) RuntimeBundle {
	copy := bundle
	copy.Problem = cloneProblem(bundle.Problem)
	return copy
}

func validImageContentID(value string) bool {
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

type approvedImagePolicy struct {
	refs    map[string]struct{}
	aliases map[string]string
}

func parseApprovedImagePolicy(data []byte) (approvedImagePolicy, error) {
	policy := approvedImagePolicy{refs: make(map[string]struct{}), aliases: make(map[string]string)}
	for index, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, ref, ok := strings.Cut(line, "=")
		name, ref = strings.TrimSpace(name), strings.TrimSpace(ref)
		if !ok || name == "" || ref == "" || strings.ContainsAny(name, " \t") {
			return approvedImagePolicy{}, fmt.Errorf("images.lock:%d must be name=registry@sha256:<64 lowercase hex>", index+1)
		}
		if !validPinnedImageReference(ref) {
			return approvedImagePolicy{}, fmt.Errorf("images.lock:%d image %q is not digest-pinned", index+1, ref)
		}
		if _, exists := policy.aliases[name]; exists {
			return approvedImagePolicy{}, fmt.Errorf("images.lock:%d duplicates name %q", index+1, name)
		}
		if _, exists := policy.refs[ref]; exists {
			return approvedImagePolicy{}, fmt.Errorf("images.lock:%d duplicates image %q", index+1, ref)
		}
		policy.aliases[name] = ref
		policy.refs[ref] = struct{}{}
	}
	if len(policy.refs) == 0 {
		return approvedImagePolicy{}, errors.New("images.lock contains no approved images")
	}
	return policy, nil
}

// validateApprovedImagePolicy covers images created by approved setup and
// verify scripts as well as the learner-facing contract. These images are
// pulled by k3s inside the session and sit outside the outer runtime binding.
// Static shell review is necessarily conservative: unsupported mutation paths
// fail closed and public enforcement still requires registry/admission policy.
func validateApprovedImagePolicy(files map[string][]byte, policy approvedImagePolicy) error {
	for _, name := range []string{"problem.yaml", "setup.sh", "verify.sh", "hint.md"} {
		for index, line := range strings.Split(string(files[name]), "\n") {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" || strings.HasPrefix(trimmed, "#") {
				continue
			}
			if name == "setup.sh" || name == "verify.sh" {
				for _, pattern := range unsupportedImageOps {
					if pattern.MatchString(line) {
						return fmt.Errorf("%s:%d uses an unsupported image mutation or remote manifest path", name, index+1)
					}
				}
			}
			for _, pattern := range workloadImagePatterns {
				for _, match := range pattern.FindAllStringSubmatch(line, -1) {
					if len(match) < 2 {
						continue
					}
					ref := strings.TrimSpace(match[1])
					if _, approved := policy.refs[ref]; !approved {
						return fmt.Errorf("%s:%d workload image %q is not an exact images.lock entry", name, index+1, ref)
					}
				}
			}
			for _, ref := range imageDigestReference.FindAllString(line, -1) {
				if _, approved := policy.refs[ref]; !approved {
					return fmt.Errorf("%s:%d digest-pinned image %q is not approved by images.lock", name, index+1, ref)
				}
			}
		}
		contents := string(files[name])
		for _, approved := range policy.aliases {
			alias := strings.SplitN(approved, "@sha256:", 2)[0]
			if !strings.Contains(alias, ":") {
				continue
			}
			remaining := contents
			for {
				position := strings.Index(remaining, alias)
				if position < 0 {
					break
				}
				suffix := remaining[position:]
				if !strings.HasPrefix(suffix, approved) {
					return fmt.Errorf("%s mentions mutable or mismatched locked image alias %q", name, alias)
				}
				remaining = suffix[len(approved):]
			}
		}
	}
	return nil
}

func validPinnedImageReference(ref string) bool {
	const marker = "@sha256:"
	index := strings.LastIndex(ref, marker)
	return index > 0 && validImageContentID(ref[index+1:])
}

// ResolveRuntime implements runner.LocalDockerRuntimeCatalog. It validates
// the immutable revision requested by the Control Plane and returns a single
// trusted snapshot; provider mechanism never comes from the browser request.
func (l *GitLoader) ResolveRuntime(ctx context.Context, ref runner.ProblemRef) (runner.LocalDockerRuntime, error) {
	if err := ctx.Err(); err != nil {
		return runner.LocalDockerRuntime{}, err
	}
	if l.strictRuntime {
		bundle, found := l.resolveCatalogBundle(ref)
		if !found {
			return runner.LocalDockerRuntime{}, runner.ErrInvalidRevision
		}
		return localRuntimeFromBundle(bundle), nil
	}
	bundle, err := l.getRuntimeBundle(ctx, ref.ID)
	if err != nil {
		return runner.LocalDockerRuntime{}, err
	}
	if ref.Revision == "" || bundle.Revision != ref.Revision {
		return runner.LocalDockerRuntime{}, runner.ErrInvalidRevision
	}
	return localRuntimeFromBundle(bundle), nil
}

// ResolveProblem returns grading and answer metadata from the exact approved
// bundle revision. This prevents mutable database/admin metadata from
// changing verification semantics underneath an approved revision.
func (l *GitLoader) ResolveProblem(ctx context.Context, ref runner.ProblemRef) (models.Problem, error) {
	if err := ctx.Err(); err != nil {
		return models.Problem{}, err
	}
	if l.strictRuntime {
		bundle, found := l.resolveCatalogBundle(ref)
		if !found {
			return models.Problem{}, runner.ErrInvalidRevision
		}
		return cloneProblem(bundle.Problem), nil
	}
	bundle, err := l.getRuntimeBundle(ctx, ref.ID)
	if err != nil {
		return models.Problem{}, err
	}
	if ref.Revision == "" || bundle.Revision != ref.Revision {
		return models.Problem{}, runner.ErrInvalidRevision
	}
	return cloneProblem(bundle.Problem), nil
}

// ResolveActiveProblem is the new-session admission lookup. Historical
// ResolveProblem remains available for durable replay, reset, and recovery,
// but a fresh session may use only the revision in the current active map.
func (l *GitLoader) ResolveActiveProblem(ctx context.Context, ref runner.ProblemRef) (models.Problem, error) {
	if err := ctx.Err(); err != nil {
		return models.Problem{}, err
	}
	if !l.strictRuntime {
		return l.ResolveProblem(ctx, ref)
	}
	if !validProblemID(ref.ID) || ref.Revision == "" {
		return models.Problem{}, runner.ErrInvalidRevision
	}
	l.catalogMu.RLock()
	activeRevision, active := l.active[ref.ID]
	bundle, found := l.history[ref.ID][ref.Revision]
	l.catalogMu.RUnlock()
	if !active || activeRevision != ref.Revision || !found {
		return models.Problem{}, runner.ErrInvalidRevision
	}
	return cloneProblem(bundle.Problem), nil
}

func (l *GitLoader) resolveCatalogBundle(ref runner.ProblemRef) (RuntimeBundle, bool) {
	if !validProblemID(ref.ID) || ref.Revision == "" {
		return RuntimeBundle{}, false
	}
	l.catalogMu.RLock()
	versions := l.history[ref.ID]
	bundle, found := versions[ref.Revision]
	l.catalogMu.RUnlock()
	if !found {
		return RuntimeBundle{}, false
	}
	return cloneRuntimeBundle(bundle), true
}

// installHistoricalBundle adds one CAS-decoded revision without changing the
// active head. It is safe for concurrent recovery/verify calls and refuses a
// second byte representation for the same durable problem revision.
func (l *GitLoader) installHistoricalBundle(bundle RuntimeBundle) error {
	if !l.strictRuntime || !validProblemID(bundle.Problem.ID) || bundle.Revision == "" ||
		bundle.Problem.Revision != bundle.Revision {
		return runner.ErrInvalidRevision
	}
	l.catalogMu.Lock()
	defer l.catalogMu.Unlock()
	versions := l.history[bundle.Problem.ID]
	if versions == nil {
		versions = make(map[string]RuntimeBundle)
		l.history[bundle.Problem.ID] = versions
	}
	if existing, found := versions[bundle.Revision]; found {
		if !reflect.DeepEqual(existing, bundle) {
			return fmt.Errorf("runtime revision %s for problem %s is already bound to different content", bundle.Revision, bundle.Problem.ID)
		}
		return nil
	}
	versions[bundle.Revision] = cloneRuntimeBundle(bundle)
	return nil
}

func localRuntimeFromBundle(bundle RuntimeBundle) runner.LocalDockerRuntime {
	image := bundle.RuntimeImage
	if image == "" {
		image = bundle.Problem.EffectiveImage()
	}
	return runner.LocalDockerRuntime{
		Revision:     bundle.Revision,
		Image:        image,
		SetupScript:  bundle.SetupScript,
		VerifyScript: bundle.VerifyScript,
		VerifyType:   bundle.Problem.VerifyType,
	}
}

func (l *GitLoader) GetProblemDir(problemID string) string {
	p, err := l.safeJoin(problemID, "")
	if err != nil {
		return ""
	}
	return p
}

func (l *GitLoader) HasSetupScript(problemID string) bool {
	path, err := l.safeJoin(problemID, "setup.sh")
	if err != nil {
		return false
	}
	_, err = os.Stat(path)
	return err == nil
}

func (l *GitLoader) HasVerifyScript(problemID string) bool {
	path, err := l.safeJoin(problemID, "verify.sh")
	if err != nil {
		return false
	}
	_, err = os.Stat(path)
	return err == nil
}

func (l *GitLoader) GetSetupScript(problemID string) (string, error) {
	path, err := l.safeJoin(problemID, "setup.sh")
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func (l *GitLoader) GetVerifyScript(problemID string) (string, error) {
	path, err := l.safeJoin(problemID, "verify.sh")
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// validateLoadedProblem enforces the id format, enum values, and choice
// rules on a problem parsed from YAML (PROB-12). It returns "" when valid.
func validateLoadedProblem(p *models.Problem) string {
	if msg := validateProblem(p); msg != "" {
		return msg
	}
	if p.VerifyType == "choice" {
		if len(p.Choices) < 2 {
			return "choice problem needs at least 2 choices"
		}
		found := false
		for _, ch := range p.Choices {
			if ch.ID == p.CorrectChoice {
				found = true
				break
			}
		}
		if !found {
			return "correct_choice must reference one of the choices"
		}
	}
	return ""
}
