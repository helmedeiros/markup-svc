# scientific/v0.1.0 — performance comparison harness

Pre-registered, Docker-reproduced benchmarks for the v0.1.0 release. See [ADR-0012](../../docs/architecture/decisions/0012-scientific-harness.md) for the methodology.

## What this harness measures

| Benchmark | What |
|---|---|
| `BenchmarkAdapter/{inmemory,firstmatch,priority,indexed}` | Per-`Decide` latency per adapter on `fixture.csv` |
| `BenchmarkDecorator/{swap,otel,metrics,full-stack}` | Decorator overhead at each layer over the baseline `indexed` adapter |
| `BenchmarkColdStart/{rules,snapshot}` | Cold start cost on the same rule set: parse CSV + build vs read snapshot + build |
| `BenchmarkRouter/single-route` | Router with one route + DefaultPolicy over the baseline adapter |

The fixture is a 50-rule CSV where each rule discriminates by `(country, customer_tier)`. The test Request matches exactly one rule, which is the realistic operational shape.

## How to run

The Docker image pins Linux/amd64 + Go 1.18 + the fixture so any machine that can run Docker produces comparable numbers:

```sh
make scientific-v0.1.0
```

Which is equivalent to:

```sh
docker build -t markup-svc-scientific-v0.1.0 -f scientific/v0.1.0/Dockerfile .
docker run --rm markup-svc-scientific-v0.1.0
```

The container runs `go test -bench=. -benchmem -count=50 -benchtime=1s ./scientific/v0.1.0/...` and prints the table. Trial-mean methodology: each of the 50 trials is one 1-second `-benchtime` run; the mean reported is across the 50 trial means; the std dev is across the 50 trial means.

## How to read the results

Two flavours of pre-registered bar:

- **Ordinal**: claims about ordering (e.g., `indexed.Decide ≤ firstmatch.Decide`). Passes when the predicted order holds by > 2 pooled SE of the difference (~95% confidence).
- **Absolute**: claims about magnitude (e.g., `full-stack overhead ≤ X ns`). Bars are set from a pilot run with headroom (pilot mean + 2σ).

Bars are committed in [`REPORT.md`](REPORT.md) before measurement. **They do not move after they are committed.** A failed bar is reported honestly in the analysis; the next release picks up the optimization.

## What this harness does NOT measure (out of scope for v0.1.0)

- Concurrent / multi-goroutine throughput (qps under load)
- Memory profile aggregation across packages
- Cross-version drift detection (next release reuses these bars)
- CI gating on benchmark results
