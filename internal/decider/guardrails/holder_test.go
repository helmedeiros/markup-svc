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

// TestHolderConcurrentDecideAndReplace exercises Decide and Replace
// from many goroutines. The writer loops on Replace until every
// reader has finished, so the writer is guaranteed to be running
// while readers execute -- the test does not rely on scheduler
// perturbation from -race to observe both configurations.
//
// Test passes when:
//   - every Decide returns a result consistent with EITHER configuration
//     (never a torn state)
//   - both configurations were observed (passes>0 AND vetoes>0)
//   - the writer completed at least one full alternation pair while
//     readers were running
//   - no data race is reported under -race; no deadlock
func TestHolderConcurrentDecideAndReplace(t *testing.T) {
	const (
		readers       = 16
		decidesPerRdr = 1000
	)
	loose := []guardrails.Rule{guardrails.FactorRange{Min: 0.0, Max: 3.0}}
	tight := []guardrails.Rule{guardrails.FactorRange{Min: 0.0, Max: 0.5}}

	h := guardrails.NewHolder(loose...)
	// Inner returns a Decision with MarkupFactor 1.5: passes loose,
	// fails tight. The Decide result classifies which configuration
	// each reader observed.
	d := h.Wrap(stubDecider{decision: markup.Decision{MarkupFactor: 1.5}})

	var passes, vetoes int64
	stop := make(chan struct{})
	var readersWG, writerWG sync.WaitGroup

	readersWG.Add(readers)
	for r := 0; r < readers; r++ {
		go func() {
			defer readersWG.Done()
			for i := 0; i < decidesPerRdr; i++ {
				_, err := d.Decide(context.Background(), markup.Request{})
				switch {
				case err == nil:
					atomic.AddInt64(&passes, 1)
				case errors.Is(err, guardrails.ErrGuardrailViolation):
					atomic.AddInt64(&vetoes, 1)
				default:
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
		for {
			select {
			case <-stop:
				return
			default:
			}
			if writes%2 == 0 {
				h.Replace(tight)
			} else {
				h.Replace(loose)
			}
			atomic.AddInt64(&writes, 1)
		}
	}()

	readersWG.Wait()
	close(stop)
	writerWG.Wait()

	// Both configurations have to have been observed during the run.
	// All-pass means Replace(tight) never took effect; all-veto means
	// Replace(loose) never took effect or the initial loose configuration
	// was never observed.
	if passes == 0 {
		t.Error("no Decide ever observed the loose configuration; Replace might be a no-op")
	}
	if vetoes == 0 {
		t.Error("no Decide ever observed the tight configuration; reader never raced with writer")
	}
	if passes+vetoes != int64(readers*decidesPerRdr) {
		t.Errorf("passes+vetoes = %d, want %d (some Decides returned an unclassified error)",
			passes+vetoes, readers*decidesPerRdr)
	}
	// Writer should have completed at least one full alternation pair
	// (Replace(tight) followed by Replace(loose)) during the readers'
	// run; an extremely fast machine that finishes all reader iterations
	// before the writer's first cycle would skip both branches and the
	// passes>0 / vetoes>0 asserts above would already catch it.
	if writes < 2 {
		t.Errorf("writer completed %d Replace calls; want >= 2", writes)
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
