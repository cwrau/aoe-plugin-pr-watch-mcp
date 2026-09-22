package registry

import (
	"context"
	"database/sql"
	"path/filepath"
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

func TestAddIsIdempotentOnConflict(t *testing.T) {
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
	if len(got) != 1 {
		t.Fatalf("List() len = %d, want 1 (re-register should upsert, not duplicate)", len(got))
	}
	if got[0].SessionID != "second" {
		t.Errorf("List()[0].SessionID = %q, want %q", got[0].SessionID, "second")
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
		ReviewCount:       3,
		CIBucket:          "failure",
		CIDetail:          "build",
		Unresolved:        2,
		LatestReviewState: "CHANGES_REQUESTED",
		LatestReviewer:    "bob",
		LatestReviewAt:    reviewAt,
		Title:             "Fix the thing",
		Description:       "Fixes the widget rendering bug.",
		NextDueAt:         newDue,
	}
	if err := s.UpdateState(ctx, reg.PRURL, update); err != nil {
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
	if r.ReviewCount != 3 || r.CIBucket != "failure" || r.CIDetail != "build" || r.Unresolved != 2 || r.LatestReviewState != "CHANGES_REQUESTED" || r.LatestReviewer != "bob" || !r.LatestReviewAt.Equal(reviewAt) || r.Title != "Fix the thing" || r.Description != "Fixes the widget rendering bug." || !r.NextDueAt.Equal(newDue) {
		t.Errorf("after UpdateState, got %+v", r)
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

func TestStopOtherMergedStopsOnlyMergedForThatSession(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	now := time.Now()

	regs := []Registration{
		{PRURL: "https://github.com/o/r/pull/1", SessionID: "sess1", Provider: "github", NextDueAt: now, CreatedAt: now}, // will become merged
		{PRURL: "https://github.com/o/r/pull/2", SessionID: "sess1", Provider: "github", NextDueAt: now, CreatedAt: now}, // new registration, kept
		{PRURL: "https://github.com/o/r/pull/3", SessionID: "sess1", Provider: "github", NextDueAt: now, CreatedAt: now}, // will become watching, must stay untouched
		{PRURL: "https://github.com/o/r/pull/4", SessionID: "sess2", Provider: "github", NextDueAt: now, CreatedAt: now}, // other session, will become merged, must stay untouched
	}
	for _, r := range regs {
		if err := s.Add(ctx, r); err != nil {
			t.Fatalf("Add() error = %v", err)
		}
	}
	if err := s.SetStatus(ctx, regs[0].PRURL, StatusMerged); err != nil {
		t.Fatalf("SetStatus() error = %v", err)
	}
	if err := s.SetStatus(ctx, regs[3].PRURL, StatusMerged); err != nil {
		t.Fatalf("SetStatus() error = %v", err)
	}

	if err := s.StopOtherMerged(ctx, "sess1", regs[1].PRURL); err != nil {
		t.Fatalf("StopOtherMerged() error = %v", err)
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
		t.Errorf("pull/1 status = %q, want %q (was merged, different PR, same session)", got[regs[0].PRURL], StatusStopped)
	}
	if got[regs[1].PRURL] != StatusWatching {
		t.Errorf("pull/2 status = %q, want %q (the newly-registered PR itself, must not be stopped)", got[regs[1].PRURL], StatusWatching)
	}
	if got[regs[2].PRURL] != StatusWatching {
		t.Errorf("pull/3 status = %q, want %q (already-watching, unrelated to merged cleanup)", got[regs[2].PRURL], StatusWatching)
	}
	if got[regs[3].PRURL] != StatusMerged {
		t.Errorf("pull/4 status = %q, want %q (a different session's merged PR must be untouched)", got[regs[3].PRURL], StatusMerged)
	}
}
