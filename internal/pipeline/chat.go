package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/claptimes5/factory/internal/mcp"
	"github.com/claptimes5/factory/internal/state"
	"github.com/claptimes5/factory/internal/store"
)

// A chat is a conversation with the orchestrator's model on the
// dashboard. Each message a person sends starts one read-only run that
// can look at the code and the factory's work and draft issues; a draft
// becomes an issue only when the person creates it. A reply doesn't wait
// for a free job slot, since it only reads, but one is written at a time,
// on the orchestrator's daily budget.

// ChatInput is carried by a chat job: the chat and the message it answers.
type ChatInput struct {
	Chat    int64 `json:"chat"`
	Message int64 `json:"message"`
}

// Limits on what a person sends and on what a reply reads of the chat.
const (
	maxChatMessage = 20000 // bytes in one message
	maxChatEarlier = 60000 // bytes of earlier turns a reply reads, newest kept
)

// chatTools are the orchestrator's tools a chat may use: those that only
// read.
var chatTools = []string{"list_items", "get_item", "get_issue", "queue_status", "get_job_log"}

// ChatUnavailable says why a chat can't reply now, or "".
func (p *Pipeline) ChatUnavailable(ctx context.Context) string {
	r, ok := p.Cfg.Routing["orchestrate"]
	if !ok {
		return "Chat runs on the orchestrator's model, and routing.orchestrate isn't set."
	}
	if _, ok := p.Harnesses[r.Harness]; !ok {
		return fmt.Sprintf("Chat runs on the orchestrator's model (%s on %s), and the %s harness isn't set up.", r.Model, r.Harness, r.Harness)
	}
	if p.Cfg.Runner.Kind == "docker" && runtime.GOOS != "linux" {
		return "Chat needs a Linux host with Docker, like the orchestrator: the daemon's binary runs inside the job container."
	}
	if _, err := os.Stat(filepath.Join(p.Cfg.Path(p.Cfg.PromptsDir), "chat.md")); err != nil {
		return "Chat needs prompts/chat.md in your prompts directory. Copy it from the factory's repository."
	}
	if p.restart.Load() {
		return "The factory is restarting to apply new settings. Send your message once it's back."
	}
	spent, err := p.orchSpend(ctx)
	if err != nil {
		return err.Error()
	}
	if spent >= p.Cfg.Orchestrator.DailyBudget {
		return fmt.Sprintf("Today's orchestration budget is spent ($%.2f of $%.2f). Chat is back tomorrow, or raise orchestrator.daily_budget in Settings.", spent, p.Cfg.Orchestrator.DailyBudget)
	}
	if why := p.harnessHeld(ctx, &store.Job{}, r.Harness); why != "" {
		return "Chat can't reply now: " + why + "."
	}
	return ""
}

