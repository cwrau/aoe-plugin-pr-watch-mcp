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
		if err := s.Store.Remove(ctx, reg.PRURL); err != nil {
			s.log().Error("drop registration for gone session", "pr_url", reg.PRURL, "error", err)
		}
		return
	case aoeclient.SessionArchived:
		// Skip the send, but still push next_due_at out using the fallback
		// interval so an archived registration doesn't spin the due-heap
		// every tick while archived.
		if err := s.Store.UpdateState(ctx, reg.PRURL, reg.ReviewCount, reg.CIBucket, reg.Unresolved, time.Now().Add(FallbackInterval)); err != nil {
			s.log().Error("reschedule archived registration", "pr_url", reg.PRURL, "error", err)
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
		// Whichever pool has less relative headroom governs the interval:
		// both calls happened, both must stay under budget.
		interval = minInterval(
			ComputeInterval(budgets.GraphQL, activeCount, time.Now()),
			ComputeInterval(budgets.REST, activeCount, time.Now()),
		)
	case "gitlab":
		var budget provider.RateBudget
		result, budget, checkErr = s.GitLab.Check(ctx, reg.PRURL)
		interval = ComputeInterval(budget, activeCount, time.Now())
	default:
		s.log().Error("unknown provider", "pr_url", reg.PRURL, "provider", reg.Provider)
		return
	}

	if checkErr != nil {
		s.log().Warn("check failed, will retry next tick", "pr_url", reg.PRURL, "error", checkErr)
		return
	}

	for _, ev := range provider.Diff(prev, result) {
		if err := s.Notify.Send(ctx, reg.SessionID, eventMessage(ev)); err != nil {
			s.log().Warn("notify failed, will retry next tick", "pr_url", reg.PRURL, "error", err)
		}
	}

	if result.State == "merged" || result.State == "closed" {
		if err := s.Store.Remove(ctx, reg.PRURL); err != nil {
			s.log().Error("drop resolved registration", "pr_url", reg.PRURL, "error", err)
		}
		return
	}

	if err := s.Store.UpdateState(ctx, reg.PRURL, result.ReviewCount, result.CIBucket, result.Unresolved, time.Now().Add(interval)); err != nil {
		s.log().Error("persist new state", "pr_url", reg.PRURL, "error", err)
	}
}

func minInterval(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
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
