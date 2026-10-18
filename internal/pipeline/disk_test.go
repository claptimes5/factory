package pipeline

import (
	"context"
	"strings"
	"testing"

	"github.com/claptimes5/factory/internal/state"
)

// No job starts without room on disk; you hear about it once, and again
// when there's room.
func TestNoJobsWhenDiskIsLow(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	var said []string
	e.p.Notify = func(_ context.Context, m string) { said = append(said, m) }
	e.p.Cfg.Budgets.MinFreeDiskGB = 1e12 // more than any disk has
	e.addIssue(1)
	for range 2 {
		if err := e.p.Drain(ctx); err != nil {
			t.Fatal(err)
		}
		e.p.diskAt = e.p.diskAt.Add(-DiskEvery) // measure again
	}
	if n, _ := e.p.Store.CountJobs(ctx, state.Queued, "triage"); n != 1 {
		t.Fatalf("queued triage = %d", n)
	}
	if len(said) != 1 || !strings.Contains(said[0], "the data directory") || !strings.Contains(e.p.DiskLow(), "Low on disk") {
		t.Fatalf("notified %q", said)
	}
	e.p.Cfg.Budgets.MinFreeDiskGB = 0.000001
	e.claude.builds = []func(string) error{goodBuild}
	e.codex.reviews = []string{pass}
	e.p.diskAt = e.p.diskAt.Add(-DiskEvery)
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if it, _ := e.p.Store.ItemByIssue(ctx, "o/r", 1); it.Stage != state.AwaitingHuman {
		t.Fatalf("stage = %s", it.Stage)
	}
	if len(said) < 2 || !strings.Contains(said[1], "room on disk again") || e.p.DiskLow() != "" {
		t.Fatalf("notified %q", said)
	}
}
