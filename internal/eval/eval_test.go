package eval

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/claptimes5/factory/internal/config"
	"github.com/claptimes5/factory/internal/harness"
	"github.com/claptimes5/factory/internal/runner"
)

// fakeAgent implements whatever the issue asks, with a deliberate bug in Sub.
type fakeAgent struct{ name string }

func (f *fakeAgent) Name() string { return f.name }
func (f *fakeAgent) Run(_ context.Context, req harness.RunRequest, _ chan<- harness.Event) (*harness.Result, error) {
	res := &harness.Result{CostUSD: 0.25, CostKnown: true}
	switch req.Stage {
	case "triage":
		res.Final = `{"decision":"ready","complexity":"S","risk":"low","question":"","summary":"s"}`
	case "review":
		res.Final = `{"decision":"pass","findings":[],"challenged_assumptions":[]}`
	case "implement":
		code, test := "func Add(a, b int) int { return a + b }\n", "func TestAdd(t *testing.T) {\n\tif Add(1, 2) != 3 {\n\t\tt.Fatal()\n\t}\n}\n"
		if strings.Contains(req.Prompt, "Sub function") {
			code, test = "func Sub(a, b int) int { return a + b }\n", "func TestSub(t *testing.T) {\n\t_ = Sub(1, 2)\n}\n"
		}
		write(req.Workdir, "f.go", "package fixture\n\n"+code)
		write(req.Workdir, "f_test.go", "package fixture\n\nimport \"testing\"\n\n"+test)
		for _, args := range [][]string{{"add", "-A"}, {"commit", "-qm", "work"}} {
			cmd := exec.Command("git", args...)
			cmd.Dir = req.Workdir
			if out, err := cmd.CombinedOutput(); err != nil {
				return nil, err
			} else {
				_ = out
			}
		}
		write(req.OutDir, "verdict.json", `{"status":"done","summary":"done","tests_added":["T"],"blocker":""}`)
	}
	return res, nil
}

func write(dir, name, body string) {
	os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755)
	os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644)
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

const cfgYAML = `
repo: o/r
base_branch: main
runner: {kind: local}
routing:
  triage:      {harness: claude, model: haiku}
  implement.S: {harness: claude, model: sonnet}
  implement.M: {harness: claude, model: sonnet}
  implement.L: {harness: claude, model: opus}
  review:      {harness: codex,  model: gpt-test}
escalation: {implement: [sonnet], max_attempts_per_tier: 1, max_review_rounds: 2}
pricing: {gpt-test: {input_per_mtok: 1, output_per_mtok: 1}}
budgets: {per_job: {S: 2, M: 6, L: 15}, triage: 0.5, review: 3, daily_total: 60}
gates:
  required: [test]
  commands: {test: go vet ./...}
`

