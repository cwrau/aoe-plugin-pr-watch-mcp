package scheduler

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/cwrau/aoe-plugin-pr-watch-mcp/internal/aoeclient"
	"github.com/cwrau/aoe-plugin-pr-watch-mcp/internal/provider"
	"github.com/cwrau/aoe-plugin-pr-watch-mcp/internal/registry"
)

type fakeWorkspaceSource struct {
	workspaces map[string][]aoeclient.WorkspaceRepo
}

func (f *fakeWorkspaceSource) LiveWorkspaces() map[string][]aoeclient.WorkspaceRepo {
	return f.workspaces
}

type resolution struct {
	provider  string
	remoteKey string
	branch    string // the resolver's own live-read branch, independent of whatever WorkspaceRepo.Branch says
}

type fakeRemoteResolver struct {
	resolutions map[string]resolution // keyed by source_path
	extras      map[string]extraRemotes
	calls       int
}

func (f *fakeRemoteResolver) Resolve(ctx context.Context, sourcePath string) (string, string, string, bool) {
	f.calls++
	r, ok := f.resolutions[sourcePath]
	if !ok {
		return "", "", "", false
	}
	return r.provider, r.remoteKey, r.branch, true
}

// TestDiscoverTickUsesResolverBranchNotWorkspaceSourceBranch is a
// regression test: confirmed live
// that aoe's own reported branch for a checkout can go stale (a worktree
// created as "japanese-ii" whose branch was later renamed, but aoe's cache
// still said "japanese-ii"), so discoverTick must use RemoteResolver's own
// live-read branch, never WorkspaceRepo.Branch.
func TestDiscoverTickUsesResolverBranchNotWorkspaceSourceBranch(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)

	workspaces := &fakeWorkspaceSource{workspaces: map[string][]aoeclient.WorkspaceRepo{
		"sess1": {{SourcePath: "/repo", Branch: "stale-cached-name"}},
	}}
	remotes := &fakeRemoteResolver{resolutions: map[string]resolution{
		"/repo": {provider: "github", remoteKey: "o/r", branch: "actual-live-branch"},
	}}
	gh := &fakeGitHubChecker{discovered: map[string]string{
		provider.GitHubBranchKey("o", "r", "actual-live-branch"): "https://github.com/o/r/pull/9",
	}}
	sched := &Scheduler{Store: store, GitHub: gh, GitLab: &fakeGitLabChecker{}, Sessions: alwaysLiveSessions(), Notify: &fakeNotifier{}, Workspaces: workspaces, Remotes: remotes}

	if err := sched.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}

	getReg(t, store, "https://github.com/o/r/pull/9", "sess1")
	if len(gh.lastDiscoverItems) != 1 || gh.lastDiscoverItems[0].Branch != "actual-live-branch" {
		t.Errorf("lastDiscoverItems = %+v, want the resolver's branch (actual-live-branch), not WorkspaceRepo.Branch (stale-cached-name)", gh.lastDiscoverItems)
	}
}

func TestDiscoverTickRegistersDiscoveredGitHubPR(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)

	workspaces := &fakeWorkspaceSource{workspaces: map[string][]aoeclient.WorkspaceRepo{
		"sess1": {{SourcePath: "/repo", Branch: "feature-x"}},
	}}
	remotes := &fakeRemoteResolver{resolutions: map[string]resolution{
		"/repo": {provider: "github", remoteKey: "o/r", branch: "feature-x"},
	}}
	gh := &fakeGitHubChecker{discovered: map[string]string{
		provider.GitHubBranchKey("o", "r", "feature-x"): "https://github.com/o/r/pull/9",
	}}
	sched := &Scheduler{Store: store, GitHub: gh, GitLab: &fakeGitLabChecker{}, Sessions: alwaysLiveSessions(), Notify: &fakeNotifier{}, Workspaces: workspaces, Remotes: remotes}

	if err := sched.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}

	reg := getReg(t, store, "https://github.com/o/r/pull/9", "sess1")
	if reg.Status != registry.StatusWatching {
		t.Errorf("auto-discovered registration Status = %q, want %q", reg.Status, registry.StatusWatching)
	}
	if len(reg.EventFilter) != 0 {
		t.Errorf("auto-discovered registration EventFilter = %v, want none (every event kind, same as a plain register call)", reg.EventFilter)
	}
}

