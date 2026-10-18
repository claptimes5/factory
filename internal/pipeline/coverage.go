package pipeline

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/claptimes5/factory/internal/gates"
)

// coverLine reads Go's per-package coverage ("ok  mod/pkg 0.1s coverage:
// 81.2% of statements", or a package without tests at 0.0%) or a plain
// "name 81.2%" from a script of the repo's own.
var (
	goCover    = regexp.MustCompile(`^(?:ok|FAIL)?\s*(\S+)\s.*?coverage:\s+([0-9.]+)% of statements`)
	plainCover = regexp.MustCompile(`^(\S+)\s+([0-9.]+)%$`)
)

// parseCoverage maps each package in a coverage run's output to its
// percentage.
func parseCoverage(out string) map[string]float64 {
	m := map[string]float64{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		mt := goCover.FindStringSubmatch(line)
		if mt == nil {
			mt = plainCover.FindStringSubmatch(strings.TrimSpace(line))
		}
		if mt == nil {
			continue
		}
		if v, err := strconv.ParseFloat(mt[2], 64); err == nil {
			m[mt[1]] = v
		}
	}
	return m
}

// coverage holds the change to the floor the base sets: each package
// whose non-test Go code it changes must be covered at least as well as
// on the base, give or take slack points. Packages are compared by
// directory, so renaming the module doesn't reset them. A package new in
// the change must have some tests. The base is measured on its own
// export, installed first; a package it can't measure has no floor.
func (p *Pipeline) coverage(ctx context.Context, jobID int64, pt *part, gateDir string, snap snapshot) gates.Result {
	g := p.repoConf(pt.repo).Gates
	r := gates.Result{Gate: "coverage"}
	files, err := p.git(pt.repo).ChangedFiles(ctx, pt.ws.Repo, pt.base, pt.head)
	if err != nil {
		r.Status, r.Detail = gates.Fail, err.Error()
		return r
	}
	dirs := map[string]bool{}
	for _, f := range files {
		if strings.HasSuffix(f, ".go") && !gates.IsTest(f) && coverable(gateDir, f) {
			dirs[filepath.Dir(f)] = true
		}
	}
	if len(dirs) == 0 {
		r.Status, r.Detail = gates.Pass, "no non-test Go code changed"
		return r
	}
	// Each side measures only the packages it has, keyed by directory.
	measure := func(root string) (map[string]float64, string, int, error) {
		var pkgs []string
		for d := range dirs {
			if fi, err := os.Stat(filepath.Join(root, d)); err == nil && fi.IsDir() {
				pkgs = append(pkgs, "./"+d)
			}
		}
		sort.Strings(pkgs)
		if len(pkgs) == 0 {
			return nil, "", 0, nil
		}
		cmd := strings.ReplaceAll(g.Coverage, "{pkgs}", strings.Join(pkgs, " "))
		code, out, err := p.gateExec(ctx, jobID, root, cmd, gateRun{repo: pt.repo})
		return byDir(parseCoverage(out), modulePath(root), dirs), out, code, err
	}
	head, out, code, err := measure(gateDir)
	if note := snap.tamperNote(gateDir); note != "" {
		r.Status, r.Detail = gates.Fail, note
		return r
	}
	if head == nil && err == nil && code == 0 && out == "" {
		r.Status, r.Detail = gates.Pass, "every changed package was removed"
		return r
	}
	if len(head) == 0 {
		if err != nil || code != 0 {
			r.Status, r.Detail = gates.Fail, fmt.Sprintf("the coverage run failed (exit %d %v)\n%s", code, err, tail(out, 2000))
		} else {
			r.Status, r.Detail = gates.Misconfigured, "the coverage command printed no coverage lines\n"+tail(out, 1000)
		}
		return r
	}
	baseDir := filepath.Join(p.jobDir(jobID), "coverbase"+pt.sfx())
	if err := p.git(pt.repo).Export(ctx, pt.ws.Repo, pt.base, baseDir); err != nil {
		r.Status, r.Detail = gates.Fail, err.Error()
		return r
	}
	defer os.RemoveAll(baseDir)
	// The base's own failures aren't the change's: a package the base
	// can't measure has no floor to hold the change to.
	if g.Install != "" {
		if code, out, err := p.gateExec(ctx, jobID, baseDir, g.Install, gateRun{repo: pt.repo, install: true}); err != nil || code != 0 {
			r.Status, r.Detail = gates.Pass, fmt.Sprintf("couldn't install the base, so no floor: exit %d %v\n%s", code, err, tail(out, 1000))
			return r
		}
	}
	if ctx.Err() != nil {
		r.Status, r.Detail = gates.Fail, ctx.Err().Error()
		return r
	}
	baseExists := map[string]bool{}
	for d := range dirs {
		if fi, err := os.Stat(filepath.Join(baseDir, d)); err == nil && fi.IsDir() {
			baseExists[d] = true
		}
	}
	base, _, _, _ := measure(baseDir)
	var drops, lines []string
	names := make([]string, 0, len(dirs))
	for d := range dirs {
		names = append(names, d)
	}
	sort.Strings(names)
	for _, d := range names {
		h, inHead := head[d]
		b, inBase := base[d]
		switch {
		case !inHead && inBase:
			// Its tests didn't run or didn't build; the test gate says why.
			line := fmt.Sprintf("%s: %.1f%% → no coverage reported", d, b)
			lines, drops = append(lines, line), append(drops, line)
		case !inHead:
		case !baseExists[d] && h == 0:
			line := fmt.Sprintf("%s: new, with no tests", d)
			lines, drops = append(lines, line), append(drops, line)
		case !inBase:
			lines = append(lines, fmt.Sprintf("%s: %.1f%% (no floor)", d, h))
		default:
			line := fmt.Sprintf("%s: %.1f%% → %.1f%%", d, b, h)
			lines = append(lines, line)
			if h < b-g.CoverageSlack-1e-9 {
				drops = append(drops, line)
			}
		}
	}
	if len(drops) > 0 {
		r.Status = gates.Fail
		r.Detail = "coverage fell in the packages this change touches; add tests for the new code:\n" + strings.Join(drops, "\n")
		if g.CoverageSlack > 0 {
			r.Detail += fmt.Sprintf("\n(allowed drop: %.1f points)", g.CoverageSlack)
		}
		return r
	}
	r.Status, r.Detail = gates.Pass, strings.Join(lines, "\n")
	return r
}

