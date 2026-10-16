package provider

import (
	"slices"
	"testing"
)

func TestDiffMergedShortCircuits(t *testing.T) {
	prev := PrevState{ReviewCount: 1, CIBucket: "success"}
	cur := CheckResult{State: "merged", ReviewCount: 5, CIBucket: "failure", UnresolvedThreadIDs: []string{"t1", "t2", "t3"}}
	got := Diff(prev, cur)
	if len(got) != 1 || got[0].Kind != "MERGED" {
		t.Fatalf("Diff() = %+v, want single MERGED event", got)
	}
}

func TestDiffClosedShortCircuits(t *testing.T) {
	prev := PrevState{}
	cur := CheckResult{State: "closed"}
	got := Diff(prev, cur)
	if len(got) != 1 || got[0].Kind != "CLOSED" {
		t.Fatalf("Diff() = %+v, want single CLOSED event", got)
	}
}

func TestDiffNewUnresolvedThreadFiresComment(t *testing.T) {
	prev := PrevState{UnresolvedThreadIDs: []string{"t1"}}
	cur := CheckResult{State: "open", UnresolvedThreadIDs: []string{"t1", "t2", "t3"}}
	got := Diff(prev, cur)
	if len(got) != 1 || got[0].Kind != "COMMENT" || got[0].Message != "2 new unresolved thread(s), 3 unresolved total" {
		t.Fatalf("Diff() = %+v, want single COMMENT event", got)
	}
}

func TestDiffCommentEventCarriesReviewHref(t *testing.T) {
	prev := PrevState{UnresolvedThreadIDs: []string{"t1"}}
	cur := CheckResult{State: "open", UnresolvedThreadIDs: []string{"t1", "t2"}, ReviewHref: "https://github.com/o/r/pull/1#discussion_r1"}
	got := Diff(prev, cur)
	if len(got) != 1 || got[0].Href != "https://github.com/o/r/pull/1#discussion_r1" {
		t.Fatalf("Diff() = %+v, want COMMENT event carrying ReviewHref", got)
	}
}

func TestDiffOnlyResolvedThreadsFiresNothing(t *testing.T) {
	prev := PrevState{UnresolvedThreadIDs: []string{"t1", "t2", "t3"}}
	cur := CheckResult{State: "open", UnresolvedThreadIDs: []string{"t1"}}
	got := Diff(prev, cur)
	if len(got) != 0 {
		t.Fatalf("Diff() = %+v, want no events (t2/t3 resolving, nothing new appearing, is not new activity)", got)
	}
}

// TestDiffNewThreadDetectedDespiteLowerNetCount is the actual bug fix: a
// plain count comparison (cur.Unresolved > prev.Unresolved) would see 3 -> 2
// and conclude nothing new happened, silently swallowing t4's arrival just
// because t2 and t3 happened to resolve in the same poll window. Comparing
// by thread identity instead must still catch it.
func TestDiffNewThreadDetectedDespiteLowerNetCount(t *testing.T) {
	prev := PrevState{UnresolvedThreadIDs: []string{"t1", "t2", "t3"}}
	cur := CheckResult{State: "open", UnresolvedThreadIDs: []string{"t1", "t4"}}
	got := Diff(prev, cur)
	if len(got) != 1 || got[0].Kind != "COMMENT" || got[0].Message != "1 new unresolved thread(s), 2 unresolved total" {
		t.Fatalf("Diff() = %+v, want a COMMENT event for t4 despite the net count dropping from 3 to 2", got)
	}
}

func TestDiffReviewIncreaseFiresReview(t *testing.T) {
	prev := PrevState{ReviewCount: 1}
	cur := CheckResult{State: "open", ReviewCount: 2, LatestReviewer: "alice", LatestReviewState: "APPROVED"}
	got := Diff(prev, cur)
	if len(got) != 1 || got[0].Kind != "REVIEW" || got[0].Message != "alice APPROVED" {
		t.Fatalf("Diff() = %+v, want single REVIEW event", got)
	}
}

func TestDiffReviewEventCarriesReviewHref(t *testing.T) {
	prev := PrevState{ReviewCount: 1}
	cur := CheckResult{State: "open", ReviewCount: 2, LatestReviewer: "alice", LatestReviewState: "APPROVED", ReviewHref: "https://github.com/o/r/pull/1#pullrequestreview-1"}
	got := Diff(prev, cur)
	if len(got) != 1 || got[0].Href != "https://github.com/o/r/pull/1#pullrequestreview-1" {
		t.Fatalf("Diff() = %+v, want REVIEW event carrying ReviewHref", got)
	}
}

