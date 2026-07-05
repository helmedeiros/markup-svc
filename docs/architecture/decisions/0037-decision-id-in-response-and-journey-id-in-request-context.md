# 37. decision_id in /decide response + journey_id in request_context

## Status

Accepted — additive, backward-compatible HTTP-contract change. Callers that ignore the new response field keep working. The change unlocks the outcome half of the C4 Learning Loop by giving downstream funnel-event emitters (a new `funnel-sim` service in the near term; real search-svc / booking-svc / payment-svc in the long term) two join keys — `decision_id` per priced offer and `journey_id` per customer session — that they can attach to `booking.v1` and `search.v1` events. Locks the wire contract before the funnel-event repo is scaffolded, so the funnel-event schema can reference these keys with confidence they will not move.

## Context

ADR-0035 minted a per-decision `decision_id` server-side and emitted it as the primary identity on the `markup.decision.v1` event. That closed the "context + decision" half of the C4 Learning Loop arrow. The "outcome" half — join a downstream booking, purchase, or timeout event back to the priced offer that led to it — is still not implementable end-to-end. Two gaps:

- **decision_id lives only inside the event, not in the response.** A caller invoking `/decide` receives the priced offer (`markup_factor`, `rule`, `model_version`, `experiment`, `correlation_id`, `engine_adapter`), but not the `decision_id` that was minted for that call. When that caller subsequently emits a downstream event (`search.v1.offers[].decision_id`, `booking.v1.decision_id`), it has nothing to put in the field. The event stream already has decision-side IDs — the response does not, so the join is a one-way street.

