package registry

import (
	"context"
	"database/sql"
	"slices"
	"testing"
)

func unresolved(t *testing.T, s *Store, sessionIDs ...string) []AutoDiscoveryTarget {
	t.Helper()
	got, err := s.UnresolvedAutoDiscoveryForSessions(context.Background(), sessionIDs)
	if err != nil {
		t.Fatalf("UnresolvedAutoDiscoveryForSessions() error = %v", err)
	}
	return got
}

func TestUpsertAutoDiscoveryTargetThenUnresolved(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	if err := s.UpsertAutoDiscoveryTarget(ctx, "sess1", "/repo", "github", "o/r", "feature-x"); err != nil {
		t.Fatalf("UpsertAutoDiscoveryTarget() error = %v", err)
	}

	got := unresolved(t, s, "sess1")
	if len(got) != 1 || got[0].SessionID != "sess1" || got[0].SourcePath != "/repo" || got[0].Provider != "github" || got[0].RemoteKey != "o/r" || got[0].Branch != "feature-x" {
		t.Fatalf("UnresolvedAutoDiscoveryForSessions() = %+v, want the freshly-inserted target", got)
	}
}

func TestMarkAutoDiscoveryResolvedStopsItShowingAsUnresolved(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	if err := s.UpsertAutoDiscoveryTarget(ctx, "sess1", "/repo", "github", "o/r", "feature-x"); err != nil {
		t.Fatalf("UpsertAutoDiscoveryTarget() error = %v", err)
	}
	if err := s.MarkAutoDiscoveryResolved(ctx, "sess1", "/repo", "feature-x"); err != nil {
		t.Fatalf("MarkAutoDiscoveryResolved() error = %v", err)
	}

	if got := unresolved(t, s, "sess1"); len(got) != 0 {
		t.Errorf("UnresolvedAutoDiscoveryForSessions() = %+v, want none (already resolved for this branch)", got)
	}
}

func TestUpsertAutoDiscoveryTargetBranchChangeResetsResolvedState(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	if err := s.UpsertAutoDiscoveryTarget(ctx, "sess1", "/repo", "github", "o/r", "feature-x"); err != nil {
		t.Fatalf("UpsertAutoDiscoveryTarget() error = %v", err)
	}
	if err := s.MarkAutoDiscoveryResolved(ctx, "sess1", "/repo", "feature-x"); err != nil {
		t.Fatalf("MarkAutoDiscoveryResolved() error = %v", err)
	}

	// An unchanged branch must not re-arm an already-resolved target...
	if err := s.UpsertAutoDiscoveryTarget(ctx, "sess1", "/repo", "github", "o/r", "feature-x"); err != nil {
		t.Fatalf("UpsertAutoDiscoveryTarget() (same branch) error = %v", err)
	}
	if got := unresolved(t, s, "sess1"); len(got) != 0 {
		t.Fatalf("UnresolvedAutoDiscoveryForSessions() = %+v, want none after re-upserting the same branch", got)
	}

	// ...but switching branches must, since the old branch's answer says
	// nothing about the new one.
	if err := s.UpsertAutoDiscoveryTarget(ctx, "sess1", "/repo", "github", "o/r", "feature-y"); err != nil {
		t.Fatalf("UpsertAutoDiscoveryTarget() (branch change) error = %v", err)
	}
	if got := unresolved(t, s, "sess1"); len(got) != 1 || got[0].Branch != "feature-y" {
		t.Fatalf("UnresolvedAutoDiscoveryForSessions() = %+v, want the new branch unresolved again", got)
	}
}

func TestUnresolvedAutoDiscoveryForSessionsOnlyReturnsAskedForSessions(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	for _, sid := range []string{"sess1", "sess2", "sess3"} {
		if err := s.UpsertAutoDiscoveryTarget(ctx, sid, "/repo-"+sid, "github", "o/r", "feature-"+sid); err != nil {
			t.Fatalf("UpsertAutoDiscoveryTarget() error = %v", err)
		}
	}
	if err := s.MarkAutoDiscoveryResolved(ctx, "sess2", "/repo-sess2", "feature-sess2"); err != nil {
		t.Fatal(err)
	}

	got := unresolved(t, s, "sess1", "sess2")
	if len(got) != 1 || got[0].SessionID != "sess1" {
		t.Fatalf("UnresolvedAutoDiscoveryForSessions() = %+v, want only sess1 (sess2 resolved, sess3 not asked for)", got)
	}
	if none := unresolved(t, s); len(none) != 0 {
		t.Errorf("UnresolvedAutoDiscoveryForSessions() with no ids = %+v, want empty", none)
	}
}

func TestPruneAutoDiscoveryForSessionRemovesDroppedRepos(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	if err := s.UpsertAutoDiscoveryTarget(ctx, "sess1", "/repo-a", "github", "o/a", "main"); err != nil {
		t.Fatalf("UpsertAutoDiscoveryTarget() error = %v", err)
	}
	if err := s.UpsertAutoDiscoveryTarget(ctx, "sess1", "/repo-b", "github", "o/b", "main"); err != nil {
		t.Fatalf("UpsertAutoDiscoveryTarget() error = %v", err)
	}

	if err := s.PruneAutoDiscoveryForSession(ctx, "sess1", []string{"/repo-a"}); err != nil {
		t.Fatalf("PruneAutoDiscoveryForSession() error = %v", err)
	}

	got := unresolved(t, s, "sess1")
	if len(got) != 1 || got[0].SourcePath != "/repo-a" {
		t.Fatalf("UnresolvedAutoDiscoveryForSessions() = %+v, want only /repo-a left", got)
	}
}

