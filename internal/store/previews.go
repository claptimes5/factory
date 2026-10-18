package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

// Preview is what the factory posted on an item's PR: its comment and the
// images it uploaded.
type Preview struct {
	ItemID    int64
	Repo      string
	PRNumber  int
	CommentID int64
	JobID     int64
	Uploads   []string // the forge's references, for removing them
	PostedAt  string
}

// SetPreview records what was posted on an item's PR.
func (s *Store) SetPreview(ctx context.Context, p Preview) error {
	if p.Uploads == nil {
		p.Uploads = []string{}
	}
	up, _ := json.Marshal(p.Uploads)
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO previews(item_id, repo, pr_number, comment_id, job_id, uploads, posted_at) VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(item_id) DO UPDATE SET repo = excluded.repo, pr_number = excluded.pr_number, comment_id = excluded.comment_id,
			job_id = excluded.job_id, uploads = excluded.uploads, posted_at = excluded.posted_at`,
			p.ItemID, p.Repo, p.PRNumber, p.CommentID, p.JobID, string(up), Now())
		return err
	})
}

const previewCols = `item_id, repo, pr_number, comment_id, job_id, uploads, posted_at`

func scanPreview(sc interface{ Scan(...any) error }) (*Preview, error) {
	var p Preview
	var up string
	if err := sc.Scan(&p.ItemID, &p.Repo, &p.PRNumber, &p.CommentID, &p.JobID, &up, &p.PostedAt); err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(up), &p.Uploads)
	return &p, nil
}

// Preview is what was posted on an item's PR, or nil.
func (s *Store) Preview(ctx context.Context, itemID int64) (*Preview, error) {
	p, err := scanPreview(s.read.QueryRowContext(ctx, `SELECT `+previewCols+` FROM previews WHERE item_id = ?`, itemID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return p, err
}

// PreviewsUp lists the previews whose images are still uploaded, oldest
// first.
func (s *Store) PreviewsUp(ctx context.Context) ([]Preview, error) {
	rows, err := s.read.QueryContext(ctx, `SELECT `+previewCols+` FROM previews WHERE uploads != '[]' ORDER BY posted_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Preview
	for rows.Next() {
		p, err := scanPreview(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}
