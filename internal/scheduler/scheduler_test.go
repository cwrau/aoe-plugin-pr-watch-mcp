package scheduler

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cwrau/aoe-plugin-pr-watch-mcp/internal/aoeclient"
	"github.com/cwrau/aoe-plugin-pr-watch-mcp/internal/provider"
	"github.com/cwrau/aoe-plugin-pr-watch-mcp/internal/registry"
)

type fakeGitHubChecker struct {
	result provider.CheckResult // applied to every url when results is nil
	// results, when set, is returned verbatim instead of applying result to
	// every url — a url present in urls but absent here simulates "missing
	// from the batch response" (e.g. PR/repo not found).
	results  map[string]provider.CheckResult
	budgets  provider.GitHubBudgets
	err      error
	calls    int      // counts CheckBatch invocations, not urls checked
	lastURLs []string // the urls argument from the most recent CheckBatch call, for dedup assertions

	// discovered backs DiscoverBranchPRs — keyed by provider.GitHubBranchKey,
	// nil/absent means "no open PR found" for that query.
	discovered        map[string]string
	discoverBudget    provider.RateBudget
	discoverErr       error
	discoverCalls     int
	lastDiscoverItems []provider.GitHubBranchQuery

	// selfLogin/selfLoginErr back SelfLogin — zero value ("" login, nil
	// error) means self-authored-review filtering never matches anything,
	// which is the right default for every test that doesn't care about it.
	selfLogin    string
	selfLoginErr error

	// afterCheck, when set, runs just before CheckBatch returns its results,
	// e.g. to cancel the poll's context as a deadline passing mid-batch.
	afterCheck func()
}

func (f *fakeGitHubChecker) CheckBatch(ctx context.Context, urls []string) (map[string]provider.CheckResult, provider.GitHubBudgets, error) {
	f.calls++
	f.lastURLs = urls
	if f.err != nil {
		return nil, provider.GitHubBudgets{}, f.err
	}
	if f.afterCheck != nil {
		defer f.afterCheck()
	}
	if f.results != nil {
		return f.results, f.budgets, nil
	}
	out := make(map[string]provider.CheckResult, len(urls))
	for _, u := range urls {
		out[u] = f.result
	}
	return out, f.budgets, nil
}

func (f *fakeGitHubChecker) DiscoverBranchPRs(ctx context.Context, items []provider.GitHubBranchQuery) (map[string]string, provider.RateBudget, error) {
	f.discoverCalls++
	f.lastDiscoverItems = items
	if f.discoverErr != nil {
		return nil, provider.RateBudget{}, f.discoverErr
	}
	if f.discovered == nil {
		return map[string]string{}, f.discoverBudget, nil
	}
	return f.discovered, f.discoverBudget, nil
}

func (f *fakeGitHubChecker) SelfLogin(ctx context.Context) (string, error) {
	return f.selfLogin, f.selfLoginErr
}

type fakeGitLabChecker struct {
	result provider.CheckResult
	budget provider.RateBudget
	err    error
	calls  int

	// discoverMRURL/discoverFound back DiscoverBranchMR.
	discoverMRURL  string
	discoverFound  bool
	discoverBudget provider.RateBudget
	discoverErr    error
	discoverCalls  int

	// selfUsername/selfUsernameErr back SelfUsername — zero value means
	// self-authored-review filtering never matches, same default rationale
	// as fakeGitHubChecker.selfLogin.
	selfUsername    string
	selfUsernameErr error
}

func (f *fakeGitLabChecker) Check(ctx context.Context, url string) (provider.CheckResult, provider.RateBudget, error) {
	f.calls++
	return f.result, f.budget, f.err
}

func (f *fakeGitLabChecker) DiscoverBranchMR(ctx context.Context, host, projectPath, branch string) (string, bool, provider.RateBudget, error) {
	f.discoverCalls++
	return f.discoverMRURL, f.discoverFound, f.discoverBudget, f.discoverErr
}

func (f *fakeGitLabChecker) SelfUsername(ctx context.Context, host string) (string, error) {
	return f.selfUsername, f.selfUsernameErr
}

type fakeNotifier struct {
	sent []string // "sessionID: message"
	err  error    // when non-nil, every Send fails with it
}

func (f *fakeNotifier) Send(ctx context.Context, sessionID, message string) error {
	if err := ctx.Err(); err != nil {
		return err // like the real RPC: a cancelled call delivers nothing
	}
	f.sent = append(f.sent, sessionID+": "+message)
	return f.err
}

func openTestStore(t *testing.T) *registry.Store {
	t.Helper()
	s, err := registry.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("registry.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestEventMessagePrefixesAutomatedPing(t *testing.T) {
	got := eventMessage("o/r#1", provider.Event{Kind: "MERGED", Message: "MERGED"})
	if got != "[pr-watch] o/r#1: MERGED" {
		t.Errorf("eventMessage() = %q, want %q", got, "[pr-watch] o/r#1: MERGED")
	}
}

func TestEventMessageAppendsHrefWhenPresent(t *testing.T) {
	got := eventMessage("o/r#1", provider.Event{Kind: "CI", Message: "failure", Href: "https://github.com/o/r/actions/runs/1"})
	want := "[pr-watch] o/r#1: CI failure (https://github.com/o/r/actions/runs/1)"
	if got != want {
		t.Errorf("eventMessage() = %q, want %q", got, want)
	}
}

func TestEventMessageOmitsHrefWhenEmpty(t *testing.T) {
	got := eventMessage("o/r#1", provider.Event{Kind: "CI", Message: "failure"})
	want := "[pr-watch] o/r#1: CI failure"
	if got != want {
		t.Errorf("eventMessage() = %q, want %q", got, want)
	}
}

func TestEventsMessageEmptyForNoEvents(t *testing.T) {
	if got := eventsMessage("o/r#1", nil); got != "" {
		t.Errorf("eventsMessage(nil) = %q, want empty", got)
	}
}

func TestEventsMessageSingleEventMatchesEventMessage(t *testing.T) {
	ev := provider.Event{Kind: "CI", Message: "failure", Href: "https://example.com/run/1"}
	if got, want := eventsMessage("o/r#1", []provider.Event{ev}), eventMessage("o/r#1", ev); got != want {
		t.Errorf("eventsMessage(single) = %q, want it to match eventMessage() = %q", got, want)
	}
}

func TestEventsMessageCombinesMultipleEvents(t *testing.T) {
	got := eventsMessage("o/r#1", []provider.Event{
		{Kind: "REVIEW", Message: "bob CHANGES_REQUESTED"},
		{Kind: "CI", Message: "failure", Href: "https://example.com/run/1"},
	})
	want := "[pr-watch] o/r#1, 2 changes:\n- REVIEW bob CHANGES_REQUESTED\n- CI failure (https://example.com/run/1)"
	if got != want {
		t.Errorf("eventsMessage() = %q, want %q", got, want)
	}
}

func TestFilterEventsEmptyFilterPassesEverythingThrough(t *testing.T) {
	events := []provider.Event{{Kind: "REVIEW"}, {Kind: "CI"}}
	got := filterEvents(events, nil)
	if len(got) != 2 {
		t.Errorf("filterEvents(events, nil) = %v, want all events unchanged", got)
	}
}

func TestFilterEventsKeepsOnlyListedKinds(t *testing.T) {
	events := []provider.Event{{Kind: "REVIEW", Message: "a"}, {Kind: "CI", Message: "b"}, {Kind: "MERGED", Message: "c"}}
	got := filterEvents(events, []string{"CI", "MERGED"})
	if len(got) != 2 || got[0].Kind != "CI" || got[1].Kind != "MERGED" {
		t.Errorf("filterEvents() = %v, want just CI and MERGED, in original order", got)
	}
}

func TestTickSendsOnCIChange(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	now := time.Now()
	reg := registry.Registration{
		PRURL: "https://github.com/o/r/pull/1", SessionID: "sess1", Provider: "github",
		CIBucket: "pending", NextDueAt: now.Add(-time.Minute), CreatedAt: now,
	}
	if err := store.Add(ctx, reg); err != nil {
		t.Fatalf("Add() error = %v", err)
	}

	gh := &fakeGitHubChecker{result: provider.CheckResult{State: "open", CIBucket: "failure"}}
	notify := &fakeNotifier{}
	sched := &Scheduler{
		Store:    store,
		GitHub:   gh,
		Sessions: alwaysLiveSessions(),
		Notify:   notify,
	}

	if err := sched.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}
	if gh.calls != 1 {
		t.Errorf("GitHub checker calls = %d, want 1", gh.calls)
	}
	if len(notify.sent) != 1 || notify.sent[0] != "sess1: [pr-watch] o/r#1: CI failure" {
		t.Errorf("notify.sent = %v, want a single CI-failure message to sess1", notify.sent)
	}
}

// TestTickPingsNameWhichPRTheyreAbout guards against an ambiguous ping: a
// session watching two different PRs at once must be able to tell which
// one each message is about just by reading it, not by cross-referencing
// state elsewhere.
func TestTickPingsNameWhichPRTheyreAbout(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	now := time.Now()
	reg1 := registry.Registration{
		PRURL: "https://github.com/o/r/pull/1", SessionID: "sess1", Provider: "github",
		CIBucket: "pending", NextDueAt: now.Add(-time.Minute), CreatedAt: now,
	}
	reg2 := registry.Registration{
		PRURL: "https://github.com/o/r/pull/2", SessionID: "sess1", Provider: "github",
		CIBucket: "pending", NextDueAt: now.Add(-time.Minute), CreatedAt: now,
	}
	if err := store.Add(ctx, reg1); err != nil {
		t.Fatalf("Add(reg1) error = %v", err)
	}
	if err := store.Add(ctx, reg2); err != nil {
		t.Fatalf("Add(reg2) error = %v", err)
	}

	gh := &fakeGitHubChecker{result: provider.CheckResult{State: "open", CIBucket: "failure"}}
	notify := &fakeNotifier{}
	sched := &Scheduler{Store: store, GitHub: gh, Sessions: alwaysLiveSessions(), Notify: notify}

	if err := sched.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}

	// Both PRs belong to one session, so they arrive as one combined message
	// (see TestTickBatchesEventsOfOneSessionAcrossRegistrations); each update
	// inside it must still name its own PR.
	if len(notify.sent) != 1 {
		t.Fatalf("notify.sent = %v, want 1 combined message for the session", notify.sent)
	}
	for _, want := range []string{"o/r#1: CI failure", "o/r#2: CI failure"} {
		if !strings.Contains(notify.sent[0], want) {
			t.Errorf("notify.sent[0] = %q, want it to name %q", notify.sent[0], want)
		}
	}
}

