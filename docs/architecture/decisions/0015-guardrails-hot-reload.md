# 15. Hot-Reload Guardrails via `POST /admin/guardrails`

## Status

Proposed — proposes `guardrails.Holder` (mirroring `swap.Decider`) plus a `POST /admin/guardrails` admin endpoint so operators can update the configured rule set without restarting the process. A companion `GET /admin/guardrails` returns the current configuration so operators can inspect drift before editing. Existing boot flags from ADR-0014 stay; hot-reload is an opt-in wiring choice in `cmd/markup-server`.

## Context

ADR-0014 ships the guardrails decorator and the four boot flags. Today an operator who needs to tighten `--max-factor` from 3.0 to 2.5 — say, because a runaway A/B variant just produced a 2.8 markup the business considers a service-impacting bug — has to redeploy the process. In-flight Decisions get the old bound; new Decisions wait for the rolling restart to complete. For an incident that requires a tightened envelope *now*, "wait for kubectl to finish the rollout" is too slow. The classic 2-AM operator move is to flip the flag and continue.

`POST /admin/reload` already hot-reloads the inner Decider against its boot-time loader (ADR-0008). The shape of the work is the same: read a new config, build the new thing, swap it into a holder, in-flight calls finish on the old one. This ADR generalizes that pattern to the guardrails rule set.

Three design questions.

### 1. Where does the mutability live — on `guardrails.Decider` or in a holder?

Two candidate shapes:

- **Method on `Decider`**: add `Replace([]Rule)` directly. Pro: smaller API surface. Con: the Decider becomes mutable, which complicates the "safe to share across goroutines" property the package documents today. Every `Decide` would need to lock around the rules slice; the no-state-fields-after-construction invariant from the ADR-0014 architecture review would be gone.

- **Holder pattern**: ship `guardrails.Holder` mirroring `swap.Decider`. The holder owns the mutable rules slice; the Decider it builds for the call chain stays immutable. `Holder.Replace([]Rule)` rebuilds and swaps atomically. Per-port consistency — every mutation surface in markup-svc is a holder (`swap.Decider` for the engine; now `guardrails.Holder` for the rules), and a future mutable-config decorator can follow the same pattern.

**Pick the holder.** Per-port consistency outweighs a small naming cost, and the existing `swap.Decider` shape gives operators a familiar mental model for the new endpoint. The Decider stays immutable; the Holder is the mutation surface.

Reusing `swap.Decider` directly does *not* solve the problem — a `swap.Decider` holding a `guardrails.Decider` would swap the entire guardrails layer, but the inner Decider that the guardrails wraps cannot be re-bound mid-swap without rebuilding the whole stack. The Holder for rules is a separate concern from the Holder for the engine, and the two compose orthogonally.

### 2. What is the JSON API shape?

`POST /admin/guardrails` with a JSON body matching the four boot flags:

```json
{
  "min_factor": 0.5,
  "max_factor": 3.0,
  "allowed_countries": ["BR", "DE", "FR"],
  "required_fields": ["customer_tier", "country"]
}
```

The semantic is **set-the-full-config**, not patch. Omitting `max_factor` means "no `FactorRange` rule" (the same as not passing `--max-factor` at boot), not "keep the current `max_factor`". An operator who wants to tighten one field reads the current config with `GET /admin/guardrails`, edits it locally, and POSTs the full intended state back.

Set-full rather than patch because:

- It matches the boot-flag semantic exactly. `--max-factor=3.0` sets `max_factor` to `3.0`; it does not merge with anything. The admin endpoint should not introduce an asymmetric mental model.
- It matches the per-route `routeReloadHandler` pattern that already lives in `cmd/markup-server` (`main.go:246-291`): a typed JSON body, full payload required, no field-level merge. This ADR follows the same precedent rather than the schemaless `POST /admin/reload` handler (`internal/httpapi/reload.go`), which takes an opaque body and re-runs an entire loader closure.
- Patch semantics introduce a per-field "unset vs not-set" ambiguity that operators have to reason about under pressure. Set-full is unambiguous.
- The `GET` → edit → `POST` round-trip is one extra HTTP call; operators on a 2-AM incident already do this for every other admin endpoint.

`GET /admin/guardrails` returns the same shape with the currently-served values. A successful `POST` returns the new state on 200 so the operator's `curl | jq` shows what just landed.

### 3. Error posture on a bad body?

Mirror the existing `routeReloadHandler` (cmd/markup-server/main.go:246-291), which is the typed-body precedent in this codebase:

- `400` with opaque JSON body on malformed JSON, on `min_factor > max_factor`, on an empty `allowed_countries` array, on an empty `required_fields` array, on an unknown `required_fields` name.
- `405` with `Allow: POST` on a non-POST method (`GET` mounted separately).
- On any validation failure: the previous rule set keeps serving. The Holder is only mutated on success.
- Operator-stderr log carries the underlying detail (which field failed, what the offending value was) — same posture `routeReloadHandler` takes on bad route names and loader errors.

