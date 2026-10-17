-- Initial schema, from SPEC.md "SQLite schema", plus the columns the
-- pipeline needs to record what each job ran with: base_sha, branch,
-- skillset (which skill set the worker had mounted) and compare_id (set
-- when the job is one arm of a `factory compare` run).

CREATE TABLE work_items (
  id            INTEGER PRIMARY KEY,
  repo          TEXT NOT NULL,
  issue_number  INTEGER NOT NULL,
  title         TEXT NOT NULL,
  stage         TEXT NOT NULL,
  complexity    TEXT,
  risk          TEXT,
  priority      INTEGER DEFAULT 0,
  parent_id     INTEGER REFERENCES work_items(id),
  branch        TEXT,
  pr_number     INTEGER,
  created_at    TEXT NOT NULL,
  updated_at    TEXT NOT NULL,
  UNIQUE(repo, issue_number)
);

CREATE TABLE dependencies (
  item_id       INTEGER NOT NULL REFERENCES work_items(id),
  depends_on_id INTEGER NOT NULL REFERENCES work_items(id),
  PRIMARY KEY (item_id, depends_on_id)
);

CREATE TABLE jobs (
  id            INTEGER PRIMARY KEY,
  item_id       INTEGER REFERENCES work_items(id),
  kind          TEXT NOT NULL,
  state         TEXT NOT NULL,
  harness       TEXT NOT NULL,
  model         TEXT NOT NULL,
  effort        TEXT,
  attempt       INTEGER NOT NULL DEFAULT 1,
  priority      INTEGER DEFAULT 0,
  container_id  TEXT,
  budget_usd    REAL NOT NULL,
  cost_usd      REAL DEFAULT 0,
  tokens_in     INTEGER DEFAULT 0,
  tokens_out    INTEGER DEFAULT 0,
  verdict       TEXT,
  error         TEXT,
  log_path      TEXT,
  queued_at     TEXT NOT NULL,
  started_at    TEXT,
  finished_at   TEXT,
  base_sha      TEXT,
  branch        TEXT,
  skillset      TEXT,
  compare_id    TEXT,
  input         TEXT
);
CREATE INDEX jobs_queue ON jobs(state, priority DESC, queued_at);
CREATE INDEX jobs_item ON jobs(item_id);
CREATE INDEX jobs_compare ON jobs(compare_id);

CREATE TABLE events (
  id        INTEGER PRIMARY KEY,
  at        TEXT NOT NULL,
  actor     TEXT NOT NULL,
  item_id   INTEGER,
  job_id    INTEGER,
  type      TEXT NOT NULL,
  reason    TEXT,
  data      TEXT
);

CREATE TABLE gate_results (
  job_id    INTEGER NOT NULL REFERENCES jobs(id),
  gate      TEXT NOT NULL,
  status    TEXT NOT NULL,
  detail    TEXT,
  PRIMARY KEY (job_id, gate)
);

CREATE TABLE spend (
  day       TEXT PRIMARY KEY,
  cost_usd  REAL NOT NULL DEFAULT 0
);
