package main

import (
	"testing"

	"github.com/cwrau/aoe-pr-watch-mcp/internal/registry"
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

	note, ok := payload.Blocks[2].(noteBlock)
	if !ok {
		t.Fatalf("Blocks[2] = %T, want noteBlock", payload.Blocks[2])
	}
	if note.Text != "This changes the widget to render correctly." {
		t.Errorf("note.Text = %q, want the full description", note.Text)
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