// Chat sends a person's message to chat chatID (0 starts a new chat) and
// starts the run that replies: it returns the chat, new when none was
// given, and the reply's job. An empty text asks again for a reply to the
// chat's last message, when it has none.
func (p *Pipeline) Chat(ctx context.Context, chatID int64, text string) (int64, int64, error) {
	text = strings.TrimSpace(text)
	if len(text) > maxChatMessage {
		return 0, 0, fmt.Errorf("that message is too long (%d bytes; the most is %d)", len(text), maxChatMessage)
	}
	// Checked and started under the scheduler's lock, so no job claims a
	// one-at-a-time harness between the check and the start.
	ctx, unlock := p.lockClaims(ctx)
	defer unlock()
	if why := p.ChatUnavailable(ctx); why != "" {
		return 0, 0, errors.New(why)
	}
	jctx, release, err := p.holdChatSlot()
	if err != nil {
		return 0, 0, err
	}
	started := false
	defer func() {
		if !started {
			release()
		}
	}()
	for _, st := range []state.JobState{state.Queued, state.Running} {
		if n, err := p.Store.CountJobs(ctx, st, store.KindChat); err != nil {
			return 0, 0, err
		} else if n > 0 {
			return 0, 0, errors.New("another chat reply is still being written: wait for it, or stop it")
		}
	}
	var msgID int64
	if chatID == 0 {
		if text == "" {
			return 0, 0, errors.New("write a message first")
		}
		id, err := p.Store.NewChat(ctx, chatTitle(text))
		if err != nil {
			return 0, 0, err
		}
		chatID = id
	} else if _, err := p.Store.GetChat(ctx, chatID); err != nil {
		return 0, 0, fmt.Errorf("no chat %d", chatID)
	}
	if text == "" {
		msgs, err := p.Store.ChatMessages(ctx, chatID)
		if err != nil {
			return 0, 0, err
		}
		if len(msgs) == 0 || msgs[len(msgs)-1].Role != "user" {
			return 0, 0, errors.New("there's nothing to reply to: write a message")
		}
		msgID = msgs[len(msgs)-1].ID
	} else {
		id, err := p.Store.AddChatMessage(ctx, store.ChatMessage{ChatID: chatID, Role: "user", Text: text})
		if err != nil {
			return 0, 0, err
		}
		msgID = id
	}
	r := p.Cfg.Routing["orchestrate"]
	fast := r.FastFor(r.Harness, r.Model)
	in, _ := json.Marshal(ChatInput{Chat: chatID, Message: msgID})
	j, err := p.Store.StartJob(ctx, store.Job{Kind: store.KindChat, Harness: r.Harness, Model: r.Model, Effort: r.Effort, Fast: fast,
		Skillset: r.Skills, Route: "orchestrate", BudgetUSD: p.Cfg.FastBudget(p.Cfg.Orchestrator.RunBudget, fast), Priority: 10, Input: string(in)})
	if err != nil {
		return 0, 0, err
	}
	p.warn(p.Store.SetChatMessageJob(ctx, msgID, j.ID), "SetChatMessageJob")
	started = true
	go func() {
		defer release()
		p.runJob(jctx, j)
	}()
	return chatID, j.ID, nil
}

// errChatClosed refuses a chat before the daemon has recovered from its
// last stop, or once it is stopping.
var errChatClosed = errors.New("the factory is starting or stopping: send your message again in a moment")

// openChat lets chat replies start, on the jobs' context; closeChat stops
// them before the daemon waits for running work.
func (p *Pipeline) openChat(ctx context.Context) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.chatCtx = ctx
}

func (p *Pipeline) closeChat() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.chatCtx = nil
}

// holdChatSlot counts a chat reply as running, which Serve and a restart
// wait for, though it takes no job slot; it returns the context it runs on and the release for when it's
// done.
func (p *Pipeline) holdChatSlot() (context.Context, func(), error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.chatCtx == nil {
		return nil, nil, errChatClosed
	}
	p.chats++
	p.wg.Add(1)
	var once sync.Once
	return p.chatCtx, func() {
		once.Do(func() {
			p.mu.Lock()
			p.chats--
			p.mu.Unlock()
			p.wg.Done()
		})
	}, nil
}

// chatTitle names a chat after its first message.
func chatTitle(text string) string {
	t := strings.Join(strings.Fields(text), " ")
	if utf8.RuneCountInString(t) <= 80 {
		return t
	}
	return string([]rune(t)[:79]) + "…"
}

// ChatPrompt is what a chat's prompt is rendered from.
type ChatPrompt struct {
	Repo    string   // the repos, comma-separated
	Repos   []string // each repo, the first the default
	Missing []string // repos that couldn't be checked out, and why
	Status  string   // the queue snapshot
	Labels  string   // the labels a draft may carry, besides the intake label
	Drafts  string   // the chat's drafts so far, fenced
	Earlier string   // the earlier turns, fenced
	Message string   // the message to reply to, fenced
}

