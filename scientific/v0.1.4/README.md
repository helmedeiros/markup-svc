# scientific/v0.1.4 — guardrails.Holder + Replace overhead

Pre-registered bars + measurement (in a follow-up commit) for the guardrails.Holder from ADR-0015. Three sub-benchmarks under `BenchmarkDecorator` interleave per pass:

- `BenchmarkDecorator/indexed-baseline` — the indexed adapter alone; reproduces v0.1.3's baseline so the delta against the Holder rows is computed in-pass.
- `BenchmarkDecorator/guardrails-holder-zero-rules` — `NewHolder().Wrap(indexed)`. Pins the lock-pair + empty-loop overhead.
- `BenchmarkDecorator/guardrails-holder-three-rules` — `NewHolder(...).Wrap(indexed)` with FactorRange + AllowedCountries(BR,DE,FR) + RequiredFields(country, customer_tier). The realistic `--guardrails-admin` production cost.

Plus `BenchmarkReplace` measuring the admin-call cost.

To reproduce:

```sh
go test -bench=. -benchmem -count=20 -benchtime=300ms -run=^$ ./scientific/v0.1.4/...
```

See [REPORT.md](REPORT.md) for bars + analysis. See [ADR-0012](../../docs/architecture/decisions/0012-scientific-harness.md) for the methodology.
