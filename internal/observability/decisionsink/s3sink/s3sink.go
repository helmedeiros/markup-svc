// Package s3sink is the ADR-0036 decisionsink adapter that writes
// markup.decision.v1 events to an S3-compatible bucket. Development
// substrate is MinIO running in compose; production substrate is real
// S3 (AWS) — same client, same wire protocol, env-var endpoint flip.
//
// Operational posture (from ADR-0036):
//
//   - Publish is non-blocking; the queue is bounded.
//   - On queue full or on flush-retry exhaustion the batch is dropped
//     with markup_decision_sink_dropped_total ticking; /decide never
//     blocks on S3 trouble.
//   - Batches flush on the first of (time window, byte budget).
//   - Object keys use Hive partition style so Spark / Snowflake / Athena
//     / BigQuery external tables read the bucket natively.
package s3sink

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/helmedeiros/markup-svc/internal/observability/decisionsink"
)

// Config carries the operator-facing knobs. Zero values fall through
// to documented defaults; New applies them.
type Config struct {
	Endpoint   string // "" for the SDK default (real AWS); set for MinIO / GCS-S3 / Azure-S3
	Region     string // default "us-east-1"
	Bucket     string // required
	AccessKey  string // typically from AWS_ACCESS_KEY_ID
	SecretKey  string // typically from AWS_SECRET_ACCESS_KEY
	UseSSL     bool
	AutoCreate bool              // true → CreateBucket if missing; false → fail loud
	KeyPrefix  string            // default "markup-decision-v1/"
	Env        string            // partition value for env=
	Instance   string            // partition value for instance=
	QueueSize  int               // default 10_000
	BatchSize  int               // default 10 * 1024 * 1024 (pre-compression)
	BatchEvery time.Duration     // default 5 * time.Minute
	Logger     decisionsink.Logger // optional; informational flush/drop events
}

// Sink is the ADR-0036 adapter. Construct with New, then Start to
// launch the background flush loop.
type Sink struct {
	cfg     Config
	client  *minio.Client
	metrics decisionsink.Metrics
	queue   chan decisionsink.Event
	seq     uint64

	dropped         uint64
	flushed         uint64
	lastDropLogNS   int64 // atomic; rate-limits Publish-side drop logs
	done            chan struct{}
}

// bufferFullLogQuietWindow is the minimum interval between two
// markup.decision.sink.buffer_full log emissions. Bursty queue-full
// failures during a sustained S3 outage would otherwise flood the
// log pipeline; one log per quiet-window is enough to alert on the
// onset and the metrics counter carries the volume.
const bufferFullLogQuietWindow = 5 * time.Second

// New constructs the Sink and verifies the bucket exists (or creates
// it when AutoCreate is true).
func New(ctx context.Context, cfg Config, metrics decisionsink.Metrics) (*Sink, error) {
	cfg = applyDefaults(cfg)
	if cfg.Bucket == "" {
		return nil, fmt.Errorf("s3sink: --decision-sink-bucket is required")
	}
	// minio-go's package-global MaxRetry defaults to 10 with binomial
	// backoff which compounds against our own bounded-backoff loop and
	// stretches one failed PUT into ~30s. Setting it to 1 makes the
	// SDK do exactly one attempt; our upload() owns the retry policy.
	minio.MaxRetry = 1
	client, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: cfg.UseSSL,
		Region: cfg.Region,
	})
	if err != nil {
		return nil, fmt.Errorf("s3sink: minio client: %w", err)
	}
	exists, err := client.BucketExists(ctx, cfg.Bucket)
	if err != nil {
		return nil, fmt.Errorf("s3sink: BucketExists %q: %w", cfg.Bucket, err)
	}
	if !exists {
		if !cfg.AutoCreate {
			return nil, fmt.Errorf("s3sink: bucket %q does not exist (set --decision-sink-bucket-auto-create or create it manually)", cfg.Bucket)
		}
		if err := client.MakeBucket(ctx, cfg.Bucket, minio.MakeBucketOptions{Region: cfg.Region}); err != nil {
			return nil, fmt.Errorf("s3sink: MakeBucket %q: %w", cfg.Bucket, err)
		}
	}
	return &Sink{
		cfg:     cfg,
		client:  client,
		metrics: metrics,
		queue:   make(chan decisionsink.Event, cfg.QueueSize),
		done:    make(chan struct{}),
	}, nil
}

