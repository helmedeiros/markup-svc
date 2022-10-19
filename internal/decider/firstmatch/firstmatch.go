// Package firstmatch wraps bre-go's engine/firstmatch.Engine behind
// the markup.Decider port. Semantics differ from the inmemory adapter:
// rules evaluate in insertion order and the first match returns
// immediately, leaving subsequent rules unevaluated. See ADR-0004.
package firstmatch

import (
	"context"
	"fmt"

	breengine "github.com/helmedeiros/bre-go/engine"
	brefirstmatch "github.com/helmedeiros/bre-go/engine/firstmatch"

	"github.com/helmedeiros/markup-svc/internal/load"
	"github.com/helmedeiros/markup-svc/internal/markup"
)

// Rule is the typed rule shape this Decider takes at construction.
// Same shape as inmemory.Rule -- the markup-side rule contract is
// the same; only the underlying engine's evaluation strategy differs.
type Rule struct {
	Name      string
	Condition func(markup.Request) bool
	Factor    float64
}

// Decider implements markup.Decider backed by bre-go's firstmatch
// engine. Constructed via New or NewFromRules; not safe for concurrent
// modification, but Decide is safe to call concurrently with itself.
type Decider struct {
	inner        *brefirstmatch.Engine
	modelVersion string
}

// New returns a Decider wired to rules with modelVersion as the tag
// every emitted Decision carries. Adapter-specific errors from bre-go
// (empty name, duplicate name, nil condition) propagate as wrapped
// errors.
func New(rules []Rule, modelVersion string) (*Decider, error) {
	e := brefirstmatch.New()
	for _, r := range rules {
		cond := r.Condition
		factor := r.Factor
		if err := e.AddRule(brefirstmatch.Rule{
			Name: r.Name,
			Condition: func(in interface{}) bool {
				req, ok := in.(markup.Request)
				return ok && cond(req)
			},
			Action: func(in interface{}) interface{} {
				return factor
			},
		}); err != nil {
			return nil, fmt.Errorf("firstmatchdecider: add rule %q: %w", r.Name, err)
		}
	}
	return &Decider{inner: e, modelVersion: modelVersion}, nil
}

// NewFromRules builds a Decider from loader-side rules (per ADR-0002
// and ADR-0004). Each Rule's pre-compiled parser.Condition is wrapped
// via markup.FactOf into the typed func(markup.Request) bool that New
// takes. load.Rule.Priority is intentionally dropped -- the firstmatch
// adapter follows insertion order; the priority adapter is the one
// that consumes that column.
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

// Decide implements markup.Decider. Returns markup.ErrNoMatch when no
// rule matched. On match, Decision.Rule carries the first-matched
// rule's name (firstmatch adapter semantics: insertion-order
// precedence).
func (d *Decider) Decide(ctx context.Context, req markup.Request) (markup.Decision, error) {
	res, err := d.inner.Execute(ctx, breengine.Request{Input: req})
	if err != nil {
		return markup.Decision{}, fmt.Errorf("firstmatchdecider: execute: %w", err)
	}
	if len(res.Matched) == 0 {
		return markup.Decision{}, markup.ErrNoMatch
	}
	factor, ok := res.Output.(float64)
	if !ok {
		return markup.Decision{}, fmt.Errorf("firstmatchdecider: rule action returned %T, expected float64", res.Output)
	}
	return markup.Decision{
		MarkupFactor:  factor,
		Rule:          res.Matched[0],
		ModelVersion:  d.modelVersion,
		CorrelationID: breengine.CorrelationIDFromContext(ctx),
		EngineAdapter: fmt.Sprintf("%T", d.inner),
	}, nil
}
