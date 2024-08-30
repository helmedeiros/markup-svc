# 32. Shadow /decide execution — run champion + challenger in parallel

## Status

Accepted — when `--shadow-admin` is enabled AND a challenger is loaded via ADR-0031's `POST /admin/load-challenger`, every `/decide` request now runs the champion synchronously (its answer is returned to the caller) and dispatches the challenger evaluation in a goroutine with a tight deadline. The challenger's verdict is never served to the customer; only the comparison metrics fire. When no challenger is loaded the handler short-circuits without spawning a goroutine or allocating shadow state, paying only one nil interface check + one `RLock`/`RUnlock` pair on `shadow.Holder.mu`.

## Context

ADR-0031 added the admin surface that loads and clears a challenger Decider into `shadow.Holder`. The Holder sat unread; `/decide` saw the champion only. This ADR closes that loop: the data plane now reads the challenger, runs it on every request, and emits the comparison signal operators need to decide whether to promote the challenger to champion.

The signal is the agreement rate (per-request "did champion and challenger agree on the markup factor?") plus a factor-delta histogram for the disagreement long tail, plus one-sided counters for the case where one side fires and the other declines. The registry-side promotion gate (an open question in the workspace ROADMAP) reads these metrics through the existing `businessstats.PromReader` (ADR-0010 in model-registry) and the `mrctl shadow` subcommand planned in later iterations.

## Decision

### Options pattern on `httpapi.Decide`

`Decide(d markup.Decider, opts ...DecideOption) http.Handler` replaces the single-argument form. Zero options yields the v0.1.x handler bit-for-bit. Existing call sites (router-mode, untraced wiring) compile unchanged.

`WithShadow(holder ChallengerHolder, metrics ShadowMetrics, timeout time.Duration, tracer trace.Tracer)` installs the shadow pipeline. The handler's hot path branches on `cfg.shadow == nil` first and on `holder.Get() loaded=false` second — two short-circuit gates so a Decide call that has no shadow wiring or no challenger loaded pays one nil interface check + one `RLock`/`RUnlock` pair on `shadow.Holder.mu` and exits.

### Goroutine dispatch

The shadow goroutine is spawned BEFORE the response body is encoded. It runs concurrently with the response write and the access-log emission. The parent request's cancellation is NOT propagated; the goroutine gets a detached context built via `trace.ContextWithSpanContext(context.Background(), trace.SpanContextFromContext(parent))`. That preserves the W3C trace context (so the `markup.challenger.evaluate` span links into the request's trace) but lets the goroutine survive the response write.

The shadow context has its own `WithTimeout(DefaultShadowTimeout)` (default 10 ms). A challenger that misses the deadline records `markup_challenger_eval_timeout_total` and exits; the comparison is dropped.

### Comparison logic

After both Decide calls return:

- If `shadowCtx.Err() == DeadlineExceeded` — record timeout, return.
- If challenger returned an error that is NOT `markup.ErrNoMatch` — record error, return.
- Otherwise classify by which side fired:
  - Champion fired + challenger declined → `markup_challenger_one_sided_total{side="champion_only"}`
  - Champion declined + challenger fired → `markup_challenger_one_sided_total{side="challenger_only"}`
  - Both declined → `markup_challenger_agreement_total{agree="true"}` (both said no rule matched — that is agreement)
  - Both fired → compare `MarkupFactor` with `factorEpsilon = 1e-9`. Equal → `agree="true"`. Different → `agree="false"` AND record the absolute delta in `markup_challenger_factor_delta`.

The agreement criterion is FACTOR equality, not rule-name equality. The rule name is provenance; the factor is what the customer pays. Two different rule names producing the same factor is functional agreement.

### Metrics

All registered against the same private Prometheus registry as `markup_decide_total` so one `/metrics` scrape carries both:

- `markup_challenger_agreement_total{agree="true"|"false"}` — counter
- `markup_challenger_one_sided_total{side="champion_only"|"challenger_only"}` — counter
- `markup_challenger_eval_timeout_total` — counter
- `markup_challenger_eval_errors_total` — counter
- `markup_challenger_factor_delta` — histogram, buckets `{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1.0}` covering the realistic factor-delta range (rules carry factors at ~3 decimal places in `[0.5, 5.0]`).

### Span

`markup.challenger.evaluate` is a child of the original request's span context. Operators paste the request's trace ID into Jaeger and see the champion's `markup.decider.decide` chain AND the parallel `markup.challenger.evaluate` chain in the same waterfall.

### Fast path when no challenger is loaded

`dispatchShadow` checks `cfg.shadow == nil` first (no shadow wired at all — most builds) and falls through. If shadow is wired, it calls `holder.Get()` and short-circuits on `loaded=false` without spawning a goroutine. The two-gate fast path is the operational invariant: a markup-svc instance that has shadow-admin enabled but no challenger loaded pays one nil interface check + one `RLock`/`RUnlock` pair on `shadow.Holder.mu` per `/decide`. The fast-path cost is not yet measured end-to-end; the bar lands in a scientific-harness commit alongside the deferred follow-ups listed below.

