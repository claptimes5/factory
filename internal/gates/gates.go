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
}

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
				out = append(out, Result{g, Misconfigured, "required gate has no command in factory.yaml"})
			}
			continue
		}
		code, output, err := exec(ctx, cmd)
		switch {
		case err != nil:
			out = append(out, Result{g, Fail, "could not run: " + err.Error()})
		case code != 0:
			out = append(out, Result{g, Fail, fmt.Sprintf("exit %d\n%s", code, tail(output, 4000))})
		default:
			out = append(out, Result{g, Pass, ""})
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
	status, err := git(ctx, repoDir, "diff", "--no-ext-diff", "--no-textconv", "--name-status", "-M", base, head)
	if err != nil {
		return Result{"diff_guard", Fail, err.Error()}
	}
	for _, line := range strings.Split(strings.TrimSpace(status), "\n") {
		f := strings.Split(line, "\t")
		if len(f) < 2 {
			continue
		}
		code, paths := f[0], f[1:]
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
		return Result{"diff_guard", Fail, err.Error()}
	}
	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		// :oldmode newmode oldsha newsha status\tpath
		if f := strings.Fields(line); len(f) >= 6 && f[1] == "160000" {
			problems = append(problems, "adds a submodule or nested repository at "+f[len(f)-1])
		}
	}

	numstat, err := git(ctx, repoDir, "diff", "--no-ext-diff", "--no-textconv", "--numstat", base, head)
	if err != nil {
		return Result{"diff_guard", Fail, err.Error()}
	}
	total := 0
	for _, line := range strings.Split(strings.TrimSpace(numstat), "\n") {
		f := strings.Fields(line)
		if len(f) < 3 || path.Base(f[2]) == "go.sum" {
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
		return Result{"diff_guard", Fail, err.Error()}
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
		return Result{"diff_guard", Fail, strings.Join(dedupe(problems), "\n")}
	}
	return Result{"diff_guard", Pass, ""}
}

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
// JavaScript/TypeScript, Python or Rust.
func IsTest(p string) bool {
	p = filepath.ToSlash(p)
	base := path.Base(p)
	for _, dir := range []string{"testdata", "test", "tests", "spec", "__tests__", "e2e", "cypress", "features"} {
		if strings.HasPrefix(p, dir+"/") || strings.Contains(p, "/"+dir+"/") {
			return true
		}
	}
	for _, suf := range []string{"_test.go", "_spec.rb", "_test.rb", "_test.py", "_test.rs"} {
		if strings.HasSuffix(base, suf) {
			return true
		}
	}
	if strings.HasPrefix(base, "test_") && strings.HasSuffix(base, ".py") {
		return true
	}
	// foo.test.ts, foo.spec.jsx and the like.
	parts := strings.Split(base, ".")
	return len(parts) >= 3 && (parts[len(parts)-2] == "test" || parts[len(parts)-2] == "spec")
}

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

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
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
