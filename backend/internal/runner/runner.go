// Package runner defines the provider-neutral boundary between the trusted
// Control Plane and untrusted problem environments.
package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"regexp"
	"time"
	"unicode/utf8"

	protocol "github.com/heung115/k8s-quiz/runnerprotocol/v1"
)

type ProviderKind string

const (
	ProviderLocalDocker ProviderKind = "local-docker"
	ProviderHomeProxmox ProviderKind = "home-proxmox"
	ProviderCloud       ProviderKind = "cloud"

	// MaxProviderOperationKeyLength is the shared Control Plane/private Runner
	// wire bound. The database may retain wider event identities, but a key
	// that crosses a provider boundary must satisfy this narrower contract.
	MaxProviderOperationKeyLength = 128
	// MaxDurableValue is the largest unsigned lifecycle value representable by
	// PostgreSQL BIGINT. Values crossing a durable or provider boundary must not
	// rely on uint64's wider range.
	MaxDurableValue = uint64(^uint64(0) >> 1)
)

var (
	ErrIdempotencyConflict     = errors.New("runner idempotency key reused with different request")
	ErrActiveSession           = errors.New("runner user already has an active session")
	ErrInvalidRevision         = errors.New("runner problem revision is not approved")
	ErrGenerationStale         = errors.New("runner session generation is stale")
	ErrAllocationNotFound      = errors.New("runner allocation not found")
	ErrLifecycleConflict       = errors.New("runner lifecycle transition conflicts with durable state")
	ErrSessionExpired          = errors.New("runner session expired")
	ErrTerminalLease           = errors.New("runner terminal lease conflict")
	ErrUnsupported             = errors.New("runner operation is unsupported")
	ErrControllerFenced        = errors.New("runner controller epoch is stale")
	ErrOperationOutcomeUnknown = errors.New("runner operation commit outcome is unknown")
	ErrProviderUnavailable     = errors.New("runner provider is unavailable")
	ErrProviderCleanupRequired = errors.New("runner provider cleanup remains required")
	ErrProviderProtocol        = errors.New("runner provider protocol is invalid")
	ErrProviderRequest         = errors.New("runner provider request is invalid")
	ErrCatalogSelectionStale   = errors.New("runner catalog selection is not the current approved head")
)

// ControllerFence is the provider-scoped, monotonically increasing authority
// issued while the PostgreSQL controller advisory lock is held. Every durable
// lifecycle mutation locks and verifies this epoch. Provider adapters must
// propagate the same value to their private mutation boundary and reject stale
// epochs atomically.
type ControllerFence struct {
	ProviderID string
	Epoch      uint64
}

type controllerFenceContextKey struct{}

func withControllerFence(ctx context.Context, fence ControllerFence) context.Context {
	return context.WithValue(ctx, controllerFenceContextKey{}, fence)
}

// ControllerFenceFromContext lets a provider adapter bind private API calls to
// the controller authority. Public-capable providers must fail closed when the
// value is absent or stale; Local Docker uses it only as a development guard.
func ControllerFenceFromContext(ctx context.Context) (ControllerFence, bool) {
	if ctx == nil {
		return ControllerFence{}, false
	}
	fence, ok := ctx.Value(controllerFenceContextKey{}).(ControllerFence)
	return fence, ok && fence.ProviderID != "" && fence.Epoch > 0 && fence.Epoch <= MaxDurableValue
}

type SessionRef struct {
	SessionID  string
	Generation uint64
}

type EndState string

const (
	EndPending   EndState = "pending"
	EndCompleted EndState = "completed"
)

// EndDecision is the durable result of one exact browser end operation.
// Pending contains only provider-neutral refs that still require absence
// proof; it is never serialized to the browser.
type EndDecision struct {
	Session SessionRef
	State   EndState
	Pending []AllocationRef
}

type ProblemRef struct {
	ID       string
	Revision string
}

// CatalogSelection binds an immutable problem artifact to the exact approved
// catalog publication from which a fresh session selected it. ProblemRef stays
// provider-neutral artifact identity; Generation is Control Plane provenance.
type CatalogSelection struct {
	Generation uint64
	Problem    ProblemRef
}

