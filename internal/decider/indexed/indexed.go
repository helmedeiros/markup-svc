// Package indexed wraps bre-go's engine/indexed.Engine behind the
// markup.Decider port. Semantics match firstmatch (insertion order is
// precedence, first match wins) but per-Decide cost is sub-linear:
// the engine buckets rules by (key-set, value-tuple) and resolves
// Execute via hash lookups followed by post-filter walks on bucket
// candidates only. See ADR-0006.
package indexed

import (
	"context"
	"fmt"

	breengine "github.com/helmedeiros/bre-go/engine"
	breindexed "github.com/helmedeiros/bre-go/engine/indexed"
	"github.com/helmedeiros/bre-go/engine/parser"

	"github.com/helmedeiros/markup-svc/internal/load"
	"github.com/helmedeiros/markup-svc/internal/markup"
)

// Rule is the typed rule shape this Decider takes at construction.
// Diverges from inmemory.Rule / firstmatch.Rule / priority.Rule: the
// indexer inspects the parser.Condition tree to bucket the rule, so
// Match must be the typed AST node, not an opaque closure.
type Rule struct {
	Name   string
	Match  parser.Condition
	Factor float64
}

// Decider implements markup.Decider backed by bre-go's indexed engine.
// Constructed via New or NewFromRules; not safe for concurrent
// modification, but Decide is safe to call concurrently with itself
// after construction (the inner engine seals via Build at New time).
type Decider struct {
	inner        *breindexed.Engine
	modelVersion string
}

// New returns a Decider wired to rules with modelVersion as the tag
// every emitted Decision carries. Adapter-specific errors from bre-go
// (empty name, nil match, non-indexable condition, no indexable
// terms, duplicate name, fanout-too-large) propagate as wrapped
// errors. Build() runs synchronously so seal-time errors also
// surface at construction rather than at the first Decide.
func New(rules []Rule, modelVersion string) (*Decider, error) {
	e := breindexed.New()
	for _, r := range rules {
		factor := r.Factor
		if err := e.AddRule(breindexed.Rule{
			Name:  r.Name,
			Match: r.Match,
			Action: func(in interface{}) interface{} {
				return factor
			},
		}); err != nil {
			return nil, fmt.Errorf("indexeddecider: add rule %q: %w", r.Name, err)
		}
	}
	if err := e.Build(); err != nil {
		return nil, fmt.Errorf("indexeddecider: build: %w", err)
	}
	return &Decider{inner: e, modelVersion: modelVersion}, nil
}

// NewFromRules builds a Decider from loader-side rules (per ADR-0002
// and ADR-0006). Each load.Rule.Condition (already a typed
// parser.Condition) becomes Rule.Match directly -- no closure
// wrapping is needed because the indexed engine consumes the AST.
func NewFromRules(rules []load.Rule, modelVersion string) (*Decider, error) {
	typed := make([]Rule, 0, len(rules))
	for _, r := range rules {
		typed = append(typed, Rule{
			Name:   r.Name,
			Match:  r.Condition,
			Factor: r.Factor,
		})
	}
	return New(typed, modelVersion)
}

// Decide implements markup.Decider. Passes markup.FactOf(req) as the
// engine input (the indexed engine consumes a fact map directly,
// unlike the closure-based adapters). Returns markup.ErrNoMatch when
// no rule matched; on match, Decision.Rule is the first matching
// rule's name (insertion-order precedence, same as firstmatch).
func (d *Decider) Decide(ctx context.Context, req markup.Request) (markup.Decision, error) {
	res, err := d.inner.Execute(ctx, breengine.Request{Input: markup.FactOf(req)})
	if err != nil {
		return markup.Decision{}, fmt.Errorf("indexeddecider: execute: %w", err)
	}
	if len(res.Matched) == 0 {
		return markup.Decision{}, markup.ErrNoMatch
	}
	factor, ok := res.Output.(float64)
	if !ok {
		return markup.Decision{}, fmt.Errorf("indexeddecider: rule action returned %T, expected float64", res.Output)
	}
	return markup.Decision{
		MarkupFactor:  factor,
		Rule:          res.Matched[0],
		ModelVersion:  d.modelVersion,
		CorrelationID: breengine.CorrelationIDFromContext(ctx),
		EngineAdapter: fmt.Sprintf("%T", d.inner),
	}, nil
}
