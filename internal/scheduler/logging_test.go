package scheduler

import (
	"bytes"
	"context"
	"log/slog"
	"slices"
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

// A silent first poll takes whatever the PR already shows as the baseline:
// no ping, state saved, and the flag cleared so the next poll reports changes.
func TestSilentFirstPollSavesBaselineWithoutPinging(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	now := time.Now()

	reg := registry.Registration{
		PRURL: "https://github.com/o/r/pull/1", SessionID: "sess1", Provider: "github",
		CIBucket: "none", NextDueAt: now.Add(-time.Second), CreatedAt: now, SilentFirstPoll: true,
	}
	if err := store.Add(ctx, reg); err != nil {
		t.Fatal(err)
	}
	gh := &fakeGitHubChecker{results: map[string]provider.CheckResult{
		reg.PRURL: {State: "open", CIBucket: "failure", TopLevelCommentIDs: []string{"c1"}, HasConflict: true},
	}, budgets: provider.GitHubBudgets{
		GraphQL: provider.RateBudget{Limit: 5000, Remaining: 4999, ResetAt: now.Add(time.Hour)},
		REST:    provider.RateBudget{Limit: 5000, Remaining: 4999, ResetAt: now.Add(time.Hour)},
	}}
	notify := &fakeNotifier{}
	var buf bytes.Buffer
	sched := &Scheduler{Store: store, GitHub: gh, Sessions: alwaysLiveSessions(), Notify: notify, Log: slog.New(slog.NewTextHandler(&buf, nil))}

	if err := sched.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}

	if len(notify.sent) != 0 {
		t.Errorf("notify.sent = %q, want nothing for a silent first poll", notify.sent)
	}
	if !strings.Contains(buf.String(), "reason=baseline") {
		t.Errorf("log lacks the baseline suppression reason:\n%s", buf.String())
	}
	got := getReg(t, store, reg.PRURL, "sess1")
	if got.CIBucket != "failure" || !got.HasConflict || !slices.Equal(got.TopLevelCommentIDs, []string{"c1"}) {
		t.Errorf("stored state = %+v, want the first poll recorded as the baseline", got)
	}
	if silent, _ := store.FirstPollSilent(ctx, reg.PRURL, "sess1"); silent {
		t.Error("the silent flag is still set after the first poll")
	}
}

// When the worker cannot tell who it is, self-authored pings get through, so
// the failure must be visible in the log rather than swallowed at debug level.
func TestSelfIdentityFailureIsLoggedAsAWarning(t *testing.T) {
	store := openTestStore(t)
	var buf bytes.Buffer
	sched := &Scheduler{
		Store: store, GitHub: &fakeGitHubChecker{selfLoginErr: context.DeadlineExceeded},
		GitLab: &fakeGitLabChecker{}, Sessions: alwaysLiveSessions(), Notify: &fakeNotifier{},
		Log: slog.New(slog.NewTextHandler(&buf, nil)),
	}
	if got := sched.selfAuthor(context.Background(), registry.Registration{Provider: "github", PRURL: "https://github.com/o/r/pull/1"}); got != "" {
		t.Errorf("selfAuthor() = %q, want empty on failure", got)
	}
	if out := buf.String(); !strings.Contains(out, "level=WARN") || !strings.Contains(out, "own account") {
		t.Errorf("log = %q, want a warning that the worker's own account could not be resolved", out)
	}
}
