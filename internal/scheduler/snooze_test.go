package scheduler

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/cwrau/aoe-plugin-pr-watch-mcp/internal/provider"
	"github.com/cwrau/aoe-plugin-pr-watch-mcp/internal/registry"
)

func TestGlobMatch(t *testing.T) {
	tests := []struct {
		pattern, s string
		want       bool
	}{
		{"gitlab.corp.example/*", "gitlab.corp.example/group/proj/-/merge_requests/1", true},
		{"gitlab.corp.example/*", "gitlab.example.com/group/proj/-/merge_requests/1", false},
		{"github.com/org/*", "github.com/org/repo/pull/1", true},
		{"github.com/org/*", "github.com/other/repo/pull/1", false},
		{"github.com/org/repo/pull/1", "github.com/org/repo/pull/1", true},
		{"github.com/org/repo/pull/1", "github.com/org/repo/pull/12", false},
		{"*", "anything/at/all", true},
		{"github.com/*/infra/*", "github.com/org/infra/pull/3", true},
		{"github.com/*/infra/*", "github.com/org/other/pull/3", false},
		{"", "github.com/org/repo/pull/1", false},
		{"github.com/org/repo/pull/?", "github.com/org/repo/pull/7", true},
		{"github.com/org/repo/pull/?", "github.com/org/repo/pull/77", false},
	}
	for _, tt := range tests {
		if got := globMatch(tt.pattern, tt.s); got != tt.want {
			t.Errorf("globMatch(%q, %q) = %v, want %v", tt.pattern, tt.s, got, tt.want)
		}
	}
}

func TestMatchesSnoozeURLIgnoresSchemeAndCase(t *testing.T) {
	tests := []struct {
		patterns []string
		url      string
		want     bool
	}{
		{[]string{"github.com/org/*"}, "https://github.com/Org/Repo/pull/1", true},
		{[]string{"https://github.com/org/*"}, "https://github.com/org/repo/pull/1", true},
		{[]string{"gitlab.corp.example/*", "github.com/org/*"}, "https://github.com/org/repo/pull/1", true},
		{[]string{"gitlab.corp.example/*"}, "https://github.com/org/repo/pull/1", false},
		{nil, "https://github.com/org/repo/pull/1", false},
	}
	for _, tt := range tests {
		if got := matchesSnooze(tt.patterns, tt.url); got != tt.want {
			t.Errorf("matchesSnooze(%v, %q) = %v, want %v", tt.patterns, tt.url, got, tt.want)
		}
	}
}

func TestShellGate(t *testing.T) {
	ctx := context.Background()

	open, err := ShellGate(ctx, "true")
	if err != nil || !open {
		t.Errorf("ShellGate(true) = (%v, %v), want (true, nil)", open, err)
	}

	open, err = ShellGate(ctx, "exit 3")
	if err != nil || open {
		t.Errorf("ShellGate(exit 3) = (%v, %v), want (false, nil): a non-zero exit is the gate answering \"closed\", not a failure", open, err)
	}

	// 127 is the shell's own "command not found": the gate could not
	// actually be evaluated, which is an error, not a "closed" answer.
	_, err = ShellGate(ctx, "definitely-not-a-real-command-xyz")
	if err == nil {
		t.Errorf("ShellGate(missing command) error = nil, want an error: a gate that cannot run must not silently read as closed")
	}
}

func snoozeTestRegs(t *testing.T, store *registry.Store, urls ...string) {
	t.Helper()
	now := time.Now()
	for i, url := range urls {
		reg := registry.Registration{
			PRURL: url, SessionID: "sess" + string(rune('A'+i)), Provider: "github",
			CIBucket: "pending", NextDueAt: now.Add(-time.Minute), CreatedAt: now,
		}
		if err := store.Add(context.Background(), reg); err != nil {
			t.Fatalf("Add() error = %v", err)
		}
	}
}

type gateCalls struct {
	answers map[string]bool
	err     error
	calls   []string
}

