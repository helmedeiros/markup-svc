package guardrails_test

import (
	"context"
	"strings"
	"testing"

	"github.com/helmedeiros/markup-svc/internal/decider/guardrails"
	"github.com/helmedeiros/markup-svc/internal/markup"
)

func TestFactorRangeAcceptsAtMin(t *testing.T) {
	r := guardrails.FactorRange{Min: 0.5, Max: 3.0}
	allowed, _ := r.Check(context.Background(), markup.Decision{MarkupFactor: 0.5}, markup.Request{})
	if !allowed {
		t.Fatal("MarkupFactor at Min was vetoed; closed interval expected")
	}
}

func TestFactorRangeAcceptsAtMax(t *testing.T) {
	r := guardrails.FactorRange{Min: 0.5, Max: 3.0}
	allowed, _ := r.Check(context.Background(), markup.Decision{MarkupFactor: 3.0}, markup.Request{})
	if !allowed {
		t.Fatal("MarkupFactor at Max was vetoed; closed interval expected")
	}
}

func TestFactorRangeRejectsBelowMin(t *testing.T) {
	r := guardrails.FactorRange{Min: 0.5, Max: 3.0}
	allowed, reason := r.Check(context.Background(), markup.Decision{MarkupFactor: 0.49}, markup.Request{})
	if allowed {
		t.Fatal("MarkupFactor below Min was allowed")
	}
	if !strings.Contains(reason, "below min") {
		t.Fatalf("reason %q does not mention 'below min'", reason)
	}
	if !strings.Contains(reason, "0.50") {
		t.Fatalf("reason %q does not include the Min value", reason)
	}
}

func TestFactorRangeRejectsAboveMax(t *testing.T) {
	r := guardrails.FactorRange{Min: 0.5, Max: 3.0}
	allowed, reason := r.Check(context.Background(), markup.Decision{MarkupFactor: 3.01}, markup.Request{})
	if allowed {
		t.Fatal("MarkupFactor above Max was allowed")
	}
	if !strings.Contains(reason, "above max") {
		t.Fatalf("reason %q does not mention 'above max'", reason)
	}
	if !strings.Contains(reason, "3.00") {
		t.Fatalf("reason %q does not include the Max value", reason)
	}
}

func TestFactorRangeMinEqualsMaxAcceptsOnlyExactValue(t *testing.T) {
	r := guardrails.FactorRange{Min: 1.0, Max: 1.0}
	allowed, _ := r.Check(context.Background(), markup.Decision{MarkupFactor: 1.0}, markup.Request{})
	if !allowed {
		t.Fatal("MarkupFactor exactly at Min==Max was vetoed; degenerate closed interval should accept")
	}
	allowed, _ = r.Check(context.Background(), markup.Decision{MarkupFactor: 0.99}, markup.Request{})
	if allowed {
		t.Fatal("MarkupFactor 0.99 was allowed with Min=Max=1.0")
	}
	allowed, _ = r.Check(context.Background(), markup.Decision{MarkupFactor: 1.01}, markup.Request{})
	if allowed {
		t.Fatal("MarkupFactor 1.01 was allowed with Min=Max=1.0")
	}
}

func TestAllowedCountriesAcceptsListedCountry(t *testing.T) {
	r := guardrails.AllowedCountries{Countries: []string{"BR", "DE", "FR"}}
	allowed, _ := r.Check(context.Background(), markup.Decision{}, markup.Request{Country: "DE"})
	if !allowed {
		t.Fatal("listed country was vetoed")
	}
}

func TestAllowedCountriesRejectsUnlistedCountry(t *testing.T) {
	r := guardrails.AllowedCountries{Countries: []string{"BR", "DE", "FR"}}
	allowed, reason := r.Check(context.Background(), markup.Decision{}, markup.Request{Country: "US"})
	if allowed {
		t.Fatal("unlisted country was allowed")
	}
	if !strings.Contains(reason, `"US"`) {
		t.Fatalf("reason %q does not quote the offending country", reason)
	}
}

