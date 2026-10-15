package provider

import "fmt"

// ShortRef derives a compact PR/MR reference (e.g. "owner/repo#42" or
// "team/project!17") from a registration's own URL and provider — no extra
// API call, just the same parsers that already validate the URL shape at
// registration time. Shared by the badge/pane labels and the scheduler's
// notification text, so a PR/MR reads identically wherever it's named.
// Falls back to the raw URL if it doesn't parse as either shape.
func ShortRef(prURL, prov string) string {
	switch prov {
	case "github":
		if owner, repo, number, err := ParseGitHubPRURL(prURL); err == nil {
			return fmt.Sprintf("%s/%s#%d", owner, repo, number)
		}
	case "gitlab":
		if _, path, iid, err := ParseGitLabMRURL(prURL); err == nil {
			return fmt.Sprintf("%s!%d", path, iid)
		}
	}
	return prURL
}
