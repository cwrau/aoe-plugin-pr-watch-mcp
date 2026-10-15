package main

import (
	"context"
	"os/exec"
	"slices"
	"testing"
)

// gitCheckoutWithRemote creates a bare-bones git checkout with origin set
// to remoteURL and its current branch pinned to a deterministic name (an
// empty repo's default init branch depends on local git config, so this
// avoids that leaking into test assertions), so
// gitRemoteOrigin/gitRemoteResolver can be exercised against real `git`
// without touching any real repo.
func gitCheckoutWithRemote(t *testing.T, remoteURL string) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "resolver-test-branch", dir},
		{"-C", dir, "remote", "add", "origin", remoteURL},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Skipf("git %v failed (%v): %s — skipping, git not usable in this environment", args, err, out)
		}
	}
	return dir
}

func TestGitRemoteOriginReadsConfiguredRemote(t *testing.T) {
	dir := gitCheckoutWithRemote(t, "git@github.com:cwrau/aoe-plugin-pr-watch-mcp.git")
	got, err := gitRemoteOrigin(context.Background(), dir)
	if err != nil {
		t.Fatalf("gitRemoteOrigin() error = %v", err)
	}
	if got != "git@github.com:cwrau/aoe-plugin-pr-watch-mcp.git" {
		t.Errorf("gitRemoteOrigin() = %q, want the configured remote verbatim", got)
	}
}

func TestGitRemoteOriginErrorsWithoutARemote(t *testing.T) {
	dir := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", dir).CombinedOutput(); err != nil {
		t.Skipf("git init failed (%v): %s — skipping, git not usable in this environment", err, out)
	}
	if _, err := gitRemoteOrigin(context.Background(), dir); err == nil {
		t.Error("gitRemoteOrigin() error = nil, want an error for a checkout with no origin remote")
	}
}

func TestGitRemoteResolverResolvesGitHubHost(t *testing.T) {
	dir := gitCheckoutWithRemote(t, "git@github.com:cwrau/aoe-plugin-pr-watch-mcp.git")
	r := &gitRemoteResolver{glabTokenFunc: func(context.Context, string) (string, error) { return "", nil }}

	prov, remoteKey, branch, ok := r.Resolve(context.Background(), dir)
	if !ok || prov != "github" || remoteKey != "cwrau/aoe-plugin-pr-watch-mcp" || branch != "resolver-test-branch" {
		t.Errorf("Resolve() = (%q, %q, %q, %v), want (github, cwrau/aoe-plugin-pr-watch-mcp, resolver-test-branch, true)", prov, remoteKey, branch, ok)
	}
}

func TestGitRemoteResolverResolvesConfiguredGitLabHost(t *testing.T) {
	dir := gitCheckoutWithRemote(t, "https://gitlab.corp.example/team/project.git")
	r := &gitRemoteResolver{glabTokenFunc: func(ctx context.Context, host string) (string, error) {
		if host != "gitlab.corp.example" {
			t.Errorf("glabTokenFunc host = %q, want gitlab.corp.example", host)
		}
		return "glab-token", nil
	}}

	prov, remoteKey, branch, ok := r.Resolve(context.Background(), dir)
	if !ok || prov != "gitlab" || remoteKey != "gitlab.corp.example/team/project" || branch != "resolver-test-branch" {
		t.Errorf("Resolve() = (%q, %q, %q, %v), want (gitlab, gitlab.corp.example/team/project, resolver-test-branch, true)", prov, remoteKey, branch, ok)
	}
}

func TestGitRemoteResolverRejectsUnconfiguredHost(t *testing.T) {
	dir := gitCheckoutWithRemote(t, "https://some-other-forge.example.com/team/project.git")
	r := &gitRemoteResolver{glabTokenFunc: func(context.Context, string) (string, error) { return "", nil }}

	_, _, _, ok := r.Resolve(context.Background(), dir)
	if ok {
		t.Error("Resolve() ok = true for a host with no resolvable glab credential, want false")
	}
}

func TestGitRemoteResolverRejectsCheckoutWithNoRemote(t *testing.T) {
	dir := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", dir).CombinedOutput(); err != nil {
		t.Skipf("git init failed (%v): %s — skipping, git not usable in this environment", err, out)
	}
	r := &gitRemoteResolver{glabTokenFunc: func(context.Context, string) (string, error) { return "token", nil }}

	_, _, _, ok := r.Resolve(context.Background(), dir)
	if ok {
		t.Error("Resolve() ok = true for a checkout with no origin remote, want false")
	}
}

