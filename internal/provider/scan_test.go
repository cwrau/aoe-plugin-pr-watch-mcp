package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestParseScanScopeURL(t *testing.T) {
	cases := []struct {
		raw  string
		want ScanScope
	}{
		{"https://github.com", ScanScope{Provider: "github", Host: "github.com", Kind: "platform"}},
		{"github.com/", ScanScope{Provider: "github", Host: "github.com", Kind: "platform"}},
		{"https://github.com/acme", ScanScope{Provider: "github", Host: "github.com", Kind: "org", Path: "acme"}},
		{"https://github.com/acme/widgets", ScanScope{Provider: "github", Host: "github.com", Kind: "repo", Path: "acme/widgets"}},
		{"https://github.com/acme/widgets.git", ScanScope{Provider: "github", Host: "github.com", Kind: "repo", Path: "acme/widgets"}},
		{"https://github.com/acme/widgets/pulls", ScanScope{Provider: "github", Host: "github.com", Kind: "repo", Path: "acme/widgets"}},
		{"https://gitlab.corp.example", ScanScope{Provider: "gitlab", Host: "gitlab.corp.example", Kind: "platform"}},
		{"https://gitlab.corp.example/group/sub/proj", ScanScope{Provider: "gitlab", Host: "gitlab.corp.example", Path: "group/sub/proj"}},
	}
	for _, c := range cases {
		got, err := ParseScanScopeURL(c.raw)
		if err != nil || got != c.want {
			t.Errorf("ParseScanScopeURL(%q) = (%+v, %v), want %+v", c.raw, got, err, c.want)
		}
	}
	for _, bad := range []string{"", "ftp://github.com/a", "https://", "not a url"} {
		if _, err := ParseScanScopeURL(bad); err == nil {
			t.Errorf("ParseScanScopeURL(%q) succeeded, want an error", bad)
		}
	}
}

func TestScanFiltersValidate(t *testing.T) {
	platform := ScanScope{Provider: "github", Host: "github.com", Kind: "platform"}
	repo := ScanScope{Provider: "github", Host: "github.com", Kind: "repo", Path: "o/r"}
	gl := ScanScope{Provider: "gitlab", Host: "gitlab.corp.example", Kind: "repo", Path: "g/p"}
	for name, tc := range map[string]struct {
		scope   ScanScope
		f       ScanFilters
		wantErr string
	}{
		"bare platform":              {platform, ScanFilters{}, "person filter"},
		"platform labels only":       {platform, ScanFilters{Labels: []string{"bug"}}, "person filter"},
		"platform review requested":  {platform, ScanFilters{ReviewRequested: "me"}, ""},
		"platform authors":           {platform, ScanFilters{Authors: []string{"alice"}}, ""},
		"bare repo is fine":          {repo, ScanFilters{}, ""},
		"bad draft":                  {repo, ScanFilters{Draft: "maybe"}, "draft"},
		"bad review status":          {repo, ScanFilters{ReviewStatus: "meh"}, "review_status"},
		"bad updated_within":         {repo, ScanFilters{UpdatedWithin: "soon"}, "updated_within"},
		"gitlab involved":            {gl, ScanFilters{Involved: "me"}, "GitHub only"},
		"gitlab mentioned":           {gl, ScanFilters{Mentioned: "me"}, "GitHub only"},
		"gitlab review status":       {gl, ScanFilters{ReviewStatus: "none"}, "GitHub only"},
		"gitlab team review request": {gl, ScanFilters{ReviewRequested: "org/team"}, "team"},
		"gitlab ok":                  {gl, ScanFilters{ReviewRequested: "me", Labels: []string{"x"}, UpdatedWithin: "14d"}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			err := tc.f.Validate(tc.scope)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("Validate() error = %v, want nil", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Errorf("Validate() error = %v, want it to mention %q", err, tc.wantErr)
			}
		})
	}
}

