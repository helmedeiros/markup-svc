# scientific/v0.1.4 — report

Pre-registered bars for the guardrails.Holder from ADR-0015. This file is the **pre-registration commit**; the measurement commit follows it in the same release window and adds the "measured" cells without moving the bars. See [ADR-0012](../../docs/architecture/decisions/0012-scientific-harness.md) for the methodology and the two-commit pre-registration discipline.

> **Pre-registration commit: no measurements yet.** The bar values below come from explicit addends against the v0.1.0 and v0.1.3 measurements, listed in the rationale paragraph for each row. The measurement commit lands after this one and adds the trial-mean / std-dev to the "measured" cells.

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

## Absolute bars (pre-registration)

Pass condition at measurement time: `measured_mean ≤ bar + 2σ_measurement`.

| Benchmark | Bar (ns/op) | Measured mean ± σ | Allocs bar | Measured allocs | Status |
|---|---:|---:|---:|---:|---|
| `BenchmarkDecorator/indexed-baseline` | ≤ 470 | — | ≤ 12 | — | pre-reg |
| `BenchmarkDecorator/guardrails-holder-zero-rules` | ≤ 490 | — | ≤ 12 | — | pre-reg |
| `BenchmarkDecorator/guardrails-holder-three-rules` | ≤ 600 | — | ≤ 12 | — | pre-reg |
| `BenchmarkReplace` | ≤ 200 | — | ≤ 2 | — | pre-reg |

**Rationale for each bar:**

- **`indexed-baseline` ≤ 470 ns/op, ≤ 12 allocs**. Reproduces the v0.1.3 measurement (430.4 ± 4.2 ns/op) within a 2σ + cross-release-noise cushion. The row exists so the delta against the Holder rows is computed in the same harness pass.

- **`guardrails-holder-zero-rules` ≤ 490 ns/op, ≤ 12 allocs**. Adds the Holder's RLock/RUnlock pair + slice-header copy + empty rules-loop walk. The lock-pair cost was measured at +10 ns over indexed in v0.1.0 for the same minimum-lock-hold pattern in `swap.Decider` (452 vs 442). Allowing the same +10 ns plus a small cushion for the empty-range overhead gives 470 + 20 = 490 ns budget. Allocations stay at 12 because the happy path makes no heap allocations through the Holder.

- **`guardrails-holder-three-rules` ≤ 600 ns/op, ≤ 12 allocs**. The v0.1.3 immutable guardrails-three-rules row measured 472.8 ± 4.2 ns/op (delta +42 ns over baseline). Adding the Holder's +10 ns lock-pair to that delta gives ~482 + a cushion to ~600 ns. Allocations stay at 12.

- **`BenchmarkReplace` ≤ 200 ns/op, ≤ 2 allocs**. `Replace` allocates one new `[]Rule` backing array via `make` + `copy` and assigns the slice header under the write lock. For a 3-rule input the `make` + `copy` is dominated by the slice-header allocation (1 alloc, ~48 bytes for the slice header + backing array) plus the lock-pair (~10 ns). The 200 ns budget is generous to absorb the interface-value copy cost and the sync.Mutex write-lock acquire/release on amd64 vs arm64. A 2-alloc bar (slice header + write lock potentially allocating internally on some Go versions) leaves room for runtime overhead while still catching a per-call escape.

## Ordinal bars (pre-registration)

Pass condition at measurement time: predicted order holds across measured trial means by > 2 pooled standard errors of the difference.

1. **`indexed-baseline ≤ guardrails-holder-zero-rules ≤ guardrails-holder-three-rules`**. Each Holder layer can only add work; a measured ordering that put baseline ABOVE zero-rules, or zero-rules ABOVE three-rules, would mean the harness is measuring noise rather than the lock-pair / rules-loop costs.

2. **`(guardrails-holder-zero-rules − indexed-baseline) ≈ swap.Decider delta from v0.1.0`**. Both are minimum-lock-hold patterns wrapping an inner Decider; the measured deltas should agree to within measurement noise. A measured difference materially above the swap.Decider's +10 ns would indicate the Holder's slice-header copy or rules-range setup adds non-trivial cost that the ADR did not account for.

3. **`(guardrails-holder-three-rules − guardrails-holder-zero-rules) ≈ (guardrails-three-rules − guardrails-zero-rules) from v0.1.3`**. The rules-loop body cost is the same between the immutable Decider and the Holder; the only difference is the lock-pair. Both rules-loop deltas should match within measurement noise.

## Analysis

Pre-registration commit. Analysis lands with the measurement commit.

## What this release's harness does NOT close

- **Replace under heavy concurrent Decide pressure.** `BenchmarkReplace` runs in isolation; it does not measure the cost of `Replace` while many goroutines are reading. The Holder's `TestHolderConcurrentDecideAndReplace` proves race-free correctness but does not measure throughput degradation. A `BenchmarkReplaceUnderLoad` row would address this if an operator-reported incident shows admin calls degrading `/decide` latency in production.

- **The veto path on the Holder is not measured.** Same gap as v0.1.3: the happy path is the production hot path; vetoes are rare. ADR-0014's "~2 allocs / ~128 B per veto" claim from the `fmt.Errorf` wrap stays analytic.

- **Cross-platform numbers.** As in every release, the Docker reference posture is Linux/amd64; the bars above are darwin / arm64 / Apple M4. Relative deltas survive; absolute ns/op values normalize.
