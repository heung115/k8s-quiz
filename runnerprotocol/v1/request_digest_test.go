package v1

import (
	"encoding/hex"
	"testing"
	"time"
)

func TestFormatCanonicalDatabaseUTC(t *testing.T) {
	value := time.Date(2026, 8, 1, 1, 2, 3, 456000000, time.UTC)
	encoded, err := FormatCanonicalDatabaseUTC(value)
	if err != nil || encoded != "2026-08-01T01:02:03.456Z" {
		t.Fatalf("FormatCanonicalDatabaseUTC=%q,%v", encoded, err)
	}
	for _, invalid := range []time.Time{
		{}, value.In(time.FixedZone("other", 3600)), value.Add(time.Nanosecond),
	} {
		if _, err := FormatCanonicalDatabaseUTC(invalid); err == nil {
			t.Fatalf("invalid database time formatted: %v", invalid)
		}
	}
}

func TestRequestDigestGoldenVectors(t *testing.T) {
	const (
		providerID = "home-proxmox-test"
		deadline   = "2026-08-01T00:00:30Z"
		sessionID  = "11111111-1111-4111-8111-111111111111"
		allocation = "bbccedbc-f431-50fe-856e-4d3090d825f0"
	)
	tests := []struct {
		name    string
		request any
		want    string
	}{
		{name: "activate", request: ActivateRequest{
			Schema: Schema, ControllerEpoch: 7, EffectDeadline: deadline,
		}, want: "aeec94ff8c0a130d5f9528984bb566986987aaab9c6dae8f5f2b72a4c4d96faa"},
		{name: "create", request: CreateRequest{
			Schema: Schema, ControllerEpoch: 7, EffectDeadline: deadline,
			AllocationID: allocation, SessionID: sessionID, Generation: 7,
			UserID: "22222222-2222-4222-8222-222222222222", CatalogGeneration: 42,
			ProblemID: "pod-crash", ProblemRevision: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			ResourceProfile: "small", ExpiresAt: "2026-08-01T01:00:00Z", IdempotencyKey: "create-session-1",
		}, want: "d5cd3b8ad0a9480d1ced8b15b6fba915c3c66d53a8461a04b97ee1ef3bd6b3e5"},
		{name: "get", request: GetRequest{
			Schema: Schema, ControllerEpoch: 7, EffectDeadline: deadline,
			AllocationID: allocation, SessionID: sessionID, Generation: 7,
		}, want: "e73bebf5e2f6412cf8da9269fccb95630cd02652a20a2e2b2df9e20b7d11c777"},
		{name: "destroy", request: DestroyRequest{
			Schema: Schema, ControllerEpoch: 7, EffectDeadline: deadline,
			AllocationID: allocation, SessionID: sessionID, Generation: 7,
		}, want: "617727b7998b4ab6e9e66f44df9f304fffcacf1128d08426c55506f766634ce2"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := RequestDigest(providerID, test.request)
			if err != nil {
				t.Fatal(err)
			}
			if encoded := hex.EncodeToString(got[:]); encoded != test.want {
				t.Fatalf("digest=%s want=%s", encoded, test.want)
			}
		})
	}
}

func TestRequestDigestExcludesAuthorityProofButBindsProviderAndSemanticRequest(t *testing.T) {
	request := CreateRequest{
		Schema: Schema, ControllerEpoch: 7, EffectDeadline: "2026-08-01T00:00:30Z",
		AllocationID: "bbccedbc-f431-50fe-856e-4d3090d825f0",
		SessionID:    "11111111-1111-4111-8111-111111111111", Generation: 7,
		UserID: "22222222-2222-4222-8222-222222222222", CatalogGeneration: 42,
		ProblemID: "pod-crash", ProblemRevision: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		ResourceProfile: "small", ExpiresAt: "2026-08-01T01:00:00Z", IdempotencyKey: "create-session-1",
	}
	baseline, err := RequestDigest("home-proxmox-test", request)
	if err != nil {
		t.Fatal(err)
	}
	request.AuthorityProof = AuthorityProof{Token: "must-not-affect-request-digest", ProofID: "other"}
	withProof, err := RequestDigest("home-proxmox-test", request)
	if err != nil {
		t.Fatal(err)
	}
	if withProof != baseline {
		t.Fatal("authority proof changed the provider idempotency/request digest")
	}
	changed := request
	changed.CatalogGeneration++
	changedDigest, err := RequestDigest("home-proxmox-test", changed)
	if err != nil {
		t.Fatal(err)
	}
	otherProvider, err := RequestDigest("home-proxmox-other", request)
	if err != nil {
		t.Fatal(err)
	}
	if changedDigest == baseline || otherProvider == baseline {
		t.Fatal("semantic request or provider change did not change the digest")
	}
}

func TestAuthorityProofFormattingRedactsToken(t *testing.T) {
	proof := AuthorityProof{Schema: AuthorityProofSchema, ProviderID: "home", Token: "secret-token"}
	if got := proof.String(); got == "" || contains(got, proof.Token) {
		t.Fatalf("proof String leaked token: %q", got)
	}
	if got := proof.GoString(); got == "" || contains(got, proof.Token) {
		t.Fatalf("proof GoString leaked token: %q", got)
	}
}

func contains(value, fragment string) bool {
	for index := 0; index+len(fragment) <= len(value); index++ {
		if value[index:index+len(fragment)] == fragment {
			return true
		}
	}
	return false
}
