package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"strconv"
)

// KindChat is a dashboard chat's reply: a read-only run that may draft
// issues, started when a person sends a message rather than queued.
const KindChat = "chat"

// Chat is a conversation on the dashboard.
type Chat struct {
	ID        int64
	Title     string
	Workspace string // the workspace it works in; "" for one from before workspaces
	CreatedAt string
	UpdatedAt string
}

// ChatMessage is one turn of a chat.
type ChatMessage struct {
	ID     int64
	ChatID int64
	Role   string // user | assistant
	Text   string
	JobID  int64 // the run that answers (user) or answered (assistant) it; 0 for none
	At     string
}

// Draft states.
const (
	DraftOpen      = "draft"
	DraftCreating  = "creating"
	DraftCreated   = "created"
	DraftDismissed = "dismissed"
)

// IssueDraft is an issue a chat proposed. It is opened only when a person
// creates it.
type IssueDraft struct {
	ID          int64
	ChatID      int64
	JobID       int64
	Repo        string
	Title       string
	Body        string
	Labels      []string
	DependsOn   []int64
	Parent      int64
	State       string
	IssueNumber int
	ItemID      int64
	Error       string
	CreatedAt   string
	UpdatedAt   string
}

// Version fingerprints what creating the draft would file, so a create
// can say which text the person read.
func (d IssueDraft) Version() string {
	h := fnv.New64a()
	b, _ := json.Marshal([]any{d.Repo, d.Title, d.Body, d.Labels, d.DependsOn, d.Parent})
	h.Write(b)
	return strconv.FormatUint(h.Sum64(), 36)
}

// NewChat starts a chat with a person's first message.
func (s *Store) NewChat(ctx context.Context, title string) (int64, error) {
	return s.NewChatIn(ctx, title, "")
}

// NewChatIn starts a chat in a workspace.
func (s *Store) NewChatIn(ctx context.Context, title, workspace string) (int64, error) {
	var id int64
	err := s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.Exec(`INSERT INTO chats(title, workspace, created_at, updated_at) VALUES (?, ?, ?, ?)`, title, workspace, Now(), Now())
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	})
	return id, err
}

// GetChat reads one chat.
func (s *Store) GetChat(ctx context.Context, id int64) (*Chat, error) {
	var c Chat
	err := s.read.QueryRowContext(ctx, `SELECT id, title, workspace, created_at, updated_at FROM chats WHERE id = ?`, id).Scan(&c.ID, &c.Title, &c.Workspace, &c.CreatedAt, &c.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// RenameChatWorkspace moves the chats of workspace from to workspace to,
// when it's renamed.
func (s *Store) RenameChatWorkspace(ctx context.Context, from, to string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE chats SET workspace = ? WHERE workspace = ?`, to, from)
		return err
	})
}

// ListChats lists chats, the most recently active first.
func (s *Store) ListChats(ctx context.Context, limit int) ([]Chat, error) {
	return s.ListChatsIn(ctx, "", false, limit)
}

// ListChatsIn lists a workspace's chats ("" for every chat), the most
// recently active first; with older, the chats from before workspaces too.
func (s *Store) ListChatsIn(ctx context.Context, workspace string, older bool, limit int) ([]Chat, error) {
	rows, err := s.read.QueryContext(ctx, `SELECT id, title, workspace, created_at, updated_at FROM chats
		WHERE ? = '' OR workspace = ? OR (? AND workspace = '') ORDER BY updated_at DESC, id DESC LIMIT ?`, workspace, workspace, older, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Chat
	for rows.Next() {
		var c Chat
		if err := rows.Scan(&c.ID, &c.Title, &c.Workspace, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// AddChatMessage appends a turn to a chat.
func (s *Store) AddChatMessage(ctx context.Context, m ChatMessage) (int64, error) {
	var id int64
	err := s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.Exec(`INSERT INTO chat_messages(chat_id, role, text, job_id, at) VALUES (?, ?, ?, ?, ?)`,
			m.ChatID, m.Role, m.Text, nullID(m.JobID), Now())
		if err != nil {
			return err
		}
		id, _ = res.LastInsertId()
		_, err = tx.Exec(`UPDATE chats SET updated_at = ? WHERE id = ?`, Now(), m.ChatID)
		return err
	})
	return id, err
}

// SetChatMessageJob records the run that answers a person's message.
func (s *Store) SetChatMessageJob(ctx context.Context, msgID, jobID int64) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE chat_messages SET job_id = ? WHERE id = ?`, jobID, msgID)
		return err
	})
}

