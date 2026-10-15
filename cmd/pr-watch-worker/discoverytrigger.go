package main

import (
	"slices"
	"sync"
	"time"
)

// discoveryTrigger turns "session X went idle" notifications (which arrive
// on the stdin goroutine) into a single debounced wake-up for the scheduler
// loop, so the discovery pass itself runs on the loop's goroutine alongside
// Tick — the scheduler's budget caches aren't synchronized. Everything that
// goes idle within one delay window is coalesced into one wake-up.
type discoveryTrigger struct {
	// C carries one wake-up per burst; the receiver calls Drain for the ids.
	C chan struct{}

	delay   time.Duration
	mu      sync.Mutex
	pending map[string]struct{}
	armed   bool
}

func newDiscoveryTrigger(delay time.Duration) *discoveryTrigger {
	return &discoveryTrigger{C: make(chan struct{}, 1), delay: delay, pending: make(map[string]struct{})}
}

// Add records sessionID and, unless a wake-up is already pending, schedules
// one delay from now.
func (t *discoveryTrigger) Add(sessionID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pending[sessionID] = struct{}{}
	if t.armed {
		return
	}
	t.armed = true
	time.AfterFunc(t.delay, func() {
		t.mu.Lock()
		t.armed = false
		t.mu.Unlock()
		select {
		case t.C <- struct{}{}:
		default: // a wake-up is already waiting; Drain will see everything
		}
	})
}

// Drain returns the distinct session ids recorded since the last Drain,
// sorted, and clears them.
func (t *discoveryTrigger) Drain() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	ids := make([]string, 0, len(t.pending))
	for id := range t.pending {
		ids = append(ids, id)
	}
	clear(t.pending)
	slices.Sort(ids)
	return ids
}