func TestPruneAutoDiscoveryForSessionEmptyKeepDeletesAll(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	if err := s.UpsertAutoDiscoveryTarget(ctx, "sess1", "/repo-a", "github", "o/a", "main"); err != nil {
		t.Fatalf("UpsertAutoDiscoveryTarget() error = %v", err)
	}
	if err := s.PruneAutoDiscoveryForSession(ctx, "sess1", nil); err != nil {
		t.Fatalf("PruneAutoDiscoveryForSession() error = %v", err)
	}

	sessions, err := s.DistinctAutoDiscoverySessionIDs(ctx)
	if err != nil {
		t.Fatalf("DistinctAutoDiscoverySessionIDs() error = %v", err)
	}
	if len(sessions) != 0 {
		t.Errorf("DistinctAutoDiscoverySessionIDs() = %v, want none", sessions)
	}
}

func TestDeleteAutoDiscoveryForSession(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	if err := s.UpsertAutoDiscoveryTarget(ctx, "sess1", "/repo-a", "github", "o/a", "main"); err != nil {
		t.Fatalf("UpsertAutoDiscoveryTarget() error = %v", err)
	}
	if err := s.UpsertAutoDiscoveryTarget(ctx, "sess2", "/repo-b", "github", "o/b", "main"); err != nil {
		t.Fatalf("UpsertAutoDiscoveryTarget() error = %v", err)
	}

	if err := s.DeleteAutoDiscoveryForSession(ctx, "sess1"); err != nil {
		t.Fatalf("DeleteAutoDiscoveryForSession() error = %v", err)
	}

	sessions, err := s.DistinctAutoDiscoverySessionIDs(ctx)
	if err != nil {
		t.Fatalf("DistinctAutoDiscoverySessionIDs() error = %v", err)
	}
	if len(sessions) != 1 || sessions[0] != "sess2" {
		t.Errorf("DistinctAutoDiscoverySessionIDs() = %v, want just [sess2]", sessions)
	}
}

// A fork checkout also probes its other remotes' repos; they ride on the
// target's row and survive branch-keyed upserts.
func TestAutoDiscoveryTargetCarriesExtraRemotesAndHeadOwners(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	if err := s.UpsertAutoDiscoveryTarget(ctx, "sess1", "/repo", "github", "me/r", "feature-x"); err != nil {
		t.Fatalf("UpsertAutoDiscoveryTarget() error = %v", err)
	}
	if got := unresolved(t, s, "sess1"); len(got) != 1 || len(got[0].ExtraRemoteKeys) != 0 || len(got[0].HeadOwners) != 0 {
		t.Fatalf("fresh target = %+v, want no extra remotes or head owners", got)
	}

	if err := s.SetAutoDiscoveryRemotes(ctx, "sess1", "/repo", []string{"up/r"}, []string{"me", "up"}); err != nil {
		t.Fatalf("SetAutoDiscoveryRemotes() error = %v", err)
	}
	// Re-upserting the primary target (every pass does) must not wipe them.
	if err := s.UpsertAutoDiscoveryTarget(ctx, "sess1", "/repo", "github", "me/r", "feature-x"); err != nil {
		t.Fatalf("UpsertAutoDiscoveryTarget() error = %v", err)
	}

	got := unresolved(t, s, "sess1")
	if len(got) != 1 || !slices.Equal(got[0].ExtraRemoteKeys, []string{"up/r"}) || !slices.Equal(got[0].HeadOwners, []string{"me", "up"}) {
		t.Fatalf("target = %+v, want extra remote up/r and head owners [me up]", got)
	}
}

// A database from before extra remotes existed gains the columns in place.
func TestOpenAddsAutoDiscoveryRemoteColumnsToAnExistingDatabase(t *testing.T) {
	path := t.TempDir() + "/old.db"
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(`CREATE TABLE auto_discovery (
		session_id TEXT NOT NULL, source_path TEXT NOT NULL, provider TEXT NOT NULL DEFAULT '',
		remote_key TEXT NOT NULL DEFAULT '', branch TEXT NOT NULL DEFAULT '', resolved_for_branch TEXT NOT NULL DEFAULT '',
		PRIMARY KEY (session_id, source_path));
		INSERT INTO auto_discovery (session_id, source_path, provider, remote_key, branch) VALUES ('sess1', '/repo', 'github', 'me/r', 'b')`); err != nil {
		t.Fatal(err)
	}
	_ = old.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = s.Close() }()
	got := unresolved(t, s, "sess1")
	if len(got) != 1 || got[0].RemoteKey != "me/r" {
		t.Fatalf("target after migration = %+v, want the existing row kept", got)
	}
}
