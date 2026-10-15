package aoeclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
)

type SessionClass int

const (
	// SessionGone covers both "never existed" and "hard-deleted", and also a
	// trashed session: none of them should be sent to, and a registration
	// pointing at one should be dropped.
	SessionGone SessionClass = iota
	SessionLive
	SessionArchived
)

// defaultResyncEvery is how often RefreshIfStale re-lists every session from
// the host. Lifecycle events keep the directory current in between, but they
// are best-effort and never replayed, so a periodic full listing is the
// backstop for anything missed.
const defaultResyncEvery = 10 * time.Minute

// WorkspaceRepo is one git checkout inside a session's workspace — the
// workspace root itself plus every other repo of a multi-repo session. The
// auto-watch discovery path reads these off the session summary rather than
// scanning directories itself. Branch is as of the last summary the host
// sent; discovery re-reads the real branch from git anyway.
type WorkspaceRepo struct {
	SourcePath string `json:"source_path"`
	Branch     string `json:"branch"`
}

// Worktree is set when the session's own root (ProjectPath) is itself a git
// worktree of another main checkout — the common single-repo case, which the
// host reports separately from WorkspaceRepos. Confirmed live: omitting it
// was why auto-watch discovery silently found nothing for that case.
type Worktree struct {
	Branch string `json:"branch"`
}

// Summary is one session as the host describes it, in `sessions.list` and in
// the `summary` of a `session.lifecycle.changed` notification (the two are
// the same shape by design).
type Summary struct {
	ID             string          `json:"id"`
	Title          string          `json:"title"`
	ProjectPath    string          `json:"project_path"`
	Archived       bool            `json:"archived"`
	Snoozed        bool            `json:"snoozed"`
	Trashed        bool            `json:"trashed"`
	WorkspaceRepos []WorkspaceRepo `json:"workspace_repos"`
	Worktree       *Worktree       `json:"worktree"`
}

// Lister fetches every session from the host (the `sessions.list` RPC),
// including archived and trashed ones. A deleted session is simply absent.
type Lister interface {
	ListSessions(ctx context.Context) ([]Summary, error)
}

// ParseSessionList decodes a `sessions.list` result: {"sessions": [...]}.
func ParseSessionList(raw json.RawMessage) ([]Summary, error) {
	var reply struct {
		Sessions []Summary `json:"sessions"`
	}
	if err := json.Unmarshal(raw, &reply); err != nil {
		return nil, fmt.Errorf("decode sessions.list result: %w", err)
	}
	return reply.Sessions, nil
}

// ParseSummary decodes one session summary, e.g. a lifecycle event's.
func ParseSummary(raw json.RawMessage) (Summary, error) {
	var s Summary
	if err := json.Unmarshal(raw, &s); err != nil {
		return Summary{}, fmt.Errorf("decode session summary: %w", err)
	}
	return s, nil
}

// Client is the worker's view of AoE sessions: their lifecycle class, title
// and checkouts. It is loaded by listing sessions over RPC (Refresh,
// RefreshIfStale) and kept current by the host's lifecycle notifications
// (ApplyLifecycle). Those arrive on a different goroutine than the
// scheduler's and the MCP handler's reads, so every method is safe for
// concurrent use.
type Client struct {
	// Lister fetches the full session list.
	Lister Lister
	// ResyncEvery is RefreshIfStale's throttle; zero means defaultResyncEvery.
	ResyncEvery time.Duration

	mu         sync.RWMutex
	sessions   map[string]Summary
	lastResync time.Time
	now        func() time.Time // test seam; defaults to time.Now
}

// New returns a Client that lists sessions through l.
func New(l Lister) *Client {
	return &Client{Lister: l}
}

func (c *Client) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

