package registry

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Status values for Registration.Status. A registration is never deleted —
// once created it lives forever, moving through these statuses so a
// permanently-set UI badge (e.g. row-badge) always has a durable source of
// truth to reconcile against, even across a worker restart.
const (
	StatusWatching = "watching" // scheduler actively polls this PR/MR
	StatusMerged   = "merged"   // terminal: PR/MR was merged
	StatusClosed   = "closed"   // terminal: PR/MR was closed without merging
	StatusStopped  = "stopped"  // terminal: owning session unregistered it, or its session is gone
)

// Registration is one watched PR/MR: which session to wake, where its
// last-seen state sits, and when the scheduler should check it next.
type Registration struct {
	// SilentFirstPoll is an input to Add only (the row's state is read with
	// Store.FirstPollSilent): the first poll then records the PR's existing
	// activity as the baseline without pinging about it. Applied when the row
	// is inserted, never when an existing one is refreshed.
	SilentFirstPoll     bool
	PRURL               string
	SessionID           string
	Provider            string // "github" | "gitlab"
	ReviewCount         int
	CIBucket            string
	CIWasFailing        bool     // sticky: CI was seen failing and hasn't been seen succeeding since; what CI_RECOVERED fires on
	CIDetail            string   // e.g. the failing check/job name; empty if not applicable/unknown
	CIHref              string   // link to the relevant job, or failing that the pipeline/run itself; empty if unknown
	ReviewHref          string   // link to the latest review/comment; empty if unknown
	UnresolvedThreadIDs []string // IDs of threads unresolved as of the last check; compared by identity so a same-window resolve+new-thread can't hide the new one
	TopLevelCommentIDs  []string // IDs of top-level PR/MR conversation comments seen as of the last check; compared by identity, same reasoning as UnresolvedThreadIDs
	LatestCommentHref   string   // link to the most recent top-level comment; empty if none
	LatestReviewState   string   // e.g. "APPROVED", "CHANGES_REQUESTED", "COMMENTED"; empty if no review yet (also empty for a DISMISSED review — treated the same as no review)
	LatestReviewer      string   // empty if no review yet
	LatestReviewAt      time.Time
	Title               string // empty until the first successful check populates it
	Description         string // the PR/MR body; empty until the first successful check populates it, or if the provider returned none
	Status              string // one of the Status* constants; see their doc comments
	NextDueAt           time.Time
	Archived            bool
	CreatedAt           time.Time
	MergedAt            time.Time // zero unless Status == StatusMerged
	HasConflict         bool      // true if the PR/MR can't currently be merged due to a conflict with its target branch
	MergeQueueState     string    // GitHub merge queue entry state; empty if not in a merge queue
	MergeQueuePosition  int       // this entry's position in the merge queue; meaningless when MergeQueueState == ""
	Blocked             bool      // GitLab only: true if blocked on an unmerged merge request dependency
	DismissedReviewHref string    // ReviewHref of a review dismiss_review couldn't clear on GitHub itself (re-requested or local-only); the scheduler hides a poll result whose ReviewHref still matches this instead of letting it resurface. Empty if nothing is suppressed.
	ThreadCommentIDs    []string  // IDs of the comments in unresolved threads as of the last check; compared by identity to spot a reply in an existing thread
	ThreadCommentsKnown bool      // false until a first check has stored ThreadCommentIDs: without a baseline every existing reply would look new
	EventFilter         []string  // provider.Event Kind values (e.g. "MERGED") this session wants notified; empty/nil means every kind. Set at register time; a later register call for the same (pr_url, session_id) with a different filter just replaces it.

	// Persona is who the session is on this PR/MR (PersonaAuthor, the default
	// when empty, or PersonaReviewer). It only sets what is notified and shown
	// when EventFilter is empty, see EffectiveEventFilter.
	Persona string
	// Review-flow state, the baseline for PUSH, READY_FOR_REVIEW/DRAFTED,
	// REVIEW_REQUESTED and the reviewer's own review chip.
	HeadSHA            string
	IsDraft            bool
	RequestedReviewers []string
	SelfReviewState    string // the worker's own account's latest review state; empty if none
	// SubscriptionID is the subscription that registered this watch, empty
	// for one registered by hand. Set when the row is inserted, never changed.
	SubscriptionID string
}

const (
	PersonaAuthor   = "author"   // watches its own PR/MR: CI, conflicts and reviews matter
	PersonaReviewer = "reviewer" // reviews someone else's PR/MR: new commits and review requests matter, CI does not
)

// reviewerEvents is a reviewer session's default event set: what happens to
// the PR/MR from the reviewing seat. CI, conflicts and merge queue are the
// author's business.
var reviewerEvents = []string{
	"REVIEW", "REVIEW_DISMISSED", "COMMENT", "PUSH", "REVIEW_REQUESTED",
	"READY_FOR_REVIEW", "DRAFTED", "THREAD_RESOLVED", "REOPENED", "MERGED", "CLOSED",
}

