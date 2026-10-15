package main

import (
	"io"
	"log/slog"
	"strings"
	"testing"
)

// A sessions.list reply for a few hundred sessions is far past bufio's
// default 64KB line limit; the loop must deliver it whole instead of dying.
func TestServeStdinDeliversLinesLargerThanDefaultScannerLimit(t *testing.T) {
	big := `{"id":1,"result":"` + strings.Repeat("x", 512*1024) + `"}`
	input := big + "\n" + `{"id":2}` + "\n"

	var got []int
	serveStdin(strings.NewReader(input), func(line []byte) { got = append(got, len(line)) }, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if len(got) != 2 {
		t.Fatalf("delivered %d lines, want 2 (lengths %v)", len(got), got)
	}
	if got[0] != len(big) {
		t.Errorf("first line length = %d, want %d", got[0], len(big))
	}
}
