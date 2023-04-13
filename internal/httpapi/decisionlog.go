package httpapi

import (
	"context"

	"github.com/helmedeiros/markup-svc/internal/markup"
)

type decisionLogKey struct{}

// decisionLogEntry stores the per-request Request + Decision that the
// access-log middleware merges into the JSON event so Kibana sees the
// matched rule, input fields, and output factor. See ADR-0023.
type decisionLogEntry struct {
	request  markup.Request
	decision markup.Decision
	noMatch  bool
}

func withDecisionContext(ctx context.Context, e decisionLogEntry) context.Context {
	return context.WithValue(ctx, decisionLogKey{}, e)
}

func decisionFromContext(ctx context.Context) (decisionLogEntry, bool) {
	e, ok := ctx.Value(decisionLogKey{}).(decisionLogEntry)
	return e, ok
}
