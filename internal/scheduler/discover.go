package scheduler

import (
	"context"
	"strings"
	"time"

	"github.com/cwrau/aoe-plugin-pr-watch-mcp/internal/aoeclient"
	"github.com/cwrau/aoe-plugin-pr-watch-mcp/internal/provider"
	"github.com/cwrau/aoe-plugin-pr-watch-mcp/internal/registry"
)

// discoverTick is the auto-watch discovery pass: for every live session's
// workspace checkout, keep registry's auto_discovery table in sync (so a
// repo that left the workspace, or a session that's gone, stops being
// probed), then ask the provider about every still-unresolved target —
// "does this branch have an open PR/MR yet". A hit registers the discovered
// PR/MR exactly as if the session had called the register tool itself (see
// finishDiscovery) — there is no separate "auto" bookkeeping; once
// registered, it's an ordinary registration in every respect, including
// that an agent unregistering it is never silently undone by a later
// discovery pass finding the same branch again (finishDiscovery only
// registers when no row exists at all, regardless of status).
//
// Unlike the real registration checks above, this doesn't apply
// skipForExhaustedBudget before spending a call: the discovery pass costs
// at most one batched GitHub call and one GitLab call per due target per
// tick, so an occasional wasted attempt against an already-exhausted pool
// is a minor, self-correcting cost (the very next real check still sees
// and respects the same shared budget state) rather than one worth adding
// a second budget-aware code path for. There is no per-target pacing
// either: how often a target gets probed is how often a pass runs
// (Scheduler.DiscoveryInterval for this periodic one; a debounced idle event
// for DiscoverSessions).
func (s *Scheduler) discoverTick(ctx context.Context, now time.Time) {
	live := s.Workspaces.LiveWorkspaces()

	liveSessionIDs := make(map[string]bool, len(live))
	for sessionID, repos := range live {
		liveSessionIDs[sessionID] = true
		s.syncSessionTargets(ctx, sessionID, repos)
	}

	// Sweep every tracked session that isn't live anymore (gone, archived,
	// or just never had a repo this tick) — otherwise its targets sit
	// forever, still due, still spending real calls for a session that will
	// never register anything.
	tracked, err := s.Store.DistinctAutoDiscoverySessionIDs(ctx)
	if err != nil {
		s.log().Error("list auto-discovery sessions", "error", err)
	} else {
		for _, sessionID := range tracked {
			if !liveSessionIDs[sessionID] {
				if err := s.Store.DeleteAutoDiscoveryForSession(ctx, sessionID); err != nil {
					s.log().Error("delete auto-discovery for gone session", "session_id", sessionID, "error", err)
				}
			}
		}
	}

	liveIDs := make([]string, 0, len(liveSessionIDs))
	for id := range liveSessionIDs {
		liveIDs = append(liveIDs, id)
	}
	targets, err := s.Store.UnresolvedAutoDiscoveryForSessions(ctx, liveIDs)
	if err != nil {
		s.log().Error("query unresolved auto-discovery targets", "error", err)
		return
	}
	s.probeTargets(ctx, targets, now)
}

// DiscoverSessions is the event-driven discovery path: one pass for just
// sessionIDs (the worker calls it when AoE reports a session went idle —
// the moment an agent has likely just pushed and opened a PR), probing
// every unresolved target of those sessions immediately, regardless of the
// pacing the slow periodic pass left on them. It reuses the last Tick's
// session snapshot, refreshing it only if a notified session isn't in it
// yet (created since the last Tick). Like
// Tick it must only be called from the scheduler loop goroutine — it
// shares the same unsynchronized budget caches.
func (s *Scheduler) DiscoverSessions(ctx context.Context, sessionIDs []string) error {
	if s.Workspaces == nil || s.Remotes == nil || len(sessionIDs) == 0 {
		return nil
	}
	// The idle event carries only a session id; checkouts come from the
	// session directory, which lifecycle events and the periodic resync keep
	// current. Only a session missing from it (its created event not yet
	// seen) is worth a forced re-listing.
	live := s.Workspaces.LiveWorkspaces()
	for _, id := range sessionIDs {
		if _, known := live[id]; !known {
			if err := s.Sessions.Refresh(ctx); err != nil {
				return err
			}
			live = s.Workspaces.LiveWorkspaces()
			break
		}
	}

	var liveIDs []string
	for _, id := range sessionIDs {
		repos, ok := live[id]
		if !ok {
			continue
		}
		liveIDs = append(liveIDs, id)
		s.syncSessionTargets(ctx, id, repos)
	}

	targets, err := s.Store.UnresolvedAutoDiscoveryForSessions(ctx, liveIDs)
	if err != nil {
		return err
	}
	s.probeTargets(ctx, targets, time.Now())
	return nil
}

