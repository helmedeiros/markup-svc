# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and the project follows [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- `scientific/v0.1.0/`: cross-adapter, cross-decorator benchmark harness with pre-registered bars. Docker pins Linux/amd64 + Go 1.18 + the rule-set fixture for reproducibility. The harness measures per-`Decide` latency for every adapter, decorator overhead at each layer, cold-start cost (rules vs snapshot), and router overhead. Methodology per ADR-0012: each trial is one `-benchtime=1s` run; n=50 trials interleaved per `-count` pass so host noise hits every measurement equally; bars are committed before measurement and do not move once committed. The v0.1.0 measurement confirms all 11 absolute bars and all 4 ordinal bars pass on the reference fixture, including the surprising `ColdStart/rules < ColdStart/snapshot` at 50 rules (the JSON-decode allocations dominate the savings from skipping the parser at this rule-set size; the crossover is real and is the next harness's gap to close).
- `make scientific-v0.1.0` target wraps the Docker build + run so any operator can reproduce the same measurements with one command.
- ADR-0012 (Accepted): scientific performance comparison harness.

## [0.1.0] - 2022-12-16

First minor release. The `v0.0.x` line shipped the engine adapter matrix, observability decorators, snapshot persistence, and hot reload one piece at a time; `v0.1.0` adds the last shape the original premise promised — variant routing and multi-model deployments — and signals the first commitment toward API stability. The exported types in `internal/markup` (Request, Decision, Decider, ErrNoMatch, FactOf), the four adapter packages, the swap holder, both observability decorators, and the router are now considered stable; breaking changes carry a SemVer bump.

### Added

- `internal/decider/router`: routing decorator at the `markup.Decider` port. `Route{ModelVersion, Variant, Decider}` + `Policy` interface + `Router` with `Decide` that picks a Route via the policy, dispatches to the inner Decider, and stamps `Decision.ModelVersion` + `Decision.Experiment` from the chosen Route post-Decide (the router is the source of truth for routing labels). Ships `HashCorrelationPolicy` (sticky-by-correlation-ID, FNV-1a hash inline so the policy path is allocation-free), `HashFieldPolicy` (sticky-by-Request-field via a closure), and `DefaultPolicy` (always first route). Routing failures surface as `ErrNoRoute`, distinct from `markup.ErrNoMatch` because the observability semantics differ (server-side problem vs domain miss).
- `cmd/markup-server` `--route` repeatable flag (`model:variant:type:path`, `type` is `rules|snapshot`) plus `--policy={hash-correlation|default}`. Router mode is mutually exclusive with `--rules`/`--snapshot`. Each route is wrapped in its own `swap.Decider` holder so `POST /admin/reload` with body `{"model_version":"v1"}` reloads only that route — other routes are untouched. `TestE2ERouterAsymmetryOverHTTP` proves two distinct correlation IDs land on different routes with different stamped `model_version`/`experiment`/`markup_factor` over the wire; `TestE2ERouterPerRouteReloadOverHTTP` proves a v1 reload swaps v1's holder while v2's holder still serves its original factor.
- ADR-0011 (Accepted): router decorator for A/B variants and multi-model routing. The "per-route hot reload" item in NOT-closed was delivered in the same release window.
- README: Multi-route deployments section with quickstart, `--route` format, `--policy` table including fallback behaviour, and complete ADR list (0001-0011).

## [0.0.4] - 2022-12-02

Observability lands. The Decider port gains two stackable decorators — one for OpenTelemetry spans, one for typed metric events. Both classify outcomes the same way so dashboards can pair span attributes with metric counters without divergence.

### Added

