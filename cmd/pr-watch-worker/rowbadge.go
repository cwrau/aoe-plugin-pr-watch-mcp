package main

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/cwrau/aoe-pr-watch-mcp/internal/provider"
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
// reachable via the eye chip's tooltip on hover). The eye chip itself is
// recolored (and re-linked, and re-tooltipped) to reflect a merge conflict
// or GitHub merge queue state — a conflict always wins (danger) over
// whatever the merge queue state would otherwise say, since it's the more
// urgent, more actionable problem. No separate conflict chip: a conflict is
// exactly the kind of "this identity marker itself has a problem" signal
// the eye chip already exists to carry, once it's colorable at all.
// StatusMerged and StatusClosed collapse to a single distinct, fixed chip
// instead — their
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
			Tooltip: fmt.Sprintf("%s — merged%s", subject, atSuffix(r.MergedAt)),
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
		chips := []badgeChip{{
			Icon:    "eye",
			Tone:    mainChipTone(r),
			Tooltip: mainChipTooltip(subject, r),
			Href:    mainChipHref(r),
		}}
		chips = append(chips,
			badgeChip{
				Text:    ci.Text,
				Tone:    ci.Tone,
				Tooltip: ciTooltip(ci.Text, r.CIDetail),
				Href:    hrefOr(r.CIHref, r.PRURL),
			},
			badgeChip{
				Text:    review.Text,
				Tone:    review.Tone,
				Tooltip: reviewTooltip(review.Text, r.LatestReviewer, r.LatestReviewAt),
				Href:    hrefOr(r.ReviewHref, r.PRURL),
			},
		)
		return chips
	}
}

// mainChipTone colors the eye chip, most urgent condition first: a merge
// conflict (danger) is a real defect in this PR/MR itself, so it always
// wins. Being blocked on a GitLab dependency (warn, not danger) isn't a
// defect at all — the MR itself may be perfectly fine, it's just waiting on
// another one to merge first, a sequencing state rather than a problem to
// fix here. Otherwise it falls back to the GitHub merge queue entry state
// (always "" for GitLab, which has no merge-queue support here) — a real
// problem there (UNMERGEABLE) is danger, actually about to merge
// (MERGEABLE) is success, and every other in-progress state (QUEUED,
// AWAITING_CHECKS, LOCKED) is info. None of these present keeps the chip's
// ordinary neutral tone.
func mainChipTone(r registry.Registration) string {
	switch {
	case r.HasConflict:
		return "danger"
	case r.Blocked:
		return "warn"
	}
	switch r.MergeQueueState {
	case "":
		return "neutral"
	case "MERGEABLE":
		return "success"
	case "UNMERGEABLE":
		return "danger"
	default:
		return "info"
	}
}

// mergeQueueLabel renders state as lowercase, space-separated text for the
// tooltip — e.g. "AWAITING_CHECKS" -> "awaiting checks".
func mergeQueueLabel(state string) string {
	return strings.ToLower(strings.ReplaceAll(state, "_", " "))
}

// mainChipTooltip appends whichever of conflict/blocked/merge-queue detail
// applies to the eye chip's usual subject tooltip, in the same priority
// order as mainChipTone; none applying leaves the tooltip as plain subject.
func mainChipTooltip(subject string, r registry.Registration) string {
	switch {
	case r.HasConflict:
		return fmt.Sprintf("%s — has a merge conflict", subject)
	case r.Blocked:
		return fmt.Sprintf("%s — blocked by another merge request", subject)
	case r.MergeQueueState != "" && r.MergeQueuePosition > 0:
		return fmt.Sprintf("%s — in merge queue: %s (position %d)", subject, mergeQueueLabel(r.MergeQueueState), r.MergeQueuePosition)
	case r.MergeQueueState != "":
		return fmt.Sprintf("%s — in merge queue: %s", subject, mergeQueueLabel(r.MergeQueueState))
	default:
		return subject
	}
}

// mainChipHref sends the eye chip straight to GitHub's conflict-resolution
// UI while there's a conflict to resolve, and to the plain PR/MR URL
// otherwise.
func mainChipHref(r registry.Registration) string {
	if r.HasConflict {
		return conflictHref(r)
	}
	return r.PRURL
}

// conflictHref links straight to GitHub's own conflict-resolution UI,
// derivable from the PR URL alone with no extra API call. GitLab has no
// equivalent worth hardcoding here: its conflict-resolution page's path
// has changed across versions and isn't reliably guessable the way
// GitHub's is, so a GitLab registration just falls back to the plain MR
// URL like it would without this badge at all.
func conflictHref(r registry.Registration) string {
	if r.Provider == "github" {
		if owner, repo, number, err := provider.ParseGitHubPRURL(r.PRURL); err == nil {
			return fmt.Sprintf("https://github.com/%s/%s/pull/%d/conflicts", owner, repo, number)
		}
	}
	return r.PRURL
}

// hrefOr returns specific if it's known, falling back to the PR/MR URL
// itself — the CI/review chips link to the exact job/pipeline or
// review/comment when the provider reported one, and to the PR/MR as a
// whole otherwise (e.g. an unreviewed PR, or a CI provider that doesn't
// expose a per-run URL).
func hrefOr(specific, fallback string) string {
	if specific != "" {
		return specific
	}
	return fallback
}

// ciTooltip appends the failing check/job name to the CI tooltip, when
// known — e.g. "CI failure: build".
func ciTooltip(bucketText, detail string) string {
	if detail == "" {
		return fmt.Sprintf("CI %s", bucketText)
	}
	return fmt.Sprintf("CI %s: %s", bucketText, detail)
}

// reviewTooltip appends who reviewed and when, when known — e.g. "review
// APPROVED by alice, 2026-09-20 12:00 UTC". Either can be known without the
// other (a reviewer without a timestamp, or vice versa), so each is
// appended independently rather than requiring both.
func reviewTooltip(stateText, reviewer string, at time.Time) string {
	s := fmt.Sprintf("review %s", stateText)
	if reviewer != "" {
		s += " by " + reviewer
	}
	if suffix := atSuffix(at); suffix != "" {
		if reviewer != "" {
			s += ","
		}
		s += suffix
	}
	return s
}

// atSuffix renders a leading " at <timestamp>" clause, or "" if t is zero.
func atSuffix(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return " at " + t.UTC().Format("2006-01-02 15:04 MST")
}