// TestDiffReviewSuppressesSameTickComment is a regression test: confirmed
// live, a COMMENTED review (coderabbitai) that includes an inline comment
// fired both COMMENT and REVIEW in the same poll, both carrying the same
// Href — the "new thread" was never independent activity, just the review
// submission's own inline comment. A new review landing in the same tick a
// new thread appears must win alone.
func TestDiffReviewSuppressesSameTickComment(t *testing.T) {
	prev := PrevState{ReviewCount: 1, UnresolvedThreadIDs: []string{"t1"}}
	cur := CheckResult{
		State:               "open",
		ReviewCount:         2,
		LatestReviewer:      "coderabbitai",
		LatestReviewState:   "COMMENTED",
		ReviewHref:          "https://github.com/o/r/pull/1#pullrequestreview-1",
		UnresolvedThreadIDs: []string{"t1", "t2"},
	}
	got := Diff(prev, cur)
	if len(got) != 1 || got[0].Kind != "REVIEW" || got[0].Message != "coderabbitai COMMENTED, 2 unresolved threads" {
		t.Fatalf("Diff() = %+v, want a single REVIEW event, COMMENT suppressed as the same review's own thread", got)
	}
}

// TestDiffNewTopLevelCommentFiresComment is a regression test: confirmed
// live, github.com/agent-of-empires/agent-of-empires/pull/4132 got a
// coderabbit summary posted as a plain Conversation-tab comment — no
// review, no inline thread — and it never pinged, since only reviews and
// review threads were tracked at all. A brand new top-level comment with no
// review in the same tick must fire on its own.
func TestDiffNewTopLevelCommentFiresComment(t *testing.T) {
	prev := PrevState{TopLevelCommentIDs: []string{"c1"}}
	cur := CheckResult{State: "open", TopLevelCommentIDs: []string{"c1", "c2"}, LatestCommentHref: "https://github.com/o/r/pull/1#issuecomment-2"}
	got := Diff(prev, cur)
	if len(got) != 1 || got[0].Kind != "COMMENT" || got[0].Message != "1 new comment(s), 2 total" || got[0].Href != "https://github.com/o/r/pull/1#issuecomment-2" {
		t.Fatalf("Diff() = %+v, want single COMMENT event for the new top-level comment", got)
	}
}

// TestDiffReviewSuppressesSameTickTopLevelComment mirrors
// TestDiffReviewSuppressesSameTickComment for the top-level-comment case: a
// review submission commonly posts its own walkthrough comment alongside
// it (coderabbit does this whenever it also finds actionable items), so
// the same review-wins-alone rule applies here too.
func TestDiffReviewSuppressesSameTickTopLevelComment(t *testing.T) {
	prev := PrevState{ReviewCount: 1, TopLevelCommentIDs: []string{"c1"}}
	cur := CheckResult{
		State:              "open",
		ReviewCount:        2,
		LatestReviewer:     "coderabbitai",
		LatestReviewState:  "COMMENTED",
		LatestReviewBody:   "Walkthrough of the change.",
		ReviewHref:         "https://github.com/o/r/pull/1#pullrequestreview-1",
		TopLevelCommentIDs: []string{"c1", "c2"},
	}
	got := Diff(prev, cur)
	if len(got) != 1 || got[0].Kind != "REVIEW" {
		t.Fatalf("Diff() = %+v, want a single REVIEW event, top-level COMMENT suppressed as the same review's own walkthrough", got)
	}
}

func TestDiffCIChangeFiresCI(t *testing.T) {
	prev := PrevState{CIBucket: "pending"}
	cur := CheckResult{State: "open", CIBucket: "failure"}
	got := Diff(prev, cur)
	if len(got) != 1 || got[0].Kind != "CI" || got[0].Message != "failure" {
		t.Fatalf("Diff() = %+v, want single CI event", got)
	}
}

func TestDiffCIEventCarriesCIHref(t *testing.T) {
	prev := PrevState{CIBucket: "pending"}
	cur := CheckResult{State: "open", CIBucket: "failure", CIHref: "https://github.com/o/r/actions/runs/1"}
	got := Diff(prev, cur)
	if len(got) != 1 || got[0].Href != "https://github.com/o/r/actions/runs/1" {
		t.Fatalf("Diff() = %+v, want CI event carrying CIHref", got)
	}
}

// TestDiffCIFiresAgainOnDifferentFailureWithSameBucket guards against the
// race where a fix-push fails again before the next poll happens to run:
// failure (run A) -> pending -> failure (run B) between two consecutive
// checks looks like "still failure" by bucket alone, since the poller never
// observed the intermediate pending state. The two runs' distinct CIHref
// values are what makes run B still reportable.
func TestDiffCIFiresAgainOnDifferentFailureWithSameBucket(t *testing.T) {
	prev := PrevState{CIBucket: "failure", CIHref: "https://github.com/o/r/actions/runs/1"}
	cur := CheckResult{State: "open", CIBucket: "failure", CIHref: "https://github.com/o/r/actions/runs/2"}
	got := Diff(prev, cur)
	if len(got) != 1 || got[0].Kind != "CI" || got[0].Href != "https://github.com/o/r/actions/runs/2" {
		t.Fatalf("Diff() = %+v, want a CI event for the new run despite an unchanged bucket", got)
	}
}

