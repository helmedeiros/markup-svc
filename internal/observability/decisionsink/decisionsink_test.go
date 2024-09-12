package decisionsink_test

import (
	"encoding/json"
	"testing"

	"github.com/helmedeiros/markup-svc/internal/observability/decisionsink"
)

// TestEvent_JSONTagsMatchADR0035 pins the wire contract. Adapter
// serialisation depends on these tags being stable; renaming any of
// them is a schema-breaking change per ADR-0035's versioning rules.
func TestEvent_JSONTagsMatchADR0035(t *testing.T) {
	e := decisionsink.Event{
		SchemaVersion: "1.0.0",
		DecisionID:    "det-1",
		Ts:            "2024-09-12T10:42:00.000000001Z",
		Env:           "production",
		ModelVersion:  "v1",
		Experiment:    "",
		EngineAdapter: "*indexed.Engine",
		Rule:          "enterprise",
		MarkupFactor:  1.15,
		DecideOutcome: "ok",
		Error:         "",
		DurationMS:    0.487,
		CorrelationID: "c-1",
		TraceID:       "t-1",
		SpanID:        "s-1",
		RequestContext: map[string]any{
			"country": "DE", "amount": 49.99,
		},
	}
	raw, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	want := []string{
		"schema_version", "decision_id", "ts", "env", "model_version",
		"experiment", "engine_adapter", "rule", "markup_factor",
		"decide_outcome", "error", "duration_ms", "correlation_id",
		"trace_id", "span_id", "request_context",
	}
	for _, k := range want {
		if _, ok := m[k]; !ok {
			t.Errorf("missing JSON key %q in serialised Event: %s", k, raw)
		}
	}
}

// TestNoopSink_DiscardsWithoutPanic pins the default-wired behaviour.
func TestNoopSink_DiscardsWithoutPanic(t *testing.T) {
	var s decisionsink.Sink = decisionsink.NoopSink{}
	s.Publish(decisionsink.Event{DecisionID: "x"})
	s.Publish(decisionsink.Event{})
}
