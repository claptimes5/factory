package store

import (
	"context"
	"database/sql"
	"errors"
)

// KV reads a value the dashboard keeps; "" when it has none.
func (s *Store) KV(ctx context.Context, key string) (string, error) {
	var v string
	err := s.read.QueryRowContext(ctx, `SELECT value FROM kv WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// SetKV keeps a value for the dashboard.
func (s *Store) SetKV(ctx context.Context, key, value string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO kv(key, value, updated_at) VALUES (?, ?, ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`, key, value, Now())
		return err
	})
}

// PushSubscription is a browser that takes push notifications.
type PushSubscription struct {
	Endpoint, P256dh, Auth, Agent, CreatedAt, FailedAt string
}

// AddPushSubscription keeps a browser's subscription, replacing its keys
// if it subscribed before.
func (s *Store) AddPushSubscription(ctx context.Context, p PushSubscription) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO push_subscriptions(endpoint, p256dh, auth, agent, created_at) VALUES (?, ?, ?, ?, ?)
			ON CONFLICT(endpoint) DO UPDATE SET p256dh = excluded.p256dh, auth = excluded.auth, agent = excluded.agent, failed_at = ''`,
			p.Endpoint, p.P256dh, p.Auth, p.Agent, Now())
		return err
	})
}

// RemovePushSubscription forgets a browser: it unsubscribed, or its push
// service says the subscription is gone.
func (s *Store) RemovePushSubscription(ctx context.Context, endpoint string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`DELETE FROM push_subscriptions WHERE endpoint = ?`, endpoint)
		return err
	})
}

// PushFailed notes when a send to a browser last failed, or that the
// last one worked (failed "").
func (s *Store) PushFailed(ctx context.Context, endpoint, failed string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE push_subscriptions SET failed_at = ? WHERE endpoint = ?`, failed, endpoint)
		return err
	})
}

// PushSubscriptions lists the browsers that take push notifications.
func (s *Store) PushSubscriptions(ctx context.Context) ([]PushSubscription, error) {
	rows, err := s.read.QueryContext(ctx, `SELECT endpoint, p256dh, auth, agent, created_at, failed_at FROM push_subscriptions ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PushSubscription
	for rows.Next() {
		var p PushSubscription
		if err := rows.Scan(&p.Endpoint, &p.P256dh, &p.Auth, &p.Agent, &p.CreatedAt, &p.FailedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// HoldNotification keeps a notification for the catch-up after quiet hours.
func (s *Store) HoldNotification(ctx context.Context, msg string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO held_notifications(message, at) VALUES (?, ?)`, msg, Now())
		return err
	})
}

// TakeHeldNotifications returns the held notifications, oldest first, and
// forgets them.
func (s *Store) TakeHeldNotifications(ctx context.Context) ([]string, error) {
	var out []string
	err := s.tx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.Query(`SELECT message FROM held_notifications ORDER BY id`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var m string
			if err := rows.Scan(&m); err != nil {
				rows.Close()
				return err
			}
			out = append(out, m)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		_, err = tx.Exec(`DELETE FROM held_notifications`)
		return err
	})
	return out, err
}
