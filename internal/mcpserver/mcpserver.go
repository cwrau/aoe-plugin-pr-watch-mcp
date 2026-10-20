package mcpserver

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/cwrau/aoe-plugin-pr-watch-mcp/internal/aoeclient"
	"github.com/cwrau/aoe-plugin-pr-watch-mcp/internal/provider"
	"github.com/cwrau/aoe-plugin-pr-watch-mcp/internal/registry"
)

type registerInput struct {
	ReportExisting bool     `json:"report_existing,omitempty" jsonschema:"optional: send a ping for the activity the PR/MR already has (reviews, comments, failing CI, conflict) on the first poll; by default that first poll is only recorded as the baseline and stays silent, and later changes ping as usual"`
	PRURLs         []string `json:"pr_urls" jsonschema:"one or more GitHub PR or GitLab MR URLs to watch — a single-element list registers just one, same as before"`
	SessionID      string   `json:"session_id" jsonschema:"the calling AoE session's id — pass $AOE_ARTIFACT_DIR's raw value directly, its basename is extracted automatically, no need to run basename yourself"`
	Events         []string `json:"events,omitempty" jsonschema:"optional: only notify for these event kinds (REVIEW, REVIEW_DISMISSED, COMMENT, CI, CI_RECOVERED, CI_SUCCESS, CONFLICT, MERGED, CLOSED, REOPENED, MERGE_QUEUE, PUSH, REVIEW_REQUESTED, READY_FOR_REVIEW, DRAFTED, THREAD_RESOLVED), e.g. [\"MERGED\"] to only hear about the merge; omit for the persona's default kinds. Applies to every URL in pr_urls alike. Re-registering the same pr_url/session_id with a different events list just replaces the filter, same as register's other idempotent-refresh behavior."`
	Persona        string   `json:"persona,omitempty" jsonschema:"optional: author (default: you watch your own PR/MR, so CI, conflicts, reviews and comments matter) or reviewer (you review someone else's PR/MR: new commits, review requests for you, draft changes and resolved threads matter, CI and conflicts do not, and the row badge shows only the PR and your own review progress). Only sets the default events and badge; an explicit events list always wins."`
}

// registerResult is one pr_urls entry's own outcome — register never fails
// the whole call just because one URL in a batch was malformed or
// otherwise rejected; every other URL in the same call still registers.
type registerResult struct {
	PRURL      string `json:"pr_url"`
	Registered bool   `json:"registered"`
	Error      string `json:"error,omitempty"` // set instead of Registered on a per-URL failure (e.g. an unrecognized pr_url shape)
}

type registerOutput struct {
	Results []registerResult `json:"results"`
}

type unregisterInput struct {
	PRURLs    []string `json:"pr_urls,omitempty" jsonschema:"optional: the specific PR/MR URL(s) to stop watching; omit entirely to stop every registration session_id owns in one call instead"`
	SessionID string   `json:"session_id" jsonschema:"the calling AoE session's id — pass $AOE_ARTIFACT_DIR's raw value directly, its basename is extracted automatically, no need to run basename yourself; must match (or, in bulk mode, own) the affected registration(s)"`
}

// unregisterResult is one pr_urls entry's own outcome, same partial-success
// shape as registerResult — only populated when pr_urls was given; the
// bulk (pr_urls omitted) path has no per-URL granularity to report.
type unregisterResult struct {
	PRURL   string `json:"pr_url"`
	Removed bool   `json:"removed"`
}

type unregisterOutput struct {
	Results []unregisterResult `json:"results,omitempty"` // one entry per pr_urls input; absent (nil) in bulk mode
	Removed bool               `json:"removed"`           // true if at least one registration was stopped
	Count   int                `json:"count"`             // how many registrations were actually moved to stopped
}

type listInput struct {
	SessionID string `json:"session_id,omitempty" jsonschema:"optional: only return registrations owned by this session — accepts the bare session id or $AOE_ARTIFACT_DIR's raw value; omit to list every session"`
	Status    string `json:"status,omitempty" jsonschema:"optional: only return registrations at this status (watching, merged, closed, stopped); omit to list every status"`
}

type listOutput struct {
	Registrations []registry.Registration `json:"registrations"`
}

