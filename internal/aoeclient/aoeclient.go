package aoeclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// ResolveServeURL reads the local AoE daemon's base URL the same way the
// `aoe` CLI itself does when no --daemon-url is given: from
// ~/.config/agent-of-empires/serve.url, written by the running daemon.
func ResolveServeURL() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	path := filepath.Join(home, ".config", "agent-of-empires", "serve.url")
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	return strings.TrimSpace(string(data)), nil
}

type SessionClass int

const (
	// SessionGone covers both "never existed" and "hard-deleted": neither
	// case should be sent to, and a registration pointing at one should be
	// dropped.
	SessionGone SessionClass = iota
	SessionLive
	SessionArchived
)

type Client struct {
	BaseURL    string
	HTTPClient *http.Client

	live     map[string]bool
	archived map[string]bool
	known    map[string]bool
}

// New resolves the daemon's base URL via ResolveServeURL and returns a
// ready-to-use Client. Call RefreshSessionSets before the first Classify.
func New() (*Client, error) {
	base, err := ResolveServeURL()
	if err != nil {
		return nil, err
	}
	return &Client{BaseURL: strings.TrimRight(base, "/"), HTTPClient: http.DefaultClient}, nil
}

type sessionRow struct {
	ID string `json:"id"`
}

func (c *Client) fetchIDs(ctx context.Context, state string) (map[string]bool, error) {
	url := strings.TrimRight(c.BaseURL, "/") + "/api/sessions"
	if state != "" {
		url += "?state=" + state
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("GET %s: unexpected status %d", url, resp.StatusCode)
	}

	var rows []sessionRow
	if err := json.NewDecoder(resp.Body).Decode(&rows); err != nil {
		return nil, fmt.Errorf("decode %s response: %w", url, err)
	}
	ids := make(map[string]bool, len(rows))
	for _, r := range rows {
		ids[r.ID] = true
	}
	return ids, nil
}

// RefreshSessionSets fetches the live, trashed, and all-known id sets once
// (three calls per scheduler cycle, not per registration) and caches them
// for subsequent Classify calls. A session in "all" but neither "live" nor
// "trashed" is archived — the documented state-filter semantics give us
// this without guessing at an undocumented per-row archived field.
func (c *Client) RefreshSessionSets(ctx context.Context) error {
	live, err := c.fetchIDs(ctx, "live")
	if err != nil {
		return err
	}
	trashed, err := c.fetchIDs(ctx, "trashed")
	if err != nil {
		return err
	}
	all, err := c.fetchIDs(ctx, "")
	if err != nil {
		return err
	}

	archived := make(map[string]bool)
	for id := range all {
		if !live[id] && !trashed[id] {
			archived[id] = true
		}
	}

	c.live = live
	c.archived = archived
	c.known = all
	return nil
}

// Classify reports a session's lifecycle state as of the last
// RefreshSessionSets call. An id absent from the last-fetched "all" set is
// SessionGone (hard-deleted or never existed).
func (c *Client) Classify(id string) SessionClass {
	if c.live[id] {
		return SessionLive
	}
	if c.archived[id] {
		return SessionArchived
	}
	return SessionGone
}
