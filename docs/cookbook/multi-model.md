# Serve multiple model versions side-by-side

## Problem

You have two (or more) model versions you want to serve from one binary — not as an A/B experiment, but as a long-running multi-model deployment where each request lands on a specific model based on its identity. Common shapes: per-tenant model selection, gradual model migration with a fast rollback path, regional routing.

## Recipe

The mechanism is the same `--route` flag as the A/B case (see [ab-rollout.md](ab-rollout.md)). For multi-model rather than A/B, leave the variant field empty:

```sh
./markup-server \
  --route=v1::rules:/etc/markup/rules-v1.csv \
  --route=v2::rules:/etc/markup/rules-v2.csv \
  --policy=hash-correlation \
  --listen=:8080
```

The empty middle segment in `v1::rules:...` is the empty variant — Decisions are stamped with `Decision.ModelVersion="v1"` and `Decision.Experiment=""`. Dashboards slice by `model_version` and ignore the experiment axis.

For per-tenant or per-region routing where the stickiness axis is a specific Request field rather than the correlation ID, write a custom main that wires `router.HashFieldPolicy`:

```go
// cmd/markup-server-tenant-routed/main.go (sketch)
import "github.com/helmedeiros/markup-svc/internal/decider/router"

policy := router.HashFieldPolicy{
    Field: func(req markup.Request) string { return req.ProductID },
}
// ... wire routes per --route flag values, then router.New(routes, policy)
```

`HashFieldPolicy` is not exposed via the `--policy` flag because the field closure cannot be passed through a string — operators with a per-field-stickiness need write a thin wrapper main against the `router.Policy` port.

## What's happening

`internal/decider/router` holds `[]Route{ModelVersion, Variant, Decider}` plus a `Policy`. Whether you label the routes as variants (`control`/`treatment`) or as model versions only (variant empty), the mechanism is the same: the policy picks one route per Decide, the router stamps the chosen route's labels on the Decision. See [ADR-0011](../architecture/decisions/0011-router.md).

Per-route hot reload (see [hot-reload.md](hot-reload.md)) works in multi-model mode too — POST `/admin/reload` with `{"model_version":"v2"}` reloads only v2's holder.

## What to check after

- Boot log line names the route count: `markup-server: listening on :8080 (… 2 routes (policy=hash-correlation))`.
- `/decide` Decisions carry `model_version` matching one of the configured routes and `experiment=""` (empty when no variant is set).
- Per-model latency dashboards (via OTel spans sliced by `rule.markup.model_version`) show two distributions.
- Per-model reload via `/admin/reload` `{"model_version":"v2"}` updates v2 without touching v1.

## Relevant ADRs and flags

- [ADR-0011](../architecture/decisions/0011-router.md) — router, the `HashFieldPolicy` extension point
- `--route` (repeatable, variant field can be empty), `--policy`
