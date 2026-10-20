package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/cwrau/aoe-plugin-pr-watch-mcp/internal/aoeclient"
	"github.com/cwrau/aoe-plugin-pr-watch-mcp/internal/provider"
	"github.com/cwrau/aoe-plugin-pr-watch-mcp/internal/registry"
)

// ScopeResolver tells whether a GitLab path is a project or a group, which
// the two look alike in a URL. The GitLab checker implements it.
type ScopeResolver interface {
	ResolveScopeKind(ctx context.Context, host, path string) (kind string, err error)
}

// Option configures NewHandler.
type Option func(*handlerConfig)

type handlerConfig struct {
	scopes ScopeResolver
}

// WithScopeResolver lets subscribe resolve GitLab project and group paths.
// Without one, a GitLab path other than a whole host cannot be subscribed to.
func WithScopeResolver(r ScopeResolver) Option {
	return func(c *handlerConfig) { c.scopes = r }
}

// notifyStatusKinds are the events a notify-mode subscription reports besides
// MATCHED: the rough status changes of PRs/MRs it does not watch.
var notifyStatusKinds = []string{"DRAFTED", "READY_FOR_REVIEW", "MERGED", "CLOSED"}

const (
	defaultMaxWatches         = 25
	defaultPlatformMaxWatches = 10
	hardMaxWatches            = 100
)

type subscribeInput struct {
	ScopeURL             string               `json:"scope_url" jsonschema:"what to watch: a repo (https://github.com/owner/repo or a GitLab project URL), an org or group (https://github.com/org, a GitLab group URL) or a whole platform (https://github.com, https://gitlab.example.com; needs a person filter)"`
	SessionID            string               `json:"session_id" jsonschema:"the calling AoE session's id — pass $AOE_ARTIFACT_DIR's raw value directly, its basename is extracted automatically"`
	Filters              provider.ScanFilters `json:"filters,omitzero" jsonschema:"optional filters narrowing the scope; 'me' means the worker's own account"`
	Persona              string               `json:"persona,omitempty" jsonschema:"optional: reviewer (default for subscriptions) or author; sets the default events and badge of every watch this subscription registers"`
	Events               []string             `json:"events,omitempty" jsonschema:"optional: event kinds the registered watches notify on, as for register; omit for the persona's defaults"`
	ReportExisting       bool                 `json:"report_existing,omitempty" jsonschema:"optional: announce the PRs/MRs that already match on the first scan instead of registering them silently"`
	MaxWatches           int                  `json:"max_watches,omitempty" jsonschema:"optional: most watches this subscription keeps registered (default 25, 10 for a platform scope, at most 100)"`
	ReleaseWhenUnmatched bool                 `json:"release_when_unmatched,omitempty" jsonschema:"optional, watch mode only: stop watching a PR/MR once it no longer matches the filters, instead of following it until merged or closed"`
	Mode                 string               `json:"mode,omitempty" jsonschema:"optional: watch (default: every match is registered as a watch) or notify (nothing is watched: you are told about new matches, draft flips, merges and closes, and decide what to register yourself, e.g. as an orchestrator)"`
}

type subscribeOutput struct {
	SubscriptionID string `json:"subscription_id"`
	Provider       string `json:"provider"`
	Host           string `json:"host"`
	ScopeKind      string `json:"scope_kind"`
	ScopePath      string `json:"scope_path"`
	Persona        string `json:"persona"`
	MaxWatches     int    `json:"max_watches"`
}

type unsubscribeInput struct {
	SubscriptionID string `json:"subscription_id" jsonschema:"the id subscribe returned"`
	SessionID      string `json:"session_id" jsonschema:"the calling AoE session's id — pass $AOE_ARTIFACT_DIR's raw value directly; must be the session that subscribed"`
}

type unsubscribeOutput struct {
	Removed bool `json:"removed"`
}

type subscriptionsInput struct {
	SessionID string `json:"session_id,omitempty" jsonschema:"optional: only this session's subscriptions (the raw $AOE_ARTIFACT_DIR value works); omit to list every session's"`
}

