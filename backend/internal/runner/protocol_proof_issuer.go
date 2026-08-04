package runner

import (
	"context"
	"encoding/base64"

	protocol "github.com/heung115/k8s-quiz/runnerprotocol/v1"
)

// ProtocolProofIssuer adapts a live ControllerLease to the public private-
// Runner protocol. It deliberately owns no cached provider or epoch: each
// proof is issued on the dedicated PostgreSQL session that owns the lease.
type ProtocolProofIssuer struct {
	lease *ControllerLease
}

func NewProtocolProofIssuer(lease *ControllerLease) (*ProtocolProofIssuer, error) {
	if lease == nil {
		return nil, ErrControllerAuthorityUnavailable
	}
	return &ProtocolProofIssuer{lease: lease}, nil
}

func (i *ProtocolProofIssuer) IssueAuthorityProof(
	ctx context.Context,
	request protocol.AuthorityProofRequest,
) (protocol.AuthorityProof, error) {
	if i == nil || i.lease == nil {
		return protocol.AuthorityProof{}, ErrControllerAuthorityUnavailable
	}
	proof, err := i.lease.IssueProof(ctx, ControllerProofSubject{
		Operation: ControllerProofOperation(request.Operation), RequestDigest: request.RequestDigest,
		EffectDeadline: request.EffectDeadline, IssuerURI: request.IssuerURI,
		AudienceURI: request.AudienceURI,
	})
	if err != nil {
		return protocol.AuthorityProof{}, err
	}
	effectDeadline, err := protocol.FormatCanonicalDatabaseUTC(proof.EffectDeadline)
	if err != nil {
		return protocol.AuthorityProof{}, err
	}
	issuedAt, err := protocol.FormatCanonicalDatabaseUTC(proof.IssuedAt)
	if err != nil {
		return protocol.AuthorityProof{}, err
	}
	expiresAt, err := protocol.FormatCanonicalDatabaseUTC(proof.ExpiresAt)
	if err != nil {
		return protocol.AuthorityProof{}, err
	}
	token := proof.Token()
	defer clear(token[:])
	return protocol.AuthorityProof{
		Schema: proof.Schema, ProviderID: proof.ProviderID, Epoch: proof.Epoch,
		LeaseID: proof.LeaseID, ProofID: proof.ProofID, Operation: string(proof.Operation),
		RequestDigest:  base64.RawURLEncoding.EncodeToString(proof.RequestDigest[:]),
		EffectDeadline: effectDeadline,
		IssuerURI:      proof.IssuerURI, AudienceURI: proof.AudienceURI,
		IssuedAt: issuedAt, ExpiresAt: expiresAt,
		Token: base64.RawURLEncoding.EncodeToString(token[:]),
	}, nil
}

var _ protocol.AuthorityProofIssuer = (*ProtocolProofIssuer)(nil)
