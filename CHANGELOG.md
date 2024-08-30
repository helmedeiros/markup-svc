# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and the project follows [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- ADR-0031 Accepted: Shadow admin surface — load and clear a challenger Decider. New `--shadow-admin` flag mounts `POST /admin/load-challenger` (reuses the ADR-0030 body-loader contract, runs Diagnose, returns the same `{rule_count, model_version}` envelope on success and the ADR-0026 `{error, healthy:false, errors, warnings}` envelope on Diagnose failure) and `DELETE /admin/challenger` (idempotent 204). The challenger lives in a new nil-aware `internal/decider/shadow.Holder` with `Load`/`Clear`/`Get`; the handler layer accepts a `ChallengerHolder` interface so the port package stays free of the adapter import. Off by default; when off, no new routes register and no challenger Holder is allocated. The compiled binary carries the new package's type metadata so it is not byte-identical to a pre-flag build, but the runtime behaviour on `/decide` is observably unchanged. First step of the shadow-Decider arc; subsequent ADRs add `/decide` consultation, registry-side push, observability surfaces, and mrctl ergonomics.

## [0.1.21] - 2023-06-21

CI fix for v0.1.19/v0.1.20 + completion of ADR-0030's test commitment.

### Fixed

- `internal/httpapi/reload_body_test.go` used `atomic.Int32` which is Go 1.19+. Switched to `atomic.AddInt32`/`atomic.LoadInt32` against an `int32` field to keep the repo's Go 1.18 baseline.

### Added

- The two cmd-level integration tests that ADR-0030 committed to but were missing from v0.1.19's commit: `TestE2EBootsWithBodyLoader_EmptyBodyReloadUnchanged` (bit-for-bit canary that wiring the body-loader does not change the empty-body file-path behaviour) and `TestE2EBodyBasedReload_CSVHappyPath` (POST-CSV-in-body → /decide returns new factor).

## [0.1.20] - 2023-06-19

CI fix. v0.1.19 used `http.MaxBytesError` which is Go 1.19+; the repo's Go baseline is 1.18. Switched to a string-equality check against the stable `"http: request body too large"` error message. Source semantics of v0.1.19 are otherwise unchanged.

## [0.1.19] - 2023-06-16

Body-based `/admin/reload`. A control plane (or any HTTP client) can now push rule sets to running markup-svc instances by POSTing the new rules in the request body — no ConfigMap mutation, no kubelet sync window, no Kubernetes RBAC. Closes ADR-0030.

### Added

