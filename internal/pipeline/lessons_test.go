package pipeline

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/claptimes5/factory/internal/config"
	"github.com/claptimes5/factory/internal/state"
	"github.com/claptimes5/factory/internal/store"
)

const (
	oneLesson = `{"lessons":[{"line":"Check arithmetic helpers for integer overflow.","why":"You added an overflow check by hand in PR #101 for #1.","evidence":["#1","PR #101"]}],"skip_reason":""}`
	twoLesson = `{"lessons":[{"line":"check arithmetic helpers for integer overflow","why":"again","evidence":["#2"]},{"line":"Document every exported function.","why":"Both #1 and #2 were blocked for it.","evidence":["#1","#2"]}],"skip_reason":""}`
)

// built takes issue n through a passing build to its draft PR.
func (e *env) built(t *testing.T, n int) *store.WorkItem {
	t.Helper()
	e.addIssue(n)
	e.claude.builds = append(e.claude.builds, goodBuild)
	e.codex.reviews = append(e.codex.reviews, pass)
	e.poll(t)
	it := e.item(t, n)
	if it.Stage != state.AwaitingHuman {
		t.Fatalf("stage = %s", it.Stage)
	}
	return it
}

// poll makes the next Drain poll GitHub again.
func (e *env) poll(t *testing.T) {
	t.Helper()
	e.p.lastPoll = e.p.lastPoll.AddDate(0, 0, -1)
	if err := e.p.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// humanPush commits a change to branch on the remote, as a person would,
// and returns the new head.
func (e *env) humanPush(t *testing.T, branch, file, body string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "human")
	git(t, filepath.Dir(dir), "clone", "-q", "-b", branch, e.remote, dir)
	writeFile(dir, file, body)
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-qm", "human fix")
	git(t, dir, "push", "-q", "origin", branch)
	return git(t, dir, "rev-parse", "HEAD")
}

func (e *env) merge(branch, head string) {
	e.gh.prs[branch].Merged = true
	e.gh.prs[branch].State = "closed"
	e.gh.prs[branch].Head.SHA = head
}

// lessonsMD is AGENTS.md on the lessons branch, without its last newline.
func (e *env) lessonsMD(t *testing.T) string {
	t.Helper()
	return git(t, e.remote, "show", LessonsBranch+":AGENTS.md")
}

