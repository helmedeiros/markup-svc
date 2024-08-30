# 33. Shadow sample rate — operator-tunable comparison frequency

## Status

Accepted — `--shadow-sample-rate` (default `1.0`) controls what fraction of `/decide` calls run the challenger comparison when a challenger is loaded. `1.0` runs every request (the ADR-0032 behavior); `0.0` disables the comparison while keeping the admin surface live; values in between sample uniformly at random. A new counter `markup_challenger_sampled_total{sampled="true"|"false"}` lets operators measure the effective comparison rate.

## Context

ADR-0032 ran the challenger on every `/decide` when one was loaded. The Negative-Consequences section acknowledged the cost: a goroutine spawn + comparison work per request, plus the detached-context allocation (~96 KB/sec at 2000 QPS). Three real use cases pushed back against the always-on default:

1. **Promotion** (the original framing). Wants a high sample rate to clear the agreement gate quickly.
2. **Steady-state monitoring** after a challenger has cleared the gate. The operator already knows the challenger is good; they just want to keep watching for drift. 100% sampling here is wasted CPU.
3. **Performance measurement and data collection.** "What would prices look like if we charged 5% more for enterprise customers?" or "Is the new indexed engine actually 2× faster than firstmatch under production load?" These workflows often run for days or weeks; 1-10% sampling is fine because the signal is statistical, not point-in-time.

A fixed always-on rate works for the first case and is wasteful for the second and third. The operator-tunable rate moves the calibration to where the decision lives.

## Decision

### `--shadow-sample-rate` flag

Float in `[0.0, 1.0]`, default `1.0`. Flag is plumbed through `wireTracedHandler` into a new `WithShadow` parameter on the `/decide` handler. The cmd shell holds the value; tests pass their own.

### `dispatchShadow` sampling logic

The fast-path order now is:

1. `cfg.shadow == nil` → return (no shadow wired).
2. champion errored with non-`ErrNoMatch` → return (no useful comparison).
3. `holder.Get() loaded=false` → return (challenger not loaded).
4. `cfg.sampleAllows() == false` → record `RecordSampled(false)` and return.
5. `RecordSampled(true)` and spawn the goroutine.

`sampleAllows` short-circuits on the two boundary values (`<= 0` always false, `>= 1` always true) without calling the random source. For values in between, it calls a sampler function (default `math/rand.Float64`, overridable via `WithShadowSampler` for tests) and compares to the rate.

Sampling is uniform random, not request-property-derived. A future enhancement could sample by correlation_id hash so replays of the same request always make the same sampling decision — useful for debugging — but adds complexity without an immediate operator pain point.

### `markup_challenger_sampled_total{sampled}`

Fires once per `/decide` where a challenger is loaded AND the champion succeeded (i.e. the path reaches the sampling check). `sampled="true"` means the comparison ran; `sampled="false"` means the sample check skipped it. The effective comparison rate over a window is `true / (true + false)`.

This is honest about the meaning of `markup_challenger_agreement_total`: the agreement rate is over SAMPLED comparisons, not over total `/decide` calls. An operator reading agreement = 99% at sample-rate=0.01 should know they sampled ~1% of decisions, not 100%.

## Consequences

### Positive

- Operators can tune the cost-vs-confidence-velocity trade-off by purpose. Promotion campaigns run at 100%; long-running data-collection runs at 1-10%; steady-state monitoring runs at whatever rate the team's drift-detection appetite requires.
- The `markup_challenger_sampled_total` counter makes the sampling decision auditable. An operator reading agreement at 99% knows the sample rate that produced it.
- Sampling at 0.0 disables comparison without un-loading the challenger. Useful for staging deploys: load the rule set, confirm Diagnose passes, hold off on comparison until production rollout.

### Negative

- Sampling slows confidence accumulation proportionally. At `sample=0.1` and 2000 QPS, the documented promote-to-champion gate threshold of `samples ≥ 10_000` takes 50 seconds to reach (vs 5 seconds at `sample=1.0`). The runbook captures this trade-off; tooling does not enforce a minimum sample rate during the calibration phase.
- Rare-case bugs become harder to catch. If a challenger fails on a 0.5% slice of traffic, at 10% sample the disagreement rate appears at 0.05% of decisions — the metric looks clean for hours before the first hit. Recommendation in the runbook: keep `sample=1.0` for the first 24 hours of a new challenger; lower only after the agreement gate has cleared once.
- The agreement metric's denominator changes meaning at sub-1.0 rates. Operators reading dashboards must cross-reference with the sampled counter; the dashboards added in pricing-observability (5d) need a note alongside the agreement panel.

### Deliberately not here

- Per-rule sampling (sample 100% on edge-case rules, 1% on common ones). Would require the data plane to know which rules are "edge-case", which is a separate observability problem. A future addition.
- Per-correlation-id deterministic sampling. Would let replays of the same request always sample-or-not consistently — useful for debugging — but adds complexity without an immediate operator pain point.
- `--shadow-sample-rate` validation that rejects out-of-range values at boot. The `sampleAllows` logic already short-circuits `<= 0` and `>= 1`; a typo'd `1.5` runs every request, a typo'd `-0.1` disables comparison. Tolerant of operator typos; operators read the boot log to confirm.

## Alternatives considered

**Adaptive sampling that ramps from 100% during calibration to 1% in steady state** — would automate the regime split the runbook describes. Rejected for v1: requires a state machine in the data plane that tracks "have we reached steady state yet?", which is a new concern. The operator-controlled flag is honest about who owns the decision.

**Sample by correlation_id hash modulo N for deterministic replay** — would make debugging easier (the same correlation_id always samples or doesn't). Rejected: adds complexity, no operator pain point yet, and would need a way to communicate the sampling threshold (the hash bucket) alongside the rate. Pure random sampling is simpler and the same in expectation.

**Compose sample rate with the existing canary observer pattern** — would let the registry's canary supervisor read effective sample rate and decide promotions only when the rate matches the gate calibration. Powerful, but parked alongside the auto-promote-from-shadow follow-up.
