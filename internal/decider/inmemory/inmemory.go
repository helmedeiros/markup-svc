// Package inmemory wraps bre-go's engine/inmemory.Engine behind the
// markup.Decider port. The inmemory adapter runs every matching
// rule's Action and the last action wins on Output. This Decider
// surfaces that semantic: when multiple rules match, the last-matched
// rule's factor and name end up on the Decision.
package inmemory

import (
	"context"
	"fmt"

	breengine "github.com/helmedeiros/bre-go/engine"
	breinmemory "github.com/helmedeiros/bre-go/engine/inmemory"

	"github.com/helmedeiros/markup-svc/internal/load"
	"github.com/helmedeiros/markup-svc/internal/markup"
)

// Rule is the typed rule shape this Decider takes at construction.
type Rule struct {
	Name      string
	Condition func(markup.Request) bool
	Factor    float64
}

// Decider implements markup.Decider. Constructed via New; not safe
// for concurrent modification, but Decide is safe to call concurrently
// with itself (the inner engine is read-only after construction).
type Decider struct {
	inner        *breinmemory.Engine
	modelVersion string
}

// NewFromRules builds a Decider from loader-side rules (per ADR-0002).
// Each Rule's pre-compiled parser.Condition is wrapped via markup.FactOf
// into the typed func(markup.Request) bool that New takes; bre-go's
// add-rule errors propagate as wrapped errors. modelVersion is the tag
// every emitted Decision carries. load.Rule.Priority is intentionally
// dropped here -- the inmemory adapter is "last action wins" per slice
// order; the priority adapter is the one that consumes Priority.
func NewFromRules(rules []load.Rule, modelVersion string) (*Decider, error) {
	typed := make([]Rule, 0, len(rules))
	for _, r := range rules {
		cond := r.Condition
		typed = append(typed, Rule{
			Name: r.Name,
			Condition: func(req markup.Request) bool {
				return cond.Eval(markup.FactOf(req))
			},
			Factor: r.Factor,
		})
	}
	return New(typed, modelVersion)
}

// New returns a Decider wired to rules with modelVersion as the tag
// every emitted Decision carries. Adapter-specific errors from
// bre-go (empty name, duplicate name, nil condition) propagate as
// wrapped errors.
func New(rules []Rule, modelVersion string) (*Decider, error) {
	e := breinmemory.New()
	for _, r := range rules {
		cond := r.Condition
		factor := r.Factor
		if err := e.AddRule(breinmemory.Rule{
			Name: r.Name,
			Condition: func(in interface{}) bool {
				req, ok := in.(markup.Request)
				return ok && cond(req)
			},
			Action: func(in interface{}) interface{} {
				return factor
			},
		}); err != nil {
			return nil, fmt.Errorf("inmemorydecider: add rule %q: %w", r.Name, err)
		}
	}
	return &Decider{inner: e, modelVersion: modelVersion}, nil
}

// Decide implements markup.Decider. Returns markup.ErrNoMatch when
// no rule matched. On match, Decision.Rule carries the last-matched
// rule's name (inmemory adapter semantics) and Decision.MarkupFactor
// carries that rule's factor.
func (d *Decider) Decide(ctx context.Context, req markup.Request) (markup.Decision, error) {
	res, err := d.inner.Execute(ctx, breengine.Request{Input: req})
	if err != nil {
		return markup.Decision{}, fmt.Errorf("inmemorydecider: execute: %w", err)
	}
	if len(res.Matched) == 0 {
		return markup.Decision{}, markup.ErrNoMatch
	}
	factor, ok := res.Output.(float64)
	if !ok {
		return markup.Decision{}, fmt.Errorf("inmemorydecider: rule action returned %T, expected float64", res.Output)
	}
	return markup.Decision{
		MarkupFactor:  factor,
		Rule:          res.Matched[len(res.Matched)-1],
		ModelVersion:  d.modelVersion,
		CorrelationID: breengine.CorrelationIDFromContext(ctx),
		EngineAdapter: fmt.Sprintf("%T", d.inner),
	}, nil
}
