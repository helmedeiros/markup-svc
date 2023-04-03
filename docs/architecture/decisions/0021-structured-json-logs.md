# 21. Structured JSON logs (boot, access, shutdown)

## Status

Accepted — `internal/jsonlog` ships a small `Logger` with `Info/Warn/Error` methods that emit `{time, level, msg, attrs}` per the platform-standard JSON shape. The cmd replaces the plain-text `markup-server: listening on ...` boot line with a `markup-server.boot` event and the `markup-server: shutting down` line with `markup-server.shutdown`. The new `httpapi.WithAccessLog` middleware emits one `markup-server.access` event per request with `method / path / status / duration_ms / correlation_id / trace_id / span_id` attrs — matching decision-gateway's `gateway.access` shape so Filebeat's `decode_json_fields` lands all platform services' log lines in the same `platform-logs-*` index with consistent `attrs.*` fields.

## Context

Before this ADR markup-svc emitted plain text on stdout. Filebeat still shipped the lines but they landed under `message` instead of `attrs.*`, so Kibana queries on `attrs.correlation_id` worked for traffic-gen + decision-gateway events but not markup-svc events. Operators investigating a request across the three services had two different query shapes to remember.

decision-gateway/ADR-0003 had already added `trace_id` + `span_id` to its access log so Kibana → Jaeger is two clicks. This ADR brings markup-svc to parity: same JSON shape, same trace-correlation fields.

## Decision

`internal/jsonlog/jsonlog.go`: a thread-safe `Logger` wrapping any `io.Writer`. `Info/Warn/Error(msg, attrs)` write a single JSON line terminated by `\n`. `attrs` is `map[string]any` carried under the `attrs` field (omitted when nil).

`internal/httpapi/accesslog.go`: `WithAccessLog(*jsonlog.Logger, http.Handler) http.Handler`. Wraps the response writer to capture status code, runs the inner handler, emits one event per request. Reads `correlation_id` from the engine context + `trace_id` / `span_id` from the OTel SpanContext when valid; omits the fields when absent so /healthz + /readyz events stay terse. Nil logger short-circuits to a pass-through so the existing zero-logger paths (tests, wireHandler) keep working.

`cmd/markup-server/main.go`: builds the logger once after flag parsing, passes it into both wire functions, mounts the middleware via `WithCorrelationID(WithTraceContext(WithAccessLog(log, mux)))` so the access log sees the correlation ID + trace context that the outer middleware extracted.

## Consequences

### Closed

- Kibana queries on `attrs.correlation_id` now hit markup-svc events too. The single-request lifecycle across traffic-gen → gateway → markup-svc is queryable with one filter.
- markup-svc → Jaeger hop becomes two clicks like the gateway: click `attrs.trace_id` in Kibana, paste into Jaeger.
- The boot + shutdown events ingest as structured records: operators slice on `attrs.adapter` to find all boots of a specific engine, on `attrs.rules` to find low-rule deployments, etc.

### Not closed

- `cmd` still uses `fmt.Fprintf(stderr, ...)` for loader-side error events (snapshot read failure, rules build failure). Those go to stderr by convention; converting them to structured events is a future cleanup if Kibana operator workflow wants them indexed.
- The access event is emitted INSIDE the trace context but OUTSIDE the metrics decorator. The OTel span's duration and the access event's `duration_ms` therefore measure roughly the same window (within ~µs); not a contract but useful for cross-validation.
- A small allocation per request for the `attrs` map. Below the noise floor at the platform's measured 10–100 µs hot path.
