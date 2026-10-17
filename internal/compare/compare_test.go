package compare

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/claptimes5/factory/internal/config"
	"github.com/claptimes5/factory/internal/github"
	"github.com/claptimes5/factory/internal/harness"
	"github.com/claptimes5/factory/internal/pipeline"
	"github.com/claptimes5/factory/internal/runner"
)

// fakeAgent builds differently depending on the skill set it was given:
// with "tdd" it writes a test, without skills it forgets one.
type fakeAgent struct{ name string }

func (f *fakeAgent) Name() string { return f.name }
func (f *fakeAgent) Run(_ context.Context, req harness.RunRequest, _ chan<- harness.Event) (*harness.Result, error) {
	res := &harness.Result{CostUSD: 0.5, CostKnown: true}
	switch req.Stage {
	case "review":
		res.Final = `{"decision":"pass","findings":[],"challenged_assumptions":[]}`
	case "implement":
		write(req.Workdir, "add.go", "package fixture\n\nfunc Add(a, b int) int { return a + b }\n")
		if filepath.Base(req.SkillsDir) == "tdd" {
			write(req.Workdir, "add_test.go", "package fixture\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif Add(1, 2) != 3 {\n\t\tt.Fatal()\n\t}\n}\n")
		}
		run(req.Workdir, "add", "-A")
		run(req.Workdir, "commit", "-qm", "work")
		write(req.OutDir, "verdict.json", `{"status":"done","summary":"done","tests_added":[],"blocker":""}`)
	}
	return res, nil
}

func write(dir, name, body string) {
	os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755)
	os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644)
}

func run(dir string, args ...string) string {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, _ := cmd.CombinedOutput()
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
  required: [vet]
  commands: {vet: go vet ./...}
  proves_fix: go test -count=1 {pkgs}
`

func TestCompareSkillSets(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	seed := filepath.Join(root, "seed")
	os.MkdirAll(seed, 0o755)
	run(seed, "init", "-q", "-b", "main")
	write(seed, "go.mod", "module fixture\n\ngo 1.23\n")
	write(seed, "doc.go", "package fixture\n")
	run(seed, "add", "-A")
	run(seed, "commit", "-qm", "base")
	remote := filepath.Join(root, "remote.git")
	run(root, "clone", "-q", "--bare", seed, remote)

	cfg, err := config.Parse([]byte(cfgYAML))
	if err != nil {
		t.Fatal(err)
	}
	wd, _ := os.Getwd()
	cfg.PromptsDir = filepath.Join(wd, "..", "..", "prompts")
	cfg.SkillsDir = filepath.Join(root, "skills")
	write(cfg.SkillsDir, "tdd/SKILL.md", "Write the test first.\n")
	gh := github.NewMemory()
	gh.AddIssue(5, "Add an Add function", "func Add(a, b int) int")

	if _, err := Run(ctx, Options{Cfg: cfg, GH: gh}, 5, []string{"tdd", "missing"}, "S", "low"); err == nil {
		t.Fatal("an unknown skill set must be refused")
	}
	dir := filepath.Join(root, "cmp")
	rep, err := Run(ctx, Options{
		Cfg: cfg, GH: gh, Runner: runner.Local{}, Dir: dir, Push: true,
		Git:       &pipeline.Git{RemoteURL: remote, Author: "factory <f@x>"},
		Harnesses: map[string]harness.Harness{"claude": &fakeAgent{"claude"}, "codex": &fakeAgent{"codex"}},
		GateExec: func(ctx context.Context, workdir, command string) (int, string, error) {
			var out strings.Builder
			code, err := runner.Local{}.Run(ctx, runner.Spec{Workdir: workdir, Cmd: []string{"sh", "-c", command}}, &out)
			return code, out.String(), err
		},
	}, 5, []string{"tdd", None}, "S", "low")
	if err != nil {
		t.Fatal(err)
	}
	tdd, none := rep.Arms[0], rep.Arms[1]
	if tdd.Stage != "awaiting_human" || tdd.Review != "pass" || !strings.Contains(tdd.DiffStat, "add_test.go") {
		t.Fatalf("tdd arm = %+v", tdd)
	}
	// Without the skill the change has no test, so proves_fix fails every attempt.
	if none.Stage != "parked" || none.Head != "" {
		t.Fatalf("none arm = %+v", none)
	}
	var provesFix string
	for _, g := range none.Gates {
		if g.Gate == "proves_fix" {
			provesFix = g.Status
		}
	}
	if provesFix != "fail" {
		t.Fatalf("none gates = %+v", none.Gates)
	}
	if got := run(remote, "branch", "--list", "factory/compare-*"); !strings.Contains(got, "factory/compare-5-tdd") {
		t.Fatalf("pushed branches = %q", got)
	}
	if run(remote, "rev-parse", "main") != rep.BaseSHA {
		t.Fatal("the base must not move")
	}
	if err := rep.Save(dir); err != nil {
		t.Fatal(err)
	}
	md, _ := os.ReadFile(filepath.Join(dir, "report.md"))
	for _, want := range []string{"|  | **tdd** | **none** |", "| Outcome | passed review | parked |", "proves_fix=fail", "<summary>Diff</summary>"} {
		if !strings.Contains(string(md), want) {
			t.Errorf("report lacks %q:\n%s", want, md)
		}
	}
}
