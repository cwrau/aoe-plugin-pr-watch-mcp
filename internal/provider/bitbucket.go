package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// bitbucketAPIBase is Bitbucket Cloud's REST API root. The credential is only
// ever sent here: a registered URL on any other host is not a Bitbucket Cloud
// PR (ParseBitbucketPRURL refuses it), so no URL a registration names can make
// the worker hand its token to a server of someone else's choosing.
const bitbucketAPIBase = "https://api.bitbucket.org/2.0"

// bitbucketMaxPages bounds how many pages of one list (comments, statuses) a
// single check follows.
const bitbucketMaxPages = 5

var bitbucketPRURLPattern = regexp.MustCompile(`^https?://bitbucket\.org/([^/]+)/([^/]+)/pull-requests/(\d+)`)

// ParseBitbucketPRURL extracts workspace, repository slug and pull request id
// from a Bitbucket Cloud PR URL (bitbucket.org/<workspace>/<repo>/pull-requests/<id>).
func ParseBitbucketPRURL(prURL string) (workspace, repo string, id int, err error) {
	m := bitbucketPRURLPattern.FindStringSubmatch(prURL)
	if m == nil {
		return "", "", 0, fmt.Errorf("not a bitbucket cloud pull request url: %s", prURL)
	}
	n, err := strconv.Atoi(m[3])
	if err != nil {
		return "", "", 0, fmt.Errorf("invalid pull request id in %s: %w", prURL, err)
	}
	return m[1], m[2], n, nil
}

// BitbucketChecker polls one Bitbucket Cloud pull request over REST. Unlike
// GitHub and GitLab there is no CLI to ask for a token: Credentials returns the
// configured one on every call, so a change in the settings applies at once.
type BitbucketChecker struct {
	HTTPClient *http.Client

	// Credentials returns the account email (optional) and the token. With an
	// email the pair is sent as HTTP Basic, which is how Bitbucket Cloud API
	// tokens authenticate; without one the token goes as a Bearer token, which
	// is how workspace and repository access tokens authenticate. An empty
	// token means Bitbucket is not configured.
	Credentials func() (email, token string)

	// apiBaseOverride replaces bitbucketAPIBase (test-only).
	apiBaseOverride string

	selfMu   sync.Mutex
	selfName string
	selfUUID string
}

func (c *BitbucketChecker) base() string {
	if c.apiBaseOverride != "" {
		return c.apiBaseOverride
	}
	return bitbucketAPIBase
}

// authHeader builds the Authorization header, or "" when no token is set.
func (c *BitbucketChecker) authHeader() string {
	if c.Credentials == nil {
		return ""
	}
	email, token := c.Credentials()
	if token == "" {
		return ""
	}
	if email != "" {
		return "Basic " + base64.StdEncoding.EncodeToString([]byte(email+":"+token))
	}
	return "Bearer " + token
}

type bitbucketAccount struct {
	Nickname    string `json:"nickname"`
	DisplayName string `json:"display_name"`
	UUID        string `json:"uuid"`
}

// name is the account's handle, the name pings and self-suppression use.
func (a bitbucketAccount) name() string {
	if a.Nickname != "" {
		return a.Nickname
	}
	return a.DisplayName
}