func (g *gateCalls) run(ctx context.Context, gate string) (bool, error) {
	g.calls = append(g.calls, gate)
	return g.answers[gate], g.err
}

func TestTickSkipsSnoozedRegistrationWithoutCheckingProvider(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	snoozeTestRegs(t, store, "https://github.com/work/r/pull/1")

	gh := &fakeGitHubChecker{result: provider.CheckResult{State: "open", CIBucket: "failure"}}
	notify := &fakeNotifier{}
	gate := &gateCalls{answers: map[string]bool{"work-hours": false}}
	sched := &Scheduler{
		Store: store, GitHub: gh, Sessions: alwaysLiveSessions(), Notify: notify,
		Snooze:  func() []SnoozeRule { return []SnoozeRule{{Match: []string{"github.com/work/*"}, Gate: "work-hours"}} },
		RunGate: gate.run,
	}
	if err := sched.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}

	if gh.calls != 0 {
		t.Errorf("GitHub checker calls = %d, want 0 while snoozed", gh.calls)
	}
	if len(notify.sent) != 0 {
		t.Errorf("notify.sent = %v, want nothing while snoozed", notify.sent)
	}
	regs, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if regs[0].CIBucket != "pending" {
		t.Errorf("CIBucket = %q, want the untouched baseline \"pending\" so the backlog diffs on resume", regs[0].CIBucket)
	}
	if !regs[0].NextDueAt.After(time.Now()) {
		t.Errorf("NextDueAt = %v, want rescheduled into the future so the due-heap doesn't spin", regs[0].NextDueAt)
	}
}

func TestTickChecksRegistrationWhenGateOpen(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	snoozeTestRegs(t, store, "https://github.com/work/r/pull/1")

	gh := &fakeGitHubChecker{result: provider.CheckResult{State: "open", CIBucket: "pending"}}
	gate := &gateCalls{answers: map[string]bool{"work-hours": true}}
	sched := &Scheduler{
		Store: store, GitHub: gh, Sessions: alwaysLiveSessions(), Notify: &fakeNotifier{},
		Snooze:  func() []SnoozeRule { return []SnoozeRule{{Match: []string{"github.com/work/*"}, Gate: "work-hours"}} },
		RunGate: gate.run,
	}
	if err := sched.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}
	if gh.calls != 1 {
		t.Errorf("GitHub checker calls = %d, want 1 when the gate is open", gh.calls)
	}
}

func TestTickChecksRegistrationMatchingNoRule(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	snoozeTestRegs(t, store, "https://github.com/personal/r/pull/1")

	gh := &fakeGitHubChecker{result: provider.CheckResult{State: "open", CIBucket: "pending"}}
	gate := &gateCalls{answers: map[string]bool{"work-hours": false}}
	sched := &Scheduler{
		Store: store, GitHub: gh, Sessions: alwaysLiveSessions(), Notify: &fakeNotifier{},
		Snooze:  func() []SnoozeRule { return []SnoozeRule{{Match: []string{"github.com/work/*"}, Gate: "work-hours"}} },
		RunGate: gate.run,
	}
	if err := sched.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}
	if gh.calls != 1 {
		t.Errorf("GitHub checker calls = %d, want 1 for a registration no rule matches", gh.calls)
	}
	if len(gate.calls) != 0 {
		t.Errorf("gate calls = %v, want none: an unmatched registration must not even evaluate a gate", gate.calls)
	}
}

