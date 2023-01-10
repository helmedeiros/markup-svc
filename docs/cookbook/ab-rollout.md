# A/B-roll a new model version

## Problem

You have a current rule set (`v1`) serving production and a candidate rule set (`v2`) you want to test against a slice of traffic. The slice should be sticky per request: a caller that lands on `v2` for their first decision should land on `v2` for any retry of the same request.

## Recipe

Put both rule sets on disk at stable paths:

```
/etc/markup/rules-v1.csv
/etc/markup/rules-v2.csv
```

Boot `markup-server` with two `--route` flags and the sticky-by-correlation-ID policy:

```sh
./markup-server \
  --route=v1:control:rules:/etc/markup/rules-v1.csv \
  --route=v2:treatment:rules:/etc/markup/rules-v2.csv \
  --policy=hash-correlation \
  --listen=:8080
```

Format of each `--route` value: `model_version:variant:source_type:source_path`.

| Field | Example | Meaning |
|---|---|---|
| `model_version` | `v1` | Stamped on every Decision served by this route. Slices observability by model. |
| `variant` | `control` | Stamped on `Decision.Experiment`. Slices observability by variant. Use `"control"` / `"treatment"` for two-sided A/B, `""` for non-A/B routes. |
| `source_type` | `rules` | Source format. `rules` for CSV, `snapshot` for a pre-built JSON snapshot. |
| `source_path` | `/etc/markup/rules-v1.csv` | On-disk path. Same shape as `--rules` single-route mode. |

Distribute traffic by ensuring every caller sends a stable `X-Correlation-ID` header:

```sh
curl -sS -X POST -H 'Content-Type: application/json' \
  -H 'X-Correlation-ID: caller-id-abc-123' \
  -d '{"customer_tier":"enterprise","country":"BR"}' \
  http://markup-svc.internal:8080/decide
```

The `hash-correlation` policy FNV-1a-hashes `X-Correlation-ID` and picks the route at `hash % len(routes)`. Same caller ID always lands on the same route across retries.

Callers without `X-Correlation-ID` fall back to the first route (the `v1:control` baseline). That keeps health probes and unauthenticated traffic on the control while you measure.

## What's happening

The router decorator from ADR-0011 sits between the HTTP transport and the four engine adapters. Each `--route` value becomes a `Route{ModelVersion, Variant, Decider}` inside the router; every route gets its own `swap.Decider` holder so per-route hot reloads work without touching the other route. The policy picks one route per `/decide` call and the router stamps `Decision.ModelVersion` + `Decision.Experiment` from the chosen route — inner Deciders cannot accidentally erase the routing labels.

With `--otel-enabled` the OpenTelemetry decorator wraps the router (composition is `otel → router → per-route swap holders → engine`), so every span carries `rule.markup.model_version` and `rule.markup.rule` attributes you can slice on in your tracing backend.

The metrics decorator from ADR-0010 is library-only in this release — `cmd/markup-server` does not wire a metrics sink for you. Operators who want metric counters / histograms sliced by `(model_version, experiment)` wrap their own Decider construction with `metrics.Wrap(decider, sink)` where `sink` is a Prometheus or OTel-metrics adapter they implement against the `metrics.Sink` port. The `DecisionMetric` event the sink receives carries the routing labels the router stamped, so per-variant counters slice cleanly.

## What to check after

- Startup log line names the route count and policy: `markup-server: listening on :8080 (… , adapter router, source 2 routes (policy=hash-correlation))`.
- Two caller IDs that the FNV-1a hash sends to different routes return Decisions with different `model_version` + `experiment`. Use distinct IDs and compare:
  ```sh
  for id in abc def ghi jkl mno; do
    curl -sS -X POST -H "Content-Type: application/json" \
      -H "X-Correlation-ID: $id" -d '{"customer_tier":"enterprise"}' \
      http://markup-svc.internal:8080/decide | grep -E 'model_version|experiment'
  done
  ```
  Expect a mix of `"model_version":"v1","experiment":"control"` and `"model_version":"v2","experiment":"treatment"` across the five probes.
- The same caller ID always lands on the same route. Repeat one ID 5×; expect 5 identical Decisions.
- With `--otel-enabled`: spans for `/decide` carry `rule.markup.model_version=v1` or `v2` so traces slice cleanly by route. Operators wiring the metrics decorator (see "What's happening" above) see counter splits there too.
- `/admin/reload` now requires a body naming the route — see [hot-reload.md](hot-reload.md).

## Relevant ADRs and flags

- [ADR-0011](../architecture/decisions/0011-router.md) — router decorator, policies, the source-of-truth stamping rule
- [ADR-0003](../architecture/decisions/0003-http-decide-route.md) — `X-Correlation-ID` middleware that carries the stickiness axis
- [ADR-0009](../architecture/decisions/0009-otel-spans.md) — `rule.markup.*` span attributes when `--otel-enabled` is set
- [ADR-0010](../architecture/decisions/0010-metrics-port.md) — `DecisionMetric.ModelVersion` / `Experiment` fields for dashboard slicing (library-only — operators wire their own sink)
- `--route` (repeatable), `--policy` (`hash-correlation` | `default`), `--otel-enabled`
