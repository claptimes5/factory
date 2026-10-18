-- The latest usage-limit windows each harness reported (subscriptions).
CREATE TABLE harness_limits (
  harness      TEXT NOT NULL,
  window       TEXT NOT NULL,
  used_percent REAL NOT NULL,
  resets_at    TEXT NOT NULL DEFAULT '',
  seen_at      TEXT NOT NULL,
  PRIMARY KEY (harness, window)
);
