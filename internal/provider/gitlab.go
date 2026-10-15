package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
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

	// TokenFunc resolves the credential to send for a given MR's host. The
	// answer is cached per host (see token) and only asked for again after
	// the host rejected it with a 401, since each call forks a
	// credential-helper subprocess; an empty or failed answer is not cached,
	// so a credential configured later is picked up. It takes the calling
	// request's own context because a hung keyring backend would otherwise
	// stall the whole scheduler. The per-host resolution is
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

	tokenMu sync.Mutex
	tokens  map[string]string // host -> credential TokenFunc last returned for it

	// apiBaseOverride replaces "https://{host}/api/v4" with a fixed base
	// (test-only: a fake host can't answer the URL's real host header).
	apiBaseOverride string

	// selfUsernames caches SelfUsername's per-host result — unlike
	// TokenFunc (deliberately never cached), a username doesn't change for
	// the lifetime of the token behind it, and GitLabChecker is
	// scheduler-only (never shared with the MCP HTTP handler the way
	// GitHubChecker is), but Check/DiscoverBranchMR still run from the same
	// scheduler goroutine sequentially — a plain map with a mutex is enough,
	// no need for anything fancier.
	selfUsernameMu sync.Mutex
	selfUsernames  map[string]string
}

func (c *GitLabChecker) tokenHeader() string {
	if c.TokenHeader != "" {
		return c.TokenHeader
	}
	return "PRIVATE-TOKEN"
}

type gitlabMRResponse struct {
	State               string `json:"state"`
	Title               string `json:"title"`
	Description         string `json:"description"`
	MergedAt            string `json:"merged_at"`
	HasConflicts        bool   `json:"has_conflicts"`
	DetailedMergeStatus string `json:"detailed_merge_status"` // "merge_request_blocked" means blocked on an unmerged MR dependency (Premium/Ultimate feature; always some other value on Free)
	Pipeline            *struct {
		ID     int    `json:"id"`
		Status string `json:"status"`
		WebURL string `json:"web_url"`
	} `json:"pipeline"`
	HeadPipeline *struct {
		ID     int    `json:"id"`
		Status string `json:"status"`
		WebURL string `json:"web_url"`
	} `json:"head_pipeline"`
}

type gitlabApprovalsResponse struct {
	ApprovedBy        []json.RawMessage `json:"approved_by"`
	Approved          *bool             `json:"approved"`
	ApprovalsRequired int               `json:"approvals_required"`
	ApprovalsLeft     int               `json:"approvals_left"`
}

// reviewState is the state to report for the latest approval: plain APPROVED
// once GitLab considers the MR approved, otherwise the progress ("APPROVED
// 1/2") so a partially approved MR does not read as ready to merge.
func (a gitlabApprovalsResponse) reviewState() string {
	incomplete := a.ApprovalsLeft > 0 || (a.Approved != nil && !*a.Approved)
	if !incomplete {
		return "APPROVED"
	}
	if a.ApprovalsRequired > 0 {
		return fmt.Sprintf("APPROVED %d/%d", len(a.ApprovedBy), a.ApprovalsRequired)
	}
	return "APPROVED (more needed)"
}

type gitlabDiscussion struct {
	ID    string `json:"id"`
	Notes []struct {
		ID         int    `json:"id"`
		System     bool   `json:"system"`
		Body       string `json:"body"`
		Resolvable bool   `json:"resolvable"`
		Resolved   bool   `json:"resolved"`
		Author     struct {
			Username string `json:"username"`
		} `json:"author"`
	} `json:"notes"`
}

