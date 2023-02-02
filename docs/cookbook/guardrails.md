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

The supported field names are the seven string fields on the request body: `product_id`, `category`, `customer_tier`, `channel`, `country`, `inventory`, `time_window`. `Amount` (numeric) is intentionally not on the list — a zero `Amount` is a legal request. An unknown field name in `--required-fields` does NOT fail boot silently; it vetoes every request at runtime with `unknown required field "..."` in the operator log, so a typo surfaces loudly on the first `/decide` call rather than waiting for an actual missing-value request.

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

Rules are evaluated in the order `FactorRange → AllowedCountries → RequiredFields` and short-circuit on the first veto. The order is fixed by the binary — there is no flag to reorder — and reflects that misconfigured factors are the most common veto cause in operator-reported incidents, so the cheapest check fires first.

## Recipe — update the active rules without restart

If a misbehaving rule set produces a Decision that needs to be vetoed *now* and waiting for a rolling restart isn't an option, enable the admin endpoint at boot:

```sh
./markup-server \
  --rules=/etc/markup/rules.csv \
  --max-factor=3.0 \
  --guardrails-admin
```

`--guardrails-admin` mounts two endpoints on the same listener as `/decide`:

- `GET /admin/guardrails` returns the configuration currently being enforced as JSON.
- `POST /admin/guardrails` accepts a full configuration body and replaces the live rules atomically. In-flight `/decide` calls finish on the previous rule set; new calls land on the replacement.

The classic 2-AM tightening:

```sh
curl http://markup:8080/admin/guardrails
# → {"factor_range":{"min":0,"max":3}}

curl -X POST http://markup:8080/admin/guardrails \
  -H 'Content-Type: application/json' \
  -d '{"factor_range":{"min":0,"max":1.5},"allowed_countries":["BR","DE","FR"]}'
# → 200 OK with the new state echoed back

curl http://markup:8080/admin/guardrails
# → {"factor_range":{"min":0,"max":1.5},"allowed_countries":["BR","DE","FR"]}
```

The endpoint follows set-the-full-config semantics: omitting `factor_range` from the body means "no FactorRange rule active", not "keep the current value". Operators that want partial updates `GET` first, edit the result locally, and `POST` the full intended state back.

When `--guardrails-admin` is NOT set, the immutable boot-time decorator from `--min-factor` / `--max-factor` / `--allowed-countries` / `--required-fields` is used and the admin endpoint is `404` — the operator opt-in matters because the Holder adds a ~10 ns lock-pair to every `Decide`.

## What's happening

The guardrails decorator sits inside the OTel tracing layer and outside the swap holder / router (see [ADR-0014](../architecture/decisions/0014-guardrails.md) for the composition decision). The call chain on a vetoed request is:

```
otel → guardrails → swap/router → engine
```

The engine runs first and returns a Decision. The guardrails decorator runs each configured `Rule.Check` in order; the first to return `(allowed=false, reason=...)` short-circuits. The decorator returns the zero Decision and an error wrapping `guardrails.ErrGuardrailViolation` with the rule's reason string. The OTel span records `codes.Error` with the wrapped message; the metrics decorator (if wired via a wrapper main per the observability recipe) classifies the event as `Err` with the wrapped error on `DecisionMetric.Err`. The HTTP handler returns 500.

When no guardrail flag is set on the command line, `guardrails.New` is never called and the decorator is not in the call path — zero per-`Decide` overhead. Detection uses `flag.FlagSet.Visit`, so `--max-factor=0` is treated as an explicit (degenerate) operator choice rather than "default left at zero".

`--guardrails-admin` swaps the immutable decorator for a `guardrails.Holder` (see [ADR-0015](../architecture/decisions/0015-guardrails-hot-reload.md)). The Holder owns a `sync.RWMutex`-protected rule slice; `Decide` does one uncontended `RLock`/`RUnlock` pair to capture the slice header before walking the rules, the same minimum-lock-hold pattern `swap.Decider` uses for the engine. `Replace` allocates a new backing array and assigns the new slice header under the write lock; in-flight `Decide` calls walk the array they already captured. The admin POST validates the body via the same `guardrails.BuildRules` the boot flags use, so the two surfaces accept and reject identical configurations.

## What to check after

