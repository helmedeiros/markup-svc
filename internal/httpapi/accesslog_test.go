package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/helmedeiros/markup-svc/internal/jsonlog"
	"github.com/helmedeiros/markup-svc/internal/markup"
)

func TestWithAccessLog_EmitsExpectedAttrs(t *testing.T) {
	var buf bytes.Buffer
	l := jsonlog.New(&buf)
	h := WithAccessLog(l, "", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/decide", nil))

	if rec.Code != http.StatusTeapot {
		t.Errorf("status=%d", rec.Code)
	}
	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v ; raw=%q", err, buf.String())
	}
	if got["msg"] != "markup-server.access" {
		t.Errorf("msg=%v", got["msg"])
	}
	attrs := got["attrs"].(map[string]any)
	if attrs["method"] != "POST" || attrs["path"] != "/decide" || attrs["status"].(float64) != 418 {
		t.Errorf("attrs=%v", attrs)
	}
	if _, ok := attrs["duration_ms"].(float64); !ok {
		t.Errorf("duration_ms missing/not float: %v", attrs["duration_ms"])
	}
}

func TestWithAccessLog_EnrichesWithRuleAndInputAndOutput(t *testing.T) {
	var buf bytes.Buffer
	l := jsonlog.New(&buf)
	// Inner handler: simulate what httpapi.Decide does -- set the
	// decision-context on the request before responding.
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := markup.Request{
			ProductID:    "p-1",
			CustomerTier: "enterprise",
			Country:      "DE",
		}
		dec := markup.Decision{
			Rule:          "enterprise",
			MarkupFactor:  1.15,
			ModelVersion:  "v1",
			EngineAdapter: "*inmemory.Engine",
		}
		// Mirror the Decide handler's *r = *r.WithContext(...) trick
		// so middleware further out can read the value.
		*r = *r.WithContext(withDecisionContext(r.Context(), decisionLogEntry{request: req, decision: dec}))
		w.WriteHeader(http.StatusOK)
	})
	h := WithAccessLog(l, "", inner)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/decide", nil))

	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	attrs := got["attrs"].(map[string]any)
	if attrs["rule"] != "enterprise" {
		t.Errorf("rule = %v", attrs["rule"])
	}
	if attrs["markup_factor"].(float64) != 1.15 {
		t.Errorf("markup_factor = %v", attrs["markup_factor"])
	}
	if attrs["model_version"] != "v1" {
		t.Errorf("model_version = %v", attrs["model_version"])
	}
	if attrs["engine_adapter"] != "*inmemory.Engine" {
		t.Errorf("engine_adapter = %v", attrs["engine_adapter"])
	}
	input := attrs["input"].(map[string]any)
	if input["product_id"] != "p-1" || input["customer_tier"] != "enterprise" || input["country"] != "DE" {
		t.Errorf("input = %v", input)
	}
}

func TestWithAccessLog_NoMatchSetsNoMatchTrue(t *testing.T) {
	var buf bytes.Buffer
	l := jsonlog.New(&buf)
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := markup.Request{Country: "ZZ"}
		*r = *r.WithContext(withDecisionContext(r.Context(), decisionLogEntry{request: req, noMatch: true}))
		w.WriteHeader(http.StatusNotFound)
	})
	WithAccessLog(l, "", inner).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/decide", nil))

	var got map[string]any
	_ = json.Unmarshal(buf.Bytes(), &got)
	attrs := got["attrs"].(map[string]any)
	if attrs["no_match"] != true {
		t.Errorf("no_match = %v", attrs["no_match"])
	}
	if _, has := attrs["rule"]; has {
		t.Errorf("rule should be absent on no_match; got %v", attrs["rule"])
	}
	if attrs["input"].(map[string]any)["country"] != "ZZ" {
		t.Errorf("input not propagated on no_match: %v", attrs["input"])
	}
}

func TestWithAccessLog_StampsEnvWhenSet(t *testing.T) {
	var buf bytes.Buffer
	l := jsonlog.New(&buf)
	h := WithAccessLog(l, "production", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/decide", nil))
	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v ; raw=%q", err, buf.String())
	}
	attrs := got["attrs"].(map[string]any)
	if attrs["env"] != "production" {
		t.Fatalf("env attr = %v, want production", attrs["env"])
	}
}

func TestWithAccessLog_OmitsEnvAttrWhenEmpty(t *testing.T) {
	var buf bytes.Buffer
	l := jsonlog.New(&buf)
	h := WithAccessLog(l, "", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/decide", nil))
	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v ; raw=%q", err, buf.String())
	}
	attrs := got["attrs"].(map[string]any)
	if _, ok := attrs["env"]; ok {
		t.Fatalf("env attr should be omitted when empty; got %v", attrs["env"])
	}
}

