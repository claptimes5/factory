package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/claptimes5/factory/internal/config"
	"github.com/claptimes5/factory/internal/state"
	"github.com/claptimes5/factory/internal/store"
)

// Chatter is the dashboard chat with the orchestrator's model, which
// drafts issues for a person to create.
type Chatter interface {
	// Chat sends text to chat chatID (0 starts one, in workspace ws) and
	// starts the reply; an empty text asks again for a reply to the last
	// message. A chat stays in the workspace it started in.
	Chat(ctx context.Context, ws string, chatID int64, text string) (chat, job int64, err error)
	// ChatUnavailable says why a chat in workspace ws can't reply now, or "".
	ChatUnavailable(ctx context.Context, ws string) string
	// CreateDraft opens a draft as an issue the factory builds, if it is
	// still the version the person read; a title or body given replaces
	// the draft's.
	CreateDraft(ctx context.Context, id int64, version, title, body string) (*store.IssueDraft, error)
	DismissDraft(ctx context.Context, id int64) error
	// OrchestrationSpend is today's spend on workspace ws's orchestrator
	// and chats ("" for every workspace's, tuning's too).
	OrchestrationSpend(ctx context.Context, ws string) (float64, error)
}

// OrchestratorInfo describes the orchestrator for the dashboard.
type OrchestratorInfo struct {
	On          bool    // it runs on its own schedule
	Harness     string  // its route, which chat runs on too
	Model       string  // the model's label
	Every       string  // how often it runs on its own
	RunBudget   float64 // dollars per run or chat reply
	DailyBudget float64 // dollars per day across it, tuning and chat
	// For is a workspace's orchestrator (may be nil: then the above).
	For func(ws string) config.Orchestration
}

// APIChats is the chat tab: whether chat can reply, what it runs on, and
// the chats so far, the most recent first.
type APIChats struct {
	Available   bool        `json:"available"`
	Unavailable string      `json:"unavailable"` // why not, when it can't
	Model       string      `json:"model"`
	Harness     string      `json:"harness"`
	Plan        bool        `json:"plan"` // the harness runs on a subscription plan
	On          bool        `json:"orchestrator_on"`
	Every       string      `json:"orchestrator_every"`
	RunBudget   float64     `json:"run_budget_usd"`
	DailyBudget float64     `json:"daily_budget_usd"`
	SpentToday  float64     `json:"spent_today_usd"`
	Chats       []APIChatAt `json:"chats"`
	Workspace   string      `json:"workspace"` // the workspace these are of; "" for every one
}

// APIChatAt is one chat in the list.
type APIChatAt struct {
	ID        int64  `json:"id"`
	Title     string `json:"title"`
	UpdatedAt string `json:"updated_at"`
}

// APIChat is one chat with its messages and drafts. Reply is the run
// answering the last message, while it runs or when it failed; null once
// it answered.
type APIChat struct {
	ID        int64            `json:"id"`
	Title     string           `json:"title"`
	Workspace string           `json:"workspace"` // the one it works in; "" for one from before workspaces
	Messages  []APIChatMessage `json:"messages"`
	Drafts    []APIDraft       `json:"drafts"`
	Reply     *APIChatReply    `json:"reply"`
}

// APIChatMessage is one turn.
type APIChatMessage struct {
	ID    int64  `json:"id"`
	Role  string `json:"role"` // user | assistant
	Text  string `json:"text"`
	At    string `json:"at"`
	JobID *int64 `json:"job_id"` // the run that wrote a reply
}

// APIChatReply is the run writing (or that failed to write) the reply to
// the last message.
type APIChatReply struct {
	JobID int64  `json:"job_id"`
	State string `json:"state"`
	Error string `json:"error"`
}

