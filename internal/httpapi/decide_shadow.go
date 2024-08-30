package httpapi

import (
	"context"
	"errors"
	"math"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/helmedeiros/markup-svc/internal/markup"
)

// DefaultShadowTimeout caps the wall-clock budget the challenger gets
// per /decide. Set well above the champion's measured p99 + room for
// goroutine scheduling jitter; a challenger that misses the budget is
// counted as a timeout and the comparison is dropped.
const DefaultShadowTimeout = 10 * time.Millisecond

// ShadowMetrics is the port the /decide shadow path emits against.
// noopShadowMetrics is the default so a handler without --shadow-admin
// wiring pays nothing.
type ShadowMetrics interface {
	RecordAgreement(agree bool)
	RecordOneSided(side string)
	RecordTimeout()
	RecordError()
	RecordFactorDelta(delta float64)
}

// NoopShadowMetrics is the safe default when no metrics sink is
// wired. All methods are no-ops.
type NoopShadowMetrics struct{}

func (NoopShadowMetrics) RecordAgreement(bool)      {}
func (NoopShadowMetrics) RecordOneSided(string)     {}
func (NoopShadowMetrics) RecordTimeout()            {}
func (NoopShadowMetrics) RecordError()              {}
func (NoopShadowMetrics) RecordFactorDelta(float64) {}

// DecideOption configures the /decide handler at construction time.
// Backwards-compatible: zero options yields the pre-ADR-0032 handler.
type DecideOption func(*decideConfig)

type decideConfig struct {
	shadow ChallengerHolder
	metrics ShadowMetrics
	timeout time.Duration
	tracer  trace.Tracer
}

// WithShadow wires the challenger Holder, metrics sink, timeout, and
// tracer into /decide. When holder.Get returns loaded=false the
// handler short-circuits without spawning a goroutine or allocating
// metrics state.
func WithShadow(holder ChallengerHolder, metrics ShadowMetrics, timeout time.Duration, tracer trace.Tracer) DecideOption {
	return func(c *decideConfig) {
		c.shadow = holder
		c.metrics = metrics
		c.timeout = timeout
		c.tracer = tracer
	}
}

// evaluateChallenger compares one champion result against the
// challenger's verdict on the same request. Runs in its own
// goroutine; never blocks the response. ctx carries the parent
// span context (so the challenger span links into the trace) but
// no cancellation — the goroutine respects only its own deadline.
// The challenger Decider is passed in directly (captured at dispatch
// time) rather than re-fetched from the holder, so a racing Clear()
// does not abort an in-flight comparison.
func evaluateChallenger(ctx context.Context, challenger markup.Decider, req markup.Request, champion markup.Decision, championErr error, m ShadowMetrics, timeout time.Duration, tracer trace.Tracer) {
	shadowCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if tracer != nil {
		var span trace.Span
		shadowCtx, span = tracer.Start(shadowCtx, "markup.challenger.evaluate")
		defer span.End()
	}

	chDecision, chErr := challenger.Decide(shadowCtx, req)

	if errors.Is(shadowCtx.Err(), context.DeadlineExceeded) || errors.Is(chErr, context.DeadlineExceeded) {
		m.RecordTimeout()
		return
	}

	if chErr != nil && !errors.Is(chErr, markup.ErrNoMatch) {
		m.RecordError()
		return
	}

	championFired := championErr == nil
	challengerFired := chErr == nil

	switch {
	case championFired && !challengerFired:
		m.RecordOneSided("champion_only")
	case !championFired && challengerFired:
		m.RecordOneSided("challenger_only")
	case !championFired && !challengerFired:
		m.RecordAgreement(true)
	default:
		delta := math.Abs(champion.MarkupFactor - chDecision.MarkupFactor)
		agree := delta < factorEpsilon
		m.RecordAgreement(agree)
		if !agree {
			m.RecordFactorDelta(delta)
		}
	}
}

// factorEpsilon is the float-equality tolerance for the markup
// factor comparison. Rule sets carry factors at three decimal
// places; 1e-9 is far below any rounding the upstream pipeline can
// produce, so equal-up-to-this is the agreement criterion.
const factorEpsilon = 1e-9
