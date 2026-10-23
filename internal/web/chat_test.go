package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/claptimes5/factory/internal/state"
	"github.com/claptimes5/factory/internal/store"
	"github.com/claptimes5/factory/internal/store/storetest"
)

// fakeChatter records what reached it and answers from the store.
type fakeChatter struct {
	st      *store.Store
	dir     string
	refuse  string
	created []int64
}

func (f *fakeChatter) Chat(ctx context.Context, chatID int64, text string, files []store.Attachment) (int64, int64, error) {
	if f.refuse != "" {
		return 0, 0, errors.New(f.refuse)
	}
	if chatID == 0 {
		chatID, _ = f.st.NewChat(ctx, text)
	}
	msg, _ := f.st.AddChatMessage(ctx, store.ChatMessage{ChatID: chatID, Role: "user", Text: text})
	for _, a := range files {
		os.MkdirAll(f.ChatFiles(chatID, msg), 0o755)
		os.WriteFile(filepath.Join(f.ChatFiles(chatID, msg), a.Name), a.Data, 0o644)
	}
	return chatID, 99, nil
}
func (f *fakeChatter) ChatFiles(chatID, msgID int64) string {
	return filepath.Join(f.dir, fmt.Sprint(chatID), fmt.Sprint(msgID))
}
func (f *fakeChatter) ChatUnavailable(context.Context) string              { return f.refuse }
func (f *fakeChatter) OrchestrationSpend(context.Context) (float64, error) { return 1.5, nil }
func (f *fakeChatter) DismissDraft(ctx context.Context, id int64) error {
	return f.st.MoveDraft(ctx, id, store.DraftOpen, store.DraftDismissed, store.IssueDraft{})
}
func (f *fakeChatter) CreateDraft(ctx context.Context, id int64, version, title, body string) (*store.IssueDraft, error) {
	if d, _ := f.st.GetDraft(ctx, id); d != nil && d.State == store.DraftOpen && d.Version() != version {
		return nil, errors.New("the draft changed")
	}
	if err := f.st.MoveDraft(ctx, id, store.DraftOpen, store.DraftCreated, store.IssueDraft{Title: title, Body: body, IssueNumber: 7, ItemID: 3}); err != nil {
		return nil, err
	}
	f.created = append(f.created, id)
	return f.st.GetDraft(ctx, id)
}