// TestWithAccessLog_EmitsMarkupDecisionV1OnOk pins the ADR-0035 contract:
// when decisionLogEntry carries a decisionID, a second JSON event with
// msg="markup.decision.v1" emits alongside the access event.
func TestWithAccessLog_EmitsMarkupDecisionV1OnOk(t *testing.T) {
	var buf bytes.Buffer
	l := jsonlog.New(&buf)
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := markup.Request{ProductID: "p-1", Country: "DE", Amount: 49.99}
		dec := markup.Decision{Rule: "enterprise", MarkupFactor: 1.15, ModelVersion: "v1", EngineAdapter: "*indexed.Engine"}
		*r = *r.WithContext(withDecisionContext(r.Context(), decisionLogEntry{
			request: req, decision: dec, outcome: "ok", decisionID: "det-id-123",
		}))
		w.WriteHeader(http.StatusOK)
	})
	WithAccessLog(l, "production", inner).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/decide", nil))

	events := decodeJSONLines(t, buf.Bytes())
	if len(events) != 2 {
		t.Fatalf("event count = %d, want 2; raw=%q", len(events), buf.String())
	}
	decision := events[1]
	if decision["msg"] != "markup.decision.v1" {
		t.Fatalf("second event msg = %v, want markup.decision.v1", decision["msg"])
	}
	attrs := decision["attrs"].(map[string]any)
	if attrs["schema_version"] != "1.0.0" {
		t.Errorf("schema_version = %v", attrs["schema_version"])
	}
	if attrs["decision_id"] != "det-id-123" {
		t.Errorf("decision_id = %v", attrs["decision_id"])
	}
	if attrs["env"] != "production" {
		t.Errorf("env = %v", attrs["env"])
	}
	if attrs["decide_outcome"] != "ok" {
		t.Errorf("decide_outcome = %v", attrs["decide_outcome"])
	}
	if attrs["rule"] != "enterprise" {
		t.Errorf("rule = %v", attrs["rule"])
	}
	if attrs["markup_factor"].(float64) != 1.15 {
		t.Errorf("markup_factor = %v", attrs["markup_factor"])
	}
	if attrs["model_version"] != "v1" {
		t.Errorf("model_version = %v", attrs["model_version"])
	}
	if attrs["engine_adapter"] != "*indexed.Engine" {
		t.Errorf("engine_adapter = %v", attrs["engine_adapter"])
	}
	if _, ok := attrs["ts"].(string); !ok {
		t.Errorf("ts missing/not string: %v", attrs["ts"])
	}
	if _, ok := attrs["duration_ms"].(float64); !ok {
		t.Errorf("duration_ms missing/not float: %v", attrs["duration_ms"])
	}
	ctx := attrs["request_context"].(map[string]any)
	if ctx["product_id"] != "p-1" || ctx["country"] != "DE" || ctx["amount"].(float64) != 49.99 {
		t.Errorf("request_context = %v", ctx)
	}
}

// TestWithAccessLog_EmitsMarkupDecisionV1OnEachOutcome covers the
// decide_outcome enum (no_match | canceled | deadline_exceeded | error)
// and confirms the rule/factor/model attrs are empty-string / zero on
// non-ok outcomes per the ADR-0035 schema's "explicit empty" rule.
func TestWithAccessLog_EmitsMarkupDecisionV1OnEachOutcome(t *testing.T) {
	cases := []struct {
		name        string
		outcome     string
		noMatch     bool
		errorMsg    string
		wantError   string
		wantNoMatch bool
	}{
		{"no_match", "no_match", true, "", "", true},
		{"canceled", "canceled", false, "context canceled", "context canceled", false},
		{"deadline", "deadline_exceeded", false, "context deadline exceeded", "context deadline exceeded", false},
		{"error", "error", false, "boom", "boom", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			l := jsonlog.New(&buf)
			inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				req := markup.Request{Country: "ZZ"}
				*r = *r.WithContext(withDecisionContext(r.Context(), decisionLogEntry{
					request: req, noMatch: tc.noMatch, outcome: tc.outcome, errorMsg: tc.errorMsg, decisionID: "det-" + tc.name,
				}))
				w.WriteHeader(http.StatusOK)
			})
			WithAccessLog(l, "production", inner).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/decide", nil))
			events := decodeJSONLines(t, buf.Bytes())
			if len(events) != 2 {
				t.Fatalf("event count = %d, want 2", len(events))
			}
			attrs := events[1]["attrs"].(map[string]any)
			if attrs["decide_outcome"] != tc.outcome {
				t.Errorf("decide_outcome = %v", attrs["decide_outcome"])
			}
			if attrs["error"] != tc.wantError {
				t.Errorf("error = %v", attrs["error"])
			}
			if attrs["rule"] != "" || attrs["model_version"] != "" || attrs["engine_adapter"] != "" {
				t.Errorf("non-ok outcome leaked decision attrs: rule=%v model=%v adapter=%v", attrs["rule"], attrs["model_version"], attrs["engine_adapter"])
			}
			if attrs["markup_factor"].(float64) != 0.0 {
				t.Errorf("markup_factor = %v on non-ok outcome, want 0", attrs["markup_factor"])
			}
		})
	}
}

// TestWithAccessLog_OmitsMarkupDecisionV1WhenNoDecisionID confirms a
// legacy decisionLogEntry without decisionID (e.g. populated directly
// by a test) does NOT trigger the new event; access log still works.
func TestWithAccessLog_OmitsMarkupDecisionV1WhenNoDecisionID(t *testing.T) {
	var buf bytes.Buffer
	l := jsonlog.New(&buf)
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := markup.Request{Country: "ZZ"}
		*r = *r.WithContext(withDecisionContext(r.Context(), decisionLogEntry{request: req, noMatch: true}))
		w.WriteHeader(http.StatusNotFound)
	})
	WithAccessLog(l, "production", inner).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/decide", nil))
	events := decodeJSONLines(t, buf.Bytes())
	if len(events) != 1 {
		t.Fatalf("event count = %d, want 1 (no decisionID = no markup.decision.v1)", len(events))
	}
	if events[0]["msg"] != "markup-server.access" {
		t.Errorf("msg = %v", events[0]["msg"])
	}
}

func decodeJSONLines(t *testing.T, raw []byte) []map[string]any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	var out []map[string]any
	for dec.More() {
		var m map[string]any
		if err := dec.Decode(&m); err != nil {
			t.Fatalf("decode: %v ; raw=%q", err, raw)
		}
		out = append(out, m)
	}
	return out
}

func TestWithAccessLog_NilLoggerIsPassThrough(t *testing.T) {
	called := false
	h := WithAccessLog(nil, "", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if !called {
		t.Errorf("inner not invoked")
	}
}