func TestDiscoverTickNeverResurrectsAnUnregisteredPR(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	now := time.Now()

	// The agent registered, then explicitly unregistered, this exact PR.
	if err := store.Add(ctx, registry.Registration{PRURL: "https://github.com/o/r/pull/9", SessionID: "sess1", Provider: "github", NextDueAt: now, CreatedAt: now}); err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	if _, err := store.SetStatusOwnedBy(ctx, "https://github.com/o/r/pull/9", "sess1", registry.StatusStopped); err != nil {
		t.Fatalf("SetStatusOwnedBy() error = %v", err)
	}

	workspaces := &fakeWorkspaceSource{workspaces: map[string][]aoeclient.WorkspaceRepo{
		"sess1": {{SourcePath: "/repo", Branch: "feature-x"}},
	}}
	remotes := &fakeRemoteResolver{resolutions: map[string]resolution{
		"/repo": {provider: "github", remoteKey: "o/r", branch: "feature-x"},
	}}
	gh := &fakeGitHubChecker{discovered: map[string]string{
		provider.GitHubBranchKey("o", "r", "feature-x"): "https://github.com/o/r/pull/9",
	}}
	sched := &Scheduler{Store: store, GitHub: gh, GitLab: &fakeGitLabChecker{}, Sessions: alwaysLiveSessions(), Notify: &fakeNotifier{}, Workspaces: workspaces, Remotes: remotes}

	if err := sched.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}

	reg := getReg(t, store, "https://github.com/o/r/pull/9", "sess1")
	if reg.Status != registry.StatusStopped {
		t.Errorf("Status = %q after a discovery pass found the same branch again, want it to stay %q (unregister must stick)", reg.Status, registry.StatusStopped)
	}

	// The target must still be marked resolved, so it doesn't keep spending
	// a real call every tick rediscovering the exact same already-known
	// answer forever.
	unresolved, err := store.UnresolvedAutoDiscoveryForSessions(ctx, []string{"sess1"})
	if err != nil {
		t.Fatalf("UnresolvedAutoDiscoveryForSessions() error = %v", err)
	}
	if len(unresolved) != 0 {
		t.Errorf("UnresolvedAutoDiscoveryForSessions() = %+v, want none (resolved, even though nothing was (re-)registered)", unresolved)
	}
}

func TestDiscoverTickFindsOpenGitLabMR(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)

	workspaces := &fakeWorkspaceSource{workspaces: map[string][]aoeclient.WorkspaceRepo{
		"sess1": {{SourcePath: "/repo", Branch: "feature-x"}},
	}}
	remotes := &fakeRemoteResolver{resolutions: map[string]resolution{
		"/repo": {provider: "gitlab", remoteKey: "gitlab.corp.example/team/project", branch: "feature-x"},
	}}
	gl := &fakeGitLabChecker{discoverFound: true, discoverMRURL: "https://gitlab.corp.example/team/project/-/merge_requests/9"}
	sched := &Scheduler{Store: store, GitHub: &fakeGitHubChecker{}, GitLab: gl, Sessions: alwaysLiveSessions(), Notify: &fakeNotifier{}, Workspaces: workspaces, Remotes: remotes}

	if err := sched.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}

	getReg(t, store, "https://gitlab.corp.example/team/project/-/merge_requests/9", "sess1")
	if silent, err := store.FirstPollSilent(ctx, "https://gitlab.corp.example/team/project/-/merge_requests/9", "sess1"); err != nil || !silent {
		t.Errorf("FirstPollSilent() = (%v, %v), want true: a discovered PR's existing activity is not pinged", silent, err)
	}
}

