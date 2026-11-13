package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

func TestParseBitbucketPRURL(t *testing.T) {
	tests := []struct {
		url             string
		workspace, repo string
		id              int
		wantErr         bool
	}{
		{url: "https://bitbucket.org/acme/widgets/pull-requests/12", workspace: "acme", repo: "widgets", id: 12},
		{url: "https://bitbucket.org/acme/widgets/pull-requests/12/diff", workspace: "acme", repo: "widgets", id: 12},
		{url: "http://bitbucket.org/acme/widgets/pull-requests/3#comment-9", workspace: "acme", repo: "widgets", id: 3},
		{url: "https://bitbucket.org/acme/widgets/pull-requests/", wantErr: true},
		{url: "https://bitbucket.example.com/acme/widgets/pull-requests/12", wantErr: true},
		{url: "https://evilbitbucket.org/acme/widgets/pull-requests/12", wantErr: true},
		{url: "https://github.com/acme/widgets/pull/12", wantErr: true},
	}
	for _, tt := range tests {
		ws, repo, id, err := ParseBitbucketPRURL(tt.url)
		if (err != nil) != tt.wantErr {
			t.Errorf("ParseBitbucketPRURL(%q) error = %v, wantErr %v", tt.url, err, tt.wantErr)
			continue
		}
		if ws != tt.workspace || repo != tt.repo || id != tt.id {
			t.Errorf("ParseBitbucketPRURL(%q) = %q, %q, %d; want %q, %q, %d", tt.url, ws, repo, id, tt.workspace, tt.repo, tt.id)
		}
	}
}

func TestShortRefBitbucket(t *testing.T) {
	if got := ShortRef("https://bitbucket.org/acme/widgets/pull-requests/12", "bitbucket"); got != "acme/widgets#12" {
		t.Errorf("ShortRef = %q, want acme/widgets#12", got)
	}
}

// bitbucketFake serves one pull request of acme/widgets from canned JSON.
type bitbucketFake struct {
	pr         map[string]any
	statuses   []map[string]any
	comments   []map[string]any
	conflicts  []map[string]any
	self       map[string]any
	gotAuth    []string
	statusCode int // overrides the PR endpoint's status when non-zero
}

