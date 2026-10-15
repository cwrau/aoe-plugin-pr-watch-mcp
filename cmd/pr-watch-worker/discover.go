package main

import (
	"context"
	"os/exec"
	"slices"
	"strings"
	"sync"

	"github.com/cwrau/aoe-plugin-pr-watch-mcp/internal/provider"
)

// remoteIdentity is one checkout's resolved provider identity — cached by
// gitRemoteResolver per source_path, since a checkout's own remote (unlike
// its branch) essentially never changes for the lifetime of a worktree.
type remoteIdentity struct {
	provider  string
	remoteKey string
	ok        bool // false caches a negative resolution too (no origin, non-git-forge remote, or unconfigured glab host) — see Resolve's doc comment
}

// gitRemoteResolver implements scheduler.RemoteResolver: shells `git remote
// get-url origin` for the checkout at sourcePath (local, no rate-limit
// cost — same category as aoe's own workspace-repo discovery this worker
// already relies on for the checkout list itself), then classifies the
// remote. An exact github.com host is GitHub; anything else is only
// treated as GitLab if glabTokenFunc actually resolves a credential for
// it — there's no way to recognize an arbitrary self-hosted GitLab
// instance purely from its URL shape (unlike github.com), so "is a
// credential configured for this host" is the same signal
// provider.GitLabChecker.TokenFunc already uses to decide a host is real,
// reused here rather than duplicated.
//
// The resolved (provider, remoteKey) is cached per source_path for the
// worker's whole lifetime — confirmed live that without this, discoverTick
// re-ran `glab config get token` (a real subprocess, ~300ms warm, ~2s
// cold — nothing like a plain `git` call) once per GitLab checkout on
// *every* tick, even though the same handful of hosts answer identically
// every time; 16 checkouts on one host alone cost ~5s of pure sequential
// subprocess spawning per 15s tick before this cache existed. The
// tradeoff: a checkout whose `origin` remote is later changed by hand (or
// a host that only gets a glab credential configured after this worker
// already cached "not configured" for it) won't be picked up without a
// worker restart — deliberate, since remotes essentially never change for
// a worktree's lifetime in practice, same "resolved once, cached forever"
// call already made for provider.GitHubChecker.SelfLogin /
// GitLabChecker.SelfUsername.
type gitRemoteResolver struct {
	glabTokenFunc func(ctx context.Context, host string) (string, error)

	mu     sync.Mutex
	cache  map[string]remoteIdentity
	extras map[string]extraRemotes
	// hasCredential caches, per host, whether glabTokenFunc found a
	// credential: many checkouts share a host, and each probe forks glab.
	hasCredential map[string]bool
}

// extraRemotes is ExtraRemotes' cached answer for one checkout.
type extraRemotes struct {
	keys   []string
	owners []string
}

// Resolve reads the checkout's current branch live every call (cheap, and
// — unlike the remote — genuinely changes often, see gitCurrentBranch's
// doc comment on aoe's own branch metadata going stale), but only resolves
// the remote identity itself on a cache miss.
func (r *gitRemoteResolver) Resolve(ctx context.Context, sourcePath string) (prov, remoteKey, branch string, ok bool) {
	branch, err := gitCurrentBranch(ctx, sourcePath)
	if err != nil || branch == "" {
		return "", "", "", false // empty on a detached HEAD — nothing to look a PR/MR up against
	}

	id, hit := r.cached(sourcePath)
	if !hit {
		id = r.resolveIdentity(ctx, sourcePath)
		r.store(sourcePath, id)
	}
	if !id.ok {
		return "", "", "", false
	}
	return id.provider, id.remoteKey, pushedBranchName(ctx, sourcePath, branch), true
}

// pushedBranchName is the head-ref name a PR/MR for the checkout would
// carry. Usually that's just the local branch name, but a branch pushed
// under a different name (confirmed live: local armenians-iv pushed to the
// fork as fix/web/sidebar-swipe-horizontal-scroll, PR #4293) has a PR whose
// head ref is the pushed name — a lookup by the local name never finds it.
// Git leaves a remote-tracking ref at HEAD for whatever name was pushed,
// even with a full refspec and no upstream configured, so any such ref is
// the answer. The local name wins whenever it was pushed as-is. Refs on the
// default branch (and main/master) are ignored: a branch with no commits of
// its own shares HEAD with origin's default branch, and a lookup by that
// name is meaningless. The local name is the fallback when nothing matches,
// including once unpushed local commits move HEAD past the pushed ref.
func pushedBranchName(ctx context.Context, sourcePath, local string) string {
	out, err := exec.CommandContext(ctx, "git", "-C", sourcePath, "for-each-ref", "--points-at", "HEAD", "--format=%(refname)", "refs/remotes/").Output()
	if err != nil {
		return local
	}
	refs := strings.Fields(string(out))
	if len(refs) == 0 {
		return local
	}

	remotes := gitRemoteNames(ctx, sourcePath)
	skip := map[string]bool{"main": true, "master": true}
	if def, err := exec.CommandContext(ctx, "git", "-C", sourcePath, "symbolic-ref", "--short", "refs/remotes/origin/HEAD").Output(); err == nil {
		skip[strings.TrimPrefix(strings.TrimSpace(string(def)), "origin/")] = true
	}

	var candidates []string
	for _, ref := range refs {
		name, ok := strings.CutPrefix(ref, "refs/remotes/")
		if !ok {
			continue
		}
		for _, remote := range remotes {
			if branch, ok := strings.CutPrefix(name, remote+"/"); ok && branch != "HEAD" && !skip[branch] {
				if branch == local {
					return local
				}
				candidates = append(candidates, branch)
				break
			}
		}
	}
	if len(candidates) == 0 {
		return local
	}
	slices.Sort(candidates)
	return candidates[0]
}

