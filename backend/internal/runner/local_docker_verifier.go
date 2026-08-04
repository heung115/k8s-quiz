package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	containerapi "github.com/k8s-quiz/backend/internal/container"
)

// DevelopmentGuestVerifier preserves local-development behavior while making
// its weak trust boundary explicit. It executes inside the learner container,
// so it can never issue trusted_external assurance or an attestation.
type DevelopmentGuestVerifier struct {
	mgr containerapi.Manager
	now func() time.Time
}

func NewDevelopmentGuestVerifier(mgr containerapi.Manager) (*DevelopmentGuestVerifier, error) {
	if mgr == nil {
		return nil, errors.New("development guest verifier requires a container manager")
	}
	return &DevelopmentGuestVerifier{mgr: mgr, now: time.Now}, nil
}

func (v *DevelopmentGuestVerifier) Verify(ctx context.Context, target LocalDockerVerifyRequest) (VerifyResult, error) {
	if err := ValidateVerifyRequest(target.Request); err != nil {
		return VerifyResult{}, err
	}
	if target.Request.Allocation.Provider != ProviderLocalDocker || target.ContainerID == "" ||
		target.Runtime.Revision != target.Request.Problem.Revision {
		return VerifyResult{}, ErrInvalidRevision
	}
	fence, ok := ControllerFenceFromContext(ctx)
	if !ok {
		return VerifyResult{}, errors.New("development guest verifier requires controller fence")
	}
	startedAt := v.now().UTC()
	var status VerifyStatus
	var evidence string
	switch target.Runtime.VerifyType {
	case "script":
		if target.Runtime.VerifyScript == "" {
			return VerifyResult{}, errors.New("approved verify script is missing")
		}
		result, err := v.mgr.Exec(ctx, target.ContainerID, []string{"/bin/sh", "-c", target.Runtime.VerifyScript})
		if err != nil {
			return VerifyResult{}, fmt.Errorf("run development guest verifier: %w", err)
		}
		status = VerifyFailed
		if result.ExitCode == 0 {
			status = VerifyPassed
		}
		// Raw output remains private evidence. The public result is an allowlisted
		// message and never includes guest stdout/stderr.
		evidence = boundedOutput(result.Stdout + result.Stderr)
	case "text":
		result, err := v.mgr.Exec(ctx, target.ContainerID, []string{"kubectl", "get", "all", "--all-namespaces"})
		if err != nil {
			return VerifyResult{}, fmt.Errorf("collect development guest evidence: %w", err)
		}
		status = VerifyNeedsGrading
		evidence = boundedOutput(result.Stdout + result.Stderr)
	default:
		return VerifyResult{}, fmt.Errorf("verify type %q is not executable by development verifier", target.Runtime.VerifyType)
	}
	feedback, err := PublicFeedbackForStatus(status)
	if err != nil {
		return VerifyResult{}, err
	}
	finishedAt := v.now().UTC()
	artifactDigest := developmentVerifierArtifactDigest(target.Runtime)
	result := VerifyResult{
		Status: status, Feedback: feedback, Evidence: evidence,
		Receipt: VerificationReceipt{
			Schema: VerificationReceiptSchema, OperationID: target.Request.IdempotencyKey,
			Allocation: target.Request.Allocation, Problem: target.Request.Problem,
			Deadline: target.Request.Deadline, Status: status, Feedback: feedback,
			EvidenceDigest: VerifyEvidenceDigest(evidence), VerifierArtifactDigest: artifactDigest,
			VerifierExecutionID: uuidFromDigest("k8s-quiz/development-verifier/v1\x00" + target.Request.IdempotencyKey),
			StartedAt:           startedAt, FinishedAt: finishedAt,
			Assurance: VerifyAssuranceDevelopmentGuest, ControllerFence: fence,
		},
	}
	if err := ValidateVerifyAuthority(target.Request, result, fence); err != nil {
		return VerifyResult{}, err
	}
	return result, nil
}

func developmentVerifierArtifactDigest(runtime LocalDockerRuntime) string {
	h := sha256.New()
	fmt.Fprintf(h, "k8s-quiz/development-verifier-artifact/v1\x00%s\x00%s\x00", runtime.Revision, runtime.VerifyType)
	h.Write([]byte(runtime.VerifyScript))
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}