func (f *bitbucketFake) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	page := func(values any) map[string]any { return map[string]any{"pagelen": 100, "page": 1, "values": values} }
	write := func(w http.ResponseWriter, r *http.Request, v any) {
		f.gotAuth = append(f.gotAuth, r.Header.Get("Authorization"))
		_ = json.NewEncoder(w).Encode(v)
	}
	mux.HandleFunc("/2.0/repositories/acme/widgets/pullrequests/12", func(w http.ResponseWriter, r *http.Request) {
		if f.statusCode != 0 {
			w.WriteHeader(f.statusCode)
			return
		}
		write(w, r, f.pr)
	})
	mux.HandleFunc("/2.0/repositories/acme/widgets/pullrequests/12/statuses", func(w http.ResponseWriter, r *http.Request) { write(w, r, page(f.statuses)) })
	mux.HandleFunc("/2.0/repositories/acme/widgets/pullrequests/12/comments", func(w http.ResponseWriter, r *http.Request) { write(w, r, page(f.comments)) })
	mux.HandleFunc("/2.0/repositories/acme/widgets/pullrequests/12/conflicts", func(w http.ResponseWriter, r *http.Request) { write(w, r, page(f.conflicts)) })
	mux.HandleFunc("/2.0/user", func(w http.ResponseWriter, r *http.Request) { write(w, r, f.self) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func (f *bitbucketFake) checker(srv *httptest.Server, email, token string) *BitbucketChecker {
	return &BitbucketChecker{
		HTTPClient:      srv.Client(),
		Credentials:     func() (string, string) { return email, token },
		apiBaseOverride: srv.URL + "/2.0",
	}
}

func account(nick string) map[string]any {
	return map[string]any{"nickname": nick, "display_name": nick + " display", "uuid": "{" + nick + "-uuid}"}
}

func openPR() map[string]any {
	return map[string]any{
		"state": "OPEN", "title": "Add widget", "description": "body", "draft": false,
		"source":       map[string]any{"commit": map[string]any{"hash": "abc123"}},
		"participants": []map[string]any{},
		"reviewers":    []map[string]any{},
		"links":        map[string]any{"html": map[string]any{"href": "https://bitbucket.org/acme/widgets/pull-requests/12"}},
	}
}

const bbURL = "https://bitbucket.org/acme/widgets/pull-requests/12"

func TestBitbucketCheckerBasics(t *testing.T) {
	f := &bitbucketFake{pr: openPR(), self: account("me")}
	f.pr["draft"] = true
	f.pr["reviewers"] = []map[string]any{{"nickname": "rev1"}, {"nickname": "rev2"}}
	f.conflicts = []map[string]any{{"path": "a.go", "scenario": "content"}}
	srv := f.server(t)

	res, _, err := f.checker(srv, "", "tok").Check(t.Context(), bbURL)
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if res.State != "open" || res.Title != "Add widget" || res.Description != "body" || res.HeadSHA != "abc123" || !res.IsDraft {
		t.Errorf("basics = %+v", res)
	}
	if !res.HasConflict {
		t.Error("HasConflict = false, want true with a file conflict listed")
	}
	if !slices.Equal(res.RequestedReviewers, []string{"rev1", "rev2"}) {
		t.Errorf("RequestedReviewers = %v", res.RequestedReviewers)
	}
}

func TestBitbucketCheckerStates(t *testing.T) {
	for state, want := range map[string]string{"OPEN": "open", "MERGED": "merged", "DECLINED": "closed", "SUPERSEDED": "closed"} {
		f := &bitbucketFake{pr: openPR(), self: account("me")}
		f.pr["state"] = state
		res, _, err := f.checker(f.server(t), "", "tok").Check(t.Context(), bbURL)
		if err != nil {
			t.Fatalf("%s: Check() error = %v", state, err)
		}
		if res.State != want {
			t.Errorf("state %s -> %q, want %q", state, res.State, want)
		}
	}
}

func TestBitbucketCheckerAuthScheme(t *testing.T) {
	f := &bitbucketFake{pr: openPR(), self: account("me")}
	srv := f.server(t)

	if _, _, err := f.checker(srv, "me@example.com", "secret").Check(t.Context(), bbURL); err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if got := f.gotAuth[0]; got != "Basic bWVAZXhhbXBsZS5jb206c2VjcmV0" {
		t.Errorf("with an email the header = %q, want Basic base64(email:token)", got)
	}

	f.gotAuth = nil
	if _, _, err := f.checker(srv, "", "secret").Check(t.Context(), bbURL); err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if got := f.gotAuth[0]; got != "Bearer secret" {
		t.Errorf("without an email the header = %q, want Bearer secret", got)
	}
}

func TestBitbucketCheckerNoTokenIsAnErrorAndSendsNothing(t *testing.T) {
	f := &bitbucketFake{pr: openPR()}
	srv := f.server(t)
	_, _, err := f.checker(srv, "", "").Check(t.Context(), bbURL)
	if err == nil {
		t.Fatal("Check() error = nil, want an error naming the missing token")
	}
	if len(f.gotAuth) != 0 {
		t.Errorf("sent %d requests without a token", len(f.gotAuth))
	}
}

func TestBitbucketCheckerRejectedTokenIsAnError(t *testing.T) {
	f := &bitbucketFake{pr: openPR(), statusCode: http.StatusUnauthorized}
	if _, _, err := f.checker(f.server(t), "", "bad").Check(t.Context(), bbURL); err == nil {
		t.Fatal("Check() error = nil, want an error on 401")
	}
}

func TestBitbucketCheckerReviews(t *testing.T) {
	participant := func(nick, state string, approved bool, at string) map[string]any {
		return map[string]any{"user": account(nick), "role": "REVIEWER", "approved": approved, "state": state, "participated_on": at}
	}
	tests := []struct {
		name         string
		participants []map[string]any
		wantCount    int
		wantState    string
		wantReviewer string
		wantSelf     string
	}{
		{name: "none", participants: []map[string]any{participant("a", "", false, "")}},
		{
			name:         "one approval",
			participants: []map[string]any{participant("a", "approved", true, "2026-11-01T10:00:00Z")},
			wantCount:    1, wantState: "APPROVED", wantReviewer: "a",
		},
		{
			name: "changes requested outranks an approval",
			participants: []map[string]any{
				participant("a", "approved", true, "2026-11-01T10:00:00Z"),
				participant("b", "changes_requested", false, "2026-11-01T09:00:00Z"),
			},
			wantCount: 1, wantState: "CHANGES_REQUESTED", wantReviewer: "b",
		},
		{
			name: "latest approval names the reviewer",
			participants: []map[string]any{
				participant("a", "approved", true, "2026-11-01T10:00:00Z"),
				participant("c", "approved", true, "2026-11-02T10:00:00Z"),
			},
			wantCount: 2, wantState: "APPROVED", wantReviewer: "c",
		},
		{
			name:         "own approval is the self review",
			participants: []map[string]any{participant("me", "approved", true, "2026-11-01T10:00:00Z")},
			wantCount:    1, wantState: "APPROVED", wantReviewer: "me", wantSelf: "APPROVED",
		},
		{
			name:         "own request for changes is the self review",
			participants: []map[string]any{participant("me", "changes_requested", false, "2026-11-01T10:00:00Z")},
			wantState:    "CHANGES_REQUESTED", wantReviewer: "me", wantSelf: "CHANGES_REQUESTED",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &bitbucketFake{pr: openPR(), self: account("me")}
			f.pr["participants"] = tt.participants
			res, _, err := f.checker(f.server(t), "", "tok").Check(t.Context(), bbURL)
			if err != nil {
				t.Fatalf("Check() error = %v", err)
			}
			if res.ReviewCount != tt.wantCount || res.LatestReviewState != tt.wantState || res.LatestReviewer != tt.wantReviewer || res.SelfReviewState != tt.wantSelf {
				t.Errorf("review = count %d state %q reviewer %q self %q; want %d %q %q %q",
					res.ReviewCount, res.LatestReviewState, res.LatestReviewer, res.SelfReviewState,
					tt.wantCount, tt.wantState, tt.wantReviewer, tt.wantSelf)
			}
		})
	}
}

func TestBitbucketCheckerCI(t *testing.T) {
	status := func(state, name string) map[string]any {
		return map[string]any{"state": state, "name": name, "url": "https://ci.example.com/" + name}
	}
	tests := []struct {
		name       string
		statuses   []map[string]any
		wantBucket string
		wantDetail string
		wantHref   string
	}{
		{name: "none", wantBucket: "none"},
		{name: "all green", statuses: []map[string]any{status("SUCCESSFUL", "build"), status("SUCCESSFUL", "lint")}, wantBucket: "success"},
		{name: "running", statuses: []map[string]any{status("SUCCESSFUL", "build"), status("INPROGRESS", "lint")}, wantBucket: "pending"},
		{name: "failed wins over running", statuses: []map[string]any{status("INPROGRESS", "lint"), status("FAILED", "build")}, wantBucket: "failure", wantDetail: "build", wantHref: "https://ci.example.com/build"},
		{name: "stopped is a failure", statuses: []map[string]any{status("STOPPED", "build")}, wantBucket: "failure", wantDetail: "build", wantHref: "https://ci.example.com/build"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &bitbucketFake{pr: openPR(), self: account("me"), statuses: tt.statuses}
			res, _, err := f.checker(f.server(t), "", "tok").Check(t.Context(), bbURL)
			if err != nil {
				t.Fatalf("Check() error = %v", err)
			}
			if res.CIBucket != tt.wantBucket || res.CIDetail != tt.wantDetail || res.CIHref != tt.wantHref {
				t.Errorf("CI = %q %q %q; want %q %q %q", res.CIBucket, res.CIDetail, res.CIHref, tt.wantBucket, tt.wantDetail, tt.wantHref)
			}
		})
	}
}

