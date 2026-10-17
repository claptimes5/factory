-- The commit an implement job produced and the gates verified, so a
-- reconciled item can be reviewed at exactly that commit.
ALTER TABLE jobs ADD COLUMN head_sha TEXT;
