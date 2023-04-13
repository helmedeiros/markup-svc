# 23. Access log carries the matched rule, inputs, and outputs

## Status

Accepted — `httpapi.Decide` writes the parsed `markup.Request` + the returned `markup.Decision` (or `no_match=true`) onto the request context via `withDecisionContext`. `WithAccessLog` reads them at end-of-frame and merges into the JSON event as `attrs.input.*` (only fields actually set), `attrs.rule`, `attrs.markup_factor`, `attrs.model_version`, `attrs.engine_adapter`, optional `attrs.experiment`, and `attrs.no_match` on the no-rule-matched path.

## Context

ADR-0021 brought markup-svc's log shape to platform parity (boot / access / shutdown JSON events). The access event carried only the HTTP envelope: method, path, status, duration_ms, plus correlation_id / trace_id / span_id for cross-signal correlation. The matched rule, the request inputs, and the resulting factor were on the OTel span (`rule.markup.rule`, `rule.markup.factor` etc. per ADR-0009) but not in Kibana. Operators looking for "every request that matched the `enterprise` rule" or "what input drove this 1.50 factor" had to pivot to Jaeger.

The pattern of logging matched-rule + inputs + outputs at every Decide is well-established in production rules-engine services; adopting it brings markup-svc to that level of operator visibility without inventing a new schema.

## Decision

`httpapi/decisionlog.go` (new file): unexported `decisionLogKey` + `decisionLogEntry` + `withDecisionContext` / `decisionFromContext` helpers. The entry holds the request, the decision, and a `noMatch` bool — mutually exclusive on the happy path.

`httpapi/decide.go`: after `d.Decide(...)` returns, the handler calls `withDecisionContext` on the request's context before writing the response. The trick `*r = *r.WithContext(...)` updates the outer request value the middleware reads — `r.WithContext` alone would mutate a copy. On `markup.ErrNoMatch` the entry is set with `noMatch: true`; on other errors no entry is set (the 5xx event stays minimal).

`httpapi/accesslog.go`: after the inner handler returns, the middleware checks for a `decisionLogEntry` in the context. If present, it merges:

- `attrs.input` — only fields actually set on the Request (zero values omitted to keep events terse)
- `attrs.rule` / `attrs.markup_factor` / `attrs.model_version` / `attrs.engine_adapter` — from the Decision
- `attrs.experiment` — only when non-empty
- `attrs.no_match: true` — instead of rule/factor when no rule matched

The `inputFields` helper is unexported and lives next to the middleware so the field list stays close to the writer.

## Consequences

### Closed

- Kibana queries gain the operator-visible filters: `attrs.rule:"enterprise"` to find every match; `attrs.markup_factor:>=1.5` for high-factor outcomes; `attrs.input.country:"BR"` for per-country slices; `attrs.no_match:true` for the no-match tail.
- The event carries the same data the OTel span already carries on the engine layer; signal symmetry across logs + traces is maintained.
- A 5xx error path stays minimal (no rule, no decision, no input). The 4xx no-match path keeps the input + no_match flag so operators can spot patterns in what's not being matched.

### Not closed

- Selective input redaction. The current Request struct holds no PII (it's all categorical: country, channel, customer_tier — no user IDs or amounts). When the schema grows to include sensitive fields, a `WithRedactedFields` option on the middleware lands.
- Per-rule input projection. Some rules use only 2 of 8 fields; logging all set fields is more verbose than necessary. Filebeat's `decode_json_fields` handles the size; if Elasticsearch index pressure becomes a problem, a per-rule input filter at the rule's matched-fields level lands.
- Latency: an extra context value read + map allocation per request. ~50 ns; below the noise floor.