func TestAllowedCountriesRejectsEmptyCountry(t *testing.T) {
	r := guardrails.AllowedCountries{Countries: []string{"BR"}}
	allowed, _ := r.Check(context.Background(), markup.Decision{}, markup.Request{})
	if allowed {
		t.Fatal("Request with empty Country was allowed; empty is not in the list")
	}
}

func TestAllowedCountriesEmptyListVetoesEverything(t *testing.T) {
	r := guardrails.AllowedCountries{Countries: nil}
	allowed, _ := r.Check(context.Background(), markup.Decision{}, markup.Request{Country: "BR"})
	if allowed {
		t.Fatal("AllowedCountries with empty list allowed a Request; documented behavior is veto-all")
	}
}

func TestAllowedCountriesIsCaseSensitive(t *testing.T) {
	r := guardrails.AllowedCountries{Countries: []string{"BR"}}
	allowed, _ := r.Check(context.Background(), markup.Decision{}, markup.Request{Country: "br"})
	if allowed {
		t.Fatal("AllowedCountries matched lowercase against uppercase entry; case-sensitive contract broken")
	}
}

func TestRequiredFieldsAcceptsWhenAllPresent(t *testing.T) {
	r := guardrails.RequiredFields{Fields: []string{"customer_tier", "country"}}
	allowed, _ := r.Check(context.Background(), markup.Decision{}, markup.Request{CustomerTier: "gold", Country: "BR"})
	if !allowed {
		t.Fatal("Request with all required fields present was vetoed")
	}
}

func TestRequiredFieldsRejectsWhenAnyMissing(t *testing.T) {
	r := guardrails.RequiredFields{Fields: []string{"customer_tier", "country"}}
	allowed, reason := r.Check(context.Background(), markup.Decision{}, markup.Request{CustomerTier: "gold"})
	if allowed {
		t.Fatal("Request missing 'country' was allowed")
	}
	if !strings.Contains(reason, `"country"`) {
		t.Fatalf("reason %q does not name the missing field", reason)
	}
}

func TestRequiredFieldsRejectsUnknownFieldName(t *testing.T) {
	r := guardrails.RequiredFields{Fields: []string{"made_up_field"}}
	allowed, reason := r.Check(context.Background(), markup.Decision{}, markup.Request{})
	if allowed {
		t.Fatal("RequiredFields silently accepted an unknown field name; should veto loudly")
	}
	if !strings.Contains(reason, "unknown") {
		t.Fatalf("reason %q does not mention 'unknown'", reason)
	}
	if !strings.Contains(reason, `"made_up_field"`) {
		t.Fatalf("reason %q does not quote the offending field name", reason)
	}
}

func TestRequiredFieldsEmptyFieldsAllowsEverything(t *testing.T) {
	r := guardrails.RequiredFields{Fields: nil}
	allowed, _ := r.Check(context.Background(), markup.Decision{}, markup.Request{})
	if !allowed {
		t.Fatal("RequiredFields with empty Fields list vetoed; zero-config should be a no-op")
	}
}

func TestRequiredFieldsCoversAllSevenStringFields(t *testing.T) {
	cases := []struct {
		field string
		req   markup.Request
	}{
		{"product_id", markup.Request{ProductID: "p1"}},
		{"category", markup.Request{Category: "c1"}},
		{"customer_tier", markup.Request{CustomerTier: "gold"}},
		{"channel", markup.Request{Channel: "web"}},
		{"country", markup.Request{Country: "BR"}},
		{"inventory", markup.Request{Inventory: "high"}},
		{"time_window", markup.Request{TimeWindow: "peak"}},
	}
	for _, tc := range cases {
		r := guardrails.RequiredFields{Fields: []string{tc.field}}
		allowed, reason := r.Check(context.Background(), markup.Decision{}, tc.req)
		if !allowed {
			t.Fatalf("field %q with populated value was vetoed: %s", tc.field, reason)
		}
		allowed, _ = r.Check(context.Background(), markup.Decision{}, markup.Request{})
		if allowed {
			t.Fatalf("field %q empty Request was allowed; should veto", tc.field)
		}
	}
}
