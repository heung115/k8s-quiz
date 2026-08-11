package runner

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func validVerifyContractFixture(provider ProviderKind) (VerifyRequest, VerifyResult, ControllerFence) {
	deadline := time.Date(2026, time.July, 31, 12, 0, 0, 0, time.UTC)
	request := VerifyRequest{
		Allocation: AllocationRef{
			ID:       "allocation-verify-contract",
			Session:  SessionRef{SessionID: "session-verify-contract", Generation: 3},
			Provider: provider,
		},
		Problem:        ProblemRef{ID: "pod-crashloop", Revision: "sha256:approved-problem-revision"},
		IdempotencyKey: "verify-operation-1", //gitleaks:allow synthetic test key
		Deadline:       deadline,
	}
	feedback, err := PublicFeedbackForStatus(VerifyFailed)
	if err != nil {
		panic(err)
	}
	evidence := "private verifier evidence: token=do-not-publish"
	assurance := VerifyAssuranceTrusted
	attestation := "test-private-verifier-attestation"
	if provider == ProviderLocalDocker {
		assurance = VerifyAssuranceDevelopmentGuest
		attestation = ""
	}
	fence := ControllerFence{ProviderID: "test:" + string(provider), Epoch: 7}
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
			VerifierExecutionID:    "verifier-execution-1",
			StartedAt:              deadline.Add(-2 * time.Second),
			FinishedAt:             deadline.Add(-time.Second),
			Assurance:              assurance,
			ControllerFence:        fence,
			Attestation:            attestation,
		},
	}
	return request, result, fence
}

func setVerifyContractStatus(t *testing.T, result *VerifyResult, status VerifyStatus) {
	t.Helper()
	feedback, err := PublicFeedbackForStatus(status)
	if err != nil {
		t.Fatal(err)
	}
	result.Status = status
	result.Feedback = feedback
	result.Receipt.Status = status
	result.Receipt.Feedback = feedback
}