type bitbucketPR struct {
	State       string `json:"state"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Draft       bool   `json:"draft"`
	UpdatedOn   string `json:"updated_on"`
	Source      struct {
		Commit struct {
			Hash string `json:"hash"`
		} `json:"commit"`
	} `json:"source"`
	Reviewers    []bitbucketAccount `json:"reviewers"`
	Participants []struct {
		User           bitbucketAccount `json:"user"`
		Approved       bool             `json:"approved"`
		State          string           `json:"state"`
		ParticipatedOn string           `json:"participated_on"`
	} `json:"participants"`
}

type bitbucketStatus struct {
	State string `json:"state"`
	Name  string `json:"name"`
	URL   string `json:"url"`
}

type bitbucketComment struct {
	ID      int              `json:"id"`
	Deleted bool             `json:"deleted"`
	User    bitbucketAccount `json:"user"`
	Content struct {
		Raw string `json:"raw"`
	} `json:"content"`
	Inline *json.RawMessage `json:"inline"`
	Parent *struct {
		ID int `json:"id"`
	} `json:"parent"`
	Resolution *struct {
		User bitbucketAccount `json:"user"`
	} `json:"resolution"`
	Links struct {
		HTML struct {
			Href string `json:"href"`
		} `json:"html"`
	} `json:"links"`
}

func mapBitbucketState(state string) string {
	switch state {
	case "MERGED":
		return "merged"
	case "DECLINED", "SUPERSEDED":
		return "closed"
	}
	return "open"
}

// Check polls one pull request.
func (c *BitbucketChecker) Check(ctx context.Context, prURL string) (CheckResult, RateBudget, error) {
	workspace, repo, id, err := ParseBitbucketPRURL(prURL)
	if err != nil {
		return CheckResult{}, RateBudget{}, err
	}
	auth := c.authHeader()
	if auth == "" {
		return CheckResult{}, RateBudget{}, errors.New("no bitbucket token configured (set it in the plugin's settings)")
	}
	prBase := fmt.Sprintf("%s/repositories/%s/%s/pullrequests/%d", c.base(), url.PathEscape(workspace), url.PathEscape(repo), id)

	var pr bitbucketPR
	if err := c.getJSON(ctx, auth, prBase, &pr); err != nil {
		return CheckResult{}, RateBudget{}, err
	}
	result := CheckResult{
		State:       mapBitbucketState(pr.State),
		Title:       pr.Title,
		Description: pr.Description,
		HeadSHA:     pr.Source.Commit.Hash,
		IsDraft:     pr.Draft,
	}
	for _, r := range pr.Reviewers {
		result.RequestedReviewers = append(result.RequestedReviewers, r.name())
	}
	if result.State == "merged" || result.State == "closed" {
		if result.State == "merged" {
			result.MergedAt = parseRFC3339OrZero(pr.UpdatedOn)
		}
		return result, RateBudget{}, nil
	}

	self, selfUUID, err := c.self(ctx, auth)
	if err != nil {
		return CheckResult{}, RateBudget{}, err
	}
	c.applyReviews(&result, pr, prURL, self, selfUUID)

	var statuses []bitbucketStatus
	if err := getBitbucketPages(ctx, c, auth, prBase+"/statuses?pagelen=100", &statuses); err != nil {
		return CheckResult{}, RateBudget{}, err
	}
	result.CIBucket, result.CIDetail, result.CIHref = bitbucketCI(statuses)

	// The conflicts endpoint redirects to the file conflicts between source and
	// destination: any entry is a conflict. A pull request it cannot resolve
	// (404) simply has none to report.
	var conflicts []json.RawMessage
	if err := getBitbucketPages(ctx, c, auth, prBase+"/conflicts?pagelen=1", &conflicts); err != nil {
		if status, ok := errors.AsType[*httpStatusError](err); !ok || status.Code != http.StatusNotFound {
			return CheckResult{}, RateBudget{}, err
		}
	}
	result.HasConflict = len(conflicts) > 0

	var comments []bitbucketComment
	if err := getBitbucketPages(ctx, c, auth, prBase+"/comments?pagelen=100", &comments); err != nil {
		return CheckResult{}, RateBudget{}, err
	}
	applyBitbucketComments(&result, comments, prURL)

	result.ApplyCommentFilters(nil)
	return result, RateBudget{}, nil
}

// applyReviews maps participants' verdicts: a standing request for changes
// outranks approvals (it blocks the merge), and the newest verdict names the
// reviewer.
func (c *BitbucketChecker) applyReviews(result *CheckResult, pr bitbucketPR, prURL, self, selfUUID string) {
	var latest time.Time
	var latestName, latestState string
	changes := false
	for _, p := range pr.Participants {
		state := ""
		switch {
		case p.State == "changes_requested":
			state = "CHANGES_REQUESTED"
		case p.Approved || p.State == "approved":
			state = "APPROVED"
		}
		if state == "" {
			continue
		}
		if state == "APPROVED" {
			result.ReviewCount++
		}
		if (selfUUID != "" && p.User.UUID == selfUUID) || (selfUUID == "" && self != "" && p.User.name() == self) {
			result.SelfReviewState = state
		}
		at := parseRFC3339OrZero(p.ParticipatedOn)
		// A verdict of the strongest kind present names the reviewer: any request
		// for changes beats every approval, and within a kind the newest wins.
		better := latestState == "" ||
			(state == "CHANGES_REQUESTED" && !changes) ||
			((state == "CHANGES_REQUESTED") == changes && at.After(latest))
		if better {
			latest, latestName, latestState = at, p.User.name(), state
		}
		if state == "CHANGES_REQUESTED" {
			changes = true
		}
	}
	if latestState != "" {
		result.LatestReviewState = latestState
		result.LatestReviewer = latestName
		result.LatestReviewAt = latest
		result.ReviewHref = prURL
	}
}

// bitbucketCI folds a pull request's build statuses into one bucket: any
// failed or stopped build fails it, else any running one leaves it pending.
func bitbucketCI(statuses []bitbucketStatus) (bucket, detail, href string) {
	if len(statuses) == 0 {
		return "none", "", ""
	}
	bucket = "success"
	for _, s := range statuses {
		switch s.State {
		case "FAILED", "STOPPED":
			return "failure", s.Name, s.URL
		case "INPROGRESS":
			bucket = "pending"
		}
	}
	return bucket, "", ""
}

// applyBitbucketComments sorts comments into review threads (an inline comment
// with no parent, unresolved while it has no resolution, replies being the
// comments pointing at it) and top-level conversation comments.
func applyBitbucketComments(result *CheckResult, comments []bitbucketComment, prURL string) {
	replies := make(map[int][]bitbucketComment)
	for _, c := range comments {
		if !c.Deleted && c.Parent != nil {
			replies[c.Parent.ID] = append(replies[c.Parent.ID], c)
		}
	}
	for _, c := range comments {
		if c.Deleted || c.Parent != nil {
			continue
		}
		id := strconv.Itoa(c.ID)
		href := c.Links.HTML.Href
		if href == "" {
			href = prURL
		}
		switch {
		case c.Inline == nil:
			result.TopLevelComments = append(result.TopLevelComments, TopLevelComment{ID: id, Href: href, Author: c.User.name(), Body: c.Content.Raw})
		case c.Resolution != nil:
			if who := c.Resolution.User.name(); who != "" {
				if result.ResolvedBy == nil {
					result.ResolvedBy = make(map[string]string)
				}
				result.ResolvedBy[id] = who
			}
		default:
			result.UnresolvedThreadIDs = append(result.UnresolvedThreadIDs, id)
			thread := append([]bitbucketComment{c}, replies[c.ID]...)
			// The newest few, matching what the other providers fetch per thread.
			if len(thread) > 3 {
				thread = thread[len(thread)-3:]
			}
			for _, t := range thread {
				tid := strconv.Itoa(t.ID)
				thref := t.Links.HTML.Href
				if thref == "" {
					thref = prURL
				}
				result.ThreadComments = append(result.ThreadComments, ThreadComment{ThreadID: id, ID: tid, Author: t.User.name(), Href: thref})
				result.ThreadCommentIDs = append(result.ThreadCommentIDs, tid)
			}
		}
	}
	for _, c := range result.TopLevelComments {
		result.TopLevelCommentIDs = append(result.TopLevelCommentIDs, c.ID)
	}
}

// self resolves and caches the token's own account.
func (c *BitbucketChecker) self(ctx context.Context, auth string) (name, uuid string, err error) {
	c.selfMu.Lock()
	name, uuid = c.selfName, c.selfUUID
	c.selfMu.Unlock()
	if name != "" || uuid != "" {
		return name, uuid, nil
	}
	var me bitbucketAccount
	if err := c.getJSON(ctx, auth, c.base()+"/user", &me); err != nil {
		return "", "", err
	}
	c.selfMu.Lock()
	c.selfName, c.selfUUID = me.name(), me.UUID
	c.selfMu.Unlock()
	return me.name(), me.UUID, nil
}

// SelfAccount is the token's own account handle, which the scheduler uses to
// keep a session from being pinged about its own reviews and comments. Empty
// with no error when no token is configured.
func (c *BitbucketChecker) SelfAccount(ctx context.Context) (string, error) {
	auth := c.authHeader()
	if auth == "" {
		return "", nil
	}
	name, _, err := c.self(ctx, auth)
	return name, err
}

// DiscoverBranchPR looks up the open pull request whose source branch is
// branch in workspace/repo, for auto-watch. A repository with no such pull
// request, or no token configured, finds nothing without being an error.
func (c *BitbucketChecker) DiscoverBranchPR(ctx context.Context, workspace, repo, branch string) (prURL string, found bool, budget RateBudget, err error) {
	auth := c.authHeader()
	if auth == "" {
		return "", false, RateBudget{}, nil
	}
	quoted := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(branch)
	q := url.Values{"q": {fmt.Sprintf(`source.branch.name="%s" AND state="OPEN"`, quoted)}, "pagelen": {"20"}}
	endpoint := fmt.Sprintf("%s/repositories/%s/%s/pullrequests?%s", c.base(), url.PathEscape(workspace), url.PathEscape(repo), q.Encode())

	var prs []struct {
		Links struct {
			HTML struct {
				Href string `json:"href"`
			} `json:"html"`
		} `json:"links"`
		Source struct {
			Repository struct {
				FullName string `json:"full_name"`
			} `json:"repository"`
		} `json:"source"`
		Destination struct {
			Repository struct {
				FullName string `json:"full_name"`
			} `json:"repository"`
		} `json:"destination"`
	}
	if err := getBitbucketPages(ctx, c, auth, endpoint, &prs); err != nil {
		return "", false, RateBudget{}, err
	}
	// The repository's list also holds pull requests from forks that happen to
	// use the same branch name; only one whose source is the repository itself
	// can be this checkout's.
	for _, pr := range prs {
		if pr.Source.Repository.FullName == pr.Destination.Repository.FullName {
			return pr.Links.HTML.Href, true, RateBudget{}, nil
		}
	}
	return "", false, RateBudget{}, nil
}

// getJSON issues one authenticated GET and decodes the body into out.
func (c *BitbucketChecker) getJSON(ctx context.Context, auth, endpoint string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("bitbucket request to %s: %w", endpoint, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if err := checkHTTPStatus(resp); err != nil {
		return fmt.Errorf("bitbucket %s: %w", endpoint, err)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode bitbucket response from %s: %w", endpoint, err)
	}
	return nil
}

// getBitbucketPages reads a paginated list into out (a pointer to a slice),
// following "next" links, but only ones under the API base: a link pointing
// anywhere else is not followed with the credential.
func getBitbucketPages[T any](ctx context.Context, c *BitbucketChecker, auth, endpoint string, out *[]T) error {
	for range bitbucketMaxPages {
		var page struct {
			Values []T    `json:"values"`
			Next   string `json:"next"`
		}
		if err := c.getJSON(ctx, auth, endpoint, &page); err != nil {
			return err
		}
		*out = append(*out, page.Values...)
		if page.Next == "" || !strings.HasPrefix(page.Next, c.base()+"/") || page.Next == endpoint {
			return nil
		}
		endpoint = page.Next
	}
	return nil
}
