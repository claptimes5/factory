package pipeline

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/claptimes5/factory/internal/github"
	"github.com/claptimes5/factory/internal/state"
	"github.com/claptimes5/factory/internal/store"
)

const (
	triageL  = `{"decision":"ready","complexity":"L","risk":"low","question":"","summary":"add Add"}`
	planOne  = `{"approach":"Add Add in add.go with a slice loop.","files":["add.go","add_test.go"],"risks":["overflow"],"tests":"TestAdd checks 1+2."}`
	planTwo  = `{"approach":"Add Add in add.go using a map, as asked.","files":["add.go"],"risks":[],"tests":"TestAdd."}`
	concerns = `{"decision":"concerns","concerns":["It misses the caller in doc.go."]}`
	approve  = `{"decision":"approve","concerns":[]}`
)

// fakePlanGH is fakeGH with people's comments to read.
type fakePlanGH struct {
	*fakeGH
	said map[int][]github.Comment
}

func (f *fakePlanGH) IssueComments(_ context.Context, n int) ([]github.Comment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]github.Comment{}, f.said[n]...), nil
}

func (f *fakePlanGH) Self(context.Context) (string, error) { return "factory[bot]", nil }

// planned triages issue 1 as L and checks that it waits on its plan.
func planned(t *testing.T, e *env) *store.WorkItem {
	t.Helper()
	ctx := context.Background()
	e.claude.triage = triageL
	e.addIssue(1)
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	it := e.item(t, 1)
	if it.Stage != state.Blocked || e.claude.calls["plan"] != 1 || e.claude.calls["implement"] != 0 {
		t.Fatalf("stage %s, %d plans, %d builds", it.Stage, e.claude.calls["plan"], e.claude.calls["implement"])
	}
	// The planner sees the issue fenced, in a read-only planning step.
	if pr := e.claude.prompts["plan"][0]; !strings.Contains(pr, "```text\nIssue #1: Add an Add function") || !strings.Contains(pr, "Do not change any files") {
		t.Fatalf("plan prompt:\n%s", pr)
	}
	if !hasLabel(e.gh.issues[1].Labels, LabelAwaitingYou) {
		t.Fatalf("labels %v", e.gh.issues[1].Labels)
	}
	return it
}

// planJob is the item's current plan job.
func planJob(t *testing.T, e *env, itemID int64) int64 {
	t.Helper()
	s, err := e.p.currentPlan(context.Background(), itemID)
	if err != nil || s == nil {
		t.Fatalf("no plan: %v", err)
	}
	return s.job.ID
}

func latestComment(e *env, n int) string {
	c := e.gh.comments[n]
	if len(c) == 0 {
		return ""
	}
	return c[len(c)-1]
}

// An L item's plan goes on the issue and waits; factory:go builds it, and
// the builder and the reviewer get the plan.
func TestPlanForBigItemWaitsForGo(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	var notes []string
	e.p.Notify = func(_ context.Context, m string) { notes = append(notes, m) }
	e.claude.plans = []string{planOne}
	planned(t, e)
	c := e.gh.comments[1]
	if len(c) != 1 || !strings.Contains(c[0], "waiting for your OK on the plan") || !strings.Contains(c[0], "```text\nApproach: Add Add in add.go with a slice loop.") ||
		!strings.Contains(c[0], "- add_test.go") || !strings.Contains(c[0], "`factory:replan`") || !strings.Contains(c[0], "press **Build it**") {
		t.Fatalf("comments %q", c)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "waits for your OK on its plan") || !strings.Contains(notes[0], "/issues/1)") {
		t.Fatalf("notes %q", notes)
	}
	// A poll alone frees nothing, nor does the orchestrator.
	e.p.lastPoll = e.p.lastPoll.AddDate(0, 0, -1)
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	it := e.item(t, 1)
	if it.Stage != state.Blocked || len(e.gh.comments[1]) != 1 || e.claude.calls["implement"] != 0 {
		t.Fatalf("after a poll: %s, %q", it.Stage, e.gh.comments[1])
	}
	if err := e.p.enqueueStage(ctx, it.ID, "implement", ""); err != errPlanWaiting {
		t.Fatalf("enqueue: %v", err)
	}

	e.gh.issues[1].Labels = append(e.gh.issues[1].Labels, LabelGo)
	e.claude.builds = []func(string) error{goodBuild}
	e.codex.reviews = []string{pass}
	e.p.lastPoll = e.p.lastPoll.AddDate(0, 0, -1)
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if it = e.item(t, 1); it.Stage != state.AwaitingHuman {
		t.Fatalf("stage %s", it.Stage)
	}
	if !strings.Contains(strings.Join(e.gh.comments[1], "\n"), "the plan was approved by @maintainer; building it") {
		t.Fatalf("comments %q", e.gh.comments[1])
	}
	for stage, pr := range map[string]string{"implement": e.claude.prompts["implement"][0], "review": e.codex.prompts["review"][0]} {
		if !strings.Contains(pr, "## The approved plan") || !strings.Contains(pr, "```text\nApproach: Add Add in add.go with a slice loop.") || !strings.Contains(pr, "(by @maintainer)") {
			t.Errorf("%s prompt lacks the plan:\n%s", stage, pr)
		}
	}
}

