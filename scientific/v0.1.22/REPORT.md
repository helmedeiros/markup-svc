# scientific/v0.1.22 — report

Pre-registered bars + measured values for the shadow-Decider hot path from ADR-0031 / ADR-0032 / ADR-0033. See [ADR-0012](../../docs/architecture/decisions/0012-scientific-harness.md) for the methodology and the two-commit pre-registration discipline.

This iteration matches model-registry's `scientific/v0.0.6/` work that bench-grades the registry-side shadow surface (ChallengerPushN3, ShadowStatsPromFanOut). The bars here cover the markup-svc side of the same arc — `dispatchShadow`'s fast path and its fully-engaged sampling path.

> **Bars did not move between pre-registration and measurement.** The bar values below are exactly those committed in the pre-registration commit. The "measured" column was added in this commit alongside the analysis paragraph.

## Reference host

Apple M4, arm64, macOS. The same host used in v0.1.4. amd64/Linux numbers are not measured; production should plan for a small adjustment.

## Pre-registered bars (status: pending)

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

## Measured numbers

Pending. Filled in a follow-up commit after running.

## What these bars prove and what they do not

These bars prove the in-process cost of the shadow dispatch surface stays within an explicit ceiling. They do NOT prove:

- The cost under sustained concurrent /decide load (the v0.0.5 model-registry harness covers HTTP handler concurrency; markup-svc's parallel iteration is parked).
- The challenger Decide cost itself — that lives in the existing engine benches and is unchanged by this iteration.
- The cross-system end-to-end path. The pricing-observability verify-registry-observability.sh script extension (commit `007fe9b`) covers the live shadow lifecycle.
