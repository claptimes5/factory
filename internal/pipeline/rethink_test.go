package pipeline

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/claptimes5/factory/internal/state"
	"github.com/claptimes5/factory/internal/store"
)

func fixBuild() func(string) error {
	round := 0
	return func(w string) error {
		round++
		writeFile(w, "add.go", fmt.Sprintf("package fixture\n\n// Add adds (round %d).\nfunc Add(a, b int) int { return a + b }\n", round))
		return commitIn(w, "fix")
	}
}

func rethinkEvents(t *testing.T, e *env, itemID int64) int {
	t.Helper()
	evs, err := e.p.Store.Events(context.Background(), itemID)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, ev := range evs {
		if ev.Type == "rethink" {
			n++
		}
	}
	return n
}

// The round cap earns one fresh build from the base branch, one size up,
// with the findings and the note that the approach may be wrong.
func TestRethinkAtRoundCap(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	e.addIssue(4)
	fix := fixBuild()
	e.claude.builds = []func(string) error{goodBuild, fix, fix, goodBuild}
	e.codex.reviews = []string{block, block, block, pass}
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	it := e.item(t, 4)
	if it.Stage != state.AwaitingHuman || it.PRNumber == 0 {
		t.Fatalf("item = %s, PR %d", it.Stage, it.PRNumber)
	}
	jobs, _ := e.p.Store.ListJobs(ctx, store.JobFilter{ItemID: it.ID, Kind: "implement"})
	if len(jobs) != 4 || !isRethink(jobs[0]) || isRethink(jobs[1]) {
		t.Fatalf("implement jobs = %d", len(jobs))
	}
	// Triage rated it M: the rethink builds on implement.L.
	if jobs[0].Route != "implement.L" || jobs[0].Model != "opus" || jobs[0].Attempt != 1 {
		t.Fatalf("rethink job = %s %s attempt %d", jobs[0].Route, jobs[0].Model, jobs[0].Attempt)
	}
	// The rethink's first reviewer rechecks what the dropped approach left.
	if rp := e.codex.prompts["review"]; !strings.Contains(rp[3], "A second approach") || !strings.Contains(rp[3], "overflow unhandled") || strings.Contains(rp[0], "A second approach") {
		t.Fatalf("rethink review prompt:\n%s", rp[3])
	}
	prompt := e.claude.prompts["implement"][3]
	if !strings.Contains(prompt, "approach itself may be wrong") || !strings.Contains(prompt, "overflow unhandled") ||
		strings.Contains(prompt, "This is a fix round") || strings.Contains(prompt, `"answers"`) {
		t.Fatalf("rethink prompt:\n%s", prompt)
	}
	// It started over: the branch holds the fresh build alone.
	if got := git(t, e.remote, "log", "--format=%s", "factory/issue-4"); strings.Count(got, "factory: #4") != 1 {
		t.Fatalf("branch history = %q", got)
	}
	if rethinkEvents(t, e, it.ID) != 1 {
		t.Fatal("the rethink should be recorded as an event")
	}
	if c := strings.Join(e.gh.comments[4], "\n"); !strings.Contains(c, "tries once more") {
		t.Fatalf("comments %q", c)
	}
	if b := e.gh.bodies[it.PRNumber]; !strings.Contains(b, "second approach") {
		t.Fatalf("PR body should say a rethink happened:\n%s", b)
	}
}

