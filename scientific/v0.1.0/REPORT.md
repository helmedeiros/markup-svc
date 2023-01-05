# scientific/v0.1.0 — report

Pre-registered bars for the v0.1.0 release. See [ADR-0012](../../docs/architecture/decisions/0012-scientific-harness.md) for the methodology.

> **These bars do not move after this commit lands.** A failed bar is reported honestly in the analysis paragraph (added in the measurement commit). The bar stays at the value committed here; the next release picks up the optimization.

## Reference host (pilot)

```
goos: darwin
goarch: arm64
cpu: Apple M4
go: 1.18 toolchain
```

The Docker reference posture per ADR-0012 is Linux/amd64. The measurement-vs-bar comparison run reproduces the bars there. Absolute numbers will normalize across the platform change; the ordinal claims survive both.

Bars below were derived from the pilot output in [`pilot.txt`](pilot.txt) using the methodology in ADR-0012 §3: each absolute bar is the pilot mean + 2σ across n=10 trial means (with trials interleaved per `-count` pass so noise hits every measurement equally); each ordinal bar names a direction that the pilot supports by more than 2 pooled standard errors of the difference between means.

## Pre-registered absolute bars (`p50 ≤ value` on the reference host)

| Benchmark | Bar (ns/op) | Allocs/op |
|---|---:|---:|
| `BenchmarkAdapter/inmemory` | ≤ 7414 | ≤ 204 |
| `BenchmarkAdapter/firstmatch` | ≤ 1032 | ≤ 28 |
| `BenchmarkAdapter/priority` | ≤ 1691 | ≤ 32 |
| `BenchmarkAdapter/indexed` | ≤ 455 | ≤ 12 |
| `BenchmarkDecorator/swap` (over indexed) | ≤ 454 | ≤ 12 |
| `BenchmarkDecorator/otel` (no-op tracer, over indexed) | ≤ 569 | ≤ 14 |
| `BenchmarkDecorator/metrics` (RecordingSink, over indexed) | ≤ 613 | ≤ 12 |
| `BenchmarkDecorator/full-stack` (metrics → otel → swap → indexed) | ≤ 734 | ≤ 14 |
| `BenchmarkRouter/single-route` (over indexed) | ≤ 466 | ≤ 12 |
| `BenchmarkColdStart/rules` | ≤ 54197 | ≤ 1608 |
| `BenchmarkColdStart/snapshot` | ≤ 166192 | ≤ 1827 |

## Pre-registered ordinal bars (ordering claims)

1. **`indexed < firstmatch < priority < inmemory` (per-`Decide` latency on this fixture).** Predicted ordering by adapter design: indexed buckets and exits, firstmatch scans until first hit, priority walks priority groups and exits, inmemory walks every matching rule. Pilot gap: 443 / 1026 / 1567 / 7115 ns. All separations exceed 2 pooled SE.
2. **`swap < otel < metrics` (decorator overhead over the indexed baseline).** Predicted by the per-decorator cost characterized in ADRs 0008-0010: swap is one RLock+RUnlock+pointer copy, otel adds a span allocation, metrics adds a value-typed event plus a sink call. Pilot gap: 451 / 534 / 604 ns. All separations exceed 2 pooled SE.
3. **`full-stack ≈ sum-of-decorator-deltas` (full stack overhead is approximately the sum of single-decorator overheads).** Pilot delta-sum is 260 ns (swap 8 + otel 91 + metrics 161); pilot full-stack delta is 282 ns. The bar is `full-stack-delta ≤ sum-of-deltas + 50 ns`; failure would indicate the decorators interact non-linearly.
4. **`ColdStart/rules < ColdStart/snapshot` on this fixture.** This is the SURPRISING claim — ADR-0007's intuition was that snapshot loading would be faster because it skips `parser.ParseToCondition`. The pilot says otherwise: snapshot loading is ~3× slower at 50 rules, dominated by JSON decode allocations. The bar pins this as the v0.1.0 measurement; the analysis paragraph (measurement commit) names the gap and characterizes where the snapshot path loses its predicted advantage at this rule-set size. **The bar does NOT promise this ordering will hold for larger rule sets** — that's a follow-up measurement, not a pre-registered claim for v0.1.0.

## Measurement vs bars

To be populated by the follow-up commit that runs the bars against a fresh `-count=50 -benchtime=1s` measurement on the same host (pilot used `-count=10 -benchtime=300ms` for speed). Each bar will be marked PASS or FAIL with the measured number alongside the bar value. Failed bars trigger an analysis paragraph; the bar itself does NOT move.

## Analysis

To be populated by the measurement commit, one paragraph per failed bar.
