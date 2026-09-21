package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
)

// hostRPC speaks JSON-RPC 2.0 with the AoE daemon over the worker's own
// stdio — the ndjson channel the host already supervises this process
// through. It supports two shapes: send is a fire-and-forget outbound
// push (used for ui.state.set — the host's reply, if any, is never
// waited on), and call is a blocking outbound request that waits for a
// matching reply (used for config.get). handleInboundLine is the single
// entry point for every line this worker reads from stdin: it either
// delivers a reply to a pending call, or — since this worker declares no
// commands of its own — answers a genuine host->worker request with a
// proper JSON-RPC method-not-found error, matching how a real JSON-RPC
// peer should behave rather than silently dropping it.
type hostRPC struct {
	mu      sync.Mutex
	out     io.Writer
	nextID  int
	pending map[int]chan rpcReply
}

type rpcReply struct {
	result json.RawMessage
	err    *rpcError
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// inboundEnvelope covers every shape a line on stdin can take: a reply to
// one of our own outbound calls (ID set, Method empty, Result or Error
// set), or a genuine host->worker request (ID set, Method set) or
// notification (ID absent, Method set). Fields are pointers/RawMessage
// throughout so "absent" and "present-but-zero-value" stay distinguishable.
type inboundEnvelope struct {
	ID     *int            `json:"id"`
	Method string          `json:"method"`
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

// newHostRPC's id counter starts in a high range so it never collides with
// the ids the host assigns to its own requests to this worker, same
// convention as the reference plugin.
func newHostRPC(out io.Writer) *hostRPC {
	return &hostRPC{out: out, nextID: 1_000_000, pending: make(map[int]chan rpcReply)}
}

func (h *hostRPC) writeMessage(msg map[string]any) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal %v: %w", msg["method"], err)
	}
	data = append(data, '\n')
	h.mu.Lock()
	_, err = h.out.Write(data)
	h.mu.Unlock()
	if err != nil {
		return fmt.Errorf("write %v: %w", msg["method"], err)
	}
	return nil
}

func (h *hostRPC) allocID() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.nextID++
	return h.nextID
}

func (h *hostRPC) send(method string, params any) error {
	return h.writeMessage(map[string]any{
		"jsonrpc": "2.0",
		"id":      h.allocID(),
		"method":  method,
		"params":  params,
	})
}

// call sends a request and blocks until handleInboundLine delivers the
// matching reply or ctx is done, whichever comes first.
func (h *hostRPC) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := h.allocID()
	ch := make(chan rpcReply, 1)
	h.mu.Lock()
	h.pending[id] = ch
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.pending, id)
		h.mu.Unlock()
	}()

	if err := h.writeMessage(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	}); err != nil {
		return nil, err
	}

	select {
	case reply := <-ch:
		if reply.err != nil {
			return nil, fmt.Errorf("%s: %s (code %d)", method, reply.err.Message, reply.err.Code)
		}
		return reply.result, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// handleInboundLine processes one line read from stdin. A reply to a call
// still in pending is delivered to its waiter; anything else with an id
// but a stray id (already timed out, or not ours) or no matching pending
// call is dropped. A genuine host->worker request (has a method) gets a
// method-not-found error reply, since this worker declares no commands.
// Malformed JSON and notifications (no id, has a method) are dropped
// silently.
func (h *hostRPC) handleInboundLine(line []byte) {
	var env inboundEnvelope
	if err := json.Unmarshal(line, &env); err != nil {
		return
	}

	if env.Method != "" {
		if env.ID == nil {
			return // notification; we have nothing to react to
		}
		_ = h.writeMessage(map[string]any{
			"jsonrpc": "2.0",
			"id":      *env.ID,
			"error": map[string]any{
				"code":    -32601,
				"message": fmt.Sprintf("unknown method %q", env.Method),
			},
		})
		return
	}

	if env.ID == nil {
		return
	}
	h.mu.Lock()
	ch, ok := h.pending[*env.ID]
	h.mu.Unlock()
	if !ok {
		return // stray reply: not one of ours, or already timed out
	}
	ch <- rpcReply{result: env.Result, err: env.Error}
}

// getStringSetting reads one of this plugin's own settings via config.get,
// returning fallback on any failure (host error, cancelled/expired ctx, or
// a missing/non-string value) — a dashboard convenience must never block
// or fail the caller over a settings read.
func (h *hostRPC) getStringSetting(ctx context.Context, key, fallback string) string {
	result, err := h.call(ctx, "config.get", map[string]any{"key": key})
	if err != nil {
		return fallback
	}
	var parsed struct {
		Value *string `json:"value"`
	}
	if err := json.Unmarshal(result, &parsed); err != nil || parsed.Value == nil {
		return fallback
	}
	return *parsed.Value
}

// pushHomePane pushes the worker's home-pane state via ui.state.set. The
// (slot, id) pair must match the [[ui]] entry declared in aoe-plugin.toml.
func (h *hostRPC) pushHomePane(payload panePayload) error {
	return h.send("ui.state.set", map[string]any{
		"slot":    homePaneSlot,
		"id":      homePaneID,
		"payload": payload,
	})
}

// setRowBadge pushes a session's row-badge state. Unlike home-pane, this
// slot is per-session, so the payload carries session_id alongside slot/id.
func (h *hostRPC) setRowBadge(sessionID string, payload rowBadgePayload) error {
	return h.send("ui.state.set", map[string]any{
		"slot":       rowBadgeSlot,
		"id":         rowBadgeID,
		"session_id": sessionID,
		"payload":    payload,
	})
}

// removeRowBadge clears a session's row-badge entirely — required when a
// session's last watched registration goes away (merged/closed/dropped),
// or the badge would otherwise linger on that session's row forever with
// stale content.
func (h *hostRPC) removeRowBadge(sessionID string) error {
	return h.send("ui.state.remove", map[string]any{
		"slot":       rowBadgeSlot,
		"id":         rowBadgeID,
		"session_id": sessionID,
	})
}
