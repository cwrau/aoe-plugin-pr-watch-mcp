package provider

import (
	"slices"
	"strings"
)

// TopLevelComment is one PR/MR conversation comment as a provider fetched
// it, before any user filter rules apply.
type TopLevelComment struct {
	ID     string
	Href   string
	Author string
	Body   string
}

// CommentFilter drops a top-level comment from what a registration tracks.
// Author, if set, must match (case-insensitive equality, ignoring a trailing
// "[bot]"). Contains, if it has any non-blank entry, must match too: the body
// must contain at least one of the entries (case-insensitive substring) —
// one rule can list several texts for one author without repeating it. A
// rule with neither an author nor a non-blank text matches nothing, so a
// half-filled settings row can never mute everything.
type CommentFilter struct {
	Author   string
	Contains []string
}

func normalizeAuthor(a string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(a)), "[bot]")
}

func (f CommentFilter) matches(c TopLevelComment) bool {
	author := normalizeAuthor(f.Author)
	var texts []string
	for _, t := range f.Contains {
		if t = strings.ToLower(strings.TrimSpace(t)); t != "" {
			texts = append(texts, t)
		}
	}
	if author == "" && len(texts) == 0 {
		return false
	}
	if author != "" && normalizeAuthor(c.Author) != author {
		return false
	}
	if len(texts) > 0 {
		body := strings.ToLower(c.Body)
		if !slices.ContainsFunc(texts, func(t string) bool { return strings.Contains(body, t) }) {
			return false
		}
	}
	return true
}

// ApplyCommentFilters derives TopLevelCommentIDs, LatestCommentHref and
// LatestCommentAuthor from the raw TopLevelComments, leaving out every
// comment a rule matches. It runs against the comments' *current* bodies on
// every poll, on purpose: a bot like CodeRabbit edits its "review in
// progress" placeholder into the real review, so a filtered comment must
// start counting the moment an edit stops it matching — it was never stored
// as seen, so it then reads as new. It builds fresh slices rather than
// writing into any existing ones, since a batched check shares one result
// across registrations that may carry different rules.
func (r *CheckResult) ApplyCommentFilters(filters []CommentFilter) {
	var ids []string
	var href, author string
	for _, c := range r.TopLevelComments {
		if matchesAny(filters, c) {
			continue
		}
		ids = append(ids, c.ID)
		href, author = c.Href, c.Author
	}
	r.TopLevelCommentIDs, r.LatestCommentHref, r.LatestCommentAuthor = ids, href, author
}

func matchesAny(filters []CommentFilter, c TopLevelComment) bool {
	for _, f := range filters {
		if f.matches(c) {
			return true
		}
	}
	return false
}