// TestDiffCISameFailureDoesNotRefire is FiresAgainOnDifferentFailureWithSameBucket's
// counterpart: an unchanged bucket AND an unchanged href (the same failing
// run reported twice, e.g. two polls before anything changed) must not
// re-fire.
func TestDiffCISameFailureDoesNotRefire(t *testing.T) {
	prev := PrevState{CIBucket: "failure", CIHref: "https://github.com/o/r/actions/runs/1"}
	cur := CheckResult{State: "open", CIBucket: "failure", CIHref: "https://github.com/o/r/actions/runs/1"}
	got := Diff(prev, cur)
	if len(got) != 0 {
		t.Fatalf("Diff() = %+v, want no CI event when both bucket and href are unchanged", got)
	}
}

// The plain CI event is failure-only; a green CI is the separate, opt-in
// CI_SUCCESS (and CI_RECOVERED after a failure).
func TestDiffPlainCIEventNeverFiresOnSuccess(t *testing.T) {
	prev := PrevState{CIBucket: "pending"}
	cur := CheckResult{State: "open", CIBucket: "success"}
	if n := countKind(Diff(prev, cur), "CI"); n != 0 {
		t.Fatalf("got %d plain CI events on a success transition, want none (only failures)", n)
	}
}

func TestDiffCIPendingNeverFires(t *testing.T) {
	prev := PrevState{CIBucket: "none"}
	cur := CheckResult{State: "open", CIBucket: "pending"}
	got := Diff(prev, cur)
	if len(got) != 0 {
		t.Fatalf("Diff() = %+v, want no CI event on a pending transition (only failure pings)", got)
	}
}

func TestDiffCIActionRequiredNeverFires(t *testing.T) {
	prev := PrevState{CIBucket: "pending"}
	cur := CheckResult{State: "open", CIBucket: "action_required"}
	got := Diff(prev, cur)
	if len(got) != 0 {
		t.Fatalf("Diff() = %+v, want no CI event on an action_required transition (only failure pings)", got)
	}
}

func TestDiffCINoneNeverFires(t *testing.T) {
	prev := PrevState{CIBucket: "failure"}
	cur := CheckResult{State: "open", CIBucket: "none"}
	got := Diff(prev, cur)
	if len(got) != 0 {
		t.Fatalf("Diff() = %+v, want no CI event when bucket reverts to none", got)
	}
}

func TestDiffNoChangeFiresNothing(t *testing.T) {
	prev := PrevState{ReviewCount: 2, CIBucket: "success", UnresolvedThreadIDs: []string{"t1"}}
	cur := CheckResult{State: "open", ReviewCount: 2, CIBucket: "success", UnresolvedThreadIDs: []string{"t1"}}
	got := Diff(prev, cur)
	if len(got) != 0 {
		t.Fatalf("Diff() = %+v, want no events on unchanged state", got)
	}
}

func TestDiffMergeQueueRemovalFiresMergeQueueEvent(t *testing.T) {
	prev := PrevState{MergeQueueState: "QUEUED"}
	cur := CheckResult{State: "open", MergeQueueState: ""}
	got := Diff(prev, cur)
	if len(got) != 1 || got[0].Kind != "MERGE_QUEUE" || got[0].Message != "removed from merge queue" {
		t.Fatalf("Diff() = %+v, want single MERGE_QUEUE event", got)
	}
}

func TestDiffMergeQueueEntryFiresNothing(t *testing.T) {
	prev := PrevState{MergeQueueState: ""}
	cur := CheckResult{State: "open", MergeQueueState: "QUEUED"}
	got := Diff(prev, cur)
	if len(got) != 0 {
		t.Fatalf("Diff() = %+v, want no event when a PR newly enters the merge queue (the badge already shows that live)", got)
	}
}

func TestDiffMergeQueueStateChangeWithinQueueFiresNothing(t *testing.T) {
	prev := PrevState{MergeQueueState: "QUEUED"}
	cur := CheckResult{State: "open", MergeQueueState: "MERGEABLE"}
	got := Diff(prev, cur)
	if len(got) != 0 {
		t.Fatalf("Diff() = %+v, want no event for progress within the queue, only for leaving it", got)
	}
}

func TestDiffMergeQueueRemovalSuppressedOnMerge(t *testing.T) {
	prev := PrevState{MergeQueueState: "MERGEABLE"}
	cur := CheckResult{State: "merged", MergeQueueState: ""}
	got := Diff(prev, cur)
	if len(got) != 1 || got[0].Kind != "MERGED" {
		t.Fatalf("Diff() = %+v, want single MERGED event, not a spurious MERGE_QUEUE removal on a successful merge", got)
	}
}

func TestDiffConflictAppearingFiresConflictEvent(t *testing.T) {
	prev := PrevState{HasConflict: false}
	cur := CheckResult{State: "open", HasConflict: true}
	got := Diff(prev, cur)
	if len(got) != 1 || got[0].Kind != "CONFLICT" {
		t.Fatalf("Diff() = %+v, want a single CONFLICT event", got)
	}
}

func TestDiffConflictStillPresentFiresNothing(t *testing.T) {
	prev := PrevState{HasConflict: true}
	cur := CheckResult{State: "open", HasConflict: true}
	got := Diff(prev, cur)
	if len(got) != 0 {
		t.Fatalf("Diff() = %+v, want no event for a conflict still present across two polls, only the transition", got)
	}
}

