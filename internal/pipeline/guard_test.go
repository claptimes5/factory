package pipeline

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/claptimes5/factory/internal/config"
	"github.com/claptimes5/factory/internal/github"
	"github.com/claptimes5/factory/internal/metrics"
	"github.com/claptimes5/factory/internal/state"
	"github.com/claptimes5/factory/internal/store"
)

func TestParseRevert(t *testing.T) {
	ours := store.FactoryMerge{Repo: "o/r", PR: 101, MergeSHA: "abcdef1234567890", HeadSHA: "1111111222", Branch: "factory/issue-5", Title: "Fix the parser (#5)"}
	five := store.FactoryMerge{Repo: "o/r", PR: 5, MergeSHA: "5555555555", Title: "Other"}
	open := []store.FactoryMerge{five, ours}
	for name, c := range map[string]struct {
		text, ref string
		want      bool
	}{
		"git revert of the merge commit":    {"Revert \"Merge pull request #101 from o/factory/issue-5\"\n\nThis reverts commit abcdef1234567890, reversing\nchanges made to 0000000.", "", true},
		"git revert by short sha":           {"Undo it\n\nThis reverts commit ABCDEF1.", "", true},
		"git revert of the squash":          {"Revert \"Fix the parser (#5) (#101)\"\n\nThis reverts commit abcdef1234567890.", "", true},
		"GitHub's revert PR, squashed":      {"Revert \"Fix the parser (#5)\" (#102)\n\nReverts o/r#101", "", true},
		"GitHub's revert PR, merged":        {"Merge pull request #102 from o/revert-101-factory/issue-5\n\nRevert \"Fix the parser (#5)\"", "", true},
		"GitHub's revert PR itself":         {"Revert \"Fix the parser (#5)\"\n\nReverts o/r#101", "revert-101-factory/issue-5", true},
		"a revert PR by its branch":         {"Revert \"Fix the parser (#5)\"\n\nIt broke prod.", "revert-101-factory/issue-5", true},
		"revert of a merge, by its subject": {"Revert \"Merge pull request #101 from o/factory/issue-5\"", "", true},
		"by title alone":                    {"Revert \"Fix the parser (#5)\"", "", true},
		"another repo's PR":                 {"Revert stuff\n\nReverts other/repo#101", "", false},
		"a reapply":                         {"Revert \"Revert \"Fix the parser (#5)\"\"\n\nThis reverts commit abcdef1234567890.", "", false},
		"not a revert":                      {"Fix the parser again (#101)\n\nMentions abcdef1234567890", "", false},
		"too short a sha":                   {"x\n\nThis reverts commit abcdef.", "", false},
		// A branch anyone could name so, on a PR that doesn't say it reverts.
		"a branch named like a revert": {"Config tweaks\n\nTidies the config.", "revert-101-config", false},
		"a merge of such a branch":     {"Merge pull request #102 from bob/revert-101-config\n\nConfig tweaks", "", false},
		// On GitHub, !101 is nothing.
		"GitLab's wording on GitHub": {"Undo\n\nThis reverts merge request !101", "", false},
		// A sha that's someone else's: the title doesn't count then.
		"a sha, and our title": {"Revert \"Fix the parser (#5)\"\n\nThis reverts commit 9999999999.", "", false},
	} {
		got := parseRevert("o/r", c.text, c.ref, false).reverted(open, false)
		if (len(got) == 1 && got[0].PR == 101) != c.want || len(got) > 1 {
			t.Errorf("%s: %+v, want ours %v", name, got, c.want)
		}
		// The issue number in the reverted title isn't a PR number.
		if slices.ContainsFunc(got, func(m store.FactoryMerge) bool { return m.PR == 5 }) {
			t.Errorf("%s: matched PR #5, the issue number in the title", name)
		}
	}
	// On GitLab only !N names a merge request: #N is an issue.
	for name, c := range map[string]struct {
		text string
		want bool
	}{
		"GitLab's revert":            {"Revert \"Fix the parser (#5)\"\n\nThis reverts merge request !101", true},
		"GitLab's revert of a merge": {"Revert \"Merge branch 'factory/issue-5' into 'main'\"\n\nThis reverts commit abcdef1234567890", true},
		"a squash, by title":         {"Revert \"Fix the parser (#5) (!101)\"", true},
		"an issue's number":          {"Undo\n\nReverts g/p#101", false},
		"a revert-N- branch's merge": {"Merge pull request #102 from o/revert-101-x\n\nRevert \"y\"", false},
	} {
		got := parseRevert("gitlab.com/g/p", c.text, "revert-101-x", true).reverted(open, true)
		if (len(got) == 1 && got[0].PR == 101) != c.want {
			t.Errorf("GitLab, %s: %+v, want %v", name, got, c.want)
		}
	}
}

func TestRevertMatchesOnlyTheMergeItNames(t *testing.T) {
	// Each rebuild of issue 5 opened a PR with the same title.
	first := store.FactoryMerge{Repo: "o/r", PR: 34, MergeSHA: "3434343434", Title: "Fix the parser (#5)"}
	second := store.FactoryMerge{Repo: "o/r", PR: 50, MergeSHA: "5050505050", Title: "Fix the parser (#5)"}
	open := []store.FactoryMerge{first, second}
	// GitHub's Revert button on #50: its number, not the title, says which.
	got := parseRevert("o/r", "Revert \"Fix the parser (#5)\"\n\nReverts o/r#50", "revert-50-factory/issue-5", false).reverted(open, false)
	if len(got) != 1 || got[0].PR != 50 {
		t.Fatalf("GitHub revert of #50 matched %+v", got)
	}
	got = parseRevert("o/r", "Revert \"Fix the parser (#5)\"\n\nThis reverts commit 3434343434.", "", false).reverted(open, false)
	if len(got) != 1 || got[0].PR != 34 {
		t.Fatalf("git revert of #34's commit matched %+v", got)
	}
	// GitLab's squash commit, beside its merge commit, names it too.
	sq := store.FactoryMerge{Repo: "g/p", PR: 7, MergeSHA: "7070707070", SquashSHA: "5a5a5a5a5a", Title: "Seven"}
	if got := parseRevert("g/p", "Revert \"Seven\"\n\nThis reverts commit 5a5a5a5a5a.", "", true).reverted([]store.FactoryMerge{sq}, true); len(got) != 1 {
		t.Fatalf("a revert of the squash commit matched %+v", got)
	}
	// The title alone, which both have: neither.
	if got := parseRevert("o/r", "Revert \"Fix the parser (#5)\"", "", false).reverted(open, false); len(got) != 0 {
		t.Fatalf("an ambiguous title matched %+v", got)
	}
	// The title alone, with one merge left open that has it: that one.
	if got := parseRevert("o/r", "Revert \"Fix the parser (#5)\"", "", false).reverted(open[1:], false); len(got) != 1 || got[0].PR != 50 {
		t.Fatalf("a title one merge has matched %+v", got)
	}
}

