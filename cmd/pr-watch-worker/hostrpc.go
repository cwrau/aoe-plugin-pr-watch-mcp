package main

import (
	"encoding/json"
	"fmt"
	"io"
	"sync"
)

// hostRPC sends outbound JSON-RPC 2.0 requests to the AoE daemon over the
// worker's own stdout — the ndjson-stdio channel the host already
// supervises this process through. Pushes are fire-and-forget: the host's
// reply (if any) is never correlated or waited on, matching the reference
// agent-of-empires.github worker's ui.state.set calls. Guarded by a mutex
// because the scheduler loop and (in the future) any other caller must
// never interleave partial JSON lines on the shared stdout stream.
type hostRPC struct {
	mu     sync.Mutex
	out    io.Writer
	nextID int
}

// newHostRPC's id counter starts in a high range so it never collides with
// the ids the host assigns to its own requests to this worker, same
// convention as the reference plugin.
func newHostRPC(out io.Writer) *hostRPC {
	return &hostRPC{out: out, nextID: 1_000_000}
}

func (h *hostRPC) send(method string, params any) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.nextID++
	data, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      h.nextID,
		"method":  method,
		"params":  params,
	})
	if err != nil {
		return fmt.Errorf("marshal %s: %w", method, err)
	}
	data = append(data, '\n')
	if _, err := h.out.Write(data); err != nil {
		return fmt.Errorf("write %s: %w", method, err)
	}
	return nil
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
