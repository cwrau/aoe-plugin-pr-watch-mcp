package registry

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// Registration is one watched PR/MR: which session to wake, where its
// last-seen state sits, and when the scheduler should check it next.
type Registration struct {
	PRURL       string
	SessionID   string
	Provider    string // "github" | "gitlab"
	ReviewCount int
	CIBucket    string
	Unresolved  int
	NextDueAt   time.Time
	Archived    bool
	CreatedAt   time.Time
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
	unresolved INTEGER NOT NULL DEFAULT 0,
	next_due_at INTEGER NOT NULL,
	archived INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL
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
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// Add registers a PR/MR, or updates its session_id and next_due_at if
// already registered (re-registering the same URL is idempotent, not an
// error).
func (s *Store) Add(ctx context.Context, r Registration) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO registrations (pr_url, session_id, provider, review_count, ci_bucket, unresolved, next_due_at, archived, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(pr_url) DO UPDATE SET session_id = excluded.session_id, next_due_at = excluded.next_due_at
	`, r.PRURL, r.SessionID, r.Provider, r.ReviewCount, r.CIBucket, r.Unresolved, r.NextDueAt.Unix(), boolToInt(r.Archived), r.CreatedAt.Unix())
	return err
}

func (s *Store) Remove(ctx context.Context, prURL string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM registrations WHERE pr_url = ?`, prURL)
	return err
}

func (s *Store) List(ctx context.Context) ([]Registration, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT pr_url, session_id, provider, review_count, ci_bucket, unresolved, next_due_at, archived, created_at FROM registrations ORDER BY next_due_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanAll(rows)
}

// DueBefore returns every registration whose next_due_at has passed,
// earliest first, so the scheduler processes the most-overdue check first.
func (s *Store) DueBefore(ctx context.Context, t time.Time) ([]Registration, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT pr_url, session_id, provider, review_count, ci_bucket, unresolved, next_due_at, archived, created_at FROM registrations WHERE next_due_at <= ? ORDER BY next_due_at ASC`, t.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanAll(rows)
}

func (s *Store) UpdateState(ctx context.Context, prURL string, reviewCount int, ciBucket string, unresolved int, nextDueAt time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE registrations SET review_count = ?, ci_bucket = ?, unresolved = ?, next_due_at = ? WHERE pr_url = ?`, reviewCount, ciBucket, unresolved, nextDueAt.Unix(), prURL)
	return err
}

func (s *Store) SetArchived(ctx context.Context, prURL string, archived bool) error {
	_, err := s.db.ExecContext(ctx, `UPDATE registrations SET archived = ? WHERE pr_url = ?`, boolToInt(archived), prURL)
	return err
}

func scanAll(rows *sql.Rows) ([]Registration, error) {
	var out []Registration
	for rows.Next() {
		var r Registration
		var nextDue, created int64
		var archived int
		if err := rows.Scan(&r.PRURL, &r.SessionID, &r.Provider, &r.ReviewCount, &r.CIBucket, &r.Unresolved, &nextDue, &archived, &created); err != nil {
			return nil, err
		}
		r.NextDueAt = time.Unix(nextDue, 0)
		r.Archived = archived != 0
		r.CreatedAt = time.Unix(created, 0)
		out = append(out, r)
	}
	return out, rows.Err()
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
