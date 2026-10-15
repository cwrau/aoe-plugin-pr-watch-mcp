package provider

import "testing"

func TestShortRefGitHub(t *testing.T) {
	got := ShortRef("https://github.com/cwrau/aoe-plugin-pr-watch-mcp/pull/42", "github")
	if got != "cwrau/aoe-plugin-pr-watch-mcp#42" {
		t.Errorf("ShortRef() = %q, want %q", got, "cwrau/aoe-plugin-pr-watch-mcp#42")
	}
}

func TestShortRefGitLab(t *testing.T) {
	got := ShortRef("https://gitlab.corp.example/team/project/-/merge_requests/17", "gitlab")
	if got != "team/project!17" {
		t.Errorf("ShortRef() = %q, want %q", got, "team/project!17")
	}
}

func TestShortRefFallsBackToURLWhenUnparseable(t *testing.T) {
	url := "https://example.com/not-a-real-pr-url"
	if got := ShortRef(url, "github"); got != url {
		t.Errorf("ShortRef() = %q, want the raw url %q as a fallback", got, url)
	}
}
