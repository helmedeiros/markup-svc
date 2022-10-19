package firstmatch_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/helmedeiros/markup-svc/internal/decider/firstmatch"
	"github.com/helmedeiros/markup-svc/internal/decider/inmemory"
	"github.com/helmedeiros/markup-svc/internal/load"
	"github.com/helmedeiros/markup-svc/internal/markup"
)

const csvRules = `name,condition,factor,priority
broad_country,country == 'BR',1.05,0
specific_tier,customer_tier == 'enterprise',1.15,0
`

func deciderFromCSV(t *testing.T, csv string) *firstmatch.Decider {
	t.Helper()
	rules, err := load.FromCSV(strings.NewReader(csv))
	if err != nil {
		t.Fatalf("load.FromCSV: %v", err)
	}
	d, err := firstmatch.NewFromRules(rules, "csv-fm")
	if err != nil {
		t.Fatalf("firstmatch.NewFromRules: %v", err)
	}
	return d
}

func TestNewFromRulesEndToEndFirstMatchWins(t *testing.T) {
	d := deciderFromCSV(t, csvRules)
	got, err := d.Decide(context.Background(), markup.Request{
		Country:      "BR",
		CustomerTier: "enterprise",
	})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if got.Rule != "broad_country" || got.MarkupFactor != 1.05 {
		t.Fatalf("Decision = %+v, want Rule=broad_country Factor=1.05", got)
	}
	if got.ModelVersion != "csv-fm" {
		t.Errorf("ModelVersion = %q, want \"csv-fm\"", got.ModelVersion)
	}
}

func TestNewFromRulesEndToEndNoMatch(t *testing.T) {
	d := deciderFromCSV(t, csvRules)
	_, err := d.Decide(context.Background(), markup.Request{
		Country:      "DE",
		CustomerTier: "consumer",
	})
	if !errors.Is(err, markup.ErrNoMatch) {
		t.Fatalf("want ErrNoMatch, got %v", err)
	}
}

// TestSemanticDifferenceFromInmemory pins the ADR-0004 promise: the
// same CSV rule set produces different Decisions through firstmatch
// vs inmemory. Without this divergence the adapter axis is purely
// cosmetic; with it, the (rules x adapter) observability slice is
// real.
func TestSemanticDifferenceFromInmemory(t *testing.T) {
	rules, err := load.FromCSV(strings.NewReader(csvRules))
	if err != nil {
		t.Fatalf("FromCSV: %v", err)
	}
	fm, err := firstmatch.NewFromRules(rules, "v1")
	if err != nil {
		t.Fatalf("firstmatch.NewFromRules: %v", err)
	}
	im, err := inmemory.NewFromRules(rules, "v1")
	if err != nil {
		t.Fatalf("inmemory.NewFromRules: %v", err)
	}
	req := markup.Request{Country: "BR", CustomerTier: "enterprise"}

	fmDecision, err := fm.Decide(context.Background(), req)
	if err != nil {
		t.Fatalf("firstmatch Decide: %v", err)
	}
	imDecision, err := im.Decide(context.Background(), req)
	if err != nil {
		t.Fatalf("inmemory Decide: %v", err)
	}

	if fmDecision.Rule == imDecision.Rule {
		t.Fatalf("expected divergence: both adapters returned Rule=%q (firstmatch=%+v, inmemory=%+v)",
			fmDecision.Rule, fmDecision, imDecision)
	}
	if fmDecision.Rule != "broad_country" {
		t.Errorf("firstmatch picked %q, want \"broad_country\" (insertion-order first)", fmDecision.Rule)
	}
	if imDecision.Rule != "specific_tier" {
		t.Errorf("inmemory picked %q, want \"specific_tier\" (slice-order last)", imDecision.Rule)
	}
	if fmDecision.EngineAdapter == imDecision.EngineAdapter {
		t.Errorf("EngineAdapter should differ: both = %q", fmDecision.EngineAdapter)
	}
}

func BenchmarkDecideViaNewFromRules(b *testing.B) {
	rules, err := load.FromCSV(strings.NewReader(csvRules))
	if err != nil {
		b.Fatalf("FromCSV: %v", err)
	}
	d, err := firstmatch.NewFromRules(rules, "csv-fm")
	if err != nil {
		b.Fatalf("NewFromRules: %v", err)
	}
	req := markup.Request{Country: "BR", CustomerTier: "enterprise"}
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = d.Decide(ctx, req)
	}
}
