# markup-svc

A dynamic markup service built on top of [bre-go](https://github.com/helmedeiros/bre-go). Demonstrates the four engine adapters side-by-side and ships HTTP endpoints, snapshot-backed model versions, A/B experiment routing, and OpenTelemetry instrumentation — all as a real consumer of the bre-go library.

## Status

[![CI](https://github.com/helmedeiros/markup-svc/actions/workflows/ci.yml/badge.svg)](https://github.com/helmedeiros/markup-svc/actions/workflows/ci.yml)

Pre-release. ADR-0001 (Proposed) defines the domain port.

## Architecture

Hexagonal. `internal/markup.Decider` is the one-method port through which every HTTP handler asks for a markup decision. Concrete adapters wrap whichever bre-go engine fits the workload (inmemory / firstmatch / priority / indexed). The Decision return type carries provenance (rule, model version, experiment, engine adapter) so observability can slice by all four.

## Layout

```
cmd/markup-server/      # HTTP service entry point
internal/markup/        # domain types: Request, Decision, Decider
docs/architecture/      # ADRs
```

## Quality gates

`make ci-local` runs `go vet`, race-detector tests, coverage gate, and the ADR-index check. Mirrors the CI workflow.
