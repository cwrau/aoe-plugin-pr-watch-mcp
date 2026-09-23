package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/cwrau/aoe-plugin-pr-watch-mcp/internal/provider"
	"github.com/cwrau/aoe-plugin-pr-watch-mcp/internal/registry"
)

// fakeGitHubReviewClient drives dismiss_review's cascade in tests without a
// real GitHub round trip. Zero value reports every review not-found — fine
// for every test that doesn't call dismiss_review at all.
type fakeGitHubReviewClient struct {
	review         provider.ReviewLookup
	found          bool
	dismissErr     error // nil = DismissReview succeeds
	requestErr     error // nil = RequestReviewer succeeds
	dismissCalls   int
	requestCalls   int
	requestedLogin string
}

func (f *fakeGitHubReviewClient) FindReview(ctx context.Context, owner, repo string, number int, reviewURL string) (provider.ReviewLookup, bool, error) {
	return f.review, f.found, nil
}

func (f *fakeGitHubReviewClient) DismissReview(ctx context.Context, reviewID string) error {
	f.dismissCalls++
	return f.dismissErr
}

func (f *fakeGitHubReviewClient) RequestReviewer(ctx context.Context, pullRequestID, login string) error {
	f.requestCalls++
	f.requestedLogin = login
	return f.requestErr
}

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
	srv := httptest.NewServer(NewHandler(store, &fakeGitHubReviewClient{}))
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
	srv := httptest.NewServer(NewHandler(store, &fakeGitHubReviewClient{}))
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
	srv := httptest.NewServer(NewHandler(store, &fakeGitHubReviewClient{}))
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
	srv := httptest.NewServer(NewHandler(store, &fakeGitHubReviewClient{}))
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
	srv := httptest.NewServer(NewHandler(store, &fakeGitHubReviewClient{}))
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
	srv := httptest.NewServer(NewHandler(store, &fakeGitHubReviewClient{}))
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
	srv := httptest.NewServer(NewHandler(store, &fakeGitHubReviewClient{}))
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
	srv := httptest.NewServer(NewHandler(store, &fakeGitHubReviewClient{}))
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

func dismissMethod(t *testing.T, result *mcp.CallToolResult) string {
	t.Helper()
	m, ok := result.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("StructuredContent = %#v, want a map", result.StructuredContent)
	}
	method, _ := m["method"].(string)
	return method
}

func registerWithReview(t *testing.T, store *registry.Store, prURL, sessionID, reviewState, reviewer, reviewHref string) {
	t.Helper()
	ctx := context.Background()
	reg := registry.Registration{PRURL: prURL, SessionID: sessionID, Provider: "github", NextDueAt: time.Now(), CreatedAt: time.Now()}
	if err := store.Add(ctx, reg); err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	if err := store.UpdateState(ctx, prURL, registry.StateUpdate{
		LatestReviewState: reviewState, LatestReviewer: reviewer, ReviewHref: reviewHref, NextDueAt: time.Now(),
	}); err != nil {
		t.Fatalf("UpdateState() error = %v", err)
	}
}

func TestDismissReviewAPIDismissSucceeds(t *testing.T) {
	store := openTestStore(t)
	reviewURL := "https://github.com/o/r/pull/1#pullrequestreview-1"
	registerWithReview(t, store, "https://github.com/o/r/pull/1", "sess1", "CHANGES_REQUESTED", "bob", reviewURL)

	gh := &fakeGitHubReviewClient{found: true, review: provider.ReviewLookup{ReviewID: "REV_1", State: "CHANGES_REQUESTED", AuthorLogin: "bob", PullRequestID: "PR_1"}}
	srv := httptest.NewServer(NewHandler(store, gh))
	defer srv.Close()

	result := callTool(t, srv, "dismiss_review", map[string]any{
		"pr_url": "https://github.com/o/r/pull/1", "session_id": "sess1", "review_url": reviewURL,
	})
	if got := dismissMethod(t, result); got != "api_dismiss" {
		t.Errorf("method = %q, want api_dismiss", got)
	}
	if gh.dismissCalls != 1 || gh.requestCalls != 0 {
		t.Errorf("dismissCalls = %d requestCalls = %d, want (1, 0)", gh.dismissCalls, gh.requestCalls)
	}

	got, ok, err := store.Get(context.Background(), "https://github.com/o/r/pull/1")
	if err != nil || !ok {
		t.Fatalf("Get() = (%+v, %v, %v)", got, ok, err)
	}
	if got.LatestReviewState != "" || got.ReviewHref != "" || got.DismissedReviewHref != "" {
		t.Errorf("Get() = %+v, want review cleared and no marker set", got)
	}
}

