package shadow_test

import (
	"context"
	"sync"
	"testing"

	"github.com/helmedeiros/markup-svc/internal/decider/shadow"
	"github.com/helmedeiros/markup-svc/internal/markup"
)

type stubDecider struct{ id string }

func (s stubDecider) Decide(_ context.Context, _ markup.Request) (markup.Decision, error) {
	return markup.Decision{Rule: s.id}, nil
}

func TestHolderZeroValueReportsAbsent(t *testing.T) {
	h := shadow.New()
	if _, loaded := h.Get(); loaded {
		t.Fatal("zero-value Holder reported a challenger")
	}
}

func TestHolderLoadInstallsChallenger(t *testing.T) {
	h := shadow.New()
	h.Load(stubDecider{id: "alpha"})
	d, loaded := h.Get()
	if !loaded {
		t.Fatal("Load did not install challenger")
	}
	got, _ := d.Decide(context.Background(), markup.Request{})
	if got.Rule != "alpha" {
		t.Fatalf("got rule %q want alpha", got.Rule)
	}
}

func TestHolderClearRemovesChallenger(t *testing.T) {
	h := shadow.New()
	h.Load(stubDecider{id: "alpha"})
	h.Clear()
	if _, loaded := h.Get(); loaded {
		t.Fatal("Clear did not remove challenger")
	}
}

func TestHolderLoadReplacesChallenger(t *testing.T) {
	h := shadow.New()
	h.Load(stubDecider{id: "alpha"})
	h.Load(stubDecider{id: "beta"})
	d, _ := h.Get()
	got, _ := d.Decide(context.Background(), markup.Request{})
	if got.Rule != "beta" {
		t.Fatalf("second Load did not replace inner; got %q", got.Rule)
	}
}

func TestHolderConcurrentGetAndSwap(t *testing.T) {
	h := shadow.New()
	h.Load(stubDecider{id: "alpha"})
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); h.Load(stubDecider{id: "beta"}) }()
		go func() {
			defer wg.Done()
			if d, loaded := h.Get(); loaded {
				_, _ = d.Decide(context.Background(), markup.Request{})
			}
		}()
	}
	wg.Wait()
}