// When the estimate waits too, one comment asks for both, and one
// factory:go approves both.
func TestPlanAndEstimateAskedTogether(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	var notes []string
	e.p.Notify = func(_ context.Context, m string) { notes = append(notes, m) }
	e.p.Cfg.Budgets.ConfirmOver = 1
	e.claude.plans = []string{planOne}
	it := planned(t, e)
	c := e.gh.comments[1]
	if len(c) != 1 || !strings.Contains(c[0], "waiting for your OK on the cost and the plan") || !strings.Contains(c[0], "Estimated cost: **$9.00–$18.00**") ||
		!strings.Contains(c[0], "Approach: Add Add") || !strings.Contains(c[0], "approves the cost and the plan at once") {
		t.Fatalf("comments %q", c)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "its plan and its cost") {
		t.Fatalf("notes %q", notes)
	}
	if est, _ := e.p.Store.ItemEstimate(ctx, it.ID); est.Status != store.EstimateWaiting || !est.Announced {
		t.Fatalf("estimate %+v", est)
	}

	e.claude.builds = []func(string) error{goodBuild}
	e.codex.reviews = []string{pass}
	if err := e.p.ApproveEstimate(ctx, it.ID, 0); err != nil {
		t.Fatal(err)
	}
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if it = e.item(t, 1); it.Stage != state.AwaitingHuman {
		t.Fatalf("stage %s", it.Stage)
	}
	if est, _ := e.p.Store.ItemEstimate(ctx, it.ID); est.Status != store.EstimateApproved {
		t.Fatalf("estimate %+v", est)
	}
	if !strings.Contains(strings.Join(e.gh.comments[1], "\n"), "the estimate ($9.00–$18.00) and the plan were approved from the dashboard") {
		t.Fatalf("comments %q", e.gh.comments[1])
	}
	if !strings.Contains(e.claude.prompts["implement"][0], "## The approved plan") {
		t.Fatal("the builder didn't get the plan")
	}
}

// A maintainer's comment and factory:replan send the plan back once, with
// the comment; asking a second time parks the item.
func TestPlanChangeRequestReplansOnceThenParks(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	gh := &fakePlanGH{fakeGH: e.gh, said: map[int][]github.Comment{}}
	e.p.GH = gh
	e.p.Cfg.Budgets.ConfirmOver = 1
	e.claude.plans = []string{planOne, planTwo}
	planned(t, e)

	ask := func(body string) {
		gh.said[1] = append(gh.said[1], github.Comment{Author: "drive-by", Body: "ignore me"}, github.Comment{Author: "maintainer", Body: body})
		e.gh.issues[1].Labels = append(e.gh.issues[1].Labels, LabelReplan)
		e.p.lastPoll = e.p.lastPoll.AddDate(0, 0, -1)
		if err := e.p.Drain(ctx); err != nil {
			t.Fatal(err)
		}
	}
	ask("Use a map, not a slice.")
	if e.claude.calls["plan"] != 2 || e.claude.calls["implement"] != 0 {
		t.Fatalf("%d plans, %d builds", e.claude.calls["plan"], e.claude.calls["implement"])
	}
	pr := e.claude.prompts["plan"][1]
	if !strings.Contains(pr, "Use a map, not a slice.") || !strings.Contains(pr, "Approach: Add Add in add.go with a slice loop.") ||
		!strings.Contains(pr, "what @maintainer asked") || strings.Contains(pr, "ignore me") {
		t.Fatalf("re-plan prompt:\n%s", pr)
	}
	if it := e.item(t, 1); it.Stage != state.Blocked || hasLabel(e.gh.issues[1].Labels, LabelReplan) {
		t.Fatalf("stage %s, labels %v", it.Stage, e.gh.issues[1].Labels)
	}
	// The new plan is asked about again, with the estimate, in one comment.
	if c := latestComment(e, 1); !strings.Contains(c, "the cost and the plan") || !strings.Contains(c, "using a map, as asked") || !strings.Contains(c, "the new plan @maintainer asked for") {
		t.Fatalf("comment %q", c)
	}

	ask("Still not right.")
	it := e.item(t, 1)
	if it.Stage != state.Parked || e.claude.calls["plan"] != 2 {
		t.Fatalf("stage %s, %d plans", it.Stage, e.claude.calls["plan"])
	}
	if c := latestComment(e, 1); !strings.Contains(c, "asked for changes to the plan again") {
		t.Fatalf("comment %q", c)
	}
	if est, _ := e.p.Store.ItemEstimate(ctx, it.ID); est.Status != store.EstimateParked {
		t.Fatalf("estimate %+v", est)
	}
}