func TestDismissReviewFallsBackToRequestReviewer(t *testing.T) {
	store := openTestStore(t)
	reviewURL := "https://github.com/o/r/pull/1#pullrequestreview-1"
	registerWithReview(t, store, "https://github.com/o/r/pull/1", "sess1", "CHANGES_REQUESTED", "bob", reviewURL)

	gh := &fakeGitHubReviewClient{
		found: true, review: provider.ReviewLookup{ReviewID: "REV_1", State: "CHANGES_REQUESTED", AuthorLogin: "bob", PullRequestID: "PR_1"},
		dismissErr: errors.New("only admins can dismiss reviews on this branch"),
	}
	srv := httptest.NewServer(NewHandler(store, gh))
	defer srv.Close()

	result := callTool(t, srv, "dismiss_review", map[string]any{
		"pr_url": "https://github.com/o/r/pull/1", "session_id": "sess1", "review_url": reviewURL,
	})
	if got := dismissMethod(t, result); got != "requested_review_again" {
		t.Errorf("method = %q, want requested_review_again", got)
	}
	if gh.dismissCalls != 1 || gh.requestCalls != 1 || gh.requestedLogin != "bob" {
		t.Errorf("dismissCalls = %d requestCalls = %d requestedLogin = %q, want (1, 1, bob)", gh.dismissCalls, gh.requestCalls, gh.requestedLogin)
	}

	got, ok, err := store.Get(context.Background(), "https://github.com/o/r/pull/1")
	if err != nil || !ok {
		t.Fatalf("Get() = (%+v, %v, %v)", got, ok, err)
	}
	if got.DismissedReviewHref != reviewURL {
		t.Errorf("DismissedReviewHref = %q, want %q (marker required since GitHub keeps reporting the old review)", got.DismissedReviewHref, reviewURL)
	}
}

func TestDismissReviewFallsBackToLocalOnly(t *testing.T) {
	store := openTestStore(t)
	reviewURL := "https://github.com/o/r/pull/1#pullrequestreview-1"
	registerWithReview(t, store, "https://github.com/o/r/pull/1", "sess1", "CHANGES_REQUESTED", "bob", reviewURL)

	gh := &fakeGitHubReviewClient{
		found: true, review: provider.ReviewLookup{ReviewID: "REV_1", State: "CHANGES_REQUESTED", AuthorLogin: "bob", PullRequestID: "PR_1"},
		dismissErr: errors.New("dismiss failed"),
		requestErr: errors.New("request failed"),
	}
	srv := httptest.NewServer(NewHandler(store, gh))
	defer srv.Close()

	result := callTool(t, srv, "dismiss_review", map[string]any{
		"pr_url": "https://github.com/o/r/pull/1", "session_id": "sess1", "review_url": reviewURL,
	})
	if got := dismissMethod(t, result); got != "local_only" {
		t.Errorf("method = %q, want local_only", got)
	}

	got, ok, err := store.Get(context.Background(), "https://github.com/o/r/pull/1")
	if err != nil || !ok {
		t.Fatalf("Get() = (%+v, %v, %v)", got, ok, err)
	}
	if got.DismissedReviewHref != reviewURL {
		t.Errorf("DismissedReviewHref = %q, want %q", got.DismissedReviewHref, reviewURL)
	}
}

