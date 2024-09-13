//go:build bench

package s3sink

import (
	"sort"
	"testing"
	"time"

	"github.com/helmedeiros/markup-svc/internal/observability/decisionsink"
)

// Bars pre-registered in scientific/v0.1.23/REPORT.md.

func BenchmarkSinkPublishNoop(b *testing.B) {
	var s decisionsink.Sink = decisionsink.NoopSink{}
	e := bigBenchEvent()
	durations := make([]time.Duration, 0, b.N)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		start := time.Now()
		s.Publish(e)
		durations = append(durations, time.Since(start))
	}
	b.StopTimer()
	p99 := benchPercentile(durations, 0.99)
	b.ReportMetric(float64(benchPercentile(durations, 0.50).Nanoseconds()), "p50-ns/op")
	b.ReportMetric(float64(p99.Nanoseconds()), "p99-ns/op")
	if p99 > 200*time.Nanosecond {
		b.Errorf("p99 %v exceeds pre-registered 200 ns bar", p99)
	}
}

func BenchmarkSinkPublishS3SinkEnqueue(b *testing.B) {
	s := &Sink{
		cfg:   applyDefaults(Config{Bucket: "test", QueueSize: 10000}),
		queue: make(chan decisionsink.Event, 10000),
	}
	e := bigBenchEvent()
	durations := make([]time.Duration, 0, b.N)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if len(s.queue) == cap(s.queue) {
			for len(s.queue) > 0 {
				<-s.queue
			}
		}
		start := time.Now()
		s.Publish(e)
		durations = append(durations, time.Since(start))
	}
	b.StopTimer()
	p99 := benchPercentile(durations, 0.99)
	b.ReportMetric(float64(benchPercentile(durations, 0.50).Nanoseconds()), "p50-ns/op")
	b.ReportMetric(float64(p99.Nanoseconds()), "p99-ns/op")
	if p99 > 5*time.Microsecond {
		b.Errorf("p99 %v exceeds pre-registered 5 µs bar", p99)
	}
}

func BenchmarkSinkPublishBufferFullDrop(b *testing.B) {
	mt := &benchMetrics{}
	s := &Sink{
		cfg:     applyDefaults(Config{Bucket: "test", QueueSize: 1}),
		metrics: mt,
		queue:   make(chan decisionsink.Event, 1),
	}
	s.queue <- decisionsink.Event{}
	e := bigBenchEvent()
	durations := make([]time.Duration, 0, b.N)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		start := time.Now()
		s.Publish(e)
		durations = append(durations, time.Since(start))
	}
	b.StopTimer()
	p99 := benchPercentile(durations, 0.99)
	b.ReportMetric(float64(benchPercentile(durations, 0.50).Nanoseconds()), "p50-ns/op")
	b.ReportMetric(float64(p99.Nanoseconds()), "p99-ns/op")
	if p99 > 1*time.Microsecond {
		b.Errorf("p99 %v exceeds pre-registered 1 µs bar", p99)
	}
}

func bigBenchEvent() decisionsink.Event {
	return decisionsink.Event{
		SchemaVersion:  decisionsink.SchemaV1,
		DecisionID:     "a1b2c3d4e5f60718a1b2c3d4e5f60718",
		Ts:             "2024-09-12T10:42:00.000000001Z",
		Env:            "production",
		ModelVersion:   "v1",
		Experiment:     "control",
		EngineAdapter:  "*indexed.Engine",
		Rule:           "enterprise",
		MarkupFactor:   1.15,
		DecideOutcome:  "ok",
		DurationMS:     0.487,
		CorrelationID:  "c-deadbeefcafebabe",
		TraceID:        "t-0123456789abcdef0123456789abcdef",
		SpanID:         "s-0123456789abcdef",
		RequestContext: map[string]any{"country": "DE", "customer_tier": "enterprise", "amount": 49.99, "channel": "web"},
	}
}

func benchPercentile(samples []time.Duration, p float64) time.Duration {
	if len(samples) == 0 {
		return 0
	}
	cp := make([]time.Duration, len(samples))
	copy(cp, samples)
	sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })
	idx := int(float64(len(cp)-1) * p)
	if idx < 0 {
		idx = 0
	}
	if idx >= len(cp) {
		idx = len(cp) - 1
	}
	return cp[idx]
}

type benchMetrics struct{}

func (benchMetrics) IncDropped(string, int) {}
func (benchMetrics) IncFlushed(int, int)    {}
