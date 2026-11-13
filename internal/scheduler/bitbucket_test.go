package scheduler

import (
	"context"
	"testing"
	"time"

	"github.com/cwrau/aoe-plugin-pr-watch-mcp/internal/aoeclient"
	"github.com/cwrau/aoe-plugin-pr-watch-mcp/internal/provider"
	"github.com/cwrau/aoe-plugin-pr-watch-mcp/internal/registry"
)

type fakeBitbucketChecker struct {
	result provider.CheckResult
	err    error
	calls  int
	self   string

	discoverURL   string
	discoverFound bool
	discoverCalls int
	discoverArgs  []string // workspace, repo, branch of the last call
}

func (f *fakeBitbucketChecker) Check(ctx context.Context, url string) (provider.CheckResult, provider.RateBudget, error) {
	f.calls++
	return f.result, provider.RateBudget{}, f.err
}

func (f *fakeBitbucketChecker) DiscoverBranchPR(ctx context.Context, workspace, repo, branch string) (string, bool, provider.RateBudget, error) {
	f.discoverCalls++
	f.discoverArgs = []string{workspace, repo, branch}
	return f.discoverURL, f.discoverFound, provider.RateBudget{}, nil
}

func (f *fakeBitbucketChecker) SelfAccount(ctx context.Context) (string, error) { return f.self, nil }

const bbPRURL = "https://bitbucket.org/acme/widgets/pull-requests/12"

func bitbucketReg(now time.Time) registry.Registration {
	return registry.Registration{
		PRURL: bbPRURL, SessionID: "sess1", Provider: "bitbucket",
		CIBucket: "none", NextDueAt: now.Add(-time.Minute), CreatedAt: now,
	}
}

func TestTickPollsBitbucketAndPingsWithItsShortRef(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	if err := store.Add(ctx, bitbucketReg(time.Now())); err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	bb := &fakeBitbucketChecker{result: provider.CheckResult{State: "open", CIBucket: "failure", CIDetail: "build", CIHref: "https://ci.example/b"}}
	notify := &fakeNotifier{}
	sched := &Scheduler{Store: store, GitHub: &fakeGitHubChecker{}, GitLab: &fakeGitLabChecker{}, Bitbucket: bb, Sessions: alwaysLiveSessions(), Notify: notify}

	if err := sched.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}
	if bb.calls != 1 {
		t.Errorf("Check calls = %d, want 1", bb.calls)
	}
	if len(notify.sent) != 1 || notify.sent[0] == "" {
		t.Fatalf("notify.sent = %v, want one CI ping", notify.sent)
	}
	if want := "sess1: [pr-watch] acme/widgets#12: CI failure"; len(notify.sent[0]) < len(want) || notify.sent[0][:len(want)] != want {
		t.Errorf("ping = %q, want it to start with %q", notify.sent[0], want)
	}
	if got := getReg(t, store, bbPRURL, "sess1"); got.CIBucket != "failure" {
		t.Errorf("persisted CIBucket = %q, want failure", got.CIBucket)
	}
}

func TestTickChecksASharedBitbucketPROnce(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	now := time.Now()
	for _, session := range []string{"sess1", "sess2"} {
		reg := bitbucketReg(now)
		reg.SessionID = session
		if err := store.Add(ctx, reg); err != nil {
			t.Fatalf("Add() error = %v", err)
		}
	}
	bb := &fakeBitbucketChecker{result: provider.CheckResult{State: "open", CIBucket: "none"}}
	sched := &Scheduler{Store: store, GitHub: &fakeGitHubChecker{}, GitLab: &fakeGitLabChecker{}, Bitbucket: bb, Sessions: alwaysLiveSessions(), Notify: &fakeNotifier{}}

	if err := sched.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}
	if bb.calls != 1 {
		t.Errorf("Check calls = %d for two sessions on one PR, want 1", bb.calls)
	}
}

func TestTickSuppressesTheBitbucketAccountsOwnReview(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	if err := store.Add(ctx, bitbucketReg(time.Now())); err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	bb := &fakeBitbucketChecker{self: "me", result: provider.CheckResult{
		State: "open", CIBucket: "none", ReviewCount: 1, LatestReviewState: "APPROVED", LatestReviewer: "me", LatestReviewAt: time.Now(),
	}}
	notify := &fakeNotifier{}
	sched := &Scheduler{Store: store, GitHub: &fakeGitHubChecker{}, GitLab: &fakeGitLabChecker{}, Bitbucket: bb, Sessions: alwaysLiveSessions(), Notify: notify}

	if err := sched.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}
	if len(notify.sent) != 0 {
		t.Errorf("notify.sent = %v, want none for the worker account's own review", notify.sent)
	}
}

// A Bitbucket registration while no Bitbucket checker is wired (the plugin
// without a token) backs off like any failed check, never panics.
func TestTickWithoutABitbucketCheckerBacksOffInsteadOfPanicking(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	if err := store.Add(ctx, bitbucketReg(time.Now())); err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	sched := &Scheduler{Store: store, GitHub: &fakeGitHubChecker{}, GitLab: &fakeGitLabChecker{}, Sessions: alwaysLiveSessions(), Notify: &fakeNotifier{}}
	if err := sched.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}
	if got := getReg(t, store, bbPRURL, "sess1"); !got.NextDueAt.After(time.Now()) {
		t.Errorf("NextDueAt = %v, want it pushed into the future", got.NextDueAt)
	}
}

func TestDiscoverTickFindsOpenBitbucketPR(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	workspaces := &fakeWorkspaceSource{workspaces: map[string][]aoeclient.WorkspaceRepo{
		"sess1": {{SourcePath: "/repo", Branch: "feature-x"}},
	}}
	remotes := &fakeRemoteResolver{resolutions: map[string]resolution{
		"/repo": {provider: "bitbucket", remoteKey: "acme/widgets", branch: "feature-x"},
	}}
	bb := &fakeBitbucketChecker{discoverFound: true, discoverURL: bbPRURL}
	sched := &Scheduler{Store: store, GitHub: &fakeGitHubChecker{}, GitLab: &fakeGitLabChecker{}, Bitbucket: bb, Sessions: alwaysLiveSessions(), Notify: &fakeNotifier{}, Workspaces: workspaces, Remotes: remotes}

	if err := sched.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}
	if got := bb.discoverArgs; len(got) != 3 || got[0] != "acme" || got[1] != "widgets" || got[2] != "feature-x" {
		t.Errorf("DiscoverBranchPR args = %v, want [acme widgets feature-x]", got)
	}
	reg := getReg(t, store, bbPRURL, "sess1")
	if reg.Provider != "bitbucket" {
		t.Errorf("registered provider = %q, want bitbucket", reg.Provider)
	}
	if silent, err := store.FirstPollSilent(ctx, bbPRURL, "sess1"); err != nil || !silent {
		t.Errorf("FirstPollSilent() = (%v, %v), want true", silent, err)
	}
}
