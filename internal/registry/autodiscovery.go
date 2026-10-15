package registry

import (
	"context"
	"fmt"
	"strings"
)

// AutoDiscoveryTarget is one (session, checkout) the auto-watch background
// probe is tracking — see the auto_discovery table's doc comment in schema.
type AutoDiscoveryTarget struct {
	SessionID  string
	SourcePath string
	Provider   string // "github" | "gitlab"
	RemoteKey  string // "owner/repo" (github) or "host/project/path" (gitlab)
	Branch     string

	// ExtraRemoteKeys are further GitHub repos ("owner/repo") of the same
	// checkout to probe for the branch's PR — a fork checkout opens its PR in
	// the upstream repo, not in the origin it pushes to. HeadOwners are the
	// owners of the checkout's remotes: a PR found in an extra repo only
	// counts if its head repo belongs to one of them, so a stranger's PR that
	// happens to share the branch name is not adopted.
	ExtraRemoteKeys []string
	HeadOwners      []string
}

// UpsertAutoDiscoveryTarget records (or refreshes) what checkout to probe
// for sessionID/sourcePath. A branch change resets resolved_for_branch —
// the point of it is "no need to probe again until something about this
// target changes", and a new branch is exactly that: the old branch's
// answer says nothing about whether the new one has an open PR/MR. An
// unchanged branch leaves it untouched, so this is idempotent: it's called
// unconditionally for every checkout of every pass without re-arming
// already-resolved targets.
func (s *Store) UpsertAutoDiscoveryTarget(ctx context.Context, sessionID, sourcePath, provider, remoteKey, branch string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO auto_discovery (session_id, source_path, provider, remote_key, branch, resolved_for_branch)
		VALUES (?, ?, ?, ?, ?, '')
		ON CONFLICT(session_id, source_path) DO UPDATE SET
			provider = excluded.provider,
			remote_key = excluded.remote_key,
			branch = excluded.branch,
			resolved_for_branch = CASE WHEN branch != excluded.branch THEN '' ELSE resolved_for_branch END
	`, sessionID, sourcePath, provider, remoteKey, branch)
	if err != nil {
		return fmt.Errorf("upsert auto-discovery target: %w", err)
	}
	return nil
}

// SetAutoDiscoveryRemotes stores the extra repos and head owners for a
// target. Kept apart from UpsertAutoDiscoveryTarget so the per-pass upsert of
// the primary target never has to know about them.
func (s *Store) SetAutoDiscoveryRemotes(ctx context.Context, sessionID, sourcePath string, extraRemoteKeys, headOwners []string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE auto_discovery SET extra_remote_keys = ?, head_owners = ? WHERE session_id = ? AND source_path = ?`,
		marshalStrings(extraRemoteKeys), marshalStrings(headOwners), sessionID, sourcePath)
	if err != nil {
		return fmt.Errorf("set auto-discovery remotes: %w", err)
	}
	return nil
}

// MarkAutoDiscoveryResolved records that branch's answer is now known for
// sessionID/sourcePath (whether or not a PR/MR was actually found), so it's
// skipped until the branch changes again.
func (s *Store) MarkAutoDiscoveryResolved(ctx context.Context, sessionID, sourcePath, branch string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE auto_discovery SET resolved_for_branch = ? WHERE session_id = ? AND source_path = ?`, branch, sessionID, sourcePath)
	if err != nil {
		return fmt.Errorf("mark auto-discovery resolved: %w", err)
	}
	return nil
}

// UnresolvedAutoDiscoveryForSessions returns every target of sessionIDs
// whose branch hasn't been resolved yet (resolved_for_branch != branch —
// including a target never probed at all, where it is still ”). Once a
// branch is resolved it's skipped until the branch itself changes
// (UpsertAutoDiscoveryTarget resets resolved_for_branch then), so a session
// sitting on a branch that already has a registration costs nothing
// further here. There's deliberately no per-target pacing: the caller's own
// cadence (a debounced event, or the slow periodic pass) is the pacing.
func (s *Store) UnresolvedAutoDiscoveryForSessions(ctx context.Context, sessionIDs []string) ([]AutoDiscoveryTarget, error) {
	if len(sessionIDs) == 0 {
		return nil, nil
	}
	placeholders := make([]string, len(sessionIDs))
	args := make([]any, len(sessionIDs))
	for i, id := range sessionIDs {
		placeholders[i] = "?"
		args[i] = id
	}
	query := fmt.Sprintf(`
		SELECT session_id, source_path, provider, remote_key, branch, extra_remote_keys, head_owners
		FROM auto_discovery
		WHERE resolved_for_branch != branch AND session_id IN (%s)
		ORDER BY session_id, source_path
	`, strings.Join(placeholders, ", "))
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query unresolved auto-discovery targets: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []AutoDiscoveryTarget
	for rows.Next() {
		var t AutoDiscoveryTarget
		var extra, owners string
		if err := rows.Scan(&t.SessionID, &t.SourcePath, &t.Provider, &t.RemoteKey, &t.Branch, &extra, &owners); err != nil {
			return nil, fmt.Errorf("scan auto-discovery target: %w", err)
		}
		t.ExtraRemoteKeys = unmarshalStrings(extra)
		t.HeadOwners = unmarshalStrings(owners)
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("row iteration: %w", err)
	}
	return out, nil
}

// DistinctAutoDiscoverySessionIDs returns every session_id with at least
// one auto_discovery row — the scheduler's own set of "sessions I might
// need to prune" each tick, independent of whether that session currently
// owns any registrations at all.
func (s *Store) DistinctAutoDiscoverySessionIDs(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT session_id FROM auto_discovery`)
	if err != nil {
		return nil, fmt.Errorf("query distinct auto-discovery sessions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan auto-discovery session id: %w", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("row iteration: %w", err)
	}
	return out, nil
}

// DeleteAutoDiscoveryForSession removes every auto_discovery row for
// sessionID outright — for a session that's gone or archived, where there's
// nothing left to probe for and no reason to keep spending budget trying.
func (s *Store) DeleteAutoDiscoveryForSession(ctx context.Context, sessionID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM auto_discovery WHERE session_id = ?`, sessionID)
	if err != nil {
		return fmt.Errorf("delete auto-discovery for session: %w", err)
	}
	return nil
}

// PruneAutoDiscoveryForSession removes any auto_discovery row for sessionID
// whose source_path isn't in keepSourcePaths — a repo that's left the
// session's workspace (removed, or the session's whole workspace changed)
// has nothing left to probe for either. An empty keepSourcePaths deletes
// every row for the session, same as DeleteAutoDiscoveryForSession.
func (s *Store) PruneAutoDiscoveryForSession(ctx context.Context, sessionID string, keepSourcePaths []string) error {
	if len(keepSourcePaths) == 0 {
		return s.DeleteAutoDiscoveryForSession(ctx, sessionID)
	}
	placeholders := make([]string, len(keepSourcePaths))
	args := make([]any, 0, len(keepSourcePaths)+1)
	args = append(args, sessionID)
	for i, p := range keepSourcePaths {
		placeholders[i] = "?"
		args = append(args, p)
	}
	query := fmt.Sprintf(`DELETE FROM auto_discovery WHERE session_id = ? AND source_path NOT IN (%s)`, strings.Join(placeholders, ", "))
	if _, err := s.db.ExecContext(ctx, query, args...); err != nil {
		return fmt.Errorf("prune auto-discovery for session: %w", err)
	}
	return nil
}
