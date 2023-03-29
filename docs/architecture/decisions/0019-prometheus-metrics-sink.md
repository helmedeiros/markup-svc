# 19. Prometheus Sink + `/metrics` endpoint

## Status

Accepted — `internal/observability/metrics/prom` ships a `prom.Sink` adapter implementing the `metrics.Sink` port from ADR-0010 plus a private `prometheus.Registry` + `promhttp.Handler` so the binary exposes Prometheus exposition at `/metrics` without polluting the global registry. The cmd grows a `--metrics-enabled` flag that wraps the Decider with the metrics decorator (the existing `metrics.Wrap`) outermost so `Duration` captures end-to-end Decider cost, and mounts the handler on the same listener as `/decide`. Pricing-observability's metrics phase (its own ADR-0003) stands up a Prometheus container that scrapes this endpoint.

## Context

ADR-0010 shipped the metrics decorator + `Sink` port: a typed `DecisionMetric` event, mutually-exclusive outcome fields (NoMatch / Canceled / Err), and a `Wrap(inner, sink)` decorator that emits one event per Decide. The package was library-only by design — production operators were expected to write the Prometheus/OTel-metrics binding in a wrapper main.

Two things forced the wrapper-main pattern to give: same gap as ADR-0016 closed for OTel SDK bootstrap. (1) The published `markup-svc:vN` image is the canonical platform image in the docker-compose stack — operators following the cookbook do not derive their own binary. (2) pricing-observability's metrics phase (the deferred-from-v0.0.1 work in that repo) needs a `/metrics` endpoint to scrape; without it the Prometheus container has nothing to point at.

The traces work (ADR-0017) gave operators "what happened in this request." Metrics give "what's been happening over time" — RPS, error rate, latency percentiles, alerting thresholds. The two complement: traces are episodic; metrics are time-series. An alert fires on a metric (e.g., `markup_decide_total{outcome="error"}` rate > 5/min); the operator clicks through to traces from the same window to investigate.

Three design questions.

### 1. Bundle Sink + handler vs return them separately

The Prometheus client_golang library lets you build a `prometheus.Registry`, register collectors, and construct a `promhttp.Handler` against it. The Sink needs the counter + histogram collectors; the handler needs the registry. Two API shapes:

- **Two-step constructor**: `prom.NewSink()` returns the Sink + registry; operator passes the registry to `promhttp.HandlerFor(...)` themselves. Pros: explicit, lets the operator add other collectors to the registry. Cons: every operator has to call `promhttp.HandlerFor` correctly; easy to attach to the default-global registry by mistake and leak process collectors.
- **One-step constructor**: `prom.New()` returns the Sink + a ready-to-mount `http.Handler`. Pros: one call, no operator-side wiring; the handler is bound to a private registry so multiple Sinks in the same process do not cross-contaminate (matters for tests; not relevant for production but keeps the API consistent). Cons: operators who want to extend the registry (add process collectors, custom metrics) need a different shape — they'd have to either build their own Sink or use a future `prom.NewWithRegistry(reg)` variant.

**Pick one-step.** The 95% case is "wire the Sink + mount the handler"; the 5% case (operator extending the registry) is documented as a follow-up `NewWithRegistry` option that lands when an operator asks. Today, two callers exist (the cmd binary + the test); both want the one-step shape.

### 2. Labels: which DecisionMetric fields become Prometheus labels

Prometheus label cardinality is a hard constraint — every unique label-value combination is a new time-series. The DecisionMetric has these fields available:

- `Adapter` (4-5 values: inmemory, firstmatch, priority, indexed, router) — low cardinality, useful for filtering.
- `ModelVersion` (a handful: v1, v2, etc.) — low cardinality, useful for A/B comparison.
- `Rule` (N values, where N = total rules across model versions) — potentially hundreds or thousands; UNBOUNDED in principle as operators add rules over time.
- `CorrelationID` (UUID v4, ~unbounded) — explicitly per-request.
- `MarkupFactor` (continuous float) — never.
- `outcome` (synthesized: ok / no_match / canceled / deadline / error) — 5 values, low cardinality.

**Labels = `adapter`, `model_version`, `outcome`.** Three low-cardinality dimensions; total series for the canonical platform = 4 adapters × ~3 model versions × 5 outcomes = ~60 series per metric × 2 metrics = ~120 series. Far below Prometheus's recommended ~1000-series-per-target ceiling.

Rule + CorrelationID + MarkupFactor are NOT labels. Operators wanting per-rule analysis use Jaeger's `rule.markup.rule` tag filter (the OTel span carries it per ADR-0009); the per-request `correlation_id` is in the gateway access log + Jaeger span; the markup factor is on the Decision in the response body.

### 3. Histogram buckets: stdlib default vs custom for sub-millisecond work

`prometheus.DefBuckets` is `{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}` seconds — designed for general-purpose HTTP service latency in the 5ms–10s range. markup-svc's per-Decide work is 10-100µs on the inmemory engine; under the default buckets, every Decide lands in the 0-5ms bucket, the histogram is mostly informative at the lower end only.

