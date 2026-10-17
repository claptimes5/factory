package pipeline

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/claptimes5/factory/internal/config"
	"github.com/claptimes5/factory/internal/github"
	"github.com/claptimes5/factory/internal/harness"
	"github.com/claptimes5/factory/internal/runner"
	"github.com/claptimes5/factory/internal/state"
	"github.com/claptimes5/factory/internal/store"
)

// --- fakes ---

type fakeGH struct {
	mu       sync.Mutex
	issues   map[int]*github.Issue
	comments map[int][]string
	prs      map[string]*github.PR
	nextPR   int
}

func newFakeGH() *fakeGH {
	return &fakeGH{issues: map[int]*github.Issue{}, comments: map[int][]string{}, prs: map[string]*github.PR{}, nextPR: 100}
}

func (f *fakeGH) ListIssues(_ context.Context, label string) ([]github.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []github.Issue
	for _, is := range f.issues {
		if hasLabel(is.Labels, label) {
			out = append(out, *is)
		}
	}
	return out, nil
}
func (f *fakeGH) GetIssue(_ context.Context, n int) (*github.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	is := *f.issues[n]
	return &is, nil
}
func (f *fakeGH) AddLabels(_ context.Context, n int, ls ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if is, ok := f.issues[n]; ok {
		for _, l := range ls {
			if !hasLabel(is.Labels, l) {
				is.Labels = append(is.Labels, l)
			}
		}
	}
	return nil
}
func (f *fakeGH) RemoveLabel(_ context.Context, n int, l string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if is, ok := f.issues[n]; ok {
		var keep []string
		for _, x := range is.Labels {
			if x != l {
				keep = append(keep, x)
			}
		}
		is.Labels = keep
	}
	return nil
}
func (f *fakeGH) Comment(_ context.Context, n int, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.comments[n] = append(f.comments[n], body)
	return nil
}
func (f *fakeGH) FindPR(_ context.Context, branch string) (*github.PR, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.prs[branch], nil
}
func (f *fakeGH) GetPR(_ context.Context, n int) (*github.PR, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, pr := range f.prs {
		if pr.Number == n {
			c := *pr
			return &c, nil
		}
	}
	return nil, fmt.Errorf("no pr %d", n)
}
func (f *fakeGH) CreatePR(_ context.Context, title, body, head, base string, draft bool) (*github.PR, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextPR++
	pr := &github.PR{Number: f.nextPR, URL: "https://example/pr", State: "open", Draft: draft}
	f.prs[head] = pr
	f.comments[pr.Number] = append(f.comments[pr.Number], "BODY:"+body)
	return pr, nil
}
func (f *fakeGH) UpdatePRBody(_ context.Context, n int, body string) error { return nil }

// fakeHarness plays a scripted role per stage. For implement it edits and
// commits in the workdir like a real builder would.
type fakeHarness struct {
	name    string
	mu      sync.Mutex
	reviews []string // successive review verdicts
	triage  string
	builds  []func(work string) error
	calls   map[string]int
	prompts map[string][]string
}

func (h *fakeHarness) Name() string { return h.name }
func (h *fakeHarness) Run(ctx context.Context, req harness.RunRequest, ev chan<- harness.Event) (*harness.Result, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.calls == nil {
		h.calls, h.prompts = map[string]int{}, map[string][]string{}
	}
	n := h.calls[req.Stage]
	h.calls[req.Stage]++
	h.prompts[req.Stage] = append(h.prompts[req.Stage], req.Prompt)
	res := &harness.Result{CostUSD: 0.1, CostKnown: true, TokensIn: 10, TokensOut: 5, Turns: 1}
	switch req.Stage {
	case "triage":
		res.Final = h.triage
	case "implement":
		if n >= len(h.builds) {
			return nil, fmt.Errorf("no scripted build %d", n)
		}
		if err := h.builds[n](req.Workdir); err != nil {
			return nil, err
		}
		os.WriteFile(filepath.Join(req.OutDir, "verdict.json"), []byte(`{"status":"done","summary":"did it","tests_added":["TestAdd"],"blocker":""}`), 0o644)
	case "review", "critic":
		if n >= len(h.reviews) {
			return nil, fmt.Errorf("no scripted review %d", n)
		}
		res.Final = h.reviews[n]
	}
	return res, nil
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeFile(dir, name, body string) error {
	p := filepath.Join(dir, name)
	os.MkdirAll(filepath.Dir(p), 0o755)
	return os.WriteFile(p, []byte(body), 0o644)
}

func commitIn(dir, msg string) error {
	for _, args := range [][]string{{"add", "-A"}, {"commit", "-qm", msg}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("%v: %s", err, out)
		}
	}
	return nil
}

