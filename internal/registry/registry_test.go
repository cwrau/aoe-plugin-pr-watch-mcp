package registry

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"slices"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestAddAndList(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	now := time.Now().Truncate(time.Second)
	reg := Registration{
		PRURL:     "https://github.com/o/r/pull/1",
		SessionID: "abc123",
		Provider:  "github",
		NextDueAt: now,
		CreatedAt: now,
	}
	if err := s.Add(ctx, reg); err != nil {
		t.Fatalf("Add() error = %v", err)
	}

	got, err := s.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("List() len = %d, want 1", len(got))
	}
	if got[0].PRURL != reg.PRURL || got[0].SessionID != reg.SessionID || got[0].Provider != reg.Provider {
		t.Errorf("List()[0] = %+v, want match of %+v", got[0], reg)
	}
	if got[0].Status != StatusWatching {
		t.Errorf("List()[0].Status = %q, want %q (a fresh registration always starts actively watched)", got[0].Status, StatusWatching)
	}
}

func TestAddIsIdempotentForSameSession(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	now := time.Now().Truncate(time.Second)

	reg := Registration{PRURL: "https://github.com/o/r/pull/1", SessionID: "sess1", Provider: "github", NextDueAt: now, CreatedAt: now}
	if err := s.Add(ctx, reg); err != nil {
		t.Fatalf("Add() first error = %v", err)
	}
	if err := s.Add(ctx, reg); err != nil {
		t.Fatalf("Add() second error = %v", err)
	}

	got, err := s.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("List() len = %d, want 1 (re-registering the same (pr_url, session_id) pair should upsert, not duplicate)", len(got))
	}
}

// TestAddCreatesSeparateRowPerSession guards the actual behavior this
// registry's primary key exists to enable: multiple sessions independently
// watching the same PR/MR each get their own row, not one shared row that
// silently reassigns ownership on the second call.
func TestAddCreatesSeparateRowPerSession(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	now := time.Now().Truncate(time.Second)

	reg := Registration{PRURL: "https://github.com/o/r/pull/1", SessionID: "first", Provider: "github", NextDueAt: now, CreatedAt: now}
	if err := s.Add(ctx, reg); err != nil {
		t.Fatalf("Add() first error = %v", err)
	}
	reg.SessionID = "second"
	if err := s.Add(ctx, reg); err != nil {
		t.Fatalf("Add() second error = %v", err)
	}

	got, err := s.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("List() len = %d, want 2 (each session gets its own independent row for the same pr_url)", len(got))
	}
	sessions := map[string]bool{got[0].SessionID: true, got[1].SessionID: true}
	if !sessions["first"] || !sessions["second"] {
		t.Errorf("List() sessions = %v, want both %q and %q", sessions, "first", "second")
	}
}

func TestAddReactivatesStatusOnReregister(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	now := time.Now().Truncate(time.Second)

	reg := Registration{PRURL: "https://github.com/o/r/pull/1", SessionID: "sess1", Provider: "github", NextDueAt: now, CreatedAt: now}
	if err := s.Add(ctx, reg); err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	if err := s.SetStatus(ctx, reg.PRURL, StatusMerged); err != nil {
		t.Fatalf("SetStatus() error = %v", err)
	}

	// Re-registering (e.g. the same PR reopened, or just re-running register)
	// must resume active watching regardless of whatever terminal status it
	// was previously left in.
	if err := s.Add(ctx, reg); err != nil {
		t.Fatalf("Add() (re-register) error = %v", err)
	}

	got, err := s.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(got) != 1 || got[0].Status != StatusWatching {
		t.Errorf("List() = %+v, want a single row back to status %q", got, StatusWatching)
	}
}

func TestAddPersistsAndUpdatesEventFilter(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	now := time.Now().Truncate(time.Second)

	reg := Registration{PRURL: "https://github.com/o/r/pull/1", SessionID: "sess1", Provider: "github", NextDueAt: now, CreatedAt: now, EventFilter: []string{"MERGED", "CLOSED"}}
	if err := s.Add(ctx, reg); err != nil {
		t.Fatalf("Add() error = %v", err)
	}

	got, ok, err := s.Get(ctx, reg.PRURL, reg.SessionID)
	if err != nil || !ok {
		t.Fatalf("Get() = %+v, %v, %v", got, ok, err)
	}
	if !slices.Equal(got.EventFilter, []string{"MERGED", "CLOSED"}) {
		t.Errorf("Get().EventFilter = %v, want [MERGED CLOSED]", got.EventFilter)
	}

	// Re-registering the same (pr_url, session_id) pair with a different
	// filter replaces it in place — no separate tool needed to change it.
	reg.EventFilter = []string{"CI"}
	if err := s.Add(ctx, reg); err != nil {
		t.Fatalf("Add() (re-register with new filter) error = %v", err)
	}
	got, ok, err = s.Get(ctx, reg.PRURL, reg.SessionID)
	if err != nil || !ok {
		t.Fatalf("Get() = %+v, %v, %v", got, ok, err)
	}
	if !slices.Equal(got.EventFilter, []string{"CI"}) {
		t.Errorf("Get().EventFilter after re-register = %v, want [CI]", got.EventFilter)
	}
}

