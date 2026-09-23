package registry

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
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
	PRURL               string
	SessionID           string
	Provider            string // "github" | "gitlab"
	ReviewCount         int
	CIBucket            string
	CIDetail            string   // e.g. the failing check/job name; empty if not applicable/unknown
	CIHref              string   // link to the relevant job, or failing that the pipeline/run itself; empty if unknown
	ReviewHref          string   // link to the latest review/comment; empty if unknown
	UnresolvedThreadIDs []string // IDs of threads unresolved as of the last check; compared by identity so a same-window resolve+new-thread can't hide the new one
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
}

type Store struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS registrations (
	pr_url TEXT PRIMARY KEY,
	session_id TEXT NOT NULL,
	provider TEXT NOT NULL,
	review_count INTEGER NOT NULL DEFAULT 0,
	ci_bucket TEXT NOT NULL DEFAULT 'none',
	ci_detail TEXT NOT NULL DEFAULT '',
	ci_href TEXT NOT NULL DEFAULT '',
	review_href TEXT NOT NULL DEFAULT '',
	unresolved_thread_ids TEXT NOT NULL DEFAULT '[]',
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
	blocked INTEGER NOT NULL DEFAULT 0
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
}

func migrateAddedColumns(db *sql.DB) error {
	for _, stmt := range columnMigrations {
		if _, err := db.Exec(stmt); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
			return err
		}
	}
	return nil
}

func (s *Store) Close() error { return s.db.Close() }

// Add registers a PR/MR, or updates its session_id, next_due_at and status
// if already registered (re-registering the same URL is idempotent, not an
// error). This upsert does not enforce ownership — whether a caller may
// take over an existing URL's session_id is decided one layer up, in the
// register tool, which rejects a cross-session reassignment outright.
// Every call, insert or update, forces status back to StatusWatching: a
// fresh registration is always actively watched, and re-registering a PR/MR
// that had previously reached a terminal status (merged/closed/stopped) is
// how that PR/MR resumes being watched.
func (s *Store) Add(ctx context.Context, r Registration) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO registrations (pr_url, session_id, provider, review_count, ci_bucket, ci_detail, ci_href, review_href, unresolved_thread_ids, latest_review_state, latest_reviewer, latest_review_at, title, description, status, next_due_at, archived, created_at, merged_at, has_conflict, merge_queue_state, merge_queue_position, blocked)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(pr_url) DO UPDATE SET session_id = excluded.session_id, next_due_at = excluded.next_due_at, status = excluded.status
	`, r.PRURL, r.SessionID, r.Provider, r.ReviewCount, r.CIBucket, r.CIDetail, r.CIHref, r.ReviewHref, marshalThreadIDs(r.UnresolvedThreadIDs), r.LatestReviewState, r.LatestReviewer, unixOrZero(r.LatestReviewAt), r.Title, r.Description, StatusWatching, r.NextDueAt.Unix(), boolToInt(r.Archived), r.CreatedAt.Unix(), unixOrZero(r.MergedAt), boolToInt(r.HasConflict), r.MergeQueueState, r.MergeQueuePosition, boolToInt(r.Blocked))
	if err != nil {
		return fmt.Errorf("add registration: %w", err)
	}
	return nil
}

// Get returns the registration for prURL, if any. The bool result is false
// (with a zero Registration and nil error) when no registration exists for
// that URL — "not found" is not treated as an error condition, matching
// callers that need to distinguish "not registered" from a real query
// failure (e.g. unregister's ownership check).
func (s *Store) Get(ctx context.Context, prURL string) (Registration, bool, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+registrationColumns+` FROM registrations WHERE pr_url = ?`, prURL)

	var r Registration
	var nextDue, created, latestReviewAt, mergedAt int64
	var archived, hasConflict, blocked int
	var unresolvedThreadIDs string
	if err := row.Scan(&r.PRURL, &r.SessionID, &r.Provider, &r.ReviewCount, &r.CIBucket, &r.CIDetail, &r.CIHref, &r.ReviewHref, &unresolvedThreadIDs, &r.LatestReviewState, &r.LatestReviewer, &latestReviewAt, &r.Title, &r.Description, &r.Status, &nextDue, &archived, &created, &mergedAt, &hasConflict, &r.MergeQueueState, &r.MergeQueuePosition, &blocked); err != nil {
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
	r.Blocked = blocked != 0
	r.UnresolvedThreadIDs = unmarshalThreadIDs(unresolvedThreadIDs)
	return r, true, nil
}

// SetStatus moves prURL to status unconditionally, without an ownership
// check — for the scheduler's own use (e.g. marking a PR closed, or
// stopping a registration whose session is gone), never exposed to the MCP
// tools directly. Marking a PR/MR merged goes through SetMerged instead, so
// its merge timestamp is never lost.
func (s *Store) SetStatus(ctx context.Context, prURL, status string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE registrations SET status = ? WHERE pr_url = ?`, status, prURL)
	if err != nil {
		return fmt.Errorf("set status: %w", err)
	}
	return nil
}

