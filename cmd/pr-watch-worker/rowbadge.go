package main

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cwrau/aoe-plugin-pr-watch-mcp/internal/provider"
	"github.com/cwrau/aoe-plugin-pr-watch-mcp/internal/registry"
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
// Chip order is by status (watching, merged, closed), then entries in a merge
// queue (per repo, by queue position), then PR/MR number, then URL as a final
// tiebreak, so the push is deterministic (the host
// doesn't diff payload content, so a stable order avoids the badge visually
// reordering itself on every push for no reason). A session with no
// displayable registrations at all (e.g. every one of its registrations is
// stopped) gets no entry in the returned map, so the caller knows to remove
// any badge it previously had.
func buildRowBadgePayloads(regs []registry.Registration) map[string]rowBadgePayload {
	return buildRowBadgePayloadsWithSubs(regs, nil)
}

// subscriptionBadgeInfo is what a session's row badge says about one of its
// active subscriptions.
type subscriptionBadgeInfo struct {
	SessionID  string
	ID         string
	Scope      string // "github.com/owner/repo", "gitlab.example.com/group", "github.com"
	Mode       string // registry.SubscriptionModeWatch or ...Notify
	Filters    provider.ScanFilters
	Tracked    int // PRs/MRs a notify subscription has told the session about
	Watches    int // watches a watch subscription has registered
	LastScanAt time.Time
	LastError  string
}

const (
	maxSubscriptionTooltipEntries = 5
	maxSubscriptionTooltipError   = 80
)

// filterSummary is the non-empty filters as sorted key=value pairs.
func filterSummary(f provider.ScanFilters) string {
	raw, err := json.Marshal(f)
	if err != nil {
		return ""
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		switch v := m[k].(type) {
		case []any:
			vals := make([]string, 0, len(v))
			for _, x := range v {
				vals = append(vals, fmt.Sprint(x))
			}
			parts = append(parts, k+"="+strings.Join(vals, ","))
		default:
			parts = append(parts, fmt.Sprintf("%s=%v", k, v))
		}
	}
	return strings.Join(parts, ", ")
}

// subscriptionChip is the one chip standing for a session's subscriptions: a
// radar with the number of PRs/MRs its notify subscriptions are tracking (a
// watch subscription's matches already have chips of their own), a warning
// tone when a scan failed, and every subscription in the tooltip.
func subscriptionChip(subs []subscriptionBadgeInfo) badgeChip {
	tracked, failing := 0, false
	var lines []string
	for i, s := range subs {
		tracked += s.Tracked
		failing = failing || s.LastError != ""
		if i >= maxSubscriptionTooltipEntries {
			continue
		}
		line := fmt.Sprintf("%s (%s", s.Scope, s.Mode)
		if f := filterSummary(s.Filters); f != "" {
			line += ", " + f
		}
		line += "): "
		if s.Mode == registry.SubscriptionModeNotify {
			line += fmt.Sprintf("%d tracked", s.Tracked)
		} else {
			line += fmt.Sprintf("%d watching", s.Watches)
		}
		if !s.LastScanAt.IsZero() {
			line += ", last scan " + s.LastScanAt.Local().Format("15:04")
		}
		if s.LastError != "" {
			e := []rune(s.LastError)
			if len(e) > maxSubscriptionTooltipError {
				e = append(e[:maxSubscriptionTooltipError-1], '…')
			}
			line += ", last error: " + string(e)
		}
		lines = append(lines, line)
	}
	if more := len(subs) - maxSubscriptionTooltipEntries; more > 0 {
		lines = append(lines, fmt.Sprintf("+%d more", more))
	}
	chip := badgeChip{Icon: "radar", Tone: "info", Tooltip: "watching scopes: " + strings.Join(lines, "; ")}
	if tracked > 0 {
		chip.Text = strconv.Itoa(tracked)
	}
	if failing {
		chip.Tone = "warn"
	}
	return chip
}

