// Package readiness measures how ready a repository is to be worked on by
// the factory: whether its checks install and pass on the base branch, are
// stable, and cover what a builder could break. Each gap comes with an
// issue the factory can work to close it.
package readiness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/claptimes5/factory/internal/config"
	"github.com/claptimes5/factory/internal/gates"
	"github.com/claptimes5/factory/internal/runner"
)

// Level is how much a finding holds the factory back.
type Level string

const (
	// Blocker: every job on the repo would fail, or pass for the wrong
	// reason, until it is fixed.
	Blocker Level = "blocker"
	// Gap: jobs work, but less of what they change is verified.
	Gap Level = "gap"
	// Good: in place.
	Good Level = "good"
)

// Finding is one thing the check looked at.
type Finding struct {
	Level  Level  `json:"level"`
	Area   string `json:"area"` // install, checks, tests, e2e, feature map, ci
	Title  string `json:"title"`
	Detail string `json:"detail,omitempty"`
	// Issue is the work that closes a gap, written for the factory's
	// builder. Nil when there is nothing to do.
	Issue *Issue `json:"issue,omitempty"`
}

// Issue is a draft GitHub issue. Key names the gap it closes, so the
// same gap isn't filed twice when its title changes.
type Issue struct {
	Key   string `json:"key"`
	Title string `json:"title"`
	Body  string `json:"body"`
}

// Run is one command the check ran.
type Run struct {
	Gate     string        `json:"gate"`
	Command  string        `json:"command"`
	Passed   bool          `json:"passed"`
	Duration time.Duration `json:"duration"`
	Output   string        `json:"output,omitempty"` // the end of it, when it failed
}

// Report is the result for one repository.
type Report struct {
	Repo    string    `json:"repo"`
	SHA     string    `json:"sha"`
	Branch  string    `json:"branch"`
	Started time.Time `json:"started"`
	Score   int       `json:"score"` // 0 to 100
	Ready   bool      `json:"ready"` // no blockers
	// Incomplete is set when the check itself couldn't run (no Docker, a
	// missing image, a timeout), so the report says nothing about the repo
	// past that point.
	Incomplete bool      `json:"incomplete,omitempty"`
	Runs       []Run     `json:"runs"`
	Findings   []Finding `json:"findings"`
}

// Exec runs a command on the export the way gates do; install marks the
// install step.
type Exec func(ctx context.Context, dir, command string, install bool) (code int, output string, err error)

// Input is what Assess needs: the repo's resolved config and a fresh
// export of its base branch.
type Input struct {
	Repo config.Repo
	SHA  string
	Dir  string
	Exec Exec
}

// slowTests is when a test run starts to hold up every job.
const slowTests = 15 * time.Minute

