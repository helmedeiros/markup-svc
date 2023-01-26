# scientific/v0.1.3 — guardrails decorator overhead

Pre-registered bars + measurement (in a follow-up commit) for the guardrails decorator landed in ADR-0014. Three sub-benchmarks under one parent so a single pass interleaves them:

- `BenchmarkDecorator/indexed-baseline` — the indexed adapter alone; reproduces v0.1.0's measured baseline so the delta against the guardrails rows is computed in-pass.
- `BenchmarkDecorator/guardrails-zero-rules` — guardrails wrapper mounted with no Rules. Pins the "no-work" overhead of the wrapper layer.
- `BenchmarkDecorator/guardrails-three-rules` — `FactorRange` + `AllowedCountries(BR,DE,FR)` + `RequiredFields(country, customer_tier)`, the realistic production configuration the cookbook recipe demonstrates.

To reproduce:

```sh
go test -bench=. -benchmem -count=50 -benchtime=1s -run=^$ ./scientific/v0.1.3/...
```

See [REPORT.md](REPORT.md) for bars + analysis. See [ADR-0012](../../docs/architecture/decisions/0012-scientific-harness.md) for the methodology.
