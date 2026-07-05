package httpapi

import (
	"context"
	"errors"

	"github.com/helmedeiros/markup-svc/internal/markup"
)

type decisionLogKey struct{}

// decisionLogEntry stores the per-request Request + Decision that the
// access-log middleware merges into the JSON event so Kibana sees the
// matched rule, input fields, and output factor (ADR-0023). It also
// carries the per-decision identity and outcome that the markup.decision.v1
// event uses (ADR-0035).
type decisionLogEntry struct {
	request    markup.Request
	decision   markup.Decision
	noMatch    bool
	decisionID string
	journeyID  string // caller-supplied; opaque to markup-svc
	outcome    string // ADR-0035 decide_outcome: ok | no_match | canceled | deadline_exceeded | error
	errorMsg   string // populated only when outcome=error
}

func withDecisionContext(ctx context.Context, e decisionLogEntry) context.Context {
	return context.WithValue(ctx, decisionLogKey{}, e)
}

func decisionFromContext(ctx context.Context) (decisionLogEntry, bool) {
	e, ok := ctx.Value(decisionLogKey{}).(decisionLogEntry)
	return e, ok
}

// outcomeFor maps an inner Decider error to the ADR-0035 decide_outcome
// enum and the optional errorMsg field. Pure function so the mapping
// is testable in isolation and extensible without growing the Decide
// closure.
func outcomeFor(err error) (string, string) {
	switch {
	case err == nil:
		return "ok", ""
	case errors.Is(err, markup.ErrNoMatch):
		return "no_match", ""
	case errors.Is(err, context.Canceled):
		return "canceled", err.Error()
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline_exceeded", err.Error()
	default:
		return "error", err.Error()
	}
}
