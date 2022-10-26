package priority_test

import (
	"context"
	"errors"
	"testing"

	breengine "github.com/helmedeiros/bre-go/engine"
	brepriority "github.com/helmedeiros/bre-go/engine/priority"

	"github.com/helmedeiros/markup-svc/internal/decider/priority"
	"github.com/helmedeiros/markup-svc/internal/markup"
)

func tier(t string) func(markup.Request) bool {
	return func(r markup.Request) bool { return r.CustomerTier == t }
}

func country(c string) func(markup.Request) bool {
	return func(r markup.Request) bool { return r.Country == c }
}

func TestNewWithValidRules(t *testing.T) {
	d, err := priority.New([]priority.Rule{
		{Name: "enterprise", Condition: tier("enterprise"), Factor: 1.10, Priority: 5},
	}, "v1")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if d == nil {
		t.Fatal("expected non-nil Decider")
	}
}

func TestNewPropagatesEmptyName(t *testing.T) {
	_, err := priority.New([]priority.Rule{
		{Name: "", Condition: tier("x"), Factor: 1.0, Priority: 0},
	}, "v1")
	if !errors.Is(err, brepriority.ErrEmptyRuleName) {
		t.Fatalf("want ErrEmptyRuleName, got %v", err)
	}
}

func TestNewPropagatesDuplicateName(t *testing.T) {
	_, err := priority.New([]priority.Rule{
		{Name: "r", Condition: tier("a"), Factor: 1.0, Priority: 0},
		{Name: "r", Condition: tier("b"), Factor: 2.0, Priority: 0},
	}, "v1")
	if !errors.Is(err, brepriority.ErrDuplicateRuleName) {
		t.Fatalf("want ErrDuplicateRuleName, got %v", err)
	}
}

func TestDecideHappyPath(t *testing.T) {
	d, err := priority.New([]priority.Rule{
		{Name: "enterprise", Condition: tier("enterprise"), Factor: 1.15, Priority: 5},
	}, "v1")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, err := d.Decide(context.Background(), markup.Request{CustomerTier: "enterprise"})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if got.MarkupFactor != 1.15 || got.Rule != "enterprise" {
		t.Errorf("Decision = %+v, want Rule=enterprise Factor=1.15", got)
	}
	if got.EngineAdapter != "*priority.Engine" {
		t.Errorf("EngineAdapter = %q, want \"*priority.Engine\"", got.EngineAdapter)
	}
}

func TestDecideNoMatchReturnsErrNoMatch(t *testing.T) {
	d, err := priority.New([]priority.Rule{
		{Name: "enterprise", Condition: tier("enterprise"), Factor: 1.15, Priority: 5},
	}, "v1")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = d.Decide(context.Background(), markup.Request{CustomerTier: "consumer"})
	if !errors.Is(err, markup.ErrNoMatch) {
		t.Fatalf("want ErrNoMatch, got %v", err)
	}
}

// TestDecideHigherPriorityWinsOverInsertionOrder is the load-bearing
// test for ADR-0005: when a higher-Priority rule is registered AFTER
// a lower-Priority rule, the higher-Priority one still wins. That is
// the property that distinguishes priority from firstmatch: insertion
// order does NOT determine precedence when priorities differ.
func TestDecideHigherPriorityWinsOverInsertionOrder(t *testing.T) {
	d, err := priority.New([]priority.Rule{
		{Name: "broad_country", Condition: country("BR"), Factor: 1.05, Priority: 5},
		{Name: "specific_tier", Condition: tier("enterprise"), Factor: 1.15, Priority: 10},
	}, "v1")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, err := d.Decide(context.Background(), markup.Request{Country: "BR", CustomerTier: "enterprise"})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if got.Rule != "specific_tier" || got.MarkupFactor != 1.15 {
		t.Fatalf("priority-over-insertion broken: got %+v (want Rule=specific_tier Factor=1.15)", got)
	}
}

// TestDecideTiesBreakByInsertionOrder pins the documented tie-break:
// equal-priority rules behave like firstmatch in the order they were
// added, which is the property that makes the adapter degenerate
// gracefully when priorities are unset.
func TestDecideTiesBreakByInsertionOrder(t *testing.T) {
	d, err := priority.New([]priority.Rule{
		{Name: "first_added", Condition: country("BR"), Factor: 1.05, Priority: 5},
		{Name: "second_added", Condition: tier("enterprise"), Factor: 1.15, Priority: 5},
	}, "v1")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, err := d.Decide(context.Background(), markup.Request{Country: "BR", CustomerTier: "enterprise"})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if got.Rule != "first_added" {
		t.Fatalf("equal-priority tie-break broken: got %q, want \"first_added\"", got.Rule)
	}
}

func TestDecidePopulatesCorrelationID(t *testing.T) {
	d, err := priority.New([]priority.Rule{
		{Name: "always", Condition: func(markup.Request) bool { return true }, Factor: 1.0, Priority: 0},
	}, "v1")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := breengine.WithCorrelationID(context.Background(), "trace-pr-1")
	got, err := d.Decide(ctx, markup.Request{})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if got.CorrelationID != "trace-pr-1" {
		t.Errorf("CorrelationID = %q, want \"trace-pr-1\"", got.CorrelationID)
	}
}
