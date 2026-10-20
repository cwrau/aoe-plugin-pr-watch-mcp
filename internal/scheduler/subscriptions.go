package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/cwrau/aoe-plugin-pr-watch-mcp/internal/aoeclient"
	"github.com/cwrau/aoe-plugin-pr-watch-mcp/internal/provider"
	"github.com/cwrau/aoe-plugin-pr-watch-mcp/internal/registry"
)

const (
	// SubscriptionScanInterval is how often a subscription is searched.
	SubscriptionScanInterval = 2 * time.Minute
	// SubscriptionErrorBackoff is how long a failed scan waits before retrying.
	SubscriptionErrorBackoff = 5 * time.Minute
	// scanResultLimit is how many matches one search asks for. It is the same
	// for every subscription so identical searches can be shared.
	scanResultLimit = 100
	// subscriptionTimeout bounds one pass over the due subscriptions.
	subscriptionTimeout = 30 * time.Second
)

// GitHubScanner searches GitHub for the pull requests of a subscription.
type GitHubScanner interface {
	SearchPullRequests(ctx context.Context, queries []string, limit int) (provider.ScanResult, error)
}

// GitLabScanner searches a GitLab host for the merge requests of a subscription.
type GitLabScanner interface {
	SearchMergeRequests(ctx context.Context, host string, searches []provider.GitLabSearch) (provider.ScanResult, error)
}

type scanOutcome struct {
	result provider.ScanResult
	err    error
}

// scanSubscriptions runs every due subscription: it searches for matching
// PRs/MRs, registers the new ones as watches for the owning session and tells
// the session about them. Identical searches (same provider and queries) are
// made once per pass and shared.
func (s *Scheduler) scanSubscriptions(ctx context.Context, now time.Time) {
	due, err := s.Store.DueSubscriptions(ctx, now)
	if err != nil {
		s.log().Error("list due subscriptions", "error", err)
		return
	}
	shared := make(map[string]scanOutcome)
	for _, sub := range due {
		s.scanSubscription(ctx, sub, now, shared)
	}
}

func (s *Scheduler) scanSubscription(ctx context.Context, sub registry.Subscription, now time.Time, shared map[string]scanOutcome) {
	switch s.Sessions.Classify(sub.SessionID) {
	case aoeclient.SessionGone:
		if _, err := s.Store.StopSubscriptionAny(ctx, sub.ID); err != nil {
			s.log().Error("stop subscription of gone session", "subscription", sub.ID, "error", err)
		} else {
			s.log().Info("stopped subscription of gone session", "subscription", sub.ID, "session_id", sub.SessionID)
		}
		return
	case aoeclient.SessionArchived:
		s.recordScan(ctx, sub, now, now.Add(FallbackInterval), "")
		return
	}

	var filters provider.ScanFilters
	if err := json.Unmarshal([]byte(sub.Filters), &filters); err != nil {
		s.failScan(ctx, sub, now, fmt.Errorf("stored filters: %w", err))
		return
	}
	scope := provider.ScanScope{Provider: sub.Provider, Host: sub.Host, Kind: sub.ScopeKind, Path: sub.ScopePath}

	var key string
	var run func() (provider.ScanResult, error)
	self := s.selfForScan(ctx, sub.Provider, sub.Host)
	switch sub.Provider {
	case "github":
		if s.GitHubScan == nil {
			return
		}
		queries, err := provider.BuildGitHubSearchQueries(scope, filters, self, now)
		if err != nil {
			s.failScan(ctx, sub, now, err)
			return
		}
		key = "github\x00" + strings.Join(queries, "\x00")
		run = func() (provider.ScanResult, error) {
			return s.GitHubScan.SearchPullRequests(ctx, queries, scanResultLimit)
		}
	case "gitlab":
		if s.GitLabScan == nil {
			return
		}
		searches, err := provider.BuildGitLabSearches(scope, filters, self, now)
		if err != nil {
			s.failScan(ctx, sub, now, err)
			return
		}
		parts := []string{"gitlab", sub.Host}
		for _, q := range searches {
			parts = append(parts, q.Endpoint+"?"+q.Query.Encode())
		}
		key = strings.Join(parts, "\x00")
		run = func() (provider.ScanResult, error) { return s.GitLabScan.SearchMergeRequests(ctx, sub.Host, searches) }
	default:
		s.failScan(ctx, sub, now, fmt.Errorf("unknown provider %q", sub.Provider))
		return
	}

	out, ok := shared[key]
	if !ok {
		out.result, out.err = run()
		shared[key] = out
	}
	if out.err != nil {
		s.failScan(ctx, sub, now, out.err)
		return
	}

	next := now.Add(SubscriptionScanInterval)
	if b := out.result.Budget; b.Limit > 0 && b.Remaining == 0 && b.ResetAt.After(next) {
		next = b.ResetAt
	}
	if !s.applyMatches(ctx, sub, filters, self, out.result) {
		s.failScan(ctx, sub, now, fmt.Errorf("registering matches failed"))
		return
	}
	s.recordScan(ctx, sub, now, next, "")
}

