package guardrails_test

import (
	"errors"
	"testing"

	"github.com/helmedeiros/markup-svc/internal/decider/guardrails"
)

func TestBuildRulesEmptySpecProducesNoRules(t *testing.T) {
	rules, err := guardrails.BuildRules(guardrails.RuleSpec{})
	if err != nil {
		t.Fatalf("BuildRules: %v", err)
	}
	if len(rules) != 0 {
		t.Fatalf("len(rules) = %d, want 0 for empty spec", len(rules))
	}
}

func TestBuildRulesFactorRange(t *testing.T) {
	rules, err := guardrails.BuildRules(guardrails.RuleSpec{
		Factor: &guardrails.FactorSpec{Min: 0.5, Max: 3.0},
	})
	if err != nil {
		t.Fatalf("BuildRules: %v", err)
	}
	if len(rules) != 1 {
		t.Fatalf("len(rules) = %d, want 1", len(rules))
	}
	fr, ok := rules[0].(guardrails.FactorRange)
	if !ok {
		t.Fatalf("rules[0] = %T, want FactorRange", rules[0])
	}
	if fr.Min != 0.5 || fr.Max != 3.0 {
		t.Errorf("FactorRange = %+v, want {Min:0.5 Max:3.0}", fr)
	}
}

func TestBuildRulesFactorRangeInverted(t *testing.T) {
	_, err := guardrails.BuildRules(guardrails.RuleSpec{
		Factor: &guardrails.FactorSpec{Min: 3.0, Max: 1.0},
	})
	if !errors.Is(err, guardrails.ErrFactorRangeInverted) {
		t.Fatalf("err = %v, want ErrFactorRangeInverted", err)
	}
}

func TestBuildRulesAllowedCountries(t *testing.T) {
	rules, err := guardrails.BuildRules(guardrails.RuleSpec{
		AllowedCountries: []string{"BR", "DE", "FR"},
	})
	if err != nil {
		t.Fatalf("BuildRules: %v", err)
	}
	ac, ok := rules[0].(guardrails.AllowedCountries)
	if !ok {
		t.Fatalf("rules[0] = %T, want AllowedCountries", rules[0])
	}
	if len(ac.Countries) != 3 {
		t.Fatalf("len(Countries) = %d, want 3", len(ac.Countries))
	}
}

func TestBuildRulesAllowedCountriesEmpty(t *testing.T) {
	_, err := guardrails.BuildRules(guardrails.RuleSpec{
		AllowedCountries: []string{},
	})
	if !errors.Is(err, guardrails.ErrAllowedCountriesEmpty) {
		t.Fatalf("err = %v, want ErrAllowedCountriesEmpty", err)
	}
}

func TestBuildRulesRequiredFields(t *testing.T) {
	rules, err := guardrails.BuildRules(guardrails.RuleSpec{
		RequiredFields: []string{"customer_tier", "country"},
	})
	if err != nil {
		t.Fatalf("BuildRules: %v", err)
	}
	rf, ok := rules[0].(guardrails.RequiredFields)
	if !ok {
		t.Fatalf("rules[0] = %T, want RequiredFields", rules[0])
	}
	if len(rf.Fields) != 2 {
		t.Fatalf("len(Fields) = %d, want 2", len(rf.Fields))
	}
}

func TestBuildRulesRequiredFieldsEmpty(t *testing.T) {
	_, err := guardrails.BuildRules(guardrails.RuleSpec{
		RequiredFields: []string{},
	})
	if !errors.Is(err, guardrails.ErrRequiredFieldsEmpty) {
		t.Fatalf("err = %v, want ErrRequiredFieldsEmpty", err)
	}
}

func TestBuildRulesAssemblesInCheapestFirstOrder(t *testing.T) {
	rules, err := guardrails.BuildRules(guardrails.RuleSpec{
		Factor:           &guardrails.FactorSpec{Min: 0.5, Max: 3.0},
		AllowedCountries: []string{"BR"},
		RequiredFields:   []string{"customer_tier"},
	})
	if err != nil {
		t.Fatalf("BuildRules: %v", err)
	}
	if len(rules) != 3 {
		t.Fatalf("len(rules) = %d, want 3", len(rules))
	}
	if _, ok := rules[0].(guardrails.FactorRange); !ok {
		t.Errorf("rules[0] = %T, want FactorRange (cheapest first)", rules[0])
	}
	if _, ok := rules[1].(guardrails.AllowedCountries); !ok {
		t.Errorf("rules[1] = %T, want AllowedCountries", rules[1])
	}
	if _, ok := rules[2].(guardrails.RequiredFields); !ok {
		t.Errorf("rules[2] = %T, want RequiredFields", rules[2])
	}
}

func TestSplitCSVDropsBlankEntries(t *testing.T) {
	cases := []struct {
		raw  string
		want []string
	}{
		{"BR,DE,FR", []string{"BR", "DE", "FR"}},
		{"BR, DE ,FR", []string{"BR", "DE", "FR"}},
		{",BR,,DE,", []string{"BR", "DE"}},
		{"", nil},
		{",, ", nil},
	}
	for _, tc := range cases {
		got := guardrails.SplitCSV(tc.raw)
		if len(got) != len(tc.want) {
			t.Errorf("SplitCSV(%q) len = %d, want %d (got=%v)", tc.raw, len(got), len(tc.want), got)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("SplitCSV(%q)[%d] = %q, want %q", tc.raw, i, got[i], tc.want[i])
			}
		}
	}
}
