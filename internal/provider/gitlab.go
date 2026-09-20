package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
)

var gitlabMRURLPattern = regexp.MustCompile(`^https?://([^/]+)/(.+)/-/merge_requests/(\d+)`)

// ParseGitLabMRURL mirrors the bash script's sed extraction of host,
// project path (which may contain nested groups), and MR iid.
func ParseGitLabMRURL(mrURL string) (host, projectPath string, iid int, err error) {
	m := gitlabMRURLPattern.FindStringSubmatch(mrURL)
	if m == nil {
		return "", "", 0, fmt.Errorf("not a gitlab merge request url: %s", mrURL)
	}
	n, err := strconv.Atoi(m[3])
	if err != nil {
		return "", "", 0, fmt.Errorf("invalid MR iid in %s: %w", mrURL, err)
	}
	return m[1], m[2], n, nil
}

// GitLabChecker polls a single MR's state, approval count, and unresolved
// discussion count via three REST calls — same shape as the bash script's
// poll_gitlab, ported to Go so the scheduler can read RateLimit-* response
// headers directly instead of shelling to `glab`.
type GitLabChecker struct {
	HTTPClient *http.Client

	// TokenFunc resolves the credential to send for a given MR's host,
	// called fresh on every Check — never cached across hosts. This is
	// deliberate: a single static token sent to whatever host a
	// registered pr_url happens to name would hand a real, working
	// GitLab credential to any host an attacker can get accepted as a
	// registration (URL shape validation alone can't distinguish a
	// trusted self-hosted GitLab instance from an attacker's server
	// formatted to look like one). Resolving per-host means an
	// unconfigured host naturally has no secret to leak; see
	// TokenFunc's caller in Check for the empty-token short-circuit.
	TokenFunc   func(host string) (string, error)
	TokenHeader string // defaults to "PRIVATE-TOKEN"; see Task 4's auth-header note

	// apiBaseOverride replaces "https://{host}/api/v4" with a fixed base
	// (test-only: a fake host can't answer the URL's real host header).
	apiBaseOverride string
}

func (c *GitLabChecker) tokenHeader() string {
	if c.TokenHeader != "" {
		return c.TokenHeader
	}
	return "PRIVATE-TOKEN"
}

type gitlabMRResponse struct {
	State    string `json:"state"`
	Pipeline *struct {
		Status string `json:"status"`
	} `json:"pipeline"`
	HeadPipeline *struct {
		Status string `json:"status"`
	} `json:"head_pipeline"`
}

type gitlabApprovalsResponse struct {
	ApprovedBy []json.RawMessage `json:"approved_by"`
}

type gitlabDiscussion struct {
	Notes []struct {
		Resolvable bool `json:"resolvable"`
		Resolved   bool `json:"resolved"`
	} `json:"notes"`
}

func mapGitLabPipelineStatus(status string) string {
	switch status {
	case "success":
		return "success"
	case "failed", "canceled":
		return "failure"
	case "running", "pending", "created", "preparing", "waiting_for_resource":
		return "pending"
	default:
		return "none"
	}
}

func (c *GitLabChecker) Check(ctx context.Context, mrURL string) (CheckResult, RateBudget, error) {
	host, projectPath, iid, err := ParseGitLabMRURL(mrURL)
	if err != nil {
		return CheckResult{}, RateBudget{}, err
	}

	if c.TokenFunc == nil {
		return CheckResult{}, RateBudget{}, fmt.Errorf("gitlab %s: no TokenFunc configured", host)
	}
	token, err := c.TokenFunc(host)
	if err != nil {
		return CheckResult{}, RateBudget{}, fmt.Errorf("resolve gitlab token for %s: %w", host, err)
	}
	if token == "" {
		return CheckResult{}, RateBudget{}, fmt.Errorf("no gitlab token configured for host %s", host)
	}

	base := c.apiBaseOverride
	if base == "" {
		base = "https://" + host
	}
	encodedPath := url.PathEscape(projectPath)

	mr, budget, err := c.getJSON(ctx, token, fmt.Sprintf("%s/api/v4/projects/%s/merge_requests/%d", base, encodedPath, iid), &gitlabMRResponse{})
	if err != nil {
		return CheckResult{}, RateBudget{}, err
	}
	mrData := mr.(*gitlabMRResponse)

	result := CheckResult{State: mapGitLabState(mrData.State)}
	if result.State == "merged" || result.State == "closed" {
		return result, budget, nil
	}

	pipelineStatus := "none"
	if mrData.Pipeline != nil && mrData.Pipeline.Status != "" {
		pipelineStatus = mrData.Pipeline.Status
	} else if mrData.HeadPipeline != nil {
		pipelineStatus = mrData.HeadPipeline.Status
	}
	result.CIBucket = mapGitLabPipelineStatus(pipelineStatus)

	approvalsRaw, _, err := c.getJSON(ctx, token, fmt.Sprintf("%s/api/v4/projects/%s/merge_requests/%d/approvals", base, encodedPath, iid), &gitlabApprovalsResponse{})
	if err != nil {
		return CheckResult{}, RateBudget{}, err
	}
	result.ReviewCount = len(approvalsRaw.(*gitlabApprovalsResponse).ApprovedBy)

	var discussions []gitlabDiscussion
	if _, _, err := c.getJSON(ctx, token, fmt.Sprintf("%s/api/v4/projects/%s/merge_requests/%d/discussions?per_page=100", base, encodedPath, iid), &discussions); err != nil {
		return CheckResult{}, RateBudget{}, err
	}
	for _, d := range discussions {
		for _, n := range d.Notes {
			if n.Resolvable && !n.Resolved {
				result.Unresolved++
				break
			}
		}
	}

	return result, budget, nil
}

func mapGitLabState(state string) string {
	switch state {
	case "merged":
		return "merged"
	case "closed":
		return "closed"
	default:
		return "open"
	}
}

// getJSON issues a GET, decodes into out (out must be a pointer, or a
// pointer-to-slice for the discussions endpoint), and returns the parsed
// RateLimit-* headers from the response. A non-2xx response is rejected
// before decoding — see checkHTTPStatus in github.go for why: an
// auth/rate-limit/outage response often isn't shaped like the expected
// payload and would otherwise decode silently into zero values.
func (c *GitLabChecker) getJSON(ctx context.Context, token, url string, out any) (any, RateBudget, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, RateBudget{}, err
	}
	req.Header.Set(c.tokenHeader(), token)

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, RateBudget{}, fmt.Errorf("gitlab request to %s: %w", url, err)
	}
	defer resp.Body.Close()

	budget := parseGitLabRateHeaders(resp)

	if err := checkHTTPStatus(resp); err != nil {
		return nil, budget, fmt.Errorf("gitlab %s: %w", url, err)
	}

	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return nil, budget, fmt.Errorf("decode gitlab response from %s: %w", url, err)
	}
	return out, budget, nil
}

func parseGitLabRateHeaders(resp *http.Response) RateBudget {
	limit, _ := strconv.Atoi(resp.Header.Get("RateLimit-Limit"))
	remaining, _ := strconv.Atoi(resp.Header.Get("RateLimit-Remaining"))
	resetUnix, _ := strconv.ParseInt(resp.Header.Get("RateLimit-Reset"), 10, 64)
	resetAt := parseUnixOrZero(resetUnix)
	return RateBudget{Limit: limit, Remaining: remaining, ResetAt: resetAt}
}
