# markup-svc

A dynamic markup service built on top of [bre-go](https://github.com/helmedeiros/bre-go). Demonstrates the four engine adapters side-by-side and ships HTTP endpoints, snapshot-backed model versions, A/B experiment routing, and OpenTelemetry instrumentation — all as a real consumer of the bre-go library.

## Status

[![CI](https://github.com/helmedeiros/markup-svc/actions/workflows/ci.yml/badge.svg)](https://github.com/helmedeiros/markup-svc/actions/workflows/ci.yml)
[![coverage](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/helmedeiros/markup-svc/badges/coverage.json)](https://github.com/helmedeiros/markup-svc/actions/workflows/ci.yml)

Pre-release. Three accepted ADRs: the domain port, the CSV rule format, and the HTTP transport. The first usable build serves `POST /decide` against a CSV-loaded inmemory `Decider`.

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

## Architecture Decision Records

- [ADR-0001](docs/architecture/decisions/0001-domain-port.md) — Domain port: `Decider` interface
- [ADR-0002](docs/architecture/decisions/0002-rule-format-csv.md) — Rule format: CSV with parser expressions
- [ADR-0003](docs/architecture/decisions/0003-http-decide-route.md) — HTTP transport: `POST /decide`

## Quality gates

`make ci-local` runs `go vet`, race-detector tests, the coverage gate (80% floor), and the ADR-index check. Mirrors the CI workflow, which additionally computes the total coverage from `coverage.out`, posts it as a sticky PR comment on pull requests, and writes the live number to the orphan `badges` branch so the shields.io badge above auto-updates on every push to `main`.
