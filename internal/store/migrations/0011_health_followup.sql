-- What main health keeps on each tracked check besides its issue: the
-- check's latest state, since when it has been missing from the branch's
-- CI, until when a won't-fix close mutes it, and how many times it was
-- filed again without passing in between.
ALTER TABLE health_issues ADD COLUMN state        TEXT    NOT NULL DEFAULT '';
ALTER TABLE health_issues ADD COLUMN absent_since TEXT    NOT NULL DEFAULT '';
ALTER TABLE health_issues ADD COLUMN muted_until  TEXT    NOT NULL DEFAULT '';
ALTER TABLE health_issues ADD COLUMN refiles      INTEGER NOT NULL DEFAULT 0;