type subscriptionView struct {
	ID                   string               `json:"id"`
	SessionID            string               `json:"session_id"`
	Provider             string               `json:"provider"`
	Host                 string               `json:"host"`
	ScopeKind            string               `json:"scope_kind"`
	ScopePath            string               `json:"scope_path"`
	Filters              provider.ScanFilters `json:"filters"`
	Persona              string               `json:"persona"`
	Events               []string             `json:"events,omitempty"`
	MaxWatches           int                  `json:"max_watches"`
	ReleaseWhenUnmatched bool                 `json:"release_when_unmatched"`
	Status               string               `json:"status"`
	Mode                 string               `json:"mode"`
	Watches              int                  `json:"watches"`
	Tracked              int                  `json:"tracked"`
	LastScanAt           *time.Time           `json:"last_scan_at,omitempty"`
	LastError            string               `json:"last_error,omitempty"`
}

type subscriptionsOutput struct {
	Subscriptions []subscriptionView `json:"subscriptions"`
}

func addSubscriptionTools(server *mcp.Server, store *registry.Store, sessions SessionValidator, push UIPusher, scopes ScopeResolver) {
	mcp.AddTool(server, &mcp.Tool{Name: "subscribe", Description: subscribeDescription},
		func(ctx context.Context, req *mcp.CallToolRequest, in subscribeInput) (*mcp.CallToolResult, subscribeOutput, error) {
			if in.SessionID == "" {
				return nil, subscribeOutput{}, fmt.Errorf("session_id is required")
			}
			in.SessionID = normalizeSessionID(in.SessionID)
			if err := sessions.Refresh(ctx); err != nil {
				return nil, subscribeOutput{}, fmt.Errorf("verify session_id: %w", err)
			}
			if sessions.Classify(in.SessionID) == aoeclient.SessionGone {
				return nil, subscribeOutput{}, fmt.Errorf("session_id %q does not match any known AoE session — pass the resolved value of $AOE_ARTIFACT_DIR, not the literal variable name", in.SessionID)
			}
			if err := validateEventFilter(in.Events); err != nil {
				return nil, subscribeOutput{}, err
			}
			mode := in.Mode
			switch mode {
			case "":
				mode = registry.SubscriptionModeWatch
			case registry.SubscriptionModeWatch:
			case registry.SubscriptionModeNotify:
				if in.ReleaseWhenUnmatched {
					return nil, subscribeOutput{}, fmt.Errorf("release_when_unmatched only applies to watch mode: a notify subscription watches nothing")
				}
				for _, kind := range in.Events {
					if !slices.Contains(notifyStatusKinds, kind) {
						return nil, subscribeOutput{}, fmt.Errorf("events entry %q does not apply to notify mode, which reports only %s", kind, strings.Join(notifyStatusKinds, ", "))
					}
				}
			default:
				return nil, subscribeOutput{}, fmt.Errorf("mode %q is not recognized (watch, notify)", mode)
			}
			persona := in.Persona
			switch persona {
			case "":
				persona = registry.PersonaReviewer
			case registry.PersonaAuthor, registry.PersonaReviewer:
			default:
				return nil, subscribeOutput{}, fmt.Errorf("persona %q is not recognized (author, reviewer)", persona)
			}

			scope, err := provider.ParseScanScopeURL(in.ScopeURL)
			if err != nil {
				return nil, subscribeOutput{}, err
			}
			if scope.Kind == "" { // a GitLab path: project or group?
				if scopes == nil {
					return nil, subscribeOutput{}, fmt.Errorf("cannot tell whether %s is a GitLab project or group: no GitLab access configured", scope.Path)
				}
				kind, err := scopes.ResolveScopeKind(ctx, scope.Host, scope.Path)
				if err != nil {
					return nil, subscribeOutput{}, err
				}
				scope.Kind = kind
			}
			if err := in.Filters.Validate(scope); err != nil {
				return nil, subscribeOutput{}, err
			}
			// Build the searches once now with a stand-in for "me", so a filter
			// combination GitHub would reject (a query past 256 characters, too
			// many searches) fails here and not silently at every scan.
			now := time.Now()
			switch scope.Provider {
			case "github":
				_, err = provider.BuildGitHubSearchQueries(scope, in.Filters, "me-placeholder", now)
			default:
				_, err = provider.BuildGitLabSearches(scope, in.Filters, "me-placeholder", now)
			}
			if err != nil {
				return nil, subscribeOutput{}, err
			}

			maxWatches := in.MaxWatches
			switch {
			case maxWatches < 0 || maxWatches > hardMaxWatches:
				return nil, subscribeOutput{}, fmt.Errorf("max_watches %d is out of range (1 to %d)", maxWatches, hardMaxWatches)
			case maxWatches == 0 && scope.Kind == "platform":
				maxWatches = defaultPlatformMaxWatches
			case maxWatches == 0:
				maxWatches = defaultMaxWatches
			}

			filters, err := json.Marshal(in.Filters)
			if err != nil {
				return nil, subscribeOutput{}, err
			}
			id, err := store.AddSubscription(ctx, registry.Subscription{
				SessionID: in.SessionID, Provider: scope.Provider, Host: scope.Host, ScopeKind: scope.Kind, ScopePath: scope.Path,
				Filters: string(filters), Persona: persona, EventFilter: in.Events, ReportExisting: in.ReportExisting,
				MaxWatches: maxWatches, ReleaseWhenUnmatched: in.ReleaseWhenUnmatched, CreatedAt: now, Mode: mode,
			})
			if err != nil {
				return nil, subscribeOutput{}, err
			}
			push.Push()
			return nil, subscribeOutput{
				SubscriptionID: id, Provider: scope.Provider, Host: scope.Host, ScopeKind: scope.Kind, ScopePath: scope.Path,
				Persona: persona, MaxWatches: maxWatches,
			}, nil
		})

	mcp.AddTool(server, &mcp.Tool{Name: "unsubscribe", Description: unsubscribeDescription},
		func(ctx context.Context, req *mcp.CallToolRequest, in unsubscribeInput) (*mcp.CallToolResult, unsubscribeOutput, error) {
			if in.SessionID == "" || in.SubscriptionID == "" {
				return nil, unsubscribeOutput{}, fmt.Errorf("session_id and subscription_id are required")
			}
			ok, err := store.StopSubscription(ctx, in.SubscriptionID, normalizeSessionID(in.SessionID))
			if err != nil {
				return nil, unsubscribeOutput{}, err
			}
			if !ok {
				return nil, unsubscribeOutput{}, fmt.Errorf("no active subscription %q owned by this session", in.SubscriptionID)
			}
			push.Push()
			return nil, unsubscribeOutput{Removed: true}, nil
		})

	mcp.AddTool(server, &mcp.Tool{Name: "subscriptions", Description: subscriptionsDescription},
		func(ctx context.Context, req *mcp.CallToolRequest, in subscriptionsInput) (*mcp.CallToolResult, subscriptionsOutput, error) {
			subs, err := store.ListSubscriptions(ctx)
			if err != nil {
				return nil, subscriptionsOutput{}, err
			}
			sessionID := ""
			if in.SessionID != "" {
				sessionID = normalizeSessionID(in.SessionID)
			}
			out := subscriptionsOutput{Subscriptions: []subscriptionView{}}
			for _, s := range subs {
				if sessionID != "" && s.SessionID != sessionID {
					continue
				}
				watches, err := store.CountSubscriptionWatches(ctx, s.ID)
				if err != nil {
					return nil, subscriptionsOutput{}, err
				}
				tracked, err := store.TrackedMatches(ctx, s.ID)
				if err != nil {
					return nil, subscriptionsOutput{}, err
				}
				v := subscriptionView{
					ID: s.ID, SessionID: s.SessionID, Provider: s.Provider, Host: s.Host, ScopeKind: s.ScopeKind, ScopePath: s.ScopePath,
					Persona: s.Persona, Events: s.EventFilter, MaxWatches: s.MaxWatches,
					ReleaseWhenUnmatched: s.ReleaseWhenUnmatched, Status: s.Status, Mode: s.Mode, Watches: watches, Tracked: len(tracked), LastError: s.LastError,
				}
				if err := json.Unmarshal([]byte(s.Filters), &v.Filters); err != nil {
					return nil, subscriptionsOutput{}, fmt.Errorf("subscription %s: stored filters: %w", s.ID, err)
				}
				if !s.LastScanAt.IsZero() {
					t := s.LastScanAt
					v.LastScanAt = &t
				}
				out.Subscriptions = append(out.Subscriptions, v)
			}
			return nil, out, nil
		})
}