func (p *Pipeline) runChat(ctx context.Context, j *store.Job) error {
	dir := p.jobDir(j.ID)
	var in ChatInput
	_ = json.Unmarshal([]byte(j.Input), &in)
	fail := func(run agentRun, st state.JobState, msg string) error {
		if p.stopIfCancelled(ctx, j, nil, run) {
			return nil
		}
		p.finish(ctx, j, run, st, "", msg)
		return nil // the person sends it again
	}
	// Checked again here: a reply queued again after a restart waits.
	if spent, err := p.orchSpend(ctx); err != nil || spent >= p.Cfg.Orchestrator.DailyBudget {
		p.finish(ctx, j, agentRun{}, state.Cancelled, "", "orchestration budget spent for today")
		return err
	}
	if p.Cfg.Runner.Kind == "docker" && runtime.GOOS != "linux" {
		return fail(agentRun{}, state.Failed, "chat needs a Linux host with Docker (the daemon's binary runs inside the job container)")
	}
	msgs, err := p.Store.ChatMessages(ctx, in.Chat)
	if err != nil {
		return fail(agentRun{}, state.Failed, err.Error())
	}
	var earlier []store.ChatMessage
	var message string
	for i, m := range msgs {
		if m.ID == in.Message {
			earlier, message = msgs[:i], m.Text
			break
		}
	}
	if message == "" {
		return fail(agentRun{}, state.Failed, fmt.Sprintf("chat %d has no message %d to reply to", in.Chat, in.Message))
	}
	drafts, err := p.Store.ChatDrafts(ctx, in.Chat)
	if err != nil {
		return fail(agentRun{}, state.Failed, err.Error())
	}
	work, missing, err := p.checkoutAll(ctx, dir)
	if err != nil {
		return fail(agentRun{}, state.Failed, err.Error())
	}
	sctx, stop := context.WithCancel(ctx)
	defer stop()
	x, err := p.withTools(sctx, dir, p.ChatTools(j.ID, in.Chat))
	if err != nil {
		return fail(agentRun{}, state.Failed, err.Error())
	}
	snapshot, _ := p.statusText(ctx)
	names := p.Cfg.RepoNames()
	d := ChatPrompt{Repo: strings.Join(names, ", "), Repos: names, Missing: missing, Status: snapshot,
		Labels: strings.Join(p.Cfg.Orchestrator.Labels, ", "), Drafts: chatDrafts(drafts), Earlier: chatEarlier(earlier), Message: fenced(message)}
	prompt, err := p.renderStep(j, "chat", nil, d)
	if err != nil {
		return fail(agentRun{}, state.Failed, err.Error())
	}
	run := p.runAgentWith(ctx, j, work, filepath.Join(dir, "out"), prompt, `{"type":"object","additionalProperties":false,"required":["reply"],"properties":{"reply":{"type":"string"}}}`,
		p.Cfg.Timeout("orchestrate"), x)
	if p.stopIfCancelled(ctx, j, nil, run) {
		return nil
	}
	if st, msg, bad := runFailure(run); bad {
		return fail(run, st, msg)
	}
	var v struct {
		Reply string `json:"reply"`
	}
	if err := json.Unmarshal([]byte(extractJSON(run.final)), &v); err != nil || strings.TrimSpace(v.Reply) == "" {
		return fail(run, state.Failed, "the reply was missing")
	}
	if _, err := p.Store.AddChatMessage(ctx, store.ChatMessage{ChatID: in.Chat, Role: "assistant", Text: v.Reply, JobID: j.ID}); err != nil {
		return fail(run, state.Failed, err.Error())
	}
	norm, _ := json.Marshal(v)
	p.finish(ctx, j, run, state.Succeeded, string(norm), "")
	return nil
}

// chatEarlier is the turns before the one being answered, fenced, the
// oldest dropped when they run long.
func chatEarlier(msgs []store.ChatMessage) string {
	var parts []string
	n := 0
	for i := len(msgs) - 1; i >= 0; i-- {
		who := "The person"
		if msgs[i].Role == "assistant" {
			who = "You"
		}
		s := who + ":\n" + fenced(msgs[i].Text)
		if n+len(s) > maxChatEarlier {
			parts = append(parts, "(earlier turns left out)\n")
			break
		}
		n += len(s)
		parts = append(parts, s)
	}
	for a, b := 0, len(parts)-1; a < b; a, b = a+1, b-1 {
		parts[a], parts[b] = parts[b], parts[a]
	}
	return strings.Join(parts, "\n")
}

// chatDrafts lists a chat's drafts for its prompt.
func chatDrafts(ds []store.IssueDraft) string {
	var b strings.Builder
	for _, d := range ds {
		what := map[string]string{store.DraftOpen: "open, waiting on the person", store.DraftCreating: "being created",
			store.DraftDismissed: "dismissed by the person"}[d.State]
		if d.State == store.DraftCreated {
			what = fmt.Sprintf("created as issue #%d (item %d)", d.IssueNumber, d.ItemID)
		}
		fmt.Fprintf(&b, "Draft %d (%s), %s:\n%s", d.ID, d.Repo, what, fenced(d.Title+"\n\n"+d.Body))
	}
	return b.String()
}

