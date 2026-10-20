package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cwrau/aoe-plugin-pr-watch-mcp/internal/aoeclient"
	"github.com/cwrau/aoe-plugin-pr-watch-mcp/internal/registry"
)

// methodsOf decodes every ui.state.set/remove line written to buf and
// returns, for each, its method and session_id (empty for the
// session-less home-pane push), in write order.
func methodsOf(t *testing.T, buf *bytes.Buffer) []struct{ Method, SessionID string } {
	t.Helper()
	var out []struct{ Method, SessionID string }
	for line := range strings.SplitSeq(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var msg struct {
			Method string `json:"method"`
			Params struct {
				SessionID string `json:"session_id"`
			} `json:"params"`
		}
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			t.Fatalf("decode outbound line: %v (%q)", err, line)
		}
		out = append(out, struct{ Method, SessionID string }{msg.Method, msg.Params.SessionID})
	}
	return out
}

func TestPushUIStateRemovesBadgeForSessionThatDroppedOut(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	store, err := registry.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("registry.Open() error = %v", err)
	}
	defer func() { _ = store.Close() }()

	now := time.Now()
	if err := store.Add(t.Context(), registry.Registration{
		PRURL: "https://github.com/o/r/pull/1", SessionID: "sess1", Provider: "github",
		NextDueAt: now, CreatedAt: now,
	}); err != nil {
		t.Fatalf("Add() error = %v", err)
	}

	aoe := aoeclient.New(nil) // pushUIState only reads titles; an empty directory falls back to raw ids

	var buf bytes.Buffer
	rpc := newHostRPC(&buf)

	// First push: sess1 has a watching registration, so it should get a
	// row-badge set and no remove.
	pushUIState(t.Context(), store, aoe, rpc, logger)
	firstRound := methodsOf(t, &buf)
	sawSetForSess1 := false
	for _, m := range firstRound {
		if m.Method == "ui.state.set" && m.SessionID == "sess1" {
			sawSetForSess1 = true
		}
		if m.Method == "ui.state.remove" {
			t.Errorf("first push (nothing dropped yet) sent a remove: %+v", m)
		}
	}
	if !sawSetForSess1 {
		t.Fatalf("first push never sent ui.state.set for sess1, got %+v", firstRound)
	}

	// Now stop the registration (as if the owner unregistered it) and push
	// again: sess1 must get an explicit remove this time. The registration
	// row itself survives (never deleted) — it's the status change alone
	// that must drive the badge to disappear, restart-safe with no
	// in-memory bookkeeping needed to know sess1 previously had one.
	if err := store.SetStatus(t.Context(), "https://github.com/o/r/pull/1", "sess1", registry.StatusStopped); err != nil {
		t.Fatalf("SetStatus() error = %v", err)
	}
	buf.Reset()

	pushUIState(t.Context(), store, aoe, rpc, logger)
	secondRound := methodsOf(t, &buf)
	sawRemoveForSess1 := false
	for _, m := range secondRound {
		if m.Method == "ui.state.remove" && m.SessionID == "sess1" {
			sawRemoveForSess1 = true
		}
	}
	if !sawRemoveForSess1 {
		t.Errorf("second push (registration dropped) never sent ui.state.remove for sess1, got %+v", secondRound)
	}
}

// TestPushUIStateReconcilesStatelesslyOnFirstCall guards the actual bug
// being fixed here: the old implementation tracked which sessions had a
// badge in an in-memory map that reset to nil on every worker restart, so a
// registration that turned terminal while the worker was down left a
// permanently stale badge behind (nothing was ever there to diff against).
// Reconciliation must instead be derived fresh from the durable registry
// state every single call — including the very first one, with no prior
// in-memory tracking at all — which this test exercises directly by calling
// pushUIState exactly once against a session that only has a stopped
// registration.
func TestPushUIStateReconcilesStatelesslyOnFirstCall(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	store, err := registry.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("registry.Open() error = %v", err)
	}
	defer func() { _ = store.Close() }()

	now := time.Now()
	if err := store.Add(t.Context(), registry.Registration{
		PRURL: "https://github.com/o/r/pull/1", SessionID: "sess1", Provider: "github",
		NextDueAt: now, CreatedAt: now,
	}); err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	if err := store.SetStatus(t.Context(), "https://github.com/o/r/pull/1", "sess1", registry.StatusStopped); err != nil {
		t.Fatalf("SetStatus() error = %v", err)
	}

	aoe := aoeclient.New(nil) // pushUIState only reads titles; an empty directory falls back to raw ids

	var buf bytes.Buffer
	rpc := newHostRPC(&buf)

	pushUIState(t.Context(), store, aoe, rpc, logger)

	sawRemoveForSess1 := false
	for _, m := range methodsOf(t, &buf) {
		if m.Method == "ui.state.set" && m.SessionID == "sess1" {
			t.Errorf("first-ever push sent ui.state.set for sess1, want no badge for an already-stopped registration")
		}
		if m.Method == "ui.state.remove" && m.SessionID == "sess1" {
			sawRemoveForSess1 = true
		}
	}
	if !sawRemoveForSess1 {
		t.Error("first-ever push never sent ui.state.remove for sess1, want it removed even with no prior in-memory state (restart-safety)")
	}
}
