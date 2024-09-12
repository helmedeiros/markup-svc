// Package decisionsink is the port through which the /decide path
// publishes one markup.decision.v1 event per request to a durable
// substrate (S3/MinIO via the s3sink adapter; future Pub/Sub / Kafka
// adapters implement the same Sink interface).
//
// Event mirrors the ADR-0035 schema field-for-field with JSON tags so
// adapters that serialise to JSONL (s3sink) keep the wire shape stable
// without re-stating the contract in adapter code. The struct is the
// authoritative shape; access-log emission keeps using the existing
// map-shaped jsonlog API. Both consume the same Event.
package decisionsink

// SchemaV1 is the markup.decision schema_version value carried on
// every v1 event. Builders import this constant rather than spelling
// the literal so a grep finds every emission site at once.
const SchemaV1 = "1.0.0"

// Event is the typed projection of markup.decision.v1 (ADR-0035) that
// every Sink adapter receives. Adapters MUST treat the JSON tags as
// the wire contract for downstream consumers; renaming a tag is a
// schema-breaking change requiring a v2 ADR + dual-emission window.
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

// Sink accepts a markup.decision.v1 event for asynchronous delivery to
// a downstream substrate. Implementations document their durability,
// batching, and backpressure posture; the port contract is "non-blocking
// best-effort" — Publish MUST NOT block the /decide path on substrate
// IO, and MUST NOT return an error that callers would have to handle.
// Substrate failures surface as adapter-side counters per ADR-0036.
type Sink interface {
	Publish(event Event)
}

// NoopSink discards events. The default when no sink is wired.
// Operators flip to a real adapter via cmd flags.
type NoopSink struct{}

func (NoopSink) Publish(Event) {}
