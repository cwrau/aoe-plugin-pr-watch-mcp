package provider

import "testing"

func TestParseGitRemoteURLSCPLike(t *testing.T) {
	host, path, ok := ParseGitRemoteURL("git@github.com:cwrau/aoe-plugin-pr-watch-mcp.git")
	if !ok || host != "github.com" || path != "cwrau/aoe-plugin-pr-watch-mcp" {
		t.Errorf("ParseGitRemoteURL() = (%q, %q, %v), want (github.com, cwrau/aoe-plugin-pr-watch-mcp, true)", host, path, ok)
	}
}

func TestParseGitRemoteURLHTTPS(t *testing.T) {
	host, path, ok := ParseGitRemoteURL("https://gitlab.corp.example/team/project.git")
	if !ok || host != "gitlab.corp.example" || path != "team/project" {
		t.Errorf("ParseGitRemoteURL() = (%q, %q, %v), want (gitlab.corp.example, team/project, true)", host, path, ok)
	}
}

func TestParseGitRemoteURLHTTPSNestedGroupNoSuffix(t *testing.T) {
	host, path, ok := ParseGitRemoteURL("https://gitlab.corp.example/group/sub/sub2/cluster/project")
	if !ok || host != "gitlab.corp.example" || path != "group/sub/sub2/cluster/project" {
		t.Errorf("ParseGitRemoteURL() = (%q, %q, %v), want the full nested group path preserved", host, path, ok)
	}
}

func TestParseGitRemoteURLSSHScheme(t *testing.T) {
	host, path, ok := ParseGitRemoteURL("ssh://git@github.com/o/r.git")
	if !ok || host != "github.com" || path != "o/r" {
		t.Errorf("ParseGitRemoteURL() = (%q, %q, %v), want (github.com, o/r, true)", host, path, ok)
	}
}

func TestParseGitRemoteURLSSHSchemeWithPort(t *testing.T) {
	host, path, ok := ParseGitRemoteURL("ssh://git@gitlab.corp.example:2222/team/project.git")
	if !ok || host != "gitlab.corp.example" || path != "team/project" {
		t.Errorf("ParseGitRemoteURL() = (%q, %q, %v), want the :port stripped from host", host, path, ok)
	}
}

func TestParseGitRemoteURLRejectsLocalPath(t *testing.T) {
	if _, _, ok := ParseGitRemoteURL("/home/user/some/local/repo"); ok {
		t.Error("ParseGitRemoteURL() ok = true for a local filesystem path, want false")
	}
}

func TestParseGitHubRemoteURLAcceptsGitHubHost(t *testing.T) {
	owner, repo, ok := ParseGitHubRemoteURL("git@github.com:cwrau/aoe-plugin-pr-watch-mcp.git")
	if !ok || owner != "cwrau" || repo != "aoe-plugin-pr-watch-mcp" {
		t.Errorf("ParseGitHubRemoteURL() = (%q, %q, %v), want (cwrau, aoe-plugin-pr-watch-mcp, true)", owner, repo, ok)
	}
}

func TestParseGitHubRemoteURLRejectsNonGitHubHost(t *testing.T) {
	if _, _, ok := ParseGitHubRemoteURL("https://gitlab.corp.example/team/project.git"); ok {
		t.Error("ParseGitHubRemoteURL() ok = true for a non-github.com host, want false")
	}
}