// EffectiveEventFilter is the event filter that applies: the explicit one the
// session registered with, else its persona's default. Empty means every kind
// except the opt-in ones, as ever.
func (r Registration) EffectiveEventFilter() []string {
	if len(r.EventFilter) > 0 {
		return r.EventFilter
	}
	if r.Persona == PersonaReviewer {
		return slices.Clone(reviewerEvents)
	}
	return nil
}

type Store struct {
	db *sql.DB
}

// schema's primary key is the (pr_url, session_id) pair, not pr_url alone:
// multiple sessions can independently watch the same PR/MR, each getting
// its own row (own PrevState baseline, own dismissed-review marker, own
// notifications) rather than one session's registration silently taking
// over another's.
const schema = `
CREATE TABLE IF NOT EXISTS registrations (
	pr_url TEXT NOT NULL,
	session_id TEXT NOT NULL,
	provider TEXT NOT NULL,
	review_count INTEGER NOT NULL DEFAULT 0,
	ci_bucket TEXT NOT NULL DEFAULT 'none',
	ci_was_failing INTEGER NOT NULL DEFAULT 0,
	ci_detail TEXT NOT NULL DEFAULT '',
	ci_href TEXT NOT NULL DEFAULT '',
	review_href TEXT NOT NULL DEFAULT '',
	unresolved_thread_ids TEXT NOT NULL DEFAULT '[]',
	top_level_comment_ids TEXT NOT NULL DEFAULT '[]',
	latest_comment_href TEXT NOT NULL DEFAULT '',
	latest_review_state TEXT NOT NULL DEFAULT '',
	latest_reviewer TEXT NOT NULL DEFAULT '',
	latest_review_at INTEGER NOT NULL DEFAULT 0,
	title TEXT NOT NULL DEFAULT '',
	description TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL DEFAULT 'watching',
	next_due_at INTEGER NOT NULL,
	archived INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL,
	merged_at INTEGER NOT NULL DEFAULT 0,
	has_conflict INTEGER NOT NULL DEFAULT 0,
	merge_queue_state TEXT NOT NULL DEFAULT '',
	merge_queue_position INTEGER NOT NULL DEFAULT 0,
	blocked INTEGER NOT NULL DEFAULT 0,
	dismissed_review_href TEXT NOT NULL DEFAULT '',
	event_filter TEXT NOT NULL DEFAULT '[]',
	thread_comment_ids TEXT NOT NULL DEFAULT 'null',
	closed_at INTEGER NOT NULL DEFAULT 0,
	silent_first_poll INTEGER NOT NULL DEFAULT 0,
	persona TEXT NOT NULL DEFAULT '',
	head_sha TEXT NOT NULL DEFAULT '',
	is_draft INTEGER NOT NULL DEFAULT 0,
	requested_reviewers TEXT NOT NULL DEFAULT '[]',
	self_review_state TEXT NOT NULL DEFAULT '',
	subscription_id TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (pr_url, session_id)
);

-- subscriptions are watch scopes (a repo, an org or group, a platform) plus
-- filters: the scheduler scans them and registers each match as a normal
-- watch tagged with the subscription's id. See subscriptions.go.
CREATE TABLE IF NOT EXISTS subscriptions (
	id TEXT PRIMARY KEY,
	session_id TEXT NOT NULL,
	provider TEXT NOT NULL,
	host TEXT NOT NULL,
	scope_kind TEXT NOT NULL,
	scope_path TEXT NOT NULL DEFAULT '',
	filters TEXT NOT NULL DEFAULT '{}',
	persona TEXT NOT NULL DEFAULT '',
	event_filter TEXT NOT NULL DEFAULT '[]',
	report_existing INTEGER NOT NULL DEFAULT 0,
	max_watches INTEGER NOT NULL DEFAULT 25,
	release_when_unmatched INTEGER NOT NULL DEFAULT 0,
	status TEXT NOT NULL DEFAULT 'active',
	created_at INTEGER NOT NULL,
	next_scan_at INTEGER NOT NULL DEFAULT 0,
	last_scan_at INTEGER NOT NULL DEFAULT 0,
	scanned INTEGER NOT NULL DEFAULT 0,
	last_error TEXT NOT NULL DEFAULT ''
);

-- auto_discovery tracks, per (session_id, source_path) checkout, which
-- branch the auto-watch probe ("does this session's current branch have an
-- open PR/MR yet") has already answered for — see the Store methods below.
-- Unlike registrations, a row here is a pure cache with no user-facing
-- meaning, so it's fine (and necessary, to bound wasted API calls) to
-- actually delete a stale one, rather than the never-delete policy
-- registrations follows for its permanent watch history/badges. (An
-- existing database may still carry a vestigial next_due_at column from
-- the pacing scheme this replaced; nothing reads or writes it.)
CREATE TABLE IF NOT EXISTS auto_discovery (
	session_id TEXT NOT NULL,
	source_path TEXT NOT NULL,
	provider TEXT NOT NULL DEFAULT '',
	remote_key TEXT NOT NULL DEFAULT '',
	branch TEXT NOT NULL DEFAULT '',
	resolved_for_branch TEXT NOT NULL DEFAULT '',
	extra_remote_keys TEXT NOT NULL DEFAULT '[]',
	head_owners TEXT NOT NULL DEFAULT '[]',
	PRIMARY KEY (session_id, source_path)
);
`

