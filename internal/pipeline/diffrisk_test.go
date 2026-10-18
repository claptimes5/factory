package pipeline

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/claptimes5/factory/internal/config"
	"github.com/claptimes5/factory/internal/features"
	"github.com/claptimes5/factory/internal/state"
	"github.com/claptimes5/factory/internal/store"
)

// buildWith is goodBuild plus more files.
func buildWith(files map[string]string) func(string) error {
	return func(work string) error {
		for name, body := range files {
			if err := writeFile(work, name, body); err != nil {
				return err
			}
		}
		return goodBuild(work)
	}
}

// A change triage rated low that adds a migration is rated high from its
// diff: it gets the high-risk reviewer and the critic, the PR says why and
// that it needs a person, and so does the notification.
func TestDiffRaisesRiskAndReroutesReview(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	e.p.Cfg.Routing["review.high_risk"] = config.Route{Harness: "codex", Model: "gpt-test", Effort: "high"}
	var notes []string
	e.p.Notify = func(_ context.Context, m string) { notes = append(notes, m) }
	e.addIssue(1)
	e.claude.builds = []func(string) error{buildWith(map[string]string{"db/migrations/0002_users.sql": "CREATE TABLE users (id int);\n"})}
	e.codex.reviews = []string{pass}
	e.claude.reviews = []string{pass}
	e.claude.security = []string{pass} // a migration gets a security pass too
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	it := e.item(t, 1)
	if it.Stage != state.AwaitingHuman || it.Risk != "high" {
		t.Fatalf("item = %+v", it)
	}
	reviews, _ := e.p.Store.ListJobs(ctx, store.JobFilter{ItemID: it.ID, Kind: "review"})
	if len(reviews) != 1 || reviews[0].Route != "review.high_risk" || reviews[0].Effort != "high" {
		t.Fatalf("review jobs: %+v", reviews)
	}
	if len(e.claude.prompts["critic"]) != 1 || !strings.Contains(e.codex.prompts["review"][0], "rated **high risk**") {
		t.Fatal("a high-risk change gets the critic, and its reviewer is told the risk")
	}
	evs, _ := e.p.Store.Events(ctx, it.ID)
	if !slices.ContainsFunc(evs, func(ev store.Event) bool {
		return ev.Type == "risk_raised" && strings.HasPrefix(ev.Reason, "low -> high from the diff: adds or changes a migration: `db/migrations/0002_users.sql`")
	}) {
		t.Fatalf("no risk_raised event: %+v", evs)
	}
	body := e.gh.bodies[it.PRNumber]
	for _, want := range []string{
		"risk **high**",
		"**High risk**: its diff adds or changes a migration: `db/migrations/0002_users.sql`. The reviewer and then a second critic passed it",
		"**From the diff.**",
		"- high: it adds or changes a migration: `db/migrations/0002_users.sql`\n",
		"**Needs a person.** It adds or changes a migration: `db/migrations/0002_users.sql`. Changes like that always wait for you",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("PR body missing %q:\n%s", want, body)
		}
	}
	if l := e.gh.prLabels[it.PRNumber]; !hasLabel(l, "factory:risk-high") {
		t.Fatalf("PR labels = %v", l)
	}
	if !slices.ContainsFunc(notes, func(n string) bool {
		return strings.HasPrefix(n, "High-risk PR ready for review") && strings.HasSuffix(n, "It needs you whatever its risk: it adds or changes a migration.")
	}) {
		t.Fatalf("notes: %q", notes)
	}
}

