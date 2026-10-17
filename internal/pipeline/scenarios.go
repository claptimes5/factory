package pipeline

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/claptimes5/factory/internal/gates"
	"github.com/claptimes5/factory/internal/runner"
)

// Hidden scenarios are end-to-end checks kept outside every checkout the
// builder sees (gates.scenarios_dir on the host). The verify stage runs each
// one against the commit under test; the builder and the reviewer only ever
// learn pass or fail, and the full output stays in the job directory for
// the human.
//
// A scenario is a file in the directory:
//   - name.sh runs with sh, in the checkout;
//   - name.spec.js, .spec.mjs or .spec.ts is a Playwright test, run with the
//     directory as the Playwright config dir (put playwright.config.* there).
//
// Each gets FACTORY_SCENARIOS (the directory) and FACTORY_WORK (the checkout)
// in its environment.

const dockerScenarios = "/scenarios"

type scenario struct{ name, file string }

func (p *Pipeline) scenarios() ([]scenario, error) {
	dir := p.Cfg.Gates.ScenariosDir
	if dir == "" {
		return nil, nil
	}
	ents, err := os.ReadDir(p.Cfg.Path(dir))
	if err != nil {
		return nil, fmt.Errorf("scenarios: %w", err)
	}
	var out []scenario
	for _, e := range ents {
		n := e.Name()
		if e.IsDir() || strings.HasPrefix(n, ".") {
			continue
		}
		for _, ext := range []string{".sh", ".spec.js", ".spec.mjs", ".spec.ts"} {
			if strings.HasSuffix(n, ext) {
				out = append(out, scenario{strings.TrimSuffix(n, ext), n})
				break
			}
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].name < out[b].name })
	return out, nil
}

func (s scenario) command(dir string) string {
	if strings.HasSuffix(s.file, ".sh") {
		return "sh " + shellQuote(filepath.Join(dir, s.file))
	}
	return fmt.Sprintf("npx --no-install playwright test --reporter=line -c %s %s", shellQuote(dir), shellQuote(s.file))
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// runScenarios runs every hidden scenario against the exported checkout.
func (p *Pipeline) runScenarios(ctx context.Context, jobID int64, gateDir string) []gates.Result {
	list, err := p.scenarios()
	if err != nil {
		return []gates.Result{{Gate: "scenarios", Status: gates.Misconfigured, Detail: err.Error()}}
	}
	host := p.Cfg.Path(p.Cfg.Gates.ScenariosDir)
	seen, work := host, gateDir
	var mounts []runner.Mount
	if _, docker := p.Runner.(*runner.Docker); docker {
		seen, work = dockerScenarios, "/work"
		mounts = []runner.Mount{{Host: host, Container: dockerScenarios, ReadOnly: true}}
	}
	logDir := filepath.Join(p.jobDir(jobID), "scenarios")
	var out []gates.Result
	for _, s := range list {
		cmd := fmt.Sprintf("FACTORY_SCENARIOS=%s FACTORY_WORK=%s %s", shellQuote(seen), shellQuote(work), s.command(seen))
		code, output, err := p.gateExecWith(ctx, jobID, gateDir, cmd, mounts)
		if mkErr := os.MkdirAll(logDir, 0o755); mkErr == nil {
			p.warn(os.WriteFile(filepath.Join(logDir, s.name+".log"), []byte(output), 0o644), "scenario log")
		}
		r := gates.Result{Gate: "scenario:" + s.name, Status: gates.Pass}
		switch {
		case err != nil:
			r.Status, r.Detail = gates.Fail, "could not run" // the error may quote the scenario
		case code != 0:
			r.Status, r.Detail = gates.Fail, fmt.Sprintf("hidden scenario failed (exit %d)", code)
		}
		out = append(out, r)
	}
	return out
}

// gateExecWith is gateExec with extra mounts (the scenarios directory).
func (p *Pipeline) gateExecWith(ctx context.Context, jobID int64, workdir, command string, mounts []runner.Mount) (int, string, error) {
	if p.GateExec != nil || len(mounts) == 0 {
		return p.gateExec(ctx, jobID, workdir, command)
	}
	var out bytes.Buffer
	outDir, err := os.MkdirTemp(p.jobDir(jobID), "gate-out-")
	if err != nil {
		return -1, "", err
	}
	defer os.RemoveAll(outDir)
	code, err := p.Runner.Run(ctx, runner.Spec{
		Name: fmt.Sprintf("%s-gate-%d", containerName(jobID), p.gateSeq.Add(1)), Image: p.Cfg.Runner.Image, ReadOnly: true,
		Workdir: workdir, OutDir: outDir, Mounts: mounts, Cmd: []string{"sh", "-c", command + " 2>&1"}, Timeout: p.Cfg.Timeout("gates"),
	}, &out)
	return code, out.String(), err
}

// Check runs one command against a checkout the way gates do (in the job
// image, read-only). `factory eval` uses it for acceptance tests.
func (p *Pipeline) Check(ctx context.Context, dir, command string) (int, string, error) {
	if err := os.MkdirAll(p.jobDir(0), 0o755); err != nil {
		return -1, "", err
	}
	return p.gateExec(ctx, 0, dir, command)
}
