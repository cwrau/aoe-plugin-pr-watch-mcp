package scheduler

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cwrau/aoe-plugin-pr-watch-mcp/internal/provider"
	"github.com/cwrau/aoe-plugin-pr-watch-mcp/internal/registry"
)

type fakeGitHubScan struct {
	queries [][]string // every SearchPullRequests call's queries
	result  provider.ScanResult
	err     error
}

func (f *fakeGitHubScan) SearchPullRequests(ctx context.Context, queries []string, limit int) (provider.ScanResult, error) {
	f.queries = append(f.queries, queries)
	return f.result, f.err
}

type fakeGitLabScan struct {
	calls  int
	result provider.ScanResult
	err    error
}

func (f *fakeGitLabScan) SearchMergeRequests(ctx context.Context, host string, searches []provider.GitLabSearch) (provider.ScanResult, error) {
	f.calls++
	return f.result, f.err
}

func ghMatch(n int, author string) provider.ScanMatch {
	return provider.ScanMatch{URL: fmt.Sprintf("https://github.com/o/r/pull/%d", n), Repo: "o/r", Author: author, HeadRef: fmt.Sprintf("feat/%d", n)}
}

type subTest struct {
	t     *testing.T
	store *registry.Store
	gh    *fakeGitHubScan
	notif *fakeNotifier
	sched *Scheduler
	now   time.Time // the fake clock, moved on by every scan
}

func newSubTest(t *testing.T, self string) *subTest {
	t.Helper()
	st := &subTest{t: t, store: openTestStore(t), gh: &fakeGitHubScan{result: provider.ScanResult{Complete: true}}, notif: &fakeNotifier{}, now: time.Now()}
	st.sched = &Scheduler{
		Store: st.store, GitHub: &fakeGitHubChecker{selfLogin: self}, GitLab: &fakeGitLabChecker{},
		Sessions: alwaysLiveSessions(), Notify: st.notif, GitHubScan: st.gh,
	}
	return st
}

func (st *subTest) subscribe(mut func(*registry.Subscription)) string {
	st.t.Helper()
	sub := registry.Subscription{
		SessionID: "sess1", Provider: "github", Host: "github.com", ScopeKind: "repo", ScopePath: "o/r",
		Filters: `{"review_requested":"me"}`, Persona: registry.PersonaReviewer, MaxWatches: 25, CreatedAt: time.Now(),
	}
	if mut != nil {
		mut(&sub)
	}
	id, err := st.store.AddSubscription(context.Background(), sub)
	if err != nil {
		st.t.Fatal(err)
	}
	return id
}

// scan runs one scan pass as if the subscription were due, then flushes pings.
func (st *subTest) scan() []string {
	st.t.Helper()
	st.notif.sent = nil
	st.now = st.now.Add(10 * time.Minute)
	st.sched.scanSubscriptions(context.Background(), st.now)
	st.sched.flushPings(context.Background())
	return st.notif.sent
}

func TestScanRegistersMatchesSilentlyThenAnnouncesNewOnes(t *testing.T) {
	st := newSubTest(t, "alice")
	id := st.subscribe(nil)
	st.gh.result.Matches = []provider.ScanMatch{ghMatch(1, "bob"), ghMatch(2, "carol")}

	if sent := st.scan(); len(sent) != 0 {
		t.Fatalf("first scan sent %q, want it silent", sent)
	}
	for _, n := range []int{1, 2} {
		reg := getReg(t, st.store, fmt.Sprintf("https://github.com/o/r/pull/%d", n), "sess1")
		if reg.SubscriptionID != id || reg.Persona != registry.PersonaReviewer || reg.Status != registry.StatusWatching {
			t.Errorf("registration %d = %+v, want a reviewer watch tagged with the subscription", n, reg)
		}
		if silent, _ := st.store.FirstPollSilent(context.Background(), reg.PRURL, "sess1"); !silent {
			t.Errorf("registration %d is not set to a silent first poll", n)
		}
	}
	if len(st.gh.queries) != 1 || !strings.Contains(st.gh.queries[0][0], "review-requested:alice") || !strings.Contains(st.gh.queries[0][0], "repo:o/r") {
		t.Errorf("queries = %v, want the filters turned into a GitHub search with the worker's login", st.gh.queries)
	}

	st.gh.result.Matches = append(st.gh.result.Matches, ghMatch(3, "dave"))
	sent := st.scan()
	want := "sess1: [pr-watch] o/r#3: MATCHED new PR by dave (https://github.com/o/r/pull/3)"
	if len(sent) != 1 || sent[0] != want {
		t.Errorf("second scan sent %q, want [%q]", sent, want)
	}
	if st.scan(); len(st.notif.sent) != 0 {
		t.Errorf("a scan with nothing new sent %q", st.notif.sent)
	}
}

