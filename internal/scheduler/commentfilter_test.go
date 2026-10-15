package scheduler

import (
	"context"
	"testing"
	"time"

	"github.com/cwrau/aoe-plugin-pr-watch-mcp/internal/provider"
	"github.com/cwrau/aoe-plugin-pr-watch-mcp/internal/registry"
)

func commentFilterFixture(t *testing.T, comments []provider.TopLevelComment) (*Scheduler, *fakeGitHubChecker, *fakeNotifier, *registry.Store) {
	t.Helper()
	store := openTestStore(t)
	now := time.Now()
	if err := store.Add(context.Background(), registry.Registration{
		PRURL: "https://github.com/o/r/pull/1", SessionID: "sess1", Provider: "github",
		NextDueAt: now.Add(-time.Minute), CreatedAt: now,
	}); err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	gh := &fakeGitHubChecker{result: providerResult(comments)}
	notify := &fakeNotifier{}
	sched := &Scheduler{
		Store: store, GitHub: gh, GitLab: &fakeGitLabChecker{}, Sessions: alwaysLiveSessions(), Notify: notify,
		CommentFilters: func() []provider.CommentFilter {
			return []provider.CommentFilter{{Author: "coderabbitai", Contains: []string{"review in progress"}}}
		},
	}
	return sched, gh, notify, store
}

// providerResult builds a CheckResult the way the real providers do: raw
// comments plus the unfiltered derived fields.
func providerResult(comments []provider.TopLevelComment) provider.CheckResult {
	r := provider.CheckResult{State: "open", TopLevelComments: comments}
	r.ApplyCommentFilters(nil)
	return r
}

// makeDue puts the registration back in the due heap so the next Tick checks it.
func makeDue(t *testing.T, store *registry.Store) {
	t.Helper()
	reg := getReg(t, store, "https://github.com/o/r/pull/1", "sess1")
	if err := store.UpdateState(context.Background(), reg.PRURL, reg.SessionID, stateUpdateFromReg(reg, time.Now().Add(-time.Minute))); err != nil {
		t.Fatalf("UpdateState() error = %v", err)
	}
}

// TestTickDropsFilteredCommentAndPingsOnceItsEditStopsMatching: the exact
// reported noise — CodeRabbit's "review in progress" placeholder — must not
// ping, must not be stored as seen, and must ping when CodeRabbit edits the
// same comment into the real review (the same id, now not matching).
func TestTickDropsFilteredCommentAndPingsOnceItsEditStopsMatching(t *testing.T) {
	ctx := context.Background()
	placeholder := provider.TopLevelComment{ID: "c1", Href: "https://github.com/o/r/pull/1#issuecomment-1", Author: "coderabbitai", Body: "Review in progress..."}
	sched, gh, notify, store := commentFilterFixture(t, []provider.TopLevelComment{placeholder})

	if err := sched.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}
	if len(notify.sent) != 0 {
		t.Fatalf("notify.sent = %v, want none for the filtered placeholder", notify.sent)
	}
	if got := getReg(t, store, "https://github.com/o/r/pull/1", "sess1"); len(got.TopLevelCommentIDs) != 0 {
		t.Fatalf("stored TopLevelCommentIDs = %v, want none (a filtered comment must not be remembered as seen)", got.TopLevelCommentIDs)
	}

	edited := placeholder
	edited.Body = "## Walkthrough\nactual findings"
	gh.result = providerResult([]provider.TopLevelComment{edited})
	makeDue(t, store)
	if err := sched.Tick(ctx); err != nil {
		t.Fatalf("Tick() (after edit) error = %v", err)
	}
	want := "sess1: [pr-watch] o/r#1: COMMENT 1 new comment(s), 1 total (https://github.com/o/r/pull/1#issuecomment-1)"
	if len(notify.sent) != 1 || notify.sent[0] != want {
		t.Errorf("notify.sent = %v, want exactly [%q]", notify.sent, want)
	}
}

func TestTickWithoutCommentFiltersStillPingsOnTheSameComment(t *testing.T) {
	ctx := context.Background()
	placeholder := provider.TopLevelComment{ID: "c1", Href: "https://github.com/o/r/pull/1#issuecomment-1", Author: "coderabbitai", Body: "Review in progress..."}
	sched, _, notify, _ := commentFilterFixture(t, []provider.TopLevelComment{placeholder})
	sched.CommentFilters = nil

	if err := sched.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}
	if len(notify.sent) != 1 {
		t.Errorf("notify.sent = %v, want one COMMENT ping when no filter rules are configured", notify.sent)
	}
}