func TestBitbucketCheckerComments(t *testing.T) {
	href := func(id int) map[string]any {
		return map[string]any{"html": map[string]any{"href": bbURL + "/comments#comment-" + string(rune('0'+id))}}
	}
	comment := func(id int, nick string, extra map[string]any) map[string]any {
		c := map[string]any{"id": id, "user": account(nick), "content": map[string]any{"raw": "text"}, "deleted": false, "links": href(id)}
		for k, v := range extra {
			c[k] = v
		}
		return c
	}
	f := &bitbucketFake{pr: openPR(), self: account("me"), comments: []map[string]any{
		comment(1, "a", nil), // top-level conversation comment
		comment(2, "a", map[string]any{"inline": map[string]any{"path": "x.go", "to": 3}}),                                                     // unresolved thread
		comment(3, "b", map[string]any{"inline": map[string]any{"path": "x.go", "to": 3}, "parent": map[string]any{"id": 2}}),                  // its reply
		comment(4, "a", map[string]any{"inline": map[string]any{"path": "y.go", "to": 1}, "resolution": map[string]any{"user": account("c")}}), // resolved thread
		comment(5, "a", map[string]any{"deleted": true}),                                                                                       // deleted
		comment(6, "b", map[string]any{"parent": map[string]any{"id": 1}}),                                                                     // reply to the top-level comment
	}}
	res, _, err := f.checker(f.server(t), "", "tok").Check(t.Context(), bbURL)
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if !slices.Equal(res.UnresolvedThreadIDs, []string{"2"}) {
		t.Errorf("UnresolvedThreadIDs = %v, want [2]", res.UnresolvedThreadIDs)
	}
	if len(res.TopLevelCommentIDs) != 1 || res.TopLevelCommentIDs[0] != "1" {
		t.Errorf("TopLevelCommentIDs = %v, want [1]", res.TopLevelCommentIDs)
	}
	var replyIDs []string
	for _, c := range res.ThreadComments {
		replyIDs = append(replyIDs, c.ID)
	}
	if !slices.Equal(replyIDs, []string{"2", "3"}) {
		t.Errorf("thread comments = %v, want the root and reply of the unresolved thread [2 3]", replyIDs)
	}
	if got := res.ResolvedBy["4"]; got != "c" {
		t.Errorf("ResolvedBy[4] = %q, want c", got)
	}
}

