package guardrails_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/helmedeiros/markup-svc/internal/decider/guardrails"
	"github.com/helmedeiros/markup-svc/internal/markup"
)

func TestHolderEmptyPassesEveryDecision(t *testing.T) {
	want := markup.Decision{MarkupFactor: 1.5, Rule: "any"}
	d := guardrails.NewHolder().Wrap(stubDecider{decision: want})

	got, err := d.Decide(context.Background(), markup.Request{})
	if err != nil {
		t.Fatalf("Decide returned error: %v", err)
	}
	if got != want {
		t.Fatalf("Decision = %#v, want %#v", got, want)
	}
}

func TestHolderInnerErrorPassesThrough(t *testing.T) {
	h := guardrails.NewHolder(guardrails.FactorRange{Min: 0.5, Max: 3.0})
	d := h.Wrap(stubDecider{err: markup.ErrNoMatch})

	_, err := d.Decide(context.Background(), markup.Request{})
	if !errors.Is(err, markup.ErrNoMatch) {
		t.Fatalf("err = %v, want ErrNoMatch", err)
	}
	if errors.Is(err, guardrails.ErrGuardrailViolation) {
		t.Fatal("inner ErrNoMatch was wrapped as ErrGuardrailViolation")
	}
}

func TestHolderReplaceObservedByNextDecide(t *testing.T) {
	h := guardrails.NewHolder(guardrails.FactorRange{Min: 0.0, Max: 1.0})
	d := h.Wrap(stubDecider{decision: markup.Decision{MarkupFactor: 0.9}})

	// 0.9 is in [0, 1] -- passes.
	if _, err := d.Decide(context.Background(), markup.Request{}); err != nil {
		t.Fatalf("first Decide: %v", err)
	}

	// Tighten the range so 0.9 is no longer allowed.
	h.Replace([]guardrails.Rule{guardrails.FactorRange{Min: 0.0, Max: 0.5}})

	_, err := d.Decide(context.Background(), markup.Request{})
	if !errors.Is(err, guardrails.ErrGuardrailViolation) {
		t.Fatalf("post-Replace Decide err = %v, want ErrGuardrailViolation", err)
	}
}

func TestHolderReplaceToEmptyPassesEveryDecision(t *testing.T) {
	h := guardrails.NewHolder(guardrails.FactorRange{Min: 0.0, Max: 0.5})
	d := h.Wrap(stubDecider{decision: markup.Decision{MarkupFactor: 0.9}})

	// Pre-Replace: 0.9 vetoed.
	if _, err := d.Decide(context.Background(), markup.Request{}); !errors.Is(err, guardrails.ErrGuardrailViolation) {
		t.Fatalf("pre-Replace err = %v, want ErrGuardrailViolation", err)
	}

	h.Replace(nil)

	if _, err := d.Decide(context.Background(), markup.Request{}); err != nil {
		t.Fatalf("post-Replace(nil) Decide returned error: %v", err)
	}
}

func TestHolderSnapshotIsDefensiveCopy(t *testing.T) {
	rule := guardrails.FactorRange{Min: 0.5, Max: 3.0}
	h := guardrails.NewHolder(rule)

	snap := h.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("len(snap) = %d, want 1", len(snap))
	}

	// Mutate the snapshot. The Holder must be unaffected.
	snap[0] = guardrails.FactorRange{Min: 0, Max: 0}

	again := h.Snapshot()
	if again[0] != rule {
		t.Fatalf("mutating Snapshot result leaked into Holder; got %+v, want %+v", again[0], rule)
	}
}

func TestNewHolderInputSliceMutationDoesNotLeakIn(t *testing.T) {
	input := []guardrails.Rule{guardrails.FactorRange{Min: 0.5, Max: 3.0}}
	h := guardrails.NewHolder(input...)

	// Mutate the caller's slice after construction; the Holder's view
	// should be the original.
	input[0] = guardrails.FactorRange{Min: 9.0, Max: 9.0}

	snap := h.Snapshot()
	fr := snap[0].(guardrails.FactorRange)
	if fr.Min != 0.5 || fr.Max != 3.0 {
		t.Fatalf("input mutation leaked into Holder; got %+v, want {0.5, 3.0}", fr)
	}
}

func TestHolderReplaceInputSliceMutationDoesNotLeakIn(t *testing.T) {
	h := guardrails.NewHolder()
	rules := []guardrails.Rule{guardrails.FactorRange{Min: 0.5, Max: 3.0}}
	h.Replace(rules)

	rules[0] = guardrails.FactorRange{Min: 9.0, Max: 9.0}

	snap := h.Snapshot()
	fr := snap[0].(guardrails.FactorRange)
	if fr.Min != 0.5 || fr.Max != 3.0 {
		t.Fatalf("Replace input mutation leaked; got %+v, want {0.5, 3.0}", fr)
	}
}