const testConfig = `
repo: o/r
base_branch: main
runner: {kind: local}
routing:
  triage:      {harness: claude, model: haiku}
  implement.S: {harness: claude, model: sonnet}
  implement.M: {harness: claude, model: sonnet, skills: baseline}
  implement.L: {harness: claude, model: opus}
  review:      {harness: codex,  model: gpt-test}
escalation: {implement: [sonnet, opus], max_attempts_per_tier: 2, max_review_rounds: 2}
budgets: {per_job: {S: 2, M: 6, L: 15}, triage: 0.5, review: 3, daily_total: 60}
pricing: {gpt-test: {input_per_mtok: 1, output_per_mtok: 1}}
gates:
  required: [types, test]
  commands:
    types: test -f go.mod
    test: grep -q "func Add" add.go
`

type env struct {
	p      *Pipeline
	gh     *fakeGH
	claude *fakeHarness
	codex  *fakeHarness
	remote string
}

func setup(t *testing.T) *env {
	t.Helper()
	root := t.TempDir()
	// A bare remote seeded with a tiny Go module.
	seed := filepath.Join(root, "seed")
	os.MkdirAll(seed, 0o755)
	git(t, seed, "init", "-q", "-b", "main")
	writeFile(seed, "go.mod", "module fixture\n\ngo 1.23\n")
	writeFile(seed, "doc.go", "package fixture\n")
	git(t, seed, "add", "-A")
	git(t, seed, "commit", "-qm", "seed")
	remote := filepath.Join(root, "remote.git")
	git(t, root, "clone", "-q", "--bare", seed, remote)

	cfg, err := config.Parse([]byte(testConfig))
	if err != nil {
		t.Fatal(err)
	}
	cfg.DataDir = filepath.Join(root, "data")
	wd, _ := os.Getwd()
	cfg.PromptsDir = filepath.Join(wd, "..", "..", "prompts")
	cfg.SkillsDir = filepath.Join(root, "skills")
	st, err := store.Open(filepath.Join(root, "f.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	gh := newFakeGH()
	cl := &fakeHarness{name: "claude", triage: `{"decision":"ready","complexity":"M","risk":"low","question":"","summary":"add Add"}`}
	cx := &fakeHarness{name: "codex"}
	p := &Pipeline{
		Cfg: cfg, Store: st, GH: gh, Git: &Git{RemoteURL: remote, Author: "factory <f@x>"}, Runner: runner.Local{},
		Harnesses: map[string]harness.Harness{"claude": cl, "codex": cx},
		GateExec: func(ctx context.Context, workdir, command string) (int, string, error) {
			var out strings.Builder
			code, err := runner.Local{}.Run(ctx, runner.Spec{Workdir: workdir, Cmd: []string{"sh", "-c", command}}, &out)
			return code, out.String(), err
		},
	}
	return &env{p: p, gh: gh, claude: cl, codex: cx, remote: remote}
}

func (e *env) addIssue(n int) {
	e.gh.issues[n] = &github.Issue{Number: n, Title: "Add an Add function", Body: "Add func Add(a, b int) int.", Labels: []string{"factory:ready"}}
}

func goodBuild(work string) error {
	if err := writeFile(work, "add.go", "package fixture\n\nfunc Add(a, b int) int { return a + b }\n"); err != nil {
		return err
	}
	writeFile(work, "add_test.go", "package fixture\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif Add(1, 2) != 3 {\n\t\tt.Fatal()\n\t}\n}\n")
	return commitIn(work, "add Add")
}

func badBuild(work string) error {
	writeFile(work, "add.go", "package fixture\n\n// TODO\n")
	return commitIn(work, "nothing useful")
}

const (
	pass  = `{"decision":"pass","findings":[],"challenged_assumptions":[]}`
	block = `{"decision":"block","findings":[{"severity":"major","file":"add.go","line":3,"claim":"overflow unhandled","evidence":"Add(MaxInt,1)"}],"challenged_assumptions":["ints are small"]}`
)

func (e *env) item(t *testing.T, n int) *store.WorkItem {
	it, err := e.p.Store.ItemByIssue(context.Background(), "o/r", n)
	if err != nil {
		t.Fatal(err)
	}
	return it
}

// --- tests ---

// The happy path: a labeled issue becomes a reviewed draft PR with no human action.
func TestIssueBecomesReviewedDraftPR(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	e.addIssue(1)
	e.claude.builds = []func(string) error{goodBuild}
	e.codex.reviews = []string{pass}

	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	it := e.item(t, 1)
	if it.Stage != state.AwaitingHuman || it.PRNumber == 0 || it.Complexity != "M" {
		t.Fatalf("item = %+v", it)
	}
	if got := git(t, e.remote, "log", "--format=%s", "-1", "factory/issue-1"); got != "add Add" {
		t.Fatalf("branch head = %q", got)
	}
	body := e.gh.comments[it.PRNumber][0]
	for _, want := range []string{"Closes #1", "GATES: PASS", "| implement | claude | sonnet | baseline |", "review verdict"} {
		if !strings.Contains(body, want) {
			t.Errorf("PR body missing %q:\n%s", want, body)
		}
	}
	// The reviewer is never shown the builder's summary.
	if strings.Contains(e.codex.prompts["review"][0], "did it") {
		t.Fatal("review prompt leaked the builder's summary")
	}
	if !strings.Contains(e.codex.prompts["review"][0], "+func Add") {
		t.Fatal("review prompt must contain the diff")
	}
	if !hasLabel(e.gh.issues[1].Labels, LabelAwaitingYou) || hasLabel(e.gh.issues[1].Labels, LabelRunning) {
		t.Fatalf("labels = %v", e.gh.issues[1].Labels)
	}
	// The skill set is recorded on the job.
	jobs, _ := e.p.Store.ListJobs(ctx, store.JobFilter{ItemID: it.ID, Kind: "implement"})
	if jobs[0].Skillset != "baseline" {
		t.Fatalf("skillset = %q", jobs[0].Skillset)
	}
	spent, _ := e.p.Store.SpendOn(ctx, store.Today())
	if spent < 0.29 {
		t.Fatalf("spend not recorded: %v", spent)
	}

	// The human merges; the next poll closes the item out.
	e.gh.prs["factory/issue-1"].Merged = true
	e.p.lastPoll = e.p.lastPoll.AddDate(0, 0, -1)
	e.p.Drain(ctx)
	if e.item(t, 1).Stage != state.Done {
		t.Fatalf("stage after merge = %s", e.item(t, 1).Stage)
	}
}

// A gate failure retries the builder with the failure attached.
func TestGateFailureRetriesWithFailure(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	e.addIssue(2)
	e.claude.builds = []func(string) error{badBuild, goodBuild}
	e.codex.reviews = []string{pass}
	e.p.Drain(ctx)
	it := e.item(t, 2)
	if it.Stage != state.AwaitingHuman {
		t.Fatalf("stage = %s", it.Stage)
	}
	if !strings.Contains(e.claude.prompts["implement"][1], "GATES: FAIL") {
		t.Fatal("retry prompt should carry the gate failure")
	}
	jobs, _ := e.p.Store.ListJobs(ctx, store.JobFilter{ItemID: it.ID, Kind: "implement"})
	if len(jobs) != 2 || jobs[1].State != state.Failed || jobs[0].Attempt != 2 {
		t.Fatalf("jobs = %+v", jobs)
	}
	gr, _ := e.p.Store.GateResults(ctx, jobs[1].ID)
	var failed bool
	for _, g := range gr {
		failed = failed || (g.Gate == "test" && g.Status == "fail")
	}
	if !failed {
		t.Fatalf("gate results = %+v", gr)
	}
}

// Failures escalate one model tier, then park.
func TestEscalationThenPark(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	e.addIssue(3)
	e.claude.builds = []func(string) error{badBuild, badBuild, badBuild, badBuild}
	e.p.Drain(ctx)
	it := e.item(t, 3)
	if it.Stage != state.Parked {
		t.Fatalf("stage = %s", it.Stage)
	}
	jobs, _ := e.p.Store.ListJobs(ctx, store.JobFilter{ItemID: it.ID, Kind: "implement"})
	var models []string
	for i := len(jobs) - 1; i >= 0; i-- {
		models = append(models, jobs[i].Model)
	}
	if strings.Join(models, ",") != "sonnet,sonnet,opus,opus" {
		t.Fatalf("models = %v", models)
	}
	if !hasLabel(e.gh.issues[3].Labels, LabelParked) {
		t.Fatal("parked issue should be labeled")
	}
}

// A reviewer block sends findings back; a third block parks.
func TestReviewBlockRoundsThenPark(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	e.addIssue(4)
	round := 0
	fix := func(w string) error {
		round++
		writeFile(w, "add.go", fmt.Sprintf("package fixture\n\n// Add adds (round %d).\nfunc Add(a, b int) int { return a + b }\n", round))
		return commitIn(w, "fix")
	}
	e.claude.builds = []func(string) error{goodBuild, fix, fix}
	e.codex.reviews = []string{block, block, block}
	e.p.Drain(ctx)
	it := e.item(t, 4)
	if it.Stage != state.Parked {
		t.Fatalf("stage = %s", it.Stage)
	}
	if !strings.Contains(e.claude.prompts["implement"][1], "overflow unhandled") {
		t.Fatal("fix round should carry the reviewer findings")
	}
	if e.claude.calls["implement"] != 3 || e.codex.calls["review"] != 3 {
		t.Fatalf("calls = %v %v", e.claude.calls, e.codex.calls)
	}
	// Fix rounds build on the branch, not from scratch.
	if got := git(t, e.remote, "log", "--format=%s", "factory/issue-4"); !strings.Contains(got, "add Add") || !strings.Contains(got, "fix") {
		t.Fatalf("branch history = %q", got)
	}
}

// An unparseable review never passes.
func TestMalformedReviewFailsClosed(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	e.addIssue(5)
	e.claude.builds = []func(string) error{goodBuild}
	e.codex.reviews = []string{"LGTM!", `{"decision":"approve"}`}
	e.p.Drain(ctx)
	it := e.item(t, 5)
	if it.Stage != state.Parked || it.PRNumber != 0 {
		t.Fatalf("item = %+v", it)
	}
}

// The builder may not edit protected paths, whatever the gates say.
func TestDiffGuardBlocksProtectedEdit(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	e.p.Cfg.Gates.DiffGuard.ProtectedPaths = []string{".github/"}
	e.addIssue(6)
	sneaky := func(w string) error {
		goodBuild(w)
		writeFile(w, ".github/workflows/x.yml", "on: push\n")
		return commitIn(w, "sneaky")
	}
	e.claude.builds = []func(string) error{sneaky, goodBuild}
	e.codex.reviews = []string{pass}
	e.p.Drain(ctx)
	jobs, _ := e.p.Store.ListJobs(ctx, store.JobFilter{ItemID: e.item(t, 6).ID, Kind: "implement"})
	last := jobs[len(jobs)-1]
	if last.State != state.Failed || !strings.Contains(last.Error, "protected path") {
		t.Fatalf("first attempt = %s %q", last.State, last.Error)
	}
}

func TestTriageParkAndUnpark(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	e.addIssue(7)
	e.claude.triage = `{"decision":"park","complexity":"S","risk":"low","question":"Which package?","summary":""}`
	e.p.Drain(ctx)
	if e.item(t, 7).Stage != state.Parked || !strings.Contains(strings.Join(e.gh.comments[7], "\n"), "Which package?") {
		t.Fatalf("not parked with question: %v", e.gh.comments[7])
	}
	// Human answers and removes the label: back through triage.
	e.gh.RemoveLabel(ctx, 7, LabelParked)
	e.claude.triage = `{"decision":"ready","complexity":"S","risk":"low","question":"","summary":"ok"}`
	e.claude.builds = []func(string) error{goodBuild}
	e.codex.reviews = []string{pass}
	e.p.lastPoll = e.p.lastPoll.AddDate(0, 0, -1)
	e.p.Drain(ctx)
	if e.item(t, 7).Stage != state.AwaitingHuman {
		t.Fatalf("stage = %s", e.item(t, 7).Stage)
	}
}

func TestDailyBudgetStopsDequeuing(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	e.p.Cfg.Budgets.DailyTotal = 0.05 // the first job's $0.10 crosses it
	var notes []string
	e.p.Notify = func(_ context.Context, m string) { notes = append(notes, m) }
	e.addIssue(8)
	e.claude.builds = []func(string) error{goodBuild}
	e.p.Drain(ctx)
	if n, _ := e.p.Store.CountJobs(ctx, state.Queued, "implement"); n != 1 {
		t.Fatalf("implement should wait in the queue, queued=%d", n)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "Daily budget") {
		t.Fatalf("notes = %v", notes)
	}
}

func TestRecoverRequeuesRunning(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	it, _, _ := e.p.Store.UpsertItem(ctx, "o/r", 9, "x")
	e.p.enqueueTriage(ctx, it, 1, "")
	e.p.Store.ClaimJob(ctx)
	if err := e.p.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if n, _ := e.p.Store.CountJobs(ctx, state.Queued, ""); n != 1 {
		t.Fatalf("queued = %d", n)
	}
}
