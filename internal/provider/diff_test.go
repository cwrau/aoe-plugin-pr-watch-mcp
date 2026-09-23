package provider

import "testing"

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

func TestDiffCISuccessNeverFires(t *testing.T) {
	prev := PrevState{CIBucket: "pending"}
	cur := CheckResult{State: "open", CIBucket: "success"}
	got := Diff(prev, cur)
	if len(got) != 0 {
		t.Fatalf("Diff() = %+v, want no CI event on a success transition (only failure pings)", got)
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

func TestDiffMultipleEventsInOneCheck(t *testing.T) {
	prev := PrevState{ReviewCount: 1, CIBucket: "pending"}
	cur := CheckResult{State: "open", ReviewCount: 2, LatestReviewer: "bob", LatestReviewState: "CHANGES_REQUESTED", CIBucket: "failure", UnresolvedThreadIDs: []string{"t1"}}
	got := Diff(prev, cur)
	if len(got) != 3 {
		t.Fatalf("Diff() = %+v, want 3 events (comment, review, ci)", got)
	}
}
