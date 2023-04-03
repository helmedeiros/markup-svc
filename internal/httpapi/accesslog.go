package httpapi

import (
	"net/http"
	"time"

	oteltrace "go.opentelemetry.io/otel/trace"

	breengine "github.com/helmedeiros/bre-go/engine"

	"github.com/helmedeiros/markup-svc/internal/jsonlog"
)

// WithAccessLog returns middleware that emits one JSON event per
// request as "markup-server.access" with attrs {method, path, status,
// duration_ms, correlation_id, trace_id, span_id}. See ADR-0021.
func WithAccessLog(l *jsonlog.Logger, next http.Handler) http.Handler {
	if l == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		attrs := map[string]any{
			"method":      r.Method,
			"path":        r.URL.Path,
			"status":      sw.status,
			"duration_ms": float64(time.Since(start)) / float64(time.Millisecond),
		}
		if cid := breengine.CorrelationIDFromContext(r.Context()); cid != "" {
			attrs["correlation_id"] = cid
		}
		if sc := oteltrace.SpanContextFromContext(r.Context()); sc.IsValid() {
			attrs["trace_id"] = sc.TraceID().String()
			attrs["span_id"] = sc.SpanID().String()
		}
		l.Info("markup-server.access", attrs)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.wroteHeader {
		return
	}
	s.wroteHeader = true
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if !s.wroteHeader {
		s.wroteHeader = true
	}
	return s.ResponseWriter.Write(b)
}
