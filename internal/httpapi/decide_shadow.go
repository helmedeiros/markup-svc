package httpapi

import (
	"context"
	"errors"
	"math"
	"math/rand"
	"time"

	breengine "github.com/helmedeiros/bre-go/engine"
	"go.opentelemetry.io/otel/trace"

	"github.com/helmedeiros/markup-svc/internal/jsonlog"
	"github.com/helmedeiros/markup-svc/internal/markup"
)

// defaultSampler is the production random source. Uses math/rand
// (cheap, not crypto). Sampling decisions are not security-sensitive;
// a request whose hash is predictable cannot exploit the choice.
var defaultSampler = rand.Float64

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
	// RecordSampled fires on every /decide where a challenger is loaded
	// regardless of whether the sample check selected this request.
	// Operators read agree-rate-over-sampled to reason about effective
	// comparison rate when --shadow-sample-rate < 1.0.
	RecordSampled(sampled bool)
	// RecordChallengerDuration records the wall-clock cost of one
	// challenger Decide call. Enables side-by-side comparison with the
	// champion's markup_decide_duration_seconds under real traffic
	// (ADR-0033 performance-measurement use case).
	RecordChallengerDuration(d time.Duration)
}

// NoopShadowMetrics is the safe default when no metrics sink is
// wired. All methods are no-ops.
type NoopShadowMetrics struct{}

func (NoopShadowMetrics) RecordAgreement(bool)      {}
func (NoopShadowMetrics) RecordOneSided(string)     {}
func (NoopShadowMetrics) RecordTimeout()            {}
func (NoopShadowMetrics) RecordError()              {}
func (NoopShadowMetrics) RecordFactorDelta(float64) {}
func (NoopShadowMetrics) RecordSampled(bool)               {}
func (NoopShadowMetrics) RecordChallengerDuration(time.Duration) {}

// DecideOption configures the /decide handler at construction time.
// Backwards-compatible: zero options yields the pre-ADR-0032 handler.
type DecideOption func(*decideConfig)

type decideConfig struct {
	shadow     ChallengerHolder
	metrics    ShadowMetrics
	timeout    time.Duration
	tracer     trace.Tracer
	sampleRate float64
	sampler    func() float64
	shadowLog  *jsonlog.Logger
}

// WithShadow wires the challenger Holder, metrics sink, timeout, and
// tracer into /decide. When holder.Get returns loaded=false the
// handler short-circuits without spawning a goroutine or allocating
// metrics state. sampleRate selects what fraction of /decide calls
// run the shadow comparison; 1.0 (or 0 — interpreted as default) runs
// every request, 0.1 runs 10%, 0.0 disables sampling but keeps the
// admin surface live (useful for "I want the challenger loaded but
// not yet running").
func WithShadow(holder ChallengerHolder, metrics ShadowMetrics, timeout time.Duration, tracer trace.Tracer, sampleRate float64) DecideOption {
	return func(c *decideConfig) {
		c.shadow = holder
		c.metrics = metrics
		c.timeout = timeout
		c.tracer = tracer
		c.sampleRate = sampleRate
	}
}

// WithShadowSampler overrides the default random sampler. Tests pass
// a deterministic function; production uses the default math/rand
// based one.
func WithShadowSampler(sample func() float64) DecideOption {
	return func(c *decideConfig) { c.sampler = sample }
}

// WithShadowLogger threads a logger into evaluateChallenger so
// interesting outcomes (timeout, error, disagreement, one-sided)
// emit a structured log event. `agree` outcomes do NOT log — the
// counter suffices and the volume would be prohibitive. Operators
// pivot through the saved Kibana searches in pricing-observability.
func WithShadowLogger(l *jsonlog.Logger) DecideOption {
	return func(c *decideConfig) { c.shadowLog = l }
}