// Assess runs the repo's install and checks on its base branch (the tests
// twice, to catch flakiness) and inspects its files.
func Assess(ctx context.Context, in Input) *Report {
	r := &Report{Repo: in.Repo.Name, SHA: in.SHA, Branch: in.Repo.BaseBranch, Started: time.Now().UTC()}
	g := in.Repo.Gates
	files := listFiles(in.Dir)
	// run runs one command. When the command couldn't be run at all, it
	// records that and returns false: the rest of the report would only
	// repeat it.
	run := func(gate, cmd string, install bool) (Run, bool) {
		start := time.Now()
		code, out, err := in.Exec(ctx, in.Dir, cmd, install)
		x := Run{Gate: gate, Command: cmd, Passed: err == nil && code == 0, Duration: time.Since(start).Round(time.Second)}
		if !x.Passed {
			if err != nil {
				out += "\n" + err.Error()
			}
			x.Output = tail(out, 3000)
		}
		r.Runs = append(r.Runs, x)
		switch {
		case errors.Is(err, runner.ErrTimeout):
			r.Incomplete = true
			r.add(Finding{Level: Blocker, Area: "checks", Title: "`" + gate + "` timed out after " + x.Duration.String(),
				Detail: "The check stopped here. Speed up or fix the hanging command, or raise timeouts.gates in factory.yaml, then run it again.\n\n" + fence(x.Output)})
			return x, false
		case err != nil || code == 125 || code == 126 || code == 127:
			// 125 is Docker failing to start the container; 126 and 127 are
			// the shell failing to run the command.
			r.Incomplete = true
			r.add(Finding{Level: Blocker, Area: "checks", Title: "Couldn't run `" + gate + "`",
				Detail: "This is a problem with the factory's setup or the command in factory.yaml, not with the repo's code, so the check stopped here. Fix it and run the check again.\n\n" + fence(x.Output)})
			return x, false
		}
		return x, true
	}

	installed := true
	if g.Install != "" {
		x, ok := run("install", g.Install, true)
		if !ok {
			return r.finish()
		}
		if !x.Passed {
			installed = false
			r.add(Finding{Level: Blocker, Area: "install", Title: "Install fails on " + r.Branch,
				Detail: "`" + g.Install + "` failed, so no other check can run.\n\n" + fence(x.Output),
				Issue: &Issue{Key: "install", Title: "Make `" + g.Install + "` work from a clean checkout",
					Body: fmt.Sprintf("The factory installs dependencies with `%s` on a fresh export of `%s`, and it fails:\n\n%s\n"+
						"Make it succeed from a clean checkout with no network access beyond the package registries. Commit any missing lockfile. "+
						"If the install command itself is wrong for this repo, say so in your summary with the command that works.", g.Install, r.Branch, fence(x.Output))}})
		} else {
			r.add(Finding{Level: Good, Area: "install", Title: "Installs from a clean checkout"})
		}
	}
	if !lockfile(files) {
		r.add(Finding{Level: Gap, Area: "install", Title: "No lockfile",
			Detail: "Without one, each install can pick up different dependency versions, so a check can fail or pass for reasons outside the change.",
			Issue: &Issue{Key: "lockfile", Title: "Commit a dependency lockfile",
				Body: "Generate and commit the lockfile for this repo's package manager (package-lock.json, pnpm-lock.yaml, yarn.lock, Gemfile.lock, uv.lock, poetry.lock or Cargo.lock) so installs are reproducible. Don't change dependency versions beyond what the lockfile pins today."}})
	}

	// Every required check must pass on the base branch, or every job fails.
	var testRun *Run
	allPassed := installed && len(g.Required) > 0
	for _, name := range orderGates(g.Required) {
		cmd := g.Commands[name]
		if cmd == "" {
			allPassed = false
			r.add(Finding{Level: Blocker, Area: "checks", Title: "The required check " + name + " has no command",
				Detail: "Add it under gates.commands in factory.yaml, or drop it from gates.required."})
			continue
		}
		if !installed {
			continue
		}
		x, ok := run(name, cmd, false)
		if !ok {
			return r.finish()
		}
		if name == "test" {
			testRun = &x
		}
		if !x.Passed {
			allPassed = false
			r.add(Finding{Level: Blocker, Area: "checks", Title: "`" + name + "` fails on " + r.Branch,
				Detail: "Every change would fail this check, whatever it does.\n\n" + fence(x.Output),
				Issue: &Issue{Key: "check:" + name, Title: fmt.Sprintf("Make the %s check pass on %s", name, r.Branch),
					Body: fmt.Sprintf("The factory runs `%s` as a required check, and it fails on `%s` before any change:\n\n%s\n"+
						"Fix the cause so it passes. Don't skip, delete or loosen tests or lint rules to get there; if a failure is a real bug, fix the bug.", cmd, r.Branch, fence(x.Output))}})
		}
	}
	if allPassed {
		r.add(Finding{Level: Good, Area: "checks", Title: "Every required check passes on " + r.Branch + ": " + strings.Join(orderGates(g.Required), ", ")})
	}
	for _, want := range []struct{ gate, why string }{
		{"test", "Nothing checks that a change works."},
		{"lint", "Style and common mistakes go unchecked, so reviewers spend time on them."},
		{"build", "A change that breaks the build can pass the other checks."},
	} {
		if !slices.Contains(g.Required, want.gate) {
			r.add(Finding{Level: Gap, Area: "checks", Title: "No " + want.gate + " check", Detail: want.why + " Add one under gates in factory.yaml."})
		}
	}

	tests := 0
	for _, f := range files {
		if gates.IsTest(f) {
			tests++
		}
	}
	switch {
	case tests == 0:
		r.add(Finding{Level: Blocker, Area: "tests", Title: "No tests",
			Detail: "The factory proves a change by its tests. With none, a reviewer can only read the code.",
			Issue: &Issue{Key: "no-tests", Title: "Add a test suite with tests for the main behaviors",
				Body: "This repo has no tests. Set up the standard test framework for its stack, wire it to the test command in factory.yaml, and add tests for the most important behaviors (the main user flows and the riskiest logic). Keep them fast and deterministic: no network, no dependence on time or order."}})
	case testRun != nil && testRun.Passed:
		// Run them again: a suite that passes and fails at random makes every
		// gate result a coin flip.
		// -count=1 in GOFLAGS keeps Go from replaying the first run's cached
		// results, keeping flags the repo sets; other tools ignore it.
		again, ok := run("test (again)", `export GOFLAGS="${GOFLAGS:+$GOFLAGS }-count=1"; `+testRun.Command, false)
		if !ok {
			return r.finish()
		}
		if !again.Passed {
			r.add(Finding{Level: Blocker, Area: "tests", Title: "Tests are flaky",
				Detail: "They passed, then failed on the same commit.\n\n" + fence(again.Output),
				Issue: &Issue{Key: "flaky", Title: "Fix flaky tests",
					Body: fmt.Sprintf("`%s` passed once and then failed on the same commit of `%s`:\n\n%s\n"+
						"Find the tests that depend on order, time, randomness, shared state or the network, and make them deterministic. Don't delete or skip them.", testRun.Command, r.Branch, fence(again.Output))}})
		} else {
			r.add(Finding{Level: Good, Area: "tests", Title: fmt.Sprintf("%d test files, passing twice in a row", tests)})
		}
		if testRun.Duration > slowTests {
			r.add(Finding{Level: Gap, Area: "tests", Title: "Tests take " + testRun.Duration.String(),
				Detail: "Every attempt waits for them, and a builder runs them several times.",
				Issue: &Issue{Key: "slow-tests", Title: "Speed up the test suite",
					Body: fmt.Sprintf("`%s` takes %s. Find the slowest tests and make them faster without losing coverage: share expensive setup, replace sleeps with waits on conditions, run independent tests in parallel.", testRun.Command, testRun.Duration)}})
		}
	}

	if e2e(files) || g.ScenariosDir != "" {
		r.add(Finding{Level: Good, Area: "e2e", Title: "Has end-to-end tests"})
	} else {
		r.add(Finding{Level: Gap, Area: "e2e", Title: "No end-to-end tests",
			Detail: "Unit tests can pass while the app is broken for its users. A few end-to-end tests of the main flows catch that.",
			Issue: &Issue{Key: "e2e", Title: "Add end-to-end smoke tests for the main user flows",
				Body: "Add a small end-to-end suite (Playwright for a web frontend, system or request specs for a Rails app, an HTTP-level test for an API) covering the two or three flows users depend on most. Run it from the test command or add it as its own check in factory.yaml. Keep it under a few minutes and free of external services."}})
	}
	if featureMap(files) {
		r.add(Finding{Level: Good, Area: "feature map", Title: "Has a feature map"})
	} else {
		r.add(Finding{Level: Gap, Area: "feature map", Title: "No feature map",
			Detail: "A feature map lists what the product does, where each feature lives and which tests prove it. The factory uses it to pick what to verify for a change and to show the reviewer its blast radius.",
			Issue: &Issue{Key: "feature-map", Title: "Start a feature map",
				Body: "Create `features.yaml` at the repo root listing the product's user-facing features. For each: `name`, a one-line `description`, `paths` (the files or directories that implement it) and `tests` (the test files that prove it). Cover the main features first; mark any feature with no tests with `tests: []` rather than leaving it out."}})
	}
	if ci(files) {
		r.add(Finding{Level: Good, Area: "ci", Title: "CI runs on the repo"})
	} else {
		r.add(Finding{Level: Gap, Area: "ci", Title: "No CI",
			Detail: "The factory checks its own changes, but changes merged any other way go unchecked."})
	}

	return r.finish()
}