// fakeGuard is fakeCI whose base branch has commits and merged PRs to read.
type fakeGuard struct {
	*fakeCI
	gmu       sync.Mutex
	commits   []github.Commit // newest first
	merged    []github.MergedPR
	lists     int // BranchCommits and MergedPRs calls
	queue     bool
	queued    map[int]string
	queueErr  error
	sinceSeen time.Time
	parents   map[string]string // first parents, by commit
	tipReads  int               // BranchSHA calls
	onQueue   func()            // called as MergeQueue is asked
	getPR     func(*github.PR)  // changes what GetPR returns
}

func (f *fakeGuard) FirstParent(_ context.Context, sha string) (string, error) {
	f.gmu.Lock()
	defer f.gmu.Unlock()
	return f.parents[sha], nil
}

func (f *fakeGuard) BranchSHA(ctx context.Context, branch string) (string, error) {
	f.gmu.Lock()
	f.tipReads++
	f.gmu.Unlock()
	return f.fakeCI.BranchSHA(ctx, branch)
}

func (f *fakeGuard) GetPR(ctx context.Context, n int) (*github.PR, error) {
	pr, err := f.fakeCI.GetPR(ctx, n)
	if err == nil && f.getPR != nil {
		f.getPR(pr)
	}
	return pr, err
}

func (f *fakeGuard) BranchCommits(_ context.Context, _ string, since time.Time, max int) ([]github.Commit, bool, error) {
	f.gmu.Lock()
	defer f.gmu.Unlock()
	f.lists++
	f.sinceSeen = since
	var out []github.Commit
	for _, c := range f.commits {
		if c.At.After(since) {
			out = append(out, c)
		}
	}
	if len(out) > max {
		return out[:max], true, nil
	}
	return out, false, nil
}

func (f *fakeGuard) MergedPRs(_ context.Context, _ string, since time.Time, max int) ([]github.MergedPR, error) {
	f.gmu.Lock()
	defer f.gmu.Unlock()
	f.lists++
	var out []github.MergedPR
	for _, pr := range f.merged {
		if pr.MergedAt.After(since) && len(out) < max {
			out = append(out, pr)
		}
	}
	return out, nil
}

func (f *fakeGuard) MergeQueue(context.Context, string) (bool, error) {
	if f.onQueue != nil {
		f.onQueue()
	}
	return f.queue, f.queueErr
}

func (f *fakeGuard) QueueMerge(_ context.Context, n int, sha, _ string) error {
	f.gmu.Lock()
	defer f.gmu.Unlock()
	if f.queued == nil {
		f.queued = map[int]string{}
	}
	f.queued[n] = sha
	return nil
}

// guardSetup is setup with a forge that has CI, commits and a merge queue
// switch, auto-merge on for low-risk S changes, and notifications kept.
func guardSetup(t *testing.T, health bool) (*env, *fakeGuard, *[]string) {
	t.Helper()
	e, ci, msgs := healthSetup(t)
	e.p.Cfg.MainHealth.Enabled = health
	g := &fakeGuard{fakeCI: ci, parents: map[string]string{}}
	e.p.GH = g
	e.p.Cfg.Repos[0].AutoMerge = &config.AutoMerge{UpTo: "low"}
	e.claude.triage = `{"decision":"ready","complexity":"S","risk":"low","question":"","summary":"add Add"}`
	now := time.Now()
	guardNow = func() time.Time { return now }
	t.Cleanup(func() { guardNow = time.Now })
	return e, g, msgs
}

func notes(msgs *[]string, sub string) int {
	n := 0
	for _, m := range *msgs {
		if strings.Contains(m, sub) {
			n++
		}
	}
	return n
}

