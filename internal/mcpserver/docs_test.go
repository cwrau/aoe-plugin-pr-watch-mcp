package mcpserver

import (
	"context"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// connect opens a real MCP client session so the test sees exactly what an
// agent sees: the initialize instructions and the advertised tool list.
func connect(t *testing.T, srv *httptest.Server) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0.0.1"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: srv.URL}, nil)
	if err != nil {
		t.Fatalf("client.Connect() error = %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(NewHandler(openTestStore(t), &fakeGitHubReviewClient{}, &fakeSessionValidator{}, &fakeUIPusher{}))
	t.Cleanup(srv.Close)
	return srv
}

func TestServerInstructionsExplainPingsAndEvents(t *testing.T) {
	session := connect(t, newTestServer(t))
	instructions := session.InitializeResult().Instructions
	if instructions == "" {
		t.Fatal("server instructions are empty; agents get no ping documentation")
	}
	kinds := make([]string, 0, len(validEventKinds))
	for kind := range validEventKinds {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	for _, kind := range kinds {
		if !strings.Contains(instructions, kind) {
			t.Errorf("instructions do not mention event kind %q", kind)
		}
	}
	for _, want := range []string{"[pr-watch]", "auto-discover", "Never act", "30 days", "Opt-in"} {
		if !strings.Contains(instructions, want) {
			t.Errorf("instructions do not mention %q", want)
		}
	}
}

func TestRegisterDescriptionListsEveryEventKind(t *testing.T) {
	session := connect(t, newTestServer(t))
	tools, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools() error = %v", err)
	}
	var desc string
	for _, tool := range tools.Tools {
		if tool.Name == "register" {
			desc = tool.Description
		}
	}
	if desc == "" {
		t.Fatal("register tool not advertised")
	}
	for kind := range validEventKinds {
		if !strings.Contains(desc, kind) {
			t.Errorf("register description does not mention event kind %q", kind)
		}
	}
	if !strings.Contains(desc, "auto-discover") {
		t.Error("register description does not say when explicit registration is needed")
	}
}

func TestEveryToolDescriptionIsSubstantial(t *testing.T) {
	session := connect(t, newTestServer(t))
	tools, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools() error = %v", err)
	}
	if len(tools.Tools) != 4 {
		t.Fatalf("tool count = %d, want 4", len(tools.Tools))
	}
	for _, tool := range tools.Tools {
		if len(tool.Description) < 150 {
			t.Errorf("tool %q description is only %d chars", tool.Name, len(tool.Description))
		}
	}
}

func TestRegisterStartsSilentUnlessReportExisting(t *testing.T) {
	store := openTestStore(t)
	srv := httptest.NewServer(NewHandler(store, &fakeGitHubReviewClient{}, &fakeSessionValidator{}, &fakeUIPusher{}))
	t.Cleanup(srv.Close)

	callTool(t, srv, "register", map[string]any{"pr_urls": []string{"https://github.com/o/r/pull/1"}, "session_id": "sess1"})
	callTool(t, srv, "register", map[string]any{"pr_urls": []string{"https://github.com/o/r/pull/2"}, "session_id": "sess1", "report_existing": true})

	ctx := context.Background()
	if silent, err := store.FirstPollSilent(ctx, "https://github.com/o/r/pull/1", "sess1"); err != nil || !silent {
		t.Errorf("default register: FirstPollSilent = (%v, %v), want true", silent, err)
	}
	if silent, err := store.FirstPollSilent(ctx, "https://github.com/o/r/pull/2", "sess1"); err != nil || silent {
		t.Errorf("report_existing register: FirstPollSilent = (%v, %v), want false", silent, err)
	}
}