// finish scores the report.
func (r *Report) finish() *Report {
	r.Score = 100
	for _, f := range r.Findings {
		switch f.Level {
		case Blocker:
			r.Score -= 25
		case Gap:
			r.Score -= 8
		}
	}
	r.Score = max(r.Score, 0)
	r.Ready = !slices.ContainsFunc(r.Findings, func(f Finding) bool { return f.Level == Blocker })
	return r
}

// add keeps findings ordered blockers first, in the order found.
func (r *Report) add(f Finding) {
	rank := map[Level]int{Blocker: 0, Gap: 1, Good: 2}
	i := len(r.Findings)
	for i > 0 && rank[r.Findings[i-1].Level] > rank[f.Level] {
		i--
	}
	r.Findings = slices.Insert(r.Findings, i, f)
}

// Issues are the drafts for every finding that has one, blockers first.
// An incomplete report has none: it can't tell which gaps are real.
func (r *Report) Issues() []Issue {
	var out []Issue
	for _, f := range r.Findings {
		if f.Issue != nil && !r.Incomplete {
			out = append(out, *f.Issue)
		}
	}
	return out
}

// Text is the report for a terminal.
func (r *Report) Text() string {
	var b strings.Builder
	verdict := "ready: the factory can start on issues now"
	switch {
	case r.Incomplete:
		verdict = "the check couldn't finish"
	case !r.Ready:
		verdict = "not ready yet: fix the blockers first"
	}
	sha := r.SHA
	if len(sha) > 7 {
		sha = sha[:7]
	}
	fmt.Fprintf(&b, "%s at %s (%s): %d/100, %s\n", r.Repo, r.Branch, sha, r.Score, verdict)
	mark := map[Level]string{Blocker: "✗", Gap: "!", Good: "✓"}
	for _, f := range r.Findings {
		fmt.Fprintf(&b, "  %s %s\n", mark[f.Level], f.Title)
	}
	return b.String()
}

