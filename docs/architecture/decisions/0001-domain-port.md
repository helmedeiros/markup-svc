# 1. Domain Port: Decider Interface for Markup Decisions

## Status

Proposed — defines the port through which the markup service makes pricing decisions. One-method interface; concrete adapters wrap whichever underlying engine fits the workload.

## Context

The markup service exists to make dynamic markup decisions given a typed request: which product, which customer tier, which channel, which country, which inventory state, which time window, what base amount. Real production markup logic varies across all of those dimensions and the rule set evolves frequently.

The service is built on top of `github.com/helmedeiros/bre-go`, a Go business rule engine library that ships four engine adapters (inmemory, firstmatch, priority, indexed). Each adapter makes different latency and semantic tradeoffs. We want to be able to:

- Pick the adapter that fits the workload at startup (configuration).
- A/B between adapters in production (experiment routing).
- Swap models (rule sets) without changing the service code.

A monolithic service that calls a concrete bre-go engine directly couples the HTTP layer to that engine's internal types. Adding a new adapter would require touching every caller.

The hexagonal pattern bre-go itself uses (`engine.Engine` port + multiple adapters) is the right shape here too: the markup service exposes a `Decider` port to its callers; concrete deciders wrap whichever bre-go engine fits the workload.

Three design questions.

### 1. What's the port surface?

Smallest viable interface:

```go
type Decider interface {
    Decide(ctx context.Context, req Request) (Decision, error)
}
```

Single method, ctx-aware. The ctx flows through to the inner bre-go engine (which has been ctx-aware since its v0.2.0) and through to whatever the rule's action does (DB lookup, audit emission, metric tag).

### 2. What's in Request?

The fields that pricing rules actually look at. Real production markup decisions care about:

- Product / category identity
- Customer tier (consumer / smb / enterprise / platform)
- Channel (web / mobile / api / store / partner)
- Country
- Inventory state (in_stock / low / oversupply)
- Time window (peak / off / holiday)
- Base amount (the price we are marking up)

Keep them all as strings except `Amount` (float64) so rule conditions written in bre-go's expression DSL work uniformly through `parser.StringCondition`, `parser.SetCondition`, and `parser.RangeCondition`.

### 3. What's in Decision?

The factor itself plus every piece of provenance an operator needs to explain "why this markup":

- `MarkupFactor` (float64; e.g., 1.15 for +15%)
- `Rule` (the named rule that fired)
- `ModelVersion` (the rule set / snapshot that served the request)
- `Experiment` (`"A"`, `"B"`, `"control"`, or `""` for no experiment)
- `CorrelationID` (request-scoped identity; ties the decision back to logs and traces)
- `EngineAdapter` (concrete engine type name; lets observability slice by adapter)

`ModelVersion` + `Experiment` + `EngineAdapter` together let dashboards slice decisions by (rule set × variant × engine) — which is the question operators actually ask in incident reviews.

## Decision

`internal/markup` ships:

```go
type Request struct {
    ProductID    string
    Category     string
    CustomerTier string
    Channel      string
    Country      string
    Inventory    string
    TimeWindow   string
    Amount       float64
}

type Decision struct {
    MarkupFactor  float64
    Rule          string
    ModelVersion  string
    Experiment    string
    CorrelationID string
    EngineAdapter string
}

type Decider interface {
    Decide(ctx context.Context, req Request) (Decision, error)
}

var ErrNoMatch = errors.New("markup: no rule matched the request")
```

`ErrNoMatch` is the "soft default" — when no rule fires, the caller decides whether to apply a fallback or reject the request. The Decider signature returns `(Decision, error)`; on `ErrNoMatch` the returned `Decision` is zero-valued.

## Consequences

### Closed by this ADR

- The port that every adapter (`inmemoryDecider`, `firstmatchDecider`, `priorityDecider`, `indexedDecider`) will implement is fixed in shape.
- HTTP handlers in `cmd/markup-server` depend only on `Decider`, not on any specific bre-go engine.
- The Decision type carries enough provenance for observability slicing across (rule, model version, experiment, adapter) without parallel APIs.

### NOT closed by this ADR

- The concrete adapter implementations. Each adapter gets its own ADR when it lands.
- Rule format. A separate ADR documents the chosen rule format.
- HTTP wire shape. The endpoint contract is its own ADR when the HTTP layer is implemented.
- Experiment routing and the model registry. Separate ADRs once they exist.
- The performance comparison harness. Belongs in a `scientific/v0.1.0/` directory once we have a Decider implementation to measure.

### Performance impact

Zero. The port is a one-method interface; calling through it costs one indirect function call per `Decide`. Per-call cost is dominated by whatever the adapter does inside.

### Validation strategy

- `internal/markup` ships with table-driven tests against the typed structs (zero-value behavior, equality semantics).
- A minimal `nilDecider` (always returns `ErrNoMatch`) lives in the test file. Confirms the Decider contract works end-to-end before any real adapter exists.
- The package is at 100% coverage from day one; future adapters land in sibling packages and keep their own coverage gates.
