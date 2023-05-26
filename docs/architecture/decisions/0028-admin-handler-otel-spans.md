# 28. OTel spans on `/admin/*` handlers

## Status

Accepted — `internal/httpapi.WithAdminSpan(tracer, spanName, next)` is a new HTTP middleware that opens one `SpanKindServer` span per admin request. cmd/markup-server wraps every admin handler with it: `/admin/reload` → `markup.admin.reload`, `/admin/diagnose` → `markup.admin.diagnose`, `/admin/guardrails` → `markup.admin.guardrails`. tracer == nil is a no-op so the wrapper is safe to mount when `--otel-enabled` is off. The middleware chains under `WithTraceContext` (already in place per ADR-0017) so the new span attaches as a child of the upstream gateway's span; admin operations now show end-to-end in Jaeger waterfalls alongside /decide.

## Context

A trace-context audit performed during pricing-observability/ADR-0018 work found that admin POSTs from traffic-gen propagate correctly through the platform (traffic-gen → decision-gateway), but the **markup-svc service is missing from admin trace waterfalls**. Concretely:

| Path | Services in trace | Spans |
|---|---|---|
| /decide | traffic-gen, decision-gateway, markup-svc | 5 (traffic.request, gateway.request, gateway.proxy.upstream, markup.decider.decide, markup.engine.evaluate) |
| /admin/reload | traffic-gen, decision-gateway | 3 (traffic.request, gateway.request, gateway.proxy.upstream) |

The trace ends at `gateway.proxy.upstream` because markup-svc's admin handlers (`Reload`, `Diagnose`, `GuardrailsAdmin`) don't open OTel spans. ADR-0009 instrumented the **Decider port** (`markup.decider.decide`); the admin handlers aren't Deciders, so they slipped past that wrap. The result is a real operational gap: a 3 am `AdminHotReloadRejected` page leaves the on-call with an upstream-only trace ending at the gateway — no markup-svc-side data on what the diagnose gate found, how long the loader took, or what correlation_id the request carried.

Two design options.

### 1. Span at the markup.Decider port (extend ADR-0009)

Refactor the admin handlers to delegate through a Decider-like interface so the existing `mkotel.Wrap` covers them.

Pros: one wrap pattern for the whole service.
Cons: forces an artificial port shape — admin handlers do file IO, config-state mutation, and validation, not "Decide(req) Decision". Squeezing them into the Decider port loses meaning. The OTel attribute table for Decide (Rule, Factor, ModelVersion) doesn't apply.

### 2. New HTTP-level middleware: `WithAdminSpan(tracer, spanName, next)`

A small middleware in `internal/httpapi` that opens a SERVER span around the wrapped handler. Span name is operator-readable (`markup.admin.reload`); attributes are HTTP-semantic (`http.method`, `http.target`, `http.status_code`) plus `rule.markup.correlation_id` reused from `internal/observability/otel` so Kibana filters on the same key.