func TestDiffConflictClearingFiresNothing(t *testing.T) {
	prev := PrevState{HasConflict: true}
	cur := CheckResult{State: "open", HasConflict: false}
	got := Diff(prev, cur)
	if len(got) != 0 {
		t.Fatalf("Diff() = %+v, want no event for a conflict clearing — the badge already reflects that", got)
	}
}

// TestDiffReviewDismissedFiresWhenReviewDisappears is a regression test:
// confirmed live — a bot's dependency-bump push reset a GitLab MR's
// approval (a project setting) with no event at all, so the badge quietly
// reverted to "no review" and nobody heard about it.
func TestDiffReviewDismissedFiresWhenReviewDisappears(t *testing.T) {
	prev := PrevState{LatestReviewState: "APPROVED", LatestReviewer: "bob", ReviewHref: "https://example.com/review/1"}
	cur := CheckResult{State: "open"}
	got := Diff(prev, cur)
	if len(got) != 1 || got[0].Kind != "REVIEW_DISMISSED" || got[0].Message != "bob's APPROVED review was dismissed" || got[0].Href != "https://example.com/review/1" {
		t.Fatalf("Diff() = %+v, want a single REVIEW_DISMISSED event naming bob's APPROVED review", got)
	}
}

func TestDiffReviewDismissedNeverFiresWithoutAPriorReview(t *testing.T) {
	prev := PrevState{}
	cur := CheckResult{State: "open"}
	got := Diff(prev, cur)
	if len(got) != 0 {
		t.Fatalf("Diff() = %+v, want no event when there was never a review to lose", got)
	}
}

func TestDiffReviewDismissedNeverFiresWhileReviewStillPresent(t *testing.T) {
	prev := PrevState{ReviewCount: 1, LatestReviewState: "APPROVED", LatestReviewer: "bob"}
	cur := CheckResult{State: "open", ReviewCount: 1, LatestReviewState: "APPROVED", LatestReviewer: "bob"}
	got := Diff(prev, cur)
	if len(got) != 0 {
		t.Fatalf("Diff() = %+v, want no event when the review is still there unchanged", got)
	}
}

// TestDiffReviewDismissedDoesNotFireWhenReplacedByNewReview guards the
// direction of the check: a review disappearing while a *different* one
// takes its place in the same poll is the ordinary REVIEW case, not a
// dismissal.
func TestDiffReviewDismissedDoesNotFireWhenReplacedByNewReview(t *testing.T) {
	prev := PrevState{ReviewCount: 1, LatestReviewState: "CHANGES_REQUESTED", LatestReviewer: "bob"}
	cur := CheckResult{State: "open", ReviewCount: 2, LatestReviewState: "APPROVED", LatestReviewer: "alice"}
	got := Diff(prev, cur)
	if len(got) != 1 || got[0].Kind != "REVIEW" {
		t.Fatalf("Diff() = %+v, want a single REVIEW event (alice's new review), not REVIEW_DISMISSED", got)
	}
}

// TestDiffMultipleEventsInOneCheck uses a new thread with no review-count
// increase alongside a CI change — a COMMENT+REVIEW combination would
// collide with TestDiffReviewSuppressesSameTickComment's suppression rule,
// so this covers independent-event bundling without that overlap.
func TestDiffMultipleEventsInOneCheck(t *testing.T) {
	prev := PrevState{ReviewCount: 1, CIBucket: "pending", UnresolvedThreadIDs: []string{"t1"}}
	cur := CheckResult{State: "open", ReviewCount: 1, CIBucket: "failure", UnresolvedThreadIDs: []string{"t1", "t2"}}
	got := Diff(prev, cur)
	if len(got) != 2 {
		t.Fatalf("Diff() = %+v, want 2 events (comment, ci)", got)
	}
}

func TestDiffCIRecoveredFiresWhenFailureBecomesSuccess(t *testing.T) {
	prev := PrevState{CIBucket: "failure", CIWasFailing: true}
	got := Diff(prev, CheckResult{State: "open", CIBucket: "success"})
	if recovered := countKind(got, "CI_RECOVERED"); recovered != 1 {
		t.Fatalf("Diff() = %+v, want a single CI_RECOVERED event", got)
	}
}

func countKind(events []Event, kind string) int {
	n := 0
	for _, e := range events {
		if e.Kind == kind {
			n++
		}
	}
	return n
}

// TestDiffCIRecoveredSurvivesAnIntermediatePendingPoll: a fix push reruns CI,
// so the usual sequence is failure -> pending -> success; the poll that sees
// success has prev.CIBucket "pending", and only the sticky flag remembers
// the earlier failure.
func TestDiffCIRecoveredSurvivesAnIntermediatePendingPoll(t *testing.T) {
	prev := PrevState{CIBucket: "pending", CIWasFailing: true}
	got := Diff(prev, CheckResult{State: "open", CIBucket: "success"})
	if countKind(got, "CI_RECOVERED") != 1 {
		t.Fatalf("Diff() = %+v, want CI_RECOVERED across the pending poll", got)
	}
}

