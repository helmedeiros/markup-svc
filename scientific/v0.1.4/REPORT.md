# scientific/v0.1.4 — report

Pre-registered bars + measured values for the guardrails.Holder from ADR-0015. See [ADR-0012](../../docs/architecture/decisions/0012-scientific-harness.md) for the methodology and the two-commit pre-registration discipline.

> **Bars did not move between pre-registration and measurement.** The bar values below are exactly those committed in the pre-registration commit. The "measured" column was added in this commit alongside the analysis paragraph.

## Reference host

Pilot and measurement both run on:

```
goos: darwin
goarch: arm64
cpu: Apple M4
go: 1.18 toolchain (project baseline)
```

Same posture as v0.1.0 and v0.1.3. Absolute numbers normalize on a Linux/amd64 reference host; relative deltas against the indexed baseline are platform-stable.

## Measurement parameters

`go test -bench=. -benchmem -count=20 -benchtime=300ms -run=^$ ./scientific/v0.1.4/...` per the v0.1.3 cadence. 20 trial means per sub-benchmark; each trial is one 300ms `b.N`-iteration run. Sub-benchmarks under `BenchmarkDecorator` interleave per pass; `BenchmarkReplace` runs independently. Standard pattern across every release's harness.

## Absolute bars: measured vs bar

Pass condition: `measured_mean ≤ bar + 2σ_measurement`.

| Benchmark | Bar (ns/op) | Measured mean ± σ | Allocs bar | Measured allocs | Status |
|---|---:|---:|---:|---:|---|
| `BenchmarkDecorator/indexed-baseline` | ≤ 470 | 442.2 ± 9.5 | ≤ 12 | 12 | ✅ PASS |
| `BenchmarkDecorator/guardrails-holder-zero-rules` | ≤ 490 | 457.0 ± 11.2 | ≤ 12 | 12 | ✅ PASS |
| `BenchmarkDecorator/guardrails-holder-three-rules` | ≤ 600 | 476.1 ± 8.5 | ≤ 12 | 12 | ✅ PASS |
| `BenchmarkReplace` | ≤ 200 | 17.2 ± 0.3 | ≤ 2 | 1 | ✅ PASS |

**Rationale for each bar:**

- **`indexed-baseline` ≤ 470 ns/op, ≤ 12 allocs**. Reproduces the v0.1.3 measurement (430.4 ± 4.2 ns/op) within a 2σ + cross-release-noise cushion. The row exists so the delta against the Holder rows is computed in the same harness pass.

- **`guardrails-holder-zero-rules` ≤ 490 ns/op, ≤ 12 allocs**. Adds the Holder's RLock/RUnlock pair + slice-header copy + empty rules-loop walk. The lock-pair cost was measured at +10 ns over indexed in v0.1.0 for the same minimum-lock-hold pattern in `swap.Decider` (452 vs 442). Allowing the same +10 ns plus a small cushion for the empty-range overhead gives 470 + 20 = 490 ns budget. Allocations stay at 12 because the happy path makes no heap allocations through the Holder.

- **`guardrails-holder-three-rules` ≤ 600 ns/op, ≤ 12 allocs**. The v0.1.3 immutable guardrails-three-rules row measured 472.8 ± 4.2 ns/op (delta +42 ns over baseline). Adding the Holder's +10 ns lock-pair to that delta gives ~482 + a cushion to ~600 ns. Allocations stay at 12.

- **`BenchmarkReplace` ≤ 200 ns/op, ≤ 2 allocs**. `Replace` allocates one new `[]Rule` backing array via `make` + `copy` and assigns the slice header under the write lock. For a 3-rule input the `make` + `copy` is dominated by the slice-header allocation (1 alloc, ~48 bytes for the slice header + backing array) plus the lock-pair (~10 ns). The 200 ns budget is generous to absorb the interface-value copy cost and the sync.Mutex write-lock acquire/release on amd64 vs arm64. A 2-alloc bar (slice header + write lock potentially allocating internally on some Go versions) leaves room for runtime overhead while still catching a per-call escape.

## Ordinal bars: measured vs bar

Pass condition at measurement time: predicted order holds across measured trial means by > 2 pooled standard errors of the difference.

