# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and the project follows [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- `internal/decider/firstmatch`: second concrete `markup.Decider` adapter, wraps bre-go's `engine/firstmatch.Engine`. Same `Rule` and `NewFromRules` shape as the inmemory adapter; semantics differ — insertion order is precedence and the first matching rule fires. Includes `TestSemanticDifferenceFromInmemory` pinning that the same `[]load.Rule` produces different `Decision.Rule` values through firstmatch vs inmemory.
- `internal/decider/priority`: third concrete adapter, wraps bre-go's `engine/priority.Engine`. `Rule` gains the `Priority int` field; `NewFromRules` finally consumes `load.Rule.Priority`. Higher Priority evaluates first, ties break by insertion order, and the adapter degenerates gracefully to firstmatch when all priorities are equal. Includes `TestSemanticDifferenceFromFirstmatch` and `TestPriorityZeroDegradesToFirstmatch` pinning both halves of the contract.
- `cmd/markup-server`: `--adapter` flag now accepts `inmemory` | `firstmatch` | `priority` (default `inmemory`); unknown names fail boot fast. `TestE2EThreeWayAdapterDivergenceOverHTTP` confirms over the HTTP wire that the three adapters produce three distinct Decisions for the same Request through the same engineered CSV.
- ADR-0004 (Accepted): first-match Decider adapter.
- ADR-0005 (Accepted): priority Decider adapter.

First usable build: `POST /decide` against a CSV-loaded inmemory `Decider`, with correlation IDs flowing through context to every Decision.

### Added

- `internal/markup` package: typed `Request`, `Decision`, `Decider` port interface, `ErrNoMatch` sentinel, `FactOf` converter producing the fact map bre-go's `parser.Condition` evaluates against. The column-to-field mapping is the single source of truth across every adapter.
- `internal/load` package: `FromCSV(io.Reader) ([]Rule, error)` reads CSVs with header `name,condition,factor,priority`; the condition column is compiled at load time by bre-go's `parser.ParseToCondition` into a typed `parser.Condition`. Per-row failures surface as `*LoadError` with the 1-indexed row number; `errors.Is`/`As` reach through.
- `internal/decider/inmemory` package: first concrete `markup.Decider` adapter, wraps `bre-go/engine/inmemory.Engine`. `New(rules []Rule, modelVersion string)` takes typed Go closures (lightweight for unit tests); `NewFromRules(rules []load.Rule, modelVersion string)` wraps each pre-compiled `parser.Condition` via `markup.FactOf` (production path). `Decide` returns `markup.ErrNoMatch` on miss and populates Decision provenance (Rule, ModelVersion, CorrelationID via `engine.CorrelationIDFromContext`, EngineAdapter via concrete type name).
- `internal/httpapi` package: `Decide(d markup.Decider) http.Handler` mounts the `POST /decide` route with unexported wire types (`decideRequest` / `decideResponse`) so JSON tags never bleed into the domain port. Error mapping: 200 (Decision), 400 (malformed / empty body), 404 (`ErrNoMatch`), 405 (`Allow: POST`), 500 (opaque). `WithCorrelationID` middleware reads `X-Correlation-ID` or generates a `crypto/rand`-backed UUID v4, injects via `engine.WithCorrelationID`, echoes on response.
- `cmd/markup-server` binary: thin `main()` over `run(ctx, args, stdout, stderr)`. Flags `--rules` / `--listen` / `--model`; signal-driven graceful shutdown with a 5s drain; `buildHandler(rules, modelVersion)` is the wiring seam exercised by end-to-end tests.
- ADR-0001 (Accepted): domain port — `Decider` interface for markup decisions.
- ADR-0002 (Accepted): rule format — CSV with bre-go parser expressions.
- ADR-0003 (Accepted): HTTP transport — `POST /decide` with internal JSON wire types and correlation ID middleware.
- Dependency: `github.com/helmedeiros/bre-go v0.19.0` (first integration).
- Makefile (lint / vet / test / cover / check-adrs / ci-local) with 80% coverage floor.
- GitHub Actions CI workflow uploading coverage to Codecov.
- Scripts: `check-adrs.sh` for ADR-index gate.
- README.md with quickstart, architecture table, HTTP contract table, ADR links, and CI + coverage badges.
- Project `.gitignore` (excludes `*.local.md`).

[Unreleased]: https://github.com/helmedeiros/markup-svc/compare/v0.0.1...HEAD
[0.0.1]: https://github.com/helmedeiros/markup-svc/releases/tag/v0.0.1
