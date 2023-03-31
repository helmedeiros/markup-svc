# 20. SpanKind=Server on the outer `markup.decider.decide` span

## Status

Accepted — `internal/observability/otel/otel.go` gains a `WithSpanKind(trace.SpanKind)` option on `Wrap`. The cmd applies `WithSpanKind(trace.SpanKindServer)` to the outermost wrap so the public `markup.decider.decide` span advertises itself as a service-entry point. The inner `markup.guardrails.check` and `markup.engine.evaluate` wraps stay at the default SpanKindInternal because they are NOT service boundaries — they are intra-process decorator layers. The change unlocks Jaeger's Service Performance Monitoring (Monitor tab) for the markup-svc service: Jaeger's SPM-storage handler filters spans to `SpanKind=SERVER OR CONSUMER` by default, so the previous all-Internal span tree showed empty in the Monitor view even when traffic was flowing.

## Context

pricing-observability/ADR-0005 stood up Jaeger's Monitor tab via the OTel Collector's spanmetrics connector — RED metrics aggregated from trace data, queried by Jaeger via Prometheus. The validation showed decision-gateway and traffic-gen correctly populated (both emit spans with the right SpanKinds: gateway has `gateway.request` as SERVER + `gateway.proxy.upstream` as CLIENT, traffic-gen has `traffic.request` as CLIENT). markup-svc showed empty.

Diagnosis: the outer `markup.decider.decide` span was created with the OTel SDK default `SpanKind=Internal` because the existing Wrap function never set one. Internal spans are intra-process and Jaeger's SPM aggregator correctly skips them — they don't represent a service hop. But for markup-svc, the outermost `Decide` call IS the public service entry point: it's the boundary the gateway hits via HTTP. Marking it Internal hid markup-svc from the Monitor tab; the spans were correct as plain trace data but mis-categorized for SPM purposes.

The fix is to set SpanKind on a per-wrap basis (Wrap is reused for three layers in markup-svc; only the outermost is a service entry). One design question.

### How to expose the SpanKind on Wrap

Two options:

- **Add an `Option` similar to the existing `WithSpanName`**: callers opt in via `Wrap(d, t, mkotel.WithSpanKind(trace.SpanKindServer))`. Pros: keeps the function signature stable; matches the existing options pattern; the default (Internal) preserves all existing call sites' behavior; the test surface is small (one option helper, one extra assertion).
- **Change Wrap's signature to take a SpanKind explicitly**: every caller must pass a kind. Pros: forces every wrap site to think about it. Cons: breaks every existing call (router wiring + guardrails wiring + tests in 5+ files); makes the common case verbose.

**Pick the Option.** Matches the existing pattern (`WithSpanName` is already there); preserves backwards compatibility; the change-set is small (one new option helper, one cmd line, one new test).

## Decision

`internal/observability/otel/otel.go`:

- `tracedDecider` struct gains a `spanKind trace.SpanKind` field.
- The constructor initialises `spanKind: trace.SpanKindInternal` so the default matches the SDK default (and matches every existing call site's emitted SpanKind).
- New `WithSpanKind(kind trace.SpanKind) Option` setter.
- `Decide` passes `trace.WithSpanKind(t.spanKind)` to `tracer.Start`.

`cmd/markup-server/main.go`: the outermost wrap calls `mkotel.Wrap(decideDecider, tracer, mkotel.WithSpanKind(trace.SpanKindServer))`. The inner two wraps (engine.evaluate, guardrails.check) keep their existing `WithSpanName(...)` only — they stay Internal.

Test: `otel_test.go` gains `TestWrap_DefaultSpanKindInternal_OverrideToServer` asserting both branches of the option.

## Consequences

### Closed by this ADR

- Jaeger Monitor tab (http://localhost:16686/monitor) renders RED panels for `markup-svc` once spans flow from a markup-svc:v0.1.9 binary. Combined with the decision-gateway + traffic-gen Monitor coverage from pricing-observability/ADR-0005, all three platform services have working SPM.
- The OTel semantic-conventions compliance improves: a `decision-service` exposing HTTP /decide IS a Server-kind span by the conventions. Plain trace data was always correct; the SpanKind label was just under-specified.
- Operators querying spanmetrics via Prometheus directly (without going through Jaeger) get correct service-level aggregations: `sum by(service_name) (rate(traces_spanmetrics_calls_total{span_kind="SPAN_KIND_SERVER"}[1m]))` now includes markup-svc.

### NOT closed by this ADR

- The `markup.guardrails.check` and `markup.engine.evaluate` inner spans stay Internal. That's correct — they are intra-process decorators, not service hops. An operator looking at SPM sees the SERVICE-level number, not the per-decorator slice. Per-decorator slices are still available via PromQL or Jaeger's tag filter (`span_name=markup.engine.evaluate`).
- The incoming W3C trace context (ADR-0017) makes the outermost span a CHILD of the gateway's `gateway.proxy.upstream` span. The parent-child relationship is correct + unchanged by this ADR; only the SpanKind on the child is updated.
- Routes with SpanKind=Consumer (e.g., a future queue-driven Decide path). Not relevant today; would land as a separate Wrap option override when a real consumer ships.

### Performance impact

The `trace.WithSpanKind(...)` SpanStartOption adds one parameter to the existing `tracer.Start` call. Cost: zero allocations, ~5 ns per Decide. Below any noise floor.

### Validation strategy

- Unit test `TestWrap_DefaultSpanKindInternal_OverrideToServer` asserts default = Internal and `WithSpanKind(Server)` = Server.
- Live smoke against the platform stack: after upgrading markup-svc to v0.1.9, Jaeger Monitor tab → service: markup-svc → RED panels populate with the same calls/p95 shape decision-gateway already shows.
- Cross-check: `curl 'http://localhost:9090/api/v1/series?match[]=traces_spanmetrics_calls_total{service_name="markup-svc",span_kind="SPAN_KIND_SERVER"}'` returns at least one series (the markup.decider.decide series at Server kind).