// The dashboard's ask for another plan does what a comment and
// factory:replan do: the ask goes on the issue, fenced, and to the
// builder; a second ask parks the item.
func TestDashboardReplan(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	e.claude.plans = []string{planOne, planTwo}
	it := planned(t, e)
	first := planJob(t, e, it.ID)
	if err := e.p.Replan(ctx, it.ID, first, "  "); err == nil || !strings.Contains(err.Error(), "what to change") {
		t.Fatalf("empty ask: %v", err)
	}
	if err := e.p.Replan(ctx, it.ID, first, "Use a map, @someone."); err != nil {
		t.Fatal(err)
	}
	c := e.gh.comments[1]
	if len(c) != 2 || !strings.Contains(c[1], "someone on the dashboard asked for changes to the plan") || !strings.Contains(c[1], "```text\nUse a map, @someone.") {
		t.Fatalf("comments %q", c)
	}
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if e.claude.calls["plan"] != 2 || e.claude.calls["implement"] != 0 {
		t.Fatalf("%d plans, %d builds", e.claude.calls["plan"], e.claude.calls["implement"])
	}
	if pr := e.claude.prompts["plan"][1]; !strings.Contains(pr, "Use a map, @someone.") || !strings.Contains(pr, "what someone on the dashboard asked") {
		t.Fatalf("re-plan prompt:\n%s", pr)
	}
	if it = e.item(t, 1); it.Stage != state.Blocked || !strings.Contains(latestComment(e, 1), "the new plan someone on the dashboard asked for") {
		t.Fatalf("stage %s, comment %q", it.Stage, latestComment(e, 1))
	}
	// A page still showing the first plan can neither approve the new
	// one unread nor send it back with the first one's feedback.
	for _, err := range []error{e.p.ApproveEstimate(ctx, it.ID, first), e.p.Replan(ctx, it.ID, first, "Use a map.")} {
		if !errors.Is(err, errPlanChanged) || !strings.Contains(err.Error(), "reload to see the new one") {
			t.Fatalf("stale page: %v", err)
		}
	}
	if it = e.item(t, 1); it.Stage != state.Blocked || e.claude.calls["implement"] != 0 {
		t.Fatalf("stale page acted: stage %s", it.Stage)
	}
	if err := e.p.Replan(ctx, it.ID, planJob(t, e, it.ID), "Still not right."); err != nil {
		t.Fatal(err)
	}
	if it = e.item(t, 1); it.Stage != state.Parked || !strings.Contains(latestComment(e, 1), "Someone on the dashboard asked for changes to the plan again") {
		t.Fatalf("stage %s, comment %q", it.Stage, latestComment(e, 1))
	}
	// Nothing waits now: asking again is refused.
	if err := e.p.Replan(ctx, it.ID, planJob(t, e, it.ID), "And again."); err == nil {
		t.Fatal("replanned a parked item")
	}
}

// factory:replan where no plan waits comes off with a note.
func TestReplanLabelOnlyForAWaitingPlan(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	e.gh.issues[7] = &github.Issue{Number: 7, Title: "x", Labels: []string{LabelReplan}}
	if err := e.p.intakeReplan(ctx, "o/r"); err != nil {
		t.Fatal(err)
	}
	if hasLabel(e.gh.issues[7].Labels, LabelReplan) || !strings.Contains(latestComment(e, 7), "only asks for a new plan") {
		t.Fatalf("labels %v, comments %q", e.gh.issues[7].Labels, e.gh.comments[7])
	}
}

