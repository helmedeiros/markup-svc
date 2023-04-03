package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	mkotel "github.com/helmedeiros/markup-svc/internal/observability/otel"
)

// TestE2EOTelSpansEmittedOverHTTP is the load-bearing test for the
// W8 Fri cmd wiring: with a recorder-backed tracer composed via
// wireTracedHandler, a real HTTP POST to /decide emits exactly one
// span whose attributes contain the markup-domain rule.markup.* keys
// promised by ADR-0009. Without this test the cmd-side composition
// could silently regress (e.g., otel.Wrap accidentally placed
// underneath swap.Decider) without breaking the existing /decide
// or /admin/reload tests.
func TestE2EOTelSpansEmittedOverHTTP(t *testing.T) {
	rulesPath := filepath.Join(t.TempDir(), "rules.csv")
	if err := writeFile(t, rulesPath, "name,condition,factor,priority\nenterprise,customer_tier == 'enterprise',1.15,10\n"); err != nil {
		t.Fatal(err)
	}

	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	tracer := tp.Tracer("markup-svc-e2e")

	loader := rulesLoader(rulesPath, "inmemory", "v0-otel", io.Discard)
	handler, _, err := wireTracedHandler(loader, tracer, guardrailsWire{}, metricsWiring{}, nil)
	if err != nil {
		t.Fatalf("wireTracedHandler: %v", err)
	}
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	resp, err := http.Post(srv.URL+"/decide", "application/json",
		strings.NewReader(`{"customer_tier":"enterprise"}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	spans := rec.Ended()
	// ADR-0017 changed the span model: per Decide call the binary
	// emits two spans (no guardrails active in this test): the inner
	// markup.engine.evaluate (child) and the outer markup.decider.decide
	// (parent). Both carry the rule.markup.* attribute set; the test
	// asserts on the outer span which is what operators key dashboards
	// off historically.
	if len(spans) != 2 {
		t.Fatalf("len(spans) = %d, want 2 (engine.evaluate + decider.decide)", len(spans))
	}
	var got = spans[0]
	for _, s := range spans {
		if s.Name() == "markup.decider.decide" {
			got = s
			break
		}
	}
	if got.Name() != "markup.decider.decide" {
		t.Errorf("did not find markup.decider.decide span; got names = %v", spanNames(spans))
	}
	attrs := got.Attributes()
	if v, ok := findAttr(attrs, mkotel.AttrAdapter); !ok || v.AsString() != "*inmemory.Engine" {
		t.Errorf("%s = %v ok=%v, want \"*inmemory.Engine\"", mkotel.AttrAdapter, v.AsString(), ok)
	}
	if v, ok := findAttr(attrs, mkotel.AttrRule); !ok || v.AsString() != "enterprise" {
		t.Errorf("%s = %v ok=%v, want \"enterprise\"", mkotel.AttrRule, v.AsString(), ok)
	}
	if v, ok := findAttr(attrs, mkotel.AttrFactor); !ok || v.AsFloat64() != 1.15 {
		t.Errorf("%s = %v ok=%v, want 1.15", mkotel.AttrFactor, v.AsFloat64(), ok)
	}
	if v, ok := findAttr(attrs, mkotel.AttrModelVersion); !ok || v.AsString() != "v0-otel" {
		t.Errorf("%s = %v ok=%v, want \"v0-otel\"", mkotel.AttrModelVersion, v.AsString(), ok)
	}
}

// TestE2EOTelSpansContinueAfterReload pins the composition correctness
// from ADR-0009: hot reloads must not lose tracing. If otel.Wrap were
// placed inside swap (under it) instead of outside, the new Decider
// installed by Swap would not be wrapped and subsequent /decide calls
// would emit no spans. This test catches that regression.
func TestE2EOTelSpansContinueAfterReload(t *testing.T) {
	rulesPath := filepath.Join(t.TempDir(), "rules.csv")
	if err := writeFile(t, rulesPath, "name,condition,factor,priority\nenterprise,customer_tier == 'enterprise',1.15,10\n"); err != nil {
		t.Fatal(err)
	}

	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	tracer := tp.Tracer("markup-svc-e2e")

	loader := rulesLoader(rulesPath, "inmemory", "v0-otel", io.Discard)
	handler, _, err := wireTracedHandler(loader, tracer, guardrailsWire{}, metricsWiring{}, nil)
	if err != nil {
		t.Fatalf("wireTracedHandler: %v", err)
	}
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	// First /decide -> span 1.
	must200(t, srv.URL+"/decide", `{"customer_tier":"enterprise"}`)

	// Overwrite CSV with a different factor and reload.
	if err := writeFile(t, rulesPath, "name,condition,factor,priority\nenterprise,customer_tier == 'enterprise',1.42,10\n"); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(srv.URL+"/admin/reload", "application/json", nil)
	if err != nil {
		t.Fatalf("POST /admin/reload: %v", err)
	}
	resp.Body.Close()

	// Second /decide -> span 2 (post-reload).
	must200(t, srv.URL+"/decide", `{"customer_tier":"enterprise"}`)

	spans := rec.Ended()
	// Per ADR-0017 each /decide emits two spans (no guardrails active
	// in this test): markup.engine.evaluate + markup.decider.decide.
	// Two /decide calls = four spans; assert the per-call pair shape.
	if len(spans) != 4 {
		t.Fatalf("len(spans) = %d, want 4 (two /decide x two spans each)", len(spans))
	}
	deciderCount := 0
	engineCount := 0
	for _, s := range spans {
		switch s.Name() {
		case "markup.decider.decide":
			deciderCount++
		case "markup.engine.evaluate":
			engineCount++
		default:
			t.Errorf("unexpected span name %q", s.Name())
		}
	}
	if deciderCount != 2 || engineCount != 2 {
		t.Errorf("span name counts: decider=%d engine=%d, want 2/2", deciderCount, engineCount)
	}
}

func spanNames(spans []sdktrace.ReadOnlySpan) []string {
	out := make([]string, len(spans))
	for i, s := range spans {
		out[i] = s.Name()
	}
	return out
}

func TestWireHandlerWithoutTracerProducesNoSpans(t *testing.T) {
	rulesPath := filepath.Join(t.TempDir(), "rules.csv")
	if err := writeFile(t, rulesPath, "name,condition,factor,priority\nenterprise,customer_tier == 'enterprise',1.15,10\n"); err != nil {
		t.Fatal(err)
	}

	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	loader := rulesLoader(rulesPath, "inmemory", "v0-otel", io.Discard)
	// Use wireHandler (no tracer). Even if a tracer provider exists,
	// no spans should be recorded for our wrapper since otel.Wrap
	// was never applied.
	handler, _, err := wireHandler(loader)
	if err != nil {
		t.Fatalf("wireHandler: %v", err)
	}
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	must200(t, srv.URL+"/decide", `{"customer_tier":"enterprise"}`)
	if len(rec.Ended()) != 0 {
		t.Errorf("recorder saw %d spans without tracer wiring; want 0", len(rec.Ended()))
	}
}

func writeFile(t *testing.T, path, body string) error {
	t.Helper()
	return os.WriteFile(path, []byte(body), 0o644)
}

func must200(t *testing.T, url, body string) {
	t.Helper()
	resp, err := http.Post(url, "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST %s status = %d; body=%s", url, resp.StatusCode, raw)
	}
}

func findAttr(kv []attribute.KeyValue, key string) (attribute.Value, bool) {
	for _, a := range kv {
		if string(a.Key) == key {
			return a.Value, true
		}
	}
	return attribute.Value{}, false
}
