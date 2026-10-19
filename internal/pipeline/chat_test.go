package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/claptimes5/factory/internal/harness"
	"github.com/claptimes5/factory/internal/state"
	"github.com/claptimes5/factory/internal/store"
)

// A chat reads and drafts; nothing reaches the forge until a person
// creates the draft, which then goes to the factory approved, like an
// issue they labeled.
func TestChatDraftsAndAPersonCreates(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	e.p.Cfg.Orchestrator.Labels = []string{"math"}
	calls := 0
	e.claude.chat = func(req harness.RunRequest) (string, error) {
		calls++
		if calls == 2 {
			if !strings.Contains(req.Prompt, "Make it Sub too") || !strings.Contains(req.Prompt, "Drafted Add.") || !strings.Contains(req.Prompt, "Add an Add function") {
				t.Errorf("a later turn's prompt lacks the conversation or its drafts:\n%s", req.Prompt)
			}
			return `{"reply":"Nothing more to draft."}`, nil
		}
		if !strings.Contains(req.Prompt, "We need an Add function") {
			t.Errorf("prompt lacks the message:\n%s", req.Prompt)
		}
		m := dialMCP(t, req)
		defer m.conn.Close()
		var list struct{ Tools []struct{ Name string } }
		json.Unmarshal(m.rpc("tools/list", nil), &list)
		var names []string
		for _, tl := range list.Tools {
			names = append(names, tl.Name)
		}
		slices.Sort(names)
		if got := strings.Join(names, ","); got != "draft_issue,get_issue,get_item,get_job_log,list_items,queue_status" {
			t.Errorf("chat tools = %s; it may only read and draft", got)
		}
		if out, isErr := m.call("draft_issue", map[string]any{"title": "x"}); !isErr || !strings.Contains(out, "title and a body") {
			t.Errorf("a draft without a body = %s", out)
		}
		if out, isErr := m.call("draft_issue", map[string]any{"title": "x", "body": "y", "repo": "else/where"}); !isErr || !strings.Contains(out, "not one of the factory's repos") {
			t.Errorf("a draft for another repo = %s", out)
		}
		out, isErr := m.call("draft_issue", map[string]any{"title": "Add", "body": "Add func Add.", "labels": []string{"math", "factory:go"}})
		if isErr || !strings.Contains(out, "dropped: factory:go") {
			t.Fatalf("draft_issue = %s", out)
		}
		var d struct{ Draft int64 }
		json.Unmarshal([]byte(strings.SplitN(out, "\n", 2)[0]), &d)
		if out, isErr := m.call("draft_issue", map[string]any{"draft": d.Draft, "title": "Add an Add function", "body": "Add func Add(a, b int) int.", "labels": []string{"math"}}); isErr {
			t.Fatalf("revising the draft = %s", out)
		}
		return `{"reply":"Drafted Add."}`, nil
	}
	chat, job, err := e.p.Chat(ctx, 0, "We need an Add function.")
	if err != nil {
		t.Fatal(err)
	}
	e.p.wg.Wait()
	if j, _ := e.p.Store.GetJob(ctx, job); j.State != state.Succeeded || j.Kind != store.KindChat || j.Model != "opus" {
		t.Fatalf("reply job = %+v", j)
	}
	msgs, _ := e.p.Store.ChatMessages(ctx, chat)
	if len(msgs) != 2 || msgs[1].Role != "assistant" || msgs[1].Text != "Drafted Add." || msgs[1].JobID != job {
		t.Fatalf("messages = %+v", msgs)
	}
	if c, _ := e.p.Store.GetChat(ctx, chat); c.Title != "We need an Add function." {
		t.Errorf("chat title = %q", c.Title)
	}
	drafts, _ := e.p.Store.ChatDrafts(ctx, chat)
	if len(drafts) != 1 || drafts[0].Title != "Add an Add function" || drafts[0].State != store.DraftOpen || strings.Join(drafts[0].Labels, ",") != "math" {
		t.Fatalf("drafts = %+v", drafts)
	}
	if len(e.gh.issues) != 0 {
		t.Fatalf("a draft reached the forge: %+v", e.gh.issues)
	}

	// The next turn reads the conversation and the drafts.
	if _, _, err := e.p.Chat(ctx, chat, "Make it Sub too."); err != nil {
		t.Fatal(err)
	}
	e.p.wg.Wait()

	d, err := e.p.CreateDraft(ctx, drafts[0].ID, "", "")
	if err != nil {
		t.Fatal(err)
	}
	is := e.gh.issues[d.IssueNumber]
	if d.State != store.DraftCreated || is == nil || strings.Join(is.Labels, ",") != "factory:ready,math" || !strings.Contains(is.Body, "Drafted in the factory's chat") {
		t.Fatalf("created %+v as %+v", d, is)
	}
	it, err := e.p.Store.ItemByIssue(ctx, "o/r", d.IssueNumber)
	if err != nil || d.ItemID != it.ID {
		t.Fatalf("item = %+v, %v (draft says %d)", it, err, d.ItemID)
	}
	if a, _ := e.p.Store.ItemApproval(ctx, it.ID); a == nil || a.ApprovedBy != "a person on the dashboard" {
		t.Errorf("approval = %+v", a)
	}
	if jobs, _ := e.p.Store.ListJobs(ctx, store.JobFilter{ItemID: it.ID, Kind: "triage"}); len(jobs) != 1 {
		t.Errorf("triage jobs = %d", len(jobs))
	}
	if _, err := e.p.CreateDraft(ctx, d.ID, "", ""); !errors.Is(err, store.ErrDraftTaken) {
		t.Errorf("creating it twice = %v", err)
	}
	if err := e.p.DismissDraft(ctx, d.ID); !errors.Is(err, store.ErrDraftTaken) {
		t.Errorf("dismissing a created draft = %v", err)
	}
}

