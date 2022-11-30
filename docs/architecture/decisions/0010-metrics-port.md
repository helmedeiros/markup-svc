# 10. Metrics Port at the Decider Layer

## Status

Accepted — `internal/observability/metrics` ships in the same release window. Unit tests cover every row of the per-outcome table (success / no-match / canceled / deadline / other error), and `TestDecisionMetricFieldSetInvariants` pins the mutual-exclusivity rules end-to-end through the decorator. `TestComposesWithOTelDecorator` confirms `metrics.Wrap(otel.Wrap(stub))` produces both a recorded metric AND a recorded span on every Decide, validating the composition order recommended for cmd integration. `TestRecordingSinkConcurrentRecordingAndRead` runs many goroutines under the race detector against the test-only `RecordingSink`.

## Context

ADR-0009 ships per-request OpenTelemetry spans. Spans answer "what did *this* request do" — granular, expensive, sampled. They are the wrong tool for the question "what is my error rate, my p99 latency, my no-match rate, sliced by (adapter, model, rule)" — that question wants aggregate counters and histograms with cheap-by-design per-request cost.

bre-go already ships the pattern at v0.18.0: `observability/metrics.Wrap(engine.Engine, ExecutionMetricSink) engine.Engine`. The port lives in `observability/execution_metric.go` with a typed `ExecutionMetric` event and a one-method `ExecutionMetricSink` interface; backends adapt to the contract. That is hexagonal architecture — the engine owns the port, the deployment owns the adapter.

markup-svc has the same shape question one level up. The Decider port runs at higher granularity than the engine (the Decision carries markup-domain fields the engine knows nothing about — `MarkupFactor`, `ModelVersion`, the named `Rule`). A metrics event emitted at the engine layer would have no access to those. So markup-svc defines its own metrics port at its own port level.

Three design questions.

### 1. Push (sink callback) or pull (collector polling)?

Two well-understood patterns:

- **Push**: the decorator builds a typed event after each `Decide` and hands it to `sink.RecordDecision(event)`. The sink aggregates (counter increments, histogram updates) into whatever shape the backend wants. Prometheus client_golang's `prometheus.CounterVec` works this way (`.WithLabelValues(...).Inc()`). OTel's `metric.Int64Counter.Add(...)` works this way. Custom in-memory aggregators work this way. The decorator has zero opinion about how the aggregation looks.
- **Pull**: the decorator maintains internal counters; a `Collect()` method exposes them in a backend-specific format. The decorator becomes the aggregation type, with one implementation per backend. This is what `prometheus.Registry.Register(collector)` does directly.

Push wins for this port because:

- It composes with multiple backends without changing the decorator.
- It matches the bre-go pattern operators already know.
- The sink is the single place to put backend-specific knowledge (histogram bucket choices, label name conventions, OTel resource attributes).
- The decorator stays a trivial "fan out one event per call" — easy to test, easy to compose with the OTel decorator.

### 2. Markup-domain metric event or reuse bre-go's `ExecutionMetric`?

bre-go's `ExecutionMetric` carries: `Adapter`, `MatchedCount`, `MatchedNames`, `Duration`, `Err`, `Canceled`, `CancelReason`. Useful, but missing the fields markup operators slice on:

- `ModelVersion` — operators rolling between two models need a per-model breakdown.
- `Rule` — single-rule counter (which rule fired N times today) is a debugging staple.
- `MarkupFactor` — histogram of factors served is the canonical "what did our pricing model actually do" dashboard.

Reusing bre-go's event would force every backend to look up these fields elsewhere (via correlation ID to the trace, via separate log scraping). Defining the markup-domain event keeps everything one consumer needs in one place:

```go
type DecisionMetric struct {
    Adapter       string
    ModelVersion  string
    Rule          string  // empty on miss
    MarkupFactor  float64 // 0 on miss
    CorrelationID string
    Duration      time.Duration
    NoMatch       bool   // distinguishes ErrNoMatch from real errors
    Err           error
    Canceled      bool
    CancelReason  string
}
```

