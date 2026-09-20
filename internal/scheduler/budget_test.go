package scheduler

import (
	"testing"
	"time"

	"github.com/cwrau/aoe-pr-watch-mcp/internal/provider"
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
	// sub-second interval. The 30s floor must still win.
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
	budget := provider.RateBudget{Limit: 5000, Remaining: 550, ResetAt: now.Add(time.Hour)}
	one := ComputeInterval(budget, 1, now)
	ten := ComputeInterval(budget, 10, now)
	if ten <= one {
		t.Errorf("ComputeInterval() with 10 active registrations (%v) should be longer than with 1 (%v) — same pool, more consumers, less-frequent-per-registration checks", ten, one)
	}
}

func TestComputeIntervalNeverPlansToExceedNinetyPercentCeiling(t *testing.T) {
	now := time.Now()
	budget := provider.RateBudget{Limit: 1000, Remaining: 900, ResetAt: now.Add(time.Hour)}
	activeRegistrations := 20
	interval := ComputeInterval(budget, activeRegistrations, now)

	// Over the remaining window, the pool-wide call rate this interval
	// implies must not exceed the usable (90%-ceiling) budget.
	checksInWindow := time.Hour / interval * time.Duration(activeRegistrations)
	usable := int(float64(budget.Limit) * (1 - RateLimitFloorFrac))
	if int(checksInWindow) > usable {
		t.Errorf("interval %v implies %d calls over the window, want <= usable budget %d", interval, checksInWindow, usable)
	}
}
