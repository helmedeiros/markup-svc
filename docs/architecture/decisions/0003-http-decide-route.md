# 3. HTTP Transport: POST /decide

## Status

Accepted — `internal/httpapi.Decide` and `internal/httpapi.WithCorrelationID` ship in the same release window. The handler covers all five status mappings (200 / 400 / 404 / 405 / 500); the middleware covers both the header-supplied and header-absent UUID-generation paths plus the (unreachable on healthy systems) `crypto/rand` failure path. ADR-0003's design held through implementation.

## Context

ADR-0001 fixed the `markup.Decider` port and ADR-0002 wired a CSV-loaded inmemory `Decider`. Both ADRs left the question of *how callers reach the Decider* open. Callers are other services on the same network, not direct Go consumers, so the service needs a transport.

Three design questions.

### 1. Which transport?

- **HTTP/JSON**: ubiquitous, debuggable with `curl`, easy to load-test, native fit for ops teams. Mature Go stdlib (`net/http`).
- **gRPC**: faster wire, typed contract, native streaming. Adds a `.proto` build step and a service-mesh dependency for callers without an existing gRPC stack.
- **Both**: doubles the surface to maintain.

HTTP/JSON first because:

1. Every internal caller already speaks it.
2. `net/http` ships in stdlib; no new deploy artefacts.
3. The performance gap on a per-`Decide` operation is small relative to the engine's per-rule evaluation cost; JSON serialization is not the bottleneck at expected QPS.
4. gRPC can land later as a parallel transport; both would speak to the same `markup.Decider` port.

### 2. What is the wire format?

The HTTP layer does *not* re-export `markup.Request` and `markup.Decision` as JSON-tagged structs. Adding `json:"..."` tags to the domain port pulls one transport's serialization concern into the domain — that's a leak the hexagonal layout exists to prevent. Instead:

```go
// internal/httpapi/decide.go (sketch)

type decideRequest struct {
    ProductID    string  `json:"product_id"`
    Category     string  `json:"category"`
    CustomerTier string  `json:"customer_tier"`
    Channel      string  `json:"channel"`
    Country      string  `json:"country"`
    Inventory    string  `json:"inventory"`
    TimeWindow   string  `json:"time_window"`
    Amount       float64 `json:"amount"`
}

type decideResponse struct {
    MarkupFactor  float64 `json:"markup_factor"`
    Rule          string  `json:"rule"`
    ModelVersion  string  `json:"model_version"`
    Experiment    string  `json:"experiment,omitempty"`
    CorrelationID string  `json:"correlation_id"`
    EngineAdapter string  `json:"engine_adapter"`
}
```

