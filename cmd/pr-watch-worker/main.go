package main

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"github.com/cwrau/aoe-plugin-pr-watch-mcp/internal/aoeclient"
	"github.com/cwrau/aoe-plugin-pr-watch-mcp/internal/mcpserver"
	"github.com/cwrau/aoe-plugin-pr-watch-mcp/internal/notifier"
	"github.com/cwrau/aoe-plugin-pr-watch-mcp/internal/provider"
	"github.com/cwrau/aoe-plugin-pr-watch-mcp/internal/registry"
	"github.com/cwrau/aoe-plugin-pr-watch-mcp/internal/scheduler"
)

const (
	defaultPort  = 8632
	tickInterval = 15 * time.Second

	// discoveryPollInterval is the slow fallback for the full auto-discovery
	// pass (git reads for every live checkout plus a probe of every
	// unresolved one). The fast path is event-driven: AoE reports a session
	// going idle and discoveryDebounce later that session alone gets a
	// DiscoverSessions pass. The periodic pass only has to catch what no
	// event covers — a worker that was down when the event fired (events
	// aren't replayed), or a PR opened outside any session's own turn.
	discoveryPollInterval = 5 * time.Minute

	// discoveryDebounce coalesces a burst of idle events (an agent flapping
	// idle/running, several sessions finishing together) into one pass.
	discoveryDebounce = 3 * time.Second

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

	// panePushTimeout bounds one full UI push (home-pane, row-badges, and
	// session panes). pushUIState runs synchronously in the scheduler loop
	// (once at startup, once after every tick), so an unbounded wait here —
	// e.g. a host that never replies — would stall the entire loop, not
	// just the pane feature.
	panePushTimeout = 10 * time.Second
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	slog.SetDefault(logger)

	dataDir, err := stateDir()
	if err != nil {
		logger.Error("resolve state dir", "error", err)
		os.Exit(1)
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		logger.Error("create data dir", "error", err)
		os.Exit(1)
	}
	// The registry used to live under the config dir; move it once so
	// existing watches and badges survive the relocation.
	if configDir, cerr := os.UserConfigDir(); cerr == nil {
		migrated, merr := migrateLegacyRegistry(filepath.Join(configDir, "pr-watch-mcp"), dataDir)
		switch {
		case merr != nil:
			logger.Error("migrate legacy registry", "error", merr)
			os.Exit(1)
		case migrated:
			logger.Info("moved registry from config dir to state dir", "to", dataDir)
		}
	}

	store, err := registry.Open(filepath.Join(dataDir, registryFileName))
	if err != nil {
		logger.Error("open registry", "error", err)
		os.Exit(1)
	}
	defer func() { _ = store.Close() }()

	// The host channel is created up front: the session directory lists
	// sessions over it and the notifier delivers pings through it. It is
	// goroutine-safe, and so is the directory — lifecycle notifications
	// update it from the stdin loop while the scheduler loop and the MCP
	// handler read it.
	rpc := newHostRPC(os.Stdout)
	aoe := aoeclient.New(rpc)

	tokenCtx, cancelToken := context.WithTimeout(context.Background(), tokenTimeout)
	ghToken, err := runToken(tokenCtx, "gh", "auth", "token")
	cancelToken()
	if err != nil {
		logger.Warn("gh auth token unavailable at startup; it is asked for again on the next GitHub request", "error", err)
	}

	// One shared client for both checkers, with an explicit timeout:
	// http.DefaultClient has none, so a stalled provider connection would
	// block a Tick forever.
	httpClient := &http.Client{Timeout: httpTimeout}

	// Shared with the MCP handler's dismiss_review tool below, not just the
	// scheduler: both need the same GitHub credentials, and there's no
	// reason to hold two separate token/client pairs for one API.
	ghChecker := &provider.GitHubChecker{
		HTTPClient: httpClient,
		// The startup token is used as is. If there was none (gh not logged
		// in yet), or GitHub rejects it, the next request asks gh again.
		Token: ghToken,
		TokenFunc: func(ctx context.Context) (string, error) {
			ctx, cancel := context.WithTimeout(ctx, tokenTimeout)
			defer cancel()
			return runToken(ctx, "gh", "auth", "token")
		},
	}

	// Resolved fresh per-host at check time, not once globally: see
	// provider.GitLabChecker.TokenFunc's doc comment for why a single
	// static token is a credential leak across hosts. Shared with
	// gitRemoteResolver below, which uses the exact same "does a
	// credential resolve for this host" probe to decide whether an
	// unrecognized workspace-checkout remote is a configured GitLab
	// instance worth auto-watching at all.
	glabTokenFunc := func(ctx context.Context, host string) (string, error) {
		return runToken(ctx, "glab", "config", "get", "token", "--host", host)
	}

	snooze := &snoozeRules{}
	filters := &commentFilters{}

	sched := &scheduler.Scheduler{
		Snooze:         snooze.get,
		CommentFilters: filters.get,
		Store:          store,
		GitHub:         ghChecker,
		GitLab: &provider.GitLabChecker{
			HTTPClient: httpClient,
			// Check's context is passed straight through so the glab fork
			// is bounded by the same deadline as the poll it serves.
			TokenFunc: glabTokenFunc,
		},
		Sessions: aoe,
		Notify:   &notifier.Notifier{RPC: rpc},
		// Sessions and Workspaces read the same directory the MCP handler
		// uses; it is safe for concurrent use.
		Workspaces:        aoe,
		Remotes:           &gitRemoteResolver{glabTokenFunc: glabTokenFunc},
		DiscoveryInterval: discoveryPollInterval,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	refreshSettings := func() {
		snooze.refresh(ctx, rpc, logger)
		filters.refresh(ctx, rpc, logger)
	}
	rpc.onSettingsChanged = refreshSettings
	discovery := newDiscoveryTrigger(discoveryDebounce)
	rpc.onSessionIdle = discovery.Add
	rpc.onSessionLifecycle = lifecycleHandler(aoe, logger)

	// Both read via config.get, whose reply the stdin loop below delivers,
	// so they must run off that loop.
	go func() {
		refreshSettings()
		ticker := time.NewTicker(snoozeRefreshEvery)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				refreshSettings()
			}
		}
	}()

	go runSchedulerLoop(ctx, sched, store, aoe, rpc, discovery, logger)
	go runHTTPServer(ctx, store, ghChecker, aoe, rpc, logger)

	// The ndjson stdio channel is how the AoE daemon supervises this
	// worker. Every line is handed to rpc.handleInboundLine, which only
	// ever needs to answer a genuine host->worker request (this worker
	// makes no blocking outbound calls of its own); EOF is still the
	// host's shutdown signal, same as the reference agent-of-empires.github
	// worker's read loop.
	serveStdin(os.Stdin, rpc.handleInboundLine, logger)
	logger.Info("stdin closed, shutting down")
}

