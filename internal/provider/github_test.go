package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
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
    } },
    "rateLimit": { "limit": 5000, "remaining": 4998, "resetAt": "2026-09-20T23:00:00Z" }
  }
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
  "data": {
    "repo0": { "pr0": { "state": "MERGED", "headRefOid": "x", "mergedAt": "2026-09-21T08:30:00Z", "reviewCount": {"totalCount":0}, "latestReview": {"nodes":[]}, "commits": {"nodes":[]} } },
    "rateLimit": { "limit": 5000, "remaining": 4997, "resetAt": "2026-09-20T23:00:00Z" }
  }
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
	if results[url].MergedAt.IsZero() {
		t.Error("CheckBatch() result.MergedAt is zero, want the parsed mergedAt")
	}
}

func TestGitHubCheckerReviewTimestamp(t *testing.T) {
	body := strings.Replace(graphqlOpenSuccessBody,
		`"author": { "login": "alice" }, "state": "APPROVED"`,
		`"author": { "login": "alice" }, "state": "APPROVED", "submittedAt": "2026-09-20T12:00:00Z"`,
		1)
	srv := fakeGitHubServer(t, body, 0)
	defer srv.Close()

	c := &GitHubChecker{HTTPClient: srv.Client(), Token: "test-token", BaseURL: srv.URL}
	url := "https://github.com/o/r/pull/1"
	results, _, err := c.CheckBatch(t.Context(), []string{url})
	if err != nil {
		t.Fatalf("CheckBatch() error = %v", err)
	}
	if results[url].LatestReviewAt.IsZero() {
		t.Error("CheckBatch() result.LatestReviewAt is zero, want the parsed submittedAt")
	}
}

func TestGitHubCheckerDismissedReviewTreatedAsNoReview(t *testing.T) {
	body := strings.Replace(graphqlOpenSuccessBody,
		`"author": { "login": "alice" }, "state": "APPROVED"`,
		`"author": { "login": "alice" }, "state": "DISMISSED"`,
		1)
	srv := fakeGitHubServer(t, body, 0)
	defer srv.Close()

	c := &GitHubChecker{HTTPClient: srv.Client(), Token: "test-token", BaseURL: srv.URL}
	url := "https://github.com/o/r/pull/1"
	results, _, err := c.CheckBatch(t.Context(), []string{url})
	if err != nil {
		t.Fatalf("CheckBatch() error = %v", err)
	}
	got := results[url]
	if got.LatestReviewState != "" || got.LatestReviewer != "" || got.ReviewHref != "" || !got.LatestReviewAt.IsZero() {
		t.Errorf("CheckBatch() result = %+v, want a dismissed review to look exactly like no review at all", got)
	}
}

func TestGitHubCheckerUnresolvedCountsOnlyUnresolvedThreads(t *testing.T) {
	body := strings.Replace(graphqlOpenSuccessBody,
		`"commits": { "nodes": [{ "commit": { "statusCheckRollup": { "state": "SUCCESS" } } }] }`,
		`"reviewThreads": { "nodes": [{ "id": "t1", "isResolved": false }, { "id": "t2", "isResolved": true }, { "id": "t3", "isResolved": false }] },
      "commits": { "nodes": [{ "commit": { "statusCheckRollup": { "state": "SUCCESS" } } }] }`,
		1)
	srv := fakeGitHubServer(t, body, 0)
	defer srv.Close()

	c := &GitHubChecker{HTTPClient: srv.Client(), Token: "test-token", BaseURL: srv.URL}
	url := "https://github.com/o/r/pull/1"
	results, _, err := c.CheckBatch(t.Context(), []string{url})
	if err != nil {
		t.Fatalf("CheckBatch() error = %v", err)
	}
	if !slices.Equal(results[url].UnresolvedThreadIDs, []string{"t1", "t3"}) {
		t.Errorf("CheckBatch() result.UnresolvedThreadIDs = %v, want [t1 t3] (only the two isResolved:false threads, t2 excluded)", results[url].UnresolvedThreadIDs)
	}
}

func TestGitHubCheckerUnresolvedEmptyWithoutReviewThreads(t *testing.T) {
	srv := fakeGitHubServer(t, graphqlOpenSuccessBody, 0)
	defer srv.Close()

	c := &GitHubChecker{HTTPClient: srv.Client(), Token: "test-token", BaseURL: srv.URL}
	url := "https://github.com/o/r/pull/1"
	results, _, err := c.CheckBatch(t.Context(), []string{url})
	if err != nil {
		t.Fatalf("CheckBatch() error = %v", err)
	}
	if len(results[url].UnresolvedThreadIDs) != 0 {
		t.Errorf("CheckBatch() result.UnresolvedThreadIDs = %v, want empty when the PR has no review threads at all", results[url].UnresolvedThreadIDs)
	}
}

func TestGitHubCheckerTopLevelCommentsTracked(t *testing.T) {
	body := strings.Replace(graphqlOpenSuccessBody,
		`"commits": { "nodes": [{ "commit": { "statusCheckRollup": { "state": "SUCCESS" } } }] }`,
		`"comments": { "nodes": [{ "id": "c1", "url": "https://github.com/o/r/pull/1#issuecomment-1", "body": "first", "author": { "login": "alice" } }, { "id": "c2", "url": "https://github.com/o/r/pull/1#issuecomment-2", "body": "second", "author": { "login": "bob" } }] },
      "commits": { "nodes": [{ "commit": { "statusCheckRollup": { "state": "SUCCESS" } } }] }`,
		1)
	srv := fakeGitHubServer(t, body, 0)
	defer srv.Close()

	c := &GitHubChecker{HTTPClient: srv.Client(), Token: "test-token", BaseURL: srv.URL}
	url := "https://github.com/o/r/pull/1"
	results, _, err := c.CheckBatch(t.Context(), []string{url})
	if err != nil {
		t.Fatalf("CheckBatch() error = %v", err)
	}
	got := results[url]
	if !slices.Equal(got.TopLevelCommentIDs, []string{"c1", "c2"}) {
		t.Errorf("CheckBatch() result.TopLevelCommentIDs = %v, want [c1 c2]", got.TopLevelCommentIDs)
	}
	if got.LatestCommentHref != "https://github.com/o/r/pull/1#issuecomment-2" {
		t.Errorf("CheckBatch() result.LatestCommentHref = %q, want the last node's url (comments(last: N) is chronological ascending)", got.LatestCommentHref)
	}
	if got.LatestCommentAuthor != "bob" {
		t.Errorf("CheckBatch() result.LatestCommentAuthor = %q, want the last node's author (bob)", got.LatestCommentAuthor)
	}
	want := []TopLevelComment{
		{ID: "c1", Href: "https://github.com/o/r/pull/1#issuecomment-1", Author: "alice", Body: "first"},
		{ID: "c2", Href: "https://github.com/o/r/pull/1#issuecomment-2", Author: "bob", Body: "second"},
	}
	if !slices.Equal(got.TopLevelComments, want) {
		t.Errorf("CheckBatch() result.TopLevelComments = %+v, want %+v (raw comments with bodies, for filter rules)", got.TopLevelComments, want)
	}
}

