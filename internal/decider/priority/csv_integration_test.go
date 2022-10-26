package priority_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/helmedeiros/markup-svc/internal/decider/firstmatch"
	"github.com/helmedeiros/markup-svc/internal/decider/priority"
	"github.com/helmedeiros/markup-svc/internal/load"
	"github.com/helmedeiros/markup-svc/internal/markup"
)

// orderedCSV: broad_country is inserted first but specific_tier has
// higher Priority. priority.Decider must pick specific_tier.
const orderedCSV = `name,condition,factor,priority
broad_country,country == 'BR',1.05,5
specific_tier,customer_tier == 'enterprise',1.15,10
`

// equalPriorityCSV: same rule set as the firstmatch integration test
// but with all priorities = 0. priority.Decider must match
// firstmatch.Decider on every Request.
const equalPriorityCSV = `name,condition,factor,priority
broad_country,country == 'BR',1.05,0
specific_tier,customer_tier == 'enterprise',1.15,0
`

func deciderFromCSV(t *testing.T, csv string) *priority.Decider {
	t.Helper()
	rules, err := load.FromCSV(strings.NewReader(csv))
	if err != nil {
		t.Fatalf("load.FromCSV: %v", err)
	}
	d, err := priority.NewFromRules(rules, "csv-pr")
	if err != nil {
		t.Fatalf("priority.NewFromRules: %v", err)
	}
	return d
}

func TestNewFromRulesEndToEndPriorityOrdering(t *testing.T) {
	d := deciderFromCSV(t, orderedCSV)
	got, err := d.Decide(context.Background(), markup.Request{
		Country:      "BR",
		CustomerTier: "enterprise",
	})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if got.Rule != "specific_tier" || got.MarkupFactor != 1.15 {
		t.Fatalf("Decision = %+v, want Rule=specific_tier Factor=1.15", got)
	}
}

func TestNewFromRulesEndToEndNoMatch(t *testing.T) {
	d := deciderFromCSV(t, orderedCSV)
	_, err := d.Decide(context.Background(), markup.Request{
		Country:      "DE",
		CustomerTier: "consumer",
	})
	if !errors.Is(err, markup.ErrNoMatch) {
		t.Fatalf("want ErrNoMatch, got %v", err)
	}
}

// TestSemanticDifferenceFromFirstmatch pins the ADR-0005 promise: the
// same CSV produces different Decisions through priority vs firstmatch
// when priorities disagree with insertion order. Without this
// divergence the priority adapter would be just firstmatch with extra
// ceremony.
func TestSemanticDifferenceFromFirstmatch(t *testing.T) {
	rules, err := load.FromCSV(strings.NewReader(orderedCSV))
	if err != nil {
		t.Fatalf("FromCSV: %v", err)
	}
	pr, err := priority.NewFromRules(rules, "v1")
	if err != nil {
		t.Fatalf("priority.NewFromRules: %v", err)
	}
	fm, err := firstmatch.NewFromRules(rules, "v1")
	if err != nil {
		t.Fatalf("firstmatch.NewFromRules: %v", err)
	}
	req := markup.Request{Country: "BR", CustomerTier: "enterprise"}

	prDecision, err := pr.Decide(context.Background(), req)
	if err != nil {
		t.Fatalf("priority Decide: %v", err)
	}
	fmDecision, err := fm.Decide(context.Background(), req)
	if err != nil {
		t.Fatalf("firstmatch Decide: %v", err)
	}

	if prDecision.Rule == fmDecision.Rule {
		t.Fatalf("expected divergence: both picked rule=%q (priority=%+v, firstmatch=%+v)",
			prDecision.Rule, prDecision, fmDecision)
	}
	if prDecision.Rule != "specific_tier" {
		t.Errorf("priority picked %q, want \"specific_tier\" (higher Priority)", prDecision.Rule)
	}
	if fmDecision.Rule != "broad_country" {
		t.Errorf("firstmatch picked %q, want \"broad_country\" (insertion-order first)", fmDecision.Rule)
	}
}

// TestPriorityZeroDegradesToFirstmatch pins the ADR-0005 promise that
// when all Priority values are equal, priority.Decider matches
// firstmatch.Decider on every Request. Together with the divergence
// test this means the adapter is both meaningfully different (when
// priorities differ) and a strict generalization (when they do not).
func TestPriorityZeroDegradesToFirstmatch(t *testing.T) {
	rules, err := load.FromCSV(strings.NewReader(equalPriorityCSV))
	if err != nil {
		t.Fatalf("FromCSV: %v", err)
	}
	pr, err := priority.NewFromRules(rules, "v1")
	if err != nil {
		t.Fatalf("priority.NewFromRules: %v", err)
	}
	fm, err := firstmatch.NewFromRules(rules, "v1")
	if err != nil {
		t.Fatalf("firstmatch.NewFromRules: %v", err)
	}

	cases := []markup.Request{
		{Country: "BR", CustomerTier: "enterprise"},
		{Country: "BR"},
		{CustomerTier: "enterprise"},
	}
	for _, req := range cases {
		prD, prErr := pr.Decide(context.Background(), req)
		fmD, fmErr := fm.Decide(context.Background(), req)
		if (prErr == nil) != (fmErr == nil) {
			t.Errorf("err mismatch for %+v: priority=%v firstmatch=%v", req, prErr, fmErr)
			continue
		}
		if prErr != nil {
			continue
		}
		if prD.Rule != fmD.Rule {
			t.Errorf("rule divergence for %+v: priority=%q firstmatch=%q (should match at equal priority)",
				req, prD.Rule, fmD.Rule)
		}
		if prD.MarkupFactor != fmD.MarkupFactor {
			t.Errorf("factor divergence for %+v: priority=%v firstmatch=%v",
				req, prD.MarkupFactor, fmD.MarkupFactor)
		}
	}
}

func BenchmarkDecideViaNewFromRules(b *testing.B) {
	rules, err := load.FromCSV(strings.NewReader(orderedCSV))
	if err != nil {
		b.Fatalf("FromCSV: %v", err)
	}
	d, err := priority.NewFromRules(rules, "csv-pr")
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