// TestTickMergesMultipleEventsIntoOneMessage guards the actual point of
// eventsMessage: a single poll that turns up more than one change (here, a
// new review landing in the same tick CI turns red) must still produce
// exactly one Send call, not one per event, so the session sees one ping to
// react to instead of several arriving back-to-back.
func TestTickMergesMultipleEventsIntoOneMessage(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	now := time.Now()
	reg := registry.Registration{
		PRURL: "https://github.com/o/r/pull/1", SessionID: "sess1", Provider: "github",
		ReviewCount: 1, CIBucket: "pending", NextDueAt: now.Add(-time.Minute), CreatedAt: now,
	}
	if err := store.Add(ctx, reg); err != nil {
		t.Fatalf("Add() error = %v", err)
	}

	gh := &fakeGitHubChecker{result: provider.CheckResult{
		State: "open", CIBucket: "failure", ReviewCount: 2, LatestReviewer: "bob", LatestReviewState: "CHANGES_REQUESTED",
	}}
	notify := &fakeNotifier{}
	sched := &Scheduler{Store: store, GitHub: gh, Sessions: alwaysLiveSessions(), Notify: notify}

	if err := sched.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}

	if len(notify.sent) != 1 {
		t.Fatalf("notify.sent = %v, want exactly 1 combined message, not one per event", notify.sent)
	}
	want := "sess1: [pr-watch] o/r#1, 2 changes:\n- REVIEW bob CHANGES_REQUESTED\n- CI failure"
	if notify.sent[0] != want {
		t.Errorf("notify.sent[0] = %q, want %q", notify.sent[0], want)
	}
}

func TestTickOnlyNotifiesFilteredEventKinds(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	now := time.Now()
	reg := registry.Registration{
		PRURL: "https://github.com/o/r/pull/1", SessionID: "sess1", Provider: "github",
		ReviewCount: 1, CIBucket: "pending", NextDueAt: now.Add(-time.Minute), CreatedAt: now,
		EventFilter: []string{"CI"},
	}
	if err := store.Add(ctx, reg); err != nil {
		t.Fatalf("Add() error = %v", err)
	}

	// Same both-at-once REVIEW+CI change as TestTickMergesMultipleEventsIntoOneMessage,
	// but this registration only asked to hear about CI — the REVIEW event
	// must still be diffed/logged (state moves on regardless) but never sent.
	gh := &fakeGitHubChecker{result: provider.CheckResult{
		State: "open", CIBucket: "failure", ReviewCount: 2, LatestReviewer: "bob", LatestReviewState: "CHANGES_REQUESTED",
	}}
	notify := &fakeNotifier{}
	sched := &Scheduler{Store: store, GitHub: gh, Sessions: alwaysLiveSessions(), Notify: notify}

	if err := sched.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}

	want := "sess1: [pr-watch] o/r#1: CI failure"
	if len(notify.sent) != 1 || notify.sent[0] != want {
		t.Errorf("notify.sent = %v, want exactly [%q] (REVIEW filtered out)", notify.sent, want)
	}
}

// TestTickFiresConflictEventOnNewConflict is a regression test:
// confirmed live that a PR/MR
// developing a merge conflict updated the badge (HasConflict is always
// persisted) but never pinged, because Diff had no CONFLICT event kind at
// all prior to this — badge-only was indistinguishable from
// filtered-out-by-choice.
// TestTickSuppressesSelfAuthoredReviewNotification is a regression test:
// confirmed live — a session got pinged
// "REVIEW cwrau COMMENTED" for its own account's review (the human, or an
// agent acting through the same gh/glab credentials), which is never news
// to the session that's watching. The badge/state must still update
// (LatestReviewState persists normally); only the notification is
// suppressed.
func TestTickSuppressesSelfAuthoredReviewNotification(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	now := time.Now()
	reg := registry.Registration{
		PRURL: "https://github.com/o/r/pull/1", SessionID: "sess1", Provider: "github",
		NextDueAt: now.Add(-time.Minute), CreatedAt: now,
	}
	if err := store.Add(ctx, reg); err != nil {
		t.Fatalf("Add() error = %v", err)
	}

	gh := &fakeGitHubChecker{
		selfLogin: "alice",
		result: provider.CheckResult{
			State: "open", ReviewCount: 1, LatestReviewer: "alice", LatestReviewState: "COMMENTED",
		},
	}
	notify := &fakeNotifier{}
	sched := &Scheduler{Store: store, GitHub: gh, GitLab: &fakeGitLabChecker{}, Sessions: alwaysLiveSessions(), Notify: notify}

	if err := sched.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}

	if len(notify.sent) != 0 {
		t.Errorf("notify.sent = %v, want none (self-authored review must not ping)", notify.sent)
	}

	got := getReg(t, store, "https://github.com/o/r/pull/1", "sess1")
	if got.LatestReviewer != "alice" || got.LatestReviewState != "COMMENTED" {
		t.Errorf("got = %+v, want the review state persisted (badge still reflects it) even though the ping was suppressed", got)
	}
}

// TestTickStillNotifiesReviewFromSomeoneElse guards against
// over-suppressing: only a review authored by the resolved self-identity
// is dropped, never anyone else's.
// TestTickFiresReviewDismissedWhenApprovalResetByPush is a regression test
// for the exact live report: a GitLab MR an agent was watching had its
// approval silently reset (GitLab's "reset approvals on push" project
// setting) by a bot's dependency-bump push, and the agent never heard
// about it — the badge just quietly reverted to "no review".
func TestTickFiresReviewDismissedWhenApprovalResetByPush(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	now := time.Now()
	reg := registry.Registration{
		PRURL: "https://gitlab.corp.example/team/project/-/merge_requests/1019", SessionID: "sess1", Provider: "gitlab",
		NextDueAt: now.Add(-time.Minute), CreatedAt: now,
		ReviewCount: 1, LatestReviewState: "APPROVED", LatestReviewer: "bob", ReviewHref: "https://gitlab.corp.example/team/project/-/merge_requests/1019#note_1",
	}
	if err := store.Add(ctx, reg); err != nil {
		t.Fatalf("Add() error = %v", err)
	}

	// A fresh poll after the push: GitLab now reports zero approvals, so
	// GitLabChecker.Check would leave every review field at its zero value.
	gl := &fakeGitLabChecker{result: provider.CheckResult{State: "open", ReviewCount: 0}}
	notify := &fakeNotifier{}
	sched := &Scheduler{Store: store, GitHub: &fakeGitHubChecker{}, GitLab: gl, Sessions: alwaysLiveSessions(), Notify: notify}

	if err := sched.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}

	want := "sess1: [pr-watch] team/project!1019: REVIEW_DISMISSED bob's APPROVED review was dismissed (https://gitlab.corp.example/team/project/-/merge_requests/1019#note_1)"
	if len(notify.sent) != 1 || notify.sent[0] != want {
		t.Errorf("notify.sent = %v, want exactly [%q]", notify.sent, want)
	}

	got := getReg(t, store, reg.PRURL, "sess1")
	if got.LatestReviewState != "" || got.LatestReviewer != "" {
		t.Errorf("got = %+v, want the review state cleared to match the reset approval", got)
	}
}

func TestTickStillNotifiesReviewFromSomeoneElse(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	now := time.Now()
	reg := registry.Registration{
		PRURL: "https://github.com/o/r/pull/1", SessionID: "sess1", Provider: "github",
		NextDueAt: now.Add(-time.Minute), CreatedAt: now,
	}
	if err := store.Add(ctx, reg); err != nil {
		t.Fatalf("Add() error = %v", err)
	}

	gh := &fakeGitHubChecker{
		selfLogin: "alice",
		result: provider.CheckResult{
			State: "open", ReviewCount: 1, LatestReviewer: "bob", LatestReviewState: "APPROVED",
		},
	}
	notify := &fakeNotifier{}
	sched := &Scheduler{Store: store, GitHub: gh, GitLab: &fakeGitLabChecker{}, Sessions: alwaysLiveSessions(), Notify: notify}

	if err := sched.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}

	want := "sess1: [pr-watch] o/r#1: REVIEW bob APPROVED"
	if len(notify.sent) != 1 || notify.sent[0] != want {
		t.Errorf("notify.sent = %v, want exactly [%q] (a real review from someone else must still ping)", notify.sent, want)
	}
}

// TestTickSuppressesSelfAuthoredGitLabReviewNotification mirrors the
// GitHub case for GitLab's per-host self-username resolution.
func TestTickSuppressesSelfAuthoredGitLabReviewNotification(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	now := time.Now()
	reg := registry.Registration{
		PRURL: "https://gitlab.corp.example/team/project/-/merge_requests/17", SessionID: "sess1", Provider: "gitlab",
		NextDueAt: now.Add(-time.Minute), CreatedAt: now,
	}
	if err := store.Add(ctx, reg); err != nil {
		t.Fatalf("Add() error = %v", err)
	}

	gl := &fakeGitLabChecker{
		selfUsername: "alice",
		result: provider.CheckResult{
			State: "open", ReviewCount: 1, LatestReviewer: "alice", LatestReviewState: "APPROVED",
		},
	}
	notify := &fakeNotifier{}
	sched := &Scheduler{Store: store, GitHub: &fakeGitHubChecker{}, GitLab: gl, Sessions: alwaysLiveSessions(), Notify: notify}

	if err := sched.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}

	if len(notify.sent) != 0 {
		t.Errorf("notify.sent = %v, want none (self-authored review must not ping)", notify.sent)
	}
}

func TestFilterSelfAuthoredEmptySelfAuthorPassesEverythingThrough(t *testing.T) {
	events := []provider.Event{{Kind: "REVIEW", Message: "bob APPROVED"}}
	got := filterSelfAuthored(events, provider.CheckResult{LatestReviewer: "bob"}, "")
	if len(got) != 1 {
		t.Errorf("filterSelfAuthored() = %v, want unchanged when selfAuthor is empty", got)
	}
}

func TestFilterSelfAuthoredOnlyDropsReviewKind(t *testing.T) {
	events := []provider.Event{{Kind: "REVIEW", Message: "cwrau APPROVED"}, {Kind: "CI", Message: "failure"}}
	got := filterSelfAuthored(events, provider.CheckResult{LatestReviewer: "alice"}, "alice")
	if len(got) != 1 || got[0].Kind != "CI" {
		t.Errorf("filterSelfAuthored() = %v, want just the CI event left", got)
	}
}

// TestFilterSelfAuthoredDropsSelfTopLevelComment is a regression test:
// confirmed live, kubernetes/cloud-provider-openstack#3063 pinged COMMENT
// for a top-level comment cwrau (the worker's own account) had posted —
// self-filtering only ever checked REVIEW, never COMMENT at all.
func TestFilterSelfAuthoredDropsSelfTopLevelComment(t *testing.T) {
	href := "https://github.com/o/r/pull/1#issuecomment-1"
	events := []provider.Event{{Kind: "COMMENT", Message: "1 new comment(s), 1 total", Href: href}}
	got := filterSelfAuthored(events, provider.CheckResult{LatestCommentHref: href, LatestCommentAuthor: "alice"}, "alice")
	if len(got) != 0 {
		t.Errorf("filterSelfAuthored() = %v, want none (self-authored top-level comment must not ping)", got)
	}
}

// TestFilterSelfAuthoredKeepsThreadCommentEvenFromSelf documents the known
// gap this fix doesn't close: a thread-based COMMENT event (new unresolved
// review thread) has no per-thread author available from either provider
// today, so it's never suppressed here even if selfAuthor happens to match
// the PR's overall latest comment author — Href distinguishes the two
// cases (a thread event's Href is the review's own href, never
// result.LatestCommentHref), so this must not be dropped.
func TestFilterSelfAuthoredKeepsThreadCommentEvenFromSelf(t *testing.T) {
	events := []provider.Event{{Kind: "COMMENT", Message: "1 new unresolved thread(s), 1 unresolved total", Href: "https://github.com/o/r/pull/1#pullrequestreview-1"}}
	got := filterSelfAuthored(events, provider.CheckResult{LatestCommentHref: "https://github.com/o/r/pull/1#issuecomment-1", LatestCommentAuthor: "alice"}, "alice")
	if len(got) != 1 {
		t.Errorf("filterSelfAuthored() = %v, want the thread COMMENT event kept (no author data for it, Href doesn't match a top-level comment)", got)
	}
}

