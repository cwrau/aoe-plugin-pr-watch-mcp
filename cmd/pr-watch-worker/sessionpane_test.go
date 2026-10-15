package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cwrau/aoe-plugin-pr-watch-mcp/internal/registry"
)

func TestBuildSessionPanePayloadEmptyGivesNoPane(t *testing.T) {
	if _, ok := buildSessionPanePayload(nil); ok {
		t.Error("buildSessionPanePayload(nil) ok = true, want false (nothing to show)")
	}
}

func TestBuildSessionPanePayloadAllStoppedGivesNoPane(t *testing.T) {
	regs := []registry.Registration{
		{PRURL: "https://github.com/o/r/pull/1", SessionID: "sess1", Provider: "github", Status: registry.StatusStopped},
	}
	if _, ok := buildSessionPanePayload(regs); ok {
		t.Error("buildSessionPanePayload() ok = true, want false when every registration is stopped")
	}
}

func TestBuildSessionPanePayloadGitHubRegistration(t *testing.T) {
	regs := []registry.Registration{
		{
			PRURL: "https://github.com/cwrau/aoe-pr-watch-mcp/pull/42", SessionID: "sess1", Provider: "github",
			Title: "Fix the widget rendering bug", Description: "This changes the widget to render correctly.",
			CIBucket: "failure", LatestReviewState: "APPROVED",
			Status: registry.StatusWatching,
		},
	}

	payload, ok := buildSessionPanePayload(regs)
	if !ok {
		t.Fatal("buildSessionPanePayload() ok = false, want true")
	}
	if payload.DefaultLocation != "right" {
		t.Errorf("DefaultLocation = %q, want %q", payload.DefaultLocation, "right")
	}
	if len(payload.Blocks) != 3 {
		t.Fatalf("Blocks len = %d, want 3 (title row, ref row, description note)", len(payload.Blocks))
	}

	titleRow, ok := payload.Blocks[0].(rowBlock)
	if !ok {
		t.Fatalf("Blocks[0] = %T, want rowBlock", payload.Blocks[0])
	}
	if titleRow.Label != "Fix the widget rendering bug" {
		t.Errorf("titleRow.Label = %q, want the PR title", titleRow.Label)
	}
	if titleRow.Href != regs[0].PRURL {
		t.Errorf("titleRow.Href = %q, want %q (title must be clickable)", titleRow.Href, regs[0].PRURL)
	}
	if len(titleRow.Badges) != 2 || titleRow.Badges[0].Text != "failure" || titleRow.Badges[1].Text != "APPROVED" {
		t.Errorf("titleRow.Badges = %+v, want CI+review badges", titleRow.Badges)
	}

	refRow, ok := payload.Blocks[1].(rowBlock)
	if !ok {
		t.Fatalf("Blocks[1] = %T, want rowBlock", payload.Blocks[1])
	}
	if refRow.Label != "cwrau/aoe-pr-watch-mcp#42" {
		t.Errorf("refRow.Label = %q, want the short GitHub ref", refRow.Label)
	}
	if refRow.Href != regs[0].PRURL {
		t.Errorf("refRow.Href = %q, want %q (ref line must be clickable too)", refRow.Href, regs[0].PRURL)
	}

	// The description is markdown (a PR/MR body); the host renders it and
	// sanitises it.
	desc, ok := payload.Blocks[2].(markdownBlock)
	if !ok {
		t.Fatalf("Blocks[2] = %T, want markdownBlock", payload.Blocks[2])
	}
	if desc.Kind != "markdown" || desc.Text != "This changes the widget to render correctly." {
		t.Errorf("description block = %+v, want kind markdown with the full description", desc)
	}
}

func TestBuildSessionPanePayloadOnlyCIBadgeForCIOnlyFilter(t *testing.T) {
	regs := []registry.Registration{
		{
			PRURL: "https://github.com/cwrau/aoe-pr-watch-mcp/pull/42", SessionID: "sess1", Provider: "github",
			CIBucket: "failure", LatestReviewState: "APPROVED",
			Status: registry.StatusWatching, EventFilter: []string{"CI"},
		},
	}

	payload, ok := buildSessionPanePayload(regs)
	if !ok {
		t.Fatal("buildSessionPanePayload() ok = false, want true")
	}
	titleRow := payload.Blocks[0].(rowBlock)
	if len(titleRow.Badges) != 1 || titleRow.Badges[0].Text != "failure" {
		t.Errorf("titleRow.Badges = %+v, want just the CI badge for a CI-only filter", titleRow.Badges)
	}
}

