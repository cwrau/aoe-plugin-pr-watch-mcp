package scheduler

import (
	"context"
	"errors"
	"slices"
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
	results map[string]provider.CheckResult
	budgets provider.GitHubBudgets
	err     error
	calls   int // counts CheckBatch invocations, not urls checked
}

func (f *fakeGitHubChecker) CheckBatch(ctx context.Context, urls []string) (map[string]provider.CheckResult, provider.GitHubBudgets, error) {
	f.calls++
	if f.err != nil {
		return nil, provider.GitHubBudgets{}, f.err
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
	t.Cleanup(func() { _ = s.Close() })
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

	got2 := getReg(t, store, reg2.PRURL)
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
	got1 := getReg(t, store, reg1.PRURL)
	got2 := getReg(t, store, reg2.PRURL)
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

	got1 := getReg(t, store, reg1.PRURL)
	if got1.CIBucket != "success" {
		t.Errorf("reg1.CIBucket = %q, want %q (present in the batch response)", got1.CIBucket, "success")
	}
	got2 := getReg(t, store, reg2.PRURL)
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
	got := getReg(t, store, reg.PRURL)
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
	got := getReg(t, store, reg.PRURL)
	if !got.HasConflict {
		t.Error("stored HasConflict = false, want true after a check reports a conflict")
	}

	if err := store.UpdateState(ctx, reg.PRURL, registry.StateUpdate{NextDueAt: now.Add(-time.Minute)}); err != nil {
		t.Fatalf("UpdateState() error = %v", err)
	}
	gh.result = provider.CheckResult{State: "open", HasConflict: false}
	if err := sched.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}
	got = getReg(t, store, reg.PRURL)
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

	got := getReg(t, store, reg.PRURL)
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

func (f fakeClassifier) RefreshSessionSets(ctx context.Context) error { return nil }
func (f fakeClassifier) Classify(id string) aoeclient.SessionClass    { return f.class }

func alwaysLiveSessions() SessionClassifier     { return fakeClassifier{aoeclient.SessionLive} }
func alwaysArchivedSessions() SessionClassifier { return fakeClassifier{aoeclient.SessionArchived} }
func alwaysGoneSessions() SessionClassifier     { return fakeClassifier{aoeclient.SessionGone} }