func TestTickFiresConflictEventOnNewConflict(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	now := time.Now()
	reg := registry.Registration{
		PRURL: "https://github.com/o/r/pull/1", SessionID: "sess1", Provider: "github",
		NextDueAt: now.Add(-time.Minute), CreatedAt: now, HasConflict: false,
	}
	if err := store.Add(ctx, reg); err != nil {
		t.Fatalf("Add() error = %v", err)
	}

	gh := &fakeGitHubChecker{result: provider.CheckResult{State: "open", HasConflict: true}}
	notify := &fakeNotifier{}
	sched := &Scheduler{Store: store, GitHub: gh, GitLab: &fakeGitLabChecker{}, Sessions: alwaysLiveSessions(), Notify: notify}

	if err := sched.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}

	want := "sess1: [pr-watch] o/r#1: CONFLICT has a merge conflict with its target branch"
	if len(notify.sent) != 1 || notify.sent[0] != want {
		t.Errorf("notify.sent = %v, want exactly [%q]", notify.sent, want)
	}

	got := getReg(t, store, "https://github.com/o/r/pull/1", "sess1")
	if !got.HasConflict {
		t.Error("HasConflict = false after persisting, want true")
	}
}

// TestTickFilteredOutConflictStillUpdatesBadgeSilently matches the exact
// scenario reported live: a registration filtered to
// ["MERGED","CLOSED","MERGE_QUEUE"] must keep persisting HasConflict (so
// the badge still changes) without ever notifying, now that CONFLICT is a
// real, filterable event kind.
func TestTickFilteredOutConflictStillUpdatesBadgeSilently(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	now := time.Now()
	reg := registry.Registration{
		PRURL: "https://github.com/o/r/pull/1", SessionID: "sess1", Provider: "github",
		NextDueAt: now.Add(-time.Minute), CreatedAt: now, HasConflict: false,
		EventFilter: []string{"MERGED", "CLOSED", "MERGE_QUEUE"},
	}
	if err := store.Add(ctx, reg); err != nil {
		t.Fatalf("Add() error = %v", err)
	}

	gh := &fakeGitHubChecker{result: provider.CheckResult{State: "open", HasConflict: true}}
	notify := &fakeNotifier{}
	sched := &Scheduler{Store: store, GitHub: gh, GitLab: &fakeGitLabChecker{}, Sessions: alwaysLiveSessions(), Notify: notify}

	if err := sched.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}

	if len(notify.sent) != 0 {
		t.Errorf("notify.sent = %v, want none (CONFLICT not in this registration's filter)", notify.sent)
	}
	got := getReg(t, store, "https://github.com/o/r/pull/1", "sess1")
	if !got.HasConflict {
		t.Error("HasConflict = false after persisting, want true — the badge must still update even when the event is filtered out of notifications")
	}
}

func TestTickSendsNothingWhenNoFilteredEventKindOccurred(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	now := time.Now()
	reg := registry.Registration{
		PRURL: "https://github.com/o/r/pull/1", SessionID: "sess1", Provider: "github",
		ReviewCount: 1, NextDueAt: now.Add(-time.Minute), CreatedAt: now,
		EventFilter: []string{"MERGED"},
	}
	if err := store.Add(ctx, reg); err != nil {
		t.Fatalf("Add() error = %v", err)
	}

	gh := &fakeGitHubChecker{result: provider.CheckResult{
		State: "open", ReviewCount: 2, LatestReviewer: "bob", LatestReviewState: "APPROVED",
	}}
	notify := &fakeNotifier{}
	sched := &Scheduler{Store: store, GitHub: gh, Sessions: alwaysLiveSessions(), Notify: notify}

	if err := sched.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}

	if len(notify.sent) != 0 {
		t.Errorf("notify.sent = %v, want none (only REVIEW happened, filter only wants MERGED)", notify.sent)
	}
}

func TestTickSkipsNotDueRegistrations(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	now := time.Now()
	reg := registry.Registration{
		PRURL: "https://github.com/o/r/pull/1", SessionID: "sess1", Provider: "github",
		NextDueAt: now.Add(time.Hour), CreatedAt: now,
	}
	if err := store.Add(ctx, reg); err != nil {
		t.Fatalf("Add() error = %v", err)
	}

	gh := &fakeGitHubChecker{}
	sched := &Scheduler{Store: store, GitHub: gh, Sessions: alwaysLiveSessions(), Notify: &fakeNotifier{}}
	if err := sched.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}
	if gh.calls != 0 {
		t.Errorf("GitHub checker calls = %d, want 0 (not due yet)", gh.calls)
	}
}

func TestTickMarksMergedInsteadOfDeregistering(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	now := time.Now()
	reg := registry.Registration{
		PRURL: "https://github.com/o/r/pull/1", SessionID: "sess1", Provider: "github",
		NextDueAt: now.Add(-time.Minute), CreatedAt: now,
	}
	if err := store.Add(ctx, reg); err != nil {
		t.Fatalf("Add() error = %v", err)
	}

	mergedAt := now.Add(-time.Minute).Truncate(time.Second)
	gh := &fakeGitHubChecker{result: provider.CheckResult{State: "merged", MergedAt: mergedAt}}
	notify := &fakeNotifier{}
	sched := &Scheduler{Store: store, GitHub: gh, Sessions: alwaysLiveSessions(), Notify: notify}
	if err := sched.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}

	remaining, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(remaining) != 1 {
		t.Fatalf("List() len = %d, want 1 (registrations are never deleted, just marked terminal)", len(remaining))
	}
	if remaining[0].Status != registry.StatusMerged {
		t.Errorf("Status = %q, want %q after MERGED", remaining[0].Status, registry.StatusMerged)
	}
	if !remaining[0].MergedAt.Equal(mergedAt) {
		t.Errorf("MergedAt = %v, want %v", remaining[0].MergedAt, mergedAt)
	}
	if len(notify.sent) != 1 || notify.sent[0] != "sess1: [pr-watch] o/r#1: MERGED" {
		t.Errorf("notify.sent = %v, want a single MERGED message", notify.sent)
	}
}

// TestTickMarksMergedForEverySessionSharingThePR pins that a real merge is
// true for every session watching that PR/MR: every row sharing the URL ends
// up merged, each getting its own notification. Each row is marked by its
// own poll, never by another session's.
func TestTickMarksMergedForEverySessionSharingThePR(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	now := time.Now()
	url := "https://github.com/o/r/pull/1"
	regA := registry.Registration{PRURL: url, SessionID: "sessA", Provider: "github", NextDueAt: now.Add(-time.Minute), CreatedAt: now}
	regB := registry.Registration{PRURL: url, SessionID: "sessB", Provider: "github", NextDueAt: now.Add(-time.Minute), CreatedAt: now}
	if err := store.Add(ctx, regA); err != nil {
		t.Fatalf("Add(regA) error = %v", err)
	}
	if err := store.Add(ctx, regB); err != nil {
		t.Fatalf("Add(regB) error = %v", err)
	}

	mergedAt := now.Add(-time.Minute).Truncate(time.Second)
	gh := &fakeGitHubChecker{result: provider.CheckResult{State: "merged", MergedAt: mergedAt}}
	notify := &fakeNotifier{}
	sched := &Scheduler{Store: store, GitHub: gh, Sessions: alwaysLiveSessions(), Notify: notify}
	if err := sched.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}

	remaining, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(remaining) != 2 {
		t.Fatalf("List() len = %d, want 2", len(remaining))
	}
	for _, r := range remaining {
		if r.Status != registry.StatusMerged || !r.MergedAt.Equal(mergedAt) {
			t.Errorf("List() row %+v, want Status %q and MergedAt %v", r, registry.StatusMerged, mergedAt)
		}
	}
	if len(notify.sent) != 2 {
		t.Errorf("notify.sent = %v, want a separate MERGED message for each session", notify.sent)
	}
}

// TestTickReportsMergeToSessionPolledAfterTheFirstOneMarkedItMerged pins that
// a session whose registration comes due later than another session's on the
// same PR/MR still hears MERGED: the earlier session's poll must not flip
// the later one's registration to merged behind its back.
func TestTickReportsMergeToSessionPolledAfterTheFirstOneMarkedItMerged(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	now := time.Now()
	url := "https://github.com/o/r/pull/1"
	early := registry.Registration{PRURL: url, SessionID: "early", Provider: "github", NextDueAt: now.Add(-time.Minute), CreatedAt: now}
	late := registry.Registration{PRURL: url, SessionID: "late", Provider: "github", NextDueAt: now.Add(time.Hour), CreatedAt: now}
	for _, r := range []registry.Registration{early, late} {
		if err := store.Add(ctx, r); err != nil {
			t.Fatalf("Add(%s) error = %v", r.SessionID, err)
		}
	}

	gh := &fakeGitHubChecker{result: provider.CheckResult{State: "merged", MergedAt: now.Add(-time.Minute).Truncate(time.Second)}}
	notify := &fakeNotifier{}
	sched := &Scheduler{Store: store, GitHub: gh, Sessions: alwaysLiveSessions(), Notify: notify}
	if err := sched.Tick(ctx); err != nil {
		t.Fatalf("first Tick() error = %v", err)
	}
	regs, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	for _, r := range regs {
		if r.SessionID != "late" {
			continue
		}
		if err := store.UpdateState(ctx, r.PRURL, r.SessionID, stateUpdateFromReg(r, now.Add(-time.Second))); err != nil {
			t.Fatalf("UpdateState() error = %v", err)
		}
	}
	if err := sched.Tick(ctx); err != nil {
		t.Fatalf("second Tick() error = %v", err)
	}

	want := map[string]bool{"early: [pr-watch] o/r#1: MERGED": false, "late: [pr-watch] o/r#1: MERGED": false}
	for _, msg := range notify.sent {
		if _, ok := want[msg]; ok {
			want[msg] = true
		}
	}
	for msg, seen := range want {
		if !seen {
			t.Errorf("notify.sent = %v, missing %q", notify.sent, msg)
		}
	}
}

func TestTickSkipsSendForArchivedSession(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	now := time.Now()
	reg := registry.Registration{
		PRURL: "https://github.com/o/r/pull/1", SessionID: "archived-sess", Provider: "github",
		CIBucket: "pending", NextDueAt: now.Add(-time.Minute), CreatedAt: now,
	}
	if err := store.Add(ctx, reg); err != nil {
		t.Fatalf("Add() error = %v", err)
	}

	gh := &fakeGitHubChecker{result: provider.CheckResult{State: "open", CIBucket: "failure"}}
	notify := &fakeNotifier{}
	sched := &Scheduler{Store: store, GitHub: gh, Sessions: alwaysArchivedSessions(), Notify: notify}
	if err := sched.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}
	if len(notify.sent) != 0 {
		t.Errorf("notify.sent = %v, want no sends to an archived session", notify.sent)
	}
	remaining, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(remaining) != 1 {
		t.Errorf("List() len = %d, want 1 (archived registration stays, just skips the send)", len(remaining))
	}
}

