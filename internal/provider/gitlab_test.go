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
func TestSelfUsernameReturnsAuthenticatedUsername(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/user", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"username": "alice"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &GitLabChecker{
		HTTPClient:      srv.Client(),
		TokenFunc:       func(context.Context, string) (string, error) { return "test-token", nil },
		apiBaseOverride: srv.URL,
	}
	got, err := c.SelfUsername(t.Context(), "gitlab.corp.example")
	if err != nil {
		t.Fatalf("SelfUsername() error = %v", err)
	}
	if got != "alice" {
		t.Errorf("SelfUsername() = %q, want %q", got, "alice")
	}
}

func TestSelfUsernameCachesPerHost(t *testing.T) {
	var calls int
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/user", func(w http.ResponseWriter, r *http.Request) {
		calls++
		_ = json.NewEncoder(w).Encode(map[string]any{"username": "alice"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &GitLabChecker{
		HTTPClient:      srv.Client(),
		TokenFunc:       func(context.Context, string) (string, error) { return "test-token", nil },
		apiBaseOverride: srv.URL,
	}
	for range 3 {
		if _, err := c.SelfUsername(t.Context(), "gitlab.corp.example"); err != nil {
			t.Fatalf("SelfUsername() error = %v", err)
		}
	}
	if calls != 1 {
		t.Errorf("requests = %d, want 1 (cached after the first call)", calls)
	}
}

func TestSelfUsernameNoTokenForHostReturnsEmptyNotError(t *testing.T) {
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
	got, err := c.SelfUsername(t.Context(), "not-a-configured-host.example.com")
	if err != nil || got != "" {
		t.Errorf("SelfUsername() = (%q, %v), want (\"\", nil) for an unconfigured host", got, err)
	}
}

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

// TestGitLabCheckerTopLevelCommentsFromIndividualNoteDiscussions verifies a
// plain top-level MR comment — a "discussion" wrapping a single note whose
// resolvable is false, GitLab's shape for individual_note comments — is
// tracked separately from UnresolvedThreadIDs (which stays scoped to
// discussions carrying at least one resolvable note), reusing the same
// /discussions response rather than a dedicated API call.
func TestGitLabCheckerTopLevelCommentsFromIndividualNoteDiscussions(t *testing.T) {
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
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"id": "thread1", "notes": []map[string]any{{"id": 1, "resolvable": true, "resolved": false}}},
			{"id": "note2", "notes": []map[string]any{{"id": 2, "resolvable": false, "resolved": false, "body": "looks good to me", "author": map[string]any{"username": "alice"}}}},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &GitLabChecker{HTTPClient: srv.Client(), TokenFunc: func(context.Context, string) (string, error) { return "test-token", nil }, apiBaseOverride: srv.URL}
	result, _, err := c.Check(t.Context(), "https://gitlab.corp.example/team/project/-/merge_requests/17")
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if !slices.Equal(result.UnresolvedThreadIDs, []string{"thread1"}) {
		t.Errorf("Check() result.UnresolvedThreadIDs = %v, want [thread1] only (note2 is a top-level comment, not a review thread)", result.UnresolvedThreadIDs)
	}
	if !slices.Equal(result.TopLevelCommentIDs, []string{"note2"}) {
		t.Errorf("Check() result.TopLevelCommentIDs = %v, want [note2] only", result.TopLevelCommentIDs)
	}
	wantHref := "https://gitlab.corp.example/team/project/-/merge_requests/17#note_2"
	if result.LatestCommentHref != wantHref {
		t.Errorf("Check() result.LatestCommentHref = %q, want %q", result.LatestCommentHref, wantHref)
	}
	if result.LatestCommentAuthor != "alice" {
		t.Errorf("Check() result.LatestCommentAuthor = %q, want alice", result.LatestCommentAuthor)
	}
	want := []TopLevelComment{{ID: "note2", Href: wantHref, Author: "alice", Body: "looks good to me"}}
	if !slices.Equal(result.TopLevelComments, want) {
		t.Errorf("Check() result.TopLevelComments = %+v, want %+v (raw comments with bodies, for filter rules)", result.TopLevelComments, want)
	}
}

// TestGitLabCheckerSystemNotesNeverCountAsTopLevelComments is a regression
// test: confirmed live, a real GitLab MR
// pinged COMMENT "3 new comment(s)" for three discussions that were all
// GitLab system notes ("requested review from @st and @mw", "assigned to
// @alice", "enabled an automatic merge...") — none of them a real comment an
// agent could go read. A system note is an individual_note discussion with
// no resolvable note, same shape as a real top-level comment, so it must be
// excluded explicitly rather than inferred from resolvable alone.
func TestGitLabCheckerSystemNotesNeverCountAsTopLevelComments(t *testing.T) {
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
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"id": "d1", "notes": []map[string]any{{"id": 1, "system": true, "resolvable": false, "body": "requested review from @st and @mw"}}},
			{"id": "d2", "notes": []map[string]any{{"id": 2, "system": true, "resolvable": false, "body": "assigned to @alice"}}},
			{"id": "d3", "notes": []map[string]any{{"id": 3, "system": true, "resolvable": false, "body": "enabled an automatic merge"}}},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &GitLabChecker{HTTPClient: srv.Client(), TokenFunc: func(context.Context, string) (string, error) { return "test-token", nil }, apiBaseOverride: srv.URL}
	result, _, err := c.Check(t.Context(), "https://gitlab.corp.example/team/project/-/merge_requests/17")
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if len(result.TopLevelCommentIDs) != 0 || result.LatestCommentHref != "" {
		t.Errorf("Check() result = %+v, want no top-level comments (every discussion here is a system note)", result)
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

func TestDiscoverBranchMRFindsOpenMR(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/projects/team%2Fproject/merge_requests", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("source_branch"); got != "feature-x" {
			t.Errorf("source_branch query param = %q, want feature-x", got)
		}
		if got := r.URL.Query().Get("state"); got != "opened" {
			t.Errorf("state query param = %q, want opened", got)
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{{"web_url": "https://gitlab.corp.example/team/project/-/merge_requests/9"}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &GitLabChecker{
		HTTPClient:      srv.Client(),
		TokenFunc:       func(context.Context, string) (string, error) { return "test-token", nil },
		apiBaseOverride: srv.URL,
	}
	mrURL, found, _, err := c.DiscoverBranchMR(t.Context(), "gitlab.corp.example", "team/project", "feature-x")
	if err != nil {
		t.Fatalf("DiscoverBranchMR() error = %v", err)
	}
	if !found || mrURL != "https://gitlab.corp.example/team/project/-/merge_requests/9" {
		t.Errorf("DiscoverBranchMR() = (%q, %v), want the discovered MR url", mrURL, found)
	}
}

func TestDiscoverBranchMRNoOpenMRReportsNotFound(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/projects/team%2Fproject/merge_requests", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &GitLabChecker{
		HTTPClient:      srv.Client(),
		TokenFunc:       func(context.Context, string) (string, error) { return "test-token", nil },
		apiBaseOverride: srv.URL,
	}
	_, found, _, err := c.DiscoverBranchMR(t.Context(), "gitlab.corp.example", "team/project", "no-mr-yet")
	if err != nil {
		t.Fatalf("DiscoverBranchMR() error = %v", err)
	}
	if found {
		t.Error("DiscoverBranchMR() found = true, want false for a branch with no open MR")
	}
}

func TestDiscoverBranchMRNoTokenForHostMakesNoRequest(t *testing.T) {
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
	_, found, _, err := c.DiscoverBranchMR(t.Context(), "not-a-configured-host.example.com", "team/project", "feature-x")
	if err != nil || found {
		t.Errorf("DiscoverBranchMR() = (found=%v, err=%v), want (false, nil) rather than an error for an unconfigured host", found, err)
	}
}

// An MR that needs two approvals and has one must not read as approved: the
// badge turned green for a single approval while GitLab still said
// not_approved. The state carries the progress instead.
func TestGitLabCheckerPartialApprovalIsNotReportedAsApproved(t *testing.T) {
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
		_ = json.NewEncoder(w).Encode(map[string]any{
			"approved": false, "approvals_required": 2, "approvals_left": 1,
			"approved_by": []map[string]any{{}},
		})
	})
	mux.HandleFunc("/api/v4/projects/team%2Fproject/merge_requests/17/notes", func(w http.ResponseWriter, r *http.Request) {
		setRateHeaders(w)
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"system": true, "body": "approved this merge request", "created_at": "2026-09-20T12:00:00Z", "author": map[string]any{"username": "st"}},
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
	if result.LatestReviewState != "APPROVED 1/2" {
		t.Errorf("LatestReviewState = %q, want %q for 1 of 2 required approvals", result.LatestReviewState, "APPROVED 1/2")
	}
	if result.LatestReviewer != "st" || result.ReviewCount != 1 {
		t.Errorf("result = %+v, want reviewer st and ReviewCount 1 still recorded so the approval pings", result)
	}
}

func TestGitLabCheckerTracksCommentsOfUnresolvedThreads(t *testing.T) {
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
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"id": "d1", "notes": []map[string]any{
				{"id": 11, "system": false, "resolvable": true, "resolved": false, "body": "a", "author": map[string]any{"username": "alice"}},
				{"id": 12, "system": false, "resolvable": true, "resolved": false, "body": "b", "author": map[string]any{"username": "bob"}},
			}},
			{"id": "d2", "notes": []map[string]any{
				{"id": 21, "system": false, "resolvable": true, "resolved": true, "body": "c", "author": map[string]any{"username": "alice"}},
			}},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &GitLabChecker{HTTPClient: srv.Client(), TokenFunc: func(context.Context, string) (string, error) { return "test-token", nil }, apiBaseOverride: srv.URL}
	mr := "https://gitlab.corp.example/team/project/-/merge_requests/17"
	result, _, err := c.Check(t.Context(), mr)
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	want := []ThreadComment{
		{ThreadID: "d1", ID: "11", Author: "alice", Href: mr + "#note_11"},
		{ThreadID: "d1", ID: "12", Author: "bob", Href: mr + "#note_12"},
	}
	if !slices.Equal(result.ThreadComments, want) {
		t.Errorf("ThreadComments = %+v, want %+v (only the unresolved discussion's notes)", result.ThreadComments, want)
	}
	if !slices.Equal(result.ThreadCommentIDs, []string{"11", "12"}) {
		t.Errorf("ThreadCommentIDs = %v, want [11 12]", result.ThreadCommentIDs)
	}
}

