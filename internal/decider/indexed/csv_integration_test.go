package indexed_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/helmedeiros/markup-svc/internal/decider/firstmatch"
	"github.com/helmedeiros/markup-svc/internal/decider/indexed"
	"github.com/helmedeiros/markup-svc/internal/load"
	"github.com/helmedeiros/markup-svc/internal/markup"
)

// indexableCSV uses only OpEq / AND-of-OpEqs so every row is
// indexable. Both indexed and firstmatch should return identical
// Decisions for every Request.
const indexableCSV = `name,condition,factor,priority
de_enterprise,country == 'DE' AND customer_tier == 'enterprise',1.20,0
br_consumer,country == 'BR' AND customer_tier == 'consumer',1.05,0
fr_any,country == 'FR',1.10,0
de_consumer,country == 'DE' AND customer_tier == 'consumer',1.02,0
`

func TestNewFromRulesEndToEnd(t *testing.T) {
	rules, err := load.FromCSV(strings.NewReader(indexableCSV))
	if err != nil {
		t.Fatalf("FromCSV: %v", err)
	}
	d, err := indexed.NewFromRules(rules, "csv-ix")
	if err != nil {
		t.Fatalf("NewFromRules: %v", err)
	}
	got, err := d.Decide(context.Background(), markup.Request{
		Country:      "FR",
		CustomerTier: "enterprise",
	})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if got.Rule != "fr_any" || got.MarkupFactor != 1.10 {
		t.Fatalf("Decision = %+v, want Rule=fr_any Factor=1.10", got)
	}
	if got.ModelVersion != "csv-ix" {
		t.Errorf("ModelVersion = %q, want \"csv-ix\"", got.ModelVersion)
	}
}

func TestNewFromRulesEndToEndNoMatch(t *testing.T) {
	rules, err := load.FromCSV(strings.NewReader(indexableCSV))
	if err != nil {
		t.Fatalf("FromCSV: %v", err)
	}
	d, err := indexed.NewFromRules(rules, "csv-ix")
	if err != nil {
		t.Fatalf("NewFromRules: %v", err)
	}
	_, err = d.Decide(context.Background(), markup.Request{
		Country:      "ZZ",
		CustomerTier: "consumer",
	})
	if !errors.Is(err, markup.ErrNoMatch) {
		t.Fatalf("want ErrNoMatch, got %v", err)
	}
}

// TestNewFromRulesRejectsNonIndexableCondition pins the fail-fast
// promise of ADR-0006: a rule whose Condition has no indexable
// positive term surfaces an error at NewFromRules time, not at
// Decide time. Matches the construction-time error posture every
// other adapter follows.
func TestNewFromRulesRejectsNonIndexableCondition(t *testing.T) {
	// Pure-negation rule -- no indexable positive term.
	const bad = `name,condition,factor,priority
neg_only,NOT country == 'XX',1.0,0
`
	rules, err := load.FromCSV(strings.NewReader(bad))
	if err != nil {
		t.Fatalf("FromCSV: %v", err)
	}
	_, err = indexed.NewFromRules(rules, "v1")
	if err == nil {
		t.Fatal("want error for non-indexable condition, got nil")
	}
	if !strings.Contains(err.Error(), "neg_only") {
		t.Errorf("error %q should name the offending rule", err.Error())
	}
}

// TestSemanticEquivalenceWithFirstmatch is the load-bearing test for
// ADR-0006: for the same CSV with only indexable conditions, the
// indexed adapter must return the same Decision (Rule + MarkupFactor)
// as the firstmatch adapter on every Request in a representative
// matrix. The indexed adapter is a sub-linear optimization; if it
// disagrees with the well-understood firstmatch baseline the
// optimization is wrong by construction.
func TestSemanticEquivalenceWithFirstmatch(t *testing.T) {
	rules, err := load.FromCSV(strings.NewReader(indexableCSV))
	if err != nil {
		t.Fatalf("FromCSV: %v", err)
	}
	ix, err := indexed.NewFromRules(rules, "v1")
	if err != nil {
		t.Fatalf("indexed.NewFromRules: %v", err)
	}
	fm, err := firstmatch.NewFromRules(rules, "v1")
	if err != nil {
		t.Fatalf("firstmatch.NewFromRules: %v", err)
	}

	cases := []markup.Request{
		{Country: "DE", CustomerTier: "enterprise"}, // matches de_enterprise
		{Country: "BR", CustomerTier: "consumer"},   // matches br_consumer
		{Country: "FR", CustomerTier: "enterprise"}, // matches fr_any
		{Country: "FR", CustomerTier: "consumer"},   // matches fr_any
		{Country: "DE", CustomerTier: "consumer"},   // matches de_consumer
		{Country: "ZZ", CustomerTier: "consumer"},   // no match
		{Country: "DE"},                             // no match (consumer/enterprise rules require tier)
	}
	for i, req := range cases {
		ixD, ixErr := ix.Decide(context.Background(), req)
		fmD, fmErr := fm.Decide(context.Background(), req)
		if (ixErr == nil) != (fmErr == nil) {
			t.Errorf("[%d] err mismatch for %+v: indexed=%v firstmatch=%v", i, req, ixErr, fmErr)
			continue
		}
		if ixErr != nil {
			continue
		}
		if ixD.Rule != fmD.Rule {
			t.Errorf("[%d] rule divergence for %+v: indexed=%q firstmatch=%q",
				i, req, ixD.Rule, fmD.Rule)
		}
		if ixD.MarkupFactor != fmD.MarkupFactor {
			t.Errorf("[%d] factor divergence for %+v: indexed=%v firstmatch=%v",
				i, req, ixD.MarkupFactor, fmD.MarkupFactor)
		}
	}
}

func BenchmarkDecideViaNewFromRulesLarge(b *testing.B) {
	// Build a 50-row CSV across 5 countries x 10 customer tiers.
	var sb strings.Builder
	sb.WriteString("name,condition,factor,priority\n")
	countries := []string{"DE", "BR", "FR", "IT", "ES"}
	tiers := []string{"t0", "t1", "t2", "t3", "t4", "t5", "t6", "t7", "t8", "t9"}
	for _, c := range countries {
		for _, t := range tiers {
			fmt.Fprintf(&sb, "%s_%s,country == '%s' AND customer_tier == '%s',1.05,0\n", c, t, c, t)
		}
	}
	rules, err := load.FromCSV(strings.NewReader(sb.String()))
	if err != nil {
		b.Fatalf("FromCSV: %v", err)
	}
	d, err := indexed.NewFromRules(rules, "v-bench")
	if err != nil {
		b.Fatalf("NewFromRules: %v", err)
	}
	req := markup.Request{Country: "IT", CustomerTier: "t7"}
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = d.Decide(ctx, req)
	}
}
