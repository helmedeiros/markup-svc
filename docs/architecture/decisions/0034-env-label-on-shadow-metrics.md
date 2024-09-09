# 34. Env label on shadow metrics + access log + decide span

## Status

Accepted — `--env` (default `"default"`) tags the process with a stable environment identifier. The label lands on every `markup_challenger_*` Prometheus series, on every `markup-server.access` JSON event, and as the `markup.env` attribute on the outermost `markup.decider.decide` span. `markup_decide_*` is intentionally NOT relabeled; existing dashboards, alerts, and PromQL keep working.

## Context

ADR-0032 emitted the five shadow comparison counters and the factor-delta histogram without an `env` label. ADR-0033 added the sample-rate counter and the challenger-latency histogram with the same posture. The implicit assumption was the ADR-0032 single-feature-single-env-per-process boundary: one markup-svc process serves one environment, so Prometheus would aggregate naturally per-instance and operators would slice via the scrape target.

Two consumers downstream of those metrics broke that assumption:

1. **Model registry `/shadow-stats`.** The registry queries Prometheus for the same counters across the entire scrape job. With markup-svc instances serving multiple envs behind one Prom instance, `markup_challenger_agreement_total` aggregates across envs — the registry-side view names an `env` per request but the underlying numbers are env-agnostic.
2. **Autopromote observer in model-registry.** The `eb62e8f` observer logs `registry.autopromote.gate_cleared` events naming a specific env, but the gate evaluation calls `shadow.Stats()` which fetches global numbers. The event names an env that the metrics can't actually attest.

Both consumers can either filter at query time (requires the label) or translate via per-instance discovery (heavy, requires every registry to learn the markup-svc topology). The label is the lightweight fix and the natural shape: env is a free-cardinality dimension at the markup-svc emission point — a single string per process.

Beyond metrics, two other observability planes carry the same per-request shape and benefit from the same dimension:

- **Access log.** `markup-server.access` JSON events already carry the request, the matched rule, and the trace IDs. Adding `attrs.env` lets a Kibana / Loki / Splunk operator scope a query to a single env without joining against the scrape topology.
- **OTel span.** The outermost `markup.decider.decide` span already gets `markup.adapter` / `markup.model_version` / `markup.rule` / `markup.factor`. Adding `markup.env` lets Jaeger SPM Monitor produce env-keyed RED-metrics views without a separate exporter slice.

## Decision

### `--env` flag

New flag on `cmd/markup-server`. String, default `"default"`. No validation beyond the default — operators set whatever stable identifier their topology uses (`production`, `staging`, `eu-west-1`, `tenant-acme`, etc.). The default lets existing deployments boot unchanged; their metrics now carry `env="default"` until the operator overrides.

The flag is plumbed to three sites:

1. `prom.New(env string)` — Sink + ShadowSink + HTTP handler. The ShadowSink keeps `env` in a struct field and prepends it to every `WithLabelValues` call. The vanilla `Sink` (Decide metrics) is unchanged.
2. `httpapi.WithAccessLog(l, env, next)` — middleware constructor accepts the env. When non-empty, every access event gets `attrs.env`.
3. `otel.WithEnv(env)` — `Option` for `otel.Wrap`. When set, `setAttrs` adds `attribute.String("markup.env", env)` to the per-outcome attribute batch (still one `SetAttributes` call per Decide).

The `markup_decide_*` counter and histogram (`markup_decide_total` + `markup_decide_duration_seconds`) are NOT relabeled. They are consumed by Grafana panels, PromQL queries, and `pricing-observability` alerts that have stabilized over multiple releases. Adding a label there is a breaking change for those consumers; the cost-benefit of asking every consumer to add an `env="..."` selector is higher than the value of consistent labeling. If a multi-feature multi-env shape ever lands in markup-svc (currently deferred per the local roadmap), the same flag plumbs through trivially.

### Backward compatibility

Existing scrape configs and dashboards keep working:

- `markup_decide_*` unchanged — no migration.
- `markup_challenger_*` gain `env="default"` for any deployment not setting the flag. PromQL like `sum(rate(markup_challenger_agreement_total[5m]))` still works because the sum collapses the label.
- Access events carry `attrs.env="default"` (or whatever the flag) — additive, not breaking.
- OTel span attribute `markup.env` is additive; consumers that don't filter on it see no change.

### Registry-side consumption

The autopromote observer in model-registry can now query the registry's `/shadow-stats` per env. The shadow-stats endpoint itself needs an env query parameter and a per-env `WithLabelValues` filter on the PromReader's queries — that's the next change in model-registry, not in markup-svc.

## Consequences

### Positive

- Closes the autopromote fidelity gap. The observer's "would promote env=production" log is now backed by env-attributable metrics.
- Multi-env registry topologies (one registry observes N markup-svc instances each serving a different env) become straightforward to query.
- Access log + span carry the same dimension, so the three observability planes (metrics / log / trace) line up on `env` from day one.

### Negative

- Cardinality grows by a factor of N where N is the number of distinct env values per scrape job. Realistic N is 1-10 in production; the label is bounded by the operator's deployment topology, not by request shape. Within Prometheus's recommended series ceiling.
- Adding the label is a metric-shape change for any consumer who happens to read the raw series with an exact label set (rather than aggregating with `sum(...)`). PromQL with `{agree="true"}` selectors keeps working; PromQL that explicitly enumerates `{}` does not. Existing pricing-observability rules use the `sum`-collapse pattern; verified via grep before this ADR.
- The `markup_decide_*` / `markup_challenger_*` asymmetry (env on shadow only) is a real inconsistency. Documented; the cost-benefit explanation lives in this ADR's Decision section.
- Per-Decide overhead: the `setAttrs` rewrite (single pre-sized slice allocation instead of two `append`-prepends) keeps the per-Decide allocation count at one in the otel decorator. The prom adapter's `WithLabelValues(env, ...)` path is O(1) after first warm-up because the prometheus client caches the resolved `*Counter` per label tuple. The access-log map already allocated per-request before this ADR; adding one `attrs["env"]` entry does not change the allocation count. No new scientific harness bar is pre-registered for the env-label cost in this commit — the next bar bundle covers it.

### Not closed

- `markup_decide_*` env keying. Deferred as documented above; a future ADR can revisit if/when a concrete consumer (e.g., a multi-tenant pricing rollout) needs it.
- Per-feature dimension. The multi-feature shape is deferred indefinitely per the local roadmap; if it ships, `feature` becomes a sibling of `env` and the plumbing pattern from this ADR is the template.

## References

- ADR-0009 — OTel span decorator (markup.decider.decide root span).
- ADR-0010 — Metrics port at the Decider layer.
- ADR-0019 — Prometheus Sink adapter + /metrics.
- ADR-0021 — Structured JSON logs across boot/access/shutdown (`markup-server.access` event).
- ADR-0032 — Shadow /decide execution (the five shadow counters + factor-delta histogram).
- ADR-0033 — Shadow sample rate (the sample-rate counter + challenger-latency histogram).
- model-registry ADR-0013 — `/shadow-stats` endpoint (the registry-side consumer that will gain a per-env filter in a follow-on).
