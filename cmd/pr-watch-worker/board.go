package main

import (
	"cmp"
	"encoding/json"
	"slices"
	"strings"
	"time"

	"github.com/cwrau/aoe-plugin-pr-watch-mcp/internal/provider"
	"github.com/cwrau/aoe-plugin-pr-watch-mcp/internal/registry"
)

const (
	boardPageSlot = "page"
	boardPageID   = "board"

	// boardPayloadBudget keeps the page state under the host's 64 KiB cap on a
	// page payload, with headroom for the envelope.
	boardPayloadBudget = 60 * 1024

	boardTitleRunes = 120
)

// The board has one view per persona, since what needs a look differs: an
// author cares about CI, conflicts and requested changes on their own PR, a
// reviewer about their own verdict and whether the author pushed since.
const (
	boardAuthorView   = "author"
	boardReviewerView = "reviewer"
)

// The columns. A card sits in the first column of its view that applies, in
// the order each view's placement function checks them.
const (
	boardNew       = "new"       // reviewer: announced by a notify-mode subscription, nobody watches it yet
	boardAttention = "attention" // author: changes requested, CI failing, a conflict, unresolved threads
	boardInReview  = "review"    // author: open and waiting
	boardReady     = "ready"     // author: approved with nothing needing attention, or in the merge queue
	boardToReview  = "to-review" // reviewer: no verdict of their own yet
	boardReReview  = "re-review" // reviewer: the author pushed since their verdict
	boardWaiting   = "waiting"   // reviewer: asked for changes or commented, nothing new since
	boardApproved  = "approved"  // reviewer: approved, nothing new since
	boardDraft     = "draft"     // both
	boardDone      = "done"      // both: merged or closed
)

type boardColumnDef struct{ ID, Title string }

var boardViewDefs = []struct {
	ID, Title string
	Columns   []boardColumnDef
}{
	{boardAuthorView, "Author", []boardColumnDef{
		{boardAttention, "Needs attention"},
		{boardInReview, "In review"},
		{boardReady, "Ready"},
		{boardDraft, "Draft"},
		{boardDone, "Done"},
	}},
	{boardReviewerView, "Reviewer", []boardColumnDef{
		{boardNew, "New, not watched"},
		{boardToReview, "To review"},
		{boardReReview, "Re-review"},
		{boardWaiting, "Waiting on author"},
		{boardApproved, "Approved by you"},
		{boardDraft, "Draft"},
		{boardDone, "Done"},
	}},
}

// boardCard is one PR/MR on the board. Facts are the same facet values the
// session filters use (ci:failing, review:approved, ...), so the page renders
// its chips from one vocabulary. Verdict is a reviewer's own last review.
type boardCard struct {
	URL          string   `json:"url"`
	Ref          string   `json:"ref"`
	Title        string   `json:"title,omitempty"`
	Provider     string   `json:"provider"`
	Session      string   `json:"session"`
	SessionTitle string   `json:"session_title,omitempty"`
	Threads      int      `json:"threads,omitempty"`
	Verdict      string   `json:"verdict,omitempty"`
	Facts        []string `json:"facts,omitempty"`

	finishedAt time.Time // not sent: orders the Done column
}

type boardColumn struct {
	ID    string      `json:"id"`
	Title string      `json:"title"`
	Cards []boardCard `json:"cards"`
}

type boardView struct {
	ID      string        `json:"id"`
	Title   string        `json:"title"`
	Columns []boardColumn `json:"columns"`
}

type boardPayload struct {
	Views []boardView `json:"views"`
	// Dropped counts finished cards left out to fit the host's payload cap.
	Dropped int `json:"dropped,omitempty"`
}

// trackedCard is a PR/MR a notify-mode subscription told its session about,
// the only thing known of it besides its URL being whether it was a draft.
type trackedCard struct {
	SessionID string
	Provider  string
	URL       string
	Draft     bool
}

func isReviewer(r registry.Registration) bool { return r.Persona == registry.PersonaReviewer }

// boardColumnFor places a registration in a column of its persona's view.
func boardColumnFor(r registry.Registration) string {
	if isReviewer(r) {
		return reviewerColumnFor(r)
	}
	return authorColumnFor(r)
}

// authorColumnFor places a PR/MR the session wrote from the same facts the
// session filters carry, so a watch whose event filter ignores CI is not put
// under "Needs attention" for a failing check its badge does not show.
func authorColumnFor(r registry.Registration) string {
	facts := registrationFilterValues(r)
	has := func(values ...string) bool {
		return slices.ContainsFunc(values, func(v string) bool { return slices.Contains(facts, v) })
	}
	switch {
	case has("pr:merged", "pr:closed"):
		return boardDone
	case has("pr:draft"):
		return boardDraft
	case has("ci:failing", "ci:action-required", "review:changes-requested", "conflicts:yes", "comments:unresolved"):
		return boardAttention
	case has("review:approved") || has("queue:queued", "queue:awaiting-checks", "queue:mergeable", "queue:locked"):
		return boardReady
	}
	return boardInReview
}

