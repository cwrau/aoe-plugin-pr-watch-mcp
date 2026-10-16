package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/cwrau/aoe-plugin-pr-watch-mcp/internal/aoeclient"
	"github.com/cwrau/aoe-plugin-pr-watch-mcp/internal/provider"
	"github.com/cwrau/aoe-plugin-pr-watch-mcp/internal/registry"
)

// GitHubChecker and GitLabChecker are narrow views of provider.GitHubChecker
// / provider.GitLabChecker, so the scheduler can be tested against fakes
// without a real HTTP round trip. DiscoverBranchPRs/DiscoverBranchMR back
// the auto-watch discovery pass (see discover.go) — resolving "does this
// branch have an open PR/MR yet" for a workspace checkout nobody's
// registered.
// SelfLogin/SelfUsername back the scheduler's own-account review
// suppression (see filterSelfAuthored in scheduler.go): both cache their
// result on the provider.*Checker instance, so calling them on every poll
// costs one real lookup total, not one per poll.
type GitHubChecker interface {
	CheckBatch(ctx context.Context, urls []string) (map[string]provider.CheckResult, provider.GitHubBudgets, error)
	DiscoverBranchPRs(ctx context.Context, items []provider.GitHubBranchQuery) (map[string]string, provider.RateBudget, error)
	SelfLogin(ctx context.Context) (string, error)
}

type GitLabChecker interface {
	Check(ctx context.Context, url string) (provider.CheckResult, provider.RateBudget, error)
	DiscoverBranchMR(ctx context.Context, host, projectPath, branch string) (string, bool, provider.RateBudget, error)
	SelfUsername(ctx context.Context, host string) (string, error)
}

// SessionClassifier is the narrow view of aoeclient.Client the scheduler
// needs: keep the session directory fresh (RefreshIfStale each tick — the
// host's lifecycle events keep it current, so that is only a throttled
// backstop — and Refresh when a specific session turns out to be unknown),
// then classify each due registration's session against it.
type SessionClassifier interface {
	RefreshIfStale(ctx context.Context) error
	Refresh(ctx context.Context) error
	Classify(id string) aoeclient.SessionClass
}

// Sender is the narrow view of notifier.Notifier the scheduler needs.
type Sender interface {
	Send(ctx context.Context, sessionID, message string) error
}

// WorkspaceSource is the narrow view of aoeclient.Client the auto-watch
// discovery pass needs: every live session's git checkouts, from the same
// directory SessionClassifier keeps fresh.
type WorkspaceSource interface {
	LiveWorkspaces() map[string][]aoeclient.WorkspaceRepo
}

// RemoteResolver identifies one workspace checkout's provider remote and
// its current branch — the worker-side implementation shells `git remote
// get-url origin` (local, no rate-limit cost) then classifies it: an exact
// github.com host is GitHub; anything else is treated as a candidate
// GitLab host and resolved only if a credential for it actually exists
// (there's no way to recognize an arbitrary self-hosted GitLab instance
// from its URL shape alone, unlike github.com — see ParseGitLabMRURL's own
// any-host acceptance). ok is false for a checkout with no origin, a
// non-git-forge remote, an unconfigured host, or a detached HEAD.
// remoteKey is registry.AutoDiscoveryTarget.RemoteKey's shape: "owner/repo"
// for github, "host/project/path" for gitlab. branch is read live via git
// rather than trusted from aoeclient.WorkspaceRepo.Branch — confirmed live
// that aoe's own cached branch goes stale the moment a checkout's branch is
// renamed/switched after its worktree was created, so discoverTick always
// re-resolves it here instead of using the value WorkspaceSource reported.
// It is also the head-ref name a PR/MR would carry, which differs from the
// local branch when it was pushed under another name (the worker-side
// implementation follows the remote-tracking ref git leaves at HEAD).
type RemoteResolver interface {
	Resolve(ctx context.Context, sourcePath string) (provider, remoteKey, branch string, ok bool)
}

// ForkRemoteResolver is optionally implemented by a RemoteResolver that can
// name a checkout's other GitHub remotes. A fork checkout pushes to its
// origin but opens its PR in the upstream repo, so discovery has to look
// there too: remoteKeys are those further repos ("owner/repo"), headOwners
// the owners of all the checkout's remotes, the only head repos whose PRs
// are accepted from them.
type ForkRemoteResolver interface {
	ExtraRemotes(ctx context.Context, sourcePath string) (remoteKeys, headOwners []string)
}

