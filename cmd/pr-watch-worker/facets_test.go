package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"testing"
	"time"

	"github.com/cwrau/aoe-plugin-pr-watch-mcp/internal/aoeclient"
	"github.com/cwrau/aoe-plugin-pr-watch-mcp/internal/registry"
)

func watching(mod func(*registry.Registration)) registry.Registration {
	r := registry.Registration{
		PRURL: "https://github.com/o/r/pull/1", SessionID: "s", Provider: "github",
		Status: registry.StatusWatching,
	}
	if mod != nil {
		mod(&r)
	}
	return r
}

func TestFilterValuesPerRegistrationState(t *testing.T) {
	tests := []struct {
		name string
		reg  registry.Registration
		want []string
	}{
		{"plain open github watch", watching(nil), []string{"persona:author", "pr:open", "provider:github", "review:waiting"}},
		{"gitlab provider", watching(func(r *registry.Registration) { r.Provider = "gitlab" }),
			[]string{"persona:author", "pr:open", "provider:gitlab", "review:waiting"}},
		{"draft", watching(func(r *registry.Registration) { r.IsDraft = true }),
			[]string{"persona:author", "pr:draft", "provider:github", "review:waiting"}},
		{"ci passing", watching(func(r *registry.Registration) { r.CIBucket = "success" }),
			[]string{"ci:passing", "persona:author", "pr:open", "provider:github", "review:waiting"}},
		{"ci failing", watching(func(r *registry.Registration) { r.CIBucket = "failure" }),
			[]string{"ci:failing", "persona:author", "pr:open", "provider:github", "review:waiting"}},
		{"ci running", watching(func(r *registry.Registration) { r.CIBucket = "pending" }),
			[]string{"ci:running", "persona:author", "pr:open", "provider:github", "review:waiting"}},
		{"ci action required", watching(func(r *registry.Registration) { r.CIBucket = "action_required" }),
			[]string{"ci:action-required", "persona:author", "pr:open", "provider:github", "review:waiting"}},
		{"approved", watching(func(r *registry.Registration) { r.LatestReviewState = "APPROVED" }),
			[]string{"persona:author", "pr:open", "provider:github", "review:approved"}},
		{"partial approval", watching(func(r *registry.Registration) { r.LatestReviewState = "APPROVED 1/2" }),
			[]string{"persona:author", "pr:open", "provider:github", "review:partial-approval"}},
		{"changes requested", watching(func(r *registry.Registration) { r.LatestReviewState = "CHANGES_REQUESTED" }),
			[]string{"persona:author", "pr:open", "provider:github", "review:changes-requested"}},
		{"commented", watching(func(r *registry.Registration) { r.LatestReviewState = "COMMENTED" }),
			[]string{"persona:author", "pr:open", "provider:github", "review:commented"}},
		{"unresolved comments", watching(func(r *registry.Registration) { r.UnresolvedThreadIDs = []string{"t1"} }),
			[]string{"comments:unresolved", "persona:author", "pr:open", "provider:github", "review:waiting"}},
		{"conflict", watching(func(r *registry.Registration) { r.HasConflict = true }),
			[]string{"conflicts:yes", "persona:author", "pr:open", "provider:github", "review:waiting"}},
		{"blocked", watching(func(r *registry.Registration) { r.Blocked = true }),
			[]string{"blocked:yes", "persona:author", "pr:open", "provider:github", "review:waiting"}},
		{"merge queue awaiting checks", watching(func(r *registry.Registration) { r.MergeQueueState = "AWAITING_CHECKS" }),
			[]string{"persona:author", "pr:open", "provider:github", "queue:awaiting-checks", "review:waiting"}},
		{"snoozed", watching(func(r *registry.Registration) { r.Snoozed = true }),
			[]string{"persona:author", "pr:open", "provider:github", "review:waiting", "snoozed:yes"}},
		{"found by a subscription", watching(func(r *registry.Registration) { r.SubscriptionID = "sub-1" }),
			[]string{"persona:author", "pr:open", "provider:github", "review:waiting", "subscription:watch"}},
		{"reviewer, own review in progress", watching(func(r *registry.Registration) { r.Persona = registry.PersonaReviewer }),
			[]string{"my_review:in-progress", "persona:reviewer", "pr:open", "provider:github", "review:waiting"}},
		{"reviewer, own approval", watching(func(r *registry.Registration) {
			r.Persona, r.SelfReviewState, r.LatestReviewState = registry.PersonaReviewer, "APPROVED", "APPROVED"
		}), []string{"my_review:approved", "persona:reviewer", "pr:open", "provider:github", "review:approved"}},
		{"merged keeps only the finished facts", watching(func(r *registry.Registration) {
			r.Status, r.CIBucket, r.HasConflict = registry.StatusMerged, "failure", true
		}), []string{"persona:author", "pr:merged", "provider:github"}},
		{"closed", watching(func(r *registry.Registration) { r.Status = registry.StatusClosed }),
			[]string{"persona:author", "pr:closed", "provider:github"}},
		{"stopped contributes nothing", watching(func(r *registry.Registration) { r.Status = registry.StatusStopped }), nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := filterValuesFor([]registry.Registration{tt.reg}, nil)["s"]
			if !slices.Equal(got, tt.want) {
				t.Errorf("filter values = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestFilterValuesUnionAcrossRegistrationsAndSubscriptions(t *testing.T) {
	regs := []registry.Registration{
		watching(func(r *registry.Registration) { r.CIBucket = "failure" }),
		watching(func(r *registry.Registration) {
			r.PRURL, r.Provider, r.CIBucket, r.IsDraft = "https://gitlab.example.com/g/p/-/merge_requests/2", "gitlab", "success", true
		}),
		{PRURL: "https://github.com/o/r/pull/3", SessionID: "other", Provider: "github", Status: registry.StatusWatching},
	}
	subs := []subscriptionBadgeInfo{
		{SessionID: "s", Mode: registry.SubscriptionModeNotify},
		{SessionID: "only-subs", Mode: registry.SubscriptionModeWatch},
	}
	got := filterValuesFor(regs, subs)

	want := []string{
		"ci:failing", "ci:passing", "persona:author", "pr:draft", "pr:open",
		"provider:github", "provider:gitlab", "review:waiting", "subscription:notify",
	}
	if !slices.Equal(got["s"], want) {
		t.Errorf("session s values = %v, want %v", got["s"], want)
	}
	if want := []string{"subscription:watch"}; !slices.Equal(got["only-subs"], want) {
		t.Errorf("subscription-only session values = %v, want %v", got["only-subs"], want)
	}
	if slices.Contains(got["other"], "subscription:notify") {
		t.Errorf("another session's values = %v, must not carry s's subscription", got["other"])
	}
}

func TestEveryEmittedFilterValueIsAnOfferedFacetOption(t *testing.T) {
	offered := map[string]bool{}
	for _, f := range filterFacets {
		for _, o := range f.Options {
			offered[o.Value] = true
		}
	}
	var regs []registry.Registration
	for _, bucket := range []string{"success", "failure", "pending", "action_required"} {
		regs = append(regs, watching(func(r *registry.Registration) { r.CIBucket = bucket }))
	}
	for _, review := range []string{"", "APPROVED", "APPROVED 1/2", "CHANGES_REQUESTED", "COMMENTED"} {
		regs = append(regs, watching(func(r *registry.Registration) { r.LatestReviewState = review }))
	}
	for _, queue := range []string{"QUEUED", "AWAITING_CHECKS", "LOCKED", "MERGEABLE", "UNMERGEABLE", "SOMETHING_NEW"} {
		regs = append(regs, watching(func(r *registry.Registration) { r.MergeQueueState = queue }))
	}
	regs = append(regs,
		watching(func(r *registry.Registration) {
			r.UnresolvedThreadIDs, r.HasConflict, r.Blocked = []string{"t"}, true, true
		}),
		watching(func(r *registry.Registration) {
			r.Snoozed, r.IsDraft, r.SubscriptionID, r.Provider = true, true, "sub", "gitlab"
		}),
		watching(func(r *registry.Registration) { r.Persona = registry.PersonaReviewer }),
		watching(func(r *registry.Registration) { r.Persona, r.SelfReviewState = registry.PersonaReviewer, "APPROVED" }),
		watching(func(r *registry.Registration) { r.Status = registry.StatusMerged }),
		watching(func(r *registry.Registration) { r.Status = registry.StatusClosed }),
	)
	subs := []subscriptionBadgeInfo{{SessionID: "s", Mode: registry.SubscriptionModeNotify}, {SessionID: "s", Mode: registry.SubscriptionModeWatch}}

	for _, v := range filterValuesFor(regs, subs)["s"] {
		if !offered[v] {
			t.Errorf("emitted value %q is not an option of any facet", v)
		}
	}
}

func TestFilterFacetsAreGenericAndDeclaredInTheManifest(t *testing.T) {
	manifest, err := os.ReadFile("../../aoe-plugin.toml")
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	declared := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^slot = "([^"]+)"\nid = "([^"]+)"`).FindAllStringSubmatch(string(manifest), -1) {
		declared[m[1]+"/"+m[2]] = true
	}
	if !declared[rowColumnSlot+"/"+rowColumnID] {
		t.Errorf("manifest declares no %s/%s", rowColumnSlot, rowColumnID)
	}
	if len(filterFacets) == 0 {
		t.Fatal("no filter facets defined")
	}
	for _, f := range filterFacets {
		if !declared[filterFacetSlot+"/"+f.ID] {
			t.Errorf("manifest declares no %s/%s; the host rejects undeclared slots", filterFacetSlot, f.ID)
		}
		if regexp.MustCompile(`(?i)github|gitlab`).MatchString(f.ID+" "+f.Label) && f.ID != "provider" {
			t.Errorf("facet %q (%q) names a platform, want a generic name", f.ID, f.Label)
		}
		if f.Column != rowColumnID {
			t.Errorf("facet %q column = %q, want %q", f.ID, f.Column, rowColumnID)
		}
		if len(f.Options) == 0 {
			t.Errorf("facet %q has no options", f.ID)
		}
	}
}

func TestHostRPCSetRowColumnCarriesTheFilterValues(t *testing.T) {
	var buf bytes.Buffer
	h := newHostRPC(&buf)
	if err := h.setRowColumn("sess1", []string{"ci:failing", "pr:open"}); err != nil {
		t.Fatalf("setRowColumn() error = %v", err)
	}
	var msg struct {
		Method string `json:"method"`
		Params struct {
			Slot      string `json:"slot"`
			ID        string `json:"id"`
			SessionID string `json:"session_id"`
			Payload   struct {
				Text         *string  `json:"text"`
				FilterValues []string `json:"filter_values"`
			} `json:"payload"`
		} `json:"params"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &msg); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if msg.Method != "ui.state.set" || msg.Params.Slot != rowColumnSlot || msg.Params.ID != rowColumnID || msg.Params.SessionID != "sess1" {
		t.Errorf("message = %+v, want ui.state.set of %s/%s for sess1", msg, rowColumnSlot, rowColumnID)
	}
	if msg.Params.Payload.Text == nil || *msg.Params.Payload.Text != "" {
		t.Errorf("payload.text = %v, want an empty string (the host requires the field; the badge already shows the words)", msg.Params.Payload.Text)
	}
	if !slices.Equal(msg.Params.Payload.FilterValues, []string{"ci:failing", "pr:open"}) {
		t.Errorf("payload.filter_values = %v", msg.Params.Payload.FilterValues)
	}
}

func TestHostRPCRemoveRowColumnAndPushFilterFacet(t *testing.T) {
	var buf bytes.Buffer
	h := newHostRPC(&buf)
	if err := h.removeRowColumn("sess1"); err != nil {
		t.Fatalf("removeRowColumn() error = %v", err)
	}
	if err := h.pushFilterFacet(filterFacets[0]); err != nil {
		t.Fatalf("pushFilterFacet() error = %v", err)
	}
	var lines [][]byte
	lines = append(lines, bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n"))...)
	if len(lines) != 2 {
		t.Fatalf("wrote %d messages, want 2", len(lines))
	}
	var remove, facet map[string]any
	_ = json.Unmarshal(lines[0], &remove)
	_ = json.Unmarshal(lines[1], &facet)
	rp := remove["params"].(map[string]any)
	if remove["method"] != "ui.state.remove" || rp["slot"] != rowColumnSlot || rp["id"] != rowColumnID || rp["session_id"] != "sess1" {
		t.Errorf("remove message = %v", remove)
	}
	fp := facet["params"].(map[string]any)
	if facet["method"] != "ui.state.set" || fp["slot"] != filterFacetSlot || fp["id"] != filterFacets[0].ID {
		t.Errorf("facet message = %v", facet)
	}
	if _, hasSession := fp["session_id"]; hasSession {
		t.Error("a facet is global, its message must not carry a session_id")
	}
	payload := fp["payload"].(map[string]any)
	if payload["column"] != rowColumnID || payload["label"] != filterFacets[0].Label {
		t.Errorf("facet payload = %v", payload)
	}
	if opts, _ := payload["options"].([]any); len(opts) != len(filterFacets[0].Options) {
		t.Errorf("facet payload options = %v, want %d", payload["options"], len(filterFacets[0].Options))
	}
}

func TestPushUIStatePushesFacetsAndTheRowColumnAndClearsDroppedSessions(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	store, err := registry.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("registry.Open() error = %v", err)
	}
	defer func() { _ = store.Close() }()
	now := time.Now()
	if err := store.Add(t.Context(), registry.Registration{
		PRURL: "https://github.com/o/r/pull/1", SessionID: "sess1", Provider: "github", NextDueAt: now, CreatedAt: now,
	}); err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	aoe := aoeclient.New(nil)
	var buf bytes.Buffer
	rpc := newHostRPC(&buf)

	type sent struct {
		Method string `json:"method"`
		Params struct {
			Slot      string `json:"slot"`
			ID        string `json:"id"`
			SessionID string `json:"session_id"`
		} `json:"params"`
	}
	decode := func() []sent {
		var out []sent
		for line := range bytes.SplitSeq(bytes.TrimSpace(buf.Bytes()), []byte("\n")) {
			var s sent
			if err := json.Unmarshal(line, &s); err != nil {
				t.Fatalf("decode %q: %v", line, err)
			}
			out = append(out, s)
		}
		buf.Reset()
		return out
	}

	pushUIState(t.Context(), store, aoe, rpc, logger)
	facets, columnFor := map[string]bool{}, map[string]string{}
	for _, s := range decode() {
		switch s.Params.Slot {
		case filterFacetSlot:
			facets[s.Params.ID] = true
		case rowColumnSlot:
			columnFor[s.Params.SessionID] = s.Method
		}
	}
	for _, f := range filterFacets {
		if !facets[f.ID] {
			t.Errorf("facet %q was not pushed", f.ID)
		}
	}
	if columnFor["sess1"] != "ui.state.set" {
		t.Errorf("row column for sess1 = %q, want ui.state.set", columnFor["sess1"])
	}

	if err := store.SetStatus(t.Context(), "https://github.com/o/r/pull/1", "sess1", registry.StatusStopped); err != nil {
		t.Fatalf("SetStatus() error = %v", err)
	}
	pushUIState(t.Context(), store, aoe, rpc, logger)
	columnFor = map[string]string{}
	for _, s := range decode() {
		if s.Params.Slot == rowColumnSlot {
			columnFor[s.Params.SessionID] = s.Method
		}
	}
	if columnFor["sess1"] != "ui.state.remove" {
		t.Errorf("row column for the dropped sess1 = %q, want ui.state.remove", columnFor["sess1"])
	}
}

// busyWatch is a watch with a value for every gated facet, so each case below
// shows exactly which ones its event filter lets through.
func busyWatch(filter []string) registry.Registration {
	return watching(func(r *registry.Registration) {
		r.EventFilter = filter
		r.CIBucket = "failure"
		r.LatestReviewState = "CHANGES_REQUESTED"
		r.HasConflict = true
		r.Blocked = true
		r.UnresolvedThreadIDs = []string{"t1"}
		r.MergeQueueState = "QUEUED"
		r.Persona = registry.PersonaReviewer
		r.SelfReviewState = "APPROVED"
	})
}

func TestFilterValuesFollowTheRegistrationsEventFilter(t *testing.T) {
	ungated := []string{"my_review:approved", "persona:reviewer", "pr:open", "provider:github"}
	all := append([]string{
		"blocked:yes", "ci:failing", "comments:unresolved", "conflicts:yes", "queue:queued", "review:changes-requested",
	}, ungated...)
	slices.Sort(all)
	tests := []struct {
		name   string
		filter []string
		want   []string
	}{
		{"an explicit filter naming every kind keeps all values", []string{"CI", "REVIEW", "CONFLICT", "COMMENT", "MERGE_QUEUE"}, all},
		{"without CI drops ci", []string{"REVIEW", "CONFLICT", "COMMENT", "MERGE_QUEUE"},
			slices.DeleteFunc(slices.Clone(all), func(v string) bool { return v == "ci:failing" })},
		{"without REVIEW drops review", []string{"CI", "CONFLICT", "COMMENT", "MERGE_QUEUE"},
			slices.DeleteFunc(slices.Clone(all), func(v string) bool { return v == "review:changes-requested" })},
		{"without CONFLICT drops conflicts and blocked, which the badge hides with it", []string{"CI", "REVIEW", "COMMENT", "MERGE_QUEUE"},
			slices.DeleteFunc(slices.Clone(all), func(v string) bool { return v == "conflicts:yes" || v == "blocked:yes" })},
		{"without COMMENT drops comments", []string{"CI", "REVIEW", "CONFLICT", "MERGE_QUEUE"},
			slices.DeleteFunc(slices.Clone(all), func(v string) bool { return v == "comments:unresolved" })},
		{"without MERGE_QUEUE drops queue", []string{"CI", "REVIEW", "CONFLICT", "COMMENT"},
			slices.DeleteFunc(slices.Clone(all), func(v string) bool { return v == "queue:queued" })},
		{"MERGED and CLOSED only keeps just the ungated facets", []string{"MERGED", "CLOSED"}, ungated},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := filterValuesFor([]registry.Registration{busyWatch(tt.filter)}, nil)["s"]
			if !slices.Equal(got, tt.want) {
				t.Errorf("filter values = %v, want %v", got, tt.want)
			}
		})
	}

	t.Run("an empty filter on an author watch keeps all values", func(t *testing.T) {
		r := busyWatch(nil)
		r.Persona = registry.PersonaAuthor
		got := filterValuesFor([]registry.Registration{r}, nil)["s"]
		want := []string{
			"blocked:yes", "ci:failing", "comments:unresolved", "conflicts:yes", "persona:author", "pr:open",
			"provider:github", "queue:queued", "review:changes-requested",
		}
		if !slices.Equal(got, want) {
			t.Errorf("filter values = %v, want %v", got, want)
		}
	})

	t.Run("the reviewer persona's default filter drops what it does not follow", func(t *testing.T) {
		got := filterValuesFor([]registry.Registration{busyWatch(nil)}, nil)["s"]
		want := []string{
			"comments:unresolved", "my_review:approved", "persona:reviewer", "pr:open", "provider:github", "review:changes-requested",
		}
		if !slices.Equal(got, want) {
			t.Errorf("filter values = %v, want %v", got, want)
		}
	})
}
