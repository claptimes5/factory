package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/claptimes5/factory/internal/config"
)

// fakeRubocop stands in for RuboCop: its baseline writes the two files
// (and install leaves a bundle behind), and lint passes once the todo
// file is there, unless stillFails.
func fakeRubocop(stillFails bool) func(ctx context.Context, dir, cmd string) (int, string, error) {
	return func(_ context.Context, dir, cmd string) (int, string, error) {
		switch {
		case cmd == "bundle install":
			os.MkdirAll(filepath.Join(dir, "vendor", "bundle"), 0o755)
			return 0, "", os.WriteFile(filepath.Join(dir, "vendor", "bundle", "gem.rb"), []byte("x"), 0o644)
		case strings.Contains(cmd, "--auto-gen-config"):
			os.WriteFile(filepath.Join(dir, ".rubocop.yml"), []byte("inherit_from: .rubocop_todo.yml\n"), 0o644)
			return 1, "2504 offenses", os.WriteFile(filepath.Join(dir, ".rubocop_todo.yml"), []byte("Style/Alias:\n  Exclude:\n    - 'test/test_helper.rb'\n"), 0o644)
		case cmd == "bundle exec rubocop":
			if _, err := os.Stat(filepath.Join(dir, ".rubocop_todo.yml")); err != nil || stillFails {
				return 1, "test/test_helper.rb:267:5: C: Style/Alias", nil
			}
			return 0, "no offenses", nil
		}
		return 0, "", nil
	}
}

// The baseline is pushed to its own branch with only the linter's files,
// and a pull request opened for it.
func TestLintBaseline(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	e.p.Cfg.Repos = []config.Repo{{Name: "o/r", Gates: &config.Gates{Install: "bundle install", Required: []string{"lint"}, Commands: map[string]string{"lint": "bundle exec rubocop"}}}}
	e.p.GateExec = fakeRubocop(false)
	if !e.p.CanBaseline("o/r") {
		t.Fatal("RuboCop can't be baselined")
	}
	url, err := e.p.LintBaseline(ctx, "o/r")
	if err != nil || url == "" {
		t.Fatalf("baseline: %q %v", url, err)
	}
	files := git(t, e.remote, "diff", "--name-only", "main", BaselineBranch)
	if strings.Join(strings.Fields(files), ",") != ".rubocop.yml,.rubocop_todo.yml" {
		t.Fatalf("changed files = %q", files)
	}
	pr := e.gh.prs[BaselineBranch]
	if pr == nil || !strings.Contains(pr.Title, "RuboCop offenses as a baseline") || !strings.Contains(pr.Body, "--auto-gen-config --no-exclude-limit") {
		t.Fatalf("pr = %+v", pr)
	}
	// Again, before it's merged: the same branch is pushed and its pull
	// request returned, not a second one.
	if again, err := e.p.LintBaseline(ctx, "o/r"); err != nil || again != url || len(e.gh.prs) != 1 {
		t.Fatalf("again: %q %v, %d pull requests", again, err, len(e.gh.prs))
	}
}

// When lint still fails with the baseline, nothing is pushed.
func TestLintBaselineThatDoesntPass(t *testing.T) {
	t.Parallel()
	e := setup(t)
	e.p.Cfg.Repos = []config.Repo{{Name: "o/r", Gates: &config.Gates{Install: "bundle install", Required: []string{"lint"}, Commands: map[string]string{"lint": "bundle exec rubocop"}}}}
	e.p.GateExec = fakeRubocop(true)
	if _, err := e.p.LintBaseline(context.Background(), "o/r"); err == nil || !strings.Contains(err.Error(), "still fails") {
		t.Fatalf("err = %v", err)
	}
	if out := git(t, e.remote, "branch", "--list", BaselineBranch); strings.TrimSpace(out) != "" {
		t.Fatalf("pushed %s", out)
	}
	e.p.Cfg.Repos[0].Gates.Commands["lint"] = "npm run lint"
	if e.p.CanBaseline("o/r") {
		t.Fatal("ESLint can't be baselined yet")
	}
}