// With plan.medium: critic, an M item's plan goes to the critic: its
// concerns send it back once, and the new plan is built with no human.
func TestPlanMediumCheckedByCritic(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	e.p.Cfg.Plan.Medium = "critic"
	e.addIssue(1)
	e.claude.plans = []string{planOne, planTwo}
	e.claude.planChecks = []string{concerns}
	e.claude.builds = []func(string) error{goodBuild}
	e.codex.reviews = []string{pass}
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	it := e.item(t, 1)
	if it.Stage != state.AwaitingHuman || e.claude.calls["plan"] != 2 || e.claude.calls["plan_check"] != 1 {
		t.Fatalf("stage %s, %d plans, %d checks", it.Stage, e.claude.calls["plan"], e.claude.calls["plan_check"])
	}
	if pr := e.claude.prompts["plan_check"][0]; !strings.Contains(pr, "Approach: Add Add in add.go with a slice loop.") || !strings.Contains(pr, "```text\nIssue #1:") {
		t.Fatalf("check prompt:\n%s", pr)
	}
	if pr := e.claude.prompts["plan"][1]; !strings.Contains(pr, "It misses the caller in doc.go.") || !strings.Contains(pr, "what the critic asked") {
		t.Fatalf("re-plan prompt:\n%s", pr)
	}
	if pr := e.claude.prompts["implement"][0]; !strings.Contains(pr, "using a map, as asked") || !strings.Contains(pr, "(after the critic's concerns)") {
		t.Fatalf("implement prompt:\n%s", pr)
	}
	if hasLabel(e.gh.issues[1].Labels, LabelGo) || strings.Contains(strings.Join(e.gh.comments[1], "\n"), "waiting for your OK") {
		t.Fatalf("asked a human: %q", e.gh.comments[1])
	}
}

// A plan the critic approves is built as it is.
func TestPlanMediumApprovedByCritic(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	e.p.Cfg.Plan.Medium = "critic"
	e.addIssue(1)
	e.claude.plans = []string{planOne}
	e.claude.planChecks = []string{approve}
	e.claude.builds = []func(string) error{goodBuild}
	e.codex.reviews = []string{pass}
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if it := e.item(t, 1); it.Stage != state.AwaitingHuman || e.claude.calls["plan"] != 1 {
		t.Fatalf("stage %s, %d plans", it.Stage, e.claude.calls["plan"])
	}
	if pr := e.claude.prompts["implement"][0]; !strings.Contains(pr, "(by the critic)") {
		t.Fatalf("implement prompt:\n%s", pr)
	}
}

// Small items, M items by default, and everything with plans off build
// with no plan.
func TestNoPlanForSmallOrWhenOff(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for name, c := range map[string]struct {
		triage string
		off    bool
	}{
		"S":      {`{"decision":"ready","complexity":"S","risk":"low","question":"","summary":"s"}`, false},
		"M":      {`{"decision":"ready","complexity":"M","risk":"low","question":"","summary":"m"}`, false},
		"L, off": {triageL, true},
	} {
		e := setup(t)
		if c.off {
			e.p.Cfg.Plan.Off()
		}
		e.claude.triage = c.triage
		e.addIssue(1)
		e.claude.builds = []func(string) error{goodBuild}
		e.codex.reviews = []string{pass}
		if err := e.p.Drain(ctx); err != nil {
			t.Fatal(err)
		}
		if it := e.item(t, 1); it.Stage != state.AwaitingHuman || e.claude.calls["plan"] != 0 || strings.Contains(e.claude.prompts["implement"][0], "approved plan") {
			t.Errorf("%s: stage %s, %d plans", name, it.Stage, e.claude.calls["plan"])
		}
	}
}

// A plan that fails twice parks the item.
func TestPlanFailingTwiceParks(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	e.claude.triage = triageL
	e.claude.plans = []string{`not json`, `{"approach":""}`}
	e.addIssue(1)
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if it := e.item(t, 1); it.Stage != state.Parked || !strings.Contains(latestComment(e, 1), "Planning failed twice") {
		t.Fatalf("stage %s, %q", it.Stage, e.gh.comments[1])
	}
}

