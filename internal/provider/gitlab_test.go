package provider

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
		json.NewEncoder(w).Encode(map[string]any{
			"state":         mrState,
			"pipeline":      map[string]any{"status": pipelineStatus},
			"head_pipeline": nil,
		})
	})
	mux.HandleFunc("/api/v4/projects/team%2Fproject/merge_requests/17/approvals", func(w http.ResponseWriter, r *http.Request) {
		setRateHeaders(w)
		approvedBy := make([]map[string]any, approvals)
		json.NewEncoder(w).Encode(map[string]any{"approved_by": approvedBy})
	})
	mux.HandleFunc("/api/v4/projects/team%2Fproject/merge_requests/17/discussions", func(w http.ResponseWriter, r *http.Request) {
		setRateHeaders(w)
		var discussions []map[string]any
		for i := 0; i < unresolved; i++ {
			discussions = append(discussions, map[string]any{
				"notes": []map[string]any{{"resolvable": true, "resolved": false}},
			})
		}
		json.NewEncoder(w).Encode(discussions)
	})
	return httptest.NewServer(mux)
}

func TestGitLabCheckerOpenWithApprovalsAndPipeline(t *testing.T) {
	srv := fakeGitLabServer(t, "opened", "success", 2, 1)
	defer srv.Close()

	c := &GitLabChecker{HTTPClient: srv.Client(), Token: "test-token", apiBaseOverride: srv.URL}
	result, budget, err := c.Check(t.Context(), "https://gitlab.corp.example/team/project/-/merge_requests/17")
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if result.State != "open" || result.ReviewCount != 2 || result.CIBucket != "success" || result.Unresolved != 1 {
		t.Errorf("Check() result = %+v", result)
	}
	if budget.Remaining != 1999 {
		t.Errorf("Check() budget = %+v", budget)
	}
}

func TestGitLabCheckerMergedShortCircuits(t *testing.T) {
	srv := fakeGitLabServer(t, "merged", "success", 0, 0)
	defer srv.Close()

	c := &GitLabChecker{HTTPClient: srv.Client(), Token: "test-token", apiBaseOverride: srv.URL}
	result, _, err := c.Check(t.Context(), "https://gitlab.corp.example/team/project/-/merge_requests/17")
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if result.State != "merged" {
		t.Errorf("Check().State = %q, want merged", result.State)
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
			c := &GitLabChecker{HTTPClient: srv.Client(), Token: "test-token", apiBaseOverride: srv.URL}
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
		w.Write([]byte(`{"message":"401 Unauthorized"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &GitLabChecker{HTTPClient: srv.Client(), Token: "test-token", apiBaseOverride: srv.URL}
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
		json.NewEncoder(w).Encode(map[string]any{
			"state":         "opened",
			"pipeline":      map[string]any{"status": "success"},
			"head_pipeline": nil,
		})
	})
	mux.HandleFunc("/api/v4/projects/team%2Fproject/merge_requests/17/approvals", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("internal error"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &GitLabChecker{HTTPClient: srv.Client(), Token: "test-token", apiBaseOverride: srv.URL}
	result, _, err := c.Check(t.Context(), "https://gitlab.corp.example/team/project/-/merge_requests/17")
	if err == nil {
		t.Fatalf("Check() error = nil, want error for 500 response on approvals; result = %+v", result)
	}
}
