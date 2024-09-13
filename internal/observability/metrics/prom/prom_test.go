package prom_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/helmedeiros/markup-svc/internal/markup"
	"github.com/helmedeiros/markup-svc/internal/observability/metrics"
	"github.com/helmedeiros/markup-svc/internal/observability/metrics/prom"
)

// TestSink_RecordDecision_ExposesCountersAndHistograms drives the
// Sink with three outcomes (ok / no_match / error) and asserts the
// /metrics scrape body contains exactly the matching counter +
// histogram lines.
func TestSink_RecordDecision_ExposesCountersAndHistograms(t *testing.T) {
	sink, _, _, handler := prom.New("test-env")

	sink.RecordDecision(metrics.DecisionMetric{
		Adapter:      "*inmemory.Engine",
		ModelVersion: "v1",
		Rule:         "enterprise",
		MarkupFactor: 1.15,
		Duration:     50 * time.Microsecond,
	})
	sink.RecordDecision(metrics.DecisionMetric{Duration: 30 * time.Microsecond, NoMatch: true})
	sink.RecordDecision(metrics.DecisionMetric{Duration: 10 * time.Millisecond, Err: errors.New("boom")})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("/metrics status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	wantSubstrs := []string{
		// counter — note the metric_name{labels} value shape
		`markup_decide_total{adapter="*inmemory.Engine",model_version="v1",outcome="ok"} 1`,
		`markup_decide_total{adapter="",model_version="",outcome="no_match"} 1`,
		`markup_decide_total{adapter="",model_version="",outcome="error"} 1`,
		// histogram — the _sum / _count are reliable markers
		`markup_decide_duration_seconds_count{adapter="*inmemory.Engine",model_version="v1",outcome="ok"} 1`,
		`markup_decide_duration_seconds_count{adapter="",model_version="",outcome="no_match"} 1`,
		`markup_decide_duration_seconds_count{adapter="",model_version="",outcome="error"} 1`,
	}
	for _, want := range wantSubstrs {
		if !strings.Contains(body, want) {
			t.Errorf("expected /metrics body to contain %q; got:\n%s", want, body)
		}
	}
}

// TestSink_AsDecorator runs the Sink behind the existing metrics.Wrap
// decorator and asserts the scrape body reflects the correct outcome
// counts for a sequence of real Decide invocations through an
// in-memory stub Decider.
func TestSink_AsDecorator(t *testing.T) {
	sink, _, _, handler := prom.New("test-env")

	inner := &stubDecider{
		decision: markup.Decision{EngineAdapter: "*stub", ModelVersion: "vTest", Rule: "default", MarkupFactor: 1.00},
	}
	d := metrics.Wrap(inner, sink)

	for i := 0; i < 3; i++ {
		_, _ = d.Decide(context.Background(), markup.Request{})
	}
	inner.err = markup.ErrNoMatch
	_, _ = d.Decide(context.Background(), markup.Request{})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := rec.Body.String()

	if !strings.Contains(body, `markup_decide_total{adapter="*stub",model_version="vTest",outcome="ok"} 3`) {
		t.Errorf("expected ok counter = 3; got:\n%s", body)
	}
	if !strings.Contains(body, `markup_decide_total{adapter="",model_version="",outcome="no_match"} 1`) {
		t.Errorf("expected no_match counter = 1; got:\n%s", body)
	}
}

// TestShadowSink_AllSeriesCarryEnvLabel exercises every Record* method
// on the ShadowSink with env="production" and asserts the /metrics
// scrape body carries env="production" on each series. Pins the
// ADR-0034 contract.
func TestShadowSink_AllSeriesCarryEnvLabel(t *testing.T) {
	_, shadow, _, handler := prom.New("production")

	shadow.RecordAgreement(true)
	shadow.RecordAgreement(false)
	shadow.RecordOneSided("champion_only")
	shadow.RecordTimeout()
	shadow.RecordError()
	shadow.RecordFactorDelta(0.123)
	shadow.RecordSampled(true)
	shadow.RecordChallengerDuration(75 * time.Microsecond)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := rec.Body.String()

	wantSubstrings := []string{
		`markup_challenger_agreement_total{agree="true",env="production"} 1`,
		`markup_challenger_agreement_total{agree="false",env="production"} 1`,
		`markup_challenger_one_sided_total{env="production",side="champion_only"} 1`,
		`markup_challenger_eval_timeout_total{env="production"} 1`,
		`markup_challenger_eval_errors_total{env="production"} 1`,
		`markup_challenger_factor_delta_count{env="production"} 1`,
		`markup_challenger_sampled_total{env="production",sampled="true"} 1`,
		`markup_challenger_decide_duration_seconds_count{env="production"} 1`,
	}
	for _, want := range wantSubstrings {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in scrape body:\n%s", want, body)
		}
	}
}

// TestPromNew_EmptyEnvDefaultsToDefault pins the constructor's "" → "default"
// fallback so a deployment that boots without setting --env produces a
// usable env label.
func TestPromNew_EmptyEnvDefaultsToDefault(t *testing.T) {
	_, shadow, _, handler := prom.New("")
	shadow.RecordAgreement(true)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if !strings.Contains(rec.Body.String(), `env="default"`) {
		t.Fatalf("empty env should default to \"default\":\n%s", rec.Body.String())
	}
}

// TestDecisionSinkMetrics_ExposesCountersWithEnvLabel pins the ADR-0036
// substrate signals. The two drop reasons + flushed counter + flushed
// bytes counter are the canonical observable surface.
func TestDecisionSinkMetrics_ExposesCountersWithEnvLabel(t *testing.T) {
	_, _, sinkMetrics, handler := prom.New("production")

	sinkMetrics.IncDropped("buffer_full", 3)
	sinkMetrics.IncDropped("flush_failed", 1)
	sinkMetrics.IncFlushed(250, 12345)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := rec.Body.String()
	want := []string{
		`markup_decision_sink_dropped_total{env="production",reason="buffer_full"} 3`,
		`markup_decision_sink_dropped_total{env="production",reason="flush_failed"} 1`,
		`markup_decision_sink_flushed_total{env="production"} 250`,
		`markup_decision_sink_flushed_bytes_total{env="production"} 12345`,
	}
	for _, w := range want {
		if !strings.Contains(body, w) {
			t.Errorf("missing %q in scrape body:\n%s", w, body)
		}
	}
}

type stubDecider struct {
	decision markup.Decision
	err      error
}

func (s *stubDecider) Decide(context.Context, markup.Request) (markup.Decision, error) {
	return s.decision, s.err
}