The `400` body is opaque so the endpoint cannot be probed for the rules schema by an unauthenticated caller. Operators see the reason in their log; bots do not.

## Decision

`internal/decider/guardrails` ships a `Holder`:

```go
// Holder owns a mutable []Rule slice and produces a markup.Decider
// that vetoes Decisions against the current slice. Replace swaps the
// slice atomically; in-flight Decides finish on the captured slice
// they read at Decide entry.
type Holder struct { /* mu sync.RWMutex; rules []Rule */ }

// NewHolder returns a Holder pre-loaded with rules. A Holder with no
// rules is a pass-through (the Wrap'd Decider returns the inner
// Decision unchanged).
func NewHolder(rules ...Rule) *Holder

// Wrap returns a markup.Decider that reads the current rules from
// the Holder on every Decide. The returned Decider holds the Holder
// by pointer; replacing the Holder's rules is observed by every
// returned Decider.
func (h *Holder) Wrap(inner markup.Decider) markup.Decider

// Replace swaps the active rules. A defensive copy is made so callers
// cannot mutate the slice after Replace returns. The previous slice
// stays alive for any Decide call that already read it under RLock.
func (h *Holder) Replace(rules []Rule)

// Snapshot returns a copy of the current rules. Useful for the
// GET /admin/guardrails endpoint and for tests.
func (h *Holder) Snapshot() []Rule
```

The internal Decider uses the same minimum-lock-hold pattern as `swap.Decider`:

```go
func (d *holderDecider) Decide(ctx context.Context, req markup.Request) (markup.Decision, error) {
    d.holder.mu.RLock()
    rules := d.holder.rules  // slice header; backing array never mutated
    d.holder.mu.RUnlock()

    decision, err := d.inner.Decide(ctx, req)
    if err != nil {
        return decision, err
    }
    for _, rule := range rules {
        if allowed, reason := rule.Check(ctx, decision, req); !allowed {
            return markup.Decision{}, fmt.Errorf("%w: %s", ErrGuardrailViolation, reason)
        }
    }
    return decision, nil
}
```

`Replace` allocates a new backing array; it never mutates the old one. A concurrent Decide that captured the old slice header under RLock walks the old array to completion. A Decide that arrives after Replace returns reads the new slice header. No coordination beyond the lock pair is needed.

`cmd/markup-server` gains:

- A new flag `--guardrails-admin` (default `false`). When set, the admin handlers are mounted. When unset, the existing boot-flag wiring is unchanged and no admin endpoint exists.
- When `--guardrails-admin` is on, `wireTracedHandler` and `wireRouterHandler` construct a `guardrails.Holder` from the boot flags (or empty if none set), wrap the inner Decider through it, and mount `POST /admin/guardrails` + `GET /admin/guardrails` on the same mux as `/decide` and `/admin/reload`.

`internal/httpapi` gains:

- `GuardrailsAdmin(holder *guardrails.Holder, parse func(body []byte) ([]guardrails.Rule, error)) http.Handler` — single handler that dispatches on method. POST reads the body, calls `parse`, on success calls `holder.Replace(rules)` and returns 200 with the new snapshot; on parse failure returns 400. GET returns the current snapshot.
- The `parse` closure lives in cmd and shares its validation core with the boot-flag wiring. Today `buildGuardrailRules` (`main.go:471-501`) couples to `*flag.FlagSet.Visit` to detect "explicitly set" — a posture that does not apply to JSON bodies, where the absent-vs-zero-value distinction is encoded structurally. The implementation splits the function: a pure `guardrails.BuildRules(min, max float64, countries, fields []string) ([]Rule, error)` lives in the guardrails package and owns the `min > max` / empty-CSV / unknown-field-name validation; the existing `buildGuardrailRules` becomes a thin `fs.Visit` adapter that calls into it; the new handler's parse closure is another adapter. Both call sites produce identical Rule slices and identical error messages.

## Consequences

### Closed by this ADR

- Operators can tighten / loosen / replace the guardrails rule set without restarting the process. The 2-AM incident response no longer requires a rolling restart.
- `GET /admin/guardrails` lets operators inspect the live config without grepping process args — useful when multiple operators have edited the rule set during an incident.
- The `Holder` + `Decider`-stays-immutable shape mirrors `swap.Decider`, so a reader of `internal/decider/*` sees the same mutation pattern across both packages.
- Body validation reuses the boot-flag validation (`min > max`, empty CSV, unknown field name), so `--max-factor=2 --min-factor=3` at boot and `{"min_factor": 3, "max_factor": 2}` at runtime fail with the same reason and the same HTTP-status posture.

### NOT closed by this ADR

