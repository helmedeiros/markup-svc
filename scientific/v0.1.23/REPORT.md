# scientific/v0.1.23 — report

Pre-registered bars + measured values for the ADR-0036 decision-event substrate hot path (`decisionsink.Sink.Publish`). See [ADR-0012](../../docs/architecture/decisions/0012-scientific-harness.md) for the methodology and the two-commit pre-registration discipline.

This iteration closes the parked debt from ADR-0035 (`markup.decision.v1` emission) and ADR-0036 (substrate adapter). Both ADRs deferred their per-Decide bar pre-registration to a follow-on commit; this is that commit on the substrate side.

> **Bars did not move between pre-registration and measurement.** The bar values below are exactly those committed in the pre-registration commit. The "measured" column is added in the measurement commit alongside the analysis paragraph.

## Reference host

Apple M4, arm64, macOS. The same host used in v0.1.4, v0.1.19, v0.1.22. amd64/Linux numbers are not measured here; production should plan for a small adjustment, more aggressive on the Noop path (the channel + atomic-op fast-paths are nearly identical across architectures, so the deltas should be small).

## Pre-registered bars (status: pre-registered)

| Benchmark | Bar | Layer | Why this bound |
|-----------|-----|-------|----------------|
| `BenchmarkSinkPublishNoop` | p99 ≤ 200 ns / op, 0 allocs/op | `decisionsink.NoopSink.Publish` — the default-wired floor | The load-bearing claim from ADR-0036: when no substrate is wired, `WithAccessLog` captures `sinkEnabled=false` at construction so this Publish never runs. The bar measures the floor: if `WithAccessLog` accidentally dispatched to NoopSink anyway, what would the per-call cost be? It should be a single empty method call dispatched through an interface — first-principles cost is ~25-50 ns on M4. The 200 ns ceiling absorbs timer-instrumentation noise; the 0-allocs floor is the structural claim ADR-0036 makes. A regression here means an Event field gained a heap-pointer default or a future change introduced an indirect boxing. |
| `BenchmarkSinkPublishS3SinkEnqueue` | p99 ≤ 5 µs / op, 0 allocs/op | `s3sink.Sink.Publish` happy path (room in the queue) | The substrate-wired hot path. The implementation is `select { case s.queue <- e: default: drop }` — one non-blocking channel send. Channel sends to a buffered chan with room are documented at ~30-50 ns on modern Go. Per-iteration `time.Now()` / `time.Since` instrumentation on macOS surfaces a fat-tail noise floor at ~2 µs on the p99; the 5 µs ceiling absorbs that without losing the regression signal (e.g., the queue becoming locked, the Event growing past one cache line). 0 allocs is the structural claim. |
| `BenchmarkSinkPublishBufferFullDrop` | p99 ≤ 1 µs / op, 0 allocs/op | `s3sink.Sink.Publish` drop path (queue full) | The S3-outage hot path. The drop branch is one failed channel send (`select` default), one atomic Add on `dropped`, one optional `metrics.IncDropped` call, plus the rate-limited log gate which short-circuits via a CAS check. The first call after a quiet window emits a log; subsequent calls within the 5s window are CAS-short-circuited. 0 allocs is the structural claim across both branches; the log emission allocates ~1 entry per 5s window which would not statistically appear in p99. |

The three bars together cover ADR-0036's two-mode operational shape: queue-room-available (the production-common case) and queue-saturated (the S3-outage case). The Noop floor is the regression backstop for `WithAccessLog`'s sinkEnabled gate.

## Method

- Bench files carry `//go:build bench` so they do not execute during `make test`. Same convention as the v0.1.22 shadow benches.
- Run with `go test -tags=bench -run NONE -bench <name> -benchmem -benchtime=1000x ./internal/observability/decisionsink/s3sink/`. The 1000x iteration count keeps the p99 (10th-highest of 1000) stable; smaller counts (100x) show ~3× variance on the tail because p99 of 100 samples is just the second-highest.
- Each bench calls `b.ReportAllocs()` and reports `p50-ns/op` + `p99-ns/op` via `b.ReportMetric`.
- Percentile-based bars use `b.ReportMetric` for p50/p99 plus a `b.Errorf` guard at the bar so a regression fails the bench instead of silently passing.

## Measured numbers (Apple M4, three-run medians)

_To be filled in by the measurement commit._

## Analysis

_To be filled in by the measurement commit._
