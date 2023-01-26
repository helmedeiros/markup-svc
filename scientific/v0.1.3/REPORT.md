# scientific/v0.1.3 — report

Pre-registered bars for the guardrails decorator landed in ADR-0014. This file is the **pre-registration commit**; the measurement commit follows it in the same release window and adds the "measured" column without moving the bars. See [ADR-0012](../../docs/architecture/decisions/0012-scientific-harness.md) for the methodology and the two-commit pre-registration discipline.

> **Pre-registration commit: no measurements yet.** The bar values below were derived from first-principles analysis of the proposed call path (one interface-dispatched `Check` per rule + the rule's own body cost) plus the indexed baseline measured in [`../v0.1.0/REPORT.md`](../v0.1.0/REPORT.md). The measurement commit lands after this one and adds the trial-mean / std-dev to the "measured" cells without editing the bar cells.

## Reference host

Pilot and measurement both run on:

```
goos: darwin
goarch: arm64
cpu: Apple M4
go: 1.18 toolchain (project baseline)
```

Same posture as v0.1.0. Absolute numbers on a Linux/amd64 reference host will normalize; relative deltas against the indexed baseline are platform-stable.

## Measurement parameters

`go test -bench=. -benchmem -count=50 -benchtime=1s -run=^$ ./scientific/v0.1.3/...` per ADR-0012. 50 trial means per sub-benchmark; each trial is one 1-second `b.N`-iteration run. The three sub-benchmarks are interleaved per pass via the `b.Run` structure so host noise hits every measurement equally — the standard pattern across every release's harness.

## Absolute bars (pre-registration)

Pass condition at measurement time: `measured_mean ≤ bar + 2σ_measurement`.

| Benchmark | Bar (ns/op) | Measured mean ± σ | Allocs bar | Measured allocs | Status |
|---|---:|---:|---:|---:|---|
| `BenchmarkDecorator/indexed-baseline` | ≤ 470 | — | ≤ 12 | — | pre-reg |
| `BenchmarkDecorator/guardrails-zero-rules` | ≤ 480 | — | ≤ 12 | — | pre-reg |
| `BenchmarkDecorator/guardrails-three-rules` | ≤ 590 | — | ≤ 12 | — | pre-reg |

**Rationale for each bar:**

- **`indexed-baseline` ≤ 470 ns/op, ≤ 12 allocs**. Reproduces v0.1.0's `BenchmarkAdapter/indexed` (measured 442 ± 4 ns, 12 allocs) within a 2σ + cross-release-noise cushion. The row exists so the delta against the guardrails rows is computed on the same machine and same pass, not against a number from a separate harness binary.

- **`guardrails-zero-rules` ≤ 480 ns/op, ≤ 12 allocs**. Adds one wrapper layer in the call path with an empty rules loop. The expected cost over baseline is one indirect method call into `guardrails.Decider.Decide` plus a single zero-iteration `for range` — well under 10 ns. Bar pins the "wrapper-but-no-work" overhead to be invisible.

- **`guardrails-three-rules` ≤ 590 ns/op, ≤ 12 allocs**. The realistic production stack: `FactorRange` + `AllowedCountries(BR,DE,FR)` + `RequiredFields(country, customer_tier)`. Expected cost per ADR-0014's analysis is ~100–120 ns over baseline (three interface-dispatched `Check` calls + a 3-entry country slice walk + a 2-field required-field switch). Bar = baseline (470) + 120 ns budget = 590 ns.

Allocations stay at 12 on every row: the happy path is alloc-free (no `fmt.Errorf` wrap, no Decision-clearing). A regression that allocated on the happy path would push allocs above 12 and fail the bar.

## Ordinal bars (pre-registration)

Pass condition at measurement time: predicted order holds across measured trial means by > 2 pooled standard errors of the difference.

1. **`indexed-baseline ≤ guardrails-zero-rules ≤ guardrails-three-rules`**. Each decorator layer can only add work to the call path; a measured ordering that put baseline ABOVE zero-rules would mean we're measuring noise, not a real decorator cost. This ordering is the sanity floor for the harness itself.

2. **`(guardrails-three-rules − indexed-baseline) ≤ 150 ns + measurement noise`**. The ADR's analytic budget for the three shipped rules. A measured delta materially above 150 ns means one of the per-rule cost estimates was wrong and the cookbook's "operators wiring deep rule lists should keep this budget in mind" guidance needs revising.

## Analysis

Pre-registration commit. Analysis lands with the measurement commit.

## What this release's harness does NOT close

- **Veto-path cost is not measured.** The happy path (all rules allow) is the production hot path; vetoes are rare. ADR-0014's "~2 allocs / ~128 B per veto" claim from the `fmt.Errorf` wrap is analytic; this harness does not pin it. A `guardrails-three-rules-veto-first` row is tracked separately and would be motivated by an operator-reported veto storm.

- **Custom-rule N-rules scaling** beyond the three shipped rules. The ADR analyzes N≈20 custom rules (~500 ns extra dispatch + body sum) but no bar pins it. Operators wiring deep custom rule lists should benchmark their own configuration.

- **Cross-platform numbers.** As with v0.1.0, the Docker reference posture is Linux/amd64 + Go 1.18; the bars above are darwin / arm64 / Apple M4. The relative deltas survive; absolute ns/op values normalize.
