# 24. Sub-millisecond histogram buckets for the Decide histogram

## Status

Accepted — `markup_decide_duration_seconds` uses a custom 14-bucket set covering `50µs–1s` instead of `prometheus.DefBuckets` (`5ms–10s`). Sub-millisecond resolution at the lower end so the Grafana p50/p95/p99 panels distinguish typical Decide latency (10–100 µs) from edge tails (1–10 ms) instead of bucketing everything below 5 ms together.

## Context

ADR-0019 wired the Prometheus Sink and used `prometheus.DefBuckets` to stay consistent with the rest of the ecosystem. After the pool-tuning + multi-arch + trace-instrumentation work, measured p99 sits around 60 µs and p50 around 17 µs — both inside the first default bucket. The Grafana `Decide latency — p50/p95/p99` panel rendered all three lines at ~5 ms, making the histogram informationally dead.

## Decision

`internal/observability/metrics/prom/prom.go` declares:

```go
var decideBuckets = []float64{
    0.00005, 0.0001, 0.00025, 0.0005,
    0.001, 0.0025, 0.005, 0.01,
    0.025, 0.05, 0.1, 0.25, 0.5, 1,
}
```

The histogram's `Buckets` field switches from `prometheus.DefBuckets` to this slice. 14 buckets total — 4 sub-millisecond + 4 in the 1–10 ms range + 6 in the 25 ms–1 s range — keeping cardinality bounded while covering the platform's measured range plus headroom.

## Consequences

### Closed

- Grafana p50/p95/p99 panels render distinct lines tracking the actual Decide curve.
- The `MarkupDecideP99Slow` alert (pricing-observability/ADR-0008, threshold 5 ms) keeps firing at the right point; the bucket at exactly 5 ms is preserved.
- Per-series cardinality stays bounded: ~3 outcomes × ~3 model versions × ~4 adapters × 14 buckets = ~500 series — well under any production Prometheus.

### Not closed

- Native histograms (Prometheus 2.40+). Could replace the bucket-array with `prometheus.NativeHistogramBucketFactor` for exponential auto-scaling. Lands when an operator's tail-sampling investigation needs the resolution at very low cost.
- Per-adapter bucket sets. The `indexed` adapter is faster than `firstmatch` at scale; a future ADR could give them different buckets if the operator finds the shared set lossy for one.
