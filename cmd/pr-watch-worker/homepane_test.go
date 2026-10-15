package main

import (
	"strings"
	"testing"

	"github.com/cwrau/aoe-plugin-pr-watch-mcp/internal/registry"
)

func TestBuildHomePanePayloadEmpty(t *testing.T) {
	payload := buildHomePanePayload(nil, func(string) string { return "" })

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

// TestBuildHomePanePayloadSectionScrolls is a regression test: confirmed
// live, with 20+ active registrations an unscrolled home-pane section
// pushed every other plugin's home-pane (and even the dashboard's own
// default panes) off-screen below it — home-pane has no outer scroll
// container of its own (docs/plugin-api.md: "several plugins' home panes
// stack"), so the section itself must cap its own height.
func TestBuildHomePanePayloadSectionScrolls(t *testing.T) {
	payload := buildHomePanePayload(nil, func(string) string { return "" })
	sec := payload.Blocks[0].(sectionBlock)
	if !sec.Scroll {
		t.Error("section.Scroll = false, want true so a long list scrolls inside the section instead of growing unbounded")
	}
}

func githubReg(ciBucket, reviewState string) registry.Registration {
	return registry.Registration{
		PRURL:             "https://github.com/cwrau/aoe-plugin-pr-watch-mcp/pull/42",
		SessionID:         "sess1",
		Provider:          "github",
		CIBucket:          ciBucket,
		LatestReviewState: reviewState,
		Status:            registry.StatusWatching,
	}
}

func TestBuildHomePanePayloadGitHubRow(t *testing.T) {
	regs := []registry.Registration{githubReg("failure", "CHANGES_REQUESTED")}
	regs[0].Title = "Fix the bug"
	payload := buildHomePanePayload(regs, func(id string) string {
		if id == "sess1" {
			return "Fix the thing"
		}
		return ""
	})

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
	if prRow.Label != "Fix the bug" {
		t.Errorf("prRow.Label = %q, want the PR title %q", prRow.Label, "Fix the bug")
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
	if sessionRow.Href != "/session/sess1" {
		t.Errorf("sessionRow.Href = %q, want %q", sessionRow.Href, "/session/sess1")
	}
}

// TestBuildHomePanePayloadOmitsCIBadgeWithoutSignal confirms the "no CI"
// placeholder text is gone: a registration with no CI signal at all (empty
// CIBucket) gets no CI badge whatsoever, rather than a badge reading "no
// CI" — the user asked to drop the badge entirely, not just reword it.
func TestBuildHomePanePayloadOmitsCIBadgeWithoutSignal(t *testing.T) {
	reg := githubReg("", "")
	payload := buildHomePanePayload([]registry.Registration{reg}, func(string) string { return "" })

	sec := payload.Blocks[0].(sectionBlock)
	prRow := sec.Children[0].(columnsBlock).Children[0].(rowBlock)
	if len(prRow.Badges) != 2 {
		t.Fatalf("prRow.Badges len = %d, want 2 (provider, review — no CI badge at all)", len(prRow.Badges))
	}
	for _, b := range prRow.Badges {
		if b.Text == "no CI" {
			t.Errorf("prRow.Badges = %+v, want no \"no CI\" badge present", prRow.Badges)
		}
	}
}

func TestBuildHomePanePayloadShowsConflictBadgeBeforeCI(t *testing.T) {
	reg := githubReg("failure", "")
	reg.HasConflict = true
	payload := buildHomePanePayload([]registry.Registration{reg}, func(string) string { return "" })

	sec := payload.Blocks[0].(sectionBlock)
	prRow := sec.Children[0].(columnsBlock).Children[0].(rowBlock)
	if len(prRow.Badges) != 4 {
		t.Fatalf("prRow.Badges len = %d, want 4 (provider, conflict, ci, review)", len(prRow.Badges))
	}
	if prRow.Badges[1].Text != "conflict" || prRow.Badges[1].Tone != "danger" {
		t.Errorf("prRow.Badges[1] = %+v, want {conflict danger}", prRow.Badges[1])
	}
	if prRow.Badges[2].Text != "failure" {
		t.Errorf("prRow.Badges[2].Text = %q, want the CI badge right after the conflict badge", prRow.Badges[2].Text)
	}
}

func TestBuildHomePanePayloadOmitsConflictBadgeWhenNoConflict(t *testing.T) {
	regs := []registry.Registration{githubReg("success", "")}
	payload := buildHomePanePayload(regs, func(string) string { return "" })

	sec := payload.Blocks[0].(sectionBlock)
	prRow := sec.Children[0].(columnsBlock).Children[0].(rowBlock)
	if len(prRow.Badges) != 3 {
		t.Fatalf("prRow.Badges len = %d, want 3 (provider, ci, review) when there's no conflict", len(prRow.Badges))
	}
	for _, b := range prRow.Badges {
		if b.Text == "conflict" {
			t.Error("prRow.Badges contains a conflict badge, want none when HasConflict is false")
		}
	}
}

func TestBuildHomePanePayloadShowsOnlyMainBadgeForMergedOnlyFilter(t *testing.T) {
	reg := githubReg("failure", "CHANGES_REQUESTED")
	reg.HasConflict = true
	reg.EventFilter = []string{"MERGED"}
	payload := buildHomePanePayload([]registry.Registration{reg}, func(string) string { return "" })

	sec := payload.Blocks[0].(sectionBlock)
	prRow := sec.Children[0].(columnsBlock).Children[0].(rowBlock)
	if len(prRow.Badges) != 1 || prRow.Badges[0].Text != "GH" {
		t.Fatalf("prRow.Badges = %+v, want just the provider badge for a MERGED-only filter", prRow.Badges)
	}
}

func TestBuildHomePanePayloadShowsMainAndCIBadgeForCIOnlyFilter(t *testing.T) {
	reg := githubReg("failure", "CHANGES_REQUESTED")
	reg.HasConflict = true
	reg.EventFilter = []string{"CI"}
	payload := buildHomePanePayload([]registry.Registration{reg}, func(string) string { return "" })

	sec := payload.Blocks[0].(sectionBlock)
	prRow := sec.Children[0].(columnsBlock).Children[0].(rowBlock)
	if len(prRow.Badges) != 2 {
		t.Fatalf("prRow.Badges = %+v, want 2 (provider, ci) for a CI-only filter — no conflict or review badge", prRow.Badges)
	}
	if prRow.Badges[0].Text != "GH" {
		t.Errorf("prRow.Badges[0].Text = %q, want %q", prRow.Badges[0].Text, "GH")
	}
	if prRow.Badges[1].Text != "failure" {
		t.Errorf("prRow.Badges[1].Text = %q, want the CI badge", prRow.Badges[1].Text)
	}
}

func TestBuildHomePanePayloadShowsEveryBadgeForEmptyFilter(t *testing.T) {
	reg := githubReg("failure", "CHANGES_REQUESTED")
	reg.HasConflict = true
	payload := buildHomePanePayload([]registry.Registration{reg}, func(string) string { return "" })

	sec := payload.Blocks[0].(sectionBlock)
	prRow := sec.Children[0].(columnsBlock).Children[0].(rowBlock)
	if len(prRow.Badges) != 4 {
		t.Fatalf("prRow.Badges = %+v, want 4 (provider, conflict, ci, review) when no filter is set", prRow.Badges)
	}
}

func TestBuildHomePanePayloadShowsMainAndConflictBadgeForConflictOnlyFilter(t *testing.T) {
	reg := githubReg("failure", "CHANGES_REQUESTED")
	reg.HasConflict = true
	reg.EventFilter = []string{"CONFLICT"}
	payload := buildHomePanePayload([]registry.Registration{reg}, func(string) string { return "" })

	sec := payload.Blocks[0].(sectionBlock)
	prRow := sec.Children[0].(columnsBlock).Children[0].(rowBlock)
	if len(prRow.Badges) != 2 {
		t.Fatalf("prRow.Badges = %+v, want 2 (provider, conflict) for a CONFLICT-only filter — no CI or review badge", prRow.Badges)
	}
	if prRow.Badges[1].Text != "conflict" {
		t.Errorf("prRow.Badges[1].Text = %q, want the conflict badge", prRow.Badges[1].Text)
	}
}

func TestBuildHomePanePayloadUsesRelativeSessionHrefRegardlessOfBaseURL(t *testing.T) {
	regs := []registry.Registration{githubReg("none", "")}
	payload := buildHomePanePayload(regs, func(string) string { return "" })

	sec := payload.Blocks[0].(sectionBlock)
	cols := sec.Children[0].(columnsBlock)
	sessionRow := cols.Children[1].(rowBlock)
	if sessionRow.Href != "/session/sess1" {
		t.Errorf("sessionRow.Href = %q, want %q (the host now accepts a plain relative href, no base URL needed)", sessionRow.Href, "/session/sess1")
	}
	if sessionRow.Label == "" {
		t.Error("sessionRow.Label is empty, want the session id fallback to still show")
	}
}

func TestBuildHomePanePayloadGitLabRow(t *testing.T) {
	regs := []registry.Registration{{
		PRURL:     "https://gitlab.corp.example/team/project/-/merge_requests/17",
		SessionID: "sess2",
		Provider:  "gitlab",
		CIBucket:  "success",
		Status:    registry.StatusWatching,
	}}
	payload := buildHomePanePayload(regs, func(string) string { return "" })

	sec := payload.Blocks[0].(sectionBlock)
	cols := sec.Children[0].(columnsBlock)
	prRow := cols.Children[0].(rowBlock)
	if prRow.Label != "team/project!17" {
		t.Errorf("prRow.Label = %q, want the short ref as a fallback label when no title has been fetched yet (%q)", prRow.Label, "team/project!17")
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

	payload := buildHomePanePayload([]registry.Registration{success, failure}, func(string) string { return "" })

	sec := payload.Blocks[0].(sectionBlock)
	first := sec.Children[0].(columnsBlock).Children[0].(rowBlock)
	if first.Href != failure.PRURL {
		t.Errorf("first row href = %q, want the failing PR first (%q)", first.Href, failure.PRURL)
	}
}

// The registry hands rows over ordered by next_due_at, which shifts on every
// poll; rows of equal severity must not follow that order or the pane jumps.
func TestBuildHomePanePayloadOrderIsIndependentOfInputOrder(t *testing.T) {
	var regs []registry.Registration
	for _, n := range []string{"1", "2", "3", "4"} {
		r := githubReg("success", "APPROVED")
		r.PRURL = "https://github.com/o/r/pull/" + n
		r.SessionID = "sess" + n
		regs = append(regs, r)
	}
	hrefs := func(in []registry.Registration) []string {
		sec := buildHomePanePayload(in, func(string) string { return "" }).Blocks[0].(sectionBlock)
		var out []string
		for _, c := range sec.Children {
			out = append(out, c.(columnsBlock).Children[0].(rowBlock).Href)
		}
		return out
	}

	want := hrefs(regs)
	reversed := []registry.Registration{regs[3], regs[1], regs[0], regs[2]}
	got := hrefs(reversed)
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("row order depends on input order: %v vs %v", want, got)
		}
	}
}

func TestBuildHomePanePayloadExcludesTerminalStatusRegistrations(t *testing.T) {
	watching := githubReg("success", "APPROVED")
	watching.PRURL = "https://github.com/o/r/pull/1"
	merged := githubReg("success", "APPROVED")
	merged.PRURL = "https://github.com/o/r/pull/2"
	merged.Status = registry.StatusMerged
	stopped := githubReg("success", "APPROVED")
	stopped.PRURL = "https://github.com/o/r/pull/3"
	stopped.Status = registry.StatusStopped

	payload := buildHomePanePayload([]registry.Registration{watching, merged, stopped}, func(string) string { return "" })

	sec := payload.Blocks[0].(sectionBlock)
	if sec.Value != "1" {
		t.Errorf("section.Value = %q, want %q (only the watching registration should count)", sec.Value, "1")
	}
	if len(sec.Children) != 1 {
		t.Fatalf("section.Children len = %d, want 1", len(sec.Children))
	}
	row := sec.Children[0].(columnsBlock).Children[0].(rowBlock)
	if row.Href != watching.PRURL {
		t.Errorf("row.Href = %q, want the still-watching registration's URL %q", row.Href, watching.PRURL)
	}
}

// TestBuildHomePanePayloadDoesNotTruncateLongLabels used to assert a
// server-side character cap here, back when the host's `columns` grid item
// had no min-width: 0 and an unbroken long Label could overflow its 50%
// track into the sibling column (confirmed live). That grid was patched
// (aoe commit 343f1050) to stack to one column below the `sm` breakpoint;
// verified live against a running dashboard at both the stacked (<sm) and
// 2-column (>=sm) widths, using the exact production row markup with an
// unbroken 120-character label, that its Label span now cleanly
// CSS-ellipsis-clips with zero overflow at either width. The host's own
// responsive clipping does a better job than any fixed character count
// could (it adapts to the pane's actual current pixel width), so this
// plugin no longer pre-truncates — it sends the real title/session label
// untouched and lets the host render it.
func TestBuildHomePanePayloadDoesNotTruncateLongLabels(t *testing.T) {
	reg := githubReg("success", "APPROVED")
	reg.Title = strings.Repeat("a very long PR title that keeps going ", 5)
	longSessionTitle := strings.Repeat("a very long session title that keeps going ", 5)

	payload := buildHomePanePayload([]registry.Registration{reg}, func(string) string { return longSessionTitle })

	sec := payload.Blocks[0].(sectionBlock)
	cols := sec.Children[0].(columnsBlock)
	prRow := cols.Children[0].(rowBlock)
	sessionRow := cols.Children[1].(rowBlock)

	if prRow.Label != reg.Title {
		t.Errorf("prRow.Label = %q, want the real title untouched, %q", prRow.Label, reg.Title)
	}
	if sessionRow.Label != longSessionTitle {
		t.Errorf("sessionRow.Label = %q, want the real session title untouched, %q", sessionRow.Label, longSessionTitle)
	}
}

func TestBuildHomePanePayloadSectionCountMatchesRegistrations(t *testing.T) {
	regs := []registry.Registration{githubReg("none", ""), githubReg("pending", "")}
	regs[0].PRURL = "https://github.com/o/r/pull/1"
	regs[1].PRURL = "https://github.com/o/r/pull/2"

	payload := buildHomePanePayload(regs, func(string) string { return "" })
	sec := payload.Blocks[0].(sectionBlock)
	if sec.Value != "2" {
		t.Errorf("section.Value = %q, want %q", sec.Value, "2")
	}
	if len(sec.Children) != 2 {
		t.Errorf("section.Children len = %d, want 2", len(sec.Children))
	}
}
