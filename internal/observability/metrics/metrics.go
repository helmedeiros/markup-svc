// Package metrics ships the markup-side metrics port: a typed
// DecisionMetric event, a single-method Sink port, and a Wrap
// decorator that emits one event per Decide. Backends (Prometheus,
// OTel metrics, custom) implement Sink; markup-svc owns the contract.
// See ADR-0010.
package metrics

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	breengine "github.com/helmedeiros/bre-go/engine"

	"github.com/helmedeiros/markup-svc/internal/markup"
)

// DecisionMetric is the typed event emitted per Decide call. NoMatch,
// Canceled, and Err are mutually exclusive per the ADR-0010 outcome
// table: success populates Adapter/ModelVersion/Rule/MarkupFactor
// from the Decision; ErrNoMatch sets NoMatch=true with empty Rule
// and zero MarkupFactor; context cancellation sets Canceled=true
// with a CancelReason; other errors set Err and leave Canceled false.
type DecisionMetric struct {
	Adapter       string
	ModelVersion  string
	Rule          string
	MarkupFactor  float64
	CorrelationID string
	Duration      time.Duration
	NoMatch       bool
	Err           error
	Canceled      bool
	CancelReason  string
}

// Sink consumes DecisionMetric events. Implementations are the
// adapter half of the hexagonal port: markup-svc owns the contract,
// backends adapt to it. RecordDecision must be safe for concurrent
// calls -- the decorator is invoked from inside Decide, which runs
// concurrently behind the swap.Decider holder.
type Sink interface {
	RecordDecision(DecisionMetric)
}

// Wrap returns inner decorated to emit one DecisionMetric per Decide
// via sink. The returned value satisfies markup.Decider so it
// composes with otel.Wrap and swap.Decider. Recommended order is
// metrics outermost (metrics.Wrap(otel.Wrap(swap.New(inner)))) so
// the metric Duration captures end-to-end Decider cost including
// tracing overhead. See ADR-0010.
func Wrap(inner markup.Decider, sink Sink) markup.Decider {
	return &meteredDecider{inner: inner, sink: sink}
}

type meteredDecider struct {
	inner markup.Decider
	sink  Sink
}

// Decide implements markup.Decider. Builds a DecisionMetric per the
// outcome table and hands it to sink before returning.
func (m *meteredDecider) Decide(ctx context.Context, req markup.Request) (markup.Decision, error) {
	start := time.Now()
	decision, err := m.inner.Decide(ctx, req)
	duration := time.Since(start)

	event := DecisionMetric{
		Duration:      duration,
		CorrelationID: breengine.CorrelationIDFromContext(ctx),
	}
	switch {
	case errors.Is(err, markup.ErrNoMatch):
		event.NoMatch = true
	case errors.Is(err, context.Canceled):
		event.Canceled = true
		event.CancelReason = "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		event.Canceled = true
		event.CancelReason = "deadline_exceeded"
	case err != nil:
		event.Err = err
	default:
		event.Adapter = decision.EngineAdapter
		event.ModelVersion = decision.ModelVersion
		event.Rule = decision.Rule
		event.MarkupFactor = decision.MarkupFactor
	}
	m.sink.RecordDecision(event)
	return decision, err
}

// RecordingSink is a thread-safe Sink that appends every recorded
// metric to an internal slice. Useful for tests and small in-memory
// aggregations; production deployments should use a Prometheus or
// OTel-metrics sink. The slice grows unbounded -- not for production.
type RecordingSink struct {
	mu      sync.Mutex
	records []DecisionMetric
}

// RecordDecision stores m. Safe for concurrent calls.
func (s *RecordingSink) RecordDecision(m DecisionMetric) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = append(s.records, m)
}

// Records returns a defensive copy of every metric recorded so far.
// Safe for concurrent calls.
func (s *RecordingSink) Records() []DecisionMetric {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]DecisionMetric, len(s.records))
	copy(out, s.records)
	return out
}

// Reset clears the recorded metrics. Safe for concurrent calls.
func (s *RecordingSink) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = nil
}

// String implements fmt.Stringer for debug-friendly output of a
// DecisionMetric value -- helpful when test assertion failures need
// to surface the captured event without printing pointers.
func (m DecisionMetric) String() string {
	return fmt.Sprintf(
		"DecisionMetric{Adapter=%q ModelVersion=%q Rule=%q MarkupFactor=%v CorrelationID=%q Duration=%s NoMatch=%v Err=%v Canceled=%v CancelReason=%q}",
		m.Adapter, m.ModelVersion, m.Rule, m.MarkupFactor, m.CorrelationID, m.Duration, m.NoMatch, m.Err, m.Canceled, m.CancelReason,
	)
}
