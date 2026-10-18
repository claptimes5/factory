package pipeline

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/claptimes5/factory/internal/config"
	"github.com/claptimes5/factory/internal/state"
	"github.com/claptimes5/factory/internal/store"
	"github.com/claptimes5/factory/internal/verdict"
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
	// Claude builds here and reads CLAUDE.md, so the PR adds one that loads AGENTS.md.
	if got := git(t, e.remote, "show", LessonsBranch+":CLAUDE.md"); got != "@AGENTS.md" {
		t.Fatalf("CLAUDE.md = %q", got)
	}
	pr := e.gh.prs[LessonsBranch]
	if pr == nil || !pr.Draft || !hasLabel(e.gh.prLabels[pr.Number], LabelAwaitingYou) || pr.Title != "Lessons from the factory for AGENTS.md" {
		t.Fatalf("lessons PR = %+v, labels %v", pr, e.gh.prLabels)
	}
	body := e.gh.bodies[pr.Number]
	for _, want := range []string{"```text\nCheck arithmetic helpers for integer overflow.\n\nWhy: You added an overflow check by hand in PR #101 for #1.\n```\n",
		"From [#1](https://github.com/o/r/issues/1), [PR 101](https://github.com/o/r/pull/101).", lessonsFooter} {
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
	if !strings.Contains(body, "```text\nDocument every exported function.") || strings.Index(body, "Document every") > strings.Index(body, lessonsFooter) ||
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
				e.codex.reviews = append(e.codex.reviews, block, resolveF1)
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
	if got, _ := learnable(sigs, 2); len(got) != 3 {
		t.Fatalf("learnable = %+v", got)
	}
	if got, _ := learnable(sigs, 4); len(got) != 0 {
		t.Fatalf("min 4: %+v", got)
	}
	// One item's findings alone are no repeat.
	if got, _ := learnable([]store.Signal{f(1, "a.go", "x"), f(1, "b.go", "y")}, 2); len(got) != 0 {
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
	if p := e.claude.prompts["learn"][1]; !strings.Contains(p, "## Lines a human declined") || !strings.Contains(p, "again.\n\n```\n- Document exported functions.\n```") ||
		!strings.Contains(p, "open lessons PR\n\n```\n- Check for overflow.\n```") {
		t.Fatalf("learn prompt:\n%s", p)
	}
	if md := e.lessonsMD(t); !strings.HasSuffix(md, "- Check for overflow.\n- Run go vet.") || strings.Contains(strings.ToLower(md), "document") {
		t.Fatalf("AGENTS.md:\n%s", md)
	}

	// You close the PR without merging: its lines are declined too, and the
	// next lessons start a fresh PR from base. Your commit on the old
	// branch isn't pushed over: the new PR has a branch of its own.
	e.gh.prs[LessonsBranch].State = "closed"
	before := git(t, e.remote, "rev-parse", LessonsBranch)
	edit(3, lesson("CHECK FOR OVERFLOW", "Name tests after behavior."))
	pr := e.gh.prs[LessonsBranch+"-2"]
	if pr == nil || pr.Number == first || pr.State != "open" || git(t, e.remote, "rev-parse", LessonsBranch) != before {
		t.Fatalf("lessons PR = %+v", pr)
	}
	if md := git(t, e.remote, "show", LessonsBranch+"-2:AGENTS.md"); !strings.HasSuffix(md, "## Lessons from the factory\n\n- Name tests after behavior.") || strings.Contains(md, "overflow") || strings.Contains(md, "vet") {
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

// clone checks out branch of the remote, as a person would, with a name
// git commits under.
func (e *env) clone(t *testing.T, branch string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "human")
	git(t, filepath.Dir(dir), "clone", "-q", "-b", branch, e.remote, dir)
	return dir
}

// edit is one human edit signal with its diff already read.
func (e *env) editSignal(t *testing.T, it *store.WorkItem, pr int) {
	t.Helper()
	if err := e.p.Store.AddSignal(context.Background(), store.Signal{Repo: "o/r", Kind: store.SignalHumanEdit, ItemID: it.ID, IssueNumber: it.IssueNumber,
		PRNumber: pr, Diff: fmt.Sprintf("+edit %d\n", pr)}); err != nil {
		t.Fatal(err)
	}
}

// A repo has one learn job queued or running at a time: a second would
// push over the first's lines.
func TestOneLearnJobAtATime(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	it := e.built(t, 1)
	e.editSignal(t, it, 201)
	e.p.learnTriggers(ctx)
	forgetDay(t, e)
	e.editSignal(t, it, 202)
	e.p.learnTriggers(ctx)
	if jobs, _ := e.p.Store.ListJobs(ctx, store.JobFilter{Kind: "learn"}); len(jobs) != 1 {
		t.Fatalf("learn jobs = %+v", jobs)
	}
	if _, err := e.p.Store.EnqueueJob(ctx, store.Job{Kind: "learn", Harness: "claude", Model: "sonnet", BudgetUSD: 1, Input: `{"repo":"o/other"}`}); err != nil {
		t.Fatal(err)
	}
	j, err := e.p.claimNext(ctx)
	if err != nil || j == nil || j.Kind != "learn" {
		t.Fatalf("first = %+v, %v", j, err)
	}
	if j, err := e.p.claimNext(ctx); err != nil || j != nil {
		t.Fatalf("a second learn job ran alongside: %+v, %v", j, err)
	}
}

// pushSymlink commits a symlink to the remote's main.
func (e *env) pushSymlink(t *testing.T, name, target string) {
	t.Helper()
	dir := e.clone(t, "main")
	if err := os.Symlink(target, filepath.Join(dir, name)); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-qm", "link "+name)
	git(t, dir, "push", "-q", "origin", "main")
}

// An AGENTS.md the factory won't write through is found before the agent
// runs: no agent is paid for, the signals are set aside, and you hear it
// once, not every day.
func TestRefusedLessonsCostNothing(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	e.pushSymlink(t, "AGENTS.md", "../outside.md")
	var notes []string
	e.p.Notify = func(_ context.Context, m string) { notes = append(notes, m) }
	for n := 1; n <= 2; n++ {
		e.built(t, n)
		branch := fmt.Sprintf("factory/issue-%d", n)
		e.merge(branch, e.humanPush(t, branch, "add.go", fmt.Sprintf("package fixture\n\n// Add adds (edit %d).\nfunc Add(a, b int) int { return a + b }\n", n)))
		learnDay = func() string { return fmt.Sprintf("day %d", n) }
		e.poll(t)
	}
	t.Cleanup(func() { learnDay = store.Today })
	jobs, _ := e.p.Store.ListJobs(ctx, store.JobFilter{Kind: "learn"})
	if len(jobs) != 2 || jobs[0].State != state.Failed || !strings.Contains(jobs[0].Error, "symlink out of the repo") {
		t.Fatalf("learn jobs = %+v", jobs)
	}
	if e.claude.calls["learn"] != 0 {
		t.Fatalf("the agent ran %d times", e.claude.calls["learn"])
	}
	if sigs, _ := e.p.Store.UnusedSignals(ctx, "o/r", ""); len(sigs) != 0 {
		t.Fatalf("unused = %+v", sigs)
	}
	told := 0
	for _, n := range notes {
		if strings.Contains(n, "Lessons for o/r can't be proposed") {
			told++
		}
	}
	if told != 1 {
		t.Fatalf("told %d times: %q", told, notes)
	}
}

// Lessons go to the file builders read: AGENTS.md or what it links to in
// the repo, CLAUDE.md for Claude unless it loads AGENTS.md, and a new
// AGENTS.md (with a CLAUDE.md loading it, for Claude) when there's none.
func TestLessonsTarget(t *testing.T) {
	ctx := context.Background()
	for name, c := range map[string]struct {
		files  map[string]string // a value of "-> x" is a symlink to x
		claude bool
		file   string
		imp    string
		refuse string
	}{
		"agents, claude":           {map[string]string{"AGENTS.md": "a"}, true, "AGENTS.md", "CLAUDE.md", ""},
		"agents, codex":            {map[string]string{"AGENTS.md": "a"}, false, "AGENTS.md", "", ""},
		"claude only":              {map[string]string{"CLAUDE.md": "c"}, true, "AGENTS.md", "CLAUDE.md", ""},
		"claude only, codex":       {map[string]string{"CLAUDE.md": "c"}, false, "AGENTS.md", "", ""},
		"both, claude":             {map[string]string{"AGENTS.md": "a", "CLAUDE.md": "c"}, true, "AGENTS.md", "CLAUDE.md", ""},
		"both, codex":              {map[string]string{"AGENTS.md": "a", "CLAUDE.md": "c"}, false, "AGENTS.md", "", ""},
		"claude loads agents":      {map[string]string{"AGENTS.md": "a", "CLAUDE.md": "# C\n\n@AGENTS.md\n"}, true, "AGENTS.md", "", ""},
		"claude links elsewhere":   {map[string]string{"CLAUDE.md": "-> docs/claude.md", "docs/claude.md": "c"}, true, "AGENTS.md", "docs/claude.md", ""},
		"agents links to claude":   {map[string]string{"AGENTS.md": "-> CLAUDE.md", "CLAUDE.md": "c"}, true, "CLAUDE.md", "", ""},
		"claude links to agents":   {map[string]string{"CLAUDE.md": "-> AGENTS.md", "AGENTS.md": "a"}, true, "AGENTS.md", "", ""},
		"agents links in the repo": {map[string]string{"AGENTS.md": "-> docs/agents.md", "docs/agents.md": "a"}, false, "docs/agents.md", "", ""},
		"neither, claude":          {nil, true, "AGENTS.md", "CLAUDE.md", ""},
		"neither, codex":           {nil, false, "AGENTS.md", "", ""},
		"link out":                 {map[string]string{"AGENTS.md": "-> ../x.md"}, true, "", "", "symlink out of the repo"},
		"link absolute":            {map[string]string{"AGENTS.md": "-> /etc/passwd"}, true, "", "", "symlink out of the repo"},
		"link to nothing":          {map[string]string{"AGENTS.md": "-> gone.md"}, true, "", "", "isn't a plain file tracked"},
		"link to a folder":         {map[string]string{"AGENTS.md": "-> docs", "docs/x.md": "x"}, true, "", "", "isn't a plain file tracked"},
		"claude links out":         {map[string]string{"AGENTS.md": "a", "CLAUDE.md": "-> ../../c"}, true, "", "", "symlink out of the repo"},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			git(t, dir, "init", "-q", "-b", "main")
			writeFile(dir, "go.mod", "module x\n")
			for f, body := range c.files {
				if target, ok := strings.CutPrefix(body, "-> "); ok {
					os.MkdirAll(filepath.Dir(filepath.Join(dir, f)), 0o755)
					if err := os.Symlink(target, filepath.Join(dir, f)); err != nil {
						t.Fatal(err)
					}
				} else {
					writeFile(dir, f, body)
				}
			}
			git(t, dir, "add", "-A")
			git(t, dir, "commit", "-qm", "x")
			file, imp, err := lessonsTarget(ctx, &Git{}, dir, "HEAD", c.claude)
			if c.refuse != "" {
				if r := (refusal{}); !errors.As(err, &r) || !strings.Contains(err.Error(), c.refuse) {
					t.Fatalf("err = %v, want a refusal with %q", err, c.refuse)
				}
				return
			}
			if err != nil || file != c.file || imp != c.imp {
				t.Fatalf("= %s %q %v, want %s %q", file, imp, err, c.file, c.imp)
			}
		})
	}
}

