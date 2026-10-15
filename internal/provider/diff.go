package provider

import (
	"fmt"
	"strings"
)

// PartialApproval reports whether a review state is an approval that does not
// satisfy the MR's approval rules yet (GitLab: "APPROVED 1/2"). It is
// tracked and shown, but only the approval that completes the requirement is
// worth a ping.
func PartialApproval(state string) bool {
	return strings.HasPrefix(state, "APPROVED ")
}

// Diff is a direct port of the bash script's emit_deltas: compares the
// last-seen state against a fresh CheckResult and returns the events that
// fire as a result. Terminal states short-circuit everything else, exactly
// as the original script's early `echo MERGED; return 1` / `echo CLOSED;
// return 1` did.
func Diff(prev PrevState, cur CheckResult) []Event {
	if cur.State == "merged" {
		return []Event{{Kind: "MERGED", Message: "MERGED"}}
	}
	if cur.State == "closed" {
		return []Event{{Kind: "CLOSED", Message: "CLOSED"}}
	}

	events := activityEvents(prev, cur)
	// Compared by href alongside the bucket, not bucket alone: a fix-push
	// that fails again before the next poll happens to run goes
	// failure -> pending -> failure between two consecutive checks, so
	// prev.CIBucket and cur.CIBucket are both "failure" even though it's a
	// different run. cur.CIHref (a per-run job/pipeline URL on both
	// providers) still differs in that case, so the second failure still
	// reports instead of silently matching the first one's already-seen
	// bucket.
	if cur.CIBucket == "failure" && (cur.CIBucket != prev.CIBucket || cur.CIHref != prev.CIHref) {
		events = append(events, Event{Kind: "CI", Message: cur.CIBucket, Href: cur.CIHref})
	}
	// CI going green again after a failure. prev.CIBucket alone can't tell:
	// a fix push reruns CI, so the poll that finally sees "success" usually
	// follows one that saw "pending", hence the sticky CIWasFailing flag.
	if cur.CIBucket == "success" && prev.CIWasFailing {
		events = append(events, Event{Kind: "CI_RECOVERED", Message: "CI is passing again"})
	}
	// CI turning green, whatever it was before. Opt-in (see OptInEventKinds):
	// most sessions only care about a failure or a recovery from one.
	if cur.CIBucket == "success" && prev.CIBucket != "success" {
		events = append(events, Event{Kind: "CI_SUCCESS", Message: "CI is passing", Href: cur.CIHref})
	}
	// A merge queue entry disappearing while the PR still reads as open. GitHub
	// records why in the PR's timeline: "merged" means it merged out of the
	// queue (the PR just has not flipped to merged yet in this poll, and
	// MERGED follows), which is no news; any other reason is named. When the
	// reason is not recorded (yet), the message says only what is known.
	if prev.MergeQueueState != "" && cur.MergeQueueState == "" {
		switch reason := cur.MergeQueueRemovalReason; reason {
		case "merged":
		case "":
			events = append(events, Event{Kind: "MERGE_QUEUE", Message: "removed from merge queue"})
		default:
			events = append(events, Event{Kind: "MERGE_QUEUE", Message: fmt.Sprintf("removed from merge queue (%s)", strings.ReplaceAll(reason, "_", " "))})
		}
	}
	// Only the false -> true transition fires: a still-conflicting PR/MR
	// across consecutive polls has nothing new to say (same reasoning CI's
	// "failure" reports only stay one-shot per state), and a conflict
	// clearing again is silent by design too — the badge already reflects
	// that, and it's not the kind of thing worth a ping the way developing
	// one is.
	if cur.HasConflict && !prev.HasConflict {
		events = append(events, Event{Kind: "CONFLICT", Message: "has a merge conflict with its target branch"})
	}
	// A review that was there last check and isn't anymore, with nothing
	// newer replacing it (that's the REVIEW case above instead) — GitHub's
	// branch-protection "dismiss stale approvals on push" and GitLab's
	// "reset approvals on push" project setting both do this silently:
	// confirmed live, a bot pushing a dependency bump reset an MR's
	// approval with no event at all, so the badge just quietly reverted to
	// "no review" and nobody heard about it. cur.ReviewCount dropping
	// (GitLab: fewer current approvers) or staying flat while the review
	// itself vanishes (GitHub: the same review re-fetched now reports
	// DISMISSED, which buildCheckResult already treats as no review) both
	// land here the same way, since this only compares the review fields
	// themselves, never ReviewCount.
	if prev.LatestReviewState != "" && !PartialApproval(prev.LatestReviewState) && cur.LatestReviewState == "" {
		events = append(events, Event{
			Kind:    "REVIEW_DISMISSED",
			Message: fmt.Sprintf("%s's %s review was dismissed", prev.LatestReviewer, prev.LatestReviewState),
			Href:    prev.ReviewHref,
		})
	}
	return events
}

