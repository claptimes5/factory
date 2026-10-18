package store

import (
	"context"
	"database/sql"
	"errors"
)

// Source states of a repo.
const (
	SourceOK         = "ok"
	SourceUnreadable = "unreadable"
)

// SourceState is what a source last read of a repo.
type SourceState struct {
	Repo     string
	Source   string
	State    string
	Detail   string // why it can't be read, or a note on the last read
	Found    int
	PolledAt string
	Notified string // the unreadable state already announced
}

// SourceIssue is an issue a source filed, and what for.
type SourceIssue struct {
	Repo        string
	Source      string
	Key         string
	IssueNumber int
	Title       string
	URL         string
	CreatedAt   string
	// BodyHash is the issue's text as filed (pipeline.IssueHash).
	BodyHash string
}

// SetSourceState records a source's latest read of a repo.
func (s *Store) SetSourceState(ctx context.Context, st SourceState) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO source_state(repo, source, state, detail, found, polled_at, notified) VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(repo, source) DO UPDATE SET state = excluded.state, detail = excluded.detail, found = excluded.found,
			polled_at = excluded.polled_at, notified = excluded.notified`,
			st.Repo, st.Source, st.State, st.Detail, st.Found, Now(), st.Notified)
		return err
	})
}

// SourceState is a source's latest read of a repo, or nil before the first.
func (s *Store) SourceState(ctx context.Context, repo, source string) (*SourceState, error) {
	var st SourceState
	err := s.read.QueryRowContext(ctx, `SELECT repo, source, state, detail, found, polled_at, notified FROM source_state WHERE repo = ? AND source = ?`, repo, source).
		Scan(&st.Repo, &st.Source, &st.State, &st.Detail, &st.Found, &st.PolledAt, &st.Notified)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &st, err
}

// SourceStates lists every source's latest read of every repo.
func (s *Store) SourceStates(ctx context.Context) ([]SourceState, error) {
	rows, err := s.read.QueryContext(ctx, `SELECT repo, source, state, detail, found, polled_at, notified FROM source_state ORDER BY source, repo`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SourceState
	for rows.Next() {
		var st SourceState
		if err := rows.Scan(&st.Repo, &st.Source, &st.State, &st.Detail, &st.Found, &st.PolledAt, &st.Notified); err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// SetSourceIssue records an issue a source filed.
func (s *Store) SetSourceIssue(ctx context.Context, si SourceIssue) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO source_issues(repo, source, key, issue_number, title, url, created_at, body_hash) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(repo, source, key) DO UPDATE SET issue_number = excluded.issue_number, title = excluded.title, url = excluded.url, body_hash = excluded.body_hash`,
			si.Repo, si.Source, si.Key, si.IssueNumber, si.Title, si.URL, Now(), si.BodyHash)
		return err
	})
}

// SourceIssues lists the issues a source filed for a repo, oldest first;
// "" for either lists them all.
func (s *Store) SourceIssues(ctx context.Context, repo, source string) ([]SourceIssue, error) {
	rows, err := s.read.QueryContext(ctx, `SELECT repo, source, key, issue_number, title, url, created_at, body_hash FROM source_issues
		WHERE (? = '' OR repo = ?) AND (? = '' OR source = ?) ORDER BY created_at, key`, repo, repo, source, source)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SourceIssue
	for rows.Next() {
		var si SourceIssue
		if err := rows.Scan(&si.Repo, &si.Source, &si.Key, &si.IssueNumber, &si.Title, &si.URL, &si.CreatedAt, &si.BodyHash); err != nil {
			return nil, err
		}
		out = append(out, si)
	}
	return out, rows.Err()
}

// ClearSourceIssue forgets an issue a source filed.
func (s *Store) ClearSourceIssue(ctx context.Context, repo, source, key string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`DELETE FROM source_issues WHERE repo = ? AND source = ? AND key = ?`, repo, source, key)
		return err
	})
}