func TestDiscoverTickKeepsProbingWhenNoOpenPRYet(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)

	workspaces := &fakeWorkspaceSource{workspaces: map[string][]aoeclient.WorkspaceRepo{
		"sess1": {{SourcePath: "/repo", Branch: "feature-x"}},
	}}
	remotes := &fakeRemoteResolver{resolutions: map[string]resolution{
		"/repo": {provider: "github", remoteKey: "o/r", branch: "feature-x"},
	}}
	gh := &fakeGitHubChecker{} // discovered stays nil: no open PR found
	sched := &Scheduler{Store: store, GitHub: gh, GitLab: &fakeGitLabChecker{}, Sessions: alwaysLiveSessions(), Notify: &fakeNotifier{}, Workspaces: workspaces, Remotes: remotes}

	if err := sched.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}

	// Not resolved (no PR found), so the next pass asks again, unlike a
	// resolved target.
	unresolved, err := store.UnresolvedAutoDiscoveryForSessions(ctx, []string{"sess1"})
	if err != nil {
		t.Fatalf("UnresolvedAutoDiscoveryForSessions() error = %v", err)
	}
	if len(unresolved) != 1 {
		t.Errorf("UnresolvedAutoDiscoveryForSessions() = %+v, want the target still pending", unresolved)
	}
	if err := sched.Tick(ctx); err != nil {
		t.Fatalf("Tick() (second) error = %v", err)
	}
	if gh.discoverCalls != 2 {
		t.Errorf("discoverCalls = %d after two passes, want 2 (an unresolved target is probed every pass)", gh.discoverCalls)
	}
}

func TestDiscoverTickPrunesRepoRemovedFromWorkspace(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)

	remotes := &fakeRemoteResolver{resolutions: map[string]resolution{
		"/repo-a": {provider: "github", remoteKey: "o/a", branch: "main"},
		"/repo-b": {provider: "github", remoteKey: "o/b", branch: "main"},
	}}
	sched := &Scheduler{
		Store: store, GitHub: &fakeGitHubChecker{}, GitLab: &fakeGitLabChecker{}, Sessions: alwaysLiveSessions(), Notify: &fakeNotifier{},
		Workspaces: &fakeWorkspaceSource{workspaces: map[string][]aoeclient.WorkspaceRepo{
			"sess1": {{SourcePath: "/repo-a", Branch: "main"}, {SourcePath: "/repo-b", Branch: "main"}},
		}},
		Remotes: remotes,
	}
	if err := sched.Tick(ctx); err != nil {
		t.Fatalf("Tick() (first) error = %v", err)
	}

	// repo-b leaves the workspace on the next tick.
	sched.Workspaces = &fakeWorkspaceSource{workspaces: map[string][]aoeclient.WorkspaceRepo{
		"sess1": {{SourcePath: "/repo-a", Branch: "main"}},
	}}
	if err := sched.Tick(ctx); err != nil {
		t.Fatalf("Tick() (second) error = %v", err)
	}

	left, err := store.UnresolvedAutoDiscoveryForSessions(ctx, []string{"sess1"})
	if err != nil {
		t.Fatalf("UnresolvedAutoDiscoveryForSessions() error = %v", err)
	}
	if len(left) != 1 || left[0].SourcePath != "/repo-a" {
		t.Fatalf("UnresolvedAutoDiscoveryForSessions() = %+v, want only /repo-a left after /repo-b left the workspace", left)
	}
}

