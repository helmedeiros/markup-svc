package httpapi_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	oteltrace "go.opentelemetry.io/otel/trace"

	breengine "github.com/helmedeiros/bre-go/engine"

	"github.com/helmedeiros/markup-svc/internal/httpapi"
	mkotel "github.com/helmedeiros/markup-svc/internal/observability/otel"
)

func adminSpanRecorder(t *testing.T) (*tracetest.SpanRecorder, oteltrace.Tracer) {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	return rec, tp.Tracer("test")
}

func findAdminAttr(kv []attribute.KeyValue, key string) (attribute.Value, bool) {
	for _, a := range kv {
		if string(a.Key) == key {
			return a.Value, true
		}
	}
	return attribute.Value{}, false
}

func TestWithAdminSpan_EmitsServerSpan(t *testing.T) {
	rec, tracer := adminSpanRecorder(t)
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := httpapi.WithAdminSpan(tracer, "markup.admin.reload", inner)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/admin/reload", nil))

	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("len(spans)=%d, want 1", len(spans))
	}
	s := spans[0]
	if s.Name() != "markup.admin.reload" {
		t.Errorf("span.Name = %q, want markup.admin.reload", s.Name())
	}
	if s.SpanKind() != oteltrace.SpanKindServer {
		t.Errorf("span.Kind = %v, want SpanKindServer", s.SpanKind())
	}
	if v, ok := findAdminAttr(s.Attributes(), "http.status_code"); !ok || v.AsInt64() != 200 {
		t.Errorf("http.status_code: ok=%v val=%v, want 200", ok, v.AsInt64())
	}
	if v, ok := findAdminAttr(s.Attributes(), "http.method"); !ok || v.AsString() != http.MethodPost {
		t.Errorf("http.method: ok=%v val=%q, want POST", ok, v.AsString())
	}
}

func TestWithAdminSpan_5xxMarksError(t *testing.T) {
	rec, tracer := adminSpanRecorder(t)
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	httpapi.WithAdminSpan(tracer, "markup.admin.reload", inner).ServeHTTP(
		httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/admin/reload", nil))

	s := rec.Ended()[0]
	if s.Status().Code != codes.Error {
		t.Errorf("status.Code = %v, want Error", s.Status().Code)
	}
}

func TestWithAdminSpan_4xxStaysOK(t *testing.T) {
	rec, tracer := adminSpanRecorder(t)
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	})
	httpapi.WithAdminSpan(tracer, "markup.admin.diagnose", inner).ServeHTTP(
		httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/admin/diagnose", nil))

	s := rec.Ended()[0]
	if s.Status().Code == codes.Error {
		t.Errorf("status.Code marked Error on 4xx; client-side errors should not error the span (ADR-0027)")
	}
	if v, ok := findAdminAttr(s.Attributes(), "http.status_code"); !ok || v.AsInt64() != 400 {
		t.Errorf("http.status_code: ok=%v val=%v, want 400", ok, v.AsInt64())
	}
}

func TestWithAdminSpan_StampsCorrelationID(t *testing.T) {
	rec, tracer := adminSpanRecorder(t)
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodPost, "/admin/reload", nil)
	ctx := breengine.WithCorrelationID(req.Context(), "cid-admin-42")
	httpapi.WithAdminSpan(tracer, "markup.admin.reload", inner).ServeHTTP(
		httptest.NewRecorder(), req.WithContext(ctx))

	s := rec.Ended()[0]
	if v, ok := findAdminAttr(s.Attributes(), mkotel.AttrCorrelationID); !ok || v.AsString() != "cid-admin-42" {
		t.Errorf("correlation_id attr: ok=%v val=%q, want cid-admin-42", ok, v.AsString())
	}
}

func TestWithAdminSpan_NilTracerIsNoop(t *testing.T) {
	called := false
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	h := httpapi.WithAdminSpan(nil, "markup.admin.reload", inner)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/admin/reload", nil))
	if !called {
		t.Error("inner handler must run even when tracer is nil")
	}
}

func TestWithAdminSpan_ChildOfUpstreamSpan(t *testing.T) {
	rec, tracer := adminSpanRecorder(t)
	parentCtx, parent := tracer.Start(context.Background(), "upstream")
	defer parent.End()

	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodPost, "/admin/reload", nil)
	httpapi.WithAdminSpan(tracer, "markup.admin.reload", inner).ServeHTTP(
		httptest.NewRecorder(), req.WithContext(parentCtx))

	parent.End()
	spans := rec.Ended()
	var admin, up oteltrace.SpanContext
	for _, s := range spans {
		if s.Name() == "markup.admin.reload" {
			admin = s.SpanContext()
		}
		if s.Name() == "upstream" {
			up = s.SpanContext()
		}
	}
	if admin.TraceID() != up.TraceID() {
		t.Errorf("admin span trace=%s not in upstream trace=%s (propagation broken)", admin.TraceID(), up.TraceID())
	}
}
