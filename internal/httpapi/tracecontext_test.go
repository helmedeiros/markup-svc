package httpapi_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/helmedeiros/markup-svc/internal/httpapi"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	oteltrace "go.opentelemetry.io/otel/trace"
)

func TestWithTraceContext_ExtractsIncomingTraceparent(t *testing.T) {
	otel.SetTextMapPropagator(propagation.TraceContext{})
	otel.SetTracerProvider(sdktrace.NewTracerProvider())
	defer otel.SetTracerProvider(oteltrace.NewNoopTracerProvider())

	const incoming = "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"

	var seenTraceID string
	captured := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sc := oteltrace.SpanContextFromContext(r.Context())
		seenTraceID = sc.TraceID().String()
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/anything", nil)
	req.Header.Set("traceparent", incoming)
	httpapi.WithTraceContext(captured).ServeHTTP(httptest.NewRecorder(), req)

	if seenTraceID != "0af7651916cd43dd8448eb211c80319c" {
		t.Errorf("trace id seen by inner handler = %q, want propagated parent trace id", seenTraceID)
	}
}

func TestWithTraceContext_NoIncomingHeader_LeavesContextValid(t *testing.T) {
	otel.SetTextMapPropagator(propagation.TraceContext{})

	captured := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Just ensure r.Context() is still a valid context.
		if r.Context() == nil {
			t.Errorf("nil context passed through middleware")
		}
		_ = context.TODO()
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodGet, "/anything", nil)
	httpapi.WithTraceContext(captured).ServeHTTP(httptest.NewRecorder(), req)
}
