-- What the daemon last read of each repo's base branch CI, and what it
-- last told you about it, so a state is announced once.
CREATE TABLE main_health (
  repo       TEXT PRIMARY KEY,
  branch     TEXT NOT NULL,
  head_sha   TEXT NOT NULL DEFAULT '',
  state      TEXT NOT NULL,               -- green | red | flaky | pending | no_ci | unreadable
  detail     TEXT NOT NULL DEFAULT '',
  checked_at TEXT NOT NULL,
  notified   TEXT NOT NULL DEFAULT ''     -- the no_ci or unreadable state last announced
);

-- Each check that is red or flaky on a repo's base branch, and the issue
-- the factory filed for it: one per check, 0 until one is filed (the
-- day's cap was reached). The row goes when the check is green again.
CREATE TABLE health_issues (
  repo         TEXT NOT NULL,
  check_name   TEXT NOT NULL,
  kind         TEXT NOT NULL,             -- red | flaky
  issue_number INTEGER NOT NULL DEFAULT 0,
  head_sha     TEXT NOT NULL,
  url          TEXT NOT NULL DEFAULT '',
  created_at   TEXT NOT NULL,
  state        TEXT NOT NULL DEFAULT '',  -- the check's latest state
  absent_since TEXT NOT NULL DEFAULT '',  -- since when it's missing from CI
  muted_until  TEXT NOT NULL DEFAULT '',  -- a won't-fix close mutes it until then
  refiles      INTEGER NOT NULL DEFAULT 0, -- filed again without passing between
  PRIMARY KEY (repo, check_name)
);
CREATE INDEX health_issues_issue ON health_issues(repo, issue_number);
