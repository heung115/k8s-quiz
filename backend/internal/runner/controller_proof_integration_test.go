package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	testProofIssuerURI   = "spiffe://k8s-quiz.test/control-plane"
	testProofAudienceURI = "spiffe://k8s-quiz.test/private-runner"
)

func testControllerProofSubject(operation ControllerProofOperation) ControllerProofSubject {
	return ControllerProofSubject{
		Operation:      operation,
		RequestDigest:  sha256.Sum256([]byte("controller-proof-integration-request")),
		EffectDeadline: time.Now().UTC().Add(30 * time.Second).Truncate(time.Microsecond),
		IssuerURI:      testProofIssuerURI,
		AudienceURI:    testProofAudienceURI,
	}
}

func consumeControllerProof(ctx context.Context, pool *pgxpool.Pool, proof ControllerAuthorityProof) (string, error) {
	token := proof.Token()
	tokenHash := sha256.Sum256(token[:])
	clear(token[:])
	var status string
	err := pool.QueryRow(ctx, `
		SELECT consume_runner_controller_proof(
			$1,$2,$3::UUID,$4::UUID,$5,$6,$7,$8,$9,$10,$11,$12
		)`,
		proof.ProviderID, int64(proof.Epoch), proof.LeaseID, proof.ProofID,
		string(proof.Operation), proof.RequestDigest[:], proof.EffectDeadline,
		proof.IssuedAt, proof.ExpiresAt, proof.IssuerURI, proof.AudienceURI, tokenHash[:],
	).Scan(&status)
	return status, err
}

func TestControllerProofIssueConsumeAndReplay(t *testing.T) {
	_, pool := openRunnerTestStore(t)
	resetRunnerTestData(t, pool)
	providerID := "home-proxmox:proof-" + newTestIdentity(t)
	lease, err := acquireControllerLease(context.Background(), pool, providerID, time.Hour, func(error) {})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := lease.Close(ctx); err != nil {
			t.Errorf("close proof lease: %v", err)
		}
	})

	var storedLeaseID string
	var storedPID int32
	var storedStart time.Time
	var storedKey int64
	if err := pool.QueryRow(context.Background(), `
		SELECT lease_id::TEXT,lease_backend_pid,lease_backend_start,lease_advisory_key
		FROM runner_controller_epochs WHERE provider_id=$1`, providerID).Scan(
		&storedLeaseID, &storedPID, &storedStart, &storedKey,
	); err != nil {
		t.Fatal(err)
	}
	if storedLeaseID != lease.leaseID || storedPID != lease.backendPID ||
		!storedStart.Equal(lease.backendStart) || storedKey != lease.key {
		t.Fatalf("stored lease identity mismatch: id=%q pid=%d start=%s key=%d",
			storedLeaseID, storedPID, storedStart, storedKey)
	}

	subject := testControllerProofSubject(ControllerProofCreate)
	proof, err := lease.IssueProof(context.Background(), subject)
	if err != nil {
		t.Fatalf("issue controller proof: %v", err)
	}
	if proof.Schema != ControllerAuthorityProofSchema || proof.ProviderID != providerID ||
		proof.Epoch != lease.Fence().Epoch || proof.LeaseID != lease.leaseID ||
		proof.Operation != subject.Operation || proof.RequestDigest != subject.RequestDigest ||
		!proof.EffectDeadline.Equal(subject.EffectDeadline) || proof.IssuerURI != subject.IssuerURI ||
		proof.AudienceURI != subject.AudienceURI {
		t.Fatalf("issued proof binding mismatch: %s", proof)
	}
	if proof.IssuedAt.Location() != time.UTC || proof.ExpiresAt.Location() != time.UTC ||
		!proof.ExpiresAt.After(proof.IssuedAt) || proof.ExpiresAt.Sub(proof.IssuedAt) > controllerProofLifetime ||
		proof.ExpiresAt.After(subject.EffectDeadline) {
		t.Fatalf("issued proof time bounds are invalid: issued=%s expires=%s deadline=%s",
			proof.IssuedAt, proof.ExpiresAt, subject.EffectDeadline)
	}
	token := proof.Token()
	tokenHex := hex.EncodeToString(token[:])
	if strings.Contains(fmt.Sprintf("%v", proof), tokenHex) || strings.Contains(fmt.Sprintf("%#v", proof), tokenHex) {
		t.Fatal("controller proof formatter exposed the bearer token")
	}
	tokenHash := sha256.Sum256(token[:])
	clear(token[:])
	var storedTokenHash []byte
	if err := pool.QueryRow(context.Background(), `
		SELECT token_hash FROM runner_controller_proofs
		WHERE provider_id=$1 AND proof_id=$2::UUID`, providerID, proof.ProofID).Scan(&storedTokenHash); err != nil {
		t.Fatal(err)
	}
	if !equalBytes(storedTokenHash, tokenHash[:]) {
		t.Fatal("stored proof token hash does not match the issued bearer token")
	}

	status, err := consumeControllerProof(context.Background(), pool, proof)
	if err != nil || status != "accepted" {
		t.Fatalf("consume controller proof: status=%q err=%v", status, err)
	}
	status, err = consumeControllerProof(context.Background(), pool, proof)
	if err != nil || status != "replayed" {
		t.Fatalf("replay controller proof: status=%q err=%v", status, err)
	}
}