type dismissReviewInput struct {
	PRURL     string `json:"pr_url" jsonschema:"the GitHub PR URL the review belongs to"`
	SessionID string `json:"session_id" jsonschema:"the calling AoE session's id — pass $AOE_ARTIFACT_DIR's raw value directly, its basename is extracted automatically, no need to run basename yourself; must match the session that registered this URL"`
	ReviewURL string `json:"review_url" jsonschema:"the exact review URL to dismiss (e.g. from this PR's badge tooltip, or the review_href field in the list tool's output)"`
}

type dismissReviewOutput struct {
	// Method reports which tier of the fallback cascade actually applied:
	// "api_dismiss" (a real GitHub dismissal, sticks permanently), "requested_review_again"
	// (couldn't dismiss, so the reviewer was re-pinged instead — GitHub still
	// shows the old review until they act again), or "local_only" (neither
	// GitHub action worked or applied, so only this tool's own badge is
	// cleared; the review itself is untouched on GitHub).
	Method string `json:"method"`
}

// GitHubReviewClient is the narrow view of provider.GitHubChecker the
// dismiss_review tool needs, so it can be tested against a fake without a
// real HTTP round trip — same pattern as scheduler.GitHubChecker.
type GitHubReviewClient interface {
	FindReview(ctx context.Context, owner, repo string, number int, reviewURL string) (provider.ReviewLookup, bool, error)
	DismissReview(ctx context.Context, reviewID string) error
	RequestReviewer(ctx context.Context, pullRequestID, login string) error
}

// UIPusher lets register/unregister/dismiss_review request an immediate
// dashboard refresh right after a mutation succeeds, instead of leaving the
// home-pane/row-badge/session-pane stale until the scheduler's next
// scheduled push (up to tickInterval later). Push does no real provider
// check of its own — it just re-renders and re-sends whatever's already in
// the registry, so it's cheap enough to call synchronously and unconditionally
// on every successful mutation.
type UIPusher interface {
	Push()
}

// SessionValidator is the narrow view of aoeclient.Client the register tool
// needs. Refreshing on every register call (rather than trusting the
// directory the lifecycle events keep current) means a session created
// moments ago — before its event has arrived — is never rejected as
// not-yet-seen. aoeclient.Client is safe for concurrent use, so this can be
// the very instance the scheduler uses.
type SessionValidator interface {
	Refresh(ctx context.Context) error
	Classify(id string) aoeclient.SessionClass
}

// normalizeSessionID accepts either a bare AoE session id or a full path
// ending in it (as $AOE_ARTIFACT_DIR's raw value would be, e.g.
// ".../artifacts/<id>") and returns just the id. Letting every session_id
// input take either form means a caller can pass $AOE_ARTIFACT_DIR straight
// through without first shelling out to `basename` itself. filepath.Base
// is a no-op on an already-bare id (no path separators to strip), so this
// is safe to apply unconditionally to any non-empty input.
func normalizeSessionID(raw string) string {
	return filepath.Base(raw)
}

// validStatuses is every registry.Status* value the list tool's status
// filter accepts.
var validStatuses = map[string]bool{
	registry.StatusWatching: true,
	registry.StatusMerged:   true,
	registry.StatusClosed:   true,
	registry.StatusStopped:  true,
}

// validEventKinds is every provider.Event Kind value the register tool's
// events filter accepts — kept in lockstep with provider.Event's Kind doc
// comment by hand, since Diff builds Kind values as plain string literals
// rather than through a shared constant set.
var validEventKinds = map[string]bool{
	"REVIEW":           true,
	"REVIEW_DISMISSED": true,
	"COMMENT":          true,
	"CI":               true,
	"CI_RECOVERED":     true,
	"CI_SUCCESS":       true,
	"REOPENED":         true,
	"CONFLICT":         true,
	"MERGED":           true,
	"CLOSED":           true,
	"MERGE_QUEUE":      true,
	"PUSH":             true,
	"REVIEW_REQUESTED": true,
	"READY_FOR_REVIEW": true,
	"DRAFTED":          true,
	"THREAD_RESOLVED":  true,
}

// validateEventFilter rejects an events list containing anything Diff could
// never actually produce — catching a typo (e.g. "MERGE") at register time
// instead of it silently matching nothing forever.
func validateEventFilter(events []string) error {
	for _, kind := range events {
		if !validEventKinds[kind] {
			return fmt.Errorf("events entry %q is not a recognized event kind (REVIEW, REVIEW_DISMISSED, COMMENT, CI, CI_RECOVERED, CI_SUCCESS, CONFLICT, MERGED, CLOSED, REOPENED, MERGE_QUEUE, PUSH, REVIEW_REQUESTED, READY_FOR_REVIEW, DRAFTED, THREAD_RESOLVED)", kind)
		}
	}
	return nil
}

