package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"strings"
	"sync"
	"testing"
)

func TestHostRPCSendWritesOneJSONLine(t *testing.T) {
	var buf bytes.Buffer
	h := newHostRPC(&buf)

	if err := h.send("ui.state.set", map[string]any{"slot": "home-pane"}); err != nil {
		t.Fatalf("send() error = %v", err)
	}

	line := strings.TrimSuffix(buf.String(), "\n")
	if strings.Contains(line, "\n") {
		t.Fatalf("send() wrote more than one line: %q", buf.String())
	}

	var msg map[string]any
	if err := json.Unmarshal([]byte(line), &msg); err != nil {
		t.Fatalf("written line is not valid JSON: %v (%q)", err, line)
	}
	if msg["jsonrpc"] != "2.0" {
		t.Errorf("jsonrpc = %v, want %q", msg["jsonrpc"], "2.0")
	}
	if msg["method"] != "ui.state.set" {
		t.Errorf("method = %v, want %q", msg["method"], "ui.state.set")
	}
	if msg["id"] == nil {
		t.Error("id is missing, want a request id present")
	}
	params, ok := msg["params"].(map[string]any)
	if !ok {
		t.Fatalf("params = %T, want an object", msg["params"])
	}
	if params["slot"] != "home-pane" {
		t.Errorf("params.slot = %v, want %q", params["slot"], "home-pane")
	}
}

func TestHostRPCSendUsesDistinctIDsPerCall(t *testing.T) {
	var buf bytes.Buffer
	h := newHostRPC(&buf)

	if err := h.send("a", nil); err != nil {
		t.Fatalf("send() error = %v", err)
	}
	if err := h.send("b", nil); err != nil {
		t.Fatalf("send() error = %v", err)
	}

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("wrote %d lines, want 2", len(lines))
	}
	var first, second map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("decode line 1: %v", err)
	}
	if err := json.Unmarshal([]byte(lines[1]), &second); err != nil {
		t.Fatalf("decode line 2: %v", err)
	}
	if first["id"] == second["id"] {
		t.Errorf("both calls got id %v, want distinct ids", first["id"])
	}
}

func TestHostRPCSendIsConcurrencySafe(t *testing.T) {
	var buf bytes.Buffer
	h := newHostRPC(&buf)

	const n = 50
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			if err := h.send("concurrent", map[string]any{"x": 1}); err != nil {
				t.Errorf("send() error = %v", err)
			}
		})
	}
	wg.Wait()

	scanner := bufio.NewScanner(&buf)
	count := 0
	for scanner.Scan() {
		var msg map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &msg); err != nil {
			t.Fatalf("concurrent sends produced an interleaved/invalid line: %v (%q)", err, scanner.Text())
		}
		count++
	}
	if count != n {
		t.Errorf("decoded %d complete lines, want %d", count, n)
	}
}

func TestHostRPCPushHomePaneSendsCorrectSlotAndID(t *testing.T) {
	var buf bytes.Buffer
	h := newHostRPC(&buf)

	payload := panePayload{Title: "PR Watch", Blocks: []any{noteBlock{Kind: "note", Text: "hi"}}}
	if err := h.pushHomePane(payload); err != nil {
		t.Fatalf("pushHomePane() error = %v", err)
	}

	var msg map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &msg); err != nil {
		t.Fatalf("decode: %v", err)
	}
	params := msg["params"].(map[string]any)
	if params["slot"] != homePaneSlot {
		t.Errorf("slot = %v, want %q", params["slot"], homePaneSlot)
	}
	if params["id"] != homePaneID {
		t.Errorf("id = %v, want %q", params["id"], homePaneID)
	}
	pushedPayload, ok := params["payload"].(map[string]any)
	if !ok {
		t.Fatalf("payload = %T, want an object", params["payload"])
	}
	if pushedPayload["title"] != "PR Watch" {
		t.Errorf("payload.title = %v, want %q", pushedPayload["title"], "PR Watch")
	}
}

func TestHostRPCSetRowBadgeSendsCorrectSlotIDAndSession(t *testing.T) {
	var buf bytes.Buffer
	h := newHostRPC(&buf)

	payload := rowBadgePayload{Items: []badgeChip{{Icon: "eye", Tone: "success", Tooltip: "hi", Href: "https://example.com"}}}
	if err := h.setRowBadge("sess1", payload); err != nil {
		t.Fatalf("setRowBadge() error = %v", err)
	}

	var msg map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &msg); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if msg["method"] != "ui.state.set" {
		t.Errorf("method = %v, want %q", msg["method"], "ui.state.set")
	}
	params := msg["params"].(map[string]any)
	if params["slot"] != rowBadgeSlot {
		t.Errorf("slot = %v, want %q", params["slot"], rowBadgeSlot)
	}
	if params["id"] != rowBadgeID {
		t.Errorf("id = %v, want %q", params["id"], rowBadgeID)
	}
	if params["session_id"] != "sess1" {
		t.Errorf("session_id = %v, want %q", params["session_id"], "sess1")
	}
	pushedPayload, ok := params["payload"].(map[string]any)
	if !ok {
		t.Fatalf("payload = %T, want an object", params["payload"])
	}
	items, ok := pushedPayload["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("payload.items = %v, want a 1-element array", pushedPayload["items"])
	}
}

