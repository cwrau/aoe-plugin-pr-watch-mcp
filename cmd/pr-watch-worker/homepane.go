package main

import (
	"fmt"
	"sort"

	"github.com/cwrau/aoe-plugin-pr-watch-mcp/internal/provider"
	"github.com/cwrau/aoe-plugin-pr-watch-mcp/internal/registry"
)

const homePaneSlot = "home-pane"
const homePaneID = "watched_prs"

// dashboardBaseURLSettingKey must match the [[settings]] key declared in
// aoe-plugin.toml. Unused for session-href building now that the host
// accepts a plain relative href (see registrationColumns) — kept, setting
// and all, only so a revert back to absolute-URL building is a small diff
// if the relative-href host support turns out not to work everywhere.
const dashboardBaseURLSettingKey = "dashboard_base_url"

// panePayload mirrors the shape ui.state.set expects for a pane-family
// slot (docs/plugin-api.md "Pane payload"), shared by home-pane and the
// per-session pane. DefaultLocation/icon/footer are per-session-pane
// concepts that don't apply to a host-wide home-pane, so home-pane's own
// builder just leaves DefaultLocation unset (omitted from the JSON).
type panePayload struct {
	Title           string `json:"title"`
	DefaultLocation string `json:"default_location,omitempty"`
	Blocks          []any  `json:"blocks"`
}

type sectionBlock struct {
	Kind     string `json:"kind"`
	Title    string `json:"title,omitempty"`
	Value    string `json:"value,omitempty"`
	Children []any  `json:"children,omitempty"`
}

type columnsBlock struct {
	Kind     string `json:"kind"`
	Children []any  `json:"children"`
}

type rowBlock struct {
	Kind   string     `json:"kind"`
	Label  string     `json:"label,omitempty"`
	Href   string     `json:"href,omitempty"`
	Badges []rowBadge `json:"badges,omitempty"`
}

type rowBadge struct {
	Text string `json:"text,omitempty"`
	Tone string `json:"tone,omitempty"`
}

type noteBlock struct {
	Kind string `json:"kind"`
	Text string `json:"text"`
}

// buildHomePanePayload renders the currently-watched registrations as a
// home-pane payload: one section listing every actively-watched
// registration, each as a PR/MR link paired with its owning session's link.
// Registrations that reached a terminal status (merged/closed/stopped) are
// excluded — the dashboard is meant to show what's actively being watched
// right now, not a permanent history (that's what the row-badge is for).
// Visual only (provider, CI bucket, and review-state badges) — there's no
// search/filter block kind in the pane vocabulary to make this interactively
// filterable, so rows are just sorted failures-first to keep the most
// actionable ones on top. titleOf resolves a session id to its display
// title (aoeclient.Client's Title method); an empty result falls back to
// the raw session id. dashboardBaseURL is accepted but currently unused —
// see registrationColumns.
func buildHomePanePayload(regs []registry.Registration, titleOf func(sessionID string) string, dashboardBaseURL string) panePayload {
	var sorted []registry.Registration
	for _, r := range regs {
		if r.Status == registry.StatusWatching {
			sorted = append(sorted, r)
		}
	}
	sort.SliceStable(sorted, func(i, j int) bool {
		return ciSeverity(sorted[i].CIBucket) < ciSeverity(sorted[j].CIBucket)
	})

	var children []any
	if len(sorted) == 0 {
		children = []any{noteBlock{Kind: "note", Text: "No PRs/MRs currently being watched."}}
	} else {
		for _, reg := range sorted {
			children = append(children, registrationColumns(reg, titleOf, dashboardBaseURL))
		}
	}

	return panePayload{
		Title: "PR Watch",
		Blocks: []any{
			sectionBlock{
				Kind:     "section",
				Title:    "Watched PRs/MRs",
				Value:    fmt.Sprintf("%d", len(sorted)),
				Children: children,
			},
		},
	}
}

