-- What the factory can learn from: the changes a human pushed to a factory
-- PR before merging it, and the blocking findings of reviewers and
-- critics. A learn job reads the unused ones of a repo and marks them used.
CREATE TABLE lesson_signals (
  id           INTEGER PRIMARY KEY,
  repo         TEXT NOT NULL,
  kind         TEXT NOT NULL,              -- human_edit | finding
  item_id      INTEGER REFERENCES work_items(id),
  issue_number INTEGER NOT NULL DEFAULT 0,
  job_id       INTEGER NOT NULL DEFAULT 0, -- the review or critic job of a finding
  pr_number    INTEGER NOT NULL DEFAULT 0,
  file         TEXT NOT NULL DEFAULT '',
  claim        TEXT NOT NULL DEFAULT '',
  severity     TEXT NOT NULL DEFAULT '',
  evidence     TEXT NOT NULL DEFAULT '',
  diff         TEXT NOT NULL DEFAULT '',   -- a human edit's changes, capped
  created_at   TEXT NOT NULL,
  learn_job    INTEGER                     -- the learn job that used it
);
-- A rerun job or a second poll records nothing twice.
CREATE UNIQUE INDEX lesson_signals_once ON lesson_signals(kind, item_id, job_id, pr_number, file, claim);
CREATE INDEX lesson_signals_unused ON lesson_signals(repo, learn_job);

-- Every line the factory proposed for a repo's AGENTS.md, and what became
-- of it: merged, or declined (its PR closed unmerged, or the line removed
-- before merge). A declined line is never proposed again.
CREATE TABLE lessons (
  id         INTEGER PRIMARY KEY,
  repo       TEXT NOT NULL,
  line       TEXT NOT NULL,
  norm       TEXT NOT NULL,                    -- the line as dedup compares it
  pr_number  INTEGER NOT NULL,
  status     TEXT NOT NULL DEFAULT 'proposed', -- proposed | merged | declined
  created_at TEXT NOT NULL,
  decided_at TEXT NOT NULL DEFAULT '',
  UNIQUE (repo, norm)
);