func TestVerifyContractValidationMatrix(t *testing.T) {
	tests := []struct {
		name     string
		provider ProviderKind
		mutate   func(*testing.T, *VerifyRequest, *VerifyResult, *ControllerFence)
		wantErr  string
	}{
		{name: "valid local development receipt", provider: ProviderLocalDocker},
		{
			name: "mismatched allocation", provider: ProviderLocalDocker,
			mutate: func(_ *testing.T, _ *VerifyRequest, result *VerifyResult, _ *ControllerFence) {
				result.Receipt.Allocation.ID = "another-allocation"
			},
			wantErr: "verify receipt allocation does not match request",
		},
		{
			name: "mismatched session generation", provider: ProviderLocalDocker,
			mutate: func(_ *testing.T, _ *VerifyRequest, result *VerifyResult, _ *ControllerFence) {
				result.Receipt.Allocation.Session.Generation++
			},
			wantErr: "verify receipt allocation does not match request",
		},
		{
			name: "mismatched problem revision", provider: ProviderLocalDocker,
			mutate: func(_ *testing.T, _ *VerifyRequest, result *VerifyResult, _ *ControllerFence) {
				result.Receipt.Problem.Revision = "sha256:different-problem-revision"
			},
			wantErr: "verify receipt problem does not match request",
		},
		{
			name: "mismatched operation", provider: ProviderLocalDocker,
			mutate: func(_ *testing.T, _ *VerifyRequest, result *VerifyResult, _ *ControllerFence) {
				result.Receipt.OperationID = "another-operation"
			},
			wantErr: "verify receipt operation does not match request",
		},
		{
			name: "mismatched deadline", provider: ProviderLocalDocker,
			mutate: func(_ *testing.T, _ *VerifyRequest, result *VerifyResult, _ *ControllerFence) {
				result.Receipt.Deadline = result.Receipt.Deadline.Add(time.Second)
			},
			wantErr: "verify receipt deadline does not match request",
		},
		{
			name: "mismatched status", provider: ProviderLocalDocker,
			mutate: func(_ *testing.T, _ *VerifyRequest, result *VerifyResult, _ *ControllerFence) {
				result.Receipt.Status = VerifyPassed
			},
			wantErr: "verify receipt verdict does not match result",
		},
		{
			name: "mismatched feedback", provider: ProviderLocalDocker,
			mutate: func(t *testing.T, _ *VerifyRequest, result *VerifyResult, _ *ControllerFence) {
				feedback, err := PublicFeedbackForStatus(VerifyPassed)
				if err != nil {
					t.Fatal(err)
				}
				result.Receipt.Feedback = feedback
			},
			wantErr: "verify receipt verdict does not match result",
		},
		{
			name: "mismatched evidence digest", provider: ProviderLocalDocker,
			mutate: func(_ *testing.T, _ *VerifyRequest, result *VerifyResult, _ *ControllerFence) {
				result.Receipt.EvidenceDigest = VerifyEvidenceDigest("different evidence")
			},
			wantErr: "verify receipt evidence digest does not match result",
		},
		{
			name: "invalid artifact digest", provider: ProviderLocalDocker,
			mutate: func(_ *testing.T, _ *VerifyRequest, result *VerifyResult, _ *ControllerFence) {
				result.Receipt.VerifierArtifactDigest = "sha256:not-a-content-digest"
			},
			wantErr: "verify receipt verifier artifact digest is invalid",
		},
		{
			name: "invalid execution identity", provider: ProviderLocalDocker,
			mutate: func(_ *testing.T, _ *VerifyRequest, result *VerifyResult, _ *ControllerFence) {
				result.Receipt.VerifierExecutionID = ""
			},
			wantErr: "verify receipt execution identity is invalid",
		},
		{
			name: "missing execution timestamp", provider: ProviderLocalDocker,
			mutate: func(_ *testing.T, _ *VerifyRequest, result *VerifyResult, _ *ControllerFence) {
				result.Receipt.StartedAt = time.Time{}
			},
			wantErr: "verify receipt execution interval is invalid",
		},
		{
			name: "reversed execution timestamps", provider: ProviderLocalDocker,
			mutate: func(_ *testing.T, _ *VerifyRequest, result *VerifyResult, _ *ControllerFence) {
				result.Receipt.FinishedAt = result.Receipt.StartedAt.Add(-time.Nanosecond)
			},
			wantErr: "verify receipt execution interval is invalid",
		},
		{
			name: "late finish", provider: ProviderLocalDocker,
			mutate: func(_ *testing.T, request *VerifyRequest, result *VerifyResult, _ *ControllerFence) {
				result.Receipt.FinishedAt = request.Deadline.Add(time.Nanosecond)
			},
			wantErr: "verify receipt finished after the request deadline",
		},
		{
			name: "local claiming trusted", provider: ProviderLocalDocker,
			mutate: func(_ *testing.T, _ *VerifyRequest, result *VerifyResult, _ *ControllerFence) {
				result.Receipt.Assurance = VerifyAssuranceTrusted
				result.Receipt.Attestation = "untrusted-claim"
			},
			wantErr: `verify assurance "trusted_external" is invalid for provider "local-docker"`,
		},
		{
			name: "home proxmox claiming development", provider: ProviderHomeProxmox,
			mutate: func(_ *testing.T, _ *VerifyRequest, result *VerifyResult, _ *ControllerFence) {
				result.Receipt.Assurance = VerifyAssuranceDevelopmentGuest
				result.Receipt.Attestation = ""
			},
			wantErr: `verify assurance "development_guest" is invalid for provider "home-proxmox"`,
		},
		{
			name: "cloud claiming development", provider: ProviderCloud,
			mutate: func(_ *testing.T, _ *VerifyRequest, result *VerifyResult, _ *ControllerFence) {
				result.Receipt.Assurance = VerifyAssuranceDevelopmentGuest
				result.Receipt.Attestation = ""
			},
			wantErr: `verify assurance "development_guest" is invalid for provider "cloud"`,
		},
		{
			name: "trusted missing attestation", provider: ProviderHomeProxmox,
			mutate: func(_ *testing.T, _ *VerifyRequest, result *VerifyResult, _ *ControllerFence) {
				result.Receipt.Attestation = ""
			},
			wantErr: "trusted verify receipt attestation is missing or invalid",
		},
		{
			name: "trusted needs grading", provider: ProviderCloud,
			mutate: func(t *testing.T, _ *VerifyRequest, result *VerifyResult, _ *ControllerFence) {
				setVerifyContractStatus(t, result, VerifyNeedsGrading)
			},
			wantErr: "trusted verifier must return a terminal grade",
		},
		{
			name: "wrong controller fence", provider: ProviderLocalDocker,
			mutate: func(_ *testing.T, _ *VerifyRequest, _ *VerifyResult, fence *ControllerFence) {
				fence.Epoch++
			},
			wantErr: "verify receipt controller fence does not match authority",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request, result, fence := validVerifyContractFixture(test.provider)
			if test.mutate != nil {
				test.mutate(t, &request, &result, &fence)
			}
			err := ValidateVerifyAuthority(request, result, fence)
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("ValidateVerifyAuthority() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("ValidateVerifyAuthority() error = %v, want containing %q", err, test.wantErr)
			}
		})
	}
}

