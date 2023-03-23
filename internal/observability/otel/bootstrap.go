package otel

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// Shutdown is the cleanup function returned by Bootstrap. Callers
// defer it at process exit so pending spans flush and the exporter
// stops cleanly.
type Shutdown func(ctx context.Context) error

// Bootstrap initialises an OTel TracerProvider with an OTLP gRPC
// exporter and sets it as the global provider. The exporter reads
// the standard OTel SDK environment variables for endpoint, headers,
// compression, TLS, etc.:
//
//   - OTEL_EXPORTER_OTLP_ENDPOINT (default localhost:4317)
//   - OTEL_SERVICE_NAME (sets resource service.name attribute)
//   - OTEL_RESOURCE_ATTRIBUTES (additional resource attributes)
//   - OTEL_EXPORTER_OTLP_PROTOCOL (must be grpc for this bootstrap;
//     http/protobuf would need the otlptracehttp client)
//
// Closes ADR-0016: --otel-enabled now produces a working SDK that
// exports to an OTLP collector instead of dropping spans on the
// no-op TracerProvider.
//
// instrumentationName is the name used for the returned tracer
// (typically the binary's import path).
//
// When --otel-enabled is not set, cmd does not call Bootstrap and
// the global TracerProvider stays the SDK's NoopTracerProvider --
// the per-Decide span operations are no-ops and zero allocations.
func Bootstrap(ctx context.Context, instrumentationName string) (trace.Tracer, Shutdown, error) {
	exp, err := otlptrace.New(ctx, otlptracegrpc.NewClient())
	if err != nil {
		return nil, nil, fmt.Errorf("otlptrace gRPC exporter: %w", err)
	}

	// resource.New picks up OTEL_SERVICE_NAME / OTEL_RESOURCE_ATTRIBUTES
	// via the default detectors. A failure here is logged but not
	// fatal -- the SDK will produce spans without a resource if the
	// detectors panic; in practice on Linux they do not.
	res, err := resource.New(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("resource detection: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
	)

	otel.SetTracerProvider(tp)

	// The global TextMapPropagator drives header-based trace context
	// hopping. ADR-0017 makes markup-svc a trace-context CONSUMER:
	// the httpapi.Decide handler extracts incoming W3C traceparent
	// so the markup.decider.decide span becomes a child of whatever
	// emitted the request (decision-gateway in the platform compose).
	// markup-svc does not call any outbound HTTP services from the
	// hot path so the Inject side of the propagator is unused, but
	// setting the propagator unconditionally keeps the global state
	// consistent with the platform's other services (decision-gateway
	// + traffic-gen) and lets future outbound calls inherit it.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	return tp.Tracer(instrumentationName), tp.Shutdown, nil
}
