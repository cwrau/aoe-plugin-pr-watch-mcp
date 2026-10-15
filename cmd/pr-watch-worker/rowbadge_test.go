package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

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
	for _, chip := range buildRowBadgePayloads(regs)["sess1"].Items {
		switch chip.Icon {
		case "eye", "check-circle", "circle-slash":
			got = append(got, chip.Href)
		}
	}
	want := []string{
		"https://github.com/o/r/pull/5",
		"https://github.com/o/r/pull/2",
		"https://github.com/o/r/pull/9",
		"https://github.com/o/r/pull/10",
	}
	if !slices.Equal(got, want) {
		t.Errorf("chip order = %v, want %v", got, want)
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
	for _, chip := range buildRowBadgePayloads(regs)["sess1"].Items {
		if chip.Icon == "eye" {
			got = append(got, chip.Href)
		}
	}
	want := []string{
		"https://github.com/o/other/pull/2",
		"https://github.com/o/r/pull/20",
		"https://github.com/o/r/pull/5",
		"https://github.com/o/r/pull/7",
		"https://github.com/o/r/pull/1",
		"https://github.com/o/r/pull/3",
	}
	if !slices.Equal(got, want) {
		t.Errorf("chip order = %v, want %v", got, want)
	}
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

// Up to the threshold every watched PR shows its eye, CI and review chips.
func TestBuildRowBadgePayloadsKeepsAllChipsAtOrBelowCompactThreshold(t *testing.T) {
	regs := compactTestRegs(5, "success", "") // 5 x 3 = 15 chips
	items := buildRowBadgePayloads(regs)["sess1"].Items
	if len(items) != 15 {
		t.Errorf("len(items) = %d, want 15 (eye+CI+review per PR, below the threshold)", len(items))
	}
}

// Past the threshold the boring states (CI success/none/pending, no review)
// drop out and only the eye chip remains for a quiet PR.
func TestBuildRowBadgePayloadsCompactsQuietPRsPastThreshold(t *testing.T) {
	regs := compactTestRegs(8, "success", "") // 24 chips uncompacted
	items := buildRowBadgePayloads(regs)["sess1"].Items
	if len(items) != 8 {
		t.Fatalf("len(items) = %d, want 8 (eye chip only per quiet PR)", len(items))
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

	var texts []string
	for _, it := range buildRowBadgePayloads(regs)["sess1"].Items {
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
	regs := compactTestRegs(8, "success", "")
	regs = append(regs, registry.Registration{PRURL: "https://github.com/o/r/pull/99", SessionID: "sess1", Provider: "github", Status: registry.StatusMerged})
	items := buildRowBadgePayloads(regs)["sess1"].Items
	if len(items) != 9 || items[0].Icon != "check-circle" {
		t.Errorf("items = %+v, want the merged chip followed by 8 eye chips", items)
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

	eyes := eyeChips(buildRowBadgePayloads(regs)["sess1"].Items)
	if eyes[0].Tone != "info" || !strings.Contains(eyes[0].Tooltip, "APPROVED 1/2") {
		t.Errorf("partially approved eye chip = %+v, want info tone and the progress in the tooltip", eyes[0])
	}
	if eyes[1].Tone != "neutral" {
		t.Errorf("fully approved eye chip tone = %q, want neutral (approval is not interesting)", eyes[1].Tone)
	}
	if eyes[2].Tone != "neutral" {
		t.Errorf("unreviewed eye chip tone = %q, want neutral", eyes[2].Tone)
	}
}

func TestBuildRowBadgePayloadsCompactPartialApprovalKeepsMoreUrgentEyeTone(t *testing.T) {
	regs := compactTestRegs(8, "success", "")
	regs[0].LatestReviewState = "APPROVED 1/2"
	regs[0].HasConflict = true

	if tone := eyeChips(buildRowBadgePayloads(regs)["sess1"].Items)[0].Tone; tone != "danger" {
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