1. **`indexed-baseline ≤ guardrails-holder-zero-rules ≤ guardrails-holder-three-rules`**. Measured: 442.2 ≤ 457.0 ≤ 476.1. Pooled SE for baseline→zero ≈ 3.3 ns; gap 14.8 / 3.3 ≈ 4.5 pooled SE. Pooled SE for zero→three ≈ 3.1 ns; gap 19.2 / 3.1 ≈ 6.1 pooled SE. Both separations clear the 2-SE threshold by 2-3×. ✅ PASS.

2. **`(guardrails-holder-zero-rules − indexed-baseline) ≈ swap.Decider delta from v0.1.0`**. Measured Holder lock-pair delta: 14.8 ns. v0.1.0 measured swap.Decider delta: 10.0 ns. Difference 4.8 ns at a combined SE of ~3 ns, ~1.6 SE — within the 2-SE threshold the methodology allows. ✅ PASS. The Holder's slice-header copy + empty-range setup runs about 5 ns slower than swap.Decider's pointer copy, which is consistent with the slice-header being 24 bytes vs the pointer's 8.

3. **`(guardrails-holder-three-rules − guardrails-holder-zero-rules) ≈ (guardrails-three-rules − guardrails-zero-rules) from v0.1.3`**. Measured Holder rules-loop delta: 19.2 ns. v0.1.3 immutable Decider rules-loop delta: 472.8 − 454.5 = 18.3 ns. Difference 0.9 ns, well inside the measurement noise. ✅ PASS. The Holder adds the lock-pair on top of the rules-loop body; the body cost itself is the same.

## Analysis

All 4 absolute + 3 ordinal bars pass; every measured mean sits below its bar with single-digit-σ noise on the relevant rows. Four points stand out:

**The Holder lock-pair lands at 14.8 ns over baseline, slightly above the swap.Decider precedent (+10 ns).** The 5 ns differential is consistent with the slice-header copy (24 bytes vs swap's 8-byte pointer copy) and a small per-Decide overhead for the empty-range loop setup. The ADR-0015 budget of "~10 ns" was tight; the measured number stays comfortably under the 490 ns absolute bar but a future Holder-shape change should expect this room rather than the swap.Decider number directly.

**The Holder rules-loop overhead matches the immutable Decider exactly.** Both produce a ~19 ns delta over their zero-rules baselines for the three shipped rules; the ADR's prediction that "the rules-loop body cost is the same between mutable and immutable" holds in measurement. Operators who switch on `--guardrails-admin` pay only the lock-pair extra, not a duplicated rules-loop cost.

**`Replace` clocks in at 17 ns / 1 alloc / 48 bytes.** For the 3-rule input the 48 bytes is exactly the backing array for an `[]Rule` of length 3 — each interface header is 16 bytes (type + pointer pair on amd64), times 3, equals 48. The slice header itself stays on the stack; `make([]Rule, 3)` + `copy()` produces a single heap allocation. The 200 ns bar leaves an order of magnitude of headroom for the slice-copy and write-lock acquire/release. At admin-call rates of a handful per day this is invisible; even a misconfigured operator script calling Replace at request-path QPS would only contend the write lock, not OOM the process.

**Allocations on every Decide row are exactly 12.** This matches the indexed baseline. Both the immutable Decider (v0.1.3) and the Holder (v0.1.4) add zero allocations on the happy path. A regression that escaped a slice header or boxed an interface in the Decide hot path would fail the bar.

The measurement parameters match v0.1.3 (20 trials × 300ms) for cross-release comparability. A future re-run at 50 trials would tighten the baseline row's σ from 9.5 to ~5 ns (square-root scaling), which is where v0.1.0 and v0.1.3 landed; this run's slightly wider noise reflects host-state variability that the cross-release deltas absorb without changing the conclusions.

## What this release's harness does NOT close

- **Replace under heavy concurrent Decide pressure.** `BenchmarkReplace` runs in isolation; it does not measure the cost of `Replace` while many goroutines are reading. The Holder's `TestHolderConcurrentDecideAndReplace` proves race-free correctness but does not measure throughput degradation. A `BenchmarkReplaceUnderLoad` row would address this if an operator-reported incident shows admin calls degrading `/decide` latency in production.

- **The veto path on the Holder is not measured.** Same gap as v0.1.3: the happy path is the production hot path; vetoes are rare. ADR-0014's "~2 allocs / ~128 B per veto" claim from the `fmt.Errorf` wrap stays analytic.

- **Cross-platform numbers.** As in every release, the Docker reference posture is Linux/amd64; the bars above are darwin / arm64 / Apple M4. Relative deltas survive; absolute ns/op values normalize.
