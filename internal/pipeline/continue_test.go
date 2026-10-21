package pipeline

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/claptimes5/factory/internal/harness"
	"github.com/claptimes5/factory/internal/runner"
	"github.com/claptimes5/factory/internal/state"
	"github.com/claptimes5/factory/internal/store"
)

// halfBuild leaves Add written but uncommitted, as a builder cut off
// mid-task would.
func halfBuild(work string) error {
	return writeFile(work, "add.go", "package fixture\n\nfunc Add(a, b int) int { return a + b }\n")
}

// finishBuild needs the half the stopped build left.
func finishBuild(work string) error {
	if _, err := os.Stat(filepath.Join(work, "add.go")); err != nil {
		return err
	}
	writeFile(work, "add_test.go", "package fixture\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif Add(1, 2) != 3 {\n\t\tt.Fatal()\n\t}\n}\n")
	return commitIn(work, "add Add")
}

func implJobs(t *testing.T, e *env, itemID int64) []*store.Job {
	t.Helper()
	jobs, err := e.p.Store.ListJobs(context.Background(), store.JobFilter{ItemID: itemID, Kind: "implement"})
	if err != nil {
		t.Fatal(err)
	}
	return jobs // newest first
}

// A build that runs out of turns keeps its work, pushes nothing, doesn't
// restart on its own, and waits for a person with its choices.
func TestStoppedBuildKeepsWorkAndWaits(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	e.addIssue(120)
	e.claude.builds = []func(string) error{halfBuild}
	e.claude.stops = []string{"turns"}
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	it := e.item(t, 120)
	if it.Stage != state.Parked {
		t.Fatalf("stage = %s", it.Stage)
	}
	jobs := implJobs(t, e, it.ID)
	if len(jobs) != 1 || e.claude.calls["implement"] != 1 {
		t.Fatalf("it restarted: %d jobs, %d calls", len(jobs), e.claude.calls["implement"])
	}
	j := jobs[0]
	if j.State != state.Failed || !strings.Contains(j.Error, "ran out of turns (80 of 80)") || !strings.Contains(j.Error, "work is kept") {
		t.Fatalf("job = %s %q", j.State, j.Error)
	}
	if e.claude.turns[0] != 80 {
		t.Fatalf("max turns = %v", e.claude.turns)
	}
	dir := e.p.jobDir(j.ID)
	if _, err := os.Stat(filepath.Join(dir, "kept", "work", "add.go")); err != nil {
		t.Fatalf("checkout not kept: %v", err)
	}
	patch, _ := os.ReadFile(filepath.Join(dir, "saved.patch"))
	if !strings.Contains(string(patch), "+func Add") {
		t.Fatalf("saved.patch = %q", patch)
	}
	rec := e.p.readStop(j.ID)
	if rec == nil || !rec.Kept || rec.Reason != "turns" || !strings.Contains(rec.Note, "Left: the test.") {
		t.Fatalf("stop record = %+v", rec)
	}
	// Nothing reached the remote.
	if out, _ := runGit(e.remote, "branch", "--list", "factory/*"); out != "" {
		t.Fatalf("pushed: %q", out)
	}
	c := e.gh.comments[120]
	last := c[len(c)-1]
	for _, want := range []string{"ran out of turns", "Continue picks up", "Retry starts it from scratch", "factory dashboard (Items > #120)", "factory:retry"} {
		if !strings.Contains(last, want) {
			t.Fatalf("comment lacks %q:\n%s", want, last)
		}
	}
	if strings.Contains(last, "http://") || strings.Contains(last, "localhost") {
		t.Fatalf("a public comment links an internal address:\n%s", last)
	}
	// The choices say what each does and on which model.
	got := map[string]Choice{}
	for _, ch := range e.p.Choices(ctx, j.ID) {
		got[ch.Action] = ch
	}
	if c := got["continue"]; !c.Available || c.Model != "claude/sonnet" || !strings.Contains(c.Explain, "80 turns") {
		t.Fatalf("continue = %+v", c)
	}
	if c := got["retry"]; !c.Available || c.Model != "claude/sonnet" || !strings.Contains(c.Explain, "from scratch") {
		t.Fatalf("retry = %+v", c)
	}
	if c := got["tier_up"]; !c.Available || c.Model != "claude/opus" {
		t.Fatalf("tier up = %+v", c)
	}
	if c := got["park"]; c.Available || !strings.Contains(c.Why, "parked now") {
		t.Fatalf("park = %+v", c)
	}
}

