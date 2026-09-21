package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cwrau/aoe-pr-watch-mcp/internal/aoeclient"
	"github.com/cwrau/aoe-pr-watch-mcp/internal/registry"
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
	defer store.Close()

	now := time.Now()
	if err := store.Add(t.Context(), registry.Registration{
		PRURL: "https://github.com/o/r/pull/1", SessionID: "sess1", Provider: "github",
		NextDueAt: now, CreatedAt: now,
	}); err != nil {
		t.Fatalf("Add() error = %v", err)
	}

	aoe, err := aoeclient.New()
	if err != nil {
		t.Fatalf("aoeclient.New() error = %v", err)
	}

	var buf bytes.Buffer
	rpc := newHostRPC(&buf)

	// Each push gets its own short-deadline context: pushUIState's
	// getStringSetting call blocks waiting for a config.get reply that
	// nothing in this test ever sends, so it must time out quickly rather
	// than eating the real panePushTimeout (10s) on every call.
	pushCtx1, cancel1 := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel1()

	// First push: sess1 has a registration, so it should get a row-badge
	// set and no remove.
	badgeSessions := pushUIState(pushCtx1, store, aoe, rpc, nil, logger)
	if !badgeSessions["sess1"] {
		t.Fatalf("badgeSessions after first push = %v, want sess1 present", badgeSessions)
	}
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

	// Now drop the registration (as if it merged/closed) and push again:
	// sess1 must get an explicit remove this time, and the returned set
	// must no longer contain it.
	if err := store.Remove(t.Context(), "https://github.com/o/r/pull/1"); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
	buf.Reset()

	pushCtx2, cancel2 := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel2()
	badgeSessions = pushUIState(pushCtx2, store, aoe, rpc, badgeSessions, logger)
	if badgeSessions["sess1"] {
		t.Errorf("badgeSessions after second push = %v, want sess1 gone", badgeSessions)
	}
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
