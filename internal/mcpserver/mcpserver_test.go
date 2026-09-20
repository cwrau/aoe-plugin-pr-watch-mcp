package mcpserver

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/cwrau/aoe-pr-watch-mcp/internal/registry"
)

func openTestStore(t *testing.T) *registry.Store {
	t.Helper()
	s, err := registry.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("registry.Open() error = %v", err)
	}
	t.Cleanup(func() { s.Close() })
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
	defer session.Close()

	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool(%s) error = %v", name, err)
	}
	if result.IsError {
		t.Fatalf("CallTool(%s) returned an error result: %+v", name, result.Content)
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

	callTool(t, srv, "unregister", map[string]any{"pr_url": "https://github.com/o/r/pull/1"})

	regs, err = store.List(context.Background())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(regs) != 0 {
		t.Fatalf("after unregister, store.List() = %+v, want empty", regs)
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