// A human's change to a factory PR is learned from: the diff is kept, a
// learn job runs, and its lesson lands in AGENTS.md in a draft PR that
// waits for a human even with auto_merge on. A second run adds to it.
func TestHumanEditBecomesALessonsPR(t *testing.T) {
	ctx := context.Background()
	e := setupWith(t, map[string]string{"AGENTS.md": "# Agents\n\nRun the tests.\n\n## Style\n\nUse gofmt.\n"})
	e.p.Cfg.AutoMerge = &config.AutoMerge{UpTo: "medium", MaxSize: "L"}
	var notes []string
	e.p.Notify = func(_ context.Context, m string) { notes = append(notes, m) }
	it := e.built(t, 1)
	head := e.humanPush(t, "factory/issue-1", "add.go", "package fixture\n\n// Add adds, and panics on overflow.\nfunc Add(a, b int) int { return a + b }\n")
	e.merge("factory/issue-1", head)
	e.claude.learn = []string{oneLesson}
	e.poll(t)

	if e.item(t, 1).Stage != state.Done {
		t.Fatalf("stage = %s", e.item(t, 1).Stage)
	}
	jobs, _ := e.p.Store.ListJobs(ctx, store.JobFilter{Kind: "learn"})
	if len(jobs) != 1 || jobs[0].State != state.Succeeded || jobs[0].Model != "sonnet" || jobs[0].BudgetUSD != 0.5 {
		t.Fatalf("learn jobs = %+v", jobs)
	}
	sigs, _ := e.p.Store.SignalsByID(ctx, []int64{1})
	if len(sigs) != 1 || sigs[0].Kind != store.SignalHumanEdit || sigs[0].ItemID != it.ID || sigs[0].PRNumber != it.PRNumber ||
		!strings.Contains(sigs[0].Diff, "+// Add adds, and panics on overflow.") || sigs[0].LearnJob != jobs[0].ID {
		t.Fatalf("signals = %+v", sigs)
	}
	if p := e.claude.prompts["learn"][0]; !strings.Contains(p, "panics on overflow") || !strings.Contains(p, "Use gofmt.") || !strings.Contains(p, "PR #101 for #1") {
		t.Fatalf("learn prompt:\n%s", p)
	}
	md := e.lessonsMD(t)
	if want := "## Style\n\nUse gofmt.\n\n## Lessons from the factory\n\n- Check arithmetic helpers for integer overflow."; !strings.HasSuffix(md, want) {
		t.Fatalf("AGENTS.md:\n%s", md)
	}
	pr := e.gh.prs[LessonsBranch]
	if pr == nil || !pr.Draft || !hasLabel(e.gh.prLabels[pr.Number], LabelAwaitingYou) {
		t.Fatalf("lessons PR = %+v, labels %v", pr, e.gh.prLabels)
	}
	body := e.gh.bodies[pr.Number]
	for _, want := range []string{"- **Check arithmetic helpers for integer overflow.**", "[#1](https://github.com/o/r/issues/1)", "[PR 101](https://github.com/o/r/pull/101)", lessonsFooter} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q:\n%s", want, body)
		}
	}
	if _, ok := e.gh.autoMerged[pr.Number]; ok || hasLabel(e.gh.prLabels[pr.Number], LabelAutoMerge) {
		t.Fatal("the lessons PR must never auto-merge")
	}
	if !strings.Contains(notes[len(notes)-1], "Opened a lessons PR with 1 lesson for AGENTS.md") {
		t.Fatalf("notes = %q", notes)
	}

	// The same day, nothing new runs.
	e.poll(t)
	if e.claude.calls["learn"] != 1 {
		t.Fatalf("learn ran %d times in a day", e.claude.calls["learn"])
	}

	// Tomorrow, a second edit: the repeated line is skipped, the new one
	// joins the same PR.
	it2 := e.built(t, 2)
	head = e.humanPush(t, "factory/issue-2", "add.go", "package fixture\n\n// Add adds two ints.\nfunc Add(a, b int) int { return a + b }\n")
	e.merge("factory/issue-2", head)
	e.claude.learn = append(e.claude.learn, twoLesson)
	forgetDay(t, e)
	notes = nil
	e.poll(t)
	if e.claude.calls["learn"] != 2 || e.gh.prs[LessonsBranch].Number != pr.Number {
		t.Fatalf("learn calls %d, PR %+v", e.claude.calls["learn"], e.gh.prs[LessonsBranch])
	}
	md = e.lessonsMD(t)
	if strings.Count(strings.ToLower(md), "integer overflow") != 1 || !strings.HasSuffix(md, "overflow.\n- Document every exported function.") {
		t.Fatalf("AGENTS.md:\n%s", md)
	}
	body = e.gh.bodies[pr.Number]
	if !strings.Contains(body, "- **Document every exported function.**") || strings.Index(body, "Document every") > strings.Index(body, lessonsFooter) ||
		strings.Count(body, "integer overflow") != 1 {
		t.Fatalf("body:\n%s", body)
	}
	if len(notes) != 2 || !strings.Contains(notes[1], "Added 1 lesson for AGENTS.md to the lessons PR") {
		t.Fatalf("notes = %q", notes)
	}
	if e.item(t, 2).ID != it2.ID || e.item(t, 2).Stage != state.Done {
		t.Fatalf("item 2 = %+v", e.item(t, 2))
	}
}

// forgetDay lets the learn loop run again, as if a day had passed.
func forgetDay(t *testing.T, e *env) {
	t.Helper()
	learnDay = func() string { return "tomorrow" }
	t.Cleanup(func() { learnDay = store.Today })
}