func TestDismissReviewCommentedGoesLocalOnlyDirectly(t *testing.T) {
	store := openTestStore(t)
	reviewURL := "https://github.com/o/r/pull/1#pullrequestreview-1"
	registerWithReview(t, store, "https://github.com/o/r/pull/1", "sess1", "COMMENTED", "bob", reviewURL)

	gh := &fakeGitHubReviewClient{found: true, review: provider.ReviewLookup{ReviewID: "REV_1", State: "COMMENTED", AuthorLogin: "bob", PullRequestID: "PR_1"}}
	srv := httptest.NewServer(NewHandler(store, gh))
	defer srv.Close()

	result := callTool(t, srv, "dismiss_review", map[string]any{
		"pr_url": "https://github.com/o/r/pull/1", "session_id": "sess1", "review_url": reviewURL,
	})
	if got := dismissMethod(t, result); got != "local_only" {
		t.Errorf("method = %q, want local_only", got)
	}
	if gh.dismissCalls != 0 || gh.requestCalls != 0 {
		t.Errorf("dismissCalls = %d requestCalls = %d, want (0, 0): GitHub can't dismiss a COMMENTED review", gh.dismissCalls, gh.requestCalls)
	}
}

func TestDismissReviewApprovedErrors(t *testing.T) {
	store := openTestStore(t)
	reviewURL := "https://github.com/o/r/pull/1#pullrequestreview-1"
	registerWithReview(t, store, "https://github.com/o/r/pull/1", "sess1", "APPROVED", "bob", reviewURL)

	gh := &fakeGitHubReviewClient{found: true, review: provider.ReviewLookup{ReviewID: "REV_1", State: "APPROVED", AuthorLogin: "bob", PullRequestID: "PR_1"}}
	srv := httptest.NewServer(NewHandler(store, gh))
	defer srv.Close()

	callToolExpectError(t, srv, "dismiss_review", map[string]any{
		"pr_url": "https://github.com/o/r/pull/1", "session_id": "sess1", "review_url": reviewURL,
	})
}

func TestDismissReviewNotFoundErrors(t *testing.T) {
	store := openTestStore(t)
	reviewURL := "https://github.com/o/r/pull/1#pullrequestreview-1"
	registerWithReview(t, store, "https://github.com/o/r/pull/1", "sess1", "CHANGES_REQUESTED", "bob", reviewURL)

	gh := &fakeGitHubReviewClient{found: false}
	srv := httptest.NewServer(NewHandler(store, gh))
	defer srv.Close()

	callToolExpectError(t, srv, "dismiss_review", map[string]any{
		"pr_url": "https://github.com/o/r/pull/1", "session_id": "sess1", "review_url": "https://github.com/o/r/pull/1#pullrequestreview-999",
	})
}

func TestDismissReviewRejectsWrongSession(t *testing.T) {
	store := openTestStore(t)
	reviewURL := "https://github.com/o/r/pull/1#pullrequestreview-1"
	registerWithReview(t, store, "https://github.com/o/r/pull/1", "sess1", "CHANGES_REQUESTED", "bob", reviewURL)

	gh := &fakeGitHubReviewClient{found: true, review: provider.ReviewLookup{ReviewID: "REV_1", State: "CHANGES_REQUESTED", AuthorLogin: "bob"}}
	srv := httptest.NewServer(NewHandler(store, gh))
	defer srv.Close()

	callToolExpectError(t, srv, "dismiss_review", map[string]any{
		"pr_url": "https://github.com/o/r/pull/1", "session_id": "sess2", "review_url": reviewURL,
	})
}

