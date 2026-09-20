package aoeclient

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestResolveServeURLReadsConfigFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	configDir := filepath.Join(dir, ".config", "agent-of-empires")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "serve.url"), []byte("http://127.0.0.1:25808/\n"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	got, err := ResolveServeURL()
	if err != nil {
		t.Fatalf("ResolveServeURL() error = %v", err)
	}
	if got != "http://127.0.0.1:25808/" {
		t.Errorf("ResolveServeURL() = %q, want %q", got, "http://127.0.0.1:25808/")
	}
}

func TestResolveServeURLMissingFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if _, err := ResolveServeURL(); err == nil {
		t.Error("ResolveServeURL() with no serve.url file, want error")
	}
}

func sessionsHandler(live, trashed, all []string) http.HandlerFunc {
	toRows := func(ids []string) []map[string]any {
		rows := make([]map[string]any, len(ids))
		for i, id := range ids {
			rows[i] = map[string]any{"id": id}
		}
		return rows
	}
	return func(w http.ResponseWriter, r *http.Request) {
		var rows []map[string]any
		switch r.URL.Query().Get("state") {
		case "live":
			rows = toRows(live)
		case "trashed":
			rows = toRows(trashed)
		case "all", "":
			rows = toRows(all)
		}
		json.NewEncoder(w).Encode(rows)
	}
}

func TestClassifyLiveArchivedGone(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/sessions", sessionsHandler(
		[]string{"live1"},
		[]string{"trashed1"},
		[]string{"live1", "trashed1", "archived1"}, // archived1 is known but neither live nor trashed
	))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, HTTPClient: srv.Client()}
	if err := c.RefreshSessionSets(t.Context()); err != nil {
		t.Fatalf("RefreshSessionSets() error = %v", err)
	}

	cases := map[string]SessionClass{
		"live1":     SessionLive,
		"trashed1":  SessionGone,
		"archived1": SessionArchived,
		"unknown1":  SessionGone,
	}
	for id, want := range cases {
		if got := c.Classify(id); got != want {
			t.Errorf("Classify(%q) = %v, want %v", id, got, want)
		}
	}
}