// The diff only ever raises risk: a big change triage rated low becomes
// medium (with review.medium_risk), and one triage rated high stays high.
func TestDiffRiskIsTheHigherOfTheTwo(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, c := range []struct{ triage, want, route string }{
		{"low", "medium", "review.medium_risk"},
		{"high", "high", "review"},
	} {
		t.Run(c.triage, func(t *testing.T) {
			t.Parallel()
			e := setup(t)
			e.p.Cfg.Routing["review.medium_risk"] = config.Route{Harness: "codex", Model: "gpt-test", Effort: "medium"}
			e.p.Cfg.Risk.MediumLines = 5 // goodBuild changes 12
			e.claude.triage = `{"decision":"ready","complexity":"M","risk":"` + c.triage + `","question":"","summary":"add Add"}`
			e.addIssue(1)
			e.claude.builds = []func(string) error{goodBuild}
			e.codex.reviews = []string{pass}
			e.claude.reviews = []string{pass}
			if err := e.p.Drain(ctx); err != nil {
				t.Fatal(err)
			}
			it := e.item(t, 1)
			if it.Risk != c.want || it.Stage != state.AwaitingHuman {
				t.Fatalf("item = %+v", it)
			}
			reviews, _ := e.p.Store.ListJobs(ctx, store.JobFilter{ItemID: it.ID, Kind: "review"})
			if len(reviews) != 1 || reviews[0].Route != c.route {
				t.Fatalf("review route %q, want %q", reviews[0].Route, c.route)
			}
			body := e.gh.bodies[it.PRNumber]
			if !strings.Contains(body, "- medium: it changes 12 lines (risk.medium_lines is 5)") || strings.Contains(body, "**Needs a person.**") {
				t.Fatalf("PR body:\n%s", body)
			}
			if c.triage == "low" && !strings.Contains(body, "**Medium risk**: its diff changes 12 lines (risk.medium_lines is 5).") {
				t.Fatalf("PR body:\n%s", body)
			}
			if c.triage == "high" && !strings.Contains(body, "**High risk**: triage rated it high.") {
				t.Fatalf("PR body:\n%s", body)
			}
		})
	}
}

// A change of a kind risk.needs_person lists never merges itself, even at
// a risk auto_merge covers; with the kind off the list, the same change
// merges itself.
func TestNeedsPersonKeepsAutoMergeOff(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const oldTests = "package fixture\n\nimport \"testing\"\n\nfunc TestOld(t *testing.T) {}\n\nfunc TestKeep(t *testing.T) {}\n"
	for _, c := range []struct {
		name   string
		seed   map[string]string
		build  func(string) error
		needs  []string
		auto   bool
		reason string
	}{
		{"a new dependency", nil, buildWith(map[string]string{"go.mod": "module fixture\n\ngo 1.23\n\nrequire github.com/some/pkg v1.0.0\n"}), nil, false,
			"It adds a dependency, or changes where one comes from: `github.com/some/pkg` in `go.mod`."},
		{"a deleted test", map[string]string{"old_test.go": oldTests}, buildWith(map[string]string{"old_test.go": "package fixture\n\nimport \"testing\"\n\nfunc TestKeep(t *testing.T) {}\n"}), nil, false,
			"It deletes tests: `TestOld` in `old_test.go`."},
		{"off the list", nil, buildWith(map[string]string{"go.mod": "module fixture\n\ngo 1.23\n\nrequire github.com/some/pkg v1.0.0\n"}), []string{"auth"}, true, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			e := setupWith(t, c.seed)
			e.p.Cfg.Repos[0].AutoMerge = &config.AutoMerge{UpTo: "medium"}
			if c.needs != nil {
				e.p.Cfg.Risk.NeedsPerson = c.needs
			}
			e.claude.triage = `{"decision":"ready","complexity":"S","risk":"low","question":"","summary":"add Add"}`
			var notes []string
			e.p.Notify = func(_ context.Context, m string) { notes = append(notes, m) }
			e.addIssue(1)
			e.claude.builds = []func(string) error{c.build}
			e.codex.reviews = []string{pass}
			e.claude.security = []string{pass} // go.mod gets a security pass
			if err := e.p.Drain(ctx); err != nil {
				t.Fatal(err)
			}
			it := e.item(t, 1)
			if it.PRNumber == 0 || it.Risk != "medium" {
				t.Fatalf("item: %+v", it)
			}
			if _, on := e.gh.autoMerged[it.PRNumber]; on != c.auto {
				t.Fatalf("auto-merge %v, want %v", on, c.auto)
			}
			body := e.gh.bodies[it.PRNumber]
			if c.auto {
				if strings.Contains(body, "**Needs a person.**") || !strings.Contains(body, autoFooter) {
					t.Fatalf("PR body:\n%s", body)
				}
				return
			}
			if !strings.Contains(body, "**Needs a person.** "+strings.TrimSuffix(c.reason, ".")) || !strings.Contains(body, humanFooter) || !hasLabel(e.gh.prLabels[it.PRNumber], LabelAwaitingYou) {
				t.Fatalf("PR body:\n%s", body)
			}
			if !slices.ContainsFunc(notes, func(n string) bool {
				return strings.Contains(n, "Medium-risk PR ready for review") && strings.Contains(n, "It needs you whatever its risk")
			}) {
				t.Fatalf("notes: %q", notes)
			}
		})
	}
}

