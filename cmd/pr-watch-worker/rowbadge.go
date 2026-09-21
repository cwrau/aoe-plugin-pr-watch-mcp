package main

import (
	"fmt"
	"sort"

	"github.com/cwrau/aoe-pr-watch-mcp/internal/registry"
)

const rowBadgeSlot = "row-badge"
const rowBadgeID = "watched_badge"

// rowBadgePayload mirrors the shape ui.state.set expects for the row-badge
// slot: a sequence of chips, each independently tone-colored and clickable
// (confirmed against the reference agent-of-empires.github plugin's own
// _badge_items/_chip construction — the docs don't spell out this slot's
// payload the way they do for "Pane payload").
type rowBadgePayload struct {
	Items []badgeChip `json:"items"`
}

type badgeChip struct {
	Icon    string `json:"icon,omitempty"`
	Text    string `json:"text,omitempty"`
	Tone    string `json:"tone"`
	Tooltip string `json:"tooltip"`
	Href    string `json:"href,omitempty"`
}

// buildRowBadgePayloads groups the registrations that should currently show
// a badge by session, one cluster of chips per registration, so a session
// watching (or having watched) several PRs/MRs gets one badge with several
// chip clusters rather than one badge each. A StatusStopped registration is
// excluded entirely — its owning session explicitly asked to stop watching
// it, so unlike merged/closed there's nothing worth permanently surfacing.
// Chip order is sorted by PR/MR URL for a deterministic push (the host
// doesn't diff payload content, so a stable order avoids the badge visually
// reordering itself on every push for no reason). A session with no
// displayable registrations at all (e.g. every one of its registrations is
// stopped) gets no entry in the returned map, so the caller knows to remove
// any badge it previously had.
func buildRowBadgePayloads(regs []registry.Registration) map[string]rowBadgePayload {
	bySession := make(map[string][]registry.Registration)
	for _, r := range regs {
		if r.Status == registry.StatusStopped {
			continue
		}
		bySession[r.SessionID] = append(bySession[r.SessionID], r)
	}

	out := make(map[string]rowBadgePayload, len(bySession))
	for sid, sessionRegs := range bySession {
		sort.Slice(sessionRegs, func(i, j int) bool {
			return sessionRegs[i].PRURL < sessionRegs[j].PRURL
		})
		var items []badgeChip
		for _, r := range sessionRegs {
			items = append(items, badgeChipsFor(r)...)
		}
		out[sid] = rowBadgePayload{Items: items}
	}
	return out
}

// badgeChipsFor renders one registration's chip cluster. A StatusWatching
// registration gets three chips: the eye identity marker plus separate,
// independently visible CI and review chips (previously that state was only
// reachable via the eye chip's tooltip on hover). StatusMerged and
// StatusClosed collapse to a single distinct, fixed chip instead — their
// CI/review state is no longer meaningful once the PR/MR itself is
// resolved, and (being terminal) that chip stays exactly as-is forever once
// set, which is the whole point of using a badge instead of a one-time
// session rename.
func badgeChipsFor(r registry.Registration) []badgeChip {
	ref := shortRef(r)
	subject := ref
	if r.Title != "" {
		subject = fmt.Sprintf("%s: %s", ref, r.Title)
	}

	switch r.Status {
	case registry.StatusMerged:
		return []badgeChip{{
			Icon:    "check-circle",
			Tone:    "success",
			Tooltip: fmt.Sprintf("%s — merged", subject),
			Href:    r.PRURL,
		}}
	case registry.StatusClosed:
		return []badgeChip{{
			Icon:    "circle-slash",
			Tone:    "neutral",
			Tooltip: fmt.Sprintf("%s — closed", subject),
			Href:    r.PRURL,
		}}
	default:
		ci := ciBadge(r.CIBucket)
		review := reviewBadge(r.LatestReviewState)
		return []badgeChip{
			{
				Icon:    "eye",
				Tone:    "neutral",
				Tooltip: subject,
				Href:    r.PRURL,
			},
			{
				Text:    ci.Text,
				Tone:    ci.Tone,
				Tooltip: fmt.Sprintf("%s — CI %s", subject, ci.Text),
				Href:    r.PRURL,
			},
			{
				Text:    review.Text,
				Tone:    review.Tone,
				Tooltip: fmt.Sprintf("%s — review %s", subject, review.Text),
				Href:    r.PRURL,
			},
		}
	}
}
