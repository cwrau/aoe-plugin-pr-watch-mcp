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

// The board's columns, left to right. A card sits in the first of Done, Draft,
// Needs attention, Ready that applies, otherwise In review; New holds what a
// notify-mode subscription announced but nobody watches yet.
const (
	boardNew       = "new"
	boardAttention = "attention"
	boardInReview  = "review"
	boardReady     = "ready"
	boardDraft     = "draft"
	boardDone      = "done"
)

var boardColumnTitles = []struct{ ID, Title string }{
	{boardNew, "New, not watched"},
	{boardAttention, "Needs attention"},
	{boardInReview, "In review"},
	{boardReady, "Ready"},
	{boardDraft, "Draft"},
	{boardDone, "Done"},
}

// boardCard is one PR/MR on the board. Facts are the same facet values the
// session filters use (ci:failing, review:approved, ...), so the page renders
// its chips from one vocabulary.
type boardCard struct {
	URL          string   `json:"url"`
	Ref          string   `json:"ref"`
	Title        string   `json:"title,omitempty"`
	Provider     string   `json:"provider"`
	Session      string   `json:"session"`
	SessionTitle string   `json:"session_title,omitempty"`
	Threads      int      `json:"threads,omitempty"`
	Facts        []string `json:"facts,omitempty"`

	finishedAt time.Time // not sent: orders the Done column
}

type boardColumn struct {
	ID    string      `json:"id"`
	Title string      `json:"title"`
	Cards []boardCard `json:"cards"`
}

type boardPayload struct {
	Columns []boardColumn `json:"columns"`
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

// boardColumnFor places a registration on the board from the same facts the
// session filters carry, so a watch whose event filter ignores CI is not put
// under "Needs attention" for a failing check its badge does not show.
func boardColumnFor(r registry.Registration) string {
	facts := registrationFilterValues(r)
	has := func(values ...string) bool {
		return slices.ContainsFunc(values, func(v string) bool { return slices.Contains(facts, v) })
	}
	switch {
	case has("pr:merged", "pr:closed"):
		return boardDone
	case has("pr:draft"):
		return boardDraft
	case has("my_review:approved"):
		return boardReady
	case has("ci:failing", "ci:action-required", "review:changes-requested", "conflicts:yes", "comments:unresolved"):
		return boardAttention
	case has("review:approved") || has("queue:queued", "queue:awaiting-checks", "queue:mergeable", "queue:locked"):
		return boardReady
	}
	return boardInReview
}

func shortTitle(title string) string {
	if r := []rune(strings.TrimSpace(title)); len(r) > boardTitleRunes {
		return string(r[:boardTitleRunes-1]) + "…"
	}
	return strings.TrimSpace(title)
}

// buildBoardPayload lays the watched PRs/MRs, and the ones only announced, out
// in columns. Stopped watches are gone. If the result would exceed the host's
// cap on a page payload, the oldest finished cards are left out first.
func buildBoardPayload(regs []registry.Registration, tracked []trackedCard, titleOf func(sessionID string) string) boardPayload {
	cols := map[string][]boardCard{}
	watched := map[string]bool{}
	for _, r := range regs {
		if r.Status == registry.StatusStopped {
			continue
		}
		watched[r.SessionID+"\x00"+r.PRURL] = true
		col := boardColumnFor(r)
		card := boardCard{
			URL: r.PRURL, Ref: shortRef(r), Title: shortTitle(r.Title), Provider: r.Provider,
			Session: r.SessionID, SessionTitle: titleOf(r.SessionID),
			Threads: len(r.UnresolvedThreadIDs), Facts: registrationFilterValues(r),
		}
		if col == boardDone {
			card.finishedAt = r.MergedAt
		}
		cols[col] = append(cols[col], card)
	}
	for _, t := range tracked {
		if watched[t.SessionID+"\x00"+t.URL] {
			continue
		}
		facts := []string{"pr:open", "provider:" + t.Provider}
		if t.Draft {
			facts = []string{"pr:draft", "provider:" + t.Provider}
		}
		cols[boardNew] = append(cols[boardNew], boardCard{
			URL: t.URL, Ref: provider.ShortRef(t.URL, t.Provider), Provider: t.Provider,
			Session: t.SessionID, SessionTitle: titleOf(t.SessionID), Facts: facts,
		})
	}

	var payload boardPayload
	for _, c := range boardColumnTitles {
		cards := cols[c.ID]
		slices.SortStableFunc(cards, func(a, b boardCard) int {
			if c.ID == boardDone {
				if d := b.finishedAt.Compare(a.finishedAt); d != 0 {
					return d
				}
			}
			return cmp.Compare(a.URL, b.URL)
		})
		payload.Columns = append(payload.Columns, boardColumn{ID: c.ID, Title: c.Title, Cards: cards})
	}
	payload.fit()
	return payload
}

// fit drops the oldest finished cards until the payload is within budget.
func (p *boardPayload) fit() {
	done := &p.Columns[len(p.Columns)-1]
	for len(done.Cards) > 0 {
		raw, err := json.Marshal(p)
		if err != nil || len(raw) <= boardPayloadBudget {
			return
		}
		// Overshoot is bounded by the average card size; cut in proportion so a
		// few hundred finished cards do not cost a marshal each.
		cut := max(1, len(done.Cards)*(len(raw)-boardPayloadBudget)/len(raw))
		cut = min(cut+1, len(done.Cards))
		done.Cards = done.Cards[:len(done.Cards)-cut]
		p.Dropped += cut
	}
}
