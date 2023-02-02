package harness

import (
	"context"
	"testing"

	"github.com/helmedeiros/markup-svc/internal/decider/guardrails"
	"github.com/helmedeiros/markup-svc/internal/decider/indexed"
	"github.com/helmedeiros/markup-svc/internal/markup"
)

// benchRequest matches one rule in the fixture (de_t5: country=DE,
// customer_tier=t5). Same shape v0.1.0 and v0.1.3 used so deltas
// across releases line up on the same fixture row.
var benchRequest = markup.Request{Country: "DE", CustomerTier: "t5"}

var benchCtx = context.Background()

// BenchmarkDecorator measures per-Decide overhead added by the
// guardrails.Holder on top of the indexed baseline.
//
// Three sub-benchmarks under one parent so a single pass interleaves
// them and host noise hits every row equally:
//
//   - indexed-baseline: the bare indexed adapter. Reproduces
//     v0.1.0/v0.1.3 baselines within measurement noise so the delta
//     against the Holder rows is computed in-pass.
//   - guardrails-holder-zero-rules: NewHolder().Wrap(indexed). Pays
//     the RLock/RUnlock pair around an empty rules slice walk;
//     proves the no-config overhead is the pure lock-pair cost.
//   - guardrails-holder-three-rules: NewHolder(FactorRange,
//     AllowedCountries, RequiredFields).Wrap(indexed) with the same
//     configuration the cookbook recipe demonstrates. The realistic
//     production cost when an operator runs --guardrails-admin with
//     the three boot flags pre-populated.
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

	b.Run("guardrails-holder-zero-rules", func(b *testing.B) {
		d := guardrails.NewHolder().Wrap(baseline)
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_, _ = d.Decide(benchCtx, benchRequest)
		}
	})

	b.Run("guardrails-holder-three-rules", func(b *testing.B) {
		d := guardrails.NewHolder(
			guardrails.FactorRange{Min: 0.5, Max: 3.0},
			guardrails.AllowedCountries{Countries: []string{"BR", "DE", "FR"}},
			guardrails.RequiredFields{Fields: []string{"country", "customer_tier"}},
		).Wrap(baseline)
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_, _ = d.Decide(benchCtx, benchRequest)
		}
	})
}

// BenchmarkReplace measures the admin-call cost. Replace allocates
// one new []Rule backing array per call; at admin-call rates
// (handful per day in practice) the allocation is invisible.
// The bar exists so a regression that makes Replace allocate the
// rules slice repeatedly, or makes it block on something other than
// the write lock, would surface.
func BenchmarkReplace(b *testing.B) {
	rules := []guardrails.Rule{
		guardrails.FactorRange{Min: 0.5, Max: 3.0},
		guardrails.AllowedCountries{Countries: []string{"BR", "DE", "FR"}},
		guardrails.RequiredFields{Fields: []string{"country", "customer_tier"}},
	}
	h := guardrails.NewHolder(rules...)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		h.Replace(rules)
	}
}
