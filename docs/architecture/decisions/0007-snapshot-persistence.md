# 7. Snapshot Persistence for the Indexed Adapter

## Status

Accepted — `internal/snapshot` ships in the same release window. `TestLoadIntoIndexedDeciderEquivalentToNewFromRules` confirms a Decider built from a snapshot produces identical Decisions to one built directly from rules across a 5-Request matrix. `TestLoadIntoIndexedDeciderRejectsMissingFactor` confirms the fail-fast posture: a snapshot whose `Factors` map omits a rule listed in the engine snapshot surfaces as `ErrMissingFactor` rather than a silent zero-factor Decision.

## Context

ADR-0006 shipped the indexed adapter — sub-linear per-`Decide` cost, paid for by an `O(rules + bucket fanout)` startup walk through every rule's condition tree. At small rule-set sizes (tens, low hundreds) that startup cost is invisible. At rule-set sizes the indexed adapter is actually built for (thousands), startup parsing and indexing become the dominant cost of every process restart — every deploy, every scale-up event, every container rotation pays the same parse+build tax.

bre-go ships two persistence formats for the indexed engine:

- **JSON snapshot (`engine/indexed.Snapshot`)**: serializable rule tree with `ExportSnapshot()` / `LoadSnapshot(snap, rebuild)`. The `rebuild` argument is a `map[string]RuleCallbacks` because action closures are not encodable; bre-go re-attaches them by rule name at load time.
- **Compiled (binary) snapshot (`engine/indexed.CompiledSnapshot`)**: pre-bucketed wire format with `ExportCompiledSnapshot()` / `LoadCompiledSnapshot(cs, rebuild)`. Same callback-rebuild pattern, faster load.

The markup-svc side has one additional concern bre-go does not solve: the Action closure we register on every rule is always `func(in interface{}) interface{} { return factor }`. The factor is per-rule data the snapshot must carry; otherwise the rebuild map cannot reconstitute the right Action.

Three design questions.

### 1. JSON snapshot or compiled snapshot first?

Pick JSON first. The bre-go compiled snapshot is faster but the JSON format is:

- Human-readable — operators can `cat` a snapshot and see exactly which rules it contains.
- Diff-friendly — snapshot drift between two CSV revisions surfaces as a JSON diff in code review.
- Forward-compatible — when a compiled snapshot lands later it can read the same `Snapshot` type and re-emit in the faster format.

The cost difference between the two is invisible at boot — both are O(rules) — and the markup server boots once per deploy, not per request. JSON is the right default for v0.0.x. Compiled is a follow-up if a real bottleneck appears.

### 2. Where do per-rule factors live?

A markup-side wrapper:

```go
type Snapshot struct {
    FormatVersion  int                       `json:"formatVersion"`
    ModelVersion   string                    `json:"modelVersion"`
    Factors        map[string]float64        `json:"factors"`
    EngineSnapshot indexed.Snapshot          `json:"engineSnapshot"`
}
```

`Factors` is the per-rule markup factor keyed by rule name. `LoadIntoIndexedDecider` builds the `map[string]indexed.RuleCallbacks` rebuild map by closing over each rule's factor from `Factors`. The bre-go snapshot owns rule structure; the markup-side snapshot owns the markup-specific data the engine cannot serialize.

`ModelVersion` rides along on the snapshot so the decisions served from it stamp the right tag without depending on a separate flag. The `--model` flag still exists as an override for ops-driven workflows; the snapshot value is the default.

### 3. Which adapters get snapshots?

Indexed only. The inmemory / firstmatch / priority adapters take closure-based rules whose conditions are opaque Go functions — there is nothing structural to snapshot. Their on-disk format is the CSV (ADR-0002), and that stays the format they boot from. Indexed has a typed `parser.Condition` AST per rule, so it can be serialized and rebuilt structurally. Snapshots are the indexed adapter's complementary on-disk format.

## Decision

`internal/snapshot` ships:

```go
type Snapshot struct {
    FormatVersion  int                `json:"formatVersion"`
    ModelVersion   string             `json:"modelVersion"`
    Factors        map[string]float64 `json:"factors"`
    EngineSnapshot indexed.Snapshot   `json:"engineSnapshot"`
}

// Build constructs a Snapshot from loader-side rules and the
// model-version tag. Internally builds an indexed.Engine, calls
// ExportSnapshot, and pairs it with the factor map.
func Build(rules []load.Rule, modelVersion string) (*Snapshot, error)

// Write serializes s as JSON to w.
func Write(w io.Writer, s *Snapshot) error

// Read deserializes a Snapshot from r. Returns ErrFormatVersionMismatch
// if FormatVersion is anything other than the version this code knows.
func Read(r io.Reader) (*Snapshot, error)

// LoadIntoIndexedDecider rebuilds an indexed.Decider from s.
// Action callbacks are re-attached from s.Factors keyed by rule name.
func LoadIntoIndexedDecider(s *Snapshot) (*indexed.Decider, error)
```

`cmd/snapshot-build` is a separate binary that wraps the CSV → Snapshot flow:

```sh
./snapshot-build --rules=rules.csv --model=v1 --out=snapshot.json
```

`cmd/markup-server` gains a `--snapshot` flag mutually exclusive with `--rules`. When `--snapshot` is set, the server cold-starts via `snapshot.LoadIntoIndexedDecider` regardless of the `--adapter` flag — the only adapter that supports snapshot loading is indexed, and the flag combination is validated at boot.

## Consequences

### Closed by this ADR

- A markup-side snapshot wrapper exists, owns the per-rule factor map, and wraps bre-go's typed snapshot.
- The indexed Decider can be constructed from either `[]load.Rule` (current path) or `*Snapshot` (new path).
- `cmd/snapshot-build` is the build-time tool that produces snapshots from CSVs.
- `cmd/markup-server --snapshot=...` is the cold-start path that skips CSV parsing and bre-go's AddRule bucketing walk.
- Snapshots carry `ModelVersion` so a decision served from a snapshot stamps the right tag without operator intervention.

### NOT closed by this ADR

- Compiled (binary) snapshot. Tracked separately under its own ADR if a real bottleneck appears.
- Hot reload. Out of scope here; tracked separately.
- Snapshot signing or content-addressing. Operations can checksum the file with stdlib tools; cryptographic integrity is a separate concern.
- Snapshot versioning across the markup format. `FormatVersion` is bumped when the wrapper schema changes; the embedded bre-go `Snapshot` has its own `FormatVersion`.

### Performance impact

Snapshot loading vs CSV loading:

- CSV path: `load.FromCSV` parses N rows, calls `parser.ParseToCondition` on each, then `indexed.New` calls `AddRule` N times (each walks the condition tree and updates bucket maps), then `Build()` seals.
- Snapshot path: JSON unmarshal into the wrapper, then `indexed.LoadSnapshot` walks the pre-typed `Match` tree (no string-parsing), calls `AddRule` N times with the already-typed conditions, `Build()` seals.

The win is the savings from `parser.ParseToCondition` and from going through any typed `Condition` directly without re-parsing. At N=1000 rules the parse step is dominant; at N=10 it is invisible.

JSON snapshot file size grows linearly with rule count and condition complexity. For markup-sized rule sets (hundreds to low thousands of rules over a small fact map) snapshot files stay in the tens-to-low-hundreds of KB range — fits in container images or sidecar mounts without ceremony.

### Validation strategy

- `internal/snapshot` ships table-driven tests for `Build`, `Write`, `Read`, `LoadIntoIndexedDecider`:
  - Round-trip: rules → Build → Write → Read → LoadIntoIndexedDecider → Decide returns the same result as the rules-only construction path on the same Request matrix.
  - Format-version mismatch: a snapshot with an unknown `FormatVersion` fails at `Read` time, not later.
  - Missing factor: a snapshot whose `Factors` map omits a rule that the engine snapshot lists surfaces as an error at `LoadIntoIndexedDecider`, not as a silent zero-factor Decision.
- `cmd/snapshot-build` ships an end-to-end test: CSV in, snapshot JSON out, snapshot loaded into an indexed Decider, sample Request produces the expected Decision.
- `cmd/markup-server` ships a `--snapshot` smoke test: server boots from a snapshot file, serves `/decide`, returns the expected Decision.
