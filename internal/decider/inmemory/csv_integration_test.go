package inmemory_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/helmedeiros/markup-svc/internal/decider/inmemory"
	"github.com/helmedeiros/markup-svc/internal/load"
	"github.com/helmedeiros/markup-svc/internal/markup"
)

const csvRules = `name,condition,factor,priority
enterprise,customer_tier == 'enterprise',1.15,10
br_peak,country == 'BR' AND time_window == 'peak',1.08,5
`

func deciderFromCSV(t *testing.T, csv string) *inmemory.Decider {
	t.Helper()
	rules, err := load.FromCSV(strings.NewReader(csv))
	if err != nil {
		t.Fatalf("load.FromCSV: %v", err)
	}
	d, err := inmemory.NewFromRules(rules, "csv-v1")
	if err != nil {
		t.Fatalf("inmemory.NewFromRules: %v", err)
	}
	return d
}

func TestNewFromRulesEndToEndEnterpriseMatches(t *testing.T) {
	d := deciderFromCSV(t, csvRules)
	got, err := d.Decide(context.Background(), markup.Request{
		CustomerTier: "enterprise",
		Country:      "DE",
	})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if got.Rule != "enterprise" || got.MarkupFactor != 1.15 {
		t.Fatalf("Decision = %+v, want Rule=enterprise Factor=1.15", got)
	}
	if got.ModelVersion != "csv-v1" {
		t.Errorf("ModelVersion = %q, want \"csv-v1\"", got.ModelVersion)
	}
}

func TestNewFromRulesEndToEndAndConditionMatches(t *testing.T) {
	d := deciderFromCSV(t, csvRules)
	got, err := d.Decide(context.Background(), markup.Request{
		Country:    "BR",
		TimeWindow: "peak",
	})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if got.Rule != "br_peak" || got.MarkupFactor != 1.08 {
		t.Fatalf("Decision = %+v, want Rule=br_peak Factor=1.08", got)
	}
}

func TestNewFromRulesEndToEndNoMatchReturnsErrNoMatch(t *testing.T) {
	d := deciderFromCSV(t, csvRules)
	_, err := d.Decide(context.Background(), markup.Request{
		CustomerTier: "consumer",
		Country:      "DE",
		TimeWindow:   "off",
	})
	if !errors.Is(err, markup.ErrNoMatch) {
		t.Fatalf("want ErrNoMatch, got %v", err)
	}
}

func TestNewFromRulesEndToEndLastMatchWins(t *testing.T) {
	d := deciderFromCSV(t, csvRules)
	got, err := d.Decide(context.Background(), markup.Request{
		CustomerTier: "enterprise",
		Country:      "BR",
		TimeWindow:   "peak",
	})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if got.Rule != "br_peak" || got.MarkupFactor != 1.08 {
		t.Fatalf("Decision = %+v, want last-matched br_peak with Factor=1.08", got)
	}
}

func TestNewFromRulesPropagatesAddRuleError(t *testing.T) {
	bad := `name,condition,factor,priority
,country == 'BR',1.0,0
`
	rules, err := load.FromCSV(strings.NewReader(bad))
	if err != nil {
		t.Fatalf("FromCSV: %v", err)
	}
	_, err = inmemory.NewFromRules(rules, "csv-v1")
	if err == nil {
		t.Fatal("want error for empty rule name, got nil")
	}
}

func TestNewFromRulesEmptyRulesYieldsErrNoMatch(t *testing.T) {
	d, err := inmemory.NewFromRules(nil, "csv-v1")
	if err != nil {
		t.Fatalf("NewFromRules: %v", err)
	}
	_, err = d.Decide(context.Background(), markup.Request{})
	if !errors.Is(err, markup.ErrNoMatch) {
		t.Fatalf("want ErrNoMatch, got %v", err)
	}
}

func BenchmarkDecideViaNewFromRules(b *testing.B) {
	rules, err := load.FromCSV(strings.NewReader(csvRules))
	if err != nil {
		b.Fatalf("FromCSV: %v", err)
	}
	d, err := inmemory.NewFromRules(rules, "csv-v1")
	if err != nil {
		b.Fatalf("NewFromRules: %v", err)
	}
	req := markup.Request{CustomerTier: "enterprise", Country: "BR", TimeWindow: "peak"}
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = d.Decide(ctx, req)
	}
}