// buildRowBadgePayloadsWithSubs is buildRowBadgePayloads plus a leading chip
// for each session's subscriptions. A session with subscriptions but no watch
// of its own still gets a badge.
func buildRowBadgePayloadsWithSubs(regs []registry.Registration, subs []subscriptionBadgeInfo) map[string]rowBadgePayload {
	subsBySession := make(map[string][]subscriptionBadgeInfo)
	for _, s := range subs {
		subsBySession[s.SessionID] = append(subsBySession[s.SessionID], s)
	}
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
			a, b := sessionRegs[i], sessionRegs[j]
			if ra, rb := statusRank(a.Status), statusRank(b.Status); ra != rb {
				return ra < rb
			}
			// Queued entries first, grouped by repo (each repo has its own
			// queue) and ordered by position within it; everything else
			// follows by number. Kept as one total order so the comparator
			// stays transitive across repos.
			if qa, qb := inMergeQueue(a), inMergeQueue(b); qa != qb {
				return qa
			} else if qa {
				if ra, rb := repoKey(a), repoKey(b); ra != rb {
					return ra < rb
				}
				if a.MergeQueuePosition != b.MergeQueuePosition {
					return a.MergeQueuePosition < b.MergeQueuePosition
				}
			}
			if na, nb := prNumber(a), prNumber(b); na != nb {
				return na < nb
			}
			return a.PRURL < b.PRURL
		})
		// Finished PRs/MRs collapse into one counted chip per kind, first on
		// the row: a session working through a chain of PRs would otherwise
		// grow one permanent chip per PR in front of the one it is on now.
		var watched, merged, closed []registry.Registration
		for _, r := range sessionRegs {
			switch r.Status {
			case registry.StatusMerged:
				merged = append(merged, r)
			case registry.StatusClosed:
				closed = append(closed, r)
			default:
				watched = append(watched, r)
			}
		}
		items := append(finishedChip(merged, registry.StatusMerged), finishedChip(closed, registry.StatusClosed)...)
		watchedItems := chipsForRegistrations(watched, false)
		if len(items)+len(watchedItems) > compactChipThreshold {
			watchedItems = chipsForRegistrations(watched, true)
		}
		items = append(items, watchedItems...)
		if s, ok := subsBySession[sid]; ok {
			items = append([]badgeChip{subscriptionChip(s)}, items...)
		}
		out[sid] = rowBadgePayload{Items: fitRowBadge(items)}
	}
	for sid, s := range subsBySession {
		if _, ok := out[sid]; !ok {
			out[sid] = rowBadgePayload{Items: []badgeChip{subscriptionChip(s)}}
		}
	}
	return out
}

// maxRowBadgeBytes is the largest row-badge payload this worker sends, a
// margin under the host's 8KB cap (past which the push is rejected and the
// badge freezes at its last accepted state).
const maxRowBadgeBytes = 7 * 1024

// maxShortTooltipRunes is what a chip's tooltip is cut to when the full ones
// do not fit.
const maxShortTooltipRunes = 80

// fitRowBadge keeps the chips under maxRowBadgeBytes even compact mode could
// not get them under: first by shortening tooltips, then by dropping chips
// from the end (the order puts the actionable ones first) and ending the row
// with a "+N" chip that says how many are missing.
func fitRowBadge(items []badgeChip) []badgeChip {
	if rowBadgeSize(items) <= maxRowBadgeBytes {
		return items
	}
	short := make([]badgeChip, len(items))
	for i, c := range items {
		if r := []rune(c.Tooltip); len(r) > maxShortTooltipRunes {
			c.Tooltip = string(r[:maxShortTooltipRunes-1]) + "…"
		}
		short[i] = c
	}
	if rowBadgeSize(short) <= maxRowBadgeBytes {
		return short
	}
	overflow := func(n int) badgeChip {
		return badgeChip{Text: "+" + strconv.Itoa(n), Tone: "neutral", Tooltip: fmt.Sprintf("%d more PRs/MRs not shown (row too large)", n)}
	}
	keep := len(short)
	for keep > 0 && rowBadgeSize(append(slices.Clone(short[:keep]), overflow(len(short)-keep))) > maxRowBadgeBytes {
		keep--
	}
	return append(slices.Clone(short[:keep]), overflow(len(short)-keep))
}

func rowBadgeSize(items []badgeChip) int {
	raw, err := json.Marshal(rowBadgePayload{Items: items})
	if err != nil {
		return 0
	}
	return len(raw)
}

