package firstmatch_test

import (
	"context"
	"errors"
	"testing"

	breengine "github.com/helmedeiros/bre-go/engine"
	brefirstmatch "github.com/helmedeiros/bre-go/engine/firstmatch"

	"github.com/helmedeiros/markup-svc/internal/decider/firstmatch"
	"github.com/helmedeiros/markup-svc/internal/markup"
)

func tier(t string) func(markup.Request) bool {
	return func(r markup.Request) bool { return r.CustomerTier == t }
}

func brOnly() func(markup.Request) bool {
	return func(r markup.Request) bool { return r.Country == "BR" }
}

func TestNewWithValidRules(t *testing.T) {
	d, err := firstmatch.New([]firstmatch.Rule{
		{Name: "enterprise", Condition: tier("enterprise"), Factor: 1.10},
	}, "v1")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if d == nil {
		t.Fatal("expected non-nil Decider")
	}
}

func TestNewPropagatesAddRuleErrors(t *testing.T) {
	_, err := firstmatch.New([]firstmatch.Rule{
		{Name: "", Condition: tier("x"), Factor: 1.0},
	}, "v1")
	if !errors.Is(err, brefirstmatch.ErrEmptyRuleName) {
		t.Fatalf("want ErrEmptyRuleName, got %v", err)
	}
}

func TestNewPropagatesDuplicateNameError(t *testing.T) {
	_, err := firstmatch.New([]firstmatch.Rule{
		{Name: "r", Condition: tier("a"), Factor: 1.0},
		{Name: "r", Condition: tier("b"), Factor: 2.0},
	}, "v1")
	if !errors.Is(err, brefirstmatch.ErrDuplicateRuleName) {
		t.Fatalf("want ErrDuplicateRuleName, got %v", err)
	}
}

func TestDecideHappyPath(t *testing.T) {
	d, err := firstmatch.New([]firstmatch.Rule{
		{Name: "enterprise", Condition: tier("enterprise"), Factor: 1.15},
	}, "v1")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, err := d.Decide(context.Background(), markup.Request{CustomerTier: "enterprise"})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if got.MarkupFactor != 1.15 {
		t.Errorf("MarkupFactor = %v, want 1.15", got.MarkupFactor)
	}
	if got.Rule != "enterprise" {
		t.Errorf("Rule = %q, want \"enterprise\"", got.Rule)
	}
	if got.EngineAdapter != "*firstmatch.Engine" {
		t.Errorf("EngineAdapter = %q, want \"*firstmatch.Engine\"", got.EngineAdapter)
	}
}

func TestDecideNoMatchReturnsErrNoMatch(t *testing.T) {
	d, err := firstmatch.New([]firstmatch.Rule{
		{Name: "enterprise", Condition: tier("enterprise"), Factor: 1.15},
	}, "v1")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, err := d.Decide(context.Background(), markup.Request{CustomerTier: "consumer"})
	if !errors.Is(err, markup.ErrNoMatch) {
		t.Fatalf("want ErrNoMatch, got %v", err)
	}
	if (got != markup.Decision{}) {
		t.Fatalf("want zero Decision, got %+v", got)
	}
}

// TestDecideFirstMatchWinsOnMultipleMatches is the load-bearing test
// for ADR-0004: firstmatch must select the FIRST matching rule in
// insertion order, regardless of whether later rules would also match.
// This is the semantic that distinguishes the adapter from inmemory.
func TestDecideFirstMatchWinsOnMultipleMatches(t *testing.T) {
	d, err := firstmatch.New([]firstmatch.Rule{
		{Name: "broad_country", Condition: brOnly(), Factor: 1.05},
		{Name: "specific_tier", Condition: tier("enterprise"), Factor: 1.15},
	}, "v1")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, err := d.Decide(context.Background(), markup.Request{Country: "BR", CustomerTier: "enterprise"})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if got.Rule != "broad_country" || got.MarkupFactor != 1.05 {
		t.Fatalf("first-match-wins broken: got %+v (want Rule=broad_country, Factor=1.05)", got)
	}
}

func TestDecidePopulatesCorrelationID(t *testing.T) {
	d, err := firstmatch.New([]firstmatch.Rule{
		{Name: "always", Condition: func(markup.Request) bool { return true }, Factor: 1.0},
	}, "v1")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := breengine.WithCorrelationID(context.Background(), "trace-fm-1")
	got, err := d.Decide(ctx, markup.Request{})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if got.CorrelationID != "trace-fm-1" {
		t.Errorf("CorrelationID = %q, want \"trace-fm-1\"", got.CorrelationID)
	}
}