// Open creates or attaches to the SQLite file at path. SQLite is
// single-writer; capping the pool at one connection avoids "database is
// locked" errors under the worker's own concurrent goroutines rather than
// papering over them with retry logic.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close() // best-effort cleanup; the schema error is what we report
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	if err := migrateAddedColumns(db); err != nil {
		_ = db.Close() // best-effort cleanup; the migration error is what we report
		return nil, fmt.Errorf("migrate schema: %w", err)
	}
	if err := migrateAutoDiscoveryColumns(db); err != nil {
		_ = db.Close() // best-effort cleanup; the migration error is what we report
		return nil, fmt.Errorf("migrate auto_discovery: %w", err)
	}
	// Runs after migrateAddedColumns: by this point an old database already
	// has every current column (just the wrong primary key), so the rebuild
	// below only ever needs to change the key, never the column set.
	if err := migrateCompositePrimaryKey(db); err != nil {
		_ = db.Close() // best-effort cleanup; the migration error is what we report
		return nil, fmt.Errorf("migrate primary key: %w", err)
	}
	return &Store{db: db}, nil
}

// columnMigrations are the ALTER TABLE statements for every column added
// after the original schema. CREATE TABLE IF NOT EXISTS only applies the
// current schema to a brand-new file; an already-existing table from an
// older version of this program needs each one explicitly. SQLite has no
// ADD COLUMN IF NOT EXISTS, so the idiomatic pattern is to attempt each and
// ignore the "duplicate column name" error a table that already has it
// returns. Ordered oldest first, though order doesn't matter since each is
// independent.
var columnMigrations = []string{
	`ALTER TABLE registrations ADD COLUMN latest_review_state TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE registrations ADD COLUMN title TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE registrations ADD COLUMN status TEXT NOT NULL DEFAULT 'watching'`,
	`ALTER TABLE registrations ADD COLUMN description TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE registrations ADD COLUMN ci_detail TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE registrations ADD COLUMN latest_reviewer TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE registrations ADD COLUMN latest_review_at INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE registrations ADD COLUMN merged_at INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE registrations ADD COLUMN has_conflict INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE registrations ADD COLUMN ci_href TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE registrations ADD COLUMN review_href TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE registrations ADD COLUMN merge_queue_state TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE registrations ADD COLUMN merge_queue_position INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE registrations ADD COLUMN blocked INTEGER NOT NULL DEFAULT 0`,
	// Supersedes the old `unresolved` (a plain count) column, which stays
	// in place unused rather than being dropped — same
	// never-destroy-a-column policy as everywhere else here. A plain count
	// couldn't tell a same-window resolve+new-thread apart from no change
	// at all; storing the actual thread IDs (as JSON) lets Diff compare by
	// identity instead.
	`ALTER TABLE registrations ADD COLUMN unresolved_thread_ids TEXT NOT NULL DEFAULT '[]'`,
	`ALTER TABLE registrations ADD COLUMN dismissed_review_href TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE registrations ADD COLUMN event_filter TEXT NOT NULL DEFAULT '[]'`,
	`ALTER TABLE registrations ADD COLUMN top_level_comment_ids TEXT NOT NULL DEFAULT '[]'`,
	`ALTER TABLE registrations ADD COLUMN latest_comment_href TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE registrations ADD COLUMN ci_was_failing INTEGER NOT NULL DEFAULT 0`,
	// 'null' (not '[]') means "no baseline yet", see Registration.ThreadCommentsKnown.
	`ALTER TABLE registrations ADD COLUMN thread_comment_ids TEXT NOT NULL DEFAULT 'null'`,
	// When the PR/MR was seen closed, for FinishedWatchWindow. Not part of
	// registrationColumns: only SQL reads it.
	`ALTER TABLE registrations ADD COLUMN closed_at INTEGER NOT NULL DEFAULT 0`,
	// 1 until the first poll has been taken as the baseline, see
	// Registration.SilentFirstPoll. Not part of registrationColumns either.
	`ALTER TABLE registrations ADD COLUMN silent_first_poll INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE registrations ADD COLUMN persona TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE registrations ADD COLUMN head_sha TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE registrations ADD COLUMN is_draft INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE registrations ADD COLUMN requested_reviewers TEXT NOT NULL DEFAULT '[]'`,
	`ALTER TABLE registrations ADD COLUMN self_review_state TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE registrations ADD COLUMN subscription_id TEXT NOT NULL DEFAULT ''`,
}

// autoDiscoveryColumnMigrations add the columns auto_discovery gained after
// its first version, same idea as columnMigrations for registrations.
var autoDiscoveryColumnMigrations = []string{
	`ALTER TABLE auto_discovery ADD COLUMN extra_remote_keys TEXT NOT NULL DEFAULT '[]'`,
	`ALTER TABLE auto_discovery ADD COLUMN head_owners TEXT NOT NULL DEFAULT '[]'`,
}

func migrateAutoDiscoveryColumns(db *sql.DB) error {
	for _, stmt := range autoDiscoveryColumnMigrations {
		if _, err := db.Exec(stmt); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
			return err
		}
	}
	return nil
}

