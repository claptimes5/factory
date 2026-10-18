-- Triage's cost estimate for each item, and whether it waits for a
-- human's OK before implement. One row per item: a re-triage replaces it.
CREATE TABLE estimates (
  item_id          INTEGER PRIMARY KEY REFERENCES work_items(id),
  triage_job       INTEGER NOT NULL DEFAULT 0,  -- the triage it was made after
  issue_hash       TEXT NOT NULL DEFAULT '',    -- the approved text it was made for
  size             TEXT NOT NULL,
  median_usd       REAL NOT NULL,               -- billed dollars
  p75_usd          REAL NOT NULL,
  total_median_usd REAL NOT NULL,               -- all the work, plan runs at the cost they report
  total_p75_usd    REAL NOT NULL,
  basis            TEXT NOT NULL,               -- repo | all | default
  samples          INTEGER NOT NULL DEFAULT 0,
  detail           TEXT NOT NULL,               -- what it's based on, in words
  threshold_usd    REAL NOT NULL DEFAULT 0,
  status           TEXT NOT NULL,               -- off | under | exempt | waiting | parked | approved
  approved_by      TEXT NOT NULL DEFAULT '',
  approved_usd     REAL NOT NULL DEFAULT 0,     -- the p75 the OK was given for
  created_at       TEXT NOT NULL,
  decided_at       TEXT NOT NULL DEFAULT '',
  announced        INTEGER NOT NULL DEFAULT 0   -- the wait's label, comment and notice went out
);

-- Items filed for a red or flaky main: their cost is never held. Kept
-- after the check is green again and its tracking row goes.
ALTER TABLE work_items ADD COLUMN main_health INTEGER NOT NULL DEFAULT 0;

-- Health fixes already in flight when this runs are marked too.
UPDATE work_items SET main_health = 1
WHERE EXISTS (SELECT 1 FROM health_issues h
              WHERE h.repo = work_items.repo AND h.issue_number = work_items.issue_number AND h.issue_number > 0);
