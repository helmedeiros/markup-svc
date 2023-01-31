package guardrails

import (
	"errors"
	"fmt"
	"strings"
)

// RuleSpec captures the four configurable axes for the shipped rules.
// Each axis is optional: a nil pointer or nil slice means "no rule of
// that type". A non-nil but invalid value (FactorSpec with Min > Max,
// empty AllowedCountries slice, empty RequiredFields slice) returns
// an error from BuildRules. Unknown field names in RequiredFields are
// NOT validated here -- the documented contract from ADR-0014 is that
// a typo'd field name vetoes at runtime with a clear reason rather
// than failing boot silently, so the validation lives in Check, not
// here.
//
// Both the boot-flag wiring in cmd/markup-server and the
// POST /admin/guardrails handler decode their inputs into a RuleSpec
// before calling BuildRules. That centralizes the min-vs-max ordering,
// empty-list rejection, and rule-order ownership so the two surfaces
// accept and reject identical configurations.
type RuleSpec struct {
	Factor           *FactorSpec
	AllowedCountries []string
	RequiredFields   []string
}

// FactorSpec is the optional FactorRange configuration.
type FactorSpec struct {
	Min float64
	Max float64
}

// Build error sentinels. Callers wrap these with their own surface-
// specific wording (the flag adapter prefixes "--min-factor / --max-factor";
// the admin handler uses the JSON field names).
var (
	// ErrFactorRangeInverted is returned when spec.Factor.Min > spec.Factor.Max.
	ErrFactorRangeInverted = errors.New("min factor must not exceed max factor")
	// ErrAllowedCountriesEmpty is returned when spec.AllowedCountries
	// is non-nil but contains no entries.
	ErrAllowedCountriesEmpty = errors.New("allowed countries set with no values")
	// ErrRequiredFieldsEmpty is returned when spec.RequiredFields is
	// non-nil but contains no entries.
	ErrRequiredFieldsEmpty = errors.New("required fields set with no values")
)

// BuildRules assembles a Rule sequence from spec. Order is:
// FactorRange -> AllowedCountries -> RequiredFields (ADR-0014's
// cheapest-first first-veto-wins ordering). The order is fixed by the
// binary; callers that want a different order build the slice
// themselves.
func BuildRules(spec RuleSpec) ([]Rule, error) {
	var rules []Rule
	if spec.Factor != nil {
		if spec.Factor.Min > spec.Factor.Max {
			return nil, fmt.Errorf("%w: %g vs %g", ErrFactorRangeInverted, spec.Factor.Min, spec.Factor.Max)
		}
		rules = append(rules, FactorRange{Min: spec.Factor.Min, Max: spec.Factor.Max})
	}
	if spec.AllowedCountries != nil {
		if len(spec.AllowedCountries) == 0 {
			return nil, ErrAllowedCountriesEmpty
		}
		rules = append(rules, AllowedCountries{Countries: spec.AllowedCountries})
	}
	if spec.RequiredFields != nil {
		if len(spec.RequiredFields) == 0 {
			return nil, ErrRequiredFieldsEmpty
		}
		rules = append(rules, RequiredFields{Fields: spec.RequiredFields})
	}
	return rules, nil
}

// SplitCSV splits a comma-separated string into trimmed, non-empty
// entries. Empty entries (from trailing commas or repeated commas)
// are silently dropped. Used by both the flag adapter (--allowed-countries=BR,DE)
// and any caller that needs the same lenient parsing.
//
// Returns a nil slice when raw has no non-empty entries, so callers
// that want to distinguish "set but empty" from "absent" must check
// for nil at the call site.
func SplitCSV(raw string) []string {
	if raw == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(raw, ",") {
		trimmed := strings.TrimSpace(part)
		if trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}