`Err`, `NoMatch`, and `(Canceled, CancelReason)` are mutually exclusive — same shape rule as bre-go's event:

| Outcome | NoMatch | Canceled | Err | Rule, Factor |
|---|---|---|---|---|
| Success | false | false | nil | populated from Decision |
| `ErrNoMatch` | true | false | nil | empty / 0 |
| `context.Canceled` | false | true (reason "canceled") | nil | empty / 0 |
| `context.DeadlineExceeded` | false | true (reason "deadline_exceeded") | nil | empty / 0 |
| Other error | false | false | set | empty / 0 |

This mirrors ADR-0009's per-outcome table so the OTel decorator and the metrics decorator classify the same outcomes the same way. Dashboards that slice by outcome see consistent results across both signals.

### 3. How does this compose with the OTel decorator?

Both are markup.Decider → markup.Decider decorators. They stack:

- `metrics.Wrap(otel.Wrap(swap.New(inner)))` — metrics outermost. The metric event's `Duration` captures the time spent inside the tracing decorator + the swap holder + the engine. Operators see end-to-end Decider cost as the metric, including any tracing overhead.
- `otel.Wrap(metrics.Wrap(swap.New(inner)))` — tracing outermost. The span's duration captures the time spent inside the metrics decorator + the swap + the engine. Operators see end-to-end Decider cost as the span, including any sink overhead.

Either order is valid. The recommendation is **metrics outermost** because:

- The metric's `Duration` is the field operators dashboard against; including tracing cost in that number is honest "what did this request *actually* take" data.
- The span sees the engine work, the swap lookup, and the sink push as part of the call — close enough for tracing context.
- Symmetric to the bre-go pattern at the engine layer (`metrics.Wrap(otel.Wrap(...))` is what the bre-go v0.18.0 ADR recommends).

cmd/markup-server's wiring is updated in a follow-up commit; this ADR ships the port + decorator + a test-only sink so the contract is exercised end-to-end without forcing operators onto a specific backend.

## Decision

`internal/observability/metrics` ships:

```go
// DecisionMetric is the typed event emitted per Decide call. See the
// per-outcome table above for the field-set invariants.
type DecisionMetric struct {
    Adapter       string
    ModelVersion  string
    Rule          string
    MarkupFactor  float64
    CorrelationID string
    Duration      time.Duration
    NoMatch       bool
    Err           error
    Canceled      bool
    CancelReason  string
}

// Sink consumes DecisionMetric events. Implementations are the
// adapter half of the hexagonal port: markup-svc owns the contract,
// backends (Prometheus, OTel metrics, custom) adapt to it.
//
// RecordDecision must be safe for concurrent calls.
type Sink interface {
    RecordDecision(DecisionMetric)
}

// Wrap returns inner decorated to emit one DecisionMetric per Decide
// via sink. The returned value satisfies markup.Decider so it
// composes with otel.Wrap and swap.Decider; the recommended order
// is metrics outermost (metrics.Wrap(otel.Wrap(swap.New(inner)))).
func Wrap(inner markup.Decider, sink Sink) markup.Decider

// RecordingSink is a thread-safe Sink that appends every recorded
// metric to an internal slice. Useful for tests and small in-memory
// aggregations; production deployments use a Prometheus or OTel
// metrics sink instead.
type RecordingSink struct{ ... }
func (s *RecordingSink) RecordDecision(m DecisionMetric)
func (s *RecordingSink) Records() []DecisionMetric
func (s *RecordingSink) Reset()
```

## Consequences

### Closed by this ADR

- A markup-domain metrics event exists with the fields operators dashboard against (Adapter, ModelVersion, Rule, MarkupFactor, plus the outcome classification).
- A single-method Sink port lets any backend plug in without changes to the decorator.
- Composition with the OTel decorator is well-defined; both share the same per-outcome classification so dashboards can pair span attributes with metric counters cleanly.
- `RecordingSink` ships as the test-only aggregator so unit tests assert on event payloads without depending on a real backend.

