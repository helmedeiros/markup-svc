# scientific/v0.1.19 — body-based `/admin/reload` benchmarks

Pre-registered bars + measured values for ADR-0030 (body-based `/admin/reload`). Follows the [ADR-0012](../../docs/architecture/decisions/0012-scientific-harness.md) protocol: bars are committed in this directory BEFORE measurement and do NOT move post-commit.

Four benchmarks pin the four operational shapes the ADR cares about:

- `BenchmarkReload_EmptyBody` — empty-body POST goes through the file loader (the bit-for-bit-compat canary made measurable).
- `BenchmarkReload_CSVBody_100Rules` — body-based path with a small CSV; representative of typical operator workflows.
- `BenchmarkReload_CSVBody_100kRules` — body-based path with a large CSV; the worst-case parse + index-build cost.
- `BenchmarkReload_SnapshotBody_100kRules` — body-based path with a pre-compiled snapshot; the fast lane the snapshot format exists for.

Run with:

```
go test -bench=. -benchmem -count=20 -benchtime=300ms -run=^$ ./scientific/v0.1.19/...
```

See [REPORT.md](REPORT.md) for the bars and measurements.