// A repo whose AGENTS.md links to its CLAUDE.md gets its lessons in
// CLAUDE.md, where Claude builders read them, with the link kept.
func TestLessonsFollowTheLink(t *testing.T) {
	e := setupWith(t, map[string]string{"CLAUDE.md": "# Claude\n\nRun the tests.\n"})
	e.pushSymlink(t, "AGENTS.md", "CLAUDE.md")
	e.built(t, 1)
	e.merge("factory/issue-1", e.humanPush(t, "factory/issue-1", "add.go", "package fixture\n\n// Add adds, checked.\nfunc Add(a, b int) int { return a + b }\n"))
	e.claude.learn = []string{oneLesson}
	e.poll(t)
	if md := git(t, e.remote, "show", LessonsBranch+":CLAUDE.md"); !strings.HasSuffix(md, "## Lessons from the factory\n\n- Check arithmetic helpers for integer overflow.") {
		t.Fatalf("CLAUDE.md:\n%s", md)
	}
	if mode := git(t, e.remote, "ls-tree", LessonsBranch, "AGENTS.md"); !strings.HasPrefix(mode, "120000 ") {
		t.Fatalf("AGENTS.md is no longer a link: %s", mode)
	}
	if pr := e.gh.prs[LessonsBranch]; pr == nil || pr.Title != "Lessons from the factory for CLAUDE.md" {
		t.Fatalf("lessons PR = %+v", pr)
	}
	if p := e.claude.prompts["learn"][0]; !strings.Contains(p, "read the repository's `CLAUDE.md`") || strings.Count(p, "Run the tests.") != 1 {
		t.Fatalf("learn prompt:\n%s", p)
	}
}