- `internal/observability/otel`: markup-domain OpenTelemetry decorator at the `markup.Decider` port. `Wrap(inner, tracer, opts...)` emits one span per `Decide` with `rule.markup.*` attributes (`adapter`, `model_version`, `rule`, `factor`, `correlation_id`). `ErrNoMatch` lands as `rule.markup.no_match=true` with span status OK (a domain outcome, not an error). `context.Canceled` and `context.DeadlineExceeded` use `rule.markup.canceled` / `rule.markup.cancel.reason`, again without `codes.Error`, so caller-driven cancellation does not inflate server-side error-rate dashboards. Other errors get `codes.Error` + `RecordError`. `WithSpanName` option overrides the default name `markup.decider.decide`.
- `cmd/markup-server` `--otel-enabled` flag (default off). When set, `wireTracedHandler` composes `otel.Wrap` *outside* the `swap.Decider` holder so spans continue to be emitted across hot reloads (`TestE2EOTelSpansContinueAfterReload` pins this composition). The reload route keeps calling `holder.Swap` directly.
- `internal/observability/metrics`: markup-side metrics port. `DecisionMetric` value type (`Adapter`, `ModelVersion`, `Rule`, `MarkupFactor`, `CorrelationID`, `Duration`, plus `NoMatch` / `Err` / `Canceled` / `CancelReason` mutually-exclusive outcome flags), single-method `Sink` interface, and `Wrap(markup.Decider, Sink)` decorator that emits one event per `Decide`. Symmetric to the OTel decorator's outcome classification so the two stack cleanly: recommended order is `metrics.Wrap(otel.Wrap(swap.New(inner)))` so the metric `Duration` captures end-to-end Decider cost. `RecordingSink` ships as the test-only aggregator; production sinks (Prometheus, OTel metrics) are operator-supplied. `TestComposesWithOTelDecorator` proves both decorators stack without interference; `TestDecisionMetricFieldSetInvariants` pins the field-set mutual-exclusivity rules across every outcome.
- ADR-0009 (Accepted): OpenTelemetry spans at the Decider port.
- ADR-0010 (Accepted): metrics port at the Decider layer.
- Dependency: `go.opentelemetry.io/otel v1.11.2` plus `otel/trace` and `otel/sdk` (test-only). Pinned to bre-go's OTel version so transitive deps dedupe.

## [0.0.3] - 2022-11-18

Snapshot persistence and hot reload land together. Rule sets can now be compiled offline into a JSON snapshot and cold-started faster than from CSV; running servers can swap in a fresh rule set via `POST /admin/reload` without a process restart.

### Added

- `internal/snapshot`: markup-side wrapper around bre-go's `engine/indexed.Snapshot` that carries per-rule factors so `Action` closures rebuild correctly. `Build` / `Write` / `Read` / `LoadIntoIndexedDecider` plus `ErrFormatVersionMismatch` and `ErrMissingFactor` sentinels. The format-version mismatch refuses older binaries from loading newer snapshots; the missing-factor sentinel ensures a snapshot whose `Factors` map omits a rule listed in the engine snapshot fails at load rather than serving silent zero-factor Decisions.
- `cmd/snapshot-build`: standalone CLI that reads a CSV via `load.FromCSV`, builds an indexed `snapshot.Snapshot`, and writes it as JSON. Usage: `snapshot-build --rules=rules.csv --model=v1 --out=snapshot.json`.
- `cmd/markup-server` `--snapshot` flag, mutually exclusive with `--rules`. Cold-starts the indexed `Decider` directly from a snapshot JSON, skipping CSV parsing and `parser.ParseToCondition`. When `--snapshot` is set, the snapshot's `ModelVersion` overrides the `--model` flag and `--adapter` is ignored (snapshots are indexed-only). `TestE2ESnapshotPathOverHTTP` confirms the cold-start path returns Decisions stamped with the snapshot's ModelVersion and `*indexed.Engine` adapter slice.
- `internal/decider/indexed.NewFromEngine`: snapshot-loader-only constructor that wraps an externally-built `*indexed.Engine` behind the `markup.Decider` port; production callers continue to use `New` / `NewFromRules`.
- `internal/decider/swap`: `swap.Decider` holder around `markup.Decider` backed by `sync.RWMutex`. Minimum-lock-hold shape — `RLock`, copy inner pointer, `RUnlock`, then dispatch — so a concurrent `Swap` is never blocked by in-flight engine work; in-flight `Decide`s finish on their captured inner. The holder itself satisfies the `markup.Decider` port so callers depend on the same abstraction. `TestSwapUnderConcurrentDecide` proves the race-detector-clean property under 16 readers × 500 calls + 50 swaps; `TestPreSwapDecideRunsOnCapturedInner` proves the captured-inner guarantee with a blocking inner.
- `internal/httpapi.Reload`: `POST /admin/reload` handler that invokes a `Loader` closure to build a fresh `Decider`, swaps it into the holder on success, returns `200` with `{rule_count, model_version}` JSON. Loader errors map to `500` with an opaque body (loader closure surfaces detail via stderr). Non-`POST` returns `405` with `Allow: POST`.
- `cmd/markup-server` mounts `/admin/reload` alongside `/decide` under the same correlation middleware. `snapshotLoader` and `rulesLoader` closures own the boot-time-capturing load logic; `wireHandler` is the composition seam. `TestE2EReloadChangesDecisionsOverHTTP` confirms a reload after editing the CSV on disk changes the next `/decide`'s `MarkupFactor`; `TestE2EReloadFailureKeepsOldDecider` confirms a failed reload returns `500` and the previous Decider keeps serving.
- ADR-0007 (Accepted): snapshot persistence for the indexed adapter.
- ADR-0008 (Accepted): hot reload via admin endpoint.

