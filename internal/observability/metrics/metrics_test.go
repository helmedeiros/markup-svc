package metrics_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	breengine "github.com/helmedeiros/bre-go/engine"

	"github.com/helmedeiros/markup-svc/internal/markup"
	"github.com/helmedeiros/markup-svc/internal/observability/metrics"
	mkotel "github.com/helmedeiros/markup-svc/internal/observability/otel"
)

type stubDecider struct {
	decision markup.Decision
	err      error
}

func (s *stubDecider) Decide(ctx context.Context, req markup.Request) (markup.Decision, error) {
	return s.decision, s.err
}

func TestWrapSuccessPopulatesDecisionFields(t *testing.T) {
	sink := &metrics.RecordingSink{}
	inner := &stubDecider{decision: markup.Decision{
		Rule:          "enterprise",
		MarkupFactor:  1.15,
		ModelVersion:  "v1",
		EngineAdapter: "*inmemory.Engine",
	}}
	wrapped := metrics.Wrap(inner, sink)

	_, err := wrapped.Decide(context.Background(), markup.Request{})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}

	recs := sink.Records()
	if len(recs) != 1 {
		t.Fatalf("len(Records) = %d, want 1", len(recs))
	}
	got := recs[0]
	if got.Rule != "enterprise" || got.MarkupFactor != 1.15 {
		t.Errorf("Decision fields lost: %s", got)
	}
	if got.ModelVersion != "v1" {
		t.Errorf("ModelVersion = %q, want \"v1\"", got.ModelVersion)
	}
	if got.Adapter != "*inmemory.Engine" {
		t.Errorf("Adapter = %q, want \"*inmemory.Engine\"", got.Adapter)
	}
	if got.NoMatch || got.Canceled || got.Err != nil {
		t.Errorf("success event must have NoMatch=false Canceled=false Err=nil; got %s", got)
	}
	if got.Duration <= 0 {
		t.Errorf("Duration = %v, want positive", got.Duration)
	}
}

func TestWrapErrNoMatchSetsNoMatchNotErr(t *testing.T) {
	sink := &metrics.RecordingSink{}
	inner := &stubDecider{err: markup.ErrNoMatch}
	wrapped := metrics.Wrap(inner, sink)

	_, err := wrapped.Decide(context.Background(), markup.Request{})
	if !errors.Is(err, markup.ErrNoMatch) {
		t.Fatalf("want ErrNoMatch, got %v", err)
	}

	got := sink.Records()[0]
	if !got.NoMatch {
		t.Errorf("NoMatch = false, want true")
	}
	if got.Err != nil {
		t.Errorf("Err must be nil on ErrNoMatch (the domain outcome)")
	}
	if got.Canceled {
		t.Errorf("Canceled = true on ErrNoMatch")
	}
	if got.Rule != "" || got.MarkupFactor != 0 {
		t.Errorf("ErrNoMatch must leave Rule/MarkupFactor zero; got %s", got)
	}
}

func TestWrapCanceledSetsCanceledNotErr(t *testing.T) {
	sink := &metrics.RecordingSink{}
	inner := &stubDecider{err: context.Canceled}
	wrapped := metrics.Wrap(inner, sink)

	_, _ = wrapped.Decide(context.Background(), markup.Request{})

	got := sink.Records()[0]
	if !got.Canceled || got.CancelReason != "canceled" {
		t.Errorf("Canceled/CancelReason = %v/%q; want true/\"canceled\"", got.Canceled, got.CancelReason)
	}
	if got.Err != nil {
		t.Errorf("Err must be nil on context.Canceled")
	}
	if got.NoMatch {
		t.Errorf("NoMatch must be false on context.Canceled")
	}
}

func TestWrapDeadlineExceededSetsDeadlineReason(t *testing.T) {
	sink := &metrics.RecordingSink{}
	inner := &stubDecider{err: context.DeadlineExceeded}
	wrapped := metrics.Wrap(inner, sink)

	_, _ = wrapped.Decide(context.Background(), markup.Request{})

	got := sink.Records()[0]
	if !got.Canceled || got.CancelReason != "deadline_exceeded" {
		t.Errorf("Canceled/CancelReason = %v/%q; want true/\"deadline_exceeded\"", got.Canceled, got.CancelReason)
	}
}

func TestWrapOtherErrorSetsErrNotCanceled(t *testing.T) {
	sentinel := errors.New("engine boom")
	sink := &metrics.RecordingSink{}
	inner := &stubDecider{err: sentinel}
	wrapped := metrics.Wrap(inner, sink)

	_, err := wrapped.Decide(context.Background(), markup.Request{})
	if !errors.Is(err, sentinel) {
		t.Fatalf("want sentinel, got %v", err)
	}

	got := sink.Records()[0]
	if !errors.Is(got.Err, sentinel) {
		t.Errorf("Err should be the sentinel; got %v", got.Err)
	}
	if got.Canceled || got.NoMatch {
		t.Errorf("real error must leave Canceled/NoMatch false; got %s", got)
	}
}

func TestWrapPopulatesCorrelationIDFromContext(t *testing.T) {
	sink := &metrics.RecordingSink{}
	inner := &stubDecider{decision: markup.Decision{Rule: "r"}}
	wrapped := metrics.Wrap(inner, sink)

	ctx := breengine.WithCorrelationID(context.Background(), "trace-mx-1")
	_, _ = wrapped.Decide(ctx, markup.Request{})

	got := sink.Records()[0]
	if got.CorrelationID != "trace-mx-1" {
		t.Errorf("CorrelationID = %q, want \"trace-mx-1\"", got.CorrelationID)
	}
}

