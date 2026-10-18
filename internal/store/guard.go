package store

import (
	"context"
	"database/sql"
	"errors"
)

// FactoryMerge is one factory PR that merged, and what happened to it on
// the base branch since.
type FactoryMerge struct {
	Repo     string
	PR       int
	ItemID   int64
	MergeSHA string // the merge or squash commit on the base branch
	// SquashSHA is GitLab's squash commit when it made a merge commit
	// too: a revert may name either.
	SquashSHA string
	HeadSHA   string // the PR's head when it merged
	Branch    string
	Title     string
	Auto      bool // merged by the auto-merge the factory turned on
	MergedAt  string
	// RevertedAt is when the factory saw a commit or PR revert it, and
	// RevertedBy which one; "" while it stands.
	RevertedAt string
	RevertedBy string
	// RedAt is when main health saw a check go red on MergeSHA, the
	// first commit it failed on; RedCheck is that check.
	RedAt    string
	RedCheck string
}

const mergeCols = `repo, pr_number, COALESCE(item_id, 0), merge_sha, squash_sha, head_sha, branch, title, auto, merged_at, reverted_at, reverted_by, red_at, red_check`

func scanMerge(sc interface{ Scan(...any) error }) (FactoryMerge, error) {
	var m FactoryMerge
	err := sc.Scan(&m.Repo, &m.PR, &m.ItemID, &m.MergeSHA, &m.SquashSHA, &m.HeadSHA, &m.Branch, &m.Title, &m.Auto, &m.MergedAt, &m.RevertedAt, &m.RevertedBy, &m.RedAt, &m.RedCheck)
	return m, err
}

// RecordMerge records a factory PR's merge. A PR already recorded keeps
// its first record.
func (s *Store) RecordMerge(ctx context.Context, m FactoryMerge) error {
	if m.MergedAt == "" {
		m.MergedAt = Now()
	}
	var item any
	if m.ItemID != 0 {
		item = m.ItemID
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO factory_merges(repo, pr_number, item_id, merge_sha, squash_sha, head_sha, branch, title, auto, merged_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(repo, pr_number) DO NOTHING`,
			m.Repo, m.PR, item, m.MergeSHA, m.SquashSHA, m.HeadSHA, m.Branch, m.Title, m.Auto, m.MergedAt)
		return err
	})
}

// FactoryMerges lists a repo's merges ("" for every repo's) merged at or
// after since, oldest first.
func (s *Store) FactoryMerges(ctx context.Context, repo, since string) ([]FactoryMerge, error) {
	rows, err := s.read.QueryContext(ctx, `SELECT `+mergeCols+` FROM factory_merges
		WHERE (? = '' OR repo = ?) AND merged_at >= ? ORDER BY merged_at, pr_number`, repo, repo, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FactoryMerge
	for rows.Next() {
		m, err := scanMerge(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// MarkReverted records that a commit or PR reverted a merge, and says
// whether it was news: a merge is reverted once.
func (s *Store) MarkReverted(ctx context.Context, repo string, pr int, by, at string) (bool, error) {
	var n int64
	err := s.tx(ctx, func(tx *sql.Tx) error {
		r, err := tx.Exec(`UPDATE factory_merges SET reverted_at = ?, reverted_by = ? WHERE repo = ? AND pr_number = ? AND reverted_at = ''`, at, by, repo, pr)
		if err == nil {
			n, err = r.RowsAffected()
		}
		return err
	})
	return n > 0, err
}

// MarkRed records that main health saw a check go red on a merge commit,
// and returns that merge when it was news: a merge turns main red once.
// It returns nil when no factory merge made the commit.
func (s *Store) MarkRed(ctx context.Context, repo, sha, check, at string) (*FactoryMerge, error) {
	if sha == "" {
		return nil, nil
	}
	var out *FactoryMerge
	err := s.tx(ctx, func(tx *sql.Tx) error {
		m, err := scanMerge(tx.QueryRow(`SELECT `+mergeCols+` FROM factory_merges WHERE repo = ? AND merge_sha = ? AND red_at = ''`, repo, sha))
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE factory_merges SET red_at = ?, red_check = ? WHERE repo = ? AND pr_number = ?`, at, check, repo, m.PR); err != nil {
			return err
		}
		m.RedAt, m.RedCheck = at, check
		out = &m
		return nil
	})
	return out, err
}