func TestVerifyContractRequestExposesNoProviderMechanisms(t *testing.T) {
	requestType := reflect.TypeOf(VerifyRequest{})
	wantTopLevel := map[string]bool{
		"Allocation": true, "Problem": true, "IdempotencyKey": true, "Deadline": true,
	}
	if requestType.NumField() != len(wantTopLevel) {
		t.Fatalf("VerifyRequest has %d fields, want exactly %d provider-neutral fields", requestType.NumField(), len(wantTopLevel))
	}
	for i := 0; i < requestType.NumField(); i++ {
		if field := requestType.Field(i); !wantTopLevel[field.Name] {
			t.Errorf("VerifyRequest exposes unexpected field %q", field.Name)
		}
	}

	forbidden := []string{"command", "script", "path", "env", "credential", "image"}
	seen := make(map[reflect.Type]bool)
	var inspect func(reflect.Type, string)
	inspect = func(valueType reflect.Type, path string) {
		for valueType.Kind() == reflect.Pointer || valueType.Kind() == reflect.Slice || valueType.Kind() == reflect.Array {
			valueType = valueType.Elem()
		}
		if valueType.Kind() != reflect.Struct || valueType == reflect.TypeOf(time.Time{}) || seen[valueType] {
			return
		}
		seen[valueType] = true
		for i := 0; i < valueType.NumField(); i++ {
			field := valueType.Field(i)
			if !field.IsExported() {
				continue
			}
			fieldPath := path + "." + field.Name
			lowerName := strings.ToLower(field.Name)
			for _, token := range forbidden {
				if strings.Contains(lowerName, token) {
					t.Errorf("provider-neutral verify request exposes forbidden %q field at %s", token, fieldPath)
				}
			}
			inspect(field.Type, fieldPath)
		}
	}
	inspect(requestType, "VerifyRequest")
}

func TestVerifyContractPublicFeedbackCannotContainRawEvidence(t *testing.T) {
	request, result, fence := validVerifyContractFixture(ProviderLocalDocker)
	secret := result.Evidence
	if strings.Contains(result.Feedback.Code, secret) || strings.Contains(result.Feedback.Message, secret) {
		t.Fatal("canonical public feedback contains private verifier evidence")
	}
	if err := ValidateVerifyAuthority(request, result, fence); err != nil {
		t.Fatalf("valid private-evidence result rejected: %v", err)
	}

	for _, field := range []string{"code", "message"} {
		t.Run(field, func(t *testing.T) {
			leaked := result
			if field == "code" {
				leaked.Feedback.Code = secret
			} else {
				leaked.Feedback.Message = secret
			}
			leaked.Receipt.Feedback = leaked.Feedback
			err := ValidateVerifyAuthority(request, leaked, fence)
			if err == nil || !strings.Contains(err.Error(), "verify result contains non-approved public feedback") {
				t.Fatalf("raw evidence in public feedback error = %v", err)
			}
		})
	}
}
