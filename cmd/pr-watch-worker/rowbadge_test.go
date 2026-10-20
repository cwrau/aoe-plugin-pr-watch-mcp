package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cwrau/aoe-plugin-pr-watch-mcp/internal/provider"
	"github.com/cwrau/aoe-plugin-pr-watch-mcp/internal/registry"
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
	// Two PRs: both open and unreviewed, so their eye and review chips merge;
	// their CI states differ, so those stay apart: eye, success, failure, review.
	if len(got["sess1"].Items) != 4 {
		t.Errorf("sess1 items = %d, want 4 (merged eye, two CI states, merged review)", len(got["sess1"].Items))
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

func TestBuildRowBadgePayloadsOnlyEyeChipForMergedOnlyFilter(t *testing.T) {
	regs := []registry.Registration{
		{PRURL: "https://github.com/o/r/pull/1", SessionID: "sess1", Provider: "github", CIBucket: "failure", LatestReviewState: "CHANGES_REQUESTED", EventFilter: []string{"MERGED"}},
	}

	items := buildRowBadgePayloads(regs)["sess1"].Items
	if len(items) != 1 || items[0].Icon != "eye" {
		t.Fatalf("items = %+v, want just the eye chip for a MERGED-only filter", items)
	}
}

func TestBuildRowBadgePayloadsEyeAndCIChipForCIOnlyFilter(t *testing.T) {
	regs := []registry.Registration{
		{PRURL: "https://github.com/o/r/pull/1", SessionID: "sess1", Provider: "github", CIBucket: "failure", LatestReviewState: "CHANGES_REQUESTED", EventFilter: []string{"CI"}},
	}

	items := buildRowBadgePayloads(regs)["sess1"].Items
	if len(items) != 2 {
		t.Fatalf("items = %+v, want 2 (eye, ci) for a CI-only filter — no review chip", items)
	}
	if items[0].Icon != "eye" {
		t.Errorf("items[0].Icon = %q, want %q", items[0].Icon, "eye")
	}
	if items[1].Text != "failure" {
		t.Errorf("items[1].Text = %q, want the CI chip", items[1].Text)
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
	if len(items) != 2 {
		t.Fatalf("items len = %d, want 2 (eye, review) — blocked recolors the eye chip, it doesn't add one, and an unset CIBucket omits the CI chip entirely", len(items))
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
	// Items[1], not [2]: CIBucket is unset here, so the CI chip is omitted
	// entirely and review is the very next chip after eye.
	review := got["sess1"].Items[1]
	if review.Href != "https://github.com/o/r/pull/1#pullrequestreview-999" {
		t.Errorf("review.Href = %q, want the specific review link over the PR URL", review.Href)
	}
}

func TestBuildRowBadgePayloadsReviewChipFallsBackToPRURLWithoutReviewHref(t *testing.T) {
	regs := []registry.Registration{
		{PRURL: "https://github.com/o/r/pull/1", SessionID: "sess1", Provider: "github"},
	}

	got := buildRowBadgePayloads(regs)
	// Items[1], not [2]: CIBucket is unset here, so the CI chip is omitted.
	review := got["sess1"].Items[1]
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
	// Items[1], not [2]: CIBucket is unset here, so the CI chip is omitted.
	review := got["sess1"].Items[1]

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
	if len(got["sess1"].Items) != 2 {
		t.Fatalf("sess1 items = %d, want 2 (the stopped registration must not contribute chips, only the watching one's eye/review — its CIBucket is unset, so no CI chip)", len(got["sess1"].Items))
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

// TestBuildRowBadgePayloadsOrdersByStatusThenNumber: chips used to sort by
// raw URL string, which puts #10 before #9. The finished PRs come first, as
// one chip each for merged and closed (linking the newest, here #5 since no
// merge times are known), then the watched PRs by number numerically.
func TestBuildRowBadgePayloadsOrdersByStatusThenNumber(t *testing.T) {
	regs := []registry.Registration{
		{PRURL: "https://github.com/o/r/pull/2", SessionID: "sess1", Provider: "github", Status: registry.StatusClosed},
		{PRURL: "https://github.com/o/r/pull/5", SessionID: "sess1", Provider: "github", Status: registry.StatusMerged},
		{PRURL: "https://github.com/o/r/pull/10", SessionID: "sess1", Provider: "github", Status: registry.StatusWatching},
		{PRURL: "https://github.com/o/r/pull/9", SessionID: "sess1", Provider: "github", Status: registry.StatusWatching},
		{PRURL: "https://github.com/o/r/pull/1", SessionID: "sess1", Provider: "github", Status: registry.StatusMerged},
	}

	var got []string
	var watchedTooltip string
	for _, chip := range buildRowBadgePayloads(regs)["sess1"].Items {
		switch chip.Icon {
		case "eye", "check-circle", "circle-slash":
			got = append(got, chip.Href)
		}
		if chip.Icon == "eye" {
			watchedTooltip = chip.Tooltip
		}
	}
	// #9 and #10 are in the same state, so they are one chip (linking #9, the
	// lower number) listing both in that order.
	want := []string{
		"https://github.com/o/r/pull/5",
		"https://github.com/o/r/pull/2",
		"https://github.com/o/r/pull/9",
	}
	if !slices.Equal(got, want) {
		t.Errorf("chip order = %v, want %v", got, want)
	}
	if !strings.Contains(watchedTooltip, "o/r#9, o/r#10") {
		t.Errorf("merged tooltip = %q, want #9 listed before #10", watchedTooltip)
	}
}

// Within one merge queue the chips follow queue position, not PR number; PRs
// outside any queue come after the queued ones, still ordered by number.
func TestBuildRowBadgePayloadsOrdersQueuedByQueuePosition(t *testing.T) {
	queued := func(url string, pos int) registry.Registration {
		return registry.Registration{PRURL: url, SessionID: "sess1", Provider: "github", Status: registry.StatusWatching, MergeQueueState: "QUEUED", MergeQueuePosition: pos}
	}
	regs := []registry.Registration{
		queued("https://github.com/o/r/pull/5", 2),
		{PRURL: "https://github.com/o/r/pull/1", SessionID: "sess1", Provider: "github", Status: registry.StatusWatching},
		queued("https://github.com/o/r/pull/20", 1),
		queued("https://github.com/o/r/pull/7", 3),
		queued("https://github.com/o/other/pull/2", 1),
		{PRURL: "https://github.com/o/r/pull/3", SessionID: "sess1", Provider: "github", Status: registry.StatusWatching},
	}

	var got []string
	var lastTooltip string
	for _, chip := range buildRowBadgePayloads(regs)["sess1"].Items {
		if chip.Icon == "eye" {
			got = append(got, chip.Href)
			lastTooltip = chip.Tooltip
		}
	}
	// Queue entries stay one chip each, in queue order; the two PRs outside any
	// queue are in the same state and so one merged chip after them.
	want := []string{
		"https://github.com/o/other/pull/2",
		"https://github.com/o/r/pull/20",
		"https://github.com/o/r/pull/5",
		"https://github.com/o/r/pull/7",
		"https://github.com/o/r/pull/1",
	}
	if !slices.Equal(got, want) {
		t.Errorf("chip order = %v, want %v", got, want)
	}
	if !strings.Contains(lastTooltip, "o/r#1, o/r#3") {
		t.Errorf("merged tooltip = %q, want the two unqueued PRs by number", lastTooltip)
	}
}

// queuedTestRegs are n PRs in a merge queue, each a chip of its own (queue
// entries are never merged), so a row of them gets long enough for compact
// mode, which equal chips otherwise never do.
func queuedTestRegs(n int, ci, review string) []registry.Registration {
	var regs []registry.Registration
	for i := 1; i <= n; i++ {
		regs = append(regs, registry.Registration{
			PRURL: fmt.Sprintf("https://github.com/o/r/pull/%d", i), SessionID: "sess1", Provider: "github", Status: registry.StatusWatching,
			CIBucket: ci, LatestReviewState: review, MergeQueueState: "QUEUED", MergeQueuePosition: i,
		})
	}
	return regs
}

func compactTestRegs(n int, ci, review string) []registry.Registration {
	var regs []registry.Registration
	for i := 1; i <= n; i++ {
		regs = append(regs, registry.Registration{
			PRURL: fmt.Sprintf("https://github.com/o/r/pull/%d", i), SessionID: "sess1", Provider: "github",
			Status: registry.StatusWatching, CIBucket: ci, LatestReviewState: review,
		})
	}
	return regs
}

// Up to the threshold (counted after equal chips are merged) every watched PR
// keeps its eye, CI and review chips.
func TestBuildRowBadgePayloadsKeepsAllChipsAtOrBelowCompactThreshold(t *testing.T) {
	regs := queuedTestRegs(14, "success", "") // 14 queue entries + merged CI + merged review = 16
	items := buildRowBadgePayloads(regs)["sess1"].Items
	if len(items) != 16 {
		t.Errorf("len(items) = %d, want 16 (14 eyes, the CI and the review chip, at the threshold)", len(items))
	}
}

// Past the threshold the boring states (CI success/none/pending, no review)
// drop out and only the eye chip remains for a quiet PR.
func TestBuildRowBadgePayloadsCompactsQuietPRsPastThreshold(t *testing.T) {
	regs := queuedTestRegs(15, "success", "") // 17 chips uncompacted
	items := buildRowBadgePayloads(regs)["sess1"].Items
	if len(items) != 15 {
		t.Fatalf("len(items) = %d, want 15 (eye chip only per quiet PR)", len(items))
	}
	for _, it := range items {
		if it.Icon != "eye" {
			t.Errorf("chip %+v is not an eye chip", it)
		}
	}
}

// Attention states keep their chips in compact mode: a failing CI run and a
// review asking for changes. An approval, partial ("APPROVED 1/2") or
// complete, is not worth a chip there.
func TestBuildRowBadgePayloadsCompactKeepsAttentionStates(t *testing.T) {
	regs := compactTestRegs(8, "success", "")
	regs[0].CIBucket = "failure"
	regs[1].LatestReviewState = "CHANGES_REQUESTED"
	regs[2].LatestReviewState = "APPROVED"
	regs[3].CIBucket = "pending"
	regs[4].LatestReviewState = "APPROVED 1/2"
	regs[5].LatestReviewState = "COMMENTED"

	// Compact mode is chosen by length; this exercises it directly.
	var texts []string
	for _, it := range groupChips(chipsForRegistrations(regs, true)) {
		if it.Icon != "eye" {
			texts = append(texts, it.Text)
		}
	}
	want := []string{"failure", "CHANGES_REQUESTED", "COMMENTED"}
	if !slices.Equal(texts, want) {
		t.Errorf("non-eye chips = %v, want %v (pending CI, 'no review' and approvals stay hidden)", texts, want)
	}
}

// The merged chip is a single chip already and stays, in front.
func TestBuildRowBadgePayloadsCompactKeepsMergedChips(t *testing.T) {
	regs := queuedTestRegs(15, "success", "")
	regs = append(regs, registry.Registration{PRURL: "https://github.com/o/r/pull/99", SessionID: "sess1", Provider: "github", Status: registry.StatusMerged})
	items := buildRowBadgePayloads(regs)["sess1"].Items
	if len(items) != 16 || items[0].Icon != "check-circle" {
		t.Errorf("items = %+v, want the merged chip followed by 15 eye chips", items)
	}
}

func eyeChips(items []badgeChip) []badgeChip {
	var out []badgeChip
	for _, it := range items {
		if it.Icon == "eye" {
			out = append(out, it)
		}
	}
	return out
}

// In compact mode a partially approved PR has no chip of its own, so its eye
// chip carries the "something is up" signal: the tone the "APPROVED 1/2" chip
// had, and the progress in the tooltip.
func TestBuildRowBadgePayloadsCompactTintsEyeChipForPartialApproval(t *testing.T) {
	regs := compactTestRegs(8, "success", "")
	regs[0].LatestReviewState = "APPROVED 1/2"
	regs[1].LatestReviewState = "APPROVED"

	// Compact mode is chosen by length; this exercises it directly. The fully
	// approved PR and the unreviewed ones look the same and are one chip.
	eyes := eyeChips(groupChips(chipsForRegistrations(regs, true)))
	if len(eyes) != 2 {
		t.Fatalf("eye chips = %+v, want the partial approval and one merged chip for the rest", eyes)
	}
	if eyes[0].Tone != "info" || !strings.Contains(eyes[0].Tooltip, "APPROVED 1/2") {
		t.Errorf("partially approved eye chip = %+v, want info tone and the progress in the tooltip", eyes[0])
	}
	if eyes[1].Tone != "neutral" || eyes[1].Text != "7" {
		t.Errorf("approved and unreviewed eye chip = %+v, want neutral and a count of 7 (approval is not interesting)", eyes[1])
	}
}

func TestBuildRowBadgePayloadsCompactPartialApprovalKeepsMoreUrgentEyeTone(t *testing.T) {
	regs := compactTestRegs(8, "success", "")
	regs[0].LatestReviewState = "APPROVED 1/2"
	regs[0].HasConflict = true

	if tone := eyeChips(groupChips(chipsForRegistrations(regs, true)))[0].Tone; tone != "danger" {
		t.Errorf("eye chip tone = %q, want danger: a conflict outranks a partial approval", tone)
	}
}

// Outside compact mode the review chip itself shows the progress, so the eye
// chip keeps its ordinary look.
func TestBuildRowBadgePayloadsNormalModeLeavesEyeChipNeutralForPartialApproval(t *testing.T) {
	regs := compactTestRegs(2, "success", "APPROVED 1/2")
	for _, e := range eyeChips(buildRowBadgePayloads(regs)["sess1"].Items) {
		if e.Tone != "neutral" {
			t.Errorf("eye chip tone = %q, want neutral below the compact threshold", e.Tone)
		}
	}
}

func finishedReg(n int, status string, at time.Time) registry.Registration {
	return registry.Registration{
		PRURL: fmt.Sprintf("https://github.com/o/r/pull/%d", n), SessionID: "sess1", Provider: "github",
		Status: status, MergedAt: at,
	}
}

// A session that merged several PRs gets one chip with the count, first on
// the row, instead of one chip per PR.
func TestBuildRowBadgePayloadsCollapsesMergedPRsIntoOneCountedChipAtTheFront(t *testing.T) {
	base := time.Date(2026, 10, 8, 10, 0, 0, 0, time.UTC)
	regs := []registry.Registration{
		{PRURL: "https://github.com/o/r/pull/9", SessionID: "sess1", Provider: "github", Status: registry.StatusWatching, CIBucket: "success"},
		finishedReg(4, registry.StatusMerged, base.Add(2*time.Hour)), // newest merge, lowest number: the link must follow the merge time
		finishedReg(7, registry.StatusMerged, base),
		finishedReg(5, registry.StatusMerged, base.Add(time.Hour)),
	}

	items := buildRowBadgePayloads(regs)["sess1"].Items
	if len(items) != 4 { // merged summary + eye + ci + review of the watched PR
		t.Fatalf("items = %+v, want 4 chips: one merged summary then the watched PR's three", items)
	}
	merged := items[0]
	if merged.Icon != "check-circle" || merged.Text != "3" || merged.Tone != "success" {
		t.Errorf("first chip = %+v, want the green merged chip with count 3", merged)
	}
	if merged.Href != "https://github.com/o/r/pull/4" {
		t.Errorf("merged chip href = %q, want the most recently merged PR (#4)", merged.Href)
	}
	if !strings.HasPrefix(merged.Tooltip, "3 merged: ") || !strings.Contains(merged.Tooltip, "o/r#4") || !strings.Contains(merged.Tooltip, "o/r#7") {
		t.Errorf("tooltip = %q, want the count and every merged PR listed", merged.Tooltip)
	}
	if items[1].Icon != "eye" {
		t.Errorf("second chip = %+v, want the watched PR's eye chip right after the summary", items[1])
	}
}

func TestBuildRowBadgePayloadsSingleMergedPRKeepsPlainChip(t *testing.T) {
	items := buildRowBadgePayloads([]registry.Registration{finishedReg(1, registry.StatusMerged, time.Now())})["sess1"].Items
	if len(items) != 1 || items[0].Icon != "check-circle" || items[0].Text != "" {
		t.Errorf("items = %+v, want the plain merged chip without a count for a single PR", items)
	}
}

func TestBuildRowBadgePayloadsCountsClosedPRsSeparatelyAfterMerged(t *testing.T) {
	regs := []registry.Registration{
		{PRURL: "https://github.com/o/r/pull/9", SessionID: "sess1", Provider: "github", Status: registry.StatusWatching},
		finishedReg(1, registry.StatusClosed, time.Time{}),
		finishedReg(2, registry.StatusClosed, time.Time{}),
		finishedReg(3, registry.StatusMerged, time.Now()),
		finishedReg(4, registry.StatusMerged, time.Now()),
	}
	items := buildRowBadgePayloads(regs)["sess1"].Items
	if len(items) < 3 || items[0].Icon != "check-circle" || items[0].Text != "2" || items[1].Icon != "circle-slash" || items[1].Text != "2" || items[2].Icon != "eye" {
		t.Fatalf("items = %+v, want merged(2), closed(2), then the watched PR", items)
	}
	if items[1].Tone != "neutral" || !strings.HasPrefix(items[1].Tooltip, "2 closed: ") {
		t.Errorf("closed chip = %+v, want a neutral chip whose tooltip says 2 closed", items[1])
	}
}

// The tooltip is bounded: a long chain of PRs must not blow the row-badge
// payload cap.
func TestBuildRowBadgePayloadsFinishedTooltipListsAtMostEightPRs(t *testing.T) {
	var regs []registry.Registration
	for i := 1; i <= 12; i++ {
		regs = append(regs, finishedReg(i, registry.StatusMerged, time.Date(2026, 10, 8, 10, i, 0, 0, time.UTC)))
	}
	chip := buildRowBadgePayloads(regs)["sess1"].Items[0]
	if chip.Text != "12" || !strings.HasSuffix(chip.Tooltip, "+4 more") || strings.Contains(chip.Tooltip, "o/r#1,") {
		t.Errorf("chip = %+v, want count 12 and a tooltip of the 8 newest ending in '+4 more'", chip)
	}
}

// The host rejects a row-badge payload over 8KB and the badge then freezes at
// its last accepted state, so a session watching a lot of busy PRs must still
// get a payload that fits, saying how many chips were left out.
func TestRowBadgePayloadStaysUnderTheHostCap(t *testing.T) {
	var regs []registry.Registration
	for i := 1; i <= 80; i++ {
		regs = append(regs, registry.Registration{
			PRURL: fmt.Sprintf("https://github.com/org/repo/pull/%d", i), SessionID: "sess1", Provider: "github",
			Title: strings.Repeat("a long pull request title ", 10), CIBucket: "failure", CIDetail: strings.Repeat("failing job name ", 10),
			LatestReviewState: "CHANGES_REQUESTED", LatestReviewer: "reviewer", ReviewHref: fmt.Sprintf("https://github.com/org/repo/pull/%d#r", i),
			HasConflict: true,
			// Queue entries are never merged, which keeps this a long row.
			MergeQueueState: "QUEUED", MergeQueuePosition: i,
		})
	}

	badge := buildRowBadgePayloads(regs)["sess1"]
	raw, err := json.Marshal(badge)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > maxRowBadgeBytes {
		t.Fatalf("payload is %d bytes, want at most %d", len(raw), maxRowBadgeBytes)
	}
	last := badge.Items[len(badge.Items)-1]
	if !strings.HasPrefix(last.Text, "+") || !strings.Contains(last.Tooltip, "not shown") {
		t.Errorf("last chip = %+v, want an overflow chip counting what was left out", last)
	}

	again, _ := json.Marshal(buildRowBadgePayloads(regs)["sess1"])
	if !bytes.Equal(raw, again) {
		t.Error("payload differs between builds, want a deterministic result")
	}
}

func TestRowBadgePayloadIsUntouchedWhenItFits(t *testing.T) {
	regs := []registry.Registration{{PRURL: "https://github.com/org/repo/pull/1", SessionID: "sess1", Provider: "github", CIBucket: "failure"}}
	for _, c := range buildRowBadgePayloads(regs)["sess1"].Items {
		if strings.HasPrefix(c.Text, "+") {
			t.Errorf("unexpected overflow chip %+v", c)
		}
	}
}

func reviewerReg(mut func(*registry.Registration)) registry.Registration {
	r := registry.Registration{
		PRURL: "https://github.com/org/repo/pull/7", SessionID: "sess1", Provider: "github", Persona: registry.PersonaReviewer,
		Title: "Fix it", CIBucket: "failure", HasConflict: true, MergeQueueState: "QUEUED", MergeQueuePosition: 2,
		LatestReviewState: "CHANGES_REQUESTED", LatestReviewer: "bob",
	}
	if mut != nil {
		mut(&r)
	}
	return r
}

// A reviewer's badge is the plain PR chip plus one review chip about the
// reviewer's own progress: nothing about CI, conflicts, the merge queue or
// other people's verdicts.
func TestReviewerBadgeShowsOnlyThePRAndOwnReviewProgress(t *testing.T) {
	items := buildRowBadgePayloads([]registry.Registration{reviewerReg(nil)})["sess1"].Items
	if len(items) != 2 {
		t.Fatalf("chips = %+v, want the eye and one review chip", items)
	}
	eye, review := items[0], items[1]
	if eye.Icon != "eye" || eye.Tone != "neutral" || strings.Contains(eye.Tooltip, "conflict") || strings.Contains(eye.Tooltip, "queue") {
		t.Errorf("eye = %+v, want the plain PR chip without conflict or queue state", eye)
	}
	if review.Text != "in progress" || review.Tone != "info" {
		t.Errorf("review = %+v, want in progress until the reviewer approves", review)
	}
}

func TestReviewerReviewChipTracksOwnApprovalOnly(t *testing.T) {
	approved := buildRowBadgePayloads([]registry.Registration{reviewerReg(func(r *registry.Registration) { r.SelfReviewState = "APPROVED" })})["sess1"].Items
	if got := approved[len(approved)-1]; got.Text != "approved" || got.Tone != "success" {
		t.Errorf("own approval chip = %+v", got)
	}

	// Someone else approving is not the reviewer's own approval.
	others := buildRowBadgePayloads([]registry.Registration{reviewerReg(func(r *registry.Registration) { r.LatestReviewState = "APPROVED" })})["sess1"].Items
	if got := others[len(others)-1]; got.Text != "in progress" {
		t.Errorf("chip with only another's approval = %+v, want in progress", got)
	}

	changes := buildRowBadgePayloads([]registry.Registration{reviewerReg(func(r *registry.Registration) { r.SelfReviewState = "CHANGES_REQUESTED" })})["sess1"].Items
	if got := changes[len(changes)-1]; got.Text != "in progress" || !strings.Contains(got.Tooltip, "CHANGES_REQUESTED") {
		t.Errorf("own changes-requested chip = %+v, want in progress naming it", got)
	}
}

// Conflict and queue colouring on the eye chip follows the events filter like
// the other chips, so a watch that is not told about conflicts does not wear
// one.
func TestEyeChipHonoursTheEventsFilterForConflictAndQueue(t *testing.T) {
	r := registry.Registration{PRURL: "https://github.com/org/repo/pull/7", SessionID: "sess1", Provider: "github", HasConflict: true, MergeQueueState: "QUEUED", EventFilter: []string{"MERGED"}}
	eye := buildRowBadgePayloads([]registry.Registration{r})["sess1"].Items[0]
	if eye.Tone != "neutral" {
		t.Errorf("eye tone = %q, want neutral for a watch that does not follow conflicts or the queue", eye.Tone)
	}
	r.EventFilter = nil
	if eye := buildRowBadgePayloads([]registry.Registration{r})["sess1"].Items[0]; eye.Tone != "danger" {
		t.Errorf("unfiltered eye tone = %q, want danger for the conflict", eye.Tone)
	}
}

func notifySubInfo(session string, tracked int) subscriptionBadgeInfo {
	return subscriptionBadgeInfo{
		SessionID: session, ID: "sub-abc", Scope: "github.com/o/r", Mode: registry.SubscriptionModeNotify,
		Filters: provider.ScanFilters{ReviewRequested: "me", Draft: "exclude"}, Tracked: tracked,
		LastScanAt: time.Date(2026, 10, 20, 13, 44, 0, 0, time.Local),
	}
}

// A session that subscribed to a scope shows it on its row, even with no
// watch of its own: an orchestrator in notify mode has nothing else to show.
func TestSubscriptionChipShowsOnASessionWithoutWatches(t *testing.T) {
	got := buildRowBadgePayloadsWithSubs(nil, []subscriptionBadgeInfo{notifySubInfo("orch", 3)})
	badge, ok := got["orch"]
	if !ok || len(badge.Items) != 1 {
		t.Fatalf("badge = %+v (%v), want one chip", badge, ok)
	}
	chip := badge.Items[0]
	if chip.Icon != "radar" || chip.Text != "3" || chip.Tone != "info" {
		t.Errorf("chip = %+v, want the radar icon counting 3 tracked PRs", chip)
	}
	for _, want := range []string{"github.com/o/r", "notify", "review_requested=me", "draft=exclude", "3 tracked", "13:44"} {
		if !strings.Contains(chip.Tooltip, want) {
			t.Errorf("tooltip %q lacks %q", chip.Tooltip, want)
		}
	}
	if len(got) != 1 {
		t.Errorf("badges for %d sessions, want just the subscriber", len(got))
	}
}

func TestSubscriptionChipComesFirstAndWarnsOnAScanError(t *testing.T) {
	regs := []registry.Registration{{PRURL: "https://github.com/o/r/pull/1", SessionID: "sess1", Provider: "github", CIBucket: "success"}}
	sub := notifySubInfo("sess1", 0)
	sub.LastError = "github is down"
	items := buildRowBadgePayloadsWithSubs(regs, []subscriptionBadgeInfo{sub})["sess1"].Items
	if len(items) < 2 || items[0].Icon != "radar" || items[1].Icon != "eye" {
		t.Fatalf("items = %+v, want the subscription chip ahead of the watch chips", items)
	}
	if items[0].Tone != "warn" || !strings.Contains(items[0].Tooltip, "github is down") || items[0].Text != "" {
		t.Errorf("chip = %+v, want a warning with the error and no count", items[0])
	}
}

func TestSubscriptionChipForWatchModeAndSeveralSubscriptions(t *testing.T) {
	watch := subscriptionBadgeInfo{SessionID: "s", ID: "sub-w", Scope: "gitlab.corp.example/g", Mode: registry.SubscriptionModeWatch, Watches: 4}
	chip := buildRowBadgePayloadsWithSubs(nil, []subscriptionBadgeInfo{watch})["s"].Items[0]
	if chip.Text != "" || !strings.Contains(chip.Tooltip, "4 watching") || !strings.Contains(chip.Tooltip, "watch") {
		t.Errorf("watch-mode chip = %+v, want no count (the watches have their own chips) and the watch count in the tooltip", chip)
	}

	var subs []subscriptionBadgeInfo
	for i := range 7 {
		s := notifySubInfo("s", 2)
		s.ID = fmt.Sprintf("sub-%d", i)
		subs = append(subs, s)
	}
	chip = buildRowBadgePayloadsWithSubs(nil, subs)["s"].Items[0]
	if chip.Text != "14" || !strings.Contains(chip.Tooltip, "+2 more") {
		t.Errorf("chip = %+v, want 14 tracked in total and the tooltip capped with +2 more", chip)
	}
}

// A snoozed watch is asleep: the PR chip wears a moon instead of the eye, so a
// glance tells "quiet because nothing happens" from "quiet because muted".
func TestSnoozedWatchShowsASleepingIcon(t *testing.T) {
	awake := registry.Registration{PRURL: "https://github.com/o/r/pull/1", SessionID: "sess1", Provider: "github", CIBucket: "success", Title: "Fix it"}
	asleep := awake
	asleep.Snoozed = true

	if eye := buildRowBadgePayloads([]registry.Registration{awake})["sess1"].Items[0]; eye.Icon != "eye" || strings.Contains(eye.Tooltip, "snoozed") {
		t.Errorf("awake chip = %+v, want the eye and no mention of snoozing", eye)
	}
	eye := buildRowBadgePayloads([]registry.Registration{asleep})["sess1"].Items[0]
	if eye.Icon != "moon" || !strings.Contains(eye.Tooltip, "snoozed") || !strings.Contains(eye.Tooltip, "Fix it") {
		t.Errorf("snoozed chip = %+v, want a moon and the tooltip saying snoozed", eye)
	}
	if eye.Href != "https://github.com/o/r/pull/1" {
		t.Errorf("snoozed chip href = %q, want it still linking to the PR", eye.Href)
	}

	// Conflict colouring survives; only the icon and tooltip change.
	asleep.HasConflict = true
	if eye := buildRowBadgePayloads([]registry.Registration{asleep})["sess1"].Items[0]; eye.Icon != "moon" || eye.Tone != "danger" {
		t.Errorf("snoozed conflicting chip = %+v, want a moon that is still danger", eye)
	}

	// A merged PR is no longer being polled, so it never sleeps.
	merged := asleep
	merged.Status = registry.StatusMerged
	if chip := buildRowBadgePayloads([]registry.Registration{merged})["sess1"].Items[0]; chip.Icon != "check-circle" {
		t.Errorf("merged chip = %+v", chip)
	}
}

// The subscription chip leads the row in every case: ahead of the finished
// counts, and it survives the size cap that drops chips from the end.
func TestSubscriptionChipLeadsEvenWithFinishedChipsAndOverflow(t *testing.T) {
	regs := []registry.Registration{
		{PRURL: "https://github.com/o/r/pull/1", SessionID: "s", Provider: "github", Status: registry.StatusMerged},
		{PRURL: "https://github.com/o/r/pull/2", SessionID: "s", Provider: "github", Status: registry.StatusClosed},
	}
	items := buildRowBadgePayloadsWithSubs(regs, []subscriptionBadgeInfo{notifySubInfo("s", 1)})["s"].Items
	if len(items) != 3 || items[0].Icon != "radar" || items[1].Icon != "check-circle" || items[2].Icon != "circle-slash" {
		t.Errorf("items = %+v, want radar, merged, closed", items)
	}

	var many []registry.Registration
	for i := 1; i <= 80; i++ {
		many = append(many, registry.Registration{
			PRURL: fmt.Sprintf("https://github.com/org/repo/pull/%d", i), SessionID: "s", Provider: "github",
			Title: strings.Repeat("a long pull request title ", 10), CIBucket: "failure", CIDetail: strings.Repeat("failing job name ", 10),
			LatestReviewState: "CHANGES_REQUESTED", LatestReviewer: "reviewer", HasConflict: true,
			MergeQueueState: "QUEUED", MergeQueuePosition: i, // never merged: keeps this a long row
		})
	}
	items = buildRowBadgePayloadsWithSubs(many, []subscriptionBadgeInfo{notifySubInfo("s", 1)})["s"].Items
	if items[0].Icon != "radar" || !strings.HasPrefix(items[len(items)-1].Text, "+") {
		t.Errorf("first chip %+v, last chip %+v: want the radar first even when chips are dropped from the end", items[0], items[len(items)-1])
	}
}

func prsInState(n int, mut func(i int, r *registry.Registration)) []registry.Registration {
	var regs []registry.Registration
	for i := 1; i <= n; i++ {
		r := registry.Registration{
			PRURL: fmt.Sprintf("https://github.com/o/r/pull/%d", i), SessionID: "s", Provider: "github", CIBucket: "success", Title: "t",
		}
		if mut != nil {
			mut(i, &r)
		}
		regs = append(regs, r)
	}
	return regs
}

func chipTexts(items []badgeChip) []string {
	var out []string
	for _, c := range items {
		out = append(out, c.Icon+"|"+c.Text+"|"+c.Tone)
	}
	return out
}

// A session watching many PRs in the same state shows one chip per state, with
// a count, instead of the same three chips per PR.
func TestIdenticalChipsAcrossPRsAreMerged(t *testing.T) {
	items := buildRowBadgePayloads(prsInState(12, nil))["s"].Items
	want := []string{"eye|12|neutral", "|success ×12|success", "|no review ×12|neutral"}
	if got := chipTexts(items); !slices.Equal(got, want) {
		t.Fatalf("chips = %v, want %v", got, want)
	}
	eye := items[0]
	if eye.Text != "12" {
		t.Errorf("eye text = %q, want the count", eye.Text)
	}
	for _, want := range []string{"12", "watching", "o/r#1", "o/r#8", "+4 more"} {
		if !strings.Contains(eye.Tooltip, want) {
			t.Errorf("eye tooltip %q lacks %q", eye.Tooltip, want)
		}
	}
	if strings.Contains(eye.Tooltip, "o/r#9") {
		t.Errorf("eye tooltip %q lists more than the cap", eye.Tooltip)
	}
	if eye.Href != "https://github.com/o/r/pull/1" {
		t.Errorf("eye href = %q, want the first PR", eye.Href)
	}
}

func TestChipsOfDifferentStatesStayApartAndGroupByKind(t *testing.T) {
	regs := prsInState(10, func(i int, r *registry.Registration) {
		if i <= 6 {
			r.CIBucket = "failure"
		}
		if i == 9 {
			r.HasConflict = true
		}
	})
	got := chipTexts(buildRowBadgePayloads(regs)["s"].Items)
	// Eyes first, then CI, then review; within a kind, in order of appearance. The
	// one conflicting PR is its own chip and keeps the usual look (no count).
	want := []string{
		"eye|9|neutral", "eye||danger",
		"|failure ×6|danger", "|success ×4|success",
		"|no review ×10|neutral",
	}
	if !slices.Equal(got, want) {
		t.Errorf("chips = %v, want %v", got, want)
	}
}

func TestAGroupOfOneKeepsItsOwnChip(t *testing.T) {
	items := buildRowBadgePayloads(prsInState(1, nil))["s"].Items
	if got, want := chipTexts(items), []string{"eye||neutral", "|success|success", "|no review|neutral"}; !slices.Equal(got, want) {
		t.Errorf("chips = %v, want %v", got, want)
	}
	if !strings.Contains(items[0].Tooltip, "o/r#1") || strings.Contains(items[0].Tooltip, "watching:") {
		t.Errorf("a single PR's tooltip = %q, want the usual per-PR text", items[0].Tooltip)
	}
}

// Merge queue entries are ordered by position, which a merged count would hide.
func TestMergeQueueEntriesAreNotMerged(t *testing.T) {
	regs := prsInState(3, func(i int, r *registry.Registration) {
		r.MergeQueueState, r.MergeQueuePosition = "QUEUED", i
	})
	eyes := 0
	for _, c := range buildRowBadgePayloads(regs)["s"].Items {
		if c.Icon == "eye" {
			eyes++
		}
	}
	if eyes != 3 {
		t.Errorf("eye chips = %d, want one per queue entry", eyes)
	}
}

func TestSnoozedAndReviewerChipsGroupToo(t *testing.T) {
	asleep := prsInState(4, func(i int, r *registry.Registration) { r.Snoozed = true })
	items := buildRowBadgePayloads(asleep)["s"].Items
	if items[0].Icon != "moon" || items[0].Text != "4" || !strings.Contains(items[0].Tooltip, "snoozed") {
		t.Errorf("snoozed group = %+v, want one moon counting 4", items[0])
	}

	reviewing := prsInState(5, func(i int, r *registry.Registration) { r.Persona = registry.PersonaReviewer })
	got := chipTexts(buildRowBadgePayloads(reviewing)["s"].Items)
	if want := []string{"eye|5|neutral", "|in progress ×5|info"}; !slices.Equal(got, want) {
		t.Errorf("reviewer chips = %v, want %v", got, want)
	}
}

// With grouping a long row of PRs no longer needs compact mode.
func TestGroupingKeepsManyIdenticalPRsInFullMode(t *testing.T) {
	items := buildRowBadgePayloads(prsInState(60, func(i int, r *registry.Registration) { r.CIBucket = "failure" }))["s"].Items
	if got, want := chipTexts(items), []string{"eye|60|neutral", "|failure ×60|danger", "|no review ×60|neutral"}; !slices.Equal(got, want) {
		t.Errorf("chips = %v, want %v", got, want)
	}
}
