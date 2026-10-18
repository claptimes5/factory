package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// Proposal statuses. A new proposal supersedes a pending one.
const (
	ProposalPending    = "pending"
	ProposalApplied    = "applied"
	ProposalDismissed  = "dismissed"
	ProposalSuperseded = "superseded"
)

// ProposedChange is one setting the tuning loop proposed, with its
// evidence. Reason says why the daemon dropped it, on dropped ones.
type ProposedChange struct {
	Path   []string `json:"path"`
	Value  any      `json:"value"`
	Why    string   `json:"why"`
	Reason string   `json:"reason,omitempty"`
}

// Proposal is one run of the tuning loop: what it proposed for
// factory.yaml, the diff when it did, and what a human made of it.
type Proposal struct {
	ID         int64
	JobID      *int64
	Status     string
	Summary    string
	Changes    []ProposedChange // accepted: on the curated list, and the file still loads
	Dropped    []ProposedChange
	Ideas      []string // prompt and skill changes, in prose, for a human
	Diff       string
	PRURL      string
	PeriodFrom string
	PeriodTo   string
	CreatedAt  string
	DecidedAt  string
	DecidedBy  string
}

const proposalCols = `id, job_id, status, summary, changes, dropped, ideas, diff, pr_url, period_from, period_to, created_at, decided_at, decided_by`

func scanProposal(r scanner) (*Proposal, error) {
	var p Proposal
	var job sql.NullInt64
	var changes, dropped, ideas string
	if err := r.Scan(&p.ID, &job, &p.Status, &p.Summary, &changes, &dropped, &ideas, &p.Diff, &p.PRURL, &p.PeriodFrom, &p.PeriodTo, &p.CreatedAt, &p.DecidedAt, &p.DecidedBy); err != nil {
		return nil, err
	}
	if job.Valid {
		p.JobID = &job.Int64
	}
	for _, f := range []struct {
		s string
		v any
	}{{changes, &p.Changes}, {dropped, &p.Dropped}, {ideas, &p.Ideas}} {
		if err := json.Unmarshal([]byte(f.s), f.v); err != nil {
			return nil, fmt.Errorf("proposal %d: %w", p.ID, err)
		}
	}
	return &p, nil
}

// AddProposal stores a pending proposal and supersedes any older pending
// one, so at most one waits for a human.
func (s *Store) AddProposal(ctx context.Context, p Proposal) (int64, error) {
	enc := func(v any, empty string) string {
		b, _ := json.Marshal(v)
		if string(b) == "null" {
			return empty
		}
		return string(b)
	}
	var id int64
	err := s.tx(ctx, func(tx *sql.Tx) error {
		now := Now()
		if _, err := tx.Exec(`UPDATE tuning_proposals SET status = ?, decided_at = ?, decided_by = 'daemon' WHERE status = ?`, ProposalSuperseded, now, ProposalPending); err != nil {
			return err
		}
		res, err := tx.Exec(`INSERT INTO tuning_proposals(job_id, status, summary, changes, dropped, ideas, diff, pr_url, period_from, period_to, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, p.JobID, ProposalPending, p.Summary, enc(p.Changes, "[]"), enc(p.Dropped, "[]"), enc(p.Ideas, "[]"),
			p.Diff, p.PRURL, p.PeriodFrom, p.PeriodTo, now)
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		if err != nil {
			return err
		}
		return insertEvent(tx, Event{Actor: "daemon", JobID: p.JobID, Type: "tuning_proposed", Reason: p.Summary, Data: fmt.Sprintf(`{"proposal":%d}`, id)})
	})
	return id, err
}

// PendingProposal is the proposal waiting for a human, or nil.
func (s *Store) PendingProposal(ctx context.Context) (*Proposal, error) {
	p, err := scanProposal(s.read.QueryRowContext(ctx, `SELECT `+proposalCols+` FROM tuning_proposals WHERE status = ? ORDER BY id DESC LIMIT 1`, ProposalPending))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return p, err
}

// GetProposal is one proposal by id.
func (s *Store) GetProposal(ctx context.Context, id int64) (*Proposal, error) {
	return scanProposal(s.read.QueryRowContext(ctx, `SELECT `+proposalCols+` FROM tuning_proposals WHERE id = ?`, id))
}

// ErrNotPending is returned when a proposal was already applied,
// dismissed or superseded.
var ErrNotPending = errors.New("this proposal is no longer pending")

// DecideProposal moves a pending proposal to applied or dismissed, and
// records who did it.
func (s *Store) DecideProposal(ctx context.Context, id int64, status, actor string) error {
	if status != ProposalApplied && status != ProposalDismissed {
		return fmt.Errorf("a proposal is applied or dismissed, not %s", status)
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE tuning_proposals SET status = ?, decided_at = ?, decided_by = ? WHERE id = ? AND status = ?`, status, Now(), actor, id, ProposalPending)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrNotPending
		}
		return insertEvent(tx, Event{Actor: actor, Type: "tuning_" + status, Reason: fmt.Sprintf("proposal %d", id)})
	})
}

// SetProposalPR records the draft PR a proposal opened.
func (s *Store) SetProposalPR(ctx context.Context, id int64, url string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE tuning_proposals SET pr_url = ? WHERE id = ?`, url, id)
		return err
	})
}

// Mark records the time a named thing last happened, in TimeFormat.
func (s *Store) Mark(ctx context.Context, name, at string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO marks(name, at) VALUES (?, ?) ON CONFLICT(name) DO UPDATE SET at = excluded.at`, name, at)
		return err
	})
}

// Marked is the time Mark last recorded for name, or "" if never.
func (s *Store) Marked(ctx context.Context, name string) (string, error) {
	var at string
	err := s.read.QueryRowContext(ctx, `SELECT at FROM marks WHERE name = ?`, name).Scan(&at)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return at, err
}