func migrateAddedColumns(db *sql.DB) error {
	for _, stmt := range columnMigrations {
		if _, err := db.Exec(stmt); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
			return err
		}
	}
	// A registration closed before closed_at existed has no date: start its
	// window now rather than never watching it. Only touches undated rows.
	if _, err := db.Exec(`UPDATE registrations SET closed_at = strftime('%s', 'now') WHERE status = 'closed' AND closed_at = 0`); err != nil {
		return err
	}
	return nil
}

// migrateCompositePrimaryKey rebuilds the registrations table onto the
// (pr_url, session_id) composite key for a database created before multiple
// sessions could watch the same PR/MR, where pr_url alone was the primary
// key. SQLite has no ALTER TABLE for changing a primary key, so the
// idiomatic path is: create a new table with the target schema, copy every
// row across (position-for-position via SELECT *, since the rebuilt table
// keeps the exact same column order — only the trailing PRIMARY KEY clause
// differs), drop the old table, then rename. A pre-existing single-key
// table can only ever have one row per pr_url, so every row is guaranteed
// to satisfy the new, less restrictive composite key — nothing to
// deduplicate first. A no-op (checked via PRAGMA table_info) for a
// database that already has the composite key, whether freshly created
// under the current schema or already migrated by an earlier run.
func migrateCompositePrimaryKey(db *sql.DB) error {
	rows, err := db.Query(`PRAGMA table_info(registrations)`)
	if err != nil {
		return fmt.Errorf("inspect registrations schema: %w", err)
	}
	sessionIDIsPrimaryKey := false
	for rows.Next() {
		var cid, notNull, pk int
		var name, colType string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &colType, &notNull, &dflt, &pk); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan registrations schema: %w", err)
		}
		if name == "session_id" && pk > 0 {
			sessionIDIsPrimaryKey = true
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate registrations schema: %w", err)
	}
	_ = rows.Close()
	if sessionIDIsPrimaryKey {
		return nil
	}

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin primary key migration: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op once committed

	rebuildDDL := strings.Replace(schema, "CREATE TABLE IF NOT EXISTS registrations (", "CREATE TABLE registrations_new (", 1)
	if _, err := tx.Exec(rebuildDDL); err != nil {
		return fmt.Errorf("create composite-key table: %w", err)
	}
	// Named columns on both sides, not SELECT * — a real pre-existing
	// database can still carry the vestigial `unresolved` column
	// (superseded by unresolved_thread_ids; deliberately never dropped, see
	// columnMigrations), which isn't part of registrationColumns/schema at
	// all. SELECT * would copy it positionally into the wrong new column
	// (or just fail outright on a column-count mismatch, as it did here
	// against a pre-migration test database) instead of correctly leaving
	// it behind.
	if _, err := tx.Exec(`INSERT INTO registrations_new (` + registrationColumns + `) SELECT ` + registrationColumns + ` FROM registrations`); err != nil {
		return fmt.Errorf("copy rows to composite-key table: %w", err)
	}
	if _, err := tx.Exec(`DROP TABLE registrations`); err != nil {
		return fmt.Errorf("drop single-key table: %w", err)
	}
	if _, err := tx.Exec(`ALTER TABLE registrations_new RENAME TO registrations`); err != nil {
		return fmt.Errorf("rename composite-key table: %w", err)
	}
	return tx.Commit()
}

func (s *Store) Close() error { return s.db.Close() }

// Add registers sessionID's watch on a PR/MR, or refreshes its next_due_at,
// status, and event_filter if that same (pr_url, session_id) pair is already
// registered (idempotent, not an error) — so calling register again with a
// different filter is how that filter changes, no separate tool needed. A
// different session_id for the same pr_url is a distinct row, not a
// conflict — multiple sessions can independently watch the same PR/MR, each
// with its own state, filter, and notifications. Every call, insert or
// update, forces status back to StatusWatching: a fresh registration is
// always actively watched, and re-registering a PR/MR that had previously
// reached a terminal status (merged/closed/stopped) is how that PR/MR
// resumes being watched.
func (s *Store) Add(ctx context.Context, r Registration) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO registrations (pr_url, session_id, provider, review_count, ci_bucket, ci_detail, ci_href, review_href, unresolved_thread_ids, top_level_comment_ids, latest_comment_href, latest_review_state, latest_reviewer, latest_review_at, title, description, status, next_due_at, archived, created_at, merged_at, has_conflict, merge_queue_state, merge_queue_position, blocked, dismissed_review_href, event_filter, silent_first_poll, persona, subscription_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(pr_url, session_id) DO UPDATE SET next_due_at = excluded.next_due_at, status = excluded.status, event_filter = excluded.event_filter, persona = excluded.persona
	`, r.PRURL, r.SessionID, r.Provider, r.ReviewCount, r.CIBucket, r.CIDetail, r.CIHref, r.ReviewHref, marshalStrings(r.UnresolvedThreadIDs), marshalStrings(r.TopLevelCommentIDs), r.LatestCommentHref, r.LatestReviewState, r.LatestReviewer, unixOrZero(r.LatestReviewAt), r.Title, r.Description, StatusWatching, r.NextDueAt.Unix(), boolToInt(r.Archived), r.CreatedAt.Unix(), unixOrZero(r.MergedAt), boolToInt(r.HasConflict), r.MergeQueueState, r.MergeQueuePosition, boolToInt(r.Blocked), r.DismissedReviewHref, marshalStrings(r.EventFilter), boolToInt(r.SilentFirstPoll), r.Persona, r.SubscriptionID)
	if err != nil {
		return fmt.Errorf("add registration: %w", err)
	}
	return nil
}

// FirstPollSilent reports whether prURL's registration for sessionID is still
// waiting for its first poll to be taken as a silent baseline. Absent rows
// report false.
func (s *Store) FirstPollSilent(ctx context.Context, prURL, sessionID string) (bool, error) {
	var silent int
	err := s.db.QueryRowContext(ctx, `SELECT silent_first_poll FROM registrations WHERE pr_url = ? AND session_id = ?`, prURL, sessionID).Scan(&silent)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("first poll silent: %w", err)
	}
	return silent != 0, nil
}

// ClearFirstPollSilent ends the silent first poll: from here on the
// registration reports what changes.
func (s *Store) ClearFirstPollSilent(ctx context.Context, prURL, sessionID string) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE registrations SET silent_first_poll = 0 WHERE pr_url = ? AND session_id = ?`, prURL, sessionID); err != nil {
		return fmt.Errorf("clear first poll silent: %w", err)
	}
	return nil
}

