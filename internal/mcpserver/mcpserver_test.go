package mcpserver

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/cwrau/aoe-plugin-pr-watch-mcp/internal/registry"
)

func openTestStore(t *testing.T) *registry.Store {
	t.Helper()
	s, err := registry.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("registry.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// callTool drives NewHandler's http.Handler through a real MCP client
// (in-process, over an httptest server) so the test exercises the actual
// wire protocol rather than calling Go functions directly.
func callTool(t *testing.T, srv *httptest.Server, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	ctx := context.Background()
	transport := &mcp.StreamableClientTransport{Endpoint: srv.URL}
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0.0.1"}, nil)
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		t.Fatalf("client.Connect() error = %v", err)
	}
	defer func() { _ = session.Close() }()

	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool(%s) error = %v", name, err)
	}
	if result.IsError {
		t.Fatalf("CallTool(%s) returned an error result: %+v", name, result.Content)
	}
	return result
}

// callToolExpectError is callTool's counterpart for cases that must be
// rejected by the tool handler (e.g. a malformed pr_url): it asserts the
// call reaches the server and comes back as a tool error, rather than
// treating IsError as a test failure.
func callToolExpectError(t *testing.T, srv *httptest.Server, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	ctx := context.Background()
	transport := &mcp.StreamableClientTransport{Endpoint: srv.URL}
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0.0.1"}, nil)
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		t.Fatalf("client.Connect() error = %v", err)
	}
	defer func() { _ = session.Close() }()

	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool(%s) error = %v", name, err)
	}
	if !result.IsError {
		t.Fatalf("CallTool(%s) = %+v, want an error result", name, result)
	}
	return result
}

func TestRegisterThenListThenUnregister(t *testing.T) {
	store := openTestStore(t)
	srv := httptest.NewServer(NewHandler(store))
	defer srv.Close()

	callTool(t, srv, "register", map[string]any{
		"pr_url":     "https://github.com/o/r/pull/1",
		"session_id": "sess1",
	})

	regs, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(regs) != 1 || regs[0].SessionID != "sess1" || regs[0].Provider != "github" {
		t.Fatalf("after register, store.List() = %+v", regs)
	}

	callTool(t, srv, "unregister", map[string]any{"pr_url": "https://github.com/o/r/pull/1", "session_id": "sess1"})

	regs, err = store.List(context.Background())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(regs) != 1 || regs[0].Status != registry.StatusStopped {
		t.Fatalf("after unregister, store.List() = %+v, want a single row at status %q (registrations are never deleted)", regs, registry.StatusStopped)
	}
}

func TestRegisterDetectsGitLabProvider(t *testing.T) {
	store := openTestStore(t)
	srv := httptest.NewServer(NewHandler(store))
	defer srv.Close()

	callTool(t, srv, "register", map[string]any{
		"pr_url":     "https://gitlab.corp.example/team/project/-/merge_requests/5",
		"session_id": "sess1",
	})

	regs, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(regs) != 1 || regs[0].Provider != "gitlab" {
		t.Fatalf("store.List() = %+v, want provider gitlab", regs)
	}
}

func TestListToolReturnsRegistrations(t *testing.T) {
	store := openTestStore(t)
	srv := httptest.NewServer(NewHandler(store))
	defer srv.Close()

	callTool(t, srv, "register", map[string]any{"pr_url": "https://github.com/o/r/pull/1", "session_id": "sess1"})
	callTool(t, srv, "register", map[string]any{"pr_url": "https://github.com/o/r/pull/2", "session_id": "sess2"})

	result := callTool(t, srv, "list", map[string]any{})
	if len(result.Content) == 0 {
		t.Fatal("list tool returned no content")
	}
}

// TestRegisterRejectsMalformedURL guards against the SSRF/token-exfiltration
// path where a pr_url that isn't shaped like a real GitHub PR or GitLab MR
// URL would otherwise get stored verbatim and later handed to a checker
// that sends the real provider auth token to whatever host it extracts from
// it. register must reject such input outright rather than defaulting it to
// "gitlab" and storing it.
func TestRegisterRejectsMalformedURL(t *testing.T) {
	store := openTestStore(t)
	srv := httptest.NewServer(NewHandler(store))
	defer srv.Close()

	callToolExpectError(t, srv, "register", map[string]any{
		"pr_url":     "https://attacker.example.com/steal-token",
		"session_id": "sess1",
	})

	regs, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(regs) != 0 {
		t.Fatalf("after rejected register, store.List() = %+v, want empty (nothing should be persisted)", regs)
	}
}

// TestUnregisterRequiresMatchingSessionID guards against one session
// deregistering another session's watch: unregister must only remove a
// registration when the caller's session_id matches the one that created
// it.
func TestUnregisterRequiresMatchingSessionID(t *testing.T) {
	store := openTestStore(t)
	srv := httptest.NewServer(NewHandler(store))
	defer srv.Close()

	callTool(t, srv, "register", map[string]any{
		"pr_url":     "https://github.com/o/r/pull/1",
		"session_id": "sess1",
	})

	// Wrong session_id: must not remove the registration.
	callTool(t, srv, "unregister", map[string]any{
		"pr_url":     "https://github.com/o/r/pull/1",
		"session_id": "sess-attacker",
	})

	regs, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(regs) != 1 {
		t.Fatalf("after unregister with wrong session_id, store.List() = %+v, want 1 registration still present", regs)
	}

	// Correct session_id: must stop it.
	callTool(t, srv, "unregister", map[string]any{
		"pr_url":     "https://github.com/o/r/pull/1",
		"session_id": "sess1",
	})

	regs, err = store.List(context.Background())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(regs) != 1 || regs[0].Status != registry.StatusStopped {
		t.Fatalf("after unregister with correct session_id, store.List() = %+v, want a single row at status %q", regs, registry.StatusStopped)
	}
}

