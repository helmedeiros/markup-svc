// Package markup defines the domain types for the markup service.
// See ADR-0001 for the design rationale.
package markup

import (
	"context"
	"errors"
)

// Request is the input to every markup decision. All fields except
// Amount are strings so rule conditions written in bre-go's expression
// DSL match uniformly via parser.StringCondition / SetCondition.
type Request struct {
	ProductID    string
	Category     string
	CustomerTier string
	Channel      string
	Country      string
	Inventory    string
	TimeWindow   string
	Amount       float64
}

// Decision is the output of every markup decision. Provenance fields
// (Rule, ModelVersion, Experiment, CorrelationID, EngineAdapter) let
// observability slice decisions by (rule set x variant x engine).
type Decision struct {
	MarkupFactor  float64
	Rule          string
	ModelVersion  string
	Experiment    string
	CorrelationID string
	EngineAdapter string
}

// Decider is the port every adapter implements. See ADR-0001.
type Decider interface {
	Decide(ctx context.Context, req Request) (Decision, error)
}

// ErrNoMatch is returned by Decide when no rule matched the request.
// The returned Decision is zero-valued; callers decide whether to
// apply a fallback or reject the request.
var ErrNoMatch = errors.New("markup: no rule matched the request")
