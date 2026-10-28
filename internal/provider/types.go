package provider

import "time"

// PrevState is the last-seen shape of a registration, as persisted in the
// registry. CheckResult is compared against it to detect new activity.
type PrevState struct {
	ReviewCount         int
	CIBucket            string
	CIHref              string   // link to the last-notified CI failure's job/run, compared alongside CIBucket so a fix-then-fail-again cycle that lands on "failure" both times a poll runs is still reported: the two failures are different runs even though the bucket string is identical
	UnresolvedThreadIDs []string // IDs of threads unresolved as of the last check; compared by identity, not count, so a resolve+new-thread happening in the same window can't cancel out and hide the new one
	MergeQueueState     string
	CIWasFailing        bool     // sticky: CI was seen failing and hasn't been seen succeeding since (it survives intermediate pending/none polls); what CI_RECOVERED fires on
	HasConflict         bool     // true if the PR/MR already had a merge conflict as of the last check
	LatestReviewState   string   // the last-seen review's state, e.g. "APPROVED"; empty if there was no review as of the last check
	LatestReviewer      string   // empty if there was no review as of the last check
	ReviewHref          string   // link to the review that's about to disappear, if it does; empty if there was no review as of the last check
	TopLevelCommentIDs  []string // IDs of top-level PR/MR conversation comments seen as of the last check (the Conversation tab, not inline review threads); compared by identity, same reasoning as UnresolvedThreadIDs

	// ThreadCommentIDs are the IDs of the comments in unresolved threads as of
	// the last check, compared by identity to spot a reply in an existing
	// thread. ThreadCommentsKnown is false for a registration that predates
	// this tracking: with no baseline every existing reply would look new, so
	// replies are not reported until one check has stored it.
	ThreadCommentIDs    []string
	ThreadCommentsKnown bool

	// HeadSHA, IsDraft and RequestedReviewers are the baseline for PUSH,
	// READY_FOR_REVIEW/DRAFTED and REVIEW_REQUESTED. An empty HeadSHA means no
	// baseline has been stored yet, and none of those events can fire: with
	// nothing to compare against, the current values would all look new.
	HeadSHA            string
	IsDraft            bool
	RequestedReviewers []string
}

// CheckResult is one provider's raw poll result for a single PR/MR.
type CheckResult struct {
	State                   string // "open" | "merged" | "closed"
	Title                   string
	Description             string // the PR/MR body, empty if the provider returned none
	ReviewCount             int
	LatestReviewer          string
	LatestReviewBody        string // the latest review's own text; empty when it has none (GitHub only, GitLab approvals carry no text)
	LatestReviewState       string
	LatestReviewAt          time.Time         // zero if unknown or no review yet
	CIBucket                string            // "none" | "success" | "failure" | "pending" | "action_required"
	CIDetail                string            // e.g. the failing check/job name; empty if not applicable/unknown
	CIHref                  string            // link to the relevant job, or failing that the pipeline/run itself; empty if unknown
	ReviewHref              string            // link to the latest review/comment; empty if unknown
	UnresolvedThreadIDs     []string          // IDs of currently-unresolved review/discussion threads; empty if none or the provider doesn't track them
	ThreadComments          []ThreadComment   // the (newest) comments of those threads as fetched; replies are detected from it. May be narrowed by ExcludeThreadCommentsBy before diffing, so it is not what gets persisted — ThreadCommentIDs is
	ThreadCommentIDs        []string          // IDs of every comment in ThreadComments as fetched, persisted as the baseline for the next check
	ResolvedBy              map[string]string // thread ID -> login/username of whoever resolved it, for the resolved threads the provider names the resolver of; used to keep a session from being told about its own resolve
	TopLevelComments        []TopLevelComment // the raw top-level comments as fetched; TopLevelCommentIDs/LatestComment* below are derived from it by ApplyCommentFilters
	TopLevelCommentIDs      []string          // IDs of top-level PR/MR conversation comments (the Conversation tab, not inline review threads — e.g. a bot's walkthrough summary); empty if none
	LatestCommentHref       string            // link to the most recent top-level comment; empty if none
	LatestCommentAuthor     string            // login/username of the most recent top-level comment's author; empty if none — used for self-authored-event suppression, same as LatestReviewer
	MergedAt                time.Time         // zero unless State == "merged"
	HasConflict             bool              // true if the PR/MR can't currently be merged due to a conflict with its target branch
	MergeQueueState         string            // GitHub merge queue entry state ("QUEUED" | "AWAITING_CHECKS" | "LOCKED" | "MERGEABLE" | "UNMERGEABLE"); empty if not in a merge queue (also always empty for GitLab, which has no merge-queue support here)
	MergeQueuePosition      int               // this entry's position in the merge queue; meaningless when MergeQueueState == ""
	MergeQueueRemovalReason string            // GitHub's recorded reason the PR left the merge queue ("merged", "invalidated", ...) when its latest queue event is a removal; empty if it is still queued, was never queued, or the removal is not recorded yet
	Blocked                 bool              // GitLab only: true if blocked on an unmerged merge request dependency (always false for GitHub, which has no such dependency feature)
	HeadSHA                 string            // the head commit of the PR/MR's source branch; empty if the provider did not report it
	HeadAuthor              string            // login of the newest commit's author, when the provider says; empty otherwise
	ForcePushed             bool              // true if the head was last moved by a force-push (GitHub only)
	IsDraft                 bool              // true while the PR/MR is a draft
	RequestedReviewers      []string          // logins/usernames a review is currently requested from (teams are not listed)
	SelfReviewSHA           string            // the head commit that review was on, when the provider says (GitHub); empty otherwise
	SelfReviewState         string            // the worker's own account's latest review state ("APPROVED", "CHANGES_REQUESTED", "COMMENTED"), empty if it has none or it was dismissed; GitLab only ever reports "APPROVED"
}

// ThreadComment is one comment inside an unresolved review thread.
type ThreadComment struct {
	ThreadID string
	ID       string
	Author   string
	Href     string
}

// ExcludeThreadCommentsBy drops author's comments from the set Diff looks
// at for new replies, so a session is not pinged about its own replies.
// ThreadCommentIDs is left alone: they still count as seen.
func (r *CheckResult) ExcludeThreadCommentsBy(author string) {
	if author == "" {
		return
	}
	var kept []ThreadComment
	for _, c := range r.ThreadComments {
		if c.Author != author {
			kept = append(kept, c)
		}
	}
	r.ThreadComments = kept
}

// OptInEventKinds are the event kinds a registration only gets when its events
// filter names them: an empty filter means every kind except these.
var OptInEventKinds = map[string]bool{
	"CI_SUCCESS":       true,
	"PUSH":             true,
	"REVIEW_REQUESTED": true,
	"READY_FOR_REVIEW": true,
	"DRAFTED":          true,
	"THREAD_RESOLVED":  true,
}

type Event struct {
	Kind    string // "REVIEW" | "REVIEW_DISMISSED" | "COMMENT" | "CI" | "CI_RECOVERED" | "CI_SUCCESS" | "CONFLICT" | "MERGED" | "CLOSED" | "REOPENED" | "MERGE_QUEUE" | "PUSH" | "REVIEW_REQUESTED" | "READY_FOR_REVIEW" | "DRAFTED" | "THREAD_RESOLVED"
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