func (s *Scheduler) recordScan(ctx context.Context, sub registry.Subscription, now, next time.Time, errMsg string) {
	if err := s.Store.RecordScan(ctx, sub.ID, now, next, errMsg); err != nil {
		s.log().Error("record subscription scan", "subscription", sub.ID, "error", err)
	}
}

func (s *Scheduler) failScan(ctx context.Context, sub registry.Subscription, now time.Time, err error) {
	s.log().Warn("subscription scan failed", "subscription", sub.ID, "session_id", sub.SessionID, "error", err)
	s.recordScan(ctx, sub, now, now.Add(SubscriptionErrorBackoff), err.Error())
}

// selfForScan is the worker's own account on a provider, "" when it cannot be
// resolved (the scan then fails if a filter says "me").
func (s *Scheduler) selfForScan(ctx context.Context, providerName, host string) string {
	switch providerName {
	case "github":
		if s.GitHub == nil {
			return ""
		}
		login, err := s.GitHub.SelfLogin(ctx)
		if err != nil {
			s.log().Warn("cannot resolve the worker's own account for a subscription scan", "provider", "github", "error", err)
			return ""
		}
		return login
	case "gitlab":
		if s.GitLab == nil {
			return ""
		}
		username, err := s.GitLab.SelfUsername(ctx, host)
		if err != nil {
			s.log().Warn("cannot resolve the worker's own account for a subscription scan", "provider", "gitlab", "host", host, "error", err)
			return ""
		}
		return username
	}
	return ""
}

// applyMatches registers the matches of one scan and announces the new ones.
// It reports false if a registration could not be stored, so the scan is
// retried and the first-scan silence is not used up.
func (s *Scheduler) applyMatches(ctx context.Context, sub registry.Subscription, filters provider.ScanFilters, self string, res provider.ScanResult) bool {
	count, err := s.Store.CountSubscriptionWatches(ctx, sub.ID)
	if err != nil {
		s.log().Error("count subscription watches", "subscription", sub.ID, "error", err)
		return false
	}

	current := make(map[string]bool)
	var added []provider.ScanMatch
	overflow := 0
	for _, m := range res.Matches {
		if !filters.Matches(m) {
			continue
		}
		if filters.Draft == "exclude" && m.Draft || filters.Draft == "only" && !m.Draft {
			continue
		}
		// A reviewer follows other people's work: the worker's own PRs are not
		// theirs to review.
		if sub.Persona == registry.PersonaReviewer && self != "" && strings.EqualFold(m.Author, self) {
			continue
		}
		current[m.URL] = true

		if _, exists, err := s.Store.Get(ctx, m.URL, sub.SessionID); err != nil {
			s.log().Error("look up registration for a subscription match", "pr_url", m.URL, "error", err)
			return false
		} else if exists {
			continue
		}
		if count+len(added) >= sub.MaxWatches {
			overflow++
			continue
		}
		reg := registry.Registration{
			PRURL: m.URL, SessionID: sub.SessionID, Provider: sub.Provider, CIBucket: "none",
			NextDueAt: time.Now(), CreatedAt: time.Now(), EventFilter: sub.EventFilter, Persona: sub.Persona,
			SilentFirstPoll: true, SubscriptionID: sub.ID,
		}
		if err := s.Store.Add(ctx, reg); err != nil {
			s.log().Error("register a subscription match", "pr_url", m.URL, "subscription", sub.ID, "error", err)
			return false
		}
		s.log().Info("subscription matched", "subscription", sub.ID, "pr_url", m.URL, "session_id", sub.SessionID)
		added = append(added, m)
	}

	if sub.ReleaseWhenUnmatched && res.Complete {
		urls, err := s.Store.SubscriptionWatchURLs(ctx, sub.ID)
		if err != nil {
			s.log().Error("list subscription watches", "subscription", sub.ID, "error", err)
		}
		for _, u := range urls {
			if current[u] {
				continue
			}
			if _, err := s.Store.SetStatusOwnedBy(ctx, u, sub.SessionID, registry.StatusStopped); err != nil {
				s.log().Error("release an unmatched watch", "pr_url", u, "error", err)
			} else {
				s.log().Info("released watch that no longer matches", "subscription", sub.ID, "pr_url", u)
			}
		}
	}

	// The first scan is silent unless asked otherwise: the PRs that already
	// match are the baseline, not news.
	if !sub.Scanned && !sub.ReportExisting {
		return true
	}
	noun := "PR"
	if sub.Provider == "gitlab" {
		noun = "MR"
	}
	for _, m := range added {
		msg := "new " + noun
		if m.Author != "" {
			msg += " by " + m.Author
		}
		ev := provider.Event{Kind: "MATCHED", Message: msg, Href: m.URL}
		s.queuePing(sub.SessionID, strings.TrimPrefix(eventsMessage(provider.ShortRef(m.URL, sub.Provider), []provider.Event{ev}), pingPrefix))
	}
	if overflow > 0 && len(added) > 0 || overflow > 0 && !sub.Scanned {
		s.queuePing(sub.SessionID, fmt.Sprintf("%s: %d more PR/MR(s) match but are not watched (max_watches %d)", sub.ID, overflow, sub.MaxWatches))
	}
	return true
}