func orderGates(required []string) []string {
	order := []string{"types", "lint", "test", "build"}
	var out []string
	for _, g := range order {
		if slices.Contains(required, g) {
			out = append(out, g)
		}
	}
	for _, g := range required {
		if !slices.Contains(out, g) {
			out = append(out, g)
		}
	}
	return out
}

func listFiles(dir string) []string {
	var out []string
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && (d.Name() == "node_modules" || d.Name() == "vendor" || d.Name() == ".venv" || d.Name() == ".git") {
			return filepath.SkipDir
		}
		if !d.IsDir() {
			rel, _ := filepath.Rel(dir, p)
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	return out
}

func has(files []string, match func(string) bool) bool { return slices.ContainsFunc(files, match) }

func lockfile(files []string) bool {
	locks := []string{"package-lock.json", "pnpm-lock.yaml", "yarn.lock", "bun.lockb", "bun.lock", "Gemfile.lock", "uv.lock", "poetry.lock", "Pipfile.lock", "Cargo.lock", "go.sum", "requirements.txt"}
	manifests := []string{"package.json", "Gemfile", "pyproject.toml", "Pipfile", "Cargo.toml", "go.mod"}
	if !has(files, func(f string) bool { return slices.Contains(manifests, f) }) {
		return true // nothing to lock
	}
	// A Go module with no dependencies has no go.sum.
	if slices.Contains(files, "go.mod") && !has(files, func(f string) bool { return slices.Contains(manifests[:5], f) }) {
		return true
	}
	return has(files, func(f string) bool { return slices.Contains(locks, f) })
}

func e2e(files []string) bool {
	return has(files, func(f string) bool {
		base := filepath.Base(f)
		return strings.HasPrefix(base, "playwright.config.") || strings.HasPrefix(base, "cypress.config.") ||
			strings.HasPrefix(f, "e2e/") || strings.Contains(f, "/e2e/") || strings.HasPrefix(f, "cypress/") ||
			strings.HasPrefix(f, "spec/system/") || strings.HasPrefix(f, "spec/features/") || strings.HasPrefix(f, "spec/requests/") ||
			strings.HasPrefix(f, "test/system/") || strings.HasPrefix(f, "test/integration/") ||
			strings.HasSuffix(f, ".feature")
	})
}

func featureMap(files []string) bool {
	return has(files, func(f string) bool {
		l := strings.ToLower(f)
		return l == "features.yaml" || l == "features.yml" || l == "features.json" || l == "features.md" ||
			strings.HasPrefix(l, "docs/features")
	})
}

func ci(files []string) bool {
	return has(files, func(f string) bool {
		return strings.HasPrefix(f, ".github/workflows/") || f == ".gitlab-ci.yml" || strings.HasPrefix(f, ".circleci/") || f == ".buildkite/pipeline.yml"
	})
}

func fence(s string) string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return ""
	}
	f := "```"
	for strings.Contains(s, f) {
		f += "`"
	}
	return f + "\n" + s + "\n" + f + "\n"
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}

// Save writes the report as JSON next to the factory's data.
func Save(dir string, r *Report) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	b, err := jsonMarshal(r)
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, strings.ReplaceAll(r.Repo, "/", "__")+".json")
	return path, os.WriteFile(path, b, 0o644)
}

func jsonMarshal(r *Report) ([]byte, error) { return json.MarshalIndent(r, "", "  ") }
