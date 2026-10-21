// Package readiness measures how ready a repository is to be worked on by
// the factory: whether its checks install and pass on the base branch, are
// stable, and cover what a builder could break. Each gap comes with an
// issue the factory can work to close it.
package readiness

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/claptimes5/factory/internal/config"
	"github.com/claptimes5/factory/internal/features"
	"github.com/claptimes5/factory/internal/gates"
	"github.com/claptimes5/factory/internal/reviewmd"
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
	// Tip: optional, and worth doing; it doesn't count against the score.
	Tip Level = "tip"
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
	// Number is the issue filed for it, once one is (0 before), and
	// State is whether that was open or closed when last seen.
	Number int    `json:"number,omitempty"`
	State  string `json:"state,omitempty"`
}

// Run is one command the check ran.
type Run struct {
	Gate     string        `json:"gate"`
	Command  string        `json:"command"`
	Passed   bool          `json:"passed"`
	Duration time.Duration `json:"duration"`
	Output   string        `json:"output,omitempty"` // the end of it, when it failed or ran no tests
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
	// Filed are the numbers of every issue the check has filed on the
	// repo, as last read, fixed or not.
	Filed []int `json:"filed,omitempty"`
	// Baselined: the commit checked has a lint baseline RuboCop wrote
	// (.rubocop_todo.yml from --auto-gen-config), so lint failing there
	// is offenses since, not the old ones.
	Baselined bool `json:"baselined,omitempty"`
	// StableSince is when the tests, run by TestCommand, last passed
	// twice in a row, by this check or one it trusted; zero if not.
	StableSince time.Time `json:"stable_since,omitzero"`
	TestCommand string    `json:"test_command,omitempty"`
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
	// Claude says Claude Code builds for the repo, so its CLAUDE.md
	// should load AGENTS.md.
	Claude bool
	// AccessChecked says the repo's host was asked what the factory's
	// token may do there, and Denied is what it may not.
	AccessChecked bool
	Denied        []Denied
	// FilesSet says the repo's files setting is set, so its untracked
	// files are copied in; Checkout, when it isn't, is a checkout of the
	// repo on this machine they could come from, or "".
	FilesSet bool
	Checkout string
	// Last is the repo's previous report, or nil: tests it proved pass
	// twice in a row, recently, run once now (see stableSince).
	Last *Report
}

// stableFor is how long tests that passed twice in a row are trusted not
// to be flaky, so a check runs them once.
const stableFor = 24 * time.Hour

// stableSince is when last's tests, run by cmd, were proved to pass twice
// in a row, if within stableFor; zero otherwise. The proof is the run
// that did it, not one that trusted it, so it ends stableFor after.
func stableSince(last *Report, cmd string) time.Time {
	if last == nil || last.TestCommand != cmd || last.StableSince.IsZero() || time.Since(last.StableSince) > stableFor {
		return time.Time{}
	}
	return last.StableSince
}

// Denied is something the factory does on the repo that its token may
// not, and what the host said about it.
type Denied struct{ What, Says string }

// slowTests is when a test run starts to hold up every job.
const slowTests = 15 * time.Minute

// access reports what the factory's token may not do on the repo: every
// change ends with a pull request, so a token that can't open one fails
// every job at its end, after the work and its cost.
func (r *Report) access(in Input) {
	if !in.AccessChecked {
		return
	}
	pr := "pull requests"
	if in.Repo.GitLab() {
		pr = "merge requests"
	}
	if len(in.Denied) == 0 {
		r.add(Finding{Level: Good, Area: "access", Title: "The factory's token can open " + pr + ", issues and labels"})
		return
	}
	var what []string
	var b strings.Builder
	b.WriteString("Every change ends with the factory opening a " + strings.TrimSuffix(pr, "s") + ", and it labels and comments on issues as it works, so without these every job would fail at its end. Give the token leave to:\n\n")
	for _, d := range in.Denied {
		what = append(what, d.What)
		b.WriteString("- " + d.What)
		if d.Says != "" {
			b.WriteString(" (it says: " + strings.TrimSpace(d.Says) + ")")
		}
		b.WriteString("\n")
	}
	if in.Repo.GitLab() {
		b.WriteString("\nOn GitLab, edit the token under your user settings, Access tokens: a fine-grained token needs each permission GitLab names above, or make a token with the `api` scope. Put a new one in the factory's env file (GITLAB_TOKEN), restart the factory, then check again.")
	} else {
		b.WriteString("\nOn GitHub, give the factory's GitHub App (or token) read and write access to pull requests, issues and contents, then check again.")
	}
	r.add(Finding{Level: Blocker, Area: "access", Title: "The factory's token can't " + joinAnd(what), Detail: b.String()})
}

