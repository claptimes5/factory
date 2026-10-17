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
func DiffGuard(ctx context.Context, repoDir, base string, g Guard) Result {
	var problems []string
	status, err := git(ctx, repoDir, "diff", "--name-status", "-M", base, "HEAD")
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
			for _, pp := range g.ProtectedPaths {
				if p == pp || (strings.HasSuffix(pp, "/") && strings.HasPrefix(p, pp)) {
					problems = append(problems, "edits protected path "+p)
				}
			}
		}
		if (code == "D" || strings.HasPrefix(code, "R")) && isTest(paths[0]) {
			problems = append(problems, "deletes or renames test "+paths[0])
		}
	}

	numstat, err := git(ctx, repoDir, "diff", "--numstat", base, "HEAD")
	if err != nil {
		return Result{"diff_guard", Fail, err.Error()}
	}
	total := 0
	for _, line := range strings.Split(strings.TrimSpace(numstat), "\n") {
		f := strings.Fields(line)
		if len(f) < 3 || path.Base(f[2]) == "go.sum" {
			continue
		}
		a, _ := strconv.Atoi(f[0])
		d, _ := strconv.Atoi(f[1])
		total += a + d
	}
	if g.MaxChangedLines > 0 && total > g.MaxChangedLines {
		problems = append(problems, fmt.Sprintf("changes %d lines (max %d); split the task", total, g.MaxChangedLines))
	}

	patch, err := git(ctx, repoDir, "diff", "-U0", base, "HEAD", "--", "*.go")
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

func isTest(p string) bool {
	return strings.HasSuffix(p, "_test.go") || strings.Contains(p, "/testdata/") || strings.HasPrefix(p, "testdata/")
}

func git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
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