func TestWrapPreservesInnerReturnedValues(t *testing.T) {
	want := markup.Decision{Rule: "r", MarkupFactor: 2.5, ModelVersion: "v9", EngineAdapter: "*x.Engine"}
	inner := &stubDecider{decision: want}
	wrapped := metrics.Wrap(inner, &metrics.RecordingSink{})

	got, err := wrapped.Decide(context.Background(), markup.Request{})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if got != want {
		t.Errorf("Decision lost in wrap: got %+v, want %+v", got, want)
	}
}

// TestRecordingSinkConcurrentRecordingAndRead stresses the sink under
// the race detector: many goroutines RecordDecision while another
// goroutine calls Records() and Reset(). No data race, no panic.
func TestRecordingSinkConcurrentRecordingAndRead(t *testing.T) {
	sink := &metrics.RecordingSink{}
	const writers = 16
	const perWriter = 200

	var wg sync.WaitGroup
	wg.Add(writers + 1)

	for i := 0; i < writers; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < perWriter; j++ {
				sink.RecordDecision(metrics.DecisionMetric{Rule: "r", Duration: time.Nanosecond})
			}
		}()
	}
	go func() {
		defer wg.Done()
		for j := 0; j < 50; j++ {
			_ = sink.Records()
			if j%10 == 0 {
				sink.Reset()
			}
		}
	}()
	wg.Wait()
}

// TestComposesWithOTelDecorator pins ADR-0010's composition promise:
// metrics.Wrap(otel.Wrap(stub)) produces both a recorded metric AND
// a recorded span on every Decide. Confirms the two decorators stack
// without interfering. The metric's Duration captures the time spent
// inside the otel decorator + the stub, which is what the ADR
// recommends ("metrics outermost").
func TestComposesWithOTelDecorator(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	sink := &metrics.RecordingSink{}
	inner := &stubDecider{decision: markup.Decision{Rule: "r", MarkupFactor: 1.0, ModelVersion: "v1", EngineAdapter: "*x.Engine"}}
	traced := mkotel.Wrap(inner, tp.Tracer("compose-test"))
	wrapped := metrics.Wrap(traced, sink)

	_, _ = wrapped.Decide(context.Background(), markup.Request{})

	if len(sink.Records()) != 1 {
		t.Fatalf("len(metric records) = %d, want 1", len(sink.Records()))
	}
	if len(rec.Ended()) != 1 {
		t.Fatalf("len(spans) = %d, want 1", len(rec.Ended()))
	}
}

// TestDecisionMetricFieldSetInvariants pins the mutual-exclusivity
// rules from the ADR-0010 outcome table. For every outcome the
// decorator can produce, NoMatch / Canceled / Err must be mutually
// exclusive and the decorator must never produce a metric where two
// or more are set.
func TestDecisionMetricFieldSetInvariants(t *testing.T) {
	cases := []struct {
		name        string
		decision    markup.Decision
		err         error
		wantNoMatch bool
		wantCanceled bool
		wantErrSet  bool
	}{
		{"success", markup.Decision{Rule: "r", MarkupFactor: 1, ModelVersion: "v", EngineAdapter: "*x.Engine"}, nil, false, false, false},
		{"no-match", markup.Decision{}, markup.ErrNoMatch, true, false, false},
		{"canceled", markup.Decision{}, context.Canceled, false, true, false},
		{"deadline_exceeded", markup.Decision{}, context.DeadlineExceeded, false, true, false},
		{"other_error", markup.Decision{}, errors.New("boom"), false, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sink := &metrics.RecordingSink{}
			wrapped := metrics.Wrap(&stubDecider{decision: tc.decision, err: tc.err}, sink)
			_, _ = wrapped.Decide(context.Background(), markup.Request{})

			got := sink.Records()[0]
			set := 0
			if got.NoMatch {
				set++
			}
			if got.Canceled {
				set++
			}
			if got.Err != nil {
				set++
			}
			if tc.name == "success" {
				if set != 0 {
					t.Errorf("success must have 0 of {NoMatch, Canceled, Err}; got %d; %s", set, got)
				}
			} else if set != 1 {
				t.Errorf("%s must have exactly 1 of {NoMatch, Canceled, Err}; got %d; %s", tc.name, set, got)
			}
			if got.NoMatch != tc.wantNoMatch {
				t.Errorf("NoMatch = %v, want %v", got.NoMatch, tc.wantNoMatch)
			}
			if got.Canceled != tc.wantCanceled {
				t.Errorf("Canceled = %v, want %v", got.Canceled, tc.wantCanceled)
			}
			if (got.Err != nil) != tc.wantErrSet {
				t.Errorf("Err presence = %v, want %v", got.Err != nil, tc.wantErrSet)
			}
		})
	}
}

func TestRecordingSinkResetClearsRecords(t *testing.T) {
	sink := &metrics.RecordingSink{}
	for i := 0; i < 5; i++ {
		sink.RecordDecision(metrics.DecisionMetric{Rule: "r"})
	}
	if got := len(sink.Records()); got != 5 {
		t.Fatalf("len = %d, want 5", got)
	}
	sink.Reset()
	if got := len(sink.Records()); got != 0 {
		t.Errorf("after Reset len = %d, want 0", got)
	}
}

func TestDecisionMetricStringContainsKeyFields(t *testing.T) {
	m := metrics.DecisionMetric{
		Adapter:      "*inmemory.Engine",
		ModelVersion: "v1",
		Rule:         "enterprise",
		MarkupFactor: 1.15,
	}
	s := m.String()
	// not exhaustive — just confirm a Stringer exists and surfaces fields
	want := []string{"*inmemory.Engine", "v1", "enterprise", "1.15"}
	for _, w := range want {
		if !contains(s, w) {
			t.Errorf("String() = %q; missing %q", s, w)
		}
	}
}

func contains(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