// joinAnd lists words as a person would: "a, b and c".
func joinAnd(s []string) string {
	if len(s) < 2 {
		return strings.Join(s, "")
	}
	return strings.Join(s[:len(s)-1], ", ") + " and " + s[len(s)-1]
}

// Assess runs the repo's install and checks on its base branch (the tests
// twice, to catch flakiness) and inspects its files.
func Assess(ctx context.Context, in Input) *Report {
	r := &Report{Repo: in.Repo.Name, SHA: in.SHA, Branch: in.Repo.BaseBranch, Started: time.Now().UTC()}
	r.access(in)
	if b, err := os.ReadFile(filepath.Join(in.Dir, ".rubocop_todo.yml")); err == nil {
		r.Baselined = strings.Contains(string(b), "--auto-gen-config")
	}
	g := in.Repo.Gates
	files := listFiles(in.Dir)
	// run runs one command. When the command couldn't be run at all, it
	// records that and returns false: the rest of the report would only
	// repeat it.
	run := func(gate, cmd string, install bool) (Run, bool) {
		start := time.Now()
		code, out, err := in.Exec(ctx, in.Dir, cmd, install)
		x := Run{Gate: gate, Command: cmd, Passed: err == nil && code == 0, Duration: time.Since(start).Round(time.Second)}
		// Kept when it failed, or passed without running a test.
		if !x.Passed || ranNoTests.MatchString(out) {
			if err != nil {
				out += "\n" + err.Error()
			}
			x.Output = excerpt(out, 3000)
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

	// Files the repo's .worktreeinclude names (a master key, a .env)
	// aren't in a fresh clone; a check that fails for one is the factory's
	// setup to fix, not a builder's.
	var include []string
	if !in.FilesSet {
		include = worktreeInclude(in.Dir)
	}
	filesHit := false
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
		if hint := servicesHint(in.Dir, files, in.Repo, x.Output); !x.Passed && hint != "" {
			// The code is fine; the check can't reach a database the
			// factory doesn't start. Only factory.yaml can fix that, which
			// builders may not edit, so no issue.
			allPassed = false
			r.add(Finding{Level: Blocker, Area: "services", Title: "`" + name + "` can't reach a service it needs",
				Detail: hint + "\n\n" + fence(x.Output)})
		} else if missing := mentioned(x.Output, include); !x.Passed && missing != "" {
			allPassed, filesHit = false, true
			r.add(Finding{Level: Blocker, Area: "files", Title: "`" + name + "` fails without " + missing + ", which git doesn't track",
				Detail: filesDetail(in, include) + "\n\n" + fence(x.Output)})
		} else if !x.Passed {
			allPassed = false
			title := "`" + name + "` fails on " + r.Branch
			if name == "lint" && r.Baselined {
				// Not the old offenses again: what came after the baseline.
				title += ": " + sinceBaseline(x.Output)
			}
			r.add(Finding{Level: Blocker, Area: "checks", Title: title,
				Detail: "Every change would fail this check, whatever it does.\n\n" + fence(x.Output),
				Issue: &Issue{Key: "check:" + name, Title: fmt.Sprintf("Make the %s check pass on %s", name, r.Branch),
					Body: fmt.Sprintf("The factory runs `%s` as a required check, and it fails on `%s` before any change:\n\n%s\n"+
						"Fix the cause so it passes. Don't skip, delete or loosen tests or lint rules to get there; if a failure is a real bug, fix the bug.", cmd, r.Branch, fence(x.Output))}})
		}
	}
	if allPassed {
		r.add(Finding{Level: Good, Area: "checks", Title: "Every required check passes on " + r.Branch + ": " + strings.Join(orderGates(g.Required), ", ")})
	}
	if len(include) > 0 && !filesHit {
		r.add(Finding{Level: Gap, Area: "files", Title: "The factory doesn't copy the files .worktreeinclude lists", Detail: filesDetail(in, include)})
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
	case testRun != nil && testRun.Passed && ranNoTests.MatchString(testRun.Output):
		// The factory's to fix, not the repo's: no issue.
		r.add(Finding{Level: Blocker, Area: "tests", Title: "`test` passes without running any tests",
			Detail: fmt.Sprintf("Its output says no tests ran, though the repo has %d test files, so every change would pass it untested. "+
				"The command is probably for another framework, such as RSpec on a Minitest suite: fix `test` under the repo's gates in factory.yaml.\n\n%s", tests, fence(testRun.Output))})
	case testRun != nil && testRun.Passed && !stableSince(in.Last, testRun.Command).IsZero():
		// A recent check ran them twice, passing: once is enough now.
		r.StableSince, r.TestCommand = stableSince(in.Last, testRun.Command), testRun.Command
		r.add(Finding{Level: Good, Area: "tests", Title: fmt.Sprintf("%d test files, passing (twice in a row on the check of %s, so once now)",
			tests, r.StableSince.UTC().Format("Jan 2 15:04 UTC"))})
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
			r.StableSince, r.TestCommand = r.Started, testRun.Command
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
		r.mapHealth(in.Dir, files)
	} else {
		r.add(Finding{Level: Gap, Area: "feature map", Title: "No feature map",
			Detail: "A feature map lists what the product does, where each feature lives and which tests prove it. The factory uses it to pick what to verify for a change and to show the reviewer its blast radius.",
			Issue: &Issue{Key: "feature-map", Title: "Start a feature map",
				Body: "Create `features.yaml` at the repo root listing the product's user-facing features, under `features:`. For each: `name`, a one-line `description`, `paths` (the files, directories ending in `/`, or globs that implement it), `tests` (the test files that prove it), and where it's cheap, `check` (a command that runs just those tests) and `risk` (`low`, `medium` or `high`; high for auth, payments, data loss). Cover the main features first; mark any feature with no tests with `tests: []` rather than leaving it out."}})
	}
	if in.Claude && !loadsAgents(in.Dir) {
		r.add(Finding{Level: Gap, Area: "instructions", Title: "CLAUDE.md doesn't load AGENTS.md",
			Detail: "Claude Code reads CLAUDE.md, not AGENTS.md, so Claude builders miss what AGENTS.md says, including the lessons the factory proposes there. Add a line `@AGENTS.md` to CLAUDE.md (create it if there's none). Builders can't edit these files, so this one is yours; the factory's next lessons PR adds it too."})
	}
	r.reviewGuide(in.Dir)
	if ci(files) {
		r.add(Finding{Level: Good, Area: "ci", Title: "CI runs on the repo"})
	} else {
		r.add(Finding{Level: Gap, Area: "ci", Title: "No CI",
			Detail: "The factory checks its own changes, but changes merged any other way go unchecked."})
	}

	return r.finish()
}

// offenses is RuboCop's count of what it found.
var offenses = regexp.MustCompile(`(\d+) offenses? detected`)

// Offenses is how many offenses the run of gate counted (RuboCop's
// summary), or -1 when it didn't say.
func (r *Report) Offenses(gate string) int {
	for _, x := range r.Runs {
		if x.Gate != gate {
			continue
		}
		if m := offenses.FindStringSubmatch(x.Output); m != nil {
			n, _ := strconv.Atoi(m[1])
			return n
		}
	}
	return -1
}

// sinceBaseline says what lint found, with a baseline in: new offenses.
func sinceBaseline(out string) string {
	m := offenses.FindStringSubmatch(out)
	switch {
	case m == nil:
		return "new offenses since the baseline"
	case m[1] == "1":
		return "1 new offense since the baseline"
	}
	return m[1] + " new offenses since the baseline"
}

// worktreeInclude are the paths dir's .worktreeinclude names, as written
// there less a leading or trailing /, or none.
func worktreeInclude(dir string) []string {
	b, err := os.ReadFile(filepath.Join(dir, ".worktreeinclude"))
	if err != nil {
		return nil
	}
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		l = strings.Trim(strings.TrimSpace(l), "/")
		if l != "" && !strings.HasPrefix(l, "#") && !strings.HasPrefix(l, "!") {
			out = append(out, l)
		}
	}
	return out
}

