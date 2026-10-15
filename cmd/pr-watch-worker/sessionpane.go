package main

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/cwrau/aoe-plugin-pr-watch-mcp/internal/registry"
)

const sessionPaneSlot = "pane"
const sessionPaneID = "pr_pane"

// maxSessionPaneBytes is the largest pane payload pushed. The host rejects a
// pane over 64KB outright, which silently freezes it at its last accepted
// state, so the budget keeps a margin for the host's own normalization.
const maxSessionPaneBytes = 56 * 1024

type dividerBlock struct {
	Kind string `json:"kind"`
}

// paneTrim records what buildSessionPanePayload had to give up to fit the
// budget, so the pane can say so.
type paneTrim struct {
	finishedDescriptionsHidden int
	finishedOmitted            int
	watchedDescriptionsHidden  int
}

func (t paneTrim) any() bool {
	return t.finishedDescriptionsHidden > 0 || t.finishedOmitted > 0 || t.watchedDescriptionsHidden > 0
}

func (t paneTrim) note() string {
	text := "Pane trimmed to fit the host's size limit:"
	if t.finishedDescriptionsHidden > 0 {
		text += fmt.Sprintf(" %d finished description(s) hidden;", t.finishedDescriptionsHidden)
	}
	if t.finishedOmitted > 0 {
		text += fmt.Sprintf(" %d older merged/closed entries omitted;", t.finishedOmitted)
	}
	if t.watchedDescriptionsHidden > 0 {
		text += fmt.Sprintf(" %d watched description(s) hidden;", t.watchedDescriptionsHidden)
	}
	return text[:len(text)-1] + "."
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
//
// A pane over maxSessionPaneBytes is shed in order: merged/closed
// descriptions first, then the oldest merged/closed entries entirely, and
// only as a last resort the watched entries' descriptions. Whatever was
// shed is named in a note at the top of the pane.
func buildSessionPanePayload(regs []registry.Registration) (panePayload, bool) {
	var visible []registry.Registration
	for _, r := range regs {
		if r.Status != registry.StatusStopped {
			visible = append(visible, r)
		}
	}
	if len(visible) == 0 {
		return panePayload{}, false
	}

	fits := func(p panePayload) bool {
		b, err := json.Marshal(p)
		return err == nil && len(b) <= maxSessionPaneBytes
	}

	p := renderSessionPane(visible, false, false, paneTrim{})
	if fits(p) {
		return p, true
	}

	var finished []registry.Registration
	for _, r := range visible {
		if isFinished(r) {
			finished = append(finished, r)
		}
	}
	hidden := 0
	for _, r := range finished {
		if r.Description != "" {
			hidden++
		}
	}
	p = renderSessionPane(visible, true, false, paneTrim{finishedDescriptionsHidden: hidden})
	if fits(p) {
		return p, true
	}

	// Oldest first; PR URL breaks ties so the choice is deterministic.
	sort.Slice(finished, func(i, j int) bool {
		if !finished[i].CreatedAt.Equal(finished[j].CreatedAt) {
			return finished[i].CreatedAt.Before(finished[j].CreatedAt)
		}
		return finished[i].PRURL < finished[j].PRURL
	})
	omitted := map[string]bool{}
	for _, r := range finished {
		omitted[r.PRURL] = true
		var kept []registry.Registration
		for _, v := range visible {
			if !omitted[v.PRURL] {
				kept = append(kept, v)
			}
		}
		p = renderSessionPane(kept, true, false, paneTrim{finishedDescriptionsHidden: hidden, finishedOmitted: len(omitted)})
		if fits(p) {
			return p, true
		}
	}

	// Only watched entries are left and they still don't fit.
	var kept []registry.Registration
	watchedDescriptions := 0
	for _, v := range visible {
		if !omitted[v.PRURL] {
			kept = append(kept, v)
			if v.Description != "" {
				watchedDescriptions++
			}
		}
	}
	return renderSessionPane(kept, true, true, paneTrim{finishedDescriptionsHidden: hidden, finishedOmitted: len(omitted), watchedDescriptionsHidden: watchedDescriptions}), true
}

func isFinished(r registry.Registration) bool {
	return r.Status == registry.StatusMerged || r.Status == registry.StatusClosed
}

// renderSessionPane lays out regs in order, optionally without the
// descriptions of finished (hideFinishedDesc) or all (hideAllDesc) entries,
// and prefixes a note describing trim when it shed anything.
func renderSessionPane(regs []registry.Registration, hideFinishedDesc, hideAllDesc bool, trim paneTrim) panePayload {
	var children []any
	if trim.any() {
		children = append(children, noteBlock{Kind: "note", Text: trim.note()}, dividerBlock{Kind: "divider"})
	}
	first := true
	for _, r := range regs {
		if !first {
			children = append(children, dividerBlock{Kind: "divider"})
		}
		first = false
		if hideAllDesc || (hideFinishedDesc && isFinished(r)) {
			r.Description = ""
		}
		children = append(children, sessionPaneBlocks(r)...)
	}
	return panePayload{
		Title:           "PR Watch",
		DefaultLocation: "right",
		Blocks:          children,
	}
}

// sessionPaneBlocks renders one registration as three stacked pieces: the
// full title, a reference line (the short owner/repo#N form for GitHub;
// GitLab's paths don't compress as recognizably, so the full URL instead),
// and the full description as a markdown block (the host renders and
// sanitises it). Title and the reference line are both clickable, direct
// links to the PR/MR itself.
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
		// Same event.Kind gating as the home-pane and row-badge: each badge
		// only shows if this registration's own EventFilter includes its
		// matching kind (or the filter is empty, meaning every kind).
		if eventFilterShows(r.EventFilter, "CI") {
			if ci, ok := ciBadge(r.CIBucket); ok {
				badges = append(badges, ci)
			}
		}
		if eventFilterShows(r.EventFilter, "REVIEW") {
			badges = append(badges, reviewBadge(r.LatestReviewState))
		}
	}

	blocks := []any{
		rowBlock{Kind: "row", Label: title, Href: r.PRURL, Badges: badges},
		rowBlock{Kind: "row", Label: ref, Href: r.PRURL},
	}
	if r.Description != "" {
		blocks = append(blocks, markdownBlock{Kind: "markdown", Text: r.Description})
	}
	return blocks
}
