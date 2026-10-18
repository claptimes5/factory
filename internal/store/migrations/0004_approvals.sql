-- What a person who may approve work approved: a hash of the issue's
-- title and body when they did, so an edit afterwards isn't built unseen.
CREATE TABLE approvals (
  item_id     INTEGER PRIMARY KEY REFERENCES work_items(id),
  issue_hash  TEXT NOT NULL,
  approved_by TEXT NOT NULL,
  at          TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);

-- Items already here were started before approvals were recorded: an
-- empty hash takes their text when next read as approved. Any other item
-- without a row has no approval and parks.
INSERT INTO approvals (item_id, issue_hash, approved_by) SELECT id, '', '' FROM work_items;