// mentioned is the first of paths that out names, or "": a check that
// says it lacks config/master.key needs that file, not a code change.
// Some failures don't name the file they lack: Rails without its master
// key reads its credentials as empty, and fails where they're used.
func mentioned(out string, paths []string) string {
	for _, p := range paths {
		if !strings.ContainsAny(p, "*?[") && strings.Contains(out, p) {
			return p
		}
	}
	for _, p := range paths {
		if strings.HasSuffix(p, "master.key") && (strings.Contains(out, "Rails.application.credentials") || strings.Contains(out, "EncryptedConfiguration")) {
			return p
		}
	}
	return ""
}

// filesDetail says how to have the factory copy what .worktreeinclude
// names into its checkouts.
func filesDetail(in Input, include []string) string {
	from := in.Checkout
	where := "Point it at a checkout of the repo on this machine"
	if from != "" {
		where = "There's a checkout of the repo at " + from + ", so"
	} else {
		from = "/path/to/your/checkout"
	}
	return "A fresh copy of the repo doesn't have files git doesn't track, and this repo's .worktreeinclude lists some it needs (" + strings.Join(include, ", ") + "). " +
		where + " add this to the repo in factory.yaml:\n\n" +
		fence("repos:\n  - name: "+in.Repo.Name+"\n    files:\n      from: "+from) +
		"\nThe factory then copies what .worktreeinclude picks into every job's checkout, as Claude Code does for its worktrees, except folders an install makes, like node_modules. They're never committed. Restart the factory, then check again."
}