func TestTickStopsRegistrationForGoneSession(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	now := time.Now()
	reg := registry.Registration{
		PRURL: "https://github.com/o/r/pull/1", SessionID: "gone-sess", Provider: "github",
		NextDueAt: now.Add(-time.Minute), CreatedAt: now,
	}
	if err := store.Add(ctx, reg); err != nil {
		t.Fatalf("Add() error = %v", err)
	}

	gh := &fakeGitHubChecker{}
	sched := &Scheduler{Store: store, GitHub: gh, Sessions: alwaysGoneSessions(), Notify: &fakeNotifier{}}
	if err := sched.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}
	if gh.calls != 0 {
		t.Errorf("GitHub checker calls = %d, want 0 (session is gone, don't waste a check)", gh.calls)
	}
	remaining, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(remaining) != 1 {
		t.Fatalf("List() len = %d, want 1 (registrations are never deleted, just marked terminal)", len(remaining))
	}
	if remaining[0].Status != registry.StatusStopped {
		t.Errorf("Status = %q, want %q for a gone session's registration", remaining[0].Status, registry.StatusStopped)
	}
}

func TestTickUsesRealActiveCountsNotPlaceholder(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	now := time.Now()
	// Two GitHub registrations sharing one pool: the interval each gets
	// should reflect 2 active consumers, not the old hardcoded placeholder.
	for i, url := range []string{"https://github.com/o/r/pull/1", "https://github.com/o/r/pull/2"} {
		reg := registry.Registration{PRURL: url, SessionID: "sess", Provider: "github", NextDueAt: now.Add(-time.Duration(i) * time.Second), CreatedAt: now}
		if err := store.Add(ctx, reg); err != nil {
			t.Fatalf("Add() error = %v", err)
		}
	}

	// Remaining/window are set so the usable headroom (5, after the
	// reserved floor) is small enough that the per-registration interval
	// clears the MinPollInterval floor for both activeCount=1 and
	// activeCount=2, so the two are actually distinguishable — too much
	// headroom (or too little, tipping into the no-headroom branch) would
	// clamp both to the same value regardless of activeCount, which would
	// make this test pass or fail independent of whether Tick uses the
	// real count at all.
	gh := &fakeGitHubChecker{
		result: provider.CheckResult{State: "open", CIBucket: "success"},
		budgets: provider.GitHubBudgets{
			GraphQL: provider.RateBudget{Limit: 5000, Remaining: 505, ResetAt: now.Add(40 * time.Minute)},
			REST:    provider.RateBudget{Limit: 5000, Remaining: 505, ResetAt: now.Add(40 * time.Minute)},
		},
	}
	sched := &Scheduler{Store: store, GitHub: gh, Sessions: alwaysLiveSessions(), Notify: &fakeNotifier{}}
	if err := sched.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}

	remaining, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	// With 2 active GitHub registrations, ComputeInterval(budget, 2, now)
	// must produce a longer per-registration interval than
	// ComputeInterval(budget, 1, now) would — confirms the real count (2),
	// not the old placeholder (1), reached ComputeInterval.
	withOne := ComputeInterval(gh.budgets.GraphQL, 1, now)
	for _, r := range remaining {
		if r.NextDueAt.Sub(now) <= withOne {
			t.Errorf("registration %s got interval %v, want > single-registration interval %v (real count of 2 should widen it)", r.PRURL, r.NextDueAt.Sub(now), withOne)
		}
	}
}

// TestTickSkipsCheckWhenGitHubPoolAlreadyKnownExhausted: tick 1 checks reg1
// for real, revealing the GraphQL pool has no headroom left. reg2 becomes
// due only after that (registered post-tick-1), so tick 2 must skip its
// real check entirely — the cached exhaustion from tick 1's result is
// still within its window — rather than spend (or fail) a call to
// rediscover what's already known. Batching means every registration
// ready in the *same* tick shares one call regardless of budget state
// (there's nothing to learn mid-tick from a call that hasn't happened
// yet); the cache only pays off *across* ticks, which is what this test
// exercises.
func TestTickSkipsCheckWhenGitHubPoolAlreadyKnownExhausted(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	now := time.Now()

	reg1 := registry.Registration{PRURL: "https://github.com/o/r/pull/1", SessionID: "sess1", Provider: "github", NextDueAt: now.Add(-2 * time.Second), CreatedAt: now}
	if err := store.Add(ctx, reg1); err != nil {
		t.Fatalf("Add() error = %v", err)
	}

	gh := &fakeGitHubChecker{
		result: provider.CheckResult{State: "open", CIBucket: "success"},
		budgets: provider.GitHubBudgets{
			// Remaining exactly at the 10% floor: no headroom.
			GraphQL: provider.RateBudget{Limit: 5000, Remaining: 500, ResetAt: now.Add(10 * time.Minute)},
			REST:    provider.RateBudget{Limit: 5000, Remaining: 4999, ResetAt: now.Add(10 * time.Minute)},
		},
	}
	sched := &Scheduler{Store: store, GitHub: gh, Sessions: alwaysLiveSessions(), Notify: &fakeNotifier{}}
	if err := sched.Tick(ctx); err != nil {
		t.Fatalf("Tick() [1] error = %v", err)
	}
	if gh.calls != 1 {
		t.Fatalf("GitHub checker calls after tick 1 = %d, want 1", gh.calls)
	}

	reg2 := registry.Registration{PRURL: "https://github.com/o/r/pull/2", SessionID: "sess2", Provider: "github", NextDueAt: now.Add(-time.Second), CreatedAt: now}
	if err := store.Add(ctx, reg2); err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	if err := sched.Tick(ctx); err != nil {
		t.Fatalf("Tick() [2] error = %v", err)
	}

	if gh.calls != 1 {
		t.Errorf("GitHub checker calls after tick 2 = %d, want still 1 (reg2 should skip its real check, the pool is already known exhausted)", gh.calls)
	}

	got2 := getReg(t, store, reg2.PRURL, reg2.SessionID)
	if !got2.NextDueAt.After(now) {
		t.Errorf("reg2 NextDueAt = %v, want it rescheduled forward even though its check was skipped", got2.NextDueAt)
	}
}

// TestTickDoesNotSkipWhenCachedGitHubBudgetHasHeadroom: the inverse of the
// above — a cached-but-non-exhausted budget must never cause a skip. Both
// registrations are ready in the same tick, so they share one batched
// call; what matters is that neither was skipped, not the call count.
func TestTickDoesNotSkipWhenCachedGitHubBudgetHasHeadroom(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	now := time.Now()

	reg1 := registry.Registration{PRURL: "https://github.com/o/r/pull/1", SessionID: "sess1", Provider: "github", NextDueAt: now.Add(-2 * time.Second), CreatedAt: now}
	reg2 := registry.Registration{PRURL: "https://github.com/o/r/pull/2", SessionID: "sess2", Provider: "github", NextDueAt: now.Add(-time.Second), CreatedAt: now}
	for _, r := range []registry.Registration{reg1, reg2} {
		if err := store.Add(ctx, r); err != nil {
			t.Fatalf("Add() error = %v", err)
		}
	}

	gh := &fakeGitHubChecker{
		result: provider.CheckResult{State: "open", CIBucket: "success"},
		budgets: provider.GitHubBudgets{
			GraphQL: provider.RateBudget{Limit: 5000, Remaining: 4999, ResetAt: now.Add(time.Hour)},
			REST:    provider.RateBudget{Limit: 5000, Remaining: 4999, ResetAt: now.Add(time.Hour)},
		},
	}
	sched := &Scheduler{Store: store, GitHub: gh, Sessions: alwaysLiveSessions(), Notify: &fakeNotifier{}}
	if err := sched.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}

	if gh.calls != 1 {
		t.Errorf("GitHub checker calls = %d, want 1 (one batched call covering both, since there's plenty of headroom to skip nothing)", gh.calls)
	}
	got1 := getReg(t, store, reg1.PRURL, reg1.SessionID)
	got2 := getReg(t, store, reg2.PRURL, reg2.SessionID)
	if got1.CIBucket != "success" || got2.CIBucket != "success" {
		t.Errorf("both registrations should have been checked for real: got1.CIBucket=%q got2.CIBucket=%q", got1.CIBucket, got2.CIBucket)
	}
}

// TestTickBacksOffOnlyTheRegistrationMissingFromABatchResponse: a batch
// response that resolved some URLs but not others (e.g. one PR/repo not
// found) must back off only the missing registration, leaving the
// successfully-resolved ones to persist normally.
func TestTickBacksOffOnlyTheRegistrationMissingFromABatchResponse(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	now := time.Now()

	reg1 := registry.Registration{PRURL: "https://github.com/o/r/pull/1", SessionID: "sess1", Provider: "github", NextDueAt: now.Add(-2 * time.Second), CreatedAt: now}
	reg2 := registry.Registration{PRURL: "https://github.com/o/r/pull/2", SessionID: "sess2", Provider: "github", NextDueAt: now.Add(-time.Second), CreatedAt: now}
	for _, r := range []registry.Registration{reg1, reg2} {
		if err := store.Add(ctx, r); err != nil {
			t.Fatalf("Add() error = %v", err)
		}
	}

	gh := &fakeGitHubChecker{
		// Only reg1's URL is present in the response — reg2's is simply
		// missing, as if its repository/PR came back null.
		results: map[string]provider.CheckResult{
			reg1.PRURL: {State: "open", CIBucket: "success"},
		},
		budgets: provider.GitHubBudgets{
			GraphQL: provider.RateBudget{Limit: 5000, Remaining: 4999, ResetAt: now.Add(time.Hour)},
			REST:    provider.RateBudget{Limit: 5000, Remaining: 4999, ResetAt: now.Add(time.Hour)},
		},
	}
	sched := &Scheduler{Store: store, GitHub: gh, Sessions: alwaysLiveSessions(), Notify: &fakeNotifier{}}
	if err := sched.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}

	got1 := getReg(t, store, reg1.PRURL, reg1.SessionID)
	if got1.CIBucket != "success" {
		t.Errorf("reg1.CIBucket = %q, want %q (present in the batch response)", got1.CIBucket, "success")
	}
	got2 := getReg(t, store, reg2.PRURL, reg2.SessionID)
	wantAtLeast := now.Truncate(time.Second).Add(MinPollInterval)
	if got2.NextDueAt.Before(wantAtLeast) {
		t.Errorf("reg2.NextDueAt = %v, want at least %v (backed off like any other failed check, missing from the batch response)", got2.NextDueAt, wantAtLeast)
	}
}

