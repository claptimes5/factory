package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

// Grooming proposal kinds.
const (
	GroomDuplicate = "duplicate"
	GroomStale     = "stale"
	GroomSplit     = "split"
)

// Grooming proposal statuses.
const (
	GroomOpen      = "open"
	GroomDismissed = "dismissed" // a human took its label off
	GroomDone      = "done"      // confirmed and acted on
	GroomGone      = "gone"      // the issue closed, or the factory took it up, first
)

// GroomProposal is what backlog grooming proposed for one issue.
type GroomProposal struct {
	ID          int64
	Repo        string
	IssueNumber int
	Kind        string
	Key         string
	IssueHash   string
	Status      string
	Canonical   int
	Why         string
	Children    []GroomChild
	JobID       *int64
	CreatedAt   string
	DecidedAt   string
	DecidedBy   string
}

// GroomChild is a proposed sub-issue, with its number once filed.
type GroomChild struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	Issue int    `json:"issue,omitempty"`
}

// GroomRun is a repo's last groom run.
type GroomRun struct {
	Repo    string
	JobID   *int64
	RanAt   string
	Read    int
	Flagged int
	Note    string
}

const groomCols = `id, repo, issue_number, kind, key, issue_hash, status, canonical, why, children, job_id, created_at, decided_at, decided_by`

func scanGroom(r scanner) (*GroomProposal, error) {
	var g GroomProposal
	var children string
	var job sql.NullInt64
	if err := r.Scan(&g.ID, &g.Repo, &g.IssueNumber, &g.Kind, &g.Key, &g.IssueHash, &g.Status, &g.Canonical, &g.Why, &children, &job, &g.CreatedAt, &g.DecidedAt, &g.DecidedBy); err != nil {
		return nil, err
	}
	if job.Valid {
		g.JobID = &job.Int64
	}
	_ = json.Unmarshal([]byte(children), &g.Children)
	return &g, nil
}

func childrenJSON(cs []GroomChild) string {
	if cs == nil {
		cs = []GroomChild{}
	}
	b, _ := json.Marshal(cs)
	return string(b)
}

// AddGroomProposal records an open proposal. An issue has at most one
// open at a time.
func (s *Store) AddGroomProposal(ctx context.Context, g GroomProposal) (int64, error) {
	var id int64
	err := s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.Exec(`INSERT INTO groom_proposals(repo, issue_number, kind, key, issue_hash, canonical, why, children, job_id, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			g.Repo, g.IssueNumber, g.Kind, g.Key, g.IssueHash, g.Canonical, g.Why, childrenJSON(g.Children), g.JobID, Now())
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	})
	return id, err
}

func (s *Store) groomProposals(ctx context.Context, q string, args ...any) ([]GroomProposal, error) {
	rows, err := s.read.QueryContext(ctx, `SELECT `+groomCols+` FROM groom_proposals `+q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GroomProposal
	for rows.Next() {
		g, err := scanGroom(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *g)
	}
	return out, rows.Err()
}

// GroomProposals lists a repo's proposals for an issue, oldest first; an
// issue of 0 lists the repo's, and a repo of "" every repo's.
func (s *Store) GroomProposals(ctx context.Context, repo string, issue int) ([]GroomProposal, error) {
	return s.groomProposals(ctx, `WHERE (? = '' OR repo = ? COLLATE NOCASE) AND (? = 0 OR issue_number = ?) ORDER BY id`, repo, repo, issue, issue)
}

// OpenGroomProposals lists the open proposals of a repo, or of every repo
// with "".
func (s *Store) OpenGroomProposals(ctx context.Context, repo string) ([]GroomProposal, error) {
	return s.groomProposals(ctx, `WHERE status = 'open' AND (? = '' OR repo = ? COLLATE NOCASE) ORDER BY id`, repo, repo)
}

// DecideGroomProposal moves an open proposal to a final status.
func (s *Store) DecideGroomProposal(ctx context.Context, id int64, status, by string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE groom_proposals SET status = ?, decided_at = ?, decided_by = ? WHERE id = ? AND status = 'open'`, status, Now(), by, id)
		return err
	})
}

