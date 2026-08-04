package v1

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"hash"
)

const requestDigestDomain = "k8s-quiz/private-runner-request/v1"

// RequestDigest returns the canonical request digest bound into an authority
// proof and the provider's idempotency ledger. AuthorityProof is deliberately
// excluded so a fresh single-use proof does not change request identity.
func RequestDigest(providerID string, request any) ([sha256.Size]byte, error) {
	digest := sha256.New()
	writeDigestString(digest, requestDigestDomain)
	writeDigestString(digest, providerID)
	switch input := request.(type) {
	case ActivateRequest:
		writeDigestEnvelope(digest, OperationActivate, PathActivate, input.Schema, input.ControllerEpoch, input.EffectDeadline)
	case CreateRequest:
		writeDigestEnvelope(digest, OperationCreate, PathCreate, input.Schema, input.ControllerEpoch, input.EffectDeadline)
		writeDigestString(digest, input.AllocationID)
		writeDigestString(digest, input.SessionID)
		writeDigestUint64(digest, input.Generation)
		writeDigestString(digest, input.UserID)
		writeDigestUint64(digest, input.CatalogGeneration)
		writeDigestString(digest, input.ProblemID)
		writeDigestString(digest, input.ProblemRevision)
		writeDigestString(digest, input.ResourceProfile)
		writeDigestString(digest, input.ExpiresAt)
		writeDigestString(digest, input.IdempotencyKey)
	case GetRequest:
		writeDigestEnvelope(digest, OperationGet, PathGet, input.Schema, input.ControllerEpoch, input.EffectDeadline)
		writeDigestString(digest, input.AllocationID)
		writeDigestString(digest, input.SessionID)
		writeDigestUint64(digest, input.Generation)
	case DestroyRequest:
		writeDigestEnvelope(digest, OperationDestroy, PathDestroy, input.Schema, input.ControllerEpoch, input.EffectDeadline)
		writeDigestString(digest, input.AllocationID)
		writeDigestString(digest, input.SessionID)
		writeDigestUint64(digest, input.Generation)
	default:
		return [sha256.Size]byte{}, errors.New("unsupported private Runner request digest type")
	}
	var result [sha256.Size]byte
	copy(result[:], digest.Sum(nil))
	return result, nil
}

func writeDigestEnvelope(digest hash.Hash, operation, path, schema string, epoch uint64, deadline string) {
	writeDigestString(digest, operation)
	writeDigestString(digest, path)
	writeDigestString(digest, schema)
	writeDigestUint64(digest, epoch)
	writeDigestString(digest, deadline)
}

func writeDigestString(digest hash.Hash, value string) {
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(value)))
	_, _ = digest.Write(length[:])
	_, _ = digest.Write([]byte(value))
}

func writeDigestUint64(digest hash.Hash, value uint64) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], value)
	_, _ = digest.Write(encoded[:])
}
