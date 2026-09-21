package scheduler

import (
	"context"
	"errors"
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
	err  error    // when non-nil, every Send fails with it
}

func (f *fakeNotifier) Send(ctx context.Context, sessionID, message string) error {
	f.sent = append(f.sent, sessionID+": "+message)
	return f.err
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
	if len(remaining) != 1 {
		t.Fatalf("List() len = %d, want 1 (registrations are never deleted, just marked terminal)", len(remaining))
	}
	if remaining[0].Status != registry.StatusMerged {
		t.Errorf("Status = %q, want %q after MERGED", remaining[0].Status, registry.StatusMerged)
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

// getReg fetches the single registration the test registered, failing the
// test if it's gone.
func getReg(t *testing.T, store *registry.Store, prURL string) registry.Registration {
	t.Helper()
	r, ok, err := store.Get(context.Background(), prURL)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if !ok {
		t.Fatalf("registration %q is gone, want it still present", prURL)
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
		ReviewCount: 1, CIBucket: "pending", Unresolved: 2,
		NextDueAt: now.Add(-time.Minute), CreatedAt: now,
	}
	if err := store.Add(ctx, reg); err != nil {
		t.Fatalf("Add() error = %v", err)
	}

	gh := &fakeGitHubChecker{result: provider.CheckResult{State: "open", ReviewCount: 1, CIBucket: "failure", Unresolved: 2, Title: "Fix the thing"}}
	notify := &fakeNotifier{err: errors.New("Acp-mode sessions have no tmux pane; send is not supported")}
	sched := &Scheduler{Store: store, GitHub: gh, Sessions: alwaysLiveSessions(), Notify: notify}
	if err := sched.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}

	if len(notify.sent) != 1 {
		t.Fatalf("notify.sent = %v, want exactly one attempted send", notify.sent)
	}
	got := getReg(t, store, reg.PRURL)
	if got.CIBucket != "failure" || got.Title != "Fix the thing" {
		t.Errorf("stored state = {ci:%q title:%q}, want the fresh check result {failure %q} persisted regardless of the failed send", got.CIBucket, got.Title, "Fix the thing")
	}
	if !got.NextDueAt.After(reg.NextDueAt) {
		t.Errorf("next_due_at = %v, want it advanced past %v (a failed send must not hold the registration due-immediately forever)", got.NextDueAt, reg.NextDueAt)
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
		ReviewCount: 3, CIBucket: "success", Unresolved: 1,
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

	got := getReg(t, store, reg.PRURL)
	// next_due_at round-trips through the registry as whole Unix seconds, so
	// compare against a second-truncated baseline rather than the raw now.
	wantAtLeast := now.Truncate(time.Second).Add(MinPollInterval)
	if got.NextDueAt.Before(wantAtLeast) {
		t.Errorf("next_due_at = %v, want at least %v (one MinPollInterval out) after a checker error", got.NextDueAt, wantAtLeast)
	}
	if got.ReviewCount != 3 || got.CIBucket != "success" || got.Unresolved != 1 {
		t.Errorf("stored state = {reviews:%d ci:%q unresolved:%d}, want the unchanged {3 success 1} (a failed check knows nothing new)", got.ReviewCount, got.CIBucket, got.Unresolved)
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