func TestBuildSessionPanePayloadNoBadgesForMergedOnlyFilter(t *testing.T) {
	regs := []registry.Registration{
		{
			PRURL: "https://github.com/cwrau/aoe-pr-watch-mcp/pull/42", SessionID: "sess1", Provider: "github",
			CIBucket: "failure", LatestReviewState: "APPROVED",
			Status: registry.StatusWatching, EventFilter: []string{"MERGED"},
		},
	}

	payload, ok := buildSessionPanePayload(regs)
	if !ok {
		t.Fatal("buildSessionPanePayload() ok = false, want true")
	}
	titleRow := payload.Blocks[0].(rowBlock)
	if len(titleRow.Badges) != 0 {
		t.Errorf("titleRow.Badges = %+v, want none for a MERGED-only filter", titleRow.Badges)
	}
}

func TestBuildSessionPanePayloadGitLabUsesFullURLNotShortRef(t *testing.T) {
	regs := []registry.Registration{
		{
			PRURL: "https://gitlab.corp.example/team/project/-/merge_requests/17", SessionID: "sess1", Provider: "gitlab",
			Title: "Fix the thing", Status: registry.StatusWatching,
		},
	}

	payload, ok := buildSessionPanePayload(regs)
	if !ok {
		t.Fatal("buildSessionPanePayload() ok = false, want true")
	}
	refRow := payload.Blocks[1].(rowBlock)
	if refRow.Label != regs[0].PRURL {
		t.Errorf("refRow.Label = %q, want the full URL for GitLab (not a short ref)", refRow.Label)
	}
	if refRow.Href != regs[0].PRURL {
		t.Errorf("refRow.Href = %q, want %q", refRow.Href, regs[0].PRURL)
	}
}

func TestBuildSessionPanePayloadFallsBackToShortRefWithoutTitle(t *testing.T) {
	regs := []registry.Registration{
		{PRURL: "https://github.com/cwrau/aoe-pr-watch-mcp/pull/42", SessionID: "sess1", Provider: "github", Status: registry.StatusWatching},
	}

	payload, ok := buildSessionPanePayload(regs)
	if !ok {
		t.Fatal("buildSessionPanePayload() ok = false, want true")
	}
	titleRow := payload.Blocks[0].(rowBlock)
	if titleRow.Label != "cwrau/aoe-pr-watch-mcp#42" {
		t.Errorf("titleRow.Label = %q, want the short ref fallback", titleRow.Label)
	}
}

func TestBuildSessionPanePayloadOmitsNoteWithoutDescription(t *testing.T) {
	regs := []registry.Registration{
		{PRURL: "https://github.com/o/r/pull/1", SessionID: "sess1", Provider: "github", Status: registry.StatusWatching},
	}

	payload, ok := buildSessionPanePayload(regs)
	if !ok {
		t.Fatal("buildSessionPanePayload() ok = false, want true")
	}
	if len(payload.Blocks) != 2 {
		t.Fatalf("Blocks len = %d, want 2 (title row + ref row, no note when there's no description)", len(payload.Blocks))
	}
}

func TestBuildSessionPanePayloadMergedAndClosedRows(t *testing.T) {
	regs := []registry.Registration{
		{PRURL: "https://github.com/o/r/pull/1", SessionID: "sess1", Provider: "github", Status: registry.StatusMerged},
		{PRURL: "https://github.com/o/r/pull/2", SessionID: "sess1", Provider: "github", Status: registry.StatusClosed},
		{PRURL: "https://github.com/o/r/pull/3", SessionID: "sess1", Provider: "github", Status: registry.StatusStopped},
	}

	payload, ok := buildSessionPanePayload(regs)
	if !ok {
		t.Fatal("buildSessionPanePayload() ok = false, want true")
	}
	// merged: title row + ref row (2), divider (1), closed: title row + ref row (2). Stopped excluded entirely.
	if len(payload.Blocks) != 5 {
		t.Fatalf("Blocks len = %d, want 5 (2 rows + divider + 2 rows; stopped excluded)", len(payload.Blocks))
	}
	merged := payload.Blocks[0].(rowBlock)
	if len(merged.Badges) != 1 || merged.Badges[0].Text != "merged" || merged.Badges[0].Tone != "success" {
		t.Errorf("merged row badges = %+v, want {merged success}", merged.Badges)
	}
	if _, ok := payload.Blocks[2].(dividerBlock); !ok {
		t.Fatalf("Blocks[2] = %T, want dividerBlock between entries", payload.Blocks[2])
	}
	closed := payload.Blocks[3].(rowBlock)
	if len(closed.Badges) != 1 || closed.Badges[0].Text != "closed" || closed.Badges[0].Tone != "neutral" {
		t.Errorf("closed row badges = %+v, want {closed neutral}", closed.Badges)
	}
}