// reviewGuide looks for a REVIEW.md, which tells the factory's reviewers
// what matters in the repo. It's the repo's people's to write (builders
// can't edit it), so no issue is filed for it.
func (r *Report) reviewGuide(dir string) {
	for _, name := range reviewmd.Files {
		b, err := readCapped(filepath.Join(dir, filepath.FromSlash(name)))
		if err != nil {
			continue
		}
		g := reviewmd.Parse(name, string(b))
		r.add(Finding{Level: Good, Area: "review", Title: "Has review guidance in " + name})
		if len(g.Notes) > 0 {
			r.add(Finding{Level: Tip, Area: "review", Title: name + " has settings the factory ignores",
				Detail: "The factory reads `skip` and `max_minor` in its front matter.\n\n" + fence(strings.Join(g.Notes, "\n"))})
		}
		return
	}
	r.add(Finding{Level: Tip, Area: "review", Title: "No REVIEW.md",
		Detail: "A short REVIEW.md at the repo root (or in .github/) tells the factory's reviewers what failure looks like in this repo, which paths to skip, how many minor findings are worth reading, and real bugs from its past. " +
			"Reviewers, the critic and the verifier read it from the base branch, and builders can't edit it. See docs/quality.md#reviewmd in the factory's docs."})
}

// loadsAgents says whether Claude, reading CLAUDE.md in dir, reads
// AGENTS.md too: it imports it, or one links to the other.
func loadsAgents(dir string) bool {
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return false
	}
	// Either may link to another file, but only one in the repo.
	inRepo := func(name string) string {
		p, err := filepath.EvalSymlinks(filepath.Join(dir, name))
		if err != nil || !strings.HasPrefix(p, root+string(filepath.Separator)) {
			return ""
		}
		return p
	}
	p := inRepo("CLAUDE.md")
	if p == "" {
		return false
	}
	if p == inRepo("AGENTS.md") {
		return true
	}
	b, err := readCapped(p)
	return err == nil && ImportsAgents(string(b))
}

