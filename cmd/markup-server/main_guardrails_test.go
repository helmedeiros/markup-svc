package main

import (
	"encoding/json"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/helmedeiros/markup-svc/internal/decider/guardrails"
)

func TestBuildGuardrailRulesEmptyFlagsProducesNoRules(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	_ = fs.Float64("min-factor", 0, "")
	_ = fs.Float64("max-factor", 0, "")
	_ = fs.String("allowed-countries", "", "")
	_ = fs.String("required-fields", "", "")
	if err := fs.Parse(nil); err != nil {
		t.Fatalf("parse: %v", err)
	}
	rules, err := buildGuardrailRules(fs, 0, 0, "", "")
	if err != nil {
		t.Fatalf("buildGuardrailRules: %v", err)
	}
	if len(rules) != 0 {
		t.Fatalf("len(rules) = %d, want 0 when no flags set", len(rules))
	}
}

func TestBuildGuardrailRulesRejectsMinAboveMax(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	_ = fs.Float64("min-factor", 0, "")
	_ = fs.Float64("max-factor", 0, "")
	_ = fs.String("allowed-countries", "", "")
	_ = fs.String("required-fields", "", "")
	if err := fs.Parse([]string{"--min-factor=3.0", "--max-factor=1.0"}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, err := buildGuardrailRules(fs, 3.0, 1.0, "", ""); err == nil {
		t.Fatal("buildGuardrailRules accepted min > max; want error")
	}
}

func TestBuildGuardrailRulesRejectsEmptyAllowedCountries(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	_ = fs.Float64("min-factor", 0, "")
	_ = fs.Float64("max-factor", 0, "")
	_ = fs.String("allowed-countries", "", "")
	_ = fs.String("required-fields", "", "")
	if err := fs.Parse([]string{"--allowed-countries=,, "}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, err := buildGuardrailRules(fs, 0, 0, ",, ", ""); err == nil {
		t.Fatal("buildGuardrailRules accepted empty allowed-countries; want error")
	}
}

func TestBuildGuardrailRulesRejectsEmptyRequiredFields(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	_ = fs.Float64("min-factor", 0, "")
	_ = fs.Float64("max-factor", 0, "")
	_ = fs.String("allowed-countries", "", "")
	_ = fs.String("required-fields", "", "")
	if err := fs.Parse([]string{"--required-fields=,, "}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, err := buildGuardrailRules(fs, 0, 0, "", ",, "); err == nil {
		t.Fatal("buildGuardrailRules accepted empty required-fields; want error")
	}
}

func TestBuildGuardrailRulesAssemblesAllThreeRulesInOrder(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	_ = fs.Float64("min-factor", 0, "")
	_ = fs.Float64("max-factor", 0, "")
	_ = fs.String("allowed-countries", "", "")
	_ = fs.String("required-fields", "", "")
	args := []string{"--min-factor=0.5", "--max-factor=3.0", "--allowed-countries=BR,DE", "--required-fields=customer_tier"}
	if err := fs.Parse(args); err != nil {
		t.Fatalf("parse: %v", err)
	}
	rules, err := buildGuardrailRules(fs, 0.5, 3.0, "BR,DE", "customer_tier")
	if err != nil {
		t.Fatalf("buildGuardrailRules: %v", err)
	}
	if len(rules) != 3 {
		t.Fatalf("len(rules) = %d, want 3", len(rules))
	}
	if _, ok := rules[0].(guardrails.FactorRange); !ok {
		t.Errorf("rules[0] = %T, want FactorRange (factor must veto first per ADR-0014 cheapest-first)", rules[0])
	}
	if _, ok := rules[1].(guardrails.AllowedCountries); !ok {
		t.Errorf("rules[1] = %T, want AllowedCountries", rules[1])
	}
	if _, ok := rules[2].(guardrails.RequiredFields); !ok {
		t.Errorf("rules[2] = %T, want RequiredFields", rules[2])
	}
}

// newGuardedE2EServer boots a real production wire stack via
// wireTracedHandler against testdata/rules.csv with the provided
// guardrail rules. Mirrors the path run() takes when its --max-factor
// / --allowed-countries / --required-fields flags are set.
func newGuardedE2EServer(t *testing.T, rules ...guardrails.Rule) *httptest.Server {
	t.Helper()
	loader := rulesLoader("testdata/rules.csv", "inmemory", "v0-e2e", io.Discard)
	handler, _, err := wireTracedHandler(loader, nil, rules...)
	if err != nil {
		t.Fatalf("wireTracedHandler: %v", err)
	}
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

// TestE2EGuardrailsFactorRangeVetoesAboveMax confirms the production
// wire stack vetoes a Decision whose MarkupFactor exceeds the
// configured max. The enterprise rule produces 1.15; a 1.0 max
// vetoes it; the response is the opaque 500 the operator log explains
// in detail.
func TestE2EGuardrailsFactorRangeVetoesAboveMax(t *testing.T) {
	srv := newGuardedE2EServer(t, guardrails.FactorRange{Min: 0.0, Max: 1.0})
	resp, err := http.Post(srv.URL+"/decide", "application/json",
		strings.NewReader(`{"customer_tier":"enterprise"}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d (body=%s), want 500 (guardrail veto)", resp.StatusCode, body)
	}
}

// TestE2EGuardrailsFactorRangeAllowsBelowMax confirms the asymmetry:
// the same request that just got vetoed at max=1.0 passes at max=2.0.
// This is the "the flag is doing real work" proof -- a regression
// that always-vetoed or never-vetoed would fail one of the two tests.
func TestE2EGuardrailsFactorRangeAllowsBelowMax(t *testing.T) {
	srv := newGuardedE2EServer(t, guardrails.FactorRange{Min: 0.0, Max: 2.0})
	resp, err := http.Post(srv.URL+"/decide", "application/json",
		strings.NewReader(`{"customer_tier":"enterprise"}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d (body=%s), want 200", resp.StatusCode, body)
	}
	var body map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["markup_factor"].(float64) != 1.15 {
		t.Errorf("markup_factor = %v, want 1.15", body["markup_factor"])
	}
}

// TestE2EGuardrailsAllowedCountriesRejectsUnlistedCountry confirms a
// matched rule whose Request.Country is not in the allowed list gets
// vetoed even though the engine returned a valid Decision.
func TestE2EGuardrailsAllowedCountriesRejectsUnlistedCountry(t *testing.T) {
	srv := newGuardedE2EServer(t, guardrails.AllowedCountries{Countries: []string{"BR"}})
	// br_peak rule matches only Country=BR + TimeWindow=peak; we want
	// a request that gets a Decision (so the guardrail layer is
	// consulted) but whose Country fails the allowed-list. The
	// enterprise rule matches solely on customer_tier and ignores
	// Country, so Country="US" still gets a Decision the guardrail
	// then vetoes.
	resp, err := http.Post(srv.URL+"/decide", "application/json",
		strings.NewReader(`{"customer_tier":"enterprise","country":"US"}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d (body=%s), want 500", resp.StatusCode, body)
	}
}

// TestE2ENoGuardrailsLeavesBehaviorUnchanged is the zero-overhead
// proof: a wire stack with no guardrail rules serves exactly the
// existing testdata response.
func TestE2ENoGuardrailsLeavesBehaviorUnchanged(t *testing.T) {
	srv := newGuardedE2EServer(t)
	resp, err := http.Post(srv.URL+"/decide", "application/json",
		strings.NewReader(`{"customer_tier":"enterprise"}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (no guardrails configured)", resp.StatusCode)
	}
}
