package provider

import (
	"slices"
	"testing"
)

func comment(id, author, body string) TopLevelComment {
	return TopLevelComment{ID: id, Href: "https://example.com/#" + id, Author: author, Body: body}
}

func TestApplyCommentFiltersWithoutRulesKeepsEverything(t *testing.T) {
	r := CheckResult{TopLevelComments: []TopLevelComment{comment("c1", "alice", "hi"), comment("c2", "bob", "yo")}}
	r.ApplyCommentFilters(nil)

	if !slices.Equal(r.TopLevelCommentIDs, []string{"c1", "c2"}) {
		t.Errorf("TopLevelCommentIDs = %v, want [c1 c2]", r.TopLevelCommentIDs)
	}
	if r.LatestCommentHref != "https://example.com/#c2" || r.LatestCommentAuthor != "bob" {
		t.Errorf("latest = (%q, %q), want the last comment's href/author", r.LatestCommentHref, r.LatestCommentAuthor)
	}
}

// TestApplyCommentFiltersDropsMatchesBeforeCountingAndLatest: a filtered
// comment must vanish from the IDs (so it can't count as new or inflate the
// total) and must not be the "latest" comment whose href/author the event
// and self-filtering use.
func TestApplyCommentFiltersDropsMatchesBeforeCountingAndLatest(t *testing.T) {
	r := CheckResult{TopLevelComments: []TopLevelComment{
		comment("c1", "alice", "real comment"),
		comment("c2", "coderabbitai", "Currently processing new changes in this PR. Review in progress."),
	}}
	r.ApplyCommentFilters([]CommentFilter{{Author: "coderabbitai", Contains: []string{"review in progress"}}})

	if !slices.Equal(r.TopLevelCommentIDs, []string{"c1"}) {
		t.Errorf("TopLevelCommentIDs = %v, want [c1] (placeholder filtered out)", r.TopLevelCommentIDs)
	}
	if r.LatestCommentHref != "https://example.com/#c1" || r.LatestCommentAuthor != "alice" {
		t.Errorf("latest = (%q, %q), want alice's comment, not the filtered placeholder", r.LatestCommentHref, r.LatestCommentAuthor)
	}
}

// TestApplyCommentFiltersEditedCommentCountsAgain is the point of
// evaluating on the current body every poll: CodeRabbit edits its
// placeholder into the real review, so once the body stops matching, the
// same comment id must come back — and read as new against a baseline that
// never stored it.
func TestApplyCommentFiltersEditedCommentCountsAgain(t *testing.T) {
	rules := []CommentFilter{{Author: "coderabbitai", Contains: []string{"review in progress"}}}

	before := CheckResult{TopLevelComments: []TopLevelComment{comment("c1", "coderabbitai", "Review in progress")}}
	before.ApplyCommentFilters(rules)
	if len(before.TopLevelCommentIDs) != 0 {
		t.Fatalf("placeholder not filtered: %v", before.TopLevelCommentIDs)
	}

	after := CheckResult{State: "open", TopLevelComments: []TopLevelComment{comment("c1", "coderabbitai", "## Walkthrough\nactual findings")}}
	after.ApplyCommentFilters(rules)

	events := Diff(PrevState{TopLevelCommentIDs: before.TopLevelCommentIDs}, after)
	if len(events) != 1 || events[0].Kind != "COMMENT" || events[0].Message != "1 new comment(s), 1 total" {
		t.Fatalf("Diff() = %+v, want one COMMENT for the now-unfiltered comment", events)
	}
}

func TestCommentFilterMatching(t *testing.T) {
	c := comment("c1", "coderabbitai[bot]", "Currently Processing new changes")
	tests := []struct {
		name string
		f    CommentFilter
		want bool
	}{
		{"author and body both match", CommentFilter{Author: "coderabbitai", Contains: []string{"currently processing"}}, true},
		{"author alone matches ([bot] suffix and case ignored)", CommentFilter{Author: "CodeRabbitAI[bot]"}, true},
		{"body alone matches", CommentFilter{Contains: []string{"PROCESSING"}}, true},
		{"author matches, body does not", CommentFilter{Author: "coderabbitai", Contains: []string{"walkthrough"}}, false},
		{"body matches, author does not", CommentFilter{Author: "alice", Contains: []string{"processing"}}, false},
		{"any listed text may match (OR), author still required", CommentFilter{Author: "coderabbitai", Contains: []string{"walkthrough", "currently processing"}}, true},
		{"no listed text matches", CommentFilter{Author: "coderabbitai", Contains: []string{"walkthrough", "summary"}}, false},
		{"a blank entry among real ones is ignored, not a match-everything", CommentFilter{Author: "coderabbitai", Contains: []string{"", "walkthrough"}}, false},
		{"empty rule matches nothing", CommentFilter{}, false},
		{"whitespace-only rule matches nothing", CommentFilter{Author: "  ", Contains: []string{" ", ""}}, false},
	}
	for _, tt := range tests {
		if got := tt.f.matches(c); got != tt.want {
			t.Errorf("%s: matches() = %v, want %v", tt.name, got, tt.want)
		}
	}
}

// TestApplyCommentFiltersDoesNotMutateSharedBackingArrays: a batched GitHub
// check fans one result out to several registrations, each with the same
// rules, so applying must never write into slices another copy shares.
func TestApplyCommentFiltersDoesNotMutateSharedBackingArrays(t *testing.T) {
	shared := []TopLevelComment{comment("c1", "coderabbitai", "review in progress"), comment("c2", "alice", "hi")}
	a := CheckResult{TopLevelComments: shared}
	b := CheckResult{TopLevelComments: shared}
	a.ApplyCommentFilters([]CommentFilter{{Author: "coderabbitai"}})
	b.ApplyCommentFilters(nil)

	if !slices.Equal(b.TopLevelCommentIDs, []string{"c1", "c2"}) {
		t.Errorf("b.TopLevelCommentIDs = %v, want [c1 c2] — a's filtering leaked", b.TopLevelCommentIDs)
	}
	if shared[0].ID != "c1" || shared[1].ID != "c2" {
		t.Errorf("shared = %+v, want untouched", shared)
	}
}