func TestBitbucketCheckerFinishedPRSkipsTheExtraCalls(t *testing.T) {
	f := &bitbucketFake{pr: openPR(), self: account("me")}
	f.pr["state"] = "MERGED"
	f.pr["updated_on"] = "2026-11-03T12:00:00Z"
	srv := f.server(t)
	res, _, err := f.checker(srv, "", "tok").Check(t.Context(), bbURL)
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if res.State != "merged" || res.MergedAt.IsZero() {
		t.Errorf("merged result = %+v, want state merged with a time", res)
	}
}

func TestBitbucketCheckerSelfAccountIsCached(t *testing.T) {
	f := &bitbucketFake{pr: openPR(), self: account("me")}
	c := f.checker(f.server(t), "", "tok")
	for range 2 {
		name, err := c.SelfAccount(context.Background())
		if err != nil || name != "me" {
			t.Fatalf("SelfAccount() = %q, %v; want me", name, err)
		}
	}
	if len(f.gotAuth) != 1 {
		t.Errorf("SelfAccount made %d requests, want 1 (cached)", len(f.gotAuth))
	}
}

func TestBitbucketCheckerDiscoverBranchPR(t *testing.T) {
	var gotQuery string
	mux := http.NewServeMux()
	mux.HandleFunc("/2.0/repositories/acme/widgets/pullrequests", func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query().Get("q")
		_ = json.NewEncoder(w).Encode(map[string]any{"values": []map[string]any{
			{ // from a fork: not this checkout's
				"links":       map[string]any{"html": map[string]any{"href": "https://bitbucket.org/acme/widgets/pull-requests/7"}},
				"source":      map[string]any{"repository": map[string]any{"full_name": "someone/widgets"}},
				"destination": map[string]any{"repository": map[string]any{"full_name": "acme/widgets"}},
			},
			{
				"links":       map[string]any{"html": map[string]any{"href": bbURL}},
				"source":      map[string]any{"repository": map[string]any{"full_name": "acme/widgets"}},
				"destination": map[string]any{"repository": map[string]any{"full_name": "acme/widgets"}},
			},
		}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := &BitbucketChecker{HTTPClient: srv.Client(), Credentials: func() (string, string) { return "", "tok" }, apiBaseOverride: srv.URL + "/2.0"}

	got, found, _, err := c.DiscoverBranchPR(t.Context(), "acme", "widgets", `feat/"x"`)
	if err != nil || !found || got != bbURL {
		t.Fatalf("DiscoverBranchPR() = %q, %v, %v; want %q", got, found, err, bbURL)
	}
	if want := `source.branch.name="feat/\"x\"" AND state="OPEN"`; gotQuery != want {
		t.Errorf("query = %q, want %q (branch quoted and escaped)", gotQuery, want)
	}
}

// bbAuthServer answers 401 unless the Authorization header is one of accepted,
// and records every header it saw.
type bbAuthServer struct {
	accepted map[string]bool
	seen     []string
}

func (a *bbAuthServer) start(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Get("Authorization")
		a.seen = append(a.seen, h)
		if !a.accepted[h] {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"nickname": "me", "uuid": "{me}"})
	}))
	t.Cleanup(srv.Close)
	return srv
}

