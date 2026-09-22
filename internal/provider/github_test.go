package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestParseGitHubPRURL(t *testing.T) {
	owner, repo, number, err := ParseGitHubPRURL("https://github.com/cwrau/aoe-pr-watch-mcp/pull/42")
	if err != nil {
		t.Fatalf("ParseGitHubPRURL() error = %v", err)
	}
	if owner != "cwrau" || repo != "aoe-pr-watch-mcp" || number != 42 {
		t.Errorf("ParseGitHubPRURL() = (%q, %q, %d), want (cwrau, aoe-pr-watch-mcp, 42)", owner, repo, number)
	}
}

func TestParseGitHubPRURLRejectsNonPR(t *testing.T) {
	if _, _, _, err := ParseGitHubPRURL("https://github.com/cwrau/aoe-pr-watch-mcp/issues/1"); err == nil {
		t.Error("ParseGitHubPRURL() on an issue URL, want error")
	}
}

// fakeGitHubServer serves a GraphQL response for POST /graphql and a REST
// actions/runs response for GET .../actions/runs, with configurable
// rate-limit signal on each. graphqlBody is returned verbatim, so callers
// write it in the batch shape buildGitHubBatchQuery produces for their
// input — for a single URL that's always aliased "repo0"/"pr0"
// (buildGitHubBatchQuery aliases the first-seen repo/PR that way
// deterministically), which every single-PR test here relies on.
func fakeGitHubServer(t *testing.T, graphqlBody string, actionRequiredCount int) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/graphql", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(graphqlBody))
	})
	mux.HandleFunc("/repos/o/r/actions/runs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Remaining", "4999")
		w.Header().Set("X-RateLimit-Reset", "9999999999")
		runs := map[string]any{"workflow_runs": []map[string]any{}}
		if actionRequiredCount > 0 {
			runList := []map[string]any{}
			for range actionRequiredCount {
				runList = append(runList, map[string]any{"conclusion": "action_required"})
			}
			runs["workflow_runs"] = runList
		}
		_ = json.NewEncoder(w).Encode(runs)
	})
	return httptest.NewServer(mux)
}

const graphqlOpenSuccessBody = `{
  "data": {
    "repo0": { "pr0": {
      "state": "OPEN",
      "title": "Fix the thing",
      "body": "This fixes the thing by doing the other thing.",
      "headRefOid": "deadbeef",
      "reviewCount": { "totalCount": 2 },
      "latestReview": { "nodes": [{ "author": { "login": "alice" }, "state": "APPROVED" }] },
      "commits": { "nodes": [{ "commit": { "statusCheckRollup": { "state": "SUCCESS" } } }] }
    } }
  },
  "rateLimit": { "limit": 5000, "remaining": 4998, "resetAt": "2026-09-20T23:00:00Z" }
}`

func TestGitHubCheckerOpenSuccess(t *testing.T) {
	srv := fakeGitHubServer(t, graphqlOpenSuccessBody, 0)
	defer srv.Close()

	c := &GitHubChecker{HTTPClient: srv.Client(), Token: "test-token", BaseURL: srv.URL}
	url := "https://github.com/o/r/pull/1"
	results, budgets, err := c.CheckBatch(t.Context(), []string{url})
	if err != nil {
		t.Fatalf("CheckBatch() error = %v", err)
	}
	result, ok := results[url]
	if !ok {
		t.Fatalf("CheckBatch() results = %+v, want an entry for %q", results, url)
	}
	if result.State != "open" || result.Title != "Fix the thing" || result.Description != "This fixes the thing by doing the other thing." || result.ReviewCount != 2 || result.LatestReviewer != "alice" || result.LatestReviewState != "APPROVED" || result.CIBucket != "success" {
		t.Errorf("CheckBatch() result = %+v", result)
	}
	if budgets.GraphQL.Remaining != 4998 || budgets.REST.Remaining != 4999 {
		t.Errorf("CheckBatch() budgets = %+v", budgets)
	}
}

const graphqlMergedBody = `{
  "data": { "repo0": { "pr0": { "state": "MERGED", "headRefOid": "x", "reviewCount": {"totalCount":0}, "latestReview": {"nodes":[]}, "commits": {"nodes":[]} } } },
  "rateLimit": { "limit": 5000, "remaining": 4997, "resetAt": "2026-09-20T23:00:00Z" }
}`

func TestGitHubCheckerMerged(t *testing.T) {
	srv := fakeGitHubServer(t, graphqlMergedBody, 0)
	defer srv.Close()

	c := &GitHubChecker{HTTPClient: srv.Client(), Token: "test-token", BaseURL: srv.URL}
	url := "https://github.com/o/r/pull/1"
	results, _, err := c.CheckBatch(t.Context(), []string{url})
	if err != nil {
		t.Fatalf("CheckBatch() error = %v", err)
	}
	if results[url].State != "merged" {
		t.Errorf("CheckBatch() result.State = %q, want merged", results[url].State)
	}
}