func TestDiscoverTickPrunesGoneSession(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)

	remotes := &fakeRemoteResolver{resolutions: map[string]resolution{
		"/repo": {provider: "github", remoteKey: "o/r", branch: "main"},
	}}
	sched := &Scheduler{
		Store: store, GitHub: &fakeGitHubChecker{}, GitLab: &fakeGitLabChecker{}, Sessions: alwaysLiveSessions(), Notify: &fakeNotifier{},
		Workspaces: &fakeWorkspaceSource{workspaces: map[string][]aoeclient.WorkspaceRepo{
			"sess1": {{SourcePath: "/repo", Branch: "main"}},
		}},
		Remotes: remotes,
	}
	if err := sched.Tick(ctx); err != nil {
		t.Fatalf("Tick() (first) error = %v", err)
	}

	// sess1's workspace no longer appears at all (session gone/archived).
	sched.Workspaces = &fakeWorkspaceSource{workspaces: map[string][]aoeclient.WorkspaceRepo{}}
	if err := sched.Tick(ctx); err != nil {
		t.Fatalf("Tick() (second) error = %v", err)
	}

	sessions, err := store.DistinctAutoDiscoverySessionIDs(ctx)
	if err != nil {
		t.Fatalf("DistinctAutoDiscoverySessionIDs() error = %v", err)
	}
	if len(sessions) != 0 {
		t.Errorf("DistinctAutoDiscoverySessionIDs() = %v, want none (sess1 no longer live)", sessions)
	}
}

func TestTickSkipsDiscoveryWhenWorkspacesOrRemotesUnset(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	// Neither Workspaces nor Remotes set — must not panic (nil-dereference
	// on s.Workspaces.LiveWorkspaces()) and must simply skip discovery.
	sched := &Scheduler{Store: store, GitHub: &fakeGitHubChecker{}, GitLab: &fakeGitLabChecker{}, Sessions: alwaysLiveSessions(), Notify: &fakeNotifier{}}
	if err := sched.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}
}

func discoveryFixture(t *testing.T) (*Scheduler, *fakeGitHubChecker, *registry.Store) {
	t.Helper()
	store := openTestStore(t)
	workspaces := &fakeWorkspaceSource{workspaces: map[string][]aoeclient.WorkspaceRepo{
		"sess1": {{SourcePath: "/repo1", Branch: "feature-1"}},
		"sess2": {{SourcePath: "/repo2", Branch: "feature-2"}},
	}}
	remotes := &fakeRemoteResolver{resolutions: map[string]resolution{
		"/repo1": {provider: "github", remoteKey: "o/r", branch: "feature-1"},
		"/repo2": {provider: "github", remoteKey: "o/r", branch: "feature-2"},
	}}
	gh := &fakeGitHubChecker{discovered: map[string]string{
		provider.GitHubBranchKey("o", "r", "feature-1"): "https://github.com/o/r/pull/1",
		provider.GitHubBranchKey("o", "r", "feature-2"): "https://github.com/o/r/pull/2",
	}}
	sched := &Scheduler{Store: store, GitHub: gh, GitLab: &fakeGitLabChecker{}, Sessions: alwaysLiveSessions(), Notify: &fakeNotifier{}, Workspaces: workspaces, Remotes: remotes}
	return sched, gh, store
}

// TestDiscoverSessionsProbesOnlyTheNotifiedSessions covers the event-driven
// path (a session just went idle): one discovery pass for just that
// session, not every checkout the periodic pass walks.
func TestDiscoverSessionsProbesOnlyTheNotifiedSessions(t *testing.T) {
	ctx := context.Background()
	sched, gh, store := discoveryFixture(t)

	if err := sched.DiscoverSessions(ctx, []string{"sess1"}); err != nil {
		t.Fatalf("DiscoverSessions() error = %v", err)
	}

	getReg(t, store, "https://github.com/o/r/pull/1", "sess1")
	if _, ok, _ := store.Get(ctx, "https://github.com/o/r/pull/2", "sess2"); ok {
		t.Error("sess2's PR was registered, want only the notified session probed")
	}
	if len(gh.lastDiscoverItems) != 1 || gh.lastDiscoverItems[0].Branch != "feature-1" {
		t.Errorf("lastDiscoverItems = %+v, want exactly sess1's branch", gh.lastDiscoverItems)
	}
}

