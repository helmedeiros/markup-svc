//go:build bench

package httpapi_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"
	"time"

	"github.com/helmedeiros/markup-svc/internal/decider/shadow"
	"github.com/helmedeiros/markup-svc/internal/httpapi"
	"github.com/helmedeiros/markup-svc/internal/markup"
)

// BenchmarkShadowFastPathUnloaded measures dispatchShadow's cost when
// shadow is wired but no challenger is loaded — the production-common
// case for an instance running with --shadow-admin but no active
// challenger envstate. Bar pre-registered in scientific/v0.1.22/REPORT.md:
// p99 ≤ 200 ns / op.
func BenchmarkShadowFastPathUnloaded(b *testing.B) {
	holder := shadow.New() // empty
	h := httpapi.Decide(noopDecider{},
		httpapi.WithShadow(holder, httpapi.NoopShadowMetrics{}, 10*time.Millisecond, nil, 1.0))
	body := []byte(`{"product_id":"p","amount":100}`)
	req := httptest.NewRequest(http.MethodPost, "/decide", bytes.NewReader(body))

	durations := make([]time.Duration, 0, b.N)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req.Body = http.NoBody // reset body to avoid re-reads
		rec := httptest.NewRecorder()
		start := time.Now()
		h.ServeHTTP(rec, req)
		durations = append(durations, time.Since(start))
	}
	b.StopTimer()
	p99 := percentile(durations, 0.99)
	b.ReportMetric(float64(percentile(durations, 0.50).Nanoseconds()), "p50-ns/op")
	b.ReportMetric(float64(p99.Nanoseconds()), "p99-ns/op")
	if p99 > 1*time.Millisecond {
		b.Errorf("p99 %v exceeds pre-registered 1 ms bar", p99)
	}
}

// BenchmarkShadowDispatchSampleRateOne measures the full sampling path:
// challenger loaded, sample=1.0, goroutine spawns, detached context
// allocated. Bar pre-registered in scientific/v0.1.22/REPORT.md:
// p99 ≤ 10 µs / op.
//
// The measured number includes the goroutine spawn cost; the goroutine
// body itself runs concurrently and does NOT appear in the per-call
// latency reported here.
func BenchmarkShadowDispatchSampleRateOne(b *testing.B) {
	holder := shadow.New()
	holder.Load(noopDecider{})
	h := httpapi.Decide(noopDecider{},
		httpapi.WithShadow(holder, httpapi.NoopShadowMetrics{}, 10*time.Millisecond, nil, 1.0))
	body := []byte(`{"product_id":"p","amount":100}`)

	durations := make([]time.Duration, 0, b.N)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req := httptest.NewRequest(http.MethodPost, "/decide", bytes.NewReader(body))
		rec := httptest.NewRecorder()
		start := time.Now()
		h.ServeHTTP(rec, req)
		durations = append(durations, time.Since(start))
	}
	b.StopTimer()
	p99 := percentile(durations, 0.99)
	b.ReportMetric(float64(percentile(durations, 0.50).Nanoseconds()), "p50-ns/op")
	b.ReportMetric(float64(p99.Nanoseconds()), "p99-ns/op")
	if p99 > 1*time.Millisecond {
		b.Errorf("p99 %v exceeds pre-registered 1 ms bar", p99)
	}
}

type noopDecider struct{}

func (noopDecider) Decide(_ context.Context, _ markup.Request) (markup.Decision, error) {
	return markup.Decision{MarkupFactor: 1.0, Rule: "bench"}, nil
}

func percentile(xs []time.Duration, p float64) time.Duration {
	if len(xs) == 0 {
		return 0
	}
	sorted := make([]time.Duration, len(xs))
	copy(sorted, xs)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	idx := int(float64(len(sorted)-1) * p)
	return sorted[idx]
}