- **journey_id has no wire position.** A caller running one session (search page render → click → book → checkout) issues N `/decide` calls that share a session identity. Today the only identity carried through is `correlation_id` (bre-go's per-request ID from `CorrelationIDFromContext`), which is per-HTTP-call, not per-session. `trace_id` is per-request too. Neither is the right shape for "group all pricing decisions and outcomes for one visitor session". A dedicated `journey_id` field, caller-supplied, echoed onto the event, is what the analytics + A/B lift math need (see `~/Code/workspaceBRE/learning-loop-plan.local.md` §Correlation model for the full argument).

Neither gap was worth solving during the ADR-0035 pass — outcome attribution was explicitly parked in that ADR's Not-closed list. It becomes worth solving now because the funnel-event work about to land in a sibling repo (`funnel-sim`) needs both keys on the wire before its event schema can be committed to.

## Decision

### Request-body change (additive, optional)

Add a nested `request_context` sub-object to the POST `/decide` JSON body. In v1 the sub-object contains exactly one optional field:

```jsonc
{
  "product_id": "…",
  "category":   "…",
  "customer_tier": "…",
  "channel":    "…",
  "country":    "…",
  "inventory":  "…",
  "time_window":"…",
  "amount":     49.99,
  "request_context": {
    "journey_id": "…"    // optional; opaque; caller-supplied
  }
}
```

Semantics:
- The field is optional. A request without `request_context`, or with `request_context: {}`, behaves exactly like the pre-ADR-0037 request.
- The field is opaque to markup-svc: no format validation, no length limit at the handler layer (the mux's request-body limit is the only backstop). The caller owns the format; the design-doc guidance (`~/Code/workspaceBRE/learning-loop-plan.local.md` §Correlation model) recommends a 32-char lowercase-hex ID for consistency with `decision_id`, but nothing in markup-svc enforces this.
- The field is caller-supplied. It is not minted server-side. A `funnel-sim` client generating synthetic sessions mints it; a real BFF passing through from the browser's cookie mints it (or extracts it from an incoming header); an internal ops caller running `/decide` for ad-hoc pricing checks may omit it and pay no penalty.

### Response-body change (additive)

Add `decision_id` to the `/decide` response body. The value is the same server-minted ID that ADR-0035 already emits on the `markup.decision.v1` event — this ADR simply surfaces it to the client.

```jsonc
{
  "decision_id":    "a1b2c3d4e5f60718…",   // 32-char hex (per ADR-0035 default)
  "markup_factor":  1.15,
  "rule":           "enterprise",
  "model_version":  "v1",
  "experiment":     "…",                    // omitempty
  "correlation_id": "c-…",
  "engine_adapter": "*inmemory.Engine"
}
```

Semantics:
- `decision_id` is emitted with `omitempty`. On a degraded host where `crypto/rand.Read` failed and the `WithDecisionIDSource` default returned `""`, the response omits the key entirely rather than emitting `"decision_id": ""` — a valid-looking but unusable value. This matches the ADR-0035 event-emission contract, which silently skips the `markup.decision.v1` event on empty ID (see `TestDecide_EmptyDecisionIDSuppressesEvent`). Callers see either a real ID or an absent key; never an empty string.
- No format guarantees beyond what ADR-0035 already documents: opaque to consumers, 32-char lowercase hex under the default source, implementation reserves the right to swap to ULID or UUID without a schema bump.

### Emission seam for journey_id

`journey_id` flows into the emitted `markup.decision.v1` event via the existing `request_context` sub-object on the event body (the `map[string]any` catch-all whose field set today mirrors the access-log `input` block). When the client supplies `request_context.journey_id`, the handler stores it on `decisionLogEntry.journeyID`. The `WithAccessLog` middleware adds `journey_id` as a key to the `ctxFields` map it builds before emission, so both the ADR-0021 access log (`attrs.input.journey_id`) and the ADR-0035 decision event (`attrs.request_context.journey_id`) carry the value.

Zero schema change on the event struct: `RequestContext map[string]any` was already there. Zero new top-level field on `markup.decision.v1`. `schema_version` stays `1.0.0` — this is an additive population of an already-defined catch-all sub-object, not a new schema field.

### Wire compatibility

- Existing callers that do not send `request_context` continue to work bit-for-bit. Existing callers that ignore the new `decision_id` response field continue to work bit-for-bit. Both changes are additive at the JSON level.
- Router mode (`wireRouterHandler` in `cmd/markup-server/main.go`) mounts the same `httpapi.Decide` handler behind the router, so the wire contract change lands in single-decider and router modes without a second edit.

## Consequences

### Positive

- The outcome half of the Learning Loop becomes implementable. A `funnel-sim` service (next arc commit) receives the `decision_id` on `/decide` and can attach it to every downstream `booking.v1.decision_id` field. Analytics joins `markup-decisions` and `funnel-events` on `decision_id` and computes conversion + elasticity per experiment arm (see `~/Code/workspaceBRE/learning-loop-plan.local.md` §Elasticity for the SQL shape).
- Session-scoped analytics unlock. A funnel query grouping by `journey_id` sees the full sequence of pricing decisions a customer saw during one visit, independent of how many `/decide` calls the search page fanned out to. `correlation_id` and `trace_id` remain useful for distributed tracing but are the wrong scope for session-level analytics (documented in the correlation-model section of the arc plan).
- Self-contained A/B experiment measurement. Two `traffic-gen` instances running the arc's session-driver at different walk probabilities produce a measurable lift signal against the same markup-svc experiment flag; conversion and elasticity fall out of a single DuckDB self-join on `decision_id`. This ADR is the wire piece that makes that end-to-end measurement possible without external tooling.
- Zero API migration burden. No consumer needs to change to keep working. Consumers that opt in (funnel-sim, later real services) get the two new keys. This applies to the JSON contract only — the per-request heap footprint is discussed in Negative below.

### Negative

- The `decideRequest` struct grows a nested sub-struct. The `fromDecision` bridge function gains a second parameter (`decisionID string`). Both are one-line diffs but touch the wire boundary; a future re-shaping of `request_context` (e.g., splitting `journey_id` out to a top-level field) has to plan for callers already sending it nested.
- `decisionLogEntry` grows by one `string` header (+16 bytes on 64-bit), from 264 bytes to 280 bytes. This struct already escapes to the heap through `context.WithValue` on every `/decide` call, so the growth is a size increase on an existing allocation, not a new allocation site. At 500 QPS the extra GC pressure is ~8 KB/s against the pre-existing ~132 KB/s baseline. Not measured against a pre-registered bar per ADR-0012 protocol — this ADR ships wire-contract work, not a performance change, and the growth is bounded and quantified.
- The middleware's `ctxFields` map now can carry a non-`markup.Request`-derived key (`journey_id` from `decisionLogEntry.journeyID`, not from `inputFields(d.request)`). Future refactors of `inputFields` must remember the journey_id augmentation happens outside it. Guarded by `TestDecide_JourneyIDEchoedToRequestContext` and `TestDecide_MissingJourneyIDLeavesKeyAbsent`.
- `journey_id` is caller-supplied and unvalidated. A caller that sends a 10 KB string as `journey_id` gets it echoed to the event without complaint; downstream S3 objects grow accordingly. Acceptable at v1 given the DMZ threat model, but flagged for the follow-on auth ADR: any client-side limit should be enforced there, not in the wire schema.

### Neutral

- `schema_version` stays `1.0.0`. Populating an already-defined catch-all sub-object with a new key is not a schema change per ADR-0035's Versioning rules ("Minor bump for new optional fields; consumers must ignore unknown fields"). Downstream consumers that unmarshal `request_context` into a fixed struct today should either move to `map[string]any` (matches the schema's actual shape) or add `journey_id` to their struct. Either path is a consumer-side choice; markup-svc emits the same schema version.

## Not closed (deferred to follow-on ADRs)

- **Detection surface for missing / malformed journey_id.** A caller that ignores the optional `journey_id` produces markup events with the key absent and, once the arc lands, funnel events with a populated key — a silent join that returns zero rows rather than a visible error. Deferred with an explicit trigger: added in arc-commit #2 (`funnel-sim`) or arc-commit #3 (`traffic-gen` session driver), whichever first emits journey_id in an integration test. Concrete shape: a `markup_decisions_missing_journey_id_total{env}` counter incremented in `WithAccessLog` when `journeyID` is empty, exposed on `/metrics` alongside the other decision counters. Not added in this commit because no caller emits journey_id yet — the counter would be uniformly zero, an over-eager metric with no signal to observe.
- **journey_id size cap.** v1 accepts any string. A caller sending a 10 KB `journey_id` inflates every MinIO object silently. Deferred with an explicit trigger: added alongside the detection counter in arc-commit #2 or #3, or when the first non-arc BFF integrator lands — whichever is earlier. Concrete shape: a 64-byte hard cap at the handler layer (2× the recommended 32-char hex format), rejected with a `markup_decisions_journey_id_oversized_total{env}` counter increment plus a debug log rather than a 400. Enforcement placement (handler vs. a future auth middleware) is decided in the follow-on ADR.
- **journey_id format enforcement.** v1 accepts any string. If a future consumer needs a stable parser (e.g., a fixed-length hex ID for partitioning), a follow-on ADR pins the format at the wire boundary. Enforcement placement (handler validation vs. edge validation in a future auth middleware) is part of that ADR.
- **Header propagation.** A caller-supplied `journey_id` is currently only accepted in the JSON body. A follow-on ADR may accept it as `X-Journey-Id` on the request headers too — useful for BFFs that add correlation IDs at the edge rather than mutating the body. Not needed for v1: `funnel-sim` synthesizes the body payload from scratch.
- **Additional request_context fields.** `journey_id` is the load-bearing first key. Future additions (`search_id`, `journey_started_at`, `journey_arm` for pre-decided A/B assignments) may want a home in the same sub-object. This ADR does not enumerate them; each is a follow-on additive change.
- **Client-side journey_id minting guidance.** The correlation-model doc recommends 32-char lowercase hex to match `decision_id`. That guidance may become a documented protocol (e.g., an internal wiki entry, an SDK helper) once real BFFs land. Not in this ADR's scope.

## References

- ADR-0003 — HTTP /decide route (where `decideRequest` / `decideResponse` live, the JSON-boundary bridge, snake_case convention).
- ADR-0035 — Decision-event contract (mints `decision_id`, defines the `markup.decision.v1` schema this ADR echoes `journey_id` into).
- ADR-0036 — Decision-event substrate (MinIO / S3 batched JSONL — where the events with the echoed `journey_id` land).
- `~/Code/workspaceBRE/learning-loop-plan.local.md` — arc-level design record. §Correlation model (why journey_id + decision_id are the load-bearing pair), §Event contracts (search.v1 / booking.v1 shapes), §Implementation plan (this ADR is arc-commit #1).
- Pricing Decision Platform C4 sketch — "Learning Loop (context + decision + outcome)" arrow. This ADR closes the outcome-attribution wire gap.