Pros: HTTP-level concerns stay in httpapi (where `WithTraceContext`, `WithCorrelationID`, `WithAccessLog` already live); reuses the `AttrCorrelationID` constant so dashboards and runbooks don't fork; nil-tracer no-op preserves the `--otel-enabled=off` path; each admin endpoint gets a distinct span name without conflating outcomes.
Cons: a second wrap pattern in the service (port-level Wrap for /decide, http-level WithAdminSpan for /admin/*). Two patterns is the cost of clean layering — the alternative was forcing admin handlers through the Decider port.

**Pick option 2.** Layering wins; the two-pattern cost is honest.

### Span status semantics

5xx → span Status = Error. 4xx → span Status = OK (per ADR-0027 the 4xx path is caller-side, not server fault, so the span correctly stays non-error and the existing tail-sampling `errors` policy doesn't fire on every rejected reload). 2xx → OK by default.

This mirrors the gateway's existing span-error convention (the gateway-side `InstrumentedTransport` marks 5xx as Error and leaves 4xx as OK) so traces show consistent semantics from edge to engine.

## Decision

`internal/httpapi/adminspan.go`:

```go
func WithAdminSpan(tracer trace.Tracer, spanName string, next http.Handler) http.Handler {
    if tracer == nil { return next }
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
```

Reuses the existing `statusRecorder` from `internal/httpapi/accesslog.go`.

cmd/markup-server wires it around each admin handler in both `wireTracedHandler` (single-route mode) and the router-mode equivalent:

```go
mux.Handle("/admin/reload", httpapi.WithAdminSpan(tracer, "markup.admin.reload", reloadH))
if diagnoseFn != nil {
    mux.Handle("/admin/diagnose", httpapi.WithAdminSpan(tracer, "markup.admin.diagnose", httpapi.Diagnose(diagnoseFn)))
}
if gw.mountAdmin != nil {
    gw.mountAdmin(mux, func(h http.Handler) http.Handler {
        return httpapi.WithAdminSpan(tracer, "markup.admin.guardrails", h)
    })
}
```

The `guardrailsWire.mountAdmin` signature changes from `func(*http.ServeMux)` to `func(*http.ServeMux, func(http.Handler) http.Handler)` so the wrapper is applied at mount time. The wrap callback lets the cmd inject `WithAdminSpan` without `buildGuardrailsWiring` needing to know about the tracer.

### Tests

`internal/httpapi/adminspan_test.go`: six unit tests using `tracetest.SpanRecorder`:

1. `TestWithAdminSpan_EmitsServerSpan` — span name + SpanKindServer + http.method + http.status_code on 200.
2. `TestWithAdminSpan_5xxMarksError` — status code 500 sets span Status to Error.
3. `TestWithAdminSpan_4xxStaysOK` — status code 400 keeps span Status as OK (ADR-0027 alignment).
4. `TestWithAdminSpan_StampsCorrelationID` — incoming correlation_id from context lands on the span via `mkotel.AttrCorrelationID`.
5. `TestWithAdminSpan_NilTracerIsNoop` — tracer == nil runs the inner handler without instrumentation.
6. `TestWithAdminSpan_ChildOfUpstreamSpan` — admin span shares the trace_id of the upstream context so propagation chains correctly.

`TestE2EOTelSpansContinueAfterReload` in `cmd/markup-server` updated to expect 5 spans (two /decide × two spans each + one `markup.admin.reload`) instead of 4. The test was the closest existing canary for OTel composition; it now also pins the admin span as part of the expected emit set.

## Consequences

### Closed

- Admin traces show all three services in Jaeger waterfalls. A page from `AdminHotReloadRejected` lets the on-call click the runbook's Jaeger deep-link and see the markup-svc-side span timing + correlation_id + http.status_code, not just the upstream gateway view.
- The pricing-observability ADR-0017 runbook Jaeger deep-links (which point at Jaeger SPM + filtered trace search) now produce meaningful results for admin endpoints because the service-level traces exist.
- The audit gap noted in pricing-observability/ADR-0018 ("admin trace waterfalls end at the gateway") is closed end-to-end.
- The same span shape covers `/admin/reload` (ADR-0008), `/admin/diagnose` (ADR-0025), `/admin/guardrails` (ADR-0015). Future admin endpoints inherit by wrapping with WithAdminSpan.

### Not closed

- The admin span doesn't carry endpoint-specific attributes beyond HTTP semantic ones. E.g., `/admin/reload` could stamp `markup.reload.rule_count` and `markup.reload.model_version` on success; `/admin/diagnose` could stamp `markup.diagnose.error_count`. These need handler-side attribute setting and break the simple middleware-only design. A follow-up ADR can route attributes via a context-attached writer if the data proves operationally useful.
- Span events for noteworthy intermediate steps (parse vs Diagnose vs Swap inside `/admin/reload`). Same shape as above — needs handler cooperation. Out of scope today; Jaeger's single-span timing is sufficient for the dominant "is the admin handler slow?" question.
- Trace-context outbound. Admin handlers don't make outbound calls today; if `/admin/diagnose` evolves to call markup-svc subcomponents over HTTP, those calls would need an instrumented transport like decision-gateway already uses. Not relevant for the current code shape.

### Performance impact

- Per admin request: one span start + end + attribute set. With `--otel-enabled` on, that's ~1–3 µs of overhead per admin call (per OTel SDK's typical span allocation cost; consistent with existing measurements in `internal/observability/otel/otel_test.go` and the scientific harness's decorator overhead measurements). Admin endpoints are operator-triggered, not on the hot path, so this is invisible.
- With `--otel-enabled` off (tracer is nil), `WithAdminSpan` returns `next` directly — zero allocation, no closure created.
- Memory: no new long-lived state. The middleware closes over (tracer, spanName, next) at construction; each request creates one span object that's released after Span.End().