func TestDueBefore(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	base := time.Now().Truncate(time.Second)

	early := Registration{PRURL: "https://github.com/o/r/pull/1", SessionID: "a", Provider: "github", NextDueAt: base.Add(-time.Hour), CreatedAt: base}
	late := Registration{PRURL: "https://github.com/o/r/pull/2", SessionID: "b", Provider: "github", NextDueAt: base.Add(time.Hour), CreatedAt: base}
	mergedButDue := Registration{PRURL: "https://github.com/o/r/pull/3", SessionID: "c", Provider: "github", NextDueAt: base.Add(-time.Hour), CreatedAt: base}
	if err := s.Add(ctx, early); err != nil {
		t.Fatalf("Add(early) error = %v", err)
	}
	if err := s.Add(ctx, late); err != nil {
		t.Fatalf("Add(late) error = %v", err)
	}
	if err := s.Add(ctx, mergedButDue); err != nil {
		t.Fatalf("Add(mergedButDue) error = %v", err)
	}
	if err := s.SetStatus(ctx, mergedButDue.PRURL, StatusMerged); err != nil {
		t.Fatalf("SetStatus() error = %v", err)
	}

	due, err := s.DueBefore(ctx, base)
	if err != nil {
		t.Fatalf("DueBefore() error = %v", err)
	}
	if len(due) != 1 || due[0].PRURL != early.PRURL {
		t.Errorf("DueBefore(%v) = %+v, want only %q (a merged row must never be picked up again even if its old next_due_at has passed)", base, due, early.PRURL)
	}
}

func TestUpdateState(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	now := time.Now().Truncate(time.Second)
	reg := Registration{PRURL: "https://github.com/o/r/pull/1", SessionID: "a", Provider: "github", NextDueAt: now, CreatedAt: now}
	if err := s.Add(ctx, reg); err != nil {
		t.Fatalf("Add() error = %v", err)
	}

	newDue := now.Add(5 * time.Minute)
	reviewAt := now.Add(-time.Hour)
	update := StateUpdate{
		ReviewCount:         3,
		CIBucket:            "failure",
		CIWasFailing:        true,
		CIDetail:            "build",
		UnresolvedThreadIDs: []string{"t1", "t2"},
		TopLevelCommentIDs:  []string{"c1", "c2"},
		LatestCommentHref:   "https://github.com/o/r/pull/1#issuecomment-1",
		LatestReviewState:   "CHANGES_REQUESTED",
		LatestReviewer:      "bob",
		LatestReviewAt:      reviewAt,
		Title:               "Fix the thing",
		Description:         "Fixes the widget rendering bug.",
		NextDueAt:           newDue,
		HasConflict:         true,
		CIHref:              "https://github.com/o/r/actions/runs/123",
		ReviewHref:          "https://github.com/o/r/pull/1#pullrequestreview-999",
		MergeQueueState:     "QUEUED",
		MergeQueuePosition:  3,
		Blocked:             true,
	}
	if err := s.UpdateState(ctx, reg.PRURL, reg.SessionID, update); err != nil {
		t.Fatalf("UpdateState() error = %v", err)
	}

	got, err := s.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("List() len = %d, want 1", len(got))
	}
	r := got[0]
	if r.ReviewCount != 3 || r.CIBucket != "failure" || !r.CIWasFailing || r.CIDetail != "build" || !slices.Equal(r.UnresolvedThreadIDs, update.UnresolvedThreadIDs) || !slices.Equal(r.TopLevelCommentIDs, update.TopLevelCommentIDs) || r.LatestCommentHref != update.LatestCommentHref || r.LatestReviewState != "CHANGES_REQUESTED" || r.LatestReviewer != "bob" || !r.LatestReviewAt.Equal(reviewAt) || r.Title != "Fix the thing" || r.Description != "Fixes the widget rendering bug." || !r.NextDueAt.Equal(newDue) || !r.HasConflict || r.CIHref != update.CIHref || r.ReviewHref != update.ReviewHref || r.MergeQueueState != update.MergeQueueState || r.MergeQueuePosition != update.MergeQueuePosition || !r.Blocked {
		t.Errorf("after UpdateState, got %+v", r)
	}
}