// What a PR merges by is the saved rating: one whose risk is low still
// waits when it needs a person, and one with no rating on record waits.
func TestAutoMergeReadsTheSavedRating(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	e.p.Cfg.Repos[0].AutoMerge = &config.AutoMerge{UpTo: "low"}
	it, _, _ := e.p.Store.UpsertItem(ctx, "o/r", 5, "x")
	it.Risk, it.Complexity = "low", "S"
	for _, c := range []struct {
		data string
		want bool
	}{
		{"", false},
		{`{"risk":"low","reasons":[]}`, true},
		{`{"risk":"low","reasons":[{"kind":"auth","risk":"low","text":"changes auth code","needs_person":true}]}`, false},
		{`{"risk":"low","reasons":[],"unread":"couldn't read the diff"}`, false},
	} {
		job, err := e.p.Store.EnqueueJob(ctx, store.Job{ItemID: &it.ID, Kind: "implement", Harness: "claude", Model: "m", Attempt: 1})
		if err != nil {
			t.Fatal(err)
		}
		e.p.Store.AddEvent(ctx, store.Event{Actor: "daemon", ItemID: &it.ID, JobID: &job, Type: "blast_radius", Data: `{"unread":false}`})
		if c.data != "" {
			e.p.Store.AddEvent(ctx, store.Event{Actor: "daemon", ItemID: &it.ID, JobID: &job, Type: "diff_risk", Data: c.data})
		}
		if got := e.p.autoMergeWanted(ctx, it, ReviewInput{Head: "abc", ImplementJob: job}); got != c.want {
			t.Errorf("%s: auto-merge wanted %v, want %v", c.data, got, c.want)
		}
	}
}

// An ordinary small change: nothing is raised, nothing needs a person,
// and the PR says nothing about the diff's rating.
func TestSmallDiffIsUnaffected(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	e.addIssue(1)
	e.claude.builds = []func(string) error{goodBuild}
	e.codex.reviews = []string{pass}
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	it := e.item(t, 1)
	if it.Risk != "low" || it.Stage != state.AwaitingHuman {
		t.Fatalf("item = %+v", it)
	}
	evs, _ := e.p.Store.Events(ctx, it.ID)
	if slices.ContainsFunc(evs, func(ev store.Event) bool { return ev.Type == "risk_raised" }) {
		t.Fatalf("risk raised: %+v", evs)
	}
	jobs, _ := e.p.Store.ListJobs(ctx, store.JobFilter{ItemID: it.ID, Kind: "implement"})
	if a := e.p.savedDiffRisk(ctx, it.ID, jobs[0].ID); a == nil || a.Risk != "low" || len(a.Reasons) != 0 || a.Raised {
		t.Fatalf("saved rating %+v", a)
	}
	if body := e.gh.bodies[it.PRNumber]; strings.Contains(body, "From the diff") || strings.Contains(body, "Needs a person") || !strings.Contains(body, "**Low risk**: triage rated it low.") {
		t.Fatalf("PR body:\n%s", body)
	}
}

