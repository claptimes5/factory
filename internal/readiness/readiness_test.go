package readiness

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/claptimes5/factory/internal/config"
	"github.com/claptimes5/factory/internal/runner"
	"gopkg.in/yaml.v3"
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
	dir := tree(t, "Gemfile", "Gemfile.lock", "app/models/user.rb", "spec/models/user_spec.rb", "spec/system/login_spec.rb", ".github/workflows/ci.yml")
	os.WriteFile(filepath.Join(dir, "features.yaml"), []byte("features:\n  - {name: Login, paths: [app/], tests: [spec/system/login_spec.rb], risk: high}\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "spec/system/login_spec.rb"), []byte("require \"prop_check\"\n"), 0o644)
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

func TestFeatureMapHealth(t *testing.T) {
	exec, _ := fails()
	for name, c := range map[string]struct{ yaml, want string }{
		"broken":   {"features: [{name: A}]\n", "The feature map doesn't parse"},
		"partial":  {"features:\n  - {name: Models, paths: [app/models/], tests: [spec/models/user_spec.rb]}\n", "Feature map covers 50% of the code"},
		"stale":    {"features:\n  - {name: All, paths: [app/, lib/gone.rb], tests: [spec/models/user_spec.rb]}\n", "1 feature map entries point at nothing"},
		"untested": {"features:\n  - {name: All, paths: [app/], tests: []}\n", "1 features have no tests: All"},
	} {
		dir := tree(t, "Gemfile", "Gemfile.lock", "app/models/user.rb", "app/controllers/home.rb", "spec/models/user_spec.rb", ".github/workflows/ci.yml")
		os.WriteFile(filepath.Join(dir, "features.yaml"), []byte(c.yaml), 0o644)
		r := Assess(context.Background(), Input{Repo: repo("", "test"), Dir: dir, Exec: exec})
		if !strings.Contains(titles(r, Gap), c.want) {
			t.Errorf("%s: gaps %s", name, titles(r, Gap))
		}
	}
}

func TestSave(t *testing.T) {
	dir := t.TempDir()
	path, err := Save(dir, &Report{Repo: "o/api"})
	if err != nil || filepath.Base(path) != "o__api.json" {
		t.Fatalf("%s %v", path, err)
	}
}

// A high-risk feature with tests but no property or fuzz test is a gap;
// a lower-risk one isn't, and an untested one is left to feature-tests.
func TestPropertyTests(t *testing.T) {
	dir := tree(t, "go.mod", "go.sum", "pay/pay.go", "pay/pay_test.go", "auth/auth.go", "auth/auth_test.go", "ui/ui.go", "ui/ui_test.go", "ops/ops.go", ".github/workflows/ci.yml")
	os.WriteFile(filepath.Join(dir, "auth/auth_test.go"), []byte("package auth\n\nfunc FuzzLogin(f *testing.F) {}\n"), 0o644)
	// A symlink out of the export doesn't count, whatever it points at.
	outside := filepath.Join(t.TempDir(), "rapid_test.go")
	os.WriteFile(outside, []byte("import \"pgregory.net/rapid\"\n"), 0o644)
	os.Remove(filepath.Join(dir, "pay/pay_test.go"))
	os.Symlink(outside, filepath.Join(dir, "pay/pay_test.go"))
	os.WriteFile(filepath.Join(dir, "features.yaml"), []byte(`features:
  - {name: Payments, paths: [pay/], tests: [pay/], risk: high}
  - {name: Auth, paths: [auth/], tests: [auth/auth_test.go], risk: high}
  - {name: UI, paths: [ui/], tests: [ui/], risk: medium}
  - {name: Ops, paths: [ops/], tests: [], risk: high}
`), 0o644)
	exec, _ := fails()
	r := Assess(context.Background(), Input{Repo: repo("", "test"), SHA: "abc1234567", Dir: dir, Exec: exec})
	if !strings.Contains(titles(r, Gap), "High-risk features have no property tests: Payments;") && !strings.HasSuffix(titles(r, Gap), "High-risk features have no property tests: Payments") {
		t.Fatalf("gaps: %s", titles(r, Gap))
	}
	var found bool
	for _, is := range r.Issues() {
		if is.Key == "property-tests" {
			found = strings.Contains(is.Body, ": Payments.") && strings.Contains(is.Body, "pgregory.net/rapid")
		}
	}
	if !found {
		t.Fatalf("no property-tests issue: %+v", r.Issues())
	}
}

