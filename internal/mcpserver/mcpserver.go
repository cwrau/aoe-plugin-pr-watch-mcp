package mcpserver

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/cwrau/aoe-plugin-pr-watch-mcp/internal/provider"
	"github.com/cwrau/aoe-plugin-pr-watch-mcp/internal/registry"
)

type registerInput struct {
	PRURL     string `json:"pr_url" jsonschema:"the GitHub PR or GitLab MR URL to watch"`
	SessionID string `json:"session_id" jsonschema:"the calling AoE session's id — pass $AOE_ARTIFACT_DIR's raw value directly, its basename is extracted automatically, no need to run basename yourself"`
}

type registerOutput struct {
	Registered bool `json:"registered"`
}

type unregisterInput struct {
	PRURL     string `json:"pr_url,omitempty" jsonschema:"optional: the specific PR/MR URL to stop watching; omit to stop every registration session_id owns instead of just one"`
	SessionID string `json:"session_id" jsonschema:"the calling AoE session's id — pass $AOE_ARTIFACT_DIR's raw value directly, its basename is extracted automatically, no need to run basename yourself; must match (or, in bulk mode, own) the affected registration(s)"`
}

type unregisterOutput struct {
	Removed bool `json:"removed"` // true if at least one registration was stopped
	Count   int  `json:"count"`   // how many registrations were actually moved to stopped (0 or 1 for a single pr_url, any number in bulk mode)
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
func NewHandler(store *registry.Store, gh GitHubReviewClient) http.Handler {
	newServer := func(*http.Request) *mcp.Server {
		server := mcp.NewServer(&mcp.Implementation{Name: "io.cwrau.pr-watch-mcp", Version: "0.1.0"}, nil)

		mcp.AddTool(server, &mcp.Tool{
			Name:        "register",
			Description: "Start watching a GitHub PR or GitLab MR for review/CI/merge activity, waking this session on changes.",
		}, func(ctx context.Context, req *mcp.CallToolRequest, in registerInput) (*mcp.CallToolResult, registerOutput, error) {
			// An empty session_id can never match a real AoE session, so the
			// scheduler would classify it SessionGone and silently delete
			// the registration on the very next tick — a watch that reports
			// success and then evaporates.
			if in.SessionID == "" {
				return nil, registerOutput{}, fmt.Errorf("session_id is required")
			}
			in.SessionID = normalizeSessionID(in.SessionID)
			prov, err := detectProvider(in.PRURL)
			if err != nil {
				return nil, registerOutput{}, err
			}
			// Refuse to reassign someone else's watch. store.Add upserts, so
			// without this check a caller could re-register another
			// session's pr_url under its own session_id and thereby both
			// hijack its notifications and gain the right to unregister it —
			// defeating unregister's ownership check entirely.
			existing, ok, err := store.Get(ctx, in.PRURL)
			if err != nil {
				return nil, registerOutput{}, err
			}
			if ok && existing.SessionID != in.SessionID {
				return nil, registerOutput{}, fmt.Errorf("pr_url %q is already registered by a different session", in.PRURL)
			}
			reg := registry.Registration{
				PRURL:     in.PRURL,
				SessionID: in.SessionID,
				Provider:  prov,
				CIBucket:  "none",
				NextDueAt: time.Now(),
				CreatedAt: time.Now(),
			}
			if err := store.Add(ctx, reg); err != nil {
				return nil, registerOutput{}, err
			}
			slog.Default().Info("registered", "pr_url", in.PRURL, "session_id", in.SessionID, "provider", prov)
			return nil, registerOutput{Registered: true}, nil
		})

		mcp.AddTool(server, &mcp.Tool{
			Name:        "unregister",
			Description: "Stop watching a previously-registered GitHub PR or GitLab MR. Omit pr_url to stop every registration session_id owns in one call instead of just one.",
		}, func(ctx context.Context, req *mcp.CallToolRequest, in unregisterInput) (*mcp.CallToolResult, unregisterOutput, error) {
			if in.SessionID == "" {
				return nil, unregisterOutput{}, fmt.Errorf("session_id is required")
			}
			in.SessionID = normalizeSessionID(in.SessionID)

			if in.PRURL == "" {
				count, err := store.StopAllOwnedBy(ctx, in.SessionID)
				if err != nil {
					return nil, unregisterOutput{}, err
				}
				if count > 0 {
					slog.Default().Info("unregistered all", "session_id", in.SessionID, "count", count)
				}
				return nil, unregisterOutput{Removed: count > 0, Count: count}, nil
			}

			// One atomic ownership-scoped update rather than a Get-then-
			// SetStatus pair: nothing can change the row's owner between the
			// check and the update. A false result covers both "not
			// registered" and "registered by a different session" without
			// distinguishing them, so a caller can't probe for which PRs/MRs
			// other sessions are watching. The registration itself is never
			// deleted, just moved to StatusStopped — its badge (if the host
			// UI has one) reflects that permanently instead of vanishing.
			removed, err := store.SetStatusOwnedBy(ctx, in.PRURL, in.SessionID, registry.StatusStopped)
			if err != nil {
				return nil, unregisterOutput{}, err
			}
			count := 0
			if removed {
				count = 1
				slog.Default().Info("unregistered", "pr_url", in.PRURL, "session_id", in.SessionID)
			}
			return nil, unregisterOutput{Removed: removed, Count: count}, nil
		})

		mcp.AddTool(server, &mcp.Tool{
			Name:        "list",
			Description: "List PR/MR registrations, across all sessions and every status (watching, merged, closed, stopped) by default. Filter with session_id and/or status, both optional.",
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
			Description: "Dismiss one GitHub PR review (changes-requested or comment) so its badge stops showing. Tries a real GitHub dismissal first, falls back to re-requesting the reviewer, then to hiding it locally only.",
		}, func(ctx context.Context, req *mcp.CallToolRequest, in dismissReviewInput) (*mcp.CallToolResult, dismissReviewOutput, error) {
			if in.SessionID == "" {
				return nil, dismissReviewOutput{}, fmt.Errorf("session_id is required")
			}
			in.SessionID = normalizeSessionID(in.SessionID)
			reg, ok, err := store.Get(ctx, in.PRURL)
			if err != nil {
				return nil, dismissReviewOutput{}, err
			}
			// Same "false covers both not-found and wrong-owner" shape as
			// unregister — a caller can't probe which PRs other sessions
			// are watching.
			if !ok || reg.SessionID != in.SessionID {
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
				if err := store.SetDismissedReview(ctx, in.PRURL, in.ReviewURL); err != nil {
					return dismissReviewOutput{}, err
				}
				slog.Default().Info("dismissed review", "pr_url", in.PRURL, "review_url", in.ReviewURL, "method", "local_only")
				return dismissReviewOutput{Method: "local_only"}, nil
			}

			switch review.State {
			case "CHANGES_REQUESTED":
				if err := gh.DismissReview(ctx, review.ReviewID); err == nil {
					if err := store.ClearReviewIfCurrent(ctx, in.PRURL, in.ReviewURL); err != nil {
						return nil, dismissReviewOutput{}, err
					}
					slog.Default().Info("dismissed review", "pr_url", in.PRURL, "review_url", in.ReviewURL, "method", "api_dismiss")
					return nil, dismissReviewOutput{Method: "api_dismiss"}, nil
				} else {
					slog.Default().Warn("dismiss review failed, trying re-request", "pr_url", in.PRURL, "review_url", in.ReviewURL, "error", err)
				}
				if err := gh.RequestReviewer(ctx, review.PullRequestID, review.AuthorLogin); err == nil {
					if err := store.SetDismissedReview(ctx, in.PRURL, in.ReviewURL); err != nil {
						return nil, dismissReviewOutput{}, err
					}
					slog.Default().Info("dismissed review", "pr_url", in.PRURL, "review_url", in.ReviewURL, "method", "requested_review_again")
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

		return server
	}

	return mcp.NewStreamableHTTPHandler(newServer, nil)
}
