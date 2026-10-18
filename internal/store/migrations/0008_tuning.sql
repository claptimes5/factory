-- What the tuning loop proposed for factory.yaml, and what became of it.
-- changes, dropped and ideas are JSON; diff is the unified diff of the
-- file when the proposal was made.
CREATE TABLE tuning_proposals (
  id          INTEGER PRIMARY KEY,
  job_id      INTEGER REFERENCES jobs(id),
  status      TEXT NOT NULL,
  summary     TEXT NOT NULL,
  changes     TEXT NOT NULL DEFAULT '[]',
  dropped     TEXT NOT NULL DEFAULT '[]',
  ideas       TEXT NOT NULL DEFAULT '[]',
  diff        TEXT NOT NULL DEFAULT '',
  pr_url      TEXT NOT NULL DEFAULT '',
  period_from TEXT NOT NULL DEFAULT '',
  period_to   TEXT NOT NULL DEFAULT '',
  created_at  TEXT NOT NULL,
  decided_at  TEXT NOT NULL DEFAULT '',
  decided_by  TEXT NOT NULL DEFAULT ''
);

-- Named times the daemon keeps across restarts, such as when tuning last ran.
CREATE TABLE marks (
  name TEXT PRIMARY KEY,
  at   TEXT NOT NULL
);