// A check that can't reach the database the repo's compose file names is
// a services problem for factory.yaml, not a code issue for the builder.
func TestServicesHint(t *testing.T) {
	dir := tree(t, "Gemfile", "Gemfile.lock", "app/models/user.rb", "spec/models/user_spec.rb", "docker-compose.yml")
	os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte("services:\n  db:\n    image: postgres:16-alpine\n  cache:\n    image: redis:7\n"), 0o644)
	exec := func(_ context.Context, _ string, cmd string, _ bool) (int, string, error) {
		if strings.Contains(cmd, "run test") {
			return 1, "PG::ConnectionBad: could not connect to server: Connection refused", nil
		}
		return 0, "", nil
	}
	r := Assess(context.Background(), Input{Repo: repo("", "test"), SHA: "abc1234567", Dir: dir, Exec: exec})
	var f *Finding
	for i := range r.Findings {
		if r.Findings[i].Area == "services" {
			f = &r.Findings[i]
		}
	}
	if f == nil || f.Level != Blocker || f.Issue != nil {
		t.Fatalf("finding = %+v\n%s", f, r.Text())
	}
	// The snippet pastes into factory.yaml as is.
	_, rest, _ := strings.Cut(f.Detail, "```yaml\n")
	snippet, _, _ := strings.Cut(rest, "```")
	var pasted struct {
		Repos []struct {
			Name     string
			Services map[string]config.Service
		}
	}
	if err := yaml.Unmarshal([]byte(snippet), &pasted); err != nil || len(pasted.Repos) != 1 {
		t.Fatalf("snippet doesn't parse (%v):\n%s", err, snippet)
	}
	pg := pasted.Repos[0].Services["postgres"]
	if pg.Image != "postgres:16" || pg.Env["POSTGRES_PASSWORD"] != "factory" || pg.Ready != "pg_isready -U postgres" || pasted.Repos[0].Services["redis"].Image != "redis:7" {
		t.Fatalf("snippet = %+v:\n%s", pasted, snippet)
	}
	// A refused connection that names none of them isn't a missing database.
	if h := servicesHint(dir, []string{"docker-compose.yml"}, repo("", "test"), "dial tcp api.example.com:443: connection refused"); h != "" {
		t.Fatalf("hint for an unrelated failure:\n%s", h)
	}
	for _, is := range r.Issues() {
		if is.Key == "check:test" {
			t.Fatal("filed a code issue for a missing service")
		}
	}
	// With services configured, or a failure of another kind, it's the code's.
	rp := repo("", "test")
	rp.Services = map[string]config.Service{"postgres": {Image: "postgres:16"}}
	r = Assess(context.Background(), Input{Repo: rp, SHA: "abc1234567", Dir: dir, Exec: exec})
	if !strings.Contains(titles(r, Blocker), "`test` fails on main") {
		t.Fatalf("with services: %s", titles(r, Blocker))
	}
}

func TestClaudeLoadsAgents(t *testing.T) {
	const gap = "CLAUDE.md doesn't load AGENTS.md"
	for name, c := range map[string]struct {
		files  map[string]string // a value of "-> x" is a symlink to x
		claude bool
		gap    bool
	}{
		"neither":               {nil, true, true},
		"codex builds":          {nil, false, false},
		"no import":             {map[string]string{"CLAUDE.md": "# Rules\n"}, true, true},
		"imports":               {map[string]string{"CLAUDE.md": "# Rules\n\n@AGENTS.md\n"}, true, false},
		"agents links":          {map[string]string{"CLAUDE.md": "rules", "AGENTS.md": "-> CLAUDE.md"}, true, false},
		"claude links":          {map[string]string{"AGENTS.md": "rules", "CLAUDE.md": "-> AGENTS.md"}, true, false},
		"links to an import":    {map[string]string{"docs/claude.md": "@AGENTS.md", "CLAUDE.md": "-> docs/claude.md"}, true, false},
		"links out of the repo": {map[string]string{"CLAUDE.md": "-> ../outside.md"}, true, true},
	} {
		t.Run(name, func(t *testing.T) {
			dir := tree(t, "go.mod", "x_test.go")
			os.WriteFile(filepath.Join(filepath.Dir(dir), "outside.md"), []byte("@AGENTS.md"), 0o644)
			for f, body := range c.files {
				p := filepath.Join(dir, f)
				os.MkdirAll(filepath.Dir(p), 0o755)
				if target, ok := strings.CutPrefix(body, "-> "); ok {
					os.Symlink(target, p)
				} else {
					os.WriteFile(p, []byte(body), 0o644)
				}
			}
			exec, _ := fails()
			r := Assess(context.Background(), Input{Repo: repo("go mod download", "test"), Dir: dir, Exec: exec, Claude: c.claude})
			if got := strings.Contains(titles(r, Gap), gap); got != c.gap {
				t.Fatalf("gap = %t, want %t:\n%s", got, c.gap, r.Text())
			}
		})
	}
}

