-- What backlog grooming proposed for each issue, and what a human made of
-- it. One open proposal per issue at a time; the rest are kept so a
-- dismissed one isn't made again while the issue reads the same.
CREATE TABLE groom_proposals (
  id           INTEGER PRIMARY KEY,
  repo         TEXT NOT NULL,
  issue_number INTEGER NOT NULL,
  kind         TEXT NOT NULL,              -- duplicate | stale | split
  key          TEXT NOT NULL,              -- what it proposes, as in the comment's marker
  issue_hash   TEXT NOT NULL,              -- the issue's text when proposed
  status       TEXT NOT NULL DEFAULT 'open', -- open | dismissed | done | gone
  canonical    INTEGER NOT NULL DEFAULT 0, -- a duplicate's issue to keep
  why          TEXT NOT NULL DEFAULT '',
  children     TEXT NOT NULL DEFAULT '[]', -- a split's sub-issues, JSON, with their numbers once filed
  job_id       INTEGER,
  created_at   TEXT NOT NULL,
  decided_at   TEXT NOT NULL DEFAULT '',
  decided_by   TEXT NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX groom_open ON groom_proposals(repo, issue_number) WHERE status = 'open';
CREATE INDEX groom_issue ON groom_proposals(repo, issue_number, kind);

-- Sub-issues a confirmed split filed, waiting to become their epic's
-- children once approved. The row goes when the child's item exists, or
-- the issue closes unbuilt.
CREATE TABLE groom_children (
  repo         TEXT NOT NULL,
  issue_number INTEGER NOT NULL,
  parent       INTEGER NOT NULL,            -- the epic's issue
  created_at   TEXT NOT NULL,
  PRIMARY KEY (repo, issue_number)
);

-- Each repo's last groom run.
CREATE TABLE groom_runs (
  repo     TEXT PRIMARY KEY,
  job_id   INTEGER,
  ran_at   TEXT NOT NULL,
  read     INTEGER NOT NULL DEFAULT 0,  -- issues it read
  flagged  INTEGER NOT NULL DEFAULT 0,
  note     TEXT NOT NULL DEFAULT ''
);
