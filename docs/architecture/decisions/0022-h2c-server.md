# 22. h2c (HTTP/2 cleartext) on the markup-svc server

## Status

Accepted — `cmd/markup-server` wraps the HTTP handler with `h2c.NewHandler(handler, &http2.Server{})` so the listener handles HTTP/1.1 + HTTP/2-over-cleartext from the same port. HTTP/1.1 clients keep working unchanged; HTTP/2 clients using prior knowledge get connection multiplexing — a single TCP connection carries N concurrent requests instead of N TCP connections.

## Context

The cross-service trace work measured the gateway → markup-svc network hop at ~310 µs median on native arm64 after the connection-pool tuning (decision-gateway/ADR-0005). That cost is mostly TCP frame round-trip + per-request HTTP/1.1 head-of-line latency. HTTP/2 multiplexing eliminates the per-connection serialization: many in-flight requests share one TCP connection, frame-interleaved.

The gateway change to use HTTP/2 upstream lives in the decision-gateway repo. This ADR is the server-side enabler — without h2c on markup-svc, the gateway has no h2 target to talk to.

## Decision

`cmd/markup-server/main.go` imports `golang.org/x/net/http2` + `golang.org/x/net/http2/h2c` (the x/net dep was already indirect; pinned to `v0.8.0` for Go-1.18 compatibility). The `http.Server`'s `Handler` becomes `h2c.NewHandler(handler, &http2.Server{})`. No new flag; the wrap is always on. HTTP/1.1 still works because h2c is a strict superset — clients negotiate per the standard.

## Consequences

### Closed

- markup-svc accepts HTTP/2 over cleartext on the same listener that already serves HTTP/1.1.
- A unit test in `main_h2c_test.go` opens an h2c client (`http2.Transport{AllowHTTP: true, DialTLSContext: net.Dialer.DialContext}`) and asserts `resp.ProtoMajor == 2`.

### Not closed

- TLS-terminated HTTP/2. The platform compose runs all services on plaintext within the Docker network. Production deployments add TLS on the gateway's external listener; backends typically stay h2c.
- gRPC. Not needed today; the platform speaks JSON over HTTP. If gRPC ships later, h2c is the substrate it would use.
- Performance measurement. The expected gain (gateway → markup-svc hop dropping from ~310 µs to ~50–100 µs at sustained QPS) lands once the gateway switches its upstream transport. That's the follow-up release in decision-gateway.
