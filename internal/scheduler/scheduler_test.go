package scheduler

import (
	"context"
	"testing"
	"time"

	"github.com/cwrau/aoe-pr-watch-mcp/internal/aoeclient"
	"github.com/cwrau/aoe-pr-watch-mcp/internal/provider"
	"github.com/cwrau/aoe-pr-watch-mcp/internal/registry"
)

type fakeGitHubChecker struct {
	result  provider.CheckResult
	budgets provider.GitHubBudgets
	err     error
	calls   int
}

func (f *fakeGitHubChecker) Check(ctx context.Context, url string) (provider.CheckResult, provider.GitHubBudgets, error) {
	f.calls++
	return f.result, f.budgets, f.err
}

type fakeGitLabChecker struct {
	result provider.CheckResult
	budget provider.RateBudget
	err    error
	calls  int
}

func (f *fakeGitLabChecker) Check(ctx context.Context, url string) (provider.CheckResult, provider.RateBudget, error) {
	f.calls++
	return f.result, f.budget, f.err
}

type fakeNotifier struct {
	sent []string // "sessionID: message"
}

func (f *fakeNotifier) Send(ctx context.Context, sessionID, message string) error {
	f.sent = append(f.sent, sessionID+": "+message)
	return nil
}

func openTestStore(t *testing.T) *registry.Store {
	t.Helper()
	s, err := registry.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("registry.Open() error = %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
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
	if len(notify.sent) != 1 || notify.sent[0] != "sess1: CI failure" {
		t.Errorf("notify.sent = %v, want a single CI-failure message to sess1", notify.sent)
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

func TestTickDeregistersOnMerged(t *testing.T) {
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
	notify := &fakeNotifier{}
	sched := &Scheduler{Store: store, GitHub: gh, Sessions: alwaysLiveSessions(), Notify: notify}
	if err := sched.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}

	remaining, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(remaining) != 0 {
		t.Errorf("List() len = %d, want 0 after MERGED", len(remaining))
	}
	if len(notify.sent) != 1 || notify.sent[0] != "sess1: MERGED" {
		t.Errorf("notify.sent = %v, want a single MERGED message", notify.sent)
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

func TestTickDropsRegistrationForGoneSession(t *testing.T) {
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
	if len(remaining) != 0 {
		t.Errorf("List() len = %d, want 0 (gone session's registration must be dropped)", len(remaining))
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

	// Remaining is set low enough (relative to Limit) that the per-
	// registration interval clears the MinPollInterval floor for both
	// activeCount=1 and activeCount=2, so the two are actually
	// distinguishable — at Remaining:4500/Limit:5000 (used elsewhere in this
	// package) both clamp to the same 30s floor regardless of activeCount,
	// which would make this test pass or fail independent of whether Tick
	// uses the real count at all.
	gh := &fakeGitHubChecker{
		result: provider.CheckResult{State: "open", CIBucket: "success"},
		budgets: provider.GitHubBudgets{
			GraphQL: provider.RateBudget{Limit: 5000, Remaining: 550, ResetAt: now.Add(time.Hour)},
			REST:    provider.RateBudget{Limit: 5000, Remaining: 550, ResetAt: now.Add(time.Hour)},
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

// alwaysLiveSessions/alwaysArchivedSessions/alwaysGoneSessions are minimal
// SessionClassifier fakes (see the SessionClassifier interface added to
// scheduler.go) so the scheduler's lifecycle branching can be tested
// without a real aoeclient.Client / HTTP server.
type fakeClassifier struct{ class aoeclient.SessionClass }

func (f fakeClassifier) RefreshSessionSets(ctx context.Context) error { return nil }
func (f fakeClassifier) Classify(id string) aoeclient.SessionClass    { return f.class }

func alwaysLiveSessions() SessionClassifier     { return fakeClassifier{aoeclient.SessionLive} }
func alwaysArchivedSessions() SessionClassifier { return fakeClassifier{aoeclient.SessionArchived} }
func alwaysGoneSessions() SessionClassifier     { return fakeClassifier{aoeclient.SessionGone} }
