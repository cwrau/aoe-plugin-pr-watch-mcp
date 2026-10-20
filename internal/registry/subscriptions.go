package registry

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"time"
)

const (
	SubscriptionActive  = "active"
	SubscriptionStopped = "stopped"
)

// Subscription is a watch scope: a repo, an org/group or a platform plus
// filters. The scheduler scans it, and every match becomes a normal
// Registration tagged with the subscription's ID.
type Subscription struct {
	ID                   string
	SessionID            string
	Provider             string // "github" | "gitlab"
	Host                 string
	ScopeKind            string // "repo" | "org" | "platform"
	ScopePath            string
	Filters              string // provider.ScanFilters as JSON; the registry does not interpret it
	Persona              string
	EventFilter          []string // applied to the watches it registers, as for register
	ReportExisting       bool     // ping the matches found by the first scan instead of registering them silently
	MaxWatches           int
	ReleaseWhenUnmatched bool // stop a watch once its PR/MR no longer matches, instead of following it to merge or close
	Status               string
	CreatedAt            time.Time
	NextScanAt           time.Time
	LastScanAt           time.Time
	Scanned              bool // at least one scan has succeeded
	LastError            string
}

const subscriptionColumns = `id, session_id, provider, host, scope_kind, scope_path, filters, persona, event_filter, report_existing, max_watches, release_when_unmatched, status, created_at, next_scan_at, last_scan_at, scanned, last_error`

func newSubscriptionID() (string, error) {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate subscription id: %w", err)
	}
	return "sub-" + hex.EncodeToString(b[:]), nil
}