- Authentication / authorization on the admin endpoint. `POST /admin/reload` is also unauthenticated today (operators are expected to gate it via Kubernetes NetworkPolicy or a sidecar). This ADR follows the same posture; a future ADR can add authentication uniformly across every `/admin/*` endpoint when there is a real consumer asking.
- Audit logging of who-changed-what-when. The Holder records the new state; it does not record the operator identity or a diff against the prior state. Out of scope for this release; if a real audit requirement lands the Holder can grow a `History()` method without a port change.
- Per-route guardrails hot-reload in multi-route deployments. The current `--guardrails-admin` flag mounts a single endpoint controlling the single outer guardrails layer. A multi-route deployment with per-route guardrails (which is already possible by wrapping each Route's Decider before adding it to the router) would need a route-keyed body shape; that is its own ADR if anyone asks.
- Persistence of admin-set rules across restarts. After a process restart the boot flags re-apply; any admin-set state is lost. Operators who want the change to survive a restart update the deployment manifest as well as the running process. This is the same posture as `POST /admin/reload` — admin endpoints affect the *running* process; durable state lives in config.

- Replace-throughput under pathological caller behavior. At admin-call rates (a handful per day, even on a busy incident) Replace acquires the write lock for sub-microsecond windows and cannot starve Decide readers in practice. A misconfigured script invoking Replace at request-path QPS would serialize Decide readers behind the writer lock; protecting against that is the operator's responsibility (same as for `POST /admin/reload`, where a loop calling reload at request-path QPS would similarly serialize the holder).

### Performance impact

The Holder's `Decide` path adds one `RLock`/`RUnlock` pair around a slice-header copy compared to the immutable `guardrails.Decider` of ADR-0014. Cost on amd64: ~10 ns for the uncontended RLock pair plus the slice-header assignment. This matches the measured swap.Decider overhead from `scientific/v0.1.0/REPORT.md` (indexed 442 → swap 452 = +10 ns on arm64/M4) — the same minimum-lock-hold pattern producing the same per-Decide overhead. The rest of the Decide path (the rules loop) is unchanged.

Aggregate per-Decide overhead in the production wiring with all decorators stacked (`otel → guardrails.Holder → swap → engine`), computed from explicit addends against the v0.1.3 and v0.1.0 measurements:

- indexed baseline: 430-442 ns
- swap.Decider overhead: +10 ns
- guardrails-three-rules body: +42 ns (measured v0.1.3)
- Holder lock-pair: +10 ns (predicted; matches swap pattern)
- otel.Wrap overhead: +85 ns (measured v0.1.0)

Sum: ~580 ns. The `scientific/v0.1.4/` harness pre-registers three new rows: `BenchmarkDecorator/guardrails-holder-zero-rules` (proves the wrapper-with-no-rules cost matches v0.1.3's immutable zero-rules cost plus the lock-pair budget), `BenchmarkDecorator/guardrails-holder-three-rules` (the realistic production cost), and `BenchmarkReplace` (bounds the admin-call cost so a regression that made Replace allocate more than one backing array would surface as a measurable per-Replace allocation regression).

`Replace` allocates one new `[]Rule` backing array per call. At admin-call rates the allocation is invisible. The v0.1.3 baseline of 12 allocs/op on the happy path stays unchanged with the Holder: any regression that introduces a per-Decide heap allocation in the Holder hot path (Replace allocating inside Decide, slice-header escape, defensive copy of the captured slice) would push the per-Decide allocation count above 12 and fail the bar.

### Validation strategy

- `internal/decider/guardrails`: unit tests for the `Holder`. Cover:
  - `TestHolderEmptyRulesPassesEveryDecision` — NewHolder() Wrap is a pass-through.
  - `TestHolderReplaceObservedByNextDecide` — Replace before a Decide changes the result; the captured-old-slice path is exercised by the concurrent test below.
  - `TestHolderSnapshotReturnsDefensiveCopy` — mutating the returned slice does not affect the Holder.
  - `TestHolderConcurrentDecideAndReplaceRaceFree` — N goroutines hammer Decide while another goroutine Replaces. Run under `-race`. No deadlock, no data race, every Decide returns either the pre-Replace or post-Replace verdict, never a torn state.
- `internal/httpapi`: handler tests for `GuardrailsAdmin`. Cover the four-status matrix (200 OK on valid POST, 200 OK on GET, 400 on malformed body, 405 on non-POST-non-GET) and the "previous rules keep serving on parse failure" invariant.
- `cmd/markup-server` e2e: boot with `--guardrails-admin --max-factor=3.0`, POST `/admin/guardrails` with `{"max_factor": 1.0}`, confirm a Decision with MarkupFactor 1.15 that was previously 200 now returns 500. POST back to `{"max_factor": 3.0}`, confirm 200 again. The asymmetric pair is the proof.
- `scientific/v0.1.4/`: pre-register three rows against the v0.1.3 immutable-Decider baseline so the lock-pair overhead is measurable on every angle that matters — `BenchmarkDecorator/guardrails-holder-zero-rules` (no-config wrapper cost), `BenchmarkDecorator/guardrails-holder-three-rules` (realistic production cost), and `BenchmarkReplace` (admin-call cost).
