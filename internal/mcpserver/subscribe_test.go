package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/cwrau/aoe-plugin-pr-watch-mcp/internal/registry"
)

type fakeScopeResolver struct {
	kinds map[string]string
	err   error
}

func (f fakeScopeResolver) ResolveScopeKind(ctx context.Context, host, path string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	if k, ok := f.kinds[host+"/"+path]; ok {
		return k, nil
	}
	return "", fmt.Errorf("%s/%s not found", host, path)
}

func subscribeServer(t *testing.T, resolver ScopeResolver) (*httptest.Server, *registry.Store) {
	t.Helper()
	store := openTestStore(t)
	srv := httptest.NewServer(NewHandler(store, &fakeGitHubReviewClient{}, &fakeSessionValidator{}, &fakeUIPusher{}, WithScopeResolver(resolver)))
	t.Cleanup(srv.Close)
	return srv, store
}

func onlySubscription(t *testing.T, store *registry.Store) registry.Subscription {
	t.Helper()
	subs, err := store.ListSubscriptions(context.Background())
	if err != nil || len(subs) != 1 {
		t.Fatalf("ListSubscriptions() = (%+v, %v), want exactly one", subs, err)
	}
	return subs[0]
}

func TestSubscribeStoresAScopeWithFiltersAndDefaults(t *testing.T) {
	srv, store := subscribeServer(t, fakeScopeResolver{})
	res := callTool(t, srv, "subscribe", map[string]any{
		"scope_url": "https://github.com/acme/widgets", "session_id": "sess1",
		"filters": map[string]any{"review_requested": "me", "draft": "exclude", "labels_not": []string{"wip"}},
	})
	sub := onlySubscription(t, store)
	if sub.SessionID != "sess1" || sub.Provider != "github" || sub.Host != "github.com" || sub.ScopeKind != "repo" || sub.ScopePath != "acme/widgets" {
		t.Errorf("subscription = %+v", sub)
	}
	if sub.Persona != registry.PersonaReviewer || sub.MaxWatches != 25 || sub.Status != registry.SubscriptionActive {
		t.Errorf("defaults = persona %q, max %d, status %q; want reviewer, 25, active", sub.Persona, sub.MaxWatches, sub.Status)
	}
	var f map[string]any
	if err := json.Unmarshal([]byte(sub.Filters), &f); err != nil || f["review_requested"] != "me" || f["draft"] != "exclude" {
		t.Errorf("stored filters = %s (%v)", sub.Filters, err)
	}
	if text := resultText(res); !strings.Contains(text, sub.ID) {
		t.Errorf("result %q does not name the subscription id %q", text, sub.ID)
	}
}

