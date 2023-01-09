# Deploy markup-svc to production

## Problem

You want to run `markup-svc` against a rule set in a container, behind a load balancer, serving `POST /decide` to upstream callers. Single-route, single-model. No A/B yet.

## Recipe

Build the binary:

```sh
go build -o markup-server ./cmd/markup-server
```

Pick a rule source. CSV first (faster to iterate, slower to cold-start at large sizes); snapshot if startup parsing has become a deploy-time pain (see [snapshot-promotion.md](snapshot-promotion.md)).

Run with explicit flags:

```sh
./markup-server \
  --rules=/etc/markup/rules.csv \
  --listen=:8080 \
  --model=v1 \
  --adapter=indexed
```

| Flag | Why this value |
|---|---|
| `--rules` | Mount your CSV at a stable container path so a rule update is a file replacement, not a binary push. |
| `--listen` | `:8080` for the cluster's internal port. Behind a reverse proxy that terminates TLS / handles routing. |
| `--model` | Stamp every Decision with this version tag so observability slices by model are real. Tag bumps belong with rule updates, not rolling restarts. |
| `--adapter=indexed` | The sub-linear adapter. For rule sets > ~100 rules where every request looks up a couple of fields, indexed is the right default. Smaller rule sets are fine on `firstmatch` or `inmemory` (see ADRs 0004 / 0005 / 0006). |

Quick smoke test from inside the cluster:

```sh
curl -sS -X POST -H 'Content-Type: application/json' \
  -H 'X-Correlation-ID: smoke-1' \
  -d '{"customer_tier":"enterprise","country":"BR"}' \
  http://markup-svc.internal:8080/decide
```

Expected response shape:

```json
{
  "markup_factor": 1.15,
  "rule": "enterprise",
  "model_version": "v1",
  "correlation_id": "smoke-1",
  "engine_adapter": "*indexed.Engine"
}
```

## What's happening

`cmd/markup-server` parses the CSV via `internal/load`, builds the chosen adapter's `Decider`, wraps it in a `swap.Decider` holder (for hot reload — see [hot-reload.md](hot-reload.md)), mounts `/decide` and `/admin/reload` behind the correlation-ID middleware, and serves until SIGTERM. SIGTERM triggers a graceful shutdown with a 5-second drain so in-flight requests finish on the existing Decider.

`X-Correlation-ID` rides through context via `engine.WithCorrelationID`; the middleware accepts the caller's header or generates a UUID v4 if absent. Every Decision carries the same ID so logs and dashboards can pivot on it. See [ADR-0003](../architecture/decisions/0003-http-decide-route.md).

## What to check after

- Startup log line names the chosen adapter and the rule count: `markup-server: listening on :8080 (50 rules, model v1, adapter indexed, source /etc/markup/rules.csv)`.
- Smoke `/decide` returns 200 with the expected `engine_adapter` field (matches the `--adapter` flag).
- A bad-rule-set CSV fails boot fast (the binary exits non-zero before the listener opens). `--rules=/path/to/broken.csv` → `markup-server: parse rules "/path/to/broken.csv": ...`.
- SIGTERM exits cleanly: `markup-server: shutting down` then process exit. In-flight requests finish.

## Relevant ADRs and flags

- [ADR-0001](../architecture/decisions/0001-domain-port.md) — domain port
- [ADR-0002](../architecture/decisions/0002-rule-format-csv.md) — CSV rule format
- [ADR-0003](../architecture/decisions/0003-http-decide-route.md) — HTTP transport
- [ADR-0006](../architecture/decisions/0006-indexed-adapter.md) — why `indexed` is the recommended default
- `./markup-server --help` for the full flag list