func TestDiffCIRecoveredNeverFiresWithoutAPriorFailure(t *testing.T) {
	for _, prevBucket := range []string{"none", "pending", "success"} {
		for _, e := range Diff(PrevState{CIBucket: prevBucket}, CheckResult{State: "open", CIBucket: "success"}) {
			if e.Kind == "CI_RECOVERED" {
				t.Errorf("prev %q -> success: got CI_RECOVERED, want none (nothing failed first)", prevBucket)
			}
		}
	}
}

func TestDiffCIRecoveredNotWhileStillPendingOrFailing(t *testing.T) {
	for _, cur := range []string{"pending", "none", "action_required"} {
		got := Diff(PrevState{CIBucket: "failure", CIWasFailing: true}, CheckResult{State: "open", CIBucket: cur})
		if len(got) != 0 {
			t.Errorf("failure -> %s: Diff() = %+v, want no events", cur, got)
		}
	}
}

func TestNextCIWasFailing(t *testing.T) {
	tests := []struct {
		name    string
		was     bool
		current string
		want    bool
	}{
		{"failure sets it", false, "failure", true},
		{"stays set through pending", true, "pending", true},
		{"stays set through none", true, "none", true},
		{"stays set through action_required", true, "action_required", true},
		{"success clears it", true, "success", false},
		{"success without a failure stays clear", false, "success", false},
		{"pending without a failure stays clear", false, "pending", false},
	}
	for _, tt := range tests {
		got := NextCIWasFailing(PrevState{CIWasFailing: tt.was}, CheckResult{CIBucket: tt.current})
		if got != tt.want {
			t.Errorf("%s: NextCIWasFailing() = %v, want %v", tt.name, got, tt.want)
		}
	}
}

// An MR that still needs approvals is not approved yet: the first of two
// required approvals stays silent, the one that completes it pings.
func TestDiffPartialApprovalDoesNotFireReview(t *testing.T) {
	prev := PrevState{}
	cur := CheckResult{State: "open", ReviewCount: 1, LatestReviewer: "st", LatestReviewState: "APPROVED 1/2"}
	if got := Diff(prev, cur); len(got) != 0 {
		t.Fatalf("Diff() = %+v, want no event for a partial approval", got)
	}
}

func TestDiffCompletingApprovalFiresReview(t *testing.T) {
	prev := PrevState{ReviewCount: 1, LatestReviewState: "APPROVED 1/2", LatestReviewer: "st"}
	cur := CheckResult{State: "open", ReviewCount: 2, LatestReviewer: "bob", LatestReviewState: "APPROVED"}
	got := Diff(prev, cur)
	if len(got) != 1 || got[0].Kind != "REVIEW" || got[0].Message != "bob APPROVED" {
		t.Fatalf("Diff() = %+v, want a single REVIEW event once the MR is fully approved", got)
	}
}

// The requirement can drop (rule change) so the existing approval is enough
// without the count moving.
func TestDiffApprovalCompletedWithoutCountChangeFiresReview(t *testing.T) {
	prev := PrevState{ReviewCount: 1, LatestReviewState: "APPROVED 1/2", LatestReviewer: "st"}
	cur := CheckResult{State: "open", ReviewCount: 1, LatestReviewer: "st", LatestReviewState: "APPROVED"}
	got := Diff(prev, cur)
	if len(got) != 1 || got[0].Kind != "REVIEW" || got[0].Message != "st APPROVED" {
		t.Fatalf("Diff() = %+v, want a REVIEW event when the MR becomes approved", got)
	}
}

// A partial approval was never announced, so losing it isn't either.
func TestDiffLosingPartialApprovalDoesNotFireDismissed(t *testing.T) {
	prev := PrevState{ReviewCount: 1, LatestReviewState: "APPROVED 1/2", LatestReviewer: "st"}
	cur := CheckResult{State: "open"}
	if got := Diff(prev, cur); len(got) != 0 {
		t.Fatalf("Diff() = %+v, want no event when an unannounced partial approval disappears", got)
	}
}

func TestDiffReviewMessageNamesUnresolvedThreadCount(t *testing.T) {
	cases := []struct {
		threads []string
		want    string
	}{
		{nil, "scholzj COMMENTED"},
		{[]string{"t1"}, "scholzj COMMENTED, 1 unresolved thread"},
		{[]string{"t1", "t2", "t3", "t4"}, "scholzj COMMENTED, 4 unresolved threads"},
	}
	for _, c := range cases {
		cur := CheckResult{State: "open", ReviewCount: 1, LatestReviewer: "scholzj", LatestReviewState: "COMMENTED", LatestReviewBody: "A few notes.", UnresolvedThreadIDs: c.threads}
		got := Diff(PrevState{}, cur)
		if len(got) != 1 || got[0].Kind != "REVIEW" || got[0].Message != c.want {
			t.Errorf("threads %v: Diff() = %+v, want a REVIEW event %q", c.threads, got, c.want)
		}
	}
}

