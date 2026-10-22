package pipeline

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/claptimes5/factory/internal/runner"

	"github.com/claptimes5/factory/internal/state"
	"github.com/claptimes5/factory/internal/store"
)

func TestRetryTierUpAfterPark(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	// The retry hears why the job it retries failed.
	var in ImplementInput
	if err := jsonUnmarshal(next[0].Input, &in); err != nil || first.Error == "" || !strings.HasPrefix(in.PreviousFailure, first.Error) {
		t.Fatalf("retry input %s lacks %q (%v)", next[0].Input, first.Error, err)
	}
	if err := e.p.Park(ctx, it.ID, "not now"); err != nil {
		t.Fatal(err)
	}
	if n, _ := e.p.Store.CountJobs(ctx, state.Queued, ""); n != 0 {
		t.Fatalf("park should cancel the queued job, %d left", n)
	}
}

// A build cut short by a Ctrl-C (its git killed before the daemon cancels
// anything) runs again at the same attempt instead of using one up.
func TestInterruptedBuildKeepsItsAttempt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	e.addIssue(2)
	e.claude.builds = []func(string) error{func(string) error { return errors.New("git clone: signal: interrupt") }, goodBuild}
	e.codex.reviews = []string{pass}
	e.p.Drain(ctx)
	it := e.item(t, 2)
	jobs, _ := e.p.Store.ListJobs(ctx, store.JobFilter{ItemID: it.ID, Kind: "implement"})
	if it.Stage != state.AwaitingHuman || len(jobs) != 2 || jobs[1].State != state.Cancelled || jobs[0].Attempt != 1 {
		t.Fatalf("stage %s, jobs %+v", it.Stage, jobs)
	}
}

func TestCancelQueuedParks(t *testing.T) {
	t.Parallel()
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

// A cancel that lands after the agent finished (here, during the gates)
// still stops the job: no PR, and the item parks.
func TestCancelDuringGatesStopsBeforePR(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	e.addIssue(90)
	e.claude.builds = []func(string) error{goodBuild}
	e.codex.reviews = []string{pass}
	inner := e.p.GateExec
	e.p.GateExec = func(ctx context.Context, workdir, command string) (int, string, error) {
		running, _ := e.p.Store.ListJobs(context.Background(), store.JobFilter{State: state.Running})
		for _, j := range running {
			if j.Kind == "implement" {
				if err := e.p.Cancel(context.Background(), j.ID); err != nil {
					t.Errorf("cancel: %v", err)
				}
			}
		}
		return inner(ctx, workdir, command)
	}
	e.p.Drain(ctx)
	it := e.item(t, 90)
	if it.Stage != state.Parked || it.PRNumber != 0 {
		t.Fatalf("item = %+v", it)
	}
	jobs, _ := e.p.Store.ListJobs(ctx, store.JobFilter{ItemID: it.ID, Kind: "implement"})
	if len(jobs) != 1 || jobs[0].State != state.Cancelled {
		t.Fatalf("implement jobs = %+v", jobs)
	}
}

// A job whose item was parked while it waited in the queue does not run.
func TestParkedItemsQueuedJobDoesNotRun(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	it, _, _ := e.p.Store.UpsertItem(ctx, "o/r", 91, "x")
	e.p.enqueueTriage(ctx, it, 1, "")
	e.p.Store.SetStage(ctx, it.ID, state.Parked, "test", "")
	e.p.Drain(ctx)
	if e.claude.calls["triage"] != 0 {
		t.Fatal("triage ran for a parked item")
	}
	jobs, _ := e.p.Store.ListJobs(ctx, store.JobFilter{ItemID: it.ID})
	if jobs[0].State != state.Cancelled {
		t.Fatalf("job = %s", jobs[0].State)
	}
}

// With the Docker runner, a fix round on the same model resumes the
// previous attempt's Claude session; a session that is gone falls back to a
// fresh one. A retry after failed gates starts from a fresh checkout, so it
// does not resume.
func TestImplementRetryResumesSession(t *testing.T) {
	t.Parallel()
	fix := func(w string) error {
		writeFile(w, "add.go", "package fixture\n\n// Add adds, checked.\nfunc Add(a, b int) int { return a + b }\n")
		return commitIn(w, "fix")
	}
	for _, gone := range []bool{false, true} {
		ctx := context.Background()
		e := setup(t)
		e.p.Runner = &runner.Docker{}
		e.addIssue(95)
		e.claude.builds = []func(string) error{badBuild, goodBuild, fix}
		e.claude.noSess = gone
		e.codex.reviews = []string{block, resolveF1}
		e.p.Drain(ctx)
		if st := e.item(t, 95).Stage; st != state.AwaitingHuman {
			t.Fatalf("gone=%v: stage = %s", gone, st)
		}
		want := []string{"", "", "session-1"}
		if gone {
			want = append(want, "")
		}
		if strings.Join(e.claude.resumes, ",") != strings.Join(want, ",") {
			t.Fatalf("gone=%v: resumes = %q", gone, e.claude.resumes)
		}
	}
}

// A reviewer may run the builder's own harness and model, so what keeps
// it independent is its session: the review, and the verifier it sends
// findings to, each start a fresh one, never resuming the build's nor
// sharing the item's session directory, even with the Docker runner, where
// a fix round on that model does resume the build.
func TestReviewNeverResumesTheBuildersSession(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	e.p.Runner = &runner.Docker{}
	e.p.Cfg.Routing["review"] = e.p.Cfg.Routing["implement.S"] // claude/sonnet, as the builder
	e.addIssue(96)
	e.claude.builds = []func(string) error{goodBuild, fixer()}
	e.claude.verifies = []string{confirmF1}
	e.claude.reviews = []string{block, resolveF1}
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if st := e.item(t, 96).Stage; st != state.AwaitingHuman {
		t.Fatalf("stage = %s", st)
	}
	if e.codex.calls["review"] != 0 || e.claude.calls["review"] != 2 || e.claude.calls["verify"] != 1 {
		t.Fatalf("calls: claude %v, codex %v", e.claude.calls, e.codex.calls)
	}
	// The fix round does resume the build: there is a session to take.
	if got := e.claude.resumes; len(got) != 2 || got[1] != "session-0" {
		t.Fatalf("implement resumes = %q", got)
	}
	for stage, ss := range e.claude.sessions {
		if stage == "implement" {
			continue
		}
		for i, s := range ss {
			if s != [2]string{} {
				t.Errorf("%s call %d ran with resume %q, session dir %q: want a fresh session", stage, i, s[0], s[1])
			}
		}
	}
}
