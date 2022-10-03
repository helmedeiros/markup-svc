package markup_test

import (
	"context"
	"errors"
	"testing"

	"github.com/helmedeiros/markup-svc/internal/markup"
)

func TestRequestZeroValue(t *testing.T) {
	var r markup.Request
	if r.Amount != 0 || r.ProductID != "" || r.CustomerTier != "" {
		t.Fatalf("Request zero value should be all empties; got %+v", r)
	}
}

func TestDecisionZeroValue(t *testing.T) {
	var d markup.Decision
	if d.MarkupFactor != 0 || d.Rule != "" || d.EngineAdapter != "" {
		t.Fatalf("Decision zero value should be all empties; got %+v", d)
	}
}

func TestErrNoMatchHasStableMessage(t *testing.T) {
	if markup.ErrNoMatch.Error() != "markup: no rule matched the request" {
		t.Fatalf("ErrNoMatch message drift: %q", markup.ErrNoMatch.Error())
	}
}

// nilDecider is the smallest possible Decider: always returns
// ErrNoMatch with a zero-valued Decision. It proves the Decider
// contract is well-formed before any real adapter exists.
type nilDecider struct{}

func (nilDecider) Decide(context.Context, markup.Request) (markup.Decision, error) {
	return markup.Decision{}, markup.ErrNoMatch
}

func TestNilDeciderSatisfiesPort(t *testing.T) {
	var d markup.Decider = nilDecider{}
	got, err := d.Decide(context.Background(), markup.Request{ProductID: "p1"})
	if !errors.Is(err, markup.ErrNoMatch) {
		t.Fatalf("expected ErrNoMatch; got %v", err)
	}
	if (got != markup.Decision{}) {
		t.Fatalf("expected zero Decision; got %+v", got)
	}
}
