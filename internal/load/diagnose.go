package load

import (
	"fmt"

	"github.com/helmedeiros/markup-svc/internal/markup"
)

// Diagnose runs the adapter-agnostic checks against a loaded rule
// set and returns one Issue per finding. Errors fail boot when the
// cmd is launched with --diagnose=on (the default). Warnings log but
// do not fail. See ADR-0025.
//
// Adapter-specific deep checks (e.g., the indexed engine's
// non-indexable-condition detector) run at adapter construction
// and produce their own errors via ErrFormatVersionMismatch /
// ErrNonIndexableCondition; those are not duplicated here.
func Diagnose(rules []Rule) markup.Diagnosis {
	out := markup.Diagnosis{}

	if len(rules) == 0 {
		out.Issues = append(out.Issues, markup.Issue{
			Kind:     markup.IssueEmptyRuleSet,
			Severity: markup.SeverityError,
			Detail:   "rule set is empty; markup-svc would return ErrNoMatch for every request",
		})
		return out
	}

	seenName := make(map[string]bool, len(rules))
	priorityCount := make(map[int]int, len(rules))
	for _, r := range rules {
		if seenName[r.Name] {
			out.Issues = append(out.Issues, markup.Issue{
				Kind:     markup.IssueDuplicateName,
				Severity: markup.SeverityError,
				Rule:     r.Name,
				Detail:   "another rule already uses this name; the second occurrence shadows the first or fails to load",
			})
		}
		seenName[r.Name] = true

		if r.Condition == nil {
			out.Issues = append(out.Issues, markup.Issue{
				Kind:     markup.IssueEmptyCondition,
				Severity: markup.SeverityError,
				Rule:     r.Name,
				Detail:   "condition is empty; the rule cannot match any request",
			})
		}

		switch {
		case r.Factor <= 0:
			out.Issues = append(out.Issues, markup.Issue{
				Kind:     markup.IssueInvalidFactor,
				Severity: markup.SeverityError,
				Rule:     r.Name,
				Detail:   fmt.Sprintf("factor %v is non-positive; markup factors must be > 0", r.Factor),
			})
		case r.Factor == 1.0:
			out.Issues = append(out.Issues, markup.Issue{
				Kind:     markup.IssueNoOpFactor,
				Severity: markup.SeverityWarning,
				Rule:     r.Name,
				Detail:   "factor 1.0 leaves the price unchanged; rule is a no-op",
			})
		case r.Factor > 10:
			out.Issues = append(out.Issues, markup.Issue{
				Kind:     markup.IssueInvalidFactor,
				Severity: markup.SeverityWarning,
				Rule:     r.Name,
				Detail:   fmt.Sprintf("factor %v is unusually large; double-check the rule", r.Factor),
			})
		}

		priorityCount[r.Priority]++
	}

	for prio, n := range priorityCount {
		if n > 1 {
			out.Issues = append(out.Issues, markup.Issue{
				Kind:     markup.IssueDuplicatePriority,
				Severity: markup.SeverityWarning,
				Detail:   fmt.Sprintf("priority %d is shared by %d rules; tie-break is adapter-dependent", prio, n),
			})
		}
	}

	return out
}