// AllocationRef is safe to retain in the Control Plane. External provider
// identifiers (container ID, VMID, address) deliberately do not cross this
// boundary.
type AllocationRef struct {
	ID       string
	Session  SessionRef
	Provider ProviderKind
}

// LocalRecoveryAllocation is the immutable durable provenance required to
// reconstruct a Local Docker allocation's trusted create specification after
// controller restart. A row without every field must be reviewed manually;
// names or ownership labels alone never authorize destructive reconciliation.
type LocalRecoveryAllocation struct {
	Ref             AllocationRef
	Selection       CatalogSelection
	ResourceProfile string
}

type CreateSessionRequest struct {
	// AllocationID is the stable provider-neutral identity reserved by the
	// Control Plane before any provider mutation. Providers must use this exact
	// value; they may not allocate a replacement identity after a retry or
	// response loss.
	AllocationID    string
	Session         SessionRef
	UserID          string
	Selection       CatalogSelection
	ResourceProfile string
	ExpiresAt       time.Time
	IdempotencyKey  string
}

// SetupSessionRequest binds the approved setup operation to one exact
// allocation and one deterministic retry identity. Providers must retain the
// result for the lifetime of the allocation: a controller may restart after
// setup succeeds but before the ready transition commits.
type SetupSessionRequest struct {
	Allocation     AllocationRef
	IdempotencyKey string
}

// SetupIdempotencyKey is controlled by the trusted Control Plane and remains
// stable across controller restart. It is never accepted from the browser.
func SetupIdempotencyKey(ref AllocationRef) string {
	return "setup:" + ref.ID
}

// AllocationIDForSession derives the UUID-shaped allocation identity shared
// by the Control Plane and every provider. The namespace is provider-neutral:
// changing providers for a future generation does not change identity rules.
func AllocationIDForSession(ref SessionRef) string {
	return protocol.AllocationIDForSession(ref.SessionID, ref.Generation)
}

// SessionIDForOperation gives an authenticated start retry one stable logical
// session identity without accepting a browser-selected session ID.
func SessionIDForOperation(userID, idempotencyKey string) string {
	return uuidFromDigest("k8s-quiz/session/v1\x00" + userID + "\x00" + idempotencyKey)
}