A more useful set for markup-svc would be `{50, 100, 200, 500, 1000, 2000, 5000, 10000} µs`. But that means custom buckets that diverge from the rest of the Prometheus ecosystem; operator dashboards that key on `prom_http_request_duration_seconds_bucket{le="0.005"}` (Prometheus's own metric naming convention) would have to learn a separate set for markup.

**Pick `prometheus.DefBuckets`.** The histogram is less informative than it could be (most samples fall in the first bucket), but the operator experience is consistent with the rest of Prometheus-instrumented services in the fleet. A `markup_decide_duration_seconds_bucket` series with custom finer-grained buckets lands as a follow-up ADR when an operator's dashboard need motivates it — explicitly tagged as v0.x ADR (the cost is bucket-set divergence within markup-svc itself).

## Decision

`internal/observability/metrics/prom/prom.go`:

- `prom.New() (*Sink, http.Handler)`: constructs a private `prometheus.Registry`, registers a `prometheus.CounterVec` named `markup_decide_total` + a `prometheus.HistogramVec` named `markup_decide_duration_seconds` (both with labels `adapter` / `model_version` / `outcome`, buckets `prometheus.DefBuckets`), and returns the Sink + a `promhttp.HandlerFor(reg, ...)` against the same registry.
- `Sink.RecordDecision(m metrics.DecisionMetric)` implements the existing `metrics.Sink` port. Synthesizes the `outcome` label from the DecisionMetric's mutually-exclusive NoMatch / Canceled / Err fields (`ok` / `no_match` / `canceled` / `deadline` / `error`). Increments the counter + observes the duration with the same label set.

`cmd/markup-server/main.go`:

- New `--metrics-enabled` flag. When set, `prom.New()` builds the Sink + handler; both are bundled into a new private struct `metricsWiring` that gets passed into both `wireRouterHandler` and `wireTracedHandler` (matching the existing `guardrailsWire` pattern).
- The wire functions wrap the Decider with `metrics.Wrap(inner, sink)` AS THE OUTERMOST decorator (matching ADR-0010's recommended order — duration covers the full stack including tracing) and mount the handler at `/metrics` on the same mux as `/decide`.
- When `--metrics-enabled` is not set, the wire functions skip the wrap + don't mount `/metrics`; the binary's behavior is unchanged.

`go.mod` gains `github.com/prometheus/client_golang v1.14.0` (the version compatible with Go 1.18).

`docker-compose.yaml` in decision-gateway (a follow-up commit, not in this release) adds `--metrics-enabled` to the markup-svc service. Pricing-observability's metrics-phase ADR-0003 + v0.0.3 release in that repo stands up the Prometheus container that scrapes this endpoint.

## Consequences

### Closed by this ADR

- Operators following the cookbook get a working `/metrics` endpoint on the canonical image — no wrapper-main required. The Prometheus container in pricing-observability v0.0.3 scrapes this endpoint and the metrics phase from ADR-0001 of that repo is live.
- Per-outcome counters answer "is my error rate spiking" + "how many requests am I serving" + "what fraction are no-match" at sub-second resolution. The histogram answers "what's my p99 Decide latency" at the same resolution.
- Metrics + traces complement: an alert on `rate(markup_decide_total{outcome="error"}[5m]) > 0.1` fires; operator clicks into Jaeger for the same window, filters by `rule.markup.adapter=inmemory`, finds the spans.

### NOT closed by this ADR

- Custom histogram buckets (the 50µs–10ms range that would be more informative for markup-svc's sub-ms Decide latency). Lands as a follow-up ADR when an operator's dashboard motivates it.
- Process / Go runtime metrics (goroutine count, heap size, GC stats). The private registry intentionally does NOT include them so the markup_decide_* metrics stay focused. Operators wanting Go runtime metrics stack a second `promhttp` handler at a different path (e.g., `/metrics/process`) with the default registry, or use a single multi-registry handler — both are operator-side wiring decisions.
- A `prom.NewWithRegistry(reg *prometheus.Registry)` variant for operators wanting to share a registry across the markup metrics + their own custom metrics. Ships when an operator asks; the menu stays small.
- HTTP-server metrics (requests-per-second by path, response-size histograms). These are gateway-level concerns; tracked separately for decision-gateway.
- Metrics for the markup_decide spans' inner layers (engine.evaluate / guardrails.check). The current scope is the outermost Decider; per-layer histograms would require multiple Sinks at different decorator positions, which adds wiring complexity. Lands as a v0.2+ follow-up.

### Performance impact

- The metrics decorator (existing per ADR-0010): one `time.Now()` + one `time.Since()` per Decide (~50 ns), one allocation for the `DecisionMetric` struct (~64 bytes), one `sink.RecordDecision` call.
- The Prometheus Sink's `RecordDecision`: one `prometheus.Labels{}` map allocation (~3 entries, ~80 bytes), one counter `Inc()` (atomic add, ~20 ns), one histogram `Observe()` (atomic update of the matching bucket counter, ~30 ns).
- Aggregate: ~50-200 ns per Decide when `--metrics-enabled` is set. Below the engine work (10-100 µs on inmemory).
- The `/metrics` HTTP handler runs only on scrape — Prometheus default scrape interval is 15s. At ~120 series, the response body is ~5 KB; serialization cost is ~100 µs on a Decide-handling goroutine ⇒ negligible at any scrape interval.

When `--metrics-enabled` is not set: zero ns delta vs the pre-ADR binary. The decorator is not wrapped; the `/metrics` route is not mounted.

### Validation strategy

- Unit tests in `prom_test.go`: exercise the Sink directly (record three outcome types, scrape `/metrics`, assert the counter + histogram lines contain the expected labels + counts); exercise the Sink behind the existing `metrics.Wrap` decorator with a stub Decider (assert the outcome label = "ok" or "no_match" depending on the stub's response).
- The `cmd/markup-server` ci-local stays green — the wire functions take the new `metricsWiring` parameter, tests pass the zero value.
- Integration smoke against the live stack: bring up the canonical compose + pricing-observability stacks with `--metrics-enabled` set; `curl http://markup-svc:8080/metrics` returns the exposition format; pricing-observability's Prometheus container scrapes successfully; Grafana dashboard renders the rate + duration time-series.
