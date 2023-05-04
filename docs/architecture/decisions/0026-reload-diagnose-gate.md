# 26. `/admin/reload` is gated on Diagnose

## Status

Accepted — `httpapi.Reload` gains a `WithReloadDiagnose(fn)` option that runs `Diagnose` against the new rule set BEFORE the swap. If `Diagnose.IsHealthy()` is false, the handler returns `400 + JSON {healthy: false, errors: [...], warnings: [...]}` and the currently-serving Decider is NOT swapped. Customers keep getting the previous (working) rules. cmd wires the gate when `diagnoseFn` is configured (i.e., `--rules` mode); other modes use the legacy two-arg form so behavior is unchanged.

## Context

ADR-0025 added boot-time Diagnose: `--diagnose=on` exits non-zero before the listener binds when the rule set has errors. Combined with the existing `/readyz`-based Kubernetes readiness probe, this makes the boot path fail-closed: a new pod with bad rules never reports Ready, so the rolling deployment halts and the old pod keeps serving. Customers see no impact.

The hot-reload path was NOT covered. `POST /admin/reload` ran the loader and swapped the active Decider regardless of whether the new rules made sense. A ConfigMap remount followed by `curl -X POST /admin/reload` could land bad rules on a running pod and customers would eat them until someone noticed. This ADR closes that gap.

## Decision

`httpapi.Reload` keeps its existing two-arg signature `Reload(holder, loader) http.Handler` (backwards compatible — existing tests and the `--snapshot` path keep working). A variadic third parameter `opts ...ReloadOption` accepts options:

```go
func Reload(holder *swap.Decider, loader Loader, opts ...ReloadOption) http.Handler
func WithReloadDiagnose(fn DiagnoseFn) ReloadOption
```

When `WithReloadDiagnose` is set, the handler runs `fn()` first. Three outcomes:

| fn outcome | response |
|---|---|
| `(Diagnosis, nil)`, healthy | proceed with the existing loader + swap path |
| `(Diagnosis, nil)`, unhealthy | `400` + JSON `{healthy: false, errors: [...], warnings: [...]}`, no swap |
| `(_, err)` | `500 + "diagnose: ..."`, no swap |

The DiagnoseFn is the same closure built at boot for `/admin/diagnose` per ADR-0025 — re-reading the file + running `load.Diagnose`. Same vocabulary, same wire shape; operators see the same JSON whether they hit `/admin/diagnose` (read-only check) or get a 400 back from `/admin/reload` (rejected swap).

`cmd/markup-server` wires the gate only when `diagnoseFn` is non-nil — i.e., in `--rules` mode. `--snapshot` mode has no per-call Diagnose (snapshots are pre-validated); the legacy two-arg call is used.

## Consequences

### Closed

- A bad rule set posted via `/admin/reload` no longer reaches customer traffic. The swap is rejected; the old rules stay active; the operator gets a structured 400 explaining what's wrong.
- The K8s safety story is symmetric. Boot (ADR-0025) and hot-reload (this ADR) both fail-closed. A pod gone wrong via either path stays out of the customer's way.
- The wire shape matches `/admin/diagnose`. Operators have one filter to remember (`attrs.kind`) and one body shape across both endpoints.

### Not closed

- `/admin/guardrails` hot-reload (markup-svc/ADR-0015) and `/admin/routes` hot-reload (decision-gateway/ADR-0008) do not yet have a Diagnose gate. The router admin already validates via `gateway.NewRouter` (rejects empty prefix, duplicate prefix, nil backend) so the most obvious shapes are blocked. The guardrails admin has no semantic gate today. A follow-up ADR can port the same DiagnoseFn pattern.
- Router-mode per-route reload (cmd's `routeReloadHandler`) does not yet take a per-route gate. Lands when an operator workflow proves it; each route would need its own DiagnoseFn closure over its rules file.
- `/readyz` continues to report only the boot-time readiness state. A future ADR could cache the last Diagnose result and have `/readyz` return 503 when unhealthy — belt-and-suspenders against any path that drifts the pod into a bad state. Not implemented today because the rule-set bad-state can only enter via boot (blocked) or reload (now blocked), so `/readyz` reflecting Diagnose would be redundant.
- Per-rule diff. The response shows the issues in the new rule set as a flat list; it does not say "this rule changed factor from 1.05 to -0.5 and is now invalid." Operators compute the diff client-side.

### Performance impact

- Per reload: one extra Diagnose run (re-reads the file + O(N) checks). Operator-triggered, not on the hot path.
- Per `/decide`: zero. The gate is in the admin path only.
