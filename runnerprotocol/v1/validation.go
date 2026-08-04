package v1

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net/url"
	"regexp"
	"strings"
	"time"
)

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

type AuthorityProofBinding struct {
	ProviderID     string
	Epoch          uint64
	Operation      string
	RequestDigest  [sha256.Size]byte
	EffectDeadline string
	IssuerURI      string
	AudienceURI    string
}

// ValidatedAuthorityProof is the parsed, bearer-free result of exact proof
// binding. TokenHash is safe to pass to the online consumption ledger; the raw
// token is cleared before this value is returned.
type ValidatedAuthorityProof struct {
	ProviderID     string
	Epoch          uint64
	LeaseID        string
	ProofID        string
	Operation      string
	RequestDigest  [sha256.Size]byte
	EffectDeadline time.Time
	IssuedAt       time.Time
	ExpiresAt      time.Time
	IssuerURI      string
	AudienceURI    string
	TokenHash      [sha256.Size]byte
}

func ValidateAuthorityProof(proof AuthorityProof, binding AuthorityProofBinding) (ValidatedAuthorityProof, error) {
	if proof.Schema != AuthorityProofSchema || proof.ProviderID != binding.ProviderID || proof.Epoch != binding.Epoch ||
		proof.Epoch == 0 || proof.Epoch > MaxDurableValue || proof.Operation != binding.Operation ||
		proof.EffectDeadline != binding.EffectDeadline || proof.IssuerURI != binding.IssuerURI ||
		proof.AudienceURI != binding.AudienceURI || !uuidPattern.MatchString(proof.LeaseID) ||
		!uuidPattern.MatchString(proof.ProofID) {
		return ValidatedAuthorityProof{}, ErrUnauthorized
	}
	encodedDigest, err := decodeCanonicalBase64URL(proof.RequestDigest, sha256.Size)
	if err != nil || subtle.ConstantTimeCompare(encodedDigest, binding.RequestDigest[:]) != 1 {
		return ValidatedAuthorityProof{}, ErrUnauthorized
	}
	issuedAt, err := ParseCanonicalDatabaseUTC(proof.IssuedAt)
	if err != nil {
		return ValidatedAuthorityProof{}, ErrUnauthorized
	}
	expiresAt, err := ParseCanonicalDatabaseUTC(proof.ExpiresAt)
	if err != nil || !expiresAt.After(issuedAt) || expiresAt.Sub(issuedAt) > MaxAuthorityProofLifetime {
		return ValidatedAuthorityProof{}, ErrUnauthorized
	}
	deadline, err := ParseCanonicalDatabaseUTC(proof.EffectDeadline)
	if err != nil || expiresAt.After(deadline) {
		return ValidatedAuthorityProof{}, ErrUnauthorized
	}
	token, err := decodeCanonicalBase64URL(proof.Token, 32)
	if err != nil {
		return ValidatedAuthorityProof{}, ErrUnauthorized
	}
	tokenHash := sha256.Sum256(token)
	clear(token)
	return ValidatedAuthorityProof{
		ProviderID: binding.ProviderID, Epoch: proof.Epoch, LeaseID: proof.LeaseID,
		ProofID: proof.ProofID, Operation: binding.Operation, RequestDigest: binding.RequestDigest,
		EffectDeadline: deadline, IssuedAt: issuedAt, ExpiresAt: expiresAt,
		IssuerURI: binding.IssuerURI, AudienceURI: binding.AudienceURI, TokenHash: tokenHash,
	}, nil
}

func ValidProviderID(value string) bool {
	if value == "" || len(value) > 128 || strings.TrimSpace(value) != value {
		return false
	}
	for _, char := range value {
		if !((char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') ||
			char == '-' || char == ':' || char == '.' || char == '_') {
			return false
		}
	}
	return true
}

func CanonicalAuthorityURI(raw string) bool {
	if raw == "" || len(raw) > 512 || strings.TrimSpace(raw) != raw {
		return false
	}
	parsed, err := url.Parse(raw)
	return err == nil && parsed.IsAbs() && parsed.Scheme != "" && parsed.Host != "" &&
		parsed.User == nil && parsed.RawQuery == "" && parsed.Fragment == "" &&
		parsed.Scheme == strings.ToLower(parsed.Scheme) && parsed.Host == strings.ToLower(parsed.Host) &&
		parsed.String() == raw
}

func ParseCanonicalUTC(value string) (time.Time, error) {
	if !strings.HasSuffix(value, "Z") {
		return time.Time{}, ErrInvalidRequest
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil || parsed.UTC().Format(time.RFC3339Nano) != value {
		return time.Time{}, ErrInvalidRequest
	}
	return parsed.UTC(), nil
}

func ParseCanonicalDatabaseUTC(value string) (time.Time, error) {
	parsed, err := ParseCanonicalUTC(value)
	if err != nil || parsed.Nanosecond()%int(time.Microsecond) != 0 {
		return time.Time{}, ErrInvalidRequest
	}
	return parsed, nil
}

// FormatCanonicalDatabaseUTC serializes a PostgreSQL timestamptz value without
// losing the microsecond precision used by the authority-proof ledger. Values
// from a non-UTC location or with sub-microsecond precision are rejected so a
// caller cannot silently change an exact proof binding while formatting it.
func FormatCanonicalDatabaseUTC(value time.Time) (string, error) {
	if value.IsZero() || value.Location() != time.UTC ||
		value.Nanosecond()%int(time.Microsecond) != 0 {
		return "", ErrInvalidRequest
	}
	encoded := value.Format(time.RFC3339Nano)
	if _, err := ParseCanonicalDatabaseUTC(encoded); err != nil {
		return "", err
	}
	return encoded, nil
}

func decodeCanonicalBase64URL(value string, size int) ([]byte, error) {
	if value == "" || strings.Contains(value, "=") {
		return nil, errors.New("non-canonical base64url")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(decoded) != size || base64.RawURLEncoding.EncodeToString(decoded) != value {
		return nil, errors.New("invalid base64url value")
	}
	return decoded, nil
}
