package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// Limit is one usage-limit window a harness last reported, such as a
// subscription's five-hour or weekly allowance.
type Limit struct {
	Harness     string
	Window      string  // "5h", "weekly", or the CLI's own name
	UsedPercent float64 // 0 to 100
	ResetsAt    string  // TimeFormat; "" when unknown
	SeenAt      string
}

// SetLimit records a window's latest reading. A reading with no reset
// time keeps the one already known, while that is still to come.
func (s *Store) SetLimit(ctx context.Context, l Limit) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO harness_limits(harness, window, used_percent, resets_at, seen_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(harness, window) DO UPDATE SET used_percent = excluded.used_percent, seen_at = excluded.seen_at,
			resets_at = CASE WHEN excluded.resets_at = '' AND harness_limits.resets_at > excluded.seen_at THEN harness_limits.resets_at ELSE excluded.resets_at END`,
			l.Harness, l.Window, l.UsedPercent, l.ResetsAt, Now())
		return err
	})
}

// Limits lists every harness's latest readings, by harness and window,
// leaving out windows that have reset since: their reading is out of date.
func (s *Store) Limits(ctx context.Context) ([]Limit, error) {
	rows, err := s.read.QueryContext(ctx, `SELECT harness, window, used_percent, resets_at, seen_at FROM harness_limits
		WHERE resets_at = '' OR resets_at > ? ORDER BY harness, window`, Now())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Limit
	for rows.Next() {
		var l Limit
		if err := rows.Scan(&l.Harness, &l.Window, &l.UsedPercent, &l.ResetsAt, &l.SeenAt); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// FormatLimits is a one-line summary, such as
// "claude 5h 42% (resets Oct 18 14:00 UTC), codex weekly 18%".
func FormatLimits(ls []Limit) string {
	parts := make([]string, 0, len(ls))
	for _, l := range ls {
		p := fmt.Sprintf("%s %s %.0f%%", l.Harness, l.Window, l.UsedPercent)
		if t, err := time.Parse(TimeFormat, l.ResetsAt); err == nil {
			p += " (resets " + t.Format("Jan 2 15:04") + " UTC)"
		}
		parts = append(parts, p)
	}
	return strings.Join(parts, ", ")
}

// ClearLimits forgets a harness's readings, or every harness's when it is
// "", so its jobs start again before the window the reading named resets.
func (s *Store) ClearLimits(ctx context.Context, harness string) (int64, error) {
	var n int64
	err := s.tx(ctx, func(tx *sql.Tx) error {
		r, err := tx.Exec(`DELETE FROM harness_limits WHERE ? = '' OR harness = ?`, harness, harness)
		if err != nil {
			return err
		}
		n, err = r.RowsAffected()
		return err
	})
	return n, err
}