func TestDismissReviewRejectsGitLab(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	reg := registry.Registration{PRURL: "https://gitlab.corp.example/team/project/-/merge_requests/5", SessionID: "sess1", Provider: "gitlab", NextDueAt: time.Now(), CreatedAt: time.Now()}
	if err := store.Add(ctx, reg); err != nil {
		t.Fatalf("Add() error = %v", err)
	}

	srv := httptest.NewServer(NewHandler(store, &fakeGitHubReviewClient{}))
	defer srv.Close()

	callToolExpectError(t, srv, "dismiss_review", map[string]any{
		"pr_url": reg.PRURL, "session_id": "sess1", "review_url": "https://gitlab.corp.example/team/project/-/merge_requests/5#note_1",
	})
}

func listRegistrations(t *testing.T, result *mcp.CallToolResult) []registry.Registration {
	t.Helper()
	m, ok := result.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("StructuredContent = %#v, want a map", result.StructuredContent)
	}
	raw, err := json.Marshal(m["registrations"])
	if err != nil {
		t.Fatalf("marshal registrations: %v", err)
	}
	var regs []registry.Registration
	if err := json.Unmarshal(raw, &regs); err != nil {
		t.Fatalf("unmarshal registrations: %v", err)
	}
	return regs
}

func unregisterCount(t *testing.T, result *mcp.CallToolResult) int {
	t.Helper()
	m, ok := result.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("StructuredContent = %#v, want a map", result.StructuredContent)
	}
	count, _ := m["count"].(float64)
	return int(count)
}

func TestListWithNoFiltersReturnsEveryStatusAndSession(t *testing.T) {
	store := openTestStore(t)
	srv := httptest.NewServer(NewHandler(store, &fakeGitHubReviewClient{}))
	defer srv.Close()
	ctx := context.Background()

	callTool(t, srv, "register", map[string]any{"pr_url": "https://github.com/o/r/pull/1", "session_id": "sess1"})
	callTool(t, srv, "register", map[string]any{"pr_url": "https://github.com/o/r/pull/2", "session_id": "sess2"})
	if err := store.SetStatus(ctx, "https://github.com/o/r/pull/2", registry.StatusMerged); err != nil {
		t.Fatalf("SetStatus() error = %v", err)
	}

	regs := listRegistrations(t, callTool(t, srv, "list", map[string]any{}))
	if len(regs) != 2 {
		t.Fatalf("list with no filters = %+v, want both registrations regardless of status", regs)
	}
}

func TestListFiltersBySessionID(t *testing.T) {
	store := openTestStore(t)
	srv := httptest.NewServer(NewHandler(store, &fakeGitHubReviewClient{}))
	defer srv.Close()

	callTool(t, srv, "register", map[string]any{"pr_url": "https://github.com/o/r/pull/1", "session_id": "sess1"})
	callTool(t, srv, "register", map[string]any{"pr_url": "https://github.com/o/r/pull/2", "session_id": "sess2"})

	regs := listRegistrations(t, callTool(t, srv, "list", map[string]any{"session_id": "sess1"}))
	if len(regs) != 1 || regs[0].SessionID != "sess1" {
		t.Errorf("list(session_id=sess1) = %+v, want only sess1's registration", regs)
	}
}

func TestListSessionIDFilterAcceptsFullArtifactDirPath(t *testing.T) {
	store := openTestStore(t)
	srv := httptest.NewServer(NewHandler(store, &fakeGitHubReviewClient{}))
	defer srv.Close()

	callTool(t, srv, "register", map[string]any{"pr_url": "https://github.com/o/r/pull/1", "session_id": "sess1"})

	regs := listRegistrations(t, callTool(t, srv, "list", map[string]any{
		"session_id": "/home/cwr/.config/agent-of-empires/artifacts/sess1",
	}))
	if len(regs) != 1 || regs[0].SessionID != "sess1" {
		t.Errorf("list(session_id=<full artifact dir path>) = %+v, want sess1's registration via basename normalization", regs)
	}
}

