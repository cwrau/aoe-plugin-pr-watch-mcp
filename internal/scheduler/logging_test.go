package scheduler

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/cwrau/aoe-plugin-pr-watch-mcp/internal/provider"
	"github.com/cwrau/aoe-plugin-pr-watch-mcp/internal/registry"
)

// The worker's stderr is the only record of what it did, so every outcome of
// a poll that produced events must be in it: what was sent to whom, what the
// filter held back, and what failed.
func TestTickLogsSentSuppressedAndFailedPings(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	now := time.Now()

	regs := []registry.Registration{
		{PRURL: "https://github.com/o/r/pull/1", SessionID: "sess-sent", Provider: "github", CIBucket: "pending", NextDueAt: now.Add(-3 * time.Second), CreatedAt: now},
		{PRURL: "https://github.com/o/r/pull/2", SessionID: "sess-quiet", Provider: "github", CIBucket: "pending", NextDueAt: now.Add(-2 * time.Second), CreatedAt: now, EventFilter: []string{"MERGED"}},
		{PRURL: "https://github.com/o/r/pull/3", SessionID: "sess-broken", Provider: "github", CIBucket: "pending", NextDueAt: now.Add(-time.Second), CreatedAt: now},
	}
	results := map[string]provider.CheckResult{}
	for _, r := range regs {
		if err := store.Add(ctx, r); err != nil {
			t.Fatalf("Add() error = %v", err)
		}
		results[r.PRURL] = provider.CheckResult{State: "open", CIBucket: "failure"}
	}
	gh := &fakeGitHubChecker{results: results, budgets: provider.GitHubBudgets{
		GraphQL: provider.RateBudget{Limit: 5000, Remaining: 4999, ResetAt: now.Add(time.Hour)},
		REST:    provider.RateBudget{Limit: 5000, Remaining: 4999, ResetAt: now.Add(time.Hour)},
	}}

	var buf bytes.Buffer
	notify := &perSessionFailNotifier{failFor: "sess-broken"}
	sched := &Scheduler{
		Store: store, GitHub: gh, Sessions: alwaysLiveSessions(), Notify: notify,
		Log: slog.New(slog.NewTextHandler(&buf, nil)),
	}
	if err := sched.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}

	out := buf.String()
	for _, want := range []string{
		"ping sent", "session_id=sess-sent",
		"ping suppressed", "session_id=sess-quiet", "reason=filter",
		"notify failed", "session_id=sess-broken",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q:\n%s", want, out)
		}
	}
	if strings.Count(out, "ping sent") != 1 {
		t.Errorf("only sess-sent should log a successful send:\n%s", out)
	}
}

type perSessionFailNotifier struct {
	failFor string
}

func (n *perSessionFailNotifier) Send(_ context.Context, sessionID, _ string) error {
	if sessionID == n.failFor {
		return context.DeadlineExceeded
	}
	return nil
}
