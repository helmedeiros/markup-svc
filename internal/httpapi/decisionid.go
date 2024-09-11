package httpapi

import (
	"crypto/rand"
	"encoding/hex"
)

// newDecisionID mints a 32-character lowercase-hex per-decision ID
// (128 bits of entropy from crypto/rand). The format is documented as
// opaque to downstream consumers per ADR-0035.
func newDecisionID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failure is fatal on healthy hosts; empty string
		// signals skip to WithAccessLog which omits the v1 event.
		return ""
	}
	return hex.EncodeToString(b[:])
}
