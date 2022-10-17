# 4. First-Match Decider Adapter

## Status

Proposed — proposes `internal/decider/firstmatch` as a second concrete `markup.Decider` adapter, wrapping bre-go's `engine/firstmatch.Engine`. Semantics: the first matching rule in insertion order fires and no others are evaluated.

## Context

ADR-0001 fixed the `Decider` port. ADR-0002 introduced the CSV rule format consumed by every adapter. ADR-0003 specified the HTTP transport that calls the active Decider.

The first concrete adapter (`internal/decider/inmemory`, ADR-0001) follows bre-go's inmemory engine semantics: every matching rule's action runs, and the last action's output wins. That fits some use cases — audit-style accumulation, layered overrides where the last rule deliberately stomps earlier ones — but it is not the natural model for most markup decisions. In production markup rule sets, operators typically want precedence: the most specific (or highest-priority-by-listing-order) rule fires and the rest are skipped. Last-wins forces operators to think about ordering in reverse.

bre-go ships `engine/firstmatch.Engine` exactly for the precedence-by-insertion-order use case: rules evaluate in registration order, the first matching rule's action returns, and the remaining rules are not evaluated.

Two design questions.

### 1. Replace the inmemory adapter, or ship as a peer?

The same rule set evaluated by inmemory vs firstmatch can give different answers (last-wins vs first-wins). Both are legitimate semantics; the choice is the operator's. Replacing inmemory would silently change the answer for any installation that relied on its accumulating behaviour. Shipping firstmatch as a peer keeps inmemory available and exposes the choice via configuration (covered separately when the adapter-selection mechanism lands).

### 2. Same `Rule` shape on the markup side?

Yes. `firstmatch.Rule` and `inmemory.Rule` in bre-go have the same surface modulo bookkeeping fields. The markup-side `firstmatch.Rule` mirrors `inmemory.Rule`:

```go
type Rule struct {
    Name      string
    Condition func(markup.Request) bool
    Factor    float64
}
```

`load.Rule` (the loader-side rule shape from ADR-0002) is unchanged: the same CSV file feeds both adapters. `load.Rule.Priority` continues to be ignored at this point — the priority adapter is the one that reads it.

## Decision

`internal/decider/firstmatch` ships:

- `Rule{Name string, Condition func(markup.Request) bool, Factor float64}` — same typed shape as `inmemory.Rule`
- `Decider` struct holding an embedded `*firstmatch.Engine` and the model version
- `New(rules []Rule, modelVersion string) (*Decider, error)` — typed constructor used by unit tests
- `NewFromRules(rules []load.Rule, modelVersion string) (*Decider, error)` — production load-time bridge, identical to inmemory's pattern (wrap pre-compiled `parser.Condition` via `markup.FactOf`)
- `Decide(ctx context.Context, req markup.Request) (markup.Decision, error)` — implements `markup.Decider`

Decision provenance:

- `Rule` = `res.Matched[0]` (the single first-match result)
- `MarkupFactor` = factor of the matched rule
- `ModelVersion` = constructor-supplied tag
- `CorrelationID` = `engine.CorrelationIDFromContext(ctx)`
- `EngineAdapter` = `"*firstmatch.Engine"`

On miss (`len(res.Matched) == 0`), Decide returns `markup.ErrNoMatch` exactly like the inmemory adapter.

## Consequences

### Closed by this ADR

- A second adapter exists. CSV rule order now has a different meaning under firstmatch (precedence) than under inmemory (most-recent-wins).
- `EngineAdapter` on `Decision` now varies meaningfully across deployments; observability dashboards that slice by adapter become useful.
- The `(rules × adapter × model_version × experiment)` matrix the project promised at ADR-0001 gains its first non-trivial axis.

### NOT closed by this ADR

- Adapter-selection mechanism in `cmd/markup-server`. Out of scope here; tracked separately under the `--adapter` flag work.
- The priority adapter. Separate ADR.
- The indexed adapter. Separate ADR.
- Mid-flight adapter swap. Out of scope; an active Decider is constructed once at startup and is immutable.

### Performance impact

Per-`Decide` allocation profile is identical to inmemory: one `markup.FactOf` map, one Decision return, the bre-go engine's own matched-slice (now at most one element instead of potentially many). The CPU profile differs by rule-set shape:

- When the matching rule sits early in the CSV, firstmatch is strictly faster: the scan stops at the first hit, so per-`Decide` cost is `O(rules scanned)` rather than `O(rules total)`.
- When no rule matches, both adapters walk the full rule set.
- When the operator deliberately orders rules from most-specific to most-general, firstmatch's worst case approaches its best case for hits.

No change to startup cost: `NewFromRules` is `O(rules)` regardless of adapter.

### Validation strategy

- Unit tests covering `New`, `NewFromRules`, `AddRule` error propagation (empty name, duplicate name), first-match-wins behaviour, and `ErrNoMatch` on miss.
- One pinned test asserting the semantic difference: the same `[]load.Rule` produces a different `Decision.Rule` (and possibly `MarkupFactor`) through `firstmatch.Decider` vs `inmemory.Decider`. This is the test that pins "yes, the adapter choice is observable", which is the whole point of the ADR.
- A `Benchmark*` for `Decide` mirroring the inmemory benchmark so per-adapter cost differences surface in CI.
