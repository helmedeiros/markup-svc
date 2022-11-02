package indexed_test

import (
	"context"
	"errors"
	"testing"

	breengine "github.com/helmedeiros/bre-go/engine"
	breindexed "github.com/helmedeiros/bre-go/engine/indexed"
	"github.com/helmedeiros/bre-go/engine/parser"

	"github.com/helmedeiros/markup-svc/internal/decider/indexed"
	"github.com/helmedeiros/markup-svc/internal/markup"
)

func eqCond(field, value string) parser.Condition {
	return parser.StringCondition{Field: field, Op: parser.OpEq, Value: value}
}

func TestNewWithValidRules(t *testing.T) {
	d, err := indexed.New([]indexed.Rule{
		{Name: "enterprise", Match: eqCond("customer_tier", "enterprise"), Factor: 1.15},
	}, "v1")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if d == nil {
		t.Fatal("expected non-nil Decider")
	}
}

func TestNewPropagatesEmptyName(t *testing.T) {
	_, err := indexed.New([]indexed.Rule{
		{Name: "", Match: eqCond("country", "BR"), Factor: 1.0},
	}, "v1")
	if !errors.Is(err, breindexed.ErrEmptyRuleName) {
		t.Fatalf("want ErrEmptyRuleName, got %v", err)
	}
}

func TestNewPropagatesNilMatch(t *testing.T) {
	_, err := indexed.New([]indexed.Rule{
		{Name: "r", Match: nil, Factor: 1.0},
	}, "v1")
	if !errors.Is(err, breindexed.ErrNilMatch) {
		t.Fatalf("want ErrNilMatch, got %v", err)
	}
}

func TestNewPropagatesDuplicateName(t *testing.T) {
	_, err := indexed.New([]indexed.Rule{
		{Name: "r", Match: eqCond("country", "BR"), Factor: 1.0},
		{Name: "r", Match: eqCond("country", "DE"), Factor: 2.0},
	}, "v1")
	if !errors.Is(err, breindexed.ErrDuplicateRuleName) {
		t.Fatalf("want ErrDuplicateRuleName, got %v", err)
	}
}

func TestNewRejectsNonIndexableCondition(t *testing.T) {
	// Pure negation: no positive indexable term, must fail at AddRule.
	pureNeg := parser.NotCondition{Child: eqCond("country", "BR")}
	_, err := indexed.New([]indexed.Rule{
		{Name: "neg", Match: pureNeg, Factor: 1.0},
	}, "v1")
	if err == nil {
		t.Fatal("want non-indexable error, got nil")
	}
}

func TestDecideHappyPath(t *testing.T) {
	d, err := indexed.New([]indexed.Rule{
		{Name: "enterprise", Match: eqCond("customer_tier", "enterprise"), Factor: 1.15},
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
	if got.EngineAdapter != "*indexed.Engine" {
		t.Errorf("EngineAdapter = %q, want \"*indexed.Engine\"", got.EngineAdapter)
	}
}

func TestDecideNoMatchReturnsErrNoMatch(t *testing.T) {
	d, err := indexed.New([]indexed.Rule{
		{Name: "enterprise", Match: eqCond("customer_tier", "enterprise"), Factor: 1.15},
	}, "v1")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = d.Decide(context.Background(), markup.Request{CustomerTier: "consumer"})
	if !errors.Is(err, markup.ErrNoMatch) {
		t.Fatalf("want ErrNoMatch, got %v", err)
	}
}

// TestDecideFirstMatchByInsertionOrder pins that the indexed adapter
// shares firstmatch semantics: when multiple rules match, the first
// rule registered wins, regardless of bucket structure.
func TestDecideFirstMatchByInsertionOrder(t *testing.T) {
	d, err := indexed.New([]indexed.Rule{
		{Name: "broad_country", Match: eqCond("country", "BR"), Factor: 1.05},
		{Name: "specific_tier", Match: eqCond("customer_tier", "enterprise"), Factor: 1.15},
	}, "v1")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, err := d.Decide(context.Background(), markup.Request{Country: "BR", CustomerTier: "enterprise"})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if got.Rule != "broad_country" || got.MarkupFactor != 1.05 {
		t.Fatalf("first-match-by-insertion broken: got %+v (want Rule=broad_country Factor=1.05)", got)
	}
}

func TestDecidePopulatesCorrelationID(t *testing.T) {
	d, err := indexed.New([]indexed.Rule{
		{Name: "always_br", Match: eqCond("country", "BR"), Factor: 1.0},
	}, "v1")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := breengine.WithCorrelationID(context.Background(), "trace-ix-1")
	got, err := d.Decide(ctx, markup.Request{Country: "BR"})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if got.CorrelationID != "trace-ix-1" {
		t.Errorf("CorrelationID = %q, want \"trace-ix-1\"", got.CorrelationID)
	}
}