// TestRegisterRejectsEmptySessionID: an empty session_id can never match a
// real AoE session, so the scheduler would classify it gone and delete the
// registration on the next tick — register must reject it up front rather
// than reporting a success that silently evaporates.
func TestRegisterRejectsEmptySessionID(t *testing.T) {
	store := openTestStore(t)
	srv := httptest.NewServer(NewHandler(store))
	defer srv.Close()

	callToolExpectError(t, srv, "register", map[string]any{
		"pr_url":     "https://github.com/o/r/pull/1",
		"session_id": "",
	})

	regs, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(regs) != 0 {
		t.Fatalf("after rejected register, store.List() = %+v, want empty", regs)
	}
}

// TestRegisterRejectsCrossSessionReassignment: store.Add upserts, so without
// an ownership check a caller could re-register another session's pr_url
// under its own session_id — hijacking its notifications and gaining the
// right to unregister it, which is exactly what unregister's ownership
// check is supposed to prevent.
func TestRegisterRejectsCrossSessionReassignment(t *testing.T) {
	store := openTestStore(t)
	srv := httptest.NewServer(NewHandler(store))
	defer srv.Close()

	callTool(t, srv, "register", map[string]any{
		"pr_url":     "https://github.com/o/r/pull/1",
		"session_id": "sess1",
	})

	callToolExpectError(t, srv, "register", map[string]any{
		"pr_url":     "https://github.com/o/r/pull/1",
		"session_id": "sess-attacker",
	})

	regs, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(regs) != 1 || regs[0].SessionID != "sess1" {
		t.Fatalf("store.List() = %+v, want the original sess1 registration untouched", regs)
	}

	// The original owner must still be able to unregister it, proving the
	// rejected call didn't take ownership.
	callTool(t, srv, "unregister", map[string]any{
		"pr_url":     "https://github.com/o/r/pull/1",
		"session_id": "sess1",
	})
	regs, err = store.List(context.Background())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(regs) != 1 || regs[0].Status != registry.StatusStopped {
		t.Fatalf("after the owner unregistered, store.List() = %+v, want a single row at status %q", regs, registry.StatusStopped)
	}
}

// TestRegisterIsIdempotentForSameSession: re-registering the same pr_url
// from the same session (e.g. reconnecting after a worker restart) must
// still succeed — the ownership check only rejects a *different* session.
func TestRegisterIsIdempotentForSameSession(t *testing.T) {
	store := openTestStore(t)
	srv := httptest.NewServer(NewHandler(store))
	defer srv.Close()

	args := map[string]any{"pr_url": "https://github.com/o/r/pull/1", "session_id": "sess1"}
	callTool(t, srv, "register", args)
	callTool(t, srv, "register", args)

	regs, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(regs) != 1 || regs[0].SessionID != "sess1" {
		t.Fatalf("store.List() = %+v, want exactly one sess1 registration", regs)
	}
}

// TestRegisterStopsSessionsOldMergedRegistration: a session that moves on to
// watching a new PR shouldn't keep an old merged one's badge cluttering its
// row/pane forever.
func TestRegisterStopsSessionsOldMergedRegistration(t *testing.T) {
	store := openTestStore(t)
	srv := httptest.NewServer(NewHandler(store))
	defer srv.Close()
	ctx := context.Background()

	callTool(t, srv, "register", map[string]any{"pr_url": "https://github.com/o/r/pull/1", "session_id": "sess1"})
	if err := store.SetStatus(ctx, "https://github.com/o/r/pull/1", registry.StatusMerged); err != nil {
		t.Fatalf("SetStatus() error = %v", err)
	}

	callTool(t, srv, "register", map[string]any{"pr_url": "https://github.com/o/r/pull/2", "session_id": "sess1"})

	regs, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	byURL := map[string]registry.Registration{}
	for _, r := range regs {
		byURL[r.PRURL] = r
	}
	if got := byURL["https://github.com/o/r/pull/1"].Status; got != registry.StatusStopped {
		t.Errorf("old merged PR status = %q, want %q", got, registry.StatusStopped)
	}
	if got := byURL["https://github.com/o/r/pull/2"].Status; got != registry.StatusWatching {
		t.Errorf("new PR status = %q, want %q", got, registry.StatusWatching)
	}
}

// TestRegisterDoesNotStopAnotherSessionsMergedRegistration guards the same
// cleanup against reaching outside the registering session's own rows.
func TestRegisterDoesNotStopAnotherSessionsMergedRegistration(t *testing.T) {
	store := openTestStore(t)
	srv := httptest.NewServer(NewHandler(store))
	defer srv.Close()
	ctx := context.Background()

	callTool(t, srv, "register", map[string]any{"pr_url": "https://github.com/o/r/pull/1", "session_id": "sess1"})
	if err := store.SetStatus(ctx, "https://github.com/o/r/pull/1", registry.StatusMerged); err != nil {
		t.Fatalf("SetStatus() error = %v", err)
	}

	callTool(t, srv, "register", map[string]any{"pr_url": "https://github.com/o/r/pull/2", "session_id": "sess2"})

	reg, ok, err := store.Get(ctx, "https://github.com/o/r/pull/1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if !ok || reg.Status != registry.StatusMerged {
		t.Errorf("sess1's merged PR = %+v, want status %q untouched by sess2's register", reg, registry.StatusMerged)
	}
}
