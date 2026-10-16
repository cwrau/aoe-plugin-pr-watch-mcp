package main

import (
	"fmt"
	"slices"
	"sort"

	"github.com/cwrau/aoe-plugin-pr-watch-mcp/internal/provider"
	"github.com/cwrau/aoe-plugin-pr-watch-mcp/internal/registry"
)

const homePaneSlot = "home-pane"
const homePaneID = "watched_prs"

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
	Scroll   bool   `json:"scroll,omitempty"`
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

// markdownBlock is the pane vocabulary's markdown kind (host api_version 15):
// the host renders and sanitises Text as GitHub-flavoured markdown. A host
// that predates it drops the block.
type markdownBlock struct {
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
// the raw session id.
func buildHomePanePayload(regs []registry.Registration, titleOf func(sessionID string) string) panePayload {
	var sorted []registry.Registration
	for _, r := range regs {
		if r.Status == registry.StatusWatching {
			sorted = append(sorted, r)
		}
	}
	// Ties break on identity, not input order: the registry returns rows by
	// next_due_at, which changes on every poll and would shuffle equal-severity
	// rows each time the pane is pushed.
	sort.Slice(sorted, func(i, j int) bool {
		a, b := sorted[i], sorted[j]
		if sa, sb := ciSeverity(a.CIBucket), ciSeverity(b.CIBucket); sa != sb {
			return sa < sb
		}
		if a.PRURL != b.PRURL {
			return a.PRURL < b.PRURL
		}
		return a.SessionID < b.SessionID
	})

	var children []any
	if len(sorted) == 0 {
		children = []any{noteBlock{Kind: "note", Text: "No PRs/MRs currently being watched."}}
	} else {
		for _, reg := range sorted {
			children = append(children, registrationColumns(reg, titleOf))
		}
	}

	return panePayload{
		Title: "PR Watch",
		Blocks: []any{
			sectionBlock{
				Kind: "section",
				// scroll caps this section's own height instead of letting
				// it grow unbounded — confirmed live: with 20+ active
				// registrations, an unscrolled section on a host-wide
				// home-pane (which "several plugins' home panes stack" on,
				// per docs/plugin-api.md, with no outer scroll container of
				// its own) pushed every other plugin's home-pane, and even
				// the dashboard's own default panes, off-screen below it.
				Title:    "Watched PRs/MRs",
				Value:    fmt.Sprintf("%d", len(sorted)),
				Scroll:   true,
				Children: children,
			},
		},
	}
}

func registrationColumns(reg registry.Registration, titleOf func(string) string) columnsBlock {
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

	// The provider badge always shows — it identifies the row, not an event
	// kind the session could have filtered out. Every other badge mirrors a
	// specific event.Kind (see registry.Registration.EventFilter's doc
	// comment): shown when reg.EventFilter is empty (the default, every
	// kind), or when its matching kind is explicitly in the filter.
	badges := []rowBadge{providerBadge(reg.Provider)}
	filter := reg.EffectiveEventFilter()
	if reg.HasConflict && eventFilterShows(filter, "CONFLICT") {
		badges = append(badges, conflictBadge())
	}
	if eventFilterShows(filter, "CI") {
		if ci, ok := ciBadge(reg.CIBucket); ok {
			badges = append(badges, ci)
		}
	}
	if eventFilterShows(filter, "REVIEW") {
		badges = append(badges, reviewBadgeFor(reg))
	}

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
// URL and provider — see provider.ShortRef, shared with the scheduler's
// notification text so a PR/MR reads identically wherever it's named.
func shortRef(reg registry.Registration) string {
	return provider.ShortRef(reg.PRURL, reg.Provider)
}

// eventFilterShows reports whether a badge tied to event kind should render
// for a registration carrying filter as its EventFilter: true when filter is
// empty (the default — every kind, so every badge shows, matching this
// dashboard's pre-filter behavior), or when kind is explicitly in it.
func eventFilterShows(filter []string, kind string) bool {
	return len(filter) == 0 || slices.Contains(filter, kind)
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

// ciBadge reports ok=false for an empty/"none" bucket — no CI signal at all
// isn't worth a badge (a row without one already reads as "nothing to show
// here"), so every caller omits the badge entirely instead of rendering a
// "no CI" placeholder.
func ciBadge(bucket string) (rowBadge, bool) {
	switch bucket {
	case "success":
		return rowBadge{Text: bucket, Tone: "success"}, true
	case "failure":
		return rowBadge{Text: bucket, Tone: "danger"}, true
	case "pending", "action_required":
		return rowBadge{Text: bucket, Tone: "warn"}, true
	default:
		return rowBadge{}, false
	}
}

// reviewBadgeFor is a registration's review badge: the latest review for an
// author, the reviewer's own progress ("in progress" until its own account
// approved) for a reviewer.
func reviewBadgeFor(r registry.Registration) rowBadge {
	if r.Persona == registry.PersonaReviewer {
		chip, _ := reviewerReviewChip(r)
		return rowBadge{Text: chip.Text, Tone: chip.Tone}
	}
	return reviewBadge(r.LatestReviewState)
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