func replyState() (PrevState, CheckResult) {
	prev := PrevState{UnresolvedThreadIDs: []string{"t1"}, ThreadCommentIDs: []string{"c1"}, ThreadCommentsKnown: true}
	cur := CheckResult{State: "open", UnresolvedThreadIDs: []string{"t1"}, ThreadComments: []ThreadComment{
		{ThreadID: "t1", ID: "c1", Author: "me", Href: "https://example.com#c1"},
	}}
	return prev, cur
}

// A reply inside a thread that was already open is new activity the
// new-thread and new-review checks never see.
func TestDiffNewReplyInExistingThreadFiresComment(t *testing.T) {
	prev, cur := replyState()
	cur.ThreadComments = append(cur.ThreadComments, ThreadComment{ThreadID: "t1", ID: "c2", Author: "scholzj", Href: "https://example.com#c2"})

	got := Diff(prev, cur)
	if len(got) != 1 || got[0].Kind != "COMMENT" || got[0].Message != "1 new thread reply, 1 unresolved total" || got[0].Href != "https://example.com#c2" {
		t.Fatalf("Diff() = %+v, want one COMMENT event for the reply, linking to it", got)
	}
}

func TestDiffSeveralRepliesAreOneEventLinkingTheNewest(t *testing.T) {
	prev, cur := replyState()
	cur.ThreadComments = append(cur.ThreadComments,
		ThreadComment{ThreadID: "t1", ID: "c2", Href: "https://example.com#c2"},
		ThreadComment{ThreadID: "t1", ID: "c3", Href: "https://example.com#c3"})

	got := Diff(prev, cur)
	if len(got) != 1 || got[0].Message != "2 new thread replies, 1 unresolved total" || got[0].Href != "https://example.com#c3" {
		t.Fatalf("Diff() = %+v, want one event for 2 replies linking the newest", got)
	}
}

// Comments of a thread that is itself new are covered by the new-thread
// event, and a first poll after upgrade has no baseline to compare with.
func TestDiffRepliesIgnoredInNewThreadsAndWithoutBaseline(t *testing.T) {
	prev, cur := replyState()
	cur.UnresolvedThreadIDs = []string{"t1", "t2"}
	cur.ThreadComments = append(cur.ThreadComments, ThreadComment{ThreadID: "t2", ID: "c9"})
	got := Diff(prev, cur)
	if len(got) != 1 || got[0].Message != "1 new unresolved thread(s), 2 unresolved total" {
		t.Errorf("new thread: Diff() = %+v, want only the new-thread event", got)
	}

	prev, cur = replyState()
	prev.ThreadCommentsKnown = false
	prev.ThreadCommentIDs = nil
	cur.ThreadComments = append(cur.ThreadComments, ThreadComment{ThreadID: "t1", ID: "c2"})
	if got := Diff(prev, cur); len(got) != 0 {
		t.Errorf("no baseline: Diff() = %+v, want no event while the baseline is unknown", got)
	}
}

// A review's own inline replies are part of that review's ping.
func TestDiffRepliesSuppressedWhenReviewFiresInSameTick(t *testing.T) {
	prev, cur := replyState()
	cur.ThreadComments = append(cur.ThreadComments, ThreadComment{ThreadID: "t1", ID: "c2"})
	cur.ReviewCount, cur.LatestReviewer, cur.LatestReviewState = 1, "scholzj", "COMMENTED"
	cur.LatestReviewBody = "A few notes."

	got := Diff(prev, cur)
	if len(got) != 1 || got[0].Kind != "REVIEW" {
		t.Fatalf("Diff() = %+v, want only the REVIEW event", got)
	}
}

func TestExcludeThreadCommentsByDropsOwnComments(t *testing.T) {
	cur := CheckResult{ThreadComments: []ThreadComment{{ID: "c1", Author: "me"}, {ID: "c2", Author: "scholzj"}}}
	cur.ExcludeThreadCommentsBy("me")
	if len(cur.ThreadComments) != 1 || cur.ThreadComments[0].ID != "c2" {
		t.Errorf("ThreadComments = %+v, want only scholzj's comment left", cur.ThreadComments)
	}
	cur.ExcludeThreadCommentsBy("")
	if len(cur.ThreadComments) != 1 {
		t.Errorf("an empty self author must exclude nothing, got %+v", cur.ThreadComments)
	}
}

