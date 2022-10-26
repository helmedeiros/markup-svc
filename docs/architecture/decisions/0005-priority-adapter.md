# 5. Priority Decider Adapter

## Status

Accepted — `internal/decider/priority` ships in the same release window. `TestSemanticDifferenceFromFirstmatch` confirms that when Priority disagrees with insertion order the priority adapter picks the higher-priority rule and the firstmatch adapter picks the earlier-inserted rule. `TestPriorityZeroDegradesToFirstmatch` confirms that when all priorities are equal the two adapters return identical Decisions for every Request — so a CSV migrated from firstmatch to priority with `priority = 0` everywhere produces identical behaviour.

## Context

ADR-0004 introduced the firstmatch adapter where rule order in the CSV becomes precedence. That works for small rule sets where a human curates the file by hand, but it falls apart at scale:

- A CSV that mixes "always-on" defaults with conditional overrides has to be ordered such that the overrides come first. The author has to know about every existing default to insert a new override at the right line.
- A CSV that grows by accretion ends up with implicit priority-by-position that nobody documented. A new rule appended at the bottom never fires for any request that an earlier broader rule covers.
- A CSV that combines multiple sources (defaults from one team, overrides from another) cannot be merged without resolving the ordering question across editorial boundaries.

bre-go ships `engine/priority.Engine` for the case where precedence is *explicit data* on each rule. The priority column already exists on `load.Rule` (added in ADR-0002 and left ignored by inmemory and firstmatch). This adapter is the one that reads it.

Three design questions.

### 1. Why priority over insertion order?

Insertion order is implicit and brittle (see above). Priority is explicit: an integer per rule says "I should be evaluated before things with lower numbers." Operators can edit a single rule's priority without touching the rest of the file, and merge tools see explicit precedence instead of position-dependent diffs.

### 2. How do ties break?

bre-go's priority engine breaks priority ties by insertion order: among rules with the same Priority, the first one registered wins. This is exactly the firstmatch semantic, scoped to the equal-priority subset. So the priority adapter degenerates gracefully:

- All priorities equal → equivalent to firstmatch over insertion order.
- All priorities distinct → strict priority ordering, insertion order irrelevant.
- Mixed → priority groups in order, with insertion-order tie-break inside each group.

This means a CSV that was authored for the firstmatch adapter (priority column all 0) produces identical Decisions through the priority adapter. Zero migration friction.

### 3. Rule shape change?

Yes, but only on the markup side. The markup adapter's `Rule` gains a `Priority int` field:

```go
type Rule struct {
    Name      string
    Condition func(markup.Request) bool
    Factor    float64
    Priority  int
}
```

`load.Rule.Priority` (which already exists) is the source. `NewFromRules` reads it through unchanged. The inmemory and firstmatch adapters' Rule shapes stay narrow — they have no use for Priority — which keeps every adapter's surface honest about what it actually consumes.

## Decision

`internal/decider/priority` ships:

- `Rule{Name string, Condition func(markup.Request) bool, Factor float64, Priority int}` — same shape as the other adapters' Rule plus the Priority field
- `Decider` struct holding an embedded `*priority.Engine` and the model version
- `New(rules []Rule, modelVersion string) (*Decider, error)` — typed constructor for unit tests
- `NewFromRules(rules []load.Rule, modelVersion string) (*Decider, error)` — production load-time bridge that finally reads `load.Rule.Priority`
- `Decide(ctx context.Context, req markup.Request) (markup.Decision, error)` — implements `markup.Decider`

Decision provenance:

- `Rule` = `res.Matched[0]` (the highest-priority match)
- `MarkupFactor` = factor of the matched rule
- `EngineAdapter` = `"*priority.Engine"`
- `ModelVersion`, `CorrelationID` — same conventions as the other adapters

On miss, `Decide` returns `markup.ErrNoMatch`.

## Consequences

### Closed by this ADR

- `load.Rule.Priority` finally has a consumer. The CSV column's purpose is no longer documentation only.
- A third concrete adapter exists. `cmd/markup-server --adapter=priority` becomes available alongside the existing `inmemory` and `firstmatch` options.
- Operators get explicit-data precedence as an alternative to position-based precedence — the choice between firstmatch and priority is now a real one, and the observability slice by `engine_adapter` covers three distinct semantics.

### NOT closed by this ADR

- The indexed adapter. Separate ADR.
- Adapter selection in `cmd/markup-server`. Out of scope here; tracked separately under the `--adapter` flag work that already exists.
- Multi-tenant priority pools. Single-tenant only at this point.
- Priority arithmetic (negative priorities, large gaps for insert-between). The integer type allows them; the ADR makes no claim about ergonomics.

### Performance impact

Per-`Decide` allocation profile matches firstmatch and inmemory: one `markup.FactOf` map, one `Decision` return, the bre-go engine's matched-slice (at most one element under priority's first-priority-match-wins semantics, like firstmatch).

CPU profile differs from firstmatch on every Decide call, not at startup:

- bre-go's `priority.Engine.Execute` re-runs `sort.SliceStable` on its internal rule slice each call to produce the descending-priority evaluation order. The stable sort is what guarantees the documented insertion-order tie-break.
- Per-`Decide` cost is therefore O(rules log rules) for the sort, plus the linear walk that returns on the first hit. Firstmatch is strictly O(rules) per `Decide` with no sort.
- For realistic markup rule-set sizes (tens to low thousands) the sort cost is microseconds and dwarfed by the per-rule `parser.Condition.Eval` walk, but it is a real per-call cost that shows up under load.
- Startup cost is unchanged from firstmatch (O(rules) for AddRule). The sort overhead lives entirely on the request path.

Allocation profile is unchanged from firstmatch. The benchmark committed in the validation strategy will surface the sort cost so any regression (or future change in bre-go's implementation) is visible in CI.

### Validation strategy

- Unit tests covering `New`, `NewFromRules`, `AddRule` error propagation (empty name, duplicate name), priority ordering, tie-breaking by insertion order, and `ErrNoMatch`.
- A `TestSemanticDifferenceFromFirstmatch` integration test pinning the load-bearing promise: the same `[]load.Rule` where priority order disagrees with insertion order produces different `Decision.Rule` values through `priority.Decider` vs `firstmatch.Decider`.
- A `TestPriorityZeroDegradesToFirstmatch` test pinning the degradation-without-friction property: when all `Priority` values are equal (e.g., 0), `priority.Decider` produces the same `Decision.Rule` as `firstmatch.Decider` for every Request.
- `Benchmark*` for `Decide` so per-adapter cost differences surface in CI.
