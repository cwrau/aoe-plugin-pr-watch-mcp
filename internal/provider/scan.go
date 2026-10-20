package provider

import (
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// ScanScope is what a subscription watches: one repo, an org/group, or a whole
// platform. Kind is empty only for a GitLab path that has not been resolved
// yet (a project and a group look alike in a URL); the subscribe tool asks the
// API and fills it in.
type ScanScope struct {
	Provider string // "github" | "gitlab"
	Host     string
	Kind     string // "repo" | "org" | "platform" | "" (GitLab, unresolved)
	Path     string // "owner/repo", "org", "group/sub/project"; empty for a platform
}

// ParseScanScopeURL reads a scope from a URL such as https://github.com/o/r,
// https://github.com/org, https://github.com or https://gitlab.example/group.
// github.com is GitHub; any other host is GitLab, whose path stays unresolved.
func ParseScanScopeURL(raw string) (ScanScope, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ScanScope{}, fmt.Errorf("scope_url is empty")
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ScanScope{}, fmt.Errorf("scope_url %q: %w", raw, err)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return ScanScope{}, fmt.Errorf("scope_url %q must be an http(s) URL", raw)
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return ScanScope{}, fmt.Errorf("scope_url %q has no host", raw)
	}
	var segs []string
	for s := range strings.SplitSeq(strings.Trim(u.Path, "/"), "/") {
		if s != "" {
			segs = append(segs, s)
		}
	}

	if host == "github.com" || host == "www.github.com" {
		scope := ScanScope{Provider: "github", Host: "github.com"}
		switch len(segs) {
		case 0:
			scope.Kind = "platform"
		case 1:
			scope.Kind, scope.Path = "org", segs[0]
		default:
			scope.Kind, scope.Path = "repo", segs[0]+"/"+strings.TrimSuffix(segs[1], ".git")
		}
		return scope, nil
	}

	// GitLab: everything before a "/-/" marker is the project or group path.
	if i := slices.Index(segs, "-"); i >= 0 {
		segs = segs[:i]
	}
	scope := ScanScope{Provider: "gitlab", Host: host}
	if len(segs) == 0 {
		scope.Kind = "platform"
		return scope, nil
	}
	segs[len(segs)-1] = strings.TrimSuffix(segs[len(segs)-1], ".git")
	scope.Path = strings.Join(segs, "/")
	return scope, nil
}

// ScanFilters narrow a scope. "me" stands for the worker's own account. All
// fields are optional. A filter the provider cannot express is rejected by
// Validate rather than silently dropped.
type ScanFilters struct {
	ReviewRequested string   `json:"review_requested,omitempty" jsonschema:"only PRs/MRs a review is requested from: me, a login, or (GitHub) a team as org/team"`
	Assignee        string   `json:"assignee,omitempty" jsonschema:"only PRs/MRs assigned to: me, a login, or none"`
	Authors         []string `json:"authors,omitempty" jsonschema:"only PRs/MRs by one of these logins (at most 5)"`
	AuthorsNot      []string `json:"authors_not,omitempty" jsonschema:"skip PRs/MRs by these logins, e.g. bots"`
	Involved        string   `json:"involved,omitempty" jsonschema:"GitHub only: PRs the login is involved in (author, assignee, mentioned, commenter, reviewer); me or a login"`
	Mentioned       string   `json:"mentioned,omitempty" jsonschema:"GitHub only: PRs that mention the login; me or a login"`
	ReviewedBy      string   `json:"reviewed_by,omitempty" jsonschema:"only PRs/MRs this login reviewed (GitLab: approved); me or a login"`
	ReviewStatus    string   `json:"review_status,omitempty" jsonschema:"GitHub only: none, required, approved or changes_requested"`
	Labels          []string `json:"labels,omitempty" jsonschema:"only PRs/MRs carrying all of these labels"`
	LabelsNot       []string `json:"labels_not,omitempty" jsonschema:"skip PRs/MRs carrying any of these labels"`
	Draft           string   `json:"draft,omitempty" jsonschema:"include (default), exclude or only drafts"`
	BaseBranch      string   `json:"base_branch,omitempty" jsonschema:"only PRs/MRs targeting this branch"`
	Branch          []string `json:"branch,omitempty" jsonschema:"only PRs/MRs whose head branch matches one of these globs (* and ?)"`
	BranchNot       []string `json:"branch_not,omitempty" jsonschema:"skip PRs/MRs whose head branch matches one of these globs"`
	Repos           []string `json:"repos,omitempty" jsonschema:"org and platform scopes: only repos matching one of these globs (owner/repo)"`
	ReposNot        []string `json:"repos_not,omitempty" jsonschema:"org and platform scopes: skip repos matching one of these globs"`
	UpdatedWithin   string   `json:"updated_within,omitempty" jsonschema:"only PRs/MRs updated within this long, e.g. 14d, 2w or 36h"`
}