func TestDiscoverSessionsSkipsSessionsThatAreNotLive(t *testing.T) {
	ctx := context.Background()
	sched, gh, _ := discoveryFixture(t)

	if err := sched.DiscoverSessions(ctx, []string{"no-such-session"}); err != nil {
		t.Fatalf("DiscoverSessions() error = %v", err)
	}
	if gh.discoverCalls != 0 {
		t.Errorf("discoverCalls = %d, want 0 for a session with no live checkout", gh.discoverCalls)
	}
}

// TestTickRunsDiscoveryOnlyEveryDiscoveryInterval: with events covering the
// fast path, the periodic pass is a slow fallback — Tick (every 15s) must
// not run it every time.
func TestTickRunsDiscoveryOnlyEveryDiscoveryInterval(t *testing.T) {
	ctx := context.Background()
	sched, _, _ := discoveryFixture(t)
	sched.DiscoveryInterval = time.Hour
	remotes := sched.Remotes.(*fakeRemoteResolver)

	if err := sched.Tick(ctx); err != nil {
		t.Fatalf("Tick() (first) error = %v", err)
	}
	afterFirst := remotes.calls
	if afterFirst == 0 {
		t.Fatal("first Tick() ran no discovery, want an immediate pass at startup")
	}
	if err := sched.Tick(ctx); err != nil {
		t.Fatalf("Tick() (second) error = %v", err)
	}
	if remotes.calls != afterFirst {
		t.Errorf("remote resolves = %d after second Tick, want still %d (interval not elapsed)", remotes.calls, afterFirst)
	}

	sched.lastDiscovery = time.Now().Add(-2 * time.Hour)
	if err := sched.Tick(ctx); err != nil {
		t.Fatalf("Tick() (third) error = %v", err)
	}
	if remotes.calls == afterFirst {
		t.Error("third Tick() (interval elapsed) ran no discovery, want another pass")
	}
}

type countingClassifier struct {
	SessionClassifier
	refreshes int
}

func (c *countingClassifier) Refresh(ctx context.Context) error {
	c.refreshes++
	return c.SessionClassifier.Refresh(ctx)
}

// TestDiscoverSessionsUsesTheCachedSnapshotForKnownSessions: the idle event
// only carries a session id, so checkouts come from the aoe list snapshot —
// but that snapshot is at most one Tick old and already has every session
// that existed then, so a known session must not cost another `aoe list`.
func TestDiscoverSessionsUsesTheCachedSnapshotForKnownSessions(t *testing.T) {
	ctx := context.Background()
	sched, _, store := discoveryFixture(t)
	classifier := &countingClassifier{SessionClassifier: alwaysLiveSessions()}
	sched.Sessions = classifier

	if err := sched.DiscoverSessions(ctx, []string{"sess1"}); err != nil {
		t.Fatalf("DiscoverSessions() error = %v", err)
	}
	if classifier.refreshes != 0 {
		t.Errorf("Refresh calls = %d, want 0 for a session already in the directory", classifier.refreshes)
	}
	getReg(t, store, "https://github.com/o/r/pull/1", "sess1")
}

// TestDiscoverSessionsRefreshesOnlyForAnUnknownSession: a session created
// since the last Tick isn't in the snapshot yet; one refresh finds it.
func TestDiscoverSessionsRefreshesOnlyForAnUnknownSession(t *testing.T) {
	ctx := context.Background()
	sched, _, _ := discoveryFixture(t)
	classifier := &countingClassifier{SessionClassifier: alwaysLiveSessions()}
	sched.Sessions = classifier

	if err := sched.DiscoverSessions(ctx, []string{"sess1", "brand-new"}); err != nil {
		t.Fatalf("DiscoverSessions() error = %v", err)
	}
	if classifier.refreshes != 1 {
		t.Errorf("Refresh calls = %d, want exactly 1 (one unknown session in the batch)", classifier.refreshes)
	}
}

// ExtraRemotes lets the fake name a checkout's other remotes like the real
// resolver does for a fork checkout.
type extraRemotes struct {
	keys   []string
	owners []string
}

func (f *fakeRemoteResolver) ExtraRemotes(ctx context.Context, sourcePath string) ([]string, []string) {
	e := f.extras[sourcePath]
	return e.keys, e.owners
}

