package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/claptimes5/factory/internal/state"
	"github.com/claptimes5/factory/internal/store"
)

// Attached files go once no one needs them: a chat's after it has been
// quiet for orchestrator.keep_files_for, a build's once its item is done
// or after as long, a chat reply's copies once it ended; never a running
// build's.
func TestSweepAttachmentsRemovesWhatNoOneNeeds(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	attach := func(dir string) string {
		if _, err := saveAttachments(dir, []store.Attachment{{Name: "a.png", Data: []byte("png")}}); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	jobFiles := func(item *int64, kind string, end bool) string {
		j, err := e.p.Store.StartJob(ctx, store.Job{ItemID: item, Kind: kind, Harness: "claude", Model: "m", BudgetUSD: 1})
		if err != nil {
			t.Fatal(err)
		}
		if end {
			e.p.Store.FinishJob(ctx, j.ID, store.Result{State: state.Succeeded})
		}
		return attach(filepath.Join(e.p.jobDir(j.ID), "out", attachDir))
	}
	chat, _ := e.p.Store.NewChat(ctx, "x")
	msg, _ := e.p.Store.AddChatMessage(ctx, store.ChatMessage{ChatID: chat, Role: "user", Text: "see"})
	chatFiles := attach(e.p.ChatFiles(chat, msg))
	done, _, _ := e.p.Store.UpsertItem(ctx, "o/r", 1, "done")
	e.p.Store.SetStage(ctx, done.ID, state.Done, "human", "merged")
	open, _, _ := e.p.Store.UpsertItem(ctx, "o/r", 2, "open")
	doneBuild := jobFiles(&done.ID, "implement", true)
	openBuild := jobFiles(&open.ID, "implement", true)
	running := jobFiles(&open.ID, "implement", false)
	reply := jobFiles(nil, store.KindChat, true)

	there := func(dir string) bool { _, err := os.Stat(dir); return err == nil }
	e.p.sweepAttachments(ctx, time.Now())
	if !there(chatFiles) || !there(openBuild) || !there(running) {
		t.Fatal("removed files still needed")
	}
	if there(doneBuild) || there(reply) {
		t.Fatal("kept a done item's files, or a finished reply's copies")
	}
	e.p.sweepAttachments(ctx, time.Now().Add(e.p.Cfg.Orchestrator.KeepFilesFor.Duration+time.Hour))
	if there(chatFiles) || there(openBuild) {
		t.Fatal("kept files past orchestrator.keep_files_for")
	}
	if !there(running) {
		t.Fatal("removed a running build's files")
	}
}
