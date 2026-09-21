package main

import (
	"strings"
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
		{PRURL: "https://github.com/cwrau/aoe-pr-watch-mcp/pull/42", SessionID: "sess1", Provider: "github", CIBucket: "failure", LatestReviewState: "CHANGES_REQUESTED", Title: "Fix the widget rendering bug"},
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
	if !strings.Contains(chip.Tooltip, "Fix the widget rendering bug") {
		t.Errorf("chip.Tooltip = %q, want it to include the PR title", chip.Tooltip)
	}
	if !strings.Contains(chip.Tooltip, "cwrau/aoe-pr-watch-mcp#42") {
		t.Errorf("chip.Tooltip = %q, want it to still include the short ref too", chip.Tooltip)
	}
}

func TestBuildRowBadgePayloadsChipTooltipFallsBackWithoutTitle(t *testing.T) {
	regs := []registry.Registration{
		{PRURL: "https://github.com/cwrau/aoe-pr-watch-mcp/pull/42", SessionID: "sess1", Provider: "github", CIBucket: "none"},
	}

	got := buildRowBadgePayloads(regs)
	chip := got["sess1"].Items[0]

	if !strings.Contains(chip.Tooltip, "cwrau/aoe-pr-watch-mcp#42") {
		t.Errorf("chip.Tooltip = %q, want the short ref when no title has been fetched yet", chip.Tooltip)
	}
}

func TestBuildRowBadgePayloadsEmptyInputGivesEmptyMap(t *testing.T) {
	got := buildRowBadgePayloads(nil)
	if len(got) != 0 {
		t.Errorf("buildRowBadgePayloads(nil) len = %d, want 0", len(got))
	}
}

func TestBuildRowBadgePayloadsMergedChipIsDistinctAndFixed(t *testing.T) {
	regs := []registry.Registration{
		{PRURL: "https://github.com/cwrau/aoe-pr-watch-mcp/pull/42", SessionID: "sess1", Provider: "github", Status: registry.StatusMerged, CIBucket: "failure", LatestReviewState: "CHANGES_REQUESTED"},
	}

	got := buildRowBadgePayloads(regs)
	chip := got["sess1"].Items[0]

	if chip.Icon != "check-circle" || chip.Tone != "success" {
		t.Errorf("merged chip = {Icon:%q Tone:%q}, want {check-circle success} regardless of stale CI/review state", chip.Icon, chip.Tone)
	}
	if !strings.Contains(chip.Tooltip, "merged") {
		t.Errorf("chip.Tooltip = %q, want it to say merged", chip.Tooltip)
	}
}

func TestBuildRowBadgePayloadsClosedChipIsDistinctAndFixed(t *testing.T) {
	regs := []registry.Registration{
		{PRURL: "https://github.com/cwrau/aoe-pr-watch-mcp/pull/42", SessionID: "sess1", Provider: "github", Status: registry.StatusClosed},
	}

	got := buildRowBadgePayloads(regs)
	chip := got["sess1"].Items[0]

	if chip.Icon != "circle-slash" || chip.Tone != "neutral" {
		t.Errorf("closed chip = {Icon:%q Tone:%q}, want {circle-slash neutral}", chip.Icon, chip.Tone)
	}
	if !strings.Contains(chip.Tooltip, "closed") {
		t.Errorf("chip.Tooltip = %q, want it to say closed", chip.Tooltip)
	}
}

func TestBuildRowBadgePayloadsExcludesStoppedRegistrations(t *testing.T) {
	regs := []registry.Registration{
		{PRURL: "https://github.com/o/r/pull/1", SessionID: "sess1", Provider: "github", Status: registry.StatusStopped},
		{PRURL: "https://github.com/o/r/pull/2", SessionID: "sess1", Provider: "github", Status: registry.StatusWatching},
	}

	got := buildRowBadgePayloads(regs)
	if len(got["sess1"].Items) != 1 {
		t.Fatalf("sess1 items = %d, want 1 (the stopped registration must not contribute a chip)", len(got["sess1"].Items))
	}
	if got["sess1"].Items[0].Href != "https://github.com/o/r/pull/2" {
		t.Errorf("sess1 chip href = %q, want the still-watching registration's URL", got["sess1"].Items[0].Href)
	}
}

func TestBuildRowBadgePayloadsSessionWithOnlyStoppedRegistrationsGetsNoEntry(t *testing.T) {
	regs := []registry.Registration{
		{PRURL: "https://github.com/o/r/pull/1", SessionID: "sess1", Provider: "github", Status: registry.StatusStopped},
	}

	got := buildRowBadgePayloads(regs)
	if _, ok := got["sess1"]; ok {
		t.Errorf("got[%q] present, want no entry so the caller knows to remove any existing badge", "sess1")
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