// evaluateChallenger compares one champion result against the
// challenger's verdict on the same request. Runs in its own
// goroutine; never blocks the response. ctx carries the parent
// span context (so the challenger span links into the trace) but
// no cancellation — the goroutine respects only its own deadline.
// The challenger Decider is passed in directly (captured at dispatch
// time) rather than re-fetched from the holder, so a racing Clear()
// does not abort an in-flight comparison.
func evaluateChallenger(ctx context.Context, challenger markup.Decider, req markup.Request, champion markup.Decision, championErr error, m ShadowMetrics, timeout time.Duration, tracer trace.Tracer, log *jsonlog.Logger) {
	shadowCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if tracer != nil {
		var span trace.Span
		shadowCtx, span = tracer.Start(shadowCtx, "markup.challenger.evaluate")
		defer span.End()
	}

	start := time.Now()
	chDecision, chErr := challenger.Decide(shadowCtx, req)
	dur := time.Since(start)
	m.RecordChallengerDuration(dur)

	if errors.Is(shadowCtx.Err(), context.DeadlineExceeded) || errors.Is(chErr, context.DeadlineExceeded) {
		m.RecordTimeout()
		emitShadowLog(log, ctx, "timeout", champion, chDecision, dur, nil, "")
		return
	}

	if chErr != nil && !errors.Is(chErr, markup.ErrNoMatch) {
		m.RecordError()
		emitShadowLog(log, ctx, "error", champion, chDecision, dur, chErr, "")
		return
	}

	championFired := championErr == nil
	challengerFired := chErr == nil

	switch {
	case championFired && !challengerFired:
		m.RecordOneSided("champion_only")
		emitShadowLog(log, ctx, "one_sided", champion, chDecision, dur, nil, "champion_only")
	case !championFired && challengerFired:
		m.RecordOneSided("challenger_only")
		emitShadowLog(log, ctx, "one_sided", champion, chDecision, dur, nil, "challenger_only")
	case !championFired && !challengerFired:
		m.RecordAgreement(true)
		// agree outcomes do not log — counter suffices and volume would be prohibitive.
	default:
		delta := math.Abs(champion.MarkupFactor - chDecision.MarkupFactor)
		agree := delta < factorEpsilon
		m.RecordAgreement(agree)
		if !agree {
			m.RecordFactorDelta(delta)
			emitShadowLog(log, ctx, "disagree", champion, chDecision, dur, nil, "")
		}
	}
}

// emitShadowLog writes one structured event per interesting outcome.
// Skipped for `agree` to keep log volume bounded; at sample=1.0 and
// 2000 QPS, a 99% agreement rate would produce ~20 events/sec instead
// of 2000.
func emitShadowLog(log *jsonlog.Logger, ctx context.Context, outcome string, champion markup.Decision, challenger markup.Decision, dur time.Duration, chErr error, side string) {
	if log == nil {
		return
	}
	attrs := map[string]any{
		"outcome":               outcome,
		"shadow_duration_ms":    float64(dur) / float64(time.Millisecond),
	}
	if cid := breengine.CorrelationIDFromContext(ctx); cid != "" {
		attrs["correlation_id"] = cid
	}
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		attrs["trace_id"] = sc.TraceID().String()
		attrs["span_id"] = sc.SpanID().String()
	}
	switch outcome {
	case "disagree":
		attrs["champion_factor"] = champion.MarkupFactor
		attrs["challenger_factor"] = challenger.MarkupFactor
		attrs["delta"] = math.Abs(champion.MarkupFactor - challenger.MarkupFactor)
		attrs["champion_rule"] = champion.Rule
		attrs["challenger_rule"] = challenger.Rule
	case "one_sided":
		attrs["side"] = side
		if side == "champion_only" {
			attrs["champion_factor"] = champion.MarkupFactor
			attrs["champion_rule"] = champion.Rule
		} else {
			attrs["challenger_factor"] = challenger.MarkupFactor
			attrs["challenger_rule"] = challenger.Rule
		}
	case "error":
		if chErr != nil {
			attrs["error"] = chErr.Error()
		}
	}
	log.Info("markup.challenger.evaluate", attrs)
}

// factorEpsilon is the float-equality tolerance for the markup
// factor comparison. Rule sets carry factors at three decimal
// places; 1e-9 is far below any rounding the upstream pipeline can
// produce, so equal-up-to-this is the agreement criterion.
const factorEpsilon = 1e-9

// sampleAllows answers "should this /decide call run the shadow
// comparison?". Effective rate of 1.0 (or unset) runs every call;
// 0.0 disables the comparison without unmounting the admin surface
// (operator wants the challenger loaded but not yet evaluated, e.g.
// dark-launch of a rule set whose Diagnose pass they want to keep
// passing on every reload).
func (c decideConfig) sampleAllows() bool {
	switch {
	case c.sampleRate <= 0:
		return false
	case c.sampleRate >= 1:
		return true
	}
	if c.sampler != nil {
		return c.sampler() < c.sampleRate
	}
	return defaultSampler() < c.sampleRate
}
