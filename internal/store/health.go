package store

import (
	"context"
	"database/sql"
	"errors"
)

// Main health states of a repo's base branch.
const (
	HealthGreen      = "green"
	HealthRed        = "red"
	HealthFlaky      = "flaky"
	HealthPending    = "pending"
	HealthNoCI       = "no_ci"
	HealthUnreadable = "unreadable"
)

// RepoHealth is what the daemon last read of a repo's base branch CI.
type RepoHealth struct {
	Repo      string
	Branch    string
	HeadSHA   string
	State     string
	Detail    string // the red or flaky checks, or why CI can't be read
	CheckedAt string
	Notified  string // the no_ci or unreadable state already announced
}

// HealthIssue is a check that is red or flaky on a repo's base branch,
// and the issue filed for it (0 until one is).
type HealthIssue struct {
	Repo        string
	Check       string
	Kind        string // red | flaky
	IssueNumber int
	HeadSHA     string
	URL         string
	CreatedAt   string
}

// SetRepoHealth records a repo's latest reading.
func (s *Store) SetRepoHealth(ctx context.Context, h RepoHealth) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO main_health(repo, branch, head_sha, state, detail, checked_at, notified) VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(repo) DO UPDATE SET branch = excluded.branch, head_sha = excluded.head_sha, state = excluded.state,
			detail = excluded.detail, checked_at = excluded.checked_at, notified = excluded.notified`,
			h.Repo, h.Branch, h.HeadSHA, h.State, h.Detail, Now(), h.Notified)
		return err
	})
}

// RepoHealth is a repo's latest reading, or nil before the first.
func (s *Store) RepoHealth(ctx context.Context, repo string) (*RepoHealth, error) {
	var h RepoHealth
	err := s.read.QueryRowContext(ctx, `SELECT repo, branch, head_sha, state, detail, checked_at, notified FROM main_health WHERE repo = ?`, repo).
		Scan(&h.Repo, &h.Branch, &h.HeadSHA, &h.State, &h.Detail, &h.CheckedAt, &h.Notified)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &h, err
}

// RepoHealths lists every repo's latest reading, by repo.
func (s *Store) RepoHealths(ctx context.Context) ([]RepoHealth, error) {
	rows, err := s.read.QueryContext(ctx, `SELECT repo, branch, head_sha, state, detail, checked_at, notified FROM main_health ORDER BY repo`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RepoHealth
	for rows.Next() {
		var h RepoHealth
		if err := rows.Scan(&h.Repo, &h.Branch, &h.HeadSHA, &h.State, &h.Detail, &h.CheckedAt, &h.Notified); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// SetHealthIssue records, or updates, a repo's red or flaky check.
func (s *Store) SetHealthIssue(ctx context.Context, h HealthIssue) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO health_issues(repo, check_name, kind, issue_number, head_sha, url, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(repo, check_name) DO UPDATE SET kind = excluded.kind, issue_number = excluded.issue_number,
			head_sha = excluded.head_sha, url = excluded.url`,
			h.Repo, h.Check, h.Kind, h.IssueNumber, h.HeadSHA, h.URL, Now())
		return err
	})
}

// HealthIssues lists a repo's red and flaky checks ("" for every repo's),
// oldest first.
func (s *Store) HealthIssues(ctx context.Context, repo string) ([]HealthIssue, error) {
	rows, err := s.read.QueryContext(ctx, `SELECT repo, check_name, kind, issue_number, head_sha, url, created_at FROM health_issues
		WHERE ? = '' OR repo = ? ORDER BY created_at, check_name`, repo, repo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HealthIssue
	for rows.Next() {
		var h HealthIssue
		if err := rows.Scan(&h.Repo, &h.Check, &h.Kind, &h.IssueNumber, &h.HeadSHA, &h.URL, &h.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// IsHealthIssue says whether issue n of repo is one the factory filed for
// a red or flaky check that is still tracked.
func (s *Store) IsHealthIssue(ctx context.Context, repo string, n int) (bool, error) {
	if n <= 0 {
		return false, nil
	}
	var c int
	err := s.read.QueryRowContext(ctx, `SELECT COUNT(*) FROM health_issues WHERE repo = ? AND issue_number = ?`, repo, n).Scan(&c)
	return c > 0, err
}

// ClearHealthIssue forgets a check once it is green again.
func (s *Store) ClearHealthIssue(ctx context.Context, repo, check string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`DELETE FROM health_issues WHERE repo = ? AND check_name = ?`, repo, check)
		return err
	})
}

// CountEventsFor counts events of a type with this reason at or after a time.
func (s *Store) CountEventsFor(ctx context.Context, typ, reason, since string) (int, error) {
	var n int
	err := s.read.QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE type = ? AND reason = ? AND at >= ?`, typ, reason, since).Scan(&n)
	return n, err
}
