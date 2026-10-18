-- What each source (sentry, dependabot, todos) last read of each repo,
-- and what it last told you about it, so a state is announced once.
CREATE TABLE source_state (
  repo      TEXT NOT NULL,
  source    TEXT NOT NULL,
  state     TEXT NOT NULL,                -- ok | unreadable
  detail    TEXT NOT NULL DEFAULT '',
  found     INTEGER NOT NULL DEFAULT 0,   -- what it found to file on its last read
  polled_at TEXT NOT NULL,
  notified  TEXT NOT NULL DEFAULT '',     -- the unreadable state last announced
  PRIMARY KEY (repo, source)
);

-- Each issue a source filed, by what it was filed for: a Sentry issue's
-- id, a Dependabot alert's number, a TODO's path and text. The row goes
-- when that's resolved and the issue is closed.
CREATE TABLE source_issues (
  repo         TEXT NOT NULL,
  source       TEXT NOT NULL,
  key          TEXT NOT NULL,
  issue_number INTEGER NOT NULL,
  title        TEXT NOT NULL DEFAULT '',
  url          TEXT NOT NULL DEFAULT '',  -- where it came from
  created_at   TEXT NOT NULL,
  body_hash    TEXT NOT NULL DEFAULT '',  -- its title and text as filed, so an edited one is never closed
  PRIMARY KEY (repo, source, key)
);
CREATE INDEX source_issues_issue ON source_issues(repo, issue_number);
