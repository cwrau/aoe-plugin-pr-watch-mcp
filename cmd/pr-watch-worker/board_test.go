package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cwrau/aoe-plugin-pr-watch-mcp/internal/aoeclient"
	"github.com/cwrau/aoe-plugin-pr-watch-mcp/internal/registry"
)

func TestBoardColumnPlacement(t *testing.T) {
	tests := []struct {
		name string
		reg  registry.Registration
		want string
	}{
		{"open, nothing yet", watching(nil), boardInReview},
		{"commented", watching(func(r *registry.Registration) { r.LatestReviewState = "COMMENTED" }), boardInReview},
		{"partial approval", watching(func(r *registry.Registration) { r.LatestReviewState = "APPROVED 1/2" }), boardInReview},
		{"approved", watching(func(r *registry.Registration) { r.LatestReviewState = "APPROVED" }), boardReady},
		{"approved, CI passing", watching(func(r *registry.Registration) { r.LatestReviewState, r.CIBucket = "APPROVED", "success" }), boardReady},
		{"approved but CI failing", watching(func(r *registry.Registration) { r.LatestReviewState, r.CIBucket = "APPROVED", "failure" }), boardAttention},
		{"approved and in the merge queue", watching(func(r *registry.Registration) { r.LatestReviewState, r.MergeQueueState = "APPROVED", "QUEUED" }), boardReady},
		{"changes requested", watching(func(r *registry.Registration) { r.LatestReviewState = "CHANGES_REQUESTED" }), boardAttention},
		{"CI failing", watching(func(r *registry.Registration) { r.CIBucket = "failure" }), boardAttention},
		{"CI needs action", watching(func(r *registry.Registration) { r.CIBucket = "action_required" }), boardAttention},
		{"CI running is not attention", watching(func(r *registry.Registration) { r.CIBucket = "pending" }), boardInReview},
		{"conflict", watching(func(r *registry.Registration) { r.HasConflict = true }), boardAttention},
		{"unresolved threads", watching(func(r *registry.Registration) { r.UnresolvedThreadIDs = []string{"t"} }), boardAttention},
		{"a draft stays a draft even with failing CI", watching(func(r *registry.Registration) { r.IsDraft, r.CIBucket = true, "failure" }), boardDraft},
		{"merged", watching(func(r *registry.Registration) { r.Status = registry.StatusMerged }), boardDone},
		{"closed", watching(func(r *registry.Registration) { r.Status = registry.StatusClosed }), boardDone},
		{"a failing check its event filter ignores is not attention", watching(func(r *registry.Registration) {
			r.CIBucket, r.EventFilter = "failure", []string{"MERGED", "CLOSED"}
		}), boardInReview},
		{"reviewer, own review in progress", watching(func(r *registry.Registration) { r.Persona = registry.PersonaReviewer }), boardInReview},
		{"reviewer, own approval", watching(func(r *registry.Registration) {
			r.Persona, r.SelfReviewState = registry.PersonaReviewer, "APPROVED"
		}), boardReady},
		{"reviewer approved, others asked for changes", watching(func(r *registry.Registration) {
			r.Persona, r.SelfReviewState, r.LatestReviewState = registry.PersonaReviewer, "APPROVED", "CHANGES_REQUESTED"
		}), boardReady},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := boardColumnFor(tt.reg); got != tt.want {
				t.Errorf("boardColumnFor() = %q, want %q", got, tt.want)
			}
		})
	}
}

func boardTitleOf(id string) string { return "title of " + id }

func cardsOf(p boardPayload, column string) []boardCard {
	for _, c := range p.Columns {
		if c.ID == column {
			return c.Cards
		}
	}
	return nil
}