// gitlabNote is one entry from the MR's /notes feed. A system note whose
// Body is GitLab's fixed "approved this merge request" string is the only
// durable source of an approval's timestamp and author: /approvals only
// reports who currently holds an approval, not when they gave it.
type gitlabNote struct {
	ID        int    `json:"id"`
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
	Name   string `json:"name"`
	WebURL string `json:"web_url"`
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

// token returns host's cached credential, calling TokenFunc only when there
// is none. Only a non-empty, successful answer is cached.
func (c *GitLabChecker) token(ctx context.Context, host string) (string, error) {
	c.tokenMu.Lock()
	cached := c.tokens[host]
	c.tokenMu.Unlock()
	if cached != "" {
		return cached, nil
	}
	token, err := c.TokenFunc(ctx, host)
	if err != nil || token == "" {
		return token, err
	}
	c.tokenMu.Lock()
	if c.tokens == nil {
		c.tokens = make(map[string]string)
	}
	c.tokens[host] = token
	c.tokenMu.Unlock()
	return token, nil
}

// forgetToken drops used wherever it is cached, after a host rejected it, so
// the next request asks TokenFunc for a fresh one.
func (c *GitLabChecker) forgetToken(used string) {
	c.tokenMu.Lock()
	defer c.tokenMu.Unlock()
	for host, token := range c.tokens {
		if token == used {
			delete(c.tokens, host)
		}
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
	token, err := c.token(ctx, host)
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

	result := CheckResult{State: mapGitLabState(mrData.State), Title: mrData.Title, Description: mrData.Description, HasConflict: mrData.HasConflicts, Blocked: mrData.DetailedMergeStatus == "merge_request_blocked"}
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
		result.CIHref = mrData.Pipeline.WebURL
	} else if mrData.HeadPipeline != nil {
		pipelineStatus = mrData.HeadPipeline.Status
		pipelineID = mrData.HeadPipeline.ID
		result.CIHref = mrData.HeadPipeline.WebURL
	}
	result.CIBucket = mapGitLabPipelineStatus(pipelineStatus)

	// On a failing pipeline, prefer the specific failing job's own link over
	// the pipeline's — same specificity buildCheckResult's CIDetail already
	// aims for, just extended to the href.
	if result.CIBucket == "failure" && pipelineID != 0 {
		detail, _, err := c.getJSON(ctx, token, fmt.Sprintf("%s/api/v4/projects/%s/pipelines/%d/jobs?scope[]=failed&per_page=1", base, encodedPath, pipelineID), &gitlabJobsResponse{})
		if err != nil {
			return CheckResult{}, RateBudget{}, err
		}
		if jobs := *detail.(*gitlabJobsResponse); len(jobs) > 0 {
			result.CIDetail = jobs[0].Name
			result.CIHref = jobs[0].WebURL
		}
	}

	// Not every instance or project offers the approvals endpoint (403/404
	// where the feature is off or the tier lacks it); such an MR simply has no
	// approvals, rather than being unwatchable.
	var approvals gitlabApprovalsResponse
	if _, _, err := c.getJSON(ctx, token, fmt.Sprintf("%s/api/v4/projects/%s/merge_requests/%d/approvals", base, encodedPath, iid), &approvals); err != nil {
		if status, ok := errors.AsType[*httpStatusError](err); !ok || (status.Code != http.StatusForbidden && status.Code != http.StatusNotFound) {
			return CheckResult{}, RateBudget{}, err
		}
		approvals = gitlabApprovalsResponse{}
	}
	result.ReviewCount = len(approvals.ApprovedBy)

	if result.ReviewCount > 0 {
		var notes []gitlabNote
		if _, _, err := c.getJSON(ctx, token, fmt.Sprintf("%s/api/v4/projects/%s/merge_requests/%d/notes?sort=desc&order_by=created_at&per_page=20", base, encodedPath, iid), &notes); err != nil {
			return CheckResult{}, RateBudget{}, err
		}
		for _, n := range notes {
			if n.System && strings.Contains(n.Body, gitlabApprovedNoteBody) {
				result.LatestReviewer = n.Author.Username
				result.LatestReviewState = approvals.reviewState()
				result.LatestReviewAt = parseRFC3339OrZero(n.CreatedAt)
				result.ReviewHref = fmt.Sprintf("%s#note_%d", mrURL, n.ID)
				break
			}
		}
	}

	var discussions []gitlabDiscussion
	if _, _, err := c.getJSON(ctx, token, fmt.Sprintf("%s/api/v4/projects/%s/merge_requests/%d/discussions?per_page=100", base, encodedPath, iid), &discussions); err != nil {
		return CheckResult{}, RateBudget{}, err
	}
	// The same /discussions response covers both inline review threads and
	// plain top-level MR comments: GitLab wraps every top-level comment as
	// its own single-note "discussion" (individual_note: true) with
	// resolvable false on that note, the same shape a resolvable-but-absent
	// note has for a genuinely un-resolvable discussion — so "no note in
	// this discussion is resolvable" is what marks it as a top-level
	// comment rather than a review thread, no separate API call needed.
	// System-generated notes (system: true — "requested review from",
	// "assigned to", "enabled an automatic merge", CI/label/description
	// changes, etc.) are also individual_note discussions with no
	// resolvable note, but they're not comments at all: confirmed live, a
	// merge request whose only new "discussions" were three system notes
	// pinged COMMENT with nothing an agent could actually go read — a
	// discussion needs at least one real (non-system) note to count.
	for _, d := range discussions {
		resolvable, unresolved, hasComment := false, false, false
		for _, n := range d.Notes {
			if n.Resolvable {
				resolvable = true
				if !n.Resolved {
					unresolved = true
				}
			} else if !n.System {
				hasComment = true
			}
		}
		if unresolved {
			result.UnresolvedThreadIDs = append(result.UnresolvedThreadIDs, d.ID)
			// The newest few real notes, matching what GitHub fetches per thread.
			var notes []ThreadComment
			for _, n := range d.Notes {
				if !n.System {
					notes = append(notes, ThreadComment{ThreadID: d.ID, ID: fmt.Sprint(n.ID), Author: n.Author.Username, Href: fmt.Sprintf("%s#note_%d", mrURL, n.ID)})
				}
			}
			if len(notes) > 3 {
				notes = notes[len(notes)-3:]
			}
			for _, c := range notes {
				result.ThreadComments = append(result.ThreadComments, c)
				result.ThreadCommentIDs = append(result.ThreadCommentIDs, c.ID)
			}
		}
		if !resolvable && hasComment {
			for _, n := range slices.Backward(d.Notes) {
				if !n.System {
					result.TopLevelComments = append(result.TopLevelComments, TopLevelComment{
						ID:     d.ID,
						Href:   fmt.Sprintf("%s#note_%d", mrURL, n.ID),
						Author: n.Author.Username,
						Body:   n.Body,
					})
					break
				}
			}
		}
	}

	result.ApplyCommentFilters(nil)
	return result, budget, nil
}

// gitlabMRSearchResult is one entry from the merge_requests list endpoint,
// used only by DiscoverBranchMR — the full gitlabMRResponse fields Check
// needs aren't asked for here, since discovery only cares whether a match
// exists at all.
type gitlabMRSearchResult struct {
	WebURL          string `json:"web_url"`
	SourceProjectID int    `json:"source_project_id"`
	TargetProjectID int    `json:"target_project_id"`
}

// DiscoverBranchMR looks up whether host/projectPath has an open MR whose
// source branch is branch — the auto-watch discovery path's GitLab leg.
// GitLab has no aliased-batch query the way GitHub's GraphQL does, so this
// stays one REST call per (host, projectPath, branch) triple, same
// one-call-per-target shape Check already has. found is false — not an
// error — for a project/branch with no open MR. A token that fails to
// resolve for host, or resolves empty, is treated the same way rather than
// as an error: the caller can't tell "not a GitLab host at all" apart from
// "a GitLab host with nothing configured for it" any other way, and both
// mean there's nothing this call can usefully do.
func (c *GitLabChecker) DiscoverBranchMR(ctx context.Context, host, projectPath, branch string) (mrURL string, found bool, budget RateBudget, err error) {
	if c.TokenFunc == nil {
		return "", false, RateBudget{}, nil
	}
	token, tokenErr := c.token(ctx, host)
	if tokenErr != nil || token == "" {
		return "", false, RateBudget{}, nil
	}

	base := c.apiBaseOverride
	if base == "" {
		base = "https://" + host
	}
	q := url.Values{"source_branch": {branch}, "state": {"opened"}, "per_page": {"20"}}
	endpoint := fmt.Sprintf("%s/api/v4/projects/%s/merge_requests?%s", base, url.PathEscape(projectPath), q.Encode())

	var results []gitlabMRSearchResult
	_, budget, err = c.getJSON(ctx, token, endpoint, &results)
	if err != nil {
		return "", false, RateBudget{}, err
	}
	// The project's MR list also holds MRs from other people's forks that
	// happen to use the same branch name; only one whose source is the
	// project itself can be this checkout's.
	for _, mr := range results {
		if mr.SourceProjectID == mr.TargetProjectID {
			return mr.WebURL, true, budget, nil
		}
	}
	return "", false, budget, nil
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

// SelfUsername resolves and caches host's authenticated account's own
// username — used by the scheduler to suppress notifying about a review
// this same account (the human or an agent acting as them, since both
// share one token) just left, which otherwise reads as the worker telling
// you about your own action. A cache miss costs one GET
// /api/v4/user using the same token Check already resolves for host; a
// hit is free. token resolution failing or reporting empty (host not
// configured) fails soft — "" with no error — the same way Check's own
// empty-token short-circuit does, since there's nothing to look up an
// identity against.
func (c *GitLabChecker) SelfUsername(ctx context.Context, host string) (string, error) {
	c.selfUsernameMu.Lock()
	if u, ok := c.selfUsernames[host]; ok {
		c.selfUsernameMu.Unlock()
		return u, nil
	}
	c.selfUsernameMu.Unlock()

	if c.TokenFunc == nil {
		return "", nil
	}
	token, err := c.token(ctx, host)
	if err != nil {
		return "", err
	}
	if token == "" {
		return "", nil
	}

	base := c.apiBaseOverride
	if base == "" {
		base = "https://" + host
	}
	var user struct {
		Username string `json:"username"`
	}
	if _, _, err := c.getJSON(ctx, token, base+"/api/v4/user", &user); err != nil {
		return "", err
	}

	c.selfUsernameMu.Lock()
	if c.selfUsernames == nil {
		c.selfUsernames = make(map[string]string)
	}
	c.selfUsernames[host] = user.Username
	c.selfUsernameMu.Unlock()
	return user.Username, nil
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
		if isUnauthorized(err) {
			c.forgetToken(token)
		}
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