// ImportsAgents says whether a CLAUDE.md loads AGENTS.md into Claude with
// an @AGENTS.md import. Claude Code skips imports in code, so fenced
// blocks, code spans and HTML comments don't count.
func ImportsAgents(md string) bool {
	return importsAgents.MatchString(notCode.ReplaceAllString(md, " "))
}

var (
	importsAgents = regexp.MustCompile(`(?m)(^|\s)@(\./)?AGENTS\.md([\s.,;:)]|$)`)
	notCode       = regexp.MustCompile("(?ms)^[ \t]*(```|~~~).*?^[ \t]*(```|~~~)[ \t]*$|`[^`\n]*`|<!--.*?-->")
)

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
	rank := map[Level]int{Blocker: 0, Gap: 1, Tip: 2, Good: 3}
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
	mark := map[Level]string{Blocker: "✗", Gap: "!", Tip: "·", Good: "✓"}
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

// skipDirs hold dependencies and build output, not the repo's own code.
var skipDirs = map[string]bool{"node_modules": true, "vendor": true, ".venv": true, ".git": true,
	"dist": true, "build": true, "target": true, "coverage": true, ".next": true, "__pycache__": true}

func listFiles(dir string) []string {
	var out []string
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && skipDirs[d.Name()] {
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

// mapHealth checks a feature map against the repo: it must parse, cover
// most of the code, point at files that exist and name tests for each
// feature.
func (r *Report) mapHealth(dir string, files []string) {
	var raw []byte
	for _, name := range features.Files {
		if b, err := readCapped(filepath.Join(dir, name)); err == nil {
			raw = b
			break
		}
	}
	if raw == nil {
		// A map in another form (features.md, docs/features): it helps
		// people, but the factory can't read it.
		r.add(Finding{Level: Good, Area: "feature map", Title: "Has a feature map"})
		r.add(Finding{Level: Gap, Area: "feature map", Title: "The feature map isn't in features.yaml",
			Detail: "The factory reads features.yaml to find a change's blast radius and run the checks that prove it.",
			Issue: &Issue{Key: "feature-map", Title: "Move the feature map to features.yaml",
				Body: "Write the existing feature map as `features.yaml` at the repo root, under `features:`, with `name`, `description`, `paths`, `tests` and, where cheap, `check` and `risk` for each feature. Keep the old file only if people read it, and say in it that features.yaml is the source of truth."}})
		return
	}
	m, err := features.Parse(raw)
	if err != nil {
		r.add(Finding{Level: Gap, Area: "feature map", Title: "The feature map doesn't parse",
			Detail: fence(err.Error()),
			Issue: &Issue{Key: "feature-map-parse", Title: "Fix features.yaml so it parses",
				Body: "The factory can't read `features.yaml`:\n\n" + fence(err.Error()) + "\nFix it without dropping features. Each feature needs `name` and `paths`; `risk` is low, medium or high."}})
		return
	}
	h := m.Check(files)
	pct := 100
	if h.Code > 0 {
		pct = h.Covered * 100 / h.Code
	}
	if pct >= 70 {
		r.add(Finding{Level: Good, Area: "feature map", Title: fmt.Sprintf("Feature map covers %d%% of the code, in %d features", pct, len(m.Features))})
	} else {
		r.add(Finding{Level: Gap, Area: "feature map", Title: fmt.Sprintf("Feature map covers %d%% of the code", pct),
			Detail: "A change to code the map doesn't cover has no known blast radius, so the reviewer can't tell what it might break.",
			Issue: &Issue{Key: "feature-map-coverage", Title: "Map the rest of the code in features.yaml",
				Body: fmt.Sprintf("`features.yaml` covers %d of %d source files. Add the features the rest implement, or add their files to the features they belong to. Shared code can belong to several features.", h.Covered, h.Code)}})
	}
	if len(h.Stale) > 0 {
		r.add(Finding{Level: Gap, Area: "feature map", Title: fmt.Sprintf("%d feature map entries point at nothing", len(h.Stale)),
			Detail: "These paths or tests match no file:\n\n" + fence(strings.Join(h.Stale, "\n")),
			Issue: &Issue{Key: "feature-map-stale", Title: "Fix stale paths in features.yaml",
				Body: "These entries in `features.yaml` match no file, so changes there aren't traced:\n\n" + fence(strings.Join(h.Stale, "\n")) + "\nPoint each at where the code or test now lives, or remove it if the feature is gone."}})
	}
	if len(h.Untested) > 0 {
		r.add(Finding{Level: Gap, Area: "feature map", Title: fmt.Sprintf("%d features have no tests: %s", len(h.Untested), strings.Join(h.Untested, ", ")),
			Detail: "A change to these can only be checked by reading it.",
			Issue: &Issue{Key: "feature-tests", Title: "Add tests for the untested features",
				Body: "These features in `features.yaml` have no tests: " + strings.Join(h.Untested, ", ") + ".\n\nFor each, add tests of its main behavior and list them under its `tests`. Start with the riskiest."}})
	}
	if gaps := propertyGaps(dir, m, files); len(gaps) > 0 {
		list := strings.Join(gaps, ", ")
		r.add(Finding{Level: Gap, Area: "tests", Title: "High-risk features have no property tests: " + list,
			Detail: "Example tests check the cases someone thought of. A property test checks a rule against hundreds of generated inputs, which is where bugs in money, permissions and parsing hide.",
			Issue: &Issue{Key: "property-tests", Title: "Add property tests for the high-risk features",
				Body: "These high-risk features in `features.yaml` have tests, but none of them is a property-based or fuzz test: " + list + ".\n\n" +
					"For each, find the rules that must hold for every input (totals add up, a round trip gives back what went in, a user never sees another user's data, an invalid input is rejected and never crashes) and write a property test for each rule with the stack's usual library: `testing/quick` or `pgregory.net/rapid` or a fuzz test in Go, Hypothesis in Python, fast-check in JavaScript or TypeScript, prop_check in Ruby, proptest in Rust. Keep each run fast and seeded so failures reproduce, and list the files under the feature's `tests`."}})
	}
}

// propertyGaps names the high-risk features whose tests have no property
// or fuzz test. Untested ones are left to the feature-tests issue.
func propertyGaps(dir string, m *features.Map, files []string) []string {
	var gaps []string
	for _, f := range m.Features {
		if f.Risk != "high" {
			continue
		}
		tests := f.TestFiles(files)
		if len(tests) == 0 {
			continue
		}
		if !slices.ContainsFunc(tests, func(t string) bool {
			if !gates.IsTest(t) {
				return false // test data, or source a tests: directory also holds
			}
			b, err := readCapped(filepath.Join(dir, filepath.FromSlash(t)))
			return err == nil && features.HasProperties(string(b))
		}) {
			gaps = append(gaps, f.Name)
		}
	}
	return gaps
}

// readCapped reads the first MiB of a regular file. The export comes from
// the repo, so a symlink in it could point anywhere on the host, or at a
// file that never ends.
func readCapped(p string) ([]byte, error) {
	st, err := os.Lstat(p)
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", p)
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, 1<<20))
}

