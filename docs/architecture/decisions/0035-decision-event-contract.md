# 35. Decision-event contract — versioned schema for downstream consumers

## Status

Accepted — lock the schema for `markup.decision.v1`. The event emits from the access-log middleware (reading the same `decisionLogEntry` already populated by the `/decide` handler), running in parallel with the existing `markup-server.access` event until a follow-on access-log-slimming ADR removes the duplicated decision attrs. `decision_id` is a 32-character hex-encoded 128-bit random ID minted from `crypto/rand`; the format is documented as opaque for downstream consumers so the implementation can swap to ULID or UUID later without a schema bump.

## Context

The Pricing Decision Platform's published C4 sketch shows a "Decision Event Stream" container in the Pricing Decision Platform boundary feeding three downstream consumers: a Feature Store, an ML Training Pipeline, and the Rule Management Pipeline. The arrow label is "Learning Loop (context + decision + outcome)". Today the corresponding runtime substrate does not exist:

- The closest thing to a per-decision event is the `markup-server.access` structured log emitted by `WithAccessLog` (ADR-0021 + ADR-0023). When the `/decide` handler populates `decisionLogEntry` on the request context, the access-log middleware enriches the JSON event with `rule`, `markup_factor`, `model_version`, `experiment`, `engine_adapter`, `no_match`, and the `input` request fields.
- That mixes two concerns: HTTP-layer telemetry (method, path, status, duration_ms, status code) and decision telemetry (rule, factor, model_version). The access log is the right place for the first; it carries the second by historical accident.
- The shape evolves freely under ADR-0023's `decisionLogEntry` struct. Any downstream consumer (Kibana saved searches in pricing-observability, a future feature store, a future training pipeline) has no schema contract. A markup-svc release that renames a field silently breaks every consumer.
- No per-decision ID exists today. `correlation_id` (W3C trace + bre-go's `CorrelationIDFromContext`) is the closest, but a single correlation ID can fan out across many downstream `/decide` calls (e.g., the gateway broadcasts a search query to N markup-svc evaluations). Decision events keyed only on `correlation_id` cannot be joined to a single decision unambiguously.
- No outcome attribution. Whether the priced offer led to a click → checkout → booking is unknown to markup-svc; even joining it externally requires a stable `decision_id` to propagate.
- No durable replay substrate. The access-log → Filebeat → Elasticsearch path optimises for ad-hoc Kibana queries and short retention; it is not suitable for offline training-set materialisation or for replaying a six-month-old decision against a new ruleset.

Three things need to land for the Learning Loop to be implementable. This ADR is the first.

1. A schema contract for decision events. **This ADR.**
2. A durable substrate (S3 batched JSONL, Kafka, or similar). Separate ADR.
3. Outcome attribution. Cross-service work; separate ADR spanning decision-gateway + downstream consumers.

## Decision

### `markup.decision.v1` schema

A versioned JSON schema for one decision event:

```jsonc
{
  "schema_version": "1.0.0",            // SemVer; additive minor, breaking major
  "decision_id": "a1b2c3d4e5f60718…",   // opaque per-decision ID (default: 32 hex chars, 128-bit from crypto/rand); minted in /decide handler
  "ts": "2024-09-12T10:42:00.123456Z",  // RFC 3339 UTC, microsecond precision
  "env": "production",                  // ADR-0034 process-level env

  // Routing / model identity
  "model_version": "v1",
  "experiment": "",                     // "" when not routed; from ADR-0011
  "engine_adapter": "*indexed.Engine",

  // The decision itself
  "decide_outcome": "ok",               // ok | no_match | canceled | deadline_exceeded | error
  "rule": "enterprise",                 // "" unless decide_outcome=ok
  "markup_factor": 1.15,                // 0 unless decide_outcome=ok
  "error": "",                          // populated only when decide_outcome=error

  // Wall-clock cost of the full /decide handler envelope (matches the
  // access-log duration_ms semantic for direct comparison).
  "duration_ms": 0.487,

  // Identity carried through the request
  "correlation_id": "c-...",            // bre-go CorrelationIDFromContext; can repeat across decisions in one flow
  "trace_id": "0123456789abcdef…",      // OTel trace ID; can repeat
  "span_id": "0123456789abcdef",        // OTel span ID for THIS decision; unique per decision
  "request_context": {                  // exact set already emitted by access log
    "product_id": "abc",
    "category": "rail",
    "customer_tier": "enterprise",
    "channel": "web",
    "country": "DE",
    "inventory": "available",
    "time_window": "off_peak",
    "amount": 49.99                     // see Privacy note below
  }
}
```

Empty-string and zero-value fields are emitted explicitly so a downstream batch consumer (e.g., Spark / Snowflake) sees a stable column schema rather than a sparse one. The `request_context` sub-object's field set matches the existing access-log `input` block bit-for-bit so the migration from access-log consumption to decision-event consumption is a key rename, not a re-modelling.

`decide_outcome` mirrors the `outcomeFor` enum already emitted by the prom adapter (`internal/observability/metrics/prom/prom.go`). Including it in v1.0 — rather than splitting `no_match`, `canceled`, and `error` across separate boolean fields — keeps the schema flat and matches the metric label set downstream consumers already see.

### Emission seam

A new structured-log event named `markup.decision.v1`. The `WithAccessLog` middleware (extended) reads the existing `decisionFromContext` lookup and, after the inner handler returns, emits the decision event in addition to the existing `markup-server.access` event. Both events run through the same `jsonlog.Logger`. The handler's responsibilities do not grow: it keeps populating `decisionLogEntry` exactly as today (per ADR-0023). The emission seam stays in middleware.

`decision_id` is minted in the `/decide` handler — earliest point where a decision exists — and stored on a new `decisionID string` field on `internal/httpapi.decisionLogEntry` (the same struct the middleware already reads via `decisionFromContext` under the private `decisionLogKey{}` context key). The handler accepts a new `httpapi.WithDecisionIDSource(fn func() string) DecideOption`, mirroring the existing `WithShadow` / `WithShadowLogger` / `WithReloadBodyLoader` pattern. The default source is an unexported `newDecisionID()` function that reads 16 bytes from `crypto/rand` and hex-encodes them; tests inject a deterministic stub. No new external dependency is introduced.

### Event-name versioning

Major-version bumps embed the version in the event name itself: `markup.decision.v1`, `markup.decision.v2`. Consumers filtering on `msg:"markup.decision.v1"` stay isolated from a future v2 emission running in parallel. Minor and patch bumps (additive within a major) keep the same event name; `schema_version` distinguishes them inside the event body for consumers that care.

### Versioning rules

`schema_version` is SemVer applied to the contract:

- **Patch** (`1.0.0` → `1.0.1`): no schema change. Reserved for documentation / wording corrections in this ADR.
- **Minor** (`1.0.0` → `1.1.0`): additive only. New optional fields. Old consumers must ignore unknown fields and keep working. A minor bump never requires a downstream code change and does not change the event name.
- **Major** (`1.0.0` → `2.0.0`): breaking. Rename, removal, type change. New event name (`markup.decision.v2`). Requires parallel emission of v1 and v2 for the migration window documented in the breaking ADR.

### Migration window vs `markup-server.access`

For the release window covering this ADR's Accepted flip through the access-log-slimming ADR's implementation:

- `markup-server.access` keeps its current shape including the `rule`, `markup_factor`, `model_version`, `experiment`, `engine_adapter`, `no_match`, and `input` enrichment from `decisionLogEntry`. Existing Kibana saved searches (e.g., the `MarkupDecideP99Slow.md` runbook's `msg:"markup-server.access" AND attrs.no_match:true`) keep working unchanged.
- `markup.decision.v1` is emitted in parallel from the same middleware.
- The access-log slimming is a separate ADR. That ADR removes the decision-context fields from `markup-server.access`, lists every consumer that needs to migrate, and updates the pricing-observability Kibana searches + runbooks. Window end is defined by that ADR's Accepted + implementation, not by a version-increment cadence.

### Privacy posture for v1

`request_context.amount` is a customer-visible monetary value. Other request-context fields (`country`, `customer_tier`, `channel`) are categorical and lower-risk; `product_id` may be tenant-sensitive depending on the deployment. The v1 schema emits these verbatim — operators who require hashing / redaction before the event leaves the process get that via a future privacy-policy ADR (see Not closed). Implementers reading this ADR before the privacy ADR ships should flag `amount` and `product_id` in their deployment review.

### Not closed (deferred to follow-on ADRs)

- **Durable substrate.** This ADR locks the schema, not where the events land. v1 default is Filebeat → Elasticsearch (existing path). S3 batched JSONL, Kafka, NATS, Pulsar are all candidates for a substrate ADR.
- **Outcome attribution.** The `outcome` field is intentionally absent from v1. Adding it requires a cross-service contract: decision-gateway propagates `decision_id` downstream, and whichever service witnesses the outcome (booking-service, checkout, payment) emits a `markup.outcome.v1` event keyed on `decision_id`. That's a multi-repo ADR.
- **Access-log slimming.** Removing the decision-context fields from `markup-server.access` is its own ADR; it defines the migration window for this ADR.
- **Backpressure / batching.** A single `jsonlog.Logger.Info` per decision at 2000 QPS is honest at current load. Buffering / batching belongs in a sink-substrate ADR if log volume becomes a bottleneck.
- **Privacy redaction.** Hashing / redaction policy for `amount`, `product_id`, and any future PII fields. Operators with PII concerns hold this ADR until the privacy policy is documented.

## Consequences

### Positive

- Downstream consumers have a stable schema. A feature store, training pipeline, or replay tool can pin to `markup.decision.v1` and expect bit-for-bit stability under additive minor bumps.
- `decision_id` is the key downstream systems need to join decisions to outcomes once outcome attribution lands.
- The split between HTTP-layer telemetry (access log) and decision telemetry (this event) cleans up the ADR-0023 conflation.
- Once a substrate ADR lands, the C4 "Decision Event Stream" container becomes implementable without re-modelling the event shape.

### Negative

- Doubles log volume on the `/decide` path during the migration window. At 2000 QPS × ~500 B/event × 2 events = ~2 MB/sec sustained. Filebeat / Elasticsearch handle that; flagged for operators with tight log-storage budgets.
- Breaking change for any consumer that explicitly enumerated the `markup-server.access` decision attrs at the end of the migration window. Mitigation: explicit deprecation list in the access-log-slimming ADR; pricing-observability's Kibana saved searches updated as part of that ADR's implementation.
- The `decision_id` mint adds one `crypto/rand.Read(16 bytes)` syscall and one hex-encode per `/decide` call. The exact per-call cost is unmeasured at the time of this ADR; a scientific-harness bar pre-registration is parked for a follow-on commit per ADR-0012 protocol.
- The middleware now branches on `decisionFromContext` to emit two events instead of one. The second `l.Info` call doubles the per-Decide structured-log emission cost. `decisionEventAttrs` allocates one `map[string]any` per emission (~14 keys) plus reuses the access-log `inputFields` map for the `request_context` sub-object to avoid a second nested allocation. Honest qualifier: the per-Decide cost of the dual emission is not yet measured against a pre-registered bar; the follow-on scientific harness commit covers it.

## References

- ADR-0021 — Structured JSON logs across boot, access, shutdown (`markup-server.access`).
- ADR-0023 — Access events carry matched rule + input + output (`decisionLogEntry` mechanism).
- ADR-0033 — Shadow sample rate + structured log (`markup.challenger.evaluate` — the same emission pattern this ADR generalises).
- ADR-0034 — Env label on shadow metrics + access log + decide span (the env field this ADR inherits).
- model-registry ADR-0013 — `/shadow-stats` endpoint (parallel pattern for aggregate-side telemetry).
- Pricing Decision Platform C4 sketch — "Learning Loop (context + decision + outcome)" arrow between the Pricing Decision API and the Decision Event Stream container.
