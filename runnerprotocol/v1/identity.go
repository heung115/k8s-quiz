package v1

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
)

// AllocationIDForSession derives the provider-neutral, UUID-shaped identity
// shared by the Control Plane, private Runner transport, and every provider.
// The algorithm is part of the durable v1 contract: changing it would break
// retries and reconciliation for existing session generations.
func AllocationIDForSession(sessionID string, generation uint64) string {
	sum := sha256.Sum256([]byte("k8s-quiz/allocation/v1\x00" + sessionID + "\x00" + strconv.FormatUint(generation, 10)))
	bytes := sum[:16]
	bytes[6] = (bytes[6] & 0x0f) | 0x50
	bytes[8] = (bytes[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(bytes)
	return fmt.Sprintf("%s-%s-%s-%s-%s", encoded[:8], encoded[8:12], encoded[12:16], encoded[16:20], encoded[20:])
}
