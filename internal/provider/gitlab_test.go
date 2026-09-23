package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

func TestParseGitLabMRURL(t *testing.T) {
	host, path, iid, err := ParseGitLabMRURL("https://gitlab.corp.example/team/project/-/merge_requests/17")
	if err != nil {
		t.Fatalf("ParseGitLabMRURL() error = %v", err)
	}
	if host != "gitlab.corp.example" || path != "team/project" || iid != 17 {
		t.Errorf("ParseGitLabMRURL() = (%q, %q, %d), want (gitlab.corp.example, team/project, 17)", host, path, iid)
	}
}

func TestParseGitLabMRURLNestedGroup(t *testing.T) {
	host, path, iid, err := ParseGitLabMRURL("https://gitlab.corp.example/group/subgroup/project/-/merge_requests/3")
	if err != nil {
		t.Fatalf("ParseGitLabMRURL() error = %v", err)
	}
	if host != "gitlab.corp.example" || path != "group/subgroup/project" || iid != 3 {
		t.Errorf("ParseGitLabMRURL() = (%q, %q, %d), want (gitlab.corp.example, group/subgroup/project, 3)", host, path, iid)
	}
}

func fakeGitLabServer(t *testing.T, mrState, pipelineStatus string, approvals, unresolved int) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	setRateHeaders := func(w http.ResponseWriter) {
		w.Header().Set("RateLimit-Limit", "2000")
		w.Header().Set("RateLimit-Remaining", "1999")
		w.Header().Set("RateLimit-Reset", "9999999999")
	}
	mux.HandleFunc("/api/v4/projects/team%2Fproject/merge_requests/17", func(w http.ResponseWriter, r *http.Request) {
		setRateHeaders(w)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"state":         mrState,
			"title":         "Fix the thing",
			"description":   "This fixes the thing by doing the other thing.",
			"pipeline":      map[string]any{"status": pipelineStatus},
			"head_pipeline": nil,
		})
	})
	mux.HandleFunc("/api/v4/projects/team%2Fproject/merge_requests/17/approvals", func(w http.ResponseWriter, r *http.Request) {
		setRateHeaders(w)
		approvedBy := make([]map[string]any, approvals)
		_ = json.NewEncoder(w).Encode(map[string]any{"approved_by": approvedBy})
	})
	mux.HandleFunc("/api/v4/projects/team%2Fproject/merge_requests/17/discussions", func(w http.ResponseWriter, r *http.Request) {
		setRateHeaders(w)
		var discussions []map[string]any
		for i := range unresolved {
			discussions = append(discussions, map[string]any{
				"id":    fmt.Sprintf("d%d", i),
				"notes": []map[string]any{{"resolvable": true, "resolved": false}},
			})
		}
		_ = json.NewEncoder(w).Encode(discussions)
	})
	mux.HandleFunc("/api/v4/projects/team%2Fproject/merge_requests/17/notes", func(w http.ResponseWriter, r *http.Request) {
		setRateHeaders(w)
		_ = json.NewEncoder(w).Encode([]map[string]any{})
	})
	mux.HandleFunc("/api/v4/projects/team%2Fproject/pipelines/", func(w http.ResponseWriter, r *http.Request) {
		setRateHeaders(w)
		_ = json.NewEncoder(w).Encode([]map[string]any{})
	})
	return httptest.NewServer(mux)
}

func TestGitLabCheckerOpenWithApprovalsAndPipeline(t *testing.T) {
	srv := fakeGitLabServer(t, "opened", "success", 2, 1)
	defer srv.Close()

	c := &GitLabChecker{HTTPClient: srv.Client(), TokenFunc: func(context.Context, string) (string, error) { return "test-token", nil }, apiBaseOverride: srv.URL}
	result, budget, err := c.Check(t.Context(), "https://gitlab.corp.example/team/project/-/merge_requests/17")
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if result.State != "open" || result.Title != "Fix the thing" || result.Description != "This fixes the thing by doing the other thing." || result.ReviewCount != 2 || result.CIBucket != "success" || !slices.Equal(result.UnresolvedThreadIDs, []string{"d0"}) || result.HasConflict {
		t.Errorf("Check() result = %+v", result)
	}
	if budget.Remaining != 1999 {
		t.Errorf("Check() budget = %+v", budget)
	}
}

