package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/cwrau/aoe-plugin-pr-watch-mcp/internal/registry"
)

func TestLoadSubscriptionBadgeInfos(t *testing.T) {
	ctx := context.Background()
	store, err := registry.Open(filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	base := registry.Subscription{SessionID: "orch", Provider: "github", Host: "github.com", ScopeKind: "repo", ScopePath: "o/r", Filters: `{"review_requested":"me"}`, MaxWatches: 5, CreatedAt: time.Now()}
	notify := base
	notify.Mode = registry.SubscriptionModeNotify
	nid, _ := store.AddSubscription(ctx, notify)
	_ = store.SetTrackedMatch(ctx, nid, "https://github.com/o/r/pull/1", false)
	_ = store.SetTrackedMatch(ctx, nid, "https://github.com/o/r/pull/2", true)

	platform := base
	platform.SessionID, platform.ScopeKind, platform.ScopePath = "rev", "platform", ""
	wid, _ := store.AddSubscription(ctx, platform)
	if err := store.Add(ctx, registry.Registration{PRURL: "https://github.com/x/y/pull/9", SessionID: "rev", Provider: "github", NextDueAt: time.Now(), CreatedAt: time.Now(), SubscriptionID: wid}); err != nil {
		t.Fatal(err)
	}
	stopped := base
	stopped.SessionID = "gone"
	sid, _ := store.AddSubscription(ctx, stopped)
	_, _ = store.StopSubscription(ctx, sid, "gone")
	_ = store.RecordScan(ctx, nid, time.Now(), time.Now().Add(time.Minute), "boom")

	infos, sessions, err := loadSubscriptionBadgeInfos(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 2 {
		t.Fatalf("infos = %+v, want the two active subscriptions", infos)
	}
	by := map[string]subscriptionBadgeInfo{}
	for _, i := range infos {
		by[i.SessionID] = i
	}
	if i := by["orch"]; i.Scope != "github.com/o/r" || i.Mode != registry.SubscriptionModeNotify || i.Tracked != 2 || i.Filters.ReviewRequested != "me" || i.LastError != "boom" {
		t.Errorf("notify info = %+v", i)
	}
	if i := by["rev"]; i.Scope != "github.com" || i.Mode != registry.SubscriptionModeWatch || i.Watches != 1 || i.Tracked != 0 {
		t.Errorf("platform watch info = %+v", i)
	}
	seen := map[string]bool{}
	for _, s := range sessions {
		seen[s] = true
	}
	if !seen["orch"] || !seen["rev"] || !seen["gone"] {
		t.Errorf("sessions = %v, want every subscriber including the stopped one, so its badge can be removed", sessions)
	}
}
