package provider

import "time"

// PrevState is the last-seen shape of a registration, as persisted in the
// registry. CheckResult is compared against it to detect new activity.
type PrevState struct {
	ReviewCount         int
	CIBucket            string
	UnresolvedThreadIDs []string // IDs of threads unresolved as of the last check; compared by identity, not count, so a resolve+new-thread happening in the same window can't cancel out and hide the new one
	MergeQueueState     string
}

// CheckResult is one provider's raw poll result for a single PR/MR.
type CheckResult struct {
	State               string // "open" | "merged" | "closed"
	Title               string
	Description         string // the PR/MR body, empty if the provider returned none
	ReviewCount         int
	LatestReviewer      string
	LatestReviewState   string
	LatestReviewAt      time.Time // zero if unknown or no review yet
	CIBucket            string    // "none" | "success" | "failure" | "pending" | "action_required"
	CIDetail            string    // e.g. the failing check/job name; empty if not applicable/unknown
	CIHref              string    // link to the relevant job, or failing that the pipeline/run itself; empty if unknown
	ReviewHref          string    // link to the latest review/comment; empty if unknown
	UnresolvedThreadIDs []string  // IDs of currently-unresolved review/discussion threads; empty if none or the provider doesn't track them
	MergedAt            time.Time // zero unless State == "merged"
	HasConflict         bool      // true if the PR/MR can't currently be merged due to a conflict with its target branch
	MergeQueueState     string    // GitHub merge queue entry state ("QUEUED" | "AWAITING_CHECKS" | "LOCKED" | "MERGEABLE" | "UNMERGEABLE"); empty if not in a merge queue (also always empty for GitLab, which has no merge-queue support here)
	MergeQueuePosition  int       // this entry's position in the merge queue; meaningless when MergeQueueState == ""
	Blocked             bool      // GitLab only: true if blocked on an unmerged merge request dependency (always false for GitHub, which has no such dependency feature)
}

type Event struct {
	Kind    string // "REVIEW" | "COMMENT" | "CI" | "MERGED" | "CLOSED" | "MERGE_QUEUE"
	Message string
	Href    string // link to the source of this event (comment, review, or CI job/run); empty if none applies
}

// RateBudget is a snapshot of a rate-limited pool's remaining headroom, as
// reported by the provider on a real API call (never fetched via a
// dedicated polling call).
type RateBudget struct {
	Limit     int
	Remaining int
	ResetAt   time.Time
}
