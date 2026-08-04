package problem

import (
	"crypto/sha256"
	"testing"

	"github.com/k8s-quiz/backend/pkg/models"
)

type catalogIdentityEntry struct {
	id       string
	revision string
}

func TestCatalogCandidateIdentityIsCanonical(t *testing.T) {
	entries := []catalogIdentityEntry{
		{id: "alpha", revision: revisionOf('1')},
		{id: "beta", revision: revisionOf('2')},
	}
	forward := candidateWithIdentityEntries(entries...)
	reverse := candidateWithIdentityEntries(entries[1], entries[0])

	forwardRef, err := forward.Identity()
	if err != nil {
		t.Fatal(err)
	}
	reverseRef, err := reverse.Identity()
	if err != nil {
		t.Fatal(err)
	}
	if forwardRef != reverseRef {
		t.Fatalf("candidate identity depends on input order: forward=%s reverse=%s", forwardRef.DigestString(), reverseRef.DigestString())
	}
	if forwardRef.DigestSchema != catalogCandidateDigestSchema {
		t.Fatalf("digest schema = %d, want %d", forwardRef.DigestSchema, catalogCandidateDigestSchema)
	}
	const want = "sha256:e6c6a35049767fca2584753f86664d16e8a4276f1083ce6ca5025b6e37199702"
	if got := forwardRef.DigestString(); got != want {
		t.Fatalf("candidate digest = %s, want golden %s", got, want)
	}
}

func TestCatalogCandidateIdentityChangesWithInventory(t *testing.T) {
	baseEntries := []catalogIdentityEntry{
		{id: "alpha", revision: revisionOf('1')},
		{id: "beta", revision: revisionOf('2')},
	}
	base := mustCatalogIdentity(t, candidateWithIdentityEntries(baseEntries...))

	tests := []struct {
		name    string
		entries []catalogIdentityEntry
	}{
		{name: "revision bit", entries: []catalogIdentityEntry{{id: "alpha", revision: revisionOf('3')}, baseEntries[1]}},
		{name: "entry removed", entries: baseEntries[:1]},
		{name: "entry added", entries: append(append([]catalogIdentityEntry(nil), baseEntries...), catalogIdentityEntry{id: "gamma", revision: revisionOf('4')})},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			changed := mustCatalogIdentity(t, candidateWithIdentityEntries(test.entries...))
			if changed == base {
				t.Fatalf("inventory change retained digest %s", base.DigestString())
			}
		})
	}
}

func TestCatalogCandidateIdentityUsesLengthPrefixes(t *testing.T) {
	left := mustCatalogIdentity(t, candidateWithIdentityEntries(catalogIdentityEntry{id: "a", revision: "bc"}))
	right := mustCatalogIdentity(t, candidateWithIdentityEntries(catalogIdentityEntry{id: "ab", revision: "c"}))
	if left == right {
		t.Fatalf("ambiguous concatenations shared digest %s", left.DigestString())
	}
}

func TestCatalogCandidateIdentityIsUnaffectedByCallerMutation(t *testing.T) {
	candidate := candidateWithIdentityEntries(catalogIdentityEntry{id: "alpha", revision: revisionOf('1')})
	want := mustCatalogIdentity(t, candidate)
	problems := candidate.Problems()
	problems[0].ID = "mutated"
	problems[0].Revision = revisionOf('f')
	problems[0].Choices = append(problems[0].Choices, models.Choice{ID: "x", Text: "caller mutation"})
	if got := mustCatalogIdentity(t, candidate); got != want {
		t.Fatalf("caller mutation changed candidate identity: got=%s want=%s", got.DigestString(), want.DigestString())
	}
}

func TestCatalogCandidateIdentityRejectsInconsistentInventory(t *testing.T) {
	tests := []struct {
		name      string
		candidate *CatalogCandidate
	}{
		{name: "nil", candidate: nil},
		{name: "missing bundle", candidate: &CatalogCandidate{ids: []string{"alpha"}, bundles: map[string]RuntimeBundle{}}},
		{name: "duplicate id", candidate: &CatalogCandidate{ids: []string{"alpha", "alpha"}, bundles: map[string]RuntimeBundle{"alpha": identityBundle("alpha", revisionOf('1')), "extra": identityBundle("extra", revisionOf('2'))}}},
		{name: "revision mismatch", candidate: &CatalogCandidate{ids: []string{"alpha"}, bundles: map[string]RuntimeBundle{"alpha": {Problem: models.Problem{ID: "alpha", Revision: revisionOf('1')}, Revision: revisionOf('2')}}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := test.candidate.Identity(); err == nil {
				t.Fatal("inconsistent candidate unexpectedly produced an identity")
			}
		})
	}
}

func candidateWithIdentityEntries(entries ...catalogIdentityEntry) *CatalogCandidate {
	ids := make([]string, 0, len(entries))
	bundles := make(map[string]RuntimeBundle, len(entries))
	artifactRefs := make(map[string]ArtifactRef, len(entries))
	for _, entry := range entries {
		ids = append(ids, entry.id)
		bundles[entry.id] = identityBundle(entry.id, entry.revision)
		artifactRefs[entry.id] = identityArtifactRef(entry.id, entry.revision)
	}
	return &CatalogCandidate{ids: ids, bundles: bundles, artifactRefs: artifactRefs}
}

func identityBundle(id, revision string) RuntimeBundle {
	return RuntimeBundle{
		Problem:     models.Problem{ID: id, Revision: revision},
		Revision:    revision,
		SourceTrust: BundleSourceDevelopmentCheckout,
	}
}

func identityArtifactRef(id, revision string) ArtifactRef {
	return ArtifactRef{
		DigestSchema: ArtifactDigestSchemaV1,
		Digest:       sha256.Sum256([]byte(id + "\x00" + revision)),
		MediaType:    RuntimeArtifactMediaTypeV1,
		Size:         int64(len(id) + len(revision) + 1),
	}
}

func revisionOf(value byte) string {
	result := make([]byte, 64)
	for index := range result {
		result[index] = value
	}
	return string(result)
}

func mustCatalogIdentity(t *testing.T, candidate *CatalogCandidate) CatalogPublicationRef {
	t.Helper()
	ref, err := candidate.Identity()
	if err != nil {
		t.Fatal(err)
	}
	return ref
}