## [0.0.2] - 2022-11-04

All four bre-go engine adapters now ship as concrete `markup.Decider` implementations. The `--adapter` flag selects which engine answers each Decision; the same CSV produces predictable Decision matrices across the four adapters per the new `TestE2EFourAdapterMatrixOverHTTP` proof.

### Added

- `internal/decider/firstmatch`: second concrete `markup.Decider` adapter, wraps bre-go's `engine/firstmatch.Engine`. Same `Rule` and `NewFromRules` shape as the inmemory adapter; semantics differ — insertion order is precedence and the first matching rule fires. Includes `TestSemanticDifferenceFromInmemory` pinning that the same `[]load.Rule` produces different `Decision.Rule` values through firstmatch vs inmemory.
- `internal/decider/priority`: third concrete adapter, wraps bre-go's `engine/priority.Engine`. `Rule` gains the `Priority int` field; `NewFromRules` finally consumes `load.Rule.Priority`. Higher Priority evaluates first, ties break by insertion order, and the adapter degenerates gracefully to firstmatch when all priorities are equal. Includes `TestSemanticDifferenceFromFirstmatch` and `TestPriorityZeroDegradesToFirstmatch` pinning both halves of the contract.
- `internal/decider/indexed`: fourth concrete adapter, wraps bre-go's `engine/indexed.Engine`. `Rule.Match` is a typed `parser.Condition` (no closure) because the indexer inspects the AST to bucket each rule. Semantics match firstmatch (insertion-order precedence) but per-Decide cost is sub-linear: O(K) hash lookups instead of O(rules) linear scan. `New` calls the engine's `Build()` synchronously so seal-time errors surface at construction. Includes `TestSemanticEquivalenceWithFirstmatch` (the safety net for the optimization) and `TestNewFromRulesRejectsNonIndexableCondition` (fail-fast at construction).
- `cmd/markup-server`: `--adapter` flag now accepts `inmemory` | `firstmatch` | `priority` | `indexed` (default `inmemory`); unknown names fail boot fast. `TestE2EFourAdapterMatrixOverHTTP` confirms over the HTTP wire that the four adapters produce the expected Decision matrix (three distinct rules; indexed agrees with firstmatch by design).
- ADR-0004 (Accepted): first-match Decider adapter.
- ADR-0005 (Accepted): priority Decider adapter.
- ADR-0006 (Accepted): indexed Decider adapter.

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

[Unreleased]: https://github.com/helmedeiros/markup-svc/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/helmedeiros/markup-svc/compare/v0.0.4...v0.1.0
[0.0.4]: https://github.com/helmedeiros/markup-svc/compare/v0.0.3...v0.0.4
[0.0.3]: https://github.com/helmedeiros/markup-svc/compare/v0.0.2...v0.0.3
[0.0.2]: https://github.com/helmedeiros/markup-svc/compare/v0.0.1...v0.0.2
[0.0.1]: https://github.com/helmedeiros/markup-svc/releases/tag/v0.0.1