const (
	basicGitEmail = "Basic Z2l0QGV4YW1wbGUuY29tOnRvaw==" // git@example.com:tok
	basicExplicit = "Basic bWVAZXhhbXBsZS5jb206dG9r"     // me@example.com:tok
)

func authChecker(srv *httptest.Server, email, token, gitEmail string, gitCalls *int) *BitbucketChecker {
	return &BitbucketChecker{
		HTTPClient:      srv.Client(),
		Credentials:     func() (string, string) { return email, token },
		apiBaseOverride: srv.URL,
		GitEmail: func(_ context.Context, _, _ string) string {
			if gitCalls != nil {
				*gitCalls++
			}
			return gitEmail
		},
	}
}

func TestBitbucketAuthBearerWorksAndNeverAsksForTheGitEmail(t *testing.T) {
	a := &bbAuthServer{accepted: map[string]bool{"Bearer tok": true}}
	gitCalls := 0
	c := authChecker(a.start(t), "", "tok", "git@example.com", &gitCalls)
	if _, err := c.SelfAccount(t.Context()); err != nil {
		t.Fatalf("SelfAccount() error = %v", err)
	}
	if gitCalls != 0 {
		t.Errorf("git email asked for %d times, want 0 when Bearer is accepted", gitCalls)
	}
}

func TestBitbucketAuthFallsBackToBasicWithTheGitEmailAndRemembersIt(t *testing.T) {
	a := &bbAuthServer{accepted: map[string]bool{basicGitEmail: true}}
	c := authChecker(a.start(t), "", "tok", "git@example.com", nil)

	if _, err := c.SelfAccount(t.Context()); err != nil {
		t.Fatalf("SelfAccount() error = %v", err)
	}
	if want := []string{"Bearer tok", basicGitEmail}; !slices.Equal(a.seen, want) {
		t.Fatalf("first call headers = %v, want %v", a.seen, want)
	}

	a.seen = nil
	var pr struct{}
	if err := c.getJSON(t.Context(), c.base()+"/anything", &pr); err != nil {
		t.Fatalf("second request error = %v", err)
	}
	if want := []string{basicGitEmail}; !slices.Equal(a.seen, want) {
		t.Errorf("later request headers = %v, want %v straight away (the working scheme is remembered)", a.seen, want)
	}
}

func TestBitbucketAuthForgetsTheSchemeWhenTheTokenChanges(t *testing.T) {
	a := &bbAuthServer{accepted: map[string]bool{basicGitEmail: true}}
	token := "tok"
	c := authChecker(a.start(t), "", "", "git@example.com", nil)
	c.Credentials = func() (string, string) { return "", token }

	if _, err := c.SelfAccount(t.Context()); err != nil {
		t.Fatalf("SelfAccount() error = %v", err)
	}
	token = "other"
	a.seen = nil
	var v struct{}
	_ = c.getJSON(t.Context(), c.base()+"/anything", &v)
	if len(a.seen) == 0 || a.seen[0] != "Bearer other" {
		t.Errorf("headers after a token change = %v, want to start over with Bearer other", a.seen)
	}
}

