package otel_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	oteltrace "go.opentelemetry.io/otel/trace"

	breengine "github.com/helmedeiros/bre-go/engine"

	mkotel "github.com/helmedeiros/markup-svc/internal/observability/otel"

	"github.com/helmedeiros/markup-svc/internal/markup"
)

type stubDecider struct {
	decision markup.Decision
	err      error
}

func (s *stubDecider) Decide(ctx context.Context, req markup.Request) (markup.Decision, error) {
	return s.decision, s.err
}

func recorderTracer(t *testing.T) (*tracetest.SpanRecorder, *sdktrace.TracerProvider) {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	return rec, tp
}

func findAttr(kv []attribute.KeyValue, key string) (attribute.Value, bool) {
	for _, a := range kv {
		if string(a.Key) == key {
			return a.Value, true
		}
	}
	return attribute.Value{}, false
}

func TestWrapEmitsSpanOnSuccess(t *testing.T) {
	rec, tp := recorderTracer(t)
	inner := &stubDecider{decision: markup.Decision{
		Rule:          "enterprise",
		MarkupFactor:  1.15,
		ModelVersion:  "v1",
		EngineAdapter: "*inmemory.Engine",
	}}
	wrapped := mkotel.Wrap(inner, tp.Tracer("test"))

	_, err := wrapped.Decide(context.Background(), markup.Request{})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}

	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("len(spans) = %d, want 1", len(spans))
	}
	got := spans[0]
	if got.Name() != "markup.decider.decide" {
		t.Errorf("span name = %q, want \"markup.decider.decide\"", got.Name())
	}
	if got.Status().Code != codes.Unset && got.Status().Code != codes.Ok {
		t.Errorf("span status = %v, want OK/Unset", got.Status().Code)
	}
	attrs := got.Attributes()
	if v, ok := findAttr(attrs, mkotel.AttrAdapter); !ok || v.AsString() != "*inmemory.Engine" {
		t.Errorf("%s = %v ok=%v, want \"*inmemory.Engine\"", mkotel.AttrAdapter, v.AsString(), ok)
	}
	if v, ok := findAttr(attrs, mkotel.AttrModelVersion); !ok || v.AsString() != "v1" {
		t.Errorf("%s = %v ok=%v, want \"v1\"", mkotel.AttrModelVersion, v.AsString(), ok)
	}
	if v, ok := findAttr(attrs, mkotel.AttrRule); !ok || v.AsString() != "enterprise" {
		t.Errorf("%s = %v ok=%v, want \"enterprise\"", mkotel.AttrRule, v.AsString(), ok)
	}
	if v, ok := findAttr(attrs, mkotel.AttrFactor); !ok || v.AsFloat64() != 1.15 {
		t.Errorf("%s = %v ok=%v, want 1.15", mkotel.AttrFactor, v.AsFloat64(), ok)
	}
}

func TestWrapErrNoMatchSetsAttributeNotErrorStatus(t *testing.T) {
	rec, tp := recorderTracer(t)
	inner := &stubDecider{err: markup.ErrNoMatch}
	wrapped := mkotel.Wrap(inner, tp.Tracer("test"))

	_, err := wrapped.Decide(context.Background(), markup.Request{})
	if !errors.Is(err, markup.ErrNoMatch) {
		t.Fatalf("want ErrNoMatch, got %v", err)
	}

	spans := rec.Ended()
	got := spans[0]
	if got.Status().Code == codes.Error {
		t.Errorf("ErrNoMatch must NOT set codes.Error (got %v)", got.Status().Code)
	}
	if v, ok := findAttr(got.Attributes(), mkotel.AttrNoMatch); !ok || !v.AsBool() {
		t.Errorf("%s missing or false on no-match span", mkotel.AttrNoMatch)
	}
}

