package main

import "github.com/cwrau/aoe-pr-watch-mcp/internal/registry"

const sessionPaneSlot = "pane"
const sessionPaneID = "pr_pane"

// buildSessionPanePayload renders one session's watched registrations as an
// in-session pane: a plain list of rows, each a direct link to a PR/MR that
// session watches (or has watched — merged/closed included, same as the
// row-badge, so the link survives after the PR/MR itself is resolved).
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
		children = append(children, sessionPaneRow(r))
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

func sessionPaneRow(r registry.Registration) rowBlock {
	label := r.Title
	if label == "" {
		label = shortRef(r)
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

	return rowBlock{
		Kind:   "row",
		Label:  label,
		Href:   r.PRURL,
		Badges: badges,
	}
}