// Blocking findings that repeat across items start a learn job; the same
// finding in one item, or with learning off, doesn't.
func TestRepeatFindingsTriggerLearn(t *testing.T) {
	for _, c := range []struct {
		name  string
		items int
		edit  bool // a human changes item 1's PR and merges it
		off   bool
		learn bool
	}{{"two items", 2, false, false, true}, {"one item", 1, false, false, false}, {"one item and an edit", 1, true, false, true}, {"off", 2, true, true, false}} {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			e := setup(t)
			if c.off {
				e.p.Cfg.Learning.Enabled = new(bool)
			}
			round := 0
			build := func(w string) error {
				round++
				writeFile(w, "add.go", fmt.Sprintf("package fixture\n\n// Add adds (round %d).\nfunc Add(a, b int) int { return a + b }\n", round))
				writeFile(w, "add_test.go", "package fixture\n")
				return commitIn(w, "build")
			}
			e.claude.learn = []string{`{"lessons":[],"skip_reason":"one-off fixes"}`}
			// One item at a time, so each is blocked once, then passes.
			for n := 1; n <= c.items; n++ {
				e.addIssue(n)
				e.claude.builds = append(e.claude.builds, build, build)
				e.codex.reviews = append(e.codex.reviews, block, pass)
				e.poll(t)
			}
			if c.edit {
				e.merge("factory/issue-1", e.humanPush(t, "factory/issue-1", "add.go", "package fixture\n\n// Add adds, checked.\nfunc Add(a, b int) int { return a + b }\n"))
			}
			e.poll(t)
			for n := 1; n <= c.items; n++ {
				if st := e.item(t, n).Stage; st != state.AwaitingHuman && !c.edit {
					t.Fatalf("item %d stage = %s", n, st)
				}
			}
			sigs, _ := e.p.Store.UnusedSignals(ctx, "o/r", "")
			jobs, _ := e.p.Store.ListJobs(ctx, store.JobFilter{Kind: "learn"})
			if c.learn {
				// A lone finding waits, unused, for one like it; the edit runs alone.
				alike, waiting := c.items, 0
				if c.edit {
					alike, waiting = 0, c.items
				}
				if len(jobs) != 1 || jobs[0].State != state.Succeeded || len(sigs) != waiting {
					t.Fatalf("jobs = %+v, unused = %+v", jobs, sigs)
				}
				p := e.claude.prompts["learn"][0]
				if strings.Contains(p, "group 1") != !c.edit || strings.Count(p, "overflow unhandled") != alike || c.edit != strings.Contains(p, "+// Add adds, checked.") {
					t.Fatalf("learn prompt:\n%s", p)
				}
				if e.gh.prs[LessonsBranch] != nil {
					t.Fatal("no lessons, no PR")
				}
				return
			}
			if len(jobs) != 0 {
				t.Fatalf("learn jobs = %+v", jobs)
			}
			want := c.items
			if c.off {
				want = 0
			}
			if len(sigs) != want {
				t.Fatalf("signals = %+v", sigs)
			}
		})
	}
}

// The lessons section is appended to, or made, without touching the rest.
func TestAppendLessons(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"", "## Lessons from the factory\n\n- A.\n- B.\n"},
		{"# Agents\n\nRun tests.\n", "# Agents\n\nRun tests.\n\n## Lessons from the factory\n\n- A.\n- B.\n"},
		{"# X\n\n## Lessons from the factory\n\n- Old.\n\n## More\n\nText\n", "# X\n\n## Lessons from the factory\n\n- Old.\n- A.\n- B.\n\n## More\n\nText\n"},
		{"## Lessons from the factory\n## Next\n", "## Lessons from the factory\n\n- A.\n- B.\n\n## Next\n"},
		{"```\n## Lessons from the factory\n```\n", "```\n## Lessons from the factory\n```\n\n## Lessons from the factory\n\n- A.\n- B.\n"},
	} {
		if got := appendLessons(c.in, []string{"A.", "B."}); got != c.want {
			t.Errorf("appendLessons(%q) =\n%q\nwant\n%q", c.in, got, c.want)
		}
	}
}

// Findings group by file glob or alike claims.
func TestGroupFindings(t *testing.T) {
	f := func(item int64, file, claim string) store.Signal {
		return store.Signal{Kind: store.SignalFinding, ItemID: item, File: file, Claim: claim}
	}
	sigs := []store.Signal{
		f(1, "internal/a/x.go", "missing error check"),
		f(2, "internal/a/y.go", "something else entirely"),
		f(3, "web/app.ts", "Errors are not checked: missing error checks"),
		f(4, "docs/readme.md", "typo"),
	}
	gs := groupFindings(sigs)
	if len(gs) != 2 || len(gs[0]) != 3 || len(gs[1]) != 1 {
		t.Fatalf("groups = %+v", gs)
	}
	if got := learnable(sigs, 2); len(got) != 3 {
		t.Fatalf("learnable = %+v", got)
	}
	if got := learnable(sigs, 4); len(got) != 0 {
		t.Fatalf("min 4: %+v", got)
	}
	// One item's findings alone are no repeat.
	if got := learnable([]store.Signal{f(1, "a.go", "x"), f(1, "b.go", "y")}, 2); len(got) != 0 {
		t.Fatalf("one item: %+v", got)
	}
}

