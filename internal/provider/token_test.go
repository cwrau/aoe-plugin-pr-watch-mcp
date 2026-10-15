package provider

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// authServer answers 401 unless the credential equals good, and records how
// many requests it saw. gitlab picks the header the credential arrives in.
func authServer(t *testing.T, good string, gitlab bool) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		got := r.Header.Get("Authorization")
		want := "bearer " + good
		if gitlab {
			got, want = r.Header.Get("PRIVATE-TOKEN"), good
		}
		if got != want {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message":"401 Unauthorized"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if gitlab {
			_, _ = w.Write([]byte(`[]`))
			return
		}
		_, _ = w.Write([]byte(`{"data": {"rateLimit": {"limit": 5000, "remaining": 4999, "resetAt": "2026-09-20T23:00:00Z"}}}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &requests
}

// tokens hands out the given values in order, then the last one forever, and
// counts how often it was asked.
type tokens struct {
	values []string
	calls  atomic.Int32
	err    error
}

func (f *tokens) get() (string, error) {
	n := int(f.calls.Add(1)) - 1
	if f.err != nil {
		return "", f.err
	}
	return f.values[min(n, len(f.values)-1)], nil
}

func gitlabTokenChecker(srv *httptest.Server, f *tokens) *GitLabChecker {
	return &GitLabChecker{HTTPClient: srv.Client(), apiBaseOverride: srv.URL,
		TokenFunc: func(context.Context, string) (string, error) { return f.get() }}
}

func discoverMR(t *testing.T, c *GitLabChecker) error {
	t.Helper()
	_, _, _, err := c.DiscoverBranchMR(t.Context(), "gitlab.example.com", "g/p", "feat")
	return err
}

// The credential command is a subprocess; it is run once per host and the
// result reused, not once per request.
func TestGitLabTokenIsFetchedOncePerHost(t *testing.T) {
	srv, _ := authServer(t, "good", true)
	f := &tokens{values: []string{"good"}}
	c := gitlabTokenChecker(srv, f)
	for range 4 {
		if err := discoverMR(t, c); err != nil {
			t.Fatalf("DiscoverBranchMR() error = %v", err)
		}
	}
	if got := f.calls.Load(); got != 1 {
		t.Errorf("token lookups = %d, want 1 for four requests", got)
	}
}

// A rejected credential is dropped, so the next request asks for a fresh one.
func TestGitLabTokenIsRefetchedAfterAnUnauthorizedResponse(t *testing.T) {
	srv, _ := authServer(t, "fresh", true)
	f := &tokens{values: []string{"stale", "fresh"}}
	c := gitlabTokenChecker(srv, f)

	if err := discoverMR(t, c); err == nil {
		t.Fatal("first request succeeded, want the 401 reported")
	}
	if err := discoverMR(t, c); err != nil {
		t.Fatalf("second request error = %v, want success with the refreshed token", err)
	}
	if got := f.calls.Load(); got != 2 {
		t.Errorf("token lookups = %d, want 2 (once, then again only after the 401)", got)
	}
	if err := discoverMR(t, c); err != nil {
		t.Fatalf("third request error = %v", err)
	}
	if got := f.calls.Load(); got != 2 {
		t.Errorf("token lookups = %d, want still 2: the fresh token is cached", got)
	}
}

// No credential for a host is not remembered: configuring one later works.
func TestGitLabMissingTokenIsNotCached(t *testing.T) {
	srv, _ := authServer(t, "good", true)
	f := &tokens{values: []string{"", "good"}}
	c := gitlabTokenChecker(srv, f)

	if mr, found, _, err := c.DiscoverBranchMR(t.Context(), "gitlab.example.com", "g/p", "feat"); err != nil || found || mr != "" {
		t.Fatalf("with no token: %q %v %v, want nothing found and no error", mr, found, err)
	}
	if err := discoverMR(t, c); err != nil {
		t.Fatalf("after the token appeared: error = %v", err)
	}
	if got := f.calls.Load(); got != 2 {
		t.Errorf("token lookups = %d, want 2: the empty answer must not be cached", got)
	}
}

func githubTokenChecker(srv *httptest.Server, static string, f *tokens) *GitHubChecker {
	c := &GitHubChecker{HTTPClient: srv.Client(), BaseURL: srv.URL, Token: static}
	if f != nil {
		c.TokenFunc = func(context.Context) (string, error) { return f.get() }
	}
	return c
}

func githubDiscover(t *testing.T, c *GitHubChecker) error {
	t.Helper()
	_, _, err := c.DiscoverBranchPRs(t.Context(), []GitHubBranchQuery{{Owner: "o", Repo: "r", Branch: "feat"}})
	return err
}

// A token that was not available at startup is fetched on first use, once.
func TestGitHubTokenIsFetchedLazilyAndCached(t *testing.T) {
	srv, _ := authServer(t, "good", false)
	f := &tokens{values: []string{"good"}}
	c := githubTokenChecker(srv, "", f)
	for range 3 {
		if err := githubDiscover(t, c); err != nil {
			t.Fatalf("DiscoverBranchPRs() error = %v", err)
		}
	}
	if got := f.calls.Load(); got != 1 {
		t.Errorf("token lookups = %d, want 1 for three requests", got)
	}
}

// A startup token is used as is, with no lookup at all.
func TestGitHubStartupTokenIsUsedWithoutALookup(t *testing.T) {
	srv, _ := authServer(t, "good", false)
	f := &tokens{values: []string{"other"}}
	c := githubTokenChecker(srv, "good", f)
	if err := githubDiscover(t, c); err != nil {
		t.Fatalf("DiscoverBranchPRs() error = %v", err)
	}
	if got := f.calls.Load(); got != 0 {
		t.Errorf("token lookups = %d, want 0 with a token already known", got)
	}
}

func TestGitHubTokenIsRefetchedAfterAnUnauthorizedResponse(t *testing.T) {
	srv, _ := authServer(t, "fresh", false)
	f := &tokens{values: []string{"fresh"}}
	c := githubTokenChecker(srv, "stale", f)

	if err := githubDiscover(t, c); err == nil {
		t.Fatal("request with the stale token succeeded, want the 401 reported")
	}
	if err := githubDiscover(t, c); err != nil {
		t.Fatalf("next request error = %v, want success with the refreshed token", err)
	}
	if got := f.calls.Load(); got != 1 {
		t.Errorf("token lookups = %d, want exactly 1 (only after the 401)", got)
	}
}

// With no token and no way to get one, no request goes out at all.
func TestGitHubWithoutAnyTokenDoesNotCallTheAPI(t *testing.T) {
	srv, requests := authServer(t, "good", false)
	f := &tokens{err: errors.New("gh: not logged in")}
	c := githubTokenChecker(srv, "", f)
	if err := githubDiscover(t, c); err == nil {
		t.Fatal("DiscoverBranchPRs() error = nil, want the lookup failure")
	}
	if got := requests.Load(); got != 0 {
		t.Errorf("API requests = %d, want 0 without a credential", got)
	}
}