// knownServices are the images for the services a repo's files point at,
// with a readiness command and test settings.
var knownServices = []struct {
	name, image, env, ready string
	signs                   []string // lowercase text in docker-compose or config files
	errs                    []string // lowercase text in a failure that couldn't reach it
}{
	{"postgres", "postgres:16", "{POSTGRES_PASSWORD: factory}", "pg_isready -U postgres", []string{"image: postgres", "image: \"postgres", "adapter: postgresql", "postgres://", "postgresql://"}, []string{"postgres", "pg::", "psycopg", "5432"}},
	{"mysql", "mysql:8", "{MYSQL_ROOT_PASSWORD: factory}", "mysqladmin ping -h 127.0.0.1 -pfactory", []string{"image: mysql", "image: mariadb", "adapter: mysql2", "mysql://"}, []string{"mysql", "mariadb", "3306"}},
	{"redis", "redis:7", "", "redis-cli ping", []string{"image: redis", "redis://"}, []string{"redis", "6379"}},
	{"mongo", "mongo:7", "", "mongosh --quiet --eval 'db.runCommand({ping:1})'", []string{"image: mongo", "mongodb://"}, []string{"mongo", "27017"}},
}

// connRefused is how a check that can't reach a service usually fails.
var connRefused = regexp.MustCompile(`(?i)connection refused|econnrefused|could not connect to server|could not translate host name|can't connect to (local )?mysql|getaddrinfo|name or service not known|no such host|redis::cannotconnecterror|server selection timed out`)