- A request that previously returned 200 with `markup_factor: 1.15` now returns 500 when you boot with `--max-factor=1.0`. The operator-stderr log shows the rejection reason for the offending Decision.
- Booting the same command with `--max-factor=2.0` against the same request returns 200 with `markup_factor: 1.15`. The asymmetry confirms the flag is wired and doing real work.
- With `--otel-enabled` + an OTLP collector: vetoed requests appear in your tracing UI as spans with `status.code = ERROR` and a `status.message` containing "guardrails: decision vetoed".
- With a custom metrics wrapper main: the metric event for a vetoed request has `Err != nil` and `errors.Is(Err, guardrails.ErrGuardrailViolation) == true`. Dashboards built on the metrics pipeline can show a veto-rate panel by filtering the error series on the sentinel.
- A request whose engine returns `ErrNoMatch` is NOT classified as a guardrail violation — the no-match propagates as 404 unchanged, and the metric event has `NoMatch=true` with `Err=nil`. The Err-rate dashboard is not poisoned with no-match traffic.
- With `--guardrails-admin`: `GET /admin/guardrails` returns the live config so an operator can confirm a `POST` took effect without waiting for the next `/decide`. A `POST` with a malformed body (missing field, inverted factor interval, unknown JSON key) returns `400` with an opaque body and the previous rule set keeps serving — the response shape is the same as a successful POST but with the unchanged config echoed back via the next `GET`.

## Mistakes to avoid

- **Passing an empty allowlist value (`--allowed-countries=`)**: the binary refuses to boot rather than silently treating it as veto-all. If you do not want a country restriction, omit the flag entirely.
- **Inverting the factor interval (`--min-factor=2.0 --max-factor=1.0`)**: the binary refuses to boot with `--min-factor (2) must not exceed --max-factor (1)`. Order the bounds before bumping the deployment.
- **Confusing case in country codes**: comparisons are case-sensitive. If your upstream sends lowercase ISO codes, normalize before the request reaches `/decide`, or write a custom `Rule` that lower-cases before comparing.
- **Putting an expensive custom Rule first when its veto rate is low**: every Decide pays the cost of every Rule until the first veto. Order rules cheapest-first if you have a custom set; a misconfigured factor rule belongs before an expensive regulatory-bounds lookup.
- **Treating a vetoed Decision as a no-match**: the metrics + OTel decorators classify vetoes as `Err`, not `NoMatch`. A regression that conflated the two would inflate either dashboard with the wrong signal; the composition tests in `internal/decider/guardrails/composition_test.go` are the regression guard.
- **Using `--min-factor=0 --max-factor=0`**: this is a documented degenerate case that vetoes every nonzero factor and permits exactly zero. Set the flags to the actual interval you want, or omit them.
- **Treating `POST /admin/guardrails` as a patch**: the body is the full intended state, not a partial update. Omitting `factor_range` means "no FactorRange rule active", not "keep the existing one". Operators that want to keep one axis and change another `GET` first, edit locally, then `POST`.
- **Exposing the admin endpoint to untrusted callers**: there is no authentication on `/admin/guardrails` (same posture as `/admin/reload`). Gate the admin port via Kubernetes NetworkPolicy, a sidecar, or a separate listen address before the binary sees user traffic.
- **Running an old binary against a new request body**: the POST decoder uses `DisallowUnknownFields` so a body carrying a key the binary does not recognize fails 400. This is intentional — schema additions are a forward-only break so operators surface the version mismatch loudly instead of silently losing intent.

## Relevant ADRs and flags

- [ADR-0014](../architecture/decisions/0014-guardrails.md) — design and composition of the guardrails decorator.
- [ADR-0015](../architecture/decisions/0015-guardrails-hot-reload.md) — Holder + admin endpoint for hot replacing the rule set without restart.
- [ADR-0009](../architecture/decisions/0009-otel-spans.md) — OTel span behavior on errors; vetoes record as `codes.Error`.
- [ADR-0010](../architecture/decisions/0010-metrics-port.md) — metrics outcome classification; vetoes are `Err`, not `NoMatch` or `Canceled`.
- Flags: `--min-factor`, `--max-factor`, `--allowed-countries`, `--required-fields`, `--guardrails-admin`.
