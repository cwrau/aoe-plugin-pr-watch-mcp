package notifier

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeAoeBinary writes a shell script standing in for the real `aoe` CLI
// that records its argv to a file, so the test can assert on exactly what
// Send invoked without needing a real AoE daemon.
func fakeAoeBinary(t *testing.T, exitCode int) (binPath, logPath string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake binary script is posix-shell only")
	}
	dir := t.TempDir()
	binPath = filepath.Join(dir, "aoe")
	logPath = filepath.Join(dir, "argv.log")
	script := "#!/bin/sh\necho \"$@\" > " + logPath + "\nexit " + itoa(exitCode) + "\n"
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	return binPath, logPath
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	if neg {
		return "-" + string(digits)
	}
	return string(digits)
}

func TestSendInvokesAoeSendWithSessionAndMessage(t *testing.T) {
	bin, log := fakeAoeBinary(t, 0)
	n := &Notifier{AoeBinary: bin}

	if err := n.Send(t.Context(), "abc123", "CI failure"); err != nil {
		t.Fatalf("Send() error = %v", err)
	}

	got, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("ReadFile(argv.log) error = %v", err)
	}
	argv := strings.TrimSpace(string(got))
	if argv != "send abc123 CI failure" {
		t.Errorf("aoe invoked with argv %q, want %q", argv, "send abc123 CI failure")
	}
}

func TestSendReturnsErrorOnNonZeroExit(t *testing.T) {
	bin, _ := fakeAoeBinary(t, 1)
	n := &Notifier{AoeBinary: bin}

	if err := n.Send(t.Context(), "abc123", "CI failure"); err == nil {
		t.Error("Send() with a failing aoe binary, want error")
	}
}
