package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/claptimes5/factory/internal/harness"
	"github.com/claptimes5/factory/internal/state"
	"github.com/claptimes5/factory/internal/store"
)

// tuneSetup is setup with factory.yaml on disk, one item built and merged,
// and notifications recorded.
func tuneSetup(t *testing.T, files map[string]string) (*env, *[]string) {
	t.Helper()
	ctx := context.Background()
	e := setupWith(t, files)
	e.p.ConfigPath = filepath.Join(t.TempDir(), "factory.yaml")
	if err := os.WriteFile(e.p.ConfigPath, []byte(testConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var msgs []string
	e.p.Notify = func(_ context.Context, m string) { mu.Lock(); msgs = append(msgs, m); mu.Unlock() }
	e.addIssue(1)
	e.claude.builds = []func(string) error{goodBuild}
	e.codex.reviews = []string{pass}
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	e.p.Cfg.Tuning.MinItems = 1
	return e, &msgs
}

func tuneJobs(t *testing.T, e *env) []*store.Job {
	t.Helper()
	jobs, err := e.p.Store.ListJobs(context.Background(), store.JobFilter{Kind: "tune"})
	if err != nil {
		t.Fatal(err)
	}
	return jobs
}

func markAgo(t *testing.T, e *env, name string, d time.Duration) {
	t.Helper()
	if err := e.p.Store.Mark(context.Background(), name, time.Now().UTC().Add(-d).Format(store.TimeFormat)); err != nil {
		t.Fatal(err)
	}
}

func TestTuneTrigger(t *testing.T) {
	ctx := context.Background()
	e, msgs := tuneSetup(t, nil)
	now := time.Now()

	// The first look only starts the clock.
	e.p.tuneTrigger(ctx, now)
	if n := len(tuneJobs(t, e)); n != 0 {
		t.Fatalf("first sight queued %d tune jobs", n)
	}
	if at, _ := e.p.Store.Marked(ctx, markTuneDue); at == "" {
		t.Fatal("the cadence clock wasn't started")
	}

	// Not due yet: nothing.
	e.p.tuneDue = time.Time{}
	markAgo(t, e, markTuneDue, time.Hour)
	e.p.tuneTrigger(ctx, now)
	if n := len(tuneJobs(t, e)); n != 0 {
		t.Fatalf("not due, but queued %d", n)
	}

	// Due, but fewer builds than min_items: skipped, and you're told.
	e.p.tuneDue = time.Time{}
	e.p.Cfg.Tuning.MinItems = 5
	markAgo(t, e, markTuneDue, 31*24*time.Hour)
	e.p.tuneTrigger(ctx, now)
	if n := len(tuneJobs(t, e)); n != 0 {
		t.Fatalf("below min_items, but queued %d", n)
	}
	if len(*msgs) == 0 || !strings.Contains((*msgs)[len(*msgs)-1], "Tuning skipped: 1 item was built") {
		t.Fatalf("notifications = %q", *msgs)
	}
	// The skip counts as the cadence firing: no retry every tick.
	e.p.Cfg.Tuning.MinItems = 1
	e.p.tuneTrigger(ctx, now.Add(time.Minute))
	if n := len(tuneJobs(t, e)); n != 0 {
		t.Fatalf("queued %d right after a skip", n)
	}

	// Due with enough builds: exactly one job, however often it's asked.
	e.p.tuneDue = time.Time{}
	markAgo(t, e, markTuneDue, 31*24*time.Hour)
	e.p.tuneTrigger(ctx, now)
	e.p.tuneTrigger(ctx, now.Add(time.Second))
	jobs := tuneJobs(t, e)
	if len(jobs) != 1 || jobs[0].Route != "tune" || jobs[0].Model != "opus" || jobs[0].BudgetUSD != e.p.Cfg.Orchestrator.RunBudget {
		t.Fatalf("tune jobs = %+v", jobs)
	}
	if _, err := e.p.Tune(ctx, "factory tune", true); err != ErrTuneQueued {
		t.Fatalf("a second run while one is queued: %v", err)
	}

	// Off: never.
	e.p.Cfg.Tuning.Every = 0
	e.p.tuneDue = time.Time{}
	markAgo(t, e, markTuneDue, 365*24*time.Hour)
	e.p.tuneTrigger(ctx, now)
	if n := len(tuneJobs(t, e)); n != 1 {
		t.Fatalf("tuning off, but %d jobs", n)
	}
}

const tuneAnswer = `{"summary":"Builds went well; the daily cap binds.","changes":[
	{"path":["budgets","daily_total"],"value":75,"why":"spend hit the cap on 3 of 30 days"},
	{"path":["gates","commands","test"],"value":"true","why":"tests are slow"},
	{"path":["runner","kind"],"value":"local","why":"faster"},
	{"path":["budgets","concurrent_jobs"],"value":0,"why":"save money"},
	{"path":["routing","review","model"],"value":"sonnet","why":"cheaper"}],
	"ideas":["Tell the builder to run go vet first: 2 parks were vet failures."]}`

func TestTuneProposal(t *testing.T) {
	ctx := context.Background()
	e, msgs := tuneSetup(t, nil)
	var req harness.RunRequest
	e.claude.tune = func(r harness.RunRequest) (string, error) { req = r; return tuneAnswer, nil }
	if _, err := e.p.Tune(ctx, "factory tune", true); err != nil {
		t.Fatal(err)
	}
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if jobs := tuneJobs(t, e); len(jobs) != 1 || jobs[0].State != state.Succeeded {
		t.Fatalf("tune jobs = %+v", jobs)
	}
	// Read-only, with no tools: it reads only what the prompt gives it.
	if !req.ReadOnly || len(req.Tools) != 0 || req.MCPConfig != "" {
		t.Fatalf("tune run = %+v", req)
	}
	for _, want := range []string{`"first_pass_rate"`, `"daily_total"`, `"park_reasons"`, "factory eval", "Prefer no change when the data is thin"} {
		if !strings.Contains(req.Prompt, want) {
			t.Errorf("prompt lacks %q", want)
		}
	}
	if strings.Contains(req.Prompt, `"tuning"`) {
		t.Error("the tuning loop's own settings were offered to it")
	}

	p, err := e.p.Store.PendingProposal(ctx)
	if err != nil || p == nil {
		t.Fatalf("pending = %v, %v", p, err)
	}
	if len(p.Changes) != 1 || strings.Join(p.Changes[0].Path, ".") != "budgets.daily_total" || p.Changes[0].Why == "" {
		t.Fatalf("kept = %+v", p.Changes)
	}
	why := map[string]string{}
	for _, d := range p.Dropped {
		why[strings.Join(d.Path, ".")] = d.Reason
	}
	for path, want := range map[string]string{
		"gates.commands.test":     "isn't a setting tuning may change",
		"runner.kind":             "isn't a setting tuning may change",
		"budgets.concurrent_jobs": "at least 1",
		"routing.review.model":    "would not load",
	} {
		if !strings.Contains(why[path], want) {
			t.Errorf("%s dropped for %q, want %q", path, why[path], want)
		}
	}
	if len(p.Ideas) != 1 || p.PRURL != "" {
		t.Fatalf("proposal = %+v", p)
	}
	if !strings.Contains(p.Diff, "--- a/factory.yaml") || !strings.Contains(p.Diff, "\n+budgets: {per_job: {XS: 0.5, S: 2, M: 6, L: 15}, triage: 0.5, review: 3, daily_total: 75}\n") ||
		!strings.Contains(p.Diff, "\n-budgets:") || strings.Count(p.Diff, "\n+") != 2 { // the header and the one line
		t.Fatalf("diff:\n%s", p.Diff)
	}
	// The daemon never writes factory.yaml itself.
	if b, _ := os.ReadFile(e.p.ConfigPath); string(b) != testConfig {
		t.Fatal("factory.yaml changed")
	}
	last := (*msgs)[len(*msgs)-1]
	if !strings.Contains(last, "Builds went well") || !strings.Contains(last, "budgets.daily_total: 75") || !strings.Contains(last, "/app/settings") {
		t.Fatalf("notification = %q", last)
	}

	// The next proposal supersedes it.
	if _, err := e.p.Tune(ctx, "again", true); err != nil {
		t.Fatal(err)
	}
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if old, _ := e.p.Store.GetProposal(ctx, p.ID); old.Status != store.ProposalSuperseded {
		t.Fatalf("old proposal is %s", old.Status)
	}
	if next, _ := e.p.Store.PendingProposal(ctx); next == nil || next.ID == p.ID {
		t.Fatalf("pending = %+v", next)
	}
}

func TestTuneOpensADraftPR(t *testing.T) {
	ctx := context.Background()
	e, _ := tuneSetup(t, map[string]string{"deploy/factory.yaml": testConfig})
	e.p.Cfg.Tuning.Repo, e.p.Cfg.Tuning.Path = "o/r", "deploy/factory.yaml"
	e.claude.tune = func(harness.RunRequest) (string, error) { return tuneAnswer, nil }
	if _, err := e.p.Tune(ctx, "factory tune", true); err != nil {
		t.Fatal(err)
	}
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	branch := "factory/tuning-" + time.Now().UTC().Format("2006-01-02")
	pr := e.gh.prs[branch]
	if pr == nil || !pr.Draft || pr.Merged {
		t.Fatalf("prs = %v", e.gh.prs)
	}
	if !hasLabel(e.gh.prLabels[pr.Number], LabelAwaitingYou) {
		t.Fatalf("labels = %v", e.gh.prLabels[pr.Number])
	}
	body := e.gh.bodies[pr.Number]
	for _, want := range []string{"Builds went well", "`budgets.daily_total`: `75`. spend hit the cap", "run go vet first", "never merges"} {
		if !strings.Contains(body, want) {
			t.Errorf("body lacks %q:\n%s", want, body)
		}
	}
	got := git(t, e.remote, "show", branch+":deploy/factory.yaml")
	if !strings.Contains(got, "daily_total: 75}") || !strings.Contains(got, "runner: {kind: local}") {
		t.Fatalf("file on the branch:\n%s", got)
	}
	if p, _ := e.p.Store.PendingProposal(ctx); p == nil || p.PRURL != pr.URL {
		t.Fatalf("proposal = %+v", p)
	}
}