func TestListFiltersByStatus(t *testing.T) {
	store := openTestStore(t)
	srv := httptest.NewServer(NewHandler(store, &fakeGitHubReviewClient{}))
	defer srv.Close()
	ctx := context.Background()

	callTool(t, srv, "register", map[string]any{"pr_url": "https://github.com/o/r/pull/1", "session_id": "sess1"})
	callTool(t, srv, "register", map[string]any{"pr_url": "https://github.com/o/r/pull/2", "session_id": "sess1"})
	if err := store.SetStatus(ctx, "https://github.com/o/r/pull/2", registry.StatusMerged); err != nil {
		t.Fatalf("SetStatus() error = %v", err)
	}

	regs := listRegistrations(t, callTool(t, srv, "list", map[string]any{"status": "merged"}))
	if len(regs) != 1 || regs[0].PRURL != "https://github.com/o/r/pull/2" {
		t.Errorf("list(status=merged) = %+v, want only the merged registration", regs)
	}
}

func TestListRejectsUnrecognizedStatus(t *testing.T) {
	store := openTestStore(t)
	srv := httptest.NewServer(NewHandler(store, &fakeGitHubReviewClient{}))
	defer srv.Close()

	callToolExpectError(t, srv, "list", map[string]any{"status": "bogus"})
}

func TestUnregisterBulkModeStopsEveryRegistrationForSession(t *testing.T) {
	store := openTestStore(t)
	srv := httptest.NewServer(NewHandler(store, &fakeGitHubReviewClient{}))
	defer srv.Close()
	ctx := context.Background()

	callTool(t, srv, "register", map[string]any{"pr_url": "https://github.com/o/r/pull/1", "session_id": "sess1"})
	callTool(t, srv, "register", map[string]any{"pr_url": "https://github.com/o/r/pull/2", "session_id": "sess1"})
	callTool(t, srv, "register", map[string]any{"pr_url": "https://github.com/o/r/pull/3", "session_id": "sess2"})
	if err := store.SetStatus(ctx, "https://github.com/o/r/pull/2", registry.StatusMerged); err != nil {
		t.Fatalf("SetStatus() error = %v", err)
	}

	result := callTool(t, srv, "unregister", map[string]any{"session_id": "sess1"})
	if count := unregisterCount(t, result); count != 2 {
		t.Errorf("unregister(session_id only) count = %d, want 2", count)
	}

	regs, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	byURL := map[string]registry.Registration{}
	for _, r := range regs {
		byURL[r.PRURL] = r
	}
	if byURL["https://github.com/o/r/pull/1"].Status != registry.StatusStopped {
		t.Errorf("pull/1 status = %q, want stopped", byURL["https://github.com/o/r/pull/1"].Status)
	}
	if byURL["https://github.com/o/r/pull/2"].Status != registry.StatusStopped {
		t.Errorf("pull/2 status = %q, want stopped", byURL["https://github.com/o/r/pull/2"].Status)
	}
	if byURL["https://github.com/o/r/pull/3"].Status != registry.StatusWatching {
		t.Errorf("pull/3 status = %q, want untouched (different session)", byURL["https://github.com/o/r/pull/3"].Status)
	}
}

func TestUnregisterRejectsEmptySessionID(t *testing.T) {
	store := openTestStore(t)
	srv := httptest.NewServer(NewHandler(store, &fakeGitHubReviewClient{}))
	defer srv.Close()

	callToolExpectError(t, srv, "unregister", map[string]any{"pr_url": "https://github.com/o/r/pull/1"})
}

func TestRegisterAcceptsFullArtifactDirPathAsSessionID(t *testing.T) {
	store := openTestStore(t)
	srv := httptest.NewServer(NewHandler(store, &fakeGitHubReviewClient{}))
	defer srv.Close()

	callTool(t, srv, "register", map[string]any{
		"pr_url":     "https://github.com/o/r/pull/1",
		"session_id": "/home/cwr/.config/agent-of-empires/artifacts/sess1",
	})

	regs, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(regs) != 1 || regs[0].SessionID != "sess1" {
		t.Fatalf("store.List() = %+v, want SessionID normalized to %q", regs, "sess1")
	}
}