// ChatTools are the tools of the run replying in chat chatID: the
// orchestrator's reads, and draft_issue.
func (p *Pipeline) ChatTools(jobID, chatID int64) []mcp.Tool {
	var out []mcp.Tool
	for _, t := range p.toolsFor(jobID, "chat") {
		for _, n := range chatTools {
			if t.Name == n {
				out = append(out, t)
			}
		}
	}
	return append(out, mcp.Tool{Name: "draft_issue",
		Description: "Draft an issue for the person to create. It is not opened until they do. Pass draft to revise one of your open drafts instead.",
		InputSchema: schema(`"draft":{"type":"integer","description":"An open draft of this chat to revise"},"title":{"type":"string"},"body":{"type":"string","description":"Markdown: the problem, the expected behavior and how to verify it"},`+
			`"parent":{"type":"integer","description":"An epic item this is part of"},"depends_on":{"type":"array","items":{"type":"integer"},"description":"Item ids that must be done first"},`+
			`"labels":{"type":"array","items":{"type":"string"}},`+repoProp, "title", "body"),
		Handler: func(ctx context.Context, raw json.RawMessage) (string, error) {
			var a struct {
				Draft     int64    `json:"draft"`
				Title     string   `json:"title"`
				Body      string   `json:"body"`
				Repo      string   `json:"repo"`
				Parent    int64    `json:"parent"`
				DependsOn []int64  `json:"depends_on"`
				Labels    []string `json:"labels"`
			}
			if err := json.Unmarshal(raw, &a); err != nil {
				return "", err
			}
			d, dropped, err := p.checkDraft(ctx, store.IssueDraft{ID: a.Draft, ChatID: chatID, JobID: jobID, Repo: a.Repo, Title: a.Title, Body: a.Body,
				Parent: a.Parent, DependsOn: a.DependsOn, Labels: a.Labels})
			if err != nil {
				return "", err
			}
			// Under act, so a revision can't land between a create's
			// version check and its claim.
			p.act.Lock()
			id, err := p.Store.SaveDraft(ctx, d)
			p.act.Unlock()
			if err != nil {
				return "", err
			}
			out := fmt.Sprintf(`{"draft":%d}`, id)
			if len(dropped) > 0 {
				out += fmt.Sprintf("\nlabels not allowed and dropped: %s", strings.Join(dropped, ", "))
			}
			return out, nil
		}})
}

// checkDraft checks a draft as create_issue checks an issue, fills in its
// repo, and keeps only the labels it may carry.
func (p *Pipeline) checkDraft(ctx context.Context, d store.IssueDraft) (store.IssueDraft, []string, error) {
	d.Title, d.Body = strings.TrimSpace(d.Title), strings.TrimSpace(d.Body)
	if d.Title == "" || d.Body == "" {
		return d, nil, errors.New("a draft needs a title and a body")
	}
	if len(d.Title) > 256 {
		return d, nil, errors.New("keep the title under 256 characters")
	}
	if d.Parent != 0 {
		parent, err := p.Store.GetItem(ctx, d.Parent)
		if err != nil {
			return d, nil, fmt.Errorf("no parent item %d", d.Parent)
		}
		if d.Repo == "" {
			d.Repo = parent.Repo
		}
		if split, err := p.Store.GroomSplit(ctx, parent.Repo, parent.IssueNumber); err != nil {
			return d, nil, err
		} else if split {
			return d, nil, fmt.Errorf("item %d's sub-issues came from backlog grooming and wait for a human's approval; don't split it again", d.Parent)
		}
	}
	repo, err := p.pickRepo(d.Repo)
	if err != nil {
		return d, nil, err
	}
	d.Repo = repo
	if err := p.checkDeps(ctx, d.DependsOn); err != nil {
		return d, nil, err
	}
	keep, dropped := p.orchestratorLabels(d.Labels)
	d.Labels = keep[1:] // the intake label goes on when it's created
	return d, dropped, nil
}

