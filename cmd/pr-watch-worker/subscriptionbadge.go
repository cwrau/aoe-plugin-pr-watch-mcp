package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/cwrau/aoe-plugin-pr-watch-mcp/internal/provider"
	"github.com/cwrau/aoe-plugin-pr-watch-mcp/internal/registry"
)

// loadSubscriptionBadgeInfos reads what the row badges say about subscriptions:
// one entry per active subscription. It also returns every session that has
// ever subscribed (stopped ones included), so a badge whose subscriptions all
// ended can be removed.
func loadSubscriptionBadgeInfos(ctx context.Context, store *registry.Store) ([]subscriptionBadgeInfo, []string, error) {
	subs, err := store.ListSubscriptions(ctx)
	if err != nil {
		return nil, nil, err
	}
	var infos []subscriptionBadgeInfo
	seen := make(map[string]bool)
	var sessions []string
	for _, s := range subs {
		if !seen[s.SessionID] {
			seen[s.SessionID] = true
			sessions = append(sessions, s.SessionID)
		}
		if s.Status != registry.SubscriptionActive {
			continue
		}
		tracked, err := store.TrackedMatches(ctx, s.ID)
		if err != nil {
			return nil, nil, err
		}
		watches, err := store.CountSubscriptionWatches(ctx, s.ID)
		if err != nil {
			return nil, nil, err
		}
		var filters provider.ScanFilters
		if err := json.Unmarshal([]byte(s.Filters), &filters); err != nil {
			return nil, nil, fmt.Errorf("subscription %s: stored filters: %w", s.ID, err)
		}
		scope := s.Host
		if s.Host != "" && s.ScopePath != "" {
			scope = s.Host + "/" + s.ScopePath
		}
		mode := s.Mode
		if mode == "" {
			mode = registry.SubscriptionModeWatch
		}
		infos = append(infos, subscriptionBadgeInfo{
			SessionID: s.SessionID, ID: s.ID, Scope: scope, Mode: mode, Filters: filters,
			Tracked: len(tracked), Watches: watches, LastScanAt: s.LastScanAt, LastError: s.LastError,
		})
	}
	return infos, sessions, nil
}

// loadTrackedBoardCards reads the PRs/MRs that active notify-mode
// subscriptions have announced to their sessions, for the board's "New, not
// watched" column.
func loadTrackedBoardCards(ctx context.Context, store *registry.Store) ([]trackedCard, error) {
	subs, err := store.ListSubscriptions(ctx)
	if err != nil {
		return nil, err
	}
	var cards []trackedCard
	for _, s := range subs {
		if s.Status != registry.SubscriptionActive || s.Mode != registry.SubscriptionModeNotify {
			continue
		}
		tracked, err := store.TrackedMatches(ctx, s.ID)
		if err != nil {
			return nil, err
		}
		for url, draft := range tracked {
			cards = append(cards, trackedCard{SessionID: s.SessionID, Provider: s.Provider, URL: url, Draft: draft})
		}
	}
	return cards, nil
}
