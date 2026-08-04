package runner

import (
	"context"
	"errors"
	"testing"

	protocol "github.com/heung115/k8s-quiz/runnerprotocol/v1"
)

func TestProtocolProofIssuerRequiresLiveLease(t *testing.T) {
	if _, err := NewProtocolProofIssuer(nil); !errors.Is(err, ErrControllerAuthorityUnavailable) {
		t.Fatalf("nil lease error=%v", err)
	}
	var issuer protocol.AuthorityProofIssuer = &ProtocolProofIssuer{}
	if _, err := issuer.IssueAuthorityProof(context.Background(), protocol.AuthorityProofRequest{}); !errors.Is(err, ErrControllerAuthorityUnavailable) {
		t.Fatalf("empty issuer error=%v", err)
	}
}
