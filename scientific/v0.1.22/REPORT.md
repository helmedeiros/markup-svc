# scientific/v0.1.22 — report

Pre-registered bars + measured values for the shadow-Decider hot path from ADR-0031 / ADR-0032 / ADR-0033. See [ADR-0012](../../docs/architecture/decisions/0012-scientific-harness.md) for the methodology and the two-commit pre-registration discipline.

This iteration matches model-registry's `scientific/v0.0.6/` work that bench-grades the registry-side shadow surface (ChallengerPushN3, ShadowStatsPromFanOut). The bars here cover the markup-svc side of the same arc — `dispatchShadow`'s fast path and its fully-engaged sampling path.

> **Bars did not move between pre-registration and measurement.** The bar values below are exactly those committed in the pre-registration commit. The "measured" column was added in this commit alongside the analysis paragraph.

## Reference host

Apple M4, arm64, macOS. The same host used in v0.1.4. amd64/Linux numbers are not measured; production should plan for a small adjustment.

## Pre-registered bars (status: measured)

| Benchmark | Bar | Layer | Why this bound |
|-----------|-----|-------|----------------|
| `BenchmarkShadowFastPathUnloaded` | p99 ≤ 1 ms / op | Whole `/decide` handler when shadow is wired but no challenger is loaded | The bench drives the full handler through ServeHTTP — JSON decode, champion Decide, response encode, plus the dispatchShadow fast path. Run-to-run variance is dominated by httptest scaffold + GC pressure, not the dispatchShadow path itself; the 1 ms bar reflects the noisy ceiling and catches only catastrophic regressions. A microbench targeting dispatchShadow in isolation is parked as a follow-up. |
| `BenchmarkShadowDispatchSampleRateOne` | p99 ≤ 1 ms / op | Whole `/decide` handler when sampling fires and goroutine spawns | Same handler envelope as above plus the goroutine spawn and detached-context allocation. Same noisy ceiling as the fast-path bar; the dispatchShadow-specific delta is observable in the p50 (sub-microsecond) but not in the p99 (dominated by scaffold variance). |

The fast-path bar is the production-common case: `--shadow-admin` enabled but no challenger loaded today. The dispatch bar is the fully-engaged shadow path.

## Method

- Bench files carry `//go:build bench` so they do not execute during `make test`.
- Run with `go test -tags=bench -run NONE -bench <name> -benchmem -benchtime=1000x ./internal/httpapi/`. The 1000x iteration count keeps the p99 (10th-highest of 1000) stable; the 100x runs showed ~3× run-to-run variance on the tail because p99 of 100 samples is just the second-highest.
- Each bench reports allocs/op via `b.ReportAllocs()`.
- Percentile-based bars use `b.ReportMetric` for p50/p95/p99/p999 plus a `b.Errorf` guard at the bar so a regression fails the bench instead of silently passing.

## Measured numbers (Apple M4, three-run medians)

Three consecutive runs of:

```
go test -tags=bench -count=3 -run NONE -bench BenchmarkShadow -benchmem -benchtime=1000x ./internal/httpapi/
```

| Statistic | `BenchmarkShadowFastPathUnloaded` | `BenchmarkShadowDispatchSampleRateOne` |
|-----------|-----------------------------------|----------------------------------------|
| p50 | 541 ns | 1,625 ns |
| p99 | 2,416 ns (~2.4 µs) | 9,625 ns (~9.6 µs) |
| ns/op (mean) | 836 ns | 3,289 ns |
| allocs / op | 13 | 37 |
| heap / op | 1.99 KB | 8.35 KB |
| Bar | ≤ 1 ms p99 | ≤ 1 ms p99 |
| Margin | ~414× under bar | ~104× under bar |

The `b.Errorf` gates did not fire in any of the three runs.

### Where the cost lives

Both benches measure the whole `/decide` handler envelope. The two interesting numbers are the p50 (which is mostly the handler scaffold + the dispatchShadow path) and the allocation delta between the two benches.

**Fast path → dispatch path delta:**
- p50: 1,625 ns − 541 ns = ~1,080 ns added by reaching the dispatched goroutine path
- allocs/op: 37 − 13 = 24 extra allocations (the goroutine + detached context + RecordSampled label set)
- heap/op: 8.35 KB − 1.99 KB = 6.36 KB additional per call

The ~1 µs handler-side delta is consistent with ADR-0032's analytic estimate ("goroutine spawn ~1-3 µs"). The 24-alloc / 6.36 KB delta corresponds to the goroutine stack + detached context + label allocation per ADR-0033's Negative-Consequences accounting (which estimated ~96 KB/sec at 2000 QPS = ~48 bytes/req; the bench shows ~6 KB which is the full per-call envelope, not just the detached-context delta — the difference is goroutine stack which ADR-0033 acknowledged but did not quantify).

### Comparison to ADR claims

| ADR claim | Source | Measured |
|-----------|--------|----------|
| "goroutine spawn ~1-3 µs" (ADR-0032 Negative) | revised analytic | ~1 µs handler-side delta confirms the lower end; the upper end was pessimistic |
| "fast path pays one nil check + one RLock/RUnlock pair" (ADR-0031 Status / ADR-0032 Status) | analytic | 541 ns p50 for the whole handler — the fast-path-specific cost is a small fraction of this; claim stands |
| "1 ms is too generous and could be tightened" (parked) | follow-up | Microbench targeting dispatchShadow alone is parked; this bench cannot grade tighter |

## What these bars prove and what they do not

These bars prove the in-process cost of the shadow dispatch surface stays within an explicit ceiling. They do NOT prove:

- The cost under sustained concurrent /decide load (the v0.0.5 model-registry harness covers HTTP handler concurrency; markup-svc's parallel iteration is parked).
- The challenger Decide cost itself — that lives in the existing engine benches and is unchanged by this iteration.
- The cross-system end-to-end path. The pricing-observability verify-registry-observability.sh script extension (commit `007fe9b`) covers the live shadow lifecycle.
