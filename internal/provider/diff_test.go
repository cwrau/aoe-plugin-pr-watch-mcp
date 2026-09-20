package provider

import "testing"

func TestDiffMergedShortCircuits(t *testing.T) {
	prev := PrevState{ReviewCount: 1, CIBucket: "success", Unresolved: 0}
	cur := CheckResult{State: "merged", ReviewCount: 5, CIBucket: "failure", Unresolved: 3}
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

func TestDiffUnresolvedIncreaseFiresComment(t *testing.T) {
	prev := PrevState{Unresolved: 1}
	cur := CheckResult{State: "open", Unresolved: 3}
	got := Diff(prev, cur)
	if len(got) != 1 || got[0].Kind != "COMMENT" || got[0].Message != "unresolved threads: 1 -> 3" {
		t.Fatalf("Diff() = %+v, want single COMMENT event", got)
	}
}

func TestDiffUnresolvedDecreaseFiresNothing(t *testing.T) {
	prev := PrevState{Unresolved: 3}
	cur := CheckResult{State: "open", Unresolved: 1}
	got := Diff(prev, cur)
	if len(got) != 0 {
		t.Fatalf("Diff() = %+v, want no events (a decrease is a resolve, not new activity)", got)
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

func TestDiffCIChangeFiresCI(t *testing.T) {
	prev := PrevState{CIBucket: "pending"}
	cur := CheckResult{State: "open", CIBucket: "failure"}
	got := Diff(prev, cur)
	if len(got) != 1 || got[0].Kind != "CI" || got[0].Message != "failure" {
		t.Fatalf("Diff() = %+v, want single CI event", got)
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
	prev := PrevState{ReviewCount: 2, CIBucket: "success", Unresolved: 0}
	cur := CheckResult{State: "open", ReviewCount: 2, CIBucket: "success", Unresolved: 0}
	got := Diff(prev, cur)
	if len(got) != 0 {
		t.Fatalf("Diff() = %+v, want no events on unchanged state", got)
	}
}

func TestDiffMultipleEventsInOneCheck(t *testing.T) {
	prev := PrevState{ReviewCount: 1, CIBucket: "pending", Unresolved: 0}
	cur := CheckResult{State: "open", ReviewCount: 2, LatestReviewer: "bob", LatestReviewState: "CHANGES_REQUESTED", CIBucket: "failure", Unresolved: 1}
	got := Diff(prev, cur)
	if len(got) != 3 {
		t.Fatalf("Diff() = %+v, want 3 events (comment, review, ci)", got)
	}
}
