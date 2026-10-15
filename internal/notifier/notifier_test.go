package notifier

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

type fakeCaller struct {
	method string
	params any
	result json.RawMessage
	err    error
	calls  int
}

func (f *fakeCaller) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	f.calls++
	f.method, f.params = method, params
	return f.result, f.err
}

func TestSendCallsTheMessageRPC(t *testing.T) {
	fc := &fakeCaller{result: json.RawMessage(`{"disposition":"sent","revived":false}`)}
	n := &Notifier{RPC: fc}

	if err := n.Send(context.Background(), "sess-1", "[pr-watch] o/r#1: CI failure"); err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	if fc.method != "sessions.message.send" {
		t.Errorf("method = %q, want sessions.message.send", fc.method)
	}
	params, ok := fc.params.(map[string]any)
	if !ok || params["session_id"] != "sess-1" || params["message"] != "[pr-watch] o/r#1: CI failure" {
		t.Errorf("params = %#v, want {session_id: sess-1, message: ...}", fc.params)
	}
}

// TestSendPassesUntrustedTextVerbatim: the message carries external content
// (comment text, reviewer names). Over RPC it is a plain JSON string, so
// nothing in it can act as a flag or command — it must arrive unchanged.
func TestSendPassesUntrustedTextVerbatim(t *testing.T) {
	fc := &fakeCaller{}
	n := &Notifier{RPC: fc}
	const msg = "--no-revive; $(rm -rf /) \"quoted\"\nnewline"

	if err := n.Send(context.Background(), "sess-1", msg); err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	if got := fc.params.(map[string]any)["message"]; got != msg {
		t.Errorf("message = %q, want it verbatim", got)
	}
}

func TestSendWrapsTheRPCErrorWithTheSessionID(t *testing.T) {
	rpcErr := errors.New("sessions.message.send: session is archived (code -32000, kind session_archived)")
	n := &Notifier{RPC: &fakeCaller{err: rpcErr}}

	err := n.Send(context.Background(), "sess-9", "hi")
	if err == nil {
		t.Fatal("Send() error = nil, want the RPC's error")
	}
	if !strings.Contains(err.Error(), "sess-9") || !errors.Is(err, rpcErr) {
		t.Errorf("Send() error = %v, want it to name the session and wrap the RPC error", err)
	}
}

func TestSendWithoutAnRPCClientFails(t *testing.T) {
	if err := (&Notifier{}).Send(context.Background(), "sess-1", "hi"); err == nil {
		t.Error("Send() error = nil without an RPC client, want an error rather than a panic")
	}
}