type Scheduler struct {
	Store      *registry.Store
	GitHub     GitHubChecker
	GitLab     GitLabChecker
	Sessions   SessionClassifier
	Notify     Sender
	Workspaces WorkspaceSource // optional: nil disables auto-watch discovery entirely
	Remotes    RemoteResolver  // optional: nil disables auto-watch discovery entirely
	Log        *slog.Logger

	// CommentFilters returns the current top-level-comment filter rules; nil
	// disables filtering. Applied to every check result against the
	// comments' current bodies before diffing, so a filtered comment is
	// neither counted nor stored as seen (see CheckResult.ApplyCommentFilters).
	// Called once per registration check, so it must be cheap (the worker
	// serves a cached copy).
	CommentFilters func() []provider.CommentFilter

	// DiscoveryInterval is how often Tick runs the full periodic discovery
	// pass (the slow fallback; session-idle events trigger DiscoverSessions
	// for the fast path). Zero runs it on every Tick.
	DiscoveryInterval time.Duration
	lastDiscovery     time.Time // when the periodic pass last ran; zero until the first

	// Snooze returns the current snooze rules; nil disables snoozing. It is
	// called per registration, so it must be cheap (the worker serves a
	// cached copy). RunGate evaluates a rule's gate command; nil means
	// ShellGate. gateMemo holds this Tick's gate answers.
	Snooze   func() []SnoozeRule
	RunGate  GateFunc
	gateMemo map[string]bool

	// pings collects this Tick's notifications per session (pingOrder keeps
	// the first-seen order so sends are deterministic); Tick flushes them as
	// one message per session. Same single-goroutine reasoning as the budget
	// caches below.
	pings     map[string][]string
	pingOrder []string

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
	// githubPointsPerPR is the GraphQL points one PR cost in the last batch
	// (0 until a batch reported its cost); pacing multiplies the consumer
	// count by it.
	githubPointsPerPR float64
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
// for GitLab, checks it immediately in the same pass — sequential and
// interleaved, so reg2's budget-exhaustion check still sees the cache
// update from reg1's real check earlier in this same tick, exactly as a
// single registration always would. Both providers dedupe by URL: several
// registrations can now share one PR/MR (multiple sessions independently
// watching it), so only one real check happens per distinct URL, with the
// shared result fanned out to every registration sharing it — for GitLab
// this is a same-tick result cache keyed by URL (see
// processGitLabRegistration), since its checks happen inline rather than
// batched. A single registration's failure (network blip, provider error)
// is logged and left for the next Tick rather than aborting the whole pass.
func (s *Scheduler) Tick(ctx context.Context) error {
	s.gateMemo = nil
	defer s.flushPings(ctx)
	if err := s.Sessions.RefreshIfStale(ctx); err != nil {
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
	gitlabCache := make(map[string]gitlabCheckResult)
	gitlabChecked := make(map[string]int) // host -> number of distinct MRs checked against it this tick
	for _, reg := range due {
		switch reg.Provider {
		case "github":
			if s.preflight(ctx, reg, now) {
				githubReady = append(githubReady, reg)
			}
		case "gitlab":
			s.processGitLabRegistration(ctx, reg, now, counts, gitlabCache, gitlabChecked)
		default:
			s.log().Error("unknown provider", "pr_url", reg.PRURL, "provider", reg.Provider)
		}
	}
	// One summary line per host touched this tick, not one per MR: GitLab
	// has no single-request batch API the way GitHub's aliased GraphQL
	// query does, so each distinct MR is still its own real HTTP call, but
	// the log stays as terse as GitHub's single-line-per-tick shape by
	// reporting each host's latest (i.e. most current) budget reading once,
	// alongside how many distinct MRs were checked against it this tick.
	for host, count := range gitlabChecked {
		budget := s.lastGitLabBudgets[host]
		s.log().Info("gitlab rate limit", "host", host, "remaining", budget.Remaining, "limit", budget.Limit, "reset_at", budget.ResetAt,
			"checked", count, "next_check_in", ComputeInterval(budget, counts["gitlab"], now))
	}

	s.processGitHubBatch(ctx, githubReady, counts)

	if s.Workspaces != nil && s.Remotes != nil && now.Sub(s.lastDiscovery) >= s.DiscoveryInterval {
		s.lastDiscovery = now
		// Its own budget, not what is left of the poll's: a busy pass before it
		// (the first one after a restart checks everything at once) would
		// otherwise leave it an expired context on every such tick.
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), discoveryTimeout)
		defer cancel()
		s.discoverTick(dctx, now)
	}
	return nil
}