const graphqlPendingRollupBody = `{
  "data": { "repo0": { "pr0": {
    "state": "OPEN", "headRefOid": "deadbeef",
    "reviewCount": {"totalCount":0}, "latestReview": {"nodes":[]},
    "commits": { "nodes": [{ "commit": { "statusCheckRollup": { "state": "PENDING" } } }] }
  } } },
  "rateLimit": { "limit": 5000, "remaining": 4996, "resetAt": "2026-09-20T23:00:00Z" }
}`

func TestGitHubCheckerActionRequiredOverridesPending(t *testing.T) {
	srv := fakeGitHubServer(t, graphqlPendingRollupBody, 1)
	defer srv.Close()

	c := &GitHubChecker{HTTPClient: srv.Client(), Token: "test-token", BaseURL: srv.URL}
	url := "https://github.com/o/r/pull/1"
	results, _, err := c.CheckBatch(t.Context(), []string{url})
	if err != nil {
		t.Fatalf("CheckBatch() error = %v", err)
	}
	if results[url].CIBucket != "action_required" {
		t.Errorf("CheckBatch() result.CIBucket = %q, want action_required (a stuck approval-gated run must override a plain pending rollup)", results[url].CIBucket)
	}
}

func TestGitHubCheckerActionRequiredDoesNotOverrideFailure(t *testing.T) {
	body := strings.Replace(graphqlPendingRollupBody, `"state": "PENDING"`, `"state": "FAILURE"`, 1)
	srv := fakeGitHubServer(t, body, 1)
	defer srv.Close()

	c := &GitHubChecker{HTTPClient: srv.Client(), Token: "test-token", BaseURL: srv.URL}
	url := "https://github.com/o/r/pull/1"
	results, _, err := c.CheckBatch(t.Context(), []string{url})
	if err != nil {
		t.Fatalf("CheckBatch() error = %v", err)
	}
	if results[url].CIBucket != "failure" {
		t.Errorf("CheckBatch() result.CIBucket = %q, want failure (a real failure must win over action_required)", results[url].CIBucket)
	}
}

