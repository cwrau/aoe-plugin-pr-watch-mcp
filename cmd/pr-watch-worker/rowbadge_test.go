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

func TestBuildRowBadgePayloadsEyeChipDangerOnConflictNoExtraChip(t *testing.T) {
	regs := []registry.Registration{
		{PRURL: "https://github.com/o/r/pull/1", SessionID: "sess1", Provider: "github", CIBucket: "failure", HasConflict: true},
	}

	got := buildRowBadgePayloads(regs)
	items := got["sess1"].Items
	if len(items) != 3 {
		t.Fatalf("items len = %d, want 3 (eye, ci, review) — a conflict recolors the eye chip, it doesn't add one", len(items))
	}
	if items[0].Tone != "danger" {
		t.Errorf("eye.Tone = %q, want danger when HasConflict is true", items[0].Tone)
	}
	if items[1].Text != "failure" {
		t.Errorf("items[1].Text = %q, want the CI chip right after the eye chip", items[1].Text)
	}
}

func TestBuildRowBadgePayloadsEyeChipToneNeutralWithoutConflict(t *testing.T) {
	regs := []registry.Registration{
		{PRURL: "https://github.com/o/r/pull/1", SessionID: "sess1", Provider: "github", CIBucket: "success"},
	}

	got := buildRowBadgePayloads(regs)
	items := got["sess1"].Items
	if len(items) != 3 {
		t.Fatalf("items len = %d, want 3 (eye, ci, review)", len(items))
	}
	if items[0].Tone != "neutral" {
		t.Errorf("eye.Tone = %q, want neutral when there's no conflict and no merge queue state", items[0].Tone)
	}
}

func TestBuildRowBadgePayloadsEyeChipTooltipMentionsConflict(t *testing.T) {
	regs := []registry.Registration{
		{PRURL: "https://github.com/o/r/pull/1", SessionID: "sess1", Provider: "github", HasConflict: true},
	}

	got := buildRowBadgePayloads(regs)
	eye := got["sess1"].Items[0]
	if !strings.Contains(eye.Tooltip, "has a merge conflict") {
		t.Errorf("eye.Tooltip = %q, want it to mention the merge conflict", eye.Tooltip)
	}
}

func TestBuildRowBadgePayloadsConflictTonePreemptsMergeQueueTone(t *testing.T) {
	regs := []registry.Registration{
		{PRURL: "https://github.com/o/r/pull/1", SessionID: "sess1", Provider: "github", HasConflict: true, MergeQueueState: "MERGEABLE"},
	}

	got := buildRowBadgePayloads(regs)
	eye := got["sess1"].Items[0]
	if eye.Tone != "danger" {
		t.Errorf("eye.Tone = %q, want danger — a real conflict must win over a stale/optimistic merge queue state", eye.Tone)
	}
}

func TestBuildRowBadgePayloadsEyeChipWarnOnBlockedGitLabDependency(t *testing.T) {
	regs := []registry.Registration{
		{PRURL: "https://gitlab.corp.example/team/project/-/merge_requests/17", SessionID: "sess1", Provider: "gitlab", Blocked: true},
	}

	got := buildRowBadgePayloads(regs)
	items := got["sess1"].Items
	if len(items) != 3 {
		t.Fatalf("items len = %d, want 3 (eye, ci, review) — blocked recolors the eye chip, it doesn't add one", len(items))
	}
	eye := items[0]
	if eye.Tone != "warn" {
		t.Errorf("eye.Tone = %q, want warn when Blocked is true", eye.Tone)
	}
	if !strings.Contains(eye.Tooltip, "blocked by another merge request") {
		t.Errorf("eye.Tooltip = %q, want it to mention being blocked", eye.Tooltip)
	}
}

func TestBuildRowBadgePayloadsConflictTonePreemptsBlocked(t *testing.T) {
	regs := []registry.Registration{
		{PRURL: "https://gitlab.corp.example/team/project/-/merge_requests/17", SessionID: "sess1", Provider: "gitlab", HasConflict: true, Blocked: true},
	}

	got := buildRowBadgePayloads(regs)
	eye := got["sess1"].Items[0]
	if eye.Tone != "danger" {
		t.Errorf("eye.Tone = %q, want danger — a real conflict must win over just being blocked on sequencing", eye.Tone)
	}
	if !strings.Contains(eye.Tooltip, "has a merge conflict") {
		t.Errorf("eye.Tooltip = %q, want the conflict message, not the blocked one", eye.Tooltip)
	}
}

func TestBuildRowBadgePayloadsEyeChipToneByMergeQueueState(t *testing.T) {
	cases := map[string]string{
		"":                "neutral",
		"QUEUED":          "info",
		"AWAITING_CHECKS": "info",
		"LOCKED":          "info",
		"MERGEABLE":       "success",
		"UNMERGEABLE":     "danger",
	}
	for state, wantTone := range cases {
		t.Run(state, func(t *testing.T) {
			regs := []registry.Registration{
				{PRURL: "https://github.com/o/r/pull/1", SessionID: "sess1", Provider: "github", MergeQueueState: state},
			}
			got := buildRowBadgePayloads(regs)
			eye := got["sess1"].Items[0]
			if eye.Tone != wantTone {
				t.Errorf("merge queue state %q -> eye.Tone = %q, want %q", state, eye.Tone, wantTone)
			}
		})
	}
}

