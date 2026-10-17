package pipeline

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/claptimes5/factory/internal/state"
	"github.com/claptimes5/factory/internal/store"
)

// A failing hidden scenario sends the work back like a failed gate, but the
// builder learns only its name and that it failed, never its contents.
func TestHiddenScenarioFailsWithoutLeaking(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	dir := t.TempDir()
	e.p.Cfg.Gates.ScenariosDir = dir
	writeFile(dir, "adds.sh", "test -f \"$FACTORY_WORK/add.go\"\n")
	// The secret check: the build must also ship sub.go.
	writeFile(dir, "subtracts.sh", "echo SCENARIO-SECRET-sub.go; test -f sub.go\n")
	e.addIssue(70)
	e.claude.builds = []func(string) error{goodBuild, func(work string) error {
		writeFile(work, "sub.go", "package fixture\n\nfunc Sub(a, b int) int { return a - b }\n")
		return goodBuild(work)
	}}
	e.codex.reviews = []string{pass}
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	it := e.item(t, 70)
	if it.Stage != state.AwaitingHuman {
		t.Fatalf("stage = %s", it.Stage)
	}
	prompts := e.claude.prompts["implement"]
	if len(prompts) != 2 || !strings.Contains(prompts[1], "scenario:subtracts") {
		t.Fatalf("the retry should name the failed scenario:\n%s", strings.Join(prompts, "\n---\n"))
	}
	for _, pr := range append(prompts, e.codex.prompts["review"]...) {
		if strings.Contains(pr, "SCENARIO-SECRET") || strings.Contains(pr, "test -f sub.go") {
			t.Fatalf("a prompt leaked the scenario:\n%s", pr)
		}
	}
	jobs, _ := e.p.Store.ListJobs(ctx, store.JobFilter{ItemID: it.ID, Kind: "implement"})
	log, err := os.ReadFile(filepath.Join(e.p.jobDir(jobs[len(jobs)-1].ID), "scenarios", "subtracts.log"))
	if err != nil || !strings.Contains(string(log), "SCENARIO-SECRET") {
		t.Fatalf("the human's copy of the output: %q %v", log, err)
	}
	if body := e.gh.comments[it.PRNumber][0]; !strings.Contains(body, "| scenario:adds | pass |") || !strings.Contains(body, "| scenario:subtracts | pass |") {
		t.Fatalf("PR body lacks scenario results:\n%s", body)
	}
}

func TestScenariosDirMissingIsMisconfigured(t *testing.T) {
	e := setup(t)
	e.p.Cfg.Gates.ScenariosDir = filepath.Join(t.TempDir(), "nope")
	rs := e.p.runScenarios(context.Background(), 1, t.TempDir())
	if len(rs) != 1 || rs[0].Status != "misconfigured" {
		t.Fatalf("results = %+v", rs)
	}
}

// A Playwright scenario runs in a real browser. Skipped where Playwright
// is not installed globally (the job image installs it).
func TestPlaywrightScenario(t *testing.T) {
	root, err := exec.Command("npm", "root", "-g").Output()
	if err != nil {
		t.Skip("npm not installed")
	}
	nodePath := strings.TrimSpace(string(root))
	if _, err := os.Stat(filepath.Join(nodePath, "playwright")); err != nil {
		t.Skip("playwright not installed globally")
	}
	t.Setenv("NODE_PATH", nodePath)
	e := setup(t)
	dir, work := t.TempDir(), t.TempDir()
	e.p.Cfg.Gates.ScenariosDir = dir
	writeFile(dir, "playwright.config.js", "module.exports = { use: { browserName: 'chromium' } };\n")
	writeFile(dir, "home.spec.js", `const { test, expect } = require('playwright/test');
const path = require('path');
test('home says hello', async ({ page }) => {
  await page.goto('file://' + path.join(process.env.FACTORY_WORK, 'index.html'));
  await expect(page.locator('#t')).toHaveText('Hello');
});
`)
	writeFile(work, "index.html", `<h1 id="t">Hello</h1>`)
	if rs := e.p.runScenarios(context.Background(), 1, work); len(rs) != 1 || rs[0].Status != "pass" {
		log, _ := os.ReadFile(filepath.Join(e.p.jobDir(1), "scenarios", "home.log"))
		t.Fatalf("results = %+v\n%s", rs, log)
	}
	writeFile(work, "index.html", `<h1 id="t">Bye</h1>`)
	if rs := e.p.runScenarios(context.Background(), 2, work); rs[0].Status != "fail" {
		t.Fatalf("a broken page must fail: %+v", rs)
	}
}