func TestChatAPI(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := storetest.Open(t, filepath.Join(t.TempDir(), "f.db"))
	fc := &fakeChatter{st: st}
	s := &Server{Store: st, Actions: &fakeActions{}, Repos: []string{"o/r"}, Chat: fc, Plans: []string{"claude"},
		IssueURL:     func(repo string, n int) string { return fmt.Sprintf("https://github.com/%s/issues/%d", repo, n) },
		Orchestrator: OrchestratorInfo{Harness: "claude", Model: "opus", Every: "1h0m0s", RunBudget: 3, DailyBudget: 15}}
	h, err := s.Handler()
	if err != nil {
		t.Fatal(err)
	}
	base := httpServer(t, h)

	var list APIChats
	getJSON(t, base+"/api/v1/chats", &list)
	if !list.Available || list.Model != "opus" || !list.Plan || list.On || list.SpentToday != 1.5 || len(list.Chats) != 0 {
		t.Fatalf("chats = %+v", list)
	}
	code, body := postJSON(t, base+"/api/v1/chats", "", `{"chat_id":0,"text":"Add CSV export"}`)
	var sent APISentChat
	if json.Unmarshal([]byte(body), &sent); code != 200 || sent.ChatID == 0 || sent.JobID != 99 {
		t.Fatalf("send = %d %s", code, body)
	}

	// The reply is being written, then fails: the chat says so.
	j, _ := st.StartJob(ctx, store.Job{Kind: store.KindChat, Harness: "claude", Model: "opus", Input: fmt.Sprintf(`{"chat":%d,"message":1}`, sent.ChatID)})
	var c APIChat
	getJSON(t, fmt.Sprintf("%s/api/v1/chats/%d", base, sent.ChatID), &c)
	if len(c.Messages) != 1 || c.Messages[0].Role != "user" || c.Reply == nil || c.Reply.JobID != j.ID || c.Reply.State != "running" {
		t.Fatalf("chat = %+v", c)
	}
	st.FinishJob(ctx, j.ID, store.Result{State: state.Failed, Error: "overloaded"})
	getJSON(t, fmt.Sprintf("%s/api/v1/chats/%d", base, sent.ChatID), &c)
	if c.Reply == nil || c.Reply.State != "failed" || c.Reply.Error != "overloaded" {
		t.Fatalf("failed reply = %+v", c.Reply)
	}
	// Answered, there's no reply to show.
	st.AddChatMessage(ctx, store.ChatMessage{ChatID: sent.ChatID, Role: "assistant", Text: "Drafted it.", JobID: j.ID})
	id, _ := st.SaveDraft(ctx, store.IssueDraft{ChatID: sent.ChatID, Repo: "o/r", Title: "CSV", Body: "Export.", Labels: []string{"billing"}})
	getJSON(t, fmt.Sprintf("%s/api/v1/chats/%d", base, sent.ChatID), &c)
	if c.Reply != nil || len(c.Messages) != 2 || c.Messages[1].JobID == nil || *c.Messages[1].JobID != j.ID || len(c.Drafts) != 1 || c.Drafts[0].IssueNumber != nil {
		t.Fatalf("answered chat = %+v", c)
	}

	if code, _ := postJSON(t, fmt.Sprintf("%s/api/v1/drafts/%d/create", base, id), "", `{"title":"CSV export"}`); code != http.StatusBadRequest {
		t.Errorf("a create that doesn't say which version it read = %d", code)
	}
	if code, _ := postJSON(t, fmt.Sprintf("%s/api/v1/drafts/%d/create", base, id), "", `{"version":"old"}`); code != http.StatusConflict {
		t.Errorf("a create of a version since changed = %d", code)
	}
	if code, body := postJSON(t, fmt.Sprintf("%s/api/v1/drafts/%d/create", base, id), "", `{"title":"CSV export","version":"`+c.Drafts[0].Version+`"}`); code != 200 || !strings.Contains(body, `"issue_url":"https://github.com/o/r/issues/7"`) || !strings.Contains(body, `"title":"CSV export"`) {
		t.Fatalf("create = %d %s", code, body)
	}
	if code, _ := postJSON(t, fmt.Sprintf("%s/api/v1/drafts/%d/create", base, id), "", `{"version":"x"}`); code != http.StatusGone {
		t.Errorf("creating twice = %d", code)
	}
	if code, _ := postJSON(t, fmt.Sprintf("%s/api/v1/drafts/%d/dismiss", base, id), "", `{}`); code != http.StatusConflict {
		t.Errorf("dismissing a created draft = %d", code)
	}
	if code, _ := postJSON(t, fmt.Sprintf("%s/api/v1/drafts/%d/create", base, id), "https://evil.example", `{"version":"x"}`); code != http.StatusForbidden {
		t.Errorf("a cross-origin create = %d", code)
	}
	if len(fc.created) != 1 {
		t.Errorf("created = %v", fc.created)
	}

	// A message whose reply never started can be tried again.
	st.AddChatMessage(ctx, store.ChatMessage{ChatID: sent.ChatID, Role: "user", Text: "And PDF?"})
	getJSON(t, fmt.Sprintf("%s/api/v1/chats/%d", base, sent.ChatID), &c)
	if c.Reply == nil || c.Reply.State != "failed" || c.Reply.JobID != 0 || c.Reply.Error == "" {
		t.Fatalf("a message with no reply started = %+v", c.Reply)
	}

	fc.refuse = "Today's orchestration budget is spent."
	getJSON(t, base+"/api/v1/chats", &list)
	if list.Available || list.Unavailable != fc.refuse || len(list.Chats) != 1 || list.Chats[0].Title != "Add CSV export" {
		t.Fatalf("chats = %+v", list)
	}
	if code, body := postJSON(t, base+"/api/v1/chats", "", `{"chat_id":0,"text":"more"}`); code != http.StatusConflict || !strings.Contains(body, "budget is spent") {
		t.Errorf("refused send = %d %s", code, body)
	}
	if code, _ := postJSON(t, base+"/api/v1/chats", "", `not json`); code != http.StatusBadRequest {
		t.Errorf("bad body = %d", code)
	}
	if r, _ := http.Get(base + "/api/v1/chats/999"); r.StatusCode != http.StatusNotFound {
		t.Errorf("no chat = %d", r.StatusCode)
	}
}

// A message's files are listed with it and served from the chat: a picture
// to show, anything else, SVG too, only as a download.
func TestChatFilesAreListedAndServedSafely(t *testing.T) {
	t.Parallel()
	st := storetest.Open(t, filepath.Join(t.TempDir(), "f.db"))
	fc := &fakeChatter{st: st, dir: t.TempDir()}
	s := &Server{Store: st, Actions: &fakeActions{}, Repos: []string{"o/r"}, Chat: fc}
	h, err := s.Handler()
	if err != nil {
		t.Fatal(err)
	}
	base := httpServer(t, h)
	code, body := postJSON(t, base+"/api/v1/chats", "", `{"chat_id":0,"text":"See","files":[{"name":"a.png","data":"cG5n"},{"name":"b.svg","data":"PHN2Zy8+"}]}`)
	var sent APISentChat
	if json.Unmarshal([]byte(body), &sent); code != 200 {
		t.Fatalf("send = %d %s", code, body)
	}
	var c APIChat
	getJSON(t, fmt.Sprintf("%s/api/v1/chats/%d", base, sent.ChatID), &c)
	if len(c.Messages) != 1 || strings.Join(c.Messages[0].Files, ",") != "a.png,b.svg" {
		t.Fatalf("messages = %+v", c.Messages)
	}
	file := fmt.Sprintf("%s/api/v1/chats/%d/messages/%d/files/", base, sent.ChatID, c.Messages[0].ID)
	r, err := http.Get(file + "a.png")
	if err != nil || r.StatusCode != 200 || r.Header.Get("Content-Type") != "image/png" || r.Header.Get("Content-Disposition") != "" {
		t.Fatalf("picture = %v %+v", err, r)
	}
	r, _ = http.Get(file + "b.svg")
	if r.StatusCode != 200 || r.Header.Get("Content-Type") != "application/octet-stream" || !strings.HasPrefix(r.Header.Get("Content-Disposition"), "attachment") {
		t.Fatalf("svg = %+v", r.Header)
	}
	for _, bad := range []string{"..%2F..%2Ff.db", ".hidden", "none.png"} {
		if r, _ := http.Get(file + bad); r.StatusCode != http.StatusNotFound {
			t.Errorf("%s = %d", bad, r.StatusCode)
		}
	}
}
