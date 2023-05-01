package load_test

import (
	"testing"

	"github.com/helmedeiros/bre-go/engine/parser"

	"github.com/helmedeiros/markup-svc/internal/load"
	"github.com/helmedeiros/markup-svc/internal/markup"
)

func newRule(name string, factor float64, priority int, cond parser.Condition) load.Rule {
	return load.Rule{Name: name, Factor: factor, Priority: priority, Condition: cond}
}

func TestDiagnose_HealthySetIsHealthy(t *testing.T) {
	cond := parser.StringCondition{Field: "country", Op: parser.OpEq, Value: "BR"}
	d := load.Diagnose([]load.Rule{
		newRule("br", 1.05, 5, cond),
		newRule("enterprise", 1.15, 10, cond),
	})
	if !d.IsHealthy() {
		t.Errorf("expected healthy, got issues=%+v", d.Issues)
	}
}

func TestDiagnose_EmptySetIsError(t *testing.T) {
	d := load.Diagnose(nil)
	if d.IsHealthy() {
		t.Fatalf("expected unhealthy on empty set")
	}
	if d.Issues[0].Kind != markup.IssueEmptyRuleSet {
		t.Errorf("Kind = %s, want %s", d.Issues[0].Kind, markup.IssueEmptyRuleSet)
	}
}

func TestDiagnose_DuplicateNameIsError(t *testing.T) {
	cond := parser.StringCondition{Field: "x", Op: parser.OpEq, Value: "y"}
	d := load.Diagnose([]load.Rule{
		newRule("dup", 1.1, 1, cond),
		newRule("dup", 1.2, 2, cond),
	})
	if d.IsHealthy() {
		t.Fatalf("expected unhealthy on duplicate name")
	}
	kinds := map[markup.IssueKind]bool{}
	for _, i := range d.Issues {
		kinds[i.Kind] = true
	}
	if !kinds[markup.IssueDuplicateName] {
		t.Errorf("issues missing IssueDuplicateName: %+v", d.Issues)
	}
}

func TestDiagnose_NonPositiveFactorIsError(t *testing.T) {
	cond := parser.StringCondition{Field: "x", Op: parser.OpEq, Value: "y"}
	d := load.Diagnose([]load.Rule{newRule("bad", 0, 1, cond)})
	if d.IsHealthy() {
		t.Fatalf("expected unhealthy on factor=0")
	}
}

func TestDiagnose_NoOpFactorIsWarning(t *testing.T) {
	cond := parser.StringCondition{Field: "x", Op: parser.OpEq, Value: "y"}
	d := load.Diagnose([]load.Rule{newRule("noop", 1.0, 1, cond)})
	if !d.IsHealthy() {
		t.Errorf("expected healthy with only warnings, got %+v", d.Issues)
	}
	if len(d.Warnings()) != 1 || d.Warnings()[0].Kind != markup.IssueNoOpFactor {
		t.Errorf("warnings = %+v", d.Warnings())
	}
}

func TestDiagnose_DuplicatePriorityIsWarning(t *testing.T) {
	cond := parser.StringCondition{Field: "x", Op: parser.OpEq, Value: "y"}
	d := load.Diagnose([]load.Rule{
		newRule("a", 1.1, 5, cond),
		newRule("b", 1.2, 5, cond),
		newRule("c", 1.3, 5, cond),
	})
	if !d.IsHealthy() {
		t.Errorf("expected healthy, got %+v", d.Issues)
	}
	found := false
	for _, w := range d.Warnings() {
		if w.Kind == markup.IssueDuplicatePriority {
			found = true
		}
	}
	if !found {
		t.Errorf("missing duplicate-priority warning: %+v", d.Issues)
	}
}

func TestDiagnose_EmptyConditionIsError(t *testing.T) {
	d := load.Diagnose([]load.Rule{newRule("x", 1.1, 1, nil)})
	if d.IsHealthy() {
		t.Fatalf("expected unhealthy on nil condition")
	}
}
