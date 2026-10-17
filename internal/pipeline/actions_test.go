package pipeline

import (
	"context"
	"testing"

	"github.com/claptimes5/factory/internal/state"
	"github.com/claptimes5/factory/internal/store"
)

func TestRetryTierUpAfterPark(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	e.p.Cfg.Escalation.Implement = []string{"sonnet", "opus", "opus-max"}
	e.addIssue(20)
	e.claude.builds = []func(string) error{badBuild, badBuild, badBuild, badBuild, badBuild, badBuild, goodBuild}
	e.p.Drain(ctx) // fails twice on each of three tiers, then parks
	it := e.item(t, 20)
	if it.Stage != state.Parked {
		t.Fatalf("stage = %s", it.Stage)
	}
	jobs, _ := e.p.Store.ListJobs(ctx, store.JobFilter{ItemID: it.ID, Kind: "implement"})
	if err := e.p.Retry(ctx, jobs[0].ID, true); err == nil {
		t.Fatal("tier up from the top tier must be refused")
	}
	if err := e.p.Retry(ctx, jobs[0].ID, false); err != nil {
		t.Fatal(err)
	}
	if err := e.p.Retry(ctx, jobs[0].ID, false); err == nil {
		t.Fatal("a second retry while one is queued must be refused")
	}
	e.codex.reviews = []string{pass}
	e.p.Drain(ctx)
	if e.item(t, 20).Stage != state.AwaitingHuman {
		t.Fatalf("stage = %s", e.item(t, 20).Stage)
	}
}

func TestRetryTierUpAttempt(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	e.addIssue(21)
	e.p.Drain(ctx) // no scripted builds: every attempt fails and the item parks
	it := e.item(t, 21)
	if it.Stage != state.Parked {
		t.Fatalf("stage = %s", it.Stage)
	}
	jobs, _ := e.p.Store.ListJobs(ctx, store.JobFilter{ItemID: it.ID, Kind: "implement"})
	first := jobs[len(jobs)-1]
	if err := e.p.Retry(ctx, first.ID, true); err != nil {
		t.Fatal(err)
	}
	next, _ := e.p.Store.ListJobs(ctx, store.JobFilter{ItemID: it.ID, Kind: "implement", State: state.Queued})
	if len(next) != 1 || next[0].Model != "opus" || next[0].Attempt != 3 {
		t.Fatalf("next = %+v", next)
	}
	if err := e.p.Park(ctx, it.ID, "not now"); err != nil {
		t.Fatal(err)
	}
	if n, _ := e.p.Store.CountJobs(ctx, state.Queued, ""); n != 0 {
		t.Fatalf("park should cancel the queued job, %d left", n)
	}
}

func TestCancelQueuedParks(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	e.addIssue(22)
	e.p.Intake(ctx)
	jobs, _ := e.p.Store.ListJobs(ctx, store.JobFilter{State: state.Queued})
	if err := e.p.Cancel(ctx, jobs[0].ID); err != nil {
		t.Fatal(err)
	}
	if e.item(t, 22).Stage != state.Parked {
		t.Fatal("cancelling should park the item")
	}
	if err := e.p.Cancel(ctx, jobs[0].ID); err == nil {
		t.Fatal("cancelling twice must fail")
	}
}
