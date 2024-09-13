// Package prom adapts the metrics.Sink port (ADR-0010) to the
// Prometheus exposition format. Registers a counter for per-Decide
// outcome counts + a histogram for per-Decide latency on a private
// prometheus.Registry; an HTTP handler exposes them at /metrics for
// Prometheus to scrape. See ADR-0019.
package prom

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/helmedeiros/markup-svc/internal/observability/metrics"
)

// Sink implements metrics.Sink. The constructor returns the Sink
// AND a registered http.Handler bound to a private prometheus.Registry
// so multiple Sinks in the same process do not cross-contaminate
// (matters for tests; production runs one per binary).
type Sink struct {
	count *prometheus.CounterVec
	dur   *prometheus.HistogramVec
}

// ShadowSink implements httpapi.ShadowMetrics. Shares the same
// private registry as Sink so /metrics exposes Decide + shadow
// counters from one scrape. The env label is prepended to every
// emission so a multi-env scrape job can be sliced per environment
// at query time (ADR-0034).
type ShadowSink struct {
	env         string
	agreement   *prometheus.CounterVec
	oneSided    *prometheus.CounterVec
	timeouts    *prometheus.CounterVec
	errs        *prometheus.CounterVec
	factorDelta *prometheus.HistogramVec
	sampled     *prometheus.CounterVec
	duration    *prometheus.HistogramVec
}

// RecordAgreement implements httpapi.ShadowMetrics.
func (s *ShadowSink) RecordAgreement(agree bool) {
	if agree {
		s.agreement.WithLabelValues(s.env, "true").Inc()
	} else {
		s.agreement.WithLabelValues(s.env, "false").Inc()
	}
}

// RecordOneSided implements httpapi.ShadowMetrics.
func (s *ShadowSink) RecordOneSided(side string) {
	s.oneSided.WithLabelValues(s.env, side).Inc()
}

// RecordTimeout implements httpapi.ShadowMetrics.
func (s *ShadowSink) RecordTimeout() { s.timeouts.WithLabelValues(s.env).Inc() }

// RecordError implements httpapi.ShadowMetrics.
func (s *ShadowSink) RecordError() { s.errs.WithLabelValues(s.env).Inc() }

// RecordFactorDelta implements httpapi.ShadowMetrics.
func (s *ShadowSink) RecordFactorDelta(delta float64) {
	s.factorDelta.WithLabelValues(s.env).Observe(delta)
}

// RecordSampled implements httpapi.ShadowMetrics.
func (s *ShadowSink) RecordSampled(sampled bool) {
	if sampled {
		s.sampled.WithLabelValues(s.env, "true").Inc()
	} else {
		s.sampled.WithLabelValues(s.env, "false").Inc()
	}
}

// RecordChallengerDuration implements httpapi.ShadowMetrics.
func (s *ShadowSink) RecordChallengerDuration(d time.Duration) {
	s.duration.WithLabelValues(s.env).Observe(d.Seconds())
}

// DecisionSinkMetrics implements decisionsink.Metrics. Exposes the two
// counters ADR-0036 names as the canonical operational signals for
// the markup.decision.v1 substrate path: drops by reason, and flush
// success counts. The bytes histogram captures payload sizes so
// operators can correlate spikes against bucket-cost growth.
type DecisionSinkMetrics struct {
	env     string
	dropped *prometheus.CounterVec
	flushed *prometheus.CounterVec
	bytes   *prometheus.CounterVec
}

// IncDropped implements decisionsink.Metrics.
func (m *DecisionSinkMetrics) IncDropped(reason string, n int) {
	m.dropped.WithLabelValues(m.env, reason).Add(float64(n))
}

// IncFlushed implements decisionsink.Metrics.
func (m *DecisionSinkMetrics) IncFlushed(events int, byteCount int) {
	m.flushed.WithLabelValues(m.env).Add(float64(events))
	m.bytes.WithLabelValues(m.env).Add(float64(byteCount))
}

// shadowFactorDeltaBuckets cover the realistic markup-factor delta
// range: rules carry factors at ~3 decimal places and live in
// [0.5, 5.0]; deltas span 1e-3 to ~1 with the long tail at 0.1-0.5.
var shadowFactorDeltaBuckets = []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1.0}

// decideBuckets covers the measured hot-path range (engine work
// 10-100us, full Decider stack 17us-1ms median, tail to ~10ms).
// prometheus.DefBuckets starts at 5ms and reads flat. See ADR-0024.
var decideBuckets = []float64{
	0.00005, 0.0001, 0.00025, 0.0005,
	0.001, 0.0025, 0.005, 0.01,
	0.025, 0.05, 0.1, 0.25, 0.5, 1,
}

