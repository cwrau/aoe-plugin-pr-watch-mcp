package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"regexp"
	"strconv"
	"strings"
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
	// Errors carries GraphQL-level failures (rate limiting, NOT_FOUND,
	// field errors). GitHub returns these with HTTP 200, so checkHTTPStatus
	// never sees them — without this, an error response decodes into a
	// zero-value PullRequest that mapGitHubState reads as a freshly "open"
	// PR with no reviews and no CI.
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
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
	// An empty state means `pullRequest` came back null (e.g. the number
	// doesn't exist in that repo). Treating that as data would hand
	// mapGitHubState("") -> "open" a zero-value CheckResult, which then gets
	// persisted as the new PrevState and makes the next successful poll
	// re-fire every review/CI event as if it were new.
	if pr.State == "" {
		return CheckResult{}, GitHubBudgets{}, fmt.Errorf("github graphql: no pullRequest in response for %s", prURL)
	}

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

	if err := checkHTTPStatus(resp); err != nil {
		return nil, fmt.Errorf("github graphql: %w", err)
	}

	var out githubGraphQLResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode github graphql response: %w", err)
	}
	if len(out.Errors) > 0 {
		messages := make([]string, 0, len(out.Errors))
		for _, e := range out.Errors {
			messages = append(messages, e.Message)
		}
		return nil, fmt.Errorf("github graphql returned %d error(s): %s", len(out.Errors), strings.Join(messages, "; "))
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
	// Escape every interpolated component: an owner/repo containing "?" or
	// "#" would otherwise retarget the request path. per_page=100 raises the
	// page size from GitHub's default of 30 so a PR with many workflow
	// reruns can't push the one action_required run this cross-check exists
	// to catch off the first page.
	query := neturl.Values{"head_sha": {headSHA}, "per_page": {"100"}}
	endpoint := fmt.Sprintf("%s/repos/%s/%s/actions/runs?%s",
		c.baseURL(), neturl.PathEscape(owner), neturl.PathEscape(repo), query.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
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

	if err := checkHTTPStatus(resp); err != nil {
		return budget, false, fmt.Errorf("github actions runs: %w", err)
	}

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

// maxHTTPErrorBodyBytes caps how much of a non-2xx response body is read
// into an error message, so a large or misbehaving error page can't blow up
// the error string.
const maxHTTPErrorBodyBytes = 512

// checkHTTPStatus returns an error describing the status code and a
// truncated response body if resp is not a 2xx response. Callers must check
// this before decoding resp.Body as JSON: an auth failure, secondary rate
// limit, or outage response often isn't shaped like the expected payload
// and would otherwise decode silently into zero values (e.g. a PR
// mis-reported as freshly "open" with no reviews), which can then get
// persisted as PrevState and cause spurious diff events on the next
// successful poll.
func checkHTTPStatus(resp *http.Response) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxHTTPErrorBodyBytes))
	return fmt.Errorf("unexpected status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
}

func parseGitHubRESTRateHeaders(resp *http.Response) RateBudget {
	limit, _ := strconv.Atoi(resp.Header.Get("X-RateLimit-Limit"))
	remaining, _ := strconv.Atoi(resp.Header.Get("X-RateLimit-Remaining"))
	resetUnix, _ := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64)
	return RateBudget{Limit: limit, Remaining: remaining, ResetAt: parseUnixOrZero(resetUnix)}
}

func parseRFC3339OrZero(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// parseUnixOrZero converts a Unix timestamp to time.Time, treating
// non-positive values (missing/unparseable header) as the zero time —
// shared by both GitHub's REST rate-limit headers and GitLab's RateLimit-*
// headers, which use the same encoding.
func parseUnixOrZero(u int64) time.Time {
	if u <= 0 {
		return time.Time{}
	}
	return time.Unix(u, 0)
}