- `internal/httpapi.ReloadBodyLoader` interface (Supports + Load) and `WithReloadBodyLoader` option on the existing `Reload` handler. When the request body is non-empty AND the loader's Supports returns true for the request's normalized media type, the handler dispatches the body to the loader, runs format-appropriate validation, and swaps the Decider on success. Empty bodies, unrecognized media types, and absent body-loader configuration all fall through to the existing file-based path bit-for-bit unchanged.
- `internal/httpapi.DiagnoseRejectedError` typed error so body-loader implementations can carry a Diagnose result back to the handler; the handler unwraps and reuses the existing `writeDiagnoseRejection` helper so the wire shape matches ADR-0026's file-path rejection exactly.
- `cmd/markup-server.bodyLoader` struct implementing `ReloadBodyLoader`. Supports `text/csv` (parsed via `load.FromCSV`, Diagnose'd, built with the boot-time adapter) and `application/json` (parsed via `snapshot.Read`, loaded via `LoadIntoIndexedDecider`). Wired unconditionally so every binary built from this tag supports body-based reload when the request shape matches; no operator-facing flag.
- 12 unit tests in `internal/httpapi/reload_body_test.go` covering happy paths, charset-normalization via `mime.ParseMediaType`, `InvalidRuleSetError` + `DiagnoseRejectedError` mappings, fall-through paths (empty body, no Content-Type, unrecognized Content-Type, no body-loader), the `BodyLoaderWired_EmptyBody_StillHitsFilePath` canary, and the 16 MB body cap returning 413.
- `scientific/v0.1.19/` harness with three pre-registered benchmarks (`BenchmarkReload_EmptyBody`, `BenchmarkReload_CSVBody_100Rules`, `BenchmarkReload_CSVBody_10kRules`) and a REPORT.md committed alongside the code. Bars + pilot results in REPORT.md per the ADR-0012 protocol.

### Changed

- `cmd/markup-server.wireTracedHandler` signature gains a `ReloadBodyLoader` parameter (passed through from boot). `wireHandler` (test helper) passes nil. Existing wire callers in tests updated.
- The /admin/reload handler's existing semantics — 405 on non-POST, 400 on Diagnose-unhealthy or InvalidRuleSetError, 500 on loader error, 200 with `ReloadResult` JSON on success — are preserved bit-for-bit on the file-based path. The body-based path uses the same status-code mapping.

### Pairs with

The control plane that operates this surface is a separate project; the data-plane contract in this release is what every consumer of body-based reload depends on. JSON snapshot path's Diagnose-skip is acknowledged as a security delta in ADR-0030's Not Closed; mitigated by the same network-level controls that protect the admin endpoint today.

## [0.1.18] - 2023-05-26

Admin handlers (`/admin/reload`, `/admin/diagnose`, `/admin/guardrails`) emit OTel SERVER spans so markup-svc shows up in admin trace waterfalls alongside `/decide`. Closes ADR-0028.

### Added

- `internal/httpapi/adminspan.go`: new `WithAdminSpan(tracer, spanName, next)` middleware. SpanKindServer; HTTP-semantic attributes (`http.method`, `http.target`, `http.status_code`); reuses `mkotel.AttrCorrelationID` from the existing OTel package. 5xx → span Error; 4xx stays OK per ADR-0027. tracer == nil is a no-op.
- Six unit tests in `internal/httpapi/adminspan_test.go` covering span emit / status / 5xx error / 4xx OK / correlation_id stamp / nil tracer / parent-context propagation via `tracetest.SpanRecorder`.

### Changed

- `cmd/markup-server` wraps each admin handler (in both single-route and router modes) with `WithAdminSpan`. Span names: `markup.admin.reload`, `markup.admin.diagnose`, `markup.admin.guardrails`.
- `guardrailsWire.mountAdmin` signature changes from `func(*http.ServeMux)` to `func(*http.ServeMux, func(http.Handler) http.Handler)` so the cmd can inject the span wrapper at mount time without `buildGuardrailsWiring` needing to know about the tracer.
- `TestE2EOTelSpansContinueAfterReload` updated to expect 5 spans (was 4) including the new `markup.admin.reload`.

### Operator-visible

A page from `AdminHotReloadRejected` now produces a Jaeger trace with all three services (traffic-gen → decision-gateway → markup-svc) instead of ending at the gateway. The on-call sees markup-svc-side timing, the correlation_id, and the HTTP status code for the rejection.

## [0.1.17] - 2023-05-19

`/admin/reload` and `/admin/diagnose` return 400 (was 500) when the rules CSV is malformed. Closes ADR-0027.

### Changed

- `internal/markup`: new `InvalidRuleSetError` type wrapping `Path` + underlying `Err` via `Unwrap`.
- `cmd/markup-server` `loadRulesFromFile`: wraps `load.FromCSV` failures with `&markup.InvalidRuleSetError{...}`. File-open failures stay as plain wrapped errors (5xx).
- `internal/httpapi/reload.go` + `diagnose.go`: shared `statusForLoadErr(err)` dispatches via `errors.As`. `InvalidRuleSetError` → 400; everything else stays 500.
- Existing E2E `TestE2EReloadFailureKeepsOldDecider` now asserts 400 with a reference to ADR-0027. Two new unit tests cover the explicit `InvalidRuleSetError` path on both endpoints.

### Operator-visible

A reload posted against a malformed CSV returns 400 with body `{"error":"reload failed"}`. The previously-loaded rules keep serving (same fail-closed posture as ADR-0026's Diagnose gate). Clients see "stop retrying" semantics.

### Pairs with

pricing-observability follow-up: `AdminHotReloadRejected` re-narrows from `status=~"[45].."` to `status=~"4.."` now that parser-error reloads return 400.

## [0.1.16] - 2023-05-04

`/admin/reload` is gated on Diagnose. A bad rule set posted via hot-reload no longer reaches customer traffic — the swap is rejected with 400 + JSON of the issues; the currently-serving rules stay active. Closes ADR-0026.

### Added

- `httpapi.ReloadOption` + `httpapi.WithReloadDiagnose(fn DiagnoseFn)` — options form on `Reload`. When set, runs `Diagnose` before the swap.
- 400 response body shape on rejection: `{healthy: false, errors: [{kind, rule, detail}], warnings: [...]}`. Matches the `/admin/diagnose` GET shape so operators have one filter to remember.
- ADR-0026.

### Changed

- `httpapi.Reload` signature gains a variadic `opts ...ReloadOption`. Backwards compatible — existing two-arg call sites continue to work unchanged.
- cmd wires `WithReloadDiagnose(diagnoseFn)` when `--rules` mode is active.

### K8s safety story

Boot path (ADR-0025) and hot-reload path (this ADR) are now both fail-closed. A pod gone wrong via either path stays out of the customer's way — boot via `/readyz` not flipping ready, hot-reload via the 400 rejection keeping the old rules active.

## [0.1.15] - 2023-05-01

`Diagnose()` + port-level sentinels. The bre-go capability matrix's two long-deferred startup-validation items ship together. Closes the "broken rule set silently returns ErrNoMatch for an hour before anyone notices" class of failure. Closes ADR-0025.

### Added

- `internal/markup`: `IssueKind` (six sentinels: `empty_rule_set`, `duplicate_name`, `duplicate_priority`, `invalid_factor`, `no_op_factor`, `empty_condition`), `Severity`, `Issue`, `Diagnosis` types with `Errors()` / `Warnings()` / `IsHealthy()`.
- `internal/load.Diagnose([]Rule) markup.Diagnosis` — adapter-agnostic checks; same sentinel set regardless of which adapter loads the rules.
- `internal/httpapi.Diagnose(fn DiagnoseFn) http.Handler` — `GET /admin/diagnose` returns the current Diagnosis as JSON with status 200 when healthy, 503 when not (curl-driven CI gates fail on a broken rule set without parsing the body).
- `cmd --diagnose=on|warn|off` (default `on`): runs Diagnose at boot, fails on errors in `on` mode, logs all issues as `markup-server.diagnose` JSON events with severity → log-level mapping.
- ADR-0025.

### Changed

- `wireTracedHandler` signature gains a `DiagnoseFn` parameter (nil in test paths).

## [0.1.14] - 2023-04-17

`markup_decide_duration_seconds` switches from `prometheus.DefBuckets` (5 ms–10 s) to a 14-bucket sub-millisecond set covering 50 µs–1 s. Measured Decide latency lives in 10–100 µs; the default buckets put everything in the first bucket and the Grafana p50/p95/p99 panels rendered flat. Closes ADR-0024.

### Changed

- `internal/observability/metrics/prom`: histogram `Buckets` field uses a new `decideBuckets` slice.

## [0.1.13] - 2023-04-13

`markup-server.access` events now carry the matched rule, the input fields, and the rule output. Operators searching Kibana for "every request that matched `enterprise`" or "what inputs drove a 1.5 factor" no longer have to pivot to Jaeger. Closes ADR-0023.

### Added

- `attrs.rule`, `attrs.markup_factor`, `attrs.model_version`, `attrs.engine_adapter`, optional `attrs.experiment` on the access event when a rule matched.
- `attrs.no_match: true` on the no-rule-matched path.
- `attrs.input.*` with only the Request fields actually set (zero values omitted to keep events terse).
- ADR-0023.

### Changed

- `httpapi.Decide` populates the request context with `{request, decision}` (or `no_match=true`) so `WithAccessLog` can merge them at the end of the frame.

## [0.1.12] - 2023-04-06

Patch. v0.1.11's image build failed because `go mod tidy` pulled `x/sys@v0.45.0` (requires Go 1.21) and `x/text@v0.37.0` transitively. The golang:1.18 base image in the Dockerfile rejected the resulting `go.mod`. Pinned `x/net@v0.8.0`, `x/sys@v0.6.0`, `x/text@v0.8.0` — all early-2023 versions compatible with Go 1.18. Source code unchanged from v0.1.11 (same h2c feature). The v0.1.12 image is the one operators should use.

## [0.1.11] - 2023-04-05

h2c on the server. The HTTP listener now handles HTTP/1.1 + HTTP/2-over-cleartext from the same port. HTTP/1.1 clients keep working unchanged; HTTP/2 clients (the gateway in its next release) get connection multiplexing — many in-flight requests share one TCP connection. Closes ADR-0022.

### Added

- `golang.org/x/net/http2` + `h2c` direct imports; the indirect `x/net` dep is bumped to `v0.8.0` (still Go-1.18-compatible).
- `cmd/markup-server`: `http.Server.Handler` wrapped with `h2c.NewHandler(handler, &http2.Server{})`.
- Smoke test asserting `resp.ProtoMajor == 2` against an `http2.Transport{AllowHTTP: true}` client.
- ADR-0022.

## [0.1.10] - 2023-04-03

Structured JSON logging. Closes the only remaining log-shape gap in the platform: markup-svc previously emitted plain text on stdout, Filebeat ingested it under `message` instead of `attrs.*`, Kibana queries on `attrs.correlation_id` worked for traffic-gen + decision-gateway but not markup-svc. v0.1.10 brings it to parity. Closes ADR-0021.

### Added

- `internal/jsonlog`: small thread-safe `Logger` emitting `{time, level, msg, attrs}` JSON lines.
- `internal/httpapi.WithAccessLog`: middleware emitting one `markup-server.access` event per request with `method / path / status / duration_ms / correlation_id / trace_id / span_id` attrs (omitted when absent). Nil logger short-circuits to pass-through.
- ADR-0021 (Accepted).

### Changed

- Boot line `markup-server: listening on ...` → `markup-server.boot` JSON event with attrs `{listen, rules, model, adapter, source}`.
- Shutdown line → `markup-server.shutdown` event.
- Composition order: `WithCorrelationID(WithTraceContext(WithAccessLog(log, mux)))` so the access middleware sees the correlation + trace context the outer middleware extracted.

## [0.1.9] - 2023-03-31

SpanKind=Server on the outermost `markup.decider.decide` span. Unlocks markup-svc in Jaeger's Monitor tab (Service Performance Monitoring): the previous all-Internal span tree was correctly skipped by Jaeger's SPM aggregator (which filters to SERVER+CONSUMER), so the Monitor view rendered empty even when traffic was flowing. The inner `markup.guardrails.check` + `markup.engine.evaluate` spans correctly stay Internal — they're intra-process decorators, not service boundaries. Closes ADR-0020.

### Added

- `internal/observability/otel.WithSpanKind(trace.SpanKind)` Option on the existing `Wrap`. Matches the existing `WithSpanName` pattern; preserves the default (Internal) so all existing call sites' behavior is unchanged.
- ADR-0020 (Accepted): SpanKind=Server on the outer Decide span. One design question answered: how to expose SpanKind on Wrap (pick Option pattern; matches `WithSpanName`, preserves backwards compatibility, minimal change-set).

### Changed

- `cmd/markup-server/main.go`: the outermost `mkotel.Wrap(...)` for `markup.decider.decide` now passes `WithSpanKind(trace.SpanKindServer)`. The inner two wraps keep their existing `WithSpanName(...)` only.

### Operator-visible

After deploying v0.1.9 with the live pricing-observability v0.0.5+ stack: Jaeger Monitor tab → service: markup-svc → RED panels populate (calls/min, error rate, p50/p95/p99 latency). Combined with decision-gateway + traffic-gen, all three platform services have working SPM out of the box.

## [0.1.8] - 2023-03-29

Metrics phase release. Closes the wrapper-main gap that ADR-0010 left for the metrics decorator: the published `markup-svc:v0.1.8` binary exposes Prometheus exposition at `/metrics` when `--metrics-enabled` is set, with no operator-side derivation required. Pricing-observability's deferred-from-v0.0.1 metrics phase consumes this endpoint. Closes ADR-0019.

### Added

- `internal/observability/metrics/prom`: new subpackage. `prom.New() (*Sink, http.Handler)` constructs a private `prometheus.Registry`, registers a `markup_decide_total` counter + `markup_decide_duration_seconds` histogram (both labeled by `adapter` / `model_version` / `outcome`), and returns the Sink + a `promhttp.HandlerFor(reg)` against the same registry. The Sink implements the existing `metrics.Sink` port from ADR-0010 with no contract changes there.
- `cmd/markup-server` flag `--metrics-enabled`. When set, the cmd wires `prom.New()` into a new private `metricsWiring` struct passed through both `wireRouterHandler` and `wireTracedHandler` (matching the existing `guardrailsWire` pattern). The metrics decorator wraps the Decider OUTERMOST per ADR-0010's recommended order so `Duration` captures end-to-end Decider cost including tracing overhead.
- `/metrics` HTTP route mounted on the same listener as `/decide` when the flag is set.
- ADR-0019 (Accepted): Prometheus Sink + /metrics endpoint. Three design questions answered: bundle Sink + handler vs return separately (pick bundle — one call, no operator-side `promhttp.HandlerFor` wiring); labels (pick adapter + model_version + outcome — low-cardinality dimensions, ~120 series total, rule + correlation_id stay in Jaeger); histogram buckets (pick `prometheus.DefBuckets` — consistent with Prometheus ecosystem, custom sub-ms buckets land as follow-up when dashboard motivates).

### Dependencies

- `github.com/prometheus/client_golang v1.14.0` (the version line compatible with Go 1.18). Transitive: `github.com/prometheus/common`, `github.com/prometheus/client_model`, `github.com/prometheus/procfs`, `github.com/cespare/xxhash/v2`, `github.com/golang/protobuf`, `github.com/matttproud/golang_protobuf_extensions`.

### Performance impact

`--metrics-enabled` off: zero ns delta vs v0.1.7. The decorator is not wrapped; the `/metrics` route is not mounted; no Prometheus code is reached.

`--metrics-enabled` on: ~50-200 ns per Decide added (one `time.Now()` + `time.Since()` from the metrics decorator + one labels-map allocation + one atomic Inc + one atomic histogram Observe). Below the engine's noise floor (10-100 µs on inmemory). The `/metrics` scrape itself runs only on the Prometheus scrape interval (default 15s) and serializes ~120 series in ~100 µs.

## [0.1.7] - 2023-03-27

Multi-arch image release. `cmd/markup-server/Dockerfile` builds with `--platform=$BUILDPLATFORM` on the build stage and cross-compiles via `GOARCH=${TARGETARCH:-amd64}`; the CI image-publish job passes `platforms: linux/amd64,linux/arm64` to `docker/build-push-action` so every published tag is a manifest list. Operators on Apple Silicon (`docker pull ghcr.io/helmedeiros/markup-svc:v0.1.7`) automatically receive the arm64 variant and skip Rosetta-2 emulation; Graviton-class AWS instances get the same image without a separate pipeline. Closes ADR-0018.

### Added

- ADR-0018 (Accepted): multi-arch image publish. Two design questions answered: cross-compile vs QEMU emulation (pick cross-compile — `--platform=$BUILDPLATFORM` keeps the build native, GOARCH does the work), manifest list vs per-arch tags (pick manifest list — no operator-visible change at pull time).
- `cmd/markup-server/Dockerfile`: `ARG BUILDPLATFORM` / `ARG TARGETOS` / `ARG TARGETARCH` declarations + GOARCH-aware build command. Defaults preserve plain `docker build` (no buildx) producing a working amd64 image.
- `.github/workflows/ci.yml`: image-publish job declares `platforms: linux/amd64,linux/arm64` on the build-push-action step.

### Performance impact

CI build time +30 seconds (one extra cross-compile invocation) vs the original amd64-only build; cache hits on subsequent runs keep steady-state close to the original. Runtime: zero difference between native amd64 and arm64; improvement is purely removal of the emulation layer when the host arch is arm64. The dev-stack Jaeger trace's per-hop network cost on Apple Silicon drops from ~800µs (mostly emulation) to ~50-100µs (actual Docker-bridge wire time), an order of magnitude improvement that makes the trace measurements representative of production behavior.

## [0.1.6] - 2023-03-23

Multi-layer Decide span release. markup-svc becomes a W3C trace context consumer: the `Bootstrap` from v0.1.5 now also sets the global TextMapPropagator to TraceContext + Baggage, the new `WithTraceContext` HTTP middleware extracts incoming `traceparent` on every request, and the Decide handler invocations land as children of the upstream caller's span instead of starting new traces. The cmd Decider wiring layers two new spans around the existing `markup.decider.decide` span: `markup.engine.evaluate` wraps the engine adapter, `markup.guardrails.check` wraps the guardrails decorator (when guardrails are active). Operators reading Jaeger see the three (or four, when guardrails are on) nested spans whose durations break the per-component cost down for bottleneck investigation. Closes ADR-0017.

### Added

- `internal/httpapi/tracecontext.go`: `WithTraceContext(next http.Handler) http.Handler`. Extracts W3C trace context from the request headers and writes it onto `r.Context()`. Safe to mount unconditionally — when `--otel-enabled` is off the global propagator is the no-op and the middleware is a ~50 ns pass-through. Composition: `WithCorrelationID(WithTraceContext(mux))` so the correlation ID is in context when the span starts.
- Two new span layers in the cmd Decider wiring:
  - `markup.engine.evaluate` wraps the engine adapter (or the router's holder for multi-route deployments). Inner span; emits whenever `--otel-enabled` is set.
  - `markup.guardrails.check` wraps the guardrails decorator. Middle span; emits ONLY when guardrails are active (the binary has any of `--allowed-countries`, `--required-fields`, `--min-factor`, `--max-factor`, or `--guardrails-admin` set).
- ADR-0017 (Accepted): incoming W3C trace context + multi-layer Decide spans. Three design questions answered: per-handler Extract vs middleware (pick middleware; cross-route consistency), single-span vs three-layer model (pick three layers; per-component cost visibility is the bottleneck-investigation win), in-package span emission vs wiring-layer wrap (pick wiring layer; reuses the existing `mkotel.Wrap` machinery and keeps engine + guardrails packages OTel-free).

### Changed

- `Bootstrap` (in `internal/observability/otel/bootstrap.go`) now sets the global TextMapPropagator to `propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{})`. Previously the propagator stayed at the SDK no-op so incoming `traceparent` was ignored; `--otel-enabled` produced root spans only. Closes the trace-stitching gap that left gateway traces + markup-svc traces in different trace IDs.
- `--otel-enabled` flag help text updated to name the three span layers and the W3C trace context behavior so `--help` is accurate.
- `cmd/markup-server/main.go` composition order changes to `WithCorrelationID(WithTraceContext(mux))`; the Decider wiring in `wireTracedHandler` + `wireRouterHandler` adds the engine + guardrails span wraps inline.
- `cmd/markup-server/main_otel_test.go` E2E expectations updated for the new 2-span-per-Decide shape (no-guardrails path: `markup.engine.evaluate` + `markup.decider.decide`).

### Performance impact

`--otel-enabled` off: ~50 ns per request added (the WithTraceContext no-op). `--otel-enabled` on, no guardrails: ~350 ns per Decide (2 span open + close pairs + 2 SetAttributes + propagator Extract). `--otel-enabled` on, guardrails active: ~475 ns per Decide (3 spans + 3 SetAttributes + Extract). All below the engine's noise floor (typical inmemory eval 20-100 µs).

## [0.1.5] - 2023-03-20

Patch release closing the gap between the OTel span decorator (ADR-0009) and an actually-exporting tracer. `--otel-enabled` on a published markup-svc binary now produces spans visible in an OTLP-compatible collector (OTel Collector, Jaeger native OTLP, Tempo, etc.) without a wrapper main. Closes pricing-observability ADR-0002's expectation that the platform-canonical image works against its OTel Collector + Jaeger stack out of the box.

### Added

- `internal/observability/otel.Bootstrap(ctx, instrumentationName)`: constructs an OTLP gRPC exporter, builds a `sdktrace.TracerProvider` with batched export and detected resource, sets it as the global `otel.TracerProvider`, returns the named tracer + a `Shutdown` cleanup function. Reads the standard OTel SDK env vars (`OTEL_EXPORTER_OTLP_ENDPOINT` default `localhost:4317`, `OTEL_SERVICE_NAME`, `OTEL_RESOURCE_ATTRIBUTES`, etc.). `cmd/markup-server` calls Bootstrap when `--otel-enabled` is set and defers Shutdown with a 5s timeout for clean span flush at process exit.
- ADR-0016 (Accepted): bootstrap the OTel SDK for `--otel-enabled`. Two design questions answered: in-binary bootstrap vs wrapper-main (pick in-binary so the published image is the trace-emitting binary the platform cookbook claims it is); gRPC vs HTTP/protobuf exporter (pick gRPC for per-`Decide` cost; HTTP wrapper-main path documented).
- Dependencies: `go.opentelemetry.io/otel/exporters/otlp/otlptrace v1.11.2` + `otlptracegrpc v1.11.2`, transitively `google.golang.org/grpc v1.51.0` + `google.golang.org/protobuf v1.28.1`.

### Changed

- `--otel-enabled` flag now produces a working SDK-bootstrapped TracerProvider when set; previously it set up the API-level tracer against the global NoopTracerProvider so spans dropped on the floor. The flag's behaviour is now what the ADR-0009 cookbook recipe always claimed.

## [0.1.4] - 2023-02-03

Hot-reload for the guardrails decorator. The boot-time flags from `v0.1.3` stay unchanged; this release adds an admin endpoint so operators can replace the active rule set without restarting the process. The classic 2-AM tightening (a misbehaving rule set produces a Decision that needs to be vetoed *now*) no longer requires waiting for a rolling restart.

### Added

- `internal/decider/guardrails.Holder`: a `sync.RWMutex`-protected `[]Rule` holder mirroring `swap.Decider`'s minimum-lock-hold pattern. `NewHolder(rules ...Rule)` defensively copies the input; `Wrap(inner markup.Decider) markup.Decider` returns a Decider that reads the active slice on every `Decide` under an `RLock`/`RUnlock` pair; `Replace(rules []Rule)` allocates a new backing array and swaps the slice header under the write lock — in-flight `Decide` calls finish on the captured slice; `Snapshot() []Rule` returns a defensive copy for the `GET` handler and tests. A `NewHolder()` (no rules) returns a pass-through Holder; a `Replace(nil)` clears the active rules.
- `internal/decider/guardrails.BuildRules` + `internal/decider/guardrails.SplitCSV` + three error sentinels (`ErrFactorRangeInverted`, `ErrAllowedCountriesEmpty`, `ErrRequiredFieldsEmpty`). The validation logic that previously lived in `cmd/markup-server` moves into the package so the boot-flag wiring and the new admin handler share a single accept/reject contract: a configuration the flags reject also fails the admin POST, and vice versa.
- `internal/httpapi.GuardrailsAdmin(holder *guardrails.Holder, errLog io.Writer) http.Handler`: a single handler that dispatches on method. `GET` returns the live `GuardrailsConfig` JSON; `POST` decodes the body with `DisallowUnknownFields`, validates via `guardrails.BuildRules`, on success calls `holder.Replace(rules)` and returns 200 with the new snapshot; non-GET/non-POST returns 405 with `Allow: GET, POST`. Validation failures route through an `errors.Is` switch on the three sentinels and return 400 with an opaque body; the previous configuration keeps serving in every failure case. `DisallowUnknownFields` is intentional — a body carrying an unrecognized key fails 400 so operators running a newer body against an older binary surface the version mismatch rather than silently losing intent.
- `cmd/markup-server` `--guardrails-admin` flag (default off). When set, `buildGuardrailsWiring` constructs a `guardrails.Holder` pre-loaded with the boot-flag rules (empty if none configured) and mounts `POST/GET /admin/guardrails` on the same mux as `/decide` for both single-engine and router-mode deployments. When unset, the previous `v0.1.3` immutable boot-time decorator is wired and the admin endpoint is `404` — operators opt in explicitly because the Holder adds a ~15 ns lock-pair to every `Decide`.
- `docs/cookbook/guardrails.md` gains a Recipe section walking operators through the `GET → edit → POST` cycle for the 2-AM tightening, the mechanics paragraph explaining the lock-pair + new-backing-array pattern, two new "what to check after" bullets covering live-config inspection and the malformed-body posture, and three new "mistakes to avoid" bullets covering the set-vs-patch confusion, the unauthenticated endpoint posture, and the `DisallowUnknownFields` break-on-unknown semantic.
- `scientific/v0.1.4/` benchmark harness with four pre-registered bars covering the Holder's `Decide` cost (`indexed-baseline`, `guardrails-holder-zero-rules`, `guardrails-holder-three-rules`) and the admin-call cost (`BenchmarkReplace`). All bars pass under measurement; the Holder lock-pair lands at +14.8 ns over the indexed baseline (within 2σ of v0.1.0's `swap.Decider` precedent at +10 ns), and `Replace` clocks in at 17 ns with one 48-byte allocation for the backing array.
- README capability matrix gains a row for hot-reload pointing at the Holder + admin handler + `--guardrails-admin` flag.
- ADR-0015 (Accepted): hot-reload guardrails via `POST /admin/guardrails`. Composition order matches `v0.1.3`: the wiring places the Holder inside the OTel decorator and outside the swap holder so a vetoed Decision still records as `codes.Error` on the trace span.

## [0.1.3] - 2023-01-27

Guardrails land. The `markup.Decider` port gains a fifth decorator that vetoes Decisions outside a configured safety envelope before they leave the server. The veto is observable on the existing OTel + metrics decorators (vetoes record as `codes.Error` on trace spans; metric events classify as `Err` with the wrapped reason on `Err`). No engine or rule-loader change — the four shipped adapters and CSV / snapshot loaders are unchanged.

### Added

- `internal/decider/guardrails`: veto decorator at the `markup.Decider` port with a single-method `Rule` port (`Check(ctx, decision, req) (allowed bool, reason string)`) and a sentinel `ErrGuardrailViolation` reachable via `errors.Is`. Three concrete rules ship: `FactorRange{Min, Max}` (closed-interval bound on `Decision.MarkupFactor`), `AllowedCountries{Countries}` (case-sensitive membership test on `Request.Country`), `RequiredFields{Fields}` (presence test for the seven string fields on `Request`; `Amount` is intentionally excluded). Decide passes inner errors through unchanged so `ErrNoMatch` does NOT get reclassified as a guardrail violation — the metrics decorator keeps NoMatch / Err separate on its dashboards. First-veto-wins ordering; subsequent rules are not consulted after the first rejection.
- `cmd/markup-server` flags: `--min-factor`, `--max-factor`, `--allowed-countries`, `--required-fields`. When at least one is set on the command line, `wireTracedHandler` (and `wireRouterHandler`) compose `guardrails.New(inner, rules...)` between the holder/router and the OTel decorator. Detection of "operator set the flag" uses `flag.FlagSet.Visit` so `--max-factor=0` is treated as an explicit (degenerate) operator choice, not "default left at zero". `--min-factor > --max-factor` fails boot; empty CSV after splitting on comma fails boot for `--allowed-countries` / `--required-fields`. When no guardrail flag is set, the decorator is not constructed and not in the call path — zero per-`Decide` overhead.
- `docs/cookbook/guardrails.md` recipe walking operators through the four flags with copy-paste commands, the otel/guardrails composition mechanics, the asymmetric `--max-factor` proof that the flag does real work, and the most common misconfigurations (empty allowlist, inverted factor interval, unknown required-field name).
- `scientific/v0.1.3/` benchmark harness with pre-registered bars for the guardrails decorator. Three sub-benchmarks under one parent so a single pass interleaves them: `indexed-baseline` (reproduces v0.1.0's measured baseline so the delta is computed in-pass), `guardrails-zero-rules` (the wrapper-with-no-work cost), `guardrails-three-rules` (the realistic production configuration the cookbook demonstrates). All 3 absolute + 2 ordinal bars pass under measurement; the three-rule production stack costs ~42 ns over the indexed baseline — well under ADR-0014's analytic budget of ~100-120 ns. Allocations stay at 12 on every row (matches indexed baseline; the decorator adds none on the happy path).
- README capability matrix gains a Guardrails row alongside the flag list; the package table gains a row for `internal/decider/guardrails`.
- ADR-0014 (Accepted): guardrails decorator at the Decider port. Default composition places guardrails inside the OTel decorator and outside the swap holder so a vetoed Decision records as `codes.Error` on the trace span and a forgotten per-route wiring cannot expose traffic to an unprotected engine. Per-route guardrails remain possible by wrapping each Route's Decider before adding it to the router.

## [0.1.2] - 2023-01-20

Production deployment artifacts. The binary stays unchanged (no API breaking changes); the release adds the container image, the Kubernetes manifests, the liveness / readiness probes that the kubelet uses to gate pods, and the cookbook recipe walking operators through `kubectl apply -k deploy/k8s/`.

### Added

- `internal/httpapi.Healthz` and `internal/httpapi.Readyz` handlers. `Healthz` returns `200 {"status":"ok"}` on `GET` (liveness; kubelet restarts a deadlocked goroutine on probe-fail); `Readyz` calls a `Ready func() (string, bool)` closure on every probe and returns `200 {"status":"ready"}` when the closure reports true, `503 {"status":"not_ready","reason":"..."}` otherwise. Both reject non-`GET` with `405` and `Allow: GET`. `cmd/markup-server` supplies the closure backed by an `atomic.Int32` flipped after the initial Decider construction succeeds.
- `cmd/markup-server/Dockerfile`: multi-stage build (golang:1.18 → `gcr.io/distroless/static-debian11:nonroot`). `CGO_ENABLED=0 GOOS=linux GOARCH=amd64` plus `-trimpath` and `-ldflags="-s -w"`. Runs as user 65532 with `ENTRYPOINT` set so operators pass flags via `docker run <image> --rules=...`.
- CI image-publish workflow. Builds the image on every push and PR (verification); pushes to `ghcr.io/helmedeiros/markup-svc` only on `main` and tag pushes. Tag scheme: `:sha-<8>` always, `:main` on main, `:<version>` on tag pushes.
- `deploy/k8s/` kustomize base: `Deployment` (2 replicas, rolling with `maxUnavailable: 0`, hardened `securityContext` with `runAsNonRoot` + seccomp `RuntimeDefault` + `readOnlyRootFilesystem` + capabilities drop ALL), `Service` (ClusterIP on 8080), `ConfigMap` carrying a sample `rules.csv`, `HorizontalPodAutoscaler` (CPU 70%, min 2 / max 10). Resource requests 100m CPU / 64 MiB memory, limits 500m / 256 MiB. Probes shipped per the `/healthz` and `/readyz` semantics above.
- `docs/cookbook/k8s-deploy.md` recipe walking operators through `kubectl apply -k deploy/k8s/`, smoke test via `kubectl port-forward`, rule update through `kubectl edit configmap` + `POST /admin/reload`, scaling, and four real mistakes-to-avoid (forgetting metrics-server, switching to `Recreate` rolling strategy, `subPath` mounting the ConfigMap, exposing the Service as `LoadBalancer`). Appendix covers the InitContainer + object-storage pattern for rule sets larger than the 1 MiB ConfigMap cap.
- ADR-0013 (Accepted): production deployment artifacts.

## [0.1.1] - 2023-01-13

Documentation + scientific harness patch release. No API changes; every exported surface from `v0.1.0` is unchanged. This release adds the falsifiable performance baselines and the operator-facing cookbook the project deferred to the polish phase.

### Added

- `scientific/v0.1.0/`: cross-adapter, cross-decorator benchmark harness with pre-registered bars. Docker pins Linux/amd64 + Go 1.18 + the rule-set fixture for reproducibility. The harness measures per-`Decide` latency for every adapter, decorator overhead at each layer, cold-start cost (rules vs snapshot), and router overhead. Methodology per ADR-0012: each trial is one `-benchtime=1s` run; n=50 trials interleaved per `-count` pass so host noise hits every measurement equally; bars are committed before measurement and do not move once committed. The v0.1.0 measurement confirms all 11 absolute bars and all 4 ordinal bars pass on the reference fixture, including the surprising `ColdStart/rules < ColdStart/snapshot` at 50 rules (the JSON-decode allocations dominate the savings from skipping the parser at this rule-set size; the crossover is real and is the next harness's gap to close).
- `make scientific-v0.1.0` target wraps the Docker build + run so any operator can reproduce the same measurements with one command.
- ADR-0012 (Accepted): scientific performance comparison harness.
- `docs/cookbook/`: operator-level recipes following a 5-part template (Problem / Recipe / What's happening / What to check after / Relevant ADRs and flags). Six recipes ship: production deployment, A/B rollout, hot reload (single-route + per-route), snapshot promotion via CI/CD, multi-model serving, observability (OTel spans + the metrics-port sink-writing pattern). Each recipe is honest about what `cmd/markup-server` wires vs what operators write themselves (e.g., the metrics decorator is library-only — operators implement a Prometheus or OTel-metrics `Sink` against the port).
- README: post-v0.1.0 status (12 Accepted ADRs, committed API surface, SemVer-bump promise), `bre-go` capability matrix mapping every demonstrated feature to its markup-svc surface, a Performance section linking the scientific harness and the `make scientific-v0.1.0` target, and ADR-0012 added to the ADR index.

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

[Unreleased]: https://github.com/helmedeiros/markup-svc/compare/v0.1.2...HEAD
[0.1.2]: https://github.com/helmedeiros/markup-svc/compare/v0.1.1...v0.1.2
[0.1.1]: https://github.com/helmedeiros/markup-svc/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/helmedeiros/markup-svc/compare/v0.0.4...v0.1.0
[0.0.4]: https://github.com/helmedeiros/markup-svc/compare/v0.0.3...v0.0.4
[0.0.3]: https://github.com/helmedeiros/markup-svc/compare/v0.0.2...v0.0.3
[0.0.2]: https://github.com/helmedeiros/markup-svc/compare/v0.0.1...v0.0.2
[0.0.1]: https://github.com/helmedeiros/markup-svc/releases/tag/v0.0.1