func sessionPaneSize(t *testing.T, p panePayload) int {
	t.Helper()
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return len(b)
}

// sessionPaneNotes is the text of every note and markdown block: the trim
// note and the descriptions.
func sessionPaneNotes(p panePayload) []string {
	var out []string
	for _, b := range p.Blocks {
		switch n := b.(type) {
		case noteBlock:
			out = append(out, n.Text)
		case markdownBlock:
			out = append(out, n.Text)
		}
	}
	return out
}

func sessionPaneHrefs(p panePayload) map[string]bool {
	out := map[string]bool{}
	for _, b := range p.Blocks {
		if r, ok := b.(rowBlock); ok && r.Href != "" {
			out[r.Href] = true
		}
	}
	return out
}

func TestBuildSessionPanePayloadWithinBudgetIsNotTrimmed(t *testing.T) {
	regs := []registry.Registration{
		{PRURL: "https://github.com/o/r/pull/1", SessionID: "s", Provider: "github", Status: registry.StatusWatching, Description: "small"},
		{PRURL: "https://github.com/o/r/pull/2", SessionID: "s", Provider: "github", Status: registry.StatusMerged, Description: "small"},
	}
	p, _ := buildSessionPanePayload(regs)
	for _, n := range sessionPaneNotes(p) {
		if strings.Contains(n, "trimmed") {
			t.Errorf("unexpected trim note %q for a small pane", n)
		}
	}
}

// The host rejects panes over 64KB, which silently freezes the pane. Finished
// entries' descriptions go first, and the pane says so.
func TestBuildSessionPanePayloadHidesFinishedDescriptionsWhenOverBudget(t *testing.T) {
	big := strings.Repeat("d", 20*1024)
	regs := []registry.Registration{
		{PRURL: "https://github.com/o/r/pull/1", SessionID: "s", Provider: "github", Status: registry.StatusWatching, Description: "watched description"},
	}
	for i := 2; i <= 5; i++ {
		regs = append(regs, registry.Registration{PRURL: fmt.Sprintf("https://github.com/o/r/pull/%d", i), SessionID: "s", Provider: "github", Status: registry.StatusMerged, Description: big})
	}

	p, ok := buildSessionPanePayload(regs)
	if !ok {
		t.Fatal("ok = false, want a pane")
	}
	if got := sessionPaneSize(t, p); got > maxSessionPaneBytes {
		t.Errorf("pane is %d bytes, want <= %d", got, maxSessionPaneBytes)
	}
	notes := strings.Join(sessionPaneNotes(p), "\n")
	if !strings.Contains(notes, "trimmed") || !strings.Contains(notes, "4 finished description") {
		t.Errorf("notes = %q, want a trim note naming the 4 hidden finished descriptions", notes)
	}
	if !strings.Contains(notes, "watched description") {
		t.Errorf("notes = %q, want the watched PR's description kept", notes)
	}
	if len(sessionPaneHrefs(p)) != 5 {
		t.Errorf("hrefs = %v, want all 5 PRs still listed", sessionPaneHrefs(p))
	}
}

// Past that, the oldest finished entries are omitted, never a watched one.
func TestBuildSessionPanePayloadDropsOldestFinishedEntriesWhenStillOverBudget(t *testing.T) {
	regs := []registry.Registration{
		{PRURL: "https://github.com/o/r/pull/1", SessionID: "s", Provider: "github", Status: registry.StatusWatching, CreatedAt: time.Unix(1, 0)},
	}
	for i := 2; i <= 400; i++ {
		regs = append(regs, registry.Registration{
			PRURL: fmt.Sprintf("https://github.com/o/r/pull/%d", i), SessionID: "s", Provider: "github",
			Status: registry.StatusMerged, CreatedAt: time.Unix(int64(i), 0),
			Title: strings.Repeat("t", 120),
		})
	}

	p, _ := buildSessionPanePayload(regs)
	if got := sessionPaneSize(t, p); got > maxSessionPaneBytes {
		t.Errorf("pane is %d bytes, want <= %d", got, maxSessionPaneBytes)
	}
	hrefs := sessionPaneHrefs(p)
	if !hrefs["https://github.com/o/r/pull/1"] {
		t.Error("watched PR missing from the trimmed pane")
	}
	if !hrefs["https://github.com/o/r/pull/400"] {
		t.Error("newest finished PR dropped, want the oldest dropped first")
	}
	if hrefs["https://github.com/o/r/pull/2"] {
		t.Error("oldest finished PR kept, want it dropped first")
	}
	notes := strings.Join(sessionPaneNotes(p), "\n")
	if !strings.Contains(notes, "omitted") {
		t.Errorf("notes = %q, want a note that finished entries were omitted", notes)
	}
}