// A merged or closed PR is still watched for late activity: comments,
// reviews and thread replies, but not CI, conflicts, or the terminal state
// itself, which was already reported once.
func TestDiffFinishedReportsActivityButNotStateOrCI(t *testing.T) {
	prev := PrevState{ReviewCount: 1, CIBucket: "success", TopLevelCommentIDs: []string{"c1"}}
	cur := CheckResult{
		State: "merged", ReviewCount: 2, LatestReviewer: "bob", LatestReviewState: "COMMENTED", LatestReviewBody: "Late thoughts.",
		CIBucket: "failure", HasConflict: true,
		TopLevelCommentIDs: []string{"c1", "c2"}, LatestCommentHref: "https://example.com#c2",
	}

	got := DiffFinished(prev, cur)
	if len(got) != 1 || got[0].Kind != "REVIEW" || got[0].Message != "bob COMMENTED" {
		t.Fatalf("DiffFinished() = %+v, want only the REVIEW (the same-tick comment folds into it), no MERGED, CI or CONFLICT", got)
	}

	cur.ReviewCount = 1
	got = DiffFinished(prev, cur)
	if len(got) != 1 || got[0].Kind != "COMMENT" || got[0].Message != "1 new comment(s), 2 total" {
		t.Fatalf("DiffFinished() = %+v, want a single COMMENT for the late comment", got)
	}

	if got := DiffFinished(PrevState{TopLevelCommentIDs: []string{"c1", "c2"}, ReviewCount: 1}, cur); len(got) != 0 {
		t.Fatalf("DiffFinished() with nothing new = %+v, want no events", got)
	}
}

// GitHub records why an entry left the merge queue. A PR that merged out of
// it is not news (MERGED follows, and a poll can see the entry gone a moment
// before the PR reads as merged), while any other reason is named.
func TestDiffMergeQueueRemovalUsesTheRecordedReason(t *testing.T) {
	prev := PrevState{MergeQueueState: "QUEUED"}
	cases := []struct {
		reason string
		want   []Event
	}{
		{"merged", nil},
		{"", []Event{{Kind: "MERGE_QUEUE", Message: "removed from merge queue"}}},
		{"invalidated", []Event{{Kind: "MERGE_QUEUE", Message: "removed from merge queue (invalidated)"}}},
		{"build_failure", []Event{{Kind: "MERGE_QUEUE", Message: "removed from merge queue (build failure)"}}},
	}
	for _, c := range cases {
		got := Diff(prev, CheckResult{State: "open", MergeQueueRemovalReason: c.reason})
		if !slices.Equal(got, c.want) {
			t.Errorf("reason %q: Diff() = %+v, want %+v", c.reason, got, c.want)
		}
	}
}

// A COMMENTED review with no text that opened no new thread has nothing to
// read (bots submit those), so it does not ping; the verdict reviews and any
// review with something in it still do.
func TestDiffCommentedReviewPingsOnlyWithSomethingToRead(t *testing.T) {
	cases := []struct {
		name     string
		state    string
		body     string
		prev     PrevState
		threads  []string
		wantPing bool
	}{
		{"empty commented", "COMMENTED", "", PrevState{}, nil, false},
		{"blank commented", "COMMENTED", " \n\t", PrevState{}, nil, false},
		{"commented with text", "COMMENTED", "Looks mostly fine, one question.", PrevState{}, nil, true},
		{"empty commented opening a thread", "COMMENTED", "", PrevState{}, []string{"t1"}, true},
		{"empty commented with only old threads", "COMMENTED", "", PrevState{UnresolvedThreadIDs: []string{"t1"}}, []string{"t1"}, false},
		{"empty approval", "APPROVED", "", PrevState{}, nil, true},
		{"empty changes requested", "CHANGES_REQUESTED", "", PrevState{}, nil, true},
	}
	for _, c := range cases {
		cur := CheckResult{State: "open", ReviewCount: 1, LatestReviewer: "bot", LatestReviewState: c.state, LatestReviewBody: c.body, UnresolvedThreadIDs: c.threads}
		got := Diff(c.prev, cur)
		var review bool
		for _, e := range got {
			if e.Kind == "REVIEW" {
				review = true
			}
		}
		if review != c.wantPing {
			t.Errorf("%s: REVIEW fired = %v, want %v (events %+v)", c.name, review, c.wantPing, got)
		}
	}
}

// CI turning green is its own event kind, opt-in (see the scheduler's
// filtering), distinct from CI_RECOVERED which needs a failure first.
func TestDiffCISuccessFiresWhenCITurnsGreen(t *testing.T) {
	for _, from := range []string{"none", "pending", "action_required"} {
		got := Diff(PrevState{CIBucket: from}, CheckResult{State: "open", CIBucket: "success", CIHref: "https://example.com/run/1"})
		if len(got) != 1 || got[0].Kind != "CI_SUCCESS" || got[0].Message != "CI is passing" || got[0].Href != "https://example.com/run/1" {
			t.Errorf("from %s: Diff() = %+v, want a single CI_SUCCESS event", from, got)
		}
	}
}

func TestDiffCISuccessStaysQuietWhileCIStaysGreen(t *testing.T) {
	if got := Diff(PrevState{CIBucket: "success"}, CheckResult{State: "open", CIBucket: "success"}); len(got) != 0 {
		t.Errorf("Diff() = %+v, want nothing for an unchanged green CI", got)
	}
}