// Continue picks up in the kept checkout, with more turns and budget and
// the person's note, and resumes the Claude session; the checkout is
// gone once taken.
func TestContinueResumesInKeptCheckout(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	e.p.Runner = &runner.Docker{}
	e.addIssue(121)
	e.claude.builds = []func(string) error{halfBuild, finishBuild}
	e.claude.stops = []string{"turns"}
	e.codex.reviews = []string{pass}
	e.p.Drain(ctx)
	it := e.item(t, 121)
	stopped := implJobs(t, e, it.ID)[0]
	if err := e.p.Continue(ctx, stopped.ID, 3, ""); err == nil {
		t.Fatal("more than +200% was allowed")
	}
	if err := e.p.Continue(ctx, stopped.ID, 0.5, "Only add Add; skip the docs."); err != nil {
		t.Fatal(err)
	}
	if err := e.p.Continue(ctx, stopped.ID, 0, ""); err == nil {
		t.Fatal("a second continue was queued")
	}
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if st := e.item(t, 121).Stage; st != state.AwaitingHuman {
		t.Fatalf("stage = %s", st)
	}
	jobs := implJobs(t, e, it.ID)
	cont := jobs[0]
	if cont.Attempt != stopped.Attempt || cont.Model != stopped.Model || cont.BudgetUSD != 9 {
		t.Fatalf("continue job = attempt %d model %s budget %v", cont.Attempt, cont.Model, cont.BudgetUSD)
	}
	if e.claude.turns[1] != 120 {
		t.Fatalf("turns = %v", e.claude.turns)
	}
	if got := e.claude.resumes; len(got) != 2 || got[1] != "session-0" {
		t.Fatalf("resumes = %q", got)
	}
	prompt := e.claude.prompts["implement"][1]
	for _, want := range []string{"Continue where you stopped", "Left: the test.", "Only add Add; skip the docs.", "120 turns and $9.00"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt lacks %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "Rules:") {
		t.Fatal("a resumed session got the whole task again")
	}
	if _, err := os.Stat(keptDir(e.p.jobDir(stopped.ID))); !os.IsNotExist(err) {
		t.Fatalf("kept checkout still there: %v", err)
	}
	// The note shows in the transcript where it went in.
	log, _ := os.ReadFile(filepath.Join(e.p.jobDir(cont.ID), "log.jsonl"))
	if !strings.Contains(string(log), `"factory_message"`) || !strings.Contains(string(log), "skip the docs") {
		t.Fatalf("log = %s", log)
	}
}

