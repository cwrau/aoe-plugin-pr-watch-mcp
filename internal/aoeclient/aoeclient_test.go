package aoeclient

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"
)

// fakeLister serves a fixed session list and counts how often it is asked.
type fakeLister struct {
	sessions []Summary
	err      error
	calls    int
}

func (f *fakeLister) ListSessions(context.Context) ([]Summary, error) {
	f.calls++
	return f.sessions, f.err
}

func newTestClient(t *testing.T, sessions ...Summary) (*Client, *fakeLister) {
	t.Helper()
	l := &fakeLister{sessions: sessions}
	return New(l), l
}

func TestRefreshClassifiesAllStates(t *testing.T) {
	c, _ := newTestClient(t,
		Summary{ID: "live"},
		Summary{ID: "snoozed", Snoozed: true},
		Summary{ID: "archived", Archived: true},
		Summary{ID: "trashed", Trashed: true},
		Summary{ID: "both", Archived: true, Trashed: true},
	)
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	want := map[string]SessionClass{
		"live":     SessionLive,
		"snoozed":  SessionLive,
		"archived": SessionArchived,
		"trashed":  SessionGone,
		"both":     SessionGone, // trashed wins: it is never sent to and its registration is dropped
		"missing":  SessionGone, // hard-deleted or never existed
	}
	for id, w := range want {
		if got := c.Classify(id); got != w {
			t.Errorf("Classify(%q) = %v, want %v", id, got, w)
		}
	}
}

func TestRefreshCachesTitles(t *testing.T) {
	c, _ := newTestClient(t, Summary{ID: "a", Title: "Fix the thing"}, Summary{ID: "b"})
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	if got := c.Title("a"); got != "Fix the thing" {
		t.Errorf("Title(a) = %q, want %q", got, "Fix the thing")
	}
	if got := c.Title("b"); got != "" {
		t.Errorf("Title(b) = %q, want empty", got)
	}
	if got := c.Title("unknown"); got != "" {
		t.Errorf("Title(unknown) = %q, want empty", got)
	}
}

func TestWorkspaceReposFromMultiRepoWorkspace(t *testing.T) {
	c, _ := newTestClient(t, Summary{ID: "a", WorkspaceRepos: []WorkspaceRepo{
		{SourcePath: "/p/aoe", Branch: "feature-x"},
		{SourcePath: "/p/aoe/vendor", Branch: "main"},
		{SourcePath: "/p/aoe", Branch: "dup"}, // same path twice: first wins
		{SourcePath: "", Branch: "ignored"},
	}})
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	want := []WorkspaceRepo{{SourcePath: "/p/aoe", Branch: "feature-x"}, {SourcePath: "/p/aoe/vendor", Branch: "main"}}
	if got := c.WorkspaceRepos("a"); !slices.Equal(got, want) {
		t.Errorf("WorkspaceRepos(a) = %+v, want %+v", got, want)
	}
}

// TestWorkspaceReposCoversWorktreeSessionRoot: a plain single-repo worktree
// session's own root is the worktree itself. Confirmed live that omitting
// this case made auto-watch discovery find nothing for the common case.
func TestWorkspaceReposCoversWorktreeSessionRoot(t *testing.T) {
	c, _ := newTestClient(t, Summary{ID: "a", ProjectPath: "/w/proj-worktrees/feature-x", Worktree: &Worktree{Branch: "feature-x"}})
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	want := []WorkspaceRepo{{SourcePath: "/w/proj-worktrees/feature-x", Branch: "feature-x"}}
	if got := c.WorkspaceRepos("a"); !slices.Equal(got, want) {
		t.Errorf("WorkspaceRepos(a) = %+v, want the worktree root", got)
	}
}

func TestWorkspaceReposCombinesWorktreeAndWorkspaceRepos(t *testing.T) {
	c, _ := newTestClient(t, Summary{
		ID:             "a",
		ProjectPath:    "/root",
		Worktree:       &Worktree{Branch: "feature-x"},
		WorkspaceRepos: []WorkspaceRepo{{SourcePath: "/root/sub", Branch: "main"}},
	})
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	want := []WorkspaceRepo{{SourcePath: "/root/sub", Branch: "main"}, {SourcePath: "/root", Branch: "feature-x"}}
	if got := c.WorkspaceRepos("a"); !slices.Equal(got, want) {
		t.Errorf("WorkspaceRepos(a) = %+v, want %+v", got, want)
	}
}

// TestWorktreeRootNotDuplicatedWhenAlsoAWorkspaceRepo: the host's
// workspace_repos can already include the session's own root.
func TestWorktreeRootNotDuplicatedWhenAlsoAWorkspaceRepo(t *testing.T) {
	c, _ := newTestClient(t, Summary{
		ID:             "a",
		ProjectPath:    "/root",
		Worktree:       &Worktree{Branch: "feature-x"},
		WorkspaceRepos: []WorkspaceRepo{{SourcePath: "/root", Branch: "feature-x"}},
	})
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	if got := c.WorkspaceRepos("a"); len(got) != 1 {
		t.Errorf("WorkspaceRepos(a) = %+v, want exactly one entry", got)
	}
}

