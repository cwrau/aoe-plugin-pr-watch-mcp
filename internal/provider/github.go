package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	neturl "net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
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

// ParseGitHubRemoteURL resolves a git remote to (owner, repo), accepting
// only an actual github.com remote — ok is false for any other host,
// including a self-hosted GitHub Enterprise instance (not supported here,
// same as ParseGitHubPRURL's hardcoded github.com). Used by the auto-watch
// discovery path to turn a workspace checkout's `origin` remote into the
// owner/repo CheckBatch/DiscoverBranchPRs need, without requiring the
// checkout to already be registered.
func ParseGitHubRemoteURL(remoteURL string) (owner, repo string, ok bool) {
	host, path, ok := ParseGitRemoteURL(remoteURL)
	if !ok || host != "github.com" {
		return "", "", false
	}
	parts := strings.Split(path, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

type GitHubBudgets struct {
	REST    RateBudget
	GraphQL RateBudget
	// GraphQLCost is how many GraphQL rate-limit points the batch's queries
	// spent in total (summed over its requests), as GitHub reports per query.
	// A PR costs several, not one; the scheduler paces by this.
	GraphQLCost int
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
	// Token is the credential in use: set up front when the caller already
	// has one, otherwise filled in from TokenFunc on first use. It is
	// dropped again when GitHub answers 401, so the next request fetches a
	// fresh one; never refetched otherwise. Read it through authToken.
	Token string
	// TokenFunc fetches a credential (typically a `gh auth token`
	// subprocess); nil means Token is all there is.
	TokenFunc func(ctx context.Context) (string, error)
	BaseURL   string // defaults to https://api.github.com; overridable for tests

	tokenMu sync.Mutex

	// selfLogin caches SelfLogin's successful result — the authenticated login
	// never changes for the lifetime of one Token. A failure is never cached:
	// one network blip must not cost the worker its own identity for good.
	// GitHubChecker is shared between the scheduler loop and the MCP HTTP
	// handler's dismiss_review tool (see main.go), so access is locked.
	selfLoginMu sync.Mutex
	selfLogin   string
}

// authToken returns the cached credential, fetching it through TokenFunc
// only when there is none. GitHubChecker is shared between the scheduler loop
// and the MCP handler, hence the lock.
func (c *GitHubChecker) authToken(ctx context.Context) (string, error) {
	c.tokenMu.Lock()
	defer c.tokenMu.Unlock()
	if c.Token != "" {
		return c.Token, nil
	}
	if c.TokenFunc == nil {
		return "", errors.New("github: no token configured")
	}
	token, err := c.TokenFunc(ctx)
	if err != nil {
		return "", fmt.Errorf("resolve github token: %w", err)
	}
	if token == "" {
		return "", errors.New("github: no token available")
	}
	c.Token = token
	return token, nil
}

// forgetToken drops used if it is still the cached credential, after GitHub
// rejected it, so the next request fetches a fresh one. A no-op without a
// TokenFunc: a fixed token has nothing to be replaced by.
func (c *GitHubChecker) forgetToken(used string) {
	if c.TokenFunc == nil {
		return
	}
	c.tokenMu.Lock()
	defer c.tokenMu.Unlock()
	if c.Token == used {
		c.Token = ""
	}
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
      mergedAt
      mergeable
      mergeQueueEntry { state position }
      queueEvents: timelineItems(last: 2, itemTypes: [ADDED_TO_MERGE_QUEUE_EVENT, REMOVED_FROM_MERGE_QUEUE_EVENT]) { nodes { __typename ... on RemovedFromMergeQueueEvent { reason } } }
      isDraft
      reviewRequests(first: 20) { nodes { requestedReviewer { __typename ... on User { login } } } }
      viewerLatestReview { state commit { oid } }
      pushEvents: timelineItems(last: 1, itemTypes: [HEAD_REF_FORCE_PUSHED_EVENT]) { nodes { ... on HeadRefForcePushedEvent { afterCommit { oid } } } }
      reviewDecision
      reviewCount: reviews { totalCount }
      latestReview: reviews(last: 1) { nodes { author { login } state body submittedAt url } }
      reviewThreads(first: 100) { nodes { id isResolved resolvedBy { login } comments(last: 3) { nodes { id url author { login } } } } }
      comments(last: 20) { nodes { id url body author { login } } }
      commits(last: 1) {
        nodes { commit { author { name user { login } } statusCheckRollup { state } } }
      }`

// githubPullRequestFields is the shape of one aliased pullRequest entry in
// a batch response.
type githubPullRequestFields struct {
	State           string `json:"state"`
	Title           string `json:"title"`
	Body            string `json:"body"`
	HeadRefOid      string `json:"headRefOid"`
	MergedAt        string `json:"mergedAt"`
	Mergeable       string `json:"mergeable"` // "MERGEABLE" | "CONFLICTING" | "UNKNOWN" (not yet computed)
	MergeQueueEntry *struct {
		State    string `json:"state"` // "QUEUED" | "AWAITING_CHECKS" | "LOCKED" | "MERGEABLE" | "UNMERGEABLE"
		Position int    `json:"position"`
	} `json:"mergeQueueEntry"` // nil if the PR isn't (or is no longer) in the repo's merge queue
	// QueueEvents are the PR's two latest merge queue timeline events,
	// oldest first. When the newest is a removal, its reason says why the
	// entry left the queue; when it is an add, any earlier removal belongs to
	// a previous stint and says nothing about the current one.
	QueueEvents struct {
		Nodes []struct {
			Typename string `json:"__typename"`
			Reason   string `json:"reason"`
		} `json:"nodes"`
	} `json:"queueEvents"`
	// ReviewDecision is branch protection's verdict (null without any
	// review requirement): REVIEW_REQUIRED next to an approving latest review
	// means more approvals are still needed.
	ReviewDecision string `json:"reviewDecision"`
	ReviewCount    struct {
		TotalCount int `json:"totalCount"`
	} `json:"reviewCount"`
	LatestReview struct {
		Nodes []struct {
			Author struct {
				Login string `json:"login"`
			} `json:"author"`
			State       string `json:"state"`
			Body        string `json:"body"`
			SubmittedAt string `json:"submittedAt"`
			URL         string `json:"url"`
		} `json:"nodes"`
	} `json:"latestReview"`
	// ReviewThreads mirrors GitLab's own scope for Unresolved: inline
	// review-comment threads only, not plain top-level PR conversation
	// comments (GitHub has no "resolvable" concept for those, same as
	// GitLab's discussions API only marks review-type notes resolvable).
	// first: 100 matches the same "good enough, not exhaustively paginated"
	// precedent as reviews(last: 1)/commits(last: 1) above — a PR with more
	// than 100 threads is rare enough not to chase further here.
	ReviewThreads struct {
		Nodes []struct {
			ID         string `json:"id"`
			IsResolved bool   `json:"isResolved"`
			ResolvedBy *struct {
				Login string `json:"login"`
			} `json:"resolvedBy"`
			// Only the newest few comments per thread: enough to spot a
			// reply, without paying GraphQL cost for whole long threads.
			Comments struct {
				Nodes []struct {
					ID     string `json:"id"`
					URL    string `json:"url"`
					Author struct {
						Login string `json:"login"`
					} `json:"author"`
				} `json:"nodes"`
			} `json:"comments"`
		} `json:"nodes"`
	} `json:"reviewThreads"`
	// Comments is the Conversation tab's own top-level comments — distinct
	// from ReviewThreads (inline code comments) and reviews (formal review
	// submissions). GitHub has no "resolvable" concept for these, so unlike
	// ReviewThreads every one seen ever stays "current" (no isResolved to
	// filter on) — last: 20 matches the same "good enough" precedent as
	// reviews(last: 1) above, since only the most recent ones matter for
	// detecting new activity.
	Comments struct {
		Nodes []struct {
			ID     string `json:"id"`
			URL    string `json:"url"`
			Body   string `json:"body"`
			Author struct {
				Login string `json:"login"`
			} `json:"author"`
		} `json:"nodes"`
	} `json:"comments"`
	IsDraft bool `json:"isDraft"`
	// ReviewRequests are the open review requests; a team request has no
	// login (only users are listed, since team membership is not known here).
	ReviewRequests struct {
		Nodes []struct {
			RequestedReviewer struct {
				Typename string `json:"__typename"`
				Login    string `json:"login"`
			} `json:"requestedReviewer"`
		} `json:"nodes"`
	} `json:"reviewRequests"`
	// ViewerLatestReview is the worker's own account's latest review.
	ViewerLatestReview *struct {
		State  string `json:"state"`
		Commit *struct {
			Oid string `json:"oid"`
		} `json:"commit"`
	} `json:"viewerLatestReview"`
	// PushEvents is the newest force-push of the head branch: it is the
	// current head's doing only while its afterCommit is the head itself.
	PushEvents struct {
		Nodes []struct {
			AfterCommit struct {
				Oid string `json:"oid"`
			} `json:"afterCommit"`
		} `json:"nodes"`
	} `json:"pushEvents"`
	Commits struct {
		Nodes []struct {
			Commit struct {
				Author struct {
					Name string `json:"name"`
					User *struct {
						Login string `json:"login"`
					} `json:"user"`
				} `json:"author"`
				StatusCheckRollup *struct {
					State string `json:"state"`
				} `json:"statusCheckRollup"`
			} `json:"commit"`
		} `json:"nodes"`
	} `json:"commits"`
}

// githubBatchGraphQLResponse is the shape of a batched query's response.
// `data` is keyed by every root query field's own alias — each repo alias
// (e.g. "repo0") *and* the fixed "rateLimit" field are siblings directly
// under `data`, since GraphQL nests every root query field there, not just
// the aliased ones. That means "rateLimit"'s raw value isn't shaped like a
// repo's PR-alias map, so Data is left untyped here (decoded per-key by
// rateLimit()/pullRequestRaw() below) rather than given one shape that
// would only fit the repo aliases.
type githubBatchGraphQLResponse struct {
	Data map[string]json.RawMessage `json:"data"`
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

// rateLimit decodes the `rateLimit` field GitHub nests inside `data` (a
// root query field, like every repo alias here — see the type doc above)
// into a RateBudget. Missing or unparseable rate limit data — which
// shouldn't happen, since the query always asks for it — reports the zero
// RateBudget, the same "no signal yet" value ComputeInterval/HasHeadroom
// already handle safely everywhere else.
func (r *githubBatchGraphQLResponse) rateLimit() RateBudget {
	rl, ok := r.rateLimitFields()
	if !ok {
		return RateBudget{}
	}
	return RateBudget{Limit: rl.Limit, Remaining: rl.Remaining, ResetAt: parseRFC3339OrZero(rl.ResetAt)}
}

// queryCost is the points GitHub charged for this query, 0 if not reported.
func (r *githubBatchGraphQLResponse) queryCost() int {
	rl, _ := r.rateLimitFields()
	return rl.Cost
}

type githubRateLimitFields struct {
	Limit     int    `json:"limit"`
	Remaining int    `json:"remaining"`
	ResetAt   string `json:"resetAt"`
	Cost      int    `json:"cost"`
}

func (r *githubBatchGraphQLResponse) rateLimitFields() (githubRateLimitFields, bool) {
	var rl githubRateLimitFields
	if err := json.Unmarshal(r.Data["rateLimit"], &rl); err != nil {
		return githubRateLimitFields{}, false
	}
	return rl, true
}

// pullRequestRaw returns the raw JSON for one aliased pullRequest entry
// (e.g. repoAlias "repo0", prAlias "pr1"), decoding its parent repo
// alias's own object on demand — data[repoAlias] is itself an object
// keyed by that repo's PR aliases, mirroring buildGitHubBatchQuery's
// nesting. A null/missing repository (not found or no access) reports
// false, same as a null/missing pullRequest within it.
func (r *githubBatchGraphQLResponse) pullRequestRaw(repoAlias, prAlias string) (json.RawMessage, bool) {
	repoRaw, ok := r.Data[repoAlias]
	if !ok || len(repoRaw) == 0 || string(repoRaw) == "null" {
		return nil, false
	}
	var repo map[string]json.RawMessage
	if err := json.Unmarshal(repoRaw, &repo); err != nil {
		return nil, false
	}
	raw, ok := repo[prAlias]
	return raw, ok
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
		HasConflict: pr.Mergeable == "CONFLICTING",
		HeadSHA:     pr.HeadRefOid,
		IsDraft:     pr.IsDraft,
	}
	for _, n := range pr.ReviewRequests.Nodes {
		if n.RequestedReviewer.Login != "" {
			result.RequestedReviewers = append(result.RequestedReviewers, n.RequestedReviewer.Login)
		}
	}
	// A dismissed review no longer counts as the account's verdict.
	if v := pr.ViewerLatestReview; v != nil && v.State != "DISMISSED" {
		result.SelfReviewState = v.State
		if v.Commit != nil {
			result.SelfReviewSHA = v.Commit.Oid
		}
	}
	if n := pr.Commits.Nodes; len(n) > 0 {
		author := n[0].Commit.Author
		if author.User != nil && author.User.Login != "" {
			result.HeadAuthor = author.User.Login
		} else {
			result.HeadAuthor = author.Name
		}
	}
	if n := pr.PushEvents.Nodes; len(n) > 0 && pr.HeadRefOid != "" && n[0].AfterCommit.Oid == pr.HeadRefOid {
		result.ForcePushed = true
	}
	if pr.MergeQueueEntry != nil {
		result.MergeQueueState = pr.MergeQueueEntry.State
		result.MergeQueuePosition = pr.MergeQueueEntry.Position
	}
	if n := pr.QueueEvents.Nodes; len(n) > 0 && n[len(n)-1].Typename == "RemovedFromMergeQueueEvent" {
		result.MergeQueueRemovalReason = n[len(n)-1].Reason
	}
	for _, t := range pr.ReviewThreads.Nodes {
		if t.IsResolved && t.ResolvedBy != nil && t.ResolvedBy.Login != "" {
			if result.ResolvedBy == nil {
				result.ResolvedBy = make(map[string]string)
			}
			result.ResolvedBy[t.ID] = t.ResolvedBy.Login
		}
		if !t.IsResolved {
			result.UnresolvedThreadIDs = append(result.UnresolvedThreadIDs, t.ID)
			for _, c := range t.Comments.Nodes {
				result.ThreadComments = append(result.ThreadComments, ThreadComment{ThreadID: t.ID, ID: c.ID, Author: c.Author.Login, Href: c.URL})
				result.ThreadCommentIDs = append(result.ThreadCommentIDs, c.ID)
			}
		}
	}
	for _, c := range pr.Comments.Nodes {
		// comments(last: N) is chronological ascending, so the final node
		// is the newest — ApplyCommentFilters relies on that order.
		result.TopLevelComments = append(result.TopLevelComments, TopLevelComment{ID: c.ID, Href: c.URL, Author: c.Author.Login, Body: c.Body})
	}
	result.ApplyCommentFilters(nil)
	// A DISMISSED review carries no current signal — GitHub updates the
	// review's own `state` in place when it's dismissed, rather than
	// removing it from the list, but a dismissed review no longer counts
	// toward anything (it doesn't block merge, doesn't reflect anyone's
	// current opinion). Treating it as if this PR had no review at all,
	// rather than surfacing "DISMISSED" as if it were a real state, is both
	// more honest and reuses every "no review yet" code path for free.
	if len(pr.LatestReview.Nodes) > 0 && pr.LatestReview.Nodes[0].State != "DISMISSED" {
		result.LatestReviewer = pr.LatestReview.Nodes[0].Author.Login
		result.LatestReviewState = pr.LatestReview.Nodes[0].State
		result.LatestReviewAt = parseRFC3339OrZero(pr.LatestReview.Nodes[0].SubmittedAt)
		result.ReviewHref = pr.LatestReview.Nodes[0].URL
		result.LatestReviewBody = pr.LatestReview.Nodes[0].Body
		if result.LatestReviewState == "APPROVED" && pr.ReviewDecision == "REVIEW_REQUIRED" {
			result.LatestReviewState = "APPROVED (more needed)"
		}
	}
	if pr.State == "MERGED" {
		result.MergedAt = parseRFC3339OrZero(pr.MergedAt)
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

	query := fmt.Sprintf("query(%s) {\n%s  rateLimit { limit remaining resetAt cost }\n}", strings.Join(varDecls, ", "), body.String())
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

// githubBatchChunkSize is how many PRs one GraphQL request asks for. GitHub
// rejects a query that is too big with "Resource limits for this query
// exceeded", and every PR here pulls in reviews, threads with comments,
// comments, queue events and checks, so a single query for everything watched
// stopped working once there were around a hundred registrations — and then
// no PR was checked at all.
const githubBatchChunkSize = 20

// CheckBatch resolves every url's PR state in GraphQL requests of at most
// githubBatchChunkSize PRs each (see buildGitHubBatchQuery), then runs the
// action_required REST cross-check per still-open result. A url missing from
// the returned map means its PR (or repo) wasn't found in the response, or
// its request failed — the caller treats that as a per-registration failure,
// same as any other check error, without failing the rest. Only when every
// request fails (network error, non-2xx, top-level GraphQL errors) is the
// error returned: there is no per-PR data to fall back to in that case.
func (c *GitHubChecker) CheckBatch(ctx context.Context, urls []string) (map[string]CheckResult, GitHubBudgets, error) {
	// A malformed URL is the caller's bug, not something a smaller query
	// would fix, so it fails the call up front.
	for _, u := range urls {
		if _, _, _, err := ParseGitHubPRURL(u); err != nil {
			return nil, GitHubBudgets{}, err
		}
	}
	if len(urls) <= githubBatchChunkSize {
		return c.checkBatchChunk(ctx, urls)
	}
	results := make(map[string]CheckResult, len(urls))
	var budgets GitHubBudgets
	var firstErr error
	succeeded := 0
	for start := 0; start < len(urls); start += githubBatchChunkSize {
		chunk := urls[start:min(start+githubBatchChunkSize, len(urls))]
		res, b, err := c.checkBatchChunk(ctx, chunk)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		succeeded++
		maps.Copy(results, res)
		budgets.GraphQLCost += b.GraphQLCost
		if b.GraphQL.Limit > 0 {
			budgets.GraphQL = b.GraphQL
		}
		if b.REST.Limit > 0 {
			budgets.REST = b.REST
		}
	}
	if succeeded == 0 {
		return nil, GitHubBudgets{}, firstErr
	}
	return results, budgets, nil
}

// checkBatchChunk is one GraphQL request's worth of CheckBatch.
func (c *GitHubChecker) checkBatchChunk(ctx context.Context, urls []string) (map[string]CheckResult, GitHubBudgets, error) {
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
		raw, ok := resp.pullRequestRaw(repoAlias, prAlias)
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

	budgets := GitHubBudgets{GraphQL: resp.rateLimit(), GraphQLCost: resp.queryCost()}

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
			budget, runs, err := c.checkActionRequired(ctx, entry.owner, entry.repo, entry.headRefOid)
			if err != nil {
				return nil, GitHubBudgets{}, err
			}
			restBudget = budget
			if runs.actionRequired && result.CIBucket != "failure" {
				result.CIBucket = "action_required"
			}
			// Confirmed live: agent-of-empires/agent-of-empires#4138 pinged
			// CI failure with an already-empty CIDetail/CIHref (the earlier
			// fix already refused to blame a cancelled run for those), but
			// the top-level "failure" bucket itself still came straight
			// from the rollup and still fired the ping — GitHub's
			// statusCheckRollup reports FAILURE for a commit whose only
			// non-success check was a force-push-cancelled run, same root
			// cause one level up. Downgrading to "pending" here (not
			// "success" — a cancelled run says nothing about whether the
			// commit's checks would have passed) only fires when
			// explainedByCancellation has already ruled out a genuine
			// failure, a pending run, or an action_required gate.
			if result.CIBucket == "failure" && runs.explainedByCancellation {
				result.CIBucket = "pending"
			}
			switch result.CIBucket {
			case "failure":
				result.CIDetail = runs.failingRunName
				result.CIHref = runs.failingRunHref
			case "action_required":
				result.CIDetail = runs.actionRequiredName
				result.CIHref = runs.actionRequiredHref
			}
		}
		results[url] = result
	}
	budgets.REST = restBudget

	return results, budgets, nil
}

// GitHubBranchQuery is one "does this branch have an open PR" question for
// DiscoverBranchPRs.
type GitHubBranchQuery struct {
	Owner  string
	Repo   string
	Branch string
	// HeadOwners, when set, restricts the match to a PR whose head repo is
	// owned by one of these logins (compared case-insensitively). Needed when
	// probing a repo the checkout does not push to, where an unrelated PR may
	// share the branch name.
	HeadOwners []string
}

// GitHubBranchKey is the map key DiscoverBranchPRs' result uses for one
// query — exported so a caller can look a query back up in the result
// without needing to reconstruct string formatting itself.
func GitHubBranchKey(owner, repo, branch string) string {
	return owner + "/" + repo + "@" + branch
}

// buildGitHubBranchDiscoveryQuery mirrors buildGitHubBatchQuery's repo-alias
// grouping (a due branch lookup on a repo another due lookup or check
// already touches doesn't cost a second `repository(...)` resolution), but
// asks each aliased repo for `pullRequests(headRefName: ..., states: [OPEN])`
// instead of a specific `pullRequest(number: ...)` — the query shape a
// branch lookup needs is different enough from a known-PR-number lookup
// that keeping them as separate builders (like FindReview/DismissReview are
// already separate from CheckBatch) is clearer than parameterizing one
// builder for both.
func buildGitHubBranchDiscoveryQuery(items []GitHubBranchQuery) githubBatchQuery {
	type repoGroup struct {
		alias string
		owner string
		repo  string
		items []GitHubBranchQuery
	}
	var groups []*repoGroup
	byRepo := make(map[string]*repoGroup)
	for _, it := range items {
		key := it.Owner + "/" + it.Repo
		g, ok := byRepo[key]
		if !ok {
			g = &repoGroup{alias: fmt.Sprintf("repo%d", len(groups)), owner: it.Owner, repo: it.Repo}
			groups = append(groups, g)
			byRepo[key] = g
		}
		g.items = append(g.items, it)
	}

	var varDecls []string
	variables := make(map[string]any)
	keyByAlias := make(map[string]string, len(items))
	var body strings.Builder
	for _, g := range groups {
		ownerVar, repoVar := g.alias+"Owner", g.alias+"Repo"
		varDecls = append(varDecls, "$"+ownerVar+": String!", "$"+repoVar+": String!")
		variables[ownerVar] = g.owner
		variables[repoVar] = g.repo
		fmt.Fprintf(&body, "  %s: repository(owner: $%s, name: $%s) {\n", g.alias, ownerVar, repoVar)
		for i, it := range g.items {
			branchAlias := fmt.Sprintf("branch%d", i)
			branchVar := g.alias + branchAlias + "Ref"
			varDecls = append(varDecls, "$"+branchVar+": String!")
			variables[branchVar] = it.Branch
			keyByAlias[g.alias+"."+branchAlias] = GitHubBranchKey(it.Owner, it.Repo, it.Branch)
			fmt.Fprintf(&body, "    %s: pullRequests(headRefName: $%s, states: [OPEN], first: 10) { nodes { url headRepositoryOwner { login } } }\n", branchAlias, branchVar)
		}
		body.WriteString("  }\n")
	}

	query := fmt.Sprintf("query(%s) {\n%s  rateLimit { limit remaining resetAt }\n}", strings.Join(varDecls, ", "), body.String())
	return githubBatchQuery{query: query, variables: variables, urlByAlias: keyByAlias}
}

// DiscoverBranchPRs resolves, for each query, whether an open PR already
// exists whose head is that branch — the auto-watch discovery path's only
// real API call: one aliased GraphQL request covering every due branch
// across however many distinct repos, same dedup-by-repo shape CheckBatch
// already uses. The result map only contains an entry for a query that
// actually found an open PR (keyed by GitHubBranchKey); a query with no
// match is simply absent, not a zero-value entry — this mirrors CheckBatch's
// own "missing means not found" contract for a batch response.
func (c *GitHubChecker) DiscoverBranchPRs(ctx context.Context, items []GitHubBranchQuery) (map[string]string, RateBudget, error) {
	if len(items) == 0 {
		return map[string]string{}, RateBudget{}, nil
	}
	bq := buildGitHubBranchDiscoveryQuery(items)
	resp, err := c.queryBatchGraphQL(ctx, bq)
	if err != nil {
		return nil, RateBudget{}, err
	}

	ownersByKey := make(map[string][]string, len(items))
	for _, it := range items {
		ownersByKey[GitHubBranchKey(it.Owner, it.Repo, it.Branch)] = it.HeadOwners
	}
	results := make(map[string]string, len(items))
	for alias, key := range bq.urlByAlias {
		repoAlias, branchAlias, ok := strings.Cut(alias, ".")
		if !ok {
			continue
		}
		raw, ok := resp.pullRequestRaw(repoAlias, branchAlias)
		if !ok || len(raw) == 0 || string(raw) == "null" {
			continue
		}
		var nodes struct {
			Nodes []struct {
				URL                 string `json:"url"`
				HeadRepositoryOwner *struct {
					Login string `json:"login"`
				} `json:"headRepositoryOwner"`
			} `json:"nodes"`
		}
		if err := json.Unmarshal(raw, &nodes); err != nil {
			continue
		}
		owners := ownersByKey[key]
		for _, n := range nodes.Nodes {
			if len(owners) == 0 || (n.HeadRepositoryOwner != nil && slices.ContainsFunc(owners, func(o string) bool { return strings.EqualFold(o, n.HeadRepositoryOwner.Login) })) {
				results[key] = n.URL
				break
			}
		}
	}
	return results, resp.rateLimit(), nil
}

// ReviewLookup is one review resolved by FindReview, carrying just what
// DismissReview/RequestReviewer need to act on it.
type ReviewLookup struct {
	PullRequestID string
	ReviewID      string
	State         string
	AuthorLogin   string
}

const githubFindReviewQuery = `query($owner: String!, $repo: String!, $number: Int!) {
  repository(owner: $owner, name: $repo) {
    pullRequest(number: $number) {
      id
      reviews(first: 100) {
        nodes { id url state author { login } }
      }
    }
  }
}`

type githubFindReviewResponse struct {
	Repository *struct {
		PullRequest *struct {
			ID      string `json:"id"`
			Reviews struct {
				Nodes []struct {
					ID     string `json:"id"`
					URL    string `json:"url"`
					State  string `json:"state"`
					Author struct {
						Login string `json:"login"`
					} `json:"author"`
				} `json:"nodes"`
			} `json:"reviews"`
		} `json:"pullRequest"`
	} `json:"repository"`
}

// FindReview resolves reviewURL to its GraphQL review id and current state,
// by fetching every review on owner/repo#number (capped at 100, same
// "good enough, not exhaustively paginated" precedent as reviewThreads/
// latestReview elsewhere in this file) and matching by url — the only
// review identifier CheckResult exposes as ReviewHref, so it's what
// dismiss_review's caller already has in hand from a badge tooltip or the
// list tool. found is false if no review's url matches (already resolved/
// deleted, or a wrong url), distinct from a real request error.
func (c *GitHubChecker) FindReview(ctx context.Context, owner, repo string, number int, reviewURL string) (ReviewLookup, bool, error) {
	var resp githubFindReviewResponse
	if err := c.doGraphQL(ctx, githubFindReviewQuery, map[string]any{
		"owner": owner, "repo": repo, "number": number,
	}, &resp); err != nil {
		return ReviewLookup{}, false, err
	}
	if resp.Repository == nil || resp.Repository.PullRequest == nil {
		return ReviewLookup{}, false, fmt.Errorf("pull request not found: %s/%s#%d", owner, repo, number)
	}
	pr := resp.Repository.PullRequest
	for _, n := range pr.Reviews.Nodes {
		if n.URL == reviewURL {
			return ReviewLookup{
				PullRequestID: pr.ID,
				ReviewID:      n.ID,
				State:         n.State,
				AuthorLogin:   n.Author.Login,
			}, true, nil
		}
	}
	return ReviewLookup{}, false, nil
}

const githubDismissReviewMutation = `mutation($reviewId: ID!, $message: String!) {
  dismissPullRequestReview(input: {pullRequestReviewId: $reviewId, message: $message}) {
    clientMutationId
  }
}`

// DismissReview dismisses reviewID via GitHub's dismissPullRequestReview
// mutation. This requires write access to the repo (and, on a branch that
// restricts review dismissal, admin/allowlisted access) and only accepts
// APPROVED/CHANGES_REQUESTED reviews — the caller falls back to
// RequestReviewer, then a local-only marker, when this fails.
func (c *GitHubChecker) DismissReview(ctx context.Context, reviewID string) error {
	return c.doGraphQL(ctx, githubDismissReviewMutation, map[string]any{
		"reviewId": reviewID,
		"message":  "Dismissed via pr-watch",
	}, nil)
}

const githubUserIDQuery = `query($login: String!) { user(login: $login) { id } }`

func (c *GitHubChecker) userID(ctx context.Context, login string) (string, error) {
	var resp struct {
		User *struct {
			ID string `json:"id"`
		} `json:"user"`
	}
	if err := c.doGraphQL(ctx, githubUserIDQuery, map[string]any{"login": login}, &resp); err != nil {
		return "", err
	}
	if resp.User == nil {
		return "", fmt.Errorf("github user not found: %s", login)
	}
	return resp.User.ID, nil
}

const githubRequestReviewsMutation = `mutation($prId: ID!, $userId: ID!) {
  requestReviews(input: {pullRequestId: $prId, userIds: [$userId]}) {
    clientMutationId
  }
}`

// RequestReviewer re-requests a review from login on pullRequestID via
// GitHub's requestReviews mutation — the middle fallback tier when a real
// dismiss isn't possible, since it at least turns a red "changes requested"
// into a pending re-review on GitHub's own PR page. It does not change the
// original review's own state, so callers must still apply a local
// suppression marker afterward or it resurfaces on the next poll.
func (c *GitHubChecker) RequestReviewer(ctx context.Context, pullRequestID, login string) error {
	userID, err := c.userID(ctx, login)
	if err != nil {
		return err
	}
	return c.doGraphQL(ctx, githubRequestReviewsMutation, map[string]any{
		"prId":   pullRequestID,
		"userId": userID,
	}, nil)
}

const githubViewerLoginQuery = `query { viewer { login } }`

// SelfLogin resolves and caches the authenticated account's own login —
// used by the scheduler to suppress notifying about a REVIEW this same
// account (the human or an agent acting as them, since both share one
// token) just left, which otherwise reads as the worker telling you about
// your own action. Resolved once per GitHubChecker (a fresh GraphQL call
// the first time, a cached value every call after — the login never
// changes for the lifetime of one Token), not once per poll.
func (c *GitHubChecker) SelfLogin(ctx context.Context) (string, error) {
	c.selfLoginMu.Lock()
	defer c.selfLoginMu.Unlock()
	if c.selfLogin != "" {
		return c.selfLogin, nil
	}
	var resp struct {
		Viewer struct {
			Login string `json:"login"`
		} `json:"viewer"`
	}
	if err := c.doGraphQL(ctx, githubViewerLoginQuery, nil, &resp); err != nil {
		return "", err
	}
	c.selfLogin = resp.Viewer.Login
	return c.selfLogin, nil
}

// doGraphQL posts one query or mutation and decodes its `data` field into
// data (skipped if nil, for mutations the caller only needs success/failure
// from). Shared by FindReview/DismissReview/RequestReviewer/userID rather
// than queryBatchGraphQL, which is tied to the batch endpoint's specific
// per-alias response shape.
func (c *GitHubChecker) doGraphQL(ctx context.Context, query string, variables map[string]any, data any) error {
	payload, err := json.Marshal(map[string]any{
		"query":     query,
		"variables": variables,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL()+"/graphql", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	token, err := c.authToken(ctx)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("github graphql request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if err := checkHTTPStatus(resp); err != nil {
		if isUnauthorized(err) {
			c.forgetToken(token)
		}
		return fmt.Errorf("github graphql: %w", err)
	}

	var out struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return fmt.Errorf("decode github graphql response: %w", err)
	}
	if len(out.Errors) > 0 {
		messages := make([]string, 0, len(out.Errors))
		for _, e := range out.Errors {
			messages = append(messages, e.Message)
		}
		return fmt.Errorf("github graphql returned %d error(s): %s", len(out.Errors), strings.Join(messages, "; "))
	}
	if data != nil && len(out.Data) > 0 {
		if err := json.Unmarshal(out.Data, data); err != nil {
			return fmt.Errorf("decode github graphql data: %w", err)
		}
	}
	return nil
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
	token, err := c.authToken(ctx)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("github graphql batch request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if err := checkHTTPStatus(resp); err != nil {
		if isUnauthorized(err) {
			c.forgetToken(token)
		}
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
		Name       string `json:"name"`
		Status     string `json:"status"` // "queued" | "in_progress" | "completed" — see explainedByCancellation's use of it
		Conclusion string `json:"conclusion"`
		HTMLURL    string `json:"html_url"`
	} `json:"workflow_runs"`
}

// actionsRunsResult is what checkActionRequired learns from one PR's
// Actions runs page: whether a run is stuck on the fork-PR
// maintainer-approval gate, plus (best-effort) the name and link of the
// first run of each of the two kinds CheckBatch might report — a genuine
// failure and an action_required gate — for CIDetail/CIHref. These are kept
// separate rather than merged into one "first non-succeeding run" search:
// CIBucket can be "failure" from the GraphQL statusCheckRollup (a signal
// this REST call never touches) while the only non-succeeding run this page
// actually has is action_required — conflating the two used to point
// CIHref at an "awaiting approval" run while the badge said "failure"
// (confirmed live: github.com/agent-of-empires/agent-of-empires/pull/4083).
// Picking the run whose conclusion actually matches the reported bucket
// (see CheckBatch) avoids that; if this page has no run of the matching
// kind, CIDetail/CIHref just stay empty rather than naming the wrong one.
// The *Href fields link to the run itself (a "pipeline"), not the specific
// job within it — narrowing further would need one more REST call per
// checked PR (GET .../actions/runs/{id}/jobs), not made here.
type actionsRunsResult struct {
	actionRequired     bool
	actionRequiredName string
	actionRequiredHref string
	failingRunName     string
	failingRunHref     string
	// explainedByCancellation is true when this SHA's Actions runs fully
	// account for a "failure" rollup as nothing but noise: at least one run
	// was cancelled, none has a genuine failure or action_required
	// conclusion, and none is still queued/in_progress (a run still running
	// might yet fail, so it's never safe to explain away until every run is
	// terminal). See CheckBatch's use of this for why "cancelled" alone is
	// required rather than "every run succeeded" — a rollup FAILURE with
	// zero cancelled runs at all means something outside Actions (a
	// different status-check provider) is the real cause, which this REST
	// call can't see and must never paper over.
	explainedByCancellation bool
}

// trueFailureConclusions are workflow_runs[].conclusion values that mean
// the run itself actually failed — distinct from action_required (gated,
// not failed) and from "skipped"/"neutral" (neither indicates a problem
// worth surfacing as the reason CI looks red). "cancelled" is deliberately
// excluded: it means the run was superseded or aborted (most commonly a
// `concurrency: cancel-in-progress` workflow cancelling the previous run
// the moment a new commit lands), not that anything in it actually broke —
// confirmed live: a force-push mid-run cancelled a CodeQL run, and it got
// cited as "the failing job" in a CI-failure ping even though nothing had
// failed, sending the reader to a log that only ever said "cancelled".
var trueFailureConclusions = map[string]bool{
	"failure":         true,
	"timed_out":       true,
	"startup_failure": true,
}

func (c *GitHubChecker) checkActionRequired(ctx context.Context, owner, repo, headSHA string) (RateBudget, actionsRunsResult, error) {
	if headSHA == "" {
		return RateBudget{}, actionsRunsResult{}, nil
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
		return RateBudget{}, actionsRunsResult{}, err
	}
	token, err := c.authToken(ctx)
	if err != nil {
		return RateBudget{}, actionsRunsResult{}, err
	}
	req.Header.Set("Authorization", "bearer "+token)

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return RateBudget{}, actionsRunsResult{}, fmt.Errorf("github actions runs request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	budget := parseGitHubRESTRateHeaders(resp)

	if err := checkHTTPStatus(resp); err != nil {
		if isUnauthorized(err) {
			c.forgetToken(token)
		}
		return budget, actionsRunsResult{}, fmt.Errorf("github actions runs: %w", err)
	}

	var out githubActionsRunsResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return budget, actionsRunsResult{}, fmt.Errorf("decode github actions runs response: %w", err)
	}
	var result actionsRunsResult
	var anyCancelled, anyNonTerminal bool
	for _, run := range out.WorkflowRuns {
		if run.Conclusion == "action_required" {
			result.actionRequired = true
			if result.actionRequiredName == "" {
				result.actionRequiredName = run.Name
				result.actionRequiredHref = run.HTMLURL
			}
		}
		if result.failingRunName == "" && trueFailureConclusions[run.Conclusion] {
			result.failingRunName = run.Name
			result.failingRunHref = run.HTMLURL
		}
		if run.Conclusion == "cancelled" {
			anyCancelled = true
		}
		if run.Status != "completed" {
			anyNonTerminal = true
		}
	}
	result.explainedByCancellation = len(out.WorkflowRuns) > 0 && anyCancelled && !anyNonTerminal && !result.actionRequired && result.failingRunName == ""
	return budget, result, nil
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
	return &httpStatusError{Code: resp.StatusCode, Body: strings.TrimSpace(string(body))}
}

// httpStatusError is a non-2xx response, typed so callers can tell a
// rejected credential (401) from other failures.
type httpStatusError struct {
	Code int
	Body string
}

func (e *httpStatusError) Error() string {
	return fmt.Sprintf("unexpected status %d: %s", e.Code, e.Body)
}

func isUnauthorized(err error) bool {
	e, ok := errors.AsType[*httpStatusError](err)
	return ok && e.Code == http.StatusUnauthorized
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
