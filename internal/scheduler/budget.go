package scheduler

import (
	"time"

	"github.com/cwrau/aoe-pr-watch-mcp/internal/provider"
)

const (
	// MinPollInterval matches the original pr-watch bash script's fixed
	// POLL_INTERVAL: never check a single registration faster than this,
	// even if the budget math would allow it.
	MinPollInterval = 5 * time.Minute

	// FallbackInterval is used when there's no rate-limit signal to budget
	// against yet: before the first real call for a pool (bootstrap), or on
	// a GitLab host that has throttling disabled and returns no
	// RateLimit-* headers at all.
	FallbackInterval = 10 * time.Minute

	// RateLimitFloorFrac is the reserved fraction of a pool's limit that
	// must never be spent: usage is paced to stay at or under
	// (1 - RateLimitFloorFrac) of the limit.
	RateLimitFloorFrac = 0.10

	// ReservedCallsPerMinute is the minimum call rate reserved for
	// something else sharing this rate-limit pool (a human running gh/glab
	// directly, say) to keep working at, sustained until the window
	// resets. The reserved floor is whichever is larger: RateLimitFloorFrac
	// of the limit, or enough headroom to sustain this rate for the rest
	// of the window — so a small pool with a long window left still
	// guarantees a usable minimum rate, not just a thin percentage of it.
	ReservedCallsPerMinute = 10.0
)

// ComputeInterval returns how often a single registration drawing from this
// budget pool should be checked, given activeRegistrations total consumers
// sharing the same pool. It spreads the pool's usable headroom (remaining
// minus the reserved floor — the larger of RateLimitFloorFrac of the limit
// or ReservedCallsPerMinute sustained for the rest of the window) evenly
// across the time left until reset, then scales that pool-wide gap up by
// activeRegistrations so each individual registration is checked roughly
// that often — and never faster than MinPollInterval.
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

	floorFrac := int(float64(budget.Limit) * RateLimitFloorFrac)
	floorRate := int(ReservedCallsPerMinute * window.Minutes())
	floor := max(floorFrac, floorRate)
	usable := budget.Remaining - floor
	if usable <= 0 {
		// No headroom left this window: don't check again until reset.
		// MinPollInterval is deliberately NOT the floor here — it caps how
		// fast we may check when there's normal headroom to spend, which is
		// the opposite situation from "we just burned through the reserved
		// floor." Falling back to it would mean the zero-budget case can
		// retry sooner than a merely-tight-but-nonzero-budget case above,
		// which is backwards. If reset itself is sooner than a sane
		// fallback wait (a very short window), use FallbackInterval
		// instead — same "no good signal to plan around" reasoning as the
		// no-rate-limit-data case above, and it's longer than
		// MinPollInterval by construction.
		if window < FallbackInterval {
			return FallbackInterval
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