Two small functions (`toMarkupRequest(decideRequest) markup.Request` and `fromDecision(markup.Decision) decideResponse`) bridge wire to domain. Field naming follows snake_case (the conventional JSON style in this codebase's API space); the bridge is the one place the convention is enforced.

### 3. How are errors mapped to status codes?

| Domain outcome | HTTP status | Body |
|---|---|---|
| Decide returns a Decision | `200 OK` | `decideResponse` JSON |
| Decide returns `markup.ErrNoMatch` | `404 Not Found` | `{"error":"no rule matched"}` |
| Malformed JSON body | `400 Bad Request` | `{"error":"<short reason>"}` |
| Body missing required field | `400 Bad Request` | `{"error":"<short reason>"}` |
| Decide returns any other error | `500 Internal Server Error` | `{"error":"internal"}` |

`ErrNoMatch` as `404` is the right shape: the request was well-formed but no rule fired. Callers branching on this status can apply their own fallback. The `500` body is intentionally opaque — the actual error lands in logs and traces, not the response, so we never leak internal state to external callers.

### 4. How does correlation flow?

bre-go's `engine.WithCorrelationID(ctx, id)` / `engine.CorrelationIDFromContext(ctx)` is already the cross-system identity mechanism every Decider populates onto `Decision.CorrelationID`. The HTTP layer needs to inject the ID into the request context before calling Decide:

- Read `X-Correlation-ID` from the request header.
- If absent or empty, generate one (`crypto/rand`-backed UUID v4).
- `ctx = engine.WithCorrelationID(ctx, id)` before dispatching to the Decider.
- Echo the active ID on the response header (`X-Correlation-ID`) regardless of whether it was supplied or generated.

The injection lives in middleware (`internal/httpapi/correlationid.go`) so every route added later inherits it for free.

## Decision

`internal/httpapi` ships:

- `Decide(d markup.Decider) http.Handler` — closure-bound to a Decider, accepts `POST /decide` only, JSON-encodes both directions, maps errors per the table above. Methods other than POST get `405 Method Not Allowed`.
- `WithCorrelationID(next http.Handler) http.Handler` — middleware reading / generating the ID and injecting it via `engine.WithCorrelationID`.
- Internal wire types (`decideRequest`, `decideResponse`) and the two bridging conversion functions. None of them are exported beyond the package.

`cmd/markup-server` wires:

```go
mux := http.NewServeMux()
mux.Handle("POST /decide", httpapi.Decide(decider))
srv := &http.Server{Handler: httpapi.WithCorrelationID(mux), ...}
```

## Consequences

### Closed by this ADR

- One route. Adding new routes goes through new ADRs.
- JSON only. No content negotiation matrix.
- snake_case field naming in wire JSON.
- ErrNoMatch maps to 404. Internal errors map to opaque 500.
- Correlation ID flows in via header or generation, never silently dropped.

### NOT closed by this ADR

- Auth. The service is currently expected to sit behind a trusted boundary; integrating auth (token, mTLS, sigv4) lands in a separate ADR.
- Rate limiting. Same boundary argument; if a noisy caller appears, a token bucket adapter lands in its own ADR.
- gRPC transport. Parallel transport when a caller actually asks.
- Multi-tenant routing. The current Decider is single-tenant; tenant routing lands when tenant identity becomes a request field.
- Streaming / batch decide. Out of scope for the single-decision route.

### Performance impact

Per-request cost, honest accounting:

- One `json.NewDecoder(r.Body).Decode(&dr)` (preferred over `io.ReadAll` + `json.Unmarshal` — avoids materializing the full body).
- One `json.NewEncoder(w).Encode(resp)` (writes streaming; the server emits chunked transfer-encoding without an explicit `Content-Length`, which is the standard `net/http` behaviour and the correct trade for single-shot JSON).
- Two wire-struct allocations: `decideRequest` (filled by Decode) and `decideResponse` (filled by the bridge). Escape analysis decides stack vs heap; the bridges are tiny so heap escape is likely.
- One `context.WithValue` allocation in the middleware to carry the correlation ID, plus one extra handler frame in the call stack. Both are constant-cost.
- If `X-Correlation-ID` is missing, one `crypto/rand`-backed UUID v4 generation. This is a syscall — measurably slower than `math/rand` but `crypto/rand` is the right choice for cross-system trace IDs (unguessable, no shared mutable state). The cost is acceptable because the typical caller already supplies the header.
- Existing per-`Decide` cost from ADR-0002: one `markup.FactOf` map allocation, one `Decision` return value.

Aggregate per-request profile (caller supplies correlation header, the typical case): ~5 heap allocations on top of the engine's per-rule evaluation. None grow with the rule set, and none are larger than the JSON buffers themselves. JSON I/O is bounded and predictable; the variable cost remains the Decider's per-rule evaluation.

### Validation strategy

- `internal/httpapi` ships table-driven tests using `httptest.NewRecorder`: happy path, ErrNoMatch → 404, malformed JSON → 400, wrong method → 405, internal error → 500.
- Correlation ID middleware tests: header supplied → passed through unchanged; header absent → generated and echoed; ctx contains the active ID.
- `cmd/markup-server` ships an end-to-end test that spawns the real server, posts JSON over the wire, asserts the response matches the Decider's output.
