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
// on the base, give or take slack points. A package new in the change has
// no floor. The base is measured on its own export, installed first.
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
		if strings.HasSuffix(f, ".go") && !gates.IsTest(f) {
			dirs["./"+filepath.Dir(f)] = true
		}
	}
	if len(dirs) == 0 {
		r.Status, r.Detail = gates.Pass, "no non-test Go code changed"
		return r
	}
	// Each side measures only the packages it has.
	present := func(root string) []string {
		var out []string
		for d := range dirs {
			if fi, err := os.Stat(filepath.Join(root, d)); err == nil && fi.IsDir() {
				out = append(out, d)
			}
		}
		sort.Strings(out)
		return out
	}
	run := func(dir string, pkgs []string) (map[string]float64, string, error) {
		cmd := strings.ReplaceAll(g.Coverage, "{pkgs}", strings.Join(pkgs, " "))
		code, out, err := p.gateExec(ctx, jobID, dir, cmd, gateRun{repo: pt.repo})
		if err == nil && code != 0 {
			err = fmt.Errorf("exit %d", code)
		}
		return parseCoverage(out), out, err
	}
	headPkgs := present(gateDir)
	if len(headPkgs) == 0 {
		r.Status, r.Detail = gates.Pass, "every changed package was removed"
		return r
	}
	head, out, err := run(gateDir, headPkgs)
	if note := snap.tamperNote(gateDir); note != "" {
		r.Status, r.Detail = gates.Fail, note
		return r
	}
	if err != nil {
		r.Status, r.Detail = gates.Fail, fmt.Sprintf("the coverage run failed (%v)\n%s", err, tail(out, 2000))
		return r
	}
	if len(head) == 0 {
		r.Status, r.Detail = gates.Misconfigured, "the coverage command printed no coverage lines\n"+tail(out, 1000)
		return r
	}
	baseDir := filepath.Join(p.jobDir(jobID), "coverbase"+pt.sfx())
	if err := p.git(pt.repo).Export(ctx, pt.ws.Repo, pt.base, baseDir); err != nil {
		r.Status, r.Detail = gates.Fail, err.Error()
		return r
	}
	defer os.RemoveAll(baseDir)
	// The base's own failures aren't the change's: without a floor to
	// measure, there's nothing to hold it to.
	noFloor := func(why string) gates.Result {
		r.Status, r.Detail = gates.Pass, "couldn't measure the base, so no floor: "+why
		return r
	}
	if g.Install != "" {
		code, out, err := p.gateExec(ctx, jobID, baseDir, g.Install, gateRun{repo: pt.repo, install: true})
		if err != nil || code != 0 {
			return noFloor(fmt.Sprintf("install exit %d %v\n%s", code, err, tail(out, 1000)))
		}
	}
	basePkgs := present(baseDir)
	if len(basePkgs) == 0 {
		r.Status, r.Detail = gates.Pass, "only new packages: no floor"
		return r
	}
	base, out, err := run(baseDir, basePkgs)
	if err != nil {
		return noFloor(fmt.Sprintf("%v\n%s", err, tail(out, 1000)))
	}
	var drops, lines []string
	names := make([]string, 0, len(head))
	for k := range head {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		b, ok := base[k]
		if !ok {
			lines = append(lines, fmt.Sprintf("%s: %.1f%% (new)", k, head[k]))
			continue
		}
		line := fmt.Sprintf("%s: %.1f%% → %.1f%%", k, b, head[k])
		lines = append(lines, line)
		if head[k] < b-g.CoverageSlack-1e-9 {
			drops = append(drops, line)
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
