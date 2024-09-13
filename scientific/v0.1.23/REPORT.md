# scientific/v0.1.23 — report

Pre-registered bars + measured values for the ADR-0036 decision-event substrate hot path (`decisionsink.Sink.Publish`). See [ADR-0012](../../docs/architecture/decisions/0012-scientific-harness.md) for the methodology and the two-commit pre-registration discipline.

This iteration closes the parked debt from ADR-0035 (`markup.decision.v1` emission) and ADR-0036 (substrate adapter). Both ADRs deferred their per-Decide bar pre-registration to a follow-on commit; this is that commit on the substrate side.

> **Bars did not move between pre-registration and measurement.** The bar values below are exactly those committed in the pre-registration commit. The "measured" column is added in the measurement commit alongside the analysis paragraph.

## Reference host

Apple M4, arm64, macOS. The same host used in v0.1.4, v0.1.19, v0.1.22. amd64/Linux numbers are not measured here; production should plan for a small adjustment, more aggressive on the Noop path (the channel + atomic-op fast-paths are nearly identical across architectures, so the deltas should be small).

## Pre-registered bars (status: measured)

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

Three consecutive runs of:

```
go test -tags=bench -count=3 -run NONE -bench BenchmarkSinkPublish -benchmem -benchtime=1000x ./internal/observability/decisionsink/s3sink/
```

| Statistic | `BenchmarkSinkPublishNoop` | `BenchmarkSinkPublishS3SinkEnqueue` | `BenchmarkSinkPublishBufferFullDrop` |
|-----------|----------------------------|--------------------------------------|---------------------------------------|
| p50 | 0 ns | 41-42 ns | 41 ns |
| p99 | 42 ns | 42 ns | 42 ns |
| Allocs / op | 0 | 0 | 0 |
| Bytes / op | 0 | 0 | 0 |
| Margin against pre-registered p99 bar | 200 / 42 = **4.8×** | 5,000 / 42 = **119×** | 1,000 / 42 = **24×** |

All three p99 numbers cleared the bar with comfortable headroom across three consecutive measurement runs. The 0-allocs structural claim held on every benchmark, every run.

The 42 ns floor across all three benches is the monotonic-clock-read resolution + the per-iteration timer-instrumentation cost on M4 — it pins the noise floor, not the production cost. The underlying primitive operations (interface dispatch, channel send, atomic Add, CAS) are observable as sub-10 ns intrinsically; the iteration timing surfaces them all as 42 ns ceilings.

## Analysis

**Default deployment cost is zero.** `BenchmarkSinkPublishNoop` clears its bar at the timer-resolution floor with 0 allocs/op. The ADR-0036 hot-path discipline ("sinkEnabled gate captured at construction") would still hold even if a future refactor accidentally removed the gate — the Publish call itself is structurally cheap. The bar at 200 ns gives 4.8× headroom over the measured 42 ns; any per-call regression that turned NoopSink into a heap-allocating call would push p99 over 1 µs and the bench would fail loudly.

**Substrate-wired enqueue is at parity with the no-op floor.** `BenchmarkSinkPublishS3SinkEnqueue` measured the same 42 ns p99 as the NoopSink path — the `select` + buffered channel send is structurally as cheap as an empty method call once the timer-instrumentation noise is factored out. This validates the ADR-0036 "non-blocking best-effort" claim concretely: enqueue on a healthy substrate adds nothing measurable to the `/decide` envelope.

**Drop path is also at parity.** `BenchmarkSinkPublishBufferFullDrop` measured 42 ns p99 against a saturated queue. The drop branch is `select default → atomic Add → metric IncDropped → CAS rate-limited log gate`. After the first iteration, the CAS gate short-circuits every subsequent iteration within the 5 s quiet window, so the inner loop runs through the cheapest path: one failed channel send + one atomic + one nil-or-noop metric call. 0 allocs holds because the rate-limited log allocates ~1 entry per 5 s, well off the p99 tail of any 1,000-iteration bench.

**Operational implication.** An operator running `markup-svc --decision-sink=s3 --metrics-enabled` against a healthy MinIO sees the wired path add ~42 ns p99 to the access-log middleware (against the underlying ~500 ns access-log envelope from v0.1.4). An operator running against a saturated MinIO sees the drop path add the same ~42 ns p99 per request, until the rate-limited log fires (once per 5 s onset) and the alert fires (after 5 m sustained). The `/decide` customer envelope stays bounded regardless of substrate health.

**Bench coverage gaps.** Three things are not covered by this iteration's bench bars and are listed under [Not closed in ADR-0036](../../docs/architecture/decisions/0036-decision-event-substrate-minio.md#not-closed-deferred-to-follow-on-adrs):

1. Full `WithAccessLog` envelope when a sink is wired — the bars here measure just `Publish`, not the middleware. A future bench can stand the full middleware against a NoopSink and against an s3sink with a discard channel.
2. End-to-end batch + flush + upload cost — the background goroutine path is not bench-measured. A future bench can drive the goroutine with a fake S3 server and assert payload size + flush latency budgets.
3. Cross-layer scientific harness — driving the full traffic-gen → decision-gateway → markup-svc → MinIO pipeline at multiple rates is a separate effort. The `scientific/platform-v0.1.0/` work is the natural next iteration of the harness story.