// Re-sized to L by the orchestrator after triage, an item is planned
// before it's built.
func TestResizedToLPlansFirst(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	e.claude.triage = `{"decision":"park","complexity":"","risk":"","question":"which file?","summary":"s"}`
	e.addIssue(1)
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	it := e.item(t, 1)
	if it.Stage != state.Parked {
		t.Fatalf("stage %s", it.Stage)
	}
	if err := e.p.enqueueStage(ctx, it.ID, "implement", "L"); err != errPlanWaiting {
		t.Fatalf("enqueue: %v", err)
	}
	e.claude.plans = []string{planOne}
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if it = e.item(t, 1); it.Stage != state.Blocked || e.claude.calls["plan"] != 1 || e.claude.calls["implement"] != 0 {
		t.Fatalf("stage %s, %d plans, %d builds", it.Stage, e.claude.calls["plan"], e.claude.calls["implement"])
	}
	if c := latestComment(e, 1); !strings.Contains(c, "waiting for your OK on the plan") {
		t.Fatalf("comment %q", c)
	}
}

// The dashboard's Retry of an old build of an L item with no plan this
// cycle plans it first, and leaves no item implementing without a job.
func TestRetryOfOldBuildPlansFirst(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	e.p.Cfg.Plan.Off()
	e.claude.triage = triageL
	e.addIssue(1)
	e.claude.builds = []func(string) error{badBuild, badBuild, badBuild, badBuild}
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	it := e.item(t, 1)
	if it.Stage != state.Parked {
		t.Fatalf("stage %s", it.Stage)
	}
	e.p.Cfg.Plan.Enabled = nil // plans turned on
	impl, _ := e.p.Store.ListJobs(ctx, store.JobFilter{ItemID: it.ID, Kind: "implement", Limit: 1})
	if err := e.p.Retry(ctx, impl[0].ID, false); err == nil || !strings.Contains(err.Error(), "needs its plan approved") {
		t.Fatalf("retry: %v", err)
	}
	if q, _ := e.p.Store.ListJobs(ctx, store.JobFilter{ItemID: it.ID, Kind: "implement", State: state.Queued}); len(q) != 0 {
		t.Fatal("an implement was queued")
	}
	e.claude.plans = []string{planOne}
	builds := e.claude.calls["implement"]
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if it = e.item(t, 1); it.Stage != state.Blocked || e.claude.calls["plan"] != 1 || e.claude.calls["implement"] != builds {
		t.Fatalf("stage %s, %d plans", it.Stage, e.claude.calls["plan"])
	}
}

// A plan once asked for keeps holding the build after the config drops
// its size; an OK still builds it.
func TestWaitingPlanHoldsAfterConfigChange(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	e.claude.plans = []string{planOne}
	it := planned(t, e)
	e.p.Cfg.Plan.Off()
	if _, err := e.p.enqueueImplement(ctx, it, 1, ImplementInput{}); err != errPlanWaiting {
		t.Fatalf("enqueue: %v", err)
	}
	e.p.lastPoll = e.p.lastPoll.AddDate(0, 0, -1)
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if it = e.item(t, 1); it.Stage != state.Blocked || e.claude.calls["implement"] != 0 {
		t.Fatalf("stage %s", it.Stage)
	}
	e.claude.builds = []func(string) error{goodBuild}
	e.codex.reviews = []string{pass}
	if err := e.p.ApproveEstimate(ctx, it.ID, 0); err != nil {
		t.Fatal(err)
	}
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if it = e.item(t, 1); it.Stage != state.AwaitingHuman || !strings.Contains(e.claude.prompts["implement"][0], "## The approved plan") {
		t.Fatalf("stage %s", it.Stage)
	}
}

// An L item's rethink at the round cap builds on, with the approved plan
// in its prompt, and isn't asked about again.
func TestRethinkKeepsTheApprovedPlan(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	e.claude.plans = []string{planOne}
	it := planned(t, e)
	fix := fixBuild()
	e.claude.builds = []func(string) error{goodBuild, fix, fix, goodBuild}
	e.codex.reviews = []string{block, block, block, pass}
	if err := e.p.ApproveEstimate(ctx, it.ID, 0); err != nil {
		t.Fatal(err)
	}
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	jobs, _ := e.p.Store.ListJobs(ctx, store.JobFilter{ItemID: it.ID, Kind: "implement"})
	if it = e.item(t, 1); it.Stage != state.AwaitingHuman || len(jobs) != 4 || !isRethink(jobs[0]) || e.claude.calls["plan"] != 1 {
		t.Fatalf("stage %s, %d builds, %d plans", it.Stage, len(jobs), e.claude.calls["plan"])
	}
	if pr := e.claude.prompts["implement"][3]; !strings.Contains(pr, "## The approved plan") || !strings.Contains(pr, "rethink how to carry it out") || !strings.Contains(pr, "approach itself may be wrong") {
		t.Fatalf("rethink prompt:\n%s", pr)
	}
}
