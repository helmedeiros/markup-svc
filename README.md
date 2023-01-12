# markup-svc

A dynamic markup service built on top of [bre-go](https://github.com/helmedeiros/bre-go). Demonstrates the four engine adapters side-by-side and ships HTTP endpoints, snapshot-backed model versions, A/B experiment routing, and OpenTelemetry instrumentation — all as a real consumer of the bre-go library.

## Status

[![CI](https://github.com/helmedeiros/markup-svc/actions/workflows/ci.yml/badge.svg)](https://github.com/helmedeiros/markup-svc/actions/workflows/ci.yml)
[![coverage](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/helmedeiros/markup-svc/badges/coverage.json)](https://github.com/helmedeiros/markup-svc/actions/workflows/ci.yml)

Released `v0.1.0`. 12 Accepted ADRs covering the domain port, the CSV rule format, the HTTP transport, all four engine adapters, snapshot persistence, hot reload, OpenTelemetry spans, the metrics port, the router (A/B + multi-model), and the scientific performance harness. The exported types on `internal/markup` plus the four adapter packages, the swap holder, the router, and the two observability decorators are committed surfaces — breaking changes carry a SemVer bump.

## Quickstart

```sh
go build -o markup-server ./cmd/markup-server
./markup-server --rules=cmd/markup-server/testdata/rules.csv --listen=:8080 --model=v1
```

In another shell:

```sh
curl -sS -X POST \
  -H 'Content-Type: application/json' \
  -H 'X-Correlation-ID: req-abc-123' \
  -d '{"customer_tier":"enterprise"}' \
  http://localhost:8080/decide
```

Response:

```json
{
  "markup_factor": 1.15,
  "rule": "enterprise",
  "model_version": "v1",
  "correlation_id": "req-abc-123",
  "engine_adapter": "*inmemory.Engine"
}
```

## bre-go capability matrix

Every capability the project set out to demonstrate against the [bre-go](https://github.com/helmedeiros/bre-go) engine library, and where it is wired in markup-svc:

| bre-go feature | markup-svc surface | Status |
|---|---|---|
| Four engine adapters (inmemory / firstmatch / priority / indexed) | `internal/decider/<adapter>` + `cmd/markup-server --adapter` dispatch | ✅ |
| CSV loader (`engine/csv`) | `internal/load.FromCSV` (typed `parser.Condition` pre-compiled at load) | ✅ |
| Typed `parser.Condition` tree | end-to-end; rules and snapshots both ride on the typed tree | ✅ |
| Correlation ID through context | `internal/httpapi.WithCorrelationID` middleware → `engine.WithCorrelationID` → every Decision | ✅ |
| JSON snapshot (`engine/indexed.Snapshot`) | `internal/snapshot` + `cmd/snapshot-build` + `cmd/markup-server --snapshot` | ✅ |
| Hot reload | `internal/decider/swap` + `POST /admin/reload` (per-route in multi-route mode) | ✅ |
| OTel span adapter (`observability/otel`) | `internal/observability/otel.Wrap` at the `markup.Decider` port + `--otel-enabled` | ✅ |
| Metrics port + decorator (`observability/metrics`) | `internal/observability/metrics` (library-only; operators wire their own `Sink`) | ✅ |
| A/B routing + multi-model | `internal/decider/router` + `--route` + `--policy` | ✅ |
| Compiled binary snapshot (`engine/indexed.CompiledSnapshot`) | not yet wired; the JSON snapshot is the v0.1.0 format | ⏳ deferred to its own ADR |

## Architecture

Hexagonal. `internal/markup.Decider` is the one-method port through which every HTTP handler asks for a markup decision. Concrete adapters wrap whichever bre-go engine fits the workload (inmemory / firstmatch / priority / indexed). The Decision return type carries provenance (rule, model version, experiment, correlation ID, engine adapter) so observability can slice by all four.

| Package | Role |
|---|---|
| `internal/markup` | domain port: `Request`, `Decision`, `Decider`, `ErrNoMatch`, `FactOf` |
| `internal/load` | CSV rule loader; produces `[]load.Rule` with pre-compiled bre-go `parser.Condition` |
| `internal/decider/inmemory` | `Decider` adapter wrapping `bre-go/engine/inmemory.Engine` (last-action-wins) |
| `internal/decider/firstmatch` | `Decider` adapter wrapping `bre-go/engine/firstmatch.Engine` (insertion-order first match) |
| `internal/decider/priority` | `Decider` adapter wrapping `bre-go/engine/priority.Engine` (highest-priority match, ties by insertion order) |
| `internal/decider/indexed` | `Decider` adapter wrapping `bre-go/engine/indexed.Engine` (sub-linear bucket lookup; same semantic as firstmatch) |
| `internal/httpapi` | HTTP transport: `Decide` handler + `WithCorrelationID` middleware + `Reload` admin handler |
| `internal/decider/swap` | atomic holder around `markup.Decider`; powers hot reload |
| `internal/decider/router` | routing decorator with `Route{ModelVersion, Variant, Decider}` + pluggable `Policy`; A/B variants and multi-model deployments |
| `internal/snapshot` | JSON snapshot format wrapping bre-go's indexed `Snapshot` + per-rule factors |
| `internal/observability/otel` | OpenTelemetry span decorator at the `markup.Decider` port |
| `internal/observability/metrics` | metrics port (`DecisionMetric` + `Sink`) + decorator at the `markup.Decider` port |
| `cmd/markup-server` | application: flag parsing + lifecycle |
| `cmd/snapshot-build` | offline CSV → snapshot JSON tool |

## Rule format

CSV with header `name,condition,factor,priority`. The `condition` column is an expression compiled by bre-go's parser. Use single-quoted string literals so quotes don't collide with CSV's own quoting rules:

```csv
name,condition,factor,priority
enterprise,customer_tier == 'enterprise',1.15,10
br_peak,country == 'BR' AND time_window == 'peak',1.08,5
```

See [ADR-0002](docs/architecture/decisions/0002-rule-format-csv.md) for the full rationale.

## HTTP contract

| Outcome | Status | Body |
|---|---|---|
| Decision found | `200` | `decideResponse` JSON |
| No rule matched (`ErrNoMatch`) | `404` | `{"error":"no rule matched"}` |
| Malformed JSON / empty body | `400` | `{"error":"malformed JSON body"}` |
| Non-`POST` method | `405` (`Allow: POST`) | `{"error":"method not allowed"}` |
| Internal error | `500` | `{"error":"internal"}` |

`X-Correlation-ID` flows in via header or is generated as UUID v4 and is echoed on every response. See [ADR-0003](docs/architecture/decisions/0003-http-decide-route.md).

## Hot reload

`POST /admin/reload` re-reads the boot-time source (`--rules` CSV or `--snapshot` JSON) from disk, builds a fresh `Decider`, and atomically swaps it into the active holder. In-flight `/decide` calls finish on their captured Decider; new calls land on the swapped one. On success: `200` with `{"rule_count": N, "model_version": "..."}`. On failure (parse error, build error): `500` with an opaque body and the previous Decider keeps serving. See [ADR-0008](docs/architecture/decisions/0008-hot-reload.md).

In multi-route mode (`--route` flag, below), the same endpoint accepts a body `{"model_version": "v1"}` naming which route to refresh; each route's holder is independent, so reloading one does not touch the others.

## Multi-route deployments (A/B + multi-model)

`cmd/markup-server` supports loading multiple rule sets as a single binary, dispatching each request to one of them via a routing policy. Useful for A/B experiments, model-version rollouts, and per-tenant rule sets.

```sh
./markup-server \
  --route=v1:control:rules:rules-control.csv \
  --route=v2:treatment:rules:rules-treatment.csv \
  --policy=hash-correlation \
  --listen=:8080
```

Each `--route` value is `model:variant:type:path` where `type` is `rules` or `snapshot`. `--policy` selects how Requests are dispatched:

| Policy | Behavior |
|---|---|
| `hash-correlation` (default) | FNV-1a hash of the `X-Correlation-ID` header → modulo route count. Same correlation ID always lands on the same route. When the header is absent (e.g., health probes), falls back to the first route. |
| `default` | Always picks the first route. Useful when the router is wired with one route (e.g., placeholder for future multi-route rollouts). |

The active route's `ModelVersion` and `Variant` are stamped onto every Decision's `model_version` and `experiment` fields, regardless of what the inner Decider writes — the router is the source of truth for routing labels. The metrics and OpenTelemetry decorators see the routing labels too, so dashboards can slice by `(adapter, model, experiment)` cleanly.

Per-route hot reload: `POST /admin/reload` with body `{"model_version": "v1"}` reloads only the v1 route's source from disk. See [ADR-0011](docs/architecture/decisions/0011-router.md).

## Performance

`scientific/v0.1.0/` ships a Docker-reproduced, pre-registered benchmark harness comparing the four adapters, the three decorators (swap / OTel / metrics), and cold-start cost (CSV vs snapshot). See [REPORT.md](scientific/v0.1.0/REPORT.md) for the v0.1.0 measurement table. Methodology and the falsifiable-bars discipline (bars committed before measurement, never moved post-commit) are documented in [ADR-0012](docs/architecture/decisions/0012-scientific-harness.md). Run the harness yourself with:

```sh
make scientific-v0.1.0
```

## Cookbook

Operator-level recipes for common deployments live under [`docs/cookbook/`](docs/cookbook/). Start with [deploy.md](docs/cookbook/deploy.md) for the production-readiness recipe, then pick the others as needed (A/B rollouts, hot reload, snapshot promotion, multi-model serving, observability wiring).

## Architecture Decision Records

- [ADR-0001](docs/architecture/decisions/0001-domain-port.md) — Domain port: `Decider` interface
- [ADR-0002](docs/architecture/decisions/0002-rule-format-csv.md) — Rule format: CSV with parser expressions
- [ADR-0003](docs/architecture/decisions/0003-http-decide-route.md) — HTTP transport: `POST /decide`
- [ADR-0004](docs/architecture/decisions/0004-firstmatch-adapter.md) — First-match Decider adapter
- [ADR-0005](docs/architecture/decisions/0005-priority-adapter.md) — Priority Decider adapter
- [ADR-0006](docs/architecture/decisions/0006-indexed-adapter.md) — Indexed Decider adapter (sub-linear lookup)
- [ADR-0007](docs/architecture/decisions/0007-snapshot-persistence.md) — Snapshot persistence for the indexed adapter
- [ADR-0008](docs/architecture/decisions/0008-hot-reload.md) — Hot reload via admin endpoint
- [ADR-0009](docs/architecture/decisions/0009-otel-spans.md) — OpenTelemetry spans at the Decider port
- [ADR-0010](docs/architecture/decisions/0010-metrics-port.md) — Metrics port at the Decider layer
- [ADR-0011](docs/architecture/decisions/0011-router.md) — Router decorator: A/B variants and multi-model routing
- [ADR-0012](docs/architecture/decisions/0012-scientific-harness.md) — Scientific performance comparison harness

## Quality gates

`make ci-local` runs `go vet`, race-detector tests, the coverage gate (80% floor), and the ADR-index check. Mirrors the CI workflow, which additionally computes the total coverage from `coverage.out`, posts it as a sticky PR comment on pull requests, and writes the live number to the orphan `badges` branch so the shields.io badge above auto-updates on every push to `main`.
