package s3sink

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/helmedeiros/markup-svc/internal/observability/decisionsink"
)

// TestObjectKey_HivePartitionLayout pins the ADR-0036 key layout so a
// downstream Spark / Snowflake / Athena reader sees stable partition
// columns. Renaming a partition key is a downstream-breaking change.
func TestObjectKey_HivePartitionLayout(t *testing.T) {
	s := &Sink{cfg: Config{
		KeyPrefix: "markup-decision-v1/",
		Env:       "production",
		Instance:  "markup-svc-abc12",
	}}
	now := time.Date(2024, 9, 12, 10, 32, 0, 0, time.UTC)
	got := s.objectKey(now, 42)
	want := "markup-decision-v1/dt=2024-09-12/hour=10/env=production/instance=markup-svc-abc12/batch-20240912T103200Z-000042.jsonl.gz"
	if got != want {
		t.Errorf("objectKey =\n  %q\nwant\n  %q", got, want)
	}
}

// TestSerializeBatch_RoundTripsAllEvents pins the gzip+JSONL wire
// shape: one event per line, gzip-decompressible, every event
// recoverable in order.
func TestSerializeBatch_RoundTripsAllEvents(t *testing.T) {
	events := []decisionsink.Event{
		{SchemaVersion: decisionsink.SchemaV1, DecisionID: "d-1", Env: "prod", DecideOutcome: "ok", Rule: "enterprise", MarkupFactor: 1.15},
		{SchemaVersion: decisionsink.SchemaV1, DecisionID: "d-2", Env: "prod", DecideOutcome: "no_match"},
		{SchemaVersion: decisionsink.SchemaV1, DecisionID: "d-3", Env: "prod", DecideOutcome: "error", Error: "boom"},
	}
	payload, err := serializeBatch(events)
	if err != nil {
		t.Fatalf("serializeBatch: %v", err)
	}
	gz, err := gzip.NewReader(bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	defer func() { _ = gz.Close() }()
	body, err := io.ReadAll(gz)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(body)), "\n")
	if len(lines) != 3 {
		t.Fatalf("lines = %d, want 3; body=%q", len(lines), body)
	}
	for i, line := range lines {
		var e decisionsink.Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("line[%d] unmarshal: %v", i, err)
		}
		if e.DecisionID != events[i].DecisionID {
			t.Errorf("line[%d] decision_id = %q, want %q", i, e.DecisionID, events[i].DecisionID)
		}
	}
}

// TestPublish_BufferFullEmitsRateLimitedLog confirms the first drop
// after a quiet window emits markup.decision.sink.buffer_full once;
// subsequent rapid drops within the window do NOT flood the log
// pipeline (the metrics counter carries the per-event volume).
func TestPublish_BufferFullEmitsRateLimitedLog(t *testing.T) {
	captured := &captureLogger{}
	s := &Sink{
		cfg:   applyDefaults(Config{Bucket: "test", QueueSize: 2, Logger: captured}),
		queue: make(chan decisionsink.Event, 2),
	}
	s.Publish(decisionsink.Event{DecisionID: "1"})
	s.Publish(decisionsink.Event{DecisionID: "2"})
	// Burst three quick drops; only the first should log.
	for i := 0; i < 3; i++ {
		s.Publish(decisionsink.Event{DecisionID: "burst"})
	}
	if got := captured.count("markup.decision.sink.buffer_full"); got != 1 {
		t.Fatalf("buffer_full log fired %d times in a burst, want 1 (rate-limited)", got)
	}
	if got := s.Dropped(); got != 3 {
		t.Errorf("Dropped() = %d, want 3 (all three burst events counted)", got)
	}
	attrs := captured.lastAttrs("markup.decision.sink.buffer_full")
	if attrs["queue_capacity"] != 2 {
		t.Errorf("queue_capacity attr = %v, want 2", attrs["queue_capacity"])
	}
}

// TestPublish_BufferFullDropsAndCounts pins the non-blocking contract:
// when the queue is full, Publish must NOT block, and must increment
// the drop counter with reason=buffer_full.
func TestPublish_BufferFullDropsAndCounts(t *testing.T) {
	mt := &countMetrics{}
	s := &Sink{
		cfg:     applyDefaults(Config{Bucket: "test", QueueSize: 2}),
		metrics: mt,
		queue:   make(chan decisionsink.Event, 2),
	}
	// Fill the queue without draining it. The third Publish must be
	// dropped.
	s.Publish(decisionsink.Event{DecisionID: "1"})
	s.Publish(decisionsink.Event{DecisionID: "2"})
	done := make(chan struct{})
	go func() {
		s.Publish(decisionsink.Event{DecisionID: "3"})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
		t.Fatalf("Publish blocked on full queue")
	}
	if got := s.Dropped(); got != 1 {
		t.Errorf("Dropped() = %d, want 1", got)
	}
	if !mt.hasDrop("buffer_full", 1) {
		t.Errorf("metrics did not record buffer_full drop: %+v", mt.drops)
	}
}