// An MR from someone else's fork can share a branch name with the checkout's.
// It lists under the target project too, but it is not this session's MR.
func TestDiscoverBranchMRIgnoresMRsFromForks(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/projects/team%2Fproject/merge_requests", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"web_url": "https://gitlab.corp.example/team/project/-/merge_requests/8", "source_project_id": 99, "target_project_id": 5},
			{"web_url": "https://gitlab.corp.example/team/project/-/merge_requests/9", "source_project_id": 5, "target_project_id": 5},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &GitLabChecker{
		HTTPClient:      srv.Client(),
		TokenFunc:       func(context.Context, string) (string, error) { return "test-token", nil },
		apiBaseOverride: srv.URL,
	}
	mrURL, found, _, err := c.DiscoverBranchMR(t.Context(), "gitlab.corp.example", "team/project", "feature-x")
	if err != nil {
		t.Fatalf("DiscoverBranchMR() error = %v", err)
	}
	if !found || mrURL != "https://gitlab.corp.example/team/project/-/merge_requests/9" {
		t.Errorf("DiscoverBranchMR() = (%q, %v), want the same-project MR 9, not the fork's MR 8", mrURL, found)
	}
}

// Some instances (or projects) do not offer the approvals endpoint at all.
// That must not make the whole MR unwatchable: it just has no approvals.
func TestGitLabCheckerWorksWithoutTheApprovalsAPI(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusNotFound} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("/api/v4/projects/team%2Fproject/merge_requests/17", func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{"state": "opened", "title": "t", "pipeline": map[string]any{"status": "success"}})
			})
			mux.HandleFunc("/api/v4/projects/team%2Fproject/merge_requests/17/approvals", func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, `{"message":"not available"}`, status)
			})
			mux.HandleFunc("/api/v4/projects/team%2Fproject/merge_requests/17/discussions", func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode([]map[string]any{{"id": "d1", "notes": []map[string]any{{"resolvable": true, "resolved": false}}}})
			})
			srv := httptest.NewServer(mux)
			defer srv.Close()

			c := &GitLabChecker{HTTPClient: srv.Client(), TokenFunc: func(context.Context, string) (string, error) { return "tok", nil }, apiBaseOverride: srv.URL}
			result, _, err := c.Check(t.Context(), "https://gitlab.corp.example/team/project/-/merge_requests/17")
			if err != nil {
				t.Fatalf("Check() error = %v, want the MR still checked without approvals", err)
			}
			if result.ReviewCount != 0 || result.CIBucket != "success" || len(result.UnresolvedThreadIDs) != 1 {
				t.Errorf("Check() = %+v, want no approvals but CI and threads intact", result)
			}
		})
	}
}

