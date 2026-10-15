package provider

import "strings"

// ParseGitRemoteURL extracts (host, path) from a git remote URL in any of
// the three common forms: `git@host:path.git` (scp-like ssh), `ssh://git@host[:port]/path.git`,
// or `https://[user@]host/path.git`. path never carries a leading/trailing
// slash or a ".git" suffix. ok is false for a shape that matches none of
// these — e.g. a local filesystem path, which has no remote host to look
// anything up against. This is host-agnostic on purpose: unlike GitHub
// (always github.com), GitLab has no single fixed hostname — self-hosted
// instances are the common case here (see ParseGitLabMRURL's own any-host
// acceptance) — so a caller resolving a GitLab remote decides for itself
// whether host is actually a configured instance, e.g. by checking whether
// a credential resolves for it.
func ParseGitRemoteURL(remoteURL string) (host, path string, ok bool) {
	url := strings.TrimSpace(remoteURL)
	var rest string
	switch {
	case strings.HasPrefix(url, "https://"):
		rest = strings.TrimPrefix(url, "https://")
	case strings.HasPrefix(url, "http://"):
		rest = strings.TrimPrefix(url, "http://")
	case strings.HasPrefix(url, "ssh://"):
		rest = strings.TrimPrefix(url, "ssh://")
		if at := strings.IndexByte(rest, '@'); at != -1 {
			rest = rest[at+1:]
		}
	case strings.HasPrefix(url, "git@"):
		rest = strings.TrimPrefix(url, "git@")
		host, path, ok = strings.Cut(rest, ":")
		if !ok || host == "" || path == "" {
			return "", "", false
		}
		return host, cleanRemotePath(path), true
	default:
		return "", "", false
	}

	authority, remainder, ok := strings.Cut(rest, "/")
	if !ok {
		return "", "", false
	}
	if _, userinfoStripped, found := strings.Cut(authority, "@"); found {
		authority = userinfoStripped
	}
	host, _, _ = strings.Cut(authority, ":") // strip an explicit :port
	path = remainder
	if host == "" || path == "" {
		return "", "", false
	}
	return host, cleanRemotePath(path), true
}

func cleanRemotePath(path string) string {
	path = strings.TrimSuffix(path, ".git")
	return strings.Trim(path, "/")
}
