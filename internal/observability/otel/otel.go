// Package otel wraps a markup.Decider with OpenTelemetry spans. One
// span per Decide; attributes are markup-domain (Rule, Factor,
// ModelVersion, EngineAdapter) rather than bre-go engine internals.
// ErrNoMatch is treated as a domain outcome (boolean attribute, span
// status OK), and cancellation is distinguished from server errors.
// See ADR-0009.
package otel

import (
	"context"
	"errors"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	breengine "github.com/helmedeiros/bre-go/engine"

	"github.com/helmedeiros/markup-svc/internal/markup"
)

// Standard markup-domain attribute keys emitted by the wrapper. The
// "rule.markup.*" prefix keeps them distinct from bre-go's
// "rule.engine.*" attributes for callers that stack both decorators.
const (
	AttrAdapter       = "rule.markup.adapter"
	AttrModelVersion  = "rule.markup.model_version"
	AttrRule          = "rule.markup.rule"
	AttrFactor        = "rule.markup.factor"
	AttrCorrelationID = "rule.markup.correlation_id"
	AttrNoMatch       = "rule.markup.no_match"
	AttrCanceled      = "rule.markup.canceled"
	AttrCancelReason  = "rule.markup.cancel.reason"
)

const defaultSpanName = "markup.decider.decide"

// Option customizes the wrapper at construction time.
type Option func(*tracedDecider)

// WithSpanName overrides the default span name ("markup.decider.decide").
func WithSpanName(name string) Option {
	return func(t *tracedDecider) { t.spanName = name }
}

// Wrap returns inner decorated with one OpenTelemetry span per Decide.
// The returned value satisfies markup.Decider so it composes with
// other Decider decorators (e.g., swap.Decider for hot reload).
func Wrap(inner markup.Decider, tracer trace.Tracer, opts ...Option) markup.Decider {
	t := &tracedDecider{inner: inner, tracer: tracer, spanName: defaultSpanName}
	for _, opt := range opts {
		opt(t)
	}
	return t
}

type tracedDecider struct {
	inner    markup.Decider
	tracer   trace.Tracer
	spanName string
}

// Decide implements markup.Decider. See ADR-0009's per-outcome table
// for the attribute set written on each branch.
func (t *tracedDecider) Decide(ctx context.Context, req markup.Request) (markup.Decision, error) {
	ctx, span := t.tracer.Start(ctx, t.spanName)
	defer span.End()

	cid := breengine.CorrelationIDFromContext(ctx)

	decision, err := t.inner.Decide(ctx, req)

	switch {
	case errors.Is(err, markup.ErrNoMatch):
		setAttrs(span, cid, attribute.Bool(AttrNoMatch, true))
	case errors.Is(err, context.Canceled):
		setAttrs(span, cid,
			attribute.Bool(AttrCanceled, true),
			attribute.String(AttrCancelReason, "canceled"),
		)
	case errors.Is(err, context.DeadlineExceeded):
		setAttrs(span, cid,
			attribute.Bool(AttrCanceled, true),
			attribute.String(AttrCancelReason, "deadline_exceeded"),
		)
	case err != nil:
		setAttrs(span, cid)
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	default:
		setAttrs(span, cid,
			attribute.String(AttrAdapter, decision.EngineAdapter),
			attribute.String(AttrModelVersion, decision.ModelVersion),
			attribute.String(AttrRule, decision.Rule),
			attribute.Float64(AttrFactor, decision.MarkupFactor),
		)
	}
	return decision, err
}

// setAttrs is the single SetAttributes call per Decide outcome. It
// prepends the correlation ID attribute when ctx carried one so the
// span carries the markup-domain trace identity alongside the
// outcome-specific attributes.
func setAttrs(span trace.Span, correlationID string, extra ...attribute.KeyValue) {
	all := extra
	if correlationID != "" {
		all = append([]attribute.KeyValue{attribute.String(AttrCorrelationID, correlationID)}, extra...)
	}
	if len(all) == 0 {
		return
	}
	span.SetAttributes(all...)
}