func TestGitHubCheckerTopLevelCommentsEmptyWithoutComments(t *testing.T) {
	srv := fakeGitHubServer(t, graphqlOpenSuccessBody, 0)
	defer srv.Close()

	c := &GitHubChecker{HTTPClient: srv.Client(), Token: "test-token", BaseURL: srv.URL}
	url := "https://github.com/o/r/pull/1"
	results, _, err := c.CheckBatch(t.Context(), []string{url})
	if err != nil {
		t.Fatalf("CheckBatch() error = %v", err)
	}
	if len(results[url].TopLevelCommentIDs) != 0 || results[url].LatestCommentHref != "" {
		t.Errorf("CheckBatch() result = %+v, want empty TopLevelCommentIDs/LatestCommentHref when the PR has no top-level comments", results[url])
	}
}

func TestGitHubCheckerCIDetailFromFailingRun(t *testing.T) {
	body := strings.Replace(graphqlPendingRollupBody, `"state": "PENDING"`, `"state": "FAILURE"`, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/graphql", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	})
	mux.HandleFunc("/repos/o/r/actions/runs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Remaining", "4999")
		w.Header().Set("X-RateLimit-Reset", "9999999999")
		runs := map[string]any{"workflow_runs": []map[string]any{
			{"name": "test", "conclusion": "success"},
			{"name": "build", "conclusion": "failure"},
		}}
		_ = json.NewEncoder(w).Encode(runs)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &GitHubChecker{HTTPClient: srv.Client(), Token: "test-token", BaseURL: srv.URL}
	url := "https://github.com/o/r/pull/1"
	results, _, err := c.CheckBatch(t.Context(), []string{url})
	if err != nil {
		t.Fatalf("CheckBatch() error = %v", err)
	}
	if results[url].CIBucket != "failure" || results[url].CIDetail != "build" {
		t.Errorf("CheckBatch() result = %+v, want CIBucket failure, CIDetail build", results[url])
	}
}

// TestGitHubCheckerNeverCitesCancelledRunAsCIDetail is a regression test:
// confirmed live — a force-push mid-run cancelled a CodeQL run (via a
// `concurrency: cancel-in-progress` workflow), and it got cited as "the
// failing job" in a CI-failure ping even though nothing had actually
// failed there; the run that really failed elsewhere is what CIDetail must
// name instead.
func TestGitHubCheckerNeverCitesCancelledRunAsCIDetail(t *testing.T) {
	body := strings.Replace(graphqlPendingRollupBody, `"state": "PENDING"`, `"state": "FAILURE"`, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/graphql", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	})
	mux.HandleFunc("/repos/o/r/actions/runs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Remaining", "4999")
		w.Header().Set("X-RateLimit-Reset", "9999999999")
		runs := map[string]any{"workflow_runs": []map[string]any{
			{"name": "CodeQL", "conclusion": "cancelled"},
			{"name": "build", "conclusion": "failure"},
		}}
		_ = json.NewEncoder(w).Encode(runs)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &GitHubChecker{HTTPClient: srv.Client(), Token: "test-token", BaseURL: srv.URL}
	url := "https://github.com/o/r/pull/1"
	results, _, err := c.CheckBatch(t.Context(), []string{url})
	if err != nil {
		t.Fatalf("CheckBatch() error = %v", err)
	}
	if results[url].CIDetail != "build" {
		t.Errorf("CheckBatch() result = %+v, want CIDetail %q (never the merely-cancelled CodeQL run)", results[url], "build")
	}
}

// TestGitHubCheckerCIDetailEmptyWhenOnlyCancelledRunsExist covers the case
// where a real failing run genuinely isn't in this page at all (e.g. it's
// a non-Actions status check the /actions/runs endpoint can't see) —
// CIDetail/CIHref must stay empty rather than naming a cancelled run as if
// it explained the failure.
func TestGitHubCheckerCIDetailEmptyWhenOnlyCancelledRunsExist(t *testing.T) {
	body := strings.Replace(graphqlPendingRollupBody, `"state": "PENDING"`, `"state": "FAILURE"`, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/graphql", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	})
	mux.HandleFunc("/repos/o/r/actions/runs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Remaining", "4999")
		w.Header().Set("X-RateLimit-Reset", "9999999999")
		runs := map[string]any{"workflow_runs": []map[string]any{
			{"name": "CodeQL", "conclusion": "cancelled"},
		}}
		_ = json.NewEncoder(w).Encode(runs)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &GitHubChecker{HTTPClient: srv.Client(), Token: "test-token", BaseURL: srv.URL}
	url := "https://github.com/o/r/pull/1"
	results, _, err := c.CheckBatch(t.Context(), []string{url})
	if err != nil {
		t.Fatalf("CheckBatch() error = %v", err)
	}
	if results[url].CIDetail != "" || results[url].CIHref != "" {
		t.Errorf("CheckBatch() result = %+v, want empty CIDetail/CIHref rather than naming the cancelled run", results[url])
	}
}

// TestGitHubCheckerDowngradesFailureExplainedByCancellation is a regression
// test: confirmed live, agent-of-empires/agent-of-empires#4138 pinged CI
// failure — with an already-empty CIDetail/CIHref, since the earlier fix
// stopped blaming a cancelled run for those — because GitHub's rollup
// itself reports FAILURE for a commit whose only non-success run was
// cancelled (superseded by a force-push). When every run for this SHA is
// terminal, none genuinely failed, none is action_required, and at least
// one is cancelled, the bucket itself must downgrade too, not just
// CIDetail/CIHref.
func TestGitHubCheckerDowngradesFailureExplainedByCancellation(t *testing.T) {
	body := strings.Replace(graphqlPendingRollupBody, `"state": "PENDING"`, `"state": "FAILURE"`, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/graphql", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	})
	mux.HandleFunc("/repos/o/r/actions/runs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Remaining", "4999")
		w.Header().Set("X-RateLimit-Reset", "9999999999")
		runs := map[string]any{"workflow_runs": []map[string]any{
			{"name": "CodeQL", "status": "completed", "conclusion": "cancelled"},
			{"name": "PR Title Check", "status": "completed", "conclusion": "success"},
		}}
		_ = json.NewEncoder(w).Encode(runs)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &GitHubChecker{HTTPClient: srv.Client(), Token: "test-token", BaseURL: srv.URL}
	url := "https://github.com/o/r/pull/1"
	results, _, err := c.CheckBatch(t.Context(), []string{url})
	if err != nil {
		t.Fatalf("CheckBatch() error = %v", err)
	}
	if results[url].CIBucket != "pending" {
		t.Errorf("CheckBatch() result.CIBucket = %q, want pending (fully explained by the cancelled run, nothing genuinely failed)", results[url].CIBucket)
	}
}

// TestGitHubCheckerKeepsFailureWithoutAnyCancelledRun guards against
// over-eager downgrading: a rollup FAILURE with every Actions run
// succeeding (no cancelled run at all) means something outside Actions — a
// different status-check provider this REST call can't see — is the real
// cause, so it must never be papered over just because Actions itself looks
// clean.
func TestGitHubCheckerKeepsFailureWithoutAnyCancelledRun(t *testing.T) {
	body := strings.Replace(graphqlPendingRollupBody, `"state": "PENDING"`, `"state": "FAILURE"`, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/graphql", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	})
	mux.HandleFunc("/repos/o/r/actions/runs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Remaining", "4999")
		w.Header().Set("X-RateLimit-Reset", "9999999999")
		runs := map[string]any{"workflow_runs": []map[string]any{
			{"name": "PR Title Check", "status": "completed", "conclusion": "success"},
		}}
		_ = json.NewEncoder(w).Encode(runs)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &GitHubChecker{HTTPClient: srv.Client(), Token: "test-token", BaseURL: srv.URL}
	url := "https://github.com/o/r/pull/1"
	results, _, err := c.CheckBatch(t.Context(), []string{url})
	if err != nil {
		t.Fatalf("CheckBatch() error = %v", err)
	}
	if results[url].CIBucket != "failure" {
		t.Errorf("CheckBatch() result.CIBucket = %q, want failure kept as-is (nothing cancelled, so nothing here explains the rollup's FAILURE)", results[url].CIBucket)
	}
}

// TestGitHubCheckerKeepsFailureWhileARunIsStillInProgress guards the other
// direction: a run still queued/in_progress might yet fail, so a cancelled
// run elsewhere can't be trusted to fully explain the rollup until every
// run is terminal.
func TestGitHubCheckerKeepsFailureWhileARunIsStillInProgress(t *testing.T) {
	body := strings.Replace(graphqlPendingRollupBody, `"state": "PENDING"`, `"state": "FAILURE"`, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/graphql", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	})
	mux.HandleFunc("/repos/o/r/actions/runs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Remaining", "4999")
		w.Header().Set("X-RateLimit-Reset", "9999999999")
		runs := map[string]any{"workflow_runs": []map[string]any{
			{"name": "CodeQL", "status": "completed", "conclusion": "cancelled"},
			{"name": "Tests", "status": "in_progress", "conclusion": ""},
		}}
		_ = json.NewEncoder(w).Encode(runs)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &GitHubChecker{HTTPClient: srv.Client(), Token: "test-token", BaseURL: srv.URL}
	url := "https://github.com/o/r/pull/1"
	results, _, err := c.CheckBatch(t.Context(), []string{url})
	if err != nil {
		t.Fatalf("CheckBatch() error = %v", err)
	}
	if results[url].CIBucket != "failure" {
		t.Errorf("CheckBatch() result.CIBucket = %q, want failure kept as-is (Tests is still in_progress, might yet fail)", results[url].CIBucket)
	}
}

const graphqlPendingRollupBody = `{
  "data": {
    "repo0": { "pr0": {
      "state": "OPEN", "headRefOid": "deadbeef",
      "reviewCount": {"totalCount":0}, "latestReview": {"nodes":[]},
      "commits": { "nodes": [{ "commit": { "statusCheckRollup": { "state": "PENDING" } } }] }
    } },
    "rateLimit": { "limit": 5000, "remaining": 4996, "resetAt": "2026-09-20T23:00:00Z" }
  }
}`

func TestGitHubCheckerHasConflictFromMergeableState(t *testing.T) {
	body := strings.Replace(graphqlOpenSuccessBody, `"headRefOid": "deadbeef",`, `"headRefOid": "deadbeef", "mergeable": "CONFLICTING",`, 1)
	srv := fakeGitHubServer(t, body, 0)
	defer srv.Close()

	c := &GitHubChecker{HTTPClient: srv.Client(), Token: "test-token", BaseURL: srv.URL}
	url := "https://github.com/o/r/pull/1"
	results, _, err := c.CheckBatch(t.Context(), []string{url})
	if err != nil {
		t.Fatalf("CheckBatch() error = %v", err)
	}
	if !results[url].HasConflict {
		t.Error("CheckBatch() result.HasConflict = false, want true when mergeable is CONFLICTING")
	}
}

func TestGitHubCheckerNoConflictWhenMergeable(t *testing.T) {
	srv := fakeGitHubServer(t, graphqlOpenSuccessBody, 0)
	defer srv.Close()

	c := &GitHubChecker{HTTPClient: srv.Client(), Token: "test-token", BaseURL: srv.URL}
	url := "https://github.com/o/r/pull/1"
	results, _, err := c.CheckBatch(t.Context(), []string{url})
	if err != nil {
		t.Fatalf("CheckBatch() error = %v", err)
	}
	if results[url].HasConflict {
		t.Error("CheckBatch() result.HasConflict = true, want false when mergeable field is absent/MERGEABLE")
	}
}

func TestGitHubCheckerReviewHrefFromLatestReviewURL(t *testing.T) {
	body := strings.Replace(graphqlOpenSuccessBody,
		`"author": { "login": "alice" }, "state": "APPROVED"`,
		`"author": { "login": "alice" }, "state": "APPROVED", "url": "https://github.com/o/r/pull/1#pullrequestreview-999"`,
		1)
	srv := fakeGitHubServer(t, body, 0)
	defer srv.Close()

	c := &GitHubChecker{HTTPClient: srv.Client(), Token: "test-token", BaseURL: srv.URL}
	url := "https://github.com/o/r/pull/1"
	results, _, err := c.CheckBatch(t.Context(), []string{url})
	if err != nil {
		t.Fatalf("CheckBatch() error = %v", err)
	}
	if results[url].ReviewHref != "https://github.com/o/r/pull/1#pullrequestreview-999" {
		t.Errorf("CheckBatch() result.ReviewHref = %q, want the review's own url", results[url].ReviewHref)
	}
}

func TestGitHubCheckerCIHrefFromFailingRunURL(t *testing.T) {
	body := strings.Replace(graphqlPendingRollupBody, `"state": "PENDING"`, `"state": "FAILURE"`, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/graphql", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	})
	mux.HandleFunc("/repos/o/r/actions/runs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Remaining", "4999")
		w.Header().Set("X-RateLimit-Reset", "9999999999")
		runs := map[string]any{"workflow_runs": []map[string]any{
			{"name": "build", "conclusion": "failure", "html_url": "https://github.com/o/r/actions/runs/123"},
		}}
		_ = json.NewEncoder(w).Encode(runs)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &GitHubChecker{HTTPClient: srv.Client(), Token: "test-token", BaseURL: srv.URL}
	url := "https://github.com/o/r/pull/1"
	results, _, err := c.CheckBatch(t.Context(), []string{url})
	if err != nil {
		t.Fatalf("CheckBatch() error = %v", err)
	}
	if results[url].CIHref != "https://github.com/o/r/actions/runs/123" {
		t.Errorf("CheckBatch() result.CIHref = %q, want the failing run's html_url", results[url].CIHref)
	}
}

func TestGitHubCheckerMergeQueueEntry(t *testing.T) {
	body := strings.Replace(graphqlOpenSuccessBody,
		`"headRefOid": "deadbeef",`,
		`"headRefOid": "deadbeef", "mergeQueueEntry": { "state": "QUEUED", "position": 3 },`,
		1)
	srv := fakeGitHubServer(t, body, 0)
	defer srv.Close()

	c := &GitHubChecker{HTTPClient: srv.Client(), Token: "test-token", BaseURL: srv.URL}
	url := "https://github.com/o/r/pull/1"
	results, _, err := c.CheckBatch(t.Context(), []string{url})
	if err != nil {
		t.Fatalf("CheckBatch() error = %v", err)
	}
	if results[url].MergeQueueState != "QUEUED" || results[url].MergeQueuePosition != 3 {
		t.Errorf("CheckBatch() result = %+v, want MergeQueueState QUEUED at position 3", results[url])
	}
}

func TestGitHubCheckerNoMergeQueueEntryWhenNotQueued(t *testing.T) {
	srv := fakeGitHubServer(t, graphqlOpenSuccessBody, 0)
	defer srv.Close()

	c := &GitHubChecker{HTTPClient: srv.Client(), Token: "test-token", BaseURL: srv.URL}
	url := "https://github.com/o/r/pull/1"
	results, _, err := c.CheckBatch(t.Context(), []string{url})
	if err != nil {
		t.Fatalf("CheckBatch() error = %v", err)
	}
	if results[url].MergeQueueState != "" {
		t.Errorf("CheckBatch() result.MergeQueueState = %q, want empty when mergeQueueEntry is null/absent", results[url].MergeQueueState)
	}
}

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

func TestGitHubCheckerActionRequiredCIDetailAndHref(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/graphql", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(graphqlPendingRollupBody))
	})
	mux.HandleFunc("/repos/o/r/actions/runs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Remaining", "4999")
		w.Header().Set("X-RateLimit-Reset", "9999999999")
		runs := map[string]any{"workflow_runs": []map[string]any{
			{"name": "deploy", "conclusion": "action_required", "html_url": "https://github.com/o/r/actions/runs/456"},
		}}
		_ = json.NewEncoder(w).Encode(runs)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &GitHubChecker{HTTPClient: srv.Client(), Token: "test-token", BaseURL: srv.URL}
	url := "https://github.com/o/r/pull/1"
	results, _, err := c.CheckBatch(t.Context(), []string{url})
	if err != nil {
		t.Fatalf("CheckBatch() error = %v", err)
	}
	if results[url].CIBucket != "action_required" {
		t.Fatalf("CheckBatch() result.CIBucket = %q, want action_required", results[url].CIBucket)
	}
	if results[url].CIDetail != "deploy" || results[url].CIHref != "https://github.com/o/r/actions/runs/456" {
		t.Errorf("CheckBatch() result = %+v, want CIDetail deploy and the matching run's href", results[url])
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
	// Regression test for github.com/agent-of-empires/agent-of-empires/pull/4083
	// (confirmed live): the rollup says failure but the only non-succeeding
	// run on this page is action_required (fakeGitHubServer's
	// actionRequiredCount=1 above), so there's no REST-visible run that
	// actually matches "failure" — CIDetail/CIHref must stay empty rather
	// than naming/linking the action_required run, which isn't what failed.
	if results[url].CIDetail != "" || results[url].CIHref != "" {
		t.Errorf("CheckBatch() result = %+v, want empty CIDetail/CIHref — the only non-succeeding run here is action_required, not the actual reason CIBucket is failure", results[url])
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
	  "data": {
	    "repo0": { "pr0": null },
	    "rateLimit": { "limit": 5000, "remaining": 4999, "resetAt": "2026-09-20T23:00:00Z" }
	  }
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
	    },
	    "rateLimit": { "limit": 5000, "remaining": 4995, "resetAt": "2026-09-20T23:00:00Z" }
	  }
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
	results, budgets, err := c.CheckBatch(t.Context(), urls)
	if err != nil {
		t.Fatalf("CheckBatch() error = %v", err)
	}

	if graphqlCalls != 1 {
		t.Errorf("graphql requests = %d, want 1 (one batched call for all 3 PRs)", graphqlCalls)
	}
	// Regression test for the rateLimit field being read from the wrong
	// JSON level (confirmed live: every "github rate limit" log line
	// showed graphql_remaining=0/graphql_limit=0 in production). `rateLimit`
	// is a root query field exactly like `repo0`/`repo1`, so it nests
	// inside `data` in the real response too — this must still parse
	// correctly alongside multiple repo aliases, not just a single one.
	if budgets.GraphQL.Remaining != 4995 || budgets.GraphQL.Limit != 5000 {
		t.Errorf("CheckBatch() budgets.GraphQL = %+v, want {Limit:5000 Remaining:4995 ...}", budgets.GraphQL)
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

func TestDiscoverBranchPRsFindsOpenPR(t *testing.T) {
	srv := dispatchGraphQLServer(t, map[string]string{
		"pullRequests(headRefName:": `{"data": {
			"repo0": {"branch0": {"nodes": [{"url": "https://github.com/o/r/pull/9"}]}},
			"rateLimit": {"limit": 5000, "remaining": 4999, "resetAt": "2026-09-20T23:00:00Z"}
		}}`,
	})
	defer srv.Close()

	c := &GitHubChecker{HTTPClient: srv.Client(), Token: "t", BaseURL: srv.URL}
	results, budget, err := c.DiscoverBranchPRs(t.Context(), []GitHubBranchQuery{{Owner: "o", Repo: "r", Branch: "feature-x"}})
	if err != nil {
		t.Fatalf("DiscoverBranchPRs() error = %v", err)
	}
	if budget.Remaining != 4999 {
		t.Errorf("budget.Remaining = %d, want 4999", budget.Remaining)
	}
	key := GitHubBranchKey("o", "r", "feature-x")
	if got := results[key]; got != "https://github.com/o/r/pull/9" {
		t.Errorf("results[%q] = %q, want the discovered PR URL", key, got)
	}
}

func TestDiscoverBranchPRsOmitsBranchesWithNoOpenPR(t *testing.T) {
	srv := dispatchGraphQLServer(t, map[string]string{
		"pullRequests(headRefName:": `{"data": {
			"repo0": {"branch0": {"nodes": []}},
			"rateLimit": {"limit": 5000, "remaining": 4999, "resetAt": "2026-09-20T23:00:00Z"}
		}}`,
	})
	defer srv.Close()

	c := &GitHubChecker{HTTPClient: srv.Client(), Token: "t", BaseURL: srv.URL}
	results, _, err := c.DiscoverBranchPRs(t.Context(), []GitHubBranchQuery{{Owner: "o", Repo: "r", Branch: "no-pr-yet"}})
	if err != nil {
		t.Fatalf("DiscoverBranchPRs() error = %v", err)
	}
	if len(results) != 0 {
		t.Errorf("results = %v, want empty for a branch with no open PR", results)
	}
}

func TestDiscoverBranchPRsBatchesAcrossRepos(t *testing.T) {
	var graphqlCalls int
	mux := http.NewServeMux()
	mux.HandleFunc("/graphql", func(w http.ResponseWriter, r *http.Request) {
		graphqlCalls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data": {
			"repo0": {"branch0": {"nodes": [{"url": "https://github.com/o/r/pull/1"}]}},
			"repo1": {"branch0": {"nodes": []}},
			"rateLimit": {"limit": 5000, "remaining": 4998, "resetAt": "2026-09-20T23:00:00Z"}
		}}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &GitHubChecker{HTTPClient: srv.Client(), Token: "t", BaseURL: srv.URL}
	results, _, err := c.DiscoverBranchPRs(t.Context(), []GitHubBranchQuery{
		{Owner: "o", Repo: "r", Branch: "feature-x"},
		{Owner: "x", Repo: "y", Branch: "feature-z"},
	})
	if err != nil {
		t.Fatalf("DiscoverBranchPRs() error = %v", err)
	}
	if graphqlCalls != 1 {
		t.Errorf("graphql requests = %d, want 1 (one batched call across both repos)", graphqlCalls)
	}
	if len(results) != 1 || results[GitHubBranchKey("o", "r", "feature-x")] != "https://github.com/o/r/pull/1" {
		t.Errorf("results = %v, want just o/r's discovered PR", results)
	}
}

// dispatchGraphQLServer serves POST /graphql, picking a canned response body
// by matching a substring against the request's query text — each of
// FindReview/DismissReview/RequestReviewer/userID sends a distinctly-shaped
// query, so tests compose the exact call sequence they expect by keying on
// what makes each one unique (a mutation's field name, or a query's
// distinguishing selection).
func dispatchGraphQLServer(t *testing.T, byQuerySubstring map[string]string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/graphql", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Query string `json:"query"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		for substr, resp := range byQuerySubstring {
			if strings.Contains(body.Query, substr) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(resp))
				return
			}
		}
		t.Fatalf("no canned response matches query: %s", body.Query)
	})
	return httptest.NewServer(mux)
}

func TestSelfLoginReturnsAuthenticatedLogin(t *testing.T) {
	srv := dispatchGraphQLServer(t, map[string]string{
		"viewer { login }": `{"data": {"viewer": {"login": "alice"}}}`,
	})
	defer srv.Close()

	c := &GitHubChecker{HTTPClient: srv.Client(), Token: "t", BaseURL: srv.URL}
	got, err := c.SelfLogin(t.Context())
	if err != nil {
		t.Fatalf("SelfLogin() error = %v", err)
	}
	if got != "alice" {
		t.Errorf("SelfLogin() = %q, want %q", got, "alice")
	}
}

func TestSelfLoginCachesAcrossCalls(t *testing.T) {
	var calls int
	mux := http.NewServeMux()
	mux.HandleFunc("/graphql", func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data": {"viewer": {"login": "alice"}}}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &GitHubChecker{HTTPClient: srv.Client(), Token: "t", BaseURL: srv.URL}
	for range 3 {
		if _, err := c.SelfLogin(t.Context()); err != nil {
			t.Fatalf("SelfLogin() error = %v", err)
		}
	}
	if calls != 1 {
		t.Errorf("graphql requests = %d, want 1 (cached after the first call)", calls)
	}
}

func TestFindReviewMatchesByURL(t *testing.T) {
	srv := dispatchGraphQLServer(t, map[string]string{
		"reviews(first: 100)": `{"data": {"repository": {"pullRequest": {"id": "PR_kwId", "reviews": {"nodes": [
			{"id": "REV_1", "url": "https://github.com/o/r/pull/1#pullrequestreview-1", "state": "COMMENTED", "author": {"login": "alice"}},
			{"id": "REV_2", "url": "https://github.com/o/r/pull/1#pullrequestreview-2", "state": "CHANGES_REQUESTED", "author": {"login": "bob"}}
		]}}}}}`,
	})
	defer srv.Close()

	c := &GitHubChecker{HTTPClient: srv.Client(), Token: "t", BaseURL: srv.URL}
	review, found, err := c.FindReview(t.Context(), "o", "r", 1, "https://github.com/o/r/pull/1#pullrequestreview-2")
	if err != nil {
		t.Fatalf("FindReview() error = %v", err)
	}
	if !found {
		t.Fatal("FindReview() found = false, want true")
	}
	if review.ReviewID != "REV_2" || review.State != "CHANGES_REQUESTED" || review.AuthorLogin != "bob" || review.PullRequestID != "PR_kwId" {
		t.Errorf("FindReview() = %+v, want the bob/CHANGES_REQUESTED review", review)
	}
}

func TestFindReviewNoMatchReportsNotFound(t *testing.T) {
	srv := dispatchGraphQLServer(t, map[string]string{
		"reviews(first: 100)": `{"data": {"repository": {"pullRequest": {"id": "PR_1", "reviews": {"nodes": [
			{"id": "REV_1", "url": "https://github.com/o/r/pull/1#pullrequestreview-1", "state": "APPROVED", "author": {"login": "alice"}}
		]}}}}}`,
	})
	defer srv.Close()

	c := &GitHubChecker{HTTPClient: srv.Client(), Token: "t", BaseURL: srv.URL}
	_, found, err := c.FindReview(t.Context(), "o", "r", 1, "https://github.com/o/r/pull/1#pullrequestreview-999")
	if err != nil {
		t.Fatalf("FindReview() error = %v", err)
	}
	if found {
		t.Error("FindReview() found = true, want false for a url that matches no review")
	}
}

func TestFindReviewMissingPullRequestErrors(t *testing.T) {
	srv := dispatchGraphQLServer(t, map[string]string{
		"reviews(first: 100)": `{"data": {"repository": {"pullRequest": null}}}`,
	})
	defer srv.Close()

	c := &GitHubChecker{HTTPClient: srv.Client(), Token: "t", BaseURL: srv.URL}
	if _, _, err := c.FindReview(t.Context(), "o", "r", 404, "https://github.com/o/r/pull/404#pullrequestreview-1"); err == nil {
		t.Error("FindReview() error = nil, want an error for a missing pull request")
	}
}

func TestDismissReviewSendsMutation(t *testing.T) {
	srv := dispatchGraphQLServer(t, map[string]string{
		"dismissPullRequestReview": `{"data": {"dismissPullRequestReview": {"clientMutationId": null}}}`,
	})
	defer srv.Close()

	c := &GitHubChecker{HTTPClient: srv.Client(), Token: "t", BaseURL: srv.URL}
	if err := c.DismissReview(t.Context(), "REV_2"); err != nil {
		t.Errorf("DismissReview() error = %v", err)
	}
}

func TestDismissReviewReportsGraphQLError(t *testing.T) {
	srv := dispatchGraphQLServer(t, map[string]string{
		"dismissPullRequestReview": `{"data": null, "errors": [{"message": "Only admins can dismiss reviews on this branch"}]}`,
	})
	defer srv.Close()

	c := &GitHubChecker{HTTPClient: srv.Client(), Token: "t", BaseURL: srv.URL}
	if err := c.DismissReview(t.Context(), "REV_2"); err == nil {
		t.Error("DismissReview() error = nil, want the branch-protection error surfaced")
	}
}

func TestRequestReviewerLooksUpUserThenMutates(t *testing.T) {
	srv := dispatchGraphQLServer(t, map[string]string{
		"user(login: $login)": `{"data": {"user": {"id": "U_bob"}}}`,
		"requestReviews":      `{"data": {"requestReviews": {"clientMutationId": null}}}`,
	})
	defer srv.Close()

	c := &GitHubChecker{HTTPClient: srv.Client(), Token: "t", BaseURL: srv.URL}
	if err := c.RequestReviewer(t.Context(), "PR_1", "bob"); err != nil {
		t.Errorf("RequestReviewer() error = %v", err)
	}
}

func TestRequestReviewerUnknownUserErrors(t *testing.T) {
	srv := dispatchGraphQLServer(t, map[string]string{
		"user(login: $login)": `{"data": {"user": null}}`,
	})
	defer srv.Close()

	c := &GitHubChecker{HTTPClient: srv.Client(), Token: "t", BaseURL: srv.URL}
	if err := c.RequestReviewer(t.Context(), "PR_1", "ghost"); err == nil {
		t.Error("RequestReviewer() error = nil, want an error for an unresolvable login")
	}
}

func TestGitHubCheckerTracksCommentsOfUnresolvedThreads(t *testing.T) {
	body := strings.Replace(graphqlOpenSuccessBody,
		`"commits": { "nodes": [{ "commit": { "statusCheckRollup": { "state": "SUCCESS" } } }] }`,
		`"reviewThreads": { "nodes": [
        { "id": "t1", "isResolved": false, "comments": { "nodes": [
          { "id": "c1", "url": "https://github.com/o/r/pull/1#discussion_r1", "author": { "login": "alice" } },
          { "id": "c2", "url": "https://github.com/o/r/pull/1#discussion_r2", "author": { "login": "bob" } } ] } },
        { "id": "t2", "isResolved": true, "comments": { "nodes": [
          { "id": "c3", "url": "https://github.com/o/r/pull/1#discussion_r3", "author": { "login": "alice" } } ] } }
      ] },
      "commits": { "nodes": [{ "commit": { "statusCheckRollup": { "state": "SUCCESS" } } }] }`,
		1)
	srv := fakeGitHubServer(t, body, 0)
	defer srv.Close()

	c := &GitHubChecker{HTTPClient: srv.Client(), Token: "test-token", BaseURL: srv.URL}
	url := "https://github.com/o/r/pull/1"
	results, _, err := c.CheckBatch(t.Context(), []string{url})
	if err != nil {
		t.Fatalf("CheckBatch() error = %v", err)
	}
	want := []ThreadComment{
		{ThreadID: "t1", ID: "c1", Author: "alice", Href: "https://github.com/o/r/pull/1#discussion_r1"},
		{ThreadID: "t1", ID: "c2", Author: "bob", Href: "https://github.com/o/r/pull/1#discussion_r2"},
	}
	if !slices.Equal(results[url].ThreadComments, want) {
		t.Errorf("ThreadComments = %+v, want %+v (only the unresolved thread's comments)", results[url].ThreadComments, want)
	}
	if !slices.Equal(results[url].ThreadCommentIDs, []string{"c1", "c2"}) {
		t.Errorf("ThreadCommentIDs = %v, want [c1 c2]", results[url].ThreadCommentIDs)
	}
}

// Probing an upstream repo for a fork's branch: PRs there that come from
// somebody else's fork under the same branch name must not be adopted.
func TestDiscoverBranchPRsFiltersByHeadOwner(t *testing.T) {
	srv := dispatchGraphQLServer(t, map[string]string{
		"pullRequests(headRefName:": `{"data": {
			"repo0": {"branch0": {"nodes": [
				{"url": "https://github.com/up/r/pull/3", "headRepositoryOwner": {"login": "stranger"}},
				{"url": "https://github.com/up/r/pull/9", "headRepositoryOwner": {"login": "Me"}}
			]}},
			"repo1": {"branch0": {"nodes": [
				{"url": "https://github.com/up/other/pull/4", "headRepositoryOwner": {"login": "stranger"}}
			]}},
			"rateLimit": {"limit": 5000, "remaining": 4999, "resetAt": "2026-09-20T23:00:00Z"}
		}}`,
	})
	defer srv.Close()

	c := &GitHubChecker{HTTPClient: srv.Client(), Token: "t", BaseURL: srv.URL}
	results, _, err := c.DiscoverBranchPRs(t.Context(), []GitHubBranchQuery{
		{Owner: "up", Repo: "r", Branch: "feature-x", HeadOwners: []string{"me", "up"}},
		{Owner: "up", Repo: "other", Branch: "feature-x", HeadOwners: []string{"me", "up"}},
	})
	if err != nil {
		t.Fatalf("DiscoverBranchPRs() error = %v", err)
	}
	if got := results[GitHubBranchKey("up", "r", "feature-x")]; got != "https://github.com/up/r/pull/9" {
		t.Errorf("up/r result = %q, want our own PR (#9, head owner matched case-insensitively), not the stranger's #3", got)
	}
	if got, ok := results[GitHubBranchKey("up", "other", "feature-x")]; ok {
		t.Errorf("up/other result = %q, want none: its only PR is a stranger's", got)
	}
}

func TestGitHubCheckerReadsWhyTheEntryLeftTheMergeQueue(t *testing.T) {
	cases := []struct {
		name   string
		nodes  string
		reason string
	}{
		{"removed as merged", `[{"__typename": "AddedToMergeQueueEvent"}, {"__typename": "RemovedFromMergeQueueEvent", "reason": "merged"}]`, "merged"},
		{"removed as invalidated", `[{"__typename": "AddedToMergeQueueEvent"}, {"__typename": "RemovedFromMergeQueueEvent", "reason": "invalidated"}]`, "invalidated"},
		// Re-queued after an earlier removal: that old reason says nothing
		// about the current stint, so it must not be reported.
		{"queued again after a removal", `[{"__typename": "RemovedFromMergeQueueEvent", "reason": "build_failure"}, {"__typename": "AddedToMergeQueueEvent"}]`, ""},
		{"never queued", `[]`, ""},
	}
	for _, c := range cases {
		body := strings.Replace(graphqlOpenSuccessBody,
			`"commits": { "nodes": [{ "commit": { "statusCheckRollup": { "state": "SUCCESS" } } }] }`,
			`"queueEvents": { "nodes": `+c.nodes+` },
      "commits": { "nodes": [{ "commit": { "statusCheckRollup": { "state": "SUCCESS" } } }] }`,
			1)
		srv := fakeGitHubServer(t, body, 0)
		ch := &GitHubChecker{HTTPClient: srv.Client(), Token: "test-token", BaseURL: srv.URL}
		url := "https://github.com/o/r/pull/1"
		results, _, err := ch.CheckBatch(t.Context(), []string{url})
		srv.Close()
		if err != nil {
			t.Fatalf("%s: CheckBatch() error = %v", c.name, err)
		}
		if got := results[url].MergeQueueRemovalReason; got != c.reason {
			t.Errorf("%s: MergeQueueRemovalReason = %q, want %q", c.name, got, c.reason)
		}
	}
}

func TestGitHubCheckerReadsTheLatestReviewBody(t *testing.T) {
	body := strings.Replace(graphqlOpenSuccessBody,
		`"state": "APPROVED" }] }`,
		`"state": "COMMENTED", "body": "Please look at the retry logic." }] }`, 1)
	srv := fakeGitHubServer(t, body, 0)
	defer srv.Close()

	c := &GitHubChecker{HTTPClient: srv.Client(), Token: "test-token", BaseURL: srv.URL}
	url := "https://github.com/o/r/pull/1"
	results, _, err := c.CheckBatch(t.Context(), []string{url})
	if err != nil {
		t.Fatalf("CheckBatch() error = %v", err)
	}
	if got := results[url].LatestReviewBody; got != "Please look at the retry logic." {
		t.Errorf("LatestReviewBody = %q, want the review's text", got)
	}
}

// With branch protection requiring more approvals than the PR has, GitHub
// reports reviewDecision REVIEW_REQUIRED even though the latest review is an
// approval. Like GitLab's "APPROVED 1/2" that approval is tracked, not
// pinged; any other decision (or none, without protection) is a plain one.
func TestGitHubCheckerPartialApprovalFromReviewDecision(t *testing.T) {
	body := func(decision string) string {
		return `{"data": {"repo0": {"pr0": {
		  "state": "OPEN", "title": "t", "headRefOid": "x", ` + decision + `
		  "reviewCount": {"totalCount": 1},
		  "latestReview": {"nodes": [{"author": {"login": "alice"}, "state": "APPROVED"}]},
		  "commits": {"nodes": []}
		}}, "rateLimit": {"limit": 5000, "remaining": 4998, "resetAt": "2026-09-20T23:00:00Z"}}}`
	}
	for _, tc := range []struct {
		name, decision, want string
	}{
		{"more approvals required", `"reviewDecision": "REVIEW_REQUIRED",`, "APPROVED (more needed)"},
		{"fully approved", `"reviewDecision": "APPROVED",`, "APPROVED"},
		{"no branch protection", `"reviewDecision": null,`, "APPROVED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := fakeGitHubServer(t, body(tc.decision), 0)
			defer srv.Close()
			c := &GitHubChecker{HTTPClient: srv.Client(), Token: "test-token", BaseURL: srv.URL}
			url := "https://github.com/o/r/pull/1"
			results, _, err := c.CheckBatch(t.Context(), []string{url})
			if err != nil {
				t.Fatalf("CheckBatch() error = %v", err)
			}
			if got := results[url].LatestReviewState; got != tc.want {
				t.Errorf("LatestReviewState = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestGitHubCheckerReadsReviewFlowFields(t *testing.T) {
	body := `{"data": {"repo0": {"pr0": {
	  "state": "OPEN", "title": "t", "headRefOid": "0123456789abcdef", "isDraft": true,
	  "reviewRequests": {"nodes": [
	    {"requestedReviewer": {"__typename": "User", "login": "alice"}},
	    {"requestedReviewer": {"__typename": "Team"}},
	    {"requestedReviewer": {"__typename": "User", "login": "bob"}}
	  ]},
	  "viewerLatestReview": {"state": "APPROVED"},
	  "pushEvents": {"nodes": [{"afterCommit": {"oid": "0123456789abcdef"}}]},
	  "reviewCount": {"totalCount": 0}, "latestReview": {"nodes": []},
	  "commits": {"nodes": [{"commit": {"author": {"user": {"login": "carol"}, "name": "Carol C"}}}]}
	}}, "rateLimit": {"limit": 5000, "remaining": 4998, "resetAt": "2026-09-20T23:00:00Z"}}}`
	srv := fakeGitHubServer(t, body, 0)
	defer srv.Close()
	c := &GitHubChecker{HTTPClient: srv.Client(), Token: "test-token", BaseURL: srv.URL}
	url := "https://github.com/o/r/pull/1"
	results, _, err := c.CheckBatch(t.Context(), []string{url})
	if err != nil {
		t.Fatalf("CheckBatch() error = %v", err)
	}
	r := results[url]
	if r.HeadSHA != "0123456789abcdef" || r.HeadAuthor != "carol" || !r.ForcePushed || !r.IsDraft {
		t.Errorf("head/draft fields = %+v", r)
	}
	if !slices.Equal(r.RequestedReviewers, []string{"alice", "bob"}) {
		t.Errorf("RequestedReviewers = %v, want the users only", r.RequestedReviewers)
	}
	if r.SelfReviewState != "APPROVED" {
		t.Errorf("SelfReviewState = %q", r.SelfReviewState)
	}
}

func TestGitHubCheckerForcePushIsOnlyTheCurrentHead(t *testing.T) {
	body := `{"data": {"repo0": {"pr0": {
	  "state": "OPEN", "title": "t", "headRefOid": "newer", "isDraft": false,
	  "pushEvents": {"nodes": [{"afterCommit": {"oid": "older"}}]},
	  "viewerLatestReview": {"state": "DISMISSED"},
	  "reviewCount": {"totalCount": 0}, "latestReview": {"nodes": []}, "commits": {"nodes": []}
	}}, "rateLimit": {"limit": 5000, "remaining": 4998, "resetAt": "2026-09-20T23:00:00Z"}}}`
	srv := fakeGitHubServer(t, body, 0)
	defer srv.Close()
	c := &GitHubChecker{HTTPClient: srv.Client(), Token: "test-token", BaseURL: srv.URL}
	url := "https://github.com/o/r/pull/1"
	results, _, err := c.CheckBatch(t.Context(), []string{url})
	if err != nil {
		t.Fatalf("CheckBatch() error = %v", err)
	}
	if r := results[url]; r.ForcePushed || r.SelfReviewState != "" {
		t.Errorf("result = %+v, want no force-push (a later head) and no self review (dismissed)", r)
	}
}