// SetMerged moves prURL to StatusMerged and records when the merge
// happened, for the scheduler's own use once a check reports a merged
// PR/MR.
func (s *Store) SetMerged(ctx context.Context, prURL string, mergedAt time.Time) error {
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

// StopOtherMerged moves every StatusMerged registration owned by sessionID
// to StatusStopped, except keepPRURL (the registration just (re-)registered,
// so a re-register of an already-merged PR/MR doesn't immediately stop
// itself). Called from the register handler so a session that moves on to a
// new PR/MR doesn't keep an old merged badge cluttering its row/pane
// forever — the old registration is never deleted, just excluded from
// display the same way an explicit unregister excludes it.
func (s *Store) StopOtherMerged(ctx context.Context, sessionID, keepPRURL string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE registrations SET status = ? WHERE session_id = ? AND status = ? AND pr_url != ?`,
		StatusStopped, sessionID, StatusMerged, keepPRURL)
	if err != nil {
		return fmt.Errorf("stop other merged registrations: %w", err)
	}
	return nil
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
	rows, err := s.db.QueryContext(ctx, `SELECT `+registrationColumns+` FROM registrations WHERE status = ? AND next_due_at <= ? ORDER BY next_due_at ASC`, StatusWatching, t.Unix())
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

// StateUpdate is the full set of poll-derived fields UpdateState persists —
// grouped into a struct rather than positional parameters since the list
// has grown past the point a caller can safely track by position alone.
type StateUpdate struct {
	ReviewCount         int
	CIBucket            string
	CIDetail            string
	CIHref              string
	ReviewHref          string
	UnresolvedThreadIDs []string
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
}

func (s *Store) UpdateState(ctx context.Context, prURL string, u StateUpdate) error {
	_, err := s.db.ExecContext(ctx, `UPDATE registrations SET review_count = ?, ci_bucket = ?, ci_detail = ?, ci_href = ?, review_href = ?, unresolved_thread_ids = ?, latest_review_state = ?, latest_reviewer = ?, latest_review_at = ?, title = ?, description = ?, next_due_at = ?, has_conflict = ?, merge_queue_state = ?, merge_queue_position = ?, blocked = ? WHERE pr_url = ?`,
		u.ReviewCount, u.CIBucket, u.CIDetail, u.CIHref, u.ReviewHref, marshalThreadIDs(u.UnresolvedThreadIDs), u.LatestReviewState, u.LatestReviewer, unixOrZero(u.LatestReviewAt), u.Title, u.Description, u.NextDueAt.Unix(), boolToInt(u.HasConflict), u.MergeQueueState, u.MergeQueuePosition, boolToInt(u.Blocked), prURL)
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
	rows, err := s.db.QueryContext(ctx, `SELECT provider, COUNT(*) FROM registrations WHERE status = ? GROUP BY provider`, StatusWatching)
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

func scanAll(rows *sql.Rows) ([]Registration, error) {
	var out []Registration
	for rows.Next() {
		var r Registration
		var nextDue, created, latestReviewAt, mergedAt int64
		var archived, hasConflict, blocked int
		var unresolvedThreadIDs string
		if err := rows.Scan(&r.PRURL, &r.SessionID, &r.Provider, &r.ReviewCount, &r.CIBucket, &r.CIDetail, &r.CIHref, &r.ReviewHref, &unresolvedThreadIDs, &r.LatestReviewState, &r.LatestReviewer, &latestReviewAt, &r.Title, &r.Description, &r.Status, &nextDue, &archived, &created, &mergedAt, &hasConflict, &r.MergeQueueState, &r.MergeQueuePosition, &blocked); err != nil {
			return nil, fmt.Errorf("scan row: %w", err)
		}
		r.NextDueAt = time.Unix(nextDue, 0)
		r.Archived = archived != 0
		r.CreatedAt = time.Unix(created, 0)
		r.LatestReviewAt = zeroOrUnix(latestReviewAt)
		r.MergedAt = zeroOrUnix(mergedAt)
		r.HasConflict = hasConflict != 0
		r.Blocked = blocked != 0
		r.UnresolvedThreadIDs = unmarshalThreadIDs(unresolvedThreadIDs)
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
const registrationColumns = `pr_url, session_id, provider, review_count, ci_bucket, ci_detail, ci_href, review_href, unresolved_thread_ids, latest_review_state, latest_reviewer, latest_review_at, title, description, status, next_due_at, archived, created_at, merged_at, has_conflict, merge_queue_state, merge_queue_position, blocked`

// marshalThreadIDs encodes ids as a JSON array for storage — "[]" for an
// empty/nil slice rather than "null", so unmarshalThreadIDs never has to
// special-case the empty case on the way back in.
func marshalThreadIDs(ids []string) string {
	if len(ids) == 0 {
		return "[]"
	}
	b, err := json.Marshal(ids)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// unmarshalThreadIDs reverses marshalThreadIDs. Malformed or empty input
// (e.g. a pre-migration row that never had this column) reports nil,
// exactly equivalent to "no threads known" — Diff treats a nil and an
// empty slice identically.
func unmarshalThreadIDs(s string) []string {
	var ids []string
	_ = json.Unmarshal([]byte(s), &ids)
	return ids
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