// detectProvider validates prURL against the same shapes the scheduler's
// checkers require (ParseGitHubPRURL / ParseGitLabMRURL) and returns which
// provider it matched. Rejecting anything that doesn't parse as a real
// PR/MR URL up front — rather than defaulting unrecognized shapes to
// "gitlab" — closes the SSRF/token-exfiltration path where an arbitrary
// pr_url would otherwise get stored verbatim and later fed to
// GitLabChecker.Check, which sends the real GitLab auth token to whatever
// host it extracts from the URL.
func detectProvider(prURL string) (string, error) {
	if _, _, _, err := provider.ParseGitHubPRURL(prURL); err == nil {
		return "github", nil
	}
	if _, _, _, err := provider.ParseGitLabMRURL(prURL); err == nil {
		return "gitlab", nil
	}
	return "", fmt.Errorf("pr_url %q is not a recognized GitHub PR or GitLab MR URL", prURL)
}

// NewHandler builds the MCP-over-HTTP handler exposing register/unregister/
// list/dismiss_review against store and gh. A fresh mcp.Server is created
// per the handler's factory function, per the SDK's NewStreamableHTTPHandler
// contract; it's cheap since all real state lives in store, not the server
// value.
func NewHandler(store *registry.Store, gh GitHubReviewClient, sessions SessionValidator, push UIPusher, opts ...Option) http.Handler {
	var cfg handlerConfig
	for _, o := range opts {
		o(&cfg)
	}
	newServer := func(*http.Request) *mcp.Server {
		server := mcp.NewServer(&mcp.Implementation{Name: "io.cwrau.pr-watch-mcp", Version: "0.1.0"}, &mcp.ServerOptions{Instructions: serverInstructions})

		mcp.AddTool(server, &mcp.Tool{
			Name:        "register",
			Description: registerDescription,
		}, func(ctx context.Context, req *mcp.CallToolRequest, in registerInput) (*mcp.CallToolResult, registerOutput, error) {
			// An empty session_id can never match a real AoE session, so the
			// scheduler would classify it SessionGone and silently delete
			// the registration on the very next tick — a watch that reports
			// success and then evaporates.
			if in.SessionID == "" {
				return nil, registerOutput{}, fmt.Errorf("session_id is required")
			}
			if len(in.PRURLs) == 0 {
				return nil, registerOutput{}, fmt.Errorf("pr_urls must contain at least one URL")
			}
			in.SessionID = normalizeSessionID(in.SessionID)
			// Reject a session_id no real AoE session will ever match right
			// here, rather than storing it and letting the scheduler discover
			// it's SessionGone (and silently auto-stop the registration, with
			// no feedback to the caller) on the very next tick — confirmed
			// live: a caller passing the literal text "$AOE_ARTIFACT_DIR"
			// (never expanded, since there's no shell involved in a tool
			// call's arguments) produced exactly that silent failure.
			if err := sessions.Refresh(ctx); err != nil {
				return nil, registerOutput{}, fmt.Errorf("verify session_id: %w", err)
			}
			if sessions.Classify(in.SessionID) == aoeclient.SessionGone {
				return nil, registerOutput{}, fmt.Errorf("session_id %q does not match any known AoE session — pass the resolved value of $AOE_ARTIFACT_DIR, not the literal variable name", in.SessionID)
			}
			if err := validateEventFilter(in.Events); err != nil {
				return nil, registerOutput{}, err
			}
			switch in.Persona {
			case "":
				in.Persona = registry.PersonaAuthor
			case registry.PersonaAuthor, registry.PersonaReviewer:
			default:
				return nil, registerOutput{}, fmt.Errorf("persona %q is not recognized (author, reviewer)", in.Persona)
			}

			// Every URL is attempted regardless of an earlier one failing —
			// one malformed pr_url in a batch must not sink the rest, same
			// "a single item's failure doesn't abort the whole pass"
			// philosophy the scheduler's own batch checks already follow.
			results := make([]registerResult, 0, len(in.PRURLs))
			anySucceeded := false
			for _, prURL := range in.PRURLs {
				prov, err := detectProvider(prURL)
				if err != nil {
					results = append(results, registerResult{PRURL: prURL, Error: err.Error()})
					continue
				}
				// No cross-session ownership check: multiple sessions can
				// independently watch the same PR/MR, each getting its own
				// row (registry's primary key is (pr_url, session_id), not
				// pr_url alone) with its own state and notifications.
				// store.Add's upsert only ever refreshes the caller's own
				// row.
				reg := registry.Registration{
					PRURL:       prURL,
					SessionID:   in.SessionID,
					Provider:    prov,
					CIBucket:    "none",
					NextDueAt:   time.Now(),
					CreatedAt:   time.Now(),
					EventFilter: in.Events,
					// Existing activity is already visible on the PR; only a
					// caller that asks gets it pinged as a backlog.
					SilentFirstPoll: !in.ReportExisting,
					Persona:         in.Persona,
				}
				if err := store.Add(ctx, reg); err != nil {
					results = append(results, registerResult{PRURL: prURL, Error: err.Error()})
					continue
				}
				slog.Default().Info("registered", "pr_url", prURL, "session_id", in.SessionID, "provider", prov)
				results = append(results, registerResult{PRURL: prURL, Registered: true})
				anySucceeded = true
			}
			if anySucceeded {
				push.Push()
				return nil, registerOutput{Results: results}, nil
			}
			// Every URL failed — including the common single-URL case, where
			// this keeps the old hard-error behavior intact rather than
			// silently "succeeding" with nothing actually registered. A
			// genuine multi-URL batch with a mix of outcomes still returns
			// normally above, with the per-URL Results carrying which ones
			// failed and why.
			errs := make([]string, len(results))
			for i, r := range results {
				errs[i] = fmt.Sprintf("%s: %s", r.PRURL, r.Error)
			}
			return nil, registerOutput{Results: results}, fmt.Errorf("no URL in pr_urls could be registered: %s", strings.Join(errs, "; "))
		})

		mcp.AddTool(server, &mcp.Tool{
			Name:        "unregister",
			Description: unregisterDescription,
		}, func(ctx context.Context, req *mcp.CallToolRequest, in unregisterInput) (*mcp.CallToolResult, unregisterOutput, error) {
			if in.SessionID == "" {
				return nil, unregisterOutput{}, fmt.Errorf("session_id is required")
			}
			in.SessionID = normalizeSessionID(in.SessionID)

			if len(in.PRURLs) == 0 {
				count, err := store.StopAllOwnedBy(ctx, in.SessionID)
				if err != nil {
					return nil, unregisterOutput{}, err
				}
				if count > 0 {
					slog.Default().Info("unregistered all", "session_id", in.SessionID, "count", count)
					push.Push()
				}
				return nil, unregisterOutput{Removed: count > 0, Count: count}, nil
			}

			// One atomic ownership-scoped update per URL rather than a
			// Get-then-SetStatus pair: nothing can change a row's owner
			// between the check and the update. A false result covers both
			// "not registered" and "registered by a different session"
			// without distinguishing them, so a caller can't probe for
			// which PRs/MRs other sessions are watching. The registration
			// itself is never deleted, just moved to StatusStopped — its
			// badge (if the host UI has one) reflects that permanently
			// instead of vanishing.
			results := make([]unregisterResult, 0, len(in.PRURLs))
			count := 0
			for _, prURL := range in.PRURLs {
				removed, err := store.SetStatusOwnedBy(ctx, prURL, in.SessionID, registry.StatusStopped)
				if err != nil {
					return nil, unregisterOutput{}, err
				}
				if removed {
					count++
					slog.Default().Info("unregistered", "pr_url", prURL, "session_id", in.SessionID)
				}
				results = append(results, unregisterResult{PRURL: prURL, Removed: removed})
			}
			if count > 0 {
				push.Push()
			}
			return nil, unregisterOutput{Results: results, Removed: count > 0, Count: count}, nil
		})

		mcp.AddTool(server, &mcp.Tool{
			Name:        "list",
			Description: listDescription,
		}, func(ctx context.Context, req *mcp.CallToolRequest, in listInput) (*mcp.CallToolResult, listOutput, error) {
			if in.Status != "" && !validStatuses[in.Status] {
				return nil, listOutput{}, fmt.Errorf("status %q is not a recognized status (watching, merged, closed, stopped)", in.Status)
			}
			sessionID := in.SessionID
			if sessionID != "" {
				sessionID = normalizeSessionID(sessionID)
			}

			regs, err := store.List(ctx)
			if err != nil {
				return nil, listOutput{}, err
			}
			filtered := regs[:0]
			for _, r := range regs {
				if sessionID != "" && r.SessionID != sessionID {
					continue
				}
				if in.Status != "" && r.Status != in.Status {
					continue
				}
				filtered = append(filtered, r)
			}
			return nil, listOutput{Registrations: filtered}, nil
		})

		mcp.AddTool(server, &mcp.Tool{
			Name:        "dismiss_review",
			Description: dismissReviewDescription,
		}, func(ctx context.Context, req *mcp.CallToolRequest, in dismissReviewInput) (*mcp.CallToolResult, dismissReviewOutput, error) {
			if in.SessionID == "" {
				return nil, dismissReviewOutput{}, fmt.Errorf("session_id is required")
			}
			in.SessionID = normalizeSessionID(in.SessionID)
			reg, ok, err := store.Get(ctx, in.PRURL, in.SessionID)
			if err != nil {
				return nil, dismissReviewOutput{}, err
			}
			if !ok {
				return nil, dismissReviewOutput{}, fmt.Errorf("pr_url %q is not registered by this session", in.PRURL)
			}
			if reg.Provider != "github" {
				return nil, dismissReviewOutput{}, fmt.Errorf("dismiss_review only supports GitHub PRs; %q is a %s registration", in.PRURL, reg.Provider)
			}
			owner, repo, number, err := provider.ParseGitHubPRURL(in.PRURL)
			if err != nil {
				return nil, dismissReviewOutput{}, err
			}

			review, found, err := gh.FindReview(ctx, owner, repo, number, in.ReviewURL)
			if err != nil {
				return nil, dismissReviewOutput{}, err
			}
			if !found {
				return nil, dismissReviewOutput{}, fmt.Errorf("review %q not found on %q (already resolved/deleted, or a wrong url)", in.ReviewURL, in.PRURL)
			}

			localOnly := func() (dismissReviewOutput, error) {
				if err := store.SetDismissedReview(ctx, in.PRURL, in.SessionID, in.ReviewURL); err != nil {
					return dismissReviewOutput{}, err
				}
				slog.Default().Info("dismissed review", "pr_url", in.PRURL, "review_url", in.ReviewURL, "method", "local_only")
				push.Push()
				return dismissReviewOutput{Method: "local_only"}, nil
			}

			switch review.State {
			case "CHANGES_REQUESTED":
				if err := gh.DismissReview(ctx, review.ReviewID); err == nil {
					if err := store.ClearReviewIfCurrent(ctx, in.PRURL, in.SessionID, in.ReviewURL); err != nil {
						return nil, dismissReviewOutput{}, err
					}
					slog.Default().Info("dismissed review", "pr_url", in.PRURL, "review_url", in.ReviewURL, "method", "api_dismiss")
					push.Push()
					return nil, dismissReviewOutput{Method: "api_dismiss"}, nil
				} else {
					slog.Default().Warn("dismiss review failed, trying re-request", "pr_url", in.PRURL, "review_url", in.ReviewURL, "error", err)
				}
				if err := gh.RequestReviewer(ctx, review.PullRequestID, review.AuthorLogin); err == nil {
					if err := store.SetDismissedReview(ctx, in.PRURL, in.SessionID, in.ReviewURL); err != nil {
						return nil, dismissReviewOutput{}, err
					}
					slog.Default().Info("dismissed review", "pr_url", in.PRURL, "review_url", in.ReviewURL, "method", "requested_review_again")
					push.Push()
					return nil, dismissReviewOutput{Method: "requested_review_again"}, nil
				} else {
					slog.Default().Warn("re-request review failed, falling back to local-only", "pr_url", in.PRURL, "review_url", in.ReviewURL, "error", err)
				}
				out, err := localOnly()
				return nil, out, err
			case "COMMENTED":
				out, err := localOnly()
				return nil, out, err
			default:
				return nil, dismissReviewOutput{}, fmt.Errorf("review %q is %s, nothing to dismiss", in.ReviewURL, review.State)
			}
		})

		addSubscriptionTools(server, store, sessions, push, cfg.scopes)

		return server
	}

	// The server is unauthenticated and loopback-only, so a web page the user
	// has open must not be able to drive it. The SDK already rejects a
	// non-loopback Host (DNS rebinding); this adds the Origin /
	// Sec-Fetch-Site check against cross-site requests. Non-browser clients
	// send neither header and pass.
	return http.NewCrossOriginProtection().Handler(mcp.NewStreamableHTTPHandler(newServer, nil))
}
