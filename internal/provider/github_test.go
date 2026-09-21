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
// rate-limit signal on each.
func fakeGitHubServer(t *testing.T, graphqlBody string, actionRequiredCount int) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/graphql", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(graphqlBody))
	})
	mux.HandleFunc("/repos/o/r/actions/runs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Remaining", "4999")
		w.Header().Set("X-RateLimit-Reset", "9999999999")
		runs := map[string]any{"workflow_runs": []map[string]any{}}
		if actionRequiredCount > 0 {
			runList := []map[string]any{}
			for i := 0; i < actionRequiredCount; i++ {
				runList = append(runList, map[string]any{"conclusion": "action_required"})
			}
			runs["workflow_runs"] = runList
		}
		json.NewEncoder(w).Encode(runs)
	})
	return httptest.NewServer(mux)
}

const graphqlOpenSuccessBody = `{
  "data": {
    "repository": { "pullRequest": {
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
	result, budgets, err := c.Check(t.Context(), "https://github.com/o/r/pull/1")
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if result.State != "open" || result.Title != "Fix the thing" || result.Description != "This fixes the thing by doing the other thing." || result.ReviewCount != 2 || result.LatestReviewer != "alice" || result.LatestReviewState != "APPROVED" || result.CIBucket != "success" {
		t.Errorf("Check() result = %+v", result)
	}
	if budgets.GraphQL.Remaining != 4998 || budgets.REST.Remaining != 4999 {
		t.Errorf("Check() budgets = %+v", budgets)
	}
}

const graphqlMergedBody = `{
  "data": { "repository": { "pullRequest": { "state": "MERGED", "headRefOid": "x", "reviewCount": {"totalCount":0}, "latestReview": {"nodes":[]}, "commits": {"nodes":[]} } } },
  "rateLimit": { "limit": 5000, "remaining": 4997, "resetAt": "2026-09-20T23:00:00Z" }
}`

func TestGitHubCheckerMerged(t *testing.T) {
	srv := fakeGitHubServer(t, graphqlMergedBody, 0)
	defer srv.Close()

	c := &GitHubChecker{HTTPClient: srv.Client(), Token: "test-token", BaseURL: srv.URL}
	result, _, err := c.Check(t.Context(), "https://github.com/o/r/pull/1")
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if result.State != "merged" {
		t.Errorf("Check().State = %q, want merged", result.State)
	}
}

const graphqlPendingRollupBody = `{
  "data": { "repository": { "pullRequest": {
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
	result, _, err := c.Check(t.Context(), "https://github.com/o/r/pull/1")
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if result.CIBucket != "action_required" {
		t.Errorf("Check().CIBucket = %q, want action_required (a stuck approval-gated run must override a plain pending rollup)", result.CIBucket)
	}
}

func TestGitHubCheckerActionRequiredDoesNotOverrideFailure(t *testing.T) {
	body := strings.Replace(graphqlPendingRollupBody, `"state": "PENDING"`, `"state": "FAILURE"`, 1)
	srv := fakeGitHubServer(t, body, 1)
	defer srv.Close()

	c := &GitHubChecker{HTTPClient: srv.Client(), Token: "test-token", BaseURL: srv.URL}
	result, _, err := c.Check(t.Context(), "https://github.com/o/r/pull/1")
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if result.CIBucket != "failure" {
		t.Errorf("Check().CIBucket = %q, want failure (a real failure must win over action_required)", result.CIBucket)
	}
}

func TestGitHubCheckerErrorsOnNon2xxGraphQLStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"message":"Bad credentials"}`))
	}))
	defer srv.Close()

	c := &GitHubChecker{HTTPClient: srv.Client(), Token: "bad-token", BaseURL: srv.URL}
	result, _, err := c.Check(t.Context(), "https://github.com/o/r/pull/1")
	if err == nil {
		t.Fatalf("Check() with a 401 graphql response, want error, got result = %+v", result)
	}
}

func TestGitHubCheckerErrorsOnNon2xxActionsRunsStatus(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/graphql", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(graphqlOpenSuccessBody))
	})
	mux.HandleFunc("/repos/o/r/actions/runs", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`internal server error`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &GitHubChecker{HTTPClient: srv.Client(), Token: "test-token", BaseURL: srv.URL}
	result, _, err := c.Check(t.Context(), "https://github.com/o/r/pull/1")
	if err == nil {
		t.Fatalf("Check() with a 500 actions/runs response, want error, got result = %+v", result)
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
	if _, _, err := c.Check(ctx, "https://github.com/o/r/pull/1"); err == nil {
		t.Error("Check() with an expired context, want error")
	}
}

// GitHub answers GraphQL-level failures (rate limiting, NOT_FOUND, field
// errors) with HTTP 200 and a body-level `errors` array. Such a response
// must be a hard error: decoded as data it becomes a zero-value "open PR,
// no reviews, no CI" that gets persisted as PrevState and makes the next
// successful poll re-fire every event as if it were new.
func TestGitHubCheckerRejectsGraphQLErrors(t *testing.T) {
	const body = `{
	  "data": { "repository": null },
	  "errors": [{ "message": "API rate limit exceeded" }]
	}`
	srv := fakeGitHubServer(t, body, 0)
	defer srv.Close()

	c := &GitHubChecker{HTTPClient: srv.Client(), Token: "test-token", BaseURL: srv.URL}
	result, _, err := c.Check(t.Context(), "https://github.com/o/r/pull/1")
	if err == nil {
		t.Fatalf("Check() on a GraphQL errors response, want error, got result = %+v", result)
	}
	if !strings.Contains(err.Error(), "API rate limit exceeded") {
		t.Errorf("Check() error = %v, want it to carry the GraphQL error message", err)
	}
}

// A null pullRequest (e.g. the PR number doesn't exist) arrives without any
// `errors` array at all, so it needs its own guard: mapGitHubState("")
// would otherwise report it as an open PR.
func TestGitHubCheckerRejectsNullPullRequest(t *testing.T) {
	const body = `{
	  "data": { "repository": { "pullRequest": null } },
	  "rateLimit": { "limit": 5000, "remaining": 4999, "resetAt": "2026-09-20T23:00:00Z" }
	}`
	srv := fakeGitHubServer(t, body, 0)
	defer srv.Close()

	c := &GitHubChecker{HTTPClient: srv.Client(), Token: "test-token", BaseURL: srv.URL}
	result, _, err := c.Check(t.Context(), "https://github.com/o/r/pull/1")
	if err == nil {
		t.Fatalf("Check() on a null pullRequest, want error, got result = %+v", result)
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
		w.Write([]byte(graphqlOpenSuccessBody))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.EscapedPath(), r.URL.Query().Encode()
		json.NewEncoder(w).Encode(map[string]any{"workflow_runs": []map[string]any{}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &GitHubChecker{HTTPClient: srv.Client(), Token: "test-token", BaseURL: srv.URL}
	if _, _, err := c.Check(t.Context(), "https://github.com/o/r/pull/1"); err != nil {
		t.Fatalf("Check() error = %v", err)
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