func TestImportsAgents(t *testing.T) {
	for md, want := range map[string]bool{
		"@AGENTS.md":                        true,
		"# Rules\n\nSee @AGENTS.md.\n":      true,
		"Read @./AGENTS.md, then start.":    true,
		"```\n@AGENTS.md\n```\n":            false,
		"~~~md\n@AGENTS.md\n~~~\n":          false,
		"Write `@AGENTS.md` to import it.":  false,
		"<!-- @AGENTS.md -->":               false,
		"```\nexample\n```\n\n@AGENTS.md\n": true,
		"mail me@AGENTS.md":                 false,
		"@AGENTS.mdx":                       false,
	} {
		if got := ImportsAgents(md); got != want {
			t.Errorf("%q: %t, want %t", md, got, want)
		}
	}
}

// A missing REVIEW.md is a tip, which costs no points; one with keys the
// factory ignores says so.
func TestReviewGuide(t *testing.T) {
	exec, _ := fails()
	dir := tree(t, "go.mod", "a_test.go")
	r := Assess(context.Background(), Input{Repo: repo("", "test"), Dir: dir, Exec: exec})
	if !strings.Contains(titles(r, Tip), "No REVIEW.md") || !strings.Contains(r.Text(), "· No REVIEW.md") {
		t.Fatalf("report:\n%s", r.Text())
	}
	without := r.Score
	os.MkdirAll(filepath.Join(dir, ".github"), 0o755)
	os.WriteFile(filepath.Join(dir, ".github", "REVIEW.md"), []byte("---\ncolour: blue\n---\nLook hard at totals.\n"), 0o644)
	r = Assess(context.Background(), Input{Repo: repo("", "test"), Dir: dir, Exec: exec})
	if !strings.Contains(titles(r, Good), "Has review guidance in .github/REVIEW.md") || !strings.Contains(titles(r, Tip), "settings the factory ignores") || r.Score != without {
		t.Fatalf("report (%d, was %d):\n%s", r.Score, without, r.Text())
	}
}

// A test command that passes without running a test, like RSpec on a
// Minitest suite, is a blocker for the factory's config, with no issue.
func TestTestsThatRanNothing(t *testing.T) {
	dir := tree(t, "Gemfile", "test/test_helper.rb", "test/models/user_test.rb")
	exec := func(_ context.Context, _ string, cmd string, _ bool) (int, string, error) {
		if cmd == "run test" {
			return 0, "Finished in 0.0003 seconds (files took 0.4 seconds to load)\n0 examples, 0 failures\n", nil
		}
		return 0, "", nil
	}
	r := Assess(context.Background(), Input{Repo: repo("", "test"), Dir: dir, Exec: exec})
	if r.Ready || titles(r, Blocker) != "`test` passes without running any tests" || strings.Contains(titles(r, Good), "passing twice") {
		t.Fatalf("report:\n%s", r.Text())
	}
	for _, is := range r.Issues() {
		if strings.Contains(is.Title, "test") && !strings.Contains(is.Title, "end-to-end") {
			t.Fatalf("an issue was planned for the factory's own config: %+v", is)
		}
	}
	if !ranNoTests.MatchString("Run options: --seed 1\n\n0 runs, 0 assertions, 0 failures, 0 errors, 0 skips\n") || ranNoTests.MatchString("12 examples, 0 failures") {
		t.Fatal("ranNoTests: Minitest's empty run, or a real RSpec run")
	}
}