func TestUpdateStateClearsHasConflict(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	now := time.Now().Truncate(time.Second)
	reg := Registration{PRURL: "https://github.com/o/r/pull/1", SessionID: "a", Provider: "github", NextDueAt: now, CreatedAt: now}
	if err := s.Add(ctx, reg); err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	if err := s.UpdateState(ctx, reg.PRURL, reg.SessionID, StateUpdate{NextDueAt: now, HasConflict: true}); err != nil {
		t.Fatalf("UpdateState() error = %v", err)
	}
	if err := s.UpdateState(ctx, reg.PRURL, reg.SessionID, StateUpdate{NextDueAt: now, HasConflict: false}); err != nil {
		t.Fatalf("UpdateState() error = %v", err)
	}

	got, _, err := s.Get(ctx, reg.PRURL, reg.SessionID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.HasConflict {
		t.Error("Get().HasConflict = true, want false once a later check reports the conflict resolved")
	}
}

func TestSetStatus(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	now := time.Now()
	reg := Registration{PRURL: "https://github.com/o/r/pull/1", SessionID: "a", Provider: "github", NextDueAt: now, CreatedAt: now}
	if err := s.Add(ctx, reg); err != nil {
		t.Fatalf("Add() error = %v", err)
	}

	if err := s.SetStatus(ctx, reg.PRURL, StatusClosed); err != nil {
		t.Fatalf("SetStatus() error = %v", err)
	}

	got, err := s.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(got) != 1 || got[0].Status != StatusClosed {
		t.Errorf("List() = %+v, want status %q (the row must survive, not be deleted)", got, StatusClosed)
	}
}

func TestSetMerged(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	now := time.Now()
	reg := Registration{PRURL: "https://github.com/o/r/pull/1", SessionID: "a", Provider: "github", NextDueAt: now, CreatedAt: now}
	if err := s.Add(ctx, reg); err != nil {
		t.Fatalf("Add() error = %v", err)
	}

	mergedAt := now.Truncate(time.Second)
	if err := s.SetMerged(ctx, reg.PRURL, mergedAt); err != nil {
		t.Fatalf("SetMerged() error = %v", err)
	}

	got, err := s.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(got) != 1 || got[0].Status != StatusMerged || !got[0].MergedAt.Equal(mergedAt) {
		t.Errorf("List() = %+v, want status %q and MergedAt %v", got, StatusMerged, mergedAt)
	}
}

func TestOpenMigratesOldSchemaMissingLatestReviewState(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "old.db")

	// Simulate a database created before latest_review_state existed:
	// open with the pre-migration schema directly, bypassing Open's own
	// (already-migrated) CREATE TABLE.
	oldSchema := `
CREATE TABLE IF NOT EXISTS registrations (
	pr_url TEXT PRIMARY KEY,
	session_id TEXT NOT NULL,
	provider TEXT NOT NULL,
	review_count INTEGER NOT NULL DEFAULT 0,
	ci_bucket TEXT NOT NULL DEFAULT 'none',
	unresolved INTEGER NOT NULL DEFAULT 0,
	next_due_at INTEGER NOT NULL,
	archived INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL
);
`
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	if _, err := db.Exec(oldSchema); err != nil {
		t.Fatalf("apply old schema: %v", err)
	}
	now := time.Now().Truncate(time.Second)
	if _, err := db.Exec(`INSERT INTO registrations (pr_url, session_id, provider, next_due_at, created_at) VALUES (?, ?, ?, ?, ?)`,
		"https://github.com/o/r/pull/1", "sess1", "github", now.Unix(), now.Unix()); err != nil {
		t.Fatalf("insert pre-migration row: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close pre-migration db: %v", err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open() on pre-migration db, error = %v", err)
	}
	defer func() { _ = s.Close() }()

	got, err := s.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("List() len = %d, want 1 (pre-existing row must survive migration)", len(got))
	}
	if got[0].LatestReviewState != "" {
		t.Errorf("LatestReviewState = %q, want empty default for a migrated pre-existing row", got[0].LatestReviewState)
	}

	// Opening a second time (column already present) must not error.
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("second Open() on already-migrated db, error = %v", err)
	}
	_ = s2.Close()
}

func TestOpenMigratesSchemaMissingTitle(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "old.db")

	// The realistic upgrade path: a database that already has
	// latest_review_state (the previous migration) but predates title.
	oldSchema := `
CREATE TABLE IF NOT EXISTS registrations (
	pr_url TEXT PRIMARY KEY,
	session_id TEXT NOT NULL,
	provider TEXT NOT NULL,
	review_count INTEGER NOT NULL DEFAULT 0,
	ci_bucket TEXT NOT NULL DEFAULT 'none',
	unresolved INTEGER NOT NULL DEFAULT 0,
	latest_review_state TEXT NOT NULL DEFAULT '',
	next_due_at INTEGER NOT NULL,
	archived INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL
);
`
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	if _, err := db.Exec(oldSchema); err != nil {
		t.Fatalf("apply old schema: %v", err)
	}
	now := time.Now().Truncate(time.Second)
	if _, err := db.Exec(`INSERT INTO registrations (pr_url, session_id, provider, next_due_at, created_at) VALUES (?, ?, ?, ?, ?)`,
		"https://github.com/o/r/pull/1", "sess1", "github", now.Unix(), now.Unix()); err != nil {
		t.Fatalf("insert pre-migration row: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close pre-migration db: %v", err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open() on pre-migration db, error = %v", err)
	}
	defer func() { _ = s.Close() }()

	got, err := s.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("List() len = %d, want 1 (pre-existing row must survive migration)", len(got))
	}
	if got[0].Title != "" {
		t.Errorf("Title = %q, want empty default for a migrated pre-existing row", got[0].Title)
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("second Open() on already-migrated db, error = %v", err)
	}
	_ = s2.Close()
}

func TestOpenMigratesSchemaMissingStatus(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "old.db")

	// The realistic upgrade path: a database with latest_review_state and
	// title (the two previous migrations) but predating status, from back
	// when a registration's only lifecycle was "exists" or "deleted".
	oldSchema := `
CREATE TABLE IF NOT EXISTS registrations (
	pr_url TEXT PRIMARY KEY,
	session_id TEXT NOT NULL,
	provider TEXT NOT NULL,
	review_count INTEGER NOT NULL DEFAULT 0,
	ci_bucket TEXT NOT NULL DEFAULT 'none',
	unresolved INTEGER NOT NULL DEFAULT 0,
	latest_review_state TEXT NOT NULL DEFAULT '',
	title TEXT NOT NULL DEFAULT '',
	next_due_at INTEGER NOT NULL,
	archived INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL
);
`
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	if _, err := db.Exec(oldSchema); err != nil {
		t.Fatalf("apply old schema: %v", err)
	}
	now := time.Now().Truncate(time.Second)
	if _, err := db.Exec(`INSERT INTO registrations (pr_url, session_id, provider, next_due_at, created_at) VALUES (?, ?, ?, ?, ?)`,
		"https://github.com/o/r/pull/1", "sess1", "github", now.Unix(), now.Unix()); err != nil {
		t.Fatalf("insert pre-migration row: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close pre-migration db: %v", err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open() on pre-migration db, error = %v", err)
	}
	defer func() { _ = s.Close() }()

	got, err := s.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("List() len = %d, want 1 (pre-existing row must survive migration)", len(got))
	}
	// Every row that existed before status was introduced was, by
	// definition, still actively being watched (the old schema deleted
	// terminal rows outright), so the column default must resolve it to
	// StatusWatching, never an empty/unset value.
	if got[0].Status != StatusWatching {
		t.Errorf("Status = %q, want %q for a row migrated from before status existed", got[0].Status, StatusWatching)
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("second Open() on already-migrated db, error = %v", err)
	}
	_ = s2.Close()
}