// MergeBySHA is the factory merge that made commit sha on a repo's base
// branch and hasn't turned it red yet, or nil.
func (s *Store) MergeBySHA(ctx context.Context, repo, sha string) (*FactoryMerge, error) {
	if sha == "" {
		return nil, nil
	}
	m, err := scanMerge(s.read.QueryRowContext(ctx, `SELECT `+mergeCols+` FROM factory_merges WHERE repo = ? AND merge_sha = ? AND red_at = ''`, repo, sha))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// Strikes are a repo's merges that were reverted, or turned main red, at
// or after since. A merge that did both is one strike.
func (s *Store) Strikes(ctx context.Context, repo, since string) ([]FactoryMerge, error) {
	rows, err := s.read.QueryContext(ctx, `SELECT `+mergeCols+` FROM factory_merges
		WHERE repo = ? AND (reverted_at >= ? AND reverted_at != '' OR red_at >= ? AND red_at != '') ORDER BY merged_at, pr_number`, repo, since, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FactoryMerge
	for rows.Next() {
		m, err := scanMerge(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// Guard is a repo's auto-merge circuit breaker, and how far the revert
// watch read its base branch.
type Guard struct {
	Repo string
	// TrippedAt is when the breaker turned auto-merge off; "" while it's on.
	TrippedAt string
	Reason    string
	// ResetAt is when it was last reset: strikes before it don't count.
	ResetAt    string
	ResetBy    string
	WatchedSHA string
	WatchedAt  string
}

// Tripped says whether the breaker has turned auto-merge off.
func (g *Guard) Tripped() bool { return g != nil && g.TrippedAt != "" }

const guardCols = `repo, tripped_at, reason, reset_at, reset_by, watched_sha, watched_at`

func scanGuard(sc interface{ Scan(...any) error }) (Guard, error) {
	var g Guard
	err := sc.Scan(&g.Repo, &g.TrippedAt, &g.Reason, &g.ResetAt, &g.ResetBy, &g.WatchedSHA, &g.WatchedAt)
	return g, err
}

// Guard is a repo's breaker, or nil before it has one.
func (s *Store) Guard(ctx context.Context, repo string) (*Guard, error) {
	g, err := scanGuard(s.read.QueryRowContext(ctx, `SELECT `+guardCols+` FROM auto_merge_guard WHERE repo = ?`, repo))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &g, nil
}

// Guards lists every repo's breaker, by repo.
func (s *Store) Guards(ctx context.Context) ([]Guard, error) {
	rows, err := s.read.QueryContext(ctx, `SELECT `+guardCols+` FROM auto_merge_guard ORDER BY repo`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Guard
	for rows.Next() {
		g, err := scanGuard(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// TripBreaker turns auto-merge off for a repo, and says whether it was
// on: a breaker trips once until it's reset.
func (s *Store) TripBreaker(ctx context.Context, repo, reason, at string) (bool, error) {
	var n int64
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT INTO auto_merge_guard(repo) VALUES (?) ON CONFLICT(repo) DO NOTHING`, repo); err != nil {
			return err
		}
		r, err := tx.Exec(`UPDATE auto_merge_guard SET tripped_at = ?, reason = ? WHERE repo = ? AND tripped_at = ''`, at, reason, repo)
		if err == nil {
			n, err = r.RowsAffected()
		}
		return err
	})
	return n > 0, err
}

// ResetBreaker turns auto-merge back on for a repo, and says whether the
// breaker was tripped. Strikes before at no longer count.
func (s *Store) ResetBreaker(ctx context.Context, repo, by, at string) (bool, error) {
	var n int64
	err := s.tx(ctx, func(tx *sql.Tx) error {
		r, err := tx.Exec(`UPDATE auto_merge_guard SET tripped_at = '', reason = '', reset_at = ?, reset_by = ? WHERE repo = ? AND tripped_at != ''`, at, by, repo)
		if err == nil {
			n, err = r.RowsAffected()
		}
		return err
	})
	return n > 0, err
}

// SetWatched records the base branch head the revert watch read.
func (s *Store) SetWatched(ctx context.Context, repo, sha, at string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO auto_merge_guard(repo, watched_sha, watched_at) VALUES (?, ?, ?)
			ON CONFLICT(repo) DO UPDATE SET watched_sha = excluded.watched_sha, watched_at = excluded.watched_at`, repo, sha, at)
		return err
	})
}
