package main

import (
	"testing"

	"github.com/cwrau/aoe-pr-watch-mcp/internal/registry"
)

func TestBuildHomePanePayloadEmpty(t *testing.T) {
	payload := buildHomePanePayload(nil, func(string) string { return "" }, "")

	if len(payload.Blocks) != 1 {
		t.Fatalf("Blocks len = %d, want 1", len(payload.Blocks))
	}
	sec, ok := payload.Blocks[0].(sectionBlock)
	if !ok {
		t.Fatalf("Blocks[0] = %T, want sectionBlock", payload.Blocks[0])
	}
	if sec.Value != "0" {
		t.Errorf("section.Value = %q, want %q", sec.Value, "0")
	}
	if len(sec.Children) != 1 {
		t.Fatalf("section.Children len = %d, want 1", len(sec.Children))
	}
	if _, ok := sec.Children[0].(noteBlock); !ok {
		t.Errorf("section.Children[0] = %T, want noteBlock", sec.Children[0])
	}
}

func githubReg(ciBucket, reviewState string) registry.Registration {
	return registry.Registration{
		PRURL:             "https://github.com/cwrau/aoe-pr-watch-mcp/pull/42",
		SessionID:         "sess1",
		Provider:          "github",
		CIBucket:          ciBucket,
		LatestReviewState: reviewState,
	}
}

func TestBuildHomePanePayloadGitHubRow(t *testing.T) {
	regs := []registry.Registration{githubReg("failure", "CHANGES_REQUESTED")}
	regs[0].Title = "Fix the widget rendering bug"
	payload := buildHomePanePayload(regs, func(id string) string {
		if id == "sess1" {
			return "Fix the thing"
		}
		return ""
	}, "http://127.0.0.1:25808")

	sec := payload.Blocks[0].(sectionBlock)
	if len(sec.Children) != 1 {
		t.Fatalf("section.Children len = %d, want 1", len(sec.Children))
	}
	cols, ok := sec.Children[0].(columnsBlock)
	if !ok {
		t.Fatalf("section.Children[0] = %T, want columnsBlock", sec.Children[0])
	}
	if len(cols.Children) != 2 {
		t.Fatalf("columns.Children len = %d, want 2", len(cols.Children))
	}

	prRow := cols.Children[0].(rowBlock)
	if prRow.Label != "Fix the widget rendering bug" {
		t.Errorf("prRow.Label = %q, want the PR title %q", prRow.Label, "Fix the widget rendering bug")
	}
	if prRow.Prefix != "cwrau/aoe-pr-watch-mcp#42" {
		t.Errorf("prRow.Prefix = %q, want the short ref %q", prRow.Prefix, "cwrau/aoe-pr-watch-mcp#42")
	}
	if prRow.Href != regs[0].PRURL {
		t.Errorf("prRow.Href = %q, want %q", prRow.Href, regs[0].PRURL)
	}
	if len(prRow.Badges) != 3 {
		t.Fatalf("prRow.Badges len = %d, want 3 (provider, ci, review)", len(prRow.Badges))
	}
	if prRow.Badges[0].Text != "GH" {
		t.Errorf("prRow.Badges[0].Text = %q, want %q", prRow.Badges[0].Text, "GH")
	}
	if prRow.Badges[1].Text != "failure" || prRow.Badges[1].Tone != "danger" {
		t.Errorf("prRow.Badges[1] = %+v, want {failure danger}", prRow.Badges[1])
	}
	if prRow.Badges[2].Text != "CHANGES_REQUESTED" || prRow.Badges[2].Tone != "danger" {
		t.Errorf("prRow.Badges[2] = %+v, want {CHANGES_REQUESTED danger}", prRow.Badges[2])
	}

	sessionRow := cols.Children[1].(rowBlock)
	if sessionRow.Label != "Fix the thing" {
		t.Errorf("sessionRow.Label = %q, want %q", sessionRow.Label, "Fix the thing")
	}
	if sessionRow.Href != "http://127.0.0.1:25808/session/sess1" {
		t.Errorf("sessionRow.Href = %q, want %q", sessionRow.Href, "http://127.0.0.1:25808/session/sess1")
	}
}

