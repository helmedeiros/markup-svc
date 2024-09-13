// Package decisionsink is the port through which the /decide path
// publishes markup.decision.v1 events to a durable substrate. See
// ADR-0035 (schema) and ADR-0036 (substrate adapter).
package decisionsink

const SchemaV1 = "1.0.0"

type Event struct {
	SchemaVersion  string         `json:"schema_version"`
	DecisionID     string         `json:"decision_id"`
	Ts             string         `json:"ts"`
	Env            string         `json:"env"`
	ModelVersion   string         `json:"model_version"`
	Experiment     string         `json:"experiment"`
	EngineAdapter  string         `json:"engine_adapter"`
	Rule           string         `json:"rule"`
	MarkupFactor   float64        `json:"markup_factor"`
	DecideOutcome  string         `json:"decide_outcome"`
	Error          string         `json:"error"`
	DurationMS     float64        `json:"duration_ms"`
	CorrelationID  string         `json:"correlation_id"`
	TraceID        string         `json:"trace_id"`
	SpanID         string         `json:"span_id"`
	RequestContext map[string]any `json:"request_context"`
}

// Sink accepts an event for asynchronous delivery. Publish MUST NOT
// block on substrate IO.
type Sink interface {
	Publish(event Event)
}

type NoopSink struct{}

func (NoopSink) Publish(Event) {}

type Logger interface {
	Info(msg string, attrs map[string]any)
}

type Metrics interface {
	IncDropped(reason string, n int)
	IncFlushed(events int, bytes int)
}
