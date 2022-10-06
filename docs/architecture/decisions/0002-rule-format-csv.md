# 2. Rule Format: CSV with Parser Expressions

## Status

Accepted — `internal/load.FromCSV` and `internal/decider/inmemory.NewFromRules` ship in the same release window and end-to-end integration tests confirm the round trip (CSV → `[]load.Rule` → `*inmemory.Decider` → `markup.Decision`). The `markup.FactOf` converter is a deliberate, documented bre-go coupling kept on the domain side so the column-to-field mapping is the single source of truth across every adapter.

## Context

ADR-0001 fixed the `Decider` port and shipped the first adapter (`internal/decider/inmemory`) using typed Go closures for `Rule.Condition`. Closures are perfect for tests but unworkable in production: every rule change would require a recompile and a redeploy. Operations needs to edit rule sets without touching code.

Three design questions.

### 1. Which on-disk format?

Candidates considered:

- **CSV with expression strings**: one row per rule; condition is a small expression DSL parsed at load time. Editable in spreadsheets. Native fit for bre-go's `engine/csv.Loader[RC]`.
- **JSON**: more flexible for nested conditions but verbose, awkward for non-engineers to edit, no native tabular tooling.
- **YAML**: human-friendly but indentation-fragile and slow to parse.
- **Embedded Go DSL**: highest type safety, lowest editability — defeats the goal.

CSV first because:

1. The condition language bre-go ships (`field == "value"`, `field IN ("a", "b")`, with `AND` / `OR` / `NOT`, parentheses) maps cleanly to a one-line cell.
2. The bre-go ecosystem already has a generic `csv.Loader[RC]` and an `engine.RuleConfigProvider[RC]` contract. Reusing them keeps the loader surface small.
3. A spreadsheet-shaped rule set is the format ops teams already know how to review and diff.
4. JSON support can land later as a second `RuleConfigProvider` — the `Decider` adapters consume parsed `Rule` records, not raw bytes, so the source format is interchangeable.

### 2. What columns?

The minimum that supports every adapter we plan to ship:

| Column | Type | Used by | Purpose |
|---|---|---|---|
| `name` | string | every adapter | The string carried on `Decision.Rule` when this rule fires. Must be non-empty and unique. |
| `condition` | string (expression) | every adapter | Predicate over `markup.Request` fields. Compiled by `parser.ParseToCondition`. |
| `factor` | float64 | every adapter | Markup multiplier carried on `Decision.MarkupFactor` when this rule fires. |
| `priority` | int (default 0) | priority adapter only | Higher fires first. Ignored by inmemory / firstmatch / indexed. |

Optional columns the priority and indexed adapters don't need yet land in their own ADRs when those adapters ship. Adding columns is backwards-compatible: the CSV reader only requires the named columns it reads.

### 3. How does the condition string land on the engine?

bre-go ships two complementary shapes:

- `parser.ParseToCondition(expr) (Condition, error)` — typed AST (`StringCondition`, `SetCondition`, `AndCondition`, `OrCondition`, `NotCondition`).
- `parser.AsRuleCondition(cond, factOf) func(interface{}) bool` — adapter glue that takes the typed tree plus a `factOf(interface{}) map[string]interface{}` converter and returns the closure every adapter's `Rule.Condition` expects.

The markup service supplies the `factOf` converter once: `markup.FactOf(req Request) map[string]interface{}` ships in `internal/markup`. Every adapter constructor uses the same converter, so the column-to-field mapping is identical regardless of which engine is configured.

Compilation happens once at load time. The compiled `Condition` is reused for every `Decide` call.

## Decision

The on-disk rule format is CSV with a single header row and the columns `name,condition,factor,priority`.

The loader package `internal/load` provides:

```go
type Rule struct {
    Name      string
    Condition parser.Condition
    Factor    float64
    Priority  int
}

func FromCSV(r io.Reader) ([]Rule, error)
```

`FromCSV` skips the header, parses every subsequent row, and returns a `[]load.Rule` with the condition column pre-compiled into a typed `parser.Condition`. Bad rows surface as a `*LoadError` carrying the 1-indexed row number — the loader stops at the first bad row rather than collecting all errors, mirroring the `engine/csv` package's contract.

The factor-conversion helper lives on `internal/markup`:

```go
func FactOf(req Request) map[string]interface{}
```

Each adapter ships a `NewFromRules([]load.Rule, modelVersion string) (*Decider, error)` constructor that:

1. Wraps the `parser.Condition` via `parser.AsRuleCondition(cond, markup.FactOf)` into the `func(interface{}) bool` shape its bre-go engine expects.
2. Reuses its existing typed `New(...)` constructor underneath. The closure-based constructor remains, so unit tests still get the lightweight typed shape.

## Consequences

### Closed by this ADR

- The CSV column set is fixed at `name,condition,factor,priority`.
- The condition expression grammar is whatever `parser.ParseToCondition` accepts. Grammar changes are owned by bre-go upstream.
- Every adapter gets a `NewFromRules` constructor whose source-to-decider mapping is identical.
- The `markup.FactOf` converter is the single source of truth for "which Request field becomes which fact key."

### NOT closed by this ADR

- JSON support. Lives in a separate ADR if and when a use case appears.
- Hot reload semantics. Currently rules load once at startup; reload mechanics are out of scope here.
- The indexed adapter's index column. That adapter has its own ADR; the CSV format can grow a column without breaking older rule sets.
- Header naming or column ordering policy. The reader is column-positional for now; named-column lookup can land later without breaking written rules.
- Validation across rules (uniqueness of name, overlap detection). Diagnostics get an ADR alongside `Diagnose()` integration.
- Numeric comparisons on `Amount`. The parser grammar shipped today is string/set only (`==`, `!=`, `IN`, `NOT IN`); rules that need `Amount > 100` are out of scope until either the grammar extends or a programmatic `parser.RangeCondition` row form lands in its own ADR.

### Performance impact

Compilation happens once at startup. Startup cost is O(rows) in CSV size; a malformed row fails boot fast, which is the right trade for catching bad rules before they touch traffic.

Per-`Decide` cost:

- One closure call per rule + one `Eval` walk over the compiled `parser.Condition` tree. `StringCondition` and `SetCondition` `Eval` paths are allocation-free (verified against `engine/parser/condition.go`): map lookup, type assertion, comparison.
- One `markup.FactOf(req) map[string]interface{}` allocation per `Decide`. The map is short-lived but it is heap pressure that scales with QPS, not with rule count. Acceptable at expected service load; a `sync.Pool` or struct-backed fact representation is a follow-up if benchmarks flag it.
- Linear in rule count for the inmemory adapter. Sub-linear matching is a per-adapter concern owned by the indexed and priority adapter ADRs.

### Validation strategy

- `internal/load` ships with table-driven CSV tests: happy path, missing header, malformed row, bad expression in condition column, non-numeric factor, non-integer priority.
- `internal/decider/inmemory` adds a `NewFromRules` test that round-trips a CSV blob through `load.FromCSV` and `inmemory.NewFromRules` and asserts the resulting Decider returns the right `Decision` for a known `Request`.
- `markup.FactOf` has a table-driven test pinning the column-to-field mapping; a regression here would silently break every adapter.
- `internal/load` and `internal/decider/inmemory` ship `Benchmark*` covering a representative rule set so per-`Decide` allocation and latency regressions surface in CI.