func TestGitLabCheckerReadsReviewFlowFields(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/projects/team%2Fproject/merge_requests/17", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"state": "opened", "title": "t", "draft": true, "sha": "0123456789abcdef",
			"reviewers": []map[string]any{{"username": "alice"}, {"username": "bob"}},
			"pipeline":  map[string]any{"status": "success"},
		})
	})
	mux.HandleFunc("/api/v4/projects/team%2Fproject/merge_requests/17/approvals", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"approved_by": []map[string]any{}, "user_has_approved": true})
	})
	mux.HandleFunc("/api/v4/projects/team%2Fproject/merge_requests/17/discussions", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &GitLabChecker{HTTPClient: srv.Client(), TokenFunc: func(context.Context, string) (string, error) { return "tok", nil }, apiBaseOverride: srv.URL}
	r, _, err := c.Check(t.Context(), "https://gitlab.corp.example/team/project/-/merge_requests/17")
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if r.HeadSHA != "0123456789abcdef" || !r.IsDraft {
		t.Errorf("head/draft = %q/%v", r.HeadSHA, r.IsDraft)
	}
	if !slices.Equal(r.RequestedReviewers, []string{"alice", "bob"}) {
		t.Errorf("RequestedReviewers = %v", r.RequestedReviewers)
	}
	if r.SelfReviewState != "APPROVED" {
		t.Errorf("SelfReviewState = %q, want APPROVED from user_has_approved", r.SelfReviewState)
	}
}

