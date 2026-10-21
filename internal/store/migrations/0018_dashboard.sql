-- Small values the dashboard keeps for its people: when Home was last
-- looked at, the web push keys.
CREATE TABLE kv (
  key        TEXT PRIMARY KEY,
  value      TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

-- Browsers that asked for push notifications (the installed web app).
CREATE TABLE push_subscriptions (
  endpoint   TEXT PRIMARY KEY,
  p256dh     TEXT NOT NULL,
  auth       TEXT NOT NULL,
  agent      TEXT NOT NULL DEFAULT '',    -- the browser, to tell devices apart
  created_at TEXT NOT NULL,
  failed_at  TEXT NOT NULL DEFAULT ''     -- the last send that failed, '' after one that worked
);

-- Notifications held during quiet hours, sent as one catch-up when they end.
CREATE TABLE held_notifications (
  id      INTEGER PRIMARY KEY,
  message TEXT NOT NULL,
  at      TEXT NOT NULL
);
