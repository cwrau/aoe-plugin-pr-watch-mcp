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
	Icon    string `json:"icon"`
	Tone    string `json:"tone"`
	Tooltip string `json:"tooltip"`
	Href    string `json:"href,omitempty"`
}

// buildRowBadgePayloads groups the currently-watched registrations by
// session, one chip per registration, so a session watching several
// PRs/MRs gets one badge with several chips rather than one badge each.
// Chip order is sorted by PR/MR URL for a deterministic push (the host
// doesn't diff payload content, so a stable order avoids the badge
// visually reordering itself on every push for no reason).
func buildRowBadgePayloads(regs []registry.Registration) map[string]rowBadgePayload {
	bySession := make(map[string][]registry.Registration)
	for _, r := range regs {
		bySession[r.SessionID] = append(bySession[r.SessionID], r)
	}

	out := make(map[string]rowBadgePayload, len(bySession))
	for sid, sessionRegs := range bySession {
		sort.Slice(sessionRegs, func(i, j int) bool {
			return sessionRegs[i].PRURL < sessionRegs[j].PRURL
		})
		items := make([]badgeChip, 0, len(sessionRegs))
		for _, r := range sessionRegs {
			ci := ciBadge(r.CIBucket)
			review := reviewBadge(r.LatestReviewState)
			ref := shortRef(r)
			subject := ref
			if r.Title != "" {
				subject = fmt.Sprintf("%s: %s", ref, r.Title)
			}
			items = append(items, badgeChip{
				Icon:    "eye",
				Tone:    ci.Tone,
				Tooltip: fmt.Sprintf("%s — CI %s, review %s", subject, ci.Text, review.Text),
				Href:    r.PRURL,
			})
		}
		out[sid] = rowBadgePayload{Items: items}
	}
	return out
}