## Consequences

### Positive

- The challenger lifecycle is end-to-end useful for the first time. ADR-0031 stored a challenger; this ADR runs it. The agreement counter starts ticking on the first `/decide` after a `POST /admin/load-challenger` succeeds.
- Customer impact is zero by construction. The champion's answer is returned synchronously; the challenger runs in a detached goroutine and its output reaches only metrics + the `markup.challenger.evaluate` span. There is no code path that could substitute the challenger's answer for the champion's.
- Trace propagation works: the challenger span links into the request's trace, so an operator investigating a divergence in `/metrics` can drill into the trace via the existing Jaeger workflow.
- The metric shape is consumable by the registry's `mrctl shadow` subcommand (planned in later iterations) without any further markup-svc work.

### Negative

- A goroutine spawn per `/decide` when a challenger is loaded. At the platform's measured 2000 QPS sustained, this is 2000 goroutines/sec created and destroyed. Public Go runtime numbers for the naive spawn-per-request path are in the ~1–3 µs/spawn range (including stack allocation); at 2000 QPS that puts goroutine scheduling at 2–6 ms of CPU/sec across all cores — well below a budget concern but not a sub-100 ns operation. Each dispatch also allocates a fresh detached context (`trace.ContextWithSpanContext(context.Background(), ...)`, ~48 bytes), adding ~96 KB/sec of short-lived heap at 2000 QPS. No goroutine pool today; if pprof under production load shows either cost dominating, a bounded semaphore + a pre-built tracer-less base context are the follow-ups.
- The shadow deadline is 10 ms by default. If the challenger Decider's p99 exceeds that under load, the timeout counter dominates and the agreement signal degrades. The deadline is `httpapi.DefaultShadowTimeout`; operators tune it via the wiring layer (not a flag today; a `--shadow-timeout` flag is a follow-up if production tuning needs it).
- The fast-path cost is real but small: when shadow is wired and the holder is empty, every `/decide` pays one nil interface check + one `RLock`/`RUnlock` pair on `shadow.Holder.mu`. The v0.1.4 REPORT measured a single Holder RLock/RUnlock pair at ~15 ns on arm64/M4 for an analogous holder (`scientific/v0.1.4/REPORT.md`); the same uncontended posture applies here, but an amd64/Linux number has not been measured. The fast-path bar is pre-registered in the scientific harness in a follow-up commit before production promotion.
- `prom.New()` registers the five shadow counters unconditionally — they appear on `/metrics` as zero-valued series whether or not `--shadow-admin` is on. Operators who alert on `sum(rate(markup_challenger_*))` see zero rather than absence, which is honest. The registration-without-wiring is a known hygiene cost; a `NewWithShadow()` constructor that conditionally adds the shadow counters is a follow-up if dashboard noise matters.

### Deliberately not here

- Goroutine pooling. The naïve spawn-per-request is honest; a pool is a follow-up if measurement shows it matters.
- A `--shadow-timeout` flag. The default ships; operators tune via the wiring layer until production data justifies a flag.
- Registry-side push of challenger bytes. That is the next iteration in the shadow arc; this ADR is purely markup-svc.
- `mrctl shadow` and the business-stats extension. Later in the arc.
- A dashboard + alerts. The pricing-observability iteration adds them.

## Alternatives considered

**Run champion and challenger concurrently in a `sync.WaitGroup`** — would let the challenger latency contribute to the request's wall-clock budget when it finishes first. Rejected: the request's contract is the champion's answer. Tying response latency to the challenger's runtime would make a slow challenger a customer-visible regression, defeating the entire safe-experimentation premise.

**Compare by rule name instead of (or in addition to) factor** — would let operators see "the same rule fired" agreement and "different rule fired" disagreement. Rejected for v1: rule names are provenance, not outcome. Two different rules producing the same factor are functionally identical to the customer. A rule-name comparison metric is a follow-up if operators report missing signal.

**Use `context.WithoutCancel` (Go 1.21+) for the detached goroutine** — would preserve all parent context values, not just the span context. Rejected: the markup-svc baseline is Go 1.18 per `feedback_go_118_atomic_patterns`. The current span-context-only detach is sufficient; richer context propagation is a follow-up if the Go-version baseline moves.

**Synchronous shadow execution behind a feature flag** — would let early operators verify the comparison logic before paying the goroutine cost. Rejected: there is no honest mode where the champion's answer waits for the challenger; either we serve the champion (this ADR) or we run a side-channel comparison (also this ADR). Synchronous shadow would invite the slow-challenger regression.
