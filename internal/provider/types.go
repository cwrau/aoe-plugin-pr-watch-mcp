package provider

import "time"

// PrevState is the last-seen shape of a registration, as persisted in the
// registry. CheckResult is compared against it to detect new activity.
type PrevState struct {
	ReviewCount int
	CIBucket    string
	Unresolved  int
}

// CheckResult is one provider's raw poll result for a single PR/MR.
type CheckResult struct {
	State              string // "open" | "merged" | "closed"
	ReviewCount        int
	LatestReviewer     string
	LatestReviewState  string
	CIBucket           string // "none" | "success" | "failure" | "pending" | "action_required"
	Unresolved         int
}

type Event struct {
	Kind    string // "REVIEW" | "COMMENT" | "CI" | "MERGED" | "CLOSED"
	Message string
}

// RateBudget is a snapshot of a rate-limited pool's remaining headroom, as
// reported by the provider on a real API call (never fetched via a
// dedicated polling call).
type RateBudget struct {
	Limit     int
	Remaining int
	ResetAt   time.Time
}