// A fork checkout pushes to origin (me/r) but its PR lives in the upstream
// repo (up/r): discovery must look there too, restricted to PRs whose head
// repo is one of the checkout's own.
func TestDiscoverTickFindsPRInUpstreamRepoOfAForkCheckout(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)

	workspaces := &fakeWorkspaceSource{workspaces: map[string][]aoeclient.WorkspaceRepo{
		"sess1": {{SourcePath: "/repo", Branch: "feat/x"}},
	}}
	remotes := &fakeRemoteResolver{
		resolutions: map[string]resolution{"/repo": {provider: "github", remoteKey: "me/r", branch: "feat/x"}},
		extras:      map[string]extraRemotes{"/repo": {keys: []string{"up/r"}, owners: []string{"me", "up"}}},
	}
	gh := &fakeGitHubChecker{discovered: map[string]string{
		provider.GitHubBranchKey("up", "r", "feat/x"): "https://github.com/up/r/pull/7",
	}}
	sched := &Scheduler{Store: store, GitHub: gh, GitLab: &fakeGitLabChecker{}, Sessions: alwaysLiveSessions(), Notify: &fakeNotifier{}, Workspaces: workspaces, Remotes: remotes}

	if err := sched.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}

	getReg(t, store, "https://github.com/up/r/pull/7", "sess1")
	var primary, extra *provider.GitHubBranchQuery
	for i := range gh.lastDiscoverItems {
		switch gh.lastDiscoverItems[i].Owner {
		case "me":
			primary = &gh.lastDiscoverItems[i]
		case "up":
			extra = &gh.lastDiscoverItems[i]
		}
	}
	if primary == nil || extra == nil {
		t.Fatalf("lastDiscoverItems = %+v, want both the origin repo and the upstream repo probed", gh.lastDiscoverItems)
	}
	if !slices.Equal(primary.HeadOwners, []string{"me", "up"}) {
		t.Errorf("origin query HeadOwners = %v, want the checkout's own remote owners [me up]: a stranger's fork PR with the same branch name is not this checkout's", primary.HeadOwners)
	}
	if len(extra.HeadOwners) != 2 {
		t.Errorf("upstream query HeadOwners = %v, want the checkout's remote owners [me up]", extra.HeadOwners)
	}
}

func TestDiscoverTickPrefersPRInOriginRepoOverUpstream(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)

	workspaces := &fakeWorkspaceSource{workspaces: map[string][]aoeclient.WorkspaceRepo{
		"sess1": {{SourcePath: "/repo", Branch: "feat/x"}},
	}}
	remotes := &fakeRemoteResolver{
		resolutions: map[string]resolution{"/repo": {provider: "github", remoteKey: "me/r", branch: "feat/x"}},
		extras:      map[string]extraRemotes{"/repo": {keys: []string{"up/r"}, owners: []string{"me", "up"}}},
	}
	gh := &fakeGitHubChecker{discovered: map[string]string{
		provider.GitHubBranchKey("me", "r", "feat/x"): "https://github.com/me/r/pull/1",
		provider.GitHubBranchKey("up", "r", "feat/x"): "https://github.com/up/r/pull/7",
	}}
	sched := &Scheduler{Store: store, GitHub: gh, GitLab: &fakeGitLabChecker{}, Sessions: alwaysLiveSessions(), Notify: &fakeNotifier{}, Workspaces: workspaces, Remotes: remotes}

	if err := sched.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}

	getReg(t, store, "https://github.com/me/r/pull/1", "sess1")
	if _, ok, _ := store.Get(ctx, "https://github.com/up/r/pull/7", "sess1"); ok {
		t.Error("registered the upstream PR as well, want only the first match")
	}
}

// ghCancelsPollContext expires the poll's context in the middle of the batch
// check, like a slow pass that has used up its deadline, and refuses discovery
// work that still carries it.
type ghCancelsPollContext struct {
	*fakeGitHubChecker
	cancel context.CancelFunc
}

