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
