// Package gates runs the deterministic checks that decide whether a
// builder's work may go to review. Exit codes decide; the agent's claims
// are ignored. A required gate with no command is MISCONFIGURED and fails.
package gates

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Status values match the gate_results.status column.
const (
	Pass          = "pass"
	Fail          = "fail"
	Misconfigured = "misconfigured"
)

// Result is one gate's outcome.
type Result struct {
	Gate   string
	Status string
	Detail string
	// Output is the end of a passing command's output, for the PR's
	// Testing section. It is never stored: it can quote secrets, so the
	// pipeline redacts it before it goes anywhere.
	Output string
	// Fault is set on a failure the sandbox caused, not the code. Like
	// Output, it is never stored.
	Fault *Fault
}

// OutputTail is how much of a passing command's output Run keeps: far
// more than a PR shows, so the pipeline can redact it whole before it
// cuts it down. A cut drops the partial first line, so no secret on it
// is kept in part.
const OutputTail = 256 << 10

// Exec runs a shell command in the checkout and returns its exit code and
// combined output. The pipeline backs this with the job container.
type Exec func(ctx context.Context, command string) (code int, output string, err error)

// Run runs every gate in order. Required gates with no command report
// MISCONFIGURED; optional gates with no command are skipped.
func Run(ctx context.Context, required []string, commands map[string]string, exec Exec) []Result {
	order := []string{"types", "lint", "test", "build", "audit"}
	req := map[string]bool{}
	for _, g := range required {
		req[g] = true
		if !contains(order, g) {
			order = append(order, g)
		}
	}
	for g := range commands {
		if !contains(order, g) {
			order = append(order, g)
		}
	}
	var out []Result
	for _, g := range order {
		cmd := strings.TrimSpace(commands[g])
		if cmd == "" {
			if req[g] {
				out = append(out, Result{Gate: g, Status: Misconfigured, Detail: "required gate has no command in factory.yaml"})
			}
			continue
		}
		code, output, err := exec(ctx, cmd)
		switch {
		case err != nil:
			out = append(out, Result{Gate: g, Status: Fail, Detail: "could not run: " + err.Error()})
		case code != 0:
			out = append(out, Result{Gate: g, Status: Fail, Detail: fmt.Sprintf("exit %d\n%s", code, Excerpt(output, 4000)), Fault: SetupFault(output)})
		default:
			out = append(out, Result{Gate: g, Status: Pass, Output: TailLines(output, OutputTail)})
		}
	}
	return out
}

// Passed reports whether every result passed.
func Passed(rs []Result) bool {
	if len(rs) == 0 {
		return false
	}
	for _, r := range rs {
		if r.Status != Pass {
			return false
		}
	}
	return true
}

// VerdictLine is the single line the agent may not paraphrase.
func VerdictLine(rs []Result) string {
	parts := make([]string, len(rs))
	for i, r := range rs {
		parts[i] = r.Gate + "=" + r.Status
	}
	v := "FAIL"
	if Passed(rs) {
		v = "PASS"
	}
	return "GATES: " + v + " " + strings.Join(parts, " ")
}

// generated are files a tool writes, not a person: the size cap doesn't
// count them, since a small change can rewrite them whole (a lockfile, or
// RuboCop's todo list when a change drops what it lists).
var generated = []string{"go.sum", "package-lock.json", "pnpm-lock.yaml", "yarn.lock", "bun.lock", "bun.lockb",
	"Gemfile.lock", "uv.lock", "poetry.lock", "Pipfile.lock", "Cargo.lock", ".rubocop_todo.yml"}

// offenses is RuboCop's count of what it found.
var offenses = regexp.MustCompile(`(\d+) offenses? detected`)

// Offenses is how many offenses a linter's output counts (RuboCop's
// summary), or -1 when it doesn't say.
func Offenses(out string) int {
	m := offenses.FindAllStringSubmatch(out, -1)
	if m == nil {
		return -1
	}
	n, _ := strconv.Atoi(m[len(m)-1][1])
	return n
}