func (g *ghCancelsPollContext) CheckBatch(ctx context.Context, urls []string) (map[string]provider.CheckResult, provider.GitHubBudgets, error) {
	res, budgets, err := g.fakeGitHubChecker.CheckBatch(ctx, urls)
	g.cancel()
	return res, budgets, err
}

func (g *ghCancelsPollContext) DiscoverBranchPRs(ctx context.Context, items []provider.GitHubBranchQuery) (map[string]string, provider.RateBudget, error) {
	if err := ctx.Err(); err != nil {
		return nil, provider.RateBudget{}, err
	}
	return g.fakeGitHubChecker.DiscoverBranchPRs(ctx, items)
}

// The periodic discovery pass runs last in a Tick; a slow pass before it must
// not leave it with an already-expired context, or it would be starved on
// every tick that is busy (e.g. the first one after a restart).
func TestDiscoveryRunsEvenWhenThePollContextIsSpent(t *testing.T) {
	store := openTestStore(t)
	now := time.Now()
	reg := registry.Registration{PRURL: "https://github.com/o/r/pull/1", SessionID: "sess1", Provider: "github", CIBucket: "none", NextDueAt: now.Add(-time.Second), CreatedAt: now}
	if err := store.Add(context.Background(), reg); err != nil {
		t.Fatal(err)
	}
	workspaces := &fakeWorkspaceSource{workspaces: map[string][]aoeclient.WorkspaceRepo{
		"sess1": {{SourcePath: "/repo", Branch: "feat/x"}},
	}}
	remotes := &fakeRemoteResolver{resolutions: map[string]resolution{
		"/repo": {provider: "github", remoteKey: "o/r", branch: "feat/x"},
	}}
	inner := &fakeGitHubChecker{
		results:    map[string]provider.CheckResult{reg.PRURL: {State: "open", CIBucket: "none"}},
		discovered: map[string]string{provider.GitHubBranchKey("o", "r", "feat/x"): "https://github.com/o/r/pull/9"},
		budgets: provider.GitHubBudgets{
			GraphQL: provider.RateBudget{Limit: 5000, Remaining: 4999, ResetAt: now.Add(time.Hour)},
			REST:    provider.RateBudget{Limit: 5000, Remaining: 4999, ResetAt: now.Add(time.Hour)},
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sched := &Scheduler{Store: store, GitHub: &ghCancelsPollContext{fakeGitHubChecker: inner, cancel: cancel}, GitLab: &fakeGitLabChecker{}, Sessions: alwaysLiveSessions(), Notify: &fakeNotifier{}, Workspaces: workspaces, Remotes: remotes}

	if err := sched.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}

	getReg(t, store, "https://github.com/o/r/pull/9", "sess1")
}

// The mirror image of a fork checkout: origin is the upstream repo and the
// fork is another remote. The PR lives in origin's repo but its head is the
// fork, so the origin query must accept the fork's owner, and still nobody
// else's.
func TestGitHubQueriesForAcceptTheForksOwnerOnTheUpstreamOrigin(t *testing.T) {
	target := registry.AutoDiscoveryTarget{
		RemoteKey: "up/r", Branch: "feat/x",
		ExtraRemoteKeys: []string{"me/r-fork"}, HeadOwners: []string{"up", "me"},
	}
	queries := githubQueriesFor(target)
	if len(queries) != 2 {
		t.Fatalf("queries = %+v, want origin and the fork repo", queries)
	}
	if q := queries[0]; q.Owner != "up" || !slices.Equal(q.HeadOwners, []string{"up", "me"}) {
		t.Errorf("origin query = %+v, want head owners [up me]", q)
	}

	plain := githubQueriesFor(registry.AutoDiscoveryTarget{RemoteKey: "up/r", Branch: "feat/x"})
	if len(plain) != 1 || !slices.Equal(plain[0].HeadOwners, []string{"up"}) {
		t.Errorf("no-fork queries = %+v, want only the origin's own owner", plain)
	}
}