// APIDraft is an issue a chat drafted.
type APIDraft struct {
	ID          int64    `json:"id"`
	Repo        string   `json:"repo"`
	Title       string   `json:"title"`
	Body        string   `json:"body"`
	Labels      []string `json:"labels"`
	DependsOn   []int64  `json:"depends_on"`
	Parent      *int64   `json:"parent"`
	State       string   `json:"state"` // draft | creating | created | dismissed
	IssueNumber *int     `json:"issue_number"`
	IssueURL    string   `json:"issue_url"`
	ItemID      *int64   `json:"item_id"`
	Error       string   `json:"error"` // why creating it last failed
	UpdatedAt   string   `json:"updated_at"`
	// Version is what a create sends back, so it files the text read.
	Version string `json:"version"`
}

// APISentChat is what sending a message started.
type APISentChat struct {
	ChatID int64 `json:"chat_id"`
	JobID  int64 `json:"job_id"`
}

func (s *Server) routesChat(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/chats", s.apiChats)
	mux.HandleFunc("POST /api/v1/chats", s.apiSendChat)
	mux.HandleFunc("GET /api/v1/chats/{id}", s.apiChat)
	mux.HandleFunc("POST /api/v1/drafts/{id}/create", s.apiCreateDraft)
	mux.HandleFunc("POST /api/v1/drafts/{id}/dismiss", s.apiDismissDraft)
}

