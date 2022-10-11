package httpapi

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"

	breengine "github.com/helmedeiros/bre-go/engine"
)

// CorrelationIDHeader is the HTTP header name carrying the cross-system
// trace identifier on both the request (caller-supplied) and the
// response (echoed back).
const CorrelationIDHeader = "X-Correlation-ID"

// randRead is the source of UUID bytes. Production code uses
// crypto/rand; tests substitute a failing reader to exercise the
// error path.
var randRead = rand.Read

// WithCorrelationID is HTTP middleware that guarantees every
// downstream handler sees a correlation ID on its request context,
// injected via engine.WithCorrelationID so the Decider's Decision
// populates Decision.CorrelationID with the same value. The ID is
// taken from the X-Correlation-ID request header if non-empty;
// otherwise generated as a crypto/rand-backed UUID v4. The active ID
// is echoed on the response header regardless of source. If UUID
// generation fails (essentially impossible on healthy systems), the
// middleware responds 500 with an opaque body.
func WithCorrelationID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(CorrelationIDHeader)
		if id == "" {
			generated, err := generateUUID()
			if err != nil {
				writeError(w, http.StatusInternalServerError, "internal")
				return
			}
			id = generated
		}
		w.Header().Set(CorrelationIDHeader, id)
		ctx := breengine.WithCorrelationID(r.Context(), id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// generateUUID returns an RFC 4122 v4 UUID drawn from crypto/rand.
func generateUUID() (string, error) {
	var b [16]byte
	if _, err := randRead(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		hex.EncodeToString(b[0:4]),
		hex.EncodeToString(b[4:6]),
		hex.EncodeToString(b[6:8]),
		hex.EncodeToString(b[8:10]),
		hex.EncodeToString(b[10:16])), nil
}