func TestGitRemoteResolverRejectsDetachedHEAD(t *testing.T) {
	dir := gitCheckoutWithRemote(t, "git@github.com:cwrau/aoe-plugin-pr-watch-mcp.git")
	if out, err := exec.Command("git", "-C", dir, "commit", "--allow-empty", "-q", "-m", "init").CombinedOutput(); err != nil {
		t.Skipf("git commit failed (%v): %s — skipping, git not usable in this environment", err, out)
	}
	if out, err := exec.Command("git", "-C", dir, "checkout", "-q", "--detach", "HEAD").CombinedOutput(); err != nil {
		t.Skipf("git checkout --detach failed (%v): %s — skipping, git not usable in this environment", err, out)
	}
	r := &gitRemoteResolver{glabTokenFunc: func(context.Context, string) (string, error) { return "", nil }}

	_, _, _, ok := r.Resolve(context.Background(), dir)
	if ok {
		t.Error("Resolve() ok = true for a detached HEAD, want false (nothing to look a PR up against)")
	}
}

// TestGitRemoteResolverCachesGitLabTokenProbe is a regression test:
// confirmed live — glab config get token cost ~300ms warm (~2s cold) per
// call, and Resolve was calling it once per checkout on every single
// tick, unconditionally. A repeat Resolve for the same source_path must
// reuse the cached identity instead of re-probing.
func TestGitRemoteResolverCachesGitLabTokenProbe(t *testing.T) {
	dir := gitCheckoutWithRemote(t, "https://gitlab.corp.example/team/project.git")
	var probes int
	r := &gitRemoteResolver{glabTokenFunc: func(context.Context, string) (string, error) {
		probes++
		return "glab-token", nil
	}}

	for range 3 {
		_, _, _, ok := r.Resolve(context.Background(), dir)
		if !ok {
			t.Fatalf("Resolve() ok = false, want true")
		}
	}
	if probes != 1 {
		t.Errorf("glabTokenFunc calls = %d, want 1 (cached after the first Resolve)", probes)
	}
}

// Many checkouts live on the same GitLab host: whether a credential exists
// for it is one answer, so it is probed once per host, not once per checkout.
func TestGitRemoteResolverProbesTheGitLabCredentialOncePerHost(t *testing.T) {
	var probes int
	r := &gitRemoteResolver{glabTokenFunc: func(context.Context, string) (string, error) {
		probes++
		return "glab-token", nil
	}}
	for range 3 {
		dir := gitCheckoutWithRemote(t, "https://gitlab.corp.example/team/project.git")
		if _, _, _, ok := r.Resolve(context.Background(), dir); !ok {
			t.Fatalf("Resolve() ok = false, want true")
		}
	}
	if probes != 1 {
		t.Errorf("glabTokenFunc calls = %d, want 1 for three checkouts on one host", probes)
	}

	other := gitCheckoutWithRemote(t, "https://gitlab.example.org/team/project.git")
	if _, _, _, ok := r.Resolve(context.Background(), other); !ok {
		t.Fatalf("Resolve() ok = false for the second host, want true")
	}
	if probes != 2 {
		t.Errorf("glabTokenFunc calls = %d, want 2 after a checkout on a different host", probes)
	}
}

// TestGitRemoteResolverCachesNegativeResolution guards the same caching
// for an unconfigured host — repeatedly probing a host that will never
// answer differently is exactly the waste this cache exists to avoid.
func TestGitRemoteResolverCachesNegativeResolution(t *testing.T) {
	dir := gitCheckoutWithRemote(t, "https://not-configured.example.com/team/project.git")
	var probes int
	r := &gitRemoteResolver{glabTokenFunc: func(context.Context, string) (string, error) {
		probes++
		return "", nil
	}}

	for range 3 {
		if _, _, _, ok := r.Resolve(context.Background(), dir); ok {
			t.Fatalf("Resolve() ok = true, want false for an unconfigured host")
		}
	}
	if probes != 1 {
		t.Errorf("glabTokenFunc calls = %d, want 1 (the negative result is cached too)", probes)
	}
}

