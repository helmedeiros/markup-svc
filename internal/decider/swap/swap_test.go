package swap_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/helmedeiros/markup-svc/internal/decider/swap"
	"github.com/helmedeiros/markup-svc/internal/markup"
)

type stubDecider struct {
	rule   string
	factor float64
	calls  int64
	err    error
}

func (s *stubDecider) Decide(ctx context.Context, req markup.Request) (markup.Decision, error) {
	atomic.AddInt64(&s.calls, 1)
	if s.err != nil {
		return markup.Decision{}, s.err
	}
	return markup.Decision{Rule: s.rule, MarkupFactor: s.factor}, nil
}

func TestNewHoldsInitial(t *testing.T) {
	stub := &stubDecider{rule: "r0", factor: 1.05}
	h := swap.New(stub)
	got, err := h.Decide(context.Background(), markup.Request{})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if got.Rule != "r0" || got.MarkupFactor != 1.05 {
		t.Errorf("Decision = %+v, want Rule=r0 Factor=1.05", got)
	}
}

func TestDecidePropagatesInnerError(t *testing.T) {
	sentinel := errors.New("boom")
	h := swap.New(&stubDecider{err: sentinel})
	_, err := h.Decide(context.Background(), markup.Request{})
	if !errors.Is(err, sentinel) {
		t.Fatalf("want sentinel, got %v", err)
	}
}

func TestSwapReplacesInner(t *testing.T) {
	first := &stubDecider{rule: "first", factor: 1.10}
	second := &stubDecider{rule: "second", factor: 1.20}
	h := swap.New(first)

	got, _ := h.Decide(context.Background(), markup.Request{})
	if got.Rule != "first" {
		t.Fatalf("pre-swap Rule = %q, want \"first\"", got.Rule)
	}

	h.Swap(second)

	got, _ = h.Decide(context.Background(), markup.Request{})
	if got.Rule != "second" {
		t.Fatalf("post-swap Rule = %q, want \"second\"", got.Rule)
	}
	if got.MarkupFactor != 1.20 {
		t.Errorf("post-swap Factor = %v, want 1.20", got.MarkupFactor)
	}
}

func TestSwapToNilInnerWouldPanicOnNextDecide(t *testing.T) {
	first := &stubDecider{rule: "first"}
	h := swap.New(first)

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on Decide after Swap(nil)")
		}
	}()
	h.Swap(nil)
	_, _ = h.Decide(context.Background(), markup.Request{})
}

// TestSwapUnderConcurrentDecide stresses the holder with many
// goroutines calling Decide while another goroutine repeatedly Swaps
// the inner. The race detector catches any unsynchronized access; the
// counters verify every Decide returns a result from a known Decider.
func TestSwapUnderConcurrentDecide(t *testing.T) {
	a := &stubDecider{rule: "A", factor: 1.0}
	b := &stubDecider{rule: "B", factor: 2.0}
	h := swap.New(a)

	const readers = 16
	const callsPerReader = 500
	const swaps = 50

	var wg sync.WaitGroup
	wg.Add(readers + 1)

	for i := 0; i < readers; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < callsPerReader; j++ {
				got, err := h.Decide(context.Background(), markup.Request{})
				if err != nil {
					t.Errorf("Decide: %v", err)
					return
				}
				if got.Rule != "A" && got.Rule != "B" {
					t.Errorf("unexpected Rule %q", got.Rule)
					return
				}
			}
		}()
	}
	go func() {
		defer wg.Done()
		for j := 0; j < swaps; j++ {
			if j%2 == 0 {
				h.Swap(b)
			} else {
				h.Swap(a)
			}
		}
	}()
	wg.Wait()

	totalCalls := atomic.LoadInt64(&a.calls) + atomic.LoadInt64(&b.calls)
	wantTotal := int64(readers * callsPerReader)
	if totalCalls != wantTotal {
		t.Errorf("total calls across both deciders = %d, want %d", totalCalls, wantTotal)
	}
}

// TestPreSwapDecideRunsOnCapturedInner pins the lock-release-then-
// dispatch behaviour: a Decide that has already passed RLock and
// captured the inner pointer runs to completion on that captured
// inner even if a Swap happens during its execution. The stub blocks
// in Decide until released so the timing is deterministic.
func TestPreSwapDecideRunsOnCapturedInner(t *testing.T) {
	gate := make(chan struct{})
	slow := &blockingDecider{rule: "slow", gate: gate}
	fast := &stubDecider{rule: "fast"}
	h := swap.New(slow)

	resCh := make(chan markup.Decision, 1)
	go func() {
		got, _ := h.Decide(context.Background(), markup.Request{})
		resCh <- got
	}()

	// Give the goroutine time to capture slow as its inner, then swap.
	for atomic.LoadInt64(&slow.entered) == 0 {
		// Yield until the goroutine reaches the gate.
	}
	h.Swap(fast)

	// Release the slow Decide so it returns. It should report slow.
	close(gate)

	got := <-resCh
	if got.Rule != "slow" {
		t.Fatalf("pre-swap Decide returned %q, want \"slow\" (captured inner)", got.Rule)
	}

	// Next call after the swap must see fast.
	got, _ = h.Decide(context.Background(), markup.Request{})
	if got.Rule != "fast" {
		t.Fatalf("post-swap Decide returned %q, want \"fast\"", got.Rule)
	}
}

type blockingDecider struct {
	rule    string
	gate    chan struct{}
	entered int64
}

func (b *blockingDecider) Decide(ctx context.Context, req markup.Request) (markup.Decision, error) {
	atomic.AddInt64(&b.entered, 1)
	<-b.gate
	return markup.Decision{Rule: b.rule}, nil
}
