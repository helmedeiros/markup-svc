# 36. Decision-event substrate — MinIO / S3-compatible batched JSONL

## Status

Accepted — introduce a `decisionsink` port at the access-log emission seam and ship an `s3sink` adapter that writes `markup.decision.v1` events as gzipped JSONL files into an S3-compatible bucket. The development substrate is MinIO running as a container in `decision-gateway/docker-compose.yaml`; the production substrate is the operator's choice of real S3 (AWS) / GCS (via the S3 gateway) / Azure Blob (via the S3 gateway). The port + middleware seam land in this commit; the `s3sink` adapter, cmd flags, and MinIO compose service land in the follow-on commit.

## Context

ADR-0035 locked the `markup.decision.v1` schema and wired its emission as a structured-log event from `WithAccessLog`. Today those events flow through the existing Filebeat → Elasticsearch path that pricing-observability already operates for `markup-server.access`. ES is the right destination for ad-hoc Kibana search and short-retention logs; it is the wrong destination for the C4 "Learning Loop":

- An ML training pipeline needs to scan months of decisions to materialise a training set. ES retention is days; the indices roll out beyond that. Pulling decisions back out of ES at training time is expensive and not the access pattern ES is tuned for.
- A feature store backfill job wants newline-JSONL or Parquet files in a partition-keyed bucket, not an Elasticsearch query API. The de-facto data-lake shape across the ML ecosystem (Snowflake, BigQuery external tables, Spark, Databricks, Vertex AI training jobs) is "files in a bucket".
- A replay tool that re-runs a six-month-old decision against a new ruleset wants point-in-time durable artifacts, not log entries that may have been rolled out.

The C4 sketch's "Decision Event Stream" container is therefore an **archive-shaped substrate**, not a stream-shaped one — for v1. (A stream-shaped substrate — Pub/Sub, Kafka, NATS — is a separate ADR if/when a downstream needs sub-second consumption.)

S3 is the dominant data-lake substrate; MinIO is the canonical S3-compatible local server. The development pattern is identical to the existing observability stack: one container, one healthcheck, one env-var flip to switch between MinIO and real S3.

## Decision

### `decisionsink.Sink` port + typed `Event`

A new port package `internal/observability/decisionsink` mirroring the precedent of `internal/observability/metrics`. The port defines a typed `Event` struct (one field per ADR-0035 schema column) plus a one-method `Sink` interface:

```go
package decisionsink

type Event struct {
    SchemaVersion  string         `json:"schema_version"`
    DecisionID     string         `json:"decision_id"`
    Ts             string         `json:"ts"`            // RFC3339Nano
    Env            string         `json:"env"`
    ModelVersion   string         `json:"model_version"`
    Experiment     string         `json:"experiment"`
    EngineAdapter  string         `json:"engine_adapter"`
    Rule           string         `json:"rule"`
    MarkupFactor   float64        `json:"markup_factor"`
    DecideOutcome  string         `json:"decide_outcome"`
    Error          string         `json:"error"`
    DurationMS     float64        `json:"duration_ms"`
    CorrelationID  string         `json:"correlation_id"`
    TraceID        string         `json:"trace_id"`
    SpanID         string         `json:"span_id"`
    RequestContext map[string]any `json:"request_context"`
}

type Sink interface {
    Publish(event Event)
}

type NoopSink struct{}

func (NoopSink) Publish(Event) {}
```

The struct is the authoritative shape of the contract — adapters never re-interpret a free-form map. The `internal/httpapi` adapter imports this package, builds the `Event`, calls `sink.Publish`, and continues emitting the existing `markup.decision.v1` structured log. The `s3sink` adapter (described next) does the same: imports the port, never imports `internal/httpapi`.

`httpapi.WithDecisionSink(sink decisionsink.Sink) DecideOption` wires the sink at the handler. Default boot ships `decisionsink.NoopSink{}` so existing operators see no behaviour change. The sink is an **additional** consumer of the same `Event`, not a replacement for the log emission.

### `internal/observability/decisionsink/s3sink` adapter

A new sub-package implementing `decisionsink.Sink`. Internal seam split into two private types so a future stream adapter (Pub/Sub, Kafka — described in Not closed) can reuse the serialisation half without inheriting S3-specific upload code:

- `batchWriter` — owns the bounded in-memory queue, the flush-trigger logic (time window OR byte budget), and the gzipped-JSONL serialisation. Emits a `[]byte` payload per flush.
- `s3uploader` — owns the S3 PUT call via `minio-go/v7`, the bounded exponential-backoff retry, and the per-batch object-key generation. Takes a `[]byte` payload; returns success or drop.

Operational behaviour:

1. `Publish(Event)` enqueues to a bounded channel (default 10,000 events). Non-blocking: a full queue drops with `markup_decision_sink_dropped_total{reason="buffer_full"}`, never blocks the `/decide` caller.
2. `batchWriter` flushes when **either** the time window (default 5 min) **or** the byte budget (default 10 MB pre-compression) is exceeded, whichever first. The flush serialises the queued `Event`s to gzipped JSONL.
3. `s3uploader` PUTs the payload to `<bucket>/<object-key>`. On failure, retries with bounded exponential backoff (100 ms / 400 ms / 1.6 s). After the bound, the batch is dropped with `markup_decision_sink_dropped_total{reason="flush_failed"}`.

The dependency is `github.com/minio/minio-go/v7` — a small client that speaks real S3 just as well as MinIO. AWS SDK v2 is avoided because it requires Go 1.19+; markup-svc is on Go 1.18 per the existing baseline.

### Object-key layout

Hive-partition style so downstream batch tools (Spark, Snowflake `COPY INTO`, Athena) read it natively:

```
markup-decision-v1/
  dt=2024-09-12/
    hour=10/
      env=production/
        instance=markup-svc-pod-abc12/
          batch-20240912T103200Z-000042.jsonl.gz
```

- `dt=` and `hour=` partitions match the Hive convention universally read by lake-shaped consumers.
- `env=` mirrors the ADR-0034 metric label dimension; a downstream consumer can filter to one env without scanning all objects.
- `instance=` keeps writers from one process isolated from another's batches so a parallel-producer deployment does not race on object names.
- `batch-<RFC3339>-<seq>.jsonl.gz` keeps file names lexicographically sortable + collision-free.

### compose stack

`decision-gateway/docker-compose.yaml` gains a `minio` service:

```yaml
minio:
  image: minio/minio:RELEASE.2024-08-26T15-33-07Z
  command: server /data --console-address ":9001"
  environment:
    MINIO_ROOT_USER: minio
    MINIO_ROOT_PASSWORD: minio12345
  volumes:
    - minio-data:/data
  ports:
    - "9000:9000"   # S3 API
    - "9001:9001"   # console
  healthcheck:
    test: ["CMD", "curl", "-f", "http://localhost:9000/minio/health/live"]
    interval: 5s
    timeout: 5s
    retries: 5
```

markup-svc gets `--decision-sink=s3`, `--decision-sink-endpoint`, `--decision-sink-bucket`, `--decision-sink-region` flags. The compose-stack defaults wire `--decision-sink=s3 --decision-sink-endpoint=http://minio:9000 --decision-sink-bucket=markup-decisions --decision-sink-region=us-east-1` and the standard `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` env vars. A real-cloud deploy unsets `--decision-sink-endpoint` and the SDK uses its default credential chain.

### Fake-consumer container (proves end-to-end)

A small `cmd/decision-replay` (or similar in a separate repo) that lists the bucket, downloads recent batches, gunzips, prints one event per line. The compose stack can run it as a one-shot service to demonstrate the full path: traffic-gen → markup-svc → MinIO → consumer.

### Backward compatibility

- Default boot ships `NoopDecisionSink`. Existing operators see no behaviour change.
- `markup.decision.v1` continues emitting through the existing log path regardless of whether the sink is wired. The sink is an additional consumer, not a replacement.
- `markup-server.access` is unchanged. The follow-on access-log-slimming ADR (referenced in ADR-0035) still slims it independently.

### Not closed (deferred to follow-on ADRs)