// A long diff is cut at whole hunks.
func TestCapDiff(t *testing.T) {
	hunk := func(n int) string { return fmt.Sprintf("@@ -%d +%d @@\n%s", n, n, strings.Repeat("+x\n", 40)) }
	diff := "diff --git a/a b/a\n--- a/a\n+++ b/a\n" + hunk(1) + hunk(2) + "diff --git a/b b/b\n--- a/b\n+++ b/b\n" + hunk(3)
	got := capDiff(diff, 200)
	if !strings.Contains(got, "@@ -1 +1 @@") || strings.Contains(got, "@@ -2") || !strings.Contains(got, "2 more hunks left out") {
		t.Fatalf("capped:\n%s", got)
	}
	if capDiff(diff, len(diff)) != diff {
		t.Fatal("a diff under the cap is kept whole")
	}
}

// A line you take out of the lessons PR, or whose PR you close without
// merging, is remembered as declined and never proposed again.
func TestDeclinedLessonsAreNotProposedAgain(t *testing.T) {
	ctx := context.Background()
	e := setupWith(t, map[string]string{"AGENTS.md": "# Agents\n\nRun the tests.\n"})
	lesson := func(lines ...string) string {
		var ls []string
		for _, l := range lines {
			ls = append(ls, fmt.Sprintf(`{"line":%q,"why":"seen in #1","evidence":["#1"]}`, l))
		}
		return `{"lessons":[` + strings.Join(ls, ",") + `],"skip_reason":""}`
	}
	edit := func(n int, learn string) {
		t.Helper()
		e.built(t, n)
		e.merge(fmt.Sprintf("factory/issue-%d", n), e.humanPush(t, fmt.Sprintf("factory/issue-%d", n), "add.go", fmt.Sprintf("package fixture\n\n// Add adds (edit %d).\nfunc Add(a, b int) int { return a + b }\n", n)))
		e.claude.learn = append(e.claude.learn, learn)
		learnDay = func() string { return fmt.Sprintf("day %d", n) }
		e.poll(t)
	}
	t.Cleanup(func() { learnDay = store.Today })

	edit(1, lesson("Check for overflow.", "Document exported functions."))
	first := e.gh.prs[LessonsBranch].Number
	// You take one line out of the open PR.
	md := strings.Replace(e.lessonsMD(t), "\n- Document exported functions.", "", 1)
	e.humanPush(t, LessonsBranch, "AGENTS.md", md+"\n")

	edit(2, lesson("document exported functions", "Run go vet."))
	if p := e.claude.prompts["learn"][1]; !strings.Contains(p, "## Lines a human declined") || !strings.Contains(p, "- Document exported functions.") {
		t.Fatalf("learn prompt:\n%s", p)
	}
	if md := e.lessonsMD(t); !strings.HasSuffix(md, "- Check for overflow.\n- Run go vet.") || strings.Contains(strings.ToLower(md), "document") {
		t.Fatalf("AGENTS.md:\n%s", md)
	}

	// You close the PR without merging: its lines are declined too, and the
	// next lessons start a fresh PR from base.
	e.gh.prs[LessonsBranch].State = "closed"
	edit(3, lesson("CHECK FOR OVERFLOW", "Name tests after behavior."))
	pr := e.gh.prs[LessonsBranch]
	if pr.Number == first || pr.State != "open" {
		t.Fatalf("lessons PR = %+v", pr)
	}
	if md := e.lessonsMD(t); !strings.HasSuffix(md, "## Lessons from the factory\n\n- Name tests after behavior.") || strings.Contains(md, "overflow") || strings.Contains(md, "vet") {
		t.Fatalf("AGENTS.md:\n%s", md)
	}
	ls, _ := e.p.Store.Lessons(ctx, "o/r")
	got := map[string]string{}
	for _, l := range ls {
		got[l.Line] = l.Status
	}
	want := map[string]string{"Check for overflow.": "declined", "Document exported functions.": "declined", "Run go vet.": "declined", "Name tests after behavior.": "proposed"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("lessons = %v", got)
	}
}
