package main

import (
	"encoding/json"
	"log/slog"

	"github.com/cwrau/aoe-plugin-pr-watch-mcp/internal/aoeclient"
)

// lifecycleHandler folds the host's session.lifecycle.changed notifications
// into the session directory, so Classify, Title and the discovery
// workspaces stay current between the periodic full listings. A "deleted"
// event carries no summary; every other kind carries the session's current
// summary, which replaces what the directory holds. An unparseable summary is
// logged and dropped — the next resync repairs the directory.
func lifecycleHandler(c *aoeclient.Client, logger *slog.Logger) func(kind, sessionID string, summary json.RawMessage) {
	return func(kind, sessionID string, raw json.RawMessage) {
		if len(raw) == 0 {
			c.ApplyLifecycle(kind, sessionID, nil)
			return
		}
		s, err := aoeclient.ParseSummary(raw)
		if err != nil {
			logger.Warn("ignoring session lifecycle event with an unparseable summary", "kind", kind, "session_id", sessionID, "error", err)
			return
		}
		c.ApplyLifecycle(kind, sessionID, &s)
	}
}