func mergeOf(t *testing.T, e *env, pr int) store.FactoryMerge {
	t.Helper()
	ms, err := e.p.Store.FactoryMerges(context.Background(), "o/r", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range ms {
		if m.PR == pr {
			return m
		}
	}
	t.Fatalf("no merge of PR %d in %+v", pr, ms)
	return store.FactoryMerge{}
}

// merged builds issue n into a PR that GitHub merged at mergeSHA, and
// closes the item out, as a poll would.
func merged(t *testing.T, e *env, n int, mergeSHA string) *store.WorkItem {
	t.Helper()
	ctx := context.Background()
	e.addIssue(n)
	e.claude.builds = append(e.claude.builds, goodBuild)
	e.codex.reviews = append(e.codex.reviews, pass)
	e.p.lastPoll = time.Time{}
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	it := e.item(t, n)
	if it.PRNumber == 0 {
		t.Fatalf("no PR for %d: %s", n, it.Stage)
	}
	pr := e.gh.prs[fmt.Sprintf("factory/issue-%d", n)]
	at := guardNow().Add(-time.Minute).UTC().Truncate(time.Second)
	pr.Merged, pr.State, pr.MergeCommit, pr.MergedAt = true, "closed", mergeSHA, &at
	if err := e.p.Intake(ctx); err != nil {
		t.Fatal(err)
	}
	if got := e.item(t, n); got.Stage != state.Done {
		t.Fatalf("stage %s", got.Stage)
	}
	return e.item(t, n)
}

func TestRevertByACommitOnMain(t *testing.T) {
	ctx := context.Background()
	e, g, msgs := guardSetup(t, true)
	e.p.Cfg.Repos[0].AutoMerge = nil // the watch runs whoever merged it
	it := merged(t, e, 1, "feedface00000001")
	m := mergeOf(t, e, it.PRNumber)
	if m.ItemID != it.ID || m.Auto || m.Title == "" {
		t.Fatalf("merge %+v", m)
	}
	// As of when the forge says it merged, not when the factory saw it.
	if want := stamp(guardNow().Add(-time.Minute).UTC().Truncate(time.Second)); m.MergedAt != want {
		t.Fatalf("merged at %s, want %s", m.MergedAt, want)
	}
	// Nothing new on main: no listing.
	g.head = "c0ffee01"
	e.p.CheckMainHealth(ctx)
	calls := g.lists
	if calls != 2 {
		t.Fatalf("first reading listed %d times", calls)
	}
	e.p.CheckMainHealth(ctx)
	if g.lists != calls {
		t.Fatal("listed again with the head unchanged")
	}
	g.head = "c0ffee02"
	g.commits = []github.Commit{{SHA: "c0ffee02", Message: "Revert \"Merge pull request #101\"\n\nThis reverts commit feedface00000001, reversing\nchanges made to 1234567.", At: guardNow()}}
	e.p.CheckMainHealth(ctx)
	m = mergeOf(t, e, it.PRNumber)
	if m.RevertedAt == "" || m.RevertedBy != "commit c0ffee02" {
		t.Fatalf("not reverted: %+v", m)
	}
	evs, _ := e.p.Store.Events(ctx, it.ID)
	if !slices.ContainsFunc(evs, func(ev store.Event) bool { return ev.Type == "pr_reverted" }) {
		t.Fatalf("no event: %+v", evs)
	}
	// Told once, whatever later readings see.
	g.head = "c0ffee03"
	e.p.CheckMainHealth(ctx)
	if n := notes(msgs, "was reverted"); n != 1 {
		t.Fatalf("told %d times: %q", n, *msgs)
	}
	// Merged and reverted: nothing left to watch for, so no listing.
	g.head = "c0ffee04"
	calls = g.lists
	e.p.CheckMainHealth(ctx)
	if g.lists != calls {
		t.Fatal("listed with nothing to watch for")
	}
}

func TestRevertByAPRWithMainHealthOff(t *testing.T) {
	ctx := context.Background()
	e, g, msgs := guardSetup(t, false)
	e.p.Cfg.Repos[0].AutoMerge = nil
	it := merged(t, e, 1, "feedface00000001")
	g.head = "abc"
	// Squashed with its message rewritten: only the PR says what it reverts.
	g.commits = []github.Commit{{SHA: "abc", Message: "Undo the parser change (#140)", At: guardNow()}}
	g.merged = []github.MergedPR{{Number: 140, Title: "Revert \"Add an Add function (#1)\"", Body: "It broke prod.", HeadRef: fmt.Sprintf("revert-%d-factory/issue-1", it.PRNumber), MergedAt: guardNow()}}
	e.p.lastPoll = time.Now()
	e.p.healthAt = time.Time{}
	if err := e.p.Tick(ctx, false); err != nil {
		t.Fatal(err)
	}
	if m := mergeOf(t, e, it.PRNumber); m.RevertedBy != "PR #140" {
		t.Fatalf("merge %+v", m)
	}
	if notes(msgs, "was reverted") != 1 {
		t.Fatalf("notes %q", *msgs)
	}
	// eval and compare turn the watch off.
	e2, g2, _ := guardSetup(t, false)
	e2.p.Cfg.NoGuard = true
	e2.p.Cfg.Repos[0].AutoMerge = nil
	merged(t, e2, 1, "feedface00000001")
	g2.head = "abc"
	e2.p.healthAt = time.Time{}
	e2.p.lastPoll = time.Now()
	e2.p.Tick(ctx, false)
	if g2.lists != 0 {
		t.Fatal("the watch read main with the guard rails off")
	}
}

func TestRevertRate(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	st := e.p.Store
	now := time.Now().UTC()
	at := func(d time.Duration) string { return now.Add(-d).Format(store.TimeFormat) }
	for _, m := range []store.FactoryMerge{
		{Repo: "o/r", PR: 1, MergedAt: at(time.Hour)},
		{Repo: "o/r", PR: 2, MergedAt: at(2 * time.Hour)},
		{Repo: "o/r", PR: 3, MergedAt: at(3 * time.Hour)},
		{Repo: "o/x", PR: 4, MergedAt: at(4 * time.Hour)},
		{Repo: "o/r", PR: 5, MergedAt: at(40 * 24 * time.Hour)}, // before the window
	} {
		st.RecordMerge(ctx, m)
	}
	st.MarkReverted(ctx, "o/r", 1, "commit a", at(0))
	st.MarkReverted(ctx, "o/r", 5, "commit b", at(0))
	st.MarkReverted(ctx, "o/x", 4, "PR #9", at(0))
	r, err := metrics.Compute(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	if r.RevertMerged != 4 || r.Reverted != 2 || r.RevertRate != 0.5 || r.RevertWindowDays != 30 {
		t.Fatalf("report %d of %d = %v over %d days", r.Reverted, r.RevertMerged, r.RevertRate, r.RevertWindowDays)
	}
	if len(r.RevertsByRepo) != 2 || r.RevertsByRepo[0].Repo != "o/r" || r.RevertsByRepo[0].Merged != 3 || r.RevertsByRepo[0].Reverted != 1 ||
		r.RevertsByRepo[1].RevertRate != 1 {
		t.Fatalf("by repo %+v", r.RevertsByRepo)
	}
	// No merges: no rate, not 0%.
	e2 := setup(t)
	if r, _ := metrics.Compute(ctx, e2.p.Store); r.RevertRate != -1 || r.RevertsByRepo == nil {
		t.Fatalf("empty %+v", r)
	}
	// A bounded period counts only what merged in it.
	r, _, _ = metrics.Period(ctx, st, now.Add(-150*time.Minute), now.Add(-90*time.Minute))
	if r.RevertMerged != 1 || r.Reverted != 0 {
		t.Fatalf("period: %d of %d", r.Reverted, r.RevertMerged)
	}
}

// waitingAutoMerge builds issue n into a PR that is set to merge itself.
func waitingAutoMerge(t *testing.T, e *env, n int) *store.WorkItem {
	t.Helper()
	e.addIssue(n)
	e.claude.builds = append(e.claude.builds, goodBuild)
	e.codex.reviews = append(e.codex.reviews, pass)
	e.p.lastPoll = time.Time{}
	if err := e.p.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	it := e.item(t, n)
	if !e.p.autoMerged(context.Background(), it) {
		t.Fatalf("item %d isn't auto-merging", n)
	}
	return it
}

func TestBreakerTripsCancelsAndResets(t *testing.T) {
	ctx := context.Background()
	e, g, msgs := guardSetup(t, true)
	e.p.Cfg.MainHealth.RequireApproval = true // its fixes would take the builds
	a := merged(t, e, 1, "aaaa000000000001")
	b := merged(t, e, 2, "bbbb000000000002")
	if b.Stage != state.Done {
		evs, _ := e.p.Store.Events(ctx, b.ID)
		t.Fatalf("%+v", evs)
	}
	if !mergeOf(t, e, a.PRNumber).Auto {
		t.Fatal("an auto-merged PR isn't recorded as one")
	}
	pending := waitingAutoMerge(t, e, 3)

	// One strike: a revert. Still on.
	g.head = "head1"
	g.commits = []github.Commit{{SHA: "head1", Message: "x\n\nThis reverts commit aaaa000000000001.", At: guardNow()}}
	e.p.CheckMainHealth(ctx)
	if bs, _ := e.p.Breaker(ctx, "o/r"); bs.Tripped || bs.Strikes != 1 || bs.Threshold != 2 {
		t.Fatalf("after one strike %+v", bs)
	}
	// The second: main health sees a check go red on b's merge commit,
	// one that wasn't red on the commit before it.
	g.head = "bbbb000000000002"
	g.parents["bbbb000000000002"] = "head1"
	g.runs["head1"] = []github.CheckRun{run(9, "test", "success")}
	g.runs["bbbb000000000002"] = []github.CheckRun{run(1, "test", "failure")}
	e.p.CheckMainHealth(ctx)
	bs, _ := e.p.Breaker(ctx, "o/r")
	if !bs.Tripped || bs.Strikes != 2 || !strings.Contains(bs.Reason, "turned main red") {
		t.Fatalf("not tripped: %+v", bs)
	}
	if notes(msgs, "Auto-merge is off for o/r") != 1 || notes(msgs, "the merge of factory PR") != 1 {
		t.Fatalf("notes %q", *msgs)
	}
	// The pending auto-merge was cancelled through the usual path, and
	// the PR waits for a human.
	if !slices.Contains(e.gh.cancelled, pending.PRNumber) || e.p.autoMerged(ctx, e.item(t, 3)) {
		t.Fatalf("pending auto-merge not cancelled: %v", e.gh.cancelled)
	}
	if !hasLabel(e.gh.prLabels[pending.PRNumber], LabelAwaitingYou) || hasLabel(e.gh.prLabels[pending.PRNumber], LabelAutoMerge) {
		t.Fatalf("labels %v", e.gh.prLabels[pending.PRNumber])
	}
	// Its description no longer says it merges itself.
	if body := e.gh.bodies[pending.PRNumber]; strings.Contains(body, autoFooter) || !strings.Contains(body, humanFooter) {
		t.Fatalf("body still promises a merge:\n%s", body)
	}
	// Further readings and strikes don't trip it, or tell you, again.
	g.head = "head3"
	g.runs["head3"] = []github.CheckRun{run(2, "lint", "failure")}
	e.p.CheckMainHealth(ctx)
	if notes(msgs, "Auto-merge is off") != 1 {
		t.Fatalf("told again: %q", *msgs)
	}
	// While tripped, a new PR waits for you.
	e.addIssue(20)
	e.claude.builds = append(e.claude.builds, goodBuild)
	e.codex.reviews = append(e.codex.reviews, pass)
	e.p.lastPoll = time.Time{}
	e.p.Drain(ctx)
	if it := e.item(t, 20); e.p.autoMerged(ctx, it) || !hasLabel(e.gh.prLabels[it.PRNumber], LabelAwaitingYou) {
		t.Fatalf("auto-merged with the breaker tripped: %+v %v", it, e.gh.prLabels[it.PRNumber])
	}
	later := guardNow().Add(time.Minute)
	guardNow = func() time.Time { return later }
	if err := e.p.ResetAutoMerge(ctx, "O/R", "me"); err != nil {
		t.Fatal(err)
	}
	if err := e.p.ResetAutoMerge(ctx, "o/r", "me"); err == nil {
		t.Fatal("reset an untripped breaker")
	}
	if bs, _ := e.p.Breaker(ctx, "o/r"); bs.Tripped || bs.Strikes != 0 {
		t.Fatalf("after reset %+v: old strikes must not count", bs)
	}
	waitingAutoMerge(t, e, 21)
	_ = b
}

// redOn has main health see check go red on sha, the base branch's new
// head, where it passed on the commit before.
func redOn(t *testing.T, e *env, g *fakeGuard, sha, check string, id int64) {
	t.Helper()
	g.head = sha
	g.parents[sha] = sha + "-parent"
	g.runs[sha+"-parent"] = []github.CheckRun{run(id+1000, check, "success")}
	g.runs[sha] = []github.CheckRun{run(id, check, "failure")}
	e.p.CheckMainHealth(context.Background())
}

func TestBreakerCoolOff(t *testing.T) {
	ctx := context.Background()
	e, g, msgs := guardSetup(t, true)
	e.p.Cfg.MainHealth.RequireApproval = true
	e.p.Cfg.Repos[0].AutoMerge.Breaker = &config.Breaker{Threshold: 1, CoolOff: config.Duration{Duration: 48 * time.Hour}}
	merged(t, e, 1, "aaaa000000000001")
	merged(t, e, 2, "bbbb000000000002")
	tripped := guardNow()
	redOn(t, e, g, "aaaa000000000001", "test", 1)
	if !e.p.breakerTripped(ctx, "o/r") || notes(msgs, "comes back on its own after 2 days") != 1 {
		t.Fatalf("not tripped: %q", *msgs)
	}
	now := tripped.Add(49 * time.Hour)
	guardNow = func() time.Time { return now }
	// Past its cool-off, status says it's back on before anything else
	// has looked, as of when the cool-off ended.
	if bs, _ := e.p.Breaker(ctx, "o/r"); bs.Tripped || bs.ResetAt != stamp(tripped.Add(48*time.Hour)) || bs.Strikes != 0 {
		t.Fatalf("status after the cool-off %+v", bs)
	}
	// A strike after it ended counts, though nothing looked in between:
	// it trips again, and you're told it came back on, then went off.
	redOn(t, e, g, "bbbb000000000002", "lint", 2)
	if notes(msgs, "back on for o/r") != 1 || notes(msgs, "Auto-merge is off for o/r") != 2 {
		t.Fatalf("notes %q", *msgs)
	}
	if bs, _ := e.p.Breaker(ctx, "o/r"); !bs.Tripped || bs.Strikes != 1 || !strings.Contains(bs.Reason, "lint") {
		t.Fatalf("not tripped again: %+v", bs)
	}
	// Its second cool-off, read lazily, says so once.
	now = now.Add(49 * time.Hour)
	for range 2 {
		if e.p.breakerTripped(ctx, "o/r") {
			t.Fatal("still tripped after its cool-off")
		}
	}
	if notes(msgs, "back on for o/r") != 2 {
		t.Fatalf("notes %q", *msgs)
	}
	// Off with the guard rails off.
	e.p.Cfg.NoGuard = true
	e.p.Store.TripBreaker(ctx, "o/r", "x", stamp(now))
	if e.p.breakerTripped(ctx, "o/r") {
		t.Fatal("eval's breaker tripped")
	}
}

func TestRedBeforeTheMergeIsNoStrike(t *testing.T) {
	ctx := context.Background()
	e, g, msgs := guardSetup(t, true)
	e.p.Cfg.MainHealth.RequireApproval = true
	it := merged(t, e, 1, "aaaa000000000001")
	// test was red on the commit before the merge too: not the PR's doing.
	g.head = "aaaa000000000001"
	g.parents["aaaa000000000001"] = "before"
	g.runs["before"] = []github.CheckRun{run(1, "test", "failure")}
	g.runs["aaaa000000000001"] = []github.CheckRun{run(2, "test", "failure")}
	e.p.CheckMainHealth(ctx)
	if m := mergeOf(t, e, it.PRNumber); m.RedAt != "" || notes(msgs, "the merge of factory PR") != 0 {
		t.Fatalf("struck for a red it inherited: %+v %q", m, *msgs)
	}
	// Not known to have passed there (still running, or missing): none.
	g.runs["before"] = []github.CheckRun{run(1, "test", "")}
	g.runs["aaaa000000000001"] = append(g.runs["aaaa000000000001"], run(4, "build", "failure"))
	e.p.CheckMainHealth(ctx)
	if m := mergeOf(t, e, it.PRNumber); m.RedAt != "" {
		t.Fatalf("struck with the parent's check missing: %+v", m)
	}
	// Unsure (the parent can't be read): no strike either.
	g.runs["aaaa000000000001"] = append(g.runs["aaaa000000000001"], run(3, "lint", "failure"))
	delete(g.parents, "aaaa000000000001")
	e.p.CheckMainHealth(ctx)
	if m := mergeOf(t, e, it.PRNumber); m.RedAt != "" {
		t.Fatalf("struck unsure: %+v", m)
	}
}

func TestRefiledRedIsNoStrike(t *testing.T) {
	ctx := context.Background()
	e, g, msgs := guardSetup(t, true)
	e.p.Cfg.MainHealth.RequireApproval = true
	// test went red on an earlier commit and its issue was closed as done.
	g.head = "before"
	g.runs["before"] = []github.CheckRun{run(1, "test", "failure")}
	e.p.CheckMainHealth(ctx)
	hs, _ := e.p.Store.HealthIssues(ctx, "o/r")
	if len(hs) != 1 || hs[0].IssueNumber == 0 {
		t.Fatalf("health issues %+v", hs)
	}
	e.gh.issues[hs[0].IssueNumber].State = "closed"
	it := merged(t, e, 1, "aaaa000000000001")
	// Still red on the factory merge after it: a refile, not a strike.
	g.head = "aaaa000000000001"
	g.parents["aaaa000000000001"] = "green"
	g.runs["aaaa000000000001"] = []github.CheckRun{run(2, "test", "failure")}
	e.p.CheckMainHealth(ctx)
	if m := mergeOf(t, e, it.PRNumber); m.RedAt != "" || notes(msgs, "the merge of factory PR") != 0 {
		t.Fatalf("a refile struck: %+v %q", m, *msgs)
	}
}

func TestRevertsReadWhenCIIsUnreadable(t *testing.T) {
	ctx := context.Background()
	e, g, _ := guardSetup(t, true)
	e.p.Cfg.Repos[0].AutoMerge = nil
	it := merged(t, e, 1, "feedface00000001")
	g.readErr = &github.HTTPError{Status: 403, Body: "Resource not accessible by integration"}
	g.head = "c0ffee02"
	g.commits = []github.Commit{{SHA: "c0ffee02", Message: "x\n\nThis reverts commit feedface00000001.", At: guardNow()}}
	e.p.CheckMainHealth(ctx)
	if m := mergeOf(t, e, it.PRNumber); m.RevertedBy != "commit c0ffee02" {
		t.Fatalf("not read with CI unreadable: %+v", m)
	}
}

func TestRevertWatchKeepsItsPlaceWhenItCantReadEverything(t *testing.T) {
	ctx := context.Background()
	e, g, _ := guardSetup(t, true)
	e.p.Cfg.Repos[0].AutoMerge = nil
	merged(t, e, 1, "feedface00000001")
	g.head = "h1"
	e.p.CheckMainHealth(ctx)
	g1, _ := e.p.Store.Guard(ctx, "o/r")
	if g1 == nil || g1.WatchedAt == "" {
		t.Fatalf("guard %+v", g1)
	}
	// More commits than one reading lists: the place it read from stays.
	later := guardNow().Add(time.Hour)
	guardNow = func() time.Time { return later }
	for i := range maxRevertCommits + 1 {
		g.commits = append(g.commits, github.Commit{SHA: fmt.Sprintf("c%04d", i), Message: "m", At: later})
	}
	g.head = "h2"
	e.p.CheckMainHealth(ctx)
	if g2, _ := e.p.Store.Guard(ctx, "o/r"); g2.WatchedAt != g1.WatchedAt || g2.WatchedSHA != "h1" {
		t.Fatalf("after a partial reading %+v, before %+v", g2, g1)
	}
	// A full one moves it on.
	g.commits = g.commits[:1]
	g.head = "h3"
	e.p.CheckMainHealth(ctx)
	if g3, _ := e.p.Store.Guard(ctx, "o/r"); g3.WatchedAt != stamp(later) {
		t.Fatalf("after a full reading %+v", g3)
	}
}

func TestBreakerTrippingWhileAPROpensStopsIt(t *testing.T) {
	ctx := context.Background()
	e, g, msgs := guardSetup(t, false)
	// It trips after openPR checked it and before auto-merge goes on.
	g.onQueue = func() {
		g.onQueue = nil
		e.p.Store.TripBreaker(ctx, "o/r", "two reverts", stamp(guardNow()))
	}
	e.addIssue(1)
	e.claude.builds = []func(string) error{goodBuild}
	e.codex.reviews = []string{pass}
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	it := e.item(t, 1)
	if _, on := e.gh.autoMerged[it.PRNumber]; on || e.p.autoMerged(ctx, it) || len(g.queued) != 0 {
		t.Fatalf("auto-merge turned on past a tripped breaker: %v", e.gh.autoMerged)
	}
	if notes(msgs, "tripped while it was opened") != 1 || !hasLabel(e.gh.prLabels[it.PRNumber], LabelAwaitingYou) ||
		strings.Contains(e.gh.bodies[it.PRNumber], autoFooter) {
		t.Fatalf("notes %q labels %v", *msgs, e.gh.prLabels[it.PRNumber])
	}
}

func TestBreakerTrippedWhileWaitingForCIHandsItBack(t *testing.T) {
	ctx := context.Background()
	e, g, msgs := guardSetup(t, false)
	e.addIssue(1)
	e.claude.builds = []func(string) error{func(work string) error {
		advanceMain(t, e.remote, "other.go")
		return goodBuild(work)
	}}
	e.codex.reviews = []string{pass}
	e.p.Drain(ctx)
	it := e.item(t, 1)
	ms := e.p.mergeState(ctx, it)
	if ms.Kind != "auto_merge_wait" {
		t.Fatalf("state %+v", ms)
	}
	// Tripped with nothing handed back (as if it had raced the trip).
	e.p.Store.TripBreaker(ctx, "o/r", "two reverts", stamp(guardNow()))
	g.head = ms.Base
	g.runs[ms.Head] = []github.CheckRun{run(1, "test", "success")}
	e.p.lastPoll = time.Time{}
	e.p.Intake(ctx)
	if _, on := e.gh.autoMerged[it.PRNumber]; on || e.p.autoMerged(ctx, it) || notes(msgs, "breaker for o/r tripped") != 1 {
		t.Fatalf("turned on past a tripped breaker: %v %q", e.gh.autoMerged, *msgs)
	}
}

func TestSyncsStartOverForANewBuild(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	it, _, err := e.p.Store.UpsertItem(ctx, "o/r", 1, "x")
	if err != nil {
		t.Fatal(err)
	}
	it.PRNumber = 7
	add := func(typ, data string) {
		if err := e.p.Store.AddEvent(ctx, store.Event{Actor: "daemon", ItemID: &it.ID, Type: typ, Reason: "PR #7", Data: data}); err != nil {
			t.Fatal(err)
		}
	}
	add("auto_merge_wait", `{"head":"s1","base":"b1"}`)
	add("auto_merge", `{"head":"s1","base":"b1"}`) // CI passed on it
	add("auto_merge_wait", `{"head":"s2","base":"b2"}`)
	if ms := e.p.mergeState(ctx, it); ms.Syncs != 2 || !slices.Equal(ms.Made, []string{"s1", "s2"}) {
		t.Fatalf("%+v", ms)
	}
	add("auto_merge_off", `{}`)
	if ms := e.p.mergeState(ctx, it); ms.Syncs != 0 || len(ms.Made) != 2 {
		t.Fatalf("after off %+v", ms)
	}
	add("auto_merge_wait", `{"head":"s3","base":"b3"}`)
	add("auto_merge", `{"head":"v2","base":"b3"}`) // a new build's head
	if ms := e.p.mergeState(ctx, it); ms.Syncs != 0 || ms.Kind != "auto_merge" {
		t.Fatalf("after a new build %+v", ms)
	}
}

func TestMergedWhileWaitingForCIIsAHumans(t *testing.T) {
	ctx := context.Background()
	e, g, _ := guardSetup(t, false)
	e.addIssue(1)
	e.claude.builds = []func(string) error{func(work string) error {
		advanceMain(t, e.remote, "other.go")
		return goodBuild(work)
	}}
	e.codex.reviews = []string{pass}
	e.p.Drain(ctx)
	it := e.item(t, 1)
	ms := e.p.mergeState(ctx, it)
	g.head = ms.Base
	pr := e.gh.prs["factory/issue-1"]
	pr.Merged, pr.State, pr.Head.SHA, pr.MergeCommit = true, "closed", ms.Head, "m1"
	e.p.lastPoll = time.Time{}
	e.p.Intake(ctx)
	if m := mergeOf(t, e, it.PRNumber); m.Auto {
		t.Fatalf("a merge while auto-merge was off on the forge counted as auto: %+v", m)
	}
	evs, _ := e.p.Store.Events(ctx, it.ID)
	if !slices.ContainsFunc(evs, func(ev store.Event) bool { return ev.Type == "pr_merged" && ev.Actor == "human" }) {
		t.Fatalf("events %+v", evs)
	}
}

func TestMergedAsMainMovedIsNotResynced(t *testing.T) {
	ctx := context.Background()
	e, g, _ := guardSetup(t, false)
	waitingAutoMerge(t, e, 1)
	it := e.item(t, 1)
	ms := e.p.mergeState(ctx, it)
	if ms.Kind != "auto_merge" || ms.Base == "" {
		t.Fatalf("state %+v", ms)
	}
	// main moved by merging it, after closeOut read it as open.
	g.head = advanceMain(t, e.remote, "other.go")
	g.getPR = func(pr *github.PR) {
		g.getPR = func(pr *github.PR) { pr.Merged, pr.State = true, "closed" }
	}
	before := git(t, e.remote, "rev-parse", "factory/issue-1")
	e.p.lastPoll = time.Time{}
	e.p.Intake(ctx)
	if got := e.p.mergeState(ctx, it); got.Kind != "auto_merge" || len(e.gh.cancelled) != 0 || git(t, e.remote, "rev-parse", "factory/issue-1") != before {
		t.Fatalf("resynced a merged PR: %+v, cancelled %v", got, e.gh.cancelled)
	}
	if got := e.item(t, 1); got.Stage != state.AwaitingHuman {
		t.Fatalf("closeOut saw it merged first: %s", got.Stage)
	}
}

func TestBaseHeadReadOncePerPass(t *testing.T) {
	e, g, _ := guardSetup(t, false)
	waitingAutoMerge(t, e, 1)
	waitingAutoMerge(t, e, 2)
	g.head = e.p.mergeState(context.Background(), e.item(t, 1)).Base
	g.tipReads = 0
	if err := e.p.closeOut(context.Background()); err != nil {
		t.Fatal(err)
	}
	if g.tipReads != 1 {
		t.Fatalf("read main's head %d times for two PRs", g.tipReads)
	}
}

// advanceMain pushes a commit to the remote's main from a clone of its own.
func advanceMain(t *testing.T, remote, file string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "other")
	git(t, filepath.Dir(dir), "clone", "-q", remote, dir)
	os.WriteFile(filepath.Join(dir, file), []byte("someone else's change\n"), 0o644)
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-qm", "unrelated")
	git(t, dir, "push", "-q", "origin", "HEAD:main")
	return git(t, dir, "rev-parse", "HEAD")
}

func TestBehindMainIsBroughtUpToDateFirst(t *testing.T) {
	ctx := context.Background()
	e, g, msgs := guardSetup(t, false)
	e.addIssue(1)
	e.claude.builds = []func(string) error{func(work string) error {
		advanceMain(t, e.remote, "other.go") // main moves while it builds
		return goodBuild(work)
	}}
	e.codex.reviews = []string{pass}
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	it := e.item(t, 1)
	jobs, _ := e.p.Store.ListJobs(ctx, store.JobFilter{ItemID: it.ID, Kind: "implement"})
	verified := jobs[0].HeadSHA
	if _, on := e.gh.autoMerged[it.PRNumber]; on {
		t.Fatal("auto-merge turned on for a branch behind main")
	}
	ms := e.p.mergeState(ctx, it)
	tip := git(t, e.remote, "rev-parse", "main")
	head := git(t, e.remote, "rev-parse", "factory/issue-1")
	if ms.Kind != "auto_merge_wait" || ms.Head != head || ms.Base != tip || head == verified {
		t.Fatalf("state %+v, head %s, tip %s", ms, head, tip)
	}
	// The new head is the verified commit merged with main, by the daemon.
	if parents := strings.Fields(git(t, e.remote, "log", "-1", "--format=%P", head)); len(parents) != 2 || parents[0] != verified || parents[1] != tip {
		t.Fatalf("parents %v", parents)
	}
	if notes(msgs, "waits for CI there first") != 1 || !hasLabel(e.gh.prLabels[it.PRNumber], LabelAutoMerge) {
		t.Fatalf("notes %q labels %v", *msgs, e.gh.prLabels[it.PRNumber])
	}
	poll := func() {
		e.p.lastPoll = time.Time{}
		if err := e.p.Intake(ctx); err != nil {
			t.Fatal(err)
		}
	}
	g.head = tip
	// CI is still running on the new head: it waits.
	g.runs[head] = []github.CheckRun{run(1, "test", "")}
	poll()
	if _, on := e.gh.autoMerged[it.PRNumber]; on {
		t.Fatal("turned on before CI passed")
	}
	g.runs[head] = []github.CheckRun{run(1, "test", ""), run(2, "lint", "success")}
	poll()
	if _, on := e.gh.autoMerged[it.PRNumber]; on {
		t.Fatal("turned on with a check still running")
	}
	g.runs[head] = []github.CheckRun{run(3, "test", "success"), run(2, "lint", "success")}
	poll()
	if got := e.gh.autoMerged[it.PRNumber]; got != head+" merge" {
		t.Fatalf("auto-merge %q, want it pinned to %s", got, head)
	}
	// Main moves again before it merges: off, up to date again, waiting.
	tip2 := advanceMain(t, e.remote, "third.go")
	g.head = tip2
	poll()
	head2 := git(t, e.remote, "rev-parse", "factory/issue-1")
	if _, on := e.gh.autoMerged[it.PRNumber]; on || head2 == head {
		t.Fatalf("still on after main moved; head %s", head2)
	}
	if ms := e.p.mergeState(ctx, it); ms.Kind != "auto_merge_wait" || ms.Head != head2 || ms.Base != tip2 || ms.Syncs != 2 {
		t.Fatalf("state %+v", ms)
	}
	// CI fails on it merged with main: it waits for you.
	g.runs[head2] = []github.CheckRun{run(3, "test", "failure")}
	poll()
	if e.p.autoMerged(ctx, it) || notes(msgs, "CI failed on it merged with main") != 1 || !hasLabel(e.gh.prLabels[it.PRNumber], LabelAwaitingYou) {
		t.Fatalf("not handed back: %q", *msgs)
	}
	// Merged later by a human at the synced head, it's not counted as changed.
	pr := e.gh.prs["factory/issue-1"]
	pr.Merged, pr.Head.SHA = true, head2
	poll()
	evs, _ := e.p.Store.Events(ctx, it.ID)
	if !slices.ContainsFunc(evs, func(ev store.Event) bool {
		return ev.Type == "pr_merged" && strings.Contains(ev.Data, `"changed":false`)
	}) {
		t.Fatalf("events %+v", evs)
	}
}

func TestBehindMainConflictWaitsForYou(t *testing.T) {
	ctx := context.Background()
	e, _, msgs := guardSetup(t, false)
	e.addIssue(1)
	e.claude.builds = []func(string) error{func(work string) error {
		advanceMain(t, e.remote, "add.go") // the same file, differently
		return goodBuild(work)
	}}
	e.codex.reviews = []string{pass}
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	it := e.item(t, 1)
	if e.p.autoMerged(ctx, it) || notes(msgs, "doesn't merge into it cleanly") != 1 || !hasLabel(e.gh.prLabels[it.PRNumber], LabelAwaitingYou) {
		t.Fatalf("notes %q", *msgs)
	}
}

func TestMergeQueueIsUsedWhenThere(t *testing.T) {
	ctx := context.Background()
	e, g, msgs := guardSetup(t, false)
	g.queue = true
	e.addIssue(1)
	e.claude.builds = []func(string) error{func(work string) error {
		advanceMain(t, e.remote, "other.go") // the queue tests it against main
		return goodBuild(work)
	}}
	e.codex.reviews = []string{pass}
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	it := e.item(t, 1)
	jobs, _ := e.p.Store.ListJobs(ctx, store.JobFilter{ItemID: it.ID, Kind: "implement"})
	if got := g.queued[it.PRNumber]; got != jobs[0].HeadSHA {
		t.Fatalf("queued %q, want the verified %s", got, jobs[0].HeadSHA)
	}
	if _, on := e.gh.autoMerged[it.PRNumber]; on {
		t.Fatal("plain auto-merge with a merge queue there")
	}
	if git(t, e.remote, "rev-parse", "factory/issue-1") != jobs[0].HeadSHA {
		t.Fatal("pushed to a PR the merge queue keeps current")
	}
	if notes(msgs, "through the merge queue") != 1 || !e.p.autoMerged(ctx, it) {
		t.Fatalf("notes %q", *msgs)
	}
	// Main moving doesn't touch it: the queue tests each merge.
	g.head = advanceMain(t, e.remote, "third.go")
	e.p.lastPoll = time.Time{}
	e.p.Intake(ctx)
	if !e.p.autoMerged(ctx, it) || len(e.gh.cancelled) != 0 {
		t.Fatal("cancelled a queued merge")
	}
	// Can't tell whether there's a queue: it waits for you.
	e2, g2, msgs2 := guardSetup(t, false)
	g2.queueErr = fmt.Errorf("github: HTTP 502")
	waiting := func() *store.WorkItem {
		e2.addIssue(1)
		e2.claude.builds = []func(string) error{goodBuild}
		e2.codex.reviews = []string{pass}
		e2.p.Drain(ctx)
		return e2.item(t, 1)
	}()
	if e2.p.autoMerged(ctx, waiting) || notes(msgs2, "merge queue") != 1 {
		t.Fatalf("notes %q", *msgs2)
	}
}

func TestSyncCloneCommandsAllGetCredentials(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	git(t, root, "init", "-q", "--bare", "-b", "main", remote)
	src := filepath.Join(root, "src")
	git(t, root, "clone", "-q", remote, src)
	os.WriteFile(filepath.Join(src, "a"), []byte("a\n"), 0o644)
	git(t, src, "add", "-A")
	git(t, src, "commit", "-qm", "a")
	git(t, src, "push", "-q", "origin", "HEAD:main")
	git(t, src, "checkout", "-qb", "topic")
	os.WriteFile(filepath.Join(src, "b"), []byte("b\n"), 0o644)
	git(t, src, "add", "-A")
	git(t, src, "commit", "-qm", "b")
	git(t, src, "push", "-q", "origin", "topic")
	git(t, src, "checkout", "-q", "main")
	os.WriteFile(filepath.Join(src, "c"), []byte("c\n"), 0o644)
	git(t, src, "add", "-A")
	git(t, src, "commit", "-qm", "c")
	git(t, src, "push", "-q", "origin", "HEAD:main")

	type call struct {
		dir  string
		args []string
		auth bool
	}
	var calls []call
	g := &Git{RemoteURL: remote, Tokens: github.StaticToken("tok"), AskPassDir: root}
	g.trace = func(dir string, args, env []string) {
		calls = append(calls, call{dir, args, slices.Contains(env, "FACTORY_GIT_PASSWORD=tok")})
	}
	// A blobless clone fetches file contents as checkout and merge need
	// them, from the remote: each command there needs the credentials.
	repo := filepath.Join(root, "sync", "repo")
	ws, err := g.PrepareSync(ctx, repo, "main", "topic")
	if err != nil {
		t.Fatal(err)
	}
	if g.IsAncestor(ctx, ws.Repo, ws.BaseSHA, ws.Start) {
		t.Fatal("topic isn't behind main")
	}
	sha, err := g.MergeInto(ctx, ws.Repo, "refs/remotes/origin/main", "merge")
	if err != nil {
		t.Fatal(err)
	}
	if err := g.Push(ctx, ws.Repo, sha, "topic", false); err != nil {
		t.Fatal(err)
	}
	g.DoneSync(repo)
	in := 0
	for _, c := range calls {
		if c.dir == repo || c.args[0] == "clone" {
			in++
			if !c.auth {
				t.Errorf("git %v in the sync clone ran without credentials", c.args)
			}
		}
	}
	if in < 5 {
		t.Fatalf("only %d commands in the sync clone: %+v", in, calls)
	}
	// Elsewhere, and once it's done, only clone, fetch and push get them.
	calls = nil
	g.IsAncestor(ctx, repo, ws.BaseSHA, sha)
	if len(calls) != 1 || calls[0].auth {
		t.Fatalf("a done sync clone still gets credentials: %+v", calls)
	}
}

func TestBreakerTrippingDuringASyncKeepsTheHeadAsTheFactorys(t *testing.T) {
	ctx := context.Background()
	e, _, msgs := guardSetup(t, false)
	e.p.git("o/r").trace = func(dir string, args, _ []string) {
		if args[0] == "push" && strings.Contains(dir, string(filepath.Separator)+"sync"+string(filepath.Separator)) {
			e.p.Store.TripBreaker(ctx, "o/r", "two reverts", stamp(guardNow()))
		}
	}
	e.addIssue(1)
	e.claude.builds = []func(string) error{func(work string) error {
		advanceMain(t, e.remote, "other.go")
		return goodBuild(work)
	}}
	e.codex.reviews = []string{pass}
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	e.p.git("o/r").trace = nil
	it := e.item(t, 1)
	head := git(t, e.remote, "rev-parse", "factory/issue-1")
	ms := e.p.mergeState(ctx, it)
	if e.p.autoMerged(ctx, it) || !slices.Contains(ms.Made, head) || notes(msgs, "tripped while it was opened") != 1 {
		t.Fatalf("state %+v head %s notes %q", ms, head, *msgs)
	}
}
