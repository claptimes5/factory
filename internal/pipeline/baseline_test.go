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
		case cmd == "bundle exec rubocop --format json":
			return 1, `{"files":[{"path":"app/a.rb","offenses":[{"cop_name":"Lint/RedundantCopDisableDirective"}]},{"path":"app/b.rb","offenses":[]}]}`, nil
		case cmd == "bundle exec rubocop":
			// As on a real repo: the generated Excludes drop the cops' own
			// until inherit_mode merges them, and a disable comment the
			// baseline made redundant is left over.
			cfg, _ := os.ReadFile(filepath.Join(dir, ".rubocop.yml"))
			todo, _ := os.ReadFile(filepath.Join(dir, ".rubocop_todo.yml"))
			switch {
			case stillFails || len(todo) == 0:
				return 1, "test/test_helper.rb:267:5: C: Style/Alias", nil
			case !strings.Contains(string(cfg), "inherit_mode:"):
				return 1, "test/x_test.rb:3:1: C: Style/Documentation", nil
			case !strings.Contains(string(todo), "'app/a.rb'"):
				return 1, "app/a.rb:1:1: W: Lint/RedundantCopDisableDirective", nil
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
	todo := git(t, e.remote, "show", BaselineBranch+":.rubocop_todo.yml")
	if !strings.Contains(todo, "Lint/RedundantCopDisableDirective:\n  Exclude:\n    - 'app/a.rb'") || !strings.Contains(todo, "Style/Alias:") {
		t.Fatalf("todo = %s", todo)
	}
	if cfg := git(t, e.remote, "show", BaselineBranch+":.rubocop.yml"); !strings.Contains(cfg, "inherit_mode:\n  merge:\n    - Exclude") || !strings.Contains(cfg, "inherit_from: .rubocop_todo.yml") {
		t.Fatalf(".rubocop.yml = %s", cfg)
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

// Files are added to a cop's Exclude, keeping its comments and entries; a
// cop the file turns off stays off.
func TestAddExcludes(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".rubocop_todo.yml")
	os.WriteFile(path, []byte("# Generated.\n\n# This cop supports safe autocorrection.\nStyle/Alias:\n  Exclude:\n    - 'a.rb'\n\nStyle/Documentation:\n  Enabled: false\n"), 0o644)
	if err := addExcludes(path, map[string][]string{"Style/Alias": {"a.rb", "b.rb"}, "Style/Documentation": {"c.rb"}, "Lint/X": {"d.rb"}}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	want := "# Generated.\n\n# This cop supports safe autocorrection.\nStyle/Alias:\n  Exclude:\n    - 'a.rb'\n    - 'b.rb'\n\nStyle/Documentation:\n  Enabled: false\n\nLint/X:\n  Exclude:\n    - 'd.rb'\n"
	if string(b) != want {
		t.Fatalf("got:\n%s\nwant:\n%s", b, want)
	}
}
