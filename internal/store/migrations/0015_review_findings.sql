-- Every reviewer finding, and how it ended. A finding keeps its id (F1,
-- F2, ...) across an item's review rounds: the verifier rules on it, the
-- builder answers it in the fix round, and the next review rules on that.
CREATE TABLE review_findings (
  item_id       INTEGER NOT NULL REFERENCES work_items(id),
  fid           TEXT NOT NULL,              -- F<seq>
  seq           INTEGER NOT NULL,
  job_id        INTEGER NOT NULL,           -- the review job that raised it
  kind          TEXT NOT NULL DEFAULT 'review', -- the reviewer that raised it: review | critic
  route         TEXT NOT NULL DEFAULT '',   -- its routing entry
  harness       TEXT NOT NULL DEFAULT '',
  model         TEXT NOT NULL DEFAULT '',
  round         INTEGER NOT NULL DEFAULT 1, -- the review round it was raised in
  severity      TEXT NOT NULL,              -- critical | major | minor | pre-existing
  file          TEXT NOT NULL DEFAULT '',
  line          INTEGER NOT NULL DEFAULT 0,
  claim         TEXT NOT NULL,
  evidence      TEXT NOT NULL DEFAULT '',
  blocking      INTEGER NOT NULL DEFAULT 0, -- it could block, so the verifier ruled on it
  check_status  TEXT NOT NULL DEFAULT '',   -- confirmed | refuted | unverified, latest
  check_reason  TEXT NOT NULL DEFAULT '',
  check_by      TEXT NOT NULL DEFAULT '',
  confirmed     INTEGER NOT NULL DEFAULT 0, -- the verifier confirmed it at least once
  answer        TEXT NOT NULL DEFAULT '',   -- the builder's: fixed | wrong | out_of_scope | unanswered
  answer_why    TEXT NOT NULL DEFAULT '',
  answer_job    INTEGER NOT NULL DEFAULT 0,
  ruling        TEXT NOT NULL DEFAULT '',   -- the next review's: resolved | upheld | withdrawn
  ruling_reason TEXT NOT NULL DEFAULT '',
  ruling_job    INTEGER NOT NULL DEFAULT 0,
  outcome       TEXT NOT NULL DEFAULT 'open',
  created_at    TEXT NOT NULL,
  updated_at    TEXT NOT NULL,
  PRIMARY KEY (item_id, fid)
);
CREATE INDEX review_findings_job ON review_findings(job_id);