func TestGitLabCheckerMergedShortCircuits(t *testing.T) {
	srv := fakeGitLabServer(t, "merged", "success", 0, 0)
	defer srv.Close()

	c := &GitLabChecker{HTTPClient: srv.Client(), TokenFunc: func(context.Context, string) (string, error) { return "test-token", nil }, apiBaseOverride: srv.URL}
	result, _, err := c.Check(t.Context(), "https://gitlab.corp.example/team/project/-/merge_requests/17")
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if result.State != "merged" {
		t.Errorf("Check().State = %q, want merged", result.State)
	}
}

func TestGitLabCheckerHasConflictsFromMRResponse(t *testing.T) {
	mux := http.NewServeMux()
	setRateHeaders := func(w http.ResponseWriter) {
		w.Header().Set("RateLimit-Limit", "2000")
		w.Header().Set("RateLimit-Remaining", "1999")
		w.Header().Set("RateLimit-Reset", "9999999999")
	}
	mux.HandleFunc("/api/v4/projects/team%2Fproject/merge_requests/17", func(w http.ResponseWriter, r *http.Request) {
		setRateHeaders(w)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"state":         "opened",
			"has_conflicts": true,
			"pipeline":      nil,
			"head_pipeline": nil,
		})
	})
	mux.HandleFunc("/api/v4/projects/team%2Fproject/merge_requests/17/approvals", func(w http.ResponseWriter, r *http.Request) {
		setRateHeaders(w)
		_ = json.NewEncoder(w).Encode(map[string]any{"approved_by": []map[string]any{}})
	})
	mux.HandleFunc("/api/v4/projects/team%2Fproject/merge_requests/17/discussions", func(w http.ResponseWriter, r *http.Request) {
		setRateHeaders(w)
		_ = json.NewEncoder(w).Encode([]map[string]any{})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &GitLabChecker{HTTPClient: srv.Client(), TokenFunc: func(context.Context, string) (string, error) { return "test-token", nil }, apiBaseOverride: srv.URL}
	result, _, err := c.Check(t.Context(), "https://gitlab.corp.example/team/project/-/merge_requests/17")
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if !result.HasConflict {
		t.Error("Check() result.HasConflict = false, want true when has_conflicts is true")
	}
}

func TestGitLabCheckerBlockedFromDetailedMergeStatus(t *testing.T) {
	mux := http.NewServeMux()
	setRateHeaders := func(w http.ResponseWriter) {
		w.Header().Set("RateLimit-Limit", "2000")
		w.Header().Set("RateLimit-Remaining", "1999")
		w.Header().Set("RateLimit-Reset", "9999999999")
	}
	mux.HandleFunc("/api/v4/projects/team%2Fproject/merge_requests/17", func(w http.ResponseWriter, r *http.Request) {
		setRateHeaders(w)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"state":                 "opened",
			"detailed_merge_status": "merge_request_blocked",
			"pipeline":              nil,
			"head_pipeline":         nil,
		})
	})
	mux.HandleFunc("/api/v4/projects/team%2Fproject/merge_requests/17/approvals", func(w http.ResponseWriter, r *http.Request) {
		setRateHeaders(w)
		_ = json.NewEncoder(w).Encode(map[string]any{"approved_by": []map[string]any{}})
	})
	mux.HandleFunc("/api/v4/projects/team%2Fproject/merge_requests/17/discussions", func(w http.ResponseWriter, r *http.Request) {
		setRateHeaders(w)
		_ = json.NewEncoder(w).Encode([]map[string]any{})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &GitLabChecker{HTTPClient: srv.Client(), TokenFunc: func(context.Context, string) (string, error) { return "test-token", nil }, apiBaseOverride: srv.URL}
	result, _, err := c.Check(t.Context(), "https://gitlab.corp.example/team/project/-/merge_requests/17")
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if !result.Blocked {
		t.Error("Check() result.Blocked = false, want true when detailed_merge_status is merge_request_blocked")
	}
}

func TestGitLabCheckerNotBlockedOnOtherDetailedMergeStatus(t *testing.T) {
	srv := fakeGitLabServer(t, "opened", "success", 0, 0)
	defer srv.Close()

	c := &GitLabChecker{HTTPClient: srv.Client(), TokenFunc: func(context.Context, string) (string, error) { return "test-token", nil }, apiBaseOverride: srv.URL}
	result, _, err := c.Check(t.Context(), "https://gitlab.corp.example/team/project/-/merge_requests/17")
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if result.Blocked {
		t.Error("Check() result.Blocked = true, want false when detailed_merge_status is absent/not merge_request_blocked")
	}
}

func TestGitLabCheckerCIHrefFromPipelineWebURL(t *testing.T) {
	mux := http.NewServeMux()
	setRateHeaders := func(w http.ResponseWriter) {
		w.Header().Set("RateLimit-Limit", "2000")
		w.Header().Set("RateLimit-Remaining", "1999")
		w.Header().Set("RateLimit-Reset", "9999999999")
	}
	mux.HandleFunc("/api/v4/projects/team%2Fproject/merge_requests/17", func(w http.ResponseWriter, r *http.Request) {
		setRateHeaders(w)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"state":         "opened",
			"pipeline":      map[string]any{"status": "success", "web_url": "https://gitlab.corp.example/team/project/-/pipelines/42"},
			"head_pipeline": nil,
		})
	})
	mux.HandleFunc("/api/v4/projects/team%2Fproject/merge_requests/17/approvals", func(w http.ResponseWriter, r *http.Request) {
		setRateHeaders(w)
		_ = json.NewEncoder(w).Encode(map[string]any{"approved_by": []map[string]any{}})
	})
	mux.HandleFunc("/api/v4/projects/team%2Fproject/merge_requests/17/discussions", func(w http.ResponseWriter, r *http.Request) {
		setRateHeaders(w)
		_ = json.NewEncoder(w).Encode([]map[string]any{})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &GitLabChecker{HTTPClient: srv.Client(), TokenFunc: func(context.Context, string) (string, error) { return "test-token", nil }, apiBaseOverride: srv.URL}
	result, _, err := c.Check(t.Context(), "https://gitlab.corp.example/team/project/-/merge_requests/17")
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if result.CIHref != "https://gitlab.corp.example/team/project/-/pipelines/42" {
		t.Errorf("Check() result.CIHref = %q, want the pipeline's own web_url", result.CIHref)
	}
}

func TestGitLabCheckerCIHrefPrefersFailingJobOverPipeline(t *testing.T) {
	mux := http.NewServeMux()
	setRateHeaders := func(w http.ResponseWriter) {
		w.Header().Set("RateLimit-Limit", "2000")
		w.Header().Set("RateLimit-Remaining", "1999")
		w.Header().Set("RateLimit-Reset", "9999999999")
	}
	mux.HandleFunc("/api/v4/projects/team%2Fproject/merge_requests/17", func(w http.ResponseWriter, r *http.Request) {
		setRateHeaders(w)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"state":         "opened",
			"pipeline":      map[string]any{"id": 42, "status": "failed", "web_url": "https://gitlab.corp.example/team/project/-/pipelines/42"},
			"head_pipeline": nil,
		})
	})
	mux.HandleFunc("/api/v4/projects/team%2Fproject/pipelines/42/jobs", func(w http.ResponseWriter, r *http.Request) {
		setRateHeaders(w)
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"name": "build", "web_url": "https://gitlab.corp.example/team/project/-/jobs/99"},
		})
	})
	mux.HandleFunc("/api/v4/projects/team%2Fproject/merge_requests/17/approvals", func(w http.ResponseWriter, r *http.Request) {
		setRateHeaders(w)
		_ = json.NewEncoder(w).Encode(map[string]any{"approved_by": []map[string]any{}})
	})
	mux.HandleFunc("/api/v4/projects/team%2Fproject/merge_requests/17/discussions", func(w http.ResponseWriter, r *http.Request) {
		setRateHeaders(w)
		_ = json.NewEncoder(w).Encode([]map[string]any{})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &GitLabChecker{HTTPClient: srv.Client(), TokenFunc: func(context.Context, string) (string, error) { return "test-token", nil }, apiBaseOverride: srv.URL}
	result, _, err := c.Check(t.Context(), "https://gitlab.corp.example/team/project/-/merge_requests/17")
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if result.CIHref != "https://gitlab.corp.example/team/project/-/jobs/99" {
		t.Errorf("Check() result.CIHref = %q, want the failing job's own web_url, not the pipeline's", result.CIHref)
	}
}

func TestGitLabCheckerReviewHrefPointsAtApprovalNote(t *testing.T) {
	mux := http.NewServeMux()
	setRateHeaders := func(w http.ResponseWriter) {
		w.Header().Set("RateLimit-Limit", "2000")
		w.Header().Set("RateLimit-Remaining", "1999")
		w.Header().Set("RateLimit-Reset", "9999999999")
	}
	mux.HandleFunc("/api/v4/projects/team%2Fproject/merge_requests/17", func(w http.ResponseWriter, r *http.Request) {
		setRateHeaders(w)
		_ = json.NewEncoder(w).Encode(map[string]any{"state": "opened", "pipeline": nil, "head_pipeline": nil})
	})
	mux.HandleFunc("/api/v4/projects/team%2Fproject/merge_requests/17/approvals", func(w http.ResponseWriter, r *http.Request) {
		setRateHeaders(w)
		_ = json.NewEncoder(w).Encode(map[string]any{"approved_by": []map[string]any{{"username": "alice"}}})
	})
	mux.HandleFunc("/api/v4/projects/team%2Fproject/merge_requests/17/notes", func(w http.ResponseWriter, r *http.Request) {
		setRateHeaders(w)
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"id": 555, "system": true, "body": "approved this merge request", "created_at": "2026-09-20T12:00:00Z", "author": map[string]any{"username": "alice"}},
		})
	})
	mux.HandleFunc("/api/v4/projects/team%2Fproject/merge_requests/17/discussions", func(w http.ResponseWriter, r *http.Request) {
		setRateHeaders(w)
		_ = json.NewEncoder(w).Encode([]map[string]any{})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &GitLabChecker{HTTPClient: srv.Client(), TokenFunc: func(context.Context, string) (string, error) { return "test-token", nil }, apiBaseOverride: srv.URL}
	mrURL := "https://gitlab.corp.example/team/project/-/merge_requests/17"
	result, _, err := c.Check(t.Context(), mrURL)
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if result.ReviewHref != mrURL+"#note_555" {
		t.Errorf("Check() result.ReviewHref = %q, want %q", result.ReviewHref, mrURL+"#note_555")
	}
}

func TestGitLabCheckerPipelineBucketing(t *testing.T) {
	cases := map[string]string{
		"success":               "success",
		"failed":                "failure",
		"canceled":              "failure",
		"running":               "pending",
		"pending":               "pending",
		"totally-unknown-value": "none",
	}
	for pipelineStatus, wantBucket := range cases {
		t.Run(pipelineStatus, func(t *testing.T) {
			srv := fakeGitLabServer(t, "opened", pipelineStatus, 0, 0)
			defer srv.Close()
			c := &GitLabChecker{HTTPClient: srv.Client(), TokenFunc: func(context.Context, string) (string, error) { return "test-token", nil }, apiBaseOverride: srv.URL}
			result, _, err := c.Check(t.Context(), "https://gitlab.corp.example/team/project/-/merge_requests/17")
			if err != nil {
				t.Fatalf("Check() error = %v", err)
			}
			if result.CIBucket != wantBucket {
				t.Errorf("pipeline status %q -> bucket %q, want %q", pipelineStatus, result.CIBucket, wantBucket)
			}
		})
	}
}

// TestGitLabCheckerNonOKStatusErrors verifies that a non-2xx response from
// the GitLab API produces an error instead of silently decoding into a
// zero-value CheckResult — mirroring github.go's checkHTTPStatus coverage
// (see TestGitHubChecker's equivalent case) for the same failure mode in
// GitLabChecker.getJSON.
func TestGitLabCheckerNonOKStatusErrors(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/projects/team%2Fproject/merge_requests/17", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"401 Unauthorized"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &GitLabChecker{HTTPClient: srv.Client(), TokenFunc: func(context.Context, string) (string, error) { return "test-token", nil }, apiBaseOverride: srv.URL}
	result, _, err := c.Check(t.Context(), "https://gitlab.corp.example/team/project/-/merge_requests/17")
	if err == nil {
		t.Fatalf("Check() error = nil, want error for 401 response; result = %+v", result)
	}
}

// TestGitLabCheckerServerErrorOnApprovals verifies a non-2xx response from a
// later call in the sequence (approvals, not just the first MR fetch) also
// errors rather than decoding a zero-value result.
func TestGitLabCheckerServerErrorOnApprovals(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/projects/team%2Fproject/merge_requests/17", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("RateLimit-Limit", "2000")
		w.Header().Set("RateLimit-Remaining", "1999")
		w.Header().Set("RateLimit-Reset", "9999999999")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"state":         "opened",
			"pipeline":      map[string]any{"status": "success"},
			"head_pipeline": nil,
		})
	})
	mux.HandleFunc("/api/v4/projects/team%2Fproject/merge_requests/17/approvals", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("internal error"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &GitLabChecker{HTTPClient: srv.Client(), TokenFunc: func(context.Context, string) (string, error) { return "test-token", nil }, apiBaseOverride: srv.URL}
	result, _, err := c.Check(t.Context(), "https://gitlab.corp.example/team/project/-/merge_requests/17")
	if err == nil {
		t.Fatalf("Check() error = nil, want error for 500 response on approvals; result = %+v", result)
	}
}

// TestGitLabCheckerNoTokenForHostMakesNoRequest is the core regression test
// for the per-host-token fix: a TokenFunc that reports "no credentials
// configured for this host" (empty string, nil error — mirroring `glab
// config get --host <unconfigured>`, which prints nothing and exits 0)
// must cause Check to fail before issuing a single HTTP request. Sending a
// real, working token to an arbitrary pr_url's host — because the token
// was resolved once globally instead of per-host — is exactly the leak
// this closes: the handler below fails the test if it's ever hit at all.
func TestGitLabCheckerNoTokenForHostMakesNoRequest(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("unexpected HTTP request made with no token configured: %s %s", r.Method, r.URL)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &GitLabChecker{
		HTTPClient:      srv.Client(),
		TokenFunc:       func(context.Context, string) (string, error) { return "", nil },
		apiBaseOverride: srv.URL,
	}
	result, _, err := c.Check(t.Context(), "https://gitlab.corp.example/team/project/-/merge_requests/17")
	if err == nil {
		t.Fatalf("Check() error = nil, want error when no token is configured for the host; result = %+v", result)
	}
}

// TestGitLabCheckerLatestReviewerAndTimestamp verifies the reviewer name and
// approval timestamp are sourced from the system note GitLab writes on
// approval, not from /approvals (which has no per-approval timestamp).
func TestGitLabCheckerLatestReviewerAndTimestamp(t *testing.T) {
	mux := http.NewServeMux()
	setRateHeaders := func(w http.ResponseWriter) {
		w.Header().Set("RateLimit-Limit", "2000")
		w.Header().Set("RateLimit-Remaining", "1999")
		w.Header().Set("RateLimit-Reset", "9999999999")
	}
	mux.HandleFunc("/api/v4/projects/team%2Fproject/merge_requests/17", func(w http.ResponseWriter, r *http.Request) {
		setRateHeaders(w)
		_ = json.NewEncoder(w).Encode(map[string]any{"state": "opened", "pipeline": nil, "head_pipeline": nil})
	})
	mux.HandleFunc("/api/v4/projects/team%2Fproject/merge_requests/17/approvals", func(w http.ResponseWriter, r *http.Request) {
		setRateHeaders(w)
		_ = json.NewEncoder(w).Encode(map[string]any{"approved_by": []map[string]any{{}}})
	})
	mux.HandleFunc("/api/v4/projects/team%2Fproject/merge_requests/17/notes", func(w http.ResponseWriter, r *http.Request) {
		setRateHeaders(w)
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"system": true, "body": "approved this merge request", "created_at": "2026-09-20T12:00:00Z", "author": map[string]any{"username": "alice"}},
		})
	})
	mux.HandleFunc("/api/v4/projects/team%2Fproject/merge_requests/17/discussions", func(w http.ResponseWriter, r *http.Request) {
		setRateHeaders(w)
		_ = json.NewEncoder(w).Encode([]map[string]any{})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &GitLabChecker{HTTPClient: srv.Client(), TokenFunc: func(context.Context, string) (string, error) { return "test-token", nil }, apiBaseOverride: srv.URL}
	result, _, err := c.Check(t.Context(), "https://gitlab.corp.example/team/project/-/merge_requests/17")
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if result.LatestReviewer != "alice" || result.LatestReviewState != "APPROVED" || result.LatestReviewAt.IsZero() {
		t.Errorf("Check() result = %+v, want reviewer alice, state APPROVED, non-zero LatestReviewAt", result)
	}
}

// TestGitLabCheckerNoNotesFetchWithoutApprovals verifies the /notes call is
// skipped entirely when nobody has approved yet — no point paying for a
// fourth request to learn a reviewer that can't exist.
func TestGitLabCheckerNoNotesFetchWithoutApprovals(t *testing.T) {
	mux := http.NewServeMux()
	setRateHeaders := func(w http.ResponseWriter) {
		w.Header().Set("RateLimit-Limit", "2000")
		w.Header().Set("RateLimit-Remaining", "1999")
		w.Header().Set("RateLimit-Reset", "9999999999")
	}
	mux.HandleFunc("/api/v4/projects/team%2Fproject/merge_requests/17", func(w http.ResponseWriter, r *http.Request) {
		setRateHeaders(w)
		_ = json.NewEncoder(w).Encode(map[string]any{"state": "opened", "pipeline": nil, "head_pipeline": nil})
	})
	mux.HandleFunc("/api/v4/projects/team%2Fproject/merge_requests/17/approvals", func(w http.ResponseWriter, r *http.Request) {
		setRateHeaders(w)
		_ = json.NewEncoder(w).Encode(map[string]any{"approved_by": []map[string]any{}})
	})
	mux.HandleFunc("/api/v4/projects/team%2Fproject/merge_requests/17/discussions", func(w http.ResponseWriter, r *http.Request) {
		setRateHeaders(w)
		_ = json.NewEncoder(w).Encode([]map[string]any{})
	})
	var notesHit bool
	mux.HandleFunc("/api/v4/projects/team%2Fproject/merge_requests/17/notes", func(w http.ResponseWriter, r *http.Request) {
		notesHit = true
		_ = json.NewEncoder(w).Encode([]map[string]any{})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &GitLabChecker{HTTPClient: srv.Client(), TokenFunc: func(context.Context, string) (string, error) { return "test-token", nil }, apiBaseOverride: srv.URL}
	if _, _, err := c.Check(t.Context(), "https://gitlab.corp.example/team/project/-/merge_requests/17"); err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if notesHit {
		t.Error("Check() hit /notes with zero approvals, want it skipped")
	}
}

// TestGitLabCheckerFailingJobName verifies CIDetail is populated from
// /pipelines/:id/jobs when the pipeline bucket is a failure.
func TestGitLabCheckerFailingJobName(t *testing.T) {
	mux := http.NewServeMux()
	setRateHeaders := func(w http.ResponseWriter) {
		w.Header().Set("RateLimit-Limit", "2000")
		w.Header().Set("RateLimit-Remaining", "1999")
		w.Header().Set("RateLimit-Reset", "9999999999")
	}
	mux.HandleFunc("/api/v4/projects/team%2Fproject/merge_requests/17", func(w http.ResponseWriter, r *http.Request) {
		setRateHeaders(w)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"state":         "opened",
			"pipeline":      map[string]any{"id": 99, "status": "failed"},
			"head_pipeline": nil,
		})
	})
	mux.HandleFunc("/api/v4/projects/team%2Fproject/pipelines/99/jobs", func(w http.ResponseWriter, r *http.Request) {
		setRateHeaders(w)
		if got := r.URL.Query().Get("scope[]"); got != "failed" {
			t.Errorf("jobs query scope[] = %q, want failed", got)
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{{"name": "test:unit"}})
	})
	mux.HandleFunc("/api/v4/projects/team%2Fproject/merge_requests/17/approvals", func(w http.ResponseWriter, r *http.Request) {
		setRateHeaders(w)
		_ = json.NewEncoder(w).Encode(map[string]any{"approved_by": []map[string]any{}})
	})
	mux.HandleFunc("/api/v4/projects/team%2Fproject/merge_requests/17/discussions", func(w http.ResponseWriter, r *http.Request) {
		setRateHeaders(w)
		_ = json.NewEncoder(w).Encode([]map[string]any{})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &GitLabChecker{HTTPClient: srv.Client(), TokenFunc: func(context.Context, string) (string, error) { return "test-token", nil }, apiBaseOverride: srv.URL}
	result, _, err := c.Check(t.Context(), "https://gitlab.corp.example/team/project/-/merge_requests/17")
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if result.CIBucket != "failure" || result.CIDetail != "test:unit" {
		t.Errorf("Check() result = %+v, want CIBucket failure, CIDetail test:unit", result)
	}
}

// TestGitLabCheckerMergedAt verifies merged_at from the MR-details response
// is surfaced on a merged result.
func TestGitLabCheckerMergedAt(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/projects/team%2Fproject/merge_requests/17", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("RateLimit-Limit", "2000")
		w.Header().Set("RateLimit-Remaining", "1999")
		w.Header().Set("RateLimit-Reset", "9999999999")
		_ = json.NewEncoder(w).Encode(map[string]any{"state": "merged", "merged_at": "2026-09-21T08:30:00Z"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &GitLabChecker{HTTPClient: srv.Client(), TokenFunc: func(context.Context, string) (string, error) { return "test-token", nil }, apiBaseOverride: srv.URL}
	result, _, err := c.Check(t.Context(), "https://gitlab.corp.example/team/project/-/merge_requests/17")
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if result.MergedAt.IsZero() {
		t.Error("Check() result.MergedAt is zero, want the parsed merged_at")
	}
}

// TokenFunc runs in the scheduler's hot path (one credential-helper fork
// per GitLab poll), so it must receive Check's own context — otherwise a
// hung keyring backend outlives the poll's deadline and stalls the whole
// scheduler.
func TestGitLabCheckerPassesCheckContextToTokenFunc(t *testing.T) {
	srv := fakeGitLabServer(t, "opened", "success", 0, 0)
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var sawCancelledCtx bool
	c := &GitLabChecker{
		HTTPClient: srv.Client(),
		TokenFunc: func(tokenCtx context.Context, host string) (string, error) {
			if tokenCtx != nil && tokenCtx.Err() != nil {
				sawCancelledCtx = true
				return "", tokenCtx.Err()
			}
			return "test-token", nil
		},
		apiBaseOverride: srv.URL,
	}
	if _, _, err := c.Check(ctx, "https://gitlab.corp.example/team/project/-/merge_requests/17"); err == nil {
		t.Error("Check() with an already-cancelled context, want error")
	}
	if !sawCancelledCtx {
		t.Error("TokenFunc did not observe Check's cancelled context, so the token subprocess would outlive the poll's deadline")
	}
}
