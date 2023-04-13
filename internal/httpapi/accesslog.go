package httpapi

import (
	"net/http"
	"time"

	oteltrace "go.opentelemetry.io/otel/trace"

	breengine "github.com/helmedeiros/bre-go/engine"

	"github.com/helmedeiros/markup-svc/internal/jsonlog"
	"github.com/helmedeiros/markup-svc/internal/markup"
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
		if d, ok := decisionFromContext(r.Context()); ok {
			attrs["input"] = inputFields(d.request)
			if d.noMatch {
				attrs["no_match"] = true
			} else {
				attrs["rule"] = d.decision.Rule
				attrs["markup_factor"] = d.decision.MarkupFactor
				attrs["model_version"] = d.decision.ModelVersion
				if d.decision.Experiment != "" {
					attrs["experiment"] = d.decision.Experiment
				}
				attrs["engine_adapter"] = d.decision.EngineAdapter
			}
		}
		l.Info("markup-server.access", attrs)
	})
}

func inputFields(r markup.Request) map[string]any {
	out := map[string]any{}
	if r.ProductID != "" {
		out["product_id"] = r.ProductID
	}
	if r.Category != "" {
		out["category"] = r.Category
	}
	if r.CustomerTier != "" {
		out["customer_tier"] = r.CustomerTier
	}
	if r.Channel != "" {
		out["channel"] = r.Channel
	}
	if r.Country != "" {
		out["country"] = r.Country
	}
	if r.Inventory != "" {
		out["inventory"] = r.Inventory
	}
	if r.TimeWindow != "" {
		out["time_window"] = r.TimeWindow
	}
	if r.Amount != 0 {
		out["amount"] = r.Amount
	}
	return out
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
