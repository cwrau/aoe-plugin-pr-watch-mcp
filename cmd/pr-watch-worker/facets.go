package main

import (
	"slices"
	"strings"

	"github.com/cwrau/aoe-plugin-pr-watch-mcp/internal/provider"
	"github.com/cwrau/aoe-plugin-pr-watch-mcp/internal/registry"
)

const (
	rowColumnSlot   = "row-column"
	rowColumnID     = "pr_status"
	filterFacetSlot = "filter-facet"
)

type facetOption struct {
	Value string `json:"value"`
	Label string `json:"label"`
	Tone  string `json:"tone,omitempty"`
}

// facetDef is one global filter-facet: the dashboard sidebar (and plugins such
// as the kanban board) filter sessions by the option values a session's
// row-column carries as filter_values.
type facetDef struct {
	ID      string
	Label   string
	Column  string
	Options []facetOption
}

func opt(facet, value, label, tone string) facetOption {
	return facetOption{Value: facet + ":" + value, Label: label, Tone: tone}
}

// filterFacets is the facet vocabulary. The CI, review, PR, comments,
// conflicts and merge-queue facets use the same ids, values and labels as the
// official GitHub plugin's, minus the platform prefix, since this plugin also
// watches GitLab. Values follow each watch's events filter like its badge
// chips do (see registrationFilterValues), but not the badge's compact mode.
var filterFacets = []facetDef{
	{ID: "ci", Label: "CI", Column: rowColumnID, Options: []facetOption{
		opt("ci", "passing", "passing", "success"),
		opt("ci", "failing", "failing", "danger"),
		opt("ci", "running", "running", "info"),
		opt("ci", "action-required", "action required", "warn"),
	}},
	{ID: "review", Label: "Review", Column: rowColumnID, Options: []facetOption{
		opt("review", "approved", "approved", "success"),
		opt("review", "partial-approval", "partial approval", "info"),
		opt("review", "changes-requested", "changes requested", "danger"),
		opt("review", "commented", "commented", "info"),
		opt("review", "waiting", "awaiting review", "neutral"),
	}},
	{ID: "pr", Label: "PR", Column: rowColumnID, Options: []facetOption{
		opt("pr", "open", "open", "success"),
		opt("pr", "draft", "draft", "warn"),
		opt("pr", "merged", "merged", "success"),
		opt("pr", "closed", "closed", "neutral"),
	}},
	{ID: "comments", Label: "Comments", Column: rowColumnID, Options: []facetOption{
		opt("comments", "unresolved", "unresolved", "warn"),
	}},
	{ID: "conflicts", Label: "Conflicts", Column: rowColumnID, Options: []facetOption{
		opt("conflicts", "yes", "conflicts", "danger"),
	}},
	{ID: "queue", Label: "Merge queue", Column: rowColumnID, Options: []facetOption{
		opt("queue", "queued", "queued", "info"),
		opt("queue", "awaiting-checks", "awaiting checks", "info"),
		opt("queue", "mergeable", "mergeable", "success"),
		opt("queue", "locked", "locked", "warn"),
		opt("queue", "unmergeable", "unmergeable", "danger"),
	}},
	{ID: "blocked", Label: "Blocked", Column: rowColumnID, Options: []facetOption{
		opt("blocked", "yes", "blocked by another MR", "warn"),
	}},
	{ID: "persona", Label: "Persona", Column: rowColumnID, Options: []facetOption{
		opt("persona", registry.PersonaAuthor, "author", "neutral"),
		opt("persona", registry.PersonaReviewer, "reviewer", "info"),
	}},
	{ID: "my_review", Label: "Your review", Column: rowColumnID, Options: []facetOption{
		opt("my_review", "in-progress", "in progress", "info"),
		opt("my_review", "approved", "approved", "success"),
	}},
	{ID: "snoozed", Label: "Snoozed", Column: rowColumnID, Options: []facetOption{
		opt("snoozed", "yes", "snoozed", "neutral"),
	}},
	{ID: "subscription", Label: "Subscription", Column: rowColumnID, Options: []facetOption{
		opt("subscription", registry.SubscriptionModeWatch, "watching matches", "info"),
		opt("subscription", registry.SubscriptionModeNotify, "notifying about matches", "info"),
	}},
	{ID: "provider", Label: "Provider", Column: rowColumnID, Options: []facetOption{
		opt("provider", "github", "GitHub", "neutral"),
		opt("provider", "gitlab", "GitLab", "neutral"),
	}},
}

