package runner

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestProviderOperationKeyContract(t *testing.T) {
	t.Parallel()
	if err := ValidateProviderOperationKey(strings.Repeat("a", MaxProviderOperationKeyLength)); err != nil {
		t.Fatalf("maximum-length safe key rejected: %v", err)
	}
	for _, value := range []string{
		"", strings.Repeat("a", MaxProviderOperationKeyLength+1), "contains space", "contains/slash", "line\nbreak",
	} {
		if err := ValidateProviderOperationKey(value); err == nil {
			t.Fatalf("unsafe provider operation key accepted: %q", value)
		}
	}
}

func TestAllocationIDForSessionStableAndGenerationBound(t *testing.T) {
	ref := SessionRef{SessionID: "11111111-1111-4111-8111-111111111111", Generation: 1}
	first := AllocationIDForSession(ref)
	if first != AllocationIDForSession(ref) {
		t.Fatal("allocation identity is not deterministic")
	}
	ref.Generation++
	if first == AllocationIDForSession(ref) {
		t.Fatal("allocation identity did not change with generation")
	}
	if len(first) != 36 || first[14] != '5' {
		t.Fatalf("allocation identity is not a UUIDv5-shaped value: %q", first)
	}
}

func TestReservationHashBindsDurableInputs(t *testing.T) {
	params := ReserveSessionParams{
		SessionID: "11111111-1111-4111-8111-111111111111", UserID: "22222222-2222-4222-8222-222222222222",
		Selection: CatalogSelection{Generation: 1, Problem: ProblemRef{ID: "pod-crash", Revision: "revision-1"}},
		Provider:  ProviderLocalDocker, ProviderID: "local-docker:test", ResourceProfile: DefaultResourceProfile,
		ExpiresAt: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), IdempotencyKey: "create-stable",
	}
	allocationID := AllocationIDForSession(SessionRef{SessionID: params.SessionID, Generation: 1})
	first, err := hashReservation(params, allocationID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := hashReservation(params, allocationID)
	if err != nil || first != second {
		t.Fatalf("reservation hash is unstable: %x %x err=%v", first, second, err)
	}
	params.Selection.Problem.Revision = "revision-2"
	changed, err := hashReservation(params, allocationID)
	if err != nil {
		t.Fatal(err)
	}
	if first == changed {
		t.Fatal("problem revision was not bound into reservation hash")
	}
	params.Selection.Problem.Revision = "revision-1"
	params.Selection.Generation = 2
	changedGeneration, err := hashReservation(params, allocationID)
	if err != nil {
		t.Fatal(err)
	}
	if first == changedGeneration {
		t.Fatal("catalog generation was not bound into reservation hash")
	}
}

func TestReservationHashBindsCanonicalExpiryInstant(t *testing.T) {
	params := ReserveSessionParams{
		SessionID: "11111111-1111-4111-8111-111111111111", UserID: "22222222-2222-4222-8222-222222222222",
		Selection: CatalogSelection{Generation: 1, Problem: ProblemRef{ID: "pod-crash", Revision: "revision-1"}}, Provider: ProviderLocalDocker,
		ProviderID: "local-docker:test", ResourceProfile: DefaultResourceProfile,
		ExpiresAt: time.Date(2026, 8, 1, 0, 0, 0, 123, time.UTC), IdempotencyKey: "create-stable",
	}
	allocationID := AllocationIDForSession(SessionRef{SessionID: params.SessionID, Generation: 1})
	first, err := hashReservation(params, allocationID)
	if err != nil {
		t.Fatal(err)
	}
	params.ExpiresAt = params.ExpiresAt.In(time.FixedZone("same-instant", 9*60*60))
	replay, err := hashReservation(params, allocationID)
	if err != nil {
		t.Fatal(err)
	}
	if first != replay {
		t.Fatal("same expiry instant in another location changed the reservation hash")
	}
	params.ExpiresAt = params.ExpiresAt.Add(time.Nanosecond)
	changed, err := hashReservation(params, allocationID)
	if err != nil {
		t.Fatal(err)
	}
	if first == changed {
		t.Fatal("different expiry instant did not conflict with the reservation hash")
	}
}

func TestPostgresStoreRequiresPool(t *testing.T) {
	if _, err := NewPostgresStore(nil, ControllerFence{ProviderID: "local-docker:test", Epoch: 1}); err == nil {
		t.Fatal("nil database pool was accepted")
	}
}

func TestDurableValueBoundsFailClosed(t *testing.T) {
	tooLarge := MaxDurableValue + 1
	if _, ok := ControllerFenceFromContext(withControllerFence(context.Background(), ControllerFence{
		ProviderID: "local-docker:test", Epoch: tooLarge,
	})); ok {
		t.Fatal("oversized controller epoch remained authoritative in context")
	}
	if err := validateReservation(ReserveSessionParams{
		UserID: "user", Selection: CatalogSelection{
			Generation: tooLarge, Problem: ProblemRef{ID: "pod-crash", Revision: "revision"},
		},
		Provider: ProviderLocalDocker, ProviderID: "local-docker:test",
		ResourceProfile: DefaultResourceProfile, ExpiresAt: time.Now().UTC().Add(time.Hour),
		IdempotencyKey: "create-bounds",
	}); err == nil {
		t.Fatal("oversized catalog generation was accepted")
	}
	if err := validateResetReservation(ReserveResetParams{
		Expected: SessionRef{SessionID: "session", Generation: MaxDurableValue},
		UserID:   "user", Provider: ProviderLocalDocker, ProviderID: "local-docker:test",
		IdempotencyKey: "reset-bounds",
	}); err == nil {
		t.Fatal("reset accepted a generation whose successor cannot fit in BIGINT")
	}
}

func TestVerifyDecisionAllowsOnlyNewExecution(t *testing.T) {
	for _, test := range []struct {
		kind VerifyDecisionKind
		want bool
	}{
		{kind: VerifyExecute, want: true},
		{kind: VerifyResume, want: false},
		{kind: VerifyGradeReplay, want: false},
		{kind: VerifyInfrastructureReplay, want: false},
	} {
		if got := (VerifyDecision{Kind: test.kind}).AllowsProviderCall(); got != test.want {
			t.Fatalf("decision %s provider permission=%v, want %v", test.kind, got, test.want)
		}
	}
}
