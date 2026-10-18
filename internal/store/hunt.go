package store

import "context"

// EventsFor returns the events of a type with this reason, oldest first.
func (s *Store) EventsFor(ctx context.Context, typ, reason string) ([]Event, error) {
	rows, err := s.read.QueryContext(ctx, `SELECT id, at, actor, item_id, job_id, type, COALESCE(reason,''), COALESCE(data,'')
		FROM events WHERE type = ? AND reason = ? ORDER BY id`, typ, reason)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.At, &e.Actor, &e.ItemID, &e.JobID, &e.Type, &e.Reason, &e.Data); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
