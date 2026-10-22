package pipeline

import (
	"context"
	"testing"
	"time"

	"github.com/claptimes5/factory/internal/readiness"
	"github.com/claptimes5/factory/internal/state"
	"github.com/claptimes5/factory/internal/store"
)

// While the readiness check has found blockers, only the work fixing
// them runs; a task waits in the queue, untouched, and starts once a
// check finds the repo ready.
func TestTasksWaitForSetup(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	notReady := &readiness.Report{Repo: "o/r", Branch: "main", Started: time.Now(), Findings: []readiness.Finding{
		{Level: readiness.Blocker, Area: "checks", Title: "`lint` fails on main", Issue: &readiness.Issue{Key: "check:lint", Number: 9}}}}
	if _, err := readiness.Save(ReadinessDir(e.p.Cfg), notReady); err != nil {
		t.Fatal(err)
	}
	e.addIssue(9)
	e.addIssue(16)
	e.claude.builds = []func(string) error{goodBuild, goodBuild}
	e.codex.reviews = []string{pass, pass}
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if st := e.item(t, 9).Stage; st != state.AwaitingHuman {
		t.Fatalf("the blocker's fix = %s", st)
	}
	task := e.item(t, 16)
	jobs, _ := e.p.Store.ListJobs(ctx, store.JobFilter{ItemID: task.ID})
	if len(jobs) != 1 || jobs[0].State != state.Queued || !e.p.SetupHolds(ctx, task) {
		t.Fatalf("the task ran before setup: %d jobs, first %+v", len(jobs), jobs[0])
	}
	notReady.Ready, notReady.Findings = true, nil
	if _, err := readiness.Save(ReadinessDir(e.p.Cfg), notReady); err != nil {
		t.Fatal(err)
	}
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if st := e.item(t, 16).Stage; st != state.AwaitingHuman {
		t.Fatalf("the task didn't start once setup passed: %s", st)
	}
}

// A build that fails a check the setup check found failing on the base
// branch too parks after one try: no model gets past it, so it isn't
// retried or offered a tier up. (Incomplete, the check holds nothing.)
func TestFailingWhatFailsOnBaseParks(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	red := &readiness.Report{Repo: "o/r", Branch: "main", Started: time.Now(), Incomplete: true, Findings: []readiness.Finding{
		{Level: readiness.Blocker, Area: "checks", Title: "`test` fails on main", Issue: &readiness.Issue{Key: "check:test"}}}}
	if _, err := readiness.Save(ReadinessDir(e.p.Cfg), red); err != nil {
		t.Fatal(err)
	}
	e.addIssue(2)
	e.claude.builds = []func(string) error{badBuild, goodBuild}
	e.p.Drain(ctx)
	it := e.item(t, 2)
	jobs, _ := e.p.Store.ListJobs(ctx, store.JobFilter{ItemID: it.ID, Kind: "implement"})
	if it.Stage != state.Parked || len(jobs) != 1 {
		t.Fatalf("stage = %s after %d builds", it.Stage, len(jobs))
	}
	for _, c := range e.p.Choices(ctx, jobs[0].ID) {
		if c.Action == "tier_up" && c.Available {
			t.Fatalf("tier up offered: %+v", c)
		}
	}
}
