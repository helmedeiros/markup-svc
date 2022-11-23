# 9. OpenTelemetry Spans at the Decider Port

## Status

Accepted — `internal/observability/otel.Wrap` ships in the same release window. Unit tests using OTel's `tracetest.SpanRecorder` cover every row of the per-outcome table by asserting both the span status and the attribute set; `TestWrapErrNoMatchSetsAttributeNotErrorStatus` and `TestWrapCanceledSetsCanceledAttributesNotErrorStatus` pin the domain-outcome and cancellation framings respectively (no `codes.Error` on either path). The `markup.Decider`-port decorator pattern composes with `swap.Decider` cleanly: tests confirm Decisions round-trip through the wrapper unchanged.

## Context

Three Accepted ADRs (0004 / 0005 / 0006) ship the four adapters; the `(rules × adapter)` matrix is provable at the Decider port (TestE2EFourAdapterMatrixOverHTTP). What is not yet possible is observing live traffic. Operators want:

- **Per-request traces** that show "which Decider served this request, how long it took, what rule fired."
- **Slice by adapter / model / experiment** for dashboards and incident review.
- **Distinguished cancellation** so a caller-imposed deadline does not look like a server-side failure.
- **No-match visibility** as a separate signal from errors — `ErrNoMatch` is a domain outcome (no rule matched the request), not a server failure.

bre-go ships `observability/otel.Wrap(engine.Engine, trace.Tracer)` at v0.17.0. That decorator wraps the bre-go engine port and emits spans around `Execute`. Adopting it requires either (a) exposing the inner `*breinmemory.Engine` from each markup-side Decider so the wrapper can compose with the bre-go layer, or (b) writing a markup-domain decorator at the `markup.Decider` port. Three design questions decide between them.

### 1. Wrap at the bre-go engine layer or the markup.Decider port?

The bre-go layer wraps `engine.Engine.Execute`. Span attributes there are engine-domain: matched rule names, matched count, engine type via `%T`. Useful, but they leak engine vocabulary into observability. Operators reading dashboards want markup terms (`MarkupFactor`, `ModelVersion`) which never appear at the bre-go layer.

The markup.Decider port wraps `Decide`. Span attributes can include the full `Decision`: `Rule`, `MarkupFactor`, `ModelVersion`, `EngineAdapter`, plus the correlation ID. The wrapper depends only on the `markup.Decider` port (Dependency Inversion); no constructor changes to the four adapter packages are needed.

**Decision**: wrap at the `markup.Decider` port. The bre-go decorator stays available for callers that build engines directly, but markup-svc instruments at its own domain layer.

### 2. How does `ErrNoMatch` map onto the span?

Three candidates:

- Treat `ErrNoMatch` as a span error (`span.RecordError` + `codes.Error`). Inflates error-rate dashboards with normal traffic.
- Drop the span on miss. Loses visibility into rule sets that never match — a serious operational signal.
- Set a boolean attribute `rule.markup.no_match=true` on the span, leave the span status at `OK`. Dashboards filter on the attribute.

The third matches the bre-go-level handling of cancellation (a boolean attribute, not an error status) and follows the same logic: the outcome is intended, not a failure.

### 3. Cancellation semantics

`context.Canceled` and `context.DeadlineExceeded` are not engine failures; they are caller intent. bre-go's v0.17.0 wrapper sets `rule.engine.canceled=true` + `rule.engine.cancel.reason=<reason>` instead of `codes.Error` so error-rate dashboards do not inflate when a client disconnects. Match that pattern: `rule.markup.canceled=true` + `rule.markup.cancel.reason=<reason>`. Other errors (parse panic, internal engine error) get `RecordError` + `codes.Error`.

## Decision

`internal/observability/otel` ships:

```go
// Standard markup-domain attribute keys. The "rule.markup.*" prefix
// keeps these attributes distinct from bre-go's "rule.engine.*"
// attributes for callers that stack both decorators.
const (
    AttrAdapter       = "rule.markup.adapter"
    AttrModelVersion  = "rule.markup.model_version"
    AttrRule          = "rule.markup.rule"
    AttrFactor        = "rule.markup.factor"
    AttrCorrelationID = "rule.markup.correlation_id"
    AttrNoMatch       = "rule.markup.no_match"
    AttrCanceled      = "rule.markup.canceled"
    AttrCancelReason  = "rule.markup.cancel.reason"
)

// Wrap returns inner decorated with one OpenTelemetry span per Decide.
// The returned value satisfies markup.Decider so it composes with the
// swap.Decider holder and any other Decider decorators.
func Wrap(inner markup.Decider, tracer trace.Tracer, opts ...Option) markup.Decider

type Option func(*tracedDecider)
func WithSpanName(name string) Option // default "markup.decider.decide"
```

