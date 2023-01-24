package guardrails_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/helmedeiros/markup-svc/internal/decider/guardrails"
	"github.com/helmedeiros/markup-svc/internal/markup"
	"github.com/helmedeiros/markup-svc/internal/observability/metrics"
	mkotel "github.com/helmedeiros/markup-svc/internal/observability/otel"
)

// Composition test validating ADR-0014's claim that placing guardrails
// inside the OTel decorator and outside the engine causes vetoes to
// surface as codes.Error on the trace span AND as Err-classified
// events on the metrics Sink, with the wrapped ErrGuardrailViolation
// reachable from both via errors.Is.

func TestVetoSurfacesOnBothMetricsAndOtel(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	sink := &metrics.RecordingSink{}

	inner := stubDecider{decision: markup.Decision{
		MarkupFactor:  5.0,
		Rule:          "boom",
		ModelVersion:  "v1",
		EngineAdapter: "*test.Engine",
	}}

	// Composition per ADR-0014: metrics → otel → guardrails → inner.
	// The veto must appear on both observability layers.
	stack := metrics.Wrap(
		mkotel.Wrap(
			guardrails.New(inner, guardrails.FactorRange{Min: 0.5, Max: 3.0}),
			tp.Tracer("test"),
		),
		sink,
	)

	_, err := stack.Decide(context.Background(), markup.Request{})

	// Caller sees the wrapped sentinel.
	if !errors.Is(err, guardrails.ErrGuardrailViolation) {
		t.Fatalf("errors.Is(err, ErrGuardrailViolation) = false; err = %v", err)
	}
	if !strings.Contains(err.Error(), "above max") {
		t.Fatalf("err = %q, want reason mentioning 'above max'", err.Error())
	}

	// Metrics Sink sees the veto as Err (not NoMatch, not Canceled).
	recs := sink.Records()
	if len(recs) != 1 {
		t.Fatalf("len(records) = %d, want 1", len(recs))
	}
	rec0 := recs[0]
	if rec0.NoMatch {
		t.Error("metric.NoMatch = true; guardrail veto should classify as Err")
	}
	if rec0.Canceled {
		t.Error("metric.Canceled = true; guardrail veto should classify as Err")
	}
	if rec0.Err == nil {
		t.Fatal("metric.Err = nil; guardrail veto should populate Err")
	}
	if !errors.Is(rec0.Err, guardrails.ErrGuardrailViolation) {
		t.Errorf("metric.Err does not wrap ErrGuardrailViolation: %v", rec0.Err)
	}

	// OTel span sees the veto as codes.Error with the wrapped message.
	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("len(spans) = %d, want 1", len(spans))
	}
	span := spans[0]
	if span.Status().Code != codes.Error {
		t.Errorf("span.Status.Code = %v, want codes.Error", span.Status().Code)
	}
	if !strings.Contains(span.Status().Description, "guardrails") {
		t.Errorf("span.Status.Description = %q, want it to mention guardrails", span.Status().Description)
	}
}

// TestInnerErrNoMatchPassesThroughCleanly confirms guardrails does NOT
// wrap an inner ErrNoMatch as ErrGuardrailViolation under composition.
// This is what makes the metrics decorator classify the no-match event
// as NoMatch (the documented ADR-0010 outcome) rather than Err -- a
// regression that wrapped inner errors would inflate the Err-rate
// dashboard with every no-match a CSV ever produces.
func TestInnerErrNoMatchPassesThroughCleanly(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	sink := &metrics.RecordingSink{}

	inner := stubDecider{err: markup.ErrNoMatch}

	stack := metrics.Wrap(
		mkotel.Wrap(
			guardrails.New(inner, guardrails.FactorRange{Min: 0.5, Max: 3.0}),
			tp.Tracer("test"),
		),
		sink,
	)

	_, err := stack.Decide(context.Background(), markup.Request{})

	if !errors.Is(err, markup.ErrNoMatch) {
		t.Fatalf("errors.Is(err, ErrNoMatch) = false; err = %v", err)
	}
	if errors.Is(err, guardrails.ErrGuardrailViolation) {
		t.Fatal("ErrNoMatch was wrapped as ErrGuardrailViolation; pass-through invariant broken")
	}

	recs := sink.Records()
	if len(recs) != 1 {
		t.Fatalf("len(records) = %d, want 1", len(recs))
	}
	if !recs[0].NoMatch {
		t.Error("metric.NoMatch = false; inner ErrNoMatch must classify as NoMatch, not Err")
	}
	if recs[0].Err != nil {
		t.Errorf("metric.Err = %v on ErrNoMatch path; want nil", recs[0].Err)
	}
}

// TestSuccessLeavesObservabilityClean confirms the happy-path composition:
// when all rules allow, no error reaches the metrics Sink and the span is
// recorded OK (not Error). This is the regression guard for "guardrails
// accidentally always vetoes" silently passing the prior test.
func TestSuccessLeavesObservabilityClean(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	sink := &metrics.RecordingSink{}

	inner := stubDecider{decision: markup.Decision{
		MarkupFactor:  1.25,
		Rule:          "calm",
		EngineAdapter: "*test.Engine",
	}}

	stack := metrics.Wrap(
		mkotel.Wrap(
			guardrails.New(inner, guardrails.FactorRange{Min: 0.5, Max: 3.0}),
			tp.Tracer("test"),
		),
		sink,
	)

	got, err := stack.Decide(context.Background(), markup.Request{})
	if err != nil {
		t.Fatalf("Decide returned error: %v", err)
	}
	if got.MarkupFactor != 1.25 {
		t.Errorf("Decision.MarkupFactor = %v, want 1.25", got.MarkupFactor)
	}

	recs := sink.Records()
	if len(recs) != 1 {
		t.Fatalf("len(records) = %d, want 1", len(recs))
	}
	if recs[0].Err != nil {
		t.Errorf("metric.Err = %v on happy path; want nil", recs[0].Err)
	}

	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("len(spans) = %d, want 1", len(spans))
	}
	if spans[0].Status().Code == codes.Error {
		t.Errorf("span.Status.Code = Error on happy path; want OK or Unset")
	}
}
