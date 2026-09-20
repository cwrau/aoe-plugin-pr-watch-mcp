package mcpserver

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/cwrau/aoe-pr-watch-mcp/internal/provider"
	"github.com/cwrau/aoe-pr-watch-mcp/internal/registry"
)

type registerInput struct {
	PRURL     string `json:"pr_url" jsonschema:"the GitHub PR or GitLab MR URL to watch"`
	SessionID string `json:"session_id" jsonschema:"the calling AoE session's id, read from $AOE_ARTIFACT_DIR's basename"`
}

type registerOutput struct {
	Registered bool `json:"registered"`
}

type unregisterInput struct {
	PRURL     string `json:"pr_url" jsonschema:"the PR/MR URL to stop watching"`
	SessionID string `json:"session_id" jsonschema:"the calling AoE session's id, read from $AOE_ARTIFACT_DIR's basename; must match the session that registered this URL"`
}

type unregisterOutput struct {
	Removed bool `json:"removed"`
}

type listInput struct{}

type listOutput struct {
	Registrations []registry.Registration `json:"registrations"`
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
// list against store. A fresh mcp.Server is created per the handler's
// factory function, per the SDK's NewStreamableHTTPHandler contract; it's
// cheap since all real state lives in store, not the server value.
func NewHandler(store *registry.Store) http.Handler {
	newServer := func(*http.Request) *mcp.Server {
		server := mcp.NewServer(&mcp.Implementation{Name: "io.cwrau.pr-watch", Version: "0.1.0"}, nil)

		mcp.AddTool(server, &mcp.Tool{
			Name:        "register",
			Description: "Start watching a GitHub PR or GitLab MR for review/CI/merge activity, waking this session on changes.",
		}, func(ctx context.Context, req *mcp.CallToolRequest, in registerInput) (*mcp.CallToolResult, registerOutput, error) {
			prov, err := detectProvider(in.PRURL)
			if err != nil {
				return nil, registerOutput{}, err
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
			return nil, registerOutput{Registered: true}, nil
		})

		mcp.AddTool(server, &mcp.Tool{
			Name:        "unregister",
			Description: "Stop watching a previously-registered GitHub PR or GitLab MR.",
		}, func(ctx context.Context, req *mcp.CallToolRequest, in unregisterInput) (*mcp.CallToolResult, unregisterOutput, error) {
			existing, ok, err := store.Get(ctx, in.PRURL)
			if err != nil {
				return nil, unregisterOutput{}, err
			}
			// Not registered, or registered by a different session: report
			// "nothing removed" without distinguishing the two cases, so a
			// caller can't probe for which PRs/MRs other sessions are
			// watching.
			if !ok || existing.SessionID != in.SessionID {
				return nil, unregisterOutput{Removed: false}, nil
			}
			if err := store.Remove(ctx, in.PRURL); err != nil {
				return nil, unregisterOutput{}, err
			}
			return nil, unregisterOutput{Removed: true}, nil
		})

		mcp.AddTool(server, &mcp.Tool{
			Name:        "list",
			Description: "List every currently-watched PR/MR, across all sessions.",
		}, func(ctx context.Context, req *mcp.CallToolRequest, in listInput) (*mcp.CallToolResult, listOutput, error) {
			regs, err := store.List(ctx)
			if err != nil {
				return nil, listOutput{}, err
			}
			return nil, listOutput{Registrations: regs}, nil
		})

		return server
	}

	return mcp.NewStreamableHTTPHandler(newServer, nil)
}
