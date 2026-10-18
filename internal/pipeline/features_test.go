package pipeline

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/claptimes5/factory/internal/config"
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
		"**High risk**: it touches Math, a high-risk feature in the feature map. The reviewer and then a second critic passed it", "check the features under Blast radius by hand", "| Math | high | `add.go`, `add_test.go` | `test -f add_test.go && echo math-ok`: pass; `add_test.go` |", "Not in the feature map: `extra.go`"} {
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
	// The builder was told which features are high risk, before it built.
	if b := e.claude.prompts["implement"][0]; !strings.Contains(b, "These features are high risk: Math (`add.go`); if you change them, treat the change as high risk. Where the change has logic") {
		t.Fatalf("implement prompt:\n%s", b)
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

// An issue triage rated high still hears where the high-risk features are.
func TestImplementPromptHighRisk(t *testing.T) {
	ctx := context.Background()
	e := setupWith(t, map[string]string{"features.yaml": featureMap})
	e.claude.triage = `{"decision":"ready","complexity":"M","risk":"high","question":"","summary":"add Add"}`
	e.addIssue(1)
	e.claude.builds = []func(string) error{goodBuild}
	e.codex.reviews = []string{pass}
	e.claude.reviews = []string{pass}
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	b := e.claude.prompts["implement"][0]
	if !strings.Contains(b, "This issue is rated high risk. These features are high risk: Math (`add.go`);") || !strings.Contains(b, "property-based test") {
		t.Fatalf("implement prompt:\n%s", b)
	}
}

// A text-only issue builds on the XS route and budget, the builder is
// told it needs no test, and the reviewer is told to check that.
func TestTextOnlyLane(t *testing.T) {
	ctx := context.Background()
	e := setupWith(t, map[string]string{"README.md": "# Fixture\n"})
	e.p.Cfg.Gates.Commands["test"] = "grep -q fixture README.md"
	e.claude.triage = `{"decision":"ready","complexity":"XS","risk":"low","question":"","summary":"fix a typo"}`
	e.addIssue(1)
	e.claude.builds = []func(string) error{func(work string) error {
		if err := writeFile(work, "README.md", "# The fixture\n"); err != nil {
			return err
		}
		return commitIn(work, "copy")
	}}
	e.codex.reviews = []string{pass}
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if b := e.claude.prompts["implement"][0]; !strings.Contains(b, "rated a text-only change") || strings.Contains(b, "Write a failing test first that captures the issue, then make it pass.") {
		t.Fatalf("implement prompt:\n%s", b)
	}
	if b := e.codex.prompts["review"][0]; !strings.Contains(b, "Triage rated this a text-only change") {
		t.Fatalf("review prompt:\n%s", b)
	}
	jobs, _ := e.p.Store.ListJobs(ctx, store.JobFilter{ItemID: e.item(t, 1).ID, Kind: "implement"})
	if len(jobs) != 1 || jobs[0].BudgetUSD != e.p.Cfg.Budgets.PerJob["XS"] {
		t.Fatalf("implement jobs: %+v", jobs)
	}
	if it := e.item(t, 1); it.PRNumber == 0 {
		t.Fatalf("no PR: stage %s", it.Stage)
	}
}

func TestStepInstructionsReachPrompts(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	r := e.p.Cfg.Routing["implement.M"]
	r.Instructions = "Name tests after the behavior."
	e.p.Cfg.Routing["implement.M"] = r
	e.p.Cfg.Repos[0].Instructions = map[string]string{"review": "Flag any exported function without a doc comment.", "triage": "Rate anything touching billing high risk."}
	e.addIssue(1)
	e.claude.builds = []func(string) error{goodBuild}
	e.codex.reviews = []string{pass}
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ prompt, want, not string }{
		{e.claude.prompts["implement"][0], "## Instructions for this step\n\nName tests after the behavior.", "doc comment"},
		{e.codex.prompts["review"][0], "## Instructions for this step\n\nFlag any exported function without a doc comment.", "Name tests"},
		{e.claude.prompts["triage"][0], "Rate anything touching billing high risk.", "doc comment"},
	} {
		if !strings.Contains(c.prompt, c.want) || strings.Contains(c.prompt, c.not) {
			t.Errorf("prompt should have %q and not %q:\n%s", c.want, c.not, c.prompt)
		}
	}
	jobs, _ := e.p.Store.ListJobs(ctx, store.JobFilter{ItemID: e.item(t, 1).ID, Kind: "implement"})
	if len(jobs) != 1 || jobs[0].Route != "implement.M" {
		t.Fatalf("implement jobs: %+v", jobs)
	}
}

func TestAutoMergeUpToARiskLevel(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name, upTo, size string
		fail             error
		auto             bool
	}{
		{"low and small", "low", "S", nil, true},
		{"turned off", "none", "S", nil, false},
		{"too big", "low", "M", nil, false},
		{"forge refuses", "medium", "XS", fmt.Errorf("auto-merge is not allowed for this repository"), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := setup(t)
			e.p.Cfg.Repos[0].AutoMerge = &config.AutoMerge{UpTo: c.upTo}
			e.claude.triage = fmt.Sprintf(`{"decision":"ready","complexity":%q,"risk":"low","question":"","summary":"add Add"}`, c.size)
			e.gh.failAutoMerge = c.fail
			var notes []string
			e.p.Notify = func(_ context.Context, m string) { notes = append(notes, m) }
			e.addIssue(1)
			e.claude.builds = []func(string) error{goodBuild}
			e.codex.reviews = []string{pass}
			if err := e.p.Drain(ctx); err != nil {
				t.Fatal(err)
			}
			it := e.item(t, 1)
			if it.PRNumber == 0 || it.Stage != state.AwaitingHuman {
				t.Fatalf("item: %+v", it)
			}
			labels := e.gh.prLabels[it.PRNumber]
			got, queued := e.gh.autoMerged[it.PRNumber]
			if queued != c.auto || slices.Contains(labels, LabelAutoMerge) != c.auto || slices.Contains(labels, LabelAwaitingYou) == c.auto {
				t.Fatalf("auto-merge %v (%q), PR labels %v", queued, got, labels)
			}
			if c.fail != nil && !slices.ContainsFunc(notes, func(n string) bool { return strings.Contains(n, "Couldn't turn on auto-merge") }) {
				t.Fatalf("notes: %q", notes)
			}
			if body := e.gh.bodies[it.PRNumber]; c.fail != nil && (!strings.Contains(body, humanFooter) || strings.Contains(body, autoFooter)) {
				t.Fatalf("a PR that won't merge itself must not say it will:\n%s", body)
			}
			if !c.auto {
				return
			}
			jobs, _ := e.p.Store.ListJobs(ctx, store.JobFilter{ItemID: it.ID, Kind: "implement"})
			if want := jobs[len(jobs)-1].HeadSHA + " merge"; got != want {
				t.Fatalf("auto-merge pinned to %q, want %q", got, want)
			}
			// GitHub merges it once CI passes; the next poll closes it out.
			e.gh.prs["factory/issue-1"].Merged = true
			e.p.lastPoll = e.p.lastPoll.AddDate(0, 0, -1)
			e.p.Drain(ctx)
			if e.item(t, 1).Stage != state.Done {
				t.Fatalf("stage after merge = %s", e.item(t, 1).Stage)
			}
			evs, _ := e.p.Store.Events(ctx, it.ID)
			if !slices.ContainsFunc(evs, func(ev store.Event) bool { return ev.Type == "pr_merged" && ev.Actor == "daemon" }) {
				t.Fatalf("no daemon merge event: %+v", evs)
			}
		})
	}
}