func TestScanReportExistingAnnouncesTheFirstMatches(t *testing.T) {
	st := newSubTest(t, "alice")
	st.subscribe(func(s *registry.Subscription) { s.ReportExisting = true })
	st.gh.result.Matches = []provider.ScanMatch{ghMatch(1, "bob"), ghMatch(2, "carol")}
	sent := st.scan()
	if len(sent) != 1 || !strings.Contains(sent[0], "2 updates") || !strings.Contains(sent[0], "o/r#1: MATCHED") || !strings.Contains(sent[0], "o/r#2: MATCHED") {
		t.Errorf("sent %q, want both matches in one batched message", sent)
	}
}

func TestScanCapsWatchesAndSaysSo(t *testing.T) {
	st := newSubTest(t, "alice")
	id := st.subscribe(func(s *registry.Subscription) { s.ReportExisting = true; s.MaxWatches = 2 })
	st.gh.result.Matches = []provider.ScanMatch{ghMatch(1, "bob"), ghMatch(2, "bob"), ghMatch(3, "bob"), ghMatch(4, "bob")}
	sent := st.scan()
	if n, _ := st.store.CountSubscriptionWatches(context.Background(), id); n != 2 {
		t.Errorf("watches = %d, want the cap of 2", n)
	}
	if len(sent) != 1 || !strings.Contains(sent[0], "2 more") || !strings.Contains(sent[0], "max_watches 2") {
		t.Errorf("sent %q, want the overflow named", sent)
	}
	// Nothing was added now, so the overflow is not repeated every scan.
	if st.scan(); len(st.notif.sent) != 0 {
		t.Errorf("a later scan repeated %q", st.notif.sent)
	}
}

func TestScanSkipsWhatTheSessionAlreadyHasOwnPRsAndFilteredOnes(t *testing.T) {
	st := newSubTest(t, "alice")
	ctx := context.Background()
	stopped := registry.Registration{PRURL: "https://github.com/o/r/pull/1", SessionID: "sess1", Provider: "github", NextDueAt: time.Now(), CreatedAt: time.Now()}
	if err := st.store.Add(ctx, stopped); err != nil {
		t.Fatal(err)
	}
	if _, err := st.store.SetStatusOwnedBy(ctx, stopped.PRURL, "sess1", registry.StatusStopped); err != nil {
		t.Fatal(err)
	}
	id := st.subscribe(func(s *registry.Subscription) {
		s.ReportExisting = true
		s.Filters = `{"review_requested":"me","repos":["o/*"],"branch_not":["wip/*"]}`
	})
	wip := ghMatch(5, "bob")
	wip.HeadRef = "wip/thing"
	other := ghMatch(6, "bob")
	other.Repo = "x/y"
	st.gh.result.Matches = []provider.ScanMatch{ghMatch(1, "bob"), ghMatch(2, "Alice"), ghMatch(3, "bob"), wip, other}

	sent := st.scan()
	urls, _ := st.store.SubscriptionWatchURLs(ctx, id)
	if len(urls) != 1 || urls[0] != "https://github.com/o/r/pull/3" {
		t.Errorf("watches = %v, want only #3: #1 was stopped by hand, #2 is the worker's own, #5 and #6 are filtered", urls)
	}
	if got := getReg(t, st.store, stopped.PRURL, "sess1"); got.Status != registry.StatusStopped {
		t.Errorf("a stopped registration was revived: %+v", got)
	}
	if len(sent) != 1 || strings.Contains(sent[0], "pull/1") || strings.Contains(sent[0], "#2") {
		t.Errorf("sent %q", sent)
	}
}