// TestRun_FlushCallsIncObjectPerBatch pins the ADR-0036 object-count
// contract: one metrics.IncObject call per successful PUT so an
// operator can catch a wrong-bucket regression that would still tick
// the bytes counter.
func TestRun_FlushCallsIncObjectPerBatch(t *testing.T) {
	srv := fakeS3Server(t, nil)
	defer srv.Close()
	mt := &countMetrics{}
	s := newSinkForTestWithMetrics(t, srv.URL, Config{
		Bucket:     "test",
		BatchSize:  1024,
		BatchEvery: time.Hour,
		QueueSize:  10,
	}, mt)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)
	s.Publish(bigEvent("obj-1"))
	s.Publish(bigEvent("obj-2"))
	s.Publish(bigEvent("obj-3"))
	waitFor(t, func() bool {
		mt.mu.Lock()
		defer mt.mu.Unlock()
		return mt.objects >= 1
	}, 2*time.Second, "at least one object PUT")
}

// TestRun_FlushesOnByteBudget proves the BatchSize trigger fires the
// flush before the time window expires. Uses a stub uploader (via the
// httptest S3 mock below) so we observe the PUT.
func TestRun_FlushesOnByteBudget(t *testing.T) {
	srv := fakeS3Server(t, nil)
	defer srv.Close()
	s := newSinkForTest(t, srv.URL, Config{
		Bucket:     "test",
		BatchSize:  1024, // tiny so two events trigger the byte-budget flush
		BatchEvery: time.Hour,
		QueueSize:  10,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)
	// Each event is ~300+ bytes (estimateSize floor 256 + fields).
	s.Publish(bigEvent("d-1"))
	s.Publish(bigEvent("d-2"))
	s.Publish(bigEvent("d-3"))
	waitFor(t, func() bool { return s.Flushed() >= 2 }, 2*time.Second, "flushed >= 2")
}

// TestRun_FlushesOnTimeWindow proves the BatchEvery trigger fires the
// flush even when the byte budget is far from saturated.
func TestRun_FlushesOnTimeWindow(t *testing.T) {
	srv := fakeS3Server(t, nil)
	defer srv.Close()
	s := newSinkForTest(t, srv.URL, Config{
		Bucket:     "test",
		BatchSize:  10 * 1024 * 1024,
		BatchEvery: 50 * time.Millisecond,
		QueueSize:  10,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)
	s.Publish(bigEvent("d-time"))
	waitFor(t, func() bool { return s.Flushed() >= 1 }, 2*time.Second, "time-window flush")
}

// TestRun_CtxCancelFlushesPendingBatch proves the graceful-shutdown
// invariant called out in ADR-0036: a running batch flushes once
// before the goroutine exits when ctx is canceled.
func TestRun_CtxCancelFlushesPendingBatch(t *testing.T) {
	srv := fakeS3Server(t, nil)
	defer srv.Close()
	s := newSinkForTest(t, srv.URL, Config{
		Bucket:     "test",
		BatchSize:  10 * 1024 * 1024,
		BatchEvery: time.Hour,
		QueueSize:  10,
	})
	ctx, cancel := context.WithCancel(context.Background())
	s.Start(ctx)
	s.Publish(bigEvent("d-shutdown"))
	// Wait for the goroutine to consume from the queue before canceling.
	waitFor(t, func() bool { return len(s.queue) == 0 }, 1*time.Second, "queue drained into batch")
	cancel()
	<-s.done
	if got := s.Flushed(); got < 1 {
		t.Errorf("graceful shutdown should flush pending batch; flushed=%d", got)
	}
}

// TestUpload_RetriesAndEventuallySucceeds pins the bounded-backoff
// retry contract. fakeS3Server returns 503 for the first two PUTs,
// then 200.
func TestUpload_RetriesAndEventuallySucceeds(t *testing.T) {
	var calls int32
	srv := fakeS3Server(t, func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodPut {
			return false
		}
		if atomic.AddInt32(&calls, 1) <= 2 {
			http.Error(w, "transient", http.StatusServiceUnavailable)
			return true
		}
		return false
	})
	defer srv.Close()
	s := newSinkForTest(t, srv.URL, Config{
		Bucket:     "test",
		BatchEvery: 50 * time.Millisecond,
		BatchSize:  10 * 1024 * 1024,
		QueueSize:  10,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)
	s.Publish(bigEvent("d-retry"))
	waitFor(t, func() bool { return s.Flushed() >= 1 }, 3*time.Second, "retry success")
	if atomic.LoadInt32(&calls) < 3 {
		t.Errorf("expected at least 3 PUTs (2 retries + success); got %d", calls)
	}
}

// TestUpload_RetriesExhaustedDropsBatch confirms a batch is dropped
// with reason=flush_failed when every retry attempt fails.
func TestUpload_RetriesExhaustedDropsBatch(t *testing.T) {
	srv := fakeS3Server(t, func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodPut {
			http.Error(w, "always fail", http.StatusServiceUnavailable)
			return true
		}
		return false
	})
	defer srv.Close()
	mt := &countMetrics{}
	s := newSinkForTestWithMetrics(t, srv.URL, Config{
		Bucket:     "test",
		BatchEvery: 30 * time.Millisecond,
		BatchSize:  10 * 1024 * 1024,
		QueueSize:  10,
	}, mt)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)
	s.Publish(bigEvent("d-drop"))
	// 3 attempts at 100ms + 400ms + 1.6s ≈ 2.1s plus the 30ms batch tick;
	// 8s budget keeps the test resilient under CI noise.
	waitFor(t, func() bool { return s.Dropped() >= 1 }, 8*time.Second, "drop after retries")
	if !mt.hasDrop("flush_failed", 1) {
		t.Errorf("metrics did not record flush_failed drop: %+v", mt.drops)
	}
}

