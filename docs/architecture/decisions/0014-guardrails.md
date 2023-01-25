# 14. Guardrails Decorator at the Decider Port

## Status

Accepted — `internal/decider/guardrails` ships the `markup.Decider`-port decorator with a single-method `Rule` port (`Check(ctx, decision, req) (allowed bool, reason string)`), the `ErrGuardrailViolation` sentinel, and three concrete rules (`FactorRange`, `AllowedCountries`, `RequiredFields`). The `cmd/markup-server` binary wires the rules from four flags: `--min-factor`, `--max-factor`, `--allowed-countries`, `--required-fields` — the original ADR sketched the first three; `--required-fields` was added during implementation so every shipped rule is reachable without writing Go. Composition tests pin the metrics + OTel decorator behavior under veto and pass-through paths. The four-flag e2e tests against `testdata/rules.csv` prove the wire is real work: the asymmetric pair (`--max-factor=1.0` vetoes the enterprise rule's 1.15; `--max-factor=2.0` allows it) is the production-equivalent proof.

## Context

Thirteen Accepted ADRs ship the engine, the adapters, snapshot, hot reload, the router, the observability decorators, the scientific harness, and the production deploy artifacts. A markup decision is now end-to-end measurable, hot-reloadable, observable, and deployable.

What is still missing is **safety**. The current stack will faithfully serve whatever Decision the engine produces — a rule with a typo in its Factor column (`14.0` instead of `1.40`), a `--snapshot` reload that picks up a partially-edited file, an A/B variant that experiments with an unbounded markup. Each of these produces a Decision the engine considers valid that the **business** considers a service-impacting bug. There is no layer that says "no, this number is not allowed to leave the building."

The Pricing Decision Platform architecture diagram has a "Guardrails" box explicitly between the Decision Router / engines and the Decision Event Stream / observability tier. This ADR closes that box.

Three design questions.

### 1. Where does Guardrails sit in the decorator stack?

The existing recommended composition is `metrics → otel → router → swap → engine`. Three candidate positions for guardrails:

- **Outside metrics** (`guardrails → metrics → otel → router → ...`). A vetoed Decision never reaches the metrics decorator. Operators dashboarding "vetoed Decision rate" lose the signal in the metrics pipeline.
- **Inside metrics, outside otel** (`metrics → guardrails → otel → router → ...`). The metrics decorator sees the vetoed Decision as an error, dashboards can slice on `Err` matching the sentinel, the OTel span sees `codes.Error` with `RecordError`. Operators get full observability of the veto event.
- **Inside the router** (`metrics → otel → router → guardrails → swap → engine`). Each route can have its own guardrails. Useful for per-experiment safety bounds (an A/B variant that wants tighter bounds than the control). But every route needs to declare its guardrails separately; an operator who forgets to wire guardrails on a new route ships traffic to an unprotected engine.

**Pick "inside metrics, outside otel" by default.** The metrics decorator + OTel span see the veto as a server-side error and dashboards correctly classify it. Per-route guardrails are still possible (wrap each route's Decider before adding it to the Router), but the default is single-pass-protection so a forgotten wiring cannot expose traffic to an unprotected engine.

### 2. What does the rule port look like?

A single-method interface keeps the port small and the testing easy:

```go
type Rule interface {
    Check(ctx context.Context, decision markup.Decision, req markup.Request) (allowed bool, reason string)
}
```

When `allowed` is true, the Decider passes the Decision through unchanged. When false, the Decider replaces the Decision with the zero value and returns `fmt.Errorf("%w: %s", ErrGuardrailViolation, reason)` so callers can `errors.Is(err, ErrGuardrailViolation)` to distinguish guardrail vetoes from other errors.

`ctx` is carried through every other method at the Decider port (`markup.Decider.Decide`, `swap.Decider.Decide`, `router.Decider.Decide`, the metrics and OTel decorators); the Rule port follows the same convention so a custom Rule implementing a dynamic-limits lookup against a config service can honor request cancellation. The shipped rules do not consult `ctx` — they are pure functions of `(decision, req)` — but the signature stays consistent across the port surface.

The reason string is what the rule wants logged. When the OTel decorator wraps the guardrails decorator, the span records `codes.Error` with the wrapped error message; when an operator wires the metrics decorator (library-provided per ADR-0010; not in `cmd/markup-server` by default), the metrics event classifies the veto as `Err` with the wrapped error preserved on `event.Err`.

### 3. What rules ship in the package?

Three rules cover the common cases without requiring operators to write custom Go code for the v0.1.3 release:

- **`FactorRange{Min, Max float64}`** — vetoes Decisions whose `MarkupFactor` is outside `[Min, Max]`. The canonical "no markup > 3×" rule.
- **`AllowedCountries{Countries []string}`** — vetoes when the Request's `Country` is not in the list. Useful for a service that should only return Decisions for a known set of markets; an unexpected country indicates a routing / config drift.
- **`RequiredFields{Fields []string}`** — vetoes when the Request lacks a named non-empty field. Covers the seven string fields on `markup.Request` (`ProductID`, `Category`, `CustomerTier`, `Channel`, `Country`, `Inventory`, `TimeWindow`); `Amount` is numeric and is excluded (a zero `Amount` is a legal request, not a missing field). Catches misconfigured upstreams that forgot to pass `CustomerTier` or `Country` (would otherwise produce a no-match silently).

Operators with more elaborate business invariants (regulatory bounds per country, per-tenant maximums, dynamic limits read from a config service) write a custom `Rule` implementation and wire it via the `New(inner, rules...)` constructor. The shipped rules are the floor, not the ceiling.

## Decision

`internal/decider/guardrails` ships:

```go
// Rule is the veto port. Implementations decide whether a given
// (Decision, Request) pair is allowed to leave the service.
type Rule interface {
    Check(ctx context.Context, decision markup.Decision, req markup.Request) (allowed bool, reason string)
}

// Decider wraps inner with a sequence of Rules. On allowed: passes
// the Decision through unchanged. On vetoed: returns the zero
// Decision and an error wrapping ErrGuardrailViolation with the
// rule's reason string.
type Decider struct { /* ... */ }

func New(inner markup.Decider, rules ...Rule) *Decider

// ErrGuardrailViolation is the sentinel distinguishing veto errors
// from other engine errors. Wrapped via fmt.Errorf("%w: <reason>")
// so errors.Is reaches it from caller code.
var ErrGuardrailViolation = errors.New("guardrails: decision vetoed")

// FactorRange vetoes Decisions whose MarkupFactor is outside [Min, Max].
type FactorRange struct{ Min, Max float64 }

// AllowedCountries vetoes when req.Country is not in Countries.
type AllowedCountries struct{ Countries []string }

// RequiredFields vetoes when req lacks any of the named non-empty fields.
// Covers the seven string fields on markup.Request; Amount (numeric) is
// out of scope — operators wanting to require a non-zero Amount add a
// custom Rule.
type RequiredFields struct{ Fields []string }
```

`cmd/markup-server` gains four flags wiring the shipped rules:

```sh
--min-factor=0.5                       # FactorRange.Min
--max-factor=3.0                       # FactorRange.Max
--allowed-countries=BR,DE,FR           # AllowedCountries.Countries
--required-fields=customer_tier,country # RequiredFields.Fields
```

When at least one of those flags is set on the command line, `wireTracedHandler` (and `wireRouterHandler`) compose `guardrails.New(inner, rules...)` between the holder/router and the OTel decorator. Detection of "operator set the flag" uses `flag.FlagSet.Visit` rather than checking against the zero default, so `--max-factor=0` is treated as an explicit (degenerate) operator choice rather than "left at default." When no guardrail flag is set the decorator is not constructed and not in the call path — zero per-`Decide` overhead.

Rule order in the assembled slice follows ADR-0014's first-veto-wins semantics and the cookbook's cheapest-first guidance: `FactorRange` → `AllowedCountries` → `RequiredFields`. Operators that want a different order write a wrapper main; the default reflects the most common veto cause in operator-reported incidents (a misconfigured factor).

Operators wanting custom rules write a small wrapper main against the `Rule` port — same pattern as the metrics-sink wrapper main documented in the observability cookbook.

## Consequences

### Closed by this ADR

- The "Guardrails" box in the Pricing Decision Platform architecture has an implementation at the `markup.Decider` port.
- `ErrGuardrailViolation` is distinguishable from `ErrNoMatch` and from other engine errors via `errors.Is`.
- OTel spans on the default `cmd/markup-server` wiring record vetoes as `codes.Error` with the wrapped error message — operators get the veto signal in trace dashboards out of the box.
- When an operator wires the metrics decorator (library-provided per ADR-0010; the default binary does not wire a Sink), the metrics event classifies vetoes as `Err` with the wrapped reason preserved on `event.Err`. The wrapper-main pattern documented in the observability cookbook is unchanged.
- Three shipped rules cover the common cases; the `Rule` port lets operators add custom business invariants without touching the package.

### NOT closed by this ADR

- Per-route guardrails in the router. Operators that want tighter bounds on an A/B variant wrap each route's Decider before adding it to the Router; the default is single outer-veto-protection so a forgotten wiring cannot expose traffic.
- Rule-set hot reload for guardrails. The flags are read at boot; changing them requires a restart. A `POST /admin/guardrails` endpoint could land in a follow-up if operators ask.
- Per-tenant guardrails (different bounds for different upstream callers). The `Rule` port is single-pair (`Decision`, `Request`); a tenant axis would come through a Request field. Out of scope for this release.
- Soft-veto / "log but allow" mode. Every Rule vetoes hard. A soft mode (log the violation, return the Decision anyway) is its own ADR if anyone asks.
- Dynamic limits read from a config service. Operators implement that via a custom Rule that reads its bounds from wherever; the shipped rules are static.

### Performance impact

Per-`Decide` overhead when guardrails are mounted:

- One method call to `inner.Decide` (the existing engine path; unchanged).
- One sequential loop over the configured `[]Rule` — for each rule, one interface-dispatched call to `Check` (itab load + indirect call, ~3 ns on amd64) plus the rule's own body cost, then one branch on `allowed`.
- On veto: one `fmt.Errorf("%w: <reason>", ...)` allocating the wrapped error — ~2 allocations / ~128 B per veto. At expected veto rates (a few per million) this is invisible; at a sustained high veto rate (a misconfigured rule set rejecting everything) it produces observable GC pressure, which is exactly the operator-visible signal a veto storm should produce.

Per-rule body costs (excluding dispatch):

- `FactorRange.Check` — two float comparisons against `decision.MarkupFactor`; sub-nanosecond body, ~3 ns total with dispatch.
- `AllowedCountries.Check` — slice walk with one string compare per entry against `req.Country`; ~3–5 ns per entry for the typical 2-letter country code, so ~30–50 ns for 10 entries plus dispatch.
- `RequiredFields.Check` — slice walk with a small string switch reading the named field on `req`; ~5–10 ns per field, ~30 ns for 3 fields plus dispatch. The switch is intentional — at N≈3 fields the Go compiler emits a length-bucketed jump table that beats a map lookup, and a precomputed `[]func(Request) bool` would add construction-time allocation without latency gain.

Aggregate per-`Decide` overhead with the three shipped rules at typical configuration sizes: ~100–120 ns, dominated by the `AllowedCountries` body plus the N interface dispatches. Well under the indexed engine's measured 442 ns / `Decide` baseline (`scientific/v0.1.0/REPORT.md`) and well under the metrics decorator's 176 ns delta.

For larger custom rule lists: at N≈20 rules the dispatch + body sum approaches ~500 ns, which is ~quarter of the indexed engine baseline. Operators wiring deep rule lists should keep this budget in mind and place cheap + high-veto-rate rules first — but that is recipe-level guidance, not a port concern, and lives in the cookbook recipe rather than the ADR.

The scientific harness pre-registers a `BenchmarkDecorator/guardrails-three-rules` bar against the indexed baseline in `scientific/v0.1.3/REPORT.md` before any number is taken, per ADR-0012's two-commit pre-registration protocol. The bar lands in the same release window as this ADR.

When no flags are set, `guardrails.New` is not called and the decorator is not in the call path. Zero overhead.

### Validation strategy

- `internal/decider/guardrails`: unit tests for the `Decider` and each shipped rule. Cover the four core behaviors:
  - `TestGuardrailsPassThroughAllowedDecision` — every rule returns true → Decision returned unchanged.
  - `TestGuardrailsVetoSurfacesAsErrGuardrailViolation` — one rule returns false → zero Decision + wrapped sentinel error with reason.
  - `TestGuardrailsPassThroughInnerError` — inner returns `ErrNoMatch` → error propagated unchanged; rules NOT consulted (no point checking guardrails on a Decision that does not exist).
  - `TestGuardrailsRunsRulesInOrderUntilFirstVeto` — first vetoing rule wins; subsequent rules NOT consulted (avoids redundant work and gives operators a deterministic reason).
- Per-rule tests pin the boundary conditions (`FactorRange` at Min, at Max, just below Min, just above Max; `AllowedCountries` with empty list = vetoes everything, with a single entry, with multiple; `RequiredFields` with a missing field, with all present).
- A `TestComposesWithMetricsAndOtel` confirms `metrics.Wrap(otel.Wrap(guardrails.New(stub, FactorRange{Min: 1.0, Max: 2.0})))` records the veto as `Err` in the metric event AND `codes.Error` on the span attribute.
- A `cmd/markup-server` e2e test sets `--max-factor=1.0` and serves a CSV whose only rule returns factor 1.15 — `/decide` returns `500` with the opaque error body, the operator-stderr log carries the reason, the previous `--max-factor=10.0` configuration (which would have allowed the 1.15) serves the same request as `200`. The asymmetry is the proof.