// Get returns sessionID's own registration for prURL, if any — scoped to
// that session since prURL alone no longer identifies a single row (see
// schema's doc comment). The bool result is false (with a zero Registration
// and nil error) when no such registration exists — "not found" is not
// treated as an error condition, matching callers that need to distinguish
// "not registered" from a real query failure.
func (s *Store) Get(ctx context.Context, prURL, sessionID string) (Registration, bool, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+registrationColumns+` FROM registrations WHERE pr_url = ? AND session_id = ?`, prURL, sessionID)

	var r Registration
	var nextDue, created, latestReviewAt, mergedAt int64
	var archived, hasConflict, blocked, ciWasFailing, isDraft int
	var unresolvedThreadIDs, topLevelCommentIDs, eventFilter, threadCommentIDs, requestedReviewers string
	if err := row.Scan(&r.PRURL, &r.SessionID, &r.Provider, &r.ReviewCount, &r.CIBucket, &ciWasFailing, &r.CIDetail, &r.CIHref, &r.ReviewHref, &unresolvedThreadIDs, &topLevelCommentIDs, &r.LatestCommentHref, &r.LatestReviewState, &r.LatestReviewer, &latestReviewAt, &r.Title, &r.Description, &r.Status, &nextDue, &archived, &created, &mergedAt, &hasConflict, &r.MergeQueueState, &r.MergeQueuePosition, &blocked, &r.DismissedReviewHref, &eventFilter, &threadCommentIDs, &r.Persona, &r.HeadSHA, &isDraft, &requestedReviewers, &r.SelfReviewState, &r.SubscriptionID); err != nil {
		if err == sql.ErrNoRows {
			return Registration{}, false, nil
		}
		return Registration{}, false, fmt.Errorf("get registration: %w", err)
	}
	r.NextDueAt = time.Unix(nextDue, 0)
	r.Archived = archived != 0
	r.CreatedAt = time.Unix(created, 0)
	r.LatestReviewAt = zeroOrUnix(latestReviewAt)
	r.MergedAt = zeroOrUnix(mergedAt)
	r.HasConflict = hasConflict != 0
	r.CIWasFailing = ciWasFailing != 0
	r.Blocked = blocked != 0
	r.UnresolvedThreadIDs = unmarshalStrings(unresolvedThreadIDs)
	r.TopLevelCommentIDs = unmarshalStrings(topLevelCommentIDs)
	r.EventFilter = unmarshalStrings(eventFilter)
	r.ThreadCommentIDs, r.ThreadCommentsKnown = unmarshalBaseline(threadCommentIDs)
	r.IsDraft = isDraft != 0
	r.RequestedReviewers = unmarshalStrings(requestedReviewers)
	return r, true, nil
}

// SetStatus moves prURL to status unconditionally, without an ownership
// check — for the scheduler's own use (e.g. marking a PR closed, or
// stopping a registration whose session is gone), never exposed to the MCP
// tools directly. Marking a PR/MR merged goes through SetMerged instead, so
// its merge timestamp is never lost.
func (s *Store) SetStatus(ctx context.Context, prURL, status string) error {
	closedAt := int64(0)
	if status == StatusClosed {
		closedAt = time.Now().Unix()
	}
	_, err := s.db.ExecContext(ctx, `UPDATE registrations SET status = ?, closed_at = CASE WHEN ? != 0 THEN ? ELSE closed_at END WHERE pr_url = ?`, status, closedAt, closedAt, prURL)
	if err != nil {
		return fmt.Errorf("set status: %w", err)
	}
	return nil
}