func TestGitLabCheckerRecordsWhoResolvedEachDiscussion(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/projects/team%2Fproject/merge_requests/17", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"state": "opened", "title": "t", "pipeline": map[string]any{"status": "success"}})
	})
	mux.HandleFunc("/api/v4/projects/team%2Fproject/merge_requests/17/approvals", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"approved_by": []map[string]any{}})
	})
	mux.HandleFunc("/api/v4/projects/team%2Fproject/merge_requests/17/discussions", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"id": "d1", "notes": []map[string]any{{"resolvable": true, "resolved": true, "resolved_by": map[string]any{"username": "alice"}}}},
			{"id": "d2", "notes": []map[string]any{{"resolvable": true, "resolved": false}}},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := &GitLabChecker{HTTPClient: srv.Client(), TokenFunc: func(context.Context, string) (string, error) { return "tok", nil }, apiBaseOverride: srv.URL}
	r, _, err := c.Check(t.Context(), "https://gitlab.corp.example/team/project/-/merge_requests/17")
	if err != nil {
		t.Fatal(err)
	}
	if r.ResolvedBy["d1"] != "alice" || len(r.ResolvedBy) != 1 {
		t.Errorf("ResolvedBy = %v, want only d1 by alice", r.ResolvedBy)
	}
	if !slices.Equal(r.UnresolvedThreadIDs, []string{"d2"}) {
		t.Errorf("UnresolvedThreadIDs = %v", r.UnresolvedThreadIDs)
	}
}
