package scheduler

import (
	"time"

	"github.com/cwrau/aoe-pr-watch-mcp/internal/provider"
)

const (
	// MinPollInterval matches the original pr-watch bash script's fixed
	// POLL_INTERVAL: never check a single registration faster than this,
	// even if the budget math would allow it.
	MinPollInterval = 30 * time.Second

	// FallbackInterval is used when there's no rate-limit signal to budget
	// against yet: before the first real call for a pool (bootstrap), or on
	// a GitLab host that has throttling disabled and returns no
	// RateLimit-* headers at all.
	FallbackInterval = 5 * time.Minute

	// RateLimitFloorFrac is the reserved fraction of a pool's limit that
	// must never be spent: usage is paced to stay at or under
	// (1 - RateLimitFloorFrac) of the limit.
	RateLimitFloorFrac = 0.10
)

// ComputeInterval returns how often a single registration drawing from this
// budget pool should be checked, given activeRegistrations total consumers
// sharing the same pool. It spreads the pool's usable headroom (remaining
// minus the reserved floor) evenly across the time left until reset, then
// scales that pool-wide gap up by activeRegistrations so each individual
// registration is checked roughly that often — and never faster than
// MinPollInterval.
func ComputeInterval(budget provider.RateBudget, activeRegistrations int, now time.Time) time.Duration {
	if activeRegistrations < 1 {
		activeRegistrations = 1
	}
	if budget.Limit == 0 || budget.ResetAt.IsZero() {
		return FallbackInterval
	}

	window := budget.ResetAt.Sub(now)
	if window <= 0 {
		return FallbackInterval
	}

	floor := int(float64(budget.Limit) * RateLimitFloorFrac)
	usable := budget.Remaining - floor
	if usable <= 0 {
		// No headroom left this window: don't check again until reset.
		// But still respect the MinPollInterval floor.
		if window < MinPollInterval {
			return MinPollInterval
		}
		return window
	}

	poolWideGap := window / time.Duration(usable)
	perRegistration := poolWideGap * time.Duration(activeRegistrations)
	if perRegistration < MinPollInterval {
		return MinPollInterval
	}
	return perRegistration
}
