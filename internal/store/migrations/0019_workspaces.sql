-- The workspace a chat works in: its repos are the ones it reads and may
-- file issues in. '' for a chat from before workspaces.
ALTER TABLE chats ADD COLUMN workspace TEXT NOT NULL DEFAULT '';