func TestOpenMigratesSchemaMissingDescription(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "old.db")

	// Realistic upgrade path: a database with every column up through
	// status, but predating description.
	oldSchema := `
CREATE TABLE IF NOT EXISTS registrations (
	pr_url TEXT PRIMARY KEY,
	session_id TEXT NOT NULL,
	provider TEXT NOT NULL,
	review_count INTEGER NOT NULL DEFAULT 0,
	ci_bucket TEXT NOT NULL DEFAULT 'none',
	unresolved INTEGER NOT NULL DEFAULT 0,
	latest_review_state TEXT NOT NULL DEFAULT '',
	title TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL DEFAULT 'watching',
	next_due_at INTEGER NOT NULL,
	archived INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL
);
`
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	if _, err := db.Exec(oldSchema); err != nil {
		t.Fatalf("apply old schema: %v", err)
	}
	now := time.Now().Truncate(time.Second)
	if _, err := db.Exec(`INSERT INTO registrations (pr_url, session_id, provider, next_due_at, created_at) VALUES (?, ?, ?, ?, ?)`,
		"https://github.com/o/r/pull/1", "sess1", "github", now.Unix(), now.Unix()); err != nil {
		t.Fatalf("insert pre-migration row: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close pre-migration db: %v", err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open() on pre-migration db, error = %v", err)
	}
	defer func() { _ = s.Close() }()

	got, err := s.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("List() len = %d, want 1 (pre-existing row must survive migration)", len(got))
	}
	if got[0].Description != "" {
		t.Errorf("Description = %q, want empty default for a migrated pre-existing row", got[0].Description)
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("second Open() on already-migrated db, error = %v", err)
	}
	_ = s2.Close()
}