func TestBuildHomePanePayloadOmitsSessionHrefWhenBaseURLUnavailable(t *testing.T) {
	regs := []registry.Registration{githubReg("none", "")}
	payload := buildHomePanePayload(regs, func(string) string { return "" }, "")

	sec := payload.Blocks[0].(sectionBlock)
	cols := sec.Children[0].(columnsBlock)
	sessionRow := cols.Children[1].(rowBlock)
	if sessionRow.Href != "" {
		t.Errorf("sessionRow.Href = %q, want empty when no dashboard base URL is available (renderer's safeHref rejects anything non-http(s) anyway, so the plugin degrading to plain text is more honest than shipping a broken href)", sessionRow.Href)
	}
	if sessionRow.Label == "" {
		t.Error("sessionRow.Label is empty, want the session id fallback to still show even without a link")
	}
}

func TestBuildHomePanePayloadTrimsTrailingSlashOnBaseURL(t *testing.T) {
	regs := []registry.Registration{githubReg("none", "")}
	payload := buildHomePanePayload(regs, func(string) string { return "" }, "http://127.0.0.1:25808/")

	sec := payload.Blocks[0].(sectionBlock)
	cols := sec.Children[0].(columnsBlock)
	sessionRow := cols.Children[1].(rowBlock)
	if sessionRow.Href != "http://127.0.0.1:25808/session/sess1" {
		t.Errorf("sessionRow.Href = %q, want no double slash before /session", sessionRow.Href)
	}
}

func TestBuildHomePanePayloadGitLabRow(t *testing.T) {
	regs := []registry.Registration{{
		PRURL:     "https://gitlab.corp.example/team/project/-/merge_requests/17",
		SessionID: "sess2",
		Provider:  "gitlab",
		CIBucket:  "success",
	}}
	payload := buildHomePanePayload(regs, func(string) string { return "" }, "")

	sec := payload.Blocks[0].(sectionBlock)
	cols := sec.Children[0].(columnsBlock)
	prRow := cols.Children[0].(rowBlock)
	if prRow.Label != "team/project!17" {
		t.Errorf("prRow.Label = %q, want the short ref as a fallback label when no title has been fetched yet (%q)", prRow.Label, "team/project!17")
	}
	if prRow.Prefix != "" {
		t.Errorf("prRow.Prefix = %q, want empty when falling back to the short ref as Label (avoid showing it twice)", prRow.Prefix)
	}
	if prRow.Badges[0].Text != "GL" {
		t.Errorf("prRow.Badges[0].Text = %q, want %q", prRow.Badges[0].Text, "GL")
	}
	if prRow.Badges[1].Text != "success" || prRow.Badges[1].Tone != "success" {
		t.Errorf("prRow.Badges[1] = %+v, want {success success}", prRow.Badges[1])
	}
	if prRow.Badges[2].Text != "no review" || prRow.Badges[2].Tone != "neutral" {
		t.Errorf("prRow.Badges[2] = %+v, want {no review neutral} for an unreviewed MR", prRow.Badges[2])
	}

	sessionRow := cols.Children[1].(rowBlock)
	if sessionRow.Label != "sess2" {
		t.Errorf("sessionRow.Label = %q, want the raw session id as fallback when titleOf returns empty", sessionRow.Label)
	}
}

func TestBuildHomePanePayloadSortsFailuresFirst(t *testing.T) {
	success := githubReg("success", "APPROVED")
	success.PRURL = "https://github.com/o/r/pull/1"
	failure := githubReg("failure", "")
	failure.PRURL = "https://github.com/o/r/pull/2"

	payload := buildHomePanePayload([]registry.Registration{success, failure}, func(string) string { return "" }, "")

	sec := payload.Blocks[0].(sectionBlock)
	first := sec.Children[0].(columnsBlock).Children[0].(rowBlock)
	if first.Href != failure.PRURL {
		t.Errorf("first row href = %q, want the failing PR first (%q)", first.Href, failure.PRURL)
	}
}

func TestBuildHomePanePayloadSectionCountMatchesRegistrations(t *testing.T) {
	regs := []registry.Registration{githubReg("none", ""), githubReg("pending", "")}
	regs[0].PRURL = "https://github.com/o/r/pull/1"
	regs[1].PRURL = "https://github.com/o/r/pull/2"

	payload := buildHomePanePayload(regs, func(string) string { return "" }, "")
	sec := payload.Blocks[0].(sectionBlock)
	if sec.Value != "2" {
		t.Errorf("section.Value = %q, want %q", sec.Value, "2")
	}
	if len(sec.Children) != 2 {
		t.Errorf("section.Children len = %d, want 2", len(sec.Children))
	}
}
