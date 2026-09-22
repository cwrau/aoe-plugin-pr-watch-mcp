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

// GitHubChecker polls a batch of PRs' state via one aliased GitHub GraphQL
// request, then cross-checks the Actions runs REST API per still-open PR
// for its head SHA to catch action_required workflow runs (stuck on a
// fork-PR maintainer-approval gate) that never produce a check-run and are
// therefore invisible to any statusCheckRollup shape. The REST leg stays
// one call per PR (not deduped by repo): head_sha-filtered is precise and
// always small regardless of a repo's overall CI volume, whereas an
// unfiltered per-repo fetch risks a busy repo's unrelated runs pushing a
// given PR's own runs off the one page fetched, silently missing the one
// CI signal that never resolves on its own. Both calls' rate-limit signal
// is read off the response itself — GraphQL via the `rateLimit` field in
// the body, REST via the X-RateLimit-* response headers — never via a
// dedicated polling call.
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

// githubPullRequestFieldsQuery is the field selection applied to every
// aliased `pullRequest(...)` in a batch query — identical to what a
// single-PR query would ask for, just repeated under each alias.
const githubPullRequestFieldsQuery = `      state
      title
      body
      headRefOid
      reviewCount: reviews { totalCount }
      latestReview: reviews(last: 1) { nodes { author { login } state } }
      commits(last: 1) {
        nodes { commit { statusCheckRollup { state } } }
      }`

// githubPullRequestFields is the shape of one aliased pullRequest entry in
// a batch response.
type githubPullRequestFields struct {
	State       string `json:"state"`
	Title       string `json:"title"`
	Body        string `json:"body"`
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
}

