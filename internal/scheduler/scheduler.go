package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/cwrau/aoe-plugin-pr-watch-mcp/internal/aoeclient"
	"github.com/cwrau/aoe-plugin-pr-watch-mcp/internal/provider"
	"github.com/cwrau/aoe-plugin-pr-watch-mcp/internal/registry"
)

// GitHubChecker and GitLabChecker are narrow views of provider.GitHubChecker
// / provider.GitLabChecker, so the scheduler can be tested against fakes
// without a real HTTP round trip.
type GitHubChecker interface {
	CheckBatch(ctx context.Context, urls []string) (map[string]provider.CheckResult, provider.GitHubBudgets, error)
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
// its session first (skip-and-keep on archived, drop on gone), then either
// collects it for GitHub's single batched call (see processGitHubBatch) or,
// for GitLab, preflights and checks it immediately in the same pass —
// sequential and interleaved, so reg2's preflight still sees the cache
// update from reg1's check earlier in this same tick, exactly as a single
// registration always would. GitHub registrations are only ever
// preflighted here; their real check is deferred to the one batched call
// after this loop, since there's nothing to learn from a check that hasn't
// happened yet. A single registration's failure (network blip, provider
// error) is logged and left for the next Tick rather than aborting the
// whole pass.
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

	now := time.Now()
	var githubReady []registry.Registration
	gitlabChecked := make(map[string]int) // host -> number of MRs checked against it this tick
	for _, reg := range due {
		switch reg.Provider {
		case "github":
			if s.preflight(ctx, reg, now) {
				githubReady = append(githubReady, reg)
			}
		case "gitlab":
			if s.preflight(ctx, reg, now) {
				if host := s.processGitLabOne(ctx, reg, counts); host != "" {
					gitlabChecked[host]++
				}
			}
		default:
			s.log().Error("unknown provider", "pr_url", reg.PRURL, "provider", reg.Provider)
		}
	}
	// One summary line per host touched this tick, not one per MR: GitLab
	// has no batch API here (unlike GitHub's aliased GraphQL query), so
	// each MR is still its own real HTTP call, but the log stays as terse
	// as GitHub's single-line-per-tick shape by reporting each host's
	// latest (i.e. most current) budget reading once, alongside how many
	// of its MRs were checked this tick.
	for host, count := range gitlabChecked {
		budget := s.lastGitLabBudgets[host]
		s.log().Info("gitlab rate limit", "host", host, "remaining", budget.Remaining, "limit", budget.Limit, "reset_at", budget.ResetAt,
			"checked", count, "next_check_in", ComputeInterval(budget, counts["gitlab"], now))
	}

	s.processGitHubBatch(ctx, githubReady, counts)
	return nil
}

// preflight handles the two things that end a registration's processing
// for this tick before any provider call: a gone session's session drops
// the registration to stopped, an archived one just reschedules. Reports
// whether reg still needs a real check this tick.
func (s *Scheduler) preflight(ctx context.Context, reg registry.Registration, now time.Time) bool {
	switch s.Sessions.Classify(reg.SessionID) {
	case aoeclient.SessionGone:
		if err := s.Store.SetStatus(ctx, reg.PRURL, registry.StatusStopped); err != nil {
			s.log().Error("stop registration for gone session", "pr_url", reg.PRURL, "error", err)
		}
		return false
	case aoeclient.SessionArchived:
		// Skip the send, but still push next_due_at out using the fallback
		// interval so an archived registration doesn't spin the due-heap
		// every tick while archived.
		if err := s.Store.UpdateState(ctx, reg.PRURL, stateUpdateFromReg(reg, now.Add(FallbackInterval))); err != nil {
			s.log().Error("reschedule archived registration", "pr_url", reg.PRURL, "error", err)
		}
		return false
	}

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
		if err := s.Store.UpdateState(ctx, reg.PRURL, stateUpdateFromReg(reg, now.Add(wait))); err != nil {
			s.log().Error("reschedule budget-exhausted registration", "pr_url", reg.PRURL, "error", err)
		}
		return false
	}
	return true
}

// processGitHubBatch resolves every ready GitHub registration in one
// GraphQL call (GitHub's GraphQL API supports aliasing many lookups into
// one request; see provider.GitHubChecker.CheckBatch), then runs each
// result through the same diff/notify/persist tail a single check always
// has. A whole-batch failure (network error, auth failure, top-level
// GraphQL error) backs off every registration in it exactly like an
// individual check failure always has — there's no partial data to save in
// that case. A registration whose URL is simply missing from a successful
// batch response (PR/repo not found) is backed off the same way,
// individually, without affecting the rest of the batch.
func (s *Scheduler) processGitHubBatch(ctx context.Context, regs []registry.Registration, counts map[string]int) {
	if len(regs) == 0 {
		return
	}
	urls := make([]string, len(regs))
	for i, reg := range regs {
		urls[i] = reg.PRURL
	}

	results, budgets, err := s.GitHub.CheckBatch(ctx, urls)
	if err != nil {
		s.log().Warn("github batch check failed, retrying after the poll floor", "count", len(regs), "error", err)
		for _, reg := range regs {
			s.finishError(ctx, reg, err)
		}
		return
	}
	s.lastGitHubBudgets = budgets

	activeCount := counts["github"]
	// Whichever pool has less relative headroom governs the interval: both
	// calls happened (for every non-terminal result in the batch), both
	// must stay under budget.
	interval := minInterval(
		ComputeInterval(budgets.GraphQL, activeCount, time.Now()),
		ComputeInterval(budgets.REST, activeCount, time.Now()),
	)
	s.log().Info("github rate limit",
		"graphql_remaining", budgets.GraphQL.Remaining, "graphql_limit", budgets.GraphQL.Limit, "graphql_reset_at", budgets.GraphQL.ResetAt,
		"rest_remaining", budgets.REST.Remaining, "rest_limit", budgets.REST.Limit, "rest_reset_at", budgets.REST.ResetAt,
		"next_check_in", interval)

	for _, reg := range regs {
		result, ok := results[reg.PRURL]
		if !ok {
			s.finishError(ctx, reg, fmt.Errorf("no result for %s in batch response", reg.PRURL))
			continue
		}
		s.finish(ctx, reg, result, interval)
	}
}