// syncSessionTargets brings sessionID's auto_discovery rows in line with
// its current checkouts: upsert each resolvable one (a branch change resets
// its state), prune rows for repos that left the workspace.
func (s *Scheduler) syncSessionTargets(ctx context.Context, sessionID string, repos []aoeclient.WorkspaceRepo) {
	keepPaths := make([]string, 0, len(repos))
	for _, repo := range repos {
		// branch comes from Resolve's own live git read, not repo.Branch —
		// see RemoteResolver's doc comment for why WorkspaceSource's
		// cached branch can't be trusted here.
		prov, remoteKey, branch, ok := s.Remotes.Resolve(ctx, repo.SourcePath)
		if !ok {
			continue
		}
		keepPaths = append(keepPaths, repo.SourcePath)
		if err := s.Store.UpsertAutoDiscoveryTarget(ctx, sessionID, repo.SourcePath, prov, remoteKey, branch); err != nil {
			s.log().Error("upsert auto-discovery target", "session_id", sessionID, "source_path", repo.SourcePath, "error", err)
		}
		if fr, ok := s.Remotes.(ForkRemoteResolver); ok && prov == "github" {
			extra, owners := fr.ExtraRemotes(ctx, repo.SourcePath)
			if err := s.Store.SetAutoDiscoveryRemotes(ctx, sessionID, repo.SourcePath, extra, owners); err != nil {
				s.log().Error("set auto-discovery remotes", "session_id", sessionID, "source_path", repo.SourcePath, "error", err)
			}
		}
	}
	if err := s.Store.PruneAutoDiscoveryForSession(ctx, sessionID, keepPaths); err != nil {
		s.log().Error("prune auto-discovery for session", "session_id", sessionID, "error", err)
	}
}

// probeTargets asks the provider about every target in due: GitHub in one
// batched call, GitLab one call per target.
func (s *Scheduler) probeTargets(ctx context.Context, due []registry.AutoDiscoveryTarget, now time.Time) {
	if len(due) == 0 {
		return
	}
	var githubDue []registry.AutoDiscoveryTarget
	var gitlabDue []registry.AutoDiscoveryTarget
	for _, t := range due {
		switch t.Provider {
		case "github":
			githubDue = append(githubDue, t)
		case "gitlab":
			gitlabDue = append(gitlabDue, t)
		default:
			s.log().Error("auto-discovery target has unknown provider", "session_id", t.SessionID, "source_path", t.SourcePath, "provider", t.Provider)
		}
	}

	s.discoverGitHub(ctx, githubDue, now)
	for _, t := range gitlabDue {
		s.discoverGitLabOne(ctx, t, now)
	}
}

// discoverGitHub resolves every unresolved GitHub auto-discovery target in one
// batched call (see provider.GitHubChecker.DiscoverBranchPRs) — the same
// dedup-by-repo shape processGitHubBatch already uses for real checks, so
// several sessions with checkouts of the same repo (different branches)
// still cost one request, not one per session.
func (s *Scheduler) discoverGitHub(ctx context.Context, due []registry.AutoDiscoveryTarget, now time.Time) {
	if len(due) == 0 {
		return
	}
	items := make([]provider.GitHubBranchQuery, 0, len(due))
	for _, t := range due {
		items = append(items, githubQueriesFor(t)...)
	}

	found, budget, err := s.GitHub.DiscoverBranchPRs(ctx, items)
	if err != nil {
		s.log().Warn("github branch discovery failed, retrying on the next pass", "count", len(due), "error", err)
		return
	}
	if budget.Limit > 0 {
		s.lastGitHubBudgets.GraphQL = budget
	}

	for _, t := range due {
		// The checkout's own repo wins over its upstream ones, in the order
		// githubQueriesFor lists them.
		var prURL string
		var foundOne bool
		for _, q := range githubQueriesFor(t) {
			if prURL, foundOne = found[provider.GitHubBranchKey(q.Owner, q.Repo, q.Branch)]; foundOne {
				break
			}
		}
		s.finishDiscovery(ctx, t, prURL, foundOne, now)
	}
}

