package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"

	"github.com/cwrau/aoe-plugin-pr-watch-mcp/internal/aoeclient"
)

// sessionStatusChangedMethod is the notification AoE pushes (api_version
// >= 14, session.read capability) on every session status transition.
const sessionStatusChangedMethod = "session.status.changed"

// sessionLifecycleChangedMethod is the notification AoE pushes (api_version
// >= 15, session.read capability) when a session is created, archived,
// trashed, renamed, deleted and so on.
const sessionLifecycleChangedMethod = "session.lifecycle.changed"

// hostRPC speaks JSON-RPC 2.0 with the AoE daemon over the worker's own
// stdio — the ndjson channel the host already supervises this process
// through. send is a fire-and-forget outbound push (used for
// ui.state.set/ui.state.remove — the host's reply, if any, is never waited
// on); this worker makes no blocking outbound requests. handleInboundLine
// is the single entry point for every line this worker reads from stdin:
// since this worker declares no commands of its own, a genuine
// host->worker request gets a proper JSON-RPC method-not-found error,
// matching how a real JSON-RPC peer should behave rather than silently
// dropping it; anything else (malformed input, a notification, a reply to
// a call this worker never made) is dropped silently.
//
// The one exception to "no blocking outbound calls" is call, used for
// config.get: it parks a waiter in pending, which handleInboundLine
// completes when the matching reply arrives. Because that reply is read by
// the same loop that feeds handleInboundLine, a caller of call must never
// run on that loop itself.
type hostRPC struct {
	mu      sync.Mutex
	out     io.Writer
	nextID  int
	pending map[int]chan inboundEnvelope

	// onSettingsChanged, when set, runs on a fresh goroutine for each
	// plugin.settings.changed notification.
	onSettingsChanged func()

	// onSessionIdle, when set, is called with the session id for each
	// session.status.changed notification whose new status is Idle (the
	// agent stopped working). It runs on the stdin loop, so it must not
	// block.
	onSessionIdle func(sessionID string)

	// onSessionLifecycle, when set, is called for each
	// session.lifecycle.changed notification with its kind, session id and
	// raw summary (absent for a "deleted" event). It runs on the stdin loop,
	// so it must not block.
	onSessionLifecycle func(kind, sessionID string, summary json.RawMessage)
}

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

// kind is the host's stable error discriminator (data.kind, e.g.
// "session_archived"), or "" if the error carries none.
func (e *rpcError) kind() string {
	var d struct {
		Kind string `json:"kind"`
	}
	if len(e.Data) == 0 || json.Unmarshal(e.Data, &d) != nil {
		return ""
	}
	return d.Kind
}

// inboundEnvelope covers every shape a line on stdin can take: a genuine
// host->worker request (ID set, Method set), a notification (ID absent,
// Method set), or a reply to some other outbound message this worker
// doesn't track (ID set, Method empty) — the last of those is always
// dropped, since this worker never blocks waiting for one. Fields are
// pointers/RawMessage throughout so "absent" and "present-but-zero-value"
// stay distinguishable.
type inboundEnvelope struct {
	ID     *int            `json:"id"`
	Method string          `json:"method"`
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
	Params json.RawMessage `json:"params"`
}

// newHostRPC's id counter starts in a high range so it never collides with
// the ids the host assigns to its own requests to this worker, same
// convention as the reference plugin.
func newHostRPC(out io.Writer) *hostRPC {
	return &hostRPC{out: out, nextID: 1_000_000, pending: make(map[int]chan inboundEnvelope)}
}

// call sends a request and waits for its reply, bounded by ctx.
func (h *hostRPC) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := h.allocID()
	reply := make(chan inboundEnvelope, 1)
	h.mu.Lock()
	h.pending[id] = reply
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
	case env := <-reply:
		if env.Error != nil {
			if kind := env.Error.kind(); kind != "" {
				return nil, fmt.Errorf("%s: %s (code %d, kind %s)", method, env.Error.Message, env.Error.Code, kind)
			}
			return nil, fmt.Errorf("%s: %s (code %d)", method, env.Error.Message, env.Error.Code)
		}
		return env.Result, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("%s: %w", method, ctx.Err())
	}
}

// Call is call under an exported name, so other packages (the notifier) can
// depend on a narrow interface instead of this one.
func (h *hostRPC) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	return h.call(ctx, method, params)
}

