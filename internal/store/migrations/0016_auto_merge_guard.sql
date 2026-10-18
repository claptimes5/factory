-- Each factory PR that merged, for the revert rate and the auto-merge
-- breaker: whether a later commit or PR reverted it, and whether main
-- health first saw a check red on its merge commit.
CREATE TABLE factory_merges (
  repo        TEXT NOT NULL,
  pr_number   INTEGER NOT NULL,
  item_id     INTEGER,
  merge_sha   TEXT NOT NULL DEFAULT '',    -- the merge or squash commit on the base branch
  squash_sha  TEXT NOT NULL DEFAULT '',    -- GitLab's squash commit, when it made a merge commit too
  head_sha    TEXT NOT NULL DEFAULT '',    -- the PR's head when it merged
  branch      TEXT NOT NULL DEFAULT '',
  title       TEXT NOT NULL DEFAULT '',
  auto        INTEGER NOT NULL DEFAULT 0,  -- merged by the auto-merge the factory turned on
  merged_at   TEXT NOT NULL,
  reverted_at TEXT NOT NULL DEFAULT '',
  reverted_by TEXT NOT NULL DEFAULT '',    -- the commit or PR that reverted it
  red_at      TEXT NOT NULL DEFAULT '',    -- when main health saw a check go red on merge_sha
  red_check   TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (repo, pr_number)
);
CREATE INDEX factory_merges_at ON factory_merges(repo, merged_at);

-- Each repo's auto-merge circuit breaker, and how far the revert watch
-- has read the base branch.
CREATE TABLE auto_merge_guard (
  repo        TEXT PRIMARY KEY,
  tripped_at  TEXT NOT NULL DEFAULT '',    -- '' while auto-merge may run
  reason      TEXT NOT NULL DEFAULT '',
  reset_at    TEXT NOT NULL DEFAULT '',    -- strikes before this don't count
  reset_by    TEXT NOT NULL DEFAULT '',
  watched_sha TEXT NOT NULL DEFAULT '',    -- the base branch head the watch last read
  watched_at  TEXT NOT NULL DEFAULT ''
);
