// Package v1 defines the public, provider-neutral wire contract used between
// the k8s-quiz Control Plane and a private Runner. This version intentionally
// exposes VM lifecycle only; guest readiness, setup, verification, terminal
// streaming, capacity and reconciliation are not part of the protocol yet.
package v1

import (
	"context"
	"crypto/sha256"
	"fmt"
	"time"
)

const (
	Schema      = "k8s-quiz.private-runner/v1"
	ErrorSchema = "k8s-quiz.private-runner-error/v1"

	PathActivate = "/private/v1/controller/activate"
	PathCreate   = "/private/v1/allocations/create"
	PathGet      = "/private/v1/allocations/get"
	PathDestroy  = "/private/v1/allocations/destroy"

	OperationActivate = "activate"
	OperationCreate   = "create"
	OperationGet      = "get"
	OperationDestroy  = "destroy"

	LifecycleScopeVMOnly = "vm_only"
	AuthorityProofSchema = "k8s-quiz.controller-authority/v1"

	// MaxDurableValue is the largest positive value that can round-trip through
	// the PostgreSQL BIGINT columns used by both sides of the protocol.
	MaxDurableValue = uint64(1<<63 - 1)

	MaxAuthorityProofLifetime = 5 * time.Second
)

// AuthorityProof is a short-lived, single-use bearer capability. String and
// GoString deliberately omit Token so ordinary structured logging cannot leak
// the bearer secret.
type AuthorityProof struct {
	Schema         string `json:"schema"`
	ProviderID     string `json:"provider_id"`
	Epoch          uint64 `json:"epoch"`
	LeaseID        string `json:"lease_id"`
	ProofID        string `json:"proof_id"`
	Operation      string `json:"operation"`
	RequestDigest  string `json:"request_digest"`
	EffectDeadline string `json:"effect_deadline"`
	IssuerURI      string `json:"issuer_uri"`
	AudienceURI    string `json:"audience_uri"`
	IssuedAt       string `json:"issued_at"`
	ExpiresAt      string `json:"expires_at"`
	Token          string `json:"token"`
}

func (p AuthorityProof) String() string {
	return fmt.Sprintf("AuthorityProof{schema:%q provider:%q epoch:%d lease:%q proof:%q operation:%q expires:%q}",
		p.Schema, p.ProviderID, p.Epoch, p.LeaseID, p.ProofID, p.Operation, p.ExpiresAt)
}

func (p AuthorityProof) GoString() string { return p.String() }

type ActivateRequest struct {
	Schema          string         `json:"schema"`
	ControllerEpoch uint64         `json:"controller_epoch"`
	EffectDeadline  string         `json:"effect_deadline"`
	AuthorityProof  AuthorityProof `json:"authority_proof"`
}

type ActivateResponse struct {
	Schema                  string `json:"schema"`
	AcceptedControllerEpoch uint64 `json:"accepted_controller_epoch"`
}

type CreateRequest struct {
	Schema            string         `json:"schema"`
	ControllerEpoch   uint64         `json:"controller_epoch"`
	EffectDeadline    string         `json:"effect_deadline"`
	AllocationID      string         `json:"allocation_id"`
	SessionID         string         `json:"session_id"`
	Generation        uint64         `json:"generation"`
	UserID            string         `json:"user_id"`
	CatalogGeneration uint64         `json:"catalog_generation"`
	ProblemID         string         `json:"problem_id"`
	ProblemRevision   string         `json:"problem_revision"`
	ResourceProfile   string         `json:"resource_profile"`
	ExpiresAt         string         `json:"expires_at"`
	IdempotencyKey    string         `json:"idempotency_key"`
	AuthorityProof    AuthorityProof `json:"authority_proof"`
}

type GetRequest struct {
	Schema          string         `json:"schema"`
	ControllerEpoch uint64         `json:"controller_epoch"`
	EffectDeadline  string         `json:"effect_deadline"`
	AllocationID    string         `json:"allocation_id"`
	SessionID       string         `json:"session_id"`
	Generation      uint64         `json:"generation"`
	AuthorityProof  AuthorityProof `json:"authority_proof"`
}

type DestroyRequest struct {
	Schema          string         `json:"schema"`
	ControllerEpoch uint64         `json:"controller_epoch"`
	EffectDeadline  string         `json:"effect_deadline"`
	AllocationID    string         `json:"allocation_id"`
	SessionID       string         `json:"session_id"`
	Generation      uint64         `json:"generation"`
	AuthorityProof  AuthorityProof `json:"authority_proof"`
}

// AllocationView is the complete wire-visible allocation projection. It must
// never gain a VMID, node, pool, storage, bridge, template, address,
// credential, ownership metadata or provider plan field.
type AllocationView struct {
	AllocationID      string `json:"allocation_id"`
	SessionID         string `json:"session_id"`
	Generation        uint64 `json:"generation"`
	CatalogGeneration uint64 `json:"catalog_generation"`
	DesiredState      string `json:"desired_state"`
	VMPhase           string `json:"vm_phase"`
	VMObservedState   string `json:"vm_observed_state"`
	ExpiresAt         string `json:"expires_at"`
	ErrorCode         string `json:"error_code,omitempty"`
	LifecycleScope    string `json:"lifecycle_scope"`
}

type AllocationResponse struct {
	Schema                  string         `json:"schema"`
	AcceptedControllerEpoch uint64         `json:"accepted_controller_epoch"`
	Allocation              AllocationView `json:"allocation"`
}

type ErrorResponse struct {
	Schema string    `json:"schema"`
	Error  WireError `json:"error"`
}

type WireError struct {
	Code      ErrorCode `json:"code"`
	Retryable bool      `json:"retryable"`
	Message   string    `json:"message"`
}

type AllocationIdentity struct {
	AllocationID string
	SessionID    string
	Generation   uint64
}

type AuthorityProofRequest struct {
	Operation      string
	RequestDigest  [sha256.Size]byte
	EffectDeadline time.Time
	IssuerURI      string
	AudienceURI    string
}

// AuthorityProofIssuer is implemented by a Control Plane adapter backed by a
// live controller lease. Implementations must never issue from a cached epoch.
type AuthorityProofIssuer interface {
	IssueAuthorityProof(context.Context, AuthorityProofRequest) (AuthorityProof, error)
}