func TestControllerProofRejectsBindingChangesBeforeConsumption(t *testing.T) {
	_, pool := openRunnerTestStore(t)
	resetRunnerTestData(t, pool)
	providerID := "home-proxmox:binding-" + newTestIdentity(t)
	lease, err := acquireControllerLease(context.Background(), pool, providerID, time.Hour, func(error) {})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lease.Close(context.Background()) }()

	proof, err := lease.IssueProof(context.Background(), testControllerProofSubject(ControllerProofDestroy))
	if err != nil {
		t.Fatal(err)
	}
	mutations := []struct {
		name   string
		mutate func(*ControllerAuthorityProof)
	}{
		{name: "epoch", mutate: func(p *ControllerAuthorityProof) { p.Epoch++ }},
		{name: "lease", mutate: func(p *ControllerAuthorityProof) { p.LeaseID = newTestIdentity(t) }},
		{name: "proof", mutate: func(p *ControllerAuthorityProof) { p.ProofID = newTestIdentity(t) }},
		{name: "operation", mutate: func(p *ControllerAuthorityProof) { p.Operation = ControllerProofGet }},
		{name: "digest", mutate: func(p *ControllerAuthorityProof) { p.RequestDigest[0] ^= 0xff }},
		{name: "deadline", mutate: func(p *ControllerAuthorityProof) { p.EffectDeadline = p.EffectDeadline.Add(time.Microsecond) }},
		{name: "expiry", mutate: func(p *ControllerAuthorityProof) { p.ExpiresAt = p.ExpiresAt.Add(time.Microsecond) }},
		{name: "issuer", mutate: func(p *ControllerAuthorityProof) { p.IssuerURI += "/other" }},
		{name: "audience", mutate: func(p *ControllerAuthorityProof) { p.AudienceURI += "/other" }},
		{name: "token", mutate: func(p *ControllerAuthorityProof) { p.token[0] ^= 0xff }},
	}
	for _, test := range mutations {
		t.Run(test.name, func(t *testing.T) {
			changed := proof
			test.mutate(&changed)
			status, err := consumeControllerProof(context.Background(), pool, changed)
			if err != nil {
				t.Fatal(err)
			}
			if status != "unauthorized" && status != "fenced" {
				t.Fatalf("changed binding status=%q", status)
			}
		})
	}
	status, err := consumeControllerProof(context.Background(), pool, proof)
	if err != nil || status != "accepted" {
		t.Fatalf("valid proof after rejected mutations: status=%q err=%v", status, err)
	}
}

