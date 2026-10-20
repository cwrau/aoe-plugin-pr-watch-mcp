package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// ScanResult is what one scan found.
type ScanResult struct {
	Matches []ScanMatch
	// Complete is false when the provider had more matches than were
	// returned, so a PR missing from Matches may still match.
	Complete bool
	Budget   RateBudget
}

const githubSearchQuery = `query($q: String!, $first: Int!) {
  search(query: $q, type: ISSUE, first: $first) {
    issueCount
    nodes {
      ... on PullRequest {
        url
        isDraft
        headRefName
        author { login }
        repository { nameWithOwner }
      }
    }
  }
  rateLimit { limit remaining resetAt cost }
}`

// SearchPullRequests runs the search queries (see BuildGitHubSearchQueries)
// and returns the union of their pull requests, at most limit per query.
func (c *GitHubChecker) SearchPullRequests(ctx context.Context, queries []string, limit int) (ScanResult, error) {
	res := ScanResult{Complete: true}
	seen := make(map[string]bool)
	for _, q := range queries {
		var resp struct {
			Search struct {
				IssueCount int `json:"issueCount"`
				Nodes      []struct {
					URL         string `json:"url"`
					IsDraft     bool   `json:"isDraft"`
					HeadRefName string `json:"headRefName"`
					Author      *struct {
						Login string `json:"login"`
					} `json:"author"`
					Repository struct {
						NameWithOwner string `json:"nameWithOwner"`
					} `json:"repository"`
				} `json:"nodes"`
			} `json:"search"`
			RateLimit struct {
				Limit     int    `json:"limit"`
				Remaining int    `json:"remaining"`
				ResetAt   string `json:"resetAt"`
			} `json:"rateLimit"`
		}
		if err := c.doGraphQL(ctx, githubSearchQuery, map[string]any{"q": q, "first": limit}, &resp); err != nil {
			return ScanResult{}, fmt.Errorf("github search %q: %w", q, err)
		}
		res.Budget = RateBudget{Limit: resp.RateLimit.Limit, Remaining: resp.RateLimit.Remaining, ResetAt: parseRFC3339OrZero(resp.RateLimit.ResetAt)}
		returned := 0
		for _, n := range resp.Search.Nodes {
			if n.URL == "" { // a node that is not a pull request
				continue
			}
			returned++
			if seen[n.URL] {
				continue
			}
			seen[n.URL] = true
			m := ScanMatch{URL: n.URL, Repo: n.Repository.NameWithOwner, HeadRef: n.HeadRefName, Draft: n.IsDraft}
			if n.Author != nil {
				m.Author = n.Author.Login
			}
			res.Matches = append(res.Matches, m)
		}
		if resp.Search.IssueCount > returned {
			res.Complete = false
		}
	}
	return res, nil
}

type gitlabScanMR struct {
	WebURL       string `json:"web_url"`
	Draft        bool   `json:"draft"`
	SourceBranch string `json:"source_branch"`
	Author       struct {
		Username string `json:"username"`
	} `json:"author"`
	References struct {
		Full string `json:"full"`
	} `json:"references"`
}

// SearchMergeRequests runs the list requests (see BuildGitLabSearches) against
// host and returns the union of their merge requests.
func (c *GitLabChecker) SearchMergeRequests(ctx context.Context, host string, searches []GitLabSearch) (ScanResult, error) {
	if c.TokenFunc == nil {
		return ScanResult{}, fmt.Errorf("no GitLab credential for %s", host)
	}
	token, err := c.token(ctx, host)
	if err != nil {
		return ScanResult{}, err
	}
	if token == "" {
		return ScanResult{}, fmt.Errorf("no GitLab credential for %s", host)
	}
	base := c.apiBaseOverride
	if base == "" {
		base = "https://" + host
	}

	res := ScanResult{Complete: true}
	seen := make(map[string]bool)
	for _, s := range searches {
		var mrs []gitlabScanMR
		_, budget, err := c.getJSON(ctx, token, base+s.Endpoint+"?"+s.Query.Encode(), &mrs)
		if err != nil {
			return ScanResult{}, err
		}
		res.Budget = budget
		if perPage := s.Query.Get("per_page"); perPage != "" && fmt.Sprint(len(mrs)) == perPage {
			res.Complete = false
		}
		for _, mr := range mrs {
			if mr.WebURL == "" || seen[mr.WebURL] {
				continue
			}
			seen[mr.WebURL] = true
			// references.full is "group/project!iid"; fall back to the URL.
			repo, _, _ := strings.Cut(mr.References.Full, "!")
			if repo == "" {
				if _, path, _, perr := ParseGitLabMRURL(mr.WebURL); perr == nil {
					repo = path
				}
			}
			res.Matches = append(res.Matches, ScanMatch{URL: mr.WebURL, Repo: repo, Author: mr.Author.Username, HeadRef: mr.SourceBranch, Draft: mr.Draft})
		}
	}
	return res, nil
}

// ResolveScopeKind tells whether path on host is a project ("repo") or a
// group ("org"): a project and a group look alike in a URL.
func (c *GitLabChecker) ResolveScopeKind(ctx context.Context, host, path string) (string, error) {
	if c.TokenFunc == nil {
		return "", fmt.Errorf("no GitLab credential for %s", host)
	}
	token, err := c.token(ctx, host)
	if err != nil {
		return "", err
	}
	if token == "" {
		return "", fmt.Errorf("no GitLab credential for %s", host)
	}
	base := c.apiBaseOverride
	if base == "" {
		base = "https://" + host
	}
	enc := url.PathEscape(path)
	for _, probe := range []struct{ endpoint, kind string }{{"/api/v4/projects/", "repo"}, {"/api/v4/groups/", "org"}} {
		_, _, err := c.getJSON(ctx, token, base+probe.endpoint+enc, &struct{}{})
		if err == nil {
			return probe.kind, nil
		}
		if status, ok := errors.AsType[*httpStatusError](err); ok && status.Code == http.StatusNotFound {
			continue
		}
		return "", err
	}
	return "", fmt.Errorf("%s/%s is neither a project nor a group the credential can see", host, path)
}
