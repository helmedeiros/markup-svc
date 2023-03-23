# 17. Incoming W3C trace context + multi-layer Decide spans

## Status

Accepted — `Bootstrap` (per ADR-0016) sets the global TextMapPropagator to W3C TraceContext + Baggage so markup-svc becomes a trace-context CONSUMER: when the caller (decision-gateway in the platform compose) emits a `traceparent` header, the `markup.decider.decide` span becomes a child of the caller's span and the whole pipeline renders as a single trace in Jaeger UI. The cmd wiring grows two new span layers around the existing `markup.decider.decide` span: `markup.engine.evaluate` wraps the engine adapter (inmemory / firstmatch / priority / indexed) and `markup.guardrails.check` wraps the guardrails decorator. Operators looking at a `/decide` trace see three (or four, including the gateway parent) nested spans whose durations break the cost down by component. `httpapi.WithTraceContext` is the new HTTP middleware that runs the propagator's Extract on every request.

## Context

ADR-0009 shipped the OTel span decorator at the Decider port: one span per `Decide` call named `markup.decider.decide`, with `rule.markup.*` attributes. ADR-0016 shipped the SDK bootstrap so the spans actually exported. That release made markup-svc the first platform service producing real traces in Jaeger. decision-gateway then shipped its own gateway-side spans (decision-gateway ADR-0002) plus W3C traceparent propagation. With the gateway side done, markup-svc was the bottleneck for end-to-end trace correlation: the gateway's traces and the markup-svc traces lived in two separate trace IDs even though they were the same request, because markup-svc never extracted the incoming W3C trace context.

That gap alone is reason enough for this ADR. The operator question "is the gateway proxy slow or is the engine slow" is unanswerable when the two halves are in separate trace IDs. Stitching them is the propagator's job, and the Decide handler is the seam where Extract has to land.

The second gap is per-component cost visibility inside markup-svc. The chain of Decider decorators (router → guardrails → engine, or any subset depending on flags) means the `markup.decider.decide` span's duration is the SUM of guardrails + engine + decorator overhead. When the operator asks "is guardrails what's making my p99 spike" the existing trace has no answer — only one span exists for the whole stack.

Three design questions.

### 1. Where to Extract incoming trace context: per-handler vs middleware

The Extract call goes somewhere. Two reasonable places:

- **Inside the Decide handler**: `ctx := otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))` before `d.Decide(ctx, ...)`. Pros: scope is limited to the routes that actually emit spans (`/decide`); admin routes stay untraced. Cons: the propagator becomes a concern of the HTTP handler package, which historically did not import OTel; and any future traced endpoint has to remember to Extract.
- **As an HTTP middleware**: `WithTraceContext` wraps the mux and runs Extract on every request. Pros: every endpoint inherits incoming trace context, including future admin spans (guardrails admin, reload admin) and the health probes (whose span on the parent's trace would clutter Jaeger and is not what we want). Cons: the global Extract runs on every request, even `/healthz` (cost: ~50ns when no header is present, ~100ns when one is).

**Pick middleware.** The 50ns floor is negligible and the consistency benefit is real: every traced caller's parent context propagates to every markup-svc endpoint by construction, not by every-handler-remembering-to-Extract. For `/healthz` + `/readyz` with no incoming traceparent the middleware is a no-op (Extract returns the context unchanged). When a Kubernetes kubelet probe HAS a parent traceparent — none do today, but a future service-mesh sidecar might — the probe span lands on the right trace.

### 2. Layer the spans how: one decorator vs three

The simplest model: keep `markup.decider.decide` as the only span. Don't add inner spans. Operator's "where is the cost" question answered via attribute filtering on the existing span (filter by `rule.markup.adapter=inmemory` to slice by engine type).

The richer model: three nested spans matching the layered Decider stack — outer `markup.decider.decide` (the public boundary), middle `markup.guardrails.check` (guardrails decorator), inner `markup.engine.evaluate` (the engine adapter). Each span's duration directly answers a cost question: outer = total, outer - middle = decorator overhead, middle - inner = guardrails cost, inner = pure engine work.

The cost of the rich model: three span open + close cycles per Decide instead of one. Per ADR-0009 each span is ~50-100 ns; aggregate ~200-300 ns extra per request. The batched exporter handles the volume increase transparently; the 3x span count shows up as the `processors: []` queue in the OTel Collector occasionally hitting its default 2048 buffer at >2000 RPS (still fine; the export is async).

**Pick the rich model.** The operator-experience win is the entire point of this ADR. 300 ns per request is below the noise floor of the engine work (typical inmemory eval is 20-100 µs); the new attribute set on each layer is identical (the existing `mkotel.Wrap` carries them through unchanged); operators reading Jaeger UI see a clean waterfall instead of one flat span.

A consequence of the layered model: when guardrails are inactive (the binary started without any guardrails flags), the middle span is skipped — only `markup.decider.decide` + `markup.engine.evaluate` emit. The wiring keeps the layer conditional so a guardrails-free deployment stays at two spans per request and the operator does not see a confusing empty `guardrails.check` span.

### 3. Span emission inside packages vs at the wiring layer

Two options to emit the inner spans:

- **Inside each package**: `guardrails.New` and each engine adapter (inmemory, firstmatch, priority, indexed) take a `tracer trace.Tracer` parameter and emit their span internally. Pros: tighter coupling between span lifetime and the work it covers; package authors control the span attributes.
- **At the wiring layer**: `cmd/markup-server/main.go` wraps each layer with `mkotel.Wrap(WithSpanName(...))` from the outside. The existing `otel.Wrap` already takes a tracer + span name option; reusing it for the inner spans is a one-line change per layer.

**Pick wiring layer.** Three reasons: (1) it reuses the existing `mkotel.Wrap` machinery — no new code in guardrails / engine packages — so the inner spans automatically get the same attribute set (`rule.markup.adapter`, `rule.markup.rule`, `rule.markup.factor`, etc.) the outer span gets; (2) it keeps the engine and guardrails packages library-only, no OTel import in them, which preserves the option to use them from a non-OTel binary; (3) it makes the conditional emission obvious (the wiring function reads top-to-bottom showing exactly when each span is added).

The cost is attribute duplication: each layer emits the same `rule.markup.*` attribute set (since `mkotel.Wrap` reads them from the same Decision). This is genuinely wasteful — three SetAttributes calls per request instead of one. The cost is ~150 ns total. The operator-readable Jaeger UI shows the same attributes at each layer which is mildly redundant but not confusing — each row shows the same rule + factor, with different durations. A future optimization (`mkotel.WrapMinimal` for inner spans, no attribute extraction) is a one-file change when the cost actually matters.

## Decision

`internal/observability/otel/bootstrap.go` is extended: after `otel.SetTracerProvider(tp)`, it also calls `otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))`. The composite is the standard combination (W3C TraceContext for the trace + parent IDs, Baggage for arbitrary cross-service key/value pairs); markup-svc does not produce baggage but accepts it transparently for any caller that sets it.

`internal/httpapi/tracecontext.go` is the new file: `func WithTraceContext(next http.Handler) http.Handler` is HTTP middleware that runs `otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))` and writes the result onto `r.Context()`. When the global propagator is the SDK no-op (i.e. `--otel-enabled` is off), Extract returns the context unchanged at a cost of ~50 ns.

`cmd/markup-server/main.go` composition order changes from `WithCorrelationID(mux)` to `WithCorrelationID(WithTraceContext(mux))`. The correlation-ID frame stays outermost so the correlation ID is in context when the trace span starts.

The Decider wiring in both `wireTracedHandler` (single-decider mode) and `wireRouterHandler` (multi-route mode) is restructured:

```go
var decideDecider markup.Decider = holder  // or router

// Engine-layer span (innermost). Always added when tracer is set
// because the engine adapter is always in the chain.
if tracer != nil {
    decideDecider = mkotel.Wrap(decideDecider, tracer,
        mkotel.WithSpanName("markup.engine.evaluate"))
}

// Guardrails-layer span (middle). Only when guardrails are active;
// emitting an empty guardrails span on a no-guardrails binary would
// confuse operators.
if gw.wrap != nil {
    decideDecider = gw.wrap(decideDecider)
    if tracer != nil {
        decideDecider = mkotel.Wrap(decideDecider, tracer,
            mkotel.WithSpanName("markup.guardrails.check"))
    }
}

// Public-boundary span (outermost). The existing markup.decider.decide
// span. Attributes match the original ADR-0009 contract so dashboards
// keyed on this span stay green.
if tracer != nil {
    decideDecider = mkotel.Wrap(decideDecider, tracer)
}
```

The `--otel-enabled` flag's help string updates to name the three layers (markup.decider.decide / markup.guardrails.check / markup.engine.evaluate) and the W3C trace context behavior so the operator running `--help` sees the full picture.

## Consequences

### Closed by this ADR

- markup-svc joins the gateway's trace as a child — Jaeger UI shows one trace per request with the gateway span as the root, the gateway.proxy.upstream span as its child, and the markup.decider.decide / markup.guardrails.check / markup.engine.evaluate spans as deeper children. Trace IDs are preserved end-to-end.
- The operator's "where is the cost" question is now answerable at the component level: gateway proxy vs decider vs guardrails vs engine, each as its own span duration. The waterfall in Jaeger UI is the diagnostic.
- The cookbook recipe in pricing-observability ADR-0002 — "open Jaeger UI, search service markup-svc, see one span per request" — gains the parent-trace + per-layer-cost view automatically. The recipe text does not need updating; the visual just gets richer.

### NOT closed by this ADR

- traffic-gen does not emit spans yet. When the user fires `curl /decide` directly the trace root is the gateway span (W3C trace context starts there because curl does not set traceparent). traffic-gen's outbound root span ships in traffic-gen v0.0.3 (separate per-repo ADR); the gateway already accepts incoming traceparent so the change there is in traffic-gen alone.
- Log-trace correlation. The JSON access log lines do not yet include `trace_id` + `span_id`. Phase 4 of the cross-service tracing rollout writes these into the `attrs` so a Kibana operator filtering by `correlation_id` can jump to Jaeger via a link. Lands as a follow-up ADR in either this repo OR pricing-observability — the cleaner place is here (the access-log writer is what needs the trace context) but the bigger work is the Kibana index pattern + URL template.
- Inner-most engine package spans. The `markup.engine.evaluate` span as wired here wraps the holder (which contains the engine), so the duration includes the holder's atomic pointer load (~10 ns) plus the engine work. For deployments at billions of RPS where 10 ns matters, a deeper-level span emitted from inside the engine package would isolate the engine cost cleanly. No consumer has asked; the holder overhead is below the engine noise floor today.
- A `mkotel.WrapMinimal` variant that emits the span name without re-extracting attributes from the Decision. The current wiring emits three SetAttributes calls per request (~150 ns redundant cost). Lands when an operator's perf instrumentation flags it.

### Performance impact

- `--otel-enabled` not set: unchanged. The propagator stays the SDK no-op; the `WithTraceContext` middleware calls Extract on the no-op (no allocation, ~50 ns); no inner spans are wrapped. Per-request delta vs the pre-ADR binary: ~50 ns.
- `--otel-enabled` set, no guardrails: per request adds 2 span open + close pairs (~150 ns) + 2 SetAttributes calls (~100 ns) + the propagator Extract (~100 ns when traceparent is present). Aggregate ~350 ns per Decide. Below the engine's noise floor (typical inmemory eval is 20-100 µs).
- `--otel-enabled` set, guardrails active: per request adds 3 spans (~225 ns) + 3 SetAttributes (~150 ns) + Extract (~100 ns). Aggregate ~475 ns. Still well under 1µs; the operator-experience gain (per-layer waterfall) is the win.

### Validation strategy

- Unit tests in `internal/httpapi/tracecontext_test.go`: incoming `traceparent` header is extracted onto request context so downstream handlers see the propagated trace ID (assertion via `oteltrace.SpanContextFromContext`); no incoming header leaves the context valid (no-op semantics).
- The E2E tests in `cmd/markup-server/main_otel_test.go` are updated to assert the new 2-span-per-Decide shape (no-guardrails path) — the test catches regressions where the layer split breaks the outer span's attribute contract or the inner span's name.
- Smoke against the live platform stack: `curl -H "X-Correlation-ID: trace-N" $GATEWAY/decide` → Jaeger UI search for any service → trace shows 5 spans: `gateway.request` → `gateway.proxy.upstream` → `markup.decider.decide` → (`markup.guardrails.check` when guardrails are on) → `markup.engine.evaluate`. The waterfall durations sum to the gateway.request duration as a sanity check.