// A check that fails for a file .worktreeinclude names, which a fresh
// clone lacks, is the factory's setup to fix: no builder issue, and the
// finding says what to add, with the checkout found on this machine.
// Once files is set, or with nothing missing, it's a check like any other.
func TestChecksThatNeedUntrackedFiles(t *testing.T) {
	ctx := context.Background()
	dir := tree(t, "Gemfile", "Gemfile.lock", "test/models/user_test.rb")
	os.WriteFile(filepath.Join(dir, ".worktreeinclude"), []byte(".env\n\n/config/master.key\nnode_modules/\n"), 0o644)
	exec := func(_ context.Context, _ string, cmd string, _ bool) (int, string, error) {
		if cmd == "run test" {
			return 1, "ActiveSupport::EncryptedConfiguration::MissingKeyError: Missing encryption key. Write it to config/master.key", nil
		}
		return 0, "", nil
	}
	in := Input{Repo: repo("bundle install", "lint", "test"), Dir: dir, Exec: exec, Checkout: "/home/me/code/api"}
	r := Assess(ctx, in)
	var f *Finding
	for i := range r.Findings {
		if r.Findings[i].Area == "files" {
			f = &r.Findings[i]
		}
	}
	if f == nil || f.Level != Blocker || f.Issue != nil || f.Title != "`test` fails without config/master.key, which git doesn't track" {
		t.Fatalf("report:\n%s", r.Text())
	}
	if !strings.Contains(f.Detail, "from: /home/me/code/api") || !strings.Contains(f.Detail, "name: o/api") {
		t.Fatalf("detail: %s", f.Detail)
	}
	for _, is := range r.Issues() {
		if strings.Contains(is.Title, "test check") {
			t.Fatalf("a builder was asked to fix a missing key: %+v", is)
		}
	}
	if s := r.Steps()[1]; s.Key != "checks" || s.State != StepFailing {
		t.Fatalf("checks step %+v", s)
	}

	in.FilesSet = true
	r = Assess(ctx, in)
	if strings.Contains(r.Text(), "git doesn't track") || titles(r, Blocker) != "`test` fails on main" {
		t.Fatalf("with files set:\n%s", r.Text())
	}
}

// With a baseline in, lint failing says it's what came since, so it
// doesn't read as the old offenses again.
func TestLintSinceTheBaseline(t *testing.T) {
	dir := tree(t, "Gemfile", "Gemfile.lock", "test/models/user_test.rb")
	os.WriteFile(filepath.Join(dir, ".rubocop_todo.yml"), []byte("# This configuration was generated by\n# `rubocop --auto-gen-config --no-exclude-limit`\n"), 0o644)
	exec := func(_ context.Context, _ string, cmd string, _ bool) (int, string, error) {
		if cmd == "run lint" {
			return 1, "2786 files inspected, 1 offense detected, 1 offense autocorrectable", nil
		}
		return 0, "", nil
	}
	r := Assess(context.Background(), Input{Repo: repo("bundle install", "lint", "test"), Dir: dir, Exec: exec})
	if !r.Baselined || titles(r, Blocker) != "`lint` fails on main: 1 new offense since the baseline" {
		t.Fatalf("report:\n%s", r.Text())
	}
	if is := r.Issues(); len(is) == 0 || is[0].Key != "check:lint" {
		t.Fatalf("issues %+v", is)
	}
}

// Rails without its master key doesn't name it: its credentials read as
// empty. That's the key missing too, not a bug for a builder.
func TestMissingMasterKeyUnnamed(t *testing.T) {
	out := "bin/rails aborted!\nNoMethodError: undefined method '[]' for nil (NoMethodError)\n\n  config.stripe.secret_key = Rails.application.credentials.stripe[:secret_key]\n"
	if got := mentioned(out, []string{".env", "config/master.key", "node_modules"}); got != "config/master.key" {
		t.Fatalf("mentioned = %q", got)
	}
	if got := mentioned(out, []string{".env"}); got != "" {
		t.Fatalf("blamed %q, which the repo doesn't list", got)
	}
}

// Tests a recent check ran twice, passing, run once; the trust ends a day
// after the check that ran them twice, not the one that trusted it.
func TestStableTestsRunOnce(t *testing.T) {
	ctx := context.Background()
	dir := tree(t, "Gemfile", "Gemfile.lock", "test/models/user_test.rb")
	exec, ran := fails()
	first := Assess(ctx, Input{Repo: repo("bundle install", "test"), Dir: dir, Exec: exec})
	if strings.Count(strings.Join(*ran, "|"), "run test") != 2 || first.StableSince.IsZero() {
		t.Fatalf("first: ran %v, stable %v", *ran, first.StableSince)
	}
	exec, ran = fails()
	second := Assess(ctx, Input{Repo: repo("bundle install", "test"), Dir: dir, Exec: exec, Last: first})
	if strings.Count(strings.Join(*ran, "|"), "run test") != 1 || !second.StableSince.Equal(first.StableSince) || !strings.Contains(titles(second, Good), "so once now") {
		t.Fatalf("second: ran %v, stable %v\n%s", *ran, second.StableSince, second.Text())
	}
	// A day after the proof, twice again.
	second.StableSince = second.StableSince.Add(-25 * time.Hour)
	exec, ran = fails()
	Assess(ctx, Input{Repo: repo("bundle install", "test"), Dir: dir, Exec: exec, Last: second})
	if strings.Count(strings.Join(*ran, "|"), "run test") != 2 {
		t.Fatalf("ran %v after the trust ran out", *ran)
	}
}
