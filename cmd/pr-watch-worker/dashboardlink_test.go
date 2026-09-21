package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveDashboardBaseURLReadsConfigFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	configDir := filepath.Join(dir, ".config", "agent-of-empires")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "serve.url"), []byte("http://127.0.0.1:25808/\n"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	got := resolveDashboardBaseURL()
	if got != "http://127.0.0.1:25808/" {
		t.Errorf("resolveDashboardBaseURL() = %q, want %q", got, "http://127.0.0.1:25808/")
	}
}

func TestResolveDashboardBaseURLMissingFileReturnsEmpty(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if got := resolveDashboardBaseURL(); got != "" {
		t.Errorf("resolveDashboardBaseURL() with no serve.url file = %q, want empty (best-effort, not fatal)", got)
	}
}
