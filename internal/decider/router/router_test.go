package router_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	breengine "github.com/helmedeiros/bre-go/engine"

	"github.com/helmedeiros/markup-svc/internal/decider/router"
	"github.com/helmedeiros/markup-svc/internal/markup"
	"github.com/helmedeiros/markup-svc/internal/observability/metrics"
	mkotel "github.com/helmedeiros/markup-svc/internal/observability/otel"
)

type stubDecider struct {
	rule         string
	factor       float64
	modelVersion string // what the stub writes (which the router should override)
	experiment   string // what the stub writes (which the router should override)
	adapter      string
	err          error
}

func (s *stubDecider) Decide(ctx context.Context, req markup.Request) (markup.Decision, error) {
	if s.err != nil {
		return markup.Decision{}, s.err
	}
	return markup.Decision{
		Rule:          s.rule,
		MarkupFactor:  s.factor,
		ModelVersion:  s.modelVersion,
		Experiment:    s.experiment,
		EngineAdapter: s.adapter,
	}, nil
}

func makeRoutes(t *testing.T) []router.Route {
	t.Helper()
	return []router.Route{
		{ModelVersion: "v1", Variant: "control", Decider: &stubDecider{rule: "r-control", factor: 1.0, modelVersion: "wrong", experiment: "wrong", adapter: "*x.Engine"}},
		{ModelVersion: "v2", Variant: "treatment", Decider: &stubDecider{rule: "r-treatment", factor: 1.42, modelVersion: "wrong", experiment: "wrong", adapter: "*x.Engine"}},
	}
}

// TestRouterStampsModelVersionAndVariant is the load-bearing test for
// the source-of-truth contract: whatever the inner Decider writes to
// ModelVersion / Experiment, the Router overrides with the chosen
// Route's labels. Inner Deciders cannot accidentally erase the routing
// decision.
func TestRouterStampsModelVersionAndVariant(t *testing.T) {
	routes := makeRoutes(t)
	r := router.New(routes, router.DefaultPolicy{})

	got, err := r.Decide(context.Background(), markup.Request{})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if got.ModelVersion != "v1" {
		t.Errorf("ModelVersion = %q, want \"v1\" (router stamp, not stub's \"wrong\")", got.ModelVersion)
	}
	if got.Experiment != "control" {
		t.Errorf("Experiment = %q, want \"control\" (router stamp, not stub's \"wrong\")", got.Experiment)
	}
	// Inner rule + factor should propagate through.
	if got.Rule != "r-control" || got.MarkupFactor != 1.0 {
		t.Errorf("inner Decision lost: %+v", got)
	}
}

func TestRouterPropagatesInnerErrNoMatch(t *testing.T) {
	routes := []router.Route{
		{ModelVersion: "v1", Variant: "", Decider: &stubDecider{err: markup.ErrNoMatch}},
	}
	r := router.New(routes, router.DefaultPolicy{})

	_, err := r.Decide(context.Background(), markup.Request{})
	if !errors.Is(err, markup.ErrNoMatch) {
		t.Fatalf("want ErrNoMatch propagated, got %v", err)
	}
	if errors.Is(err, router.ErrNoRoute) {
		t.Errorf("ErrNoMatch must NOT be misclassified as ErrNoRoute")
	}
}

func TestRouterErrNoRouteOnEmptyRoutes(t *testing.T) {
	r := router.New(nil, router.DefaultPolicy{})
	_, err := r.Decide(context.Background(), markup.Request{})
	if !errors.Is(err, router.ErrNoRoute) {
		t.Fatalf("want ErrNoRoute, got %v", err)
	}
}

type failingPolicy struct{ msg string }

func (p failingPolicy) Choose(context.Context, markup.Request, []router.Route) (router.Route, error) {
	return router.Route{}, errors.New(p.msg)
}

func TestRouterErrNoRouteWrapsPolicyError(t *testing.T) {
	routes := makeRoutes(t)
	r := router.New(routes, failingPolicy{msg: "policy rejected"})

	_, err := r.Decide(context.Background(), markup.Request{})
	if !errors.Is(err, router.ErrNoRoute) {
		t.Fatalf("want ErrNoRoute, got %v", err)
	}
	if !strings.Contains(err.Error(), "policy rejected") {
		t.Errorf("error %q should carry policy's reason", err.Error())
	}
}

func TestNewIsDefensiveAgainstSliceMutation(t *testing.T) {
	routes := makeRoutes(t)
	r := router.New(routes, router.DefaultPolicy{})

	// Mutate the caller's slice after construction.
	routes[0].ModelVersion = "mutated"

	got, err := r.Decide(context.Background(), markup.Request{})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if got.ModelVersion != "v1" {
		t.Errorf("caller mutation leaked into Router: ModelVersion = %q", got.ModelVersion)
	}
}

// TestHashCorrelationPolicyStickyAcrossCalls confirms the sticky
// promise: same correlation ID -> same Route across many calls.
// Without this property A/B dashboards would compare a moving target
// against itself.
func TestHashCorrelationPolicyStickyAcrossCalls(t *testing.T) {
	routes := makeRoutes(t)
	r := router.New(routes, router.HashCorrelationPolicy{})

	ctx := breengine.WithCorrelationID(context.Background(), "trace-sticky-1")

	first, err := r.Decide(ctx, markup.Request{})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	for i := 0; i < 100; i++ {
		got, err := r.Decide(ctx, markup.Request{})
		if err != nil {
			t.Fatalf("[%d] Decide: %v", i, err)
		}
		if got.ModelVersion != first.ModelVersion || got.Experiment != first.Experiment {
			t.Fatalf("[%d] stickiness broken: first=%s/%s, got=%s/%s",
				i, first.ModelVersion, first.Experiment, got.ModelVersion, got.Experiment)
		}
	}
}