func (s *Server) apiChats(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ws, ok := s.reqWorkspace(w, r)
	if !ok {
		return
	}
	o := s.Orchestrator
	out := APIChats{Model: o.Model, Harness: o.Harness, On: o.On, Every: o.Every, RunBudget: o.RunBudget, DailyBudget: o.DailyBudget, Chats: []APIChatAt{}, Workspace: ws}
	if o.For != nil && ws != "" {
		f := o.For(ws)
		out.On, out.Every, out.RunBudget, out.DailyBudget = f.On, f.Interval.String(), f.RunBudget, f.DailyBudget
	}
	for _, h := range s.Plans {
		out.Plan = out.Plan || h == o.Harness
	}
	if s.Chat == nil {
		out.Unavailable = "Chat isn't available in this dashboard."
		writeJSON(w, http.StatusOK, out)
		return
	}
	if ws == "" {
		out.Unavailable = "Choose a workspace to chat in."
	} else {
		out.Unavailable = s.Chat.ChatUnavailable(ctx, ws)
	}
	out.Available = out.Unavailable == ""
	out.SpentToday, _ = s.Chat.OrchestrationSpend(ctx, ws)
	// A chat from before workspaces works in the factory's only one.
	chats, err := s.Store.ListChatsIn(ctx, ws, ws != "" && ws == s.soleWorkspace(), 50)
	if err != nil {
		apiErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	for _, c := range chats {
		out.Chats = append(out.Chats, APIChatAt{ID: c.ID, Title: c.Title, UpdatedAt: c.UpdatedAt})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) apiChat(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	c, err := s.Store.GetChat(ctx, id)
	if err != nil {
		apiErr(w, http.StatusNotFound, "no such chat")
		return
	}
	msgs, err := s.Store.ChatMessages(ctx, id)
	if err != nil {
		apiErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	drafts, err := s.Store.ChatDrafts(ctx, id)
	if err != nil {
		apiErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := APIChat{ID: c.ID, Title: c.Title, Workspace: c.Workspace, Messages: []APIChatMessage{}, Drafts: []APIDraft{}}
	for _, m := range msgs {
		am := APIChatMessage{ID: m.ID, Role: m.Role, Text: m.Text, At: m.At}
		if m.Role == "assistant" && m.JobID != 0 {
			jid := m.JobID
			am.JobID = &jid
		}
		out.Messages = append(out.Messages, am)
	}
	for _, d := range drafts {
		out.Drafts = append(out.Drafts, s.apiDraft(d))
	}
	if last := len(msgs) - 1; last >= 0 && msgs[last].Role == "user" {
		j, msg := s.lastChatJob(ctx, id)
		switch {
		case j != nil && (msg == msgs[last].ID || !j.State.Terminal()):
			out.Reply = &APIChatReply{JobID: j.ID, State: string(j.State), Error: j.Error}
		default:
			// Its reply never started: Try again starts one.
			out.Reply = &APIChatReply{State: string(state.Failed), Error: "no reply was started for this message"}
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// lastChatJob is the latest reply run of a chat and the message it
// answers, or nil. A reply queued again (for a sign-in) is a job of its
// own, so the message's job may not be the latest.
func (s *Server) lastChatJob(ctx context.Context, chatID int64) (*store.Job, int64) {
	jobs, err := s.Store.ListJobs(ctx, store.JobFilter{Kind: store.KindChat, Limit: 200})
	if err != nil {
		return nil, 0
	}
	for _, j := range jobs {
		var in struct {
			Chat    int64 `json:"chat"`
			Message int64 `json:"message"`
		}
		if json.Unmarshal([]byte(j.Input), &in) == nil && in.Chat == chatID {
			return j, in.Message
		}
	}
	return nil, 0
}

func (s *Server) apiDraft(d store.IssueDraft) APIDraft {
	out := APIDraft{ID: d.ID, Repo: d.Repo, Title: d.Title, Body: d.Body, Labels: d.Labels, DependsOn: d.DependsOn, State: d.State, Error: d.Error, UpdatedAt: d.UpdatedAt, Version: d.Version()}
	if out.Labels == nil {
		out.Labels = []string{}
	}
	if out.DependsOn == nil {
		out.DependsOn = []int64{}
	}
	if d.Parent != 0 {
		p := d.Parent
		out.Parent = &p
	}
	if d.IssueNumber != 0 {
		n := d.IssueNumber
		out.IssueNumber = &n
		if s.IssueURL != nil {
			out.IssueURL = s.IssueURL(d.Repo, n)
		}
	}
	if d.ItemID != 0 {
		it := d.ItemID
		out.ItemID = &it
	}
	return out
}

func (s *Server) apiSendChat(w http.ResponseWriter, r *http.Request) {
	if !jsonOnly(w, r) {
		return
	}
	if s.Chat == nil {
		apiErr(w, http.StatusNotFound, "chat isn't available in this dashboard")
		return
	}
	var in struct {
		ChatID    int64  `json:"chat_id"`
		Text      string `json:"text"`
		Workspace string `json:"workspace"` // for a new chat; one already started keeps its own
	}
	if err := s.readBody(r, &in); err != nil {
		apiErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if in.Workspace != "" && !s.hasWorkspace(in.Workspace) {
		apiErr(w, http.StatusBadRequest, "no workspace "+in.Workspace)
		return
	}
	chat, job, err := s.Chat.Chat(r.Context(), in.Workspace, in.ChatID, in.Text)
	if err != nil {
		apiErr(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, APISentChat{ChatID: chat, JobID: job})
}

func (s *Server) apiCreateDraft(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok || !jsonOnly(w, r) {
		return
	}
	if s.Chat == nil {
		apiErr(w, http.StatusNotFound, "chat isn't available in this dashboard")
		return
	}
	var in struct {
		Version string `json:"version"`
		Title   string `json:"title"`
		Body    string `json:"body"`
	}
	if err := s.readBody(r, &in); err != nil {
		apiErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if in.Version == "" {
		apiErr(w, http.StatusBadRequest, "send the draft's version, so it files the text you read")
		return
	}
	d, err := s.Chat.CreateDraft(r.Context(), id, in.Version, in.Title, in.Body)
	if err != nil {
		code := http.StatusConflict
		if errors.Is(err, store.ErrDraftTaken) {
			code = http.StatusGone
		}
		apiErr(w, code, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.apiDraft(*d))
}

func (s *Server) apiDismissDraft(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok || !jsonOnly(w, r) {
		return
	}
	if s.Chat == nil {
		apiErr(w, http.StatusNotFound, "chat isn't available in this dashboard")
		return
	}
	s.act(w, s.Chat.DismissDraft(r.Context(), id), "Dismissed.")
}
