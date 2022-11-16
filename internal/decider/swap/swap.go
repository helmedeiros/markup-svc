// Package swap provides an in-memory holder around markup.Decider so
// the inner Decider can be replaced at runtime without disrupting
// in-flight Decide calls. The holder itself satisfies markup.Decider
// so callers depend on the same port. See ADR-0008.
package swap

import (
	"context"
	"sync"

	"github.com/helmedeiros/markup-svc/internal/markup"
)

// Decider holds a markup.Decider behind a sync.RWMutex and satisfies
// markup.Decider itself. Decide acquires the read lock just long
// enough to copy the inner pointer, then releases it before calling
// through; in-flight Decides finish on their captured inner while
// new Decides started after a Swap returns see the replacement.
//
// This minimum-lock-hold shape means a concurrent Swap never waits
// for engine work to complete -- the swap takes effect as soon as
// the writer grabs the Lock, and any Decide already past its RLock
// runs concurrently to completion on its captured Decider.
type Decider struct {
	mu    sync.RWMutex
	inner markup.Decider
}

// New returns a holder pre-loaded with initial. initial must not be
// nil; passing nil produces a holder whose Decide panics with a nil
// pointer dereference on first call.
func New(initial markup.Decider) *Decider {
	return &Decider{inner: initial}
}

// Decide implements markup.Decider. Copies the inner pointer under
// RLock, releases the lock, then dispatches. The captured inner stays
// alive through the local variable for the duration of the call.
func (d *Decider) Decide(ctx context.Context, req markup.Request) (markup.Decision, error) {
	d.mu.RLock()
	inner := d.inner
	d.mu.RUnlock()
	return inner.Decide(ctx, req)
}

// Swap replaces the inner Decider atomically. A concurrent Decide
// that has already captured the previous inner runs through to
// completion on it; Decides started after Swap returns observe next.
func (d *Decider) Swap(next markup.Decider) {
	d.mu.Lock()
	d.inner = next
	d.mu.Unlock()
}