func TestBuildGitHubSearchQueries(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	repo := ScanScope{Provider: "github", Host: "github.com", Kind: "repo", Path: "o/r"}

	got, err := BuildGitHubSearchQueries(repo, ScanFilters{
		ReviewRequested: "me", Draft: "exclude", Labels: []string{"bug", "needs review"}, LabelsNot: []string{"wip"},
		AuthorsNot: []string{"dependabot[bot]"}, BaseBranch: "main", ReviewStatus: "required", UpdatedWithin: "14d",
	}, "alice", now)
	if err != nil {
		t.Fatal(err)
	}
	want := `is:pr is:open archived:false repo:o/r review-requested:alice draft:false base:main review:required label:bug label:"needs review" -label:wip -author:dependabot[bot] updated:>=2026-10-06`
	if len(got) != 1 || got[0] != want {
		t.Errorf("query = %q, want [%q]", got, want)
	}

	org := ScanScope{Provider: "github", Host: "github.com", Kind: "org", Path: "acme"}
	got, _ = BuildGitHubSearchQueries(org, ScanFilters{ReviewRequested: "me"}, "alice", now)
	if len(got) != 2 || !strings.Contains(got[0], "org:acme") || !strings.Contains(got[1], "user:acme") {
		t.Errorf("org queries = %q, want one org: and one user: query", got)
	}

	platform := ScanScope{Provider: "github", Host: "github.com", Kind: "platform"}
	got, _ = BuildGitHubSearchQueries(platform, ScanFilters{Authors: []string{"bob", "carol"}, Assignee: "none", Involved: "me", Mentioned: "dave", ReviewedBy: "me", ReviewRequested: "acme/core"}, "alice", now)
	if len(got) != 2 || !strings.Contains(got[0], "author:bob") || !strings.Contains(got[1], "author:carol") {
		t.Fatalf("authors union queries = %q", got)
	}
	for _, q := range got {
		for _, part := range []string{"no:assignee", "involves:alice", "mentions:dave", "reviewed-by:alice", "team-review-requested:acme/core"} {
			if !strings.Contains(q, part) {
				t.Errorf("query %q lacks %q", q, part)
			}
		}
		if strings.Contains(q, "repo:") || strings.Contains(q, "org:") {
			t.Errorf("platform query %q has a scope qualifier", q)
		}
	}

	if _, err := BuildGitHubSearchQueries(repo, ScanFilters{Labels: []string{strings.Repeat("x", 300)}}, "alice", now); err == nil {
		t.Error("an over-long query was accepted, GitHub rejects queries past 256 characters")
	}
	if _, err := BuildGitHubSearchQueries(repo, ScanFilters{ReviewRequested: "me"}, "", now); err == nil {
		t.Error(`"me" with no known own account was accepted`)
	}
}

func TestBuildGitLabSearch(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	f := ScanFilters{ReviewRequested: "me", Assignee: "none", Labels: []string{"a", "b"}, LabelsNot: []string{"wip"}, Draft: "exclude", BaseBranch: "main", ReviewedBy: "carol", UpdatedWithin: "1d"}

	cases := []struct {
		scope    ScanScope
		endpoint string
		extra    url.Values
	}{
		{ScanScope{Provider: "gitlab", Host: "h", Kind: "repo", Path: "g/p"}, "/api/v4/projects/g%2Fp/merge_requests", nil},
		{ScanScope{Provider: "gitlab", Host: "h", Kind: "org", Path: "g/sub"}, "/api/v4/groups/g%2Fsub/merge_requests", url.Values{"include_subgroups": {"true"}}},
		{ScanScope{Provider: "gitlab", Host: "h", Kind: "platform"}, "/api/v4/merge_requests", url.Values{"scope": {"all"}}},
	}
	for _, c := range cases {
		got, err := BuildGitLabSearches(c.scope, f, "alice", now)
		if err != nil || len(got) != 1 {
			t.Fatalf("BuildGitLabSearches(%s) = (%v, %v)", c.scope.Kind, got, err)
		}
		if got[0].Endpoint != c.endpoint {
			t.Errorf("%s endpoint = %q, want %q", c.scope.Kind, got[0].Endpoint, c.endpoint)
		}
		q := got[0].Query
		for k, want := range map[string]string{
			"state": "opened", "reviewer_username": "alice", "assignee_id": "None", "labels": "a,b", "not[labels]": "wip",
			"draft": "no", "target_branch": "main", "approved_by_usernames": "carol", "updated_after": "2026-10-19T12:00:00Z",
		} {
			if q.Get(k) != want {
				t.Errorf("%s: %s = %q, want %q", c.scope.Kind, k, q.Get(k), want)
			}
		}
		for k, v := range c.extra {
			if q.Get(k) != v[0] {
				t.Errorf("%s: %s = %q, want %q", c.scope.Kind, k, q.Get(k), v[0])
			}
		}
	}

	got, _ := BuildGitLabSearches(ScanScope{Provider: "gitlab", Host: "h", Kind: "repo", Path: "g/p"}, ScanFilters{Authors: []string{"bob", "carol"}}, "alice", now)
	if len(got) != 2 || got[0].Query.Get("author_username") != "bob" || got[1].Query.Get("author_username") != "carol" {
		t.Errorf("authors union searches = %+v", got)
	}
}