func TestBitbucketAuthFallbackNeedsAGitEmail(t *testing.T) {
	a := &bbAuthServer{accepted: map[string]bool{basicGitEmail: true}}
	c := authChecker(a.start(t), "", "tok", "", nil)
	var v struct{}
	err := c.getJSON(t.Context(), c.base()+"/anything", &v)
	if err == nil {
		t.Fatal("getJSON() error = nil, want the 401 when there is no git email to fall back to")
	}
	if want := []string{"Bearer tok"}; !slices.Equal(a.seen, want) {
		t.Errorf("headers = %v, want only %v", a.seen, want)
	}
}

// When every scheme is refused the error names each one: reporting only the
// last (the Basic fallback) hid that Bearer was refused first, and why.
func TestBitbucketAuthErrorNamesEveryRefusedScheme(t *testing.T) {
	a := &bbAuthServer{accepted: map[string]bool{}}
	c := authChecker(a.start(t), "", "tok", "git@example.com", nil)
	var v struct{}
	err := c.getJSON(t.Context(), c.base()+"/anything", &v)
	if err == nil {
		t.Fatal("getJSON() error = nil, want the refusal")
	}
	msg := err.Error()
	for _, want := range []string{"Bearer", "Basic (git email)"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error = %q, want it to name %q", msg, want)
		}
	}
	if strings.Contains(msg, "tok") && strings.Contains(msg, "Bearer tok") {
		t.Errorf("error = %q leaks the token", msg)
	}
}

// Git identities are per directory, so the fallback email has to come from
// the project the request is about: the checker names the repository to the
// callback, for polls and for discovery alike.
func TestBitbucketAuthAsksForTheGitEmailOfTheRepositoryInQuestion(t *testing.T) {
	a := &bbAuthServer{accepted: map[string]bool{basicGitEmail: true}}
	var asked [][2]string
	c := authChecker(a.start(t), "", "tok", "", nil)
	c.GitEmail = func(_ context.Context, workspace, repo string) string {
		asked = append(asked, [2]string{workspace, repo})
		return "git@example.com"
	}

	if _, _, _, err := c.DiscoverBranchPR(t.Context(), "acme", "widgets", "feature"); err != nil {
		t.Fatalf("DiscoverBranchPR() error = %v", err)
	}
	if want := [][2]string{{"acme", "widgets"}}; !slices.Equal(asked, want) {
		t.Errorf("GitEmail asked for %v, want %v", asked, want)
	}

	// A request that is about no repository (the account lookup) passes none.
	asked = nil
	c.authMu.Lock()
	c.authWorked = "" // forget the scheme learned above
	c.authMu.Unlock()
	if _, err := c.SelfAccount(t.Context()); err != nil {
		t.Fatalf("SelfAccount() error = %v", err)
	}
	if want := [][2]string{{"", ""}}; !slices.Equal(asked, want) {
		t.Errorf("GitEmail asked for %v on the account lookup, want %v", asked, want)
	}
}

func TestBitbucketAuthExplicitEmailIsBasicOnlyWithNoFallback(t *testing.T) {
	a := &bbAuthServer{accepted: map[string]bool{}}
	gitCalls := 0
	c := authChecker(a.start(t), "me@example.com", "tok", "git@example.com", &gitCalls)
	var v struct{}
	if err := c.getJSON(t.Context(), c.base()+"/anything", &v); err == nil {
		t.Fatal("getJSON() error = nil, want the 401")
	}
	if want := []string{basicExplicit}; !slices.Equal(a.seen, want) || gitCalls != 0 {
		t.Errorf("headers = %v, git calls = %d; want only %v and no git email lookup", a.seen, gitCalls, want)
	}
}

func TestBitbucketCheckerDiscoverWithoutATokenFindsNothing(t *testing.T) {
	c := &BitbucketChecker{HTTPClient: http.DefaultClient, Credentials: func() (string, string) { return "", "" }}
	if _, found, _, err := c.DiscoverBranchPR(t.Context(), "acme", "widgets", "main"); found || err != nil {
		t.Errorf("DiscoverBranchPR() found=%v err=%v, want neither without a token", found, err)
	}
}