// TestGitRemoteResolverBranchStaysLiveAcrossCachedIdentity guards against
// caching too much: the remote identity is cached, but the branch must
// always be read fresh, since (unlike the remote) it changes often — that
// was the whole point of resolving it here instead of trusting aoe's own
// cached branch metadata.
func TestGitRemoteResolverBranchStaysLiveAcrossCachedIdentity(t *testing.T) {
	dir := gitCheckoutWithRemote(t, "git@github.com:cwrau/aoe-plugin-pr-watch-mcp.git")
	r := &gitRemoteResolver{glabTokenFunc: func(context.Context, string) (string, error) { return "", nil }}

	_, _, branch, ok := r.Resolve(context.Background(), dir)
	if !ok || branch != "resolver-test-branch" {
		t.Fatalf("Resolve() branch = %q, ok = %v, want (resolver-test-branch, true)", branch, ok)
	}

	if out, err := exec.Command("git", "-C", dir, "checkout", "-q", "-b", "a-new-branch").CombinedOutput(); err != nil {
		t.Skipf("git checkout -b failed (%v): %s — skipping, git not usable in this environment", err, out)
	}

	_, _, branch, ok = r.Resolve(context.Background(), dir)
	if !ok || branch != "a-new-branch" {
		t.Errorf("Resolve() branch = %q, ok = %v, want (a-new-branch, true) — branch must never come from the identity cache", branch, ok)
	}
}

// commitIn makes one empty commit (identity and signing pinned so the test
// doesn't depend on the host's git config), giving HEAD something for a
// remote-tracking ref to point at.
func commitIn(t *testing.T, dir, msg string) {
	t.Helper()
	out, err := exec.Command("git", "-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-q", "-m", msg).CombinedOutput()
	if err != nil {
		t.Skipf("git commit failed (%v): %s — skipping, git not usable in this environment", err, out)
	}
}

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
		t.Skipf("git %v failed (%v): %s — skipping, git not usable in this environment", args, err, out)
	}
}

func resolveBranch(t *testing.T, dir string) string {
	t.Helper()
	r := &gitRemoteResolver{glabTokenFunc: func(context.Context, string) (string, error) { return "", nil }}
	_, _, branch, ok := r.Resolve(context.Background(), dir)
	if !ok {
		t.Fatalf("Resolve() ok = false, want true")
	}
	return branch
}

// TestGitRemoteResolverFollowsRenamedPushedRef is a regression test:
// confirmed live, a worktree on local branch armenians-iv was pushed to the
// fork as fix/web/sidebar-swipe-horizontal-scroll (PR #4293) with a full
// refspec, so no upstream was configured — discovery kept looking up the
// local name and never found the PR. Git still leaves a remote-tracking ref
// at HEAD for the pushed name; that's the branch the PR actually has.
func TestGitRemoteResolverFollowsRenamedPushedRef(t *testing.T) {
	dir := gitCheckoutWithRemote(t, "git@github.com:upstream/repo.git")
	commitIn(t, dir, "init")
	gitIn(t, dir, "remote", "add", "fork", "git@github.com:me/repo.git")
	gitIn(t, dir, "update-ref", "refs/remotes/fork/fix/web/renamed", "HEAD")

	if got := resolveBranch(t, dir); got != "fix/web/renamed" {
		t.Errorf("Resolve() branch = %q, want the pushed ref name fix/web/renamed", got)
	}
}

func TestGitRemoteResolverKeepsLocalNameWhenPushedUnderSameName(t *testing.T) {
	dir := gitCheckoutWithRemote(t, "git@github.com:upstream/repo.git")
	commitIn(t, dir, "init")
	gitIn(t, dir, "remote", "add", "fork", "git@github.com:me/repo.git")
	gitIn(t, dir, "update-ref", "refs/remotes/fork/resolver-test-branch", "HEAD")
	gitIn(t, dir, "update-ref", "refs/remotes/fork/other-name", "HEAD")

	if got := resolveBranch(t, dir); got != "resolver-test-branch" {
		t.Errorf("Resolve() branch = %q, want the local name when it was pushed as-is", got)
	}
}

