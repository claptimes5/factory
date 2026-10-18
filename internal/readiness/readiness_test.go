package readiness

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/claptimes5/factory/internal/config"
	"github.com/claptimes5/factory/internal/runner"
)

func tree(t *testing.T, files ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, f := range files {
		p := filepath.Join(dir, f)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte("x"), 0o644)
	}
	return dir
}

func repo(install string, required ...string) config.Repo {
	cmds := map[string]string{}
	for _, g := range required {
		cmds[g] = "run " + g
	}
	return config.Repo{Name: "o/api", BaseBranch: "main", Gates: &config.Gates{Install: install, Required: required, Commands: cmds}}
}

// fails makes an Exec where the listed commands fail; "flaky:cmd" fails
// only the rerun.
func fails(cmds ...string) (Exec, *[]string) {
	var ran []string
	seen := map[string]int{}
	return func(_ context.Context, _ string, cmd string, _ bool) (int, string, error) {
		ran = append(ran, cmd)
		seen[cmd]++
		for _, c := range cmds {
			if c == cmd || (c == "flaky:"+strings.TrimPrefix(cmd, again) && seen[cmd] == 1 && strings.HasPrefix(cmd, again)) {
				return 1, "boom: " + cmd, nil
			}
		}
		return 0, "", nil
	}, &ran
}

const again = `export GOFLAGS="${GOFLAGS:+$GOFLAGS }-count=1"; `

func titles(r *Report, l Level) string {
	var out []string
	for _, f := range r.Findings {
		if f.Level == l {
			out = append(out, f.Title)
		}
	}
	return strings.Join(out, "; ")
}

func TestReadyRepo(t *testing.T) {
	dir := tree(t, "Gemfile", "Gemfile.lock", "spec/models/user_spec.rb", "spec/system/login_spec.rb", "features.yaml", ".github/workflows/ci.yml")
	exec, ran := fails()
	r := Assess(context.Background(), Input{Repo: repo("bundle install", "lint", "test", "build"), SHA: "abc1234567", Dir: dir, Exec: exec})
	if !r.Ready || r.Score != 100 || titles(r, Blocker) != "" || titles(r, Gap) != "" {
		t.Fatalf("report:\n%s", r.Text())
	}
	if got := strings.Join(*ran, ","); got != "bundle install,run lint,run test,run build,"+again+"run test" {
		t.Fatalf("ran %s", got)
	}
	if len(r.Issues()) != 0 {
		t.Fatalf("issues for a ready repo: %+v", r.Issues())
	}
}

func TestBlockersComeWithIssues(t *testing.T) {
	ctx := context.Background()
	// Tests fail on main; no lint, no e2e, no feature map, no CI.
	dir := tree(t, "package.json", "package-lock.json", "src/App.test.tsx")
	exec, _ := fails("run test")
	r := Assess(ctx, Input{Repo: repo("npm ci", "test", "build"), Dir: dir, Exec: exec})
	if r.Ready || titles(r, Blocker) != "`test` fails on main" {
		t.Fatalf("report:\n%s", r.Text())
	}
	if !strings.Contains(titles(r, Gap), "No lint check") || !strings.Contains(titles(r, Gap), "No feature map") {
		t.Fatalf("gaps: %s", titles(r, Gap))
	}
	is := r.Issues()
	if len(is) < 3 || is[0].Title != "Make the test check pass on main" || !strings.Contains(is[0].Body, "boom: run test") {
		t.Fatalf("issues: %+v", is)
	}

	// Install fails: nothing else runs.
	exec, ran := fails("npm ci")
	r = Assess(ctx, Input{Repo: repo("npm ci", "test"), Dir: dir, Exec: exec})
	if len(*ran) != 1 || titles(r, Blocker) != "Install fails on main" {
		t.Fatalf("ran %v\n%s", *ran, r.Text())
	}

	// Flaky tests.
	exec, _ = fails("flaky:run test")
	r = Assess(ctx, Input{Repo: repo("", "test"), Dir: dir, Exec: exec})
	if titles(r, Blocker) != "Tests are flaky" {
		t.Fatalf("report:\n%s", r.Text())
	}

	// No tests at all.
	exec, _ = fails()
	r = Assess(ctx, Input{Repo: repo("", "test"), Dir: tree(t, "go.mod", "main.go"), Exec: exec})
	if titles(r, Blocker) != "No tests" || r.Score >= 75 {
		t.Fatalf("report:\n%s", r.Text())
	}
}

func TestCouldNotRun(t *testing.T) {
	ctx := context.Background()
	dir := tree(t, "go.mod", "a_test.go")
	for name, exec := range map[string]Exec{
		"no docker": func(context.Context, string, string, bool) (int, string, error) {
			return -1, "", errors.New("docker: not found")
		},
		"missing command": func(context.Context, string, string, bool) (int, string, error) {
			return 127, "sh: run: not found", nil
		},
		"timeout": func(context.Context, string, string, bool) (int, string, error) { return -1, "", runner.ErrTimeout },
	} {
		r := Assess(ctx, Input{Repo: repo("", "lint", "test"), Dir: dir, Exec: exec})
		if !r.Incomplete || len(r.Runs) != 1 || len(r.Issues()) != 0 || !strings.Contains(r.Text(), "couldn't finish") {
			t.Errorf("%s: runs %d, issues %+v\n%s", name, len(r.Runs), r.Issues(), r.Text())
		}
	}
}

func TestMissingCommand(t *testing.T) {
	exec, _ := fails()
	rp := repo("", "test", "lint")
	delete(rp.Gates.Commands, "lint")
	r := Assess(context.Background(), Input{Repo: rp, Dir: tree(t, "go.mod", "a_test.go"), Exec: exec})
	if strings.Contains(titles(r, Good), "Every required check passes") {
		t.Fatalf("report:\n%s", r.Text())
	}
}

func TestSave(t *testing.T) {
	dir := t.TempDir()
	path, err := Save(dir, &Report{Repo: "o/api"})
	if err != nil || filepath.Base(path) != "o__api.json" {
		t.Fatalf("%s %v", path, err)
	}
}
