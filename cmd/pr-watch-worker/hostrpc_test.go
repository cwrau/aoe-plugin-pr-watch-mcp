package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
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

// syncBuffer wraps bytes.Buffer with a mutex: hostRPC's own writes are
// already serialized internally, but a test that polls Len()/String() from
// the main goroutine while a call() goroutine is concurrently writing
// needs the read side synchronized too, or the race detector (correctly)
// flags the unsynchronized access to the underlying buffer memory.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *syncBuffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Len()
}

// outboundID reads the single most-recently-written outbound line from buf
// and returns its "id" field, for a test to build a matching reply.
func outboundID(t *testing.T, buf *syncBuffer) int {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	var msg map[string]any
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &msg); err != nil {
		t.Fatalf("decode outbound line: %v (%q)", err, lines[len(lines)-1])
	}
	idFloat, ok := msg["id"].(float64)
	if !ok {
		t.Fatalf("outbound id = %T, want a number", msg["id"])
	}
	return int(idFloat)
}

func TestHostRPCCallDeliversMatchingReply(t *testing.T) {
	buf := &syncBuffer{}
	h := newHostRPC(buf)

	type callResult struct {
		result json.RawMessage
		err    error
	}
	done := make(chan callResult, 1)
	go func() {
		result, err := h.call(context.Background(), "config.get", map[string]any{"key": "dashboard_base_url"})
		done <- callResult{result, err}
	}()

	// Poll until the call has actually written its request, then reply to it.
	deadline := time.Now().Add(2 * time.Second)
	var id int
	for {
		if buf.Len() > 0 {
			id = outboundID(t, buf)
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the outbound config.get request")
		}
		time.Sleep(time.Millisecond)
	}

	reply := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{"value":"https://example.com","revision":3}}`, id)
	h.handleInboundLine([]byte(reply))

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("call() error = %v", r.err)
		}
		var parsed struct {
			Value string `json:"value"`
		}
		if err := json.Unmarshal(r.result, &parsed); err != nil {
			t.Fatalf("unmarshal result: %v", err)
		}
		if parsed.Value != "https://example.com" {
			t.Errorf("result.value = %q, want %q", parsed.Value, "https://example.com")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("call() did not return after its reply was delivered")
	}
}

func TestHostRPCCallReturnsErrorFromHostErrorReply(t *testing.T) {
	buf := &syncBuffer{}
	h := newHostRPC(buf)

	done := make(chan error, 1)
	go func() {
		_, err := h.call(context.Background(), "config.get", map[string]any{"key": "x"})
		done <- err
	}()

	deadline := time.Now().Add(2 * time.Second)
	var id int
	for {
		if buf.Len() > 0 {
			id = outboundID(t, buf)
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the outbound request")
		}
		time.Sleep(time.Millisecond)
	}

	reply := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"error":{"code":-32000,"message":"boom"}}`, id)
	h.handleInboundLine([]byte(reply))

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("call() error = nil, want an error from the host's error reply")
		}
		if !strings.Contains(err.Error(), "boom") {
			t.Errorf("call() error = %v, want it to mention %q", err, "boom")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("call() did not return")
	}
}

func TestHostRPCCallRespectsContextCancellation(t *testing.T) {
	var buf bytes.Buffer
	h := newHostRPC(&buf)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := h.call(ctx, "config.get", map[string]any{"key": "x"}); err == nil {
		t.Error("call() with an already-cancelled context, want error")
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

func TestHostRPCGetStringSettingReturnsHostValue(t *testing.T) {
	buf := &syncBuffer{}
	h := newHostRPC(buf)

	done := make(chan string, 1)
	go func() {
		done <- h.getStringSetting(context.Background(), "dashboard_base_url", "fallback")
	}()

	deadline := time.Now().Add(2 * time.Second)
	var id int
	for {
		if buf.Len() > 0 {
			id = outboundID(t, buf)
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the outbound config.get request")
		}
		time.Sleep(time.Millisecond)
	}

	reply := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{"value":"https://agent.example.com","revision":1}}`, id)
	h.handleInboundLine([]byte(reply))

	select {
	case got := <-done:
		if got != "https://agent.example.com" {
			t.Errorf("getStringSetting() = %q, want %q", got, "https://agent.example.com")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("getStringSetting() did not return")
	}
}

func TestHostRPCGetStringSettingFallsBackOnHostError(t *testing.T) {
	buf := &syncBuffer{}
	h := newHostRPC(buf)

	done := make(chan string, 1)
	go func() {
		done <- h.getStringSetting(context.Background(), "dashboard_base_url", "fallback")
	}()

	deadline := time.Now().Add(2 * time.Second)
	var id int
	for {
		if buf.Len() > 0 {
			id = outboundID(t, buf)
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the outbound request")
		}
		time.Sleep(time.Millisecond)
	}

	reply := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"error":{"code":-32000,"message":"no such setting"}}`, id)
	h.handleInboundLine([]byte(reply))

	select {
	case got := <-done:
		if got != "fallback" {
			t.Errorf("getStringSetting() = %q, want fallback %q on host error", got, "fallback")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("getStringSetting() did not return")
	}
}

func TestHostRPCGetStringSettingFallsBackOnCancelledContext(t *testing.T) {
	var buf bytes.Buffer
	h := newHostRPC(&buf)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if got := h.getStringSetting(ctx, "dashboard_base_url", "fallback"); got != "fallback" {
		t.Errorf("getStringSetting() with a cancelled context = %q, want fallback %q", got, "fallback")
	}
}