func TestHostRPCRemoveRowBadgeSendsCorrectSlotIDAndSession(t *testing.T) {
	var buf bytes.Buffer
	h := newHostRPC(&buf)

	if err := h.removeRowBadge("sess1"); err != nil {
		t.Fatalf("removeRowBadge() error = %v", err)
	}

	var msg map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &msg); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if msg["method"] != "ui.state.remove" {
		t.Errorf("method = %v, want %q", msg["method"], "ui.state.remove")
	}
	params := msg["params"].(map[string]any)
	if params["slot"] != rowBadgeSlot {
		t.Errorf("slot = %v, want %q", params["slot"], rowBadgeSlot)
	}
	if params["id"] != rowBadgeID {
		t.Errorf("id = %v, want %q", params["id"], rowBadgeID)
	}
	if params["session_id"] != "sess1" {
		t.Errorf("session_id = %v, want %q", params["session_id"], "sess1")
	}
	if _, hasPayload := params["payload"]; hasPayload {
		t.Error("ui.state.remove params carry a payload key, want none")
	}
}

func TestHostRPCHandleInboundLineRepliesMethodNotFoundForRequest(t *testing.T) {
	var buf bytes.Buffer
	h := newHostRPC(&buf)

	h.handleInboundLine([]byte(`{"jsonrpc":"2.0","id":42,"method":"some.unknown.method","params":{}}`))

	var msg map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &msg); err != nil {
		t.Fatalf("decode auto-reply: %v (%q)", err, buf.String())
	}
	if msg["id"] != float64(42) {
		t.Errorf("auto-reply id = %v, want 42", msg["id"])
	}
	errObj, ok := msg["error"].(map[string]any)
	if !ok {
		t.Fatalf("auto-reply error = %T, want an object", msg["error"])
	}
	if errObj["code"] != float64(-32601) {
		t.Errorf("auto-reply error.code = %v, want -32601 (method not found)", errObj["code"])
	}
}

func TestHostRPCHandleInboundLineIgnoresMalformedOrNotification(t *testing.T) {
	var buf bytes.Buffer
	h := newHostRPC(&buf)

	h.handleInboundLine([]byte("not json at all"))
	h.handleInboundLine([]byte(`{"jsonrpc":"2.0","method":"plugin.settings.changed","params":{}}`))
	h.handleInboundLine([]byte(`{"jsonrpc":"2.0","id":999,"result":{}}`)) // reply to nothing we're waiting on

	if buf.Len() != 0 {
		t.Errorf("handleInboundLine() wrote %q for malformed/notification/stray-reply input, want nothing", buf.String())
	}
}

func sessionStatusLine(sessionID, from, to string) []byte {
	return []byte(`{"jsonrpc":"2.0","method":"session.status.changed","params":{"session_id":"` + sessionID + `","title":"t","from":"` + from + `","to":"` + to + `","at":"2026-10-03T14:00:00Z"}}`)
}

// TestHostRPCSessionStatusChangedIdleNotifies: AoE pushes every status
// transition as an id-less notification; only a transition *to* Idle ("the
// agent stopped working") is worth a discovery pass, and a notification is
// never replied to.
func TestHostRPCSessionStatusChangedIdleNotifies(t *testing.T) {
	var buf bytes.Buffer
	h := newHostRPC(&buf)
	var idle []string
	h.onSessionIdle = func(sessionID string) { idle = append(idle, sessionID) }

	h.handleInboundLine(sessionStatusLine("abc", "Running", "Idle"))
	h.handleInboundLine(sessionStatusLine("def", "Idle", "Running"))
	h.handleInboundLine(sessionStatusLine("ghi", "Running", "Waiting"))
	h.handleInboundLine(sessionStatusLine("jkl", "Running", "Idle"))

	if len(idle) != 2 || idle[0] != "abc" || idle[1] != "jkl" {
		t.Errorf("onSessionIdle calls = %v, want [abc jkl] (only transitions to Idle)", idle)
	}
	if buf.Len() != 0 {
		t.Errorf("handleInboundLine() wrote %q for a notification, want nothing", buf.String())
	}
}