// A session that's gone falls back to a new one with the whole task and
// the continue section; a Retry instead drops the kept work.
func TestContinueFallbacksAndRetryDropsKept(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	e.p.Runner = &runner.Docker{}
	e.addIssue(122)
	e.claude.builds = []func(string) error{halfBuild, finishBuild}
	e.claude.stops = []string{"budget"}
	e.claude.noSess = true
	e.codex.reviews = []string{pass}
	e.p.Drain(ctx)
	it := e.item(t, 122)
	stopped := implJobs(t, e, it.ID)[0]
	if rec := e.p.readStop(stopped.ID); rec == nil || !strings.Contains(rec.Summary, "$6.00 budget") {
		t.Fatalf("stop = %+v", rec)
	}
	if err := e.p.Continue(ctx, stopped.ID, 0, ""); err != nil {
		t.Fatal(err)
	}
	e.p.Drain(ctx)
	if st := e.item(t, 122).Stage; st != state.AwaitingHuman {
		t.Fatalf("stage = %s", st)
	}
	prompts := e.claude.prompts["implement"]
	if last := prompts[len(prompts)-1]; !strings.Contains(last, "Rules:") || !strings.Contains(last, "Continue where you stopped") {
		t.Fatalf("fresh session prompt:\n%s", last)
	}

	// Another item stops; a retry starts over and the kept work goes.
	e2 := setup(t)
	e2.addIssue(123)
	e2.claude.builds = []func(string) error{halfBuild, goodBuild}
	e2.claude.stops = []string{"turns"}
	e2.codex.reviews = []string{pass}
	e2.p.Drain(ctx)
	it2 := e2.item(t, 123)
	stopped2 := implJobs(t, e2, it2.ID)[0]
	if err := e2.p.Retry(ctx, stopped2.ID, false); err != nil {
		t.Fatal(err)
	}
	e2.p.Drain(ctx)
	if _, err := os.Stat(keptDir(e2.p.jobDir(stopped2.ID))); !os.IsNotExist(err) {
		t.Fatalf("kept checkout survived a retry: %v", err)
	}
	if err := e2.p.Continue(ctx, stopped2.ID, 0, ""); err == nil {
		t.Fatal("continued a build whose work is gone")
	}
	for _, ch := range e2.p.Choices(ctx, stopped2.ID) {
		if ch.Action == "continue" {
			t.Fatalf("continue offered with nothing kept: %+v", ch)
		}
	}
}

// Steering reaches only a running Claude build.
func TestSteerRefusals(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	it, _, _ := e.p.Store.UpsertItem(ctx, "o/r", 124, "x")
	id, _ := e.p.Store.EnqueueJob(ctx, store.Job{ItemID: &it.ID, Kind: "implement", Harness: "codex", Model: "m", Attempt: 1, BudgetUSD: 1})
	if err := e.p.Steer(ctx, id, "hi"); err == nil || !strings.Contains(err.Error(), "not running") {
		t.Fatalf("queued: %v", err)
	}
	if err := e.p.Steer(ctx, id, " "); err == nil {
		t.Fatal("empty message sent")
	}
	if why := SteerUnsupported(&store.Job{Kind: "implement", Harness: "codex"}); !strings.Contains(why, "Codex") {
		t.Fatalf("why = %q", why)
	}
}

// turnsSeen records each stage's turn cap.
type turnsSeen struct {
	harness.Harness
	mu   sync.Mutex
	caps map[string]int
}

func (h *turnsSeen) Run(ctx context.Context, req harness.RunRequest, ev chan<- harness.Event) (*harness.Result, error) {
	h.mu.Lock()
	h.caps[req.Stage] = req.MaxTurns
	h.mu.Unlock()
	return h.Harness.Run(ctx, req, ev)
}

// budgets.max_turns is the builder's; the other stages keep theirs.
func TestMaxTurnsConfig(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	e.p.Cfg.Budgets.MaxTurns = 33
	seen := &turnsSeen{caps: map[string]int{}}
	for name, h := range e.p.Harnesses {
		e.p.Harnesses[name] = &turnsSeen{Harness: h, caps: seen.caps}
	}
	e.addIssue(125)
	e.claude.builds = []func(string) error{goodBuild}
	e.codex.reviews = []string{pass}
	e.p.Drain(ctx)
	if len(e.claude.turns) != 1 || e.claude.turns[0] != 33 {
		t.Fatalf("turns = %v", e.claude.turns)
	}
	if seen.caps["triage"] != stageTurns || seen.caps["review"] != stageTurns {
		t.Fatalf("other stages: %v", seen.caps)
	}
}

func runGit(dir string, args ...string) (string, error) {
	g := &Git{}
	return g.run(context.Background(), dir, nil, args...)
}