// A repo with its own CLAUDE.md gets its lessons in AGENTS.md, and its
// CLAUDE.md keeps what it says and gains a line loading AGENTS.md.
func TestLessonsLoadedFromClaudeMD(t *testing.T) {
	e := setupWith(t, map[string]string{"CLAUDE.md": "# Claude\n\nRun the tests.\n"})
	e.built(t, 1)
	e.merge("factory/issue-1", e.humanPush(t, "factory/issue-1", "add.go", "package fixture\n\n// Add adds, checked.\nfunc Add(a, b int) int { return a + b }\n"))
	e.claude.learn = []string{oneLesson}
	e.poll(t)
	if md := git(t, e.remote, "show", LessonsBranch+":AGENTS.md"); !strings.Contains(md, "- Check arithmetic helpers for integer overflow.") {
		t.Fatalf("AGENTS.md:\n%s", md)
	}
	if md := git(t, e.remote, "show", LessonsBranch+":CLAUDE.md"); md != "# Claude\n\nRun the tests.\n\n@AGENTS.md" {
		t.Fatalf("CLAUDE.md:\n%s", md)
	}
}

// learnFrom has a human edit item n's PR, then runs the day's learn job
// with the scripted lessons.
func (e *env) learnFrom(t *testing.T, n int, lessons ...string) {
	t.Helper()
	var ls []string
	for _, l := range lessons {
		ls = append(ls, fmt.Sprintf(`{"line":%q,"why":"seen in #1","evidence":["#1"]}`, l))
	}
	e.built(t, n)
	e.merge(fmt.Sprintf("factory/issue-%d", n), e.humanPush(t, fmt.Sprintf("factory/issue-%d", n), "add.go", fmt.Sprintf("package fixture\n\n// Add adds (edit %d).\nfunc Add(a, b int) int { return a + b }\n", n)))
	e.claude.learn = append(e.claude.learn, `{"lessons":[`+strings.Join(ls, ",")+`],"skip_reason":""}`)
	learnDay = func() string { return fmt.Sprintf("day %d", n) }
	t.Cleanup(func() { learnDay = store.Today })
	e.poll(t)
}