func TestControllerProofConcurrentConsumeAcceptsExactlyOnce(t *testing.T) {
	_, pool := openRunnerTestStore(t)
	resetRunnerTestData(t, pool)
	providerID := "home-proxmox:concurrent-proof-" + newTestIdentity(t)
	lease, err := acquireControllerLease(context.Background(), pool, providerID, time.Hour, func(error) {})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lease.Close(context.Background()) }()
	proof, err := lease.IssueProof(context.Background(), testControllerProofSubject(ControllerProofGet))
	if err != nil {
		t.Fatal(err)
	}

	const consumers = 24
	start := make(chan struct{})
	results := make(chan string, consumers)
	errorsCh := make(chan error, consumers)
	var wait sync.WaitGroup
	for i := 0; i < consumers; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			status, err := consumeControllerProof(context.Background(), pool, proof)
			if err != nil {
				errorsCh <- err
				return
			}
			results <- status
		}()
	}
	close(start)
	wait.Wait()
	close(results)
	close(errorsCh)
	for err := range errorsCh {
		t.Fatalf("concurrent proof consume: %v", err)
	}
	accepted, replayed := 0, 0
	for status := range results {
		switch status {
		case "accepted":
			accepted++
		case "replayed":
			replayed++
		default:
			t.Fatalf("unexpected concurrent consume status %q", status)
		}
	}
	if accepted != 1 || replayed != consumers-1 {
		t.Fatalf("concurrent proof statuses accepted=%d replayed=%d", accepted, replayed)
	}
}

func TestControllerProofIsFencedByNormalCloseAndSuccessor(t *testing.T) {
	_, pool := openRunnerTestStore(t)
	resetRunnerTestData(t, pool)
	providerID := "home-proxmox:proof-takeover-" + newTestIdentity(t)
	oldLease, err := acquireControllerLease(context.Background(), pool, providerID, time.Hour, func(error) {})
	if err != nil {
		t.Fatal(err)
	}
	oldProof, err := oldLease.IssueProof(context.Background(), testControllerProofSubject(ControllerProofActivate))
	if err != nil {
		t.Fatal(err)
	}
	if err := oldLease.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	status, err := consumeControllerProof(context.Background(), pool, oldProof)
	if err != nil || status != "fenced" {
		t.Fatalf("old proof after close: status=%q err=%v", status, err)
	}

	newLease, err := acquireControllerLease(context.Background(), pool, providerID, time.Hour, func(error) {})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = newLease.Close(context.Background()) }()
	if newLease.Fence().Epoch != oldLease.Fence().Epoch+1 || newLease.leaseID == oldLease.leaseID {
		t.Fatalf("successor authority did not rotate epoch/lease: old=%d/%s new=%d/%s",
			oldLease.Fence().Epoch, oldLease.leaseID, newLease.Fence().Epoch, newLease.leaseID)
	}
	status, err = consumeControllerProof(context.Background(), pool, oldProof)
	if err != nil || status != "fenced" {
		t.Fatalf("old proof after successor: status=%q err=%v", status, err)
	}
	newProof, err := newLease.IssueProof(context.Background(), testControllerProofSubject(ControllerProofActivate))
	if err != nil {
		t.Fatal(err)
	}
	status, err = consumeControllerProof(context.Background(), pool, newProof)
	if err != nil || status != "accepted" {
		t.Fatalf("successor proof: status=%q err=%v", status, err)
	}
}