func TestBoardPayloadColumnsAndCards(t *testing.T) {
	regs := []registry.Registration{
		watching(func(r *registry.Registration) {
			r.PRURL, r.Title, r.CIBucket, r.UnresolvedThreadIDs, r.Snoozed = "https://github.com/o/r/pull/1", "fix the thing", "failure", []string{"a", "b"}, true
		}),
		watching(func(r *registry.Registration) {
			r.PRURL, r.SessionID, r.Provider = "https://gitlab.example.com/g/p/-/merge_requests/2", "s2", "gitlab"
		}),
		watching(func(r *registry.Registration) {
			r.PRURL, r.Status = "https://github.com/o/r/pull/3", registry.StatusStopped
		}),
	}
	p := buildBoardPayload(regs, nil, boardTitleOf)

	wantOrder := []string{boardNew, boardAttention, boardInReview, boardReady, boardDraft, boardDone}
	var got []string
	for _, c := range p.Columns {
		got = append(got, c.ID)
	}
	if !slices.Equal(got, wantOrder) {
		t.Fatalf("column order = %v, want %v", got, wantOrder)
	}
	titles := map[string]string{
		boardNew: "New, not watched", boardAttention: "Needs attention", boardInReview: "In review",
		boardReady: "Ready", boardDraft: "Draft", boardDone: "Done",
	}
	for _, c := range p.Columns {
		if c.Title != titles[c.ID] {
			t.Errorf("column %q title = %q, want %q", c.ID, c.Title, titles[c.ID])
		}
	}

	attention := cardsOf(p, boardAttention)
	if len(attention) != 1 {
		t.Fatalf("attention cards = %+v, want 1", attention)
	}
	c := attention[0]
	if c.URL != "https://github.com/o/r/pull/1" || c.Ref != "o/r#1" || c.Title != "fix the thing" ||
		c.Session != "s" || c.SessionTitle != "title of s" || c.Provider != "github" || c.Threads != 2 {
		t.Errorf("card = %+v", c)
	}
	for _, want := range []string{"ci:failing", "comments:unresolved", "snoozed:yes"} {
		if !slices.Contains(c.Facts, want) {
			t.Errorf("card facts = %v, want %q among them", c.Facts, want)
		}
	}
	review := cardsOf(p, boardInReview)
	if len(review) != 1 || review[0].Ref != "g/p!2" || review[0].Provider != "gitlab" {
		t.Errorf("in review cards = %+v, want the GitLab MR g/p!2 (stopped watches never show)", review)
	}
}

func TestBoardPayloadUnwatchedColumnFromNotifySubscriptions(t *testing.T) {
	regs := []registry.Registration{
		watching(func(r *registry.Registration) { r.PRURL = "https://github.com/o/r/pull/1" }),
	}
	tracked := []trackedCard{
		{SessionID: "orch", Provider: "github", URL: "https://github.com/o/r/pull/7", Draft: false},
		{SessionID: "orch", Provider: "github", URL: "https://github.com/o/r/pull/8", Draft: true},
		{SessionID: "s", Provider: "github", URL: "https://github.com/o/r/pull/1"}, // already watched by that session
	}
	p := buildBoardPayload(regs, tracked, boardTitleOf)

	fresh := cardsOf(p, boardNew)
	if len(fresh) != 2 {
		t.Fatalf("new cards = %+v, want 2 (the one s already watches is not new)", fresh)
	}
	if fresh[0].Ref != "o/r#7" || fresh[0].Session != "orch" || fresh[0].SessionTitle != "title of orch" {
		t.Errorf("first new card = %+v", fresh[0])
	}
	if slices.Contains(fresh[0].Facts, "pr:draft") || !slices.Contains(fresh[1].Facts, "pr:draft") {
		t.Errorf("draft facts: %v and %v, want pr:draft on the second only", fresh[0].Facts, fresh[1].Facts)
	}
	if len(cardsOf(p, boardInReview)) != 1 {
		t.Errorf("the watched PR must still show once, in its own column")
	}
}

func TestBoardPayloadDoneColumnShowsTheNewestFirst(t *testing.T) {
	now := time.Now()
	mk := func(n int, status string, at time.Time) registry.Registration {
		return watching(func(r *registry.Registration) {
			r.PRURL, r.Status, r.MergedAt = fmt.Sprintf("https://github.com/o/r/pull/%d", n), status, at
		})
	}
	p := buildBoardPayload([]registry.Registration{
		mk(1, registry.StatusMerged, now.Add(-3*time.Hour)),
		mk(2, registry.StatusMerged, now.Add(-time.Hour)),
		mk(3, registry.StatusMerged, now.Add(-2*time.Hour)),
	}, nil, boardTitleOf)
	var refs []string
	for _, c := range cardsOf(p, boardDone) {
		refs = append(refs, c.Ref)
	}
	if want := []string{"o/r#2", "o/r#3", "o/r#1"}; !slices.Equal(refs, want) {
		t.Errorf("done order = %v, want %v", refs, want)
	}
}