### NOT closed by this ADR

- Prometheus exposition (`/admin/metrics` endpoint, `prometheus.CounterVec` bindings). Tracked separately under its own ADR.
- OTel metrics sink (bridge from `DecisionMetric` to `metric.Int64Counter` / `metric.Float64Histogram`). Tracked separately.
- cmd/markup-server `--metrics-enabled` flag. Lands alongside the first concrete backend ADR — shipping a flag with no concrete sink would just be a `--no-op-decorator-enabled` flag.
- Histogram bucket policy. Belongs to whichever backend adapter ADR commits to a specific exposition.
- Per-rule cardinality controls. A heavily-templated rule set could explode label cardinality through the Rule field. The Sink implementation owns that constraint; the port carries the raw event.

### Performance impact

Per-`Decide` overhead from the decorator:

- One `time.Now()` at entry, one `time.Since(start)` at exit. On Linux/amd64 these are ~50 ns each via vDSO `clock_gettime`; macOS uses a Mach syscall (typically slower) and Windows uses `QueryPerformanceCounter` (variable). The Linux number is the floor.
- One `DecisionMetric` value constructed at the decorator and passed by value to `sink.RecordDecision(DecisionMetric)`. Whether it lives on the stack or the heap depends on escape analysis: if the sink stores the value (e.g., `RecordingSink` appending to a slice; an OTel metrics sink building label keys), the value escapes; if the sink only reads scalar fields and discards (e.g., a counter-only sink that immediately calls `.Inc()` on derived labels), escape analysis can keep the value stack-allocated. The decorator does not force a heap allocation — the sink does.
- One method call to `sink.RecordDecision(event)`. The sink's cost is its own; the decorator does not block on it.
- One `engine.CorrelationIDFromContext` lookup (~10 ns).

Aggregate per-`Decide` overhead independent of sink choice and exclusive of sink work: ~100-150 ns on Linux/amd64 (two clock reads + a context lookup + value construction). Against the engine's microsecond-scale `parser.Condition.Eval`, marginal.

Sink cost is backend-specific. A `prometheus.CounterVec.WithLabelValues(...).Inc()` is sub-microsecond; an OTel metric add is similar. A `RecordingSink` appends to an internal slice — Go's slice growth is amortized doubling, so reallocation cost is `O(1)` amortized per record with occasional larger allocations as capacity doubles. Acceptable for tests; not for production (the slice grows unbounded). The port's promise that `RecordDecision` is concurrent-safe means the sink owns whatever synchronization it needs (`RecordingSink` uses a mutex; an OTel metrics sink uses atomic counters).

### Validation strategy

- `internal/observability/metrics`: unit tests for the decorator using `RecordingSink` as the aggregator. Cover every row of the per-outcome table — success populates `Adapter`/`ModelVersion`/`Rule`/`MarkupFactor`; `ErrNoMatch` sets `NoMatch=true` and leaves `Rule`/`MarkupFactor` zero; `context.Canceled` and `context.DeadlineExceeded` set `Canceled=true` with the right `CancelReason`; other errors set `Err` and leave `Canceled` false. `Duration` is non-zero on every call.
- `RecordingSink`: concurrency test under the race detector — many goroutines call `RecordDecision` while another goroutine calls `Records()` and `Reset()`. No data race, no lost records.
- Composition test: `metrics.Wrap(otel.Wrap(stubDecider))` produces both a recorded metric AND a recorded span on every Decide. Confirms the two decorators stack without interfering.
- `TestDecisionMetricFieldSetInvariants` pins the mutual-exclusivity rules from the outcome table — a metric with `NoMatch=true` AND `Err != nil` is malformed; the decorator never produces one.
