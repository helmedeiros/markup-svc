package httpapi

import (
	"net/http"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	breengine "github.com/helmedeiros/bre-go/engine"

	mkotel "github.com/helmedeiros/markup-svc/internal/observability/otel"
)

// WithAdminSpan wraps next with one OTel SERVER span per request.
// The trace-context middleware (WithTraceContext) must run before
// this so the span attaches as a child of the upstream gateway span.
// tracer == nil is a no-op so the wrapper is safe to mount when
// --otel-enabled is off. See ADR-0028.
func WithAdminSpan(tracer trace.Tracer, spanName string, next http.Handler) http.Handler {
	if tracer == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, span := tracer.Start(r.Context(), spanName,
			trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(
				attribute.String("http.method", r.Method),
				attribute.String("http.target", r.URL.Path),
			),
		)
		defer span.End()
		if cid := breengine.CorrelationIDFromContext(ctx); cid != "" {
			span.SetAttributes(attribute.String(mkotel.AttrCorrelationID, cid))
		}
		sw := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r.WithContext(ctx))
		span.SetAttributes(attribute.Int("http.status_code", sw.status))
		if sw.status >= 500 {
			span.SetStatus(codes.Error, http.StatusText(sw.status))
		}
	})
}