// TestTickSkipsCheckWhenGitLabHostPoolAlreadyKnownExhausted mirrors the
// GitHub case for GitLab's per-host cache.
func TestTickSkipsCheckWhenGitLabHostPoolAlreadyKnownExhausted(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	now := time.Now()

	reg1 := registry.Registration{PRURL: "https://gitlab.corp.example/team/project/-/merge_requests/1", SessionID: "sess1", Provider: "gitlab", NextDueAt: now.Add(-2 * time.Second), CreatedAt: now}
	reg2 := registry.Registration{PRURL: "https://gitlab.corp.example/team/project/-/merge_requests/2", SessionID: "sess2", Provider: "gitlab", NextDueAt: now.Add(-time.Second), CreatedAt: now}
	for _, r := range []registry.Registration{reg1, reg2} {
		if err := store.Add(ctx, r); err != nil {
			t.Fatalf("Add() error = %v", err)
		}
	}

	gl := &fakeGitLabChecker{
		result: provider.CheckResult{State: "open", CIBucket: "success"},
		budget: provider.RateBudget{Limit: 2000, Remaining: 200, ResetAt: now.Add(10 * time.Minute)},
	}
	sched := &Scheduler{Store: store, GitLab: gl, Sessions: alwaysLiveSessions(), Notify: &fakeNotifier{}}
	if err := sched.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}

	if gl.calls != 1 {
		t.Errorf("GitLab checker calls = %d, want 1 (reg2 should skip its real check once reg1's result reveals the host pool is exhausted)", gl.calls)
	}
}

// getReg fetches the registration the test registered under sessionID,
// failing the test if it's gone.
func getReg(t *testing.T, store *registry.Store, prURL, sessionID string) registry.Registration {
	t.Helper()
	r, ok, err := store.Get(context.Background(), prURL, sessionID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if !ok {
		t.Fatalf("registration %q (session %q) is gone, want it still present", prURL, sessionID)
	}
	return r
}

// TestTickPersistsStateEvenWhenNotifyFails: notify is best-effort, not a
// gate on persistence. A session that can structurally never receive a
// message (e.g. an ACP-mode AoE session — `aoe send` always refuses those,
// there is no future retry that would ever succeed) must not freeze the
// registration's displayed state or its polling schedule forever just
// because Send keeps failing.
func TestTickPersistsStateEvenWhenNotifyFails(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	now := time.Now()
	reg := registry.Registration{
		PRURL: "https://github.com/o/r/pull/1", SessionID: "sess1", Provider: "github",
		ReviewCount: 1, CIBucket: "pending", UnresolvedThreadIDs: []string{"t1", "t2"},
		NextDueAt: now.Add(-time.Minute), CreatedAt: now,
	}
	if err := store.Add(ctx, reg); err != nil {
		t.Fatalf("Add() error = %v", err)
	}

	gh := &fakeGitHubChecker{result: provider.CheckResult{State: "open", ReviewCount: 1, CIBucket: "failure", UnresolvedThreadIDs: []string{"t1", "t2"}, Title: "Fix the thing"}}
	notify := &fakeNotifier{err: errors.New("Acp-mode sessions have no tmux pane; send is not supported")}
	sched := &Scheduler{Store: store, GitHub: gh, Sessions: alwaysLiveSessions(), Notify: notify}
	if err := sched.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}

	if len(notify.sent) != 1 {
		t.Fatalf("notify.sent = %v, want exactly one attempted send", notify.sent)
	}
	got := getReg(t, store, reg.PRURL, reg.SessionID)
	if got.CIBucket != "failure" || got.Title != "Fix the thing" {
		t.Errorf("stored state = {ci:%q title:%q}, want the fresh check result {failure %q} persisted regardless of the failed send", got.CIBucket, got.Title, "Fix the thing")
	}
	if !got.NextDueAt.After(reg.NextDueAt) {
		t.Errorf("next_due_at = %v, want it advanced past %v (a failed send must not hold the registration due-immediately forever)", got.NextDueAt, reg.NextDueAt)
	}
}

// TestTickPersistsHasConflict verifies a check result's HasConflict flag
// makes it all the way through processGitHubBatch/finish into the stored
// registration, and that a later check reporting the conflict resolved
// clears it back out again (not just a one-way latch like MergedAt/Status).
func TestTickPersistsHasConflict(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	now := time.Now()
	reg := registry.Registration{
		PRURL: "https://github.com/o/r/pull/1", SessionID: "sess1", Provider: "github",
		NextDueAt: now.Add(-time.Minute), CreatedAt: now,
	}
	if err := store.Add(ctx, reg); err != nil {
		t.Fatalf("Add() error = %v", err)
	}

	gh := &fakeGitHubChecker{result: provider.CheckResult{State: "open", HasConflict: true}}
	sched := &Scheduler{Store: store, GitHub: gh, Sessions: alwaysLiveSessions(), Notify: &fakeNotifier{}}
	if err := sched.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}
	got := getReg(t, store, reg.PRURL, reg.SessionID)
	if !got.HasConflict {
		t.Error("stored HasConflict = false, want true after a check reports a conflict")
	}

	if err := store.UpdateState(ctx, reg.PRURL, reg.SessionID, registry.StateUpdate{NextDueAt: now.Add(-time.Minute)}); err != nil {
		t.Fatalf("UpdateState() error = %v", err)
	}
	gh.result = provider.CheckResult{State: "open", HasConflict: false}
	if err := sched.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}
	got = getReg(t, store, reg.PRURL, reg.SessionID)
	if got.HasConflict {
		t.Error("stored HasConflict = true, want false once a later check reports the conflict resolved")
	}
}

// TestTickMarksMergedEvenWhenNotifyFails: same best-effort principle on the
// terminal path — a MERGED session that can never receive the notification
// must still move to StatusMerged rather than staying "watching" and
// re-attempting the undeliverable send on every tick forever.
func TestTickMarksMergedEvenWhenNotifyFails(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	now := time.Now()
	reg := registry.Registration{
		PRURL: "https://github.com/o/r/pull/1", SessionID: "sess1", Provider: "github",
		NextDueAt: now.Add(-time.Minute), CreatedAt: now,
	}
	if err := store.Add(ctx, reg); err != nil {
		t.Fatalf("Add() error = %v", err)
	}

	gh := &fakeGitHubChecker{result: provider.CheckResult{State: "merged"}}
	notify := &fakeNotifier{err: errors.New("Acp-mode sessions have no tmux pane; send is not supported")}
	sched := &Scheduler{Store: store, GitHub: gh, Sessions: alwaysLiveSessions(), Notify: notify}
	if err := sched.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}

	remaining, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(remaining) != 1 || remaining[0].Status != registry.StatusMerged {
		t.Fatalf("List() = %+v, want a single row at status %q even though the notification failed", remaining, registry.StatusMerged)
	}
}

// TestTickBacksOffOnCheckerError: a registration whose check keeps failing
// must not be retried on every 15s tick forever — push next_due_at out by at
// least the poll floor, leaving the last-seen state untouched.
func TestTickBacksOffOnCheckerError(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	now := time.Now()
	reg := registry.Registration{
		PRURL: "https://github.com/o/r/pull/1", SessionID: "sess1", Provider: "github",
		ReviewCount: 3, CIBucket: "success", UnresolvedThreadIDs: []string{"t1"},
		NextDueAt: now.Add(-time.Minute), CreatedAt: now,
	}
	if err := store.Add(ctx, reg); err != nil {
		t.Fatalf("Add() error = %v", err)
	}

	gh := &fakeGitHubChecker{err: errors.New("bad credentials")}
	sched := &Scheduler{Store: store, GitHub: gh, Sessions: alwaysLiveSessions(), Notify: &fakeNotifier{}}
	if err := sched.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}

	got := getReg(t, store, reg.PRURL, reg.SessionID)
	// next_due_at round-trips through the registry as whole Unix seconds, so
	// compare against a second-truncated baseline rather than the raw now.
	wantAtLeast := now.Truncate(time.Second).Add(MinPollInterval)
	if got.NextDueAt.Before(wantAtLeast) {
		t.Errorf("next_due_at = %v, want at least %v (one MinPollInterval out) after a checker error", got.NextDueAt, wantAtLeast)
	}
	if got.ReviewCount != 3 || got.CIBucket != "success" || !slices.Equal(got.UnresolvedThreadIDs, []string{"t1"}) {
		t.Errorf("stored state = {reviews:%d ci:%q unresolved:%v}, want the unchanged {3 success [t1]} (a failed check knows nothing new)", got.ReviewCount, got.CIBucket, got.UnresolvedThreadIDs)
	}
}

// alwaysLiveSessions/alwaysArchivedSessions/alwaysGoneSessions are minimal
// SessionClassifier fakes (see the SessionClassifier interface added to
// scheduler.go) so the scheduler's lifecycle branching can be tested
// without a real aoeclient.Client / HTTP server.
type fakeClassifier struct{ class aoeclient.SessionClass }

func (f fakeClassifier) RefreshIfStale(ctx context.Context) error  { return nil }
func (f fakeClassifier) Refresh(ctx context.Context) error         { return nil }
func (f fakeClassifier) Classify(id string) aoeclient.SessionClass { return f.class }

func alwaysLiveSessions() SessionClassifier     { return fakeClassifier{aoeclient.SessionLive} }
func alwaysArchivedSessions() SessionClassifier { return fakeClassifier{aoeclient.SessionArchived} }
func alwaysGoneSessions() SessionClassifier     { return fakeClassifier{aoeclient.SessionGone} }

// TestTickDedupesGitHubCheckForSharedURL guards the actual point of the
// registry's composite (pr_url, session_id) key: two sessions independently
// watching the same PR must still cost one real GitHub check, not two, with
// the shared result fanned out to each session's own row.
func TestTickDedupesGitHubCheckForSharedURL(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	now := time.Now()
	url := "https://github.com/o/r/pull/1"
	regA := registry.Registration{PRURL: url, SessionID: "sessA", Provider: "github", NextDueAt: now.Add(-time.Minute), CreatedAt: now}
	regB := registry.Registration{PRURL: url, SessionID: "sessB", Provider: "github", NextDueAt: now.Add(-time.Minute), CreatedAt: now}
	if err := store.Add(ctx, regA); err != nil {
		t.Fatalf("Add(regA) error = %v", err)
	}
	if err := store.Add(ctx, regB); err != nil {
		t.Fatalf("Add(regB) error = %v", err)
	}

	gh := &fakeGitHubChecker{result: provider.CheckResult{State: "open", CIBucket: "failure"}}
	notify := &fakeNotifier{}
	sched := &Scheduler{Store: store, GitHub: gh, Sessions: alwaysLiveSessions(), Notify: notify}

	if err := sched.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}

	if gh.calls != 1 {
		t.Errorf("CheckBatch calls = %d, want 1 (one batched call regardless of how many registrations)", gh.calls)
	}
	if len(gh.lastURLs) != 1 {
		t.Errorf("CheckBatch urls = %v, want exactly 1 distinct url, not one per registration", gh.lastURLs)
	}

	regs, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(regs) != 2 {
		t.Fatalf("List() len = %d, want 2 (both sessions' own rows updated)", len(regs))
	}
	for _, r := range regs {
		if r.CIBucket != "failure" {
			t.Errorf("List() row %+v, want CIBucket failure for both sessions", r)
		}
	}
	if len(notify.sent) != 2 {
		t.Errorf("notify.sent = %v, want a separate ping for each session watching this PR", notify.sent)
	}
}