// A lessons PR opened before lessons moved to AGENTS.md has its lines in
// CLAUDE.md: they stay proposed, not declined, and aren't proposed again.
// The PR says why it changes CLAUDE.md, and adds the import once.
func TestLessonsInClaudeMDOnAnOpenPRStayProposed(t *testing.T) {
	ctx := context.Background()
	e := setupWith(t, map[string]string{"AGENTS.md": "# Agents\n", "CLAUDE.md": "# Claude\n"})
	e.learnFrom(t, 1, "Check for overflow.")
	if pr := e.gh.prs[LessonsBranch]; pr == nil || !strings.Contains(pr.Body, "adds an `@AGENTS.md` line to `CLAUDE.md`") {
		t.Fatalf("lessons PR = %+v", pr)
	}
	// The line moves to CLAUDE.md, as the old code put it.
	e.humanPush(t, LessonsBranch, "AGENTS.md", "# Agents\n")
	e.humanPush(t, LessonsBranch, "CLAUDE.md", "# Claude\n\n@AGENTS.md\n\n## Lessons from the factory\n\n- Check for overflow.\n")
	e.learnFrom(t, 2, "Check for overflow.", "Run go vet.")
	ls, _ := e.p.Store.Lessons(ctx, "o/r")
	for _, l := range ls {
		if l.Status != store.LessonProposed {
			t.Errorf("%s is %s", l.Line, l.Status)
		}
	}
	if md := e.lessonsMD(t); strings.Contains(md, "overflow") || !strings.Contains(md, "- Run go vet.") {
		t.Fatalf("AGENTS.md:\n%s", md)
	}
	if md := git(t, e.remote, "show", LessonsBranch+":CLAUDE.md"); strings.Count(md, "@AGENTS.md") != 1 {
		t.Fatalf("CLAUDE.md:\n%s", md)
	}
}

