# scientific/v0.1.0 — report

> **Status: scaffold only.** The pre-registered bars (see ADR-0012 §Validation strategy) land in a follow-up commit. The measurement-vs-bar comparison lands in the commit after that. This two-commit split is what makes the bars falsifiable — they are committed before any number is taken, and they do not move once committed.

This file's structure when it ships in full:

## Reference host

`lscpu` summary of the host that produced the numbers below, so reproductions on different hardware see the reference posture (per ADR-0012 §How to run reproducibly).

## Pre-registered bars

### Ordinal bars

Claims about ordering between adapters / decorators. Pass condition: predicted order holds across trial means by > 2 pooled SE of the difference (~95% confidence the ordering is real and not noise).

### Absolute bars

Claims about cost magnitude. Pass condition: measurement does not exceed bar + 2σ of the measurement's own trial means. Bars are set from a pilot run with headroom (pilot mean + 2σ); the pilot script + raw output land in the same commit as the bar.

## Measurements

The 50-trial-mean table per `BenchmarkAdapter/*`, `BenchmarkDecorator/*`, `BenchmarkColdStart/*`, `BenchmarkRouter/*`. Format: `mean ± std dev` in nanoseconds, plus `allocs/op` from `b.ReportAllocs()`.

## Analysis

One paragraph per bar that did not pass, naming the bar, the measurement, and the deviation. The bar stays committed at its original value; the analysis explains the gap honestly. The next release picks up the optimization.
