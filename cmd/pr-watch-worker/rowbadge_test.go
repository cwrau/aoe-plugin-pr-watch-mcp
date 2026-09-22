package main

import (
	"strings"
	"testing"
	"time"

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
	if len(got["sess1"].Items) != 6 {
		t.Errorf("sess1 items = %d, want 6 (two registrations, three chips each: eye/CI/review)", len(got["sess1"].Items))
	}
	if len(got["sess2"].Items) != 3 {
		t.Errorf("sess2 items = %d, want 3 (one registration, three chips)", len(got["sess2"].Items))
	}
}

func TestBuildRowBadgePayloadsChipFields(t *testing.T) {
	regs := []registry.Registration{
		{PRURL: "https://github.com/cwrau/aoe-pr-watch-mcp/pull/42", SessionID: "sess1", Provider: "github", CIBucket: "failure", LatestReviewState: "CHANGES_REQUESTED", Title: "Fix the widget rendering bug"},
	}

	got := buildRowBadgePayloads(regs)
	items := got["sess1"].Items
	if len(items) != 3 {
		t.Fatalf("items len = %d, want 3 (eye, CI, review)", len(items))
	}
	eye, ci, review := items[0], items[1], items[2]

	if eye.Icon != "eye" || eye.Tone != "neutral" {
		t.Errorf("eye chip = {Icon:%q Tone:%q}, want {eye neutral} (a pure identity marker now that CI/review are their own chips)", eye.Icon, eye.Tone)
	}
	if eye.Href != regs[0].PRURL {
		t.Errorf("eye.Href = %q, want %q", eye.Href, regs[0].PRURL)
	}
	if !strings.Contains(eye.Tooltip, "Fix the widget rendering bug") || !strings.Contains(eye.Tooltip, "cwrau/aoe-pr-watch-mcp#42") {
		t.Errorf("eye.Tooltip = %q, want it to include both the title and the short ref", eye.Tooltip)
	}

	if ci.Text != "failure" || ci.Tone != "danger" {
		t.Errorf("ci chip = {Text:%q Tone:%q}, want {failure danger}", ci.Text, ci.Tone)
	}
	if !strings.Contains(ci.Tooltip, "CI failure") {
		t.Errorf("ci.Tooltip = %q, want it to mention CI failure", ci.Tooltip)
	}

	if review.Text != "CHANGES_REQUESTED" || review.Tone != "danger" {
		t.Errorf("review chip = {Text:%q Tone:%q}, want {CHANGES_REQUESTED danger}", review.Text, review.Tone)
	}
	if !strings.Contains(review.Tooltip, "review CHANGES_REQUESTED") {
		t.Errorf("review.Tooltip = %q, want it to mention the review state", review.Tooltip)
	}
}

func TestBuildRowBadgePayloadsReviewTooltipIncludesReviewerAndTimestamp(t *testing.T) {
	at := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	regs := []registry.Registration{
		{PRURL: "https://github.com/o/r/pull/1", SessionID: "sess1", Provider: "github", LatestReviewState: "APPROVED", LatestReviewer: "alice", LatestReviewAt: at},
	}

	got := buildRowBadgePayloads(regs)
	review := got["sess1"].Items[2]

	if !strings.Contains(review.Tooltip, "alice") || !strings.Contains(review.Tooltip, "2026-09-20 12:00") {
		t.Errorf("review.Tooltip = %q, want it to include reviewer alice and the timestamp", review.Tooltip)
	}
}

func TestBuildRowBadgePayloadsCITooltipIncludesDetail(t *testing.T) {
	regs := []registry.Registration{
		{PRURL: "https://github.com/o/r/pull/1", SessionID: "sess1", Provider: "github", CIBucket: "failure", CIDetail: "build"},
	}

	got := buildRowBadgePayloads(regs)
	ci := got["sess1"].Items[1]

	if !strings.Contains(ci.Tooltip, "build") {
		t.Errorf("ci.Tooltip = %q, want it to include the failing check name", ci.Tooltip)
	}
}

func TestBuildRowBadgePayloadsMergedTooltipIncludesTimestamp(t *testing.T) {
	at := time.Date(2026, 9, 21, 8, 30, 0, 0, time.UTC)
	regs := []registry.Registration{
		{PRURL: "https://github.com/o/r/pull/1", SessionID: "sess1", Provider: "github", Status: registry.StatusMerged, MergedAt: at},
	}

	got := buildRowBadgePayloads(regs)
	chip := got["sess1"].Items[0]

	if !strings.Contains(chip.Tooltip, "2026-09-21 08:30") {
		t.Errorf("chip.Tooltip = %q, want it to include the merge timestamp", chip.Tooltip)
	}
}

func TestBuildRowBadgePayloadsChipTooltipFallsBackWithoutTitle(t *testing.T) {
	regs := []registry.Registration{
		{PRURL: "https://github.com/cwrau/aoe-pr-watch-mcp/pull/42", SessionID: "sess1", Provider: "github", CIBucket: "none"},
	}

	got := buildRowBadgePayloads(regs)
	eye := got["sess1"].Items[0]

	if !strings.Contains(eye.Tooltip, "cwrau/aoe-pr-watch-mcp#42") {
		t.Errorf("eye.Tooltip = %q, want the short ref when no title has been fetched yet", eye.Tooltip)
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
	if len(got["sess1"].Items) != 3 {
		t.Fatalf("sess1 items = %d, want 3 (the stopped registration must not contribute chips, only the watching one's eye/CI/review)", len(got["sess1"].Items))
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
