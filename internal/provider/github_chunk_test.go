package provider

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// chunkServer answers every aliased pullRequest in a batch request with a
// merged PR (so no REST cross-check is needed) and records how many PRs each
// request asked for. A request that asks for a PR number in poison fails the
// way GitHub fails an over-large query.
type chunkServer struct {
	*httptest.Server
	mu     sync.Mutex
	sizes  []int
	poison map[int]bool
}

func newChunkServer(t *testing.T, poison ...int) *chunkServer {
	t.Helper()
	cs := &chunkServer{poison: map[int]bool{}}
	for _, p := range poison {
		cs.poison[p] = true
	}
	cs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Variables map[string]any `json:"variables"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)

		data := map[string]map[string]any{}
		prs := 0
		failed := false
		for name, v := range req.Variables {
			if !strings.HasSuffix(name, "Number") {
				continue
			}
			prs++
			n := int(v.(float64))
			if cs.poison[n] {
				failed = true
			}
			repoAlias, prAlias, _ := strings.Cut(strings.TrimSuffix(name, "Number"), "pr")
			prAlias = "pr" + prAlias
			if data[repoAlias] == nil {
				data[repoAlias] = map[string]any{}
			}
			data[repoAlias][prAlias] = map[string]any{"state": "MERGED", "title": fmt.Sprintf("PR %d", n), "headRefOid": "x", "mergedAt": "2026-09-21T08:30:00Z"}
		}
		cs.mu.Lock()
		cs.sizes = append(cs.sizes, prs)
		cs.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if failed {
			_ = json.NewEncoder(w).Encode(map[string]any{"errors": []map[string]string{{"message": "Resource limits for this query exceeded."}}})
			return
		}
		data["rateLimit"] = nil
		out := map[string]any{"rateLimit": map[string]any{"limit": 5000, "remaining": 4000, "resetAt": "2026-09-20T23:00:00Z", "cost": 7}}
		for k, v := range data {
			if k != "rateLimit" {
				out[k] = v
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": out})
	}))
	t.Cleanup(cs.Close)
	return cs
}

func prURLs(n int) []string {
	out := make([]string, 0, n)
	for i := 1; i <= n; i++ {
		out = append(out, fmt.Sprintf("https://github.com/o/r/pull/%d", i))
	}
	return out
}

// One query for every watched PR exceeds GitHub's per-query resource limit
// once there are enough of them: the batch is split so each request stays
// small, and every PR still gets its result.
func TestCheckBatchSplitsLargeBatchesIntoSmallerQueries(t *testing.T) {
	cs := newChunkServer(t)
	c := &GitHubChecker{HTTPClient: cs.Client(), Token: "t", BaseURL: cs.URL}

	urls := prURLs(100)
	results, budgets, err := c.CheckBatch(t.Context(), urls)
	if err != nil {
		t.Fatalf("CheckBatch() error = %v", err)
	}
	if len(results) != 100 {
		t.Errorf("results = %d, want one per URL (100)", len(results))
	}
	if budgets.GraphQL.Remaining == 0 {
		t.Errorf("budgets = %+v, want the GraphQL budget carried over from the chunks", budgets)
	}
	if len(cs.sizes) < 2 {
		t.Fatalf("requests = %v, want the batch split into several", cs.sizes)
	}
	// The cost of every request counts, so the scheduler can pace by what a
	// pass really spends, not by the PR count.
	if want := 7 * len(cs.sizes); budgets.GraphQLCost != want {
		t.Errorf("GraphQLCost = %d, want %d (7 points for each of the %d requests)", budgets.GraphQLCost, want, len(cs.sizes))
	}
	for _, n := range cs.sizes {
		if n > githubBatchChunkSize {
			t.Errorf("a request asked for %d PRs, want at most %d", n, githubBatchChunkSize)
		}
	}
}

// A chunk GitHub rejects only costs its own PRs; the rest are still checked.
func TestCheckBatchOneFailingChunkDoesNotLoseTheOthers(t *testing.T) {
	cs := newChunkServer(t, 3) // PR 3 lands in the first chunk
	c := &GitHubChecker{HTTPClient: cs.Client(), Token: "t", BaseURL: cs.URL}

	urls := prURLs(100)
	results, _, err := c.CheckBatch(t.Context(), urls)
	if err != nil {
		t.Fatalf("CheckBatch() error = %v, want the other chunks' results", err)
	}
	if _, ok := results[urls[2]]; ok {
		t.Error("PR 3 has a result, want it missing from the failed chunk")
	}
	if _, ok := results[urls[99]]; !ok {
		t.Error("PR 100 is missing, want it checked despite the other chunk failing")
	}
	if len(results) < 100-githubBatchChunkSize || len(results) >= 100 {
		t.Errorf("results = %d, want every PR but those of the failed chunk", len(results))
	}
}

// With every chunk failing there is nothing to report but the error.
func TestCheckBatchReturnsTheErrorWhenEveryChunkFails(t *testing.T) {
	var all []int
	for i := 1; i <= 60; i++ {
		all = append(all, i)
	}
	cs := newChunkServer(t, all...)
	c := &GitHubChecker{HTTPClient: cs.Client(), Token: "t", BaseURL: cs.URL}
	if _, _, err := c.CheckBatch(t.Context(), prURLs(60)); err == nil {
		t.Fatal("CheckBatch() error = nil, want the failure when no chunk succeeded")
	}
}
