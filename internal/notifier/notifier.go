package notifier

import (
	"context"
	"fmt"
	"os/exec"
)

// Notifier wakes an AoE session by shelling out to the `aoe send` CLI
// rather than calling the daemon's HTTP /send endpoint directly: the CLI's
// auto-revive-on-dead-or-stopped-session behavior is explicitly documented
// (docs/cli/reference.md), while the raw HTTP endpoint's revive semantics
// are not fully spelled out. This call only happens on a real
// state-change event (not every poll tick), so the subprocess overhead is
// a non-issue.
type Notifier struct {
	AoeBinary string // defaults to "aoe" resolved on PATH
}

func (n *Notifier) binary() string {
	if n.AoeBinary != "" {
		return n.AoeBinary
	}
	return "aoe"
}

func (n *Notifier) Send(ctx context.Context, sessionID, message string) error {
	cmd := exec.CommandContext(ctx, n.binary(), "send", sessionID, message)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("aoe send %s: %w: %s", sessionID, err, string(output))
	}
	return nil
}
