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

	// httpTimeout bounds every provider API call. Without it a half-open
	// connection to GitHub/GitLab would hang a Tick indefinitely.
	httpTimeout = 30 * time.Second

	// tickTimeout bounds one whole scheduler pass. A normal tick is far
	// shorter; this only exists so a pathological one can't wedge the loop
	// forever.
	tickTimeout = 60 * time.Second

	// tokenTimeout bounds a credential-helper subprocess (`gh auth token`,
	// `glab config get token`), which can block on a stuck keyring or dbus
	// backend.
	tokenTimeout = 10 * time.Second

	// panePushTimeout bounds one home-pane push, including its config.get
	// round trip for dashboard_base_url. pushHomePane runs synchronously in
	// the scheduler loop (once at startup, once after every tick), so an
	// unbounded wait here — e.g. a host that never replies — would stall
	// the entire loop, not just the pane feature.
	panePushTimeout = 10 * time.Second
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
		logger.Error("create aoe session client", "error", err)
		os.Exit(1)
	}

	tokenCtx, cancelToken := context.WithTimeout(context.Background(), tokenTimeout)
	ghToken, err := runToken(tokenCtx, "gh", "auth", "token")
	cancelToken()
	if err != nil {
		logger.Warn("gh auth token unavailable at startup; GitHub checks will fail until it is", "error", err)
	}

	// One shared client for both checkers, with an explicit timeout:
	// http.DefaultClient has none, so a stalled provider connection would
	// block a Tick forever.
	httpClient := &http.Client{Timeout: httpTimeout}

	sched := &scheduler.Scheduler{
		Store: store,
		GitHub: &provider.GitHubChecker{
			HTTPClient: httpClient,
			Token:      ghToken,
		},
		GitLab: &provider.GitLabChecker{
			HTTPClient: httpClient,
			// Resolved fresh per-host at check time, not once globally:
			// see provider.GitLabChecker.TokenFunc's doc comment for why
			// a single static token is a credential leak across hosts.
			// Check's context is passed straight through so the glab fork
			// is bounded by the same deadline as the poll it serves.
			TokenFunc: func(ctx context.Context, host string) (string, error) {
				return runToken(ctx, "glab", "config", "get", "token", "--host", host)
			},
		},
		Sessions: aoe,
		Notify:   &notifier.Notifier{},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rpc := newHostRPC(os.Stdout)

	go runSchedulerLoop(ctx, sched, store, aoe, rpc, logger)
	go runHTTPServer(ctx, store, logger)

	// The ndjson stdio channel is how the AoE daemon supervises this
	// worker, and also how it answers this worker's own outbound calls
	// (config.get for settings). Every line is handed to
	// rpc.handleInboundLine, which delivers replies to whichever call is
	// waiting on them; EOF is still the host's shutdown signal, same as
	// the reference agent-of-empires.github worker's read loop.
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		rpc.handleInboundLine(scanner.Bytes())
	}
	logger.Info("stdin closed, shutting down")
}

func runSchedulerLoop(ctx context.Context, sched *scheduler.Scheduler, store *registry.Store, aoe *aoeclient.Client, rpc *hostRPC, logger *slog.Logger) {
	// Push once at startup so the home-pane and row-badges aren't blank
	// until the first tick completes (up to tickInterval later). Session
	// titles may still be unresolved at this point (RefreshSessionSets
	// hasn't run yet), so rows briefly fall back to raw session ids until
	// the first tick fills them in.
	pushUIState(ctx, store, aoe, rpc, logger)

	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Bound each pass: ctx alone is only cancelled at shutdown, so
			// without a deadline a single wedged tick would stop the loop
			// from ever running again.
			tickCtx, cancel := context.WithTimeout(ctx, tickTimeout)
			err := sched.Tick(tickCtx)
			cancel()
			if err != nil {
				logger.Error("scheduler tick", "error", err)
			}
			pushUIState(ctx, store, aoe, rpc, logger)
		}
	}
}

// pushUIState renders and pushes both UI slots this worker owns — the
// global home-pane and each session's row-badge — from one consistent
// snapshot of the registry. Because registrations are never deleted (see
// registry.Store.Add's doc comment), the set of session ids that have ever
// registered anything is itself durable state, readable fresh from the
// store every call — so reconciliation (deciding which sessions should have
// a badge right now, and issuing ui.state.remove for the rest) needs no
// in-memory bookkeeping across calls and is safe across a worker restart:
// a badgeSessions parameter tracked in memory would silently go stale the
// moment the worker restarted mid-way through a session's watch lifecycle.
func pushUIState(ctx context.Context, store *registry.Store, aoe *aoeclient.Client, rpc *hostRPC, logger *slog.Logger) {
	pushCtx, cancel := context.WithTimeout(ctx, panePushTimeout)
	defer cancel()

	regs, err := store.List(pushCtx)
	if err != nil {
		logger.Warn("list registrations for UI push", "error", err)
		return
	}

	baseURL := rpc.getStringSetting(pushCtx, dashboardBaseURLSettingKey, "")
	if err := rpc.pushHomePane(buildHomePanePayload(regs, aoe.Title, baseURL)); err != nil {
		logger.Warn("push home pane", "error", err)
	}

	badges := buildRowBadgePayloads(regs)
	allSessions := make(map[string]bool, len(regs))
	for _, r := range regs {
		allSessions[r.SessionID] = true
	}
	for sessionID := range allSessions {
		if payload, ok := badges[sessionID]; ok {
			if err := rpc.setRowBadge(sessionID, payload); err != nil {
				logger.Warn("set row badge", "session_id", sessionID, "error", err)
			}
		} else if err := rpc.removeRowBadge(sessionID); err != nil {
			logger.Warn("remove row badge", "session_id", sessionID, "error", err)
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

// runToken shells out to a CLI's token-printing subcommand: once at startup
// for the GitHub token, and once per GitLab poll for the GitLab token (see
// provider.GitLabChecker.TokenFunc — it resolves per-host at check time, so
// every GitLab registration's every poll forks one `glab`). That puts it in
// the scheduler's hot path, which is why it runs under the caller's context
// rather than unbounded: a stuck keyring backend would otherwise hang the
// whole scheduler, not just one registration. The alternative (parsing gh's
// /glab's own credential storage directly) is far more fragile across
// keyring backends, so the subprocess stays.
func runToken(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).Output()
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
