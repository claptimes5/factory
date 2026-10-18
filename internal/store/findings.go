package store

import (
	"context"
	"database/sql"
	"fmt"
)

// Finding outcomes: how a finding ended, or where it stands.
const (
	OutcomeOpen        = "open"         // it blocks, waiting for the builder or the next review
	OutcomeNote        = "note"         // it couldn't block: shown, not answered
	OutcomePreExisting = "pre-existing" // a problem the change didn't introduce
	OutcomeRefuted     = "refuted"      // the verifier refuted it: kept as a note
	OutcomeFixed       = "fixed"        // the builder fixed it (and no later review upheld it)
	OutcomeDisputed    = "disputed"     // the builder said it's wrong; no review has ruled yet
	OutcomeOutOfScope  = "out_of_scope" // the builder said it's out of scope
	OutcomeResolved    = "resolved"     // a later review found it gone, though the builder didn't say fixed
	OutcomeWithdrawn   = "withdrawn"    // a later review agreed it was wrong or out of scope
)

// ReviewFinding is one finding a review raised, and what became of it.
type ReviewFinding struct {
	ItemID   int64
	FID      string // F1
	Seq      int
	JobID    int64  // the review job that raised it
	Kind     string // review | critic
	Route    string
	Harness  string
	Model    string
	Round    int
	Severity string
	File     string
	Line     int
	Claim    string
	Evidence string
	Blocking bool

	Check       string // confirmed | refuted | unverified
	CheckReason string
	CheckBy     string
	Confirmed   bool

	Answer    string
	AnswerWhy string
	AnswerJob int64

	Ruling       string
	RulingReason string
	RulingJob    int64

	Outcome   string
	CreatedAt string
	UpdatedAt string
}

// FindingID is the id of an item's seq'th finding.
func FindingID(seq int) string { return fmt.Sprintf("F%d", seq) }

// NextFindingSeq is the seq an item's next finding gets: one past the
// highest any other job raised, so a job that runs again reuses its own.
func (s *Store) NextFindingSeq(ctx context.Context, itemID, jobID int64) (int, error) {
	var n int
	err := s.read.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) FROM review_findings WHERE item_id = ? AND job_id != ?`, itemID, jobID).Scan(&n)
	return n + 1, err
}

// SaveReview records one review's findings in one go: raised are the
// findings it raised (any it raised before, when it ran again, are
// replaced), and updated are earlier findings it ruled on or carried.
func (s *Store) SaveReview(ctx context.Context, jobID int64, raised, updated []ReviewFinding) error {
	now := Now()
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM review_findings WHERE job_id = ?`, jobID); err != nil {
			return err
		}
		for _, f := range raised {
			if f.Outcome == "" {
				f.Outcome = OutcomeOpen
			}
			if _, err := tx.Exec(`INSERT INTO review_findings(item_id, fid, seq, job_id, kind, route, harness, model, round, severity, file, line, claim, evidence,
				blocking, check_status, check_reason, check_by, confirmed, outcome, created_at, updated_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				f.ItemID, f.FID, f.Seq, jobID, f.Kind, f.Route, f.Harness, f.Model, f.Round, f.Severity, f.File, f.Line, f.Claim, f.Evidence,
				f.Blocking, f.Check, f.CheckReason, f.CheckBy, f.Confirmed, f.Outcome, now, now); err != nil {
				return err
			}
		}
		for _, f := range updated {
			if err := updateFinding(tx, f, now); err != nil {
				return err
			}
		}
		return nil
	})
}

func updateFinding(tx *sql.Tx, f ReviewFinding, now string) error {
	_, err := tx.Exec(`UPDATE review_findings SET blocking = ?, check_status = ?, check_reason = ?, check_by = ?, confirmed = ?,
		answer = ?, answer_why = ?, answer_job = ?, ruling = ?, ruling_reason = ?, ruling_job = ?, outcome = ?, updated_at = ?
		WHERE item_id = ? AND fid = ?`,
		f.Blocking, f.Check, f.CheckReason, f.CheckBy, f.Confirmed, f.Answer, f.AnswerWhy, f.AnswerJob,
		f.Ruling, f.RulingReason, f.RulingJob, f.Outcome, now, f.ItemID, f.FID)
	return err
}

// UpdateFindings saves the mutable columns of findings already recorded:
// a builder's answers, say.
func (s *Store) UpdateFindings(ctx context.Context, fs []ReviewFinding) error {
	now := Now()
	return s.tx(ctx, func(tx *sql.Tx) error {
		for _, f := range fs {
			if err := updateFinding(tx, f, now); err != nil {
				return err
			}
		}
		return nil
	})
}

const findingCols = `item_id, fid, seq, job_id, kind, route, harness, model, round, severity, file, line, claim, evidence, blocking,
	check_status, check_reason, check_by, confirmed, answer, answer_why, answer_job, ruling, ruling_reason, ruling_job, outcome, created_at, updated_at`

func (s *Store) findings(ctx context.Context, where string, args ...any) ([]ReviewFinding, error) {
	rows, err := s.read.QueryContext(ctx, `SELECT `+findingCols+` FROM review_findings `+where+` ORDER BY item_id, seq`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ReviewFinding
	for rows.Next() {
		var f ReviewFinding
		if err := rows.Scan(&f.ItemID, &f.FID, &f.Seq, &f.JobID, &f.Kind, &f.Route, &f.Harness, &f.Model, &f.Round, &f.Severity, &f.File, &f.Line,
			&f.Claim, &f.Evidence, &f.Blocking, &f.Check, &f.CheckReason, &f.CheckBy, &f.Confirmed, &f.Answer, &f.AnswerWhy, &f.AnswerJob,
			&f.Ruling, &f.RulingReason, &f.RulingJob, &f.Outcome, &f.CreatedAt, &f.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// ItemFindings are the findings jobs after sinceJob raised on an item
// (pass its latest triage job, so a fresh start leaves the old ones
// behind), in the order they were raised.
func (s *Store) ItemFindings(ctx context.Context, itemID, sinceJob int64) ([]ReviewFinding, error) {
	return s.findings(ctx, `WHERE item_id = ? AND job_id > ?`, itemID, sinceJob)
}

// AllFindings is every recorded finding, for the metrics.
func (s *Store) AllFindings(ctx context.Context) ([]ReviewFinding, error) {
	return s.findings(ctx, ``)
}
