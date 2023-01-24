package guardrails_test

import (
	"context"
	"errors"
	"testing"

	"github.com/helmedeiros/markup-svc/internal/decider/guardrails"
	"github.com/helmedeiros/markup-svc/internal/markup"
)

// stubDecider returns a fixed (Decision, error) regardless of input.
type stubDecider struct {
	decision markup.Decision
	err      error
}

func (s stubDecider) Decide(context.Context, markup.Request) (markup.Decision, error) {
	return s.decision, s.err
}

// stubRule returns a configured (allowed, reason) and counts calls.
// The calls counter lets tests assert ordering and short-circuit
// behavior without relying on log inspection.
type stubRule struct {
	allowed bool
	reason  string
	calls   *int
}

func (r stubRule) Check(context.Context, markup.Decision, markup.Request) (bool, string) {
	if r.calls != nil {
		*r.calls++
	}
	return r.allowed, r.reason
}

func TestGuardrailsPassThroughAllowedDecision(t *testing.T) {
	want := markup.Decision{MarkupFactor: 1.25, Rule: "rule-a"}
	d := guardrails.New(
		stubDecider{decision: want},
		stubRule{allowed: true},
		stubRule{allowed: true},
	)

	got, err := d.Decide(context.Background(), markup.Request{})
	if err != nil {
		t.Fatalf("Decide returned error: %v", err)
	}
	if got != want {
		t.Fatalf("Decision = %#v, want %#v", got, want)
	}
}

func TestGuardrailsVetoSurfacesAsErrGuardrailViolation(t *testing.T) {
	d := guardrails.New(
		stubDecider{decision: markup.Decision{MarkupFactor: 5.0}},
		stubRule{allowed: false, reason: "factor 5.00 above max 3.00"},
	)

	got, err := d.Decide(context.Background(), markup.Request{})
	if err == nil {
		t.Fatal("Decide returned nil error; want ErrGuardrailViolation")
	}
	if !errors.Is(err, guardrails.ErrGuardrailViolation) {
		t.Fatalf("errors.Is(err, ErrGuardrailViolation) = false; err = %v", err)
	}
	wantMsg := "guardrails: decision vetoed: factor 5.00 above max 3.00"
	if err.Error() != wantMsg {
		t.Fatalf("err.Error() = %q, want %q", err.Error(), wantMsg)
	}
	if got != (markup.Decision{}) {
		t.Fatalf("Decision on veto = %#v, want zero", got)
	}
}

func TestGuardrailsPassThroughInnerError(t *testing.T) {
	calls := 0
	d := guardrails.New(
		stubDecider{err: markup.ErrNoMatch},
		stubRule{allowed: false, reason: "should not be consulted", calls: &calls},
	)

	_, err := d.Decide(context.Background(), markup.Request{})
	if !errors.Is(err, markup.ErrNoMatch) {
		t.Fatalf("errors.Is(err, ErrNoMatch) = false; err = %v", err)
	}
	if errors.Is(err, guardrails.ErrGuardrailViolation) {
		t.Fatal("inner ErrNoMatch was wrapped as ErrGuardrailViolation")
	}
	if calls != 0 {
		t.Fatalf("rule was consulted on inner error; calls = %d", calls)
	}
}

func TestGuardrailsRunsRulesInOrderUntilFirstVeto(t *testing.T) {
	firstCalls, secondCalls, thirdCalls := 0, 0, 0
	d := guardrails.New(
		stubDecider{decision: markup.Decision{MarkupFactor: 1.5}},
		stubRule{allowed: true, calls: &firstCalls},
		stubRule{allowed: false, reason: "second rule veto", calls: &secondCalls},
		stubRule{allowed: false, reason: "third rule veto", calls: &thirdCalls},
	)

	_, err := d.Decide(context.Background(), markup.Request{})
	if err == nil || err.Error() != "guardrails: decision vetoed: second rule veto" {
		t.Fatalf("err = %v, want wrap with second rule's reason", err)
	}
	if firstCalls != 1 {
		t.Fatalf("first rule calls = %d, want 1", firstCalls)
	}
	if secondCalls != 1 {
		t.Fatalf("second rule calls = %d, want 1", secondCalls)
	}
	if thirdCalls != 0 {
		t.Fatalf("third rule was consulted after veto; calls = %d", thirdCalls)
	}
}

// ctxRule captures the context it receives so a test can assert the
// Decider propagated the caller's ctx rather than substituting one of
// its own (e.g., a regression introducing context.Background() inside
// Decide).
type ctxRule struct {
	captured context.Context
}

func (r *ctxRule) Check(ctx context.Context, _ markup.Decision, _ markup.Request) (bool, string) {
	r.captured = ctx
	return true, ""
}

func TestGuardrailsRulePropagatesContext(t *testing.T) {
	type ctxKey struct{}
	rule := &ctxRule{}
	d := guardrails.New(stubDecider{decision: markup.Decision{MarkupFactor: 1.0}}, rule)

	ctx := context.WithValue(context.Background(), ctxKey{}, "sentinel")
	if _, err := d.Decide(ctx, markup.Request{}); err != nil {
		t.Fatalf("Decide returned error: %v", err)
	}
	if got, _ := rule.captured.Value(ctxKey{}).(string); got != "sentinel" {
		t.Fatalf("rule received ctx value = %q, want %q", got, "sentinel")
	}
}

func TestGuardrailsEmptyReasonStillWrapsSentinel(t *testing.T) {
	d := guardrails.New(
		stubDecider{decision: markup.Decision{MarkupFactor: 1.0}},
		stubRule{allowed: false, reason: ""},
	)

	_, err := d.Decide(context.Background(), markup.Request{})
	if !errors.Is(err, guardrails.ErrGuardrailViolation) {
		t.Fatalf("errors.Is(err, ErrGuardrailViolation) = false; err = %v", err)
	}
	// Empty reason produces a trailing ": " in the wrap. This is
	// intentional -- rules with no reason are misconfigured, and the
	// trailing colon makes the gap visible in logs.
	wantMsg := "guardrails: decision vetoed: "
	if err.Error() != wantMsg {
		t.Fatalf("err.Error() = %q, want %q", err.Error(), wantMsg)
	}
}

func TestGuardrailsNoRulesPassesEveryDecision(t *testing.T) {
	want := markup.Decision{MarkupFactor: 2.5, Rule: "any"}
	d := guardrails.New(stubDecider{decision: want})

	got, err := d.Decide(context.Background(), markup.Request{})
	if err != nil {
		t.Fatalf("Decide returned error with no rules: %v", err)
	}
	if got != want {
		t.Fatalf("Decision = %#v, want %#v", got, want)
	}
}
