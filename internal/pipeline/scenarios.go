package pipeline

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
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

func (p *Pipeline) scenarios(dir string) ([]scenario, error) {
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
	path := filepath.Join(dir, s.file)
	if strings.HasSuffix(s.file, ".sh") {
		return "sh " + shellQuote(path)
	}
	// The global Playwright, by absolute path and from a neutral directory:
	// npx would prefer a node_modules/.bin/playwright the builder committed.
	// The filter is the scenario's anchored path, so a.spec.js does not
	// also run beta.spec.js; results go to a writable temp directory.
	return fmt.Sprintf(`NP="${NODE_PATH:-/usr/lib/node_modules}"; cd "${TMPDIR:-/tmp}" && node "${NP%%%%:*}/playwright/cli.js" test --reporter=line --output "${TMPDIR:-/tmp}/pw-results" -c %s %s`,
		shellQuote(dir), shellQuote("^"+regexp.QuoteMeta(path)+"$"))
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// runScenarios runs every hidden scenario in dir against the exported checkout.
func (p *Pipeline) runScenarios(ctx context.Context, repo string, jobID int64, gateDir, dir string) []gates.Result {
	list, err := p.scenarios(dir)
	if err != nil {
		return []gates.Result{{Gate: "scenarios", Status: gates.Misconfigured, Detail: err.Error()}}
	}
	host := p.Cfg.Path(dir)
	seen, work := host, gateDir
	var mounts []runner.Mount
	pathEnv := "" // in the job image, a fixed PATH: nothing from the checkout
	if _, docker := p.Runner.(*runner.Docker); docker {
		seen, work = dockerScenarios, "/work"
		pathEnv = "PATH=/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin "
		mounts = []runner.Mount{{Host: host, Container: dockerScenarios, ReadOnly: true}}
	}
	logDir := filepath.Join(p.jobDir(jobID), "scenarios")
	var out []gates.Result
	for i, s := range list {
		cmd := fmt.Sprintf("export %sFACTORY_SCENARIOS=%s FACTORY_WORK=%s; %s", pathEnv, shellQuote(seen), shellQuote(work), s.command(seen))
		code, output, err := p.gateExec(ctx, jobID, gateDir, cmd, gateRun{repo: repo, mounts: mounts, noNet: true})
		// Agents see an opaque id: even a scenario's name hints at the check.
		id := fmt.Sprintf("scenario:%d", i+1)
		if mkErr := os.MkdirAll(logDir, 0o755); mkErr == nil {
			p.warn(os.WriteFile(filepath.Join(logDir, fmt.Sprintf("%d-%s.log", i+1, s.name)), []byte(output), 0o644), "scenario log")
		}
		r := gates.Result{Gate: id, Status: gates.Pass}
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

// Check runs one command against a checkout the way gates do: in the
// repo's job image, after its install step. `factory eval` uses it for
// acceptance tests.
func (p *Pipeline) Check(ctx context.Context, repo, dir, command string) (int, string, error) {
	if err := os.MkdirAll(p.jobDir(0), 0o755); err != nil {
		return -1, "", err
	}
	if install := p.repoConf(repo).Gates.Install; install != "" {
		code, out, err := p.gateExec(ctx, 0, dir, install, gateRun{repo: repo, install: true})
		if err != nil || code != 0 {
			return code, "install failed:\n" + out, err
		}
	}
	return p.gateExec(ctx, 0, dir, command, gateRun{repo: repo})
}
