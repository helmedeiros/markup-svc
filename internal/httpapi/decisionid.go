package httpapi

import (
	"crypto/rand"
	"encoding/hex"
)

// newDecisionID returns 16 random bytes hex-encoded, or "" on
// crypto/rand failure (WithAccessLog skips emission on empty IDs).
func newDecisionID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	return hex.EncodeToString(b[:])
}