// Guard is the diff guard's configuration.
type Guard struct {
	MaxChangedLines int
	ProtectedPaths  []string
}

// DiffGuard blocks changes a builder must not make: deleted or renamed
// tests, new t.Skip or //nolint, protected-path edits (workflows, prompts,
// factory.yaml, lint config) and oversized diffs.
//
// base and head are commits in a daemon-owned repository.
func DiffGuard(ctx context.Context, repoDir, base, head string, g Guard) Result {
	var problems []string
	// -z: paths come raw, never C-quoted, so a name with a quote, a
	// newline or a non-ASCII letter can't slip past the protected paths.
	status, err := git(ctx, repoDir, "diff", "--no-ext-diff", "--no-textconv", "--name-status", "-z", "-M", base, head)
	if err != nil {
		return Result{Gate: "diff_guard", Status: Fail, Detail: err.Error()}
	}
	fields := strings.Split(strings.TrimSuffix(status, "\x00"), "\x00")
	for i := 0; i < len(fields); {
		code := fields[i]
		n := 1 // a rename or copy names two paths
		if strings.HasPrefix(code, "R") || strings.HasPrefix(code, "C") {
			n = 2
		}
		if code == "" || i+1+n > len(fields) {
			break
		}
		paths := fields[i+1 : i+1+n]
		i += 1 + n
		for _, p := range paths {
			if protected(p, g.ProtectedPaths) {
				problems = append(problems, "edits protected path "+p)
			}
		}
		if (code == "D" || strings.HasPrefix(code, "R")) && IsTest(paths[0]) {
			problems = append(problems, "deletes or renames test "+paths[0])
		}
	}

	raw, err := git(ctx, repoDir, "diff", "--no-ext-diff", "--no-textconv", "--raw", "--no-renames", base, head)
	if err != nil {
		return Result{Gate: "diff_guard", Status: Fail, Detail: err.Error()}
	}
	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		// :oldmode newmode oldsha newsha status\tpath
		if f := strings.Fields(line); len(f) >= 6 && f[1] == "160000" {
			problems = append(problems, "adds a submodule or nested repository at "+f[len(f)-1])
		}
	}

	numstat, err := git(ctx, repoDir, "diff", "--no-ext-diff", "--no-textconv", "--numstat", base, head)
	if err != nil {
		return Result{Gate: "diff_guard", Status: Fail, Detail: err.Error()}
	}
	total := 0
	for _, line := range strings.Split(strings.TrimSpace(numstat), "\n") {
		f := strings.Fields(line)
		if len(f) < 3 || contains(generated, path.Base(f[2])) {
			continue
		}
		if f[0] == "-" { // binary file
			total += binaryLines
			continue
		}
		a, _ := strconv.Atoi(f[0])
		d, _ := strconv.Atoi(f[1])
		total += a + d
	}
	if g.MaxChangedLines > 0 && total > g.MaxChangedLines {
		problems = append(problems, fmt.Sprintf("changes %d lines (max %d); split the task", total, g.MaxChangedLines))
	}

	patch, err := git(ctx, repoDir, "diff", "--no-ext-diff", "--no-textconv", "-U0", base, head, "--", "*.go")
	if err != nil {
		return Result{Gate: "diff_guard", Status: Fail, Detail: err.Error()}
	}
	file := ""
	for _, line := range strings.Split(patch, "\n") {
		if strings.HasPrefix(line, "+++ ") {
			file = strings.TrimPrefix(strings.TrimPrefix(line, "+++ "), "b/")
			continue
		}
		if !strings.HasPrefix(line, "+") {
			continue
		}
		if strings.Contains(line, "t.Skip(") || strings.Contains(line, "t.SkipNow(") {
			problems = append(problems, "adds a test skip in "+file)
		}
		if strings.Contains(line, "//nolint") {
			problems = append(problems, "adds //nolint in "+file)
		}
	}

	if len(problems) > 0 {
		return Result{Gate: "diff_guard", Status: Fail, Detail: strings.Join(dedupe(problems), "\n")}
	}
	return Result{Gate: "diff_guard", Status: Pass, Detail: ""}
}

