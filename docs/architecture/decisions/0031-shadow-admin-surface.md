# 31. Shadow admin surface — load and clear a challenger Decider

## Status

Accepted — `--shadow-admin` mounts `POST /admin/load-challenger` and `DELETE /admin/challenger`. The challenger is held in a new `shadow.Holder` alongside the existing `swap.Decider` champion holder. The admin surface lands first; `/decide` does not yet consult the challenger. A later ADR adds the `/decide` shadow execution path.

## Context

ADR-0009 in `model-registry` closed the challenger lifecycle on the registry side: `POST /promote role=challenger` stores a hash in envstate, `POST /reject` clears it, `mrctl diff` shows what changed. The registry stores metadata only. The data plane — `markup-svc` — does not yet read the challenger; nothing in the runtime path can answer "what would the challenger have decided here?".

The shadow-Decider arc is broken into five sub-iterations (admin surface, `/decide` execution, registry-side push, observability dashboards + alerts, mrctl ergonomics). This ADR documents the first: the admin surface that accepts and removes the challenger bytes. The remaining sub-iterations land under their own ADRs.

Shipping the admin surface in isolation has operational value even before `/decide` consults the challenger: operators can manually verify the surface against a real markup-svc instance, the registry's rolling-deployer integration has a stable target to wire against, and an admin-only deployment is observably equivalent on `/decide` to the pre-flag binary (the challenger is held but never executed).

## Decision

### `shadow.Holder`

A new package `internal/decider/shadow` holds an optional `markup.Decider`. The Holder is nil-aware: zero-value reports `loaded=false` via `Get`, `Load` installs one, `Clear` removes it.

```go
type Holder struct {
    mu    sync.RWMutex
    inner markup.Decider
}

func (h *Holder) Load(d markup.Decider)
func (h *Holder) Clear()
func (h *Holder) Get() (markup.Decider, bool)
```

The shape mirrors `swap.Decider` (ADR-0008) but supports the empty state. `swap.Decider`'s `Decide` panics on a nil inner. The shadow path needs an "empty until loaded" state, so the Holder exposes presence through `Get`'s second return value rather than via the Decider interface.

### `POST /admin/load-challenger`

The handler reuses the existing `ReloadBodyLoader` (ADR-0030). The same body shape — `text/csv` or `application/json` — that drives `/admin/reload` drives `/admin/load-challenger`. The loader runs the existing Diagnose pass; a failure surfaces as `400` with the ADR-0026 envelope (`{error, healthy:false, errors:[], warnings:[]}`), which is bit-for-bit identical to `/admin/reload`'s rejection shape.

A successful load installs the new Decider into the shadow Holder and returns `200` with the `{rule_count, model_version}` envelope `/admin/reload` already uses. Empty body, unsupported media type, oversize body, and non-`POST` map to `400`, `415`, `413`, `405` respectively — same posture as `/admin/reload`.

### `DELETE /admin/challenger`

Clears the holder. Returns `204 No Content` whether or not a challenger was loaded; the operation is idempotent.

### `--shadow-admin` flag

The cmd shell mounts both routes only when `--shadow-admin` is set. Off by default. When off, no new routes are registered, no new types appear on the `/decide` call path, and the challenger Holder is never allocated. The compiled binary does include the shadow package's type metadata; the binary is therefore not byte-identical to a pre-flag build, but the runtime behaviour on `/decide` is observably unchanged.

This matches the v0.1.4 / v0.1.18 pattern of gating admin surfaces behind explicit opt-in flags (`--diagnose`, `--guardrails-admin`).

### Hex boundary

`internal/httpapi` defines a `ChallengerHolder` interface (`Load(markup.Decider) / Clear() / Get() (markup.Decider, bool)`). The handlers accept that interface; `shadow.Holder` satisfies it. The handler file does not import `internal/decider/shadow`. The cmd shell wires the concrete `shadow.Holder` into the interface at composition time.

### Spans

Both handlers wrap in `WithAdminSpan` (ADR-0028):

- `markup.admin.load_challenger`
- `markup.admin.clear_challenger`

The names follow the existing `markup.admin.<verb>` convention. The `/decide`-side ADR that lands next in the arc introduces `markup.challenger.evaluate` as a child of `markup.decider.decide`.

## Consequences

### Positive

- The shadow-Decider arc lands incrementally. Each sub-iteration is shippable in isolation; the admin-only deployment is observably a no-op on production `/decide` while operators verify the surface.
- The registry's rolling deployer (next iteration) has a stable admin target before its own work starts.
- The `shadow.Holder` shape is testable in isolation (zero / load / clear / replace / concurrent get-and-swap) without any HTTP surface.
- The wire shape is borrowed wholesale from `/admin/reload`: operators who can already drive `/admin/reload` can drive `/admin/load-challenger` with one curl arg change. No new mental model.
- `ChallengerHolder` interface in `internal/httpapi` keeps the port package free of the adapter import. Handler tests substitute fakes without pulling `internal/decider/shadow`.

### Negative

- A second admin surface that shares logic with `/admin/reload` widens the test matrix. Mitigation: both handlers reuse `ReloadBodyLoader`, so format support cannot diverge accidentally; the divergence is contained to the holder write target.
- The cmd-shell signature of `wireTracedHandler` grew by one parameter (`shadowAdmin bool`). Existing call sites in tests all updated in this ADR's commit; future call sites must pass the flag.
- A challenger loaded via `/admin/load-challenger` survives only the running process. Markup-svc restart loses it. The registry side will re-push on boot via the same surface, so the persistence story is "registry is the source of truth", not "markup-svc persists the challenger" — matches the existing `/admin/reload` posture for champion rules.

### Intent for the next iteration (not measured here)

- `Holder.Get()` is designed to support a zero-allocation fast path when `loaded == false`. The next ADR (the one that wires shadow execution into `/decide`) confirms the escape analysis and registers a bar in the scientific harness for both the loaded and unloaded paths.
- Adding `Get()` to the `/decide` hot path introduces a second `sync.RWMutex` `RLock`/`RUnlock` pair beyond the existing `swap.Decider` one. The next ADR quantifies the added cost (`BenchmarkDecideWithShadowHolderUnloaded` + `BenchmarkDecideWithShadowHolderLoaded`).

### Deliberately not here

- `/decide` shadow execution. Next ADR.
- Registry-side push of challenger bytes. Lives in `model-registry`.
- Comparison metrics (`markup_challenger_agreement_total`, factor delta histogram). Next ADR.
- Persistence across markup-svc restarts. The registry re-pushes.
- A "shadow mode" flag on `/admin/reload` (`?role=challenger`). Resolved in favour of a distinct route to isolate failure modes; the admin surface stays small.

## Alternatives considered

**Grow `?role=challenger` on `/admin/reload`** — halves the admin surface (one route, one curl invocation pattern). Rejected: an operator pushing the wrong query param ships the wrong role; the registry's rolling deployer needs to distinguish the two calls' failure modes (a challenger-push failure must NOT roll back the champion); and the spans would either collide on name or grow conditional logic.

**Store challenger bytes alongside champion bytes in `swap.Decider` and extend its Decide to branch** — would let `/decide` consume both through one type. Rejected: `swap.Decider.Decide` is on the hot path; conditional logic against an optional second slot is a regression risk for the champion path. The next ADR will compose at the handler level, not inside the Holder.

**Persist challenger to disk** — would survive markup-svc restarts. Rejected: it puts markup-svc in the source-of-truth business, which the model-registry exists to avoid. The registry's next-iteration push handles restart.