// githubQueriesFor is the branch lookups for one target: the repo the
// checkout pushes to, restricted to PRs from that repo itself (anyone can
// open a PR from their own fork under the same branch name, and it is not
// this session's), then each extra remote's repo restricted to PRs from the
// checkout's own remote owners.
func githubQueriesFor(t registry.AutoDiscoveryTarget) []provider.GitHubBranchQuery {
	var out []provider.GitHubBranchQuery
	if owner, repo, ok := strings.Cut(t.RemoteKey, "/"); ok {
		out = append(out, provider.GitHubBranchQuery{Owner: owner, Repo: repo, Branch: t.Branch, HeadOwners: []string{owner}})
	}
	for _, key := range t.ExtraRemoteKeys {
		if owner, repo, ok := strings.Cut(key, "/"); ok {
			out = append(out, provider.GitHubBranchQuery{Owner: owner, Repo: repo, Branch: t.Branch, HeadOwners: t.HeadOwners})
		}
	}
	return out
}

// discoverGitLabOne resolves one unresolved GitLab auto-discovery target —
// GitLab has no aliased-batch equivalent, so this stays one REST call per
// target, same as discovery's GitHub leg costs one per distinct repo.
func (s *Scheduler) discoverGitLabOne(ctx context.Context, t registry.AutoDiscoveryTarget, now time.Time) {
	host, path, ok := strings.Cut(t.RemoteKey, "/")
	if !ok {
		return
	}
	prURL, found, budget, err := s.GitLab.DiscoverBranchMR(ctx, host, path, t.Branch)
	if err != nil {
		s.log().Warn("gitlab branch discovery failed, retrying on the next pass", "session_id", t.SessionID, "source_path", t.SourcePath, "error", err)
		return
	}
	if budget.Limit > 0 {
		if s.lastGitLabBudgets == nil {
			s.lastGitLabBudgets = make(map[string]provider.RateBudget)
		}
		s.lastGitLabBudgets[host] = budget
	}
	s.finishDiscovery(ctx, t, prURL, found, now)
}

// finishDiscovery applies one auto-discovery target's answer. found=false
// (no open PR/MR yet) leaves the target unresolved, so the next pass asks
// again. found=true
// registers prURL for t.SessionID — but only if no registration already
// exists for that exact (pr_url, session_id) pair, regardless of its
// status: registry.Store.Add forces status back to watching on every call,
// so blindly re-Adding an already-stopped row here would silently undo an
// agent's own unregister the very next time this branch gets probed. Either
// way the target is marked resolved for this branch — once the answer is
// known (found or not-yet-found-but-registration-already-exists), there's
// nothing left this target can teach us until the branch itself changes.
// A genuine "not found yet" instead stays unresolved, so it keeps getting
// probed on every pass until a PR/MR actually appears.
func (s *Scheduler) finishDiscovery(ctx context.Context, t registry.AutoDiscoveryTarget, prURL string, found bool, now time.Time) {
	if !found {
		return
	}

	_, exists, err := s.Store.Get(ctx, prURL, t.SessionID)
	if err != nil {
		s.log().Error("check existing registration for auto-discovered PR", "pr_url", prURL, "session_id", t.SessionID, "error", err)
		return
	}
	if !exists {
		reg := registry.Registration{
			PRURL:     prURL,
			SessionID: t.SessionID,
			Provider:  t.Provider,
			CIBucket:  "none",
			NextDueAt: now,
			CreatedAt: now,
		}
		if err := s.Store.Add(ctx, reg); err != nil {
			s.log().Error("auto-register discovered PR", "pr_url", prURL, "session_id", t.SessionID, "error", err)
			return
		}
		s.log().Info("auto-discovered PR", "pr_url", prURL, "session_id", t.SessionID, "branch", t.Branch)
	}
	if err := s.Store.MarkAutoDiscoveryResolved(ctx, t.SessionID, t.SourcePath, t.Branch); err != nil {
		s.log().Error("mark auto-discovery resolved", "session_id", t.SessionID, "source_path", t.SourcePath, "error", err)
	}
}