func TestHostRPCSessionStatusChangedWithoutHandlerIsIgnored(t *testing.T) {
	h := newHostRPC(&bytes.Buffer{})
	h.handleInboundLine(sessionStatusLine("abc", "Running", "Idle")) // must not panic
}

func sessionLifecycleLine(kind, sessionID, summary string) []byte {
	s := `{"jsonrpc":"2.0","method":"session.lifecycle.changed","params":{"session_id":"` + sessionID + `","kind":"` + kind + `","at":"2026-10-05T10:00:00Z"`
	if summary != "" {
		s += `,"summary":` + summary
	}
	return []byte(s + `}}`)
}

// TestHostRPCSessionLifecycleChangedDelivers: every lifecycle kind reaches
// the handler with its summary (absent for "deleted"), and a notification
// is never replied to.
func TestHostRPCSessionLifecycleChangedDelivers(t *testing.T) {
	var buf bytes.Buffer
	h := newHostRPC(&buf)
	type got struct{ kind, id, summary string }
	var calls []got
	h.onSessionLifecycle = func(kind, sessionID string, summary json.RawMessage) {
		calls = append(calls, got{kind, sessionID, string(summary)})
	}

	h.handleInboundLine(sessionLifecycleLine("created", "abc", `{"id":"abc","title":"T"}`))
	h.handleInboundLine(sessionLifecycleLine("deleted", "abc", ""))

	want := []got{{"created", "abc", `{"id":"abc","title":"T"}`}, {"deleted", "abc", ""}}
	if len(calls) != 2 || calls[0] != want[0] || calls[1] != want[1] {
		t.Errorf("onSessionLifecycle calls = %+v, want %+v", calls, want)
	}
	if buf.Len() != 0 {
		t.Errorf("handleInboundLine() wrote %q for a notification, want nothing", buf.String())
	}
}

func TestHostRPCSessionLifecycleMalformedIsIgnored(t *testing.T) {
	h := newHostRPC(&bytes.Buffer{})
	called := false
	h.onSessionLifecycle = func(string, string, json.RawMessage) { called = true }

	h.handleInboundLine([]byte(`{"jsonrpc":"2.0","method":"session.lifecycle.changed","params":"nope"}`))
	h.handleInboundLine([]byte(`{"jsonrpc":"2.0","method":"session.lifecycle.changed","params":{"kind":"created"}}`)) // no session_id
	if called {
		t.Error("onSessionLifecycle called for a malformed notification, want it ignored")
	}
}

func TestHostRPCCallErrorCarriesTheStableKind(t *testing.T) {
	h := newReplyingHost(`{}`)
	h.replyErr = true
	h.errData = `{"kind":"session_archived"}`

	_, err := h.rpc.call(t.Context(), "sessions.message.send", map[string]any{"session_id": "s", "message": "m"})
	if err == nil {
		t.Fatal("call() error = nil, want the host's error")
	}
	if !strings.Contains(err.Error(), "session_archived") || !strings.Contains(err.Error(), "boom") {
		t.Errorf("call() error = %q, want it to carry both the message and data.kind", err)
	}
}

func TestHostRPCCallExportedWrapperMatchesCall(t *testing.T) {
	h := newReplyingHost(`{"ok":true}`)
	got, err := h.rpc.Call(t.Context(), "any.method", nil)
	if err != nil || string(got) != `{"ok":true}` {
		t.Errorf("Call() = %s, %v, want the reply result", got, err)
	}
}

func TestHostRPCListSessionsParsesTheHostReply(t *testing.T) {
	h := newReplyingHost(`{"sessions":[{"id":"a","title":"One","archived":true},{"id":"b","trashed":true}]}`)

	got, err := h.rpc.ListSessions(t.Context())
	if err != nil {
		t.Fatalf("ListSessions() error = %v", err)
	}
	if len(got) != 2 || got[0].ID != "a" || !got[0].Archived || !got[1].Trashed {
		t.Errorf("ListSessions() = %+v", got)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.seen) != 1 || h.seen[0] != "sessions.list" {
		t.Errorf("methods called = %v, want exactly [sessions.list]", h.seen)
	}
	// No "exclude": the directory needs archived and trashed sessions too,
	// to tell them apart from deleted ones.
	if strings.Contains(h.params[0], "exclude") {
		t.Errorf("params = %s, want no exclude filter", h.params[0])
	}
}

func TestHostRPCListSessionsPropagatesErrors(t *testing.T) {
	h := newReplyingHost(`{}`)
	h.replyErr = true
	if _, err := h.rpc.ListSessions(t.Context()); err == nil {
		t.Error("ListSessions() error = nil on an RPC error, want it propagated")
	}
}
