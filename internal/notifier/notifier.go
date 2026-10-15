package notifier

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// Caller makes one blocking RPC call to the AoE host. The worker's host
// channel implements it.
type Caller interface {
	Call(ctx context.Context, method string, params any) (json.RawMessage, error)
}

// Notifier delivers a message into an AoE session through the host's
// `sessions.message.send` RPC (capability `session.message`). The host
// matches what typing into the session would do: a stopped or dormant
// session is woken, a busy agent has the message steered in or queued, an
// archived or trashed session is refused. This call only happens on a real
// state-change event, not every poll tick.
type Notifier struct {
	RPC Caller
}

func (n *Notifier) Send(ctx context.Context, sessionID, message string) error {
	if n.RPC == nil {
		return errors.New("notifier: no RPC client configured")
	}
	// The message is untrusted external content (GitHub/GitLab comment,
	// reviewer name, ...). As an RPC param it is just a JSON string, so
	// unlike a CLI argument it can never be read as a flag.
	if _, err := n.RPC.Call(ctx, "sessions.message.send", map[string]any{
		"session_id": sessionID,
		"message":    message,
	}); err != nil {
		return fmt.Errorf("send to session %s: %w", sessionID, err)
	}
	return nil
}
