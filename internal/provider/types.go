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
	State             string // "open" | "merged" | "closed"
	Title             string
	Description       string // the PR/MR body, empty if the provider returned none
	ReviewCount       int
	LatestReviewer    string
	LatestReviewState string
	LatestReviewAt    time.Time // zero if unknown or no review yet
	CIBucket          string    // "none" | "success" | "failure" | "pending" | "action_required"
	CIDetail          string    // e.g. the failing check/job name; empty if not applicable/unknown
	Unresolved        int
	MergedAt          time.Time // zero unless State == "merged"
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