const maxScanAuthors = 5

var updatedWithinRE = regexp.MustCompile(`^(\d+)([hdw])$`)

// ParseUpdatedWithin reads "14d", "2w" or "36h".
func ParseUpdatedWithin(s string) (time.Duration, error) {
	m := updatedWithinRE.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, fmt.Errorf("updated_within %q is not a duration like 14d, 2w or 36h", s)
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("updated_within %q must be positive", s)
	}
	unit := map[string]time.Duration{"h": time.Hour, "d": 24 * time.Hour, "w": 7 * 24 * time.Hour}[m[2]]
	return time.Duration(n) * unit, nil
}

// hasPersonFilter reports whether the filters narrow a scan to people, which
// a platform-wide scope needs: without one it would be every open PR on the
// platform.
func (f ScanFilters) hasPersonFilter() bool {
	return f.ReviewRequested != "" || (f.Assignee != "" && f.Assignee != "none") ||
		len(f.Authors) > 0 || f.Involved != "" || f.Mentioned != "" || f.ReviewedBy != ""
}

// Validate rejects filters that are malformed, that the scope's provider
// cannot express, or that are too loose for the scope.
func (f ScanFilters) Validate(scope ScanScope) error {
	if !slices.Contains([]string{"", "include", "exclude", "only"}, f.Draft) {
		return fmt.Errorf("draft %q is not include, exclude or only", f.Draft)
	}
	if !slices.Contains([]string{"", "none", "required", "approved", "changes_requested"}, f.ReviewStatus) {
		return fmt.Errorf("review_status %q is not none, required, approved or changes_requested", f.ReviewStatus)
	}
	if f.UpdatedWithin != "" {
		if _, err := ParseUpdatedWithin(f.UpdatedWithin); err != nil {
			return err
		}
	}
	if len(f.Authors) > maxScanAuthors {
		return fmt.Errorf("authors lists %d logins, at most %d are supported", len(f.Authors), maxScanAuthors)
	}
	if scope.Kind == "platform" && !f.hasPersonFilter() {
		return fmt.Errorf("a platform scope needs a person filter (review_requested, assignee, authors, involved, mentioned or reviewed_by): without one it would watch every open PR/MR on %s", scope.Host)
	}
	if scope.Provider == "gitlab" {
		switch {
		case f.Involved != "":
			return fmt.Errorf("involved is GitHub only")
		case f.Mentioned != "":
			return fmt.Errorf("mentioned is GitHub only")
		case f.ReviewStatus != "":
			return fmt.Errorf("review_status is GitHub only")
		case strings.Contains(f.ReviewRequested, "/"):
			return fmt.Errorf("team review requests are GitHub only")
		}
	}
	return nil
}

// ScanMatch is one PR/MR a scan found.
type ScanMatch struct {
	URL     string
	Repo    string // owner/repo, or the GitLab project path
	Author  string
	HeadRef string
	Draft   bool
}

