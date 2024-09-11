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
// When env is non-empty, attrs.env carries the process-level
// environment identifier (ADR-0034).
//
// When the inner handler populated decisionLogEntry, the middleware
// also emits a parallel "markup.decision.v1" event carrying the
// ADR-0035 contract. The two events share the same logger and
// correlation identity; downstream consumers filter by msg.
func WithAccessLog(l *jsonlog.Logger, env string, next http.Handler) http.Handler {
	if l == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		durationMS := float64(time.Since(start)) / float64(time.Millisecond)
		ctx := r.Context()
		cid := breengine.CorrelationIDFromContext(ctx)
		var traceID, spanID string
		if sc := oteltrace.SpanContextFromContext(ctx); sc.IsValid() {
			traceID = sc.TraceID().String()
			spanID = sc.SpanID().String()
		}
		attrs := map[string]any{
			"method":      r.Method,
			"path":        r.URL.Path,
			"status":      sw.status,
			"duration_ms": durationMS,
		}
		if env != "" {
			attrs["env"] = env
		}
		if cid != "" {
			attrs["correlation_id"] = cid
		}
		if traceID != "" {
			attrs["trace_id"] = traceID
			attrs["span_id"] = spanID
		}
		d, hasDecision := decisionFromContext(ctx)
		var ctxFields map[string]any
		if hasDecision {
			ctxFields = inputFields(d.request)
			attrs["input"] = ctxFields
			if d.noMatch {
				attrs["no_match"] = true
			} else if d.outcome == "" || d.outcome == "ok" {
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
		// markup.decision.v1 (ADR-0035) emits only when the Decide
		// handler populated decisionID; a test that constructs a
		// decisionLogEntry directly does not trigger the new event.
		if hasDecision && d.decisionID != "" {
			l.Info("markup.decision.v1", decisionEventAttrs(d, env, durationMS, start, cid, traceID, spanID, ctxFields))
		}
	})
}

// decisionEventAttrs builds the ADR-0035 markup.decision.v1 attribute
// set. ts is the request start (the closest time we have to "when the
// decision was made"); duration_ms is the full /decide handler
// envelope, matching the access-log semantic. Every field in the v1
// schema is emitted explicitly — empty strings and zero floats are
// stable columns for downstream batch consumers (Spark / Snowflake),
// not sparse keys.
func decisionEventAttrs(d decisionLogEntry, env string, durationMS float64, start time.Time, cid, traceID, spanID string, context map[string]any) map[string]any {
	attrs := map[string]any{
		"schema_version":  "1.0.0",
		"decision_id":     d.decisionID,
		"ts":              start.UTC().Format(time.RFC3339Nano),
		"env":             env,
		"decide_outcome":  d.outcome,
		"duration_ms":     durationMS,
		"correlation_id":  cid,
		"trace_id":        traceID,
		"span_id":         spanID,
		"error":           d.errorMsg,
		"request_context": context,
	}
	if d.outcome == "ok" {
		attrs["model_version"] = d.decision.ModelVersion
		attrs["experiment"] = d.decision.Experiment
		attrs["engine_adapter"] = d.decision.EngineAdapter
		attrs["rule"] = d.decision.Rule
		attrs["markup_factor"] = d.decision.MarkupFactor
	} else {
		attrs["model_version"] = ""
		attrs["experiment"] = ""
		attrs["engine_adapter"] = ""
		attrs["rule"] = ""
		attrs["markup_factor"] = 0.0
	}
	return attrs
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