func TestDiffCIRecoveryFiresBothSuccessKinds(t *testing.T) {
	got := Diff(PrevState{CIBucket: "pending", CIWasFailing: true}, CheckResult{State: "open", CIBucket: "success"})
	kinds := map[string]bool{}
	for _, e := range got {
		kinds[e.Kind] = true
	}
	if len(got) != 2 || !kinds["CI_RECOVERED"] || !kinds["CI_SUCCESS"] {
		t.Errorf("Diff() = %+v, want CI_RECOVERED and CI_SUCCESS", got)
	}
}

// The review-flow events need a stored baseline (a head commit); without one
// none of them can fire, resolved threads included.
func TestDiffReviewFlowEventsNeedABaseline(t *testing.T) {
	prev := PrevState{UnresolvedThreadIDs: []string{"t1"}, RequestedReviewers: nil}
	cur := CheckResult{State: "open", HeadSHA: "bbb", IsDraft: true, RequestedReviewers: []string{"me"}}
	if got := Diff(prev, cur); len(got) != 0 {
		t.Errorf("Diff() without baseline = %+v, want nothing", got)
	}
}

func kinds(events []Event) []string {
	var out []string
	for _, e := range events {
		out = append(out, e.Kind)
	}
	return out
}

func TestDiffPushFiresWhenTheHeadMoves(t *testing.T) {
	prev := PrevState{HeadSHA: "aaaaaaa111"}
	got := Diff(prev, CheckResult{State: "open", HeadSHA: "bbbbbbb222", HeadAuthor: "alice"})
	if len(got) != 1 || got[0].Kind != "PUSH" || got[0].Message != "new commits by alice, head now bbbbbbb" {
		t.Fatalf("Diff() = %+v, want one PUSH naming the author and the short head", got)
	}

	got = Diff(prev, CheckResult{State: "open", HeadSHA: "bbbbbbb222", ForcePushed: true})
	if len(got) != 1 || got[0].Message != "force-push, head now bbbbbbb" {
		t.Errorf("force-push Diff() = %+v", got)
	}

	if got := Diff(prev, CheckResult{State: "open", HeadSHA: "aaaaaaa111"}); len(got) != 0 {
		t.Errorf("unchanged head produced %+v", got)
	}
	// No baseline yet (a row from before this tracking, or its first poll):
	// the head being "new" is no news.
	if got := Diff(PrevState{}, CheckResult{State: "open", HeadSHA: "bbbbbbb222"}); len(got) != 0 {
		t.Errorf("unknown baseline produced %+v", got)
	}
}

func TestDiffDraftTransitions(t *testing.T) {
	base := PrevState{HeadSHA: "aaa"}
	cur := CheckResult{State: "open", HeadSHA: "aaa"}

	prevDraft := base
	prevDraft.IsDraft = true
	if got := kinds(Diff(prevDraft, cur)); len(got) != 1 || got[0] != "READY_FOR_REVIEW" {
		t.Errorf("draft -> ready kinds = %v", got)
	}
	cur.IsDraft = true
	if got := kinds(Diff(base, cur)); len(got) != 1 || got[0] != "DRAFTED" {
		t.Errorf("ready -> draft kinds = %v", got)
	}
	if got := Diff(prevDraft, cur); len(got) != 0 {
		t.Errorf("still a draft produced %+v", got)
	}
	// Baseline unknown: no transition can be claimed.
	if got := Diff(PrevState{IsDraft: true}, CheckResult{State: "open", HeadSHA: "aaa"}); len(got) != 0 {
		t.Errorf("unknown baseline produced %+v", got)
	}
}

func TestDiffReviewRequestedNamesNewlyRequestedReviewers(t *testing.T) {
	prev := PrevState{HeadSHA: "aaa", RequestedReviewers: []string{"bob"}}
	got := Diff(prev, CheckResult{State: "open", HeadSHA: "aaa", RequestedReviewers: []string{"bob", "carol", "dave"}})
	if len(got) != 1 || got[0].Kind != "REVIEW_REQUESTED" || got[0].Message != "review requested from carol, dave" {
		t.Fatalf("Diff() = %+v", got)
	}
	if got := Diff(prev, CheckResult{State: "open", HeadSHA: "aaa", RequestedReviewers: []string{"bob"}}); len(got) != 0 {
		t.Errorf("same requests produced %+v", got)
	}
}

func TestDiffThreadResolved(t *testing.T) {
	prev := PrevState{HeadSHA: "aaa", UnresolvedThreadIDs: []string{"t1", "t2", "t3"}}
	got := Diff(prev, CheckResult{State: "open", HeadSHA: "aaa", UnresolvedThreadIDs: []string{"t3"}})
	if len(got) != 1 || got[0].Kind != "THREAD_RESOLVED" || got[0].Message != "2 thread(s) resolved, 1 unresolved left" {
		t.Fatalf("Diff() = %+v", got)
	}
	// A resolve and a new thread in one window: both are reported.
	got = Diff(prev, CheckResult{State: "open", HeadSHA: "aaa", UnresolvedThreadIDs: []string{"t2", "t3", "t4"}})
	if k := kinds(got); len(k) != 2 {
		t.Errorf("resolve + new thread kinds = %v, want COMMENT and THREAD_RESOLVED", k)
	}
}