// AddSubscription stores s, due for its first scan at once, and returns its
// generated ID.
func (s *Store) AddSubscription(ctx context.Context, sub Subscription) (string, error) {
	id, err := newSubscriptionID()
	if err != nil {
		return "", err
	}
	filters := sub.Filters
	if filters == "" {
		filters = "{}"
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO subscriptions (`+subscriptionColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, sub.SessionID, sub.Provider, sub.Host, sub.ScopeKind, sub.ScopePath, filters, sub.Persona, marshalStrings(sub.EventFilter),
		boolToInt(sub.ReportExisting), sub.MaxWatches, boolToInt(sub.ReleaseWhenUnmatched), SubscriptionActive, sub.CreatedAt.Unix(), 0, 0, 0, "")
	if err != nil {
		return "", fmt.Errorf("add subscription: %w", err)
	}
	return id, nil
}

type rowScanner interface{ Scan(dest ...any) error }

func scanSubscription(row rowScanner) (Subscription, error) {
	var sub Subscription
	var eventFilter string
	var reportExisting, release, scanned int
	var created, next, last int64
	if err := row.Scan(&sub.ID, &sub.SessionID, &sub.Provider, &sub.Host, &sub.ScopeKind, &sub.ScopePath, &sub.Filters, &sub.Persona, &eventFilter,
		&reportExisting, &sub.MaxWatches, &release, &sub.Status, &created, &next, &last, &scanned, &sub.LastError); err != nil {
		return Subscription{}, err
	}
	sub.EventFilter = unmarshalStrings(eventFilter)
	sub.ReportExisting = reportExisting != 0
	sub.ReleaseWhenUnmatched = release != 0
	sub.Scanned = scanned != 0
	sub.CreatedAt = time.Unix(created, 0)
	sub.NextScanAt = time.Unix(next, 0)
	sub.LastScanAt = zeroOrUnix(last)
	return sub, nil
}

// GetSubscription returns the subscription with id, if any.
func (s *Store) GetSubscription(ctx context.Context, id string) (Subscription, bool, error) {
	sub, err := scanSubscription(s.db.QueryRowContext(ctx, `SELECT `+subscriptionColumns+` FROM subscriptions WHERE id = ?`, id))
	if err == sql.ErrNoRows {
		return Subscription{}, false, nil
	}
	if err != nil {
		return Subscription{}, false, fmt.Errorf("get subscription: %w", err)
	}
	return sub, true, nil
}

func (s *Store) querySubscriptions(ctx context.Context, where string, args ...any) ([]Subscription, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+subscriptionColumns+` FROM subscriptions `+where, args...)
	if err != nil {
		return nil, fmt.Errorf("query subscriptions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Subscription
	for rows.Next() {
		sub, err := scanSubscription(rows)
		if err != nil {
			return nil, fmt.Errorf("scan subscription: %w", err)
		}
		out = append(out, sub)
	}
	return out, rows.Err()
}

// ListSubscriptions returns every subscription of every status, oldest first.
func (s *Store) ListSubscriptions(ctx context.Context) ([]Subscription, error) {
	return s.querySubscriptions(ctx, `ORDER BY created_at ASC, id ASC`)
}

// DueSubscriptions returns the active subscriptions whose next scan is due.
func (s *Store) DueSubscriptions(ctx context.Context, now time.Time) ([]Subscription, error) {
	return s.querySubscriptions(ctx, `WHERE status = ? AND next_scan_at <= ? ORDER BY next_scan_at ASC, id ASC`, SubscriptionActive, now.Unix())
}

// RecordScan stores the outcome of a scan and when the next one is due. A
// failed scan (errMsg set) keeps the first scan pending, so the matches an
// outage hid are still found silently rather than announced as new.
func (s *Store) RecordScan(ctx context.Context, id string, now, next time.Time, errMsg string) error {
	scannedClause := ""
	if errMsg == "" {
		scannedClause = ", scanned = 1"
	}
	_, err := s.db.ExecContext(ctx, `UPDATE subscriptions SET last_scan_at = ?, next_scan_at = ?, last_error = ?`+scannedClause+` WHERE id = ?`,
		now.Unix(), next.Unix(), errMsg, id)
	if err != nil {
		return fmt.Errorf("record scan: %w", err)
	}
	return nil
}

// stopSubscription ends the subscription and moves its still-watching
// registrations to stopped (never deleted). With a non-empty sessionID only
// that session's own subscription qualifies.
func (s *Store) stopSubscription(ctx context.Context, id, sessionID string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin stop subscription: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op once committed

	query := `UPDATE subscriptions SET status = ? WHERE id = ? AND status = ?`
	args := []any{SubscriptionStopped, id, SubscriptionActive}
	if sessionID != "" {
		query += ` AND session_id = ?`
		args = append(args, sessionID)
	}
	res, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return false, fmt.Errorf("stop subscription: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE registrations SET status = ? WHERE subscription_id = ? AND status = ?`, StatusStopped, id, StatusWatching); err != nil {
		return false, fmt.Errorf("stop subscription watches: %w", err)
	}
	return true, tx.Commit()
}

// StopSubscription stops sessionID's own subscription id and its watches.
// It reports false when there is no such active subscription of that session.
func (s *Store) StopSubscription(ctx context.Context, id, sessionID string) (bool, error) {
	if sessionID == "" {
		return false, fmt.Errorf("stop subscription: session id is required")
	}
	return s.stopSubscription(ctx, id, sessionID)
}

// StopSubscriptionAny stops a subscription whatever its owner, for the
// scheduler's own use (the owning session is gone).
func (s *Store) StopSubscriptionAny(ctx context.Context, id string) (bool, error) {
	return s.stopSubscription(ctx, id, "")
}

// CountSubscriptionWatches is how many watches the subscription has
// registered that are still being watched.
func (s *Store) CountSubscriptionWatches(ctx context.Context, id string) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM registrations WHERE subscription_id = ? AND status = ?`, id, StatusWatching).Scan(&n); err != nil {
		return 0, fmt.Errorf("count subscription watches: %w", err)
	}
	return n, nil
}

// SubscriptionWatchURLs lists the PR/MR URLs the subscription registered that
// are still being watched.
func (s *Store) SubscriptionWatchURLs(ctx context.Context, id string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT pr_url FROM registrations WHERE subscription_id = ? AND status = ? ORDER BY pr_url`, id, StatusWatching)
	if err != nil {
		return nil, fmt.Errorf("list subscription watches: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var urls []string
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			return nil, err
		}
		urls = append(urls, u)
	}
	return urls, rows.Err()
}