// SetGroomChildren saves a split's sub-issues, as filed so far.
func (s *Store) SetGroomChildren(ctx context.Context, id int64, cs []GroomChild) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE groom_proposals SET children = ? WHERE id = ?`, childrenJSON(cs), id)
		return err
	})
}

// AddGroomChild records a sub-issue a split filed under parent.
func (s *Store) AddGroomChild(ctx context.Context, repo string, issue, parent int) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO groom_children(repo, issue_number, parent, created_at) VALUES (?, ?, ?, ?) ON CONFLICT DO NOTHING`, repo, issue, parent, Now())
		return err
	})
}

// GroomParent is the epic issue a split filed issue under, or 0.
func (s *Store) GroomParent(ctx context.Context, repo string, issue int) (int, error) {
	var parent int
	err := s.read.QueryRowContext(ctx, `SELECT parent FROM groom_children WHERE repo = ? COLLATE NOCASE AND issue_number = ?`, repo, issue).Scan(&parent)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return parent, err
}

// GroomChildren lists the sub-issues still waiting for approval: a repo's
// (every repo's with "") under a parent (any with 0), as repo and number.
func (s *Store) GroomChildren(ctx context.Context, repo string, parent int) ([]GroomChildRow, error) {
	rows, err := s.read.QueryContext(ctx, `SELECT repo, issue_number, parent FROM groom_children WHERE (? = '' OR repo = ? COLLATE NOCASE) AND (? = 0 OR parent = ?) ORDER BY repo, issue_number`,
		repo, repo, parent, parent)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GroomChildRow
	for rows.Next() {
		var c GroomChildRow
		if err := rows.Scan(&c.Repo, &c.Issue, &c.Parent); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// GroomChildRow is a sub-issue waiting for approval.
type GroomChildRow struct {
	Repo          string
	Issue, Parent int
}

// ClearGroomChild forgets a sub-issue: it was approved, or closed.
func (s *Store) ClearGroomChild(ctx context.Context, repo string, issue int) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`DELETE FROM groom_children WHERE repo = ? COLLATE NOCASE AND issue_number = ?`, repo, issue)
		return err
	})
}

// SetGroomRun records a repo's latest groom run.
func (s *Store) SetGroomRun(ctx context.Context, r GroomRun) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO groom_runs(repo, job_id, ran_at, read, flagged, note) VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT(repo) DO UPDATE SET job_id = excluded.job_id, ran_at = excluded.ran_at, read = excluded.read, flagged = excluded.flagged, note = excluded.note`,
			r.Repo, r.JobID, Now(), r.Read, r.Flagged, r.Note)
		return err
	})
}

// GroomRuns lists every repo's latest groom run.
func (s *Store) GroomRuns(ctx context.Context) ([]GroomRun, error) {
	rows, err := s.read.QueryContext(ctx, `SELECT repo, job_id, ran_at, read, flagged, note FROM groom_runs ORDER BY repo`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GroomRun
	for rows.Next() {
		var r GroomRun
		var job sql.NullInt64
		if err := rows.Scan(&r.Repo, &job, &r.RanAt, &r.Read, &r.Flagged, &r.Note); err != nil {
			return nil, err
		}
		if job.Valid {
			r.JobID = &job.Int64
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// DeleteGroomProposal forgets a proposal that was never posted.
func (s *Store) DeleteGroomProposal(ctx context.Context, id int64) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`DELETE FROM groom_proposals WHERE id = ?`, id)
		return err
	})
}

// GroomSplit says whether a confirmed split made this issue an epic.
func (s *Store) GroomSplit(ctx context.Context, repo string, issue int) (bool, error) {
	var n int
	err := s.read.QueryRowContext(ctx, `SELECT COUNT(*) FROM groom_proposals WHERE repo = ? COLLATE NOCASE AND issue_number = ? AND kind = 'split' AND status = 'done'`, repo, issue).Scan(&n)
	return n > 0, err
}