func TestScanReleaseWhenUnmatchedNeedsACompleteResult(t *testing.T) {
	for _, tc := range []struct {
		name     string
		release  bool
		complete bool
		want     string
	}{
		{"kept by default", false, true, registry.StatusWatching},
		{"released", true, true, registry.StatusStopped},
		{"kept when the result is incomplete", true, false, registry.StatusWatching},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newSubTest(t, "alice")
			ctx := context.Background()
			st.subscribe(func(s *registry.Subscription) { s.ReleaseWhenUnmatched = tc.release })
			st.gh.result = provider.ScanResult{Complete: true, Matches: []provider.ScanMatch{ghMatch(1, "bob"), ghMatch(2, "bob")}}
			st.scan()
			st.gh.result = provider.ScanResult{Complete: tc.complete, Matches: []provider.ScanMatch{ghMatch(1, "bob")}}
			st.scan()
			if got := getReg(t, st.store, "https://github.com/o/r/pull/2", "sess1"); got.Status != tc.want {
				t.Errorf("#2 status = %q, want %q", got.Status, tc.want)
			}
			if got := getReg(t, st.store, "https://github.com/o/r/pull/1", "sess1"); got.Status != registry.StatusWatching {
				t.Errorf("#1 status = %q, want it still watched", got.Status)
			}
			_ = ctx
		})
	}
}

func TestScanStopsASubscriptionWhoseSessionIsGone(t *testing.T) {
	st := newSubTest(t, "alice")
	ctx := context.Background()
	id := st.subscribe(nil)
	st.gh.result.Matches = []provider.ScanMatch{ghMatch(1, "bob")}
	st.scan()
	st.sched.Sessions = alwaysGoneSessions()
	st.scan()
	sub, _, _ := st.store.GetSubscription(ctx, id)
	if sub.Status != registry.SubscriptionStopped {
		t.Errorf("status = %q, want stopped", sub.Status)
	}
	if got := getReg(t, st.store, "https://github.com/o/r/pull/1", "sess1"); got.Status != registry.StatusStopped {
		t.Errorf("derived watch status = %q, want stopped", got.Status)
	}
}

func TestScanErrorIsRecordedAndRetriedLater(t *testing.T) {
	st := newSubTest(t, "alice")
	ctx := context.Background()
	id := st.subscribe(nil)
	st.gh.err = fmt.Errorf("github is down")
	st.scan()
	sub, _, _ := st.store.GetSubscription(ctx, id)
	if !strings.Contains(sub.LastError, "github is down") || sub.Scanned {
		t.Errorf("subscription = %+v, want the error kept and the first scan still pending", sub)
	}
	if due, _ := st.store.DueSubscriptions(ctx, time.Now()); len(due) != 0 {
		t.Errorf("a failed subscription is due again at once: %+v", due)
	}
}

func TestScanSharesOneSearchBetweenIdenticalSubscriptions(t *testing.T) {
	st := newSubTest(t, "alice")
	st.subscribe(nil)
	st.subscribe(func(s *registry.Subscription) { s.SessionID = "sess2" })
	st.gh.result.Matches = []provider.ScanMatch{ghMatch(1, "bob")}
	st.scan()
	if len(st.gh.queries) != 1 {
		t.Errorf("searches = %d, want the identical query shared", len(st.gh.queries))
	}
	for _, sess := range []string{"sess1", "sess2"} {
		getReg(t, st.store, "https://github.com/o/r/pull/1", sess)
	}
}

func TestScanGitLabSubscription(t *testing.T) {
	st := newSubTest(t, "alice")
	gl := &fakeGitLabScan{result: provider.ScanResult{Complete: true, Matches: []provider.ScanMatch{
		{URL: "https://gitlab.corp.example/g/p/-/merge_requests/3", Repo: "g/p", Author: "bob", HeadRef: "feat/x"},
	}}}
	st.sched.GitLabScan = gl
	st.sched.GitLab = &fakeGitLabChecker{selfUsername: "alice"}
	st.subscribe(func(s *registry.Subscription) {
		s.Provider, s.Host, s.ScopePath, s.ScopeKind = "gitlab", "gitlab.corp.example", "g", "org"
	})
	st.scan()
	if gl.calls != 1 {
		t.Fatalf("gitlab searches = %d", gl.calls)
	}
	reg := getReg(t, st.store, "https://gitlab.corp.example/g/p/-/merge_requests/3", "sess1")
	if reg.Provider != "gitlab" || reg.Persona != registry.PersonaReviewer {
		t.Errorf("registration = %+v", reg)
	}
}

func TestTickScansDueSubscriptions(t *testing.T) {
	st := newSubTest(t, "alice")
	id := st.subscribe(nil)
	st.gh.result.Matches = []provider.ScanMatch{ghMatch(1, "bob")}
	if err := st.sched.Tick(context.Background()); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}
	sub, _, _ := st.store.GetSubscription(context.Background(), id)
	if !sub.Scanned {
		t.Errorf("subscription = %+v, want it scanned by Tick", sub)
	}
	getReg(t, st.store, "https://github.com/o/r/pull/1", "sess1")
}