func TestScanFiltersMatchClientSide(t *testing.T) {
	m := ScanMatch{Repo: "acme/widgets", Author: "Bob", HeadRef: "renovate/go-deps"}
	for name, tc := range map[string]struct {
		f    ScanFilters
		want bool
	}{
		"no filters":           {ScanFilters{}, true},
		"repo glob hit":        {ScanFilters{Repos: []string{"acme/*"}}, true},
		"repo glob miss":       {ScanFilters{Repos: []string{"other/*"}}, false},
		"repos_not hit":        {ScanFilters{ReposNot: []string{"acme/wid*"}}, false},
		"repos_not miss":       {ScanFilters{ReposNot: []string{"acme/other"}}, true},
		"branch glob hit":      {ScanFilters{Branch: []string{"renovate/*"}}, true},
		"branch glob miss":     {ScanFilters{Branch: []string{"feat/*"}}, false},
		"branch_not hit":       {ScanFilters{BranchNot: []string{"renovate/*"}}, false},
		"authors_not any case": {ScanFilters{AuthorsNot: []string{"bob"}}, false},
		"authors hit":          {ScanFilters{Authors: []string{"bob", "x"}}, true},
		"authors miss":         {ScanFilters{Authors: []string{"x"}}, false},
	} {
		if got := tc.f.Matches(m); got != tc.want {
			t.Errorf("%s: Matches() = %v, want %v", name, got, tc.want)
		}
	}
}

