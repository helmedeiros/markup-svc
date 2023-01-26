# Veto Decisions outside a safety envelope

## Problem

Your engine will faithfully serve whatever Decision a CSV rule produces — including a typo (`14.0` instead of `1.40`), a stray rule from a partially-edited hot reload, or an A/B variant that experiments with an unbounded factor. You want a layer between the engine and the wire that says "no, this number is not allowed to leave the building" and surfaces the rejection as an explicit error rather than serving the bad Decision.

## Recipe — factor range

Enable the factor guardrail at boot:

```sh
./markup-server \
  --rules=/etc/markup/rules.csv \
  --min-factor=0.5 \
  --max-factor=3.0
```

Every Decision whose `MarkupFactor` falls outside `[0.5, 3.0]` is rejected by the guardrails decorator before the response leaves the server. `POST /decide` returns `500 Internal Server Error` with an opaque body; the operator-stderr log carries the reason (`factor 14.00 above max 3.00`) so you can pinpoint which rule misbehaved.

The interval is closed: `MarkupFactor == 0.5` and `MarkupFactor == 3.0` are allowed; `0.49` and `3.01` are not.

## Recipe — country allowlist

Pin the set of countries the service is meant to serve:

```sh
./markup-server \
  --rules=/etc/markup/rules.csv \
  --allowed-countries=BR,DE,FR
```

When a `/decide` request arrives with `country` outside `{BR, DE, FR}`, the request engine still runs (a rule may match on `customer_tier` alone and produce a Decision), but the guardrails decorator vetoes the Decision before it is serialized. Comparison is case-sensitive: an upstream caller passing `"br"` instead of `"BR"` gets vetoed loudly rather than silently bypassing the allowlist.

Empty `--allowed-countries=` (with no values) fails boot. Omit the flag if you don't want a country restriction.

## Recipe — required request fields

Catch upstreams that forgot to pass a field your CSV rules rely on:

```sh
./markup-server \
  --rules=/etc/markup/rules.csv \
  --required-fields=customer_tier,country
```

Without this guardrail, a request with `{"customer_tier":""}` against a rule set that depends on `customer_tier` produces a 404 no-match. With the guardrail, the same request gets a 500 with `required field "customer_tier" is empty` in the operator log. The asymmetry tells the operator "your upstream broke" instead of "your rules don't cover this case."

The supported field names are the seven string fields on the request body: `product_id`, `category`, `customer_tier`, `channel`, `country`, `inventory`, `time_window`. `Amount` (numeric) is intentionally not on the list — a zero `Amount` is a legal request.

## Recipe — composing all three

Flags compose. Use as many as the deployment needs:

```sh
./markup-server \
  --rules=/etc/markup/rules.csv \
  --min-factor=0.5 \
  --max-factor=3.0 \
  --allowed-countries=BR,DE,FR \
  --required-fields=customer_tier,country
```

Rules are evaluated in the order `FactorRange → AllowedCountries → RequiredFields` and short-circuit on the first veto. Place the cheapest + highest-veto-rate rule first; the default ordering reflects that misconfigured factors are the most common veto cause in operator-reported incidents.

## What's happening

The guardrails decorator sits inside the OTel tracing layer and outside the swap holder / router (see [ADR-0014](../architecture/decisions/0014-guardrails.md) for the composition decision). The call chain on a vetoed request is:

```
otel → guardrails → swap/router → engine
```

The engine runs first and returns a Decision. The guardrails decorator runs each configured `Rule.Check` in order; the first to return `(allowed=false, reason=...)` short-circuits. The decorator returns the zero Decision and an error wrapping `guardrails.ErrGuardrailViolation` with the rule's reason string. The OTel span records `codes.Error` with the wrapped message; the metrics decorator (if wired via a wrapper main per the observability recipe) classifies the event as `Err` with the wrapped error on `DecisionMetric.Err`. The HTTP handler returns 500.

When no guardrail flag is set on the command line, `guardrails.New` is never called and the decorator is not in the call path — zero per-`Decide` overhead. Detection uses `flag.FlagSet.Visit`, so `--max-factor=0` is treated as an explicit (degenerate) operator choice rather than "default left at zero".

## What to check after

- A request that previously returned 200 with `markup_factor: 1.15` now returns 500 when you boot with `--max-factor=1.0`. The operator-stderr log shows the rejection reason for the offending Decision.
- Booting the same command with `--max-factor=2.0` against the same request returns 200 with `markup_factor: 1.15`. The asymmetry confirms the flag is wired and doing real work.
- With `--otel-enabled` + an OTLP collector: vetoed requests appear in your tracing UI as spans with `status.code = ERROR` and a `status.message` containing "guardrails: decision vetoed".
- With a custom metrics wrapper main: the metric event for a vetoed request has `Err != nil` and `errors.Is(Err, guardrails.ErrGuardrailViolation) == true`. Dashboards built on the metrics pipeline can show a veto-rate panel by filtering the error series on the sentinel.
- A request whose engine returns `ErrNoMatch` is NOT classified as a guardrail violation — the no-match propagates as 404 unchanged, and the metric event has `NoMatch=true` with `Err=nil`. The Err-rate dashboard is not poisoned with no-match traffic.

## Mistakes to avoid

- **Passing an empty allowlist value (`--allowed-countries=`)**: the binary refuses to boot rather than silently treating it as veto-all. If you do not want a country restriction, omit the flag entirely.
- **Confusing case in country codes**: comparisons are case-sensitive. If your upstream sends lowercase ISO codes, normalize before the request reaches `/decide`, or write a custom `Rule` that lower-cases before comparing.
- **Putting an expensive custom Rule first when its veto rate is low**: every Decide pays the cost of every Rule until the first veto. Order rules cheapest-first if you have a custom set; a misconfigured factor rule belongs before an expensive regulatory-bounds lookup.
- **Treating a vetoed Decision as a no-match**: the metrics + OTel decorators classify vetoes as `Err`, not `NoMatch`. A regression that conflated the two would inflate either dashboard with the wrong signal; the composition tests in `internal/decider/guardrails/composition_test.go` are the regression guard.
- **Using `--min-factor=0 --max-factor=0`**: this is a documented degenerate case that vetoes every nonzero factor and permits exactly zero. Set the flags to the actual interval you want, or omit them.

## Relevant ADRs and flags

- [ADR-0014](../architecture/decisions/0014-guardrails.md) — design and composition of the guardrails decorator.
- [ADR-0009](../architecture/decisions/0009-otel-spans.md) — OTel span behavior on errors; vetoes record as `codes.Error`.
- [ADR-0010](../architecture/decisions/0010-metrics-port.md) — metrics outcome classification; vetoes are `Err`, not `NoMatch` or `Canceled`.
- Flags: `--min-factor`, `--max-factor`, `--allowed-countries`, `--required-fields`.