// dashboardBaseURL is unused: the host's pane renderer now accepts a plain
// relative href, so the session row always links to /session/<id> directly
// rather than needing an absolute URL built from a user-configured base.
// The parameter (and its setting, see dashboardBaseURLSettingKey) stays in
// place only so reverting to absolute-URL building is a small diff if the
// host's relative-href support doesn't pan out.
func registrationColumns(reg registry.Registration, titleOf func(string) string, dashboardBaseURL string) columnsBlock {
	sessionLabel := titleOf(reg.SessionID)
	if sessionLabel == "" {
		sessionLabel = reg.SessionID
	}

	sessionHref := "/session/" + reg.SessionID

	// Prefer the PR/MR's real title once the first successful check has
	// fetched it; before that (or if a fetch ever comes back empty), fall
	// back to the derived short ref instead. No Prefix field: the host used
	// to render it in a shrink-0 span that never truncated via CSS the way
	// Label does, so a long org/repo name there alone overflowed the grid
	// column and bled into the session column beside it (confirmed live) —
	// still no Prefix field here for that reason, but Label itself is no
	// longer truncated server-side (see homepane_test.go's
	// TestBuildHomePanePayloadDoesNotTruncateLongLabels for why: the host's
	// `columns` grid was patched to stack to one column below the `sm`
	// breakpoint and, verified live against a running dashboard at both the
	// stacked and 2-column widths, its Label span now cleanly
	// CSS-ellipsis-clips an unbroken 120-character string with no overflow
	// either way — sending the real, untruncated title lets the host's own
	// responsive clipping do a better job than a fixed character count ever
	// could).
	prLabel := reg.Title
	if prLabel == "" {
		prLabel = shortRef(reg)
	}

	badges := []rowBadge{providerBadge(reg.Provider)}
	if reg.HasConflict {
		badges = append(badges, conflictBadge())
	}
	badges = append(badges, ciBadge(reg.CIBucket), reviewBadge(reg.LatestReviewState))

	return columnsBlock{
		Kind: "columns",
		Children: []any{
			rowBlock{
				Kind:   "row",
				Label:  prLabel,
				Href:   reg.PRURL,
				Badges: badges,
			},
			rowBlock{
				Kind:  "row",
				Label: sessionLabel,
				Href:  sessionHref,
			},
		},
	}
}

// shortRef derives a compact PR/MR reference from the registration's own
// URL and provider — no extra API call, just the same parsers the checkers
// already use to validate the URL shape at registration time.
func shortRef(reg registry.Registration) string {
	switch reg.Provider {
	case "github":
		if owner, repo, number, err := provider.ParseGitHubPRURL(reg.PRURL); err == nil {
			return fmt.Sprintf("%s/%s#%d", owner, repo, number)
		}
	case "gitlab":
		if _, path, iid, err := provider.ParseGitLabMRURL(reg.PRURL); err == nil {
			return fmt.Sprintf("%s!%d", path, iid)
		}
	}
	return reg.PRURL
}

func providerBadge(prov string) rowBadge {
	if prov == "github" {
		return rowBadge{Text: "GH", Tone: "neutral"}
	}
	return rowBadge{Text: "GL", Tone: "neutral"}
}

func conflictBadge() rowBadge {
	return rowBadge{Text: "conflict", Tone: "danger"}
}

func ciBadge(bucket string) rowBadge {
	tone := "neutral"
	text := bucket
	switch bucket {
	case "success":
		tone = "success"
	case "failure":
		tone = "danger"
	case "pending", "action_required":
		tone = "warn"
	case "", "none":
		text = "no CI"
	}
	return rowBadge{Text: text, Tone: tone}
}

func reviewBadge(state string) rowBadge {
	if state == "" {
		return rowBadge{Text: "no review", Tone: "neutral"}
	}
	tone := "info"
	switch state {
	case "APPROVED":
		tone = "success"
	case "CHANGES_REQUESTED":
		tone = "danger"
	}
	return rowBadge{Text: state, Tone: tone}
}

// ciSeverity ranks CI buckets so the most actionable registrations sort
// first: a real failure or a gated action_required run before a pending
// check, ahead of a clean success, ahead of no CI signal at all.
func ciSeverity(bucket string) int {
	switch bucket {
	case "failure":
		return 0
	case "action_required":
		return 1
	case "pending":
		return 2
	case "success":
		return 3
	default:
		return 4
	}
}
