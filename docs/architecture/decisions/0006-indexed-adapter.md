# 6. Indexed Decider Adapter

## Status

Proposed — proposes `internal/decider/indexed` as the fourth concrete `markup.Decider` adapter, wrapping bre-go's `engine/indexed.Engine`. Semantics match firstmatch (insertion-order precedence, first match wins) but cost model differs: O(K) hash lookups per `Decide` instead of O(rules) linear scan.

## Context

ADR-0001 fixed the `Decider` port. ADRs 0004 and 0005 shipped firstmatch and priority — three adapters in total, three distinct semantics, all linear-scan in the per-`Decide` cost dimension. At small rule-set sizes that's invisible. At large rule sets the linear walk through every `parser.Condition` per request dominates per-`Decide` latency, and the gains from `parser.StringCondition`'s allocation-free `Eval` are eaten by the `O(rules)` factor.

bre-go ships `engine/indexed.Engine` to address exactly this case. The adapter:

1. Inspects each rule's `parser.Condition` at construction time, extracts the (field, equality-value) pairs that bound the rule, and buckets the rule by `(key-set, value-tuple)`.
2. Builds an immutable snapshot at `Build()` time. Subsequent `Execute` reads lock-free from the snapshot via an atomic pointer.
3. On `Execute`, projects the input fact map onto each key-set, hashes for the value tuple, and walks only the rules that bucket-match. Non-indexable terms (negations, ranges) remain as post-filters that run on the bucket's candidate set.

For a rule set where every rule is `country == 'X'` with `X` varying, the bucket lookup is `O(1)` for the relevant country. Whatever matches in firstmatch's `Decide` after a linear scan, indexed finds in a hash lookup plus the matched-rule post-filter walk.

Three design questions.

### 1. Rule shape on the markup side

bre-go's `indexed.Rule.Match` is `parser.Condition` directly — a typed AST node — not a closure of `func(input interface{}) bool` like inmemory / firstmatch / priority. That is because the indexer needs to *inspect* the condition tree to decide which terms are indexable. A closure is opaque.

This is genuinely cleaner for the markup-svc side, not a regression:

- `load.Rule.Condition` is already a `parser.Condition` (compiled at CSV load time, ADR-0002).
- The other adapters wrap that `parser.Condition` in a closure via `markup.FactOf`. The indexed adapter consumes it directly.
- `markup.FactOf` is still needed, but it moves from the rule-construction site (where the other adapters use it) to the `Decide` call site, where the engine wants a `map[string]interface{}` input.

So the markup-side rule for indexed becomes:

```go
type Rule struct {
    Name   string
    Match  parser.Condition
    Factor float64
}
```

This is a *different* `Rule` from `inmemory.Rule` / `firstmatch.Rule` / `priority.Rule`. That is fine — each adapter's `Rule` reflects what its engine actually consumes. ISP (Interface Segregation) over false symmetry.

### 2. What about non-indexable conditions?

`indexed.Engine.AddRule` returns `ErrNonIndexableCondition`, `ErrNoIndexableTerms`, or `FanoutTooLargeError` for rule shapes the indexer cannot represent. The CSV rule format we ship today (ADR-0002) uses `==`, `!=`, `IN`, `NOT IN`, `AND`, `OR`, `NOT`. Of those:

- `==` and `IN` are indexable directly.
- `!=` and `NOT IN` become post-filters on bucket candidates.
- `AND` of indexables is one bucket; `AND` of one indexable + one negation indexes by the positive term and post-filters the negation.
- A rule whose `Match` is entirely negations (e.g., `country != 'XX'` with no other clause) has no indexable term and returns `ErrNoIndexableTerms`.

We surface those errors at `NewFromRules` time so a CSV that does not fit the adapter fails boot fast, mirroring the inmemory / firstmatch / priority adapter error-propagation pattern.

### 3. Build phase

`indexed.Engine` requires `Build()` before `Execute` returns indexed results. `NewFromRules` calls `Build()` explicitly so any seal-time error (currently none beyond the AddRule errors already surfaced) appears at construction. The alternative — relying on `Execute`'s implicit Build on first call — would defer the failure mode to a request, which violates the fail-fast posture every other adapter follows.

## Decision

`internal/decider/indexed` ships:

- `Rule{Name string, Match parser.Condition, Factor float64}` — the typed rule shape this adapter takes
- `Decider` struct holding an embedded `*indexed.Engine` and the model version
- `New(rules []Rule, modelVersion string) (*Decider, error)` — typed constructor; calls `Build()` before returning
- `NewFromRules(rules []load.Rule, modelVersion string) (*Decider, error)` — production bridge; each `load.Rule.Condition` becomes `Rule.Match` directly with no closure wrapping
- `Decide(ctx context.Context, req markup.Request) (markup.Decision, error)` — implements `markup.Decider`. Passes `markup.FactOf(req)` as the engine input (the indexed engine wants a fact map, not a closure-wrapped request)

Decision provenance:

- `Rule` = `res.Matched[0]` (first match wins, same as firstmatch)
- `MarkupFactor` = factor of the matched rule
- `EngineAdapter` = `"*indexed.Engine"`
- `ModelVersion`, `CorrelationID` — same conventions as the other adapters

On miss, `Decide` returns `markup.ErrNoMatch`.

## Consequences

### Closed by this ADR

- A fourth concrete adapter exists, sharing semantic with firstmatch (first match wins, insertion-order precedence) but with sub-linear lookup cost.
- `cmd/markup-server --adapter=indexed` becomes available alongside the existing three options.
- The `Rule` shape diverges from the other adapters (`Match parser.Condition` vs `Condition func(markup.Request) bool`). The divergence is intentional and ADR-0006 documents the rationale: the indexed engine inspects the condition tree, so an opaque closure would defeat the indexer.

### NOT closed by this ADR

- Compiled snapshot persistence. bre-go's indexed adapter ships snapshot v1 and v2 formats; consuming them is out of scope here and tracked separately.
- Adaptive routing (run firstmatch for small rule sets, indexed for large ones). Decision routing across adapters is out of scope.
- Hot reload. Out of scope here.
- Indexable-condition diagnostics in the CSV loader. A non-indexable rule fails at `NewFromRules` time; surfacing the failure at CSV-load with column references is a usability follow-up.

### Performance impact

Per-`Decide` allocation profile differs from firstmatch in two ways:

- `markup.FactOf` map is built once per `Decide` (same as other adapters) but it is *also* what the engine consumes directly as `req.Input` — no closure-side allocation per rule.
- The indexed engine allocates the matched-slice (`[]string{r.Name}`) on a hit and nothing on a miss (verified in `engine/indexed/indexed.go:307`).
- No per-`Decide` sort. The sort happens once during `Build()` at startup (key-set ordering); after that, every `Execute` is a sequence of hash lookups followed by post-filter `Eval` walks on bucket candidates only.

CPU profile:

- Startup cost is `O(rules + sum of bucket fanouts)` for `AddRule` + `O(K)` for `Build()` where K is the number of distinct key-sets. Realistic rule sets keep K small (the number of distinct field combinations the CSV mentions).
- Per-`Decide` cost is `O(K)` bucket projections + a hash lookup per bucket + `O(matching candidates)` post-filter `Eval` walk. For a rule set where every rule discriminates by `country`, K=1, and per-`Decide` is one hash lookup plus the post-filter pass on the country-specific bucket.
- The win over firstmatch grows with rule-set size. For 10 rules over 1 field, the difference is microseconds; for 10,000 rules over a handful of fields, indexed is asymptotically faster by orders of magnitude.

Allocation profile improves vs firstmatch for the hit case: the `parser.Condition.Eval` walk runs on far fewer rules.

### Validation strategy

- Unit tests covering `New`, `NewFromRules`, `AddRule` error propagation (empty name, nil Match, duplicate name, non-indexable Match), `Decide` happy path, `ErrNoMatch` on miss, correlation ID flow.
- A `TestSemanticEquivalenceWithFirstmatch` integration test: for the same CSV with only indexable conditions, `indexed.Decider` returns the same `Decision.Rule` and `MarkupFactor` as `firstmatch.Decider` on every Request in a representative matrix. Without this guarantee the indexed adapter would silently disagree with the well-understood firstmatch baseline.
- A `BenchmarkDecide*` mirroring the other adapter benchmarks; ideally with a moderately large rule-set fixture so the sub-linear behaviour is visible (vs the toy 2-3 rule fixtures the other adapters use).
- A `TestNewFromRulesRejectsNonIndexableCondition` test pinning the fail-fast posture: a load.Rule whose Condition is pure negation surfaces an error at `NewFromRules` rather than at `Decide`.
