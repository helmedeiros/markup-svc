package guardrails

import (
	"context"
	"fmt"

	"github.com/helmedeiros/markup-svc/internal/markup"
)

// FactorRange vetoes Decisions whose MarkupFactor falls outside the
// closed interval [Min, Max]. The canonical "no markup greater than
// 3x and no markup smaller than 0.5x" safety rule.
//
// FactorRange{} (the zero value, Min=Max=0) vetoes every Decision
// with a nonzero MarkupFactor and permits exactly MarkupFactor=0,
// because 0<0 and 0>0 are both false. This is the documented
// degenerate behavior; the zero value is not a sentinel for "no
// bound". An operator that wants only an upper bound sets Min to a
// value below any Decision the engine ever produces (e.g., 0).
type FactorRange struct {
	Min float64
	Max float64
}

// Check implements Rule. Returns (false, reason) when MarkupFactor is
// strictly less than Min or strictly greater than Max; otherwise
// returns (true, "").
func (r FactorRange) Check(_ context.Context, decision markup.Decision, _ markup.Request) (bool, string) {
	if decision.MarkupFactor < r.Min {
		return false, fmt.Sprintf("factor %.2f below min %.2f", decision.MarkupFactor, r.Min)
	}
	if decision.MarkupFactor > r.Max {
		return false, fmt.Sprintf("factor %.2f above max %.2f", decision.MarkupFactor, r.Max)
	}
	return true, ""
}

// AllowedCountries vetoes when the Request's Country is not in
// Countries. An empty Countries list vetoes every Decision (no
// country is allowed) -- operators wanting "no country restriction"
// should omit the rule entirely rather than passing an empty list.
//
// Comparison is case-sensitive: "BR" and "br" are different. Upstream
// callers are expected to normalize country codes before posting to
// /decide; a rule mismatch surfaces as a guardrails veto with a clear
// reason rather than a silent no-match.
type AllowedCountries struct {
	Countries []string
}

// Check implements Rule. Walks Countries with one string comparison
// per entry; returns (true, "") on the first match. With no match
// after the full walk, returns (false, reason).
func (r AllowedCountries) Check(_ context.Context, _ markup.Decision, req markup.Request) (bool, string) {
	for _, c := range r.Countries {
		if c == req.Country {
			return true, ""
		}
	}
	return false, fmt.Sprintf("country %q not in allowed list", req.Country)
}

// RequiredFields vetoes when the Request lacks a named non-empty
// field. Covers the seven string fields on markup.Request:
// "product_id", "category", "customer_tier", "channel", "country",
// "inventory", "time_window". Amount (numeric) is excluded -- a zero
// Amount is a legal Request, not a missing field.
//
// Unknown field names are treated as always-missing -- they will
// fail every Decide call until removed from the configuration. This
// is intentional: a typo in a field name should be loud, not silent.
type RequiredFields struct {
	Fields []string
}

// Check implements Rule. For each named field, looks up the value on
// req via a small switch and reports the first missing one. The
// switch is intentional -- at the handful of field names operators
// configure, the Go compiler emits a length-bucketed jump table that
// beats a map lookup, and a precomputed predicate slice would add
// construction-time allocation without latency gain. See ADR-0014.
func (r RequiredFields) Check(_ context.Context, _ markup.Decision, req markup.Request) (bool, string) {
	for _, field := range r.Fields {
		var value string
		switch field {
		case "product_id":
			value = req.ProductID
		case "category":
			value = req.Category
		case "customer_tier":
			value = req.CustomerTier
		case "channel":
			value = req.Channel
		case "country":
			value = req.Country
		case "inventory":
			value = req.Inventory
		case "time_window":
			value = req.TimeWindow
		default:
			return false, fmt.Sprintf("unknown required field %q", field)
		}
		if value == "" {
			return false, fmt.Sprintf("required field %q is empty", field)
		}
	}
	return true, ""
}