func TestBoardPayloadStaysUnderTheHostCapByDroppingOldFinishedCards(t *testing.T) {
	now := time.Now()
	var regs []registry.Registration
	for i := range 400 {
		i := i
		regs = append(regs, watching(func(r *registry.Registration) {
			r.PRURL = fmt.Sprintf("https://github.com/o/r/pull/%d", i+1)
			r.Title = strings.Repeat("long title ", 12)
			r.Status, r.MergedAt = registry.StatusMerged, now.Add(-time.Duration(i)*time.Minute)
		}))
	}
	open := watching(func(r *registry.Registration) { r.PRURL = "https://github.com/o/r/pull/9999" })
	regs = append(regs, open)

	p := buildBoardPayload(regs, nil, boardTitleOf)
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if len(raw) > boardPayloadBudget {
		t.Errorf("payload is %d bytes, want at most %d", len(raw), boardPayloadBudget)
	}
	if p.Dropped == 0 {
		t.Errorf("Dropped = 0, want the number of finished cards left out")
	}
	if len(cardsOf(p, boardInReview)) != 1 {
		t.Errorf("the open PR must survive the cut")
	}
	done := cardsOf(p, boardDone)
	if len(done) == 0 || done[0].Ref != "o/r#1" {
		t.Errorf("done cards = %d, first %+v: the newest finished ones must be the ones kept", len(done), done)
	}
	if len(done)+p.Dropped != 400 {
		t.Errorf("kept %d + dropped %d != 400", len(done), p.Dropped)
	}
}

func TestBoardCardTitleIsShortened(t *testing.T) {
	p := buildBoardPayload([]registry.Registration{watching(func(r *registry.Registration) { r.Title = strings.Repeat("x", 500) })}, nil, boardTitleOf)
	if got := len([]rune(cardsOf(p, boardInReview)[0].Title)); got > 120 {
		t.Errorf("title is %d runes, want at most 120", got)
	}
}

func TestHostRPCPushBoardPageSendsTheGlobalPageState(t *testing.T) {
	var buf bytes.Buffer
	h := newHostRPC(&buf)
	if err := h.pushBoardPage(boardPayload{Columns: []boardColumn{{ID: boardNew, Title: "New"}}}); err != nil {
		t.Fatalf("pushBoardPage() error = %v", err)
	}
	var msg map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &msg); err != nil {
		t.Fatalf("decode: %v", err)
	}
	params := msg["params"].(map[string]any)
	if msg["method"] != "ui.state.set" || params["slot"] != boardPageSlot || params["id"] != boardPageID {
		t.Errorf("message = %v, want ui.state.set of %s/%s", msg, boardPageSlot, boardPageID)
	}
	if _, has := params["session_id"]; has {
		t.Error("a page's state is global, it must not carry a session_id")
	}
	if cols, _ := params["payload"].(map[string]any)["columns"].([]any); len(cols) != 1 {
		t.Errorf("payload = %v", params["payload"])
	}
}

func TestPushUIStatePushesTheBoardAndNoLongerTheHomePane(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	store, err := registry.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("registry.Open() error = %v", err)
	}
	defer func() { _ = store.Close() }()
	now := time.Now()
	if err := store.Add(t.Context(), registry.Registration{
		PRURL: "https://github.com/o/r/pull/1", SessionID: "sess1", Provider: "github", NextDueAt: now, CreatedAt: now,
	}); err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	var buf bytes.Buffer
	pushUIState(t.Context(), store, aoeclient.New(nil), newHostRPC(&buf), logger)

	sawBoard := false
	for line := range bytes.SplitSeq(bytes.TrimSpace(buf.Bytes()), []byte("\n")) {
		var m struct {
			Params struct {
				Slot string `json:"slot"`
				ID   string `json:"id"`
			} `json:"params"`
		}
		if err := json.Unmarshal(line, &m); err != nil {
			t.Fatalf("decode %q: %v", line, err)
		}
		if m.Params.Slot == "home-pane" {
			t.Errorf("a home-pane was pushed, the board replaces it")
		}
		if m.Params.Slot == boardPageSlot && m.Params.ID == boardPageID {
			sawBoard = true
		}
	}
	if !sawBoard {
		t.Error("the board page state was not pushed")
	}
}