// New constructs a Sink + the matching /metrics HTTP handler.
// The Sink is registered against a private Registry; the handler
// is built via promhttp.HandlerFor against the same Registry so
// the exposition includes only the markup-svc Decide metrics, no
// process collector noise. Operators wanting Go runtime metrics
// stack a second registry; this Sink stays focused on the Decide
// signal.
//
// Labels on the counter + histogram:
//
//   - adapter       — Decision.EngineAdapter; "" for ErrNoMatch /
//     Canceled / Err outcomes per the ADR-0010 outcome table.
//   - model_version — Decision.ModelVersion; "" for the same.
//   - outcome       — one of: ok, no_match, canceled, deadline, error.
//     Synthesized from the DecisionMetric's mutually-exclusive
//     NoMatch/Canceled/Err fields.
//
// Rule is NOT a label: with N rules per model version, the label
// cardinality grows linearly + can run into Prometheus's recommended
// ceiling at ~1000 series. Operators wanting per-rule QPS use the
// span attribute filter in Jaeger (rule.markup.rule).
//
// Histogram buckets: prometheus.DefBuckets is the stdlib default
// (5ms-10s). Per-Decide work runs at 20-100us on the inmemory engine,
// so the default buckets all land in the first ten-bucket sweep --
// the histogram is mostly informative at the lower end. A
// custom-buckets ADR can land if production tail-latency
// investigation needs different breakpoints.
func New(env string) (*Sink, *ShadowSink, *DecisionSinkMetrics, http.Handler) {
	if env == "" {
		env = "default"
	}
	reg := prometheus.NewRegistry()
	count := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "markup_decide_total",
			Help: "Total markup-svc Decide calls labeled by adapter / model_version / outcome (ADR-0019).",
		},
		[]string{"adapter", "model_version", "outcome"},
	)
	dur := prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "markup_decide_duration_seconds",
			Help:    "Markup-svc Decide latency in seconds labeled by adapter / model_version / outcome (ADR-0019 + ADR-0024).",
			Buckets: decideBuckets,
		},
		[]string{"adapter", "model_version", "outcome"},
	)
	shadowAgree := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "markup_challenger_agreement_total",
			Help: "Shadow Decider comparison outcomes (ADR-0032). agree=true|false; both-decline counts as agree=true. env per ADR-0034.",
		},
		[]string{"env", "agree"},
	)
	shadowOneSided := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "markup_challenger_one_sided_total",
			Help: "Shadow comparison where exactly one of champion/challenger fired (ADR-0032). side=champion_only|challenger_only. env per ADR-0034.",
		},
		[]string{"env", "side"},
	)
	shadowTimeouts := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "markup_challenger_eval_timeout_total",
			Help: "Shadow Decider missed its evaluation deadline (ADR-0032). env per ADR-0034.",
		},
		[]string{"env"},
	)
	shadowErrs := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "markup_challenger_eval_errors_total",
			Help: "Shadow Decider returned a non-ErrNoMatch error (ADR-0032). env per ADR-0034.",
		},
		[]string{"env"},
	)
	shadowDelta := prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "markup_challenger_factor_delta",
			Help:    "abs(champion_factor - challenger_factor) recorded only on disagreement (ADR-0032). env per ADR-0034.",
			Buckets: shadowFactorDeltaBuckets,
		},
		[]string{"env"},
	)
	shadowSampled := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "markup_challenger_sampled_total",
			Help: "Per /decide call where a challenger is loaded, whether the sample check selected the request (sampled=true|false). Effective comparison rate = true / (true + false) (ADR-0033). env per ADR-0034.",
		},
		[]string{"env", "sampled"},
	)
	shadowDuration := prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "markup_challenger_decide_duration_seconds",
			Help:    "Wall-clock cost of one challenger Decide call (ADR-0033). Buckets match markup_decide_duration_seconds so champion / challenger latencies are directly comparable. env per ADR-0034.",
			Buckets: decideBuckets,
		},
		[]string{"env"},
	)
	sinkDropped := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "markup_decision_sink_dropped_total",
			Help: "markup.decision.v1 events dropped by the decision-sink adapter (ADR-0036). reason=buffer_full|serialize_failed|flush_failed. env per ADR-0034.",
		},
		[]string{"env", "reason"},
	)
	sinkFlushed := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "markup_decision_sink_flushed_total",
			Help: "markup.decision.v1 events successfully delivered by the decision-sink adapter to the substrate (ADR-0036). env per ADR-0034.",
		},
		[]string{"env"},
	)
	sinkBytes := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "markup_decision_sink_flushed_bytes_total",
			Help: "Post-compression bytes successfully delivered by the decision-sink adapter (ADR-0036). env per ADR-0034.",
		},
		[]string{"env"},
	)
	reg.MustRegister(count, dur, shadowAgree, shadowOneSided, shadowTimeouts, shadowErrs, shadowDelta, shadowSampled, shadowDuration, sinkDropped, sinkFlushed, sinkBytes)
	handler := promhttp.HandlerFor(reg, promhttp.HandlerOpts{})
	return &Sink{count: count, dur: dur},
		&ShadowSink{
			env:       env,
			agreement: shadowAgree, oneSided: shadowOneSided,
			timeouts: shadowTimeouts, errs: shadowErrs,
			factorDelta: shadowDelta, sampled: shadowSampled,
			duration: shadowDuration,
		},
		&DecisionSinkMetrics{env: env, dropped: sinkDropped, flushed: sinkFlushed, bytes: sinkBytes},
		handler
}

// RecordDecision implements metrics.Sink.
func (s *Sink) RecordDecision(m metrics.DecisionMetric) {
	outcome := outcomeFor(m)
	labels := prometheus.Labels{
		"adapter":       m.Adapter,
		"model_version": m.ModelVersion,
		"outcome":       outcome,
	}
	s.count.With(labels).Inc()
	s.dur.With(labels).Observe(m.Duration.Seconds())
}

// outcomeFor maps the DecisionMetric's mutually-exclusive outcome
// fields to a short, low-cardinality outcome label suitable for
// Prometheus filtering.
func outcomeFor(m metrics.DecisionMetric) string {
	switch {
	case m.NoMatch:
		return "no_match"
	case m.Canceled:
		if m.CancelReason == "deadline_exceeded" {
			return "deadline"
		}
		return "canceled"
	case m.Err != nil:
		return "error"
	default:
		return "ok"
	}
}