// stopOne runs issue n's first build into its turn cap and returns the
// stopped job; builds are the ones after it.
func stopOne(t *testing.T, e *env, n int, builds ...func(string) error) *store.Job {
	t.Helper()
	e.p.Runner = &runner.Docker{}
	e.addIssue(n)
	e.claude.builds = append([]func(string) error{halfBuild}, builds...)
	e.claude.stops = []string{"turns"}
	e.codex.reviews = []string{pass}
	if err := e.p.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	return implJobs(t, e, e.item(t, n).ID)[0]
}

func keptWork(e *env, id int64) bool {
	_, err := os.Stat(filepath.Join(keptDir(e.p.jobDir(id)), "work", "add.go"))
	return err == nil
}

// A continue cancelled mid-run gives the work back to the build it
// continued, and the item can continue again from there.
func TestCancelledContinueGivesTheWorkBack(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	var stopped *store.Job
	cancelIt := func(string) error {
		jobs := implJobs(t, e, *stopped.ItemID)
		// Docker isn't here to kill the container; the cancel still lands.
		_ = e.p.Cancel(ctx, jobs[0].ID)
		return nil
	}
	stopped = stopOne(t, e, 126, cancelIt, finishBuild)
	if err := e.p.Continue(ctx, stopped.ID, 0, ""); err != nil {
		t.Fatal(err)
	}
	e.p.Drain(ctx)
	cont := implJobs(t, e, *stopped.ItemID)[0]
	if cont.State != state.Cancelled || e.item(t, 126).Stage != state.Parked {
		t.Fatalf("continue = %s, item %s", cont.State, e.item(t, 126).Stage)
	}
	if !keptWork(e, stopped.ID) || e.p.keptTakenBy(stopped.ID) != 0 {
		t.Fatal("the cancelled continue lost the kept work")
	}
	// Continue from the cancelled job picks up the same work.
	if err := e.p.Continue(ctx, cont.ID, 0, ""); err != nil {
		t.Fatal(err)
	}
	e.p.Drain(ctx)
	if st := e.item(t, 126).Stage; st != state.AwaitingHuman {
		t.Fatalf("stage = %s", st)
	}
	if _, err := os.Stat(keptDir(e.p.jobDir(stopped.ID))); !os.IsNotExist(err) {
		t.Fatalf("kept work still there once used: %v", err)
	}
}

// A continue the daemon restarts in the middle gets the kept work back
// before its rerun; a continue whose kept work is gone says so and
// builds nothing.
func TestContinueSurvivesRestartAndSaysWhenWorkIsGone(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	stopped := stopOne(t, e, 127, finishBuild)
	if err := e.p.Continue(ctx, stopped.ID, 0, ""); err != nil {
		t.Fatal(err)
	}
	cont := implJobs(t, e, *stopped.ItemID)[0]
	// The interrupted run had taken the work into its own directory.
	if _, err := e.p.takeKept(e.p.jobDir(cont.ID), stopped.ID, cont.ID); err != nil {
		t.Fatal(err)
	}
	if keptWork(e, stopped.ID) || e.p.keptTakenBy(stopped.ID) != cont.ID {
		t.Fatal("not taken")
	}
	e.p.restoreInterrupted(cont)
	if !keptWork(e, stopped.ID) {
		t.Fatal("the restart lost the kept work")
	}
	e.p.Drain(ctx)
	if st := e.item(t, 127).Stage; st != state.AwaitingHuman {
		t.Fatalf("stage = %s", st)
	}

	e2 := setup(t)
	stopped2 := stopOne(t, e2, 128, finishBuild)
	if err := e2.p.Continue(ctx, stopped2.ID, 0, ""); err != nil {
		t.Fatal(err)
	}
	os.RemoveAll(filepath.Join(keptDir(e2.p.jobDir(stopped2.ID)), "work"))
	e2.p.Drain(ctx)
	it := e2.item(t, 128)
	cont2 := implJobs(t, e2, it.ID)[0]
	if it.Stage != state.Parked || cont2.State != state.Failed || !strings.Contains(cont2.Error, "the work it kept is gone") {
		t.Fatalf("item %s, job %s %q", it.Stage, cont2.State, cont2.Error)
	}
	if e2.claude.calls["implement"] != 1 {
		t.Fatalf("it built anyway: %d builds", e2.claude.calls["implement"])
	}
}

