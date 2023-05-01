# 25. `Diagnose()` + port-level sentinels

## Status

Accepted — `internal/markup` ships `Issue` / `Severity` / `Diagnosis` types + six adapter-agnostic `IssueKind` sentinels (`empty_rule_set`, `duplicate_name`, `duplicate_priority`, `invalid_factor`, `no_op_factor`, `empty_condition`). `internal/load.Diagnose([]Rule)` runs the rule-set checks and returns a `Diagnosis`. `internal/httpapi.Diagnose(fn)` mounts on `GET /admin/diagnose` returning the current Diagnosis as JSON with status `200` when healthy and `503` when at least one error is present so curl-driven CI gates fail on a broken rule set without parsing the body. `cmd --diagnose=on|warn|off` (default `on`) runs Diagnose at boot and fails on errors; emits one structured `markup-server.diagnose` JSON event per issue.

## Context

The bre-go capability matrix has two long-deferred items: `Diagnose()` (v0.14.0) and port-level sentinels (v0.19.0). Together they close the "broken rule set silently returns ErrNoMatch for an hour before anyone notices" class of failure. Without them, a typo in `rules.csv` — wrong factor sign, duplicate rule name, condition that compiles but never matches — surfaces as elevated `markup_decide_total{outcome="no_match"}` at runtime; the operator's investigation path is to grep logs, find a request that should have matched, and reverse-engineer the rule that's wrong.

Sentinels are the vocabulary Diagnose returns. Keeping them adapter-agnostic means the same `IssueKind` string serves whether the rule set is loaded into inmemory, firstmatch, priority, or indexed adapters — operator dashboards key on `attrs.kind` without branching by adapter.

## Decision

**`internal/markup/diagnose.go`** ships:

```go
type IssueKind string
const (
    IssueEmptyRuleSet      IssueKind = "empty_rule_set"
    IssueDuplicateName     IssueKind = "duplicate_name"
    IssueDuplicatePriority IssueKind = "duplicate_priority"
    IssueInvalidFactor     IssueKind = "invalid_factor"
    IssueNoOpFactor        IssueKind = "no_op_factor"
    IssueEmptyCondition    IssueKind = "empty_condition"
)

type Severity int  // SeverityError | SeverityWarning
type Issue       struct { Kind IssueKind; Severity Severity; Rule string; Detail string }
type Diagnosis   struct { Issues []Issue }
func (d Diagnosis) Errors() []Issue
func (d Diagnosis) Warnings() []Issue
func (d Diagnosis) IsHealthy() bool   // no SeverityError issues present
```

**`internal/load.Diagnose([]Rule) markup.Diagnosis`** runs the adapter-agnostic checks:

| Check | Severity | Sentinel |
|---|---|---|
| empty rule set | error | empty_rule_set |
| duplicate rule name | error | duplicate_name |
| nil/empty condition | error | empty_condition |
| factor ≤ 0 | error | invalid_factor |
| factor == 1.0 | warning | no_op_factor |
| factor > 10 | warning | invalid_factor |
| duplicate priority across rules | warning | duplicate_priority |

Adapter-specific deep checks (e.g., indexed engine's non-indexable-condition rejection) keep producing their own errors via the existing `ErrNonIndexableCondition` path; this ADR does not duplicate them.

**`internal/httpapi.Diagnose(fn DiagnoseFn) http.Handler`** mounts on `GET /admin/diagnose`. Returns:

```json
{
  "healthy": true,
  "errors":   [{"kind":"...", "rule":"...", "detail":"..."}],
  "warnings": [{"kind":"...", "rule":"...", "detail":"..."}]
}
```

Status 200 when healthy, 503 when not — so operator CI scripts can `curl --fail-with-body` and gate on the HTTP code.

**`cmd/markup-server`** gains `--diagnose=on|warn|off` (default `on`):

- `on` — runs Diagnose at boot. On any error, fails boot with a non-zero exit and the error count in the exit message. Issues also emit as `markup-server.diagnose` JSON events (one per issue, severity → log level).
- `warn` — runs Diagnose at boot. Logs every issue, including errors, but starts anyway. Useful for staged rollouts where operators want visibility without breaking the boot.
- `off` — skips Diagnose entirely. The admin endpoint stays mounted (still useful for on-demand checks).

Diagnose is wired only in `--rules` mode (the CSV path). `--snapshot` rule sets are pre-validated; `--route` mode loops across routes but uses the same loader path per route (a future ADR can extend Diagnose across routes when an operator workflow proves it).

## Consequences

### Closed

- A broken rule set fails boot loud + early. Boot logs surface exactly which rules are wrong via one `markup-server.diagnose` JSON event per issue with the sentinel `kind`, rule name, and detail.
- `GET /admin/diagnose` lets operators re-run the check on demand without restarting — useful after a hot-reload via `POST /admin/reload` to confirm the new rule set is clean.
- The sentinel set gives operator dashboards a stable filter shape. `attrs.kind:"duplicate_name"` finds every duplicate-name event across model versions + tenants without knowing which adapter loaded the rules.

### Not closed

- Adapter-specific deep checks (unreachable rules per the priority adapter's tie-break behavior, indexable-fact analysis for indexed). Adapters that already fail-fast via their own errors don't need this; adapters that don't could extend `Diagnose` via a Diagnoser interface in a follow-up ADR.
- Cross-rule reasoning (rule A is a superset of rule B). Out of scope.
- `Diagnose` in `--route` and `--snapshot` modes. Route mode loops across N rule files; current ADR runs Diagnose only in `--rules` mode. Lands when an operator workflow asks.
- Cross-runs diagnose history. Each call returns the current Diagnosis; there's no persistence or diff. Operators wanting "what changed since last reload" run two curls and diff in their shell.

### Performance impact

- Boot cost: one pass over the rule slice (~O(N) lookups for duplicate-name + duplicate-priority via maps). At typical N=10-100 rules, ~100 ns. Negligible.
- Admin endpoint: re-reads rules from disk + runs the same O(N) pass per call. Operator-triggered, not on the hot path.
- No per-Decide cost. Diagnose runs at boot + on admin GET only.
