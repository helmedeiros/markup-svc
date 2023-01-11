# Wire OpenTelemetry spans and metrics into your stack

## Problem

You want per-request traces in your tracing backend (Jaeger / Tempo / any OTLP receiver) and per-Decision counters / histograms in your metrics backend (Prometheus / OTel metrics), with both signals sliced by `(model_version, experiment, rule, adapter)` so dashboards line up with the decisions the service is actually making.

## Recipe — OpenTelemetry spans

Enable the OTel span decorator at boot:

```sh
./markup-server \
  --rules=/etc/markup/rules.csv \
  --otel-enabled
```

Point the OTel SDK at your exporter via the standard env vars:

```sh
export OTEL_SERVICE_NAME=markup-svc
export OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-collector.internal:4317
export OTEL_TRACES_SAMPLER=parentbased_traceidratio
export OTEL_TRACES_SAMPLER_ARG=0.10
```

(`--otel-enabled` without an SDK-configured exporter falls back to the no-op tracer — spans are emitted but dropped, useful for "would this break my throughput?" smoke tests but not for actual tracing.)

Every `/decide` call emits one span named `markup.decider.decide` with the following attributes:

| Attribute | Source |
|---|---|
| `rule.markup.adapter` | `Decision.EngineAdapter` (`*indexed.Engine`, etc.) |
| `rule.markup.model_version` | `Decision.ModelVersion` (stamped by router in multi-route mode) |
| `rule.markup.rule` | `Decision.Rule` (name of the rule that fired) |
| `rule.markup.factor` | `Decision.MarkupFactor` (the served markup multiplier) |
| `rule.markup.correlation_id` | from `X-Correlation-ID` request header or generated UUID |
| `rule.markup.no_match` | `true` when no rule matched (NOT an error) |
| `rule.markup.canceled` / `cancel.reason` | when ctx was canceled or deadline exceeded |

Span status is `OK` on success, `OK` on `ErrNoMatch` (the no-match attribute is the signal), `OK` on context cancellation (the cancellation attributes are the signal), `Error` on any other engine error.

## Recipe — metrics

The metrics decorator from [ADR-0010](../architecture/decisions/0010-metrics-port.md) ships library-only — `cmd/markup-server` does not wire a sink for you. To get metric events into your backend, you write a small wrapper main:

```go
// cmd/markup-server-with-metrics/main.go (example)
package main

import (
    // ...
    "github.com/helmedeiros/markup-svc/internal/observability/metrics"
)

// PrometheusSink implements metrics.Sink with prometheus client_golang.
type PrometheusSink struct {
    decisions *prometheus.CounterVec
    latency   *prometheus.HistogramVec
}

func (s *PrometheusSink) RecordDecision(m metrics.DecisionMetric) {
    labels := prometheus.Labels{
        "adapter":       m.Adapter,
        "model_version": m.ModelVersion,
        "rule":          m.Rule,
        "no_match":      strconv.FormatBool(m.NoMatch),
    }
    s.decisions.With(labels).Inc()
    s.latency.With(labels).Observe(m.Duration.Seconds())
}
```

Then wrap your Decider construction:

```go
decider := buildDecider(...)
sink := &PrometheusSink{ /* ... initialise CounterVec + HistogramVec */ }
decider = metrics.Wrap(decider, sink)
// continue with otel.Wrap, swap.New, etc.
```

Recommended composition order per ADR-0010: `metrics → otel → swap → engine` (metrics outermost so its `Duration` captures end-to-end Decider cost including tracing overhead).

Mount a Prometheus `/metrics` endpoint on the same `http.Server` and you have a per-Decision counter / histogram sliced by every label the `DecisionMetric` event carries.

## What's happening

The OTel and metrics decorators sit at the `markup.Decider` port. Both share the same per-outcome classification (success / no-match / canceled / deadline / other error), which is why dashboards from the two signals slice consistently: a counter increment with `no_match=true` corresponds one-to-one with a span carrying `rule.markup.no_match=true`. Same outcome, two signals.

The `--otel-enabled` flag wires the OTel decorator above the `swap.Decider` holder so hot reloads do not lose tracing — a Decide running on the just-swapped Decider still goes through the same tracer (see [ADR-0009](../architecture/decisions/0009-otel-spans.md)). Operators wiring metrics should follow the same composition (metrics outermost) so the same hot-reload-preserves-decorator-wiring property holds.

## What to check after

- With `--otel-enabled` + an OTLP collector running: spans labeled `markup.decider.decide` appear in the tracing UI within seconds of `/decide` traffic. Click into a span and see the `rule.markup.*` attribute set.
- Without `--otel-enabled`: spans do not flow (the wrapper is not mounted). `/decide` latency unchanged.
- With a custom metrics-wrapped main: `/metrics` endpoint exposes `markup_decisions_total{adapter=...,model_version=...,rule=...,no_match=...}` counters. Latency histogram has the same labels.
- Cancellation (e.g., client disconnects before `/decide` returns): span shows `rule.markup.canceled=true` + `rule.markup.cancel.reason="canceled"`, status stays OK. Metrics event has `Canceled=true`, `CancelReason="canceled"`, `Err=nil`. Caller cancellation does not inflate the server-side error-rate dashboard.

## Mistakes to avoid

- **Treating `ErrNoMatch` as a server error.** Both decorators deliberately classify it as a domain outcome (boolean attribute, not span status; `NoMatch=true` not `Err`). Dashboards that gate alerts on `Err` will quietly skip no-match traffic — that is intended.
- **Putting metrics inside otel inside swap.** Hot reloads work correctly only when both decorators wrap the swap holder (their composition is preserved across reloads). Inverting the order means a reload swaps the inner Decider out from under the decorators and subsequent calls bypass the wrapper.
- **Forgetting to set `OTEL_SERVICE_NAME`.** The OTLP collector groups by service; an unset name puts every markup-svc span into a generic bucket and operators lose the ability to filter.

## Relevant ADRs and flags

- [ADR-0009](../architecture/decisions/0009-otel-spans.md) — OTel span decorator at the Decider port
- [ADR-0010](../architecture/decisions/0010-metrics-port.md) — metrics port + decorator; library-only (operators wire the sink)
- [ADR-0003](../architecture/decisions/0003-http-decide-route.md) — correlation-ID middleware that ties trace + decision identity
- `--otel-enabled` flag