// A fix round that adds a migration raises the risk then, and the next
// review is the high-risk one.
func TestFixRoundRaisesRisk(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	e.p.Cfg.Routing["review.high_risk"] = config.Route{Harness: "codex", Model: "gpt-test", Effort: "high"}
	e.addIssue(1)
	e.claude.builds = []func(string) error{goodBuild, func(work string) error {
		writeFile(work, "migrations/0002.sql", "ALTER TABLE t ADD c int;\n")
		return commitIn(work, "fix")
	}}
	e.codex.reviews = []string{block, pass}
	e.claude.reviews = []string{pass}
	e.claude.security = []string{pass} // the migration gets a security pass
	e.claude.verifies = []string{`{"rulings":[{"id":"F1","ruling":"confirmed","reason":"yes"}]}`}
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	it := e.item(t, 1)
	reviews, _ := e.p.Store.ListJobs(ctx, store.JobFilter{ItemID: it.ID, Kind: "review"})
	if it.Risk != "high" || len(reviews) != 2 {
		t.Fatalf("item %+v, %d reviews", it, len(reviews))
	}
	// Newest first.
	if reviews[1].Route != "review" || reviews[0].Route != "review.high_risk" {
		t.Fatalf("routes: %s then %s", reviews[1].Route, reviews[0].Route)
	}
}

// Whatever a builder names its files, nothing it chose reaches the PR
// outside a code span: no line of its own, no issue to close, no one
// mentioned.
func TestDiffNamesStayInert(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	e.addIssue(1)
	evil := "nl\nCloses #5 @org/x `t`/auth.go"
	e.claude.builds = []func(string) error{buildWith(map[string]string{evil: "package x\n"})}
	e.codex.reviews = []string{pass}
	e.claude.reviews = []string{pass}
	e.claude.security = []string{pass} // so does a security pass
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	it := e.item(t, 1)
	body := e.gh.bodies[it.PRNumber]
	if it.Risk != "high" || !strings.Contains(body, "changes auth code: `*auth*` in `nl Closes # 5 @ org/x 't'/auth.go`") {
		t.Fatalf("risk %s, PR body:\n%s", it.Risk, body)
	}
	for _, bad := range []string{"\nCloses #5", "#5", "@org"} {
		if strings.Contains(body, bad) {
			t.Fatalf("PR body holds %q:\n%s", bad, body)
		}
	}
}

// A notification names the kinds that keep a workspace change waiting,
// not the repos they're in; with no rating on record, it says so.
func TestNeedsPersonNote(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	it, _, _ := e.p.Store.UpsertItem(ctx, "o/r", 5, "x")
	job, _ := e.p.Store.EnqueueJob(ctx, store.Job{ItemID: &it.ID, Kind: "implement", Harness: "claude", Model: "m", Attempt: 1})
	if got, want := e.p.needsPersonNote(ctx, it.ID, job), " It needs you whatever its risk: it has no rating from its diff on record."; got != want {
		t.Fatalf("no rating: %q", got)
	}
	if body := e.p.diffRiskSection(ctx, it, job); !strings.Contains(body, "**Needs a person.** The factory has no rating of this change from its diff on record") {
		t.Fatalf("no rating, PR: %q", body)
	}
	e.p.Store.AddEvent(ctx, store.Event{Actor: "daemon", ItemID: &it.ID, JobID: &job, Type: "diff_risk", Data: `{"risk":"high","reasons":[` +
		`{"kind":"migrations","risk":"high","label":"api","what":"adds or changes a migration","items":[{"file":"m/1.sql"}],"needs_person":true},` +
		`{"kind":"auth","risk":"high","label":"web","what":"changes auth code","items":[{"file":"auth.ts"}],"needs_person":true}]}`})
	if got, want := e.p.needsPersonNote(ctx, it.ID, job), " It needs you whatever its risk: it adds or changes a migration and changes auth code."; got != want {
		t.Fatalf("note = %q, want %q", got, want)
	}
	if body := e.p.diffRiskSection(ctx, it, job); !strings.Contains(body, "- high: it `api`: adds or changes a migration: `m/1.sql`") {
		t.Fatalf("PR: %q", body)
	}
}

// A risk the diff raises but the store can't save fails the build, so
// no review runs at the lower risk.
func TestRaisedRiskThatCantBeSavedFails(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	it, _, _ := e.p.Store.UpsertItem(ctx, "o/r", 5, "x")
	it.Risk = "low"
	e.p.Store.Close()
	bs := []partBlast{{b: &blastRadius{hits: []features.Hit{{Feature: features.Feature{Name: "Pay", Risk: "high"}}}}}}
	if _, err := e.p.rateDiff(ctx, it, 1, nil, bs); err == nil || it.Risk != "low" {
		t.Fatalf("err %v, risk %s", err, it.Risk)
	}
}