// coverable says a changed Go file counts toward its package's floor:
// not vendored, not generated, and in the repo's root module (go test
// can't name a nested module's packages from the root).
func coverable(root, f string) bool {
	parts := strings.Split(filepath.ToSlash(f), "/")
	for _, p := range parts[:len(parts)-1] {
		if p == "vendor" || p == "testdata" {
			return false
		}
	}
	for d := filepath.Dir(f); d != "." && d != "/"; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(root, d, "go.mod")); err == nil {
			return false
		}
	}
	b, err := os.ReadFile(filepath.Join(root, f))
	if err != nil {
		return true // removed: its package may have lost tested code
	}
	return !generated.Match(b)
}

// generated is Go's marker for generated files.
var generated = regexp.MustCompile(`(?m)^// Code generated .* DO NOT EDIT\.$`)

// modulePath is the module named by root's go.mod, or "".
func modulePath(root string) string {
	b, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		if f := strings.Fields(line); len(f) >= 2 && f[0] == "module" {
			return strings.Trim(f[1], `"`)
		}
	}
	return ""
}

// byDir keys a run's coverage by the directories asked for: an import
// path under mod maps to its directory; a script's own names that are
// directories pass through.
func byDir(cov map[string]float64, mod string, dirs map[string]bool) map[string]float64 {
	out := map[string]float64{}
	for name, v := range cov {
		d := name
		switch {
		case mod != "" && name == mod:
			d = "."
		case mod != "" && strings.HasPrefix(name, mod+"/"):
			d = strings.TrimPrefix(name, mod+"/")
		}
		if dirs[d] {
			out[d] = v
		}
	}
	return out
}