// An import added on base since the lessons PR opened isn't added again
// on the PR's branch.
func TestLessonsImportAddedOnBaseIsNotRepeated(t *testing.T) {
	e := setupWith(t, map[string]string{"AGENTS.md": "# Agents\n", "CLAUDE.md": "# Claude\n"})
	e.learnFrom(t, 1, "Check for overflow.")
	e.humanPush(t, LessonsBranch, "CLAUDE.md", "# Claude\n")
	e.humanPush(t, "main", "CLAUDE.md", "# Claude\n\n@AGENTS.md\n")
	e.learnFrom(t, 2, "Run go vet.")
	if md := git(t, e.remote, "show", LessonsBranch+":CLAUDE.md"); md != "# Claude" {
		t.Fatalf("CLAUDE.md on the branch:\n%s", md)
	}
}

// The file is written through an os.Root, never through a symlink, which
// on a case-insensitive disk could stand in for AGENTS.md unseen by git.
func TestWritePlain(t *testing.T) {
	dir, outside := t.TempDir(), filepath.Join(t.TempDir(), "outside.md")
	writeFile(filepath.Dir(outside), "outside.md", "keep\n")
	if err := os.Symlink(outside, filepath.Join(dir, "AGENTS.md")); err != nil {
		t.Fatal(err)
	}
	writeFile(dir, "CLAUDE.md", "old\n")
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := writePlain(root, "AGENTS.md", []byte("x\n")); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("through a symlink: %v", err)
	}
	if b, _ := os.ReadFile(outside); string(b) != "keep\n" {
		t.Fatalf("outside = %q", b)
	}
	if err := checkWritable(dir, &lessonsPlan{File: "AGENTS.md"}); err == nil {
		t.Fatal("checkWritable passed a symlink")
	}
	for _, f := range []string{"CLAUDE.md", "NEW.md"} {
		if err := writePlain(root, f, []byte("new\n")); err != nil {
			t.Fatal(err)
		}
		if b, _ := os.ReadFile(filepath.Join(dir, f)); string(b) != "new\n" {
			t.Fatalf("%s = %q", f, b)
		}
	}
}

// Model text in the PR body is fenced, so a "fixed #12" or an @mention in
// it acts on nothing, and a long why is cut.
func TestLessonEntriesAreFenced(t *testing.T) {
	e := setup(t)
	why := "You fixed #12 by hand, cc @bob. " + strings.Repeat("More. ", 300)
	got := e.p.lessonEntries("o/r", []verdict.Lesson{{Line: "Check for overflow.", Why: why, Evidence: []string{"#1"}}}, []store.Signal{{IssueNumber: 1}})
	if !strings.HasPrefix(got, "```text\nCheck for overflow.\n\nWhy: You fixed #12 by hand, cc @bob. More.") || !strings.Contains(got, "\n```\nFrom [#1](https://github.com/o/r/issues/1).\n") {
		t.Fatalf("entries:\n%s", got)
	}
	if len(got) > maxWhy+200 {
		t.Fatalf("why not cut: %d bytes", len(got))
	}
}