// ChatMessages lists a chat's turns in order.
func (s *Store) ChatMessages(ctx context.Context, chatID int64) ([]ChatMessage, error) {
	rows, err := s.read.QueryContext(ctx, `SELECT id, chat_id, role, text, COALESCE(job_id, 0), at FROM chat_messages WHERE chat_id = ? ORDER BY id`, chatID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ChatMessage
	for rows.Next() {
		var m ChatMessage
		if err := rows.Scan(&m.ID, &m.ChatID, &m.Role, &m.Text, &m.JobID, &m.At); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func nullID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

const draftCols = `id, chat_id, job_id, repo, title, body, labels, depends_on, parent, state, issue_number, item_id, error, created_at, updated_at`

func scanDraft(sc interface{ Scan(...any) error }) (*IssueDraft, error) {
	var d IssueDraft
	var labels, deps string
	if err := sc.Scan(&d.ID, &d.ChatID, &d.JobID, &d.Repo, &d.Title, &d.Body, &labels, &deps, &d.Parent, &d.State, &d.IssueNumber, &d.ItemID, &d.Error, &d.CreatedAt, &d.UpdatedAt); err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(labels), &d.Labels)
	_ = json.Unmarshal([]byte(deps), &d.DependsOn)
	return &d, nil
}

// SaveDraft writes a chat's draft: a new one when d.ID is 0, else it
// revises that draft, which must be the chat's and still open.
func (s *Store) SaveDraft(ctx context.Context, d IssueDraft) (int64, error) {
	if d.Labels == nil {
		d.Labels = []string{}
	}
	if d.DependsOn == nil {
		d.DependsOn = []int64{}
	}
	labels, _ := json.Marshal(d.Labels)
	deps, _ := json.Marshal(d.DependsOn)
	id := d.ID
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if id == 0 {
			res, err := tx.Exec(`INSERT INTO issue_drafts(chat_id, job_id, repo, title, body, labels, depends_on, parent, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				d.ChatID, d.JobID, d.Repo, d.Title, d.Body, string(labels), string(deps), d.Parent, Now(), Now())
			if err != nil {
				return err
			}
			id, err = res.LastInsertId()
			return err
		}
		res, err := tx.Exec(`UPDATE issue_drafts SET job_id = ?, repo = ?, title = ?, body = ?, labels = ?, depends_on = ?, parent = ?, error = '', updated_at = ?
			WHERE id = ? AND chat_id = ? AND state = 'draft'`,
			d.JobID, d.Repo, d.Title, d.Body, string(labels), string(deps), d.Parent, Now(), id, d.ChatID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("draft %d isn't an open draft of this chat", id)
		}
		return nil
	})
	return id, err
}

// GetDraft reads one draft.
func (s *Store) GetDraft(ctx context.Context, id int64) (*IssueDraft, error) {
	return scanDraft(s.read.QueryRowContext(ctx, `SELECT `+draftCols+` FROM issue_drafts WHERE id = ?`, id))
}

// ChatDrafts lists a chat's drafts in the order they were first written.
func (s *Store) ChatDrafts(ctx context.Context, chatID int64) ([]IssueDraft, error) {
	rows, err := s.read.QueryContext(ctx, `SELECT `+draftCols+` FROM issue_drafts WHERE chat_id = ? ORDER BY id`, chatID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []IssueDraft
	for rows.Next() {
		d, err := scanDraft(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *d)
	}
	return out, rows.Err()
}

// ErrDraftTaken says a draft isn't open any more: it was created,
// dismissed, or is being created.
var ErrDraftTaken = errors.New("that draft was already created or dismissed")

// MoveDraft moves a draft from one state to another, with what it became;
// it returns ErrDraftTaken when the draft isn't in state from.
func (s *Store) MoveDraft(ctx context.Context, id int64, from, to string, set IssueDraft) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE issue_drafts SET state = ?, title = CASE WHEN ? = '' THEN title ELSE ? END, body = CASE WHEN ? = '' THEN body ELSE ? END,
			issue_number = CASE WHEN ? = 0 THEN issue_number ELSE ? END, item_id = CASE WHEN ? = 0 THEN item_id ELSE ? END, error = ?, updated_at = ?
			WHERE id = ? AND state = ?`,
			to, set.Title, set.Title, set.Body, set.Body, set.IssueNumber, set.IssueNumber, set.ItemID, set.ItemID, set.Error, Now(), id, from)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrDraftTaken
		}
		return nil
	})
}

// ReopenCreating puts back to open each draft the daemon stopped in the
// middle of creating, saying why: the forge may have opened its issue.
func (s *Store) ReopenCreating(ctx context.Context, why string) (int, error) {
	var n int64
	err := s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE issue_drafts SET state = 'draft', error = ?, updated_at = ? WHERE state = 'creating'`, why, Now())
		if err != nil {
			return err
		}
		n, _ = res.RowsAffected()
		return nil
	})
	return int(n), err
}
