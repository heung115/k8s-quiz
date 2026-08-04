package runner

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	ControllerAuthorityProofSchema = "k8s-quiz.controller-authority/v1"
	controllerProofLifetime        = 5 * time.Second
	maxControllerProofEffectWindow = 2 * time.Minute
)

var (
	ErrControllerProofInvalid = errors.New("runner controller proof subject is invalid")
)

type ControllerProofOperation string

const (
	ControllerProofActivate ControllerProofOperation = "activate"
	ControllerProofCreate   ControllerProofOperation = "create"
	ControllerProofGet      ControllerProofOperation = "get"
	ControllerProofDestroy  ControllerProofOperation = "destroy"
)

type ControllerProofSubject struct {
	Operation      ControllerProofOperation
	RequestDigest  [sha256.Size]byte
	EffectDeadline time.Time
	IssuerURI      string
	AudienceURI    string
}

// ControllerAuthorityProof is a short-lived, single-use bearer capability.
// It is not a signature. The private Runner must consume it online against the
// authoritative PostgreSQL primary before applying any provider effect.
//
// token is intentionally unexported so ordinary struct logging cannot reveal
// the bearer secret. Token returns a copy for the private API wire adapter.
type ControllerAuthorityProof struct {
	Schema         string
	ProviderID     string
	Epoch          uint64
	LeaseID        string
	ProofID        string
	Operation      ControllerProofOperation
	RequestDigest  [sha256.Size]byte
	EffectDeadline time.Time
	IssuerURI      string
	AudienceURI    string
	IssuedAt       time.Time
	ExpiresAt      time.Time
	token          [32]byte
}

func (p ControllerAuthorityProof) Token() [32]byte {
	return p.token
}

func (p ControllerAuthorityProof) String() string {
	return fmt.Sprintf("ControllerAuthorityProof{schema:%q provider:%q epoch:%d lease:%q proof:%q operation:%q expires:%s}",
		p.Schema, p.ProviderID, p.Epoch, p.LeaseID, p.ProofID, p.Operation,
		p.ExpiresAt.UTC().Format(time.RFC3339Nano))
}

func (p ControllerAuthorityProof) GoString() string { return p.String() }

