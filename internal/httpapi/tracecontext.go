package httpapi

import (
	"net/http"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
)

// WithTraceContext is HTTP middleware that extracts the W3C trace
// context from the incoming request headers and writes it onto the
// request context. Downstream handlers + the Decider's OTel span
// (per ADR-0009) become children of the upstream caller's span (the
// gateway in the platform compose) instead of starting a fresh trace.
//
// When no trace context is in the headers, the propagator returns the
// context unchanged and the Decide handler emits a root span as
// before. So this middleware is safe to mount unconditionally —
// when --otel-enabled is off the global TextMapPropagator is the SDK
// default no-op, the Extract call is cheap, and nothing changes.
//
// Composition: place INSIDE WithCorrelationID so the correlation ID
// is in the context when the span starts and OUTSIDE the rest of the
// mux so /admin/* + /healthz / readyz routes also inherit any
// incoming trace context — operators tracing an admin flow get the
// same span chain as a /decide call. See ADR-0017.
func WithTraceContext(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
