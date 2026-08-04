package v1

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

type fixedProofIssuer struct {
	providerID string
	epoch      uint64
}

func (i fixedProofIssuer) IssueAuthorityProof(_ context.Context, request AuthorityProofRequest) (AuthorityProof, error) {
	issuedAt := request.EffectDeadline.Add(-2 * time.Second)
	expiresAt := request.EffectDeadline.Add(-time.Second)
	return AuthorityProof{
		Schema: AuthorityProofSchema, ProviderID: i.providerID, Epoch: i.epoch,
		LeaseID:   "00000000-0000-4000-8000-000000000001",
		ProofID:   "00000000-0000-4000-8000-000000000002",
		Operation: request.Operation, RequestDigest: base64.RawURLEncoding.EncodeToString(request.RequestDigest[:]),
		EffectDeadline: request.EffectDeadline.Format(time.RFC3339Nano),
		IssuerURI:      request.IssuerURI, AudienceURI: request.AudienceURI,
		IssuedAt: issuedAt.Format(time.RFC3339Nano), ExpiresAt: expiresAt.Format(time.RFC3339Nano),
		Token: base64.RawURLEncoding.EncodeToString(make([]byte, sha256.Size)),
	}, nil
}

func TestCallTreatsMalformedMutationResponseAsUnknownForEveryStatus(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		media    string
		encoding string
	}{
		{name: "success bad media", status: http.StatusOK, media: "text/plain"},
		{name: "client error bad media", status: http.StatusBadRequest, media: "text/plain"},
		{name: "server error bad media", status: http.StatusServiceUnavailable, media: "text/plain"},
		{name: "client error encoded", status: http.StatusConflict, media: "application/json", encoding: "br"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
				response.Header().Set("Content-Type", test.media)
				if test.encoding != "" {
					response.Header().Set("Content-Encoding", test.encoding)
				}
				response.WriteHeader(test.status)
				_, _ = response.Write([]byte(`{"error":"untrusted"}`))
			}))
			defer server.Close()

			baseURL, err := url.Parse(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			client := &Client{baseURL: baseURL, httpClient: server.Client(), maxResponseBody: 4096}
			err = client.call(context.Background(), "/mutation", struct{}{}, &struct{}{}, true)
			if !errors.Is(err, ErrOutcomeUnknown) || !errors.Is(err, ErrProtocolViolation) {
				t.Fatalf("mutation error=%v, want OutcomeUnknown and ProtocolViolation", err)
			}
		})
	}
}

func TestCallKeepsMalformedReadResponseAProtocolViolation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "text/plain")
		response.WriteHeader(http.StatusBadRequest)
	}))
	defer server.Close()
	baseURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	client := &Client{baseURL: baseURL, httpClient: server.Client(), maxResponseBody: 4096}
	err = client.call(context.Background(), "/read", struct{}{}, &struct{}{}, false)
	if !errors.Is(err, ErrProtocolViolation) || errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("read error=%v, want only ProtocolViolation", err)
	}
}

func TestMutationMethodsTreatSemanticSuccessMismatchAsUnknown(t *testing.T) {
	const (
		providerID = "home-proxmox:test"
		issuerURI  = "spiffe://k8s-quiz/control-plane"
		audience   = "spiffe://k8s-quiz/private-runner"
		deadline   = "2026-08-01T00:00:30Z"
	)
	validAllocation := AllocationView{
		AllocationID: "allocation-1", SessionID: "session-1", Generation: 3,
		CatalogGeneration: 9, DesiredState: "active", VMPhase: "vm_running",
		VMObservedState: "vm_running", ExpiresAt: "2026-08-01T01:00:00Z",
		LifecycleScope: LifecycleScopeVMOnly,
	}
	tests := []struct {
		name     string
		response any
		invoke   func(*Client) error
	}{
		{
			name:     "activate epoch mismatch",
			response: ActivateResponse{Schema: Schema, AcceptedControllerEpoch: 8},
			invoke: func(client *Client) error {
				return client.ActivateController(context.Background(), ActivateRequest{
					Schema: Schema, ControllerEpoch: 7, EffectDeadline: deadline,
				})
			},
		},
		{
			name:     "create catalog mismatch",
			response: AllocationResponse{Schema: Schema, AcceptedControllerEpoch: 7, Allocation: validAllocation},
			invoke: func(client *Client) error {
				_, err := client.Create(context.Background(), CreateRequest{
					Schema: Schema, ControllerEpoch: 7, EffectDeadline: deadline,
					AllocationID: "allocation-1", SessionID: "session-1", Generation: 3,
					CatalogGeneration: 10,
				})
				return err
			},
		},
		{
			name:     "destroy identity mismatch",
			response: AllocationResponse{Schema: Schema, AcceptedControllerEpoch: 7, Allocation: validAllocation},
			invoke: func(client *Client) error {
				_, err := client.Destroy(context.Background(), DestroyRequest{
					Schema: Schema, ControllerEpoch: 7, EffectDeadline: deadline,
					AllocationID: "different-allocation", SessionID: "session-1", Generation: 3,
				})
				return err
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
				response.Header().Set("Content-Type", "application/json")
				if err := json.NewEncoder(response).Encode(test.response); err != nil {
					t.Errorf("encode response: %v", err)
				}
			}))
			defer server.Close()
			baseURL, err := url.Parse(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			client := &Client{
				baseURL: baseURL, httpClient: server.Client(), maxResponseBody: 4096,
				providerID: providerID, authorityIssuerURI: issuerURI,
				authorityAudienceURI: audience, proofIssuer: fixedProofIssuer{providerID: providerID, epoch: 7},
			}
			err = test.invoke(client)
			if !errors.Is(err, ErrOutcomeUnknown) || !errors.Is(err, ErrProtocolViolation) {
				t.Fatalf("mutation error=%v, want OutcomeUnknown and ProtocolViolation", err)
			}
		})
	}
}