// queueFacetValues are the merge-queue states the queue facet offers; any other
// state counts as queued, like the badge's own fallback.
var queueFacetValues = []string{"queued", "awaiting-checks", "mergeable", "locked", "unmergeable"}

// filterValuesFor is, per session, the sorted union of the facet values of
// every registration it still has (stopped ones are gone) and of the
// subscriptions it holds. A session left with no values is absent.
func filterValuesFor(regs []registry.Registration, subs []subscriptionBadgeInfo) map[string][]string {
	sets := map[string]map[string]bool{}
	add := func(session string, values ...string) {
		if sets[session] == nil {
			sets[session] = map[string]bool{}
		}
		for _, v := range values {
			sets[session][v] = true
		}
	}
	for _, r := range regs {
		if r.Status == registry.StatusStopped {
			continue
		}
		add(r.SessionID, registrationFilterValues(r)...)
	}
	for _, s := range subs {
		add(s.SessionID, "subscription:"+s.Mode)
	}
	out := make(map[string][]string, len(sets))
	for session, set := range sets {
		out[session] = slices.Sorted(func(yield func(string) bool) {
			for v := range set {
				if !yield(v) {
					return
				}
			}
		})
	}
	return out
}

func registrationFilterValues(r registry.Registration) []string {
	persona := registry.PersonaAuthor
	if r.Persona == registry.PersonaReviewer {
		persona = registry.PersonaReviewer
	}
	values := []string{"persona:" + persona, "provider:" + r.Provider}
	switch r.Status {
	case registry.StatusMerged:
		return append(values, "pr:merged")
	case registry.StatusClosed:
		return append(values, "pr:closed")
	}
	if r.IsDraft {
		values = append(values, "pr:draft")
	} else {
		values = append(values, "pr:open")
	}
	// A fact the watch's event filter does not follow stays out, like its
	// badge chip: a facet matching a PR whose chip is not shown would look
	// wrong. The persona's default filter counts, and so does an explicit one.
	filter := r.EffectiveEventFilter()
	shows := func(kind string) bool { return eventFilterShows(filter, kind) }
	if shows("CI") {
		switch r.CIBucket {
		case "success":
			values = append(values, "ci:passing")
		case "failure":
			values = append(values, "ci:failing")
		case "pending":
			values = append(values, "ci:running")
		case "action_required":
			values = append(values, "ci:action-required")
		}
	}
	if shows("REVIEW") {
		switch {
		case r.LatestReviewState == "":
			values = append(values, "review:waiting")
		case r.LatestReviewState == "APPROVED":
			values = append(values, "review:approved")
		case provider.PartialApproval(r.LatestReviewState):
			values = append(values, "review:partial-approval")
		case r.LatestReviewState == "CHANGES_REQUESTED":
			values = append(values, "review:changes-requested")
		case r.LatestReviewState == "COMMENTED":
			values = append(values, "review:commented")
		}
	}
	// The reviewer's own progress is its review chip, which no event filter
	// hides, so it is never gated.
	if r.Persona == registry.PersonaReviewer {
		if r.SelfReviewState == "APPROVED" {
			values = append(values, "my_review:approved")
		} else {
			values = append(values, "my_review:in-progress")
		}
	}
	if shows("COMMENT") && len(r.UnresolvedThreadIDs) > 0 {
		values = append(values, "comments:unresolved")
	}
	// The badge hides a blocked MR together with a conflict (both ride the
	// eye chip's colour, which CONFLICT governs), so blocked follows CONFLICT.
	if shows("CONFLICT") {
		if r.HasConflict {
			values = append(values, "conflicts:yes")
		}
		if r.Blocked {
			values = append(values, "blocked:yes")
		}
	}
	if shows("MERGE_QUEUE") && r.MergeQueueState != "" {
		state := strings.ToLower(strings.ReplaceAll(r.MergeQueueState, "_", "-"))
		if !slices.Contains(queueFacetValues, state) {
			state = "queued"
		}
		values = append(values, "queue:"+state)
	}
	if r.Snoozed {
		values = append(values, "snoozed:yes")
	}
	if r.SubscriptionID != "" {
		values = append(values, "subscription:"+registry.SubscriptionModeWatch)
	}
	return values
}
