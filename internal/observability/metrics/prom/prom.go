// Package prom adapts the metrics.Sink port (ADR-0010) to the
// Prometheus exposition format. Registers a counter for per-Decide
// outcome counts + a histogram for per-Decide latency on a private
// prometheus.Registry; an HTTP handler exposes them at /metrics for
// Prometheus to scrape. See ADR-0019.
package prom

import (
	"net/http"

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
func New() (*Sink, http.Handler) {
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
			Help:    "Markup-svc Decide latency in seconds labeled by adapter / model_version / outcome (ADR-0019).",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"adapter", "model_version", "outcome"},
	)
	reg.MustRegister(count, dur)
	handler := promhttp.HandlerFor(reg, promhttp.HandlerOpts{})
	return &Sink{count: count, dur: dur}, handler
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