// fakeS3Server stands in for MinIO/S3 during unit tests. It handles
// HEAD bucket as 200, PUT object as 200 (or whatever the optional
// custom handler returns). minio-go runs HEAD against the bucket on
// startup; we always answer 200 there.
func fakeS3Server(t *testing.T, custom func(http.ResponseWriter, *http.Request) bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if custom != nil && custom(w, r) {
			return
		}
		switch r.Method {
		case http.MethodHead:
			// minio-go BucketExists probe
			w.WriteHeader(http.StatusOK)
		case http.MethodPut:
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
}

// newSinkForTest builds a Sink pointed at the fake server. Bypasses
// the full New(ctx, ...) flow because the fake server's HEAD response
// is enough for the unit-test paths we care about.
func newSinkForTest(t *testing.T, endpoint string, cfg Config) *Sink {
	return newSinkForTestWithMetrics(t, endpoint, cfg, nil)
}

func newSinkForTestWithMetrics(t *testing.T, endpoint string, cfg Config, m decisionsink.Metrics) *Sink {
	t.Helper()
	cfg.Endpoint = strings.TrimPrefix(endpoint, "http://")
	cfg.AccessKey = "test"
	cfg.SecretKey = "test12345"
	cfg.UseSSL = false
	cfg.AutoCreate = false
	cfg.Env = "test"
	cfg.Instance = "unit"
	s, err := New(context.Background(), cfg, m)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func bigEvent(id string) decisionsink.Event {
	return decisionsink.Event{
		SchemaVersion:  decisionsink.SchemaV1,
		DecisionID:     id,
		Ts:             "2024-09-12T10:42:00.000000001Z",
		Env:            "test",
		ModelVersion:   "v1",
		Experiment:     "control",
		EngineAdapter:  "*indexed.Engine",
		Rule:           "enterprise",
		MarkupFactor:   1.15,
		DecideOutcome:  "ok",
		Error:          "",
		DurationMS:     0.487,
		CorrelationID:  "c-1",
		TraceID:        "t-deadbeefcafebabe",
		SpanID:         "s-12345678",
		RequestContext: map[string]any{"country": "DE", "customer_tier": "enterprise", "amount": 49.99},
	}
}

func waitFor(t *testing.T, cond func() bool, timeout time.Duration, label string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", label)
}

type captureLogger struct {
	mu     sync.Mutex
	events []logEvent
}

type logEvent struct {
	msg   string
	attrs map[string]any
}

func (c *captureLogger) Info(msg string, attrs map[string]any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, logEvent{msg: msg, attrs: attrs})
}

func (c *captureLogger) count(msg string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, e := range c.events {
		if e.msg == msg {
			n++
		}
	}
	return n
}

func (c *captureLogger) lastAttrs(msg string) map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := len(c.events) - 1; i >= 0; i-- {
		if c.events[i].msg == msg {
			return c.events[i].attrs
		}
	}
	return nil
}

type countMetrics struct {
	mu      sync.Mutex
	drops   []dropRecord
	flushes int
	objects int
}

type dropRecord struct {
	reason string
	n      int
}

func (c *countMetrics) IncDropped(reason string, n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.drops = append(c.drops, dropRecord{reason: reason, n: n})
}

func (c *countMetrics) IncFlushed(_ int, _ int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.flushes++
}

func (c *countMetrics) IncObject() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.objects++
}

func (c *countMetrics) hasDrop(reason string, atLeast int) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	total := 0
	for _, d := range c.drops {
		if d.reason == reason {
			total += d.n
		}
	}
	return total >= atLeast
}