// statusRank orders chip clusters: still-watching first (the actionable
// ones), then merged, then closed.
func statusRank(status string) int {
	switch status {
	case registry.StatusWatching:
		return 0
	case registry.StatusMerged:
		return 1
	default:
		return 2
	}
}

// prNumber is the registration's PR number / MR iid, or 0 if its URL can't
// be parsed — compared numerically, since URL string order puts #10 before
// #9. Registrations in different repos can share a number; the caller falls
// back to the URL to keep the order deterministic.
func prNumber(r registry.Registration) int {
	if r.Provider == "github" {
		if _, _, n, err := provider.ParseGitHubPRURL(r.PRURL); err == nil {
			return n
		}
		return 0
	}
	if _, _, n, err := provider.ParseGitLabMRURL(r.PRURL); err == nil {
		return n
	}
	return 0
}

// inMergeQueue reports whether the registration currently has a merge queue
// entry, whose position is then worth ordering by.
func inMergeQueue(r registry.Registration) bool {
	return r.MergeQueueState != ""
}

// repoKey identifies the repo (and so the merge queue) a registration
// belongs to; empty if its URL can't be parsed.
func repoKey(r registry.Registration) string {
	if r.Provider == "github" {
		if owner, repo, _, err := provider.ParseGitHubPRURL(r.PRURL); err == nil {
			return owner + "/" + repo
		}
		return ""
	}
	if host, path, _, err := provider.ParseGitLabMRURL(r.PRURL); err == nil {
		return host + "/" + path
	}
	return ""
}

// maxFinishedTooltipRefs bounds the PR list in a counted chip's tooltip, so a
// long chain of PRs cannot push the row-badge payload past the host's cap.
const maxFinishedTooltipRefs = 8

// finishedChip is the single chip standing for every merged (or closed)
// registration of a session. One registration keeps its plain chip; several
// get the same icon with their count, linking to the newest and listing the
// newest few in the tooltip.
func finishedChip(regs []registry.Registration, status string) []badgeChip {
	switch len(regs) {
	case 0:
		return nil
	case 1:
		return badgeChipsFor(regs[0], false)
	}
	newest := slices.Clone(regs)
	sort.Slice(newest, func(i, j int) bool {
		a, b := newest[i], newest[j]
		if !a.MergedAt.Equal(b.MergedAt) {
			return a.MergedAt.After(b.MergedAt)
		}
		if na, nb := prNumber(a), prNumber(b); na != nb {
			return na > nb
		}
		return a.PRURL > b.PRURL
	})

	shown := newest
	if len(shown) > maxFinishedTooltipRefs {
		shown = shown[:maxFinishedTooltipRefs]
	}
	refs := make([]string, 0, len(shown))
	for _, r := range shown {
		refs = append(refs, shortRef(r))
	}
	list := strings.Join(refs, ", ")
	if more := len(newest) - len(shown); more > 0 {
		list += fmt.Sprintf(", +%d more", more)
	}

	chip := badgeChip{Text: strconv.Itoa(len(regs)), Tooltip: fmt.Sprintf("%d %s: %s", len(regs), status, list), Href: newest[0].PRURL}
	if status == registry.StatusMerged {
		chip.Icon, chip.Tone = "check-circle", "success"
	} else {
		chip.Icon, chip.Tone = "circle-slash", "neutral"
	}
	return []badgeChip{chip}
}

// compactChipThreshold is the chip count past which a session's badge
// switches to compact mode. A session watching many PRs would otherwise fill
// the row and run into the host's 8KB row-badge payload cap, past which the
// push is rejected and the badge freezes.
const compactChipThreshold = 16

func chipsForRegistrations(regs []registry.Registration, compact bool) []badgeChip {
	var items []badgeChip
	for _, r := range regs {
		items = append(items, badgeChipsFor(r, compact)...)
	}
	return items
}

// compactCIChip reports whether a CI state is worth a chip in compact mode:
// only the ones needing a look. Success, none and pending are quiet.
func compactCIChip(bucket string) bool {
	return bucket == "failure" || bucket == "action_required"
}

