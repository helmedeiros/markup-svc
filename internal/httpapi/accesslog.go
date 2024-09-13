package httpapi

import (
	"net/http"
	"time"

	oteltrace "go.opentelemetry.io/otel/trace"

	breengine "github.com/helmedeiros/bre-go/engine"

	"github.com/helmedeiros/markup-svc/internal/jsonlog"
	"github.com/helmedeiros/markup-svc/internal/markup"
	"github.com/helmedeiros/markup-svc/internal/observability/decisionsink"
)

// WithAccessLog emits markup-server.access (ADR-0021) per request.
// When decisionLogEntry is populated, it also emits markup.decision.v1
// (ADR-0035) and, if a non-Noop sink is wired, Publishes the typed
// Event (ADR-0036). sinkEnabled is captured at construction so the
// default deployment pays no per-request sink cost.
func WithAccessLog(l *jsonlog.Logger, env string, sink decisionsink.Sink, next http.Handler) http.Handler {
	if l == nil {
		return next
	}
	sinkEnabled := sink != nil
	if _, noop := sink.(decisionsink.NoopSink); noop {
		sinkEnabled = false
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
				// outcome "" = legacy test path bypassing ADR-0035 mapping.
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
		if hasDecision && d.decisionID != "" {
			ts := start.UTC().Format(time.RFC3339Nano)
			l.Info("markup.decision.v1", decisionEventAttrs(d, env, durationMS, ts, cid, traceID, spanID, ctxFields))
			if sinkEnabled {
				sink.Publish(buildSinkEvent(d, env, durationMS, ts, cid, traceID, spanID, ctxFields))
			}
		}
	})
}

// buildSinkEvent's non-ok branch is explicit so a future Event field
// gaining a non-zero default cannot leak stale values onto the sink.
func buildSinkEvent(d decisionLogEntry, env string, durationMS float64, ts string, cid, traceID, spanID string, context map[string]any) decisionsink.Event {
	e := decisionsink.Event{
		SchemaVersion:  decisionsink.SchemaV1,
		DecisionID:     d.decisionID,
		Ts:             ts,
		Env:            env,
		DecideOutcome:  d.outcome,
		Error:          d.errorMsg,
		DurationMS:     durationMS,
		CorrelationID:  cid,
		TraceID:        traceID,
		SpanID:         spanID,
		RequestContext: context,
	}
	if d.outcome == "ok" {
		e.ModelVersion = d.decision.ModelVersion
		e.Experiment = d.decision.Experiment
		e.EngineAdapter = d.decision.EngineAdapter
		e.Rule = d.decision.Rule
		e.MarkupFactor = d.decision.MarkupFactor
	} else {
		e.ModelVersion = ""
		e.Experiment = ""
		e.EngineAdapter = ""
		e.Rule = ""
		e.MarkupFactor = 0.0
	}
	return e
}

// decisionEventAttrs emits every v1 field explicitly so downstream
// batch consumers see stable columns rather than sparse keys.
func decisionEventAttrs(d decisionLogEntry, env string, durationMS float64, ts string, cid, traceID, spanID string, context map[string]any) map[string]any {
	attrs := map[string]any{
		"schema_version":  decisionsink.SchemaV1,
		"decision_id":     d.decisionID,
		"ts":              ts,
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