// A replay set of two cases: one the factory solves, one where its code
// passes its own tests but fails the hidden acceptance test.
func TestReplaySetReportsCostPerPass(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	src := filepath.Join(root, "src")
	os.MkdirAll(src, 0o755)
	git(t, src, "init", "-q", "-b", "main")
	write(src, "go.mod", "module fixture\n\ngo 1.23\n")
	write(src, "doc.go", "package fixture\n")
	git(t, src, "add", "-A")
	git(t, src, "commit", "-qm", "base")
	base := git(t, src, "rev-parse", "HEAD")
	write(src, "later.go", "package fixture\n\n// THE-KNOWN-GOOD-FIX\n")
	git(t, src, "add", "-A")
	git(t, src, "commit", "-qm", "later")

	set := filepath.Join(root, "evals")
	for name, c := range map[string]string{
		"add": "title: Add an Add function\nbody: func Add(a, b int) int\naccept: go test -count=1 -run TestAccept ./...\n",
		"sub": "title: Add a Sub function\nbody: func Sub(a, b int) int returns a-b\naccept: go test -count=1 -run TestAccept ./...\n",
	} {
		write(set, name+"/case.yaml", c+"repo: "+src+"\nbase_sha: "+base+"\n")
	}
	write(set, "add/accept/accept_test.go", "package fixture\n\nimport \"testing\"\n\nfunc TestAccept(t *testing.T) {\n\tif Add(2, 2) != 4 {\n\t\tt.Fatal()\n\t}\n}\n")
	write(set, "sub/accept/accept_test.go", "package fixture\n\nimport \"testing\"\n\nfunc TestAccept(t *testing.T) {\n\tif Sub(5, 2) != 3 {\n\t\tt.Fatal(\"wrong\")\n\t}\n}\n")

	cases, err := Load(set)
	if err != nil || len(cases) != 2 {
		t.Fatalf("cases %v %v", cases, err)
	}
	cfg, err := config.Parse([]byte(cfgYAML))
	if err != nil {
		t.Fatal(err)
	}
	wd, _ := os.Getwd()
	cfg.PromptsDir = filepath.Join(wd, "..", "..", "prompts")
	rep, err := Run(ctx, Options{
		Cfg: cfg, Runner: runner.Local{}, WorkDir: filepath.Join(root, "work"), Parallel: 2,
		Harnesses: map[string]harness.Harness{"claude": &fakeAgent{"claude"}, "codex": &fakeAgent{"codex"}},
		GateExec: func(ctx context.Context, workdir, command string) (int, string, error) {
			var out strings.Builder
			code, err := runner.Local{}.Run(ctx, runner.Spec{Workdir: workdir, Cmd: []string{"sh", "-c", command}}, &out)
			return code, out.String(), err
		},
	}, cases)
	if err != nil {
		t.Fatal(err)
	}
	// The builder's copy must not hold anything after the base.
	frozen := filepath.Join(root, "work", "00-add", "remote.git")
	if out := git(t, root, "--git-dir="+frozen, "log", "--all", "--format=%s"); strings.Contains(out, "later") {
		t.Fatalf("frozen remote still has later commits: %s", out)
	}
	if out, _ := exec.Command("git", "--git-dir="+frozen, "cat-file", "--batch-all-objects", "--batch-check").Output(); len(out) > 0 {
		objs := strings.Fields(string(out))
		for i := 0; i+2 < len(objs); i += 3 {
			body, _ := exec.Command("git", "--git-dir="+frozen, "cat-file", "-p", objs[i]).Output()
			if strings.Contains(string(body), "THE-KNOWN-GOOD-FIX") {
				t.Fatal("the fix's content is still in the frozen remote")
			}
		}
	}
	if rep.Cases != 2 || rep.Passed != 1 || rep.PassRate != 0.5 {
		t.Fatalf("report:\n%s", rep.Table())
	}
	if !rep.Results[0].Passed || rep.Results[1].Passed || !strings.Contains(rep.Results[1].Reason, "acceptance failed") {
		t.Fatalf("results:\n%s", rep.Table())
	}
	// triage + implement + review per case, at $0.25 each.
	if rep.CostUSD != 1.5 || rep.CostPerPass != 1.5 {
		t.Fatalf("cost %.2f per pass %.2f", rep.CostUSD, rep.CostPerPass)
	}
	if rep.Results[0].Rounds != 1 || rep.MeanRounds != 1 || !strings.Contains(rep.Table(), "review rounds 1.0") {
		t.Fatalf("review rounds:\n%s", rep.Table())
	}
	if !strings.Contains(rep.Table(), "cost per pass $1.50") {
		t.Fatal(rep.Table())
	}
}

func TestCompareRejectsWorseCandidates(t *testing.T) {
	base := &Report{PassRate: 0.8, Passed: 8, CostPerPass: 2}
	if why := Compare(&Report{PassRate: 0.8, Passed: 8, CostPerPass: 1.5}, base); len(why) != 0 {
		t.Fatalf("a cheaper candidate is fine: %v", why)
	}
	if why := Compare(&Report{PassRate: 0.7, Passed: 7, CostPerPass: 1.5}, base); len(why) != 1 || !strings.Contains(why[0], "pass rate") {
		t.Fatalf("lower pass rate: %v", why)
	}
	if why := Compare(&Report{PassRate: 0.9, Passed: 9, CostPerPass: 2.5}, base); len(why) != 1 || !strings.Contains(why[0], "cost per pass") {
		t.Fatalf("higher cost: %v", why)
	}
}
