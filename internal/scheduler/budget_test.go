package scheduler

import (
	"testing"
	"time"

	"github.com/cwrau/aoe-plugin-pr-watch-mcp/internal/provider"
)

func TestComputeIntervalZeroBudgetUsesFallback(t *testing.T) {
	budget := provider.RateBudget{}
	got := ComputeInterval(budget, 5, time.Now())
	if got != FallbackInterval {
		t.Errorf("ComputeInterval(zero budget) = %v, want fallback %v", got, FallbackInterval)
	}
}

func TestComputeIntervalNeverBelowFloor(t *testing.T) {
	now := time.Now()
	// Huge headroom, tiny window, one registration: naive math would want a
	// sub-second interval. The MinPollInterval floor must still win.
	budget := provider.RateBudget{Limit: 5000, Remaining: 4999, ResetAt: now.Add(time.Second)}
	got := ComputeInterval(budget, 1, now)
	if got < MinPollInterval {
		t.Errorf("ComputeInterval() = %v, want >= floor %v", got, MinPollInterval)
	}
}

func TestComputeIntervalNearFloorWaitsForReset(t *testing.T) {
	now := time.Now()
	// remaining is already at the 10% floor: no usable headroom left this window.
	budget := provider.RateBudget{Limit: 5000, Remaining: 500, ResetAt: now.Add(10 * time.Minute)}
	got := ComputeInterval(budget, 3, now)
	if got < 10*time.Minute {
		t.Errorf("ComputeInterval() = %v, want >= time until reset (%v) when no headroom remains", got, 10*time.Minute)
	}
}

func TestComputeIntervalNoHeadroomShortWindowUsesFallback(t *testing.T) {
	now := time.Now()
	// No usable headroom (Remaining=500 is exactly the floor), and window is
	// only 5s. MinPollInterval must NOT apply here: it's a ceiling on how
	// fast we may check when there IS headroom to spend, not a safe wait
	// for "we just burned through the whole budget" — that gets the longer
	// FallbackInterval instead, same as when there's no rate-limit signal
	// at all.
	budget := provider.RateBudget{Limit: 5000, Remaining: 500, ResetAt: now.Add(5 * time.Second)}
	got := ComputeInterval(budget, 1, now)
	if got != FallbackInterval {
		t.Errorf("ComputeInterval() with short window and no headroom = %v, want fallback %v", got, FallbackInterval)
	}
}

func TestComputeIntervalResetInThePastUsesFallback(t *testing.T) {
	now := time.Now()
	budget := provider.RateBudget{Limit: 5000, Remaining: 100, ResetAt: now.Add(-time.Minute)}
	got := ComputeInterval(budget, 3, now)
	if got != FallbackInterval {
		t.Errorf("ComputeInterval(stale reset) = %v, want fallback %v", got, FallbackInterval)
	}
}

func TestComputeIntervalScalesWithActiveRegistrations(t *testing.T) {
	now := time.Now()
	// Remaining is set low enough (relative to Limit and the 40-minute
	// window) that usable headroom is tiny (5) and the 1-registration
	// interval already clears MinPollInterval on its own — otherwise both
	// the 1- and 10-registration cases would clamp to the same floor and
	// be indistinguishable, which would make this test pass regardless of
	// whether ComputeInterval uses the real count at all. floorRate here
	// (10/min * 40min = 400) stays under floorFrac (10% of 5000 = 500), so
	// the fraction-based floor is what's actually in effect.
	budget := provider.RateBudget{Limit: 5000, Remaining: 505, ResetAt: now.Add(40 * time.Minute)}
	one := ComputeInterval(budget, 1, now)
	ten := ComputeInterval(budget, 10, now)
	if one <= MinPollInterval {
		t.Fatalf("ComputeInterval(1) = %v, want it to clear MinPollInterval (%v) on its own so scaling is actually observable", one, MinPollInterval)
	}
	if ten <= one {
		t.Errorf("ComputeInterval() with 10 active registrations (%v) should be longer than with 1 (%v) — same pool, more consumers, less-frequent-per-registration checks", ten, one)
	}
}

func TestComputeIntervalNeverExceedsReservedFloorCeiling(t *testing.T) {
	now := time.Now()
	budget := provider.RateBudget{Limit: 1000, Remaining: 900, ResetAt: now.Add(time.Hour)}
	activeRegistrations := 20
	interval := ComputeInterval(budget, activeRegistrations, now)

	// Over the remaining window, the pool-wide call rate this interval
	// implies must not exceed the usable budget under the reserved floor —
	// whichever of the fraction- or rate-based reservation is larger.
	checksInWindow := time.Hour / interval * time.Duration(activeRegistrations)
	floorFrac := int(float64(budget.Limit) * RateLimitFloorFrac)
	floorRate := int(ReservedCallsPerMinute * time.Hour.Minutes())
	usable := budget.Limit - max(floorFrac, floorRate)
	if int(checksInWindow) > usable {
		t.Errorf("interval %v implies %d calls over the window, want <= usable budget %d", interval, checksInWindow, usable)
	}
}

func TestComputeIntervalReservedRateFloorDominatesSmallPool(t *testing.T) {
	now := time.Now()
	// A small pool with a long window left: 10% of the limit (5) is much
	// smaller than what 10 calls/min sustained for 30 minutes demands
	// (300), so the rate-based floor must win and starve out all headroom.
	budget := provider.RateBudget{Limit: 50, Remaining: 45, ResetAt: now.Add(30 * time.Minute)}
	got := ComputeInterval(budget, 1, now)
	if got < 30*time.Minute {
		t.Errorf("ComputeInterval() = %v, want >= the time until reset (%v): the rate-based floor should leave no usable headroom here", got, 30*time.Minute)
	}
}

func TestHasHeadroomTrueWithNoSignalYet(t *testing.T) {
	if !HasHeadroom(provider.RateBudget{}, time.Now()) {
		t.Error("HasHeadroom(zero budget) = false, want true (nothing stale to distrust, only a real call can tell us)")
	}
}

func TestHasHeadroomTrueOnceWindowElapsed(t *testing.T) {
	now := time.Now()
	budget := provider.RateBudget{Limit: 5000, Remaining: 100, ResetAt: now.Add(-time.Minute)}
	if !HasHeadroom(budget, now) {
		t.Error("HasHeadroom(elapsed window) = false, want true: a stale reading past its own reset must not block a fresh call")
	}
}

func TestHasHeadroomFalseWhenReservedFloorExhausted(t *testing.T) {
	now := time.Now()
	budget := provider.RateBudget{Limit: 5000, Remaining: 500, ResetAt: now.Add(10 * time.Minute)}
	if HasHeadroom(budget, now) {
		t.Error("HasHeadroom() = true, want false: Remaining is already at the reserved floor")
	}
}

func TestHasHeadroomTrueAboveReservedFloor(t *testing.T) {
	now := time.Now()
	budget := provider.RateBudget{Limit: 5000, Remaining: 4000, ResetAt: now.Add(10 * time.Minute)}
	if !HasHeadroom(budget, now) {
		t.Error("HasHeadroom() = false, want true: well above the reserved floor")
	}
}