- **Schema evolution at the substrate.** When `markup.decision.v1` minor-bumps to v1.1 (adds an optional field), the S3 objects keep the same key prefix; downstream consumers ignore unknown fields per the contract. When v2 ships (breaking), a new `markup-decision-v2/` key prefix runs in parallel for the documented migration window. Spelled out here as the policy; no code change in this ADR.
- **Stream substrate.** A future Pub/Sub or Kafka adapter implements the same `DecisionSink` port. Same emission seam, different durability target. Whether to wire both substrates simultaneously is a follow-on operator choice.
- **Compression and format alternatives.** Gzipped JSONL is the lowest-friction default. Parquet would compress better and self-describe schema for Spark/Snowflake, but a markup-svc-side Parquet writer is non-trivial. A separate "Parquet conversion" job (e.g., a scheduled batch container) can transcode JSONL → Parquet downstream.
- **Audit / retention policy.** How long markup-svc keeps batches in MinIO before the bucket lifecycle policy deletes them is an operational choice, not a markup-svc decision.
- **Replay tool.** Beyond the `cmd/decision-replay` demo, a real replay tool that re-runs decisions against a new ruleset is its own arc (cross-references `mrctl shadow` semantics).

## Consequences

### Positive

- The C4 "Decision Event Stream" container has a concrete substrate that downstream Feature Store / ML Training Pipeline / replay tools can actually consume.
- The development experience matches the existing observability-stack pattern: one compose service, one healthcheck, runnable on a laptop.
- The cloud-flip is one env-var change away (`--decision-sink-endpoint` unset → SDK uses AWS / GCS S3 endpoint via the default credential chain).
- Hive-partitioned object keys mean Spark / Snowflake / Athena / BigQuery external tables read the bucket natively without a transcode step.
- The fast-path remains untouched on the `/decide` handler: enqueue is non-blocking; backpressure surfaces as a metric.

### Negative

- New dependency: `github.com/minio/minio-go/v7`. Bounded — the client is small and Go-1.18 compatible — but it is the first external network-IO dependency markup-svc takes on.
- A new failure mode: the sink can drop events when the buffer is full or when flush retries are exhausted. Operators read `markup_decision_sink_dropped_total` to monitor; runbook lands in pricing-observability alongside the existing markup runbooks.
- Two consumers of the same `markup.decision.v1` payload now exist when a sink is wired: the structured-log emission AND the typed sink. The middleware computes the `ts` string once and hands it to both builders; it reuses the same `inputFields` map for `request_context` so the second build does not re-allocate the nested map. The remaining per-Decide cost when a sink is wired is one `decisionsink.Event` struct (~232 B) + one interface dispatch; this allocation is the cost of the typed contract.
- Default deployment cost is zero. `WithAccessLog` captures a `sinkEnabled` boolean at construction by type-asserting against `NoopSink`. When the cmd wiring passes nil or `NoopSink{}`, the per-request hot path skips the Event build, skips the interface call, and pays no allocation. Operators who do not opt into a substrate see no change.
- Per-Decide cost of the sink-enqueue path WHEN A SUBSTRATE IS WIRED is unmeasured at the time of this ADR; a scientific-harness bar pre-registration is parked at `scientific/v0.1.23/` for a follow-on commit per ADR-0012 protocol. The default-deployment 0-allocation claim above will be the matching bar's pre-registered floor.

### Not closed (deferred to follow-on ADRs)

- **Per-Decide bench bar for sink-enqueue.** Same ADR-0012 parking pattern as ADR-0035.
- **PII redaction at the sink.** ADR-0035's privacy notes carry forward; redaction policy applies at the schema layer, not at the sink.
- **Schema registry.** An external schema-registry service (versioning, validation at write time) is over-engineering for v1; deferred until a real downstream consumer needs it.

## References

- ADR-0034 — env label across shadow metrics + access log + decide span (the env dimension this ADR's partition keys inherit).
- ADR-0035 — decision-event contract (`markup.decision.v1`).
- model-registry ADR-0012 — challenger-deploy rolling pattern (similar bounded-buffer + retry pattern; reused here).
- Pricing Decision Platform C4 sketch — "Decision Event Stream" container, "Learning Loop (context + decision + outcome)" arrow.
- `github.com/minio/minio-go/v7` — S3-compatible Go client.
- MinIO Server documentation — local single-node configuration.
