-- What a person who may approve work approved: a hash of the issue's
-- title and body when they did, so an edit afterwards isn't built unseen.
CREATE TABLE approvals (
  item_id     INTEGER PRIMARY KEY REFERENCES work_items(id),
  issue_hash  TEXT NOT NULL,
  approved_by TEXT NOT NULL,
  at          TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);