// activityEvents are the events about what people did on a PR/MR: new
// reviews, comments, threads and thread replies. They apply to a finished
// (merged or closed) PR/MR as much as to an open one.
func activityEvents(prev PrevState, cur CheckResult) []Event {
	var events []Event
	// A partial approval stays silent; the review that completes the
	// requirement fires, whether by the count rising or by the requirement
	// dropping under an existing approval.
	reviewFired := cur.LatestReviewer != "" && !PartialApproval(cur.LatestReviewState) &&
		(cur.ReviewCount > prev.ReviewCount || PartialApproval(prev.LatestReviewState))
	// A COMMENTED review with no text and no new thread has nothing to read
	// (bots submit those): it stays silent, though still tracked. Verdict
	// reviews always fire, and replies it carries are reported by their own
	// events since reviewFired is then false.
	if reviewFired && cur.LatestReviewState == "COMMENTED" && strings.TrimSpace(cur.LatestReviewBody) == "" &&
		len(newIDsIn(prev.UnresolvedThreadIDs, cur.UnresolvedThreadIDs)) == 0 {
		reviewFired = false
	}
	// Compared by identity, not just count: if one thread gets resolved
	// while another new one appears in the same poll window, a plain count
	// comparison nets to "no change" and silently swallows the new thread.
	// newIDsIn catches it regardless of what else resolved in the same
	// window; the same helper is reused below for top-level comments, which
	// need the identical guarantee for the identical reason.
	//
	// Both suppressed when a REVIEW fires in the same tick: confirmed live,
	// a COMMENTED review whose only content is an inline comment fires both
	// a new unresolved thread and a new review in the same poll — same
	// ReviewHref on both — so the "new thread" isn't independent activity,
	// just that review's own comment. A review submission commonly posts a
	// top-level walkthrough comment alongside it too (e.g. coderabbit), so
	// the same reasoning applies there. The REVIEW event alone (which
	// points at the same review) is enough to send someone to look.
	if newIDs := newIDsIn(prev.UnresolvedThreadIDs, cur.UnresolvedThreadIDs); len(newIDs) > 0 && !reviewFired {
		events = append(events, Event{
			Kind:    "COMMENT",
			Message: fmt.Sprintf("%d new unresolved thread(s), %d unresolved total", len(newIDs), len(cur.UnresolvedThreadIDs)),
			Href:    cur.ReviewHref,
		})
	}
	if newComments := newIDsIn(prev.TopLevelCommentIDs, cur.TopLevelCommentIDs); len(newComments) > 0 && !reviewFired {
		events = append(events, Event{
			Kind:    "COMMENT",
			Message: fmt.Sprintf("%d new comment(s), %d total", len(newComments), len(cur.TopLevelCommentIDs)),
			Href:    cur.LatestCommentHref,
		})
	}
	// A reply in a thread that was already open. Comments of a thread that is
	// itself new are covered by the new-thread event above, and a review's own
	// inline replies by its REVIEW event, same as for new threads.
	if prev.ThreadCommentsKnown && !reviewFired {
		if replies := newReplies(prev, cur); len(replies) > 0 {
			noun := "replies"
			if len(replies) == 1 {
				noun = "reply"
			}
			events = append(events, Event{
				Kind:    "COMMENT",
				Message: fmt.Sprintf("%d new thread %s, %d unresolved total", len(replies), noun, len(cur.UnresolvedThreadIDs)),
				Href:    replies[len(replies)-1].Href,
			})
		}
	}
	if reviewFired {
		message := fmt.Sprintf("%s %s", cur.LatestReviewer, cur.LatestReviewState)
		// The inline comments of a review arrive as threads, not as events of
		// their own (see above), so say how many are open.
		switch n := len(cur.UnresolvedThreadIDs); n {
		case 0:
		case 1:
			message += ", 1 unresolved thread"
		default:
			message += fmt.Sprintf(", %d unresolved threads", n)
		}
		events = append(events, Event{
			Kind:    "REVIEW",
			Message: message,
			Href:    cur.ReviewHref,
		})
	}
	return events
}

// DiffFinished is Diff for a merged or closed PR/MR that is still being
// watched for late activity: only the activity events. Its state was
// reported when it finished, and CI, conflicts and merge queue no longer
// matter.
func DiffFinished(prev PrevState, cur CheckResult) []Event {
	return activityEvents(prev, cur)
}

// newReplies returns the comments cur has in threads that were already open
// at the last check and that were not seen then, oldest first.
func newReplies(prev PrevState, cur CheckResult) []ThreadComment {
	seenThread := make(map[string]bool, len(prev.UnresolvedThreadIDs))
	for _, id := range prev.UnresolvedThreadIDs {
		seenThread[id] = true
	}
	seenComment := make(map[string]bool, len(prev.ThreadCommentIDs))
	for _, id := range prev.ThreadCommentIDs {
		seenComment[id] = true
	}
	var out []ThreadComment
	for _, c := range cur.ThreadComments {
		if seenThread[c.ThreadID] && !seenComment[c.ID] {
			out = append(out, c)
		}
	}
	return out
}

// newIDsIn returns the ids present in cur but not in prev — genuinely new
// since the last check, independent of how many other ids dropped out (were
// resolved, or otherwise vanished) in the same window. Shared by the
// unresolved-thread and top-level-comment comparisons above.
func newIDsIn(prev, cur []string) []string {
	prevSet := make(map[string]bool, len(prev))
	for _, id := range prev {
		prevSet[id] = true
	}
	var newIDs []string
	for _, id := range cur {
		if !prevSet[id] {
			newIDs = append(newIDs, id)
		}
	}
	return newIDs
}

// NextCIWasFailing is the CIWasFailing flag to persist after a poll: set by
// a failure, held through any non-success bucket (pending, none,
// action_required), cleared by success — the moment CI_RECOVERED fires.
func NextCIWasFailing(prev PrevState, cur CheckResult) bool {
	switch cur.CIBucket {
	case "failure":
		return true
	case "success":
		return false
	default:
		return prev.CIWasFailing
	}
}
