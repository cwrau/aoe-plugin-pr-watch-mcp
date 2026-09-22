package scheduler

import (
	"context"
	"log/slog"
	"time"

	"github.com/cwrau/aoe-pr-watch-mcp/internal/aoeclient"
	"github.com/cwrau/aoe-pr-watch-mcp/internal/provider"
	"github.com/cwrau/aoe-pr-watch-mcp/internal/registry"
)

// GitHubChecker and GitLabChecker are narrow views of provider.GitHubChecker
// / provider.GitLabChecker, so the scheduler can be tested against fakes
// without a real HTTP round trip.
type GitHubChecker interface {
	Check(ctx context.Context, url string) (provider.CheckResult, provider.GitHubBudgets, error)
}

type GitLabChecker interface {
	Check(ctx context.Context, url string) (provider.CheckResult, provider.RateBudget, error)
}

// SessionClassifier is the narrow view of aoeclient.Client the scheduler
// needs: refresh the live/archived/gone sets once per tick, then classify
// each due registration's session against that snapshot.
type SessionClassifier interface {
	RefreshSessionSets(ctx context.Context) error
	Classify(id string) aoeclient.SessionClass
}

// Sender is the narrow view of notifier.Notifier the scheduler needs.
type Sender interface {
	Send(ctx context.Context, sessionID, message string) error
}

type Scheduler struct {
	Store    *registry.Store
	GitHub   GitHubChecker
	GitLab   GitLabChecker
	Sessions SessionClassifier
	Notify   Sender
	Log      *slog.Logger

	// lastGitHubBudgets/lastGitLabBudgets cache the most recently observed
	// budget for each pool (GitLab keyed by host — each host has its own
	// separate pool). Several registrations can share one pool, so once
	// any of them reveals it's exhausted there's no point spending (or
	// failing) a real API call to rediscover that for every other due
	// registration in the same pool this tick — skipForExhaustedBudget
	// checks this cache first. Tick/processOne run strictly sequentially
	// (one scheduler loop goroutine), so no locking is needed.
	lastGitHubBudgets provider.GitHubBudgets
	lastGitLabBudgets map[string]provider.RateBudget
}

func (s *Scheduler) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

// Tick processes every registration currently due for a check: classifies
// its session first (skip-and-keep on archived, drop on gone), runs the
// matching provider's Check, diffs against the last-seen state, sends any
// resulting events, and persists the new state plus the next due time. A
// single registration's failure (network blip, provider error) is logged
// and left for the next Tick rather than aborting the whole batch.
func (s *Scheduler) Tick(ctx context.Context) error {
	if err := s.Sessions.RefreshSessionSets(ctx); err != nil {
		return err
	}

	counts, err := s.Store.CountByProvider(ctx)
	if err != nil {
		return err
	}

	due, err := s.Store.DueBefore(ctx, time.Now())
	if err != nil {
		return err
	}

	for _, reg := range due {
		s.processOne(ctx, reg, counts)
	}
	return nil
}