func TestAutoMergeIsCancelledBeforeAnythingNewCanMerge(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	e.p.Cfg.Repos[0].AutoMerge = &config.AutoMerge{UpTo: "low"}
	e.claude.triage = `{"decision":"ready","complexity":"S","risk":"low","question":"","summary":"add Add"}`
	e.addIssue(1)
	e.claude.builds = []func(string) error{goodBuild}
	e.codex.reviews = []string{pass}
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	it := e.item(t, 1)
	if _, on := e.gh.autoMerged[it.PRNumber]; !on {
		t.Fatal("auto-merge should be on")
	}
	// Waiting on CI or an approval, a human parks it: it must not merge.
	if err := e.p.Park(ctx, it.ID, "not yet"); err != nil {
		t.Fatal(err)
	}
	if _, on := e.gh.autoMerged[it.PRNumber]; on || !slices.Contains(e.gh.cancelled, it.PRNumber) {
		t.Fatalf("auto-merge still on after parking; cancelled %v", e.gh.cancelled)
	}
	if slices.Contains(e.gh.prLabels[it.PRNumber], LabelAutoMerge) {
		t.Fatal("auto-merge label left on")
	}
	if p := e.p; p.autoMerged(ctx, e.item(t, 1)) {
		t.Fatal("the item still reads as auto-merging")
	}
}

func TestNoAutoMergeWhenTheFeatureMapCantBeRead(t *testing.T) {
	ctx := context.Background()
	e := setupWith(t, map[string]string{"features.yaml": "features: [not valid"})
	e.p.Cfg.Repos[0].AutoMerge = &config.AutoMerge{UpTo: "low"}
	e.claude.triage = `{"decision":"ready","complexity":"S","risk":"low","question":"","summary":"add Add"}`
	e.addIssue(1)
	e.claude.builds = []func(string) error{goodBuild}
	e.codex.reviews = []string{pass}
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	it := e.item(t, 1)
	if it.PRNumber == 0 {
		t.Fatalf("no PR: %s", it.Stage)
	}
	if _, on := e.gh.autoMerged[it.PRNumber]; on {
		t.Fatal("auto-merged with an unreadable feature map")
	}
}
