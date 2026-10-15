package main

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/cwrau/aoe-plugin-pr-watch-mcp/internal/provider"
)

func TestParseCommentFilters(t *testing.T) {
	raw := json.RawMessage(`{
		"value": [
			{"_id": "a", "author": " coderabbitai ", "contains": ["review in progress", " summarize by coderabbit.ai ", "", "  "]},
			{"_id": "b", "author": "renovate"},
			{"_id": "c", "contains": ["Currently processing"]},
			{"_id": "d", "author": "  ", "contains": []},
			{"_id": "e", "contains": ["", " "]},
			{"_id": "f"}
		],
		"revision": 2
	}`)
	got, err := parseCommentFilters(raw)
	if err != nil {
		t.Fatalf("parseCommentFilters() error = %v", err)
	}
	want := []provider.CommentFilter{
		{Author: "coderabbitai", Contains: []string{"review in progress", "summarize by coderabbit.ai"}},
		{Author: "renovate"},
		{Contains: []string{"Currently processing"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseCommentFilters() = %+v, want %+v (blank texts dropped; rows with neither an author nor a text dropped, so a half-filled row can't mute everything)", got, want)
	}
}

// TestParseCommentFiltersAcceptsALegacySingleString: contains used to be a
// plain string setting, and the host keeps whatever value was stored until
// the row is next edited — a rule saved before the field became a list must
// keep working rather than silently stop muting.
func TestParseCommentFiltersAcceptsALegacySingleString(t *testing.T) {
	got, err := parseCommentFilters(json.RawMessage(`{"value": [{"_id": "a", "author": "coderabbitai", "contains": "review in progress"}]}`))
	if err != nil {
		t.Fatalf("parseCommentFilters() error = %v", err)
	}
	want := []provider.CommentFilter{{Author: "coderabbitai", Contains: []string{"review in progress"}}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseCommentFilters() = %+v, want %+v", got, want)
	}
}

func TestParseCommentFiltersNullValueMeansNoRules(t *testing.T) {
	got, err := parseCommentFilters(json.RawMessage(`{"value": null}`))
	if err != nil || len(got) != 0 {
		t.Errorf("parseCommentFilters() = %+v, %v, want no rules and no error", got, err)
	}
}

func TestParseCommentFiltersRejectsGarbage(t *testing.T) {
	if _, err := parseCommentFilters(json.RawMessage(`{"value": "not a list"}`)); err == nil {
		t.Error("parseCommentFilters() error = nil, want an error for a non-list value")
	}
}

// TestCommentFiltersRefreshKeepsPreviousOnFailure mirrors the snooze
// setting's guarantee: a transient host hiccup must not silently drop the
// user's filter rules (which would start pinging the muted noise again).
func TestCommentFiltersRefreshKeepsPreviousOnFailure(t *testing.T) {
	h := newReplyingHost(`{"value":[{"_id":"a","author":"coderabbitai","contains":["review in progress"]}]}`)
	f := &commentFilters{}

	f.refresh(context.Background(), h.rpc, testLogger())
	if got := f.get(); len(got) != 1 || got[0].Author != "coderabbitai" {
		t.Fatalf("get() after refresh = %+v, want the one loaded rule", got)
	}

	h.mu.Lock()
	h.replyErr = true
	h.mu.Unlock()
	f.refresh(context.Background(), h.rpc, testLogger())
	if got := f.get(); len(got) != 1 {
		t.Errorf("get() after failed refresh = %+v, want the previous rule kept", got)
	}

	h.mu.Lock()
	h.replyErr = false
	h.reply = `{"value":[]}`
	h.mu.Unlock()
	f.refresh(context.Background(), h.rpc, testLogger())
	if got := f.get(); len(got) != 0 {
		t.Errorf("get() after emptying the setting = %+v, want no rules", got)
	}
}
