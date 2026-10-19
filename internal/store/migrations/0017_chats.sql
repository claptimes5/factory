-- Conversations with the orchestrator's model on the dashboard, and the
-- issues it drafts in them. A draft becomes an issue only when a person
-- creates it.
CREATE TABLE chats (
  id         INTEGER PRIMARY KEY,
  title      TEXT NOT NULL,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

CREATE TABLE chat_messages (
  id      INTEGER PRIMARY KEY,
  chat_id INTEGER NOT NULL REFERENCES chats(id),
  role    TEXT NOT NULL,              -- user | assistant
  text    TEXT NOT NULL,
  job_id  INTEGER,                    -- the run that answered (assistant) or answers (user) it
  at      TEXT NOT NULL
);
CREATE INDEX chat_messages_chat ON chat_messages(chat_id, id);

CREATE TABLE issue_drafts (
  id           INTEGER PRIMARY KEY,
  chat_id      INTEGER NOT NULL REFERENCES chats(id),
  job_id       INTEGER NOT NULL DEFAULT 0,  -- the run that last wrote it
  repo         TEXT NOT NULL,
  title        TEXT NOT NULL,
  body         TEXT NOT NULL,
  labels       TEXT NOT NULL DEFAULT '[]',
  depends_on   TEXT NOT NULL DEFAULT '[]',  -- item ids
  parent       INTEGER NOT NULL DEFAULT 0,  -- an epic item, or 0
  state        TEXT NOT NULL DEFAULT 'draft', -- draft | creating | created | dismissed
  issue_number INTEGER NOT NULL DEFAULT 0,
  item_id      INTEGER NOT NULL DEFAULT 0,
  error        TEXT NOT NULL DEFAULT '',    -- why creating it last failed
  created_at   TEXT NOT NULL,
  updated_at   TEXT NOT NULL
);
CREATE INDEX issue_drafts_chat ON issue_drafts(chat_id, id);