// githubBatchGraphQLResponse is the shape of a batched query's response:
// `data` is keyed by repo alias, each value keyed by that repo's PR
// aliases — exactly mirroring the aliasing buildGitHubBatchQuery
// generates, so `Data["repo0"]["pr1"]` is that repo's second PR's raw
// field JSON, decoded lazily since a null repository or pullRequest (not
// found / no access) must be told apart from a real payload.
type githubBatchGraphQLResponse struct {
	Data      map[string]map[string]json.RawMessage `json:"data"`
	RateLimit struct {
		Limit     int    `json:"limit"`
		Remaining int    `json:"remaining"`
		ResetAt   string `json:"resetAt"`
	} `json:"rateLimit"`
	// Errors carries GraphQL-level failures (rate limiting, NOT_FOUND,
	// field errors). GitHub returns these with HTTP 200, so checkHTTPStatus
	// never sees them. Any top-level error fails the whole batch rather
	// than trying to attribute it to one alias via the error's `path` —
	// the caller backs off every registration in the batch the same way a
	// single failed check always has.
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

// buildCheckResult maps one PR's raw GraphQL fields to a CheckResult. Does
// not set CIBucket to "action_required" — that's layered on by the
// caller, which alone knows whether the per-PR REST cross-check found one.
func buildCheckResult(pr githubPullRequestFields) CheckResult {
	result := CheckResult{
		State:       mapGitHubState(pr.State),
		Title:       pr.Title,
		Description: pr.Body,
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
	return result
}

type githubBatchItem struct {
	url    string
	owner  string
	repo   string
	number int
}

// githubBatchQuery is a built batch request: the query text, its
// variables, and a lookup from "<repoAlias>.<prAlias>" back to the
// original PR URL, so the response (aliased the same way) can be mapped
// back to what the caller asked for.
type githubBatchQuery struct {
	query      string
	variables  map[string]any
	urlByAlias map[string]string
}

// buildGitHubBatchQuery groups items by (owner, repo), aliasing each
// distinct repo once and nesting one aliased pullRequest per item inside
// it — so two due PRs sharing a repo resolve `repository(owner,name)`
// once, not twice.
func buildGitHubBatchQuery(items []githubBatchItem) githubBatchQuery {
	type repoGroup struct {
		alias string
		owner string
		repo  string
		items []githubBatchItem
	}
	var groups []*repoGroup
	byRepo := make(map[string]*repoGroup)
	for _, it := range items {
		key := it.owner + "/" + it.repo
		g, ok := byRepo[key]
		if !ok {
			g = &repoGroup{alias: fmt.Sprintf("repo%d", len(groups)), owner: it.owner, repo: it.repo}
			groups = append(groups, g)
			byRepo[key] = g
		}
		g.items = append(g.items, it)
	}

	var varDecls []string
	variables := make(map[string]any)
	urlByAlias := make(map[string]string, len(items))
	var body strings.Builder
	for _, g := range groups {
		ownerVar, repoVar := g.alias+"Owner", g.alias+"Repo"
		varDecls = append(varDecls, "$"+ownerVar+": String!", "$"+repoVar+": String!")
		variables[ownerVar] = g.owner
		variables[repoVar] = g.repo
		fmt.Fprintf(&body, "  %s: repository(owner: $%s, name: $%s) {\n", g.alias, ownerVar, repoVar)
		for i, it := range g.items {
			prAlias := fmt.Sprintf("pr%d", i)
			numVar := g.alias + prAlias + "Number"
			varDecls = append(varDecls, "$"+numVar+": Int!")
			variables[numVar] = it.number
			urlByAlias[g.alias+"."+prAlias] = it.url
			fmt.Fprintf(&body, "    %s: pullRequest(number: $%s) {\n%s\n    }\n", prAlias, numVar, githubPullRequestFieldsQuery)
		}
		body.WriteString("  }\n")
	}

	query := fmt.Sprintf("query(%s) {\n%s  rateLimit { limit remaining resetAt }\n}", strings.Join(varDecls, ", "), body.String())
	return githubBatchQuery{query: query, variables: variables, urlByAlias: urlByAlias}
}

// githubBatchEntry is one successfully-resolved PR from a batch response,
// carrying the fields buildCheckResult doesn't expose on CheckResult but
// that the action_required REST cross-check needs (headRefOid, and
// owner/repo since the URL alone isn't re-parsed).
type githubBatchEntry struct {
	result     CheckResult
	headRefOid string
	owner      string
	repo       string
}

// CheckBatch resolves every url's PR state in one GraphQL request (see
// buildGitHubBatchQuery), then runs the action_required REST cross-check
// per still-open result. A url missing from the returned map means its PR
// (or repo) wasn't found in the response — the caller treats that as a
// per-registration failure, same as any other check error, without
// failing the rest of the batch. A whole-request failure (network error,
// non-2xx, top-level GraphQL errors) fails the entire batch instead: there
// is no per-PR data to fall back to in that case.
func (c *GitHubChecker) CheckBatch(ctx context.Context, urls []string) (map[string]CheckResult, GitHubBudgets, error) {
	items := make([]githubBatchItem, 0, len(urls))
	itemByURL := make(map[string]githubBatchItem, len(urls))
	for _, u := range urls {
		owner, repo, number, err := ParseGitHubPRURL(u)
		if err != nil {
			return nil, GitHubBudgets{}, err
		}
		it := githubBatchItem{url: u, owner: owner, repo: repo, number: number}
		items = append(items, it)
		itemByURL[u] = it
	}
	if len(items) == 0 {
		return map[string]CheckResult{}, GitHubBudgets{}, nil
	}

	bq := buildGitHubBatchQuery(items)
	resp, err := c.queryBatchGraphQL(ctx, bq)
	if err != nil {
		return nil, GitHubBudgets{}, err
	}

	entries := make(map[string]githubBatchEntry, len(items))
	for alias, url := range bq.urlByAlias {
		repoAlias, prAlias, ok := strings.Cut(alias, ".")
		if !ok {
			continue
		}
		raw, ok := resp.Data[repoAlias][prAlias]
		// A null repository or pullRequest (not found / no access) decodes
		// as a missing map entry or a literal JSON "null" here — either
		// way there is nothing to report for this one URL, same case
		// buildCheckResult's caller used to reject via pr.State == "".
		if !ok || len(raw) == 0 || string(raw) == "null" {
			continue
		}
		var pr githubPullRequestFields
		if err := json.Unmarshal(raw, &pr); err != nil || pr.State == "" {
			continue
		}
		it := itemByURL[url]
		entries[url] = githubBatchEntry{
			result:     buildCheckResult(pr),
			headRefOid: pr.HeadRefOid,
			owner:      it.owner,
			repo:       it.repo,
		}
	}

	budgets := GitHubBudgets{
		GraphQL: RateBudget{
			Limit:     resp.RateLimit.Limit,
			Remaining: resp.RateLimit.Remaining,
			ResetAt:   parseRFC3339OrZero(resp.RateLimit.ResetAt),
		},
	}

	// Terminal PRs don't need the action_required cross-check — same
	// early-out the single-PR path always had. The REST budget reported
	// back is whichever call happened last; every registration in this
	// batch already shares one GraphQL budget reading, so a slightly
	// stale REST reading for some of them is the same trade-off the
	// single-PR path made implicitly (it only ever reported its own one
	// PR's REST budget too).
	results := make(map[string]CheckResult, len(entries))
	var restBudget RateBudget
	for url, entry := range entries {
		result := entry.result
		if result.State != "merged" && result.State != "closed" {
			budget, actionRequired, err := c.checkActionRequired(ctx, entry.owner, entry.repo, entry.headRefOid)
			if err != nil {
				return nil, GitHubBudgets{}, err
			}
			restBudget = budget
			if actionRequired && result.CIBucket != "failure" {
				result.CIBucket = "action_required"
			}
		}
		results[url] = result
	}
	budgets.REST = restBudget

	return results, budgets, nil
}

func (c *GitHubChecker) queryBatchGraphQL(ctx context.Context, bq githubBatchQuery) (*githubBatchGraphQLResponse, error) {
	payload, err := json.Marshal(map[string]any{
		"query":     bq.query,
		"variables": bq.variables,
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
		return nil, fmt.Errorf("github graphql batch request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if err := checkHTTPStatus(resp); err != nil {
		return nil, fmt.Errorf("github graphql batch: %w", err)
	}

	var out githubBatchGraphQLResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode github graphql batch response: %w", err)
	}
	if len(out.Errors) > 0 {
		messages := make([]string, 0, len(out.Errors))
		for _, e := range out.Errors {
			messages = append(messages, e.Message)
		}
		return nil, fmt.Errorf("github graphql batch returned %d error(s): %s", len(out.Errors), strings.Join(messages, "; "))
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
	defer func() { _ = resp.Body.Close() }()

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