// IssueProof records a bearer-token hash on the same dedicated PostgreSQL
// session that owns the provider's advisory lease. DB clock_timestamp is the
// authority for issuance and expiry. A proof row produced during concurrent
// Close is deliberately not returned to the caller.
func (l *ControllerLease) IssueProof(ctx context.Context, subject ControllerProofSubject) (ControllerAuthorityProof, error) {
	if l == nil {
		return ControllerAuthorityProof{}, ErrControllerAuthorityUnavailable
	}
	if err := validateControllerProofSubject(subject); err != nil {
		return ControllerAuthorityProof{}, err
	}
	if err := ctx.Err(); err != nil {
		return ControllerAuthorityProof{}, err
	}

	l.sessionMu.Lock()
	defer l.sessionMu.Unlock()
	if err := ctx.Err(); err != nil {
		return ControllerAuthorityProof{}, err
	}
	l.mu.Lock()
	active := l.state == controllerLeaseActive
	l.mu.Unlock()
	if !active {
		return ControllerAuthorityProof{}, ErrControllerAuthorityUnavailable
	}

	proofID, err := newUUID()
	if err != nil {
		return ControllerAuthorityProof{}, fmt.Errorf("generate runner controller proof id: %w", err)
	}
	var token [32]byte
	if _, err := rand.Read(token[:]); err != nil {
		return ControllerAuthorityProof{}, fmt.Errorf("generate runner controller proof token: %w", err)
	}
	tokenHash := sha256.Sum256(token[:])
	var issuedAt, expiresAt time.Time
	err = l.conn.QueryRow(ctx, `
		WITH db_time AS MATERIALIZED (
			SELECT clock_timestamp() AS issued_at
		), live AS MATERIALIZED (
			SELECT db_time.issued_at
			FROM runner_controller_epochs authority
			CROSS JOIN db_time
			WHERE authority.provider_id=$1
			  AND authority.epoch=$2
			  AND authority.lease_id=$3::UUID
			  AND authority.lease_backend_pid=pg_backend_pid()
			  AND authority.lease_backend_start=$4
			  AND authority.lease_advisory_key=$5
			  AND EXISTS (
				SELECT 1
				FROM pg_catalog.pg_locks held_lock
				WHERE held_lock.pid=pg_backend_pid()
				  AND held_lock.locktype='advisory'
				  AND held_lock.database=(
					  SELECT oid FROM pg_catalog.pg_database
					  WHERE datname=current_database()
				  )
				  AND held_lock.classid::BIGINT=(($5::BIGINT >> 32) & 4294967295)
				  AND held_lock.objid::BIGINT=($5::BIGINT & 4294967295)
				  AND held_lock.objsubid=1
				  AND held_lock.mode='ExclusiveLock'
				  AND held_lock.granted
			  )
		), inserted AS (
			INSERT INTO runner_controller_proofs (
				provider_id,epoch,lease_id,proof_id,operation,request_digest,
				effect_deadline,issuer_uri,audience_uri,token_hash,issued_at,expires_at
			)
			SELECT $1,$2,$3::UUID,$6::UUID,$7,$8,$9,$10,$11,$12,
			       live.issued_at,LEAST($9::TIMESTAMPTZ,live.issued_at+INTERVAL '5 seconds')
			FROM live
			WHERE $9::TIMESTAMPTZ > live.issued_at
			  AND $9::TIMESTAMPTZ <= live.issued_at+INTERVAL '2 minutes'
			RETURNING issued_at,expires_at
		)
		SELECT issued_at,expires_at FROM inserted`,
		l.providerID, int64(l.epoch), l.leaseID, l.backendStart, l.key,
		proofID, string(subject.Operation), subject.RequestDigest[:], subject.EffectDeadline,
		subject.IssuerURI, subject.AudienceURI, tokenHash[:],
	).Scan(&issuedAt, &expiresAt)
	if err != nil {
		clear(token[:])
		if errors.Is(err, pgx.ErrNoRows) {
			return ControllerAuthorityProof{}, ErrControllerAuthorityUnavailable
		}
		probeCtx, cancel := context.WithTimeout(context.Background(), defaultLeaseHealthInterval)
		alive, probeErr := l.probeSession(probeCtx)
		cancel()
		if probeErr != nil || alive != 1 {
			cause := err
			if probeErr != nil {
				cause = errors.Join(cause, probeErr)
			}
			loss := fmt.Errorf("%w: proof issuance lost the lease: %v", ErrControllerLeaseLost, cause)
			if l.requestLossLocked(loss) {
				l.signalLossLocked(loss)
			}
			return ControllerAuthorityProof{}, loss
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ControllerAuthorityProof{}, ctxErr
		}
		return ControllerAuthorityProof{}, fmt.Errorf("issue runner controller proof: %w", err)
	}

	l.mu.Lock()
	active = l.state == controllerLeaseActive
	l.mu.Unlock()
	if !active {
		clear(token[:])
		return ControllerAuthorityProof{}, ErrControllerAuthorityUnavailable
	}
	return ControllerAuthorityProof{
		Schema: ControllerAuthorityProofSchema, ProviderID: l.providerID,
		Epoch: l.epoch, LeaseID: l.leaseID, ProofID: proofID,
		Operation: subject.Operation, RequestDigest: subject.RequestDigest,
		EffectDeadline: subject.EffectDeadline, IssuerURI: subject.IssuerURI,
		AudienceURI: subject.AudienceURI, IssuedAt: issuedAt.UTC(),
		ExpiresAt: expiresAt.UTC(), token: token,
	}, nil
}

func validateControllerProofSubject(subject ControllerProofSubject) error {
	switch subject.Operation {
	case ControllerProofActivate, ControllerProofCreate, ControllerProofGet, ControllerProofDestroy:
	default:
		return ErrControllerProofInvalid
	}
	if subject.EffectDeadline.IsZero() || subject.EffectDeadline.Location() != time.UTC ||
		subject.EffectDeadline.Nanosecond()%int(time.Microsecond) != 0 {
		return ErrControllerProofInvalid
	}
	if !canonicalAuthorityURI(subject.IssuerURI) || !canonicalAuthorityURI(subject.AudienceURI) {
		return ErrControllerProofInvalid
	}
	return nil
}

func canonicalAuthorityURI(raw string) bool {
	if raw == "" || len(raw) > 512 || strings.TrimSpace(raw) != raw {
		return false
	}
	parsed, err := url.Parse(raw)
	return err == nil && parsed.IsAbs() && parsed.Scheme != "" && parsed.Host != "" &&
		parsed.User == nil && parsed.RawQuery == "" && parsed.Fragment == "" &&
		parsed.Scheme == strings.ToLower(parsed.Scheme) && parsed.Host == strings.ToLower(parsed.Host) &&
		parsed.String() == raw
}
