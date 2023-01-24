package guardrails

import (
	"context"
	"errors"
	"fmt"

	"github.com/helmedeiros/markup-svc/internal/markup"
)

// ErrGuardrailViolation is the sentinel that distinguishes veto
// errors from other engine errors. The Decider wraps it with the
// vetoing rule's reason via fmt.Errorf("%w: <reason>"), so callers
// reach it with errors.Is(err, ErrGuardrailViolation) and read the
// reason with err.Error().
var ErrGuardrailViolation = errors.New("guardrails: decision vetoed")

// Rule is the veto port. Implementations decide whether a given
// (Decision, Request) pair is allowed to leave the service. ctx is
// carried for consistency with every other method at the Decider
// port; the shipped Rules in this package do not consult it, but a
// custom Rule that performs an out-of-process lookup (e.g., a
// dynamic-limits config service) honors request cancellation.
type Rule interface {
	Check(ctx context.Context, decision markup.Decision, req markup.Request) (allowed bool, reason string)
}

// Decider wraps an inner markup.Decider with a sequence of Rules and
// itself satisfies markup.Decider. On allowed: the inner Decision is
// returned unchanged. On the first vetoing Rule: the zero Decision
// is returned with an error wrapping ErrGuardrailViolation. Rules
// after the first veto are not consulted; the order in which Rules
// are passed to New therefore determines which reason an operator
// sees when a Decision violates multiple invariants.
//
// When inner returns an error, Rules are not consulted -- there is
// no Decision to veto, and operators reading the metrics / span
// stream should see the inner error verbatim (e.g., ErrNoMatch),
// not a guardrails wrap of it.
type Decider struct {
	inner markup.Decider
	rules []Rule
}

// New returns a Decider that wraps inner with the given Rules. The
// returned Decider holds inner and rules by reference; mutating
// rules after construction is not safe. A New call with no Rules
// returns a Decider that passes every Decision through (useful for
// composition tests).
func New(inner markup.Decider, rules ...Rule) *Decider {
	return &Decider{inner: inner, rules: rules}
}

// Decide implements markup.Decider. Calls inner.Decide first; on
// inner error, returns it verbatim (rules are not consulted). On
// inner success, runs each Rule in order; the first to return
// allowed=false short-circuits with a wrapped ErrGuardrailViolation
// carrying the rule's reason. When all rules allow, returns the
// inner Decision unchanged.
func (d *Decider) Decide(ctx context.Context, req markup.Request) (markup.Decision, error) {
	decision, err := d.inner.Decide(ctx, req)
	if err != nil {
		return decision, err
	}
	for _, rule := range d.rules {
		if allowed, reason := rule.Check(ctx, decision, req); !allowed {
			return markup.Decision{}, fmt.Errorf("%w: %s", ErrGuardrailViolation, reason)
		}
	}
	return decision, nil
}
