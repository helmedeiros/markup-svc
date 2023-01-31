package guardrails

import (
	"context"
	"fmt"
	"sync"

	"github.com/helmedeiros/markup-svc/internal/markup"
)

// Holder owns a mutable []Rule slice behind a sync.RWMutex and
// produces a markup.Decider via Wrap that vetoes Decisions against
// the current slice. Replace swaps the slice atomically: in-flight
// Decides finish on the captured slice they read at Decide entry.
// Mirrors swap.Decider's minimum-lock-hold pattern; the rationale
// and tradeoffs are documented in ADR-0015.
//
// A Holder with no rules is a pass-through (the Wrap'd Decider
// returns the inner Decision unchanged). Operators who want to mount
// the admin endpoint without configuring any rule at boot construct
// a Holder via NewHolder() with no arguments and rely on
// POST /admin/guardrails to set the configuration at runtime.
type Holder struct {
	mu    sync.RWMutex
	rules []Rule
}

// NewHolder returns a Holder pre-loaded with rules. The input slice
// is defensively copied, so the caller can mutate the slice after
// NewHolder returns without affecting the Holder.
func NewHolder(rules ...Rule) *Holder {
	copied := make([]Rule, len(rules))
	copy(copied, rules)
	return &Holder{rules: copied}
}

// Wrap returns a markup.Decider that reads the current rules from
// h on every Decide. The returned Decider holds h by pointer; any
// subsequent Replace on h is observed by every Decider Wrap returned.
func (h *Holder) Wrap(inner markup.Decider) markup.Decider {
	return &holderDecider{holder: h, inner: inner}
}

// Replace swaps the active rules. The input slice is defensively
// copied so callers cannot mutate the slice the Holder uses after
// Replace returns. A concurrent Decide that captured the previous
// slice header walks the previous backing array to completion; a
// Decide that arrives after Replace returns reads the new slice.
func (h *Holder) Replace(rules []Rule) {
	copied := make([]Rule, len(rules))
	copy(copied, rules)
	h.mu.Lock()
	h.rules = copied
	h.mu.Unlock()
}

// Snapshot returns a defensive copy of the current rules. Callers
// can mutate the returned slice without affecting the Holder.
// Used by the GET /admin/guardrails handler and by tests.
func (h *Holder) Snapshot() []Rule {
	h.mu.RLock()
	rules := h.rules
	h.mu.RUnlock()
	snap := make([]Rule, len(rules))
	copy(snap, rules)
	return snap
}

type holderDecider struct {
	holder *Holder
	inner  markup.Decider
}

// Decide implements markup.Decider. The lock-pair acquires under
// RLock, copies the slice header, releases the lock, then dispatches.
// The captured slice header points at a backing array Replace never
// mutates -- Replace allocates a new array and assigns the new header.
// This is the same minimum-lock-hold pattern as swap.Decider.
func (d *holderDecider) Decide(ctx context.Context, req markup.Request) (markup.Decision, error) {
	d.holder.mu.RLock()
	rules := d.holder.rules
	d.holder.mu.RUnlock()

	decision, err := d.inner.Decide(ctx, req)
	if err != nil {
		return decision, err
	}
	for _, rule := range rules {
		if allowed, reason := rule.Check(ctx, decision, req); !allowed {
			return markup.Decision{}, fmt.Errorf("%w: %s", ErrGuardrailViolation, reason)
		}
	}
	return decision, nil
}
