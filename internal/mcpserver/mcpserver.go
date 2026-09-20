package mcpserver

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

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
	PRURL string `json:"pr_url" jsonschema:"the PR/MR URL to stop watching"`
}

type unregisterOutput struct {
	Removed bool `json:"removed"`
}

type listInput struct{}

type listOutput struct {
	Registrations []registry.Registration `json:"registrations"`
}

// detectProvider mirrors the URL shapes ParseGitHubPRURL/ParseGitLabMRURL
// already recognize, without importing the provider package just for this
// one branch: github.com PR URLs are "github", everything else register
// accepts is "gitlab".
func detectProvider(prURL string) string {
	if strings.HasPrefix(prURL, "https://github.com/") {
		return "github"
	}
	return "gitlab"
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
			reg := registry.Registration{
				PRURL:     in.PRURL,
				SessionID: in.SessionID,
				Provider:  detectProvider(in.PRURL),
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