func TestGitHubCheckerErrorsOnNon2xxGraphQLStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"Bad credentials"}`))
	}))
	defer srv.Close()

	c := &GitHubChecker{HTTPClient: srv.Client(), Token: "bad-token", BaseURL: srv.URL}
	results, _, err := c.CheckBatch(t.Context(), []string{"https://github.com/o/r/pull/1"})
	if err == nil {
		t.Fatalf("CheckBatch() with a 401 graphql response, want error, got results = %+v", results)
	}
}

func TestGitHubCheckerErrorsOnNon2xxActionsRunsStatus(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/graphql", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(graphqlOpenSuccessBody))
	})
	mux.HandleFunc("/repos/o/r/actions/runs", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`internal server error`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &GitHubChecker{HTTPClient: srv.Client(), Token: "test-token", BaseURL: srv.URL}
	results, _, err := c.CheckBatch(t.Context(), []string{"https://github.com/o/r/pull/1"})
	if err == nil {
		t.Fatalf("CheckBatch() with a 500 actions/runs response, want error, got results = %+v", results)
	}
}

func TestGitHubCheckerRespectsContextTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()

	c := &GitHubChecker{HTTPClient: srv.Client(), Token: "test-token", BaseURL: srv.URL}
	if _, _, err := c.CheckBatch(ctx, []string{"https://github.com/o/r/pull/1"}); err == nil {
		t.Error("CheckBatch() with an expired context, want error")
	}
}

// GitHub answers GraphQL-level failures (rate limiting, NOT_FOUND, field
// errors) with HTTP 200 and a body-level `errors` array. Such a response
// must fail the whole batch: there's no per-PR data to attribute it to
// without parsing the error's `path`, which CheckBatch deliberately
// doesn't do.
func TestGitHubCheckerRejectsGraphQLErrors(t *testing.T) {
	const body = `{
	  "data": {},
	  "errors": [{ "message": "API rate limit exceeded" }]
	}`
	srv := fakeGitHubServer(t, body, 0)
	defer srv.Close()

	c := &GitHubChecker{HTTPClient: srv.Client(), Token: "test-token", BaseURL: srv.URL}
	results, _, err := c.CheckBatch(t.Context(), []string{"https://github.com/o/r/pull/1"})
	if err == nil {
		t.Fatalf("CheckBatch() on a GraphQL errors response, want error, got results = %+v", results)
	}
	if !strings.Contains(err.Error(), "API rate limit exceeded") {
		t.Errorf("CheckBatch() error = %v, want it to carry the GraphQL error message", err)
	}
}

// A null pullRequest (e.g. the PR number doesn't exist) arrives without any
// top-level `errors` array — since this is scoped to one PR within a
// batch, not a whole-request failure, CheckBatch itself must still
// succeed; the affected URL is simply omitted from the results map for the
// caller to treat as its own per-registration failure.
func TestGitHubCheckerOmitsNullPullRequestFromResults(t *testing.T) {
	const body = `{
	  "data": { "repo0": { "pr0": null } },
	  "rateLimit": { "limit": 5000, "remaining": 4999, "resetAt": "2026-09-20T23:00:00Z" }
	}`
	srv := fakeGitHubServer(t, body, 0)
	defer srv.Close()

	c := &GitHubChecker{HTTPClient: srv.Client(), Token: "test-token", BaseURL: srv.URL}
	url := "https://github.com/o/r/pull/1"
	results, _, err := c.CheckBatch(t.Context(), []string{url})
	if err != nil {
		t.Fatalf("CheckBatch() error = %v, want a null pullRequest to be a soft per-URL miss, not a batch error", err)
	}
	if _, ok := results[url]; ok {
		t.Errorf("results[%q] present, want it omitted for a null pullRequest", url)
	}
}

// checkActionRequired must escape its path components and ask for a full
// page of runs: GitHub's default page size is 30, small enough for workflow
// reruns to bury the one action_required run this cross-check exists for.
func TestGitHubCheckerActionRequiredQueryIsEscapedAndPaginated(t *testing.T) {
	var gotPath, gotQuery string
	mux := http.NewServeMux()
	mux.HandleFunc("/graphql", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(graphqlOpenSuccessBody))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.EscapedPath(), r.URL.Query().Encode()
		_ = json.NewEncoder(w).Encode(map[string]any{"workflow_runs": []map[string]any{}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &GitHubChecker{HTTPClient: srv.Client(), Token: "test-token", BaseURL: srv.URL}
	if _, _, err := c.CheckBatch(t.Context(), []string{"https://github.com/o/r/pull/1"}); err != nil {
		t.Fatalf("CheckBatch() error = %v", err)
	}
	if gotPath != "/repos/o/r/actions/runs" {
		t.Errorf("actions/runs path = %q, want /repos/o/r/actions/runs", gotPath)
	}
	if !strings.Contains(gotQuery, "per_page=100") {
		t.Errorf("actions/runs query = %q, want it to request per_page=100", gotQuery)
	}
	if !strings.Contains(gotQuery, "head_sha=deadbeef") {
		t.Errorf("actions/runs query = %q, want head_sha=deadbeef", gotQuery)
	}
}

// TestGitHubCheckerBatchesMultiplePRsIntoOneGraphQLRequest is the actual
// point of CheckBatch: N due PRs — including two sharing one repo — must
// resolve in exactly one GraphQL round trip, not N.
func TestGitHubCheckerBatchesMultiplePRsIntoOneGraphQLRequest(t *testing.T) {
	// buildGitHubBatchQuery aliases repos/PRs in first-seen order, so for
	// this input: repo0 = o/r (pr0 = #1, pr1 = #2), repo1 = x/y (pr0 = #5).
	const batchBody = `{
	  "data": {
	    "repo0": {
	      "pr0": { "state": "OPEN", "title": "PR one", "headRefOid": "sha1", "reviewCount": {"totalCount":0}, "latestReview": {"nodes":[]}, "commits": {"nodes":[]} },
	      "pr1": { "state": "OPEN", "title": "PR two", "headRefOid": "sha2", "reviewCount": {"totalCount":0}, "latestReview": {"nodes":[]}, "commits": {"nodes":[]} }
	    },
	    "repo1": {
	      "pr0": { "state": "OPEN", "title": "PR five", "headRefOid": "sha5", "reviewCount": {"totalCount":0}, "latestReview": {"nodes":[]}, "commits": {"nodes":[]} }
	    }
	  },
	  "rateLimit": { "limit": 5000, "remaining": 4995, "resetAt": "2026-09-20T23:00:00Z" }
	}`

	var graphqlCalls int
	mux := http.NewServeMux()
	mux.HandleFunc("/graphql", func(w http.ResponseWriter, r *http.Request) {
		graphqlCalls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(batchBody))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"workflow_runs": []map[string]any{}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &GitHubChecker{HTTPClient: srv.Client(), Token: "test-token", BaseURL: srv.URL}
	urls := []string{
		"https://github.com/o/r/pull/1",
		"https://github.com/o/r/pull/2",
		"https://github.com/x/y/pull/5",
	}
	results, _, err := c.CheckBatch(t.Context(), urls)
	if err != nil {
		t.Fatalf("CheckBatch() error = %v", err)
	}

	if graphqlCalls != 1 {
		t.Errorf("graphql requests = %d, want 1 (one batched call for all 3 PRs)", graphqlCalls)
	}
	want := map[string]string{
		"https://github.com/o/r/pull/1": "PR one",
		"https://github.com/o/r/pull/2": "PR two",
		"https://github.com/x/y/pull/5": "PR five",
	}
	for url, wantTitle := range want {
		got, ok := results[url]
		if !ok {
			t.Errorf("results[%q] missing", url)
			continue
		}
		if got.Title != wantTitle {
			t.Errorf("results[%q].Title = %q, want %q", url, got.Title, wantTitle)
		}
	}
}