func TestParseUpdatedWithin(t *testing.T) {
	for in, want := range map[string]time.Duration{"14d": 14 * 24 * time.Hour, "2w": 14 * 24 * time.Hour, "36h": 36 * time.Hour} {
		if got, err := ParseUpdatedWithin(in); err != nil || got != want {
			t.Errorf("ParseUpdatedWithin(%q) = (%v, %v), want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "d", "-1d", "1m", "x"} {
		if _, err := ParseUpdatedWithin(bad); err == nil {
			t.Errorf("ParseUpdatedWithin(%q) succeeded", bad)
		}
	}
}

func searchServer(t *testing.T, byQuery map[string]string, seen *[]string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/graphql", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Variables struct {
				Q     string `json:"q"`
				First int    `json:"first"`
			} `json:"variables"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		*seen = append(*seen, fmt.Sprintf("%s|%d", body.Variables.Q, body.Variables.First))
		resp, ok := byQuery[body.Variables.Q]
		if !ok {
			http.Error(w, "unexpected query "+body.Variables.Q, http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(resp))
	})
	return httptest.NewServer(mux)
}

const searchRate = `"rateLimit": {"limit": 5000, "remaining": 4990, "resetAt": "2026-10-20T13:00:00Z", "cost": 1}`

func TestGitHubSearchPullRequestsUnionsQueries(t *testing.T) {
	var seen []string
	srv := searchServer(t, map[string]string{
		"q1": `{"data": {"search": {"issueCount": 2, "nodes": [
		  {"url": "https://github.com/o/a/pull/1", "isDraft": false, "headRefName": "feat/a", "author": {"login": "bob"}, "repository": {"nameWithOwner": "o/a"}},
		  {"url": "https://github.com/o/a/pull/2", "isDraft": true, "headRefName": "feat/b", "author": {"login": "carol"}, "repository": {"nameWithOwner": "o/a"}}]}, ` + searchRate + `}}`,
		"q2": `{"data": {"search": {"issueCount": 1, "nodes": [
		  {"url": "https://github.com/o/a/pull/2", "isDraft": true, "headRefName": "feat/b", "author": {"login": "carol"}, "repository": {"nameWithOwner": "o/a"}}]}, ` + searchRate + `}}`,
	}, &seen)
	defer srv.Close()

	c := &GitHubChecker{HTTPClient: srv.Client(), Token: "t", BaseURL: srv.URL}
	res, err := c.SearchPullRequests(t.Context(), []string{"q1", "q2"}, 50)
	if err != nil {
		t.Fatalf("SearchPullRequests() error = %v", err)
	}
	if len(res.Matches) != 2 || res.Matches[0].URL != "https://github.com/o/a/pull/1" || res.Matches[1].Author != "carol" || !res.Matches[1].Draft || res.Matches[0].HeadRef != "feat/a" || res.Matches[0].Repo != "o/a" {
		t.Errorf("matches = %+v, want the deduplicated union with author, head ref, draft and repo", res.Matches)
	}
	if !res.Complete {
		t.Error("Complete = false although every issueCount was returned in full")
	}
	if res.Budget.Remaining != 4990 || res.Budget.Limit != 5000 {
		t.Errorf("budget = %+v", res.Budget)
	}
	if want := []string{"q1|50", "q2|50"}; !slices.Equal(seen, want) {
		t.Errorf("requests = %v, want %v", seen, want)
	}
}

func TestGitHubSearchPullRequestsFlagsAnIncompleteResult(t *testing.T) {
	var seen []string
	srv := searchServer(t, map[string]string{
		"q": `{"data": {"search": {"issueCount": 7, "nodes": [{"url": "https://github.com/o/a/pull/1", "repository": {"nameWithOwner": "o/a"}}]}, ` + searchRate + `}}`,
	}, &seen)
	defer srv.Close()
	c := &GitHubChecker{HTTPClient: srv.Client(), Token: "t", BaseURL: srv.URL}
	res, err := c.SearchPullRequests(t.Context(), []string{"q"}, 1)
	if err != nil || res.Complete {
		t.Errorf("SearchPullRequests() = (%+v, %v), want an incomplete result", res, err)
	}
	if _, err := c.SearchPullRequests(t.Context(), []string{"unknown"}, 1); err == nil {
		t.Error("a failing search returned no error")
	}
}

func TestGitLabSearchMergeRequests(t *testing.T) {
	mux := http.NewServeMux()
	var gotQuery url.Values
	mux.HandleFunc("/api/v4/groups/g/merge_requests", func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		w.Header().Set("RateLimit-Limit", "2000")
		w.Header().Set("RateLimit-Remaining", "1999")
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"web_url": "https://gitlab.corp.example/g/p/-/merge_requests/3", "draft": true, "source_branch": "feat/x", "author": map[string]any{"username": "bob"}, "references": map[string]any{"full": "g/p!3"}},
			{"web_url": "https://gitlab.corp.example/g/sub/q/-/merge_requests/9", "source_branch": "fix/y", "author": map[string]any{"username": "carol"}},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := &GitLabChecker{HTTPClient: srv.Client(), TokenFunc: func(context.Context, string) (string, error) { return "tok", nil }, apiBaseOverride: srv.URL}

	res, err := c.SearchMergeRequests(t.Context(), "gitlab.corp.example", []GitLabSearch{{Endpoint: "/api/v4/groups/g/merge_requests", Query: url.Values{"state": {"opened"}, "per_page": {"100"}}}})
	if err != nil {
		t.Fatalf("SearchMergeRequests() error = %v", err)
	}
	if gotQuery.Get("state") != "opened" {
		t.Errorf("query = %v", gotQuery)
	}
	if len(res.Matches) != 2 || res.Matches[0].Repo != "g/p" || !res.Matches[0].Draft || res.Matches[0].Author != "bob" || res.Matches[0].HeadRef != "feat/x" {
		t.Errorf("matches = %+v", res.Matches)
	}
	if res.Matches[1].Repo != "g/sub/q" {
		t.Errorf("repo without references = %q, want it parsed from the URL", res.Matches[1].Repo)
	}
	if !res.Complete || res.Budget.Remaining != 1999 {
		t.Errorf("complete = %v, budget = %+v", res.Complete, res.Budget)
	}
}

func TestResolveGitLabScope(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/projects/g%2Fp", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"id": 1}`)) })
	mux.HandleFunc("/api/v4/groups/g%2Fsub", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"id": 2}`)) })
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := &GitLabChecker{HTTPClient: srv.Client(), TokenFunc: func(context.Context, string) (string, error) { return "tok", nil }, apiBaseOverride: srv.URL}

	for path, want := range map[string]string{"g/p": "repo", "g/sub": "org"} {
		if got, err := c.ResolveScopeKind(t.Context(), "h", path); err != nil || got != want {
			t.Errorf("ResolveScopeKind(%q) = (%q, %v), want %q", path, got, err, want)
		}
	}
	if _, err := c.ResolveScopeKind(t.Context(), "h", "nope/nothing"); err == nil {
		t.Error("an unknown path resolved")
	}
}