// reviewerColumnFor places a PR/MR the session reviews by the account's own
// verdict on it, and whether the head moved since. CI, conflicts and other
// reviewers' verdicts are the author's business, not the reviewer's.
func reviewerColumnFor(r registry.Registration) string {
	switch {
	case r.Status == registry.StatusMerged || r.Status == registry.StatusClosed:
		return boardDone
	case r.IsDraft:
		return boardDraft
	case r.SelfReviewState != "" && r.SelfReviewSHA != "" && r.HeadSHA != "" && r.HeadSHA != r.SelfReviewSHA:
		return boardReReview
	case r.SelfReviewState == "APPROVED":
		return boardApproved
	case r.SelfReviewState != "":
		return boardWaiting
	}
	return boardToReview
}

func shortTitle(title string) string {
	if r := []rune(strings.TrimSpace(title)); len(r) > boardTitleRunes {
		return string(r[:boardTitleRunes-1]) + "…"
	}
	return strings.TrimSpace(title)
}

// verdictOf is a reviewer's own last review as the card shows it.
func verdictOf(r registry.Registration) string {
	return strings.ToLower(strings.ReplaceAll(r.SelfReviewState, "_", "-"))
}

// buildBoardPayload lays the watched PRs/MRs, and the ones only announced, out
// in each persona's columns. Stopped watches are gone. If the result would
// exceed the host's cap on a page payload, the oldest finished cards are left
// out first.
func buildBoardPayload(regs []registry.Registration, tracked []trackedCard, titleOf func(sessionID string) string) boardPayload {
	cols := map[string]map[string][]boardCard{boardAuthorView: {}, boardReviewerView: {}}
	watched := map[string]bool{}
	for _, r := range regs {
		if r.Status == registry.StatusStopped {
			continue
		}
		watched[r.SessionID+"\x00"+r.PRURL] = true
		card := boardCard{
			URL: r.PRURL, Ref: shortRef(r), Title: shortTitle(r.Title), Provider: r.Provider,
			Session: r.SessionID, SessionTitle: titleOf(r.SessionID),
			Threads: len(r.UnresolvedThreadIDs), Facts: registrationFilterValues(r),
		}
		view, col := boardAuthorView, boardColumnFor(r)
		if isReviewer(r) {
			view, card.Verdict = boardReviewerView, verdictOf(r)
		}
		if col == boardDone {
			card.finishedAt = r.MergedAt
		}
		cols[view][col] = append(cols[view][col], card)
	}
	for _, t := range tracked {
		if watched[t.SessionID+"\x00"+t.URL] {
			continue
		}
		facts := []string{"pr:open", "provider:" + t.Provider}
		if t.Draft {
			facts = []string{"pr:draft", "provider:" + t.Provider}
		}
		cols[boardReviewerView][boardNew] = append(cols[boardReviewerView][boardNew], boardCard{
			URL: t.URL, Ref: provider.ShortRef(t.URL, t.Provider), Provider: t.Provider,
			Session: t.SessionID, SessionTitle: titleOf(t.SessionID), Facts: facts,
		})
	}

	var payload boardPayload
	for _, v := range boardViewDefs {
		view := boardView{ID: v.ID, Title: v.Title}
		for _, c := range v.Columns {
			cards := cols[v.ID][c.ID]
			if cards == nil {
				cards = []boardCard{} // encodes as [], not null, which the page reads the length of
			}
			slices.SortStableFunc(cards, func(a, b boardCard) int {
				if c.ID == boardDone {
					if d := b.finishedAt.Compare(a.finishedAt); d != 0 {
						return d
					}
				}
				return cmp.Compare(a.URL, b.URL)
			})
			view.Columns = append(view.Columns, boardColumn{ID: c.ID, Title: c.Title, Cards: cards})
		}
		payload.Views = append(payload.Views, view)
	}
	payload.fit()
	return payload
}

// fit drops the oldest finished cards, from whichever view holds them, until
// the payload is within budget.
func (p *boardPayload) fit() {
	var dones []*boardColumn
	for i := range p.Views {
		cols := p.Views[i].Columns
		dones = append(dones, &cols[len(cols)-1])
	}
	for {
		raw, err := json.Marshal(p)
		if err != nil || len(raw) <= boardPayloadBudget {
			return
		}
		// Cut in proportion to the overshoot so a few hundred finished cards
		// do not cost a marshal each.
		total := 0
		for _, d := range dones {
			total += len(d.Cards)
		}
		if total == 0 {
			return
		}
		for range min(total, max(1, total*(len(raw)-boardPayloadBudget)/len(raw))+1) {
			var oldest *boardColumn
			for _, d := range dones {
				if len(d.Cards) == 0 {
					continue
				}
				if oldest == nil || d.Cards[len(d.Cards)-1].finishedAt.Before(oldest.Cards[len(oldest.Cards)-1].finishedAt) {
					oldest = d
				}
			}
			if oldest == nil {
				break
			}
			oldest.Cards = oldest.Cards[:len(oldest.Cards)-1]
			p.Dropped++
		}
	}
}