func TestWorkspaceReposOnlyForLiveSessions(t *testing.T) {
	repos := []WorkspaceRepo{{SourcePath: "/p/aoe", Branch: "main"}}
	c, _ := newTestClient(t,
		Summary{ID: "live", WorkspaceRepos: repos},
		Summary{ID: "archived", Archived: true, WorkspaceRepos: repos},
		Summary{ID: "trashed", Trashed: true, WorkspaceRepos: repos},
		Summary{ID: "bare"},
	)
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	live := c.LiveWorkspaces()
	if len(live) != 1 || len(live["live"]) != 1 {
		t.Errorf("LiveWorkspaces() = %+v, want only the live session with checkouts", live)
	}
	if got := c.WorkspaceRepos("archived"); len(got) != 0 {
		t.Errorf("WorkspaceRepos(archived) = %+v, want none", got)
	}
}

func TestLiveWorkspacesReturnsACopy(t *testing.T) {
	c, _ := newTestClient(t, Summary{ID: "a", WorkspaceRepos: []WorkspaceRepo{{SourcePath: "/p", Branch: "main"}}})
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	c.LiveWorkspaces()["a"][0].Branch = "mutated"
	if got := c.WorkspaceRepos("a")[0].Branch; got != "main" {
		t.Errorf("branch = %q after a caller mutated LiveWorkspaces()'s result, want it unchanged", got)
	}
}

func TestRefreshFailureKeepsThePreviousSnapshot(t *testing.T) {
	c, l := newTestClient(t, Summary{ID: "a", Title: "kept"})
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	l.err = errors.New("host unavailable")
	if err := c.Refresh(context.Background()); err == nil {
		t.Fatal("Refresh() error = nil, want the lister's error")
	}
	if c.Classify("a") != SessionLive || c.Title("a") != "kept" {
		t.Error("a failed refresh dropped the previous snapshot, want it kept")
	}
}

func TestRefreshWithoutALister(t *testing.T) {
	if err := New(nil).Refresh(context.Background()); err == nil {
		t.Error("Refresh() error = nil without a Lister, want an error")
	}
}

// TestRefreshIfStaleThrottles: events keep the directory current, so the
// per-tick call must not hit the host every time — only a periodic resync.
func TestRefreshIfStaleThrottles(t *testing.T) {
	c, l := newTestClient(t, Summary{ID: "a"})
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }
	c.ResyncEvery = 10 * time.Minute

	for i := range 3 {
		if err := c.RefreshIfStale(context.Background()); err != nil {
			t.Fatalf("RefreshIfStale() #%d error = %v", i, err)
		}
	}
	if l.calls != 1 {
		t.Fatalf("lister calls = %d after three immediate calls, want 1 (the first load)", l.calls)
	}

	now = now.Add(9 * time.Minute)
	_ = c.RefreshIfStale(context.Background())
	if l.calls != 1 {
		t.Errorf("lister calls = %d inside the interval, want still 1", l.calls)
	}

	now = now.Add(2 * time.Minute)
	_ = c.RefreshIfStale(context.Background())
	if l.calls != 2 {
		t.Errorf("lister calls = %d past the interval, want 2", l.calls)
	}
}

func TestRefreshIfStaleRetriesAfterAFailedLoad(t *testing.T) {
	c, l := newTestClient(t, Summary{ID: "a"})
	l.err = errors.New("not ready")
	if err := c.RefreshIfStale(context.Background()); err == nil {
		t.Fatal("RefreshIfStale() error = nil, want the failure")
	}
	l.err = nil
	if err := c.RefreshIfStale(context.Background()); err != nil {
		t.Fatalf("RefreshIfStale() retry error = %v", err)
	}
	if l.calls != 2 {
		t.Errorf("lister calls = %d, want 2: a failed load must not count as a completed resync", l.calls)
	}
}

func TestRefreshForcedIgnoresTheThrottle(t *testing.T) {
	c, l := newTestClient(t, Summary{ID: "a"})
	_ = c.Refresh(context.Background())
	_ = c.Refresh(context.Background())
	if l.calls != 2 {
		t.Errorf("lister calls = %d, want 2: Refresh is always a real resync", l.calls)
	}
}