// TestTickDedupesGitLabCheckForSharedURL is TestTickDedupesGitHubCheckForSharedURL's
// GitLab counterpart: GitLab has no batch API, so this exercises the
// same-tick result cache in processGitLabRegistration instead of a shared
// batch call.
func TestTickDedupesGitLabCheckForSharedURL(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	now := time.Now()
	url := "https://gitlab.corp.example/team/project/-/merge_requests/1"
	regA := registry.Registration{PRURL: url, SessionID: "sessA", Provider: "gitlab", NextDueAt: now.Add(-time.Minute), CreatedAt: now}
	regB := registry.Registration{PRURL: url, SessionID: "sessB", Provider: "gitlab", NextDueAt: now.Add(-time.Minute), CreatedAt: now}
	if err := store.Add(ctx, regA); err != nil {
		t.Fatalf("Add(regA) error = %v", err)
	}
	if err := store.Add(ctx, regB); err != nil {
		t.Fatalf("Add(regB) error = %v", err)
	}

	gl := &fakeGitLabChecker{
		result: provider.CheckResult{State: "open", CIBucket: "success"},
		budget: provider.RateBudget{Limit: 2000, Remaining: 1999, ResetAt: now.Add(10 * time.Minute)},
	}
	notify := &fakeNotifier{}
	sched := &Scheduler{Store: store, GitLab: gl, Sessions: alwaysLiveSessions(), Notify: notify}

	if err := sched.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}

	if gl.calls != 1 {
		t.Errorf("GitLab checker calls = %d, want 1 (one real check regardless of how many sessions share the MR)", gl.calls)
	}

	regs, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(regs) != 2 {
		t.Fatalf("List() len = %d, want 2 (both sessions' own rows updated)", len(regs))
	}
	for _, r := range regs {
		if r.CIBucket != "success" {
			t.Errorf("List() row %+v, want CIBucket success for both sessions", r)
		}
	}
}

func TestTickSuppressesDismissedReview(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	now := time.Now()
	reviewHref := "https://github.com/o/r/pull/1#pullrequestreview-1"
	reg := registry.Registration{
		PRURL: "https://github.com/o/r/pull/1", SessionID: "sess1", Provider: "github",
		ReviewCount: 1, NextDueAt: now.Add(-time.Minute), CreatedAt: now,
	}
	if err := store.Add(ctx, reg); err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	if err := store.SetDismissedReview(ctx, reg.PRURL, reg.SessionID, reviewHref); err != nil {
		t.Fatalf("SetDismissedReview() error = %v", err)
	}

	gh := &fakeGitHubChecker{result: provider.CheckResult{
		State: "open", ReviewCount: 1, LatestReviewState: "COMMENTED", LatestReviewer: "bob", ReviewHref: reviewHref,
	}}
	notify := &fakeNotifier{}
	sched := &Scheduler{Store: store, GitHub: gh, Sessions: alwaysLiveSessions(), Notify: notify}

	if err := sched.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}

	got, ok, err := store.Get(ctx, reg.PRURL, reg.SessionID)
	if err != nil || !ok {
		t.Fatalf("Get() = (%+v, %v, %v)", got, ok, err)
	}
	if got.LatestReviewState != "" || got.LatestReviewer != "" || got.ReviewHref != "" {
		t.Errorf("Get() = %+v, want the dismissed review still suppressed after a poll that reports the same href", got)
	}
	if len(notify.sent) != 0 {
		t.Errorf("notify.sent = %v, want no REVIEW ping for a review already seen and dismissed", notify.sent)
	}
}

func TestTickUnsuppressesOnNewReview(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	now := time.Now()
	oldHref := "https://github.com/o/r/pull/1#pullrequestreview-1"
	newHref := "https://github.com/o/r/pull/1#pullrequestreview-2"
	reg := registry.Registration{
		PRURL: "https://github.com/o/r/pull/1", SessionID: "sess1", Provider: "github",
		ReviewCount: 1, NextDueAt: now.Add(-time.Minute), CreatedAt: now,
	}
	if err := store.Add(ctx, reg); err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	if err := store.SetDismissedReview(ctx, reg.PRURL, reg.SessionID, oldHref); err != nil {
		t.Fatalf("SetDismissedReview() error = %v", err)
	}

	gh := &fakeGitHubChecker{result: provider.CheckResult{
		State: "open", ReviewCount: 2, LatestReviewState: "CHANGES_REQUESTED", LatestReviewer: "carol", ReviewHref: newHref,
	}}
	notify := &fakeNotifier{}
	sched := &Scheduler{Store: store, GitHub: gh, Sessions: alwaysLiveSessions(), Notify: notify}

	if err := sched.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}

	got, ok, err := store.Get(ctx, reg.PRURL, reg.SessionID)
	if err != nil || !ok {
		t.Fatalf("Get() = (%+v, %v, %v)", got, ok, err)
	}
	if got.LatestReviewState != "CHANGES_REQUESTED" || got.LatestReviewer != "carol" || got.ReviewHref != newHref {
		t.Errorf("Get() = %+v, want the new review to flow through despite an unrelated dismissed marker", got)
	}
}

// TestTickPingsAgainWhenFailureRunChangesButBucketDoesnt is scheduler.finish's
// end-to-end guard for the same race diff_test.go covers at the Diff level:
// a fix-push that fails again before the next Tick sees "failure" both
// times, so the ping must key off the run's CIHref changing, not just the
// bucket.
func TestTickPingsAgainWhenFailureRunChangesButBucketDoesnt(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	now := time.Now()
	reg := registry.Registration{
		PRURL: "https://github.com/o/r/pull/1", SessionID: "sess1", Provider: "github",
		CIBucket: "failure", CIHref: "https://github.com/o/r/actions/runs/1", NextDueAt: now.Add(-time.Minute), CreatedAt: now,
	}
	if err := store.Add(ctx, reg); err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	if err := store.UpdateState(ctx, reg.PRURL, reg.SessionID, registry.StateUpdate{
		CIBucket: "failure", CIHref: "https://github.com/o/r/actions/runs/1", NextDueAt: now.Add(-time.Minute),
	}); err != nil {
		t.Fatalf("UpdateState() error = %v", err)
	}

	gh := &fakeGitHubChecker{result: provider.CheckResult{State: "open", CIBucket: "failure", CIHref: "https://github.com/o/r/actions/runs/2"}}
	notify := &fakeNotifier{}
	sched := &Scheduler{Store: store, GitHub: gh, Sessions: alwaysLiveSessions(), Notify: notify}

	if err := sched.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}

	if len(notify.sent) != 1 || notify.sent[0] != "sess1: [pr-watch] o/r#1: CI failure (https://github.com/o/r/actions/runs/2)" {
		t.Errorf("notify.sent = %v, want a single ping for the new run despite an unchanged bucket", notify.sent)
	}
}

// TestTickPersistsCIWasFailingAndFiresRecoveredAfterPending walks the real
// sequence a fix push produces: failure, then a pending rerun, then success.
// Only the sticky flag carries "it was failing" across the pending poll.
func TestTickPersistsCIWasFailingAndFiresRecoveredAfterPending(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	now := time.Now()
	const prURL = "https://github.com/o/r/pull/1"
	if err := store.Add(ctx, registry.Registration{PRURL: prURL, SessionID: "sess1", Provider: "github", NextDueAt: now.Add(-time.Minute), CreatedAt: now}); err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	gh := &fakeGitHubChecker{}
	notify := &fakeNotifier{}
	sched := &Scheduler{Store: store, GitHub: gh, GitLab: &fakeGitLabChecker{}, Sessions: alwaysLiveSessions(), Notify: notify}

	step := func(bucket string) {
		t.Helper()
		gh.result = provider.CheckResult{State: "open", CIBucket: bucket}
		reg := getReg(t, store, prURL, "sess1")
		if err := store.UpdateState(ctx, prURL, "sess1", stateUpdateFromReg(reg, time.Now().Add(-time.Minute))); err != nil {
			t.Fatalf("UpdateState() error = %v", err)
		}
		if err := sched.Tick(ctx); err != nil {
			t.Fatalf("Tick() error = %v", err)
		}
	}

	step("failure")
	if got := getReg(t, store, prURL, "sess1"); !got.CIWasFailing {
		t.Fatal("CIWasFailing = false after a failing check, want true")
	}
	step("pending")
	if got := getReg(t, store, prURL, "sess1"); !got.CIWasFailing {
		t.Fatal("CIWasFailing = false after a pending check, want it held")
	}
	if len(notify.sent) != 1 {
		t.Fatalf("notify.sent = %v, want only the failure ping so far", notify.sent)
	}

	step("success")
	want := "sess1: [pr-watch] o/r#1: CI_RECOVERED CI is passing again"
	if len(notify.sent) != 2 || notify.sent[1] != want {
		t.Errorf("notify.sent = %v, want the recovery ping %q second", notify.sent, want)
	}
	if got := getReg(t, store, prURL, "sess1"); got.CIWasFailing {
		t.Error("CIWasFailing = true after success, want cleared")
	}

	step("success")
	if len(notify.sent) != 2 {
		t.Errorf("notify.sent = %v, want no repeat recovery ping", notify.sent)
	}
}

// A session watching several PRs/MRs gets one message per tick, not one per
// PR: separate pings queue up as separate turns in the agent (e.g. while a
// long command runs) and each has to be read and answered on its own.
func TestTickBatchesEventsOfOneSessionAcrossRegistrations(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	now := time.Now()

	mk := func(n int, session string) registry.Registration {
		return registry.Registration{
			PRURL: fmt.Sprintf("https://github.com/o/r/pull/%d", n), SessionID: session, Provider: "github",
			CIBucket: "pending", NextDueAt: now.Add(-time.Duration(10-n) * time.Second), CreatedAt: now,
		}
	}
	regs := []registry.Registration{mk(1, "sess1"), mk(2, "sess1"), mk(3, "sess2")}
	results := map[string]provider.CheckResult{}
	for _, r := range regs {
		if err := store.Add(ctx, r); err != nil {
			t.Fatalf("Add() error = %v", err)
		}
		results[r.PRURL] = provider.CheckResult{State: "open", CIBucket: "failure"}
	}

	gh := &fakeGitHubChecker{results: results, budgets: provider.GitHubBudgets{
		GraphQL: provider.RateBudget{Limit: 5000, Remaining: 4999, ResetAt: now.Add(time.Hour)},
		REST:    provider.RateBudget{Limit: 5000, Remaining: 4999, ResetAt: now.Add(time.Hour)},
	}}
	notify := &fakeNotifier{}
	sched := &Scheduler{Store: store, GitHub: gh, Sessions: alwaysLiveSessions(), Notify: notify}

	if err := sched.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}

	if len(notify.sent) != 2 {
		t.Fatalf("notify.sent = %q, want one message per session (2)", notify.sent)
	}
	bySession := map[string]string{}
	for _, s := range notify.sent {
		id, msg, _ := strings.Cut(s, ": ")
		bySession[id] = msg
	}
	want1 := "[pr-watch] 2 updates:\n\no/r#1: CI failure\n\no/r#2: CI failure"
	if bySession["sess1"] != want1 {
		t.Errorf("sess1 message = %q, want %q", bySession["sess1"], want1)
	}
	if want2 := "[pr-watch] o/r#3: CI failure"; bySession["sess2"] != want2 {
		t.Errorf("sess2 message = %q, want a single update unchanged: %q", bySession["sess2"], want2)
	}
}

