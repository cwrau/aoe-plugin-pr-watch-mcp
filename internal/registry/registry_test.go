package registry

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { s.Close() })
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

func TestRemove(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	now := time.Now()
	reg := Registration{PRURL: "https://github.com/o/r/pull/1", SessionID: "abc", Provider: "github", NextDueAt: now, CreatedAt: now}
	if err := s.Add(ctx, reg); err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	if err := s.Remove(ctx, reg.PRURL); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
	got, err := s.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(got) != 0 {
		t.Errorf("List() len = %d, want 0 after Remove", len(got))
	}
}

func TestDueBefore(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	base := time.Now().Truncate(time.Second)

	early := Registration{PRURL: "https://github.com/o/r/pull/1", SessionID: "a", Provider: "github", NextDueAt: base.Add(-time.Hour), CreatedAt: base}
	late := Registration{PRURL: "https://github.com/o/r/pull/2", SessionID: "b", Provider: "github", NextDueAt: base.Add(time.Hour), CreatedAt: base}
	if err := s.Add(ctx, early); err != nil {
		t.Fatalf("Add(early) error = %v", err)
	}
	if err := s.Add(ctx, late); err != nil {
		t.Fatalf("Add(late) error = %v", err)
	}

	due, err := s.DueBefore(ctx, base)
	if err != nil {
		t.Fatalf("DueBefore() error = %v", err)
	}
	if len(due) != 1 || due[0].PRURL != early.PRURL {
		t.Errorf("DueBefore(%v) = %+v, want only %q", base, due, early.PRURL)
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
	if err := s.UpdateState(ctx, reg.PRURL, 3, "failure", 2, newDue); err != nil {
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
	if r.ReviewCount != 3 || r.CIBucket != "failure" || r.Unresolved != 2 || !r.NextDueAt.Equal(newDue) {
		t.Errorf("after UpdateState, got %+v", r)
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
	}
	for _, r := range regs {
		if err := s.Add(ctx, r); err != nil {
			t.Fatalf("Add() error = %v", err)
		}
	}

	got, err := s.CountByProvider(ctx)
	if err != nil {
		t.Fatalf("CountByProvider() error = %v", err)
	}
	if got["github"] != 2 || got["gitlab"] != 1 {
		t.Errorf("CountByProvider() = %v, want {github:2, gitlab:1}", got)
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
