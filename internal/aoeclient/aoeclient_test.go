package aoeclient

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
)

// fakeAoeBinary writes a shell script standing in for the real `aoe` CLI
// that prints stdout verbatim and exits with exitCode, so RefreshSessionSets
// can be exercised without a real AoE daemon. Same pattern as
// internal/notifier's fakeAoeBinary.
func fakeAoeBinary(t *testing.T, stdout string, exitCode int) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake binary script is posix-shell only")
	}
	binPath := filepath.Join(t.TempDir(), "aoe")
	script := "#!/bin/sh\ncat <<'EOF'\n" + stdout + "\nEOF\nexit " + strconv.Itoa(exitCode) + "\n"
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	return binPath
}

func TestRefreshSessionSetsClassifiesAllThreeStates(t *testing.T) {
	bin := fakeAoeBinary(t, `[{"id":"a","state":"live"},{"id":"b","state":"archived"},{"id":"c","state":"trashed"}]`, 0)
	c := &Client{AoeBinary: bin}

	if err := c.RefreshSessionSets(t.Context()); err != nil {
		t.Fatalf("RefreshSessionSets() error = %v", err)
	}

	cases := map[string]SessionClass{
		"a":       SessionLive,
		"b":       SessionArchived,
		"c":       SessionGone, // trashed folds into Gone: don't send, drop the registration
		"unknown": SessionGone,
	}
	for id, want := range cases {
		if got := c.Classify(id); got != want {
			t.Errorf("Classify(%q) = %v, want %v", id, got, want)
		}
	}
}

func TestRefreshSessionSetsFailsOnNonZeroExit(t *testing.T) {
	bin := fakeAoeBinary(t, "", 1)
	c := &Client{AoeBinary: bin}

	if err := c.RefreshSessionSets(t.Context()); err == nil {
		t.Error("RefreshSessionSets() with a failing aoe binary, want error")
	}
}

func TestRefreshSessionSetsFailsOnMalformedJSON(t *testing.T) {
	bin := fakeAoeBinary(t, "not json at all", 0)
	c := &Client{AoeBinary: bin}

	if err := c.RefreshSessionSets(t.Context()); err == nil {
		t.Error("RefreshSessionSets() with malformed JSON output, want error")
	}
}

func TestNewReturnsUsableClient(t *testing.T) {
	c, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if c.binary() != "aoe" {
		t.Errorf("binary() = %q, want %q", c.binary(), "aoe")
	}
	// Classify before any refresh must not panic on the nil map.
	if got := c.Classify("anything"); got != SessionGone {
		t.Errorf("Classify() before refresh = %v, want SessionGone", got)
	}
}
