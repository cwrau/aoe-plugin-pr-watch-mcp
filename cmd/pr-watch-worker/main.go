package main

import (
	"bufio"
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"github.com/cwrau/aoe-pr-watch-mcp/internal/aoeclient"
	"github.com/cwrau/aoe-pr-watch-mcp/internal/mcpserver"
	"github.com/cwrau/aoe-pr-watch-mcp/internal/notifier"
	"github.com/cwrau/aoe-pr-watch-mcp/internal/provider"
	"github.com/cwrau/aoe-pr-watch-mcp/internal/registry"
	"github.com/cwrau/aoe-pr-watch-mcp/internal/scheduler"
)

const (
	defaultPort  = 8632
	tickInterval = 15 * time.Second
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	slog.SetDefault(logger)

	dataDir, err := os.UserConfigDir()
	if err != nil {
		logger.Error("resolve config dir", "error", err)
		os.Exit(1)
	}
	dataDir = filepath.Join(dataDir, "pr-watch-mcp")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		logger.Error("create data dir", "error", err)
		os.Exit(1)
	}

	store, err := registry.Open(filepath.Join(dataDir, "registry.db"))
	if err != nil {
		logger.Error("open registry", "error", err)
		os.Exit(1)
	}
	defer store.Close()

	aoe, err := aoeclient.New()
	if err != nil {
		logger.Error("resolve aoe daemon URL", "error", err)
		os.Exit(1)
	}

	ghToken, err := runToken("gh", "auth", "token")
	if err != nil {
		logger.Warn("gh auth token unavailable at startup; GitHub checks will fail until it is", "error", err)
	}

	sched := &scheduler.Scheduler{
		Store: store,
		GitHub: &provider.GitHubChecker{
			HTTPClient: http.DefaultClient,
			Token:      ghToken,
		},
		GitLab: &provider.GitLabChecker{
			HTTPClient: http.DefaultClient,
			Token:      mustGitLabToken(logger),
		},
		Sessions: aoe,
		Notify:   &notifier.Notifier{},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go runSchedulerLoop(ctx, sched, logger)
	go runHTTPServer(ctx, store, logger)

	// The ndjson stdio channel is how the AoE daemon supervises this
	// worker: it never sends us anything we need to act on (we declare no
	// commands), so this loop's only job is to notice stdin EOF, the host's
	// shutdown signal, same as the reference agent-of-empires.github
	// worker's read loop.
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		// No inbound RPCs to dispatch; drain and ignore.
	}
	logger.Info("stdin closed, shutting down")
}

func runSchedulerLoop(ctx context.Context, sched *scheduler.Scheduler, logger *slog.Logger) {
	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := sched.Tick(ctx); err != nil {
				logger.Error("scheduler tick", "error", err)
			}
		}
	}
}

func runHTTPServer(ctx context.Context, store *registry.Store, logger *slog.Logger) {
	port := defaultPort
	if raw := os.Getenv("PR_WATCH_MCP_PORT"); raw != "" {
		if p, err := strconv.Atoi(raw); err == nil {
			port = p
		}
	}
	srv := &http.Server{
		Addr:    "127.0.0.1:" + strconv.Itoa(port),
		Handler: mcpserver.NewHandler(store),
	}
	go func() {
		<-ctx.Done()
		srv.Close()
	}()
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Error("mcp http server", "error", err)
	}
}

// runToken shells out to a CLI's token-printing subcommand once at
// startup. Low frequency (not per-poll), so subprocess overhead doesn't
// matter; the alternative (parsing gh's/glab's own credential storage
// directly) is far more fragile across keyring backends.
func runToken(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).Output()
	if err != nil {
		return "", err
	}
	return string(bytesTrimSpace(out)), nil
}

func bytesTrimSpace(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r' || b[len(b)-1] == ' ') {
		b = b[:len(b)-1]
	}
	return b
}

func mustGitLabToken(logger *slog.Logger) string {
	token, err := runToken("glab", "config", "get", "token")
	if err != nil {
		logger.Warn("glab config get token unavailable at startup; GitLab checks will fail until it is", "error", err)
		return ""
	}
	return token
}
