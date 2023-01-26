package harness

import (
	"context"
	"testing"

	"github.com/helmedeiros/markup-svc/internal/decider/guardrails"
	"github.com/helmedeiros/markup-svc/internal/decider/indexed"
	"github.com/helmedeiros/markup-svc/internal/markup"
)

// benchRequest matches one rule in the fixture (de_t5: country=DE,
// customer_tier=t5). Same shape v0.1.0 used so deltas line up.
var benchRequest = markup.Request{Country: "DE", CustomerTier: "t5"}

var benchCtx = context.Background()

// BenchmarkDecorator measures per-Decide overhead added by the
// guardrails decorator on top of the indexed baseline. Three sub-benchmarks:
//
//   - indexed-baseline: the bare indexed adapter. Reproduces v0.1.0's
//     BenchmarkAdapter/indexed within measurement noise so the delta
//     against the guardrails rows is the pure decorator overhead.
//   - guardrails-zero-rules: guardrails.New(indexed) with no Rules. The
//     decorator code is exercised (the wrapper sits in the call path)
//     but the rules loop is empty. Proves the "no rules → no work"
//     property the cmd flag guard relies on; cost should match
//     baseline within 2σ.
//   - guardrails-three-rules: guardrails.New(indexed, FactorRange,
//     AllowedCountries, RequiredFields) with the same configuration
//     the cookbook recipe demonstrates. The realistic "operator
//     wired all three flags" production cost.
func BenchmarkDecorator(b *testing.B) {
	rules := loadFixture(b)
	baseline, err := indexed.NewFromRules(rules, "bench")
	if err != nil {
		b.Fatalf("indexed.NewFromRules: %v", err)
	}

	b.Run("indexed-baseline", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_, _ = baseline.Decide(benchCtx, benchRequest)
		}
	})

	b.Run("guardrails-zero-rules", func(b *testing.B) {
		d := guardrails.New(baseline)
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_, _ = d.Decide(benchCtx, benchRequest)
		}
	})

	b.Run("guardrails-three-rules", func(b *testing.B) {
		d := guardrails.New(
			baseline,
			guardrails.FactorRange{Min: 0.5, Max: 3.0},
			guardrails.AllowedCountries{Countries: []string{"BR", "DE", "FR"}},
			guardrails.RequiredFields{Fields: []string{"country", "customer_tier"}},
		)
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_, _ = d.Decide(benchCtx, benchRequest)
		}
	})
}