// SetMerged moves prURL to StatusMerged and records when the merge
// happened, for the scheduler's own use once a check reports a merged
// PR/MR.
func (s *Store) SetMerged(ctx context.Context, prURL string, mergedAt time.Time) error {
	if mergedAt.IsZero() {
		mergedAt = time.Now() // the watch window runs from the merge; an unreported date counts as now
	}
	_, err := s.db.ExecContext(ctx, `UPDATE registrations SET status = ?, merged_at = ? WHERE pr_url = ?`, StatusMerged, unixOrZero(mergedAt), prURL)
	if err != nil {
		return fmt.Errorf("set merged: %w", err)
	}
	return nil
}

// SetStatusOwnedBy moves prURL to status only if it belongs to sessionID,
// reporting whether a row was actually updated. The check and the update
// are one statement so a concurrent re-register can't slip between them. A
// false result deliberately doesn't distinguish "not registered" from
// "registered by someone else", so a caller can't probe for which PRs/MRs
// other sessions are watching. The registration is never deleted — this
// mirrors the former RemoveOwnedBy's ownership semantics, but leaves the
// row in place at a terminal status instead.
func (s *Store) SetStatusOwnedBy(ctx context.Context, prURL, sessionID, status string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE registrations SET status = ? WHERE pr_url = ? AND session_id = ?`, status, prURL, sessionID)
	if err != nil {
		return false, fmt.Errorf("set owned status: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("set owned status: rows affected: %w", err)
	}
	return affected > 0, nil
}

// StopAllOwnedBy moves every not-already-stopped registration owned by
// sessionID to StatusStopped in one statement, reporting how many rows were
// actually changed — unregister's bulk mode (no pr_url given) uses this to
// clear everything a session owns without requiring one call per URL.
func (s *Store) StopAllOwnedBy(ctx context.Context, sessionID string) (int, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE registrations SET status = ? WHERE session_id = ? AND status != ?`, StatusStopped, sessionID, StatusStopped)
	if err != nil {
		return 0, fmt.Errorf("stop all owned: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("stop all owned: rows affected: %w", err)
	}
	return int(affected), nil
}