// maxInboundLine bounds one ndjson line from the host. A sessions.list reply
// carries a summary per session (hundreds of KB for a busy host), far past
// bufio.Scanner's 64KB default, which would end the loop and with it the
// worker.
const maxInboundLine = 64 << 20

// serveStdin hands every line from r to handle until EOF or a read error.
func serveStdin(r io.Reader, handle func([]byte), logger *slog.Logger) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64<<10), maxInboundLine)
	for scanner.Scan() {
		handle(scanner.Bytes())
	}
	if err := scanner.Err(); err != nil {
		logger.Error("read stdin", "error", err)
	}
}

func runSchedulerLoop(ctx context.Context, sched *scheduler.Scheduler, store *registry.Store, aoe *aoeclient.Client, rpc *hostRPC, discovery *discoveryTrigger, logger *slog.Logger) {
	// Load the session directory once up front (this blocks on the host's
	// reply, which the stdin loop delivers), then push so the home-pane and
	// row-badges aren't blank until the first tick completes. If the
	// listing fails, titles fall back to raw session ids and the first tick
	// retries it.
	if err := aoe.RefreshIfStale(ctx); err != nil {
		logger.Warn("initial session listing failed", "error", err)
	}
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
		case <-discovery.C:
			ids := discovery.Drain()
			if len(ids) == 0 {
				continue
			}
			tickCtx, cancel := context.WithTimeout(ctx, tickTimeout)
			err := sched.DiscoverSessions(tickCtx, ids)
			cancel()
			if err != nil {
				logger.Error("session discovery", "sessions", ids, "error", err)
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

	if err := rpc.pushHomePane(buildHomePanePayload(regs, aoe.Title)); err != nil {
		logger.Warn("push home pane", "error", err)
	}

	badges := buildRowBadgePayloads(regs)
	regsBySession := make(map[string][]registry.Registration, len(regs))
	allSessions := make(map[string]bool, len(regs))
	for _, r := range regs {
		allSessions[r.SessionID] = true
		regsBySession[r.SessionID] = append(regsBySession[r.SessionID], r)
	}
	for sessionID := range allSessions {
		if payload, ok := badges[sessionID]; ok {
			if err := rpc.setRowBadge(sessionID, payload); err != nil {
				logger.Warn("set row badge", "session_id", sessionID, "error", err)
			}
		} else if err := rpc.removeRowBadge(sessionID); err != nil {
			logger.Warn("remove row badge", "session_id", sessionID, "error", err)
		}

		if payload, ok := buildSessionPanePayload(regsBySession[sessionID]); ok {
			if err := rpc.pushSessionPane(sessionID, payload); err != nil {
				logger.Warn("push session pane", "session_id", sessionID, "error", err)
			}
		} else if err := rpc.removeSessionPane(sessionID); err != nil {
			logger.Warn("remove session pane", "session_id", sessionID, "error", err)
		}
	}
}

// immediateUIPusher implements mcpserver.UIPusher for the HTTP handler's
// register/unregister/dismiss_review tools: Push re-renders and re-sends the
// dashboard right after a mutation succeeds, the same as pushUIState's
// scheduled call, just triggered from a different goroutine.
// context.Background() as the base (rather than the tool call's own
// request-scoped ctx) is deliberate: that ctx is gone the instant the HTTP
// response is written, before this fire-and-forget push would even start.
type immediateUIPusher struct {
	store *registry.Store
	aoe   *aoeclient.Client
	rpc   *hostRPC
	log   *slog.Logger
}

func (p *immediateUIPusher) Push() {
	pushUIState(context.Background(), p.store, p.aoe, p.rpc, p.log)
}

func runHTTPServer(ctx context.Context, store *registry.Store, gh *provider.GitHubChecker, sessions *aoeclient.Client, rpc *hostRPC, logger *slog.Logger) {
	port := defaultPort
	if raw := os.Getenv("PR_WATCH_MCP_PORT"); raw != "" {
		if p, err := strconv.Atoi(raw); err == nil {
			port = p
		}
	}
	srv := &http.Server{
		Addr:    "127.0.0.1:" + strconv.Itoa(port),
		Handler: mcpserver.NewHandler(store, gh, sessions, &immediateUIPusher{store: store, aoe: sessions, rpc: rpc, log: logger}),
	}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Error("mcp http server", "error", err)
	}
}

// runToken shells out to a CLI's token-printing subcommand. The providers
// cache what it returns (GitHub's token, and GitLab's per host) and only ask
// again after a 401 or when there was no token, so it is rare, but it can
// still run in the scheduler's path, which is why it runs under the caller's
// context rather than unbounded: a stuck keyring backend would otherwise hang
// the whole scheduler, not just one registration. The alternative (parsing gh's
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
