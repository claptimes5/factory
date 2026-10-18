package store

import (
	"context"
	"database/sql"
	"strings"
)

// Signal is one thing a learn job can learn from: the changes a human
// pushed to a factory PR before merging it (kind human_edit), or one
// blocking reviewer or critic finding (kind finding).
type Signal struct {
	ID          int64
	Repo        string
	Kind        string // human_edit | finding
	ItemID      int64
	IssueNumber int
	JobID       int64 // the review or critic job of a finding
	PRNumber    int
	File        string
	Claim       string
	Severity    string
	Evidence    string
	Diff        string // a human edit's changes; "" until the learn job reads them
	FromSHA     string // a human edit: the factory's last push
	HeadSHA     string // and the head the PR merged at
	CreatedAt   string
	LearnJob    int64 // the learn job that used it; 0 while unused
}

// Signal kinds.
const (
	SignalHumanEdit = "human_edit"
	SignalFinding   = "finding"
)

// AddSignal records a signal. The same one recorded again (a retried
// review job, a second poll) is ignored. The repo is kept lowercased.
func (s *Store) AddSignal(ctx context.Context, sig Signal) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO lesson_signals(repo, kind, item_id, issue_number, job_id, pr_number, file, claim, severity, evidence, diff, from_sha, head_sha, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`,
			strings.ToLower(sig.Repo), sig.Kind, sig.ItemID, sig.IssueNumber, sig.JobID, sig.PRNumber, sig.File, sig.Claim, sig.Severity, sig.Evidence, sig.Diff, sig.FromSHA, sig.HeadSHA, Now())
		return err
	})
}

// SetSignalDiff saves the changes of a human edit, once read.
func (s *Store) SetSignalDiff(ctx context.Context, id int64, diff string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE lesson_signals SET diff = ? WHERE id = ?`, diff, id)
		return err
	})
}

const signalCols = `id, repo, kind, COALESCE(item_id,0), issue_number, job_id, pr_number, file, claim, severity, evidence, diff, from_sha, head_sha, created_at, COALESCE(learn_job,0)`

func (s *Store) signals(ctx context.Context, q string, args ...any) ([]Signal, error) {
	rows, err := s.read.QueryContext(ctx, `SELECT `+signalCols+` FROM lesson_signals `+q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Signal
	for rows.Next() {
		var g Signal
		if err := rows.Scan(&g.ID, &g.Repo, &g.Kind, &g.ItemID, &g.IssueNumber, &g.JobID, &g.PRNumber, &g.File, &g.Claim, &g.Severity, &g.Evidence, &g.Diff, &g.FromSHA, &g.HeadSHA, &g.CreatedAt, &g.LearnJob); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// UnusedSignals lists a repo's signals no learn job has used, recorded at
// or after since (TimeFormat; "" for all), oldest first.
func (s *Store) UnusedSignals(ctx context.Context, repo, since string) ([]Signal, error) {
	return s.signals(ctx, unusedSignals, strings.ToLower(repo), since)
}

// unusedSignals matches lesson_signals_unused: the repo is stored
// lowercased, so no COLLATE, which the index couldn't serve.
const unusedSignals = `WHERE repo = ? AND learn_job IS NULL AND created_at >= ? ORDER BY id`

// SignalsByID returns the signals with these ids, oldest first.
func (s *Store) SignalsByID(ctx context.Context, ids []int64) ([]Signal, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	return s.signals(ctx, `WHERE id IN (?`+strings.Repeat(",?", len(ids)-1)+`) ORDER BY id`, args...)
}

// UseSignals marks signals as used by a learn job.
func (s *Store) UseSignals(ctx context.Context, ids []int64, learnJob int64) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		for _, id := range ids {
			if _, err := tx.Exec(`UPDATE lesson_signals SET learn_job = ? WHERE id = ?`, learnJob, id); err != nil {
				return err
			}
		}
		return nil
	})
}

// Lesson is a line the factory proposed for a repo's AGENTS.md or CLAUDE.md.
type Lesson struct {
	ID        int64
	Repo      string
	Line      string
	Norm      string // the line as dedup compares it
	PRNumber  int
	Status    string // proposed | merged | declined
	CreatedAt string
	DecidedAt string
}

// Lesson statuses.
const (
	LessonProposed = "proposed"
	LessonMerged   = "merged"
	LessonDeclined = "declined"
)

// AddLesson records a proposed line. One already known by its norm keeps
// its status; a proposed one moves to the PR that proposes it now (it is
// recorded before its PR is opened, with PR 0, and again after).
func (s *Store) AddLesson(ctx context.Context, l Lesson) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO lessons(repo, line, norm, pr_number, created_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (repo, norm) DO UPDATE SET pr_number = excluded.pr_number, line = excluded.line WHERE lessons.status = 'proposed'`,
			strings.ToLower(l.Repo), l.Line, l.Norm, l.PRNumber, Now())
		return err
	})
}

// Lessons lists a repo's proposed lines, oldest first.
func (s *Store) Lessons(ctx context.Context, repo string) ([]Lesson, error) {
	rows, err := s.read.QueryContext(ctx, `SELECT id, repo, line, norm, pr_number, status, created_at, decided_at FROM lessons WHERE repo = ? ORDER BY id`, strings.ToLower(repo))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Lesson
	for rows.Next() {
		var l Lesson
		if err := rows.Scan(&l.ID, &l.Repo, &l.Line, &l.Norm, &l.PRNumber, &l.Status, &l.CreatedAt, &l.DecidedAt); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// DecideLesson sets what became of a proposed line.
func (s *Store) DecideLesson(ctx context.Context, id int64, status string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE lessons SET status = ?, decided_at = ? WHERE id = ?`, status, Now(), id)
		return err
	})
}