func resultText(r *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range r.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

func TestSubscribePlatformNeedsAPersonFilterAndGetsASmallerCap(t *testing.T) {
	srv, store := subscribeServer(t, fakeScopeResolver{})
	callToolExpectError(t, srv, "subscribe", map[string]any{"scope_url": "https://github.com", "session_id": "sess1", "filters": map[string]any{"labels": []string{"bug"}}})
	if subs, _ := store.ListSubscriptions(context.Background()); len(subs) != 0 {
		t.Fatalf("a bare platform subscription was stored: %+v", subs)
	}
	callTool(t, srv, "subscribe", map[string]any{"scope_url": "https://github.com", "session_id": "sess1", "filters": map[string]any{"review_requested": "me"}})
	if sub := onlySubscription(t, store); sub.ScopeKind != "platform" || sub.MaxWatches != 10 {
		t.Errorf("subscription = %+v, want a platform scope capped at 10", sub)
	}
}

func TestSubscribeResolvesGitLabProjectsAndGroups(t *testing.T) {
	srv, store := subscribeServer(t, fakeScopeResolver{kinds: map[string]string{"gitlab.corp.example/g/sub": "org", "gitlab.corp.example/g/p": "repo"}})
	callTool(t, srv, "subscribe", map[string]any{"scope_url": "https://gitlab.corp.example/g/sub", "session_id": "sess1", "filters": map[string]any{"review_requested": "me"}})
	if sub := onlySubscription(t, store); sub.Provider != "gitlab" || sub.ScopeKind != "org" || sub.ScopePath != "g/sub" || sub.Host != "gitlab.corp.example" {
		t.Errorf("subscription = %+v", sub)
	}
	callToolExpectError(t, srv, "subscribe", map[string]any{"scope_url": "https://gitlab.corp.example/nope/none", "session_id": "sess1"})
}

func TestSubscribeRejectsBadInput(t *testing.T) {
	srv, store := subscribeServer(t, fakeScopeResolver{kinds: map[string]string{"gitlab.corp.example/g/p": "repo"}})
	base := func(mut func(map[string]any)) map[string]any {
		in := map[string]any{"scope_url": "https://github.com/acme/widgets", "session_id": "sess1"}
		mut(in)
		return in
	}
	for name, in := range map[string]map[string]any{
		"no session":      base(func(m map[string]any) { delete(m, "session_id") }),
		"bad scope":       base(func(m map[string]any) { m["scope_url"] = "ftp://x" }),
		"bad persona":     base(func(m map[string]any) { m["persona"] = "boss" }),
		"bad event":       base(func(m map[string]any) { m["events"] = []string{"NOPE"} }),
		"bad draft":       base(func(m map[string]any) { m["filters"] = map[string]any{"draft": "maybe"} }),
		"max too large":   base(func(m map[string]any) { m["max_watches"] = 101 }),
		"max negative":    base(func(m map[string]any) { m["max_watches"] = -1 }),
		"too long query":  base(func(m map[string]any) { m["filters"] = map[string]any{"labels": []string{strings.Repeat("x", 300)}} }),
		"gitlab involved": {"scope_url": "https://gitlab.corp.example/g/p", "session_id": "sess1", "filters": map[string]any{"involved": "me"}},
	} {
		t.Run(name, func(t *testing.T) {
			callToolExpectError(t, srv, "subscribe", in)
		})
	}
	if subs, _ := store.ListSubscriptions(context.Background()); len(subs) != 0 {
		t.Errorf("rejected input still stored %+v", subs)
	}
}

func TestUnsubscribeStopsTheSubscriptionAndItsWatchesForTheOwnerOnly(t *testing.T) {
	srv, store := subscribeServer(t, fakeScopeResolver{})
	callTool(t, srv, "subscribe", map[string]any{"scope_url": "https://github.com/acme/widgets", "session_id": "sess1"})
	sub := onlySubscription(t, store)
	ctx := context.Background()
	reg := registry.Registration{PRURL: "https://github.com/acme/widgets/pull/1", SessionID: "sess1", Provider: "github", SubscriptionID: sub.ID}
	if err := store.Add(ctx, reg); err != nil {
		t.Fatal(err)
	}

	callToolExpectError(t, srv, "unsubscribe", map[string]any{"subscription_id": sub.ID, "session_id": "sess2"})
	if got, _, _ := store.GetSubscription(ctx, sub.ID); got.Status != registry.SubscriptionActive {
		t.Fatal("another session stopped the subscription")
	}
	callTool(t, srv, "unsubscribe", map[string]any{"subscription_id": sub.ID, "session_id": "/home/u/.config/agent-of-empires/artifacts/sess1"})
	if got, _, _ := store.GetSubscription(ctx, sub.ID); got.Status != registry.SubscriptionStopped {
		t.Errorf("status = %q, want stopped", got.Status)
	}
	if got, _, _ := store.Get(ctx, reg.PRURL, "sess1"); got.Status != registry.StatusStopped {
		t.Errorf("derived watch status = %q, want stopped", got.Status)
	}
	callToolExpectError(t, srv, "unsubscribe", map[string]any{"subscription_id": sub.ID, "session_id": "sess1"})
}

func TestSubscriptionsListsByOwner(t *testing.T) {
	srv, store := subscribeServer(t, fakeScopeResolver{})
	callTool(t, srv, "subscribe", map[string]any{"scope_url": "https://github.com/acme/widgets", "session_id": "sess1", "filters": map[string]any{"review_requested": "me"}})
	callTool(t, srv, "subscribe", map[string]any{"scope_url": "https://github.com/acme/other", "session_id": "sess2"})

	res := callTool(t, srv, "subscriptions", map[string]any{"session_id": "sess1"})
	var out struct {
		Subscriptions []struct {
			ID        string         `json:"id"`
			ScopePath string         `json:"scope_path"`
			Filters   map[string]any `json:"filters"`
			Watches   int            `json:"watches"`
		} `json:"subscriptions"`
	}
	raw, _ := json.Marshal(res.StructuredContent)
	if err := json.Unmarshal(raw, &out); err != nil || len(out.Subscriptions) != 1 || out.Subscriptions[0].ScopePath != "acme/widgets" || out.Subscriptions[0].Filters["review_requested"] != "me" {
		t.Errorf("subscriptions = %s (%v), want only sess1's with decoded filters", raw, err)
	}
	all := callTool(t, srv, "subscriptions", map[string]any{})
	raw, _ = json.Marshal(all.StructuredContent)
	if !strings.Contains(string(raw), "acme/other") || !strings.Contains(string(raw), "acme/widgets") {
		t.Errorf("unfiltered list = %s", raw)
	}
	_ = store
}

func TestSubscribeNotifyModeIsStoredAndValidated(t *testing.T) {
	srv, store := subscribeServer(t, fakeScopeResolver{})
	callTool(t, srv, "subscribe", map[string]any{
		"scope_url": "https://github.com/acme/widgets", "session_id": "sess1", "mode": "notify",
		"events":  []string{"READY_FOR_REVIEW", "MERGED"},
		"filters": map[string]any{"draft": "include"},
	})
	sub := onlySubscription(t, store)
	if sub.Mode != registry.SubscriptionModeNotify || len(sub.EventFilter) != 2 {
		t.Errorf("subscription = %+v, want notify mode with the two status events", sub)
	}

	for name, in := range map[string]map[string]any{
		"unknown mode":                    {"mode": "spy"},
		"ci event in notify mode":         {"mode": "notify", "events": []string{"CI"}},
		"release is a watch-mode setting": {"mode": "notify", "release_when_unmatched": true},
	} {
		t.Run(name, func(t *testing.T) {
			in["scope_url"], in["session_id"] = "https://github.com/acme/widgets", "sess1"
			callToolExpectError(t, srv, "subscribe", in)
		})
	}
	if subs, _ := store.ListSubscriptions(context.Background()); len(subs) != 1 {
		t.Errorf("rejected input stored %d extra subscriptions", len(subs)-1)
	}

	callTool(t, srv, "subscribe", map[string]any{"scope_url": "https://github.com/acme/other", "session_id": "sess1"})
	subs, _ := store.ListSubscriptions(context.Background())
	if subs[1].Mode != registry.SubscriptionModeWatch {
		t.Errorf("default mode = %q, want watch", subs[1].Mode)
	}
}

func TestSubscriptionsShowsModeAndTrackedCount(t *testing.T) {
	srv, store := subscribeServer(t, fakeScopeResolver{})
	callTool(t, srv, "subscribe", map[string]any{"scope_url": "https://github.com/acme/widgets", "session_id": "sess1", "mode": "notify"})
	sub := onlySubscription(t, store)
	_ = store.SetTrackedMatch(context.Background(), sub.ID, "https://github.com/acme/widgets/pull/1", false)
	_ = store.SetTrackedMatch(context.Background(), sub.ID, "https://github.com/acme/widgets/pull/2", true)

	res := callTool(t, srv, "subscriptions", map[string]any{"session_id": "sess1"})
	raw, _ := json.Marshal(res.StructuredContent)
	var out struct {
		Subscriptions []struct {
			Mode    string `json:"mode"`
			Tracked int    `json:"tracked"`
			Watches int    `json:"watches"`
		} `json:"subscriptions"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || len(out.Subscriptions) != 1 || out.Subscriptions[0].Mode != "notify" || out.Subscriptions[0].Tracked != 2 || out.Subscriptions[0].Watches != 0 {
		t.Errorf("subscriptions = %s (%v), want mode notify, 2 tracked, 0 watches", raw, err)
	}
}