// TestOpenMigratesToCompositePrimaryKeyAllowingMultipleSessions is the
// regression test for the live bug this migration fixes: a pre-existing
// database still has the old pr_url-only primary key (plus the vestigial
// `unresolved` column no real production database has ever had dropped),
// which must survive migration with its data intact, and — the actual
// point of the composite key — a second session must be able to register
// its own independent watch on a pr_url a first session already owns,
// something the old single-column key made structurally impossible.
func TestOpenMigratesToCompositePrimaryKeyAllowingMultipleSessions(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "old.db")

	oldSchema := `
CREATE TABLE IF NOT EXISTS registrations (
	pr_url TEXT PRIMARY KEY,
	session_id TEXT NOT NULL,
	provider TEXT NOT NULL,
	review_count INTEGER NOT NULL DEFAULT 0,
	ci_bucket TEXT NOT NULL DEFAULT 'none',
	unresolved INTEGER NOT NULL DEFAULT 0,
	latest_review_state TEXT NOT NULL DEFAULT '',
	title TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL DEFAULT 'watching',
	description TEXT NOT NULL DEFAULT '',
	ci_detail TEXT NOT NULL DEFAULT '',
	latest_reviewer TEXT NOT NULL DEFAULT '',
	latest_review_at INTEGER NOT NULL DEFAULT 0,
	merged_at INTEGER NOT NULL DEFAULT 0,
	has_conflict INTEGER NOT NULL DEFAULT 0,
	ci_href TEXT NOT NULL DEFAULT '',
	review_href TEXT NOT NULL DEFAULT '',
	merge_queue_state TEXT NOT NULL DEFAULT '',
	merge_queue_position INTEGER NOT NULL DEFAULT 0,
	blocked INTEGER NOT NULL DEFAULT 0,
	unresolved_thread_ids TEXT NOT NULL DEFAULT '[]',
	next_due_at INTEGER NOT NULL,
	archived INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL
);
`
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	if _, err := db.Exec(oldSchema); err != nil {
		t.Fatalf("apply old schema: %v", err)
	}
	now := time.Now().Truncate(time.Second)
	if _, err := db.Exec(`INSERT INTO registrations (pr_url, session_id, provider, unresolved, next_due_at, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		"https://github.com/o/r/pull/1", "sess1", "github", 2, now.Unix(), now.Unix()); err != nil {
		t.Fatalf("insert pre-migration row: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close pre-migration db: %v", err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open() on pre-migration db, error = %v", err)
	}
	defer func() { _ = s.Close() }()

	got, err := s.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(got) != 1 || got[0].SessionID != "sess1" {
		t.Fatalf("List() = %+v, want the pre-existing sess1 row to survive migration", got)
	}

	// The actual regression: a second session registering the exact same
	// pr_url an existing (even long-dead) row already owns must succeed as
	// its own independent row, not be silently rejected or merged into the
	// first session's row the way the old single-column key forced.
	if err := s.Add(ctx, Registration{
		PRURL: "https://github.com/o/r/pull/1", SessionID: "sess2", Provider: "github", NextDueAt: now, CreatedAt: now,
	}); err != nil {
		t.Fatalf("Add() for a second session on an already-registered pr_url, error = %v", err)
	}

	got, err = s.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("List() len = %d, want 2 (both sessions' independent registrations for the same pr_url)", len(got))
	}
	sessions := map[string]bool{got[0].SessionID: true, got[1].SessionID: true}
	if !sessions["sess1"] || !sessions["sess2"] {
		t.Errorf("List() sessions = %v, want both %q and %q", sessions, "sess1", "sess2")
	}
}

func TestCountByProvider(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	now := time.Now()
	regs := []Registration{
		{PRURL: "https://github.com/o/r/pull/1", SessionID: "a", Provider: "github", NextDueAt: now, CreatedAt: now},
		{PRURL: "https://github.com/o/r/pull/2", SessionID: "b", Provider: "github", NextDueAt: now, CreatedAt: now},
		{PRURL: "https://gitlab.example.com/g/p/-/merge_requests/1", SessionID: "c", Provider: "gitlab", NextDueAt: now, CreatedAt: now},
		{PRURL: "https://github.com/o/r/pull/3", SessionID: "d", Provider: "github", NextDueAt: now, CreatedAt: now},
	}
	for _, r := range regs {
		if err := s.Add(ctx, r); err != nil {
			t.Fatalf("Add() error = %v", err)
		}
	}
	if err := s.SetStatus(ctx, "https://github.com/o/r/pull/3", StatusMerged); err != nil {
		t.Fatalf("SetStatus() error = %v", err)
	}

	got, err := s.CountByProvider(ctx)
	if err != nil {
		t.Fatalf("CountByProvider() error = %v", err)
	}
	if got["github"] != 2 || got["gitlab"] != 1 {
		t.Errorf("CountByProvider() = %v, want {github:2, gitlab:1} (the merged row must not count toward the rate-limit budget spread)", got)
	}
}

func TestSetArchived(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	now := time.Now()
	reg := Registration{PRURL: "https://github.com/o/r/pull/1", SessionID: "a", Provider: "github", NextDueAt: now, CreatedAt: now}
	if err := s.Add(ctx, reg); err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	if err := s.SetArchived(ctx, reg.PRURL, true); err != nil {
		t.Fatalf("SetArchived() error = %v", err)
	}
	got, err := s.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if !got[0].Archived {
		t.Errorf("Archived = false, want true")
	}
}

func TestSetStatusOwnedByMatchingSession(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	now := time.Now()
	reg := Registration{PRURL: "https://github.com/o/r/pull/1", SessionID: "owner", Provider: "github", NextDueAt: now, CreatedAt: now}
	if err := s.Add(ctx, reg); err != nil {
		t.Fatalf("Add() error = %v", err)
	}

	ok, err := s.SetStatusOwnedBy(ctx, reg.PRURL, "owner", StatusStopped)
	if err != nil {
		t.Fatalf("SetStatusOwnedBy() error = %v", err)
	}
	if !ok {
		t.Error("SetStatusOwnedBy() = false, want true for the owning session")
	}
	got, err := s.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(got) != 1 || got[0].Status != StatusStopped {
		t.Errorf("List() = %+v, want status %q (the row must survive, not be deleted)", got, StatusStopped)
	}
}

func TestSetStatusOwnedByWrongSessionLeavesItAlone(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	now := time.Now()
	reg := Registration{PRURL: "https://github.com/o/r/pull/1", SessionID: "owner", Provider: "github", NextDueAt: now, CreatedAt: now}
	if err := s.Add(ctx, reg); err != nil {
		t.Fatalf("Add() error = %v", err)
	}

	ok, err := s.SetStatusOwnedBy(ctx, reg.PRURL, "someone-else", StatusStopped)
	if err != nil {
		t.Fatalf("SetStatusOwnedBy() error = %v", err)
	}
	if ok {
		t.Error("SetStatusOwnedBy() = true, want false for a non-owning session")
	}
	got, err := s.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(got) != 1 || got[0].Status != StatusWatching {
		t.Errorf("List() = %+v, want the owner's registration untouched at %q", got, StatusWatching)
	}
}

func TestSetStatusOwnedByUnknownURL(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	ok, err := s.SetStatusOwnedBy(ctx, "https://github.com/o/r/pull/404", "anyone", StatusStopped)
	if err != nil {
		t.Fatalf("SetStatusOwnedBy() error = %v", err)
	}
	if ok {
		t.Error("SetStatusOwnedBy() on an unregistered URL = true, want false")
	}
}

func TestClearReviewIfCurrentBlanksMatchingReview(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	now := time.Now()
	reg := Registration{
		PRURL: "https://github.com/o/r/pull/1", SessionID: "a", Provider: "github",
		NextDueAt: now, CreatedAt: now,
	}
	if err := s.Add(ctx, reg); err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	reviewHref := "https://github.com/o/r/pull/1#pullrequestreview-1"
	update := StateUpdate{LatestReviewState: "CHANGES_REQUESTED", LatestReviewer: "bob", ReviewHref: reviewHref, NextDueAt: now}
	if err := s.UpdateState(ctx, reg.PRURL, reg.SessionID, update); err != nil {
		t.Fatalf("UpdateState() error = %v", err)
	}

	if err := s.ClearReviewIfCurrent(ctx, reg.PRURL, reg.SessionID, reviewHref); err != nil {
		t.Fatalf("ClearReviewIfCurrent() error = %v", err)
	}

	got, ok, err := s.Get(ctx, reg.PRURL, reg.SessionID)
	if err != nil || !ok {
		t.Fatalf("Get() = (%+v, %v, %v)", got, ok, err)
	}
	if got.LatestReviewState != "" || got.LatestReviewer != "" || got.ReviewHref != "" || !got.LatestReviewAt.IsZero() {
		t.Errorf("Get() = %+v, want every latest-review field blanked", got)
	}
	if got.DismissedReviewHref != "" {
		t.Errorf("DismissedReviewHref = %q, want empty (ClearReviewIfCurrent sets no marker)", got.DismissedReviewHref)
	}
}

func TestClearReviewIfCurrentIgnoresStaleHref(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	now := time.Now()
	reg := Registration{
		PRURL: "https://github.com/o/r/pull/1", SessionID: "a", Provider: "github",
		NextDueAt: now, CreatedAt: now,
	}
	if err := s.Add(ctx, reg); err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	currentHref := "https://github.com/o/r/pull/1#pullrequestreview-2"
	update := StateUpdate{LatestReviewState: "APPROVED", LatestReviewer: "alice", ReviewHref: currentHref, NextDueAt: now}
	if err := s.UpdateState(ctx, reg.PRURL, reg.SessionID, update); err != nil {
		t.Fatalf("UpdateState() error = %v", err)
	}

	if err := s.ClearReviewIfCurrent(ctx, reg.PRURL, reg.SessionID, "https://github.com/o/r/pull/1#pullrequestreview-1"); err != nil {
		t.Fatalf("ClearReviewIfCurrent() error = %v", err)
	}

	got, ok, err := s.Get(ctx, reg.PRURL, reg.SessionID)
	if err != nil || !ok {
		t.Fatalf("Get() = (%+v, %v, %v)", got, ok, err)
	}
	if got.LatestReviewState != "APPROVED" || got.ReviewHref != currentHref {
		t.Errorf("Get() = %+v, want the current review untouched since href didn't match", got)
	}
}

func TestSetDismissedReviewMarksAndBlanksWhenCurrent(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	now := time.Now()
	reg := Registration{
		PRURL: "https://github.com/o/r/pull/1", SessionID: "a", Provider: "github",
		NextDueAt: now, CreatedAt: now,
	}
	if err := s.Add(ctx, reg); err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	reviewHref := "https://github.com/o/r/pull/1#pullrequestreview-1"
	update := StateUpdate{LatestReviewState: "COMMENTED", LatestReviewer: "bob", ReviewHref: reviewHref, NextDueAt: now}
	if err := s.UpdateState(ctx, reg.PRURL, reg.SessionID, update); err != nil {
		t.Fatalf("UpdateState() error = %v", err)
	}

	if err := s.SetDismissedReview(ctx, reg.PRURL, reg.SessionID, reviewHref); err != nil {
		t.Fatalf("SetDismissedReview() error = %v", err)
	}

	got, ok, err := s.Get(ctx, reg.PRURL, reg.SessionID)
	if err != nil || !ok {
		t.Fatalf("Get() = (%+v, %v, %v)", got, ok, err)
	}
	if got.DismissedReviewHref != reviewHref {
		t.Errorf("DismissedReviewHref = %q, want %q", got.DismissedReviewHref, reviewHref)
	}
	if got.LatestReviewState != "" || got.LatestReviewer != "" || got.ReviewHref != "" || !got.LatestReviewAt.IsZero() {
		t.Errorf("Get() = %+v, want every latest-review field blanked since href matched the current review", got)
	}
}

func TestSetDismissedReviewMarksOnlyWhenNotCurrent(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	now := time.Now()
	reg := Registration{
		PRURL: "https://github.com/o/r/pull/1", SessionID: "a", Provider: "github",
		NextDueAt: now, CreatedAt: now,
	}
	if err := s.Add(ctx, reg); err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	currentHref := "https://github.com/o/r/pull/1#pullrequestreview-2"
	update := StateUpdate{LatestReviewState: "CHANGES_REQUESTED", LatestReviewer: "carol", ReviewHref: currentHref, NextDueAt: now}
	if err := s.UpdateState(ctx, reg.PRURL, reg.SessionID, update); err != nil {
		t.Fatalf("UpdateState() error = %v", err)
	}

	olderHref := "https://github.com/o/r/pull/1#pullrequestreview-1"
	if err := s.SetDismissedReview(ctx, reg.PRURL, reg.SessionID, olderHref); err != nil {
		t.Fatalf("SetDismissedReview() error = %v", err)
	}

	got, ok, err := s.Get(ctx, reg.PRURL, reg.SessionID)
	if err != nil || !ok {
		t.Fatalf("Get() = (%+v, %v, %v)", got, ok, err)
	}
	if got.DismissedReviewHref != olderHref {
		t.Errorf("DismissedReviewHref = %q, want %q", got.DismissedReviewHref, olderHref)
	}
	if got.LatestReviewState != "CHANGES_REQUESTED" || got.ReviewHref != currentHref {
		t.Errorf("Get() = %+v, want the currently-displayed review left untouched", got)
	}
}

func TestStopAllOwnedByStopsOnlyThatSessionsNonStopped(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	now := time.Now()

	regs := []Registration{
		{PRURL: "https://github.com/o/r/pull/1", SessionID: "sess1", Provider: "github", NextDueAt: now, CreatedAt: now}, // watching, will be stopped
		{PRURL: "https://github.com/o/r/pull/2", SessionID: "sess1", Provider: "github", NextDueAt: now, CreatedAt: now}, // will become merged, will be stopped
		{PRURL: "https://github.com/o/r/pull/3", SessionID: "sess2", Provider: "github", NextDueAt: now, CreatedAt: now}, // other session, must stay untouched
	}
	for _, r := range regs {
		if err := s.Add(ctx, r); err != nil {
			t.Fatalf("Add() error = %v", err)
		}
	}
	if err := s.SetStatus(ctx, regs[1].PRURL, StatusMerged); err != nil {
		t.Fatalf("SetStatus() error = %v", err)
	}

	count, err := s.StopAllOwnedBy(ctx, "sess1")
	if err != nil {
		t.Fatalf("StopAllOwnedBy() error = %v", err)
	}
	if count != 2 {
		t.Errorf("count = %d, want 2", count)
	}

	got := map[string]string{}
	all, err := s.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	for _, r := range all {
		got[r.PRURL] = r.Status
	}
	if got[regs[0].PRURL] != StatusStopped {
		t.Errorf("pull/1 status = %q, want %q", got[regs[0].PRURL], StatusStopped)
	}
	if got[regs[1].PRURL] != StatusStopped {
		t.Errorf("pull/2 status = %q, want %q", got[regs[1].PRURL], StatusStopped)
	}
	if got[regs[2].PRURL] != StatusWatching {
		t.Errorf("pull/3 status = %q, want %q (a different session's registration must be untouched)", got[regs[2].PRURL], StatusWatching)
	}
}

func TestStopAllOwnedByIsIdempotent(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	now := time.Now()
	reg := Registration{PRURL: "https://github.com/o/r/pull/1", SessionID: "sess1", Provider: "github", NextDueAt: now, CreatedAt: now}
	if err := s.Add(ctx, reg); err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	if _, err := s.StopAllOwnedBy(ctx, "sess1"); err != nil {
		t.Fatalf("StopAllOwnedBy() error = %v", err)
	}

	count, err := s.StopAllOwnedBy(ctx, "sess1")
	if err != nil {
		t.Fatalf("StopAllOwnedBy() second call error = %v", err)
	}
	if count != 0 {
		t.Errorf("count = %d, want 0 (already stopped, nothing left to change)", count)
	}
}

// A registration that predates thread-comment tracking has no baseline;
// the first stored state (even an empty one) is the baseline from then on.
func TestThreadCommentBaselineIsUnknownUntilFirstUpdate(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	now := time.Now().Truncate(time.Second)
	reg := Registration{PRURL: "https://github.com/o/r/pull/1", SessionID: "a", Provider: "github", NextDueAt: now, CreatedAt: now}
	if err := s.Add(ctx, reg); err != nil {
		t.Fatalf("Add() error = %v", err)
	}

	got, _, _ := s.Get(ctx, reg.PRURL, "a")
	if got.ThreadCommentsKnown || len(got.ThreadCommentIDs) != 0 {
		t.Errorf("fresh registration = %+v, want an unknown thread-comment baseline", got)
	}

	if err := s.UpdateState(ctx, reg.PRURL, "a", StateUpdate{ThreadCommentIDs: []string{"c1", "c2"}, ThreadCommentsKnown: true, NextDueAt: now}); err != nil {
		t.Fatalf("UpdateState() error = %v", err)
	}
	got, _, _ = s.Get(ctx, reg.PRURL, "a")
	if !got.ThreadCommentsKnown || !slices.Equal(got.ThreadCommentIDs, []string{"c1", "c2"}) {
		t.Errorf("after update = %+v, want known baseline [c1 c2]", got)
	}

	if err := s.UpdateState(ctx, reg.PRURL, "a", StateUpdate{ThreadCommentsKnown: true, NextDueAt: now}); err != nil {
		t.Fatalf("UpdateState() error = %v", err)
	}
	got, _, _ = s.Get(ctx, reg.PRURL, "a")
	if !got.ThreadCommentsKnown || len(got.ThreadCommentIDs) != 0 {
		t.Errorf("after an empty update = %+v, want a known, empty baseline (no unresolved threads is a real state)", got)
	}
	all, _ := s.List(ctx)
	if !all[0].ThreadCommentsKnown {
		t.Errorf("List() lost the known flag: %+v", all[0])
	}

	// A reschedule-only update of a row that never had a baseline must not
	// invent one.
	other := Registration{PRURL: "https://github.com/o/r/pull/2", SessionID: "a", Provider: "github", NextDueAt: now, CreatedAt: now}
	if err := s.Add(ctx, other); err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	if err := s.UpdateState(ctx, other.PRURL, "a", StateUpdate{NextDueAt: now}); err != nil {
		t.Fatalf("UpdateState() error = %v", err)
	}
	if got, _, _ := s.Get(ctx, other.PRURL, "a"); got.ThreadCommentsKnown {
		t.Errorf("got = %+v, want the baseline to stay unknown", got)
	}
}

// Finished PRs stay due for FinishedWatchWindow after they finish, so late
// comments and a reopen are noticed; after that they are left alone.
func TestDueBeforeIncludesRecentlyFinishedRegistrations(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	now := time.Now().Truncate(time.Second)
	past := now.Add(-time.Minute)

	add := func(n int, status string, finished time.Time) string {
		url := fmt.Sprintf("https://github.com/o/r/pull/%d", n)
		if err := s.Add(ctx, Registration{PRURL: url, SessionID: "a", Provider: "github", NextDueAt: past, CreatedAt: now}); err != nil {
			t.Fatalf("Add() error = %v", err)
		}
		switch status {
		case StatusMerged:
			if err := s.SetMerged(ctx, url, finished); err != nil {
				t.Fatal(err)
			}
			if finished.IsZero() { // a legacy row from before merge dates were recorded
				if _, err := s.db.Exec(`UPDATE registrations SET merged_at = 0 WHERE pr_url = ?`, url); err != nil {
					t.Fatal(err)
				}
			}
		case StatusClosed, StatusStopped:
			if err := s.SetStatus(ctx, url, status); err != nil {
				t.Fatal(err)
			}
			if status == StatusClosed {
				if _, err := s.db.Exec(`UPDATE registrations SET closed_at = ? WHERE pr_url = ?`, finished.Unix(), url); err != nil {
					t.Fatal(err)
				}
			}
		}
		return url
	}
	watching := add(1, StatusWatching, time.Time{})
	mergedRecent := add(2, StatusMerged, now.Add(-24*time.Hour))
	mergedOld := add(3, StatusMerged, now.Add(-31*24*time.Hour))
	mergedUnknown := add(4, StatusMerged, time.Time{})
	closedRecent := add(5, StatusClosed, now.Add(-24*time.Hour))
	closedOld := add(6, StatusClosed, now.Add(-31*24*time.Hour))
	stopped := add(7, StatusStopped, time.Time{})

	due, err := s.DueBefore(ctx, now)
	if err != nil {
		t.Fatalf("DueBefore() error = %v", err)
	}
	got := map[string]bool{}
	for _, r := range due {
		got[r.PRURL] = true
	}
	for _, want := range []string{watching, mergedRecent, closedRecent} {
		if !got[want] {
			t.Errorf("%s not due, want it polled", want)
		}
	}
	for _, unwanted := range []string{mergedOld, mergedUnknown, closedOld, stopped} {
		if got[unwanted] {
			t.Errorf("%s is due, want it left alone (outside the window, undated, or stopped)", unwanted)
		}
	}

	counts, err := s.CountByProvider(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if counts["github"] != 3 {
		t.Errorf("CountByProvider()[github] = %d, want 3 (the polled ones, so the rate budget is shared over them)", counts["github"])
	}
}

func TestReopenedRegistrationIsWatchedAgain(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	now := time.Now().Truncate(time.Second)
	reg := Registration{PRURL: "https://github.com/o/r/pull/1", SessionID: "a", Provider: "github", NextDueAt: now.Add(-time.Minute), CreatedAt: now}
	if err := s.Add(ctx, reg); err != nil {
		t.Fatal(err)
	}
	if err := s.SetStatus(ctx, reg.PRURL, StatusClosed); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.SetStatusOwnedBy(ctx, reg.PRURL, "a", StatusWatching); err != nil || !ok {
		t.Fatalf("SetStatusOwnedBy() = %v, %v", ok, err)
	}
	if got, _, _ := s.Get(ctx, reg.PRURL, "a"); got.Status != StatusWatching {
		t.Errorf("Status = %q, want watching", got.Status)
	}
}

func TestSilentFirstPollIsSetOnInsertOnlyAndCleared(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	base := Registration{PRURL: "https://github.com/o/r/pull/1", SessionID: "s1", Provider: "github", NextDueAt: time.Now(), CreatedAt: time.Now()}

	silent, err := s.FirstPollSilent(ctx, base.PRURL, base.SessionID)
	if err != nil || silent {
		t.Fatalf("FirstPollSilent() before any row = (%v, %v), want (false, nil)", silent, err)
	}

	quiet := base
	quiet.SilentFirstPoll = true
	if err := s.Add(ctx, quiet); err != nil {
		t.Fatal(err)
	}
	if silent, _ := s.FirstPollSilent(ctx, base.PRURL, base.SessionID); !silent {
		t.Error("FirstPollSilent() after Add(SilentFirstPoll) = false, want true")
	}

	// Re-registering must not touch the flag, either way.
	if err := s.Add(ctx, base); err != nil {
		t.Fatal(err)
	}
	if silent, _ := s.FirstPollSilent(ctx, base.PRURL, base.SessionID); !silent {
		t.Error("re-Add without the flag cleared it; only the first poll may do that")
	}

	if err := s.ClearFirstPollSilent(ctx, base.PRURL, base.SessionID); err != nil {
		t.Fatal(err)
	}
	if silent, _ := s.FirstPollSilent(ctx, base.PRURL, base.SessionID); silent {
		t.Error("FirstPollSilent() after Clear = true, want false")
	}
	loud := quiet
	if err := s.Add(ctx, loud); err != nil {
		t.Fatal(err)
	}
	if silent, _ := s.FirstPollSilent(ctx, base.PRURL, base.SessionID); silent {
		t.Error("re-Add with the flag re-armed a polled registration")
	}
}