func TestApplyLifecycleKeepsTheDirectoryCurrent(t *testing.T) {
	c, _ := newTestClient(t)

	c.ApplyLifecycle("created", "a", &Summary{ID: "a", Title: "new"})
	if c.Classify("a") != SessionLive || c.Title("a") != "new" {
		t.Fatal("a created event did not add the session")
	}

	c.ApplyLifecycle("renamed", "a", &Summary{ID: "a", Title: "renamed"})
	if got := c.Title("a"); got != "renamed" {
		t.Errorf("Title(a) = %q after renamed, want renamed", got)
	}

	c.ApplyLifecycle("archived", "a", &Summary{ID: "a", Archived: true})
	if c.Classify("a") != SessionArchived {
		t.Error("an archived event did not archive the session")
	}

	c.ApplyLifecycle("unarchived", "a", &Summary{ID: "a"})
	if c.Classify("a") != SessionLive {
		t.Error("an unarchived event did not restore the session to live")
	}

	c.ApplyLifecycle("trashed", "a", &Summary{ID: "a", Trashed: true})
	if c.Classify("a") != SessionGone {
		t.Error("a trashed event did not mark the session gone")
	}

	c.ApplyLifecycle("restored", "a", &Summary{ID: "a"})
	if c.Classify("a") != SessionLive {
		t.Error("a restored event did not bring the session back")
	}

	c.ApplyLifecycle("deleted", "a", nil)
	if c.Classify("a") != SessionGone || c.Title("a") != "" {
		t.Error("a deleted event did not remove the session")
	}
}

func TestApplyLifecycleWorkspaceChanged(t *testing.T) {
	c, _ := newTestClient(t, Summary{ID: "a", WorkspaceRepos: []WorkspaceRepo{{SourcePath: "/p/one", Branch: "main"}}})
	_ = c.Refresh(context.Background())

	c.ApplyLifecycle("workspace_changed", "a", &Summary{ID: "a", WorkspaceRepos: []WorkspaceRepo{{SourcePath: "/p/two", Branch: "main"}}})
	if got := c.WorkspaceRepos("a"); len(got) != 1 || got[0].SourcePath != "/p/two" {
		t.Errorf("WorkspaceRepos(a) = %+v, want the new checkout set", got)
	}
}

func TestApplyLifecycleWithoutSummaryIsIgnoredExceptDelete(t *testing.T) {
	c, _ := newTestClient(t, Summary{ID: "a", Title: "kept"})
	_ = c.Refresh(context.Background())
	c.ApplyLifecycle("renamed", "a", nil) // malformed: no summary
	if got := c.Title("a"); got != "kept" {
		t.Errorf("Title(a) = %q, want unchanged when a non-delete event carries no summary", got)
	}
}

func TestParseSessionList(t *testing.T) {
	raw := json.RawMessage(`{"sessions":[
		{"id":"a","title":"One","project_path":"/p/one","tool":"claude","status":"Idle","archived":false,"snoozed":true,"trashed":false,
		 "workspace_repos":[{"source_path":"/p/one","branch":"feature"}],
		 "worktree":{"branch":"feature","main_repo_path":"/p/main","managed_by_aoe":true}},
		{"id":"b","archived":true,"trashed":true}
	]}`)
	got, err := ParseSessionList(raw)
	if err != nil {
		t.Fatalf("ParseSessionList() error = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ParseSessionList() = %+v, want 2 sessions", got)
	}
	a := got[0]
	if a.ID != "a" || a.Title != "One" || a.ProjectPath != "/p/one" || !a.Snoozed || a.Archived || a.Trashed {
		t.Errorf("session a = %+v", a)
	}
	if a.Worktree == nil || a.Worktree.Branch != "feature" {
		t.Errorf("session a worktree = %+v, want branch feature", a.Worktree)
	}
	if len(a.WorkspaceRepos) != 1 || a.WorkspaceRepos[0] != (WorkspaceRepo{SourcePath: "/p/one", Branch: "feature"}) {
		t.Errorf("session a workspace_repos = %+v", a.WorkspaceRepos)
	}
	if b := got[1]; !b.Archived || !b.Trashed || b.Worktree != nil {
		t.Errorf("session b = %+v, want archived+trashed, no worktree", b)
	}
}

func TestParseSessionListRejectsGarbage(t *testing.T) {
	if _, err := ParseSessionList(json.RawMessage(`{"sessions":"nope"}`)); err == nil {
		t.Error("ParseSessionList() error = nil for a non-list, want an error")
	}
	if _, err := ParseSessionList(json.RawMessage(`not json`)); err == nil {
		t.Error("ParseSessionList() error = nil for invalid JSON, want an error")
	}
}

func TestParseSummary(t *testing.T) {
	s, err := ParseSummary(json.RawMessage(`{"id":"a","title":"T","trashed":true}`))
	if err != nil {
		t.Fatalf("ParseSummary() error = %v", err)
	}
	if s.ID != "a" || s.Title != "T" || !s.Trashed {
		t.Errorf("ParseSummary() = %+v", s)
	}
	if _, err := ParseSummary(json.RawMessage(`[1]`)); err == nil {
		t.Error("ParseSummary() error = nil for a non-object, want an error")
	}
}
