package main

import (
	"os"
	"path/filepath"
	"strings"
)

// resolveDashboardBaseURL reads the local AoE daemon's base URL from
// ~/.config/agent-of-empires/serve.url, purely to build an absolute
// session-link href for the home pane — a plain, unauthenticated local
// file read, not an API call (unlike the daemon's authenticated HTTP API,
// which aoeclient deliberately does not use). Returns "" on any failure
// (missing file, unreadable home dir): the home-pane push is best-effort,
// so a session row simply renders without a link rather than the whole
// pane push failing.
func resolveDashboardBaseURL() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(home, ".config", "agent-of-empires", "serve.url"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}
