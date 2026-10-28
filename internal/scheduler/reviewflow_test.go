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

// tickResolving polls a reviewer watch that had threads t1 and t2 open and now
// has only t2 (and t3 if keepT3) left, resolved by whoever resolvedBy names.
func tickResolving(t *testing.T, resolvedBy map[string]string, self string) []string {
	t.Helper()
	ctx := context.Background()
	store := openTestStore(t)
	now := time.Now()
	reg := registry.Registration{
		PRURL: "https://github.com/o/r/pull/1", SessionID: "sess1", Provider: "github", CIBucket: "none",
		NextDueAt: now.Add(-time.Second), CreatedAt: now, Persona: registry.PersonaReviewer,
	}
	if err := store.Add(ctx, reg); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateState(ctx, reg.PRURL, "sess1", registry.StateUpdate{
		CIBucket: "none", NextDueAt: now.Add(-time.Second), HeadSHA: "aaaaaaaaaa",
		UnresolvedThreadIDs: []string{"t1", "t2", "t3"}, ThreadCommentsKnown: true,
	}); err != nil {
		t.Fatal(err)
	}
	gh := &fakeGitHubChecker{selfLogin: self, results: map[string]provider.CheckResult{reg.PRURL: {
		State: "open", HeadSHA: "aaaaaaaaaa", UnresolvedThreadIDs: []string{"t3"}, ResolvedBy: resolvedBy,
	}}, budgets: provider.GitHubBudgets{
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

// Resolving a thread yourself is not news to you; resolving someone else's is.
func TestThreadResolvedIsNotReportedWhenTheWorkersOwnAccountResolvedIt(t *testing.T) {
	if got := tickResolving(t, map[string]string{"t1": "Alice", "t2": "alice"}, "alice"); len(got) != 0 {
		t.Errorf("sent %q for threads the worker's own account resolved", got)
	}

	got := tickResolving(t, map[string]string{"t1": "bob", "t2": "bob"}, "alice")
	if len(got) != 1 || got[0] != "sess1: [pr-watch] o/r#1: THREAD_RESOLVED 2 thread(s) resolved, 1 unresolved left" {
		t.Errorf("sent %q, want the resolve by bob reported", got)
	}

	// Mixed: only the one resolved by someone else counts.
	got = tickResolving(t, map[string]string{"t1": "alice", "t2": "bob"}, "alice")
	if len(got) != 1 || !strings.Contains(got[0], "1 thread(s) resolved, 1 unresolved left") {
		t.Errorf("sent %q, want one thread counted (the one bob resolved)", got)
	}

	// A resolver the provider does not name could be anyone: it is reported.
	got = tickResolving(t, nil, "alice")
	if len(got) != 1 || !strings.Contains(got[0], "2 thread(s) resolved") {
		t.Errorf("sent %q, want an unattributed resolve reported", got)
	}
}

// selfReviewTick polls a reviewer watch once with result, after making it due.
func selfReviewTick(t *testing.T, store *registry.Store, sched *Scheduler, gh *fakeGitHubChecker, url string, result provider.CheckResult) registry.Registration {
	t.Helper()
	ctx := context.Background()
	reg := getReg(t, store, url, "sess1")
	if err := store.UpdateState(ctx, url, "sess1", stateUpdateFromReg(reg, time.Now().Add(-time.Second))); err != nil {
		t.Fatal(err)
	}
	gh.results = map[string]provider.CheckResult{url: result}
	if err := sched.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	return getReg(t, store, url, "sess1")
}

func newSelfReviewWatch(t *testing.T) (*registry.Store, *Scheduler, *fakeGitHubChecker, string) {
	t.Helper()
	store := openTestStore(t)
	now := time.Now()
	reg := registry.Registration{PRURL: "https://github.com/o/r/pull/1", SessionID: "sess1", Provider: "github", CIBucket: "none", NextDueAt: now.Add(-time.Second), CreatedAt: now, Persona: registry.PersonaReviewer}
	if err := store.Add(context.Background(), reg); err != nil {
		t.Fatal(err)
	}
	gh := &fakeGitHubChecker{budgets: provider.GitHubBudgets{
		GraphQL: provider.RateBudget{Limit: 5000, Remaining: 4999, ResetAt: now.Add(time.Hour)},
		REST:    provider.RateBudget{Limit: 5000, Remaining: 4999, ResetAt: now.Add(time.Hour)},
	}}
	return store, &Scheduler{Store: store, GitHub: gh, Sessions: alwaysLiveSessions(), Notify: &fakeNotifier{}}, gh, reg.PRURL
}

func TestSelfReviewCommitFromTheProviderIsStored(t *testing.T) {
	store, sched, gh, url := newSelfReviewWatch(t)
	got := selfReviewTick(t, store, sched, gh, url, provider.CheckResult{
		State: "open", HeadSHA: "new", SelfReviewState: "COMMENTED", SelfReviewSHA: "old",
	})
	if got.SelfReviewSHA != "old" {
		t.Errorf("SelfReviewSHA = %q, want old, the commit the provider says the review was on", got.SelfReviewSHA)
	}
}

// GitLab does not say which commit an approval was on, so the head the worker
// saw when it first saw the verdict stands in, and stays while the verdict does.
func TestSelfReviewCommitFallsBackToTheHeadSeenWithTheVerdict(t *testing.T) {
	store, sched, gh, url := newSelfReviewWatch(t)
	got := selfReviewTick(t, store, sched, gh, url, provider.CheckResult{State: "open", HeadSHA: "h1", SelfReviewState: "APPROVED"})
	if got.SelfReviewSHA != "h1" {
		t.Fatalf("SelfReviewSHA = %q, want h1, the head seen with the new verdict", got.SelfReviewSHA)
	}
	got = selfReviewTick(t, store, sched, gh, url, provider.CheckResult{State: "open", HeadSHA: "h2", SelfReviewState: "APPROVED"})
	if got.SelfReviewSHA != "h1" {
		t.Errorf("SelfReviewSHA = %q after a push with the same verdict, want h1 kept", got.SelfReviewSHA)
	}
	got = selfReviewTick(t, store, sched, gh, url, provider.CheckResult{State: "open", HeadSHA: "h2"})
	if got.SelfReviewSHA != "" {
		t.Errorf("SelfReviewSHA = %q after the verdict went away, want it cleared", got.SelfReviewSHA)
	}
}