func uuidFromDigest(value string) string {
	sum := sha256.Sum256([]byte(value))
	b := sum[:16]
	b[6] = (b[6] & 0x0f) | 0x50
	b[8] = (b[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(b)
	return fmt.Sprintf("%s-%s-%s-%s-%s", encoded[:8], encoded[8:12], encoded[12:16], encoded[16:20], encoded[20:])
}

type OpenTerminalRequest struct {
	Allocation      AllocationRef
	UserID          string
	LeaseID         string
	ReplacesLeaseID string
}

type TerminalSession interface {
	io.ReadWriteCloser
	Resize(cols, rows int) error
}

type VerifyStatus string

const (
	VerifyPassed       VerifyStatus = "passed"
	VerifyFailed       VerifyStatus = "failed"
	VerifyNeedsGrading VerifyStatus = "needs_grading"
)

type VerifyResult struct {
	Status   VerifyStatus
	Feedback PublicVerifyFeedback
	// Evidence is private grading input. It is never persisted in lifecycle
	// events, attempts, or browser responses. Public-capable verifiers must
	// return a terminal pass/fail instead of delegating raw evidence grading to
	// the Control Plane.
	Evidence string
	Receipt  VerificationReceipt
}

const VerificationReceiptSchema = "k8s-quiz.verification-receipt/v1"

type VerifyAssurance string

const (
	// VerifyAssuranceDevelopmentGuest explicitly identifies the legacy local
	// adapter whose toolchain and Kubernetes API are learner-controlled. It is
	// accepted only for ProviderLocalDocker, which configuration already limits
	// to loopback development.
	VerifyAssuranceDevelopmentGuest VerifyAssurance = "development_guest"
	// VerifyAssuranceTrusted is reserved for a private verifier plane outside
	// the learner's writable OS and Kubernetes control-plane trust domain.
	VerifyAssuranceTrusted VerifyAssurance = "trusted_external"
)

type PublicVerifyFeedback struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type VerifyRequest struct {
	Allocation     AllocationRef
	Problem        ProblemRef
	IdempotencyKey string
	Deadline       time.Time
}

// VerificationReceipt binds a grade to the exact operation subject and the
// controller authority under which it ran. Trusted providers additionally
// authenticate Attestation at their private service boundary; the public
// application never accepts browser-provided receipt fields.
type VerificationReceipt struct {
	Schema                 string
	OperationID            string
	Allocation             AllocationRef
	Problem                ProblemRef
	Deadline               time.Time
	Status                 VerifyStatus
	Feedback               PublicVerifyFeedback
	EvidenceDigest         string
	VerifierArtifactDigest string
	VerifierExecutionID    string
	StartedAt              time.Time
	FinishedAt             time.Time
	Assurance              VerifyAssurance
	ControllerFence        ControllerFence
	Attestation            string
}

var sha256ContentIDPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
var providerOperationKeyPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

// ValidateProviderOperationKey rejects whitespace, control characters and
// punctuation that would create ambiguous operation identities across
// transports. It is deliberately ASCII and identical to private Runner v1.
func ValidateProviderOperationKey(value string) error {
	if len(value) > MaxProviderOperationKeyLength || !providerOperationKeyPattern.MatchString(value) {
		return errors.New("runner provider operation key is invalid")
	}
	return nil
}

func PublicFeedbackForStatus(status VerifyStatus) (PublicVerifyFeedback, error) {
	switch status {
	case VerifyPassed:
		return PublicVerifyFeedback{Code: "verification_passed", Message: "Verification passed."}, nil
	case VerifyFailed:
		return PublicVerifyFeedback{Code: "verification_failed", Message: "Verification did not pass."}, nil
	case VerifyNeedsGrading:
		return PublicVerifyFeedback{Code: "evidence_collected", Message: "Verification evidence was collected."}, nil
	default:
		return PublicVerifyFeedback{}, fmt.Errorf("unknown verify status %q", status)
	}
}

func VerifyEvidenceDigest(evidence string) string {
	sum := sha256.Sum256([]byte(evidence))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func ValidateVerifyRequest(request VerifyRequest) error {
	switch {
	case request.Allocation.ID == "" || request.Allocation.Session.SessionID == "" ||
		request.Allocation.Session.Generation == 0 || request.Allocation.Provider == "":
		return errors.New("verify allocation identity is incomplete")
	case request.Problem.ID == "" || request.Problem.Revision == "":
		return errors.New("verify problem identity is incomplete")
	case ValidateProviderOperationKey(request.IdempotencyKey) != nil:
		return errors.New("verify idempotency key is invalid")
	case request.Deadline.IsZero():
		return errors.New("verify deadline is required")
	default:
		return nil
	}
}

func ValidateVerifyResult(request VerifyRequest, result VerifyResult) error {
	if err := ValidateVerifyRequest(request); err != nil {
		return err
	}
	receipt := result.Receipt
	feedback, err := PublicFeedbackForStatus(result.Status)
	if err != nil {
		return err
	}
	switch {
	case result.Feedback != feedback:
		return errors.New("verify result contains non-approved public feedback")
	case receipt.Schema != VerificationReceiptSchema:
		return errors.New("verify receipt schema is invalid")
	case receipt.OperationID != request.IdempotencyKey:
		return errors.New("verify receipt operation does not match request")
	case receipt.Allocation != request.Allocation:
		return errors.New("verify receipt allocation does not match request")
	case receipt.Problem != request.Problem:
		return errors.New("verify receipt problem does not match request")
	case !receipt.Deadline.Equal(request.Deadline):
		return errors.New("verify receipt deadline does not match request")
	case receipt.Status != result.Status || receipt.Feedback != result.Feedback:
		return errors.New("verify receipt verdict does not match result")
	case receipt.EvidenceDigest != VerifyEvidenceDigest(result.Evidence):
		return errors.New("verify receipt evidence digest does not match result")
	case !sha256ContentIDPattern.MatchString(receipt.VerifierArtifactDigest):
		return errors.New("verify receipt verifier artifact digest is invalid")
	case receipt.VerifierExecutionID == "" || len(receipt.VerifierExecutionID) > 128:
		return errors.New("verify receipt execution identity is invalid")
	case receipt.StartedAt.IsZero() || receipt.FinishedAt.IsZero() || receipt.FinishedAt.Before(receipt.StartedAt):
		return errors.New("verify receipt execution interval is invalid")
	case receipt.FinishedAt.After(request.Deadline):
		return errors.New("verify receipt finished after the request deadline")
	case !utf8.ValidString(result.Feedback.Message) || len(result.Feedback.Message) > 1024:
		return errors.New("verify public feedback is invalid")
	}
	expectedAssurance := VerifyAssuranceTrusted
	if request.Allocation.Provider == ProviderLocalDocker {
		expectedAssurance = VerifyAssuranceDevelopmentGuest
	}
	if receipt.Assurance != expectedAssurance {
		return fmt.Errorf("verify assurance %q is invalid for provider %q", receipt.Assurance, request.Allocation.Provider)
	}
	if receipt.Assurance == VerifyAssuranceTrusted {
		if result.Status == VerifyNeedsGrading {
			return errors.New("trusted verifier must return a terminal grade")
		}
		if receipt.Attestation == "" || len(receipt.Attestation) > 4096 {
			return errors.New("trusted verify receipt attestation is missing or invalid")
		}
	} else if receipt.Attestation != "" {
		return errors.New("development guest verifier cannot claim an attestation")
	}
	return nil
}

// ValidateVerifyAuthority validates structural subject binding and the current
// controller fence. It deliberately does not authenticate a trusted_external
// Attestation. Non-local results become authoritative only through an
// AuthorityRunner created by NewTrustedAuthorityRunner with a cryptographic
// VerifyAttestationVerifier.
func ValidateVerifyAuthority(request VerifyRequest, result VerifyResult, fence ControllerFence) error {
	if err := ValidateVerifyResult(request, result); err != nil {
		return err
	}
	if fence.ProviderID == "" || fence.Epoch == 0 || result.Receipt.ControllerFence != fence {
		return errors.New("verify receipt controller fence does not match authority")
	}
	return nil
}

type VerifyDecisionKind string

const (
	VerifyExecute              VerifyDecisionKind = "execute"
	VerifyResume               VerifyDecisionKind = "resume"
	VerifyGradeReplay          VerifyDecisionKind = "grade_replay"
	VerifyInfrastructureReplay VerifyDecisionKind = "infrastructure_replay"
)

// VerifyDecision is the durable admission result for one user-scoped client
// operation. Only Execute authorizes a provider call. Resume means another
// caller already owns the running operation and is read-only for ordinary
// requests; controller restart recovery terminalizes it without speculative
// provider execution. Terminal replay decisions never call the provider.
type VerifyDecision struct {
	Kind      VerifyDecisionKind
	Session   SessionRef
	Success   bool
	Log       string
	ErrorCode string
}

func (d VerifyDecision) AllowsProviderCall() bool {
	return d.Kind == VerifyExecute
}

// ChoiceSubmission is the complete browser-visible identity of one choice
// grading operation. The durable store binds every field, including ChoiceID,
// into runner_operations.request_hash before publishing a result. Reusing an
// operation ID with a different problem, generation, or answer is therefore a
// conflict rather than a second grading attempt.
type ChoiceSubmission struct {
	OperationID string
	ProblemID   string
	Session     SessionRef
	ChoiceID    string
}

type ObservedState string

const (
	ObservedRunning ObservedState = "running"
	ObservedStopped ObservedState = "stopped"
	ObservedAbsent  ObservedState = "absent"
)

type Observation struct {
	Allocation AllocationRef
	State      ObservedState
}

type ReconcileMode string

const (
	// ReconcileModeReportOnly is the zero-value/default mode. It may inspect
	// exact resources and recommend actions, but it never authorizes mutation.
	ReconcileModeReportOnly ReconcileMode = "report_only"
	ReconcileModeApply      ReconcileMode = "apply"
)

type ReconcileDesiredState string

const (
	ReconcileDesiredActive ReconcileDesiredState = "active"
	ReconcileDesiredAbsent ReconcileDesiredState = "absent"
)

// ReconcileOwnership is the complete provider-neutral ownership subject used
// by reconciliation. Provider adapters may add private evidence (for example,
// Docker's create-spec hash), but may never weaken these exact fields to a
// scope-wide label match.
type ReconcileOwnership struct {
	Provider     ProviderKind
	Scope        string
	AllocationID string
	SessionID    string
	Generation   uint64
}

type ReconcileExpectation struct {
	Ownership       ReconcileOwnership
	Desired         ReconcileDesiredState
	Selection       CatalogSelection
	ResourceProfile string
}

type ReconcileFindingKind string

const (
	ReconcileFindingExpectationsRequired  ReconcileFindingKind = "expectations_required"
	ReconcileFindingIncompleteExpectation ReconcileFindingKind = "incomplete_expectation"
	ReconcileFindingInventoryUnavailable  ReconcileFindingKind = "inventory_unavailable"
	ReconcileFindingAmbiguousOwnership    ReconcileFindingKind = "ambiguous_ownership"
	ReconcileFindingAdopted               ReconcileFindingKind = "adopted"
	ReconcileFindingMissing               ReconcileFindingKind = "missing"
	ReconcileFindingCleanupRequired       ReconcileFindingKind = "cleanup_required"
)

type ReconcileFinding struct {
	Kind      ReconcileFindingKind
	Ownership ReconcileOwnership
	Desired   ReconcileDesiredState
	Message   string
}

type ReconcileActionKind string

const (
	ReconcileActionManualReview ReconcileActionKind = "manual_review"
	ReconcileActionObserved     ReconcileActionKind = "observed"
	ReconcileActionRetained     ReconcileActionKind = "retained"
	ReconcileActionRemove       ReconcileActionKind = "remove"
	ReconcileActionDestroyed    ReconcileActionKind = "destroyed"
)

// ReconcileAction records either a safe recommendation or a completed exact
// mutation. Applied is always false in report-only mode.
type ReconcileAction struct {
	Kind      ReconcileActionKind
	Ownership ReconcileOwnership
	Applied   bool
}

type ReconcileRequest struct {
	// Mode defaults to report_only. Apply is retained temporarily for callers
	// compiled against the first contract: it selects apply only when Mode is
	// empty and does not itself provide cleanup authority.
	Mode         ReconcileMode
	Apply        bool
	Expectations []ReconcileExpectation
}

type ReconcileResult struct {
	Mode     ReconcileMode
	Findings []ReconcileFinding
	Actions  []ReconcileAction
}

// Runner exposes only domain operations. It contains no provider image,
// command, script, mount, host path, environment, privilege, or network
// controls. WaitReady and SetupSession are Slice-1 lifecycle operations; a
// durable event stream replaces them in the next platform slice.
type Runner interface {
	Kind() ProviderKind
	CreateSession(context.Context, CreateSessionRequest) (AllocationRef, error)
	WaitReady(context.Context, AllocationRef, time.Duration) error
	SetupSession(context.Context, SetupSessionRequest) error
	OpenTerminal(context.Context, OpenTerminalRequest) (TerminalSession, error)
	VerifySession(context.Context, VerifyRequest) (VerifyResult, error)
	GetSession(context.Context, AllocationRef) (Observation, error)
	DestroySession(context.Context, AllocationRef) error
	Reconcile(context.Context, ReconcileRequest) (ReconcileResult, error)
}

// StartupRecoveryRunner exposes the narrow provider operations allowed while
// public admission is closed and the controller gate is recovering. The
// AuthorityRunner implementation binds every call to the same controller
// fence and rejects results after lease loss. Ordinary providers implement
// Runner only; callers receive this capability only through AuthorityRunner.
type StartupRecoveryRunner interface {
	CreateSessionRecovery(context.Context, CreateSessionRequest) (AllocationRef, error)
	WaitReadyRecovery(context.Context, AllocationRef, time.Duration) error
	SetupSessionRecovery(context.Context, SetupSessionRequest) error
	GetSessionRecovery(context.Context, AllocationRef) (Observation, error)
	DestroySessionRecovery(context.Context, AllocationRef) error
}