// threadReplyFixture registers one PR whose thread t1 holds comment c1 and
// stores the given baseline, then runs a Tick in which GitHub reports the
// thread with the given comments.
func threadReplyFixture(t *testing.T, known bool, comments []provider.ThreadComment) (*fakeNotifier, *registry.Store, registry.Registration) {
	t.Helper()
	ctx := context.Background()
	store := openTestStore(t)
	now := time.Now()
	reg := registry.Registration{
		PRURL: "https://github.com/o/r/pull/1", SessionID: "sess1", Provider: "github",
		CIBucket: "none", UnresolvedThreadIDs: []string{"t1"}, NextDueAt: now.Add(-time.Minute), CreatedAt: now,
	}
	if err := store.Add(ctx, reg); err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	// Store the baseline the way a previous check would have.
	if err := store.UpdateState(ctx, reg.PRURL, reg.SessionID, registry.StateUpdate{
		CIBucket: "none", UnresolvedThreadIDs: []string{"t1"}, ThreadCommentIDs: []string{"c1"}, ThreadCommentsKnown: known, NextDueAt: now.Add(-time.Minute),
	}); err != nil {
		t.Fatalf("UpdateState() error = %v", err)
	}

	var ids []string
	for _, c := range comments {
		ids = append(ids, c.ID)
	}
	gh := &fakeGitHubChecker{selfLogin: "alice", result: provider.CheckResult{
		State: "open", CIBucket: "none", UnresolvedThreadIDs: []string{"t1"}, ThreadComments: comments, ThreadCommentIDs: ids,
	}}
	notify := &fakeNotifier{}
	sched := &Scheduler{Store: store, GitHub: gh, Sessions: alwaysLiveSessions(), Notify: notify}
	if err := sched.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}
	return notify, store, reg
}

func TestTickPingsOnReplyInExistingThread(t *testing.T) {
	notify, store, reg := threadReplyFixture(t, true, []provider.ThreadComment{
		{ThreadID: "t1", ID: "c1", Author: "alice"},
		{ThreadID: "t1", ID: "c2", Author: "scholzj", Href: "https://github.com/o/r/pull/1#discussion_r2"},
	})
	want := "sess1: [pr-watch] o/r#1: COMMENT 1 new thread reply, 1 unresolved total (https://github.com/o/r/pull/1#discussion_r2)"
	if len(notify.sent) != 1 || notify.sent[0] != want {
		t.Fatalf("notify.sent = %q, want [%q]", notify.sent, want)
	}
	if got := getReg(t, store, reg.PRURL, reg.SessionID); !slices.Equal(got.ThreadCommentIDs, []string{"c1", "c2"}) || !got.ThreadCommentsKnown {
		t.Errorf("stored baseline = %v known=%v, want [c1 c2] known so the reply pings only once", got.ThreadCommentIDs, got.ThreadCommentsKnown)
	}
}

// The session's own replies never ping it, but still count as seen.
func TestTickDoesNotPingOnOwnThreadReply(t *testing.T) {
	notify, store, reg := threadReplyFixture(t, true, []provider.ThreadComment{
		{ThreadID: "t1", ID: "c1", Author: "scholzj"},
		{ThreadID: "t1", ID: "c2", Author: "alice"},
	})
	if len(notify.sent) != 0 {
		t.Fatalf("notify.sent = %q, want nothing for the session's own reply", notify.sent)
	}
	if got := getReg(t, store, reg.PRURL, reg.SessionID); !slices.Equal(got.ThreadCommentIDs, []string{"c1", "c2"}) {
		t.Errorf("stored baseline = %v, want the own reply recorded as seen", got.ThreadCommentIDs)
	}
}

// A registration from before this tracking has no baseline: the first check
// stores one without reporting what was already there.
func TestTickSeedsMissingThreadBaselineSilently(t *testing.T) {
	notify, store, reg := threadReplyFixture(t, false, []provider.ThreadComment{
		{ThreadID: "t1", ID: "c1", Author: "scholzj"},
		{ThreadID: "t1", ID: "c2", Author: "scholzj"},
	})
	if len(notify.sent) != 0 {
		t.Fatalf("notify.sent = %q, want nothing while seeding the baseline", notify.sent)
	}
	if got := getReg(t, store, reg.PRURL, reg.SessionID); !got.ThreadCommentsKnown || !slices.Equal(got.ThreadCommentIDs, []string{"c1", "c2"}) {
		t.Errorf("stored baseline = %v known=%v, want [c1 c2] known", got.ThreadCommentIDs, got.ThreadCommentsKnown)
	}
}

// finishedFixture sets up a registration that already finished (merged or
// closed) with a stored baseline of one comment and one review, then runs a
// Tick in which GitHub reports result.
func finishedFixture(t *testing.T, status string, result provider.CheckResult) (*fakeNotifier, *registry.Store, registry.Registration) {
	t.Helper()
	ctx := context.Background()
	store := openTestStore(t)
	now := time.Now()
	reg := registry.Registration{PRURL: "https://github.com/o/r/pull/1", SessionID: "sess1", Provider: "github", NextDueAt: now.Add(-time.Minute), CreatedAt: now}
	if err := store.Add(ctx, reg); err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	if err := store.UpdateState(ctx, reg.PRURL, reg.SessionID, registry.StateUpdate{
		ReviewCount: 1, CIBucket: "success", TopLevelCommentIDs: []string{"c1"}, NextDueAt: now.Add(-time.Minute),
	}); err != nil {
		t.Fatalf("UpdateState() error = %v", err)
	}
	switch status {
	case registry.StatusMerged:
		if err := store.SetMerged(ctx, reg.PRURL, reg.SessionID, now.Add(-time.Hour)); err != nil {
			t.Fatal(err)
		}
	case registry.StatusClosed:
		if err := store.SetStatus(ctx, reg.PRURL, reg.SessionID, registry.StatusClosed); err != nil {
			t.Fatal(err)
		}
	}

	gh := &fakeGitHubChecker{selfLogin: "alice", result: result}
	notify := &fakeNotifier{}
	sched := &Scheduler{Store: store, GitHub: gh, Sessions: alwaysLiveSessions(), Notify: notify}
	if err := sched.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}
	return notify, store, reg
}

// A merged PR still gets late comments, tagged so the session knows it is
// finished, and the MERGED ping is not repeated on every poll.
func TestTickReportsLateCommentOnMergedPR(t *testing.T) {
	notify, store, reg := finishedFixture(t, registry.StatusMerged, provider.CheckResult{
		State: "merged", ReviewCount: 1, CIBucket: "success", TopLevelCommentIDs: []string{"c1", "c2"},
		LatestCommentHref: "https://github.com/o/r/pull/1#issuecomment-2", LatestCommentAuthor: "scholzj",
	})
	want := "sess1: [pr-watch] o/r#1 (merged): COMMENT 1 new comment(s), 2 total (https://github.com/o/r/pull/1#issuecomment-2)"
	if len(notify.sent) != 1 || notify.sent[0] != want {
		t.Fatalf("notify.sent = %q, want [%q]", notify.sent, want)
	}
	got := getReg(t, store, reg.PRURL, reg.SessionID)
	if got.Status != registry.StatusMerged {
		t.Errorf("Status = %q, want it to stay merged", got.Status)
	}
	if !got.NextDueAt.After(time.Now()) {
		t.Errorf("NextDueAt = %v, want it moved into the future so the PR keeps being polled at the normal cadence", got.NextDueAt)
	}
	if !slices.Equal(got.TopLevelCommentIDs, []string{"c1", "c2"}) {
		t.Errorf("TopLevelCommentIDs = %v, want the new baseline stored so the comment pings once", got.TopLevelCommentIDs)
	}
}

func TestTickStaysQuietOnFinishedPRWithNothingNew(t *testing.T) {
	for _, status := range []string{registry.StatusMerged, registry.StatusClosed} {
		state := "merged"
		if status == registry.StatusClosed {
			state = "closed"
		}
		notify, store, reg := finishedFixture(t, status, provider.CheckResult{
			State: state, ReviewCount: 1, CIBucket: "failure", TopLevelCommentIDs: []string{"c1"},
		})
		if len(notify.sent) != 0 {
			t.Errorf("%s: notify.sent = %q, want nothing (no repeated terminal ping, no CI ping)", status, notify.sent)
		}
		got := getReg(t, store, reg.PRURL, reg.SessionID)
		if got.Status != status || !got.NextDueAt.After(time.Now()) {
			t.Errorf("%s: got status %q next due %v, want it unchanged and rescheduled", status, got.Status, got.NextDueAt)
		}
	}
}

func TestTickReportsLateReviewOnClosedPR(t *testing.T) {
	notify, _, _ := finishedFixture(t, registry.StatusClosed, provider.CheckResult{
		State: "closed", ReviewCount: 2, CIBucket: "success", TopLevelCommentIDs: []string{"c1"},
		LatestReviewer: "scholzj", LatestReviewState: "COMMENTED", LatestReviewBody: "Late thoughts.",
	})
	want := "sess1: [pr-watch] o/r#1 (closed): REVIEW scholzj COMMENTED"
	if len(notify.sent) != 1 || notify.sent[0] != want {
		t.Fatalf("notify.sent = %q, want [%q]", notify.sent, want)
	}
}

// A closed PR that is open again is watched like any other from then on.
func TestTickReopensClosedPR(t *testing.T) {
	notify, store, reg := finishedFixture(t, registry.StatusClosed, provider.CheckResult{
		State: "open", ReviewCount: 1, CIBucket: "success", TopLevelCommentIDs: []string{"c1"},
	})
	want := "sess1: [pr-watch] o/r#1: REOPENED"
	if len(notify.sent) != 1 || notify.sent[0] != want {
		t.Fatalf("notify.sent = %q, want [%q]", notify.sent, want)
	}
	if got := getReg(t, store, reg.PRURL, reg.SessionID); got.Status != registry.StatusWatching {
		t.Errorf("Status = %q, want watching again", got.Status)
	}
}

// A merged PR of a session that is gone is no longer polled, but it must not
// be overwritten with "stopped": it is history, not an unregistered watch.
func TestTickLeavesFinishedRegistrationOfGoneSessionAlone(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	now := time.Now()
	reg := registry.Registration{PRURL: "https://github.com/o/r/pull/1", SessionID: "gone", Provider: "github", NextDueAt: now.Add(-time.Minute), CreatedAt: now}
	if err := store.Add(ctx, reg); err != nil {
		t.Fatal(err)
	}
	if err := store.SetMerged(ctx, reg.PRURL, reg.SessionID, now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	gh := &fakeGitHubChecker{result: provider.CheckResult{State: "merged"}}
	notify := &fakeNotifier{}
	sched := &Scheduler{Store: store, GitHub: gh, Sessions: alwaysGoneSessions(), Notify: notify}
	if err := sched.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}

	got := getReg(t, store, reg.PRURL, reg.SessionID)
	if got.Status != registry.StatusMerged {
		t.Errorf("Status = %q, want merged kept", got.Status)
	}
	if !got.NextDueAt.After(time.Now()) {
		t.Errorf("NextDueAt = %v, want it pushed out so the tick does not revisit it every time", got.NextDueAt)
	}
	if len(notify.sent) != 0 {
		t.Errorf("notify.sent = %q, want nothing", notify.sent)
	}
}

