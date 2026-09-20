package provider

import "fmt"

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

	var events []Event
	if cur.Unresolved > prev.Unresolved {
		events = append(events, Event{
			Kind:    "COMMENT",
			Message: fmt.Sprintf("unresolved threads: %d -> %d", prev.Unresolved, cur.Unresolved),
		})
	}
	if cur.ReviewCount > prev.ReviewCount && cur.LatestReviewer != "" {
		events = append(events, Event{
			Kind:    "REVIEW",
			Message: fmt.Sprintf("%s %s", cur.LatestReviewer, cur.LatestReviewState),
		})
	}
	if cur.CIBucket != prev.CIBucket && cur.CIBucket != "none" {
		events = append(events, Event{Kind: "CI", Message: cur.CIBucket})
	}
	return events
}
