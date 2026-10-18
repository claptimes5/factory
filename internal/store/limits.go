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

// SetLimit records a window's latest reading.
func (s *Store) SetLimit(ctx context.Context, l Limit) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO harness_limits(harness, window, used_percent, resets_at, seen_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(harness, window) DO UPDATE SET used_percent = excluded.used_percent, resets_at = excluded.resets_at, seen_at = excluded.seen_at`,
			l.Harness, l.Window, l.UsedPercent, l.ResetsAt, Now())
		return err
	})
}

// Limits lists every harness's latest readings, by harness and window.
func (s *Store) Limits(ctx context.Context) ([]Limit, error) {
	rows, err := s.read.QueryContext(ctx, `SELECT harness, window, used_percent, resets_at, seen_at FROM harness_limits ORDER BY harness, window`)
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