// Every rule matching a registration is a stage it has to pass: it is only
// checked while all of those gates are open, e.g. work PRs only during work
// time and, like everything else, not during a break.
func TestTickChecksRegistrationOnlyWhenEveryMatchingGateIsOpen(t *testing.T) {
	cases := []struct {
		name      string
		answers   map[string]bool
		wantCalls int
	}{
		{"both open", map[string]bool{"work-hours": true, "no-break": true}, 1},
		{"work hours closed", map[string]bool{"work-hours": false, "no-break": true}, 0},
		{"break", map[string]bool{"work-hours": true, "no-break": false}, 0},
		{"both closed", map[string]bool{"work-hours": false, "no-break": false}, 0},
	}
	for _, c := range cases {
		ctx := context.Background()
		store := openTestStore(t)
		snoozeTestRegs(t, store, "https://github.com/work/special/pull/1")

		gh := &fakeGitHubChecker{result: provider.CheckResult{State: "open", CIBucket: "pending"}}
		gate := &gateCalls{answers: c.answers}
		sched := &Scheduler{
			Store: store, GitHub: gh, Sessions: alwaysLiveSessions(), Notify: &fakeNotifier{},
			Snooze: func() []SnoozeRule {
				return []SnoozeRule{
					{Match: []string{"github.com/work/*"}, Gate: "work-hours"},
					{Match: []string{"*"}, Gate: "no-break"},
				}
			},
			RunGate: gate.run,
		}
		if err := sched.Tick(ctx); err != nil {
			t.Fatalf("%s: Tick() error = %v", c.name, err)
		}
		if gh.calls != c.wantCalls {
			t.Errorf("%s: GitHub checker calls = %d, want %d", c.name, gh.calls, c.wantCalls)
		}
	}
}

// A registration is only held to the rules that match it: a private PR is
// not stopped by the work-hours gate.
func TestTickIgnoresGatesOfRulesThatDoNotMatch(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	snoozeTestRegs(t, store, "https://github.com/me/private/pull/1")

	gh := &fakeGitHubChecker{result: provider.CheckResult{State: "open", CIBucket: "pending"}}
	gate := &gateCalls{answers: map[string]bool{"work-hours": false, "no-break": true}}
	sched := &Scheduler{
		Store: store, GitHub: gh, Sessions: alwaysLiveSessions(), Notify: &fakeNotifier{},
		Snooze: func() []SnoozeRule {
			return []SnoozeRule{
				{Match: []string{"github.com/work/*"}, Gate: "work-hours"},
				{Match: []string{"*"}, Gate: "no-break"},
			}
		},
		RunGate: gate.run,
	}
	if err := sched.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}
	if gh.calls != 1 {
		t.Errorf("GitHub checker calls = %d, want 1: the closed work-hours gate does not apply to this PR", gh.calls)
	}
	if slices.Contains(gate.calls, "work-hours") {
		t.Errorf("gate calls = %v, want the non-matching rule's gate not evaluated", gate.calls)
	}
}

// One closed gate is enough, so the remaining ones are not run for it.
func TestTickStopsEvaluatingGatesAtTheFirstClosedOne(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	snoozeTestRegs(t, store, "https://github.com/work/a/pull/1")

	gate := &gateCalls{answers: map[string]bool{"work-hours": false, "no-break": true}}
	sched := &Scheduler{
		Store: store, GitHub: &fakeGitHubChecker{}, Sessions: alwaysLiveSessions(), Notify: &fakeNotifier{},
		Snooze: func() []SnoozeRule {
			return []SnoozeRule{
				{Match: []string{"github.com/work/*"}, Gate: "work-hours"},
				{Match: []string{"*"}, Gate: "no-break"},
			}
		},
		RunGate: gate.run,
	}
	if err := sched.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}
	if !slices.Equal(gate.calls, []string{"work-hours"}) {
		t.Errorf("gate calls = %v, want only work-hours: it is closed, so the later gate is moot", gate.calls)
	}
}

func TestTickRunsEachDistinctGateOncePerTick(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	snoozeTestRegs(t, store,
		"https://github.com/work/a/pull/1",
		"https://github.com/work/b/pull/2",
		"https://github.com/work/c/pull/3",
	)

	gate := &gateCalls{answers: map[string]bool{"work-hours": false}}
	sched := &Scheduler{
		Store: store, GitHub: &fakeGitHubChecker{}, Sessions: alwaysLiveSessions(), Notify: &fakeNotifier{},
		Snooze:  func() []SnoozeRule { return []SnoozeRule{{Match: []string{"github.com/work/*"}, Gate: "work-hours"}} },
		RunGate: gate.run,
	}
	if err := sched.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}
	if len(gate.calls) != 1 {
		t.Errorf("gate calls = %v, want exactly 1: same gate command shared by three registrations", gate.calls)
	}
}