// Matches applies the filters no search qualifier covers (globs on repo and
// branch) and re-checks the author lists, so results are right even where a
// provider could only narrow them loosely.
func (f ScanFilters) Matches(m ScanMatch) bool {
	if len(f.Repos) > 0 && !globAny(f.Repos, m.Repo) {
		return false
	}
	if globAny(f.ReposNot, m.Repo) {
		return false
	}
	if len(f.Branch) > 0 && !globAny(f.Branch, m.HeadRef) {
		return false
	}
	if globAny(f.BranchNot, m.HeadRef) {
		return false
	}
	if len(f.Authors) > 0 && !containsFold(f.Authors, m.Author) {
		return false
	}
	return !containsFold(f.AuthorsNot, m.Author)
}

func containsFold(list []string, s string) bool {
	return slices.ContainsFunc(list, func(x string) bool { return strings.EqualFold(x, s) })
}

func globAny(patterns []string, s string) bool {
	for _, p := range patterns {
		if globMatch(p, s) {
			return true
		}
	}
	return false
}

// globMatch is a case-insensitive match where * is any run of characters
// (slashes included) and ? is exactly one.
func globMatch(pattern, s string) bool {
	var b strings.Builder
	b.WriteString("(?is)^")
	for _, r := range pattern {
		switch r {
		case '*':
			b.WriteString(".*")
		case '?':
			b.WriteString(".")
		default:
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	return err == nil && re.MatchString(s)
}

const (
	maxGitHubSearchQueryLen = 256 // GitHub rejects longer search queries
	maxScanQueries          = 10
)

// resolveMe turns "me" into the worker's own login.
func resolveMe(v, self string) (string, error) {
	if v != "me" {
		return v, nil
	}
	if self == "" {
		return "", fmt.Errorf(`"me" needs the worker's own account, which could not be resolved`)
	}
	return self, nil
}

func searchValue(v string) string {
	if strings.ContainsAny(v, " \"") {
		return strconv.Quote(v)
	}
	return v
}

// BuildGitHubSearchQueries turns a scope and filters into GitHub search
// queries whose union is the match set: an org is searched as org: and user:
// (a name may be either), and a list of authors is one query per author.
func BuildGitHubSearchQueries(scope ScanScope, f ScanFilters, self string, now time.Time) ([]string, error) {
	var filterParts []string
	if f.ReviewRequested != "" {
		if strings.Contains(f.ReviewRequested, "/") {
			filterParts = append(filterParts, "team-review-requested:"+f.ReviewRequested)
		} else {
			login, err := resolveMe(f.ReviewRequested, self)
			if err != nil {
				return nil, err
			}
			filterParts = append(filterParts, "review-requested:"+login)
		}
	}
	if f.Assignee == "none" {
		filterParts = append(filterParts, "no:assignee")
	} else if f.Assignee != "" {
		login, err := resolveMe(f.Assignee, self)
		if err != nil {
			return nil, err
		}
		filterParts = append(filterParts, "assignee:"+login)
	}
	for _, q := range []struct{ qualifier, value string }{
		{"involves", f.Involved}, {"mentions", f.Mentioned}, {"reviewed-by", f.ReviewedBy},
	} {
		if q.value == "" {
			continue
		}
		login, err := resolveMe(q.value, self)
		if err != nil {
			return nil, err
		}
		filterParts = append(filterParts, q.qualifier+":"+login)
	}
	switch f.Draft {
	case "exclude":
		filterParts = append(filterParts, "draft:false")
	case "only":
		filterParts = append(filterParts, "draft:true")
	}
	if f.BaseBranch != "" {
		filterParts = append(filterParts, "base:"+searchValue(f.BaseBranch))
	}
	if f.ReviewStatus != "" {
		filterParts = append(filterParts, "review:"+f.ReviewStatus)
	}
	for _, l := range f.Labels {
		filterParts = append(filterParts, "label:"+searchValue(l))
	}
	for _, l := range f.LabelsNot {
		filterParts = append(filterParts, "-label:"+searchValue(l))
	}
	for _, a := range f.AuthorsNot {
		filterParts = append(filterParts, "-author:"+a)
	}
	if f.UpdatedWithin != "" {
		d, err := ParseUpdatedWithin(f.UpdatedWithin)
		if err != nil {
			return nil, err
		}
		filterParts = append(filterParts, "updated:>="+now.Add(-d).UTC().Format("2006-01-02"))
	}

	var scopes []string
	switch scope.Kind {
	case "repo":
		scopes = []string{"repo:" + scope.Path}
	case "org":
		scopes = []string{"org:" + scope.Path, "user:" + scope.Path}
	default:
		scopes = []string{""}
	}
	authors := f.Authors
	if len(authors) == 0 {
		authors = []string{""}
	}
	if len(scopes)*len(authors) > maxScanQueries {
		return nil, fmt.Errorf("the filters expand to %d searches, at most %d are supported", len(scopes)*len(authors), maxScanQueries)
	}

	var queries []string
	for _, author := range authors {
		for _, sc := range scopes {
			parts := []string{"is:pr", "is:open", "archived:false"}
			if sc != "" {
				parts = append(parts, sc)
			}
			parts = append(parts, filterParts...)
			if author != "" {
				parts = append(parts, "author:"+author)
			}
			q := strings.Join(parts, " ")
			if len(q) > maxGitHubSearchQueryLen {
				return nil, fmt.Errorf("the search query is %d characters, GitHub accepts at most %d: use fewer or shorter filters", len(q), maxGitHubSearchQueryLen)
			}
			queries = append(queries, q)
		}
	}
	return queries, nil
}

// GitLabSearch is one merge-requests list request: an endpoint under
// /api/v4 and its query parameters.
type GitLabSearch struct {
	Endpoint string
	Query    url.Values
}

// BuildGitLabSearches turns a resolved scope and filters into GitLab list
// requests whose union is the match set (a list of authors is one request per
// author, since the API takes a single author).
func BuildGitLabSearches(scope ScanScope, f ScanFilters, self string, now time.Time) ([]GitLabSearch, error) {
	var endpoint string
	base := url.Values{"state": {"opened"}, "per_page": {"100"}}
	switch scope.Kind {
	case "repo":
		endpoint = "/api/v4/projects/" + url.PathEscape(scope.Path) + "/merge_requests"
	case "org":
		endpoint = "/api/v4/groups/" + url.PathEscape(scope.Path) + "/merge_requests"
		base.Set("include_subgroups", "true")
	case "platform":
		endpoint = "/api/v4/merge_requests"
		base.Set("scope", "all")
	default:
		return nil, fmt.Errorf("gitlab scope %q is not resolved to a project or group yet", scope.Path)
	}

	if f.ReviewRequested != "" {
		login, err := resolveMe(f.ReviewRequested, self)
		if err != nil {
			return nil, err
		}
		base.Set("reviewer_username", login)
	}
	if f.Assignee == "none" {
		base.Set("assignee_id", "None")
	} else if f.Assignee != "" {
		login, err := resolveMe(f.Assignee, self)
		if err != nil {
			return nil, err
		}
		base.Set("assignee_username", login)
	}
	if f.ReviewedBy != "" {
		login, err := resolveMe(f.ReviewedBy, self)
		if err != nil {
			return nil, err
		}
		base.Set("approved_by_usernames", login)
	}
	if len(f.Labels) > 0 {
		base.Set("labels", strings.Join(f.Labels, ","))
	}
	if len(f.LabelsNot) > 0 {
		base.Set("not[labels]", strings.Join(f.LabelsNot, ","))
	}
	switch f.Draft {
	case "exclude":
		base.Set("draft", "no")
	case "only":
		base.Set("draft", "yes")
	}
	if f.BaseBranch != "" {
		base.Set("target_branch", f.BaseBranch)
	}
	if f.UpdatedWithin != "" {
		d, err := ParseUpdatedWithin(f.UpdatedWithin)
		if err != nil {
			return nil, err
		}
		base.Set("updated_after", now.Add(-d).UTC().Format(time.RFC3339))
	}

	authors := f.Authors
	if len(authors) == 0 {
		authors = []string{""}
	}
	var out []GitLabSearch
	for _, a := range authors {
		q := url.Values{}
		for k, v := range base {
			q[k] = slices.Clone(v)
		}
		if a != "" {
			q.Set("author_username", a)
		}
		out = append(out, GitLabSearch{Endpoint: endpoint, Query: q})
	}
	return out, nil
}