// Merged and closed PRs are only watched for late activity, so they are
// checked far less often than open ones; the normal cadence would spend the
// API budget on the ~100 finished PRs that are the least likely to change.
func TestTickPollsFinishedPRsLessOftenThanOpenOnes(t *testing.T) {
	_, store, reg := finishedFixture(t, registry.StatusMerged, provider.CheckResult{
		State: "merged", ReviewCount: 1, CIBucket: "success", TopLevelCommentIDs: []string{"c1"},
	})
	got := getReg(t, store, reg.PRURL, reg.SessionID)
	if min := time.Now().Add(FinishedPollInterval - time.Minute); got.NextDueAt.Before(min) {
		t.Errorf("NextDueAt = %v, want at least ~%s out for a finished PR", got.NextDueAt, FinishedPollInterval)
	}
}

// The pacing assumed one rate-limit point per PR, but a batched query costs
// several per PR; the scheduler learns the real cost from the response and
// paces for it.
func TestTickPacesGitHubPollingByTheRealQueryCost(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	now := time.Now()
	reg := registry.Registration{PRURL: "https://github.com/o/r/pull/1", SessionID: "sess1", Provider: "github", CIBucket: "none", NextDueAt: now.Add(-time.Minute), CreatedAt: now}
	if err := store.Add(ctx, reg); err != nil {
		t.Fatal(err)
	}
	budget := provider.RateBudget{Limit: 5000, Remaining: 2500, ResetAt: now.Add(time.Hour)}
	gh := &fakeGitHubChecker{
		result:  provider.CheckResult{State: "open", CIBucket: "none"},
		budgets: provider.GitHubBudgets{GraphQL: budget, REST: budget, GraphQLCost: 20},
	}
	sched := &Scheduler{Store: store, GitHub: gh, Sessions: alwaysLiveSessions(), Notify: &fakeNotifier{}}
	if err := sched.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}

	unweighted := ComputeInterval(budget, 1, now)
	weighted := ComputeInterval(budget, 20, now)
	if weighted <= unweighted {
		t.Fatalf("test setup: weighted %v is not above unweighted %v", weighted, unweighted)
	}
	got := getReg(t, store, reg.PRURL, reg.SessionID)
	if wait := got.NextDueAt.Sub(now); wait < weighted-2*time.Second {
		t.Errorf("next check in %v, want about %v: 20 points per PR must stretch the interval beyond the %v of a 1-point poll", wait, weighted, unweighted)
	}
}

// A pool that reported nothing (no open PR in the batch, so no REST call)
// has no say: it must not push the interval to the no-signal fallback.
func TestTickPacingIgnoresAPoolWithoutRateLimitData(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	now := time.Now()
	reg := registry.Registration{PRURL: "https://github.com/o/r/pull/1", SessionID: "sess1", Provider: "github", CIBucket: "none", NextDueAt: now.Add(-time.Minute), CreatedAt: now}
	if err := store.Add(ctx, reg); err != nil {
		t.Fatal(err)
	}
	budget := provider.RateBudget{Limit: 5000, Remaining: 2500, ResetAt: now.Add(time.Hour)}
	gh := &fakeGitHubChecker{
		result:  provider.CheckResult{State: "open", CIBucket: "none"},
		budgets: provider.GitHubBudgets{GraphQL: budget}, // REST left empty
	}
	sched := &Scheduler{Store: store, GitHub: gh, Sessions: alwaysLiveSessions(), Notify: &fakeNotifier{}}
	if err := sched.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}
	want := ComputeInterval(budget, 1, now)
	got := getReg(t, store, reg.PRURL, reg.SessionID)
	if wait := got.NextDueAt.Sub(now); wait > want+2*time.Second {
		t.Errorf("next check in %v, want about %v from the GraphQL pool alone, not the %v no-signal fallback", wait, want, FallbackInterval)
	}
}

// The poll's deadline can pass while the results of a big batch are still
// being processed. State must still be saved then: unsaved state means the
// same events are found and pinged again on every following poll.
func TestTickPersistsStateWhenThePollContextExpiresMidBatch(t *testing.T) {
	store := openTestStore(t)
	now := time.Now()
	reg := registry.Registration{PRURL: "https://github.com/o/r/pull/1", SessionID: "sess1", Provider: "github", CIBucket: "none", TopLevelCommentIDs: []string{"c1"}, NextDueAt: now.Add(-time.Minute), CreatedAt: now}
	if err := store.Add(context.Background(), reg); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateState(context.Background(), reg.PRURL, reg.SessionID, registry.StateUpdate{CIBucket: "none", TopLevelCommentIDs: []string{"c1"}, NextDueAt: now.Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	gh := &fakeGitHubChecker{
		result:     provider.CheckResult{State: "open", CIBucket: "none", TopLevelCommentIDs: []string{"c1", "c2"}, LatestCommentHref: "https://github.com/o/r/pull/1#issuecomment-2", LatestCommentAuthor: "bob"},
		afterCheck: cancel, // the deadline passes as the batch comes back
	}
	notify := &fakeNotifier{}
	sched := &Scheduler{Store: store, GitHub: gh, Sessions: alwaysLiveSessions(), Notify: notify}
	_ = sched.Tick(ctx)

	got := getReg(t, store, reg.PRURL, reg.SessionID)
	if !slices.Equal(got.TopLevelCommentIDs, []string{"c1", "c2"}) {
		t.Errorf("stored comment ids = %v, want [c1 c2]: state must be saved even though the poll context expired", got.TopLevelCommentIDs)
	}
	if !got.NextDueAt.After(time.Now()) {
		t.Errorf("NextDueAt = %v, want it moved into the future so the PR is not polled again at once", got.NextDueAt)
	}
	if len(notify.sent) != 1 {
		t.Errorf("pings = %q, want exactly one", notify.sent)
	}

	// The next poll finds nothing new.
	gh.afterCheck = nil
	if err := store.UpdateState(context.Background(), reg.PRURL, reg.SessionID, stateUpdateFromReg(getReg(t, store, reg.PRURL, reg.SessionID), time.Now().Add(-time.Minute))); err != nil {
		t.Fatal(err)
	}
	_ = sched.Tick(context.Background())
	if len(notify.sent) != 1 {
		t.Errorf("pings after the next poll = %q, want still just the first", notify.sent)
	}
}

// A PR that has just merged gets a fresh baseline saved, so the first poll as
// a finished PR reports only what happens after the merge, not whatever
// piled up since the last poll while it was still open.
func TestTickSavesABaselineWhenAPRMerges(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	now := time.Now()
	reg := registry.Registration{PRURL: "https://github.com/o/r/pull/1", SessionID: "sess1", Provider: "github", CIBucket: "none", NextDueAt: now.Add(-time.Minute), CreatedAt: now}
	if err := store.Add(ctx, reg); err != nil {
		t.Fatal(err)
	}
	gh := &fakeGitHubChecker{result: provider.CheckResult{State: "merged", MergedAt: now, CIBucket: "success", TopLevelCommentIDs: []string{"c1", "c2"}, ReviewCount: 1}}
	notify := &fakeNotifier{}
	sched := &Scheduler{Store: store, GitHub: gh, Sessions: alwaysLiveSessions(), Notify: notify}
	if err := sched.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if len(notify.sent) != 1 || !strings.Contains(notify.sent[0], "MERGED") {
		t.Fatalf("pings = %q, want just MERGED", notify.sent)
	}
	got := getReg(t, store, reg.PRURL, reg.SessionID)
	if got.Status != registry.StatusMerged || !slices.Equal(got.TopLevelCommentIDs, []string{"c1", "c2"}) || got.ReviewCount != 1 {
		t.Errorf("stored = %+v, want merged with the state seen at the merge as baseline", got)
	}
	if !got.NextDueAt.After(time.Now().Add(FinishedPollInterval - time.Minute)) {
		t.Errorf("NextDueAt = %v, want the finished-PR cadence from the start", got.NextDueAt)
	}

	if err := store.UpdateState(ctx, reg.PRURL, reg.SessionID, stateUpdateFromReg(getReg(t, store, reg.PRURL, reg.SessionID), time.Now().Add(-time.Minute))); err != nil {
		t.Fatal(err)
	}
	if err := sched.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if len(notify.sent) != 1 {
		t.Errorf("pings = %q after the first finished poll, want nothing more: nothing happened since the merge", notify.sent)
	}
}

func ciGreenTick(t *testing.T, filter []string, wasFailing bool) []string {
	t.Helper()
	ctx := context.Background()
	store := openTestStore(t)
	now := time.Now()
	reg := registry.Registration{PRURL: "https://github.com/o/r/pull/1", SessionID: "sess1", Provider: "github", CIBucket: "pending", CIWasFailing: wasFailing, EventFilter: filter, NextDueAt: now.Add(-time.Minute), CreatedAt: now}
	if err := store.Add(ctx, reg); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateState(ctx, reg.PRURL, reg.SessionID, registry.StateUpdate{CIBucket: "pending", CIWasFailing: wasFailing, NextDueAt: now.Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}
	notify := &fakeNotifier{}
	sched := &Scheduler{Store: store, GitHub: &fakeGitHubChecker{result: provider.CheckResult{State: "open", CIBucket: "success"}}, Sessions: alwaysLiveSessions(), Notify: notify}
	if err := sched.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	return notify.sent
}

// CI success is off unless asked for: a registration without an events
// filter (every kind) does not get it.
func TestCISuccessIsNotSentByDefault(t *testing.T) {
	if sent := ciGreenTick(t, nil, false); len(sent) != 0 {
		t.Errorf("sent = %q, want nothing: CI_SUCCESS is opt-in", sent)
	}
}

func TestCISuccessIsSentWhenRequested(t *testing.T) {
	sent := ciGreenTick(t, []string{"CI_SUCCESS"}, false)
	if len(sent) != 1 || sent[0] != "sess1: [pr-watch] o/r#1: CI_SUCCESS CI is passing" {
		t.Errorf("sent = %q, want the CI_SUCCESS ping", sent)
	}
}

func TestCISuccessIsSentAlongsideOtherRequestedKinds(t *testing.T) {
	if sent := ciGreenTick(t, []string{"REVIEW", "CI_SUCCESS"}, false); len(sent) != 1 {
		t.Errorf("sent = %q, want the CI_SUCCESS ping", sent)
	}
}

// After a failure both kinds fire; a session that asked for both is told once.
func TestCISuccessIsDroppedWhenCIRecoveredIsSentToo(t *testing.T) {
	sent := ciGreenTick(t, []string{"CI_RECOVERED", "CI_SUCCESS"}, true)
	if len(sent) != 1 || strings.Contains(sent[0], "CI_SUCCESS") || !strings.Contains(sent[0], "CI_RECOVERED") {
		t.Errorf("sent = %q, want only the CI_RECOVERED ping", sent)
	}
}

// With only CI_SUCCESS requested, a recovery still reports as success.
func TestCISuccessStillSentAfterARecoveryWhenCIRecoveredIsNotRequested(t *testing.T) {
	sent := ciGreenTick(t, []string{"CI_SUCCESS"}, true)
	if len(sent) != 1 || !strings.Contains(sent[0], "CI_SUCCESS") {
		t.Errorf("sent = %q, want the CI_SUCCESS ping", sent)
	}
}