// TestHashCorrelationPolicyDistributionRoughlyEven confirms two
// variants split traffic in roughly equal halves over 10k distinct
// correlation IDs. The exact split is hash-dependent but the tail
// gap shouldn't exceed 10% of the population at this sample size.
func TestHashCorrelationPolicyDistributionRoughlyEven(t *testing.T) {
	routes := makeRoutes(t)
	r := router.New(routes, router.HashCorrelationPolicy{})

	counts := map[string]int{}
	const n = 10_000
	for i := 0; i < n; i++ {
		ctx := breengine.WithCorrelationID(context.Background(), fmt.Sprintf("id-%d", i))
		got, _ := r.Decide(ctx, markup.Request{})
		counts[got.Experiment]++
	}
	if len(counts) != 2 {
		t.Fatalf("expected 2 distinct variants, got %d: %+v", len(counts), counts)
	}
	for v, c := range counts {
		if c < n*4/10 || c > n*6/10 {
			t.Errorf("variant %q saw %d / %d (outside 40-60%% band)", v, c, n)
		}
	}
}

func TestHashCorrelationPolicyFallsBackToFirstWhenNoCorrelationID(t *testing.T) {
	routes := makeRoutes(t)
	r := router.New(routes, router.HashCorrelationPolicy{})

	got, err := r.Decide(context.Background(), markup.Request{})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if got.ModelVersion != "v1" || got.Experiment != "control" {
		t.Errorf("no-correlation-ID fallback wrong: got %s/%s, want v1/control", got.ModelVersion, got.Experiment)
	}
}

func TestHashFieldPolicyStickyByRequestField(t *testing.T) {
	routes := makeRoutes(t)
	policy := router.HashFieldPolicy{Field: func(req markup.Request) string { return req.ProductID }}
	r := router.New(routes, policy)

	first, err := r.Decide(context.Background(), markup.Request{ProductID: "pid-42"})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	for i := 0; i < 50; i++ {
		got, _ := r.Decide(context.Background(), markup.Request{ProductID: "pid-42"})
		if got.ModelVersion != first.ModelVersion {
			t.Fatalf("[%d] stickiness broken: first=%s, got=%s", i, first.ModelVersion, got.ModelVersion)
		}
	}
}

func TestHashFieldPolicyErrorsWhenFieldNil(t *testing.T) {
	routes := makeRoutes(t)
	r := router.New(routes, router.HashFieldPolicy{Field: nil})
	_, err := r.Decide(context.Background(), markup.Request{})
	if !errors.Is(err, router.ErrNoRoute) {
		t.Fatalf("want ErrNoRoute, got %v", err)
	}
}

func TestHashFieldPolicyFallsBackToFirstWhenFieldEmpty(t *testing.T) {
	routes := makeRoutes(t)
	policy := router.HashFieldPolicy{Field: func(req markup.Request) string { return req.ProductID }}
	r := router.New(routes, policy)

	got, err := r.Decide(context.Background(), markup.Request{ProductID: ""})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if got.ModelVersion != "v1" {
		t.Errorf("empty-field fallback wrong: ModelVersion = %s, want v1", got.ModelVersion)
	}
}

func TestDefaultPolicyAlwaysReturnsFirstRoute(t *testing.T) {
	routes := makeRoutes(t)
	r := router.New(routes, router.DefaultPolicy{})

	for i := 0; i < 50; i++ {
		ctx := breengine.WithCorrelationID(context.Background(), fmt.Sprintf("id-%d", i))
		got, _ := r.Decide(ctx, markup.Request{})
		if got.ModelVersion != "v1" {
			t.Fatalf("[%d] DefaultPolicy must always pick first route; got %s", i, got.ModelVersion)
		}
	}
}

// TestRouterStacksWithMetricsAndOtel confirms ADR-0011's composition
// promise: a Router wrapped by otel + metrics produces both signals
// with the route's stamped (ModelVersion, Variant) labels. Operators'
// dashboards see the routing decision in both the span and the metric
// without a separate accounting.
func TestRouterStacksWithMetricsAndOtel(t *testing.T) {
	routes := makeRoutes(t)
	r := router.New(routes, router.DefaultPolicy{})

	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	sink := &metrics.RecordingSink{}
	stack := metrics.Wrap(mkotel.Wrap(r, tp.Tracer("router-compose")), sink)

	_, err := stack.Decide(context.Background(), markup.Request{})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}

	if len(rec.Ended()) != 1 {
		t.Fatalf("len(spans) = %d, want 1", len(rec.Ended()))
	}
	if len(sink.Records()) != 1 {
		t.Fatalf("len(metric records) = %d, want 1", len(sink.Records()))
	}
	got := sink.Records()[0]
	if got.ModelVersion != "v1" || got.Rule != "r-control" {
		t.Errorf("metric event lost the router stamp / inner data: %s", got)
	}
}