// classifySession handles the two things that end a registration's
// processing for this tick based on session state alone, independent of
// any rate-limit budget: a gone session's registration drops to stopped
// (scoped to that session's own row — another session may still be
// watching the same PR/MR), an archived one just reschedules. Reports
// whether reg still needs a real check this tick.
func (s *Scheduler) classifySession(ctx context.Context, reg registry.Registration, now time.Time) bool {
	switch s.Sessions.Classify(reg.SessionID) {
	case aoeclient.SessionGone:
		// A merged/closed registration is history, not a live watch: keep its
		// status and just stop polling it.
		if reg.Status == registry.StatusMerged || reg.Status == registry.StatusClosed {
			if err := s.Store.UpdateState(ctx, reg.PRURL, reg.SessionID, stateUpdateFromReg(reg, now.Add(FallbackInterval))); err != nil {
				s.log().Error("reschedule finished registration of gone session", "pr_url", reg.PRURL, "error", err)
			}
			return false
		}
		if _, err := s.Store.SetStatusOwnedBy(ctx, reg.PRURL, reg.SessionID, registry.StatusStopped); err != nil {
			s.log().Error("stop registration for gone session", "pr_url", reg.PRURL, "session_id", reg.SessionID, "error", err)
		}
		return false
	case aoeclient.SessionArchived:
		// Skip the send, but still push next_due_at out using the fallback
		// interval so an archived registration doesn't spin the due-heap
		// every tick while archived.
		if err := s.Store.UpdateState(ctx, reg.PRURL, reg.SessionID, stateUpdateFromReg(reg, now.Add(FallbackInterval))); err != nil {
			s.log().Error("reschedule archived registration", "pr_url", reg.PRURL, "error", err)
		}
		return false
	}

	if s.isSnoozed(ctx, reg) {
		// Stored state is left untouched on purpose: the baseline stays at
		// what was last seen, so the first check after the gate opens diffs
		// against it and reports everything that happened while snoozed.
		if err := s.Store.UpdateState(ctx, reg.PRURL, reg.SessionID, stateUpdateFromReg(reg, now.Add(SnoozeRecheckInterval))); err != nil {
			s.log().Error("reschedule snoozed registration", "pr_url", reg.PRURL, "error", err)
		}
		return false
	}
	return true
}

