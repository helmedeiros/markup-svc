// Package s3sink is the ADR-0036 decisionsink adapter that writes
// markup.decision.v1 events to an S3-compatible bucket.
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

type Config struct {
	Endpoint   string
	Region     string
	Bucket     string
	AccessKey  string
	SecretKey  string
	UseSSL     bool
	AutoCreate bool
	KeyPrefix  string
	Env        string
	Instance   string
	QueueSize  int
	BatchSize  int
	BatchEvery time.Duration
	Logger     decisionsink.Logger
}

type Sink struct {
	cfg     Config
	client  *minio.Client
	metrics decisionsink.Metrics
	queue   chan decisionsink.Event
	seq     uint64

	dropped       uint64
	flushed       uint64
	lastDropLogNS int64
	done          chan struct{}
}

const bufferFullLogQuietWindow = 5 * time.Second

func New(ctx context.Context, cfg Config, metrics decisionsink.Metrics) (*Sink, error) {
	cfg = applyDefaults(cfg)
	if cfg.Bucket == "" {
		return nil, fmt.Errorf("s3sink: --decision-sink-bucket is required")
	}
	// minio-go's package-global MaxRetry compounds with our own bounded
	// backoff; set to 1 so upload() owns the retry policy.
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

func (s *Sink) Start(ctx context.Context) {
	go s.run(ctx)
}

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
				s.metrics.IncObject()
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
			// Drain queue + flush with fresh ctx so SIGTERM at deploy
			// doesn't strand events and the upload doesn't race the
			// canceled parent.
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

// estimateSize gates the BatchSize trigger without re-serialising.
func estimateSize(e decisionsink.Event) int {
	n := 256
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