func (s *Scheduler) processOne(ctx context.Context, reg registry.Registration, counts map[string]int) {
	switch s.Sessions.Classify(reg.SessionID) {
	case aoeclient.SessionGone:
		if err := s.Store.SetStatus(ctx, reg.PRURL, registry.StatusStopped); err != nil {
			s.log().Error("stop registration for gone session", "pr_url", reg.PRURL, "error", err)
		}
		return
	case aoeclient.SessionArchived:
		// Skip the send, but still push next_due_at out using the fallback
		// interval so an archived registration doesn't spin the due-heap
		// every tick while archived.
		if err := s.Store.UpdateState(ctx, reg.PRURL, reg.ReviewCount, reg.CIBucket, reg.Unresolved, reg.LatestReviewState, reg.Title, reg.Description, time.Now().Add(FallbackInterval)); err != nil {
			s.log().Error("reschedule archived registration", "pr_url", reg.PRURL, "error", err)
		}
		return
	}

	now := time.Now()
	if skip, resumeAt := s.skipForExhaustedBudget(reg, now); skip {
		// We already know from a previous check this tick (or an earlier
		// one still within its window) that this pool's reserved floor is
		// exhausted — spending a real call here would either fail outright
		// or eat further into headroom we're deliberately not touching, to
		// find out something we already know. Reschedule using the same
		// no-headroom pacing ComputeInterval would apply, without ever
		// hitting the network.
		wait := max(resumeAt.Sub(now), FallbackInterval)
		s.log().Debug("skipping check, cached pool budget already exhausted", "pr_url", reg.PRURL, "resume_at", resumeAt)
		if err := s.Store.UpdateState(ctx, reg.PRURL, reg.ReviewCount, reg.CIBucket, reg.Unresolved, reg.LatestReviewState, reg.Title, reg.Description, now.Add(wait)); err != nil {
			s.log().Error("reschedule budget-exhausted registration", "pr_url", reg.PRURL, "error", err)
		}
		return
	}

	prev := provider.PrevState{ReviewCount: reg.ReviewCount, CIBucket: reg.CIBucket, Unresolved: reg.Unresolved}
	activeCount := counts[reg.Provider]

	var (
		result   provider.CheckResult
		interval time.Duration
		checkErr error
	)
	switch reg.Provider {
	case "github":
		var budgets provider.GitHubBudgets
		result, budgets, checkErr = s.GitHub.Check(ctx, reg.PRURL)
		if checkErr == nil {
			s.lastGitHubBudgets = budgets
		}
		// Whichever pool has less relative headroom governs the interval:
		// both calls happened, both must stay under budget.
		interval = minInterval(
			ComputeInterval(budgets.GraphQL, activeCount, time.Now()),
			ComputeInterval(budgets.REST, activeCount, time.Now()),
		)
	case "gitlab":
		var budget provider.RateBudget
		result, budget, checkErr = s.GitLab.Check(ctx, reg.PRURL)
		if checkErr == nil {
			if host, _, _, err := provider.ParseGitLabMRURL(reg.PRURL); err == nil {
				if s.lastGitLabBudgets == nil {
					s.lastGitLabBudgets = make(map[string]provider.RateBudget)
				}
				s.lastGitLabBudgets[host] = budget
			}
		}
		interval = ComputeInterval(budget, activeCount, time.Now())
	default:
		s.log().Error("unknown provider", "pr_url", reg.PRURL, "provider", reg.Provider)
		return
	}

	if checkErr != nil {
		// Push next_due_at out by the poll floor before giving up on this
		// registration: a permanently-broken one (revoked token, deleted
		// repo) would otherwise be retried on every scheduler tick forever,
		// far below MinPollInterval and outside the rate-limit budget math.
		// The stored state is written back unchanged — only the due time
		// moves — so the next successful check still diffs against the real
		// last-seen state.
		s.log().Warn("check failed, retrying after the poll floor", "pr_url", reg.PRURL, "retry_in", MinPollInterval, "error", checkErr)
		if err := s.Store.UpdateState(ctx, reg.PRURL, reg.ReviewCount, reg.CIBucket, reg.Unresolved, reg.LatestReviewState, reg.Title, reg.Description, time.Now().Add(MinPollInterval)); err != nil {
			s.log().Error("back off failed check", "pr_url", reg.PRURL, "error", err)
		}
		return
	}

	// Best-effort only: a session whose Sender can never deliver (e.g. an
	// ACP-mode AoE session, which has no tmux pane for `aoe send` to write
	// into) would otherwise fail every single tick forever. Gating state
	// persistence on delivery success used to mean such a registration's
	// title/CI/review state froze at its very first check for good, and its
	// next_due_at never advanced past "immediately due" — hammering Send in
	// a tight retry loop indefinitely instead of backing off to the normal
	// polling interval. Logging the failure and moving on trades "guaranteed
	// eventual delivery" for "the dashboard stays fresh and polling stays
	// paced" — the right trade given delivery to a structurally incompatible
	// session was never going to succeed on a later retry anyway.
	for _, ev := range provider.Diff(prev, result) {
		if err := s.Notify.Send(ctx, reg.SessionID, eventMessage(ev)); err != nil {
			s.log().Warn("notify failed", "pr_url", reg.PRURL, "session_id", reg.SessionID, "error", err)
		}
	}

	if result.State == registry.StatusMerged || result.State == registry.StatusClosed {
		if err := s.Store.SetStatus(ctx, reg.PRURL, result.State); err != nil {
			s.log().Error("mark resolved registration", "pr_url", reg.PRURL, "error", err)
		}
		return
	}

	if err := s.Store.UpdateState(ctx, reg.PRURL, result.ReviewCount, result.CIBucket, result.Unresolved, result.LatestReviewState, result.Title, result.Description, time.Now().Add(interval)); err != nil {
		s.log().Error("persist new state", "pr_url", reg.PRURL, "error", err)
	}
}

func minInterval(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

// skipForExhaustedBudget reports whether reg's pool is already known, from
// a cached prior observation still within its own window, to have no
// headroom left above the reserved floor — and if so, when it's worth
// trying again (the later of any exhausted budget's own ResetAt). A cache
// miss, or one past its window, always reports false: there's nothing
// stale to distrust, so the only way to find out is a real call.
func (s *Scheduler) skipForExhaustedBudget(reg registry.Registration, now time.Time) (bool, time.Time) {
	switch reg.Provider {
	case "github":
		skip := false
		var resumeAt time.Time
		for _, b := range []provider.RateBudget{s.lastGitHubBudgets.GraphQL, s.lastGitHubBudgets.REST} {
			if budgetFresh(b, now) && !HasHeadroom(b, now) {
				skip = true
				if b.ResetAt.After(resumeAt) {
					resumeAt = b.ResetAt
				}
			}
		}
		return skip, resumeAt
	case "gitlab":
		host, _, _, err := provider.ParseGitLabMRURL(reg.PRURL)
		if err != nil {
			return false, time.Time{}
		}
		b, ok := s.lastGitLabBudgets[host]
		if ok && budgetFresh(b, now) && !HasHeadroom(b, now) {
			return true, b.ResetAt
		}
	}
	return false, time.Time{}
}

// budgetFresh reports whether b carries a real signal whose window hasn't
// elapsed yet — the same "is this worth trusting at all" gate HasHeadroom
// applies internally, exposed here so skipForExhaustedBudget can tell "no
// headroom" apart from "no (usable) signal" before deciding to skip.
func budgetFresh(b provider.RateBudget, now time.Time) bool {
	return b.Limit > 0 && !b.ResetAt.IsZero() && b.ResetAt.After(now)
}

// eventMessage renders a provider.Event as the text sent to the session.
// provider.Diff's terminal events (MERGED, CLOSED) set Message equal to
// Kind, so sending Message alone is already self-describing; its
// non-terminal events (CI, REVIEW, COMMENT) set Message to just the changed
// value (e.g. CIBucket "failure", or "alice APPROVED"), so the Kind is
// prefixed to keep the notification self-describing without duplicating it
// for the terminal cases.
func eventMessage(ev provider.Event) string {
	if ev.Kind == ev.Message {
		return ev.Message
	}
	return ev.Kind + " " + ev.Message
}