func TestControllerProofIssuanceDetectsTerminatedLeaseBeforeMonitorTick(t *testing.T) {
	_, pool := openRunnerTestStore(t)
	resetRunnerTestData(t, pool)
	providerID := "home-proxmox:proof-death-" + newTestIdentity(t)
	lease, err := acquireControllerLease(context.Background(), pool, providerID, time.Hour, func(error) {})
	if err != nil {
		t.Fatal(err)
	}
	proof, err := lease.IssueProof(context.Background(), testControllerProofSubject(ControllerProofCreate))
	if err != nil {
		t.Fatal(err)
	}
	var terminated bool
	if err := pool.QueryRow(context.Background(), `SELECT pg_terminate_backend($1)`, lease.backendPID).Scan(&terminated); err != nil {
		t.Fatal(err)
	}
	if !terminated {
		t.Fatal("controller lease backend was not terminated")
	}
	_, err = lease.IssueProof(context.Background(), testControllerProofSubject(ControllerProofGet))
	if !errors.Is(err, ErrControllerLeaseLost) {
		t.Fatalf("proof issuance after backend death error=%v, want ErrControllerLeaseLost", err)
	}
	select {
	case loss := <-lease.Lost():
		if !errors.Is(loss, ErrControllerLeaseLost) {
			t.Fatalf("lease loss=%v", loss)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("proof issuance did not trigger immediate lease loss")
	}
	status, err := consumeControllerProof(context.Background(), pool, proof)
	if err != nil || status != "fenced" {
		t.Fatalf("proof issued before backend death: status=%q err=%v", status, err)
	}
	if err := lease.Close(context.Background()); !errors.Is(err, ErrControllerLeaseLost) {
		t.Fatalf("close terminated proof lease=%v", err)
	}
}

func TestControllerProofRejectsArbitraryEpochWithoutChangingHighWater(t *testing.T) {
	_, pool := openRunnerTestStore(t)
	resetRunnerTestData(t, pool)
	providerID := "home-proxmox:proof-poison-" + newTestIdentity(t)
	lease, err := acquireControllerLease(context.Background(), pool, providerID, time.Hour, func(error) {})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lease.Close(context.Background()) }()
	proof, err := lease.IssueProof(context.Background(), testControllerProofSubject(ControllerProofActivate))
	if err != nil {
		t.Fatal(err)
	}
	proof.Epoch = MaxDurableValue
	status, err := consumeControllerProof(context.Background(), pool, proof)
	if err != nil || status != "fenced" {
		t.Fatalf("arbitrary epoch proof: status=%q err=%v", status, err)
	}
	var epoch int64
	if err := pool.QueryRow(context.Background(), `
		SELECT epoch FROM runner_controller_epochs WHERE provider_id=$1`, providerID).Scan(&epoch); err != nil {
		t.Fatal(err)
	}
	if uint64(epoch) != lease.Fence().Epoch {
		t.Fatalf("invalid proof changed controller high-water to %d", epoch)
	}
}

func TestControllerProofSubjectValidation(t *testing.T) {
	valid := testControllerProofSubject(ControllerProofCreate)
	tests := []struct {
		name   string
		mutate func(*ControllerProofSubject)
	}{
		{name: "operation", mutate: func(s *ControllerProofSubject) { s.Operation = "other" }},
		{name: "deadline timezone", mutate: func(s *ControllerProofSubject) { s.EffectDeadline = s.EffectDeadline.In(time.FixedZone("other", 3600)) }},
		{name: "deadline precision", mutate: func(s *ControllerProofSubject) { s.EffectDeadline = s.EffectDeadline.Add(time.Nanosecond) }},
		{name: "issuer", mutate: func(s *ControllerProofSubject) { s.IssuerURI = "relative" }},
		{name: "audience query", mutate: func(s *ControllerProofSubject) { s.AudienceURI += "?secret=value" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			subject := valid
			test.mutate(&subject)
			if !errors.Is(validateControllerProofSubject(subject), ErrControllerProofInvalid) {
				t.Fatal("invalid proof subject was accepted")
			}
		})
	}
}

func equalBytes(left, right []byte) bool {
	if len(left) != len(right) {
		return false
	}
	var difference byte
	for i := range left {
		difference |= left[i] ^ right[i]
	}
	return difference == 0
}
