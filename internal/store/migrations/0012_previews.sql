-- The screenshots on each item's PR: the one comment the factory keeps
-- up to date, and the forge's references to the uploaded images, kept
-- until the PR merges or closes or previews.keep_for passes.
CREATE TABLE previews (
  item_id    INTEGER PRIMARY KEY,
  repo       TEXT NOT NULL,
  pr_number  INTEGER NOT NULL,
  comment_id INTEGER NOT NULL DEFAULT 0,
  job_id     INTEGER NOT NULL DEFAULT 0,  -- the implement job whose screenshots are up
  uploads    TEXT NOT NULL DEFAULT '[]',  -- JSON list of the forge's references
  posted_at  TEXT NOT NULL
);