func TestWrapCanceledSetsCanceledAttributesNotErrorStatus(t *testing.T) {
	rec, tp := recorderTracer(t)
	inner := &stubDecider{err: context.Canceled}
	wrapped := mkotel.Wrap(inner, tp.Tracer("test"))

	_, err := wrapped.Decide(context.Background(), markup.Request{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}

	got := rec.Ended()[0]
	if got.Status().Code == codes.Error {
		t.Errorf("canceled must NOT set codes.Error (got %v)", got.Status().Code)
	}
	if v, ok := findAttr(got.Attributes(), mkotel.AttrCanceled); !ok || !v.AsBool() {
		t.Errorf("%s missing or false", mkotel.AttrCanceled)
	}
	if v, ok := findAttr(got.Attributes(), mkotel.AttrCancelReason); !ok || v.AsString() != "canceled" {
		t.Errorf("%s = %v, want \"canceled\"", mkotel.AttrCancelReason, v.AsString())
	}
}

func TestWrapDeadlineExceededSetsDeadlineReason(t *testing.T) {
	rec, tp := recorderTracer(t)
	inner := &stubDecider{err: context.DeadlineExceeded}
	wrapped := mkotel.Wrap(inner, tp.Tracer("test"))

	_, _ = wrapped.Decide(context.Background(), markup.Request{})

	got := rec.Ended()[0]
	if v, ok := findAttr(got.Attributes(), mkotel.AttrCanceled); !ok || !v.AsBool() {
		t.Errorf("%s missing or false", mkotel.AttrCanceled)
	}
	if v, ok := findAttr(got.Attributes(), mkotel.AttrCancelReason); !ok || v.AsString() != "deadline_exceeded" {
		t.Errorf("%s = %v, want \"deadline_exceeded\"", mkotel.AttrCancelReason, v.AsString())
	}
}

func TestWrapOtherErrorSetsErrorStatusAndRecordsError(t *testing.T) {
	rec, tp := recorderTracer(t)
	sentinel := errors.New("engine boom")
	inner := &stubDecider{err: sentinel}
	wrapped := mkotel.Wrap(inner, tp.Tracer("test"))

	_, err := wrapped.Decide(context.Background(), markup.Request{})
	if !errors.Is(err, sentinel) {
		t.Fatalf("want sentinel, got %v", err)
	}

	got := rec.Ended()[0]
	if got.Status().Code != codes.Error {
		t.Errorf("status = %v, want codes.Error", got.Status().Code)
	}
	if !strings.Contains(got.Status().Description, "engine boom") {
		t.Errorf("status description %q should mention the error", got.Status().Description)
	}
	if len(got.Events()) == 0 {
		t.Error("expected RecordError to produce an event")
	}
}

func TestWrapCorrelationIDFromContextAppearsOnSpan(t *testing.T) {
	rec, tp := recorderTracer(t)
	inner := &stubDecider{decision: markup.Decision{Rule: "x", MarkupFactor: 1.0, ModelVersion: "v1", EngineAdapter: "*x.Engine"}}
	wrapped := mkotel.Wrap(inner, tp.Tracer("test"))

	ctx := breengine.WithCorrelationID(context.Background(), "trace-abc-1")
	_, _ = wrapped.Decide(ctx, markup.Request{})

	got := rec.Ended()[0]
	v, ok := findAttr(got.Attributes(), mkotel.AttrCorrelationID)
	if !ok || v.AsString() != "trace-abc-1" {
		t.Errorf("%s = %v ok=%v, want \"trace-abc-1\"", mkotel.AttrCorrelationID, v.AsString(), ok)
	}
}

func TestWrapNoCorrelationIDAttrWhenAbsent(t *testing.T) {
	rec, tp := recorderTracer(t)
	inner := &stubDecider{decision: markup.Decision{Rule: "x", MarkupFactor: 1.0, ModelVersion: "v1", EngineAdapter: "*x.Engine"}}
	wrapped := mkotel.Wrap(inner, tp.Tracer("test"))

	_, _ = wrapped.Decide(context.Background(), markup.Request{})

	got := rec.Ended()[0]
	if _, ok := findAttr(got.Attributes(), mkotel.AttrCorrelationID); ok {
		t.Errorf("%s should be absent when ctx carries no correlation ID", mkotel.AttrCorrelationID)
	}
}

func TestWithEnvStampsMarkupEnvAttribute(t *testing.T) {
	rec, tp := recorderTracer(t)
	inner := &stubDecider{decision: markup.Decision{Rule: "x", MarkupFactor: 1.0, ModelVersion: "v1", EngineAdapter: "*x.Engine"}}
	wrapped := mkotel.Wrap(inner, tp.Tracer("test"), mkotel.WithEnv("production"))

	_, _ = wrapped.Decide(context.Background(), markup.Request{})

	got := rec.Ended()[0]
	v, ok := findAttr(got.Attributes(), mkotel.AttrEnv)
	if !ok || v.AsString() != "production" {
		t.Errorf("%s = %v ok=%v, want \"production\"", mkotel.AttrEnv, v.AsString(), ok)
	}
}

func TestWithEnvOmittedWhenEmpty(t *testing.T) {
	rec, tp := recorderTracer(t)
	inner := &stubDecider{decision: markup.Decision{Rule: "x", MarkupFactor: 1.0, ModelVersion: "v1", EngineAdapter: "*x.Engine"}}
	wrapped := mkotel.Wrap(inner, tp.Tracer("test"))

	_, _ = wrapped.Decide(context.Background(), markup.Request{})

	got := rec.Ended()[0]
	if _, ok := findAttr(got.Attributes(), mkotel.AttrEnv); ok {
		t.Errorf("%s should be absent when WithEnv is not set", mkotel.AttrEnv)
	}
}

func TestWithSpanNameOverridesDefault(t *testing.T) {
	rec, tp := recorderTracer(t)
	inner := &stubDecider{decision: markup.Decision{Rule: "x", MarkupFactor: 1.0, ModelVersion: "v1", EngineAdapter: "*x.Engine"}}
	wrapped := mkotel.Wrap(inner, tp.Tracer("test"), mkotel.WithSpanName("custom.span"))

	_, _ = wrapped.Decide(context.Background(), markup.Request{})

	got := rec.Ended()[0]
	if got.Name() != "custom.span" {
		t.Errorf("span name = %q, want \"custom.span\"", got.Name())
	}
}

func TestWrapPreservesInnerReturnedValues(t *testing.T) {
	_, tp := recorderTracer(t)
	want := markup.Decision{Rule: "r", MarkupFactor: 2.5, ModelVersion: "vN", EngineAdapter: "*x.Engine"}
	inner := &stubDecider{decision: want}
	wrapped := mkotel.Wrap(inner, tp.Tracer("test"))

	got, err := wrapped.Decide(context.Background(), markup.Request{})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if got != want {
		t.Errorf("Decision lost in wrap: got %+v, want %+v", got, want)
	}
}

// Spec-level guard: a Decide that returns within a very short window
// should still produce a span observable via the recorder (no async
// drop). Confirms the SimpleSpanProcessor-style behaviour of the
// recorder is wired correctly.
func TestWrapSpanIsObservableSynchronously(t *testing.T) {
	rec, tp := recorderTracer(t)
	inner := &stubDecider{decision: markup.Decision{Rule: "x", MarkupFactor: 1.0, ModelVersion: "v1", EngineAdapter: "*x.Engine"}}
	wrapped := mkotel.Wrap(inner, tp.Tracer("test"))

	start := time.Now()
	_, _ = wrapped.Decide(context.Background(), markup.Request{})
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Errorf("Decide took %v; tracer not synchronous?", elapsed)
	}
	if len(rec.Ended()) != 1 {
		t.Errorf("recorder saw %d spans, want 1", len(rec.Ended()))
	}
}


func TestWrap_DefaultSpanKindInternal_OverrideToServer(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	defer func() { _ = tp.Shutdown(context.Background()) }()
	tracer := tp.Tracer("test")

	defaultWrap := mkotel.Wrap(&stubDecider{decision: markup.Decision{Rule: "r"}}, tracer)
	serverWrap := mkotel.Wrap(&stubDecider{decision: markup.Decision{Rule: "r"}}, tracer, mkotel.WithSpanKind(oteltrace.SpanKindServer))

	if _, err := defaultWrap.Decide(context.Background(), markup.Request{}); err != nil {
		t.Fatalf("default wrap Decide: %v", err)
	}
	if _, err := serverWrap.Decide(context.Background(), markup.Request{}); err != nil {
		t.Fatalf("server wrap Decide: %v", err)
	}

	spans := rec.Ended()
	if len(spans) != 2 {
		t.Fatalf("len(spans) = %d, want 2", len(spans))
	}
	if got := spans[0].SpanKind(); got != oteltrace.SpanKindInternal {
		t.Errorf("default span kind = %v, want Internal", got)
	}
	if got := spans[1].SpanKind(); got != oteltrace.SpanKindServer {
		t.Errorf("WithSpanKind(Server) span kind = %v, want Server", got)
	}
}