// Protected says whether the diff guard protects path p under list.
func Protected(p string, list []string) bool { return protected(p, list) }

// protected matches a path against the protected list: "dir/" protects a
// tree, "a/b.yml" one file, and a bare name such as "CLAUDE.md" that file
// name in any directory.
func protected(p string, list []string) bool {
	for _, pp := range list {
		switch {
		case strings.HasSuffix(pp, "/"):
			if strings.HasPrefix(p, pp) {
				return true
			}
		case strings.Contains(pp, "/"):
			if p == pp {
				return true
			}
		default:
			if path.Base(p) == pp {
				return true
			}
		}
	}
	return false
}

// binaryLines is what one changed binary file counts toward max_changed_lines.
const binaryLines = 100

// IsTest reports whether a path is test code or test data, in Go, Ruby,
// JavaScript/TypeScript, Python or Rust. It goes by file names, plus a few
// directories only when the file in them is code of that kind: a bare
// features/ or spec/ directory is as often app code or an API spec.
func IsTest(p string) bool {
	p = filepath.ToSlash(p)
	base := path.Base(p)
	ext := path.Ext(base)
	in := func(dir string) bool { return strings.HasPrefix(p, dir+"/") || strings.Contains(p, "/"+dir+"/") }
	if in("testdata") {
		return true
	}
	if ext == ".snap" || in("__snapshots__") || in("fixtures") || in("__fixtures__") {
		return false
	}
	for _, suf := range []string{"_test.go", "_spec.rb", "_test.rb", "_test.py", ".feature"} {
		if strings.HasSuffix(base, suf) {
			return true
		}
	}
	if strings.HasPrefix(base, "test_") && ext == ".py" {
		return true
	}
	if jsCode[ext] {
		// foo.test.ts, foo.spec.jsx and the like.
		parts := strings.Split(base, ".")
		if len(parts) >= 3 && (parts[len(parts)-2] == "test" || parts[len(parts)-2] == "spec") {
			return true
		}
		if in("__tests__") || in("cypress") {
			return true
		}
	}
	code := jsCode[ext] || ext == ".go" || ext == ".rb" || ext == ".py" || ext == ".rs"
	switch {
	case !code:
		return false
	case in("test") || in("tests") || in("e2e"):
		// Helpers and suites (test/test_helper.rb, mocha's test/*.js,
		// test/e2e/framework/util.go).
		return true
	case in("spec"):
		// RSpec and Jasmine; a Go or Python spec/ package is usually code.
		return ext == ".rb" || jsCode[ext]
	}
	return false
}

var jsCode = map[string]bool{".js": true, ".jsx": true, ".ts": true, ".tsx": true, ".mjs": true, ".cjs": true, ".vue": true, ".svelte": true}

func git(ctx context.Context, dir string, args ...string) (string, error) {
	safe := []string{"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "--no-pager"}
	cmd := exec.CommandContext(ctx, "git", append(safe, args...)...)
	cmd.Dir = dir
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

// Excerpt keeps at most about n bytes of a failed command's output, in
// whole lines: its start, where most errors say what went wrong, and its
// end, where the rest do, with "…" between them.
func Excerpt(s string, n int) string {
	if len(s) <= n {
		return s
	}
	head := s[:n/3]
	if i := strings.LastIndexByte(head, '\n'); i >= 0 {
		head = head[:i+1]
	} else {
		head = ""
	}
	return head + TailLines(s[len(head):], n-len(head))
}

// TailLines keeps at most the last n bytes of s, in whole lines: when it
// cuts, the partial line at the cut goes too, and "…" marks the cut.
func TailLines(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := len(s) - n
	if s[cut-1] == '\n' { // the cut is at a line's start: nothing partial
		return "…\n" + s[cut:]
	}
	s = s[cut:]
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return "…\n" + s[i+1:]
	}
	return "…"
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

func dedupe(ss []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range ss {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
