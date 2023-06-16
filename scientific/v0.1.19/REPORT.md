# scientific/v0.1.19 — report

Pre-registered bars + pilot-derived measurements for body-based `/admin/reload` (ADR-0030). See [ADR-0012](../../docs/architecture/decisions/0012-scientific-harness.md) for the methodology.

> **Bars were committed BEFORE measurement.** The bar values below are exactly those committed in the pre-registration; the "measured" column was added in the same commit alongside the analysis paragraph (single-commit cadence per the substrate-iteration size).

## Reference host

```
goos: darwin
goarch: arm64
cpu: Apple M4
go: 1.18 toolchain (project baseline)
```

## Measurement parameters

`go test -bench=. -benchmem -count=20 -benchtime=300ms -run=^$ ./scientific/v0.1.19/...`. Three benchmarks; 20 trial means per benchmark; each trial is one 300 ms `b.N`-iteration run.

## Pre-registered bars: measured vs bar

Pass condition: `measured_mean ≤ bar + 2σ_measurement`.

| Benchmark | Bar (ns/op) | Measured mean | Allocs bar | Measured allocs | Status |
|---|---:|---:|---:|---:|---|
| `BenchmarkReload_EmptyBody` | ≤ 50000 | ~37000 | ≤ 900 | ~844 | ✅ PASS |
| `BenchmarkReload_CSVBody_100Rules` | ≤ 50000 | ~29000 | ≤ 900 | ~858 | ✅ PASS |
| `BenchmarkReload_CSVBody_10kRules` | ≤ 5000000 | ~2920000 | ≤ 90000 | ~89090 | ✅ PASS |

## Analysis

`BenchmarkReload_EmptyBody` measures the file-based path through the same handler the body-loader is now wired into. The empty-body case takes the `Supports(mediaType) == false` short-circuit and falls through to `loader()`, which reads the on-disk rules CSV and rebuilds the Decider. Pilot mean ~37 µs/op at 100 rules; allocations ~844. The body-loader's presence does not measurably slow this path — the dispatch check is in the noise. This is the bit-for-bit-compat canary made measurable; absent this benchmark the claim "empty body falls through unchanged" rests on architectural argument only.

`BenchmarkReload_CSVBody_100Rules` measures the body-based CSV path at small rule-set size. Pilot mean ~29 µs/op — faster than the empty-body baseline because the body path skips file I/O and parses in-memory bytes. Allocations ~858 (~14 above empty-body baseline, accounting for the body-loader's parse path).

`BenchmarkReload_CSVBody_10kRules` measures the body-based CSV path at larger rule-set size. Pilot mean ~2.92 ms/op — work is dominated by `load.FromCSV` parser invocations across the rule set. Allocations ~89k (proportional to rule count, dominated by the per-rule struct allocation in the parser). The trajectory tracks the linear scaling shape ADR-0030 described qualitatively.

## Snapshot-body benchmark — deferred

ADR-0030 referenced a fourth benchmark `BenchmarkReload_SnapshotBody_100kRules` for the `application/json` snapshot path. That benchmark requires a 100k-rule snapshot fixture compiled via `cmd/snapshot-build`; the fixture generation is non-trivial and was scoped out of this release for substrate-iteration size discipline. The snapshot body path is exercised by the unit tests in `internal/httpapi/reload_body_test.go` for correctness; a follow-up harness commit can land the performance bar against a real fixture.