// ErrDraftChanged refuses to create a draft that changed since the
// person read it: they approve the text they saw.
var ErrDraftChanged = errors.New("the draft changed since you read it: look it over again, then create it")

// CreateDraft opens draft id as an issue the factory builds, approved by
// the person who created it on the dashboard. seen is the draft's
// version they read; a title or body given replaces the draft's.
func (p *Pipeline) CreateDraft(ctx context.Context, id int64, seen, title, body string) (*store.IssueDraft, error) {
	// Once started it finishes, whoever closes the page: an issue the
	// forge opened must get its item, its approval and its draft's state.
	ctx = context.WithoutCancel(ctx)
	p.act.Lock()
	defer p.act.Unlock()
	d, err := p.Store.GetDraft(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("no draft %d", id)
	}
	if d.State != store.DraftOpen {
		return nil, store.ErrDraftTaken
	}
	if seen != d.Version() {
		return nil, ErrDraftChanged
	}
	if strings.TrimSpace(title) != "" {
		d.Title = title
	}
	if strings.TrimSpace(body) != "" {
		d.Body = body
	}
	// Checked again: items, repos and labels may have changed since.
	checked, _, err := p.checkDraft(ctx, *d)
	if err != nil {
		return nil, err
	}
	if err := p.Store.MoveDraft(ctx, id, store.DraftOpen, store.DraftCreating, store.IssueDraft{Title: checked.Title, Body: checked.Body}); err != nil {
		return nil, err
	}
	labels, _ := p.orchestratorLabels(checked.Labels)
	num, err := p.gh(checked.Repo).CreateIssue(ctx, checked.Title, checked.Body+"\n\n_Drafted in the factory's chat and opened from its dashboard._", labels)
	if err != nil {
		p.warn(p.Store.MoveDraft(ctx, id, store.DraftCreating, store.DraftOpen, store.IssueDraft{Error: truncate(err.Error(), 500)}), "MoveDraft")
		return nil, err
	}
	p.warn(p.Store.MoveDraft(ctx, id, store.DraftCreating, store.DraftCreated, store.IssueDraft{IssueNumber: num}), "MoveDraft")
	it, created, err := p.Store.UpsertItem(ctx, checked.Repo, num, checked.Title)
	if err != nil {
		return nil, err
	}
	p.warn(p.Store.MoveDraft(ctx, id, store.DraftCreated, store.DraftCreated, store.IssueDraft{ItemID: it.ID}), "MoveDraft")
	reason := fmt.Sprintf("created from draft %d of chat %d", id, d.ChatID)
	p.warn(p.Store.AddEvent(ctx, store.Event{Actor: "human", ItemID: &it.ID, Type: "create_issue", Reason: reason}), "AddEvent")
	if created {
		// The person approved this text by creating it; the host may
		// tidy it, so the approval is of the issue as stored.
		is, err := p.gh(checked.Repo).GetIssue(ctx, num)
		if err == nil {
			err = p.approve(ctx, it, *is, "a person on the dashboard")
		}
		p.warn(err, "approve")
	}
	if checked.Parent != 0 {
		p.warn(p.Store.SetParent(ctx, it.ID, checked.Parent), "SetParent")
	}
	if len(checked.DependsOn) > 0 {
		if err := p.Store.SetDependencies(ctx, it.ID, checked.DependsOn, "human", reason); err != nil {
			return nil, err
		}
	}
	if created {
		if err := p.enqueueTriage(ctx, it, 1, ""); err != nil {
			return nil, err
		}
	}
	return p.Store.GetDraft(ctx, id)
}

// OrchestrationSpend is today's spend on the orchestrator, tuning and
// chat, against orchestrator.daily_budget.
func (p *Pipeline) OrchestrationSpend(ctx context.Context) (float64, error) {
	return p.orchSpend(ctx)
}

// DismissDraft sets a draft aside without opening it.
func (p *Pipeline) DismissDraft(ctx context.Context, id int64) error {
	p.act.Lock()
	defer p.act.Unlock()
	if _, err := p.Store.GetDraft(ctx, id); err != nil {
		return fmt.Errorf("no draft %d", id)
	}
	return p.Store.MoveDraft(ctx, id, store.DraftOpen, store.DraftDismissed, store.IssueDraft{})
}