func TestTickGateErrorFailsOpen(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	snoozeTestRegs(t, store, "https://github.com/work/r/pull/1")

	gh := &fakeGitHubChecker{result: provider.CheckResult{State: "open", CIBucket: "pending"}}
	gate := &gateCalls{err: errors.New("sh: not found")}
	sched := &Scheduler{
		Store: store, GitHub: gh, Sessions: alwaysLiveSessions(), Notify: &fakeNotifier{},
		Snooze:  func() []SnoozeRule { return []SnoozeRule{{Match: []string{"github.com/work/*"}, Gate: "broken"}} },
		RunGate: gate.run,
	}
	if err := sched.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}
	if gh.calls != 1 {
		t.Errorf("GitHub checker calls = %d, want 1: a gate that cannot be evaluated must not silently mute notifications", gh.calls)
	}
}

func TestTickSnoozesGitLabRegistrations(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	now := time.Now()
	reg := registry.Registration{
		PRURL: "https://gitlab.corp.example/g/p/-/merge_requests/1", SessionID: "sessA", Provider: "gitlab",
		NextDueAt: now.Add(-time.Minute), CreatedAt: now,
	}
	if err := store.Add(ctx, reg); err != nil {
		t.Fatalf("Add() error = %v", err)
	}

	gl := &fakeGitLabChecker{result: provider.CheckResult{State: "open"}}
	gate := &gateCalls{answers: map[string]bool{"work-hours": false}}
	sched := &Scheduler{
		Store: store, GitLab: gl, Sessions: alwaysLiveSessions(), Notify: &fakeNotifier{},
		Snooze: func() []SnoozeRule {
			return []SnoozeRule{{Match: []string{"gitlab.corp.example/*"}, Gate: "work-hours"}}
		},
		RunGate: gate.run,
	}
	if err := sched.Tick(ctx); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}
	if gl.calls != 0 {
		t.Errorf("GitLab checker calls = %d, want 0 while snoozed", gl.calls)
	}
}

// A snoozed watch is marked so its badge can show it: set while the gate is
// closed, cleared as soon as the watch is checked again.
func TestSnoozedFlagFollowsTheGate(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	now := time.Now()
	reg := registry.Registration{
		PRURL: "https://gitlab.corp.example/g/p/-/merge_requests/1", SessionID: "sessA", Provider: "gitlab",
		NextDueAt: now.Add(-time.Minute), CreatedAt: now,
	}
	if err := store.Add(ctx, reg); err != nil {
		t.Fatal(err)
	}
	gl := &fakeGitLabChecker{result: provider.CheckResult{State: "open"}}
	gate := &gateCalls{answers: map[string]bool{"work-hours": false}}
	sched := &Scheduler{
		Store: store, GitLab: gl, Sessions: alwaysLiveSessions(), Notify: &fakeNotifier{},
		Snooze: func() []SnoozeRule {
			return []SnoozeRule{{Match: []string{"gitlab.corp.example/*"}, Gate: "work-hours"}}
		},
		RunGate: gate.run,
	}
	if err := sched.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if got := getReg(t, store, reg.PRURL, "sessA"); !got.Snoozed {
		t.Fatalf("registration = %+v, want it marked snoozed while the gate is closed", got)
	}

	// The gate opens; the next check is due again (the snooze recheck is a minute).
	gate.answers["work-hours"] = true
	if err := store.UpdateState(ctx, reg.PRURL, "sessA", registry.StateUpdate{NextDueAt: now.Add(-time.Second), CIBucket: "none"}); err != nil {
		t.Fatal(err)
	}
	if err := sched.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if got := getReg(t, store, reg.PRURL, "sessA"); got.Snoozed {
		t.Errorf("registration = %+v, want the snoozed mark cleared once the gate opened", got)
	}
	if gl.calls == 0 {
		t.Error("the watch was not checked after the gate opened")
	}
}
