// Package shadow holds an optional challenger markup.Decider
// alongside the active champion. Unlike swap.Decider, the zero
// Holder carries no inner; Get reports presence so /decide can
// fast-path when no challenger is loaded.
package shadow

import (
	"sync"

	"github.com/helmedeiros/markup-svc/internal/markup"
)

type Holder struct {
	mu    sync.RWMutex
	inner markup.Decider
}

func New() *Holder { return &Holder{} }

func (h *Holder) Load(d markup.Decider) {
	h.mu.Lock()
	h.inner = d
	h.mu.Unlock()
}

func (h *Holder) Clear() {
	h.mu.Lock()
	h.inner = nil
	h.mu.Unlock()
}

func (h *Holder) Get() (markup.Decider, bool) {
	h.mu.RLock()
	d := h.inner
	h.mu.RUnlock()
	return d, d != nil
}
