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
	for _, want := range []string{"feature:Math=pass", "## Blast radius", "| Math | high | `add.go`, `add_test.go` | `test -f add_test.go && echo math-ok`: pass; `add_test.go` |", "Not in the feature map: `extra.go`"} {
		if !strings.Contains(body, want) {
			t.Errorf("PR body missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "| Docs |") {
		t.Errorf("an untouched feature is in the blast radius:\n%s", body)
	}
	var prompt string
	for _, h := range []*fakeHarness{e.codex, e.claude} {
		if len(h.prompts["review"]) > 0 {
			prompt = h.prompts["review"][0]
		}
	}
	if !strings.Contains(prompt, "## Blast radius") || !strings.Contains(prompt, "| Math |") {
		t.Fatalf("review prompt has no blast radius:\n%s", prompt)
	}
}

// A failing feature check fails the gates, like any other check.
func TestFeatureCheckFails(t *testing.T) {
	ctx := context.Background()
	e := setupWith(t, map[string]string{"features.yaml": strings.Replace(featureMap, "test -f add_test.go && echo math-ok", "echo broken; exit 3", 1)})
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