func TestPushUIStateShowsNotifySubscriptionMatchesInTheNewColumn(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	store, err := registry.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("registry.Open() error = %v", err)
	}
	defer func() { _ = store.Close() }()
	subID, err := store.AddSubscription(t.Context(), registry.Subscription{
		SessionID: "orch", Provider: "github", Host: "github.com", ScopeKind: "repo", ScopePath: "o/r",
		Filters: "{}", Mode: registry.SubscriptionModeNotify,
	})
	if err != nil {
		t.Fatalf("AddSubscription() error = %v", err)
	}
	if err := store.SetTrackedMatch(t.Context(), subID, "https://github.com/o/r/pull/7", true); err != nil {
		t.Fatalf("SetTrackedMatch() error = %v", err)
	}
	var buf bytes.Buffer
	pushUIState(t.Context(), store, aoeclient.New(nil), newHostRPC(&buf), logger)

	for line := range bytes.SplitSeq(bytes.TrimSpace(buf.Bytes()), []byte("\n")) {
		var m struct {
			Params struct {
				Slot    string       `json:"slot"`
				Payload boardPayload `json:"payload"`
			} `json:"params"`
		}
		if err := json.Unmarshal(line, &m); err != nil {
			t.Fatalf("decode %q: %v", line, err)
		}
		if m.Params.Slot != boardPageSlot {
			continue
		}
		fresh := cardsOf(m.Params.Payload, boardNew)
		if len(fresh) != 1 || fresh[0].Ref != "o/r#7" || fresh[0].Session != "orch" || !slices.Contains(fresh[0].Facts, "pr:draft") {
			t.Errorf("new cards = %+v, want the tracked draft o/r#7 of session orch", fresh)
		}
		return
	}
	t.Error("the board page state was not pushed")
}

func TestManifestDeclaresTheBoardPageAndItsFileIsSelfContained(t *testing.T) {
	manifest, err := os.ReadFile("../../aoe-plugin.toml")
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	text := string(manifest)
	m := regexp.MustCompile(`(?m)^slot = "page"\nid = "` + boardPageID + `"\ntitle = "[^"]+"\nicon = "[^"]+"\nentry = "([^"]+\.html)"`).FindStringSubmatch(text)
	if m == nil {
		t.Fatalf("manifest declares no page %q with a title, icon and .html entry", boardPageID)
	}
	if v := regexp.MustCompile(`(?m)^api_version = (\d+)`).FindStringSubmatch(text); v == nil || v[1] < "16" || len(v[1]) < 2 {
		t.Errorf("api_version = %v, a page needs at least 16", v)
	}
	if strings.Contains(text, `slot = "home-pane"`) {
		t.Error("the manifest still declares the home-pane the board replaces")
	}

	page, err := os.ReadFile(filepath.Join("../..", m[1]))
	if err != nil {
		t.Fatalf("read the page entry %s: %v", m[1], err)
	}
	// The host serves one file and blocks every other load, so nothing may
	// point outside it.
	for _, banned := range []string{"http://", "https://", `src="`, "src='", "<link", "@import", "fetch(", "XMLHttpRequest"} {
		if bytes.Contains(page, []byte(banned)) {
			t.Errorf("page contains %q, which the sandbox would block or that points outside the file", banned)
		}
	}
	for _, need := range []string{"aoeBridge", "plugins.uiState", "openSession", "postMessage"} {
		if !bytes.Contains(page, []byte(need)) {
			t.Errorf("page never uses %q", need)
		}
	}
}

// A column with no cards must reach the page as an empty list: a null would
// break the page's reading of its length.
func TestBoardPayloadEncodesEmptyColumnsAsEmptyLists(t *testing.T) {
	raw, err := json.Marshal(buildBoardPayload(nil, nil, boardTitleOf))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if bytes.Contains(raw, []byte(`"cards":null`)) {
		t.Errorf("payload = %s, want no null card lists", raw)
	}
	if got := bytes.Count(raw, []byte(`"cards":[]`)); got != len(boardColumnTitles) {
		t.Errorf("payload = %s, want %d empty card lists", raw, len(boardColumnTitles))
	}
}
