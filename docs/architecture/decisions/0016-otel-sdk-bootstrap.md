# 16. Bootstrap the OTel SDK for `--otel-enabled`

## Status

Accepted — `internal/observability/otel/Bootstrap` constructs an OTLP gRPC exporter, builds a `sdktrace.TracerProvider` with a batched span processor and a detected resource, sets it as the global `otel.TracerProvider`, and returns a named tracer plus a shutdown function. `cmd/markup-server` calls `Bootstrap` when `--otel-enabled` is set; otherwise the binary leaves the global as the NoopTracerProvider so per-`Decide` spans are zero-cost no-ops. The Shutdown closer runs at process exit with a 5s context timeout so in-flight batched spans flush. ADR-0009 (the OTel decorator) keeps its position; this ADR fills the gap between "the decorator wraps a tracer" and "the tracer actually exports".

## Context

ADR-0009 shipped the OTel decorator: every `Decide` call goes through `internal/observability/otel.Wrap` which opens a span via the configured tracer. The decorator is tested + correct + has the right shape. The cmd code for `--otel-enabled` was:

```go
if *otelEnabled {
    tracer = otel.Tracer("github.com/helmedeiros/markup-svc/cmd/markup-server")
}
```

This calls `otel.Tracer(...)` which returns a tracer from the **global TracerProvider**. The global default is the SDK's NoopTracerProvider. Without explicit SDK bootstrap, every span the decorator opens is a no-op span — spans drop on the floor, exporters see nothing, Jaeger / OTel Collector / Grafana Tempo see nothing.

The original ADR-0009 cookbook recipe was honest about this: it said operators "wire their own exporter via a wrapper main per the metrics-port pattern" (the metrics decorator is also library-only per ADR-0010, with operators expected to build a wrapper binary that constructs a Sink). The cookbook prose for OTel said the same thing: `--otel-enabled without an SDK-configured exporter falls back to the no-op tracer`.

The wrapper-main pattern fails as soon as the platform tries to use the published `ghcr.io/helmedeiros/markup-svc:v0.1.4` image directly. There is no wrapper binary in that image; operators wiring the platform-canonical docker-compose with `--otel-enabled` set + the standard OTEL env vars get zero spans. The pricing-observability project ADR-0002 (traces phase) explicitly depends on this working — it stands up an OTel Collector ready to receive markup-svc spans, but the spans never arrive because the published binary does not bootstrap the SDK.

Two design questions.

### 1. Bootstrap inside the binary vs continue the wrapper-main pattern

Wrapper main:

- Pro: keeps `markup-svc` library-only as far as the SDK exporter is concerned; operators with non-standard exporter requirements (gRPC + TLS, HTTP/protobuf, sampled head, custom resource detectors) write their own.
- Con: the published image cannot benefit from `--otel-enabled` without operators re-deriving + rebuilding. The compose stack across the platform repos pins specific tagged images; operators following the platform cookbook recipes hit a working observability stack only if they go build a fork. That violates the cookbook's documented promise.

Bootstrap inside the binary:

- Pro: the published `markup-svc:vN` image gains a working OTel SDK exporter out of the box when `--otel-enabled` + the standard OTLP env vars are set. The cookbook recipe works as written against the official image.
- Con: the binary takes a transitive dependency on `go.opentelemetry.io/otel/exporters/otlp/otlptrace` and `otlptracegrpc` and through them on `google.golang.org/grpc`. Module size grows; build time grows by a few seconds.

**Pick the in-binary bootstrap.** The dependency cost is small (grpc + protobuf are well-known, well-maintained); the operator-experience benefit is real (the official image becomes the trace-emitting binary the platform cookbook claims it is). Operators with non-standard exporter requirements (HTTP/protobuf, mTLS, sampled-head) still write a wrapper main — the `Bootstrap` function is the default convenience, not the only path.

### 2. gRPC vs HTTP/protobuf exporter

The OTel SDK ships two OTLP exporters:

- **gRPC** via `otlptracegrpc`: lower per-request overhead, multiplexed streams, HTTP/2 framing. Standard for service-to-service trace export. Default OTLP port is `4317`.
- **HTTP/protobuf** via `otlptracehttp`: easier through corporate proxies that mishandle HTTP/2, simpler debugging via curl. Default OTLP port is `4318`.

**Pick gRPC.** Per-`Decide` overhead matters and gRPC is materially cheaper; the OTel Collector accepts both protocols on the same container, so an operator who needs HTTP/protobuf wraps the `Bootstrap` function in a wrapper main and uses `otlptracehttp` instead. The `OTEL_EXPORTER_OTLP_PROTOCOL` env var is documented in the function godoc; operators setting it to anything other than `grpc` need the wrapper main today.

## Decision

`internal/observability/otel` ships `Bootstrap`:

```go
type Shutdown func(ctx context.Context) error

func Bootstrap(ctx context.Context, instrumentationName string) (trace.Tracer, Shutdown, error)
```

`Bootstrap`:

1. Constructs an `otlptracegrpc.NewClient()` — reads `OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_EXPORTER_OTLP_HEADERS`, `OTEL_EXPORTER_OTLP_COMPRESSION`, and TLS env vars per the OTel SDK conventions.
2. Wraps the client in `otlptrace.New(ctx, client)`.
3. Detects resources via `resource.New(ctx)` — picks up `OTEL_SERVICE_NAME`, `OTEL_RESOURCE_ATTRIBUTES`, process metadata, OS metadata.
4. Builds `sdktrace.NewTracerProvider(WithBatcher(exp), WithResource(res))` — batched span export, default batch size + timeout.
5. Sets it as global via `otel.SetTracerProvider(tp)`.
6. Returns `tp.Tracer(instrumentationName)` and `tp.Shutdown`.

`cmd/markup-server/main.go` calls `Bootstrap` when `--otel-enabled` is set, then `defer`s the shutdown with a 5s timeout context at process exit. The shutdown flushes batched spans, closes the gRPC connection.

When `--otel-enabled` is not set, `cmd/markup-server` never calls `Bootstrap`; the global TracerProvider stays the SDK's NoopTracerProvider; per-`Decide` spans are no-ops with zero allocation. The OTel decorator code path stays unchanged in both modes.

The `--otel-enabled` flag's help string updates to reflect the new behavior: spans now ship via OTLP gRPC to the endpoint named by `OTEL_EXPORTER_OTLP_ENDPOINT` (defaulting to `localhost:4317`), per the OTel SDK conventions.

## Consequences

### Closed by this ADR

- `markup-svc:v0.1.5` (the release this ADR ships in) emits trace spans against any OTLP-compatible collector (OTel Collector, Jaeger native OTLP, Tempo, Datadog OTLP receiver) once the operator sets `--otel-enabled` + `OTEL_EXPORTER_OTLP_ENDPOINT`. No wrapper-main required.
- The cookbook recipe in pricing-observability ADR-0002 works against the published image without re-derivation.
- Per-`Decide` overhead with `--otel-enabled` set + endpoint reachable: one span open + batched async export. The batcher's span processor writes to an internal buffer at submit-time (sub-microsecond per call) and the actual gRPC send happens on a background goroutine. The hot path is not blocked on the exporter.

### NOT closed by this ADR

- HTTP/protobuf exporter (`otlptracehttp`). Operators needing it write a wrapper main calling `sdktrace.NewTracerProvider` directly. A `BootstrapHTTP` companion function lands when a real consumer asks; the menu stays small.
- Sampling. `Bootstrap` uses the SDK's default `ParentBased(AlwaysOn)` sampler. Operators tuning sampling rates wrap-main with a custom sampler or set `OTEL_TRACES_SAMPLER=parentbased_traceidratio` + `OTEL_TRACES_SAMPLER_ARG=0.1`; both are standard SDK env vars Bootstrap honors through the SDK's auto-init.
- Metrics. `Bootstrap` is traces-only. A symmetric `BootstrapMetrics` for the `internal/observability/metrics` decorator lands once at least one operator deploys both signals against the same collector.
- mTLS to the collector. `otlptracegrpc.NewClient` honors `OTEL_EXPORTER_OTLP_CERTIFICATE` / `OTEL_EXPORTER_OTLP_CLIENT_KEY`. Documented in the function godoc; no explicit setup required.

### Performance impact

`Bootstrap` runs once at boot. Per-`Decide` overhead:

- `--otel-enabled` not set: unchanged. The OTel decorator is not in the call chain; its absence is zero ns.
- `--otel-enabled` set, endpoint reachable: the decorator's span open + close cost (existing per ADR-0009, ~50–100 ns per call) plus the batched processor's submit to its internal queue (~30 ns per call on amd64). Aggregate ~100–150 ns added to the existing engine + decorator stack. Well below the metrics decorator's measured 176 ns delta in `scientific/v0.1.0/REPORT.md`.
- `--otel-enabled` set, endpoint unreachable: the batched processor's queue grows until its max size, then it drops spans + logs a warning. The hot path stays alive; no per-`Decide` blocking. Operators with a temporarily-unreachable collector see span loss as a warning, not a service outage.

A `scientific/v0.1.5` harness row could pin the with-OTLP overhead but is out of scope for this ADR; the existing v0.1.4 numbers stay accurate for the `--otel-enabled` not-set case.

### Validation strategy

- `internal/observability/otel`: the existing tests for the decorator cover the per-`Decide` span behavior; `Bootstrap` is a single-shot init function. A test that constructs a Bootstrap against an in-process noop endpoint and asserts `Shutdown` flushes cleanly would catch a regression but is not strictly required at v0.1.5 (the function is 20 lines and tested transitively by the manual smoke).
- Manual smoke: bring up the pricing-observability traces stack (ADR-0002 in that repo), start `markup-svc:v0.1.5` with `--otel-enabled` + `OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-collector:4317`, fire 5 `POST /decide` requests through the gateway, open Jaeger UI at `http://localhost:16686`, search for service `markup-svc`, observe 5 traces with the `markup.decider.decide` span name and the `rule.markup.*` attributes from ADR-0009.
- The smoke is the proof; the platform cookbook recipes link to it.
