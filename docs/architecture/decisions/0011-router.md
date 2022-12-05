# 11. Router Decorator: A/B Variants and Multi-Model Routing

## Status

Proposed — proposes `internal/decider/router` as a `markup.Decider` decorator that holds N inner Deciders keyed by `(ModelVersion, Variant)` and selects which one serves each `Decide` call via a pluggable `Policy`. The router stamps `Decision.ModelVersion` and `Decision.Experiment` from the chosen route so observability slices by `(adapter, model, experiment)` get the right values, regardless of what the inner Decider returns.

## Context

ADRs 0001-0010 land the four adapters, snapshot persistence, hot reload, and the observability decorators. Every running server today serves traffic through exactly one Decider — one engine, one rule set, one model version. That is sufficient for the first production rollout, but the markup-svc capability matrix has always promised more:

- **A/B experiments** — operations wants to roll a new model out to 10% of traffic and compare its dashboards (no-match rate, p99 latency, average factor) against the control population. Today the only way to do this is to deploy two servers and split traffic at the load-balancer; no in-process answer exists.
- **Multi-model routing** — different request shapes (tenant, channel, country) want different rule sets. A tenant-aware rollout cannot be done with one Decider unless every rule re-encodes the tenant axis, which explodes rule-set cardinality.
- **Sticky variant assignment** — once a request is in variant `B`, every retry of that request (same correlation ID) should also be in `B` so dashboards can compare like-for-like instead of accidentally rolling the dice twice. The HTTP middleware already carries the correlation ID through `engine.WithCorrelationID`; this is the natural carrier.

The `Decision.Experiment` field has been on the Decision type since ADR-0001 and has been threading through OTel attributes and metric events since ADRs 0009-0010 — but no part of the stack writes to it. This ADR is the one that finally does.

Three design questions.

### 1. One decorator or two — variants and model versions as separate concerns?

Two candidates:

- **Two decorators**: `variant.Router(model_v1_decider)` for A/B; `model.Registry(decider_v1, decider_v2)` for multi-model. Compose as `model.Registry → variant.Router → swap → adapter` or vice versa.
- **One decorator**: a single `router.Decider` that holds `[]Route{ModelVersion, Variant, Decider}` and dispatches via one `Policy.Choose(Request, CorrelationID) → Route`. Variants and models are just two label axes on the same route.

One decorator wins because:

1. The mechanism is identical — given a request, pick one of N inner Deciders, stamp the chosen route's labels onto the Decision.
2. Two decorators would compose awkwardly: routing-by-model after routing-by-variant means each model has two variants; routing-by-variant after routing-by-model means each variant has two models. Both shapes work but only one is natural for a given operator, and the wrong order silently produces wrong dashboards.
3. The `Policy` is the only thing that varies: a model-only deployment uses a policy that ignores the variant axis; an A/B-only deployment uses a policy that ignores the model axis; a combined deployment routes on both. One decorator, one composition seam.

### 2. How does the policy decide?

Three behaviours operators ask for:

- **Sticky-by-correlation-ID**: hash the correlation ID into the variant space and pick the matching route. Same request → same variant across retries. Default for A/B experiments.
- **Sticky-by-request-attribute**: hash `Request.ProductID` (or any field) instead of the correlation ID. Useful when retries don't carry the same correlation ID but the request shape is stable.
- **Always-default**: a single named route handles everything. Useful for staged rollouts where the router is wired but only one route is active.

A `Policy` interface with one method covers all three. Implementations are the adapter half of a small hexagonal port:

```go
type Policy interface {
    Choose(ctx context.Context, req markup.Request, routes []Route) (Route, error)
}
```

The router package ships:

- `HashCorrelationPolicy` — sticky-by-correlation-ID, defaults the variant when no correlation ID is in context.
- `DefaultPolicy` — always returns the first route. Useful when the router is wired with one route.
- A custom policy interface so deployment-specific routing logic stays out of markup-svc.

### 3. What does the router do when no route matches?

`Policy.Choose` returns an error — there are no routes, or the request shape disagrees with what the policy expected. The router maps this to `markup.ErrNoMatch`? Or to a separate error?

`ErrNoMatch` is reserved for "the engine evaluated every rule and none fired". A routing failure is different — the request never reached an engine at all. Surfacing it as `ErrNoMatch` would lie to the observability layer about what happened.

The router defines its own sentinel:

```go
var ErrNoRoute = errors.New("router: no route matched the request")
```

Distinct from `ErrNoMatch`. The OTel decorator and metrics decorator (ADRs 0009-0010) classify it as "other error" — `codes.Error` on the span, `Err` set on the metric — because a routing failure IS a server-side problem (misconfigured router, missing default policy, etc.) rather than a domain "no rule fired" outcome.

## Decision

`internal/decider/router` ships:

```go
// Route bundles a Decider with the (ModelVersion, Variant) labels
// the router stamps on every Decision served by that Decider.
type Route struct {
    ModelVersion string  // "v1", "v2", ...
    Variant      string  // "control", "A", "B", or "" for non-A/B routes
    Decider      markup.Decider
}

// Policy picks which Route serves a given Request. Implementations
// are the deployment-specific routing logic; the router package
// ships three: HashCorrelationPolicy (sticky by correlation ID),
// HashFieldPolicy (sticky by Request field), DefaultPolicy (always
// first route).
type Policy interface {
    Choose(ctx context.Context, req markup.Request, routes []Route) (Route, error)
}

// Router implements markup.Decider. Decide picks a Route via the
// policy, dispatches to the route's Decider, and stamps the route's
// ModelVersion + Variant onto the returned Decision so observability
// sees the routing decision instead of whatever the inner Decider
// chose to write.
type Router struct {
    routes []Route
    policy Policy
}

func New(routes []Route, policy Policy) *Router

// ErrNoRoute is returned when Policy.Choose returns an error.
var ErrNoRoute = errors.New("router: no route matched the request")
```

`Decision.Experiment` becomes the canonical "which variant served this" field; `Decision.ModelVersion` the canonical "which model served this" field. Both are stamped by the router post-Decide, so inner Deciders cannot accidentally erase the routing decision.

cmd/markup-server wiring (W10 Fri commit, not this ADR):

```sh
markup-server --route=v1:rules:rules-v1.csv --route=v2:rules:rules-v2.csv --policy=hash-correlation
```

Each `--route` adds a `Route{ModelVersion, Variant}` and points at a CSV or snapshot source. `--policy` picks the policy by name.

## Consequences

### Closed by this ADR

- The router decorator exists and satisfies `markup.Decider` so it composes with `metrics`, `otel`, and `swap` like every other decorator.
- A/B variant routing and multi-model routing share one mechanism. Operators wire the policy that fits their case.
- `Decision.Experiment` and `Decision.ModelVersion` are stamped from the routing decision, not from inner Deciders. Inner Deciders that write to these fields are silently overridden — the router is the source of truth.
- `ErrNoRoute` is distinct from `ErrNoMatch` and classifies as a real error in observability decorators.

### NOT closed by this ADR

- Per-route hot reload. The router holds inner Deciders; if each inner is itself a `swap.Decider` then per-route reload is supported, but the cmd wiring (which routes get their own holder, which share one) is a separate decision tracked under the W11 work.
- Traffic-shifting controls (move 50%→75% of traffic to variant B). The W10 policies are static once the server boots; runtime shifting needs a new policy that consults a knob (file watch, admin endpoint).
- Multi-tenancy routing. The router can route by `Request` fields the tenant lives on, but the marshaling of tenant identity into the request is out of scope.
- Experiment definition language. Operators describe variants via cmd flags today; a richer DSL is its own ADR.
- Policy ABAC (consult an external service to decide the variant). Out of scope; deployments that need it write a custom `Policy`.

### Performance impact

Per-`Decide` overhead from the router:

- One `Policy.Choose` call. `HashCorrelationPolicy` is one `engine.CorrelationIDFromContext` lookup + one FNV-1a hash of the string + one modulo by route count. On Linux/amd64 the hash is sub-microsecond; the modulo and slice index are essentially free.
- One method call to the chosen inner Decider's `Decide`. Same shape as any decorator dispatch.
- The route's `ModelVersion` and `Variant` are stamped onto the returned Decision via two string assignments after the inner call. Constant cost.

Aggregate per-`Decide` overhead independent of policy choice: ~100-150 ns on Linux/amd64. macOS and Windows clock-and-hash costs are within the same order. Against the engine's microsecond-scale work, negligible.

The router does NOT serialize the inner `Decide` calls — different goroutines hitting different routes can run concurrently. The `routes` slice and the `policy` field are read-only after construction; no locking required.

### Validation strategy

- `internal/decider/router`: unit tests covering the load-bearing properties — `TestRouterStampsModelVersionAndVariant` confirms the routing decision overrides whatever the inner Decider writes; `TestRouterPropagatesInnerErrNoMatch` confirms domain misses propagate unchanged; `TestRouterErrNoRouteOnPolicyFailure` confirms the routing error is distinct from `ErrNoMatch`.
- `HashCorrelationPolicy`: `TestStickyAcrossCallsWithSameCorrelationID` confirms same correlation → same variant across N calls; `TestVariantsApproximatelyEvenAcrossManyIDs` confirms two variants split traffic roughly 50/50 over 10,000 hashed IDs.
- Composition: `TestRouterStacksWithMetricsAndOtel` runs `metrics.Wrap(otel.Wrap(router.New([routes], policy)))` and confirms both a metric event AND a span are recorded with the route's ModelVersion + Variant on both signals.
- `cmd/markup-server` integration test (W10 Fri): `--route=v1:rules:rules-a.csv --route=v2:rules:rules-b.csv --policy=hash-correlation` and POST `/decide` with two different correlation IDs returns Decisions stamped with different `model_version` and `experiment` fields. The asymmetry is the proof — without the router, the same Request would yield the same model and same variant.