func TestBuildRowBadgePayloadsEyeChipTooltipIncludesQueuePosition(t *testing.T) {
	regs := []registry.Registration{
		{PRURL: "https://github.com/cwrau/aoe-pr-watch-mcp/pull/42", SessionID: "sess1", Provider: "github", MergeQueueState: "QUEUED", MergeQueuePosition: 3},
	}
	got := buildRowBadgePayloads(regs)
	eye := got["sess1"].Items[0]
	if !strings.Contains(eye.Tooltip, "in merge queue: queued (position 3)") {
		t.Errorf("eye.Tooltip = %q, want it to mention the merge queue state and position", eye.Tooltip)
	}
}

func TestBuildRowBadgePayloadsEyeChipTooltipUnchangedWithoutMergeQueue(t *testing.T) {
	regs := []registry.Registration{
		{PRURL: "https://github.com/cwrau/aoe-pr-watch-mcp/pull/42", SessionID: "sess1", Provider: "github"},
	}
	got := buildRowBadgePayloads(regs)
	eye := got["sess1"].Items[0]
	if strings.Contains(eye.Tooltip, "merge queue") {
		t.Errorf("eye.Tooltip = %q, want no merge queue mention when MergeQueueState is empty", eye.Tooltip)
	}
}

func TestBuildRowBadgePayloadsEyeChipLinksToGitHubConflictsUIOnConflict(t *testing.T) {
	regs := []registry.Registration{
		{PRURL: "https://github.com/cwrau/aoe-pr-watch-mcp/pull/42", SessionID: "sess1", Provider: "github", HasConflict: true},
	}

	got := buildRowBadgePayloads(regs)
	eye := got["sess1"].Items[0]
	if eye.Href != "https://github.com/cwrau/aoe-pr-watch-mcp/pull/42/conflicts" {
		t.Errorf("eye.Href = %q, want the GitHub conflicts UI", eye.Href)
	}
}

func TestBuildRowBadgePayloadsEyeChipFallsBackToMRURLForGitLabConflict(t *testing.T) {
	regs := []registry.Registration{
		{PRURL: "https://gitlab.corp.example/team/project/-/merge_requests/17", SessionID: "sess1", Provider: "gitlab", HasConflict: true},
	}

	got := buildRowBadgePayloads(regs)
	eye := got["sess1"].Items[0]
	if eye.Href != regs[0].PRURL {
		t.Errorf("eye.Href = %q, want the plain MR URL fallback for GitLab", eye.Href)
	}
}

func TestBuildRowBadgePayloadsCIChipLinksToCIHref(t *testing.T) {
	regs := []registry.Registration{
		{PRURL: "https://github.com/o/r/pull/1", SessionID: "sess1", Provider: "github", CIBucket: "failure", CIHref: "https://github.com/o/r/actions/runs/123"},
	}

	got := buildRowBadgePayloads(regs)
	ci := got["sess1"].Items[1]
	if ci.Href != "https://github.com/o/r/actions/runs/123" {
		t.Errorf("ci.Href = %q, want the specific run link over the PR URL", ci.Href)
	}
}

func TestBuildRowBadgePayloadsCIChipFallsBackToPRURLWithoutCIHref(t *testing.T) {
	regs := []registry.Registration{
		{PRURL: "https://github.com/o/r/pull/1", SessionID: "sess1", Provider: "github", CIBucket: "pending"},
	}

	got := buildRowBadgePayloads(regs)
	ci := got["sess1"].Items[1]
	if ci.Href != regs[0].PRURL {
		t.Errorf("ci.Href = %q, want the PR URL fallback when no CIHref is known", ci.Href)
	}
}

func TestBuildRowBadgePayloadsReviewChipLinksToReviewHref(t *testing.T) {
	regs := []registry.Registration{
		{PRURL: "https://github.com/o/r/pull/1", SessionID: "sess1", Provider: "github", LatestReviewState: "APPROVED", ReviewHref: "https://github.com/o/r/pull/1#pullrequestreview-999"},
	}

	got := buildRowBadgePayloads(regs)
	review := got["sess1"].Items[2]
	if review.Href != "https://github.com/o/r/pull/1#pullrequestreview-999" {
		t.Errorf("review.Href = %q, want the specific review link over the PR URL", review.Href)
	}
}

func TestBuildRowBadgePayloadsReviewChipFallsBackToPRURLWithoutReviewHref(t *testing.T) {
	regs := []registry.Registration{
		{PRURL: "https://github.com/o/r/pull/1", SessionID: "sess1", Provider: "github"},
	}

	got := buildRowBadgePayloads(regs)
	review := got["sess1"].Items[2]
	if review.Href != regs[0].PRURL {
		t.Errorf("review.Href = %q, want the PR URL fallback when no ReviewHref is known", review.Href)
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
