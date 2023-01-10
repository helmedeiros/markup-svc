# Push a rule fix without restarting

## Problem

You spotted a wrong factor in production. You want to fix it on disk and have the running `markup-server` pick up the change without restarting — in-flight requests should finish on the old rule set, new requests should land on the new one, and a parse error in the new file should not break the running server.

## Recipe

Edit the CSV (or snapshot) at the same path the running server was booted with. Single-route deployment:

```sh
# Server was booted with --rules=/etc/markup/rules.csv
$EDITOR /etc/markup/rules.csv
```

Then POST `/admin/reload`. Single-route deployment (one rule set; body is ignored):

```sh
curl -sS -X POST \
  http://markup-svc.internal:8080/admin/reload
```

Multi-route deployment (A/B or multi-model — see [ab-rollout.md](ab-rollout.md)) — the body names which route to reload:

```sh
curl -sS -X POST \
  -H 'Content-Type: application/json' \
  -d '{"model_version":"v1"}' \
  http://markup-svc.internal:8080/admin/reload
```

Expected response (200 on success):

```json
{
  "rule_count": 50,
  "model_version": "v1"
}
```

## What's happening

`internal/decider/swap.Decider` holds the active Decider behind a `sync.RWMutex` in a minimum-lock-hold shape: `Decide` acquires the read lock, copies the inner pointer to a local variable, releases the lock, then calls `inner.Decide`. A concurrent `Swap` is never blocked by engine work — it acquires the write lock just long enough to replace the inner pointer. In-flight Decides finish on their captured inner; new Decides starting after Swap returns observe the replacement. See [ADR-0008](../architecture/decisions/0008-hot-reload.md).

The reload handler reads the source path, parses it, builds a fresh Decider, and calls `holder.Swap(next)` only on success. If parsing fails, the handler returns `500` with an opaque body and the previous Decider keeps serving — a bad reload never partially-replaces the rule set.

In multi-route mode each route has its own swap holder so the v1 reload runs the v1 loader and swaps the v1 holder, leaving v2 untouched. The body's `model_version` field names which holder receives the swap.

## What to check after

- Reload response is 200 with the new `rule_count` and `model_version` (or 500 if parsing failed — in which case the server keeps serving the previous rule set).
- `/decide` returns the new `markup_factor` for a request that hits the edited rule:
  ```sh
  curl -sS -X POST -H 'Content-Type: application/json' \
    -d '{"customer_tier":"enterprise"}' \
    http://markup-svc.internal:8080/decide
  # markup_factor in the response should reflect the edit
  ```
- In-flight requests during the swap completed on the previous Decider (no errors spike).
- On failure: server log shows the loader error to stderr; HTTP response is `500 {"error":"reload failed"}` (opaque body — operator looks at logs for detail).
- Repeat reloads with no change are cheap — no rebuild if the source did not change, but the swap still increments the holder's underlying pointer once per call.

## Mistakes to avoid

- **Editing the file in-place during a multi-replica deployment** — the reload is per-replica. Coordinate the rollout across replicas (sequential reload, or push the new file via your config-management layer and reload everywhere after the file lands).
- **Forgetting the body in multi-route mode** — the empty body returns `400`. The single-route mode does not need a `model_version`; multi-route does.
- **Hot-reloading a `--snapshot` deployment with an updated CSV** — `--snapshot` cold-starts from the JSON file, not the CSV. Rebuild the snapshot first (see [snapshot-promotion.md](snapshot-promotion.md)) then reload.

## Relevant ADRs and flags

- [ADR-0008](../architecture/decisions/0008-hot-reload.md) — the swap holder + reload handler
- [ADR-0011](../architecture/decisions/0011-router.md) — per-route reload semantics in multi-route mode
- `POST /admin/reload` — the endpoint
