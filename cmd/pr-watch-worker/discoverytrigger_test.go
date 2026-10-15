package main

import (
	"slices"
	"testing"
	"time"
)

// TestDiscoveryTriggerCoalescesABurst: a session flapping idle/running, or
// several sessions going idle together, must produce one wake-up carrying
// the distinct session ids, not one per event.
func TestDiscoveryTriggerCoalescesABurst(t *testing.T) {
	trig := newDiscoveryTrigger(20 * time.Millisecond)
	trig.Add("b")
	trig.Add("a")
	trig.Add("b")

	select {
	case <-trig.C:
	case <-time.After(time.Second):
		t.Fatal("no wake-up within 1s of Add")
	}
	if got := trig.Drain(); !slices.Equal(got, []string{"a", "b"}) {
		t.Errorf("Drain() = %v, want [a b] (distinct, sorted)", got)
	}
	select {
	case <-trig.C:
		t.Error("second wake-up for one burst, want exactly one")
	case <-time.After(100 * time.Millisecond):
	}
	if got := trig.Drain(); len(got) != 0 {
		t.Errorf("Drain() after drain = %v, want empty", got)
	}
}

func TestDiscoveryTriggerRearmsAfterFiring(t *testing.T) {
	trig := newDiscoveryTrigger(10 * time.Millisecond)
	trig.Add("a")
	<-trig.C
	trig.Drain()

	trig.Add("b")
	select {
	case <-trig.C:
	case <-time.After(time.Second):
		t.Fatal("no wake-up for an Add after the first burst fired")
	}
	if got := trig.Drain(); !slices.Equal(got, []string{"b"}) {
		t.Errorf("Drain() = %v, want [b]", got)
	}
}