// What a human changed is their own commits: not what an "Update branch"
// merge or a rebase brought in from base, nor the factory's work rebased.
func TestHumanDiffIsTheHumansOwn(t *testing.T) {
	for _, how := range []string{"update branch", "rebase"} {
		t.Run(how, func(t *testing.T) {
			ctx := context.Background()
			e := setup(t)
			e.built(t, 1)
			// Base moves on.
			main := e.clone(t, "main")
			writeFile(main, "base.go", "package fixture\n\n// Base change.\n")
			git(t, main, "add", "-A")
			git(t, main, "commit", "-qm", "base change")
			git(t, main, "push", "-q", "origin", "main")
			dir := e.clone(t, "factory/issue-1")
			if how == "rebase" {
				git(t, dir, "rebase", "-q", "origin/main")
			} else {
				git(t, dir, "-c", "user.name=t", "-c", "user.email=t@t", "merge", "-q", "--no-edit", "origin/main")
			}
			writeFile(dir, "fix.go", "package fixture\n\n// Human fix.\n")
			git(t, dir, "add", "-A")
			git(t, dir, "commit", "-qm", "human fix")
			git(t, dir, "push", "-q", "-f", "origin", "factory/issue-1")
			head := git(t, dir, "rev-parse", "HEAD")
			if how == "update branch" {
				// Merged with a merge commit, and base moves on again.
				git(t, main, "pull", "-q", "--no-rebase", "origin", "main")
				git(t, main, "fetch", "-q", "origin", "factory/issue-1")
				git(t, main, "-c", "user.name=t", "-c", "user.email=t@t", "merge", "-q", "--no-ff", "--no-edit", head)
				writeFile(main, "later.go", "package fixture\n\n// Later change.\n")
				git(t, main, "add", "-A")
				git(t, main, "commit", "-qm", "later")
				git(t, main, "push", "-q", "origin", "main")
			}
			e.merge("factory/issue-1", head)
			e.claude.learn = []string{`{"lessons":[],"skip_reason":"one-off"}`}
			e.poll(t)
			sigs, _ := e.p.Store.SignalsByID(ctx, []int64{1})
			if len(sigs) != 1 || !strings.Contains(sigs[0].Diff, "+// Human fix.") || strings.Contains(sigs[0].Diff, "Base change") ||
				strings.Contains(sigs[0].Diff, "Later change") || strings.Contains(sigs[0].Diff, "func Add") || sigs[0].LearnJob == 0 {
				t.Fatalf("signal = %+v", sigs)
			}
		})
	}
}

// A merge records only the two commits; the learn job reads the diff, and
// one it can't read now waits for the next run rather than being lost.
func TestHumanEditIsReadLater(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	e.built(t, 1)
	e.merge("factory/issue-1", e.humanPush(t, "factory/issue-1", "add.go", "package fixture\n\n// Add adds, checked.\nfunc Add(a, b int) int { return a + b }\n"))
	learn := e.p.Cfg.Routing["learn"]
	delete(e.p.Cfg.Routing, "learn")
	e.poll(t)
	sigs, _ := e.p.Store.UnusedSignals(ctx, "o/r", "")
	if len(sigs) != 1 || sigs[0].FromSHA == "" || sigs[0].HeadSHA != e.gh.prs["factory/issue-1"].Head.SHA || sigs[0].Diff != "" {
		t.Fatalf("signals = %+v", sigs)
	}
	// The head is gone from the remote: no diff, no agent, and it waits.
	it := e.item(t, 1)
	if err := e.p.Store.AddSignal(ctx, store.Signal{Repo: "o/r", Kind: store.SignalHumanEdit, ItemID: it.ID, PRNumber: 999, FromSHA: sigs[0].FromSHA,
		HeadSHA: strings.Repeat("ab", 20)}); err != nil {
		t.Fatal(err)
	}
	e.p.Cfg.Routing["learn"] = learn
	e.claude.learn = []string{`{"lessons":[],"skip_reason":"one-off"}`}
	e.poll(t)
	if e.claude.calls["learn"] != 1 || !strings.Contains(e.claude.prompts["learn"][0], "+// Add adds, checked.") {
		t.Fatalf("learn calls %d", e.claude.calls["learn"])
	}
	left, _ := e.p.Store.UnusedSignals(ctx, "o/r", "")
	if len(left) != 1 || left[0].PRNumber != 999 {
		t.Fatalf("unused = %+v", left)
	}
}

// Signals the caps leave out are set aside with the rest, so they don't
// start a job on old news the next day.
func TestTrimmedSignalsAreUsed(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	it := e.built(t, 1)
	for pr := 201; pr < 201+maxEdits+2; pr++ {
		e.editSignal(t, it, pr)
	}
	e.claude.learn = []string{`{"lessons":[],"skip_reason":"one-off"}`}
	e.poll(t)
	if p := e.claude.prompts["learn"][0]; strings.Contains(p, "+edit 201") || !strings.Contains(p, "+edit 207") {
		t.Fatalf("learn prompt:\n%s", p)
	}
	if left, _ := e.p.Store.UnusedSignals(ctx, "o/r", ""); len(left) != 0 {
		t.Fatalf("unused = %+v", left)
	}
	var fs []store.Signal
	for i := range maxGroupSize + 2 {
		fs = append(fs, store.Signal{ID: int64(i), Kind: store.SignalFinding, ItemID: int64(i % 2), File: "a/x.go", Claim: "c"})
	}
	use, trimmed := learnable(fs, 2)
	if len(use) != maxGroupSize || len(trimmed) != 2 || trimmed[0].ID != 0 {
		t.Fatalf("use %d, trimmed %+v", len(use), trimmed)
	}
}

