package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"time"
)

var githubPRURLPattern = regexp.MustCompile(`^https://github\.com/([^/]+)/([^/]+)/pull/(\d+)`)

// ParseGitHubPRURL mirrors the bash script's sed extraction of owner/repo/number.
func ParseGitHubPRURL(url string) (owner, repo string, number int, err error) {
	m := githubPRURLPattern.FindStringSubmatch(url)
	if m == nil {
		return "", "", 0, fmt.Errorf("not a github pull request url: %s", url)
	}
	n, err := strconv.Atoi(m[3])
	if err != nil {
		return "", "", 0, fmt.Errorf("invalid PR number in %s: %w", url, err)
	}
	return m[1], m[2], n, nil
}

type GitHubBudgets struct {
	REST    RateBudget
	GraphQL RateBudget
}

// GitHubChecker polls a single PR's state via GitHub's GraphQL API, then
// cross-checks the Actions runs REST API for the head SHA to catch
// action_required workflow runs (stuck on a fork-PR maintainer-approval
// gate) that never produce a check-run and are therefore invisible to any
// statusCheckRollup shape. Both calls' rate-limit signal is read off the
// response itself — GraphQL via the `rateLimit` field in the body, REST via
// the X-RateLimit-* response headers — never via a dedicated polling call.
type GitHubChecker struct {
	HTTPClient *http.Client
	Token      string
	BaseURL    string // defaults to https://api.github.com; overridable for tests
}

func (c *GitHubChecker) baseURL() string {
	if c.BaseURL != "" {
		return c.BaseURL
	}
	return "https://api.github.com"
}

const githubQuery = `
query($owner: String!, $repo: String!, $number: Int!) {
  repository(owner: $owner, name: $repo) {
    pullRequest(number: $number) {
      state
      headRefOid
      reviewCount: reviews { totalCount }
      latestReview: reviews(last: 1) { nodes { author { login } state } }
      commits(last: 1) {
        nodes { commit { statusCheckRollup { state } } }
      }
    }
  }
  rateLimit { limit remaining resetAt }
}
`

type githubGraphQLResponse struct {
	Data struct {
		Repository struct {
			PullRequest struct {
				State       string `json:"state"`
				HeadRefOid  string `json:"headRefOid"`
				ReviewCount struct {
					TotalCount int `json:"totalCount"`
				} `json:"reviewCount"`
				LatestReview struct {
					Nodes []struct {
						Author struct {
							Login string `json:"login"`
						} `json:"author"`
						State string `json:"state"`
					} `json:"nodes"`
				} `json:"latestReview"`
				Commits struct {
					Nodes []struct {
						Commit struct {
							StatusCheckRollup *struct {
								State string `json:"state"`
							} `json:"statusCheckRollup"`
						} `json:"commit"`
					} `json:"nodes"`
				} `json:"commits"`
			} `json:"pullRequest"`
		} `json:"repository"`
	} `json:"data"`
	RateLimit struct {
		Limit     int    `json:"limit"`
		Remaining int    `json:"remaining"`
		ResetAt   string `json:"resetAt"`
	} `json:"rateLimit"`
}

func mapGitHubRollupState(state string) string {
	switch state {
	case "SUCCESS":
		return "success"
	case "FAILURE", "ERROR":
		return "failure"
	case "PENDING", "EXPECTED":
		return "pending"
	default:
		return "none"
	}
}

