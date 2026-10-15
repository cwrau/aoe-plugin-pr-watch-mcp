package main

import (
	"encoding/json"
	"testing"

	"github.com/cwrau/aoe-plugin-pr-watch-mcp/internal/aoeclient"
)

func TestLifecycleHandlerKeepsTheDirectoryCurrent(t *testing.T) {
	c := aoeclient.New(nil)
	handle := lifecycleHandler(c, testLogger())

	handle("created", "a", json.RawMessage(`{"id":"a","title":"New","project_path":"/w/a","worktree":{"branch":"feat"}}`))
	if c.Classify("a") != aoeclient.SessionLive || c.Title("a") != "New" {
		t.Fatal("a created event did not add the session")
	}
	if repos := c.WorkspaceRepos("a"); len(repos) != 1 || repos[0].Branch != "feat" {
		t.Errorf("WorkspaceRepos(a) = %+v, want the worktree root from the summary", repos)
	}

	handle("trashed", "a", json.RawMessage(`{"id":"a","trashed":true}`))
	if c.Classify("a") != aoeclient.SessionGone {
		t.Error("a trashed event did not mark the session gone")
	}

	handle("deleted", "a", nil)
	if c.Title("a") != "" {
		t.Error("a deleted event did not remove the session")
	}
}

func TestLifecycleHandlerIgnoresAnUnparseableSummary(t *testing.T) {
	c := aoeclient.New(nil)
	handle := lifecycleHandler(c, testLogger())
	handle("created", "a", json.RawMessage(`"not an object"`))
	if c.Classify("a") != aoeclient.SessionGone {
		t.Error("a malformed summary created a session, want it ignored")
	}
}