// Kept work goes only on a definite answer: a store error keeps it.
func TestKeptStaleNeedsADefiniteAnswer(t *testing.T) {
	t.Parallel()
	e := setup(t)
	stopped := stopOne(t, e, 129)
	gone, cancel := context.WithCancel(context.Background())
	cancel()
	if why := e.p.keptStale(gone, stopped.ID); why != "" {
		t.Fatalf("a failed lookup removes kept work: %q", why)
	}
	if why := e.p.keptStale(context.Background(), stopped.ID+1000); why != "its job is gone" {
		t.Fatalf("missing job: %q", why)
	}
	if why := e.p.keptStale(context.Background(), stopped.ID); why != "" {
		t.Fatalf("live: %q", why)
	}
}

// Old kept work a continue has taken isn't swept: a cancel gives it back.
func TestKeptStaleSparesTakenWork(t *testing.T) {
	t.Parallel()
	e := setup(t)
	stopped := stopOne(t, e, 130)
	path := filepath.Join(keptDir(e.p.jobDir(stopped.ID)), "kept.json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var k map[string]any
	if err := json.Unmarshal(b, &k); err != nil {
		t.Fatal(err)
	}
	k["at"] = time.Now().Add(-keepStoppedFor - time.Hour).UTC().Format(store.TimeFormat)
	b, _ = json.Marshal(k)
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	if why := e.p.keptStale(context.Background(), stopped.ID); why == "" {
		t.Fatal("old untaken work isn't swept")
	}
	if err := os.WriteFile(filepath.Join(keptDir(e.p.jobDir(stopped.ID)), takenFile), []byte("999"), 0o644); err != nil {
		t.Fatal(err)
	}
	if why := e.p.keptStale(context.Background(), stopped.ID); why != "" {
		t.Fatalf("taken work is swept: %q", why)
	}
}

// A build the factory's shutdown cuts short keeps its work and is queued
// again to continue it, even when the continue is cut short too.
func TestShutdownKeepsABuildsWork(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	e.addIssue(129)
	var cut context.CancelFunc
	shutdown := func(build func(string) error) func(string) error {
		return func(work string) error {
			err := build(work)
			e.p.stopping.Store(true)
			cut()
			return err
		}
	}
	e.claude.builds = []func(string) error{shutdown(halfBuild), shutdown(func(string) error { return nil }), finishBuild}
	e.codex.reviews = []string{pass}
	for range 2 {
		var jctx context.Context
		jctx, cut = context.WithCancel(ctx)
		e.p.Drain(jctx)
		e.p.stopping.Store(false)
	}
	it := e.item(t, 129)
	jobs := implJobs(t, e, it.ID)
	if len(jobs) != 3 {
		t.Fatalf("%d builds", len(jobs))
	}
	first, second := jobs[2], jobs[1]
	for _, j := range []*store.Job{first, second} {
		if j.State != state.Cancelled || !strings.Contains(j.Error, "shut down") || !strings.Contains(j.Error, "kept") {
			t.Fatalf("job %d = %s %q", j.ID, j.State, j.Error)
		}
	}
	if !keptWork(e, first.ID) || e.p.keptTakenBy(first.ID) != 0 {
		t.Fatal("the work wasn't kept for the next start")
	}
	var in ImplementInput
	_ = json.Unmarshal([]byte(jobs[0].Input), &in)
	if jobs[0].State != state.Queued || in.Continue == nil || in.Continue.From != first.ID || jobs[0].Attempt != first.Attempt {
		t.Fatalf("next = %s %+v attempt %d", jobs[0].State, in.Continue, jobs[0].Attempt)
	}
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if st := e.item(t, 129).Stage; st != state.AwaitingHuman {
		t.Fatalf("stage = %s", st)
	}
}
