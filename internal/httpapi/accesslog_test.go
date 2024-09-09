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
