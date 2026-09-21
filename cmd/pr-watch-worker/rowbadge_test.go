package main

import (
	"testing"

	"github.com/cwrau/aoe-pr-watch-mcp/internal/registry"
)

func TestBuildRowBadgePayloadsGroupsBySession(t *testing.T) {
	regs := []registry.Registration{
		{PRURL: "https://github.com/o/r/pull/1", SessionID: "sess1", Provider: "github", CIBucket: "success"},
		{PRURL: "https://github.com/o/r/pull/2", SessionID: "sess1", Provider: "github", CIBucket: "failure"},
		{PRURL: "https://github.com/o/r2/pull/3", SessionID: "sess2", Provider: "github", CIBucket: "pending"},
	}

	got := buildRowBadgePayloads(regs)

	if len(got) != 2 {
		t.Fatalf("buildRowBadgePayloads() len = %d, want 2 sessions", len(got))
	}
	if len(got["sess1"].Items) != 2 {
		t.Errorf("sess1 items = %d, want 2 (two registrations)", len(got["sess1"].Items))
	}
	if len(got["sess2"].Items) != 1 {
		t.Errorf("sess2 items = %d, want 1", len(got["sess2"].Items))
	}
}

func TestBuildRowBadgePayloadsChipFields(t *testing.T) {
	regs := []registry.Registration{
		{PRURL: "https://github.com/cwrau/aoe-pr-watch-mcp/pull/42", SessionID: "sess1", Provider: "github", CIBucket: "failure", LatestReviewState: "CHANGES_REQUESTED"},
	}

	got := buildRowBadgePayloads(regs)
	chip := got["sess1"].Items[0]

	if chip.Icon != "eye" {
		t.Errorf("chip.Icon = %q, want %q", chip.Icon, "eye")
	}
	if chip.Tone != "danger" {
		t.Errorf("chip.Tone = %q, want %q (failure CI should drive the chip tone)", chip.Tone, "danger")
	}
	if chip.Href != regs[0].PRURL {
		t.Errorf("chip.Href = %q, want %q", chip.Href, regs[0].PRURL)
	}
	if chip.Tooltip == "" {
		t.Error("chip.Tooltip is empty, want a short PR ref + status summary")
	}
}

func TestBuildRowBadgePayloadsEmptyInputGivesEmptyMap(t *testing.T) {
	got := buildRowBadgePayloads(nil)
	if len(got) != 0 {
		t.Errorf("buildRowBadgePayloads(nil) len = %d, want 0", len(got))
	}
}

func TestBuildRowBadgePayloadsChipOrderIsDeterministic(t *testing.T) {
	regs := []registry.Registration{
		{PRURL: "https://github.com/o/r/pull/9", SessionID: "sess1", Provider: "github", CIBucket: "none"},
		{PRURL: "https://github.com/o/r/pull/1", SessionID: "sess1", Provider: "github", CIBucket: "none"},
	}

	first := buildRowBadgePayloads(regs)
	second := buildRowBadgePayloads(regs)

	if first["sess1"].Items[0].Href != second["sess1"].Items[0].Href {
		t.Error("buildRowBadgePayloads() produced different chip order across calls, want deterministic ordering")
	}
	if first["sess1"].Items[0].Href != "https://github.com/o/r/pull/1" {
		t.Errorf("first chip href = %q, want the lexicographically-first PR URL for determinism", first["sess1"].Items[0].Href)
	}
}
