package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
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
// discussion count via REST — the original three calls of the bash script's
// poll_gitlab (ported to Go so the scheduler can read RateLimit-* response
// headers directly instead of shelling to `glab`), plus two conditional
// calls added since: /notes (only once at least one approval exists) for
// the latest approver's name and timestamp, and /pipelines/:id/jobs (only
// on a failing pipeline) for the failing job's name.
type GitLabChecker struct {
	HTTPClient *http.Client

	// TokenFunc resolves the credential to send for a given MR's host,
	// called fresh on every Check — never cached across hosts. It takes
	// Check's own context because resolving a token typically means forking
	// a credential-helper subprocess in the scheduler's hot path, and a
	// hung keyring backend would otherwise stall the whole scheduler. This
	// per-host resolution is
	// deliberate: a single static token sent to whatever host a
	// registered pr_url happens to name would hand a real, working
	// GitLab credential to any host an attacker can get accepted as a
	// registration (URL shape validation alone can't distinguish a
	// trusted self-hosted GitLab instance from an attacker's server
	// formatted to look like one). Resolving per-host means an
	// unconfigured host naturally has no secret to leak; see
	// TokenFunc's caller in Check for the empty-token short-circuit.
	TokenFunc   func(ctx context.Context, host string) (string, error)
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
	State       string `json:"state"`
	Title       string `json:"title"`
	Description string `json:"description"`
	MergedAt    string `json:"merged_at"`
	Pipeline    *struct {
		ID     int    `json:"id"`
		Status string `json:"status"`
	} `json:"pipeline"`
	HeadPipeline *struct {
		ID     int    `json:"id"`
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

// gitlabNote is one entry from the MR's /notes feed. A system note whose
// Body is GitLab's fixed "approved this merge request" string is the only
// durable source of an approval's timestamp and author: /approvals only
// reports who currently holds an approval, not when they gave it.
type gitlabNote struct {
	System    bool   `json:"system"`
	Body      string `json:"body"`
	CreatedAt string `json:"created_at"`
	Author    struct {
		Username string `json:"username"`
	} `json:"author"`
}

// gitlabApprovedNoteBody is the fixed system-note text GitLab writes when a
// user approves an MR — not documented as a stable API contract, but it's
// the same string GitLab's own web UI activity feed renders from, so a
// wording change would be a user-visible break for GitLab itself too.
const gitlabApprovedNoteBody = "approved this merge request"

type gitlabJobsResponse []struct {
	Name string `json:"name"`
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
	token, err := c.TokenFunc(ctx, host)
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

	result := CheckResult{State: mapGitLabState(mrData.State), Title: mrData.Title, Description: mrData.Description}
	if result.State == "merged" || result.State == "closed" {
		if result.State == "merged" {
			result.MergedAt = parseRFC3339OrZero(mrData.MergedAt)
		}
		return result, budget, nil
	}

	pipelineStatus := "none"
	var pipelineID int
	if mrData.Pipeline != nil && mrData.Pipeline.Status != "" {
		pipelineStatus = mrData.Pipeline.Status
		pipelineID = mrData.Pipeline.ID
	} else if mrData.HeadPipeline != nil {
		pipelineStatus = mrData.HeadPipeline.Status
		pipelineID = mrData.HeadPipeline.ID
	}
	result.CIBucket = mapGitLabPipelineStatus(pipelineStatus)

	if result.CIBucket == "failure" && pipelineID != 0 {
		detail, _, err := c.getJSON(ctx, token, fmt.Sprintf("%s/api/v4/projects/%s/pipelines/%d/jobs?scope[]=failed&per_page=1", base, encodedPath, pipelineID), &gitlabJobsResponse{})
		if err != nil {
			return CheckResult{}, RateBudget{}, err
		}
		if jobs := *detail.(*gitlabJobsResponse); len(jobs) > 0 {
			result.CIDetail = jobs[0].Name
		}
	}

	approvalsRaw, _, err := c.getJSON(ctx, token, fmt.Sprintf("%s/api/v4/projects/%s/merge_requests/%d/approvals", base, encodedPath, iid), &gitlabApprovalsResponse{})
	if err != nil {
		return CheckResult{}, RateBudget{}, err
	}
	result.ReviewCount = len(approvalsRaw.(*gitlabApprovalsResponse).ApprovedBy)

	if result.ReviewCount > 0 {
		var notes []gitlabNote
		if _, _, err := c.getJSON(ctx, token, fmt.Sprintf("%s/api/v4/projects/%s/merge_requests/%d/notes?sort=desc&order_by=created_at&per_page=20", base, encodedPath, iid), &notes); err != nil {
			return CheckResult{}, RateBudget{}, err
		}
		for _, n := range notes {
			if n.System && strings.Contains(n.Body, gitlabApprovedNoteBody) {
				result.LatestReviewer = n.Author.Username
				result.LatestReviewState = "APPROVED"
				result.LatestReviewAt = parseRFC3339OrZero(n.CreatedAt)
				break
			}
		}
	}

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
	defer func() { _ = resp.Body.Close() }()

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