// A rethink that hits the cap too parks, saying so; its fix rounds stay on
// the rethink's route. Resuming the item after a restart doesn't rethink
// again.
func TestRethinkCapParksOnce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	e.addIssue(4)
	fix := fixBuild()
	e.claude.builds = []func(string) error{goodBuild, fix, fix, goodBuild, fix, fix}
	e.codex.reviews = []string{block, block, block, block, block, block}
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	it := e.item(t, 4)
	if it.Stage != state.Parked {
		t.Fatalf("stage = %s", it.Stage)
	}
	if e.claude.calls["implement"] != 6 || e.codex.calls["review"] != 6 {
		t.Fatalf("calls = %v %v", e.claude.calls, e.codex.calls)
	}
	jobs, _ := e.p.Store.ListJobs(ctx, store.JobFilter{ItemID: it.ID, Kind: "implement"})
	for _, j := range jobs[:3] {
		if j.Route != "implement.L" {
			t.Fatalf("job %d after the rethink on %s", j.ID, j.Route)
		}
	}
	c := e.gh.comments[4]
	if !strings.Contains(c[len(c)-1], "second, fresh attempt") {
		t.Fatalf("park notice = %q", c[len(c)-1])
	}
	// A restart before the park landed: the item resumes from its last
	// review and parks, without a second rethink.
	all, _ := e.p.Store.ListJobs(ctx, store.JobFilter{ItemID: it.ID})
	if err := e.p.resume(ctx, it, all); err != nil {
		t.Fatal(err)
	}
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if it = e.item(t, 4); it.Stage != state.Parked || e.claude.calls["implement"] != 6 || rethinkEvents(t, e, it.ID) != 1 {
		t.Fatalf("after restart: %s, %d builds, %d rethinks", it.Stage, e.claude.calls["implement"], rethinkEvents(t, e, it.ID))
	}
	// A retry goes by the rethink's route: L is on the top tier already.
	if err := e.p.Retry(ctx, jobs[0].ID, true); err == nil || !strings.Contains(err.Error(), "top model tier") {
		t.Fatalf("tier-up retry = %v", err)
	}
	if err := e.p.Retry(ctx, jobs[0].ID, false); err != nil {
		t.Fatal(err)
	}
	if q, _ := e.p.Store.ListJobs(ctx, store.JobFilter{ItemID: it.ID, Kind: "implement", State: state.Queued}); len(q) != 1 || q[0].Route != "implement.L" {
		t.Fatalf("retry = %+v", q)
	}
}

// review.rethink: false parks at the cap, as before.
func TestRethinkOff(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	off := false
	e.p.Cfg.Reviews.Rethink = &off
	e.addIssue(4)
	fix := fixBuild()
	e.claude.builds = []func(string) error{goodBuild, fix, fix}
	e.codex.reviews = []string{block, block, block}
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	it := e.item(t, 4)
	if it.Stage != state.Parked || e.claude.calls["implement"] != 3 || rethinkEvents(t, e, it.ID) != 0 {
		t.Fatalf("stage %s, %d builds", it.Stage, e.claude.calls["implement"])
	}
}

// Questions park at once, with no rethink.
func TestRethinkNotForQuestions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	e.addIssue(4)
	e.claude.builds = []func(string) error{goodBuild}
	e.codex.reviews = []string{`{"decision":"questions","findings":[{"severity":"major","file":"add.go","line":1,"claim":"which package?","evidence":"-"}],"challenged_assumptions":[]}`}
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	it := e.item(t, 4)
	if it.Stage != state.Parked || e.claude.calls["implement"] != 1 || rethinkEvents(t, e, it.ID) != 0 {
		t.Fatalf("stage %s, %d builds", it.Stage, e.claude.calls["implement"])
	}
}

// An item whose estimate can't cover another build and review parks at
// the cap, saying why.
func TestRethinkNotOverBudget(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	e.p.Cfg.Budgets.ConfirmOver = 10 // M's estimate is $9 at most; a rethink on L adds $18
	e.addIssue(4)
	fix := fixBuild()
	e.claude.builds = []func(string) error{goodBuild, fix, fix}
	e.codex.reviews = []string{block, block, block}
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	it := e.item(t, 4)
	if it.Stage != state.Parked || e.claude.calls["implement"] != 3 || rethinkEvents(t, e, it.ID) != 0 {
		t.Fatalf("stage %s, %d builds", it.Stage, e.claude.calls["implement"])
	}
	c := e.gh.comments[4]
	if !strings.Contains(c[len(c)-1], "No fresh attempt first") {
		t.Fatalf("park notice = %q", c[len(c)-1])
	}
}

// With no route one size up, the rethink builds on the item's own; with
// neither, the item parks at the cap and says so.
func TestRethinkRouteFallback(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	e.claude.triage = `{"decision":"ready","complexity":"S","risk":"low","question":"","summary":"add Add"}`
	delete(e.p.Cfg.Routing, "implement.M")
	e.addIssue(4)
	fix := fixBuild()
	e.claude.builds = []func(string) error{goodBuild, fix, fix, goodBuild}
	e.codex.reviews = []string{block, block, block, pass}
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	it := e.item(t, 4)
	jobs, _ := e.p.Store.ListJobs(ctx, store.JobFilter{ItemID: it.ID, Kind: "implement"})
	if it.Stage != state.AwaitingHuman || len(jobs) != 4 || !isRethink(jobs[0]) || jobs[0].Route != "implement.S" {
		t.Fatalf("stage %s, %d builds", it.Stage, len(jobs))
	}

	delete(e.p.Cfg.Routing, "implement.S")
	if _, why := e.p.rethinkTo(&store.WorkItem{Complexity: "S"}); !strings.Contains(why, "no routing.implement.M") {
		t.Fatalf("why = %q", why)
	}
}
