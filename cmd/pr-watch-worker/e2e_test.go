package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestWorkerSurvivesAHugeSessionsListReply runs the real worker binary against
// a fake AoE host speaking ndjson over its stdio: a sessions.list reply far
// past bufio's 64KB line limit already killed the worker once, and only a real
// process on real pipes shows that the loop, the RPC plumbing and the UI push
// still work together.
func TestWorkerSurvivesAHugeSessionsListReply(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the worker binary")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "worker")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Skipf("go build failed (%v): %s", err, out)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("no loopback listener: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	cmd := exec.Command(bin)
	cmd.Env = []string{
		"HOME=" + dir,
		"XDG_STATE_HOME=" + filepath.Join(dir, "state"),
		"PATH=/nonexistent",
		fmt.Sprintf("PR_WATCH_MCP_PORT=%d", port),
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	var big []map[string]any
	for i := range 1500 {
		big = append(big, map[string]any{"id": fmt.Sprintf("s%04d", i), "title": strings.Repeat("t", 600), "project_path": "/p"})
	}
	bigReply, _ := json.Marshal(map[string]any{"sessions": big})
	if len(bigReply) < 512<<10 {
		t.Fatalf("test reply is only %d bytes, want a line well past 64KB", len(bigReply))
	}

	reply := func(id any, result json.RawMessage) {
		line, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
		_, _ = stdin.Write(append(line, '\n'))
	}
	uiPush := make(chan struct{}, 1)
	go func() {
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 0, 64<<10), 64<<20)
		for sc.Scan() {
			var msg struct {
				ID     any    `json:"id"`
				Method string `json:"method"`
			}
			if json.Unmarshal(sc.Bytes(), &msg) != nil || msg.Method == "" || msg.ID == nil {
				continue
			}
			switch msg.Method {
			case "sessions.list":
				reply(msg.ID, bigReply)
			case "ui.state.set":
				select {
				case uiPush <- struct{}{}:
				default:
				}
				reply(msg.ID, json.RawMessage(`{}`))
			default:
				reply(msg.ID, json.RawMessage(`{}`))
			}
		}
	}()

	select {
	case <-uiPush:
	case err := <-exited:
		t.Fatalf("worker exited early after the big reply: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("worker never pushed UI state after the big sessions.list reply")
	}

	req, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d/", port), strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("MCP server not reachable: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("MCP initialize status = %d, want 200", resp.StatusCode)
	}

	_ = stdin.Close() // the host's shutdown signal
	select {
	case err := <-exited:
		if err != nil {
			t.Errorf("worker exit = %v, want a clean exit on stdin EOF", err)
		}
	case <-time.After(15 * time.Second):
		t.Error("worker did not exit after stdin closed")
	}
	_ = os.Remove(bin)
}
