package pipeline

import (
	"context"
	"strings"
	"testing"

	"github.com/claptimes5/factory/internal/state"
	"github.com/claptimes5/factory/internal/store"
)

const featureMap = `features:
  - name: Math
    description: Adding numbers
    paths: [add.go]
    tests: [add_test.go]
    check: "test -f add_test.go && echo math-ok"
    risk: high
  - name: Docs
    paths: [doc.go]
    tests: []
`

// A change to a mapped feature runs that feature's check, is reviewed at
// the feature's risk, and shows its blast radius to the reviewer and in
// the PR.
func TestFeatureMapBlastRadius(t *testing.T) {
	ctx := context.Background()
	e := setupWith(t, map[string]string{"features.yaml": featureMap})
	e.addIssue(1)
	e.claude.builds = []func(string) error{func(work string) error {
		writeFile(work, "extra.go", "package fixture\n")
		return goodBuild(work)
	}}
	e.codex.reviews = []string{pass}
	e.claude.reviews = []string{pass}

	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	it := e.item(t, 1)
	if it.Stage != state.AwaitingHuman || it.Risk != "high" {
		t.Fatalf("item = %+v", it)
	}
	evs, _ := e.p.Store.Events(ctx, it.ID)
	raised := false
	for _, ev := range evs {
		raised = raised || (ev.Type == "risk_raised" && ev.Reason == "low -> high: touches Math")
	}
	if !raised {
		t.Fatalf("no risk_raised event in %+v", evs)
	}
	body := e.gh.comments[it.PRNumber][0]
	for _, want := range []string{"feature:Math=pass", "## Blast radius",
		"**High risk**: it touches Math, a high-risk feature in the feature map. Two independent reviewers passed it", "check the features under Blast radius by hand", "| Math | high | `add.go`, `add_test.go` | `test -f add_test.go && echo math-ok`: pass; `add_test.go` |", "Not in the feature map: `extra.go`"} {
		if !strings.Contains(body, want) {
			t.Errorf("PR body missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "| Docs |") {
		t.Errorf("an untouched feature is in the blast radius:\n%s", body)
	}
	var prompt string
	if len(e.codex.prompts["review"]) > 0 {
		prompt = e.codex.prompts["review"][0]
	}
	if !strings.Contains(prompt, "## Blast radius") || !strings.Contains(prompt, "| Math |") || !strings.Contains(prompt, "rated **high risk**") {
		t.Fatalf("review prompt has no blast radius or risk:\n%s", prompt)
	}
	// Raised to high, it got the critic too, and the PR says so in its label.
	if len(e.codex.prompts["review"]) != 1 || len(e.claude.prompts["critic"]) != 1 {
		t.Fatalf("want the reviewer and the critic: codex %d, claude %d", len(e.codex.prompts["review"]), len(e.claude.prompts["critic"]))
	}
	if l := e.gh.prLabels[it.PRNumber]; !hasLabel(l, "factory:risk-high") || hasLabel(l, "factory:risk-low") {
		t.Fatalf("PR labels = %v", l)
	}
}

// A failing feature check fails the gates, like any other check.
func TestFeatureCheckFails(t *testing.T) {
	ctx := context.Background()
	// Passes on base, which has no add.go; fails once the change adds it.
	e := setupWith(t, map[string]string{"features.yaml": strings.Replace(featureMap, "test -f add_test.go && echo math-ok", "test ! -f add.go || { echo broken; exit 3; }", 1)})
	e.addIssue(1)
	e.claude.builds = []func(string) error{goodBuild, goodBuild, goodBuild}
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	it := e.item(t, 1)
	if it.Stage == state.AwaitingHuman || it.PRNumber != 0 {
		t.Fatalf("item = %+v", it)
	}
	jobs, _ := e.p.Store.ListJobs(ctx, store.JobFilter{ItemID: it.ID})
	found := false
	for _, j := range jobs {
		gr, _ := e.p.Store.GateResults(ctx, j.ID)
		for _, g := range gr {
			found = found || (g.Gate == "feature:Math" && g.Status == "fail" && strings.Contains(g.Detail, "broken"))
		}
	}
	if !found {
		t.Fatal("no failing feature:Math gate")
	}
}

// A check that fails on the base branch too doesn't block the change: the
// builder can't fix the map, and it isn't the change's fault.
func TestFeatureCheckRedOnBase(t *testing.T) {
	ctx := context.Background()
	e := setupWith(t, map[string]string{"features.yaml": strings.Replace(featureMap, "test -f add_test.go && echo math-ok", "exit 3", 1)})
	e.addIssue(1)
	e.claude.builds = []func(string) error{goodBuild}
	e.codex.reviews = []string{pass}
	e.claude.reviews = []string{pass}
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	it := e.item(t, 1)
	if it.Stage != state.AwaitingHuman {
		t.Fatalf("item = %+v", it)
	}
	if body := e.gh.comments[it.PRNumber][0]; !strings.Contains(body, "`exit 3`: **fails, as on the base branch**") {
		t.Fatalf("PR body:\n%s", body)
	}
}

// The base gets the change's install step before the check reruns there,
// so a check that only fails for want of dependencies isn't excused.
func TestFeatureCheckBaseGetsInstall(t *testing.T) {
	ctx := context.Background()
	e := setupWith(t, map[string]string{"features.yaml": strings.Replace(featureMap, "test -f add_test.go && echo math-ok",
		"test -f .installed || exit 3; test ! -f add.go || exit 3", 1)})
	e.p.Cfg.Gates.Install = "touch .installed"
	e.addIssue(1)
	e.claude.builds = []func(string) error{goodBuild, goodBuild, goodBuild, goodBuild}
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if it := e.item(t, 1); it.Stage == state.AwaitingHuman {
		t.Fatalf("a real failure was excused as red on base: %+v", it)
	}
}
