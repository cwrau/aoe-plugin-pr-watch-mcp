package aoeclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

type SessionClass int

const (
	// SessionGone covers both "never existed" and "hard-deleted": neither
	// case should be sent to, and a registration pointing at one should be
	// dropped.
	SessionGone SessionClass = iota
	SessionLive
	SessionArchived
)

// Client classifies AoE sessions by shelling out to the `aoe` CLI rather
// than calling the daemon's HTTP API directly. The daemon can be run with
// `--auth passphrase`, in which case the public HTTP API rejects every
// unauthenticated request (401 "Passphrase login required") and there is no
// static bearer token a headless worker could present — only an interactive
// browser login would do. The local CLI has a local-trust path the raw HTTP
// API does not grant, so it works with no login at all. Same
// "shell out to the trusted local CLI" pattern as the notifier.
type Client struct {
	AoeBinary string // defaults to "aoe" resolved on PATH; overridable for tests

	classes map[string]SessionClass
}

// New returns a ready-to-use Client. Call RefreshSessionSets before the
// first Classify.
func New() (*Client, error) {
	return &Client{}, nil
}

func (c *Client) binary() string {
	if c.AoeBinary != "" {
		return c.AoeBinary
	}
	return "aoe"
}

type sessionRow struct {
	ID    string `json:"id"`
	State string `json:"state"`
}

// RefreshSessionSets runs `aoe list --json --all` once (one call per
// scheduler cycle, not per registration) and caches the resulting id ->
// class mapping for subsequent Classify calls. The CLI reports an exact
// per-row state — "live", "archived" or "trashed" — so no set-difference
// inference is needed. Trashed ids are deliberately left out of the map so
// Classify falls through to SessionGone for them: a trashed session is not
// sent to and its registration is dropped, identical to an id the daemon
// no longer knows about at all.
func (c *Client) RefreshSessionSets(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, c.binary(), "list", "--json", "--all")
	out, err := cmd.Output()
	if err != nil {
		var stderr string
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			stderr = strings.TrimSpace(string(exitErr.Stderr))
		}
		if stderr != "" {
			return fmt.Errorf("%s list --json --all: %w: %s", c.binary(), err, stderr)
		}
		return fmt.Errorf("%s list --json --all: %w", c.binary(), err)
	}

	var rows []sessionRow
	if err := json.Unmarshal(out, &rows); err != nil {
		return fmt.Errorf("decode %s list --json --all output: %w", c.binary(), err)
	}

	classes := make(map[string]SessionClass, len(rows))
	for _, r := range rows {
		switch r.State {
		case "live":
			classes[r.ID] = SessionLive
		case "archived":
			classes[r.ID] = SessionArchived
		}
	}
	c.classes = classes
	return nil
}

// Classify reports a session's lifecycle state as of the last
// RefreshSessionSets call. An id absent from that snapshot is SessionGone
// (trashed, hard-deleted, or never existed).
func (c *Client) Classify(id string) SessionClass {
	if cls, ok := c.classes[id]; ok {
		return cls
	}
	return SessionGone
}
