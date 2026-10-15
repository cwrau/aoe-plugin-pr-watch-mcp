package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cwrau/aoe-plugin-pr-watch-mcp/internal/scheduler"
)

func TestParseSnoozeRules(t *testing.T) {
	raw := json.RawMessage(`{
		"value": [
			{"_id": "a", "match": ["gitlab.corp.example/*", " github.com/work/* "], "gate": "systemctl --user is-active --quiet work.target"},
			{"_id": "b", "match": ["github.com/other/*"], "gate": "other-check"},
			{"_id": "c", "match": [], "gate": "no-patterns"},
			{"_id": "d", "match": ["github.com/x/*"], "gate": "  "},
			{"_id": "e", "match": ["", "  "], "gate": "only-blank-patterns"}
		],
		"revision": 4
	}`)
	got, err := parseSnoozeRules(raw)
	if err != nil {
		t.Fatalf("parseSnoozeRules() error = %v", err)
	}
	want := []scheduler.SnoozeRule{
		{Match: []string{"gitlab.corp.example/*", "github.com/work/*"}, Gate: "systemctl --user is-active --quiet work.target"},
		{Match: []string{"github.com/other/*"}, Gate: "other-check"},
	}
	if len(got) != len(want) {
		t.Fatalf("parseSnoozeRules() = %+v, want %+v (incomplete items dropped)", got, want)
	}
	for i := range want {
		if got[i].Gate != want[i].Gate || !slices.Equal(got[i].Match, want[i].Match) {
			t.Errorf("rule[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestParseSnoozeRulesNullValueMeansNoRules(t *testing.T) {
	got, err := parseSnoozeRules(json.RawMessage(`{"value": null}`))
	if err != nil {
		t.Fatalf("parseSnoozeRules() error = %v", err)
	}
	if len(got) != 0 {
		t.Errorf("parseSnoozeRules() = %+v, want none", got)
	}
}

func TestParseSnoozeRulesRejectsGarbage(t *testing.T) {
	if _, err := parseSnoozeRules(json.RawMessage(`{"value": "not a list"}`)); err == nil {
		t.Error("parseSnoozeRules() error = nil, want an error for a non-list value")
	}
}

// replyingHost wires a hostRPC's output to a fake host that answers each
// config.get with reply (or an error object when replyErr is set).
type replyingHost struct {
	mu       sync.Mutex
	rpc      *hostRPC
	reply    string // JSON for "result"
	replyErr bool
	errData  string // JSON for the error's "data", when replyErr is set; empty means none
	seen     []string
	params   []string // raw params JSON of each request, parallel to seen
}

func newReplyingHost(reply string) *replyingHost {
	h := &replyingHost{reply: reply}
	pr, pw := io.Pipe()
	h.rpc = newHostRPC(pw)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := pr.Read(buf)
			if err != nil {
				return
			}
			for line := range strings.SplitSeq(strings.TrimSpace(string(buf[:n])), "\n") {
				var req struct {
					ID     int             `json:"id"`
					Method string          `json:"method"`
					Params json.RawMessage `json:"params"`
				}
				if json.Unmarshal([]byte(line), &req) != nil {
					continue
				}
				h.mu.Lock()
				h.seen = append(h.seen, req.Method)
				h.params = append(h.params, string(req.Params))
				var resp string
				if h.replyErr {
					data := ""
					if h.errData != "" {
						data = `,"data":` + h.errData
					}
					resp = `{"jsonrpc":"2.0","id":` + itoa(req.ID) + `,"error":{"code":-32000,"message":"boom"` + data + `}}`
				} else {
					resp = `{"jsonrpc":"2.0","id":` + itoa(req.ID) + `,"result":` + h.reply + `}`
				}
				h.mu.Unlock()
				h.rpc.handleInboundLine([]byte(resp))
			}
		}
	}()
	return h
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestHostRPCCallReturnsMatchingReply(t *testing.T) {
	h := newReplyingHost(`{"value": 42}`)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	got, err := h.rpc.call(ctx, "config.get", map[string]any{"key": "x"})
	if err != nil {
		t.Fatalf("call() error = %v", err)
	}
	if string(got) != `{"value": 42}` {
		t.Errorf("call() = %s, want the reply result", got)
	}
}

func TestHostRPCCallSurfacesRPCError(t *testing.T) {
	h := newReplyingHost(`null`)
	h.replyErr = true
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if _, err := h.rpc.call(ctx, "config.get", nil); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("call() error = %v, want the host's error message", err)
	}
}

func TestHostRPCCallTimesOutWithoutReply(t *testing.T) {
	var buf bytes.Buffer
	h := newHostRPC(&buf) // nobody ever answers
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	if _, err := h.call(ctx, "config.get", nil); err == nil {
		t.Error("call() error = nil, want a timeout error")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.pending) != 0 {
		t.Errorf("pending = %d waiters, want 0 (timed-out call must clean up)", len(h.pending))
	}
}

func TestHostRPCIgnoresReplyForUnknownID(t *testing.T) {
	var buf bytes.Buffer
	h := newHostRPC(&buf)
	h.handleInboundLine([]byte(`{"jsonrpc":"2.0","id":999,"result":{}}`)) // must not panic or block
}

func TestHostRPCSettingsChangedNotificationTriggersCallback(t *testing.T) {
	var buf bytes.Buffer
	h := newHostRPC(&buf)
	fired := make(chan struct{}, 1)
	h.onSettingsChanged = func() { fired <- struct{}{} }

	h.handleInboundLine([]byte(`{"jsonrpc":"2.0","method":"plugin.settings.changed","params":{"revision":2,"changed_keys":["snooze"]}}`))

	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("onSettingsChanged was not invoked")
	}
	if buf.Len() != 0 {
		t.Errorf("wrote %q in response to a notification, want nothing", buf.String())
	}
}

func TestSnoozeRulesRefreshLoadsAndKeepsPreviousOnFailure(t *testing.T) {
	h := newReplyingHost(`{"value":[{"_id":"a","match":["github.com/work/*"],"gate":"g"}]}`)
	s := &snoozeRules{}

	s.refresh(context.Background(), h.rpc, testLogger())
	if got := s.get(); len(got) != 1 || got[0].Gate != "g" {
		t.Fatalf("get() after refresh = %+v, want the one loaded rule", got)
	}

	h.mu.Lock()
	h.replyErr = true
	h.mu.Unlock()
	s.refresh(context.Background(), h.rpc, testLogger())
	if got := s.get(); len(got) != 1 || got[0].Gate != "g" {
		t.Errorf("get() after failed refresh = %+v, want the previous rule kept", got)
	}

	h.mu.Lock()
	h.replyErr = false
	h.reply = `{"value":[]}`
	h.mu.Unlock()
	s.refresh(context.Background(), h.rpc, testLogger())
	if got := s.get(); len(got) != 0 {
		t.Errorf("get() after emptying the setting = %+v, want no rules (clearing must un-snooze)", got)
	}
}
