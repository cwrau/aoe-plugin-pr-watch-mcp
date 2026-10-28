package main

import (
	"slices"

	"github.com/cwrau/aoe-plugin-pr-watch-mcp/internal/provider"
	"github.com/cwrau/aoe-plugin-pr-watch-mcp/internal/registry"
)

// panePayload mirrors the shape ui.state.set expects for a pane slot
// (docs/plugin-api.md "Pane payload"), used by the per-session pane.
type panePayload struct {
	Title           string `json:"title"`
	DefaultLocation string `json:"default_location,omitempty"`
	Blocks          []any  `json:"blocks"`
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
