package main

import (
	"context"

	"github.com/cwrau/aoe-plugin-pr-watch-mcp/internal/registry"
)

// gitEmailFor is the git email to try as the Bitbucket Basic username for a
// request about workspace/repo. Git identities are scoped per directory, so it
// is read from a checkout of that repository (one a session has open), not
// from whatever identity the worker itself runs under, which is only the last
// resort, for a repository no known checkout holds.
func gitEmailFor(ctx context.Context, store *registry.Store, workspace, repo string) string {
	ctx, cancel := context.WithTimeout(ctx, tokenTimeout)
	defer cancel()
	if workspace != "" && repo != "" {
		if paths, err := store.AutoDiscoveryPathsForRemote(ctx, "bitbucket", workspace+"/"+repo); err == nil {
			if email := emailFromCheckouts(ctx, paths); email != "" {
				return email
			}
		}
	}
	email, err := runToken(ctx, "git", "config", "--get", "user.email")
	if err != nil {
		return ""
	}
	return email
}

// emailFromCheckouts returns the first non-empty user.email git reports for
// the given checkouts, each read with that checkout's own config (so its
// directory-scoped identity applies). A path that is not a git checkout is
// skipped.
func emailFromCheckouts(ctx context.Context, paths []string) string {
	for _, path := range paths {
		if email, err := runToken(ctx, "git", "-C", path, "config", "--get", "user.email"); err == nil && email != "" {
			return email
		}
	}
	return ""
}
