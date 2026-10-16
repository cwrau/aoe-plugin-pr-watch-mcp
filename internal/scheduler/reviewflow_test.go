package scheduler

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cwrau/aoe-plugin-pr-watch-mcp/internal/provider"
	"github.com/cwrau/aoe-plugin-pr-watch-mcp/internal/registry"
)

// tickWithBaseline registers a watch whose stored head is "aaa", polls it once
// with result and returns what was sent to the session.
func tickWithBaseline(t *testing.T, persona string, filter []string, prevRequested []string, result provider.CheckResult, self string) []string {
	t.Helper()
	ctx := context.Background()
	store := openTestStore(t)
	now := time.Now()
	reg := registry.Registration{
		PRURL: "https://github.com/o/r/pull/1", SessionID: "sess1", Provider: "github", CIBucket: "none",
		NextDueAt: now.Add(-time.Second), CreatedAt: now, Persona: persona, EventFilter: filter,
	}
	if err := store.Add(ctx, reg); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateState(ctx, reg.PRURL, "sess1", registry.StateUpdate{
		CIBucket: "none", NextDueAt: now.Add(-time.Second), HeadSHA: "aaaaaaaaaa", RequestedReviewers: prevRequested,
	}); err != nil {
		t.Fatal(err)
	}
	result.State = "open"
	gh := &fakeGitHubChecker{selfLogin: self, results: map[string]provider.CheckResult{reg.PRURL: result}, budgets: provider.GitHubBudgets{
		GraphQL: provider.RateBudget{Limit: 5000, Remaining: 4999, ResetAt: now.Add(time.Hour)},
		REST:    provider.RateBudget{Limit: 5000, Remaining: 4999, ResetAt: now.Add(time.Hour)},
	}}
	notify := &fakeNotifier{}
	sched := &Scheduler{Store: store, GitHub: gh, Sessions: alwaysLiveSessions(), Notify: notify}
	if err := sched.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}
	return notify.sent
}

func TestReviewerPersonaGetsPushPingsButAuthorDoesNot(t *testing.T) {
	pushed := provider.CheckResult{HeadSHA: "bbbbbbbbbb", HeadAuthor: "carol", CIBucket: "failure"}

	got := tickWithBaseline(t, registry.PersonaReviewer, nil, nil, pushed, "alice")
	if len(got) != 1 || got[0] != "sess1: [pr-watch] o/r#1: PUSH new commits by carol, head now bbbbbbb" {
		t.Errorf("reviewer sent %q, want one PUSH ping and no CI one", got)
	}

	if got := tickWithBaseline(t, registry.PersonaAuthor, nil, nil, pushed, "alice"); len(got) != 1 || !strings.Contains(got[0], "CI failure") || strings.Contains(got[0], "PUSH") {
		t.Errorf("author sent %q, want only the CI failure (PUSH is opt-in)", got)
	}

	if got := tickWithBaseline(t, registry.PersonaAuthor, []string{"PUSH"}, nil, pushed, "alice"); len(got) != 1 || !strings.Contains(got[0], "PUSH") {
		t.Errorf("author naming PUSH sent %q, want the PUSH ping", got)
	}
}

func TestReviewRequestedOnlyPingsWhenItIsForMe(t *testing.T) {
	forOther := provider.CheckResult{HeadSHA: "aaaaaaaaaa", RequestedReviewers: []string{"bob"}}
	if got := tickWithBaseline(t, registry.PersonaReviewer, nil, nil, forOther, "alice"); len(got) != 0 {
		t.Errorf("a request for someone else sent %q", got)
	}

	forMe := provider.CheckResult{HeadSHA: "aaaaaaaaaa", RequestedReviewers: []string{"bob", "Alice"}}
	got := tickWithBaseline(t, registry.PersonaReviewer, nil, []string{"bob"}, forMe, "alice")
	if len(got) != 1 || got[0] != "sess1: [pr-watch] o/r#1: REVIEW_REQUESTED review requested from you" {
		t.Errorf("a request for me sent %q", got)
	}

	// Already requested at the last poll: nothing new.
	if got := tickWithBaseline(t, registry.PersonaReviewer, nil, []string{"alice"}, provider.CheckResult{HeadSHA: "aaaaaaaaaa", RequestedReviewers: []string{"alice"}}, "alice"); len(got) != 0 {
		t.Errorf("a standing request sent %q", got)
	}
}

func TestReviewFlowStateIsPersisted(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	now := time.Now()
	reg := registry.Registration{PRURL: "https://github.com/o/r/pull/1", SessionID: "sess1", Provider: "github", CIBucket: "none", NextDueAt: now.Add(-time.Second), CreatedAt: now, Persona: registry.PersonaReviewer}
	if err := store.Add(ctx, reg); err != nil {
		t.Fatal(err)
	}
	gh := &fakeGitHubChecker{results: map[string]provider.CheckResult{reg.PRURL: {
		State: "open", HeadSHA: "cccc", IsDraft: true, RequestedReviewers: []string{"alice"}, SelfReviewState: "APPROVED",
	}}, budgets: provider.GitHubBudgets{
		GraphQL: provider.RateBudget{Limit: 5000, Remaining: 4999, ResetAt: now.Add(time.Hour)},
		REST:    provider.RateBudget{Limit: 5000, Remaining: 4999, ResetAt: now.Add(time.Hour)},
	}}
	sched := &Scheduler{Store: store, GitHub: gh, Sessions: alwaysLiveSessions(), Notify: &fakeNotifier{}}
	if err := sched.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	got := getReg(t, store, reg.PRURL, "sess1")
	if got.HeadSHA != "cccc" || !got.IsDraft || len(got.RequestedReviewers) != 1 || got.SelfReviewState != "APPROVED" {
		t.Errorf("stored = %+v", got)
	}
}