// TestGitRemoteResolverIgnoresDefaultBranchRef guards the obvious trap: a
// fresh branch with no commits of its own sits at the same commit as
// origin/main, so origin/main "points at HEAD" too — and a PR lookup by head
// ref `main` would be nonsense.
func TestGitRemoteResolverIgnoresDefaultBranchRef(t *testing.T) {
	dir := gitCheckoutWithRemote(t, "git@github.com:upstream/repo.git")
	commitIn(t, dir, "init")
	gitIn(t, dir, "update-ref", "refs/remotes/origin/develop", "HEAD")
	gitIn(t, dir, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/develop")
	gitIn(t, dir, "update-ref", "refs/remotes/origin/main", "HEAD")

	if got := resolveBranch(t, dir); got != "resolver-test-branch" {
		t.Errorf("Resolve() branch = %q, want the local name, not the default branch / main", got)
	}
}

func TestGitRemoteResolverIgnoresStaleRemoteRef(t *testing.T) {
	dir := gitCheckoutWithRemote(t, "git@github.com:upstream/repo.git")
	commitIn(t, dir, "init")
	gitIn(t, dir, "remote", "add", "fork", "git@github.com:me/repo.git")
	gitIn(t, dir, "update-ref", "refs/remotes/fork/old-push", "HEAD")
	commitIn(t, dir, "newer local work")

	if got := resolveBranch(t, dir); got != "resolver-test-branch" {
		t.Errorf("Resolve() branch = %q, want the local name when no remote ref is at HEAD", got)
	}
}

func gitAddRemote(t *testing.T, dir, name, url string) {
	t.Helper()
	if out, err := exec.Command("git", "-C", dir, "remote", "add", name, url).CombinedOutput(); err != nil {
		t.Skipf("git remote add failed (%v): %s", err, out)
	}
}

// A fork checkout: origin is the fork, upstream the real repo. The PR is
// opened in upstream, so it is named as an extra repo to probe, and both
// owners are the heads a PR found there may come from.
func TestGitRemoteResolverExtraRemotesNamesUpstreamOfAFork(t *testing.T) {
	dir := gitCheckoutWithRemote(t, "git@github.com:me/r.git")
	gitAddRemote(t, dir, "upstream", "https://github.com/Up/r")
	gitAddRemote(t, dir, "mirror", "git@github.com:me/r.git") // same repo as origin: nothing to add
	gitAddRemote(t, dir, "elsewhere", "git@gitlab.example.com:g/p.git")

	r := &gitRemoteResolver{glabTokenFunc: func(context.Context, string) (string, error) { return "", nil }}
	keys, owners := r.ExtraRemotes(context.Background(), dir)
	if !slices.Equal(keys, []string{"Up/r"}) {
		t.Errorf("keys = %v, want only [Up/r] (not origin's repo again, not a non-GitHub remote)", keys)
	}
	if !slices.Equal(owners, []string{"me", "Up"}) {
		t.Errorf("owners = %v, want the GitHub owners of all remotes [me Up]", owners)
	}
}

func TestGitRemoteResolverExtraRemotesEmptyWithOnlyOrigin(t *testing.T) {
	dir := gitCheckoutWithRemote(t, "git@github.com:o/r.git")
	r := &gitRemoteResolver{glabTokenFunc: func(context.Context, string) (string, error) { return "", nil }}
	if keys, owners := r.ExtraRemotes(context.Background(), dir); len(keys) != 0 || len(owners) != 0 {
		t.Errorf("keys, owners = %v, %v, want none for a checkout with a single remote", keys, owners)
	}
}

func TestGitRemoteResolverExtraRemotesIgnoresGitLabCheckouts(t *testing.T) {
	dir := gitCheckoutWithRemote(t, "git@gitlab.example.com:g/p.git")
	gitAddRemote(t, dir, "upstream", "https://github.com/up/r")
	r := &gitRemoteResolver{glabTokenFunc: func(context.Context, string) (string, error) { return "tok", nil }}
	if keys, _ := r.ExtraRemotes(context.Background(), dir); len(keys) != 0 {
		t.Errorf("keys = %v, want none: extra remotes are only probed for a GitHub origin", keys)
	}
}

// A checkout's config can name a remote anything, including text git would
// parse as an option. Those names must never reach a git command line.
func TestGitRemoteNamesSkipsOptionLikeNames(t *testing.T) {
	dir := gitCheckoutWithRemote(t, "https://github.com/me/repo.git")
	if out, err := exec.Command("git", "-C", dir, "config", "remote.--all.url", "https://github.com/up/stream.git").CombinedOutput(); err != nil {
		t.Skipf("git config failed (%v): %s", err, out)
	}
	got := gitRemoteNames(context.Background(), dir)
	if len(got) != 1 || got[0] != "origin" {
		t.Errorf("gitRemoteNames() = %v, want [origin]", got)
	}
}
