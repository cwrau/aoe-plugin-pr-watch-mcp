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
	// Compared by thread identity, not just count: if one thread gets
	// resolved while another new one appears in the same poll window, a
	// plain count comparison nets to "no change" and silently swallows the
	// new thread. newUnresolvedThreads catches it regardless of what else
	// resolved in the same window.
	if newIDs := newUnresolvedThreads(prev.UnresolvedThreadIDs, cur.UnresolvedThreadIDs); len(newIDs) > 0 {
		events = append(events, Event{
			Kind:    "COMMENT",
			Message: fmt.Sprintf("%d new unresolved thread(s), %d unresolved total", len(newIDs), len(cur.UnresolvedThreadIDs)),
			Href:    cur.ReviewHref,
		})
	}
	if cur.ReviewCount > prev.ReviewCount && cur.LatestReviewer != "" {
		events = append(events, Event{
			Kind:    "REVIEW",
			Message: fmt.Sprintf("%s %s", cur.LatestReviewer, cur.LatestReviewState),
			Href:    cur.ReviewHref,
		})
	}
	if cur.CIBucket != prev.CIBucket && cur.CIBucket == "failure" {
		events = append(events, Event{Kind: "CI", Message: cur.CIBucket, Href: cur.CIHref})
	}
	// A merge queue entry disappearing while the PR is still open (not
	// merged — that path short-circuited above) means it got dequeued
	// without merging. GitHub doesn't say why once the entry is gone (CI
	// failure in the merge group is the common case, but a manual removal
	// or the PR going stale/unmergeable can do it too), so the message
	// stays honest about what's actually known rather than guessing a
	// specific cause.
	if prev.MergeQueueState != "" && cur.MergeQueueState == "" {
		events = append(events, Event{Kind: "MERGE_QUEUE", Message: "removed from merge queue"})
	}
	return events
}

// newUnresolvedThreads returns the ids present in cur but not in prev —
// threads that are genuinely new since the last check, independent of how
// many other threads got resolved in the same window.
func newUnresolvedThreads(prev, cur []string) []string {
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