// Refresh lists every session now and replaces the directory with the
// result. A failed listing keeps the previous snapshot.
func (c *Client) Refresh(ctx context.Context) error {
	if c.Lister == nil {
		return errors.New("aoeclient: no Lister configured")
	}
	list, err := c.Lister.ListSessions(ctx)
	if err != nil {
		return err
	}
	sessions := make(map[string]Summary, len(list))
	for _, s := range list {
		sessions[s.ID] = s
	}
	c.mu.Lock()
	c.sessions = sessions
	c.lastResync = c.clock()
	c.mu.Unlock()
	return nil
}

// RefreshIfStale is Refresh, but only when the directory has never been
// loaded or the last full listing is older than ResyncEvery. The scheduler
// calls it every tick: lifecycle events keep the directory current, so this
// is just the periodic backstop.
func (c *Client) RefreshIfStale(ctx context.Context) error {
	every := c.ResyncEvery
	if every == 0 {
		every = defaultResyncEvery
	}
	c.mu.RLock()
	fresh := !c.lastResync.IsZero() && c.clock().Sub(c.lastResync) < every
	c.mu.RUnlock()
	if fresh {
		return nil
	}
	return c.Refresh(ctx)
}

// ApplyLifecycle folds one `session.lifecycle.changed` notification into the
// directory: "deleted" removes the session, any other kind replaces it with
// the event's summary. A non-delete event without a summary is malformed and
// ignored.
func (c *Client) ApplyLifecycle(kind, id string, summary *Summary) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if kind == "deleted" {
		delete(c.sessions, id)
		return
	}
	if summary == nil {
		return
	}
	if c.sessions == nil {
		c.sessions = make(map[string]Summary)
	}
	s := *summary
	s.ID = id
	c.sessions[id] = s
}

// Classify reports a session's lifecycle state. An id the directory doesn't
// know, or one in the trash, is SessionGone.
func (c *Client) Classify(id string) SessionClass {
	c.mu.RLock()
	defer c.mu.RUnlock()
	s, ok := c.sessions[id]
	switch {
	case !ok || s.Trashed:
		return SessionGone
	case s.Archived:
		return SessionArchived
	default:
		return SessionLive
	}
}

// Title returns a session's title, or "" if the id is unknown or untitled.
func (c *Client) Title(id string) string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.sessions[id].Title
}

// WorkspaceRepos returns the git checkouts inside a live session's
// workspace — empty for an archived or gone session, or a live one with no
// checkouts at all.
func (c *Client) WorkspaceRepos(id string) []WorkspaceRepo {
	c.mu.RLock()
	defer c.mu.RUnlock()
	s, ok := c.sessions[id]
	if !ok {
		return nil
	}
	return liveRepos(s)
}

// LiveWorkspaces returns every live session's checkouts, keyed by session
// id — implements scheduler.WorkspaceSource. Only a session with at least
// one checkout is present, and the result is a copy the caller may keep.
func (c *Client) LiveWorkspaces() map[string][]WorkspaceRepo {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make(map[string][]WorkspaceRepo)
	for id, s := range c.sessions {
		if repos := liveRepos(s); len(repos) > 0 {
			out[id] = repos
		}
	}
	return out
}

// liveRepos is s's checkouts, deduplicated by path, and only for a live
// session: an archived or trashed session isn't being worked on, so there's
// nothing new to discover a PR/MR for — the same "inactive session, don't
// spend budget on it" reasoning the scheduler applies to every other poll.
// The returned slice is always freshly allocated.
func liveRepos(s Summary) []WorkspaceRepo {
	if s.Archived || s.Trashed {
		return nil
	}
	seen := make(map[string]bool, len(s.WorkspaceRepos)+1)
	var repos []WorkspaceRepo
	for _, r := range s.WorkspaceRepos {
		if r.SourcePath == "" || seen[r.SourcePath] {
			continue
		}
		seen[r.SourcePath] = true
		repos = append(repos, r)
	}
	// The session's own root is a separate case from WorkspaceRepos: see
	// Worktree's doc comment.
	if s.Worktree != nil && s.ProjectPath != "" && !seen[s.ProjectPath] {
		repos = append(repos, WorkspaceRepo{SourcePath: s.ProjectPath, Branch: s.Worktree.Branch})
	}
	return repos
}
