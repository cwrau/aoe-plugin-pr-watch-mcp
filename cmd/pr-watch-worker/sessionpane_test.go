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

func TestBuildSessionPanePayloadWatchingRow(t *testing.T) {
	regs := []registry.Registration{
		{
			PRURL: "https://github.com/cwrau/aoe-pr-watch-mcp/pull/42", SessionID: "sess1", Provider: "github",
			Title: "Fix the widget rendering bug", CIBucket: "failure", LatestReviewState: "APPROVED",
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
	if len(payload.Blocks) != 1 {
		t.Fatalf("Blocks len = %d, want 1", len(payload.Blocks))
	}
	row, ok := payload.Blocks[0].(rowBlock)
	if !ok {
		t.Fatalf("Blocks[0] = %T, want rowBlock", payload.Blocks[0])
	}
	if row.Label != "Fix the widget rendering bug" {
		t.Errorf("row.Label = %q, want the PR title", row.Label)
	}
	if row.Href != regs[0].PRURL {
		t.Errorf("row.Href = %q, want %q", row.Href, regs[0].PRURL)
	}
	if len(row.Badges) != 2 || row.Badges[0].Text != "failure" || row.Badges[1].Text != "APPROVED" {
		t.Errorf("row.Badges = %+v, want CI+review badges", row.Badges)
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
	row := payload.Blocks[0].(rowBlock)
	if row.Label != "cwrau/aoe-pr-watch-mcp#42" {
		t.Errorf("row.Label = %q, want the short ref fallback", row.Label)
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
	if len(payload.Blocks) != 2 {
		t.Fatalf("Blocks len = %d, want 2 (merged + closed; stopped excluded)", len(payload.Blocks))
	}
	merged := payload.Blocks[0].(rowBlock)
	if len(merged.Badges) != 1 || merged.Badges[0].Text != "merged" || merged.Badges[0].Tone != "success" {
		t.Errorf("merged row badges = %+v, want {merged success}", merged.Badges)
	}
	closed := payload.Blocks[1].(rowBlock)
	if len(closed.Badges) != 1 || closed.Badges[0].Text != "closed" || closed.Badges[0].Tone != "neutral" {
		t.Errorf("closed row badges = %+v, want {closed neutral}", closed.Badges)
	}
}