// Start launches the background flush goroutine. Cancel ctx to stop.
func (s *Sink) Start(ctx context.Context) {
	go s.run(ctx)
}

// Publish enqueues one event. Non-blocking — a full queue increments
// the drop counter and emits a rate-limited structured log on the
// onset of bursts (first event after a 5s quiet window). The log
// names the queue depth and the lifetime drop count so operators see
// the early signal without the log pipeline drowning under sustained
// pressure.
func (s *Sink) Publish(e decisionsink.Event) {
	select {
	case s.queue <- e:
	default:
		s.recordDrop("buffer_full", 1)
		s.maybeLogBufferFullDrop()
	}
}

func (s *Sink) maybeLogBufferFullDrop() {
	if s.cfg.Logger == nil {
		return
	}
	now := time.Now().UnixNano()
	prev := atomic.LoadInt64(&s.lastDropLogNS)
	if now-prev < int64(bufferFullLogQuietWindow) {
		return
	}
	if !atomic.CompareAndSwapInt64(&s.lastDropLogNS, prev, now) {
		return
	}
	s.cfg.Logger.Info("markup.decision.sink.buffer_full", map[string]any{
		"queue_capacity":    s.cfg.QueueSize,
		"lifetime_dropped":  atomic.LoadUint64(&s.dropped),
	})
}

func (s *Sink) run(ctx context.Context) {
	defer close(s.done)
	batch := make([]decisionsink.Event, 0, 256)
	batchBytes := 0
	timer := time.NewTimer(s.cfg.BatchEvery)
	defer timer.Stop()

	flush := func(uploadCtx context.Context) {
		if len(batch) == 0 {
			return
		}
		payload, err := serializeBatch(batch)
		if err != nil {
			s.recordDrop("serialize_failed", len(batch))
			batch = batch[:0]
			batchBytes = 0
			return
		}
		if err := s.upload(uploadCtx, payload); err != nil {
			s.recordDrop("flush_failed", len(batch))
			s.log("markup.decision.sink.flush_failed", map[string]any{
				"events": len(batch),
				"bytes":  len(payload),
				"error":  err.Error(),
			})
		} else {
			atomic.AddUint64(&s.flushed, uint64(len(batch)))
			if s.metrics != nil {
				s.metrics.IncFlushed(len(batch), len(payload))
			}
			s.log("markup.decision.sink.flushed", map[string]any{
				"events": len(batch),
				"bytes":  len(payload),
			})
		}
		batch = batch[:0]
		batchBytes = 0
	}

	resetTimer := func() {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(s.cfg.BatchEvery)
	}

	for {
		select {
		case <-ctx.Done():
			// Graceful shutdown: drain anything still in the queue
			// into the current batch so a SIGTERM during a deploy
			// does not strand pending events, then flush with a
			// fresh context bounded to 10s so the final upload does
			// not race the canceled parent ctx.
			for {
				select {
				case e := <-s.queue:
					batch = append(batch, e)
					batchBytes += estimateSize(e)
				default:
					goto drained
				}
			}
		drained:
			shutdownCtx, scancel := context.WithTimeout(context.Background(), 10*time.Second)
			flush(shutdownCtx)
			scancel()
			return
		case e := <-s.queue:
			batch = append(batch, e)
			batchBytes += estimateSize(e)
			if batchBytes >= s.cfg.BatchSize {
				flush(ctx)
				resetTimer()
			}
		case <-timer.C:
			flush(ctx)
			timer.Reset(s.cfg.BatchEvery)
		}
	}
}