func (s *Store) List(ctx context.Context) ([]Registration, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+registrationColumns+` FROM registrations ORDER BY next_due_at ASC`)
	if err != nil {
		return nil, fmt.Errorf("query registrations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	regs, err := scanAll(rows)
	if err != nil {
		return nil, fmt.Errorf("scan registrations: %w", err)
	}
	return regs, nil
}

// DueBefore returns every actively-watched registration whose next_due_at
// has passed, earliest first, so the scheduler processes the most-overdue
// check first. Terminal-status registrations are excluded: they're never
// polled again, even if their old next_due_at is in the past.
func (s *Store) DueBefore(ctx context.Context, t time.Time) ([]Registration, error) {
	cutoff := t.Add(-FinishedWatchWindow).Unix()
	rows, err := s.db.QueryContext(ctx, `SELECT `+registrationColumns+` FROM registrations WHERE `+polledPredicate+` AND next_due_at <= ? ORDER BY next_due_at ASC`, StatusWatching, StatusClosed, cutoff, StatusMerged, cutoff, t.Unix())
	if err != nil {
		return nil, fmt.Errorf("query due registrations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	regs, err := scanAll(rows)
	if err != nil {
		return nil, fmt.Errorf("scan due registrations: %w", err)
	}
	return regs, nil
}

// FinishedWatchWindow is how long after it was merged or closed a PR/MR is
// still polled, to notice late comments and reviews and a reopen. After that
// it keeps its badge but is left alone.
const FinishedWatchWindow = 30 * 24 * time.Hour

// polledPredicate selects the registrations the scheduler polls: the watched
// ones plus the ones finished within FinishedWatchWindow. Arguments, in
// order: watching, closed, closed cutoff, merged, merged cutoff. A merged
// registration without a merge date (from before it was recorded) is not
// included.
const polledPredicate = `(status = ? OR (status = ? AND closed_at > ?) OR (status = ? AND merged_at > ?))`

// StateUpdate is the full set of poll-derived fields UpdateState persists —
// grouped into a struct rather than positional parameters since the list
// has grown past the point a caller can safely track by position alone.
type StateUpdate struct {
	ReviewCount         int
	CIBucket            string
	CIWasFailing        bool
	CIDetail            string
	CIHref              string
	ReviewHref          string
	UnresolvedThreadIDs []string
	TopLevelCommentIDs  []string
	ThreadCommentIDs    []string
	ThreadCommentsKnown bool // false keeps the baseline unknown (a reschedule-only update of a registration that never had one)
	LatestCommentHref   string
	LatestReviewState   string
	LatestReviewer      string
	LatestReviewAt      time.Time
	Title               string
	Description         string
	NextDueAt           time.Time
	HasConflict         bool
	MergeQueueState     string
	MergeQueuePosition  int
	Blocked             bool
	HeadSHA             string
	IsDraft             bool
	RequestedReviewers  []string
	SelfReviewState     string
}

// UpdateState persists one poll result for sessionID's own registration on
// prURL — scoped to that session, not every row sharing prURL: each
// session watching the same PR/MR tracks its own PrevState-relevant fields
// (e.g. DismissedReviewHref) independently, so an unscoped update would let
// whichever session's row the scheduler processes last in a tick clobber
// every other session's row sharing that PR/MR.
func (s *Store) UpdateState(ctx context.Context, prURL, sessionID string, u StateUpdate) error {
	_, err := s.db.ExecContext(ctx, `UPDATE registrations SET review_count = ?, ci_bucket = ?, ci_was_failing = ?, ci_detail = ?, ci_href = ?, review_href = ?, unresolved_thread_ids = ?, top_level_comment_ids = ?, latest_comment_href = ?, latest_review_state = ?, latest_reviewer = ?, latest_review_at = ?, title = ?, description = ?, next_due_at = ?, has_conflict = ?, merge_queue_state = ?, merge_queue_position = ?, blocked = ?, thread_comment_ids = ?, head_sha = ?, is_draft = ?, requested_reviewers = ?, self_review_state = ? WHERE pr_url = ? AND session_id = ?`,
		u.ReviewCount, u.CIBucket, boolToInt(u.CIWasFailing), u.CIDetail, u.CIHref, u.ReviewHref, marshalStrings(u.UnresolvedThreadIDs), marshalStrings(u.TopLevelCommentIDs), u.LatestCommentHref, u.LatestReviewState, u.LatestReviewer, unixOrZero(u.LatestReviewAt), u.Title, u.Description, u.NextDueAt.Unix(), boolToInt(u.HasConflict), u.MergeQueueState, u.MergeQueuePosition, boolToInt(u.Blocked), marshalBaseline(u.ThreadCommentIDs, u.ThreadCommentsKnown), u.HeadSHA, boolToInt(u.IsDraft), marshalStrings(u.RequestedReviewers), u.SelfReviewState, prURL, sessionID)
	if err != nil {
		return fmt.Errorf("update state: %w", err)
	}
	return nil
}

// CountByProvider returns the number of actively-watched PRs/MRs per
// provider, used by the scheduler to spread each rate-limit pool's budget
// across its real number of consumers rather than a guess. Terminal-status
// registrations don't consume any polling budget, so they're excluded.
func (s *Store) CountByProvider(ctx context.Context) (map[string]int, error) {
	cutoff := time.Now().Add(-FinishedWatchWindow).Unix()
	rows, err := s.db.QueryContext(ctx, `SELECT provider, COUNT(*) FROM registrations WHERE `+polledPredicate+` GROUP BY provider`, StatusWatching, StatusClosed, cutoff, StatusMerged, cutoff)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	counts := make(map[string]int)
	for rows.Next() {
		var provider string
		var count int
		if err := rows.Scan(&provider, &count); err != nil {
			return nil, err
		}
		counts[provider] = count
	}
	return counts, rows.Err()
}

func (s *Store) SetArchived(ctx context.Context, prURL string, archived bool) error {
	_, err := s.db.ExecContext(ctx, `UPDATE registrations SET archived = ? WHERE pr_url = ?`, boolToInt(archived), prURL)
	if err != nil {
		return fmt.Errorf("set archived: %w", err)
	}
	return nil
}

// ClearReviewIfCurrent blanks the four latest-review fields on sessionID's
// own registration for prURL, but only if href is still what's currently
// shown as its review — for dismiss_review's real-API-dismiss path, where
// GitHub itself now reports the review as DISMISSED and buildCheckResult
// already treats that as no review forever, so no dismissed_review_href
// marker is needed here (unlike SetDismissedReview below). Scoped to
// sessionID's own row: another session watching the same PR/MR didn't ask
// to dismiss anything, so its view of the review stays accurate — it'll
// pick up the real GitHub-side dismissal on its own next poll regardless,
// via the same DISMISSED-state filtering. If href isn't the currently-
// displayed review (an older review from a different reviewer, say),
// there's nothing locally to clear and this is a no-op.
func (s *Store) ClearReviewIfCurrent(ctx context.Context, prURL, sessionID, href string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE registrations SET latest_review_state = '', latest_reviewer = '', latest_review_at = 0, review_href = ''
		WHERE pr_url = ? AND session_id = ? AND review_href = ?`, prURL, sessionID, href)
	if err != nil {
		return fmt.Errorf("clear review if current: %w", err)
	}
	return nil
}

// SetDismissedReview marks href as suppressed for sessionID's own
// registration on prURL: the scheduler hides any future poll result whose
// ReviewHref still matches it for that session's row, since GitHub itself
// keeps reporting this exact review (a re-request doesn't change the
// original review's own state, and the local-only fallback never touches
// GitHub at all) and it would otherwise resurface on the very next tick.
// Scoped to sessionID's own row for the same reason as ClearReviewIfCurrent
// above — another session sharing this PR/MR made no dismiss request of
// its own. If href is currently displayed as this row's review, the four
// latest-review fields are blanked in the same statement — instant badge
// feedback instead of waiting for that next poll.
func (s *Store) SetDismissedReview(ctx context.Context, prURL, sessionID, href string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE registrations SET
			dismissed_review_href = ?,
			latest_review_state = CASE WHEN review_href = ? THEN '' ELSE latest_review_state END,
			latest_reviewer      = CASE WHEN review_href = ? THEN '' ELSE latest_reviewer END,
			latest_review_at     = CASE WHEN review_href = ? THEN 0  ELSE latest_review_at END,
			review_href          = CASE WHEN review_href = ? THEN '' ELSE review_href END
		WHERE pr_url = ? AND session_id = ?`, href, href, href, href, href, prURL, sessionID)
	if err != nil {
		return fmt.Errorf("set dismissed review: %w", err)
	}
	return nil
}

func scanAll(rows *sql.Rows) ([]Registration, error) {
	var out []Registration
	for rows.Next() {
		var r Registration
		var nextDue, created, latestReviewAt, mergedAt int64
		var archived, hasConflict, blocked, ciWasFailing, isDraft int
		var unresolvedThreadIDs, topLevelCommentIDs, eventFilter, threadCommentIDs, requestedReviewers string
		if err := rows.Scan(&r.PRURL, &r.SessionID, &r.Provider, &r.ReviewCount, &r.CIBucket, &ciWasFailing, &r.CIDetail, &r.CIHref, &r.ReviewHref, &unresolvedThreadIDs, &topLevelCommentIDs, &r.LatestCommentHref, &r.LatestReviewState, &r.LatestReviewer, &latestReviewAt, &r.Title, &r.Description, &r.Status, &nextDue, &archived, &created, &mergedAt, &hasConflict, &r.MergeQueueState, &r.MergeQueuePosition, &blocked, &r.DismissedReviewHref, &eventFilter, &threadCommentIDs, &r.Persona, &r.HeadSHA, &isDraft, &requestedReviewers, &r.SelfReviewState, &r.SubscriptionID); err != nil {
			return nil, fmt.Errorf("scan row: %w", err)
		}
		r.NextDueAt = time.Unix(nextDue, 0)
		r.Archived = archived != 0
		r.CreatedAt = time.Unix(created, 0)
		r.LatestReviewAt = zeroOrUnix(latestReviewAt)
		r.MergedAt = zeroOrUnix(mergedAt)
		r.HasConflict = hasConflict != 0
		r.CIWasFailing = ciWasFailing != 0
		r.Blocked = blocked != 0
		r.UnresolvedThreadIDs = unmarshalStrings(unresolvedThreadIDs)
		r.TopLevelCommentIDs = unmarshalStrings(topLevelCommentIDs)
		r.EventFilter = unmarshalStrings(eventFilter)
		r.ThreadCommentIDs, r.ThreadCommentsKnown = unmarshalBaseline(threadCommentIDs)
		r.IsDraft = isDraft != 0
		r.RequestedReviewers = unmarshalStrings(requestedReviewers)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("row iteration: %w", err)
	}
	return out, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// registrationColumns is the column list, in Registration/scanAll's fixed
// scan order, shared by every SELECT * from registrations — Get, List, and
// DueBefore must agree on exactly this order or Scan silently assigns the
// wrong value to the wrong field.
const registrationColumns = `pr_url, session_id, provider, review_count, ci_bucket, ci_was_failing, ci_detail, ci_href, review_href, unresolved_thread_ids, top_level_comment_ids, latest_comment_href, latest_review_state, latest_reviewer, latest_review_at, title, description, status, next_due_at, archived, created_at, merged_at, has_conflict, merge_queue_state, merge_queue_position, blocked, dismissed_review_href, event_filter, thread_comment_ids, persona, head_sha, is_draft, requested_reviewers, self_review_state, subscription_id`

// marshalStrings encodes a string slice as a JSON array for storage — "[]"
// for an empty/nil slice rather than "null", so unmarshalStrings never has
// to special-case the empty case on the way back in. Shared by
// unresolved_thread_ids and event_filter, both stored the same way.
func marshalStrings(ss []string) string {
	if len(ss) == 0 {
		return "[]"
	}
	b, err := json.Marshal(ss)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// unmarshalStrings reverses marshalStrings. Malformed or empty input (e.g. a
// pre-migration row that never had the column) reports nil, exactly
// equivalent to "none stored" — callers (Diff's thread-id comparison,
// eventsMessage's filter check) treat a nil and an empty slice identically.
func unmarshalStrings(s string) []string {
	var ss []string
	_ = json.Unmarshal([]byte(s), &ss)
	return ss
}

// marshalBaseline is unmarshalBaseline's inverse.
func marshalBaseline(ids []string, known bool) string {
	if !known {
		return "null"
	}
	return marshalStrings(ids)
}

// unmarshalBaseline reads thread_comment_ids: the stored literal 'null' (the
// column default) is "no baseline yet", anything else a known list, empty
// included.
func unmarshalBaseline(s string) ([]string, bool) {
	if s == "" || s == "null" {
		return nil, false
	}
	return unmarshalStrings(s), true
}

// unixOrZero converts t to a Unix timestamp, or 0 for the zero time — 0
// itself (1970-01-01) is never a real review/merge timestamp this program
// would ever store, so it doubles as the "unknown/not yet happened" sentinel
// zeroOrUnix reverses.
func unixOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

func zeroOrUnix(v int64) time.Time {
	if v == 0 {
		return time.Time{}
	}
	return time.Unix(v, 0)
}
