package main

import "github.com/cwrau/aoe-plugin-pr-watch-mcp/internal/registry"

const sessionPaneSlot = "pane"
const sessionPaneID = "pr_pane"

type dividerBlock struct {
	Kind string `json:"kind"`
}

// buildSessionPanePayload renders one session's watched registrations as an
// in-session pane: for each PR/MR that session watches (or has watched —
// merged/closed included, same as the row-badge, so the link survives after
// the PR/MR itself is resolved), a title line, a ref/URL line, and its
// description, title and ref/URL both linking straight to the PR/MR.
// StatusStopped registrations are excluded, same reasoning as
// buildRowBadgePayloads: the session explicitly asked to stop watching it.
// The bool result is false when there is nothing to show (every
// registration stopped, or none at all), so the caller knows to remove any
// pane it previously pushed rather than leave an empty one docked.
func buildSessionPanePayload(regs []registry.Registration) (panePayload, bool) {
	var children []any
	for _, r := range regs {
		if r.Status == registry.StatusStopped {
			continue
		}
		if len(children) > 0 {
			children = append(children, dividerBlock{Kind: "divider"})
		}
		children = append(children, sessionPaneBlocks(r)...)
	}
	if len(children) == 0 {
		return panePayload{}, false
	}
	return panePayload{
		Title:           "PR Watch",
		DefaultLocation: "right",
		Blocks:          children,
	}, true
}

// sessionPaneBlocks renders one registration as three stacked pieces: the
// full title, a reference line (the short owner/repo#N form for GitHub;
// GitLab's paths don't compress as recognizably, so the full URL instead),
// and the full description as plain, unlinked text. Title and the
// reference line are both clickable, direct links to the PR/MR itself.
func sessionPaneBlocks(r registry.Registration) []any {
	title := r.Title
	if title == "" {
		title = shortRef(r)
	}

	ref := shortRef(r)
	if r.Provider == "gitlab" {
		ref = r.PRURL
	}

	var badges []rowBadge
	switch r.Status {
	case registry.StatusMerged:
		badges = []rowBadge{{Text: "merged", Tone: "success"}}
	case registry.StatusClosed:
		badges = []rowBadge{{Text: "closed", Tone: "neutral"}}
	default:
		badges = []rowBadge{ciBadge(r.CIBucket), reviewBadge(r.LatestReviewState)}
	}

	blocks := []any{
		rowBlock{Kind: "row", Label: title, Href: r.PRURL, Badges: badges},
		rowBlock{Kind: "row", Label: ref, Href: r.PRURL},
	}
	if r.Description != "" {
		blocks = append(blocks, noteBlock{Kind: "note", Text: r.Description})
	}
	return blocks
}
