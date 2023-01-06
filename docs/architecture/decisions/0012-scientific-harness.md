# 12. Scientific Performance Comparison Harness

## Status

Accepted — `scientific/v0.1.0/` ships in the same release window. The harness scaffolding (Dockerfile, fixture, Makefile target, README, smoke test) landed in commit `0597d4c`; the benchmark function implementations in `dca898b`; the pre-registered bars + pilot raw output in `558c78c`. The measurement-vs-bar comparison is in `REPORT.md` and confirms all 11 absolute bars and all 4 ordinal bars pass — including the surprising `ColdStart/rules < ColdStart/snapshot` bar that ADR-0007's intuition would have inverted. The two-commit split between pre-registration and measurement worked as designed: bars committed before any number was taken, bars unchanged in the measurement commit.

## Context

Eleven ADRs ship the markup-svc engine layer, four engine adapters, three observability decorators, snapshot persistence, hot reload, and the router. Every ADR's Performance impact section makes specific quantitative claims:

- ADR-0001: per-`Decide` cost is "one indirect function call" beyond the engine's evaluation.
- ADR-0004 / 0005 / 0006: per-`Decide` cost varies by adapter — `inmemory` O(rules) walk + last-action-wins, `firstmatch` O(rules scanned) with early exit, `priority` O(rules log rules + scanned) with per-Execute sort, `indexed` O(K) hash lookups + post-filter walk on bucket candidates.
- ADR-0008: swap.Decider adds RLock + RUnlock + pointer copy per Decide — tens of nanoseconds.
- ADR-0009: OTel decorator adds tracer.Start + SetAttributes + context node — ~100 ns with the no-op tracer.
- ADR-0010: metrics decorator adds time.Now + time.Since + DecisionMetric construction + sink call — ~100-150 ns.
- ADR-0011: router adds Policy.Choose + dispatch + two string assignments — ~100-150 ns.

These numbers are credible per file inspection but they have never been *measured together on the same workload*. Operators choosing an adapter cannot answer "which adapter is fastest for my rule set?" with anything but back-of-envelope arithmetic; the project itself cannot answer "what is the cost of stacking the full decorator chain?" with measurements.

This ADR proposes the harness that produces those measurements honestly, reproducibly, and in a way that survives version-to-version comparison.

Three design questions.

### 1. What to measure?

The questions operators ask and the project's premise both need answered:

- **Per-adapter per-Decide latency** on a fixed rule set. One number per adapter. Trials interleave adapters per `-count` iteration (the harness file is one `*_test.go` so `go test -bench=.` runs each adapter's bench once per pass, then repeats `-count` times), which makes the n=50 comparison fair — a thermal blip or other system noise hits every adapter equally rather than punishing whichever runs last. Each "trial" is one `-benchtime=1s` run; the mean reported is across trial means; the standard deviation reported is across trial means. Where the comparison is "is adapter A faster than adapter B," the harness reports the difference between means with the pooled standard error; the pre-registered bar names the direction and the threshold (see §3).
- **Decorator overhead at the stack boundary**. Three single-decorator measurements (swap-only, otel-only, metrics-only) plus one full-stack measurement (metrics over otel over swap over engine). The full-stack number is what production deployments pay; the single-decorator numbers tell operators which layer to remove first if latency budget runs out.
- **Cold-start cost**: rules path vs snapshot path on the same indexed rule set. ADR-0007 claims the snapshot path skips `parser.ParseToCondition`. This is the measurement that proves it.
- **Router overhead on the Decide path**: per-Decide cost with the router added vs without it, on the same single-route rule set so the comparison is fair (router with one route has nothing to dispatch around; the cost is pure overhead).

Out of scope for v0.1.0:

- Throughput / qps under concurrent load. Latency at p50/p99 under N concurrent goroutines is its own harness; one workload at a time.
- Memory profile. The benchmarks in each package's `*_test.go` already report `allocs/op`; aggregated reporting belongs to a metrics dashboard, not the scientific harness.
- Cross-version drift. Once v0.1.0's numbers are committed, v0.2.0's harness will reuse the same bars to detect regressions; that integration is the v0.2.0 work, not this one.

### 2. How to run reproducibly?

Three constraints:

1. **Hardware variance**: laptop vs CI runner vs cloud VM all produce different absolute numbers. The harness reports normalized comparisons (adapter-vs-adapter, decorator-vs-decorator) so the raw machine cost factors out.
2. **OS / Go version variance**: Linux/amd64 with the project's Go 1.18 baseline is the reference platform. Anything else is informational only.
3. **Compiler / scheduler variance**: each trial is one `-benchtime=1s` run (so each trial loops `b.N` iterations until it accumulates 1s of timed work — for ~100 ns operations that is roughly 10M iterations per trial, plenty); `-count=50` produces 50 such trial means; the report shows mean ± standard deviation across the 50 trial means. Adapters are interleaved per `-count` pass so noise hits every measurement equally.

Residual variance the Docker image does NOT pin: CPU frequency governor (turbo, `cpufreq`), simultaneous multi-threading (SMT) state, and thermal throttling all vary across runs on the same host and the same image. The harness mitigates with the trial-mean methodology above (noise averages out across n=50) but cannot eliminate the residual. REPORT.md states the host's `lscpu` summary alongside the numbers so operators reproducing on different hardware see the reference machine's posture.

`scientific/v0.1.0/` ships:

- `harness.go`: a `go test -bench` driver that runs all the benchmarks listed above against a single committed rule-set fixture (`fixture.csv`), in a single process, with `b.ReportAllocs()` on every benchmark so allocation deltas surface alongside time deltas.
- `Dockerfile`: pins Linux/amd64 + the project's Go 1.18 + the rule-set fixture. `docker build && docker run` produces the same numbers on any machine that can run the image.
- `Makefile` target `scientific-v0.1.0`: builds the image, runs the bench, prints the table.
- `REPORT.md`: the written-up results. Pre-registered bars at the top; measurement table next; analysis paragraph last. The bars at the top of REPORT.md are committed before the bench runs; if the bench misses a bar, the misses are reported in the analysis and the bars stay put. **Bars never move post-commit.**

### 3. What's the pass/fail criterion?

Two flavours of bar:

- **Ordinal bars** — claims about ordering. Example: `inmemory.Decide ≤ firstmatch.Decide ≤ priority.Decide on a 50-rule set where every rule matches the request`. The bar passes if the predicted order holds across the trial means by more than two pooled standard errors of the difference (≈ 95% confidence the ordering is real and not noise).
- **Absolute bars** — claims about cost magnitude. Example: `full-stack overhead (metrics + otel + swap) ≤ 300 ns at p50 on Linux/amd64`. Bars of this kind are set from a pilot run *with headroom* — the pilot's mean + 2σ is the bar, not the pilot's mean — so a pass is a real signal rather than a lucky run. The pilot script + its raw output land in the same commit that introduces the bar so the methodology is auditable.

The 2σ headroom on absolute bars (and the 2-pooled-SE threshold on ordinal bars) gives roughly a 2.5% false-failure rate from noise alone, which is the right tradeoff: bars should be hard to violate by random scheduling jitter and easy to violate by a real regression.

Failure to meet a bar does NOT block the ADR's acceptance or the release. The project commits to honest reporting: if v0.1.0's full-stack overhead turns out to be 450 ns when the bar was 300 ns, REPORT.md states that, the analysis explains why, and v0.2.0's work picks up the optimization. The bar is the falsifiable claim; the measurement either supports it or it doesn't.

This mirrors the bre-go pattern at v0.15.0+. Honest framing is what makes the harness "scientific" rather than benchmark theater.

## Decision

`scientific/v0.1.0/` ships:

```
scientific/v0.1.0/
  Dockerfile
  Makefile.scientific          # included by top-level Makefile
  fixture.csv                  # 50-rule fixture, all matching the test Request
  harness_test.go              # Go benchmark driver
  REPORT.md                    # pre-registered bars + measured table + analysis
  README.md                    # how to run the harness
```

The benchmarks measured:

| Benchmark | What it measures |
|---|---|
| `BenchmarkAdapter/inmemory` | Per-`Decide` cost via `inmemory.NewFromRules` on `fixture.csv` |
| `BenchmarkAdapter/firstmatch` | Same, but firstmatch adapter |
| `BenchmarkAdapter/priority` | Same, but priority adapter (per-Execute sort cost included) |
| `BenchmarkAdapter/indexed` | Same, but indexed adapter (cold start includes Build()) |
| `BenchmarkDecorator/swap` | swap.Decider over the chosen baseline adapter |
| `BenchmarkDecorator/otel` | otel.Wrap with no-op tracer over the chosen baseline |
| `BenchmarkDecorator/metrics` | metrics.Wrap with RecordingSink over the chosen baseline |
| `BenchmarkDecorator/full-stack` | metrics → otel → swap → baseline (the production composition) |
| `BenchmarkColdStart/rules` | Time to `load.FromCSV` + `indexed.NewFromRules` + `Build()` |
| `BenchmarkColdStart/snapshot` | Time to `snapshot.Read` + `snapshot.LoadIntoIndexedDecider` |
| `BenchmarkRouter/single-route` | Router with one route + DefaultPolicy over the chosen baseline |

The Makefile target:

```makefile
.PHONY: scientific-v0.1.0
scientific-v0.1.0:
	docker build -t markup-svc-scientific-v0.1.0 -f scientific/v0.1.0/Dockerfile .
	docker run --rm markup-svc-scientific-v0.1.0
```

## Consequences

### Closed by this ADR

- The performance claims sprinkled across ADRs 0001-0011 have a single measurement source. When a number disagrees with an ADR's claim, both can be checked against the same fixture.
- Operators choosing an adapter have side-by-side latency data on a published workload.
- The harness is reproducible: any operator with Docker can re-run the same numbers on any machine.
- Pre-registered bars give the project a falsifiable performance commitment. Future releases will be measured against the same bars; regressions surface as the new measurement crossing a previously-met bar.

### NOT closed by this ADR

- Concurrent-load throughput. One workload at a time.
- Memory profile aggregation. Per-package `*_test.go` benchmarks already report `allocs/op`; aggregated dashboarding is its own work.
- Cross-version drift detection. v0.2.0's harness reuses these bars to detect regressions; the comparison infrastructure is v0.2.0 work.
- Auto-publication of REPORT.md to a docs site. Operators read the committed REPORT.md directly in this release.
- Continuous benchmarking in CI. The bars are checked manually for now; gating CI on benchmark results requires a stable runner that this project does not yet have.

### Performance impact

The harness itself runs out-of-band (operator invokes the Makefile target, not on every CI push). Per-`Decide` production cost is not changed by adding the harness. The harness's own runtime cost is the test environment's: building the Docker image and running n=50 trials of each benchmark takes a few minutes on commodity hardware; bounded and offline.

### Validation strategy

The harness's own validation:

- Each benchmark function pre-allocates its rule set + Decider before `b.ResetTimer()` so construction cost is not counted in the steady-state measurement.
- Each benchmark uses `b.ReportAllocs()` so allocation deltas surface alongside time deltas.
- The harness runs every benchmark from a clean Docker image so neighbouring tests cannot pollute each other's caches.
- REPORT.md commits the pre-registered bars in the same commit that introduces them; the measurement-vs-bar comparison runs in the follow-up commit so the bars cannot retroactively move to match the measurements.

`go test ./scientific/...` runs the harness as standard `go test -bench` benchmarks so the package compiles in the project's normal CI. Coverage of the harness itself is not gated; the harness measures, it does not implement business logic.