func gitRemoteNames(ctx context.Context, sourcePath string) []string {
	out, err := exec.CommandContext(ctx, "git", "-C", sourcePath, "remote").Output()
	if err != nil {
		return nil
	}
	return strings.Fields(string(out))
}

// ExtraRemotes implements scheduler.ForkRemoteResolver. A fork checkout
// pushes to origin (the fork) and opens its PR in a different remote's repo
// (typically named upstream), so for a GitHub origin it names the GitHub
// repos of the checkout's other remotes, plus the owners of every GitHub
// remote including origin: the only head repos a PR found in one of those
// other repos may come from. GitLab checkouts get nothing. Cached per
// checkout for the worker's lifetime like the origin identity, for the same
// reason (a worktree's remotes essentially never change, and each lookup is
// a subprocess).
func (r *gitRemoteResolver) ExtraRemotes(ctx context.Context, sourcePath string) ([]string, []string) {
	r.mu.Lock()
	cached, hit := r.extras[sourcePath]
	r.mu.Unlock()
	if hit {
		return cached.keys, cached.owners
	}

	var found extraRemotes
	originURL, err := gitRemoteOrigin(ctx, sourcePath)
	if err == nil {
		if owner, repo, ok := provider.ParseGitHubRemoteURL(originURL); ok {
			originKey := owner + "/" + repo
			found.owners = []string{owner}
			for _, name := range gitRemoteNames(ctx, sourcePath) {
				if name == "origin" {
					continue
				}
				out, err := exec.CommandContext(ctx, "git", "-C", sourcePath, "remote", "get-url", name).Output()
				if err != nil {
					continue
				}
				o, rp, ok := provider.ParseGitHubRemoteURL(strings.TrimSpace(string(out)))
				if !ok {
					continue
				}
				if !slices.ContainsFunc(found.owners, func(x string) bool { return strings.EqualFold(x, o) }) {
					found.owners = append(found.owners, o)
				}
				key := o + "/" + rp
				if !strings.EqualFold(key, originKey) && !slices.ContainsFunc(found.keys, func(x string) bool { return strings.EqualFold(x, key) }) {
					found.keys = append(found.keys, key)
				}
			}
		}
	}
	if len(found.keys) == 0 {
		found.owners = nil // nothing extra to probe, so nothing to filter
	}

	r.mu.Lock()
	if r.extras == nil {
		r.extras = make(map[string]extraRemotes)
	}
	r.extras[sourcePath] = found
	r.mu.Unlock()
	return found.keys, found.owners
}

func (r *gitRemoteResolver) resolveIdentity(ctx context.Context, sourcePath string) remoteIdentity {
	remoteURL, err := gitRemoteOrigin(ctx, sourcePath)
	if err != nil || remoteURL == "" {
		return remoteIdentity{}
	}
	if owner, repo, ok := provider.ParseGitHubRemoteURL(remoteURL); ok {
		return remoteIdentity{provider: "github", remoteKey: owner + "/" + repo, ok: true}
	}
	host, path, ok := provider.ParseGitRemoteURL(remoteURL)
	if !ok {
		return remoteIdentity{}
	}
	if !r.credentialFor(ctx, host) {
		return remoteIdentity{}
	}
	return remoteIdentity{provider: "gitlab", remoteKey: host + "/" + path, ok: true}
}

// credentialFor reports whether a GitLab credential is configured for host,
// asking glabTokenFunc once per host.
func (r *gitRemoteResolver) credentialFor(ctx context.Context, host string) bool {
	r.mu.Lock()
	known, hit := r.hasCredential[host]
	r.mu.Unlock()
	if hit {
		return known
	}
	token, err := r.glabTokenFunc(ctx, host)
	has := err == nil && token != ""
	r.mu.Lock()
	if r.hasCredential == nil {
		r.hasCredential = make(map[string]bool)
	}
	r.hasCredential[host] = has
	r.mu.Unlock()
	return has
}

func (r *gitRemoteResolver) cached(sourcePath string) (remoteIdentity, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id, ok := r.cache[sourcePath]
	return id, ok
}

func (r *gitRemoteResolver) store(sourcePath string, id remoteIdentity) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cache == nil {
		r.cache = make(map[string]remoteIdentity)
	}
	r.cache[sourcePath] = id
}

// gitRemoteOrigin shells `git -C sourcePath remote get-url origin`, bounded
// by ctx same as every other exec.CommandContext this worker runs.
func gitRemoteOrigin(ctx context.Context, sourcePath string) (string, error) {
	out, err := exec.CommandContext(ctx, "git", "-C", sourcePath, "remote", "get-url", "origin").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// gitCurrentBranch shells `git -C sourcePath branch --show-current`, which
// (unlike `rev-parse --abbrev-ref HEAD`) reports an empty string on a
// detached HEAD instead of the literal text "HEAD", so the caller doesn't
// need to special-case that.
func gitCurrentBranch(ctx context.Context, sourcePath string) (string, error) {
	out, err := exec.CommandContext(ctx, "git", "-C", sourcePath, "branch", "--show-current").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