func (c *GitHubChecker) Check(ctx context.Context, prURL string) (CheckResult, GitHubBudgets, error) {
	owner, repo, number, err := ParseGitHubPRURL(prURL)
	if err != nil {
		return CheckResult{}, GitHubBudgets{}, err
	}

	gqlResp, err := c.queryGraphQL(ctx, owner, repo, number)
	if err != nil {
		return CheckResult{}, GitHubBudgets{}, err
	}
	pr := gqlResp.Data.Repository.PullRequest

	result := CheckResult{
		State:       mapGitHubState(pr.State),
		ReviewCount: pr.ReviewCount.TotalCount,
	}
	if len(pr.LatestReview.Nodes) > 0 {
		result.LatestReviewer = pr.LatestReview.Nodes[0].Author.Login
		result.LatestReviewState = pr.LatestReview.Nodes[0].State
	}
	if len(pr.Commits.Nodes) > 0 && pr.Commits.Nodes[0].Commit.StatusCheckRollup != nil {
		result.CIBucket = mapGitHubRollupState(pr.Commits.Nodes[0].Commit.StatusCheckRollup.State)
	} else {
		result.CIBucket = "none"
	}

	budgets := GitHubBudgets{
		GraphQL: RateBudget{
			Limit:     gqlResp.RateLimit.Limit,
			Remaining: gqlResp.RateLimit.Remaining,
			ResetAt:   parseRFC3339OrZero(gqlResp.RateLimit.ResetAt),
		},
	}

	// Terminal states don't need the action_required cross-check: the PR is
	// already resolved, matching the bash script's early-return before ever
	// reaching CI bucket logic.
	if result.State == "merged" || result.State == "closed" {
		return result, budgets, nil
	}

	restBudget, actionRequired, err := c.checkActionRequired(ctx, owner, repo, pr.HeadRefOid)
	if err != nil {
		return CheckResult{}, GitHubBudgets{}, err
	}
	budgets.REST = restBudget
	if actionRequired && result.CIBucket != "failure" {
		result.CIBucket = "action_required"
	}

	return result, budgets, nil
}

func mapGitHubState(state string) string {
	switch state {
	case "MERGED":
		return "merged"
	case "CLOSED":
		return "closed"
	default:
		return "open"
	}
}

func (c *GitHubChecker) queryGraphQL(ctx context.Context, owner, repo string, number int) (*githubGraphQLResponse, error) {
	payload, err := json.Marshal(map[string]any{
		"query": githubQuery,
		"variables": map[string]any{
			"owner":  owner,
			"repo":   repo,
			"number": number,
		},
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL()+"/graphql", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("github graphql request: %w", err)
	}
	defer resp.Body.Close()

	var out githubGraphQLResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode github graphql response: %w", err)
	}
	return &out, nil
}

type githubActionsRunsResponse struct {
	WorkflowRuns []struct {
		Conclusion string `json:"conclusion"`
	} `json:"workflow_runs"`
}

func (c *GitHubChecker) checkActionRequired(ctx context.Context, owner, repo, headSHA string) (RateBudget, bool, error) {
	if headSHA == "" {
		return RateBudget{}, false, nil
	}
	url := fmt.Sprintf("%s/repos/%s/%s/actions/runs?head_sha=%s", c.baseURL(), owner, repo, headSHA)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return RateBudget{}, false, err
	}
	req.Header.Set("Authorization", "bearer "+c.Token)

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return RateBudget{}, false, fmt.Errorf("github actions runs request: %w", err)
	}
	defer resp.Body.Close()

	budget := parseGitHubRESTRateHeaders(resp)

	var out githubActionsRunsResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return budget, false, fmt.Errorf("decode github actions runs response: %w", err)
	}
	for _, run := range out.WorkflowRuns {
		if run.Conclusion == "action_required" {
			return budget, true, nil
		}
	}
	return budget, false, nil
}

func parseGitHubRESTRateHeaders(resp *http.Response) RateBudget {
	limit, _ := strconv.Atoi(resp.Header.Get("X-RateLimit-Limit"))
	remaining, _ := strconv.Atoi(resp.Header.Get("X-RateLimit-Remaining"))
	resetUnix, _ := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64)
	var resetAt time.Time
	if resetUnix > 0 {
		resetAt = time.Unix(resetUnix, 0)
	}
	return RateBudget{Limit: limit, Remaining: remaining, ResetAt: resetAt}
}

func parseRFC3339OrZero(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}