// compactReviewChip reports whether a review state is worth a chip in
// compact mode. No review and approvals (complete or partial, e.g. "APPROVED
// 1/2") are quiet; a verdict like changes requested or a comment is not.
func compactReviewChip(state string) bool {
	return state != "" && !strings.HasPrefix(state, "APPROVED")
}

// badgeChipsFor renders one registration's chip cluster. In compact mode a
// watched registration keeps its eye chip and gets CI/review chips only in
// attention states (CI failure or action required; a review verdict other
// than an approval, such as changes requested). A StatusWatching
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
func badgeChipsFor(r registry.Registration, compact bool) []badgeChip {
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
		// The eye chip always shows — it's this registration's identity
		// marker, not an event kind the session could have filtered out.
		// The CI/review chips each mirror a specific event.Kind (see
		// eventFilterShows and registry.Registration.EventFilter's doc
		// comment), so a session that registered with e.g. events:
		// ["MERGED"] only gets the eye chip here — same principle as the
		// home-pane's badges.
		// The eye chip's conflict and queue colouring is those events' badge,
		// so it follows the events filter too: a watch (a reviewer's, say) not
		// told about conflicts or the queue does not wear them.
		filter := r.EffectiveEventFilter()
		if !eventFilterShows(filter, "CONFLICT") {
			r.HasConflict, r.Blocked = false, false
		}
		if !eventFilterShows(filter, "MERGE_QUEUE") {
			r.MergeQueueState, r.MergeQueuePosition = "", 0
		}
		eye := badgeChip{
			Icon:    "eye",
			Tone:    mainChipTone(r),
			Tooltip: mainChipTooltip(subject, r),
			Href:    mainChipHref(r),
		}
		// A partial approval gets no chip of its own in compact mode, so the
		// eye chip carries it: the tone the "APPROVED 1/2" chip had (unless
		// something more urgent already colours it) and the progress in the
		// tooltip, enough to tell "something is up" without taking a slot.
		if compact && provider.PartialApproval(r.LatestReviewState) {
			if eye.Tone == "neutral" {
				eye.Tone = "info"
			}
			eye.Tooltip += "; review " + r.LatestReviewState
		}
		chips := []badgeChip{eye}
		if eventFilterShows(filter, "CI") && (!compact || compactCIChip(r.CIBucket)) {
			if ci, ok := ciBadge(r.CIBucket); ok {
				chips = append(chips, badgeChip{
					Text:    ci.Text,
					Tone:    ci.Tone,
					Tooltip: ciTooltip(ci.Text, r.CIDetail),
					Href:    hrefOr(r.CIHref, r.PRURL),
				})
			}
		}
		if r.Persona == registry.PersonaReviewer {
			// A reviewer's review chip is its own progress: in progress until
			// its own account has approved. Quiet in compact mode once done.
			if chip, inProgress := reviewerReviewChip(r); inProgress || !compact {
				chips = append(chips, chip)
			}
		} else if eventFilterShows(filter, "REVIEW") && (!compact || compactReviewChip(r.LatestReviewState)) {
			review := reviewBadge(r.LatestReviewState)
			chips = append(chips, badgeChip{
				Text:    review.Text,
				Tone:    review.Tone,
				Tooltip: reviewTooltip(review.Text, r.LatestReviewer, r.LatestReviewAt),
				Href:    hrefOr(r.ReviewHref, r.PRURL),
			})
		}
		return chips
	}
}

// reviewerReviewChip is the review chip of a reviewer-persona watch: "in
// progress" (info) until the worker's own account has approved, then
// "approved" (success). Only its own verdict counts, never another reviewer's
// approval; the second result is whether the review is still in progress.
func reviewerReviewChip(r registry.Registration) (badgeChip, bool) {
	href := hrefOr(r.ReviewHref, r.PRURL)
	if r.SelfReviewState == "APPROVED" {
		return badgeChip{Text: "approved", Tone: "success", Tooltip: "you approved this", Href: href}, false
	}
	tooltip := "your review is in progress"
	if r.SelfReviewState != "" {
		tooltip += "; you last: " + r.SelfReviewState
	}
	return badgeChip{Text: "in progress", Tone: "info", Tooltip: tooltip, Href: href}, true
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