// upload PUTs payload at a fresh object key. Bounded exponential
// backoff: three attempts at 100 ms / 400 ms / 1.6 s. Returns the
// last error if all attempts fail.
func (s *Sink) upload(ctx context.Context, payload []byte) error {
	seq := atomic.AddUint64(&s.seq, 1)
	key := s.objectKey(time.Now().UTC(), seq)
	backoffs := []time.Duration{100 * time.Millisecond, 400 * time.Millisecond, 1600 * time.Millisecond}
	var lastErr error
	for i, wait := range backoffs {
		_, err := s.client.PutObject(ctx, s.cfg.Bucket, key, bytes.NewReader(payload), int64(len(payload)),
			minio.PutObjectOptions{ContentType: "application/x-gzip", ContentEncoding: "gzip"})
		if err == nil {
			return nil
		}
		lastErr = err
		if i < len(backoffs)-1 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
		}
	}
	return lastErr
}

// objectKey builds the Hive-partition-style key for one batch.
//
//	markup-decision-v1/dt=2024-09-12/hour=10/env=production/
//	  instance=markup-svc-abc12/batch-20240912T103200Z-000042.jsonl.gz
func (s *Sink) objectKey(now time.Time, seq uint64) string {
	return fmt.Sprintf("%sdt=%s/hour=%02d/env=%s/instance=%s/batch-%s-%06d.jsonl.gz",
		s.cfg.KeyPrefix,
		now.Format("2006-01-02"),
		now.Hour(),
		s.cfg.Env,
		s.cfg.Instance,
		now.Format("20060102T150405Z"),
		seq,
	)
}

// serializeBatch writes each Event as one JSON line, gzip-compressed.
func serializeBatch(events []decisionsink.Event) ([]byte, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	enc := json.NewEncoder(gz)
	for _, e := range events {
		if err := enc.Encode(e); err != nil {
			_ = gz.Close()
			return nil, err
		}
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// estimateSize ballparks the pre-compression bytes one event will
// contribute to the batch buffer so the BatchSize trigger fires
// before memory blows up. The estimate is intentionally coarse —
// exact size requires serialising the event, which is the work we're
// trying to amortise.
func estimateSize(e decisionsink.Event) int {
	n := 256 // baseline: fixed fields + JSON quoting
	n += len(e.DecisionID) + len(e.Ts) + len(e.Env) + len(e.ModelVersion) +
		len(e.Experiment) + len(e.EngineAdapter) + len(e.Rule) +
		len(e.DecideOutcome) + len(e.Error) + len(e.CorrelationID) +
		len(e.TraceID) + len(e.SpanID)
	for k, v := range e.RequestContext {
		n += len(k) + 16
		if s, ok := v.(string); ok {
			n += len(s)
		}
	}
	return n
}

func (s *Sink) recordDrop(reason string, n int) {
	atomic.AddUint64(&s.dropped, uint64(n))
	if s.metrics != nil {
		s.metrics.IncDropped(reason, n)
	}
}

func (s *Sink) log(msg string, attrs map[string]any) {
	if s.cfg.Logger == nil {
		return
	}
	s.cfg.Logger.Info(msg, attrs)
}

// Dropped returns the lifetime count of dropped events.
func (s *Sink) Dropped() uint64 { return atomic.LoadUint64(&s.dropped) }

// Flushed returns the lifetime count of events successfully delivered.
func (s *Sink) Flushed() uint64 { return atomic.LoadUint64(&s.flushed) }

func applyDefaults(c Config) Config {
	if c.Region == "" {
		c.Region = "us-east-1"
	}
	if c.KeyPrefix == "" {
		c.KeyPrefix = "markup-decision-v1/"
	}
	if c.QueueSize == 0 {
		c.QueueSize = 10000
	}
	if c.BatchSize == 0 {
		c.BatchSize = 10 * 1024 * 1024
	}
	if c.BatchEvery == 0 {
		c.BatchEvery = 5 * time.Minute
	}
	return c
}
