# Architecture Decision Records

Each file in this folder captures one architecture decision made on the markup-svc codebase, following the standard ADR shape (Status / Context / Decision / Consequences).

New decisions get the next number and a short kebab-case slug:

```
NNNN-short-decision-name.md
```

`scripts/check-adrs.sh` (wired into `make ci-local`) verifies that:

1. Every ADR file is indexed in this README.
2. Every README link points at a file that exists.
3. Every ADR file has a `## Status` line with one of: `Proposed`, `Accepted`, `Superseded by ADR-NNNN`, `Deprecated`.
4. Every ADR file has the four standard sections: `## Status`, `## Context`, `## Decision`, `## Consequences`.

## Index

| # | Title | Status |
|---|---|---|
| [0001](0001-domain-port.md) | Domain port: Decider interface for markup decisions | ✅ Accepted |
| [0002](0002-rule-format-csv.md) | Rule format: CSV with parser expressions | ✅ Accepted |
| [0003](0003-http-decide-route.md) | HTTP transport: POST /decide | ✅ Accepted |
| [0004](0004-firstmatch-adapter.md) | First-match Decider adapter | ✅ Accepted |
| [0005](0005-priority-adapter.md) | Priority Decider adapter | ✅ Accepted |
| [0006](0006-indexed-adapter.md) | Indexed Decider adapter | ✅ Accepted |
| [0007](0007-snapshot-persistence.md) | Snapshot persistence for the indexed adapter | ✅ Accepted |
| [0008](0008-hot-reload.md) | Hot reload via admin endpoint | ✅ Accepted |
| [0009](0009-otel-spans.md) | OpenTelemetry spans at the Decider port | ✅ Accepted |
| [0010](0010-metrics-port.md) | Metrics port at the Decider layer | ✅ Accepted |
| [0011](0011-router.md) | Router decorator: A/B variants and multi-model routing | ✅ Accepted |
| [0012](0012-scientific-harness.md) | Scientific performance comparison harness | ✅ Accepted |
| [0013](0013-production-deploy.md) | Production deployment artifacts (Dockerfile + Kubernetes manifests) | ✅ Accepted |
| [0014](0014-guardrails.md) | Guardrails decorator at the Decider port | ✅ Accepted |
| [0015](0015-guardrails-hot-reload.md) | Hot-reload guardrails via POST /admin/guardrails | ✅ Accepted |
| [0016](0016-otel-sdk-bootstrap.md) | Bootstrap the OTel SDK for --otel-enabled | ✅ Accepted |
| [0017](0017-incoming-trace-context-multi-layer-spans.md) | Incoming W3C trace context + multi-layer Decide spans | ✅ Accepted |
| [0018](0018-multi-arch-images.md) | Multi-arch (linux/amd64 + linux/arm64) image publish | ✅ Accepted |
| [0019](0019-prometheus-metrics-sink.md) | Prometheus Sink + /metrics endpoint | ✅ Accepted |
| [0020](0020-spankind-server-on-outer-decide.md) | SpanKind=Server on the outer markup.decider.decide span | ✅ Accepted |
| [0021](0021-structured-json-logs.md) | Structured JSON logs (boot, access, shutdown) | ✅ Accepted |
| [0022](0022-h2c-server.md) | h2c (HTTP/2 cleartext) on the markup-svc server | ✅ Accepted |
| [0023](0023-access-log-decision-fields.md) | Access log carries the matched rule, inputs, and outputs | ✅ Accepted |
| [0024](0024-sub-ms-histogram-buckets.md) | Sub-millisecond histogram buckets for the Decide histogram | ✅ Accepted |
| [0025](0025-diagnose-and-sentinels.md) | Diagnose() + port-level sentinels | ✅ Accepted |
| [0026](0026-reload-diagnose-gate.md) | /admin/reload is gated on Diagnose | ✅ Accepted |
| [0027](0027-invalid-rule-set-400.md) | InvalidRuleSetError ↦ 400 on /admin/reload + /admin/diagnose | ✅ Accepted |
| [0028](0028-admin-handler-otel-spans.md) | OTel spans on /admin/* handlers | ✅ Accepted |
| [0030](0030-body-based-reload.md) | Body-based /admin/reload | ✅ Accepted |
| [0031](0031-shadow-admin-surface.md) | Shadow admin surface — load and clear a challenger Decider | ✅ Accepted |
| [0032](0032-shadow-decide-execution.md) | Shadow /decide execution — run champion + challenger in parallel | ✅ Accepted |
| [0033](0033-shadow-sample-rate.md) | Shadow sample rate — operator-tunable comparison frequency | ✅ Accepted |