// A failed reply can be asked for again; one reply is written at a time,
// on the orchestrator's daily budget.
func TestChatRetryOneAtATimeAndBudget(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	fail := true
	e.claude.chat = func(harness.RunRequest) (string, error) {
		if fail {
			return "", errors.New("overloaded")
		}
		return `{"reply":"Here."}`, nil
	}
	chat, job, err := e.p.Chat(ctx, 0, "What's stuck?")
	if err != nil {
		t.Fatal(err)
	}
	e.p.wg.Wait()
	if j, _ := e.p.Store.GetJob(ctx, job); j.State != state.Failed {
		t.Fatalf("job = %+v", j)
	}
	fail = false
	if _, again, err := e.p.Chat(ctx, chat, ""); err != nil || again == job {
		t.Fatalf("asking again = %d, %v", again, err)
	}
	e.p.wg.Wait()
	msgs, _ := e.p.Store.ChatMessages(ctx, chat)
	if len(msgs) != 2 || msgs[0].Text != "What's stuck?" || msgs[1].Text != "Here." {
		t.Fatalf("messages = %+v", msgs)
	}
	if _, _, err := e.p.Chat(ctx, chat, ""); err == nil || !strings.Contains(err.Error(), "nothing to reply to") {
		t.Errorf("asking again with nothing unanswered = %v", err)
	}
	if _, _, err := e.p.Chat(ctx, 0, ""); err == nil {
		t.Error("an empty first message was accepted")
	}

	// A reply being written holds the next.
	running, err := e.p.Store.StartJob(ctx, store.Job{Kind: store.KindChat, Harness: "claude", Model: "opus", BudgetUSD: 1, Input: `{"chat":1,"message":1}`})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.p.Chat(ctx, chat, "And now?"); err == nil || !strings.Contains(err.Error(), "still being written") {
		t.Errorf("a second reply at once = %v", err)
	}
	e.p.Store.FinishJob(ctx, running.ID, store.Result{State: state.Cancelled})

	// Its spend is the orchestrator's.
	e.p.Cfg.Orchestrator.DailyBudget = 0.05 // the reply above cost $0.10
	if why := e.p.ChatUnavailable(ctx); !strings.Contains(why, "orchestration budget is spent") {
		t.Errorf("unavailable = %q", why)
	}
	if _, _, err := e.p.Chat(ctx, chat, "And now?"); err == nil {
		t.Error("a reply started past the budget")
	}
	if n, _ := e.p.Store.CountEventsSince(ctx, "orchestrator", "create_issue", "2000-01-01"); n != 0 {
		t.Errorf("chat counted against the orchestrator's issue cap: %d", n)
	}
}

// A draft the forge refuses stays a draft, with why.
func TestCreateDraftRefusedStaysOpen(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	chat, _ := e.p.Store.NewChat(ctx, "c")
	id, err := e.p.Store.SaveDraft(ctx, store.IssueDraft{ChatID: chat, Repo: "o/r", Title: "t", Body: "b"})
	if err != nil {
		t.Fatal(err)
	}
	e.p.GH = refusingGH{e.gh}
	if _, err := e.p.CreateDraft(ctx, id, "", ""); err == nil {
		t.Fatal("created through a refusing forge")
	}
	d, _ := e.p.Store.GetDraft(ctx, id)
	if d.State != store.DraftOpen || !strings.Contains(d.Error, "label missing") {
		t.Fatalf("draft = %+v", d)
	}
	if err := e.p.DismissDraft(ctx, id); err != nil {
		t.Fatal(err)
	}
	if d, _ := e.p.Store.GetDraft(ctx, id); d.State != store.DraftDismissed || d.Error != "" {
		t.Errorf("dismissed draft = %+v", d)
	}
}

type refusingGH struct{ *fakeGH }

func (refusingGH) CreateIssue(context.Context, string, string, []string) (int, error) {
	return 0, errors.New("label missing")
}
