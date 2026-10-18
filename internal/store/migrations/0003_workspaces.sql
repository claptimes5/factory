-- Workspace items change several repos at once: each implement job
-- records the commit it verified in every repo it changed, and each item
-- the PR it opened in every one of them.
CREATE TABLE job_heads (
  job_id   INTEGER NOT NULL REFERENCES jobs(id),
  repo     TEXT NOT NULL,
  head_sha TEXT NOT NULL,
  PRIMARY KEY (job_id, repo)
);
CREATE TABLE item_prs (
  item_id   INTEGER NOT NULL REFERENCES work_items(id),
  repo      TEXT NOT NULL,
  pr_number INTEGER NOT NULL,
  PRIMARY KEY (item_id, repo)
);