// Lines are recorded once pushed, before the PR: a PR that fails to open
// leaves them tracked.
func TestLessonsRecordedBeforeThePR(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	e.built(t, 1)
	e.merge("factory/issue-1", e.humanPush(t, "factory/issue-1", "add.go", "package fixture\n\n// Add adds, checked.\nfunc Add(a, b int) int { return a + b }\n"))
	e.claude.learn = []string{oneLesson}
	e.gh.failCreatePR = 1
	e.poll(t)
	jobs, _ := e.p.Store.ListJobs(ctx, store.JobFilter{Kind: "learn"})
	if len(jobs) != 1 || jobs[0].State != state.Failed {
		t.Fatalf("learn jobs = %+v", jobs)
	}
	ls, _ := e.p.Store.Lessons(ctx, "o/r")
	if len(ls) != 1 || ls[0].PRNumber != 0 || ls[0].Status != store.LessonProposed {
		t.Fatalf("lessons = %+v", ls)
	}
}

// A merged line a human later deletes from the file counts as declined:
// the factory doesn't put it back.
func TestDeletedLessonIsDeclined(t *testing.T) {
	ctx := context.Background()
	e := setupWith(t, map[string]string{"AGENTS.md": "# Agents\n\nRun the tests.\n"})
	lesson := func(line string) string {
		return fmt.Sprintf(`{"lessons":[{"line":%q,"why":"seen in #1","evidence":["#1"]}],"skip_reason":""}`, line)
	}
	edit := func(n int, learn string) {
		t.Helper()
		e.built(t, n)
		branch := fmt.Sprintf("factory/issue-%d", n)
		e.merge(branch, e.humanPush(t, branch, "add.go", fmt.Sprintf("package fixture\n\n// Add adds (edit %d).\nfunc Add(a, b int) int { return a + b }\n", n)))
		e.claude.learn = append(e.claude.learn, learn)
		learnDay = func() string { return fmt.Sprintf("day %d", n) }
		e.poll(t)
	}
	t.Cleanup(func() { learnDay = store.Today })
	edit(1, lesson("Check for overflow."))
	// You merge the lessons PR, then later take the line out.
	main := e.clone(t, "main")
	git(t, main, "-c", "user.name=t", "-c", "user.email=t@t", "merge", "-q", "--no-ff", "--no-edit", "origin/"+LessonsBranch)
	git(t, main, "push", "-q", "origin", "main")
	e.gh.prs[LessonsBranch].Merged, e.gh.prs[LessonsBranch].State = true, "closed"
	edit(2, lesson("Run go vet."))
	md := git(t, e.remote, "show", "main:AGENTS.md")
	e.humanPush(t, "main", "AGENTS.md", strings.Replace(md, "\n- Check for overflow.", "", 1)+"\n")
	// And close the next PR, so the one after starts from base again.
	e.gh.prs[LessonsBranch].State = "closed"
	before := git(t, e.remote, "rev-parse", LessonsBranch)
	edit(3, lesson("Check for overflow."))
	if p := e.claude.prompts["learn"][2]; !strings.Contains(p, "## Lines a human declined") || !strings.Contains(p, "- Check for overflow.") {
		t.Fatalf("learn prompt:\n%s", p)
	}
	ls, _ := e.p.Store.Lessons(ctx, "o/r")
	got := map[string]string{}
	for _, l := range ls {
		got[l.Line] = l.Status
	}
	if got["Check for overflow."] != store.LessonDeclined || got["Run go vet."] != store.LessonDeclined {
		t.Fatalf("lessons = %v", got)
	}
	if git(t, e.remote, "rev-parse", LessonsBranch) != before || e.gh.prs[LessonsBranch].State != "closed" {
		t.Fatal("the deleted line was proposed again")
	}
}
