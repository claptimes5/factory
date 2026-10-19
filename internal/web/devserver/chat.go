package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/claptimes5/factory/internal/state"
	"github.com/claptimes5/factory/internal/store"
)

// chatter stands in for the chat: it answers at once with a canned reply
// and a draft, and "creates" drafts in the throwaway database only.
type chatter struct {
	st     *store.Store
	issues atomic.Int64
}

func (c *chatter) ChatUnavailable(context.Context) string { return "" }

func (c *chatter) OrchestrationSpend(context.Context) (float64, error) { return 1.84, nil }

func (c *chatter) Chat(ctx context.Context, chatID int64, text string) (int64, int64, error) {
	text = strings.TrimSpace(text)
	if strings.Contains(text, "busy") {
		return 0, 0, errors.New("another chat reply is still being written: wait for it, or stop it")
	}
	if chatID == 0 {
		if text == "" {
			return 0, 0, errors.New("write a message first")
		}
		id, err := c.st.NewChat(ctx, text)
		if err != nil {
			return 0, 0, err
		}
		chatID = id
	}
	if text != "" {
		if _, err := c.st.AddChatMessage(ctx, store.ChatMessage{ChatID: chatID, Role: "user", Text: text}); err != nil {
			return 0, 0, err
		}
	}
	if _, err := c.st.SaveDraft(ctx, store.IssueDraft{ChatID: chatID, Repo: "acme/api", Title: "Draft: " + chatTitle(text),
		Body: "**Problem**\n\n" + text + "\n\n**Expected**\n\nIt works.\n\n**Verify**\n\n`go test ./...`"}); err != nil {
		return 0, 0, err
	}
	_, err := c.st.AddChatMessage(ctx, store.ChatMessage{ChatID: chatID, Role: "assistant",
		Text: "The devserver doesn't run a model. Here's a sample draft for **" + chatTitle(text) + "**."})
	return chatID, 0, err
}

func chatTitle(text string) string {
	if text == "" {
		return "your last message"
	}
	if len(text) > 60 {
		return text[:60] + "…"
	}
	return text
}

func (c *chatter) CreateDraft(ctx context.Context, id int64, title, body string) (*store.IssueDraft, error) {
	d, err := c.st.GetDraft(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("no draft %d", id)
	}
	if d.State != store.DraftOpen {
		return nil, store.ErrDraftTaken
	}
	if strings.Contains(title+d.Title, "refuse") {
		return nil, errors.New("devserver: GitHub refused the issue: label factory:ready doesn't exist")
	}
	n := 300 + int(c.issues.Add(1))
	if err := c.st.MoveDraft(ctx, id, store.DraftOpen, store.DraftCreated, store.IssueDraft{Title: title, Body: body, IssueNumber: n, ItemID: 1}); err != nil {
		return nil, err
	}
	return c.st.GetDraft(ctx, id)
}

func (c *chatter) DismissDraft(ctx context.Context, id int64) error {
	return c.st.MoveDraft(ctx, id, store.DraftOpen, store.DraftDismissed, store.IssueDraft{})
}

// seedChats is two chats: one that drafted issues, one whose reply failed.
func seedChats(ctx context.Context, st *store.Store) error {
	c1, err := st.NewChat(ctx, "Split the CSV export into issues")
	if err != nil {
		return err
	}
	msgs := []store.ChatMessage{
		{ChatID: c1, Role: "user", Text: "We need CSV export for invoices. Can you split it into issues?"},
		{ChatID: c1, Role: "assistant", Text: "I read `billing/export.go`. Export is two pieces of work:\n\n1. A CSV writer for invoices.\n2. A download button on the invoices page, which needs the first.\n\nI drafted both below. Should the export include voided invoices?"},
		{ChatID: c1, Role: "user", Text: "No, leave voided ones out."},
		{ChatID: c1, Role: "assistant", Text: "Done: the writer's draft now says voided invoices are skipped."},
	}
	for _, m := range msgs {
		if _, err := st.AddChatMessage(ctx, m); err != nil {
			return err
		}
	}
	for _, d := range []store.IssueDraft{
		{ChatID: c1, Repo: "acme/api", Title: "Write invoices as CSV", Body: "**Problem**\n\nThere's no way to export invoices.\n\n**Expected**\n\n`billing.ExportCSV` writes one row per invoice, skipping voided ones, with a header row.\n\n**Verify**\n\n`go test ./billing -run CSV`", Labels: []string{"billing"}},
		{ChatID: c1, Repo: "acme/web", Title: "Add a Download CSV button to the invoices page", Body: "**Problem**\n\nPeople copy invoices by hand.\n\n**Expected**\n\nA Download CSV button on /invoices calls the export.\n\nNeeds the CSV writer first."},
		{ChatID: c1, Repo: "acme/api", Title: "Export invoices as PDF", Body: "Out of scope for now."},
	} {
		if _, err := st.SaveDraft(ctx, d); err != nil {
			return err
		}
	}
	if err := st.MoveDraft(ctx, 3, store.DraftOpen, store.DraftDismissed, store.IssueDraft{}); err != nil {
		return err
	}
	c2, err := st.NewChat(ctx, "Why did the login fix fail twice?")
	if err != nil {
		return err
	}
	msg, err := st.AddChatMessage(ctx, store.ChatMessage{ChatID: c2, Role: "user", Text: "Why did the login fix fail twice?"})
	if err != nil {
		return err
	}
	j, err := st.StartJob(ctx, store.Job{Kind: store.KindChat, Harness: "claude", Model: "opus", BudgetUSD: 2, Route: "orchestrate", Input: fmt.Sprintf(`{"chat":%d,"message":%d}`, c2, msg)})
	if err != nil {
		return err
	}
	if err := st.SetChatMessageJob(ctx, msg, j.ID); err != nil {
		return err
	}
	return st.FinishJob(ctx, j.ID, store.Result{State: state.Failed, CostUSD: 0.12, Error: "claude exited with code 1: overloaded"})
}