// processGitLabOne checks one GitLab MR and returns the host it belongs to
// (so Tick can log one rate-limit summary per host per tick instead of one
// per MR), or "" if the URL didn't parse or the check itself failed (in
// either case there's no fresh budget reading to fold into that summary).
func (s *Scheduler) processGitLabOne(ctx context.Context, reg registry.Registration, counts map[string]int) string {
	result, budget, checkErr := s.GitLab.Check(ctx, reg.PRURL)
	if checkErr != nil {
		s.finishError(ctx, reg, checkErr)
		return ""
	}
	interval := ComputeInterval(budget, counts["gitlab"], time.Now())
	host, _, _, err := provider.ParseGitLabMRURL(reg.PRURL)
	if err == nil {
		if s.lastGitLabBudgets == nil {
			s.lastGitLabBudgets = make(map[string]provider.RateBudget)
		}
		s.lastGitLabBudgets[host] = budget
	}
	s.finish(ctx, reg, result, interval)
	return host
}

// finishError is the shared backoff path a failed check (or a batch
// response missing that registration's URL) always takes: push next_due_at
// out by the poll floor rather than retrying on every scheduler tick
// forever, leaving the stored state untouched so the next successful check
// still diffs against the real last-seen state.
func (s *Scheduler) finishError(ctx context.Context, reg registry.Registration, checkErr error) {
	s.log().Warn("check failed, retrying after the poll floor", "pr_url", reg.PRURL, "retry_in", MinPollInterval, "error", checkErr)
	if err := s.Store.UpdateState(ctx, reg.PRURL, stateUpdateFromReg(reg, time.Now().Add(MinPollInterval))); err != nil {
		s.log().Error("back off failed check", "pr_url", reg.PRURL, "error", err)
	}
}

// stateUpdateFromReg builds a registry.StateUpdate carrying reg's own
// already-persisted fields unchanged, for the reschedule-only paths
// (archived, budget-exhausted, failed check) that push next_due_at out
// without learning anything new from a real check.
func stateUpdateFromReg(reg registry.Registration, nextDueAt time.Time) registry.StateUpdate {
	return registry.StateUpdate{
		ReviewCount:         reg.ReviewCount,
		CIBucket:            reg.CIBucket,
		CIDetail:            reg.CIDetail,
		CIHref:              reg.CIHref,
		ReviewHref:          reg.ReviewHref,
		UnresolvedThreadIDs: reg.UnresolvedThreadIDs,
		LatestReviewState:   reg.LatestReviewState,
		LatestReviewer:      reg.LatestReviewer,
		LatestReviewAt:      reg.LatestReviewAt,
		Title:               reg.Title,
		Description:         reg.Description,
		NextDueAt:           nextDueAt,
		HasConflict:         reg.HasConflict,
		MergeQueueState:     reg.MergeQueueState,
		MergeQueuePosition:  reg.MergeQueuePosition,
		Blocked:             reg.Blocked,
	}
}

// finish is the shared tail every successful check (single or batched)
// runs: diff against the last-seen state, send any resulting events
// best-effort, then either mark a terminal PR resolved or persist the new
// state and next due time.
func (s *Scheduler) finish(ctx context.Context, reg registry.Registration, result provider.CheckResult, interval time.Duration) {
	prev := provider.PrevState{ReviewCount: reg.ReviewCount, CIBucket: reg.CIBucket, UnresolvedThreadIDs: reg.UnresolvedThreadIDs, MergeQueueState: reg.MergeQueueState}

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
		s.log().Info("event", "pr_url", reg.PRURL, "session_id", reg.SessionID, "kind", ev.Kind, "message", ev.Message)
		if err := s.Notify.Send(ctx, reg.SessionID, eventMessage(ev)); err != nil {
			s.log().Warn("notify failed", "pr_url", reg.PRURL, "session_id", reg.SessionID, "error", err)
		}
	}

	if result.State == registry.StatusMerged {
		if err := s.Store.SetMerged(ctx, reg.PRURL, result.MergedAt); err != nil {
			s.log().Error("mark merged registration", "pr_url", reg.PRURL, "error", err)
		}
		return
	}
	if result.State == registry.StatusClosed {
		if err := s.Store.SetStatus(ctx, reg.PRURL, result.State); err != nil {
			s.log().Error("mark resolved registration", "pr_url", reg.PRURL, "error", err)
		}
		return
	}

	update := registry.StateUpdate{
		ReviewCount:         result.ReviewCount,
		CIBucket:            result.CIBucket,
		CIDetail:            result.CIDetail,
		CIHref:              result.CIHref,
		ReviewHref:          result.ReviewHref,
		UnresolvedThreadIDs: result.UnresolvedThreadIDs,
		LatestReviewState:   result.LatestReviewState,
		LatestReviewer:      result.LatestReviewer,
		LatestReviewAt:      result.LatestReviewAt,
		Title:               result.Title,
		Description:         result.Description,
		NextDueAt:           time.Now().Add(interval),
		HasConflict:         result.HasConflict,
		MergeQueueState:     result.MergeQueueState,
		MergeQueuePosition:  result.MergeQueuePosition,
		Blocked:             result.Blocked,
	}
	if err := s.Store.UpdateState(ctx, reg.PRURL, update); err != nil {
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