// TestHolderReplaceSequentialObservability proves Replace actually
// swaps the active rules and is observed by the next Decide. The
// test is fully sequential -- no goroutines, no scheduler dependency
// -- so it cannot flake. The race-free concurrent behavior is
// covered separately by TestHolderConcurrentDecideAndReplaceRaceFree
// below, which asserts only race-cleanliness, not observation
// timing.
func TestHolderReplaceSequentialObservability(t *testing.T) {
	loose := []guardrails.Rule{guardrails.FactorRange{Min: 0.0, Max: 3.0}}
	tight := []guardrails.Rule{guardrails.FactorRange{Min: 0.0, Max: 0.5}}

	h := guardrails.NewHolder(loose...)
	d := h.Wrap(stubDecider{decision: markup.Decision{MarkupFactor: 1.5}})

	// 1.5 in [0, 3] -- passes.
	if _, err := d.Decide(context.Background(), markup.Request{}); err != nil {
		t.Fatalf("pre-Replace Decide returned error: %v", err)
	}

	// Tighten -- next Decide must observe the new bounds.
	h.Replace(tight)
	if _, err := d.Decide(context.Background(), markup.Request{}); !errors.Is(err, guardrails.ErrGuardrailViolation) {
		t.Fatalf("post-tight Decide err = %v, want ErrGuardrailViolation", err)
	}

	// Loosen again -- next Decide must observe the relaxed bounds.
	h.Replace(loose)
	if _, err := d.Decide(context.Background(), markup.Request{}); err != nil {
		t.Fatalf("post-loose Decide returned error: %v", err)
	}
}

// TestHolderConcurrentDecideAndReplaceRaceFree exercises Decide and
// Replace from many goroutines simultaneously. The test runs under
// -race; its job is to surface any data race or deadlock in the
// minimum-lock-hold pattern. The test does NOT assert that both
// configurations were observed in the read stream -- that's the
// scheduler's prerogative, not a correctness invariant. Replace's
// observability is pinned by TestHolderReplaceSequentialObservability
// above, which is fully deterministic.
//
// Test passes when:
//   - no Decide returns an unclassified error (only nil or wrapped sentinel)
//   - no data race is reported under -race; no deadlock
//   - the writer completed at least one Replace while readers ran
//     (otherwise the test would not actually be exercising the
//     concurrent path)
func TestHolderConcurrentDecideAndReplaceRaceFree(t *testing.T) {
	const (
		readers       = 16
		decidesPerRdr = 1000
	)
	loose := []guardrails.Rule{guardrails.FactorRange{Min: 0.0, Max: 3.0}}
	tight := []guardrails.Rule{guardrails.FactorRange{Min: 0.0, Max: 0.5}}

	h := guardrails.NewHolder(loose...)
	d := h.Wrap(stubDecider{decision: markup.Decision{MarkupFactor: 1.5}})

	stop := make(chan struct{})
	var readersWG, writerWG sync.WaitGroup

	readersWG.Add(readers)
	for r := 0; r < readers; r++ {
		go func() {
			defer readersWG.Done()
			for i := 0; i < decidesPerRdr; i++ {
				_, err := d.Decide(context.Background(), markup.Request{})
				if err != nil && !errors.Is(err, guardrails.ErrGuardrailViolation) {
					t.Errorf("unexpected error: %v", err)
					return
				}
			}
		}()
	}

	var writes int64
	writerWG.Add(1)
	go func() {
		defer writerWG.Done()
		i := int64(0)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if i%2 == 0 {
				h.Replace(tight)
			} else {
				h.Replace(loose)
			}
			i++
			atomic.StoreInt64(&writes, i)
		}
	}()

	readersWG.Wait()
	close(stop)
	writerWG.Wait()

	// The writer keeps replacing until the readers finish, so at
	// least one Replace had to happen during the readers' run -- if
	// not, the test isn't exercising the concurrent path at all.
	if atomic.LoadInt64(&writes) == 0 {
		t.Error("writer completed zero Replace calls; concurrent path not exercised")
	}
}

// TestHolderConcurrentSnapshotAndReplace exercises the
// GET /admin/guardrails path under concurrent admin writes.
// Snapshot must always return a valid, internally-consistent rule
// slice -- never the partially-replaced state of the Holder.
func TestHolderConcurrentSnapshotAndReplace(t *testing.T) {
	const readers = 8
	configs := [][]guardrails.Rule{
		{guardrails.FactorRange{Min: 0.0, Max: 1.0}},
		{guardrails.FactorRange{Min: 0.0, Max: 2.0}, guardrails.AllowedCountries{Countries: []string{"BR"}}},
		nil,
	}

	h := guardrails.NewHolder(configs[0]...)

	stop := make(chan struct{})
	var readersWG, writerWG sync.WaitGroup

	readersWG.Add(readers)
	for r := 0; r < readers; r++ {
		go func() {
			defer readersWG.Done()
			for i := 0; i < 500; i++ {
				snap := h.Snapshot()
				// Snapshot must always be one of the three configured
				// states -- never partial. We pin the cardinality since
				// the configs have distinct lengths (1, 2, 0).
				switch len(snap) {
				case 0, 1, 2:
					// OK
				default:
					t.Errorf("Snapshot returned slice of length %d; want 0, 1, or 2", len(snap))
					return
				}
			}
		}()
	}

	writerWG.Add(1)
	go func() {
		defer writerWG.Done()
		i := 0
		for {
			select {
			case <-stop:
				return
			default:
			}
			h.Replace(configs[i%len(configs)])
			i++
		}
	}()

	readersWG.Wait()
	close(stop)
	writerWG.Wait()
}

// TestNewHolderSnapshotReturnsEmptyNonNilSlice pins the contract the
// admin GET handler relies on: Snapshot of a fresh Holder is the empty
// slice, not nil, so encoding/json emits `[]` rather than `null`.
func TestNewHolderSnapshotReturnsEmptyNonNilSlice(t *testing.T) {
	h := guardrails.NewHolder()
	snap := h.Snapshot()
	if snap == nil {
		t.Fatal("Snapshot returned nil; want empty non-nil slice for clean JSON encoding")
	}
	if len(snap) != 0 {
		t.Fatalf("len(snap) = %d, want 0", len(snap))
	}
}