// servicesHint suggests services for factory.yaml when a check failed
// the way one does that can't reach a database, the repo has none
// configured, and its files say which it uses; "" otherwise.
func servicesHint(dir string, files []string, repo config.Repo, output string) string {
	if len(repo.Services) > 0 || !connRefused.MatchString(output) {
		return ""
	}
	var text strings.Builder
	for _, f := range files {
		base := path.Base(f)
		if strings.HasPrefix(base, "docker-compose") || strings.HasPrefix(base, "compose.") || f == "config/database.yml" || base == ".env.example" || base == ".env.test" {
			if b, err := readCapped(filepath.Join(dir, filepath.FromSlash(f))); err == nil {
				text.Write(bytes.ToLower(b))
			}
		}
	}
	// The failure has to name one of them too: a check that can't reach
	// some web API isn't missing the repo's database.
	out := strings.ToLower(output)
	var snippet strings.Builder
	named := false
	for _, s := range knownServices {
		if slices.ContainsFunc(s.signs, func(sign string) bool { return strings.Contains(text.String(), sign) }) {
			named = named || slices.ContainsFunc(s.errs, func(e string) bool { return strings.Contains(out, e) })
			fmt.Fprintf(&snippet, "      %s:\n        image: %s\n", s.name, s.image)
			if s.env != "" {
				fmt.Fprintf(&snippet, "        env: %s\n", s.env)
			}
			fmt.Fprintf(&snippet, "        ready: %s\n", s.ready)
		}
	}
	if !named {
		return ""
	}
	return "The check couldn't connect to a service, and the repo's files point at the ones below. The factory starts services for every build and check when factory.yaml lists them under the repo; each is reached by its name as the host (`postgres:5432`), so point the test settings at it in the repo's `env:` (for example `DATABASE_URL`):\n\n```yaml\nrepos:\n  - name: " + repo.Name + "\n    services:\n" + snippet.String() + "```"
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

// excerpt keeps about n bytes of a command's output: its start, where an
// error usually says what went wrong (a stack trace's message comes before
// its frames), and its end, where a summary is.
func excerpt(s string, n int) string {
	if len(s) <= n {
		return s
	}
	head, end := n/3, n-n/3
	return strings.ToValidUTF8(s[:head], "") + "\n…\n" + strings.ToValidUTF8(s[len(s)-end:], "")
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
	path := Path(dir, r.Repo)
	return path, os.WriteFile(path, b, 0o644)
}

func jsonMarshal(r *Report) ([]byte, error) { return json.MarshalIndent(r, "", "  ") }

// ranNoTests matches a test runner's summary of a run with no tests:
// RSpec, Minitest, pytest, unittest, Jest and Vitest.
var ranNoTests = regexp.MustCompile(`(?m)\b0 examples, 0 failures|^0 runs, 0 assertions|collected 0 items|^Ran 0 tests|No tests found|No test files found`)