Per-`Decide` behaviour:

| Outcome | Status | Attributes set |
|---|---|---|
| Success | `OK` (default) | `AttrAdapter`, `AttrModelVersion`, `AttrRule`, `AttrFactor`, `AttrCorrelationID` (if present in ctx) |
| `ErrNoMatch` | `OK` (default) | `AttrNoMatch=true`, `AttrCorrelationID` (if present in ctx) |
| `context.Canceled` | `OK` (default) | `AttrCanceled=true`, `AttrCancelReason="canceled"`, `AttrCorrelationID` (if present in ctx) |
| `context.DeadlineExceeded` | `OK` (default) | `AttrCanceled=true`, `AttrCancelReason="deadline_exceeded"`, `AttrCorrelationID` (if present in ctx) |
| Any other error | `codes.Error` + `RecordError(err)` | `AttrCorrelationID` (if present in ctx) |

`AttrCorrelationID` always tries to read from the context via `engine.CorrelationIDFromContext` (the existing identity carrier from ADR-0003).

`cmd/markup-server` wires the wrapper via a new `--otel-enabled` flag (defaulting off so the toolchain stays clean for callers who do not need traces). When set, `wireHandler` composes `otel.Wrap` around the loaded Decider before handing it to the `swap.Decider` holder so the swap stays the outermost decorator.

## Consequences

### Closed by this ADR

- One span per `Decide` is emitted when the wrapper is mounted.
- Attributes are markup-domain (`rule.markup.*`) and dashboard-ready for the `(adapter × model × experiment)` slice.
- `ErrNoMatch` is observable as a separate signal from errors; dashboards see no-match rate distinctly from server-error rate.
- Cancellation is observable as caller-driven, not server-driven.
- The wrapper depends only on the `markup.Decider` port — no changes to the four adapter packages.

### NOT closed by this ADR

- Metrics. Tracked separately under its own ADR.
- Exporter configuration (stdout vs Jaeger vs OTLP). The `--otel-enabled` flag selects a stdout exporter for v0.0.4; production-grade OTLP/HTTP wiring is its own ADR.
- Sampling policy. Default tracer sampling applies; per-route or per-tenant sampling is out of scope.
- Span linking from the HTTP layer (parent span from incoming W3C `traceparent`). Currently the wrapper starts a root span; HTTP-layer span linking is its own ADR if a real consumer needs it.
- Bre-go-level engine spans. Callers wanting both layers can compose, but markup-svc's default is markup-domain spans only.

### Performance impact

Per-`Decide` overhead from the wrapper:

- `tracer.Start(ctx, name)` runs in every call. With the no-op tracer (`trace/noop`), the returned span is a shared singleton and only the per-call context node allocates; cost is one `context.WithValue` ~ tens of nanoseconds.
- The wrapper makes a single `SetAttributes(...)` call per outcome with 2-5 `KeyValue` arguments (one slice allocation per Decide for the variadic args).
- `engine.CorrelationIDFromContext` is one `context.Value` lookup with a type assertion, ~10 ns.
- With a real exporter (stdout, OTLP, Jaeger), the in-process work the wrapper does is unchanged. Exporter pipelines route through an asynchronous batch span processor by default; the wrapper itself does not block on export.

Aggregate per-`Decide` overhead with the no-op tracer: ~100 ns, dominated by the context node allocation. With a real exporter, the wrapper's in-process cost is unchanged from the no-op case; export latency lives in the batch processor's goroutine, off the request path.

When ctx is already canceled at entry, `tracer.Start` still runs, the inner `Decide` returns immediately with `context.Canceled`, and the wrapper sets only the cancellation attributes (`AttrCanceled` + `AttrCancelReason` + `AttrCorrelationID`). Domain attributes are not computed because the Decision is zero-valued; the span still appears in the trace with the cancellation reason.

### Validation strategy

- `internal/observability/otel`: unit tests using `go.opentelemetry.io/otel/sdk/trace/tracetest` (in-memory exporter). Cover every row of the per-Decide outcome table — happy path attributes, no-match attribute, canceled (both reasons), error span with `codes.Error`. Plus a `WithSpanName` test to pin the option's effect.
- The tracetest exporter exposes recorded spans synchronously so assertions on attribute values are deterministic.
- A `cmd/markup-server` smoke test confirms the `--otel-enabled` flag composes the wrapper into `wireHandler` without breaking the existing `/decide` and `/admin/reload` routes.