// ListSessions implements aoeclient.Lister over the `sessions.list` RPC. No
// exclude filter: the directory needs archived and trashed sessions too, to
// tell them apart from deleted ones. Like call, it must never run on the
// stdin loop.
func (h *hostRPC) ListSessions(ctx context.Context) ([]aoeclient.Summary, error) {
	raw, err := h.call(ctx, "sessions.list", map[string]any{})
	if err != nil {
		return nil, err
	}
	return aoeclient.ParseSessionList(raw)
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

// handleInboundLine processes one line read from stdin. A genuine
// host->worker request (has a method) gets a method-not-found error reply,
// since this worker declares no commands. Malformed JSON, notifications (no
// id, has a method), and a reply to some other outbound message (no
// method, since this worker never blocks waiting for one) are all dropped
// silently.
func (h *hostRPC) handleInboundLine(line []byte) {
	var env inboundEnvelope
	if err := json.Unmarshal(line, &env); err != nil {
		return
	}

	if env.Method == "" && env.ID != nil {
		h.mu.Lock()
		reply, ok := h.pending[*env.ID]
		h.mu.Unlock()
		if ok {
			reply <- env // buffered 1; only one reply per id is ever delivered
		}
		return
	}

	if env.Method != "" {
		if env.ID == nil {
			switch {
			case env.Method == settingsChangedMethod && h.onSettingsChanged != nil:
				go h.onSettingsChanged()
			case env.Method == sessionStatusChangedMethod && h.onSessionIdle != nil:
				h.handleSessionStatusChanged(env.Params)
			case env.Method == sessionLifecycleChangedMethod && h.onSessionLifecycle != nil:
				h.handleSessionLifecycleChanged(env.Params)
			}
			return
		}
		_ = h.writeMessage(map[string]any{
			"jsonrpc": "2.0",
			"id":      *env.ID,
			"error": map[string]any{
				"code":    -32601,
				"message": fmt.Sprintf("unknown method %q", env.Method),
			},
		})
	}
}

func (h *hostRPC) handleSessionStatusChanged(params json.RawMessage) {
	var p struct {
		SessionID string `json:"session_id"`
		To        string `json:"to"`
	}
	if err := json.Unmarshal(params, &p); err != nil || p.SessionID == "" {
		return
	}
	if p.To == "Idle" {
		h.onSessionIdle(p.SessionID)
	}
}

func (h *hostRPC) handleSessionLifecycleChanged(params json.RawMessage) {
	var p struct {
		SessionID string          `json:"session_id"`
		Kind      string          `json:"kind"`
		Summary   json.RawMessage `json:"summary"`
	}
	if err := json.Unmarshal(params, &p); err != nil || p.SessionID == "" || p.Kind == "" {
		return
	}
	h.onSessionLifecycle(p.Kind, p.SessionID, p.Summary)
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

// pushSessionPane pushes one session's in-session pane state.
func (h *hostRPC) pushSessionPane(sessionID string, payload panePayload) error {
	return h.send("ui.state.set", map[string]any{
		"slot":       sessionPaneSlot,
		"id":         sessionPaneID,
		"session_id": sessionID,
		"payload":    payload,
	})
}

// rowColumnPayload is a session's row-column cell. The host requires the text
// field, but the words are the badge's job, so it stays empty: the cell exists
// to carry filter_values for the facets.
type rowColumnPayload struct {
	Text         string   `json:"text"`
	FilterValues []string `json:"filter_values,omitempty"`
}

// setRowColumn pushes a session's facet values as its row-column cell.
func (h *hostRPC) setRowColumn(sessionID string, filterValues []string) error {
	return h.send("ui.state.set", map[string]any{
		"slot":       rowColumnSlot,
		"id":         rowColumnID,
		"session_id": sessionID,
		"payload":    rowColumnPayload{FilterValues: filterValues},
	})
}

// removeRowColumn clears a session's facet values, for a session with nothing
// left to filter on.
func (h *hostRPC) removeRowColumn(sessionID string) error {
	return h.send("ui.state.remove", map[string]any{
		"slot":       rowColumnSlot,
		"id":         rowColumnID,
		"session_id": sessionID,
	})
}

// pushFilterFacet pushes one global filter facet and its options.
func (h *hostRPC) pushFilterFacet(f facetDef) error {
	return h.send("ui.state.set", map[string]any{
		"slot": filterFacetSlot,
		"id":   f.ID,
		"payload": map[string]any{
			"label":   f.Label,
			"column":  f.Column,
			"options": f.Options,
		},
	})
}

// removeSessionPane clears a session's pane entirely — same reasoning as
// removeRowBadge: nothing left to show must not mean a stale pane lingers.
func (h *hostRPC) removeSessionPane(sessionID string) error {
	return h.send("ui.state.remove", map[string]any{
		"slot":       sessionPaneSlot,
		"id":         sessionPaneID,
		"session_id": sessionID,
	})
}