// preflight additionally applies the budget-exhaustion check on top of
// classifySession — used by GitHub, whose real checks are always deferred
// to one end-of-tick batch call regardless of this check's outcome, so
// there's no same-tick cache-interleaving concern to it running early for
// every registration up front. Reports whether reg still needs a real
// check this tick.
func (s *Scheduler) preflight(ctx context.Context, reg registry.Registration, now time.Time) bool {
	if !s.classifySession(ctx, reg, now) {
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
		if err := s.Store.UpdateState(ctx, reg.PRURL, reg.SessionID, stateUpdateFromReg(reg, now.Add(wait))); err != nil {
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
// has. Deduplicated by URL before the call: two or more sessions watching
// the same PR still cost one alias/one real check, not one per session —
// the same shared result then fans out to each registration sharing that
// URL below, each diffed/persisted against its own session-scoped state. A
// whole-batch failure (network error, auth failure, top-level GraphQL
// error) backs off every registration in it exactly like an individual
// check failure always has — there's no partial data to save in that case.
// A registration whose URL is simply missing from a successful batch
// response (PR/repo not found) is backed off the same way, individually,
// without affecting the rest of the batch.
func (s *Scheduler) processGitHubBatch(ctx context.Context, regs []registry.Registration, counts map[string]int) {
	if len(regs) == 0 {
		return
	}
	urls := dedupeURLs(regs)

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
	// ComputeInterval counts one call per registration, but a PR in a batched
	// query costs several GraphQL points; learn the real figure from the
	// response and pace the GraphQL pool by it. REST stays one call per
	// open PR.
	if budgets.GraphQLCost > 0 && len(urls) > 0 {
		s.githubPointsPerPR = max(1, float64(budgets.GraphQLCost)/float64(len(urls)))
	}
	graphQLConsumers := activeCount
	if s.githubPointsPerPR > 1 {
		graphQLConsumers = int(math.Ceil(float64(activeCount) * s.githubPointsPerPR))
	}
	// The pool that needs the longer wait governs the interval: both calls
	// happened (for every non-terminal result in the batch), so both must
	// stay under budget. A pool that reported nothing (a batch of only
	// finished PRs makes no REST call) has no say.
	now := time.Now()
	var interval time.Duration
	if budgets.GraphQL.Limit > 0 {
		interval = ComputeInterval(budgets.GraphQL, graphQLConsumers, now)
	}
	if budgets.REST.Limit > 0 {
		interval = max(interval, ComputeInterval(budgets.REST, activeCount, now))
	}
	if interval == 0 {
		interval = FallbackInterval
	}
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

// processGitLabBatch checks every distinct MR URL among regs exactly once —
// GitLab has no single-request batch API the way GitHub's aliased GraphQL
// query does, so this is still one real HTTP call per distinct URL, just
// never more than one even when several sessions share it — then fans the
// shared result out to every registration sharing that URL via the same
// diff/notify/persist tail a single check always has. Returns how many
// distinct MRs were checked against each host, for Tick's one-summary-
// line-per-host log.
// gitlabCheckResult caches one URL's real GitLab check outcome for the rest
// of the current tick, so a second registration sharing that URL (another
// session watching the same MR) reuses it for free instead of triggering a
// second real HTTP call.
type gitlabCheckResult struct {
	result   provider.CheckResult
	interval time.Duration
	checkErr error
}

// processGitLabRegistration handles one due GitLab registration: session
// state first (classifySession, same as GitHub's preflight), then either
// reuses this tick's cached result for reg's URL — a second session sharing
// an already-checked MR, at zero further budget cost — or, budget
// permitting, performs the one real check for that URL. Unlike GitHub's
// batched end-of-tick call, this runs inline per-registration so a check's
// own budget update is visible to the very next registration's
// skipForExhaustedBudget in the same tick, exactly as a single registration
// always would be — but only for a genuinely new URL: a cache hit is free,
// so it must never be gated by (or itself trigger) a budget-exhaustion
// skip. cache is shared across the whole tick's GitLab registrations;
// checked accumulates per-host counts of genuinely new checks (not cache
// reuses) for Tick's rate-limit summary log.
func (s *Scheduler) processGitLabRegistration(ctx context.Context, reg registry.Registration, now time.Time, counts map[string]int, cache map[string]gitlabCheckResult, checked map[string]int) {
	if !s.classifySession(ctx, reg, now) {
		return
	}

	if cached, ok := cache[reg.PRURL]; ok {
		if cached.checkErr != nil {
			s.finishError(ctx, reg, cached.checkErr)
		} else {
			s.finish(ctx, reg, cached.result, cached.interval)
		}
		return
	}

	if skip, resumeAt := s.skipForExhaustedBudget(reg, now); skip {
		wait := max(resumeAt.Sub(now), FallbackInterval)
		s.log().Debug("skipping check, cached pool budget already exhausted", "pr_url", reg.PRURL, "resume_at", resumeAt)
		if err := s.Store.UpdateState(ctx, reg.PRURL, reg.SessionID, stateUpdateFromReg(reg, now.Add(wait))); err != nil {
			s.log().Error("reschedule budget-exhausted registration", "pr_url", reg.PRURL, "error", err)
		}
		return
	}

	result, budget, checkErr := s.GitLab.Check(ctx, reg.PRURL)
	var interval time.Duration
	if checkErr == nil {
		interval = ComputeInterval(budget, counts["gitlab"], time.Now())
		if host, _, _, err := provider.ParseGitLabMRURL(reg.PRURL); err == nil {
			if s.lastGitLabBudgets == nil {
				s.lastGitLabBudgets = make(map[string]provider.RateBudget)
			}
			s.lastGitLabBudgets[host] = budget
			checked[host]++
		}
	}
	cache[reg.PRURL] = gitlabCheckResult{result: result, interval: interval, checkErr: checkErr}

	if checkErr != nil {
		s.finishError(ctx, reg, checkErr)
	} else {
		s.finish(ctx, reg, result, interval)
	}
}

// finishError is the shared backoff path a failed check (or a batch
// response missing that registration's URL) always takes: push next_due_at
// out by the poll floor rather than retrying on every scheduler tick
// forever, leaving the stored state untouched so the next successful check
// still diffs against the real last-seen state.
func (s *Scheduler) finishError(ctx context.Context, reg registry.Registration, checkErr error) {
	s.log().Warn("check failed, retrying after the poll floor", "pr_url", reg.PRURL, "retry_in", MinPollInterval, "error", checkErr)
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), persistTimeout)
	defer cancel()
	if err := s.Store.UpdateState(pctx, reg.PRURL, reg.SessionID, stateUpdateFromReg(reg, time.Now().Add(MinPollInterval))); err != nil {
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
		CIWasFailing:        reg.CIWasFailing,
		CIDetail:            reg.CIDetail,
		CIHref:              reg.CIHref,
		ReviewHref:          reg.ReviewHref,
		UnresolvedThreadIDs: reg.UnresolvedThreadIDs,
		TopLevelCommentIDs:  reg.TopLevelCommentIDs,
		ThreadCommentIDs:    reg.ThreadCommentIDs,
		ThreadCommentsKnown: reg.ThreadCommentsKnown,
		LatestCommentHref:   reg.LatestCommentHref,
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
		HeadSHA:             reg.HeadSHA,
		IsDraft:             reg.IsDraft,
		RequestedReviewers:  reg.RequestedReviewers,
		SelfReviewState:     reg.SelfReviewState,
	}
}

// finish is the shared tail every successful check (single or batched)
// runs: diff against the last-seen state, send any resulting events
// best-effort, then either mark a terminal PR resolved or persist the new
// state and next due time.
func (s *Scheduler) finish(ctx context.Context, reg registry.Registration, result provider.CheckResult, interval time.Duration) {
	if s.CommentFilters != nil {
		result.ApplyCommentFilters(s.CommentFilters())
	}
	prev := provider.PrevState{
		ReviewCount:         reg.ReviewCount,
		CIBucket:            reg.CIBucket,
		CIWasFailing:        reg.CIWasFailing,
		CIHref:              reg.CIHref,
		UnresolvedThreadIDs: reg.UnresolvedThreadIDs,
		TopLevelCommentIDs:  reg.TopLevelCommentIDs,
		ThreadCommentIDs:    reg.ThreadCommentIDs,
		ThreadCommentsKnown: reg.ThreadCommentsKnown,
		MergeQueueState:     reg.MergeQueueState,
		HasConflict:         reg.HasConflict,
		LatestReviewState:   reg.LatestReviewState,
		LatestReviewer:      reg.LatestReviewer,
		ReviewHref:          reg.ReviewHref,
		HeadSHA:             reg.HeadSHA,
		IsDraft:             reg.IsDraft,
		RequestedReviewers:  reg.RequestedReviewers,
	}
	// The session's own replies are not news to it. They stay in
	// result.ThreadCommentIDs, so they still count as seen next time.
	selfAuthor := s.selfAuthor(ctx, reg)
	result.ExcludeThreadCommentsBy(selfAuthor)

	// Saving state must not depend on the poll's own deadline: when a big
	// batch comes back just as it expires, the registrations still have to be
	// stored, or the same events are found and pinged again on every poll.
	pctx, cancelPersist := context.WithTimeout(context.WithoutCancel(ctx), persistTimeout)
	defer cancelPersist()

	// Best-effort only: a session whose Sender can never deliver (e.g. an
	// archived or otherwise unreachable AoE session, which the host refuses
	// the message for) would otherwise fail every single tick forever. Gating state
	// persistence on delivery success used to mean such a registration's
	// title/CI/review state froze at its very first check for good, and its
	// next_due_at never advanced past "immediately due" — hammering Send in
	// a tight retry loop indefinitely instead of backing off to the normal
	// polling interval. Logging the failure and moving on trades "guaranteed
	// eventual delivery" for "the dashboard stays fresh and polling stays
	// paced" — the right trade given delivery to a structurally incompatible
	// session was never going to succeed on a later retry anyway.
	//
	// Every event from this one poll is sent as a single combined message,
	// not one Send per event: a PR that changed in several ways at once
	// (e.g. a new review landing right as CI turns red) should read as one
	// ping to react to, not a flurry of separate ones landing back-to-back.
	// A merged or closed PR/MR keeps being polled for FinishedWatchWindow so
	// late comments and reviews, and a reopen, are noticed. Its terminal state
	// was reported when it finished, so only activity is diffed, and a
	// registration whose PR/MR is open again goes back to normal watching.
	finished := reg.Status == registry.StatusMerged || reg.Status == registry.StatusClosed
	reopened := finished && result.State == "open"
	var events []provider.Event
	switch {
	case reopened:
		events = append([]provider.Event{{Kind: "REOPENED", Message: "REOPENED"}}, provider.DiffFinished(prev, result)...)
		if _, err := s.Store.SetStatusOwnedBy(pctx, reg.PRURL, reg.SessionID, registry.StatusWatching); err != nil {
			s.log().Error("reopen registration", "pr_url", reg.PRURL, "session_id", reg.SessionID, "error", err)
		}
	case finished:
		events = provider.DiffFinished(prev, result)
		if reg.Status == registry.StatusClosed && result.State == registry.StatusMerged {
			events = append([]provider.Event{{Kind: "MERGED", Message: "MERGED"}}, events...)
		}
	default:
		events = provider.Diff(prev, result)
	}
	for _, ev := range events {
		s.log().Info("event", "pr_url", reg.PRURL, "session_id", reg.SessionID, "kind", ev.Kind, "message", ev.Message)
	}
	// reg.EventFilter is this session's own choice at register time (empty
	// means every kind); it only ever trims what gets sent, never what's
	// logged above or how the terminal-status handling below behaves — a
	// PR/MR that's actually merged moves to StatusMerged for every session
	// watching it regardless of whether that session asked to hear about it.
	notifyEvents := filterEvents(events, reg.EffectiveEventFilter())
	notifyEvents = filterReviewRequests(notifyEvents, prev, result, selfAuthor)
	if len(events) > 0 && len(notifyEvents) == 0 {
		s.log().Info("ping suppressed", "pr_url", reg.PRURL, "session_id", reg.SessionID, "reason", "filter", "events", len(events))
	}
	beforeSelf := len(notifyEvents)
	notifyEvents = filterSelfAuthored(notifyEvents, result, selfAuthor)
	if beforeSelf > 0 && len(notifyEvents) == 0 {
		s.log().Info("ping suppressed", "pr_url", reg.PRURL, "session_id", reg.SessionID, "reason", "self-authored", "events", beforeSelf)
	}
	// A watch registered to start silent takes this first poll as its
	// baseline: whatever the PR already shows is not news to the session.
	silentFirst, err := s.Store.FirstPollSilent(pctx, reg.PRURL, reg.SessionID)
	if err != nil {
		s.log().Error("read first poll flag", "pr_url", reg.PRURL, "session_id", reg.SessionID, "error", err)
	}
	if silentFirst {
		if len(notifyEvents) > 0 {
			s.log().Info("ping suppressed", "pr_url", reg.PRURL, "session_id", reg.SessionID, "reason", "baseline", "events", len(notifyEvents))
		}
		notifyEvents = nil
	}
	// The ping goes out only once the state it reports is saved: if saving
	// fails, the next poll finds the same events again and pings then, rather
	// than the session being told over and over.
	ping := func() {
		if len(notifyEvents) == 0 {
			return
		}
		ref := provider.ShortRef(reg.PRURL, reg.Provider)
		if finished && !reopened {
			ref += " (" + reg.Status + ")"
		}
		s.queuePing(reg.SessionID, strings.TrimPrefix(eventsMessage(ref, notifyEvents), pingPrefix))
	}

	// A PR/MR that just merged or closed gets its status set and then its
	// state saved like any other poll, as the baseline the finished-PR checks
	// compare against; without it the first of those would report everything
	// that piled up before the merge as late activity.
	newlyTerminal := !finished && (result.State == registry.StatusMerged || result.State == registry.StatusClosed)
	if result.State == registry.StatusMerged && (newlyTerminal || reg.Status == registry.StatusClosed) {
		if err := s.Store.SetMerged(pctx, reg.PRURL, result.MergedAt); err != nil {
			s.log().Error("mark merged registration", "pr_url", reg.PRURL, "error", err)
			return
		}
	}
	if newlyTerminal && result.State == registry.StatusClosed {
		if err := s.Store.SetStatus(pctx, reg.PRURL, result.State); err != nil {
			s.log().Error("mark resolved registration", "pr_url", reg.PRURL, "error", err)
			return
		}
	}

	// Finished ones are only watched for late activity: check them no more
	// often than FinishedPollInterval, whatever the open ones' pacing is.
	if newlyTerminal || (finished && !reopened) {
		interval = max(interval, FinishedPollInterval)
	}

	// A review dismiss_review couldn't clear on GitHub itself (re-requested,
	// or the local-only fallback) stays suppressed here: GitHub keeps
	// reporting the exact same review every poll, so without this check it
	// would resurface on the very next tick, undoing the dismissal.
	// ReviewCount is left untouched either way, so no spurious REVIEW event
	// fires from Diff (above) for a review it's already seen.
	latestReviewState, latestReviewer, latestReviewAt, reviewHref := result.LatestReviewState, result.LatestReviewer, result.LatestReviewAt, result.ReviewHref
	if reg.DismissedReviewHref != "" && result.ReviewHref == reg.DismissedReviewHref {
		latestReviewState, latestReviewer, latestReviewAt, reviewHref = "", "", time.Time{}, ""
	}

	update := registry.StateUpdate{
		ReviewCount:         result.ReviewCount,
		CIBucket:            result.CIBucket,
		CIWasFailing:        provider.NextCIWasFailing(prev, result),
		CIDetail:            result.CIDetail,
		CIHref:              result.CIHref,
		ReviewHref:          reviewHref,
		UnresolvedThreadIDs: result.UnresolvedThreadIDs,
		TopLevelCommentIDs:  result.TopLevelCommentIDs,
		ThreadCommentIDs:    result.ThreadCommentIDs,
		ThreadCommentsKnown: true,
		LatestCommentHref:   result.LatestCommentHref,
		LatestReviewState:   latestReviewState,
		LatestReviewer:      latestReviewer,
		LatestReviewAt:      latestReviewAt,
		Title:               result.Title,
		Description:         result.Description,
		NextDueAt:           time.Now().Add(interval),
		HasConflict:         result.HasConflict,
		MergeQueueState:     result.MergeQueueState,
		MergeQueuePosition:  result.MergeQueuePosition,
		Blocked:             result.Blocked,
		HeadSHA:             reg.HeadSHA,
		IsDraft:             reg.IsDraft,
		RequestedReviewers:  reg.RequestedReviewers,
		SelfReviewState:     result.SelfReviewState,
	}
	// A result that carries no head (a finished GitLab MR's early return)
	// keeps the stored baseline rather than erasing it.
	if result.HeadSHA != "" {
		update.HeadSHA, update.IsDraft, update.RequestedReviewers = result.HeadSHA, result.IsDraft, result.RequestedReviewers
	}
	if err := s.Store.UpdateState(pctx, reg.PRURL, reg.SessionID, update); err != nil {
		s.log().Error("persist new state", "pr_url", reg.PRURL, "error", err)
		return
	}
	if silentFirst {
		if err := s.Store.ClearFirstPollSilent(pctx, reg.PRURL, reg.SessionID); err != nil {
			s.log().Error("clear first poll flag", "pr_url", reg.PRURL, "session_id", reg.SessionID, "error", err)
		}
	}
	ping()
}

// dedupeURLs returns the distinct PR/MR URLs among regs, first-seen order —
// several registrations can now share one URL (multiple sessions
// independently watching the same PR/MR), and each URL should only be
// checked once regardless of how many registrations reference it.
func dedupeURLs(regs []registry.Registration) []string {
	seen := make(map[string]bool, len(regs))
	urls := make([]string, 0, len(regs))
	for _, reg := range regs {
		if seen[reg.PRURL] {
			continue
		}
		seen[reg.PRURL] = true
		urls = append(urls, reg.PRURL)
	}
	return urls
}

// filterEvents keeps only the events whose Kind appears in filter, preserving
// order — used to apply a registration's own EventFilter to one poll's
// events before they're turned into a notification. An empty/nil filter
// (the default: no filter registered) passes every event through except the
// opt-in kinds (provider.OptInEventKinds), which only a filter naming them
// lets through. CI_SUCCESS is also dropped when CI_RECOVERED is sent in the
// same poll: both say CI is green again, and the session is told once.
func filterEvents(events []provider.Event, filter []string) []provider.Event {
	allowed := make(map[string]bool, len(filter))
	for _, kind := range filter {
		allowed[kind] = true
	}
	var out []provider.Event
	for _, ev := range events {
		if len(filter) == 0 {
			if !provider.OptInEventKinds[ev.Kind] {
				out = append(out, ev)
			}
		} else if allowed[ev.Kind] {
			out = append(out, ev)
		}
	}
	return dropSuccessAfterRecovery(out)
}

// dropSuccessAfterRecovery removes CI_SUCCESS when CI_RECOVERED is present.
func dropSuccessAfterRecovery(events []provider.Event) []provider.Event {
	if !slices.ContainsFunc(events, func(e provider.Event) bool { return e.Kind == "CI_RECOVERED" }) {
		return events
	}
	return slices.DeleteFunc(slices.Clone(events), func(e provider.Event) bool { return e.Kind == "CI_SUCCESS" })
}

// selfAuthor resolves the account whose own reviews/actions should never
// trigger a notification for reg — the worker's own GitHub/GitLab login,
// since the agent acts through the same token the human's `gh`/`glab` CLI
// already uses, so "someone else reviewed this" and "I (or an agent acting
// as me) reviewed this" are otherwise indistinguishable from a bare login
// string. Resolved lazily via the checker's own cache (see
// provider.GitHubChecker.SelfLogin / GitLabChecker.SelfUsername), so this
// costs a real lookup once, not once per poll. A resolution failure (no
// token, network error, unparseable pr_url) reports "" — filtering simply
// stays inactive rather than blocking the poll on an identity check that
// isn't essential to it.
func (s *Scheduler) selfAuthor(ctx context.Context, reg registry.Registration) string {
	switch reg.Provider {
	case "github":
		login, err := s.GitHub.SelfLogin(ctx)
		if err != nil {
			s.log().Debug("resolve github self login", "error", err)
			return ""
		}
		return login
	case "gitlab":
		host, _, _, err := provider.ParseGitLabMRURL(reg.PRURL)
		if err != nil {
			return ""
		}
		username, err := s.GitLab.SelfUsername(ctx, host)
		if err != nil {
			s.log().Debug("resolve gitlab self username", "host", host, "error", err)
			return ""
		}
		return username
	}
	return ""
}

// filterSelfAuthored drops a REVIEW event whose reviewer is selfAuthor, or a
// top-level-comment COMMENT event whose author is selfAuthor — the worker's
// own account acting on a PR/MR (directly, or an agent acting on its
// behalf, since both share one token) isn't news to the session that
// watches it; the state still updates and the badge still reflects it,
// only the ping is suppressed. Confirmed live: a self-authored top-level
// comment (agent noting a stale label needed no action) pinged same as any
// other — self-filtering only ever covered REVIEW, never COMMENT, for
// either kind of comment. An empty selfAuthor (identity couldn't be
// resolved) passes every event through unchanged.
//
// A thread-based COMMENT event (a new unresolved review thread) is never
// suppressed here — GitHub's reviewThreads/GitLab's discussions don't carry
// a per-thread author today, so there's nothing to check yet. The
// top-level-comment case is distinguished from it by Href, not Kind (both
// share Kind "COMMENT"): a thread event's Href is the review's own href
// (cur.ReviewHref in Diff), a top-level comment's is
// result.LatestCommentHref — matching against the latter, guarded by it
// being non-empty, is enough to tell them apart without parsing
// Event.Message. Same "at most one of this kind per poll" reasoning as
// REVIEW's result.LatestReviewer check below applies to
// result.LatestCommentAuthor.
// filterReviewRequests narrows REVIEW_REQUESTED to the requests that are for
// the worker's own account (the session reviewing on your behalf): a request
// for someone else is not news to it. The message then says "you". Without a
// known own account nothing can be told to be for it, so the events go.
func filterReviewRequests(events []provider.Event, prev provider.PrevState, result provider.CheckResult, self string) []provider.Event {
	var out []provider.Event
	for _, ev := range events {
		if ev.Kind != "REVIEW_REQUESTED" {
			out = append(out, ev)
			continue
		}
		if self == "" || !slices.ContainsFunc(result.RequestedReviewers, func(r string) bool { return strings.EqualFold(r, self) }) ||
			slices.ContainsFunc(prev.RequestedReviewers, func(r string) bool { return strings.EqualFold(r, self) }) {
			continue
		}
		ev.Message = "review requested from you"
		out = append(out, ev)
	}
	return out
}

func filterSelfAuthored(events []provider.Event, result provider.CheckResult, selfAuthor string) []provider.Event {
	if selfAuthor == "" {
		return events
	}
	var out []provider.Event
	for _, ev := range events {
		if ev.Kind == "REVIEW" && result.LatestReviewer == selfAuthor {
			continue
		}
		if ev.Kind == "COMMENT" && result.LatestCommentHref != "" && ev.Href == result.LatestCommentHref && result.LatestCommentAuthor == selfAuthor {
			continue
		}
		out = append(out, ev)
	}
	return out
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

// formatEventBody renders one provider.Event's text, without the shared
// "[pr-watch] " prefix — shared by eventMessage (a single event) and
// eventsMessage (several combined under one prefix instead of repeating it
// per line). provider.Diff's terminal events (MERGED, CLOSED) set Message
// equal to Kind, so sending Message alone is already self-describing; its
// non-terminal events (CI, REVIEW, COMMENT) set Message to just the changed
// value (e.g. CIBucket "failure", or "alice APPROVED"), so the Kind is
// prefixed to keep it self-describing without duplicating it for the
// terminal cases. When the event names a source (a comment, review, or CI
// job/run), its Href follows in parens so the link is one tap away.
func formatEventBody(ev provider.Event) string {
	body := ev.Message
	if ev.Kind != ev.Message {
		body = ev.Kind + " " + ev.Message
	}
	if ev.Href != "" {
		body += " (" + ev.Href + ")"
	}
	return body
}

// pingPrefix marks every message as an automated ping rather than a human
// one.
const pingPrefix = "[pr-watch] "

// persistTimeout bounds one registration's state save, which runs even after
// the poll's own context has expired.
const persistTimeout = 10 * time.Second

// discoveryTimeout bounds the periodic discovery pass, which runs on a
// context of its own (see Tick).
const discoveryTimeout = 30 * time.Second

// sendTimeout bounds one ping's delivery, matching the host's own bound on a
// message call.
const sendTimeout = 30 * time.Second

// queuePing holds one registration's update (a message body without the
// prefix) until the end of the Tick.
func (s *Scheduler) queuePing(sessionID, section string) {
	if s.pings == nil {
		s.pings = make(map[string][]string)
	}
	if _, ok := s.pings[sessionID]; !ok {
		s.pingOrder = append(s.pingOrder, sessionID)
	}
	s.pings[sessionID] = append(s.pings[sessionID], section)
}

// flushPings sends each session everything that happened across all its
// PRs/MRs this Tick as one message. Separate pings queue up as separate
// turns in the agent (typically while a long command is running), and each
// then has to be read and answered on its own; one combined message reads as
// a single thing to react to. Best-effort, same as before: a session that
// can't receive (archived, unreachable) is logged, never retried, so its
// registrations' state still advances.
func (s *Scheduler) flushPings(ctx context.Context) {
	order := s.pingOrder
	pings := s.pings
	s.pings, s.pingOrder = nil, nil
	for _, sessionID := range order {
		sections := pings[sessionID]
		var msg string
		if len(sections) == 1 {
			msg = pingPrefix + sections[0]
		} else {
			msg = fmt.Sprintf("%s%d updates:\n\n%s", pingPrefix, len(sections), strings.Join(sections, "\n\n"))
		}
		// The state these report is already saved, so they must go out even if
		// the poll's deadline has passed in the meantime; otherwise they are
		// lost for good.
		sendCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sendTimeout)
		err := s.Notify.Send(sendCtx, sessionID, msg)
		cancel()
		if err != nil {
			s.log().Warn("notify failed", "session_id", sessionID, "updates", len(sections), "error", err)
			continue
		}
		s.log().Info("ping sent", "session_id", sessionID, "updates", len(sections), "bytes", len(msg))
	}
}

// eventMessage renders a single provider.Event as the text sent to the
// session, prefixed with "[pr-watch] <ref>: " so it reads as an automated
// ping rather than a human message in the session, and — since a session
// can watch more than one PR/MR at once — names which one this is about.
func eventMessage(ref string, ev provider.Event) string {
	return pingPrefix + ref + ": " + formatEventBody(ev)
}

// eventsMessage combines every event from one poll into a single ping, so a
// registration that changed in several ways at once (e.g. a new review
// landing right as CI turns red) produces one message to react to instead
// of a flurry of separate ones landing back-to-back. A single event formats
// exactly as eventMessage; several are listed one per line under one
// shared "[pr-watch] <ref>, N changes:" header instead. Empty input reports
// "" — finish only calls Send at all when there's at least one event.
func eventsMessage(ref string, events []provider.Event) string {
	if len(events) == 0 {
		return ""
	}
	if len(events) == 1 {
		return eventMessage(ref, events[0])
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s%s, %d changes:", pingPrefix, ref, len(events))
	for _, ev := range events {
		b.WriteString("\n- ")
		b.WriteString(formatEventBody(ev))
	}
	return b.String()
}
