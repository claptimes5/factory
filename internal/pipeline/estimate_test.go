package pipeline

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/claptimes5/factory/internal/state"
	"github.com/claptimes5/factory/internal/store"
)

// past seeds a finished item of a size in a repo, built for impl dollars
// and reviewed for rev.
func (e *env) past(t *testing.T, repo string, n int, size string, impl, rev float64) {
	t.Helper()
	ctx := context.Background()
	st := e.p.Store
	it, _, err := st.UpsertItem(ctx, repo, n, "past")
	if err != nil {
		t.Fatal(err)
	}
	st.UpdateItem(ctx, it.ID, store.ItemUpdate{Complexity: &size})
	for _, x := range []struct {
		kind string
		usd  float64
	}{{"implement", impl}, {"review", rev}} {
		id, _ := st.EnqueueJob(ctx, store.Job{ItemID: &it.ID, Kind: x.kind, Harness: map[string]string{"implement": "claude", "review": "codex"}[x.kind], Model: "m", BudgetUSD: 99})
		st.ClaimJob(ctx)
		st.FinishJob(ctx, id, store.Result{State: state.Succeeded, CostUSD: x.usd})
	}
	for _, to := range []state.Stage{state.Ready, state.Implementing, state.Reviewing, state.AwaitingHuman, state.Done} {
		if err := st.SetStage(ctx, it.ID, to, "daemon", ""); err != nil {
			t.Fatal(err)
		}
	}
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestQuantile(t *testing.T) {
	t.Parallel()
	vs := []float64{4, 1, 3, 2}
	if q := quantile(vs, 0.5); !near(q, 2.5) {
		t.Fatalf("median %v", q)
	}
	if q := quantile(vs, 0.75); !near(q, 3.25) {
		t.Fatalf("p75 %v", q)
	}
	if quantile(nil, 0.5) != 0 || quantile([]float64{7}, 0.75) != 7 {
		t.Fatal("edge cases")
	}
}

func TestEstimateRange(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		e    store.Estimate
		want string
	}{
		{store.Estimate{MedianUSD: 1.8, P75USD: 3.1, TotalMedianUSD: 1.8, TotalP75USD: 3.1}, "$1.80–$3.10"},
		{store.Estimate{MedianUSD: 2, P75USD: 2, TotalMedianUSD: 2, TotalP75USD: 2}, "$2.00"},
		{store.Estimate{MedianUSD: 0.5, P75USD: 1, TotalMedianUSD: 2, TotalP75USD: 4}, "$0.50–$1.00 + plan usage"},
		{store.Estimate{TotalMedianUSD: 2, TotalP75USD: 4}, "on your plan"},
	} {
		if got := c.e.Range(); got != c.want {
			t.Errorf("%+v: %q, want %q", c.e, got, c.want)
		}
	}
}

// The estimate is the repo's own history once it has enough of the size,
// else every repo's, else the budgets'.
func TestEstimateFallsBackFromRepoToAllToBudgets(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	it := &store.WorkItem{ID: 999, Repo: "o/r", Complexity: "M", Risk: "low"}

	// Nothing yet: the budgets. implement.M's cap is $6, a review's $3.
	got, err := e.p.estimate(ctx, it)
	if err != nil {
		t.Fatal(err)
	}
	if got.Basis != "default" || !near(got.Median, 4.5) || !near(got.P75, 9) ||
		!strings.Contains(got.Detail, "too few past M items to go by (0; it takes 5)") || !strings.Contains(got.Detail, "$6.00 cap") {
		t.Fatalf("no history: %+v", got)
	}

	// Four here, three elsewhere: too few here, enough across all repos.
	for i, c := range []float64{1, 2, 3, 4} {
		e.past(t, "o/r", 10+i, "M", c, 0.5)
	}
	e.past(t, "o/r", 20, "S", 50, 0) // another size never counts
	for i, c := range []float64{5, 6, 7} {
		e.past(t, "o/other", 30+i, "M", c, 0.5)
	}
	got, _ = e.p.estimate(ctx, it)
	if got.Basis != "all" || got.Samples != 7 || !near(got.Median, 4.5) || !near(got.P75, 6) {
		t.Fatalf("all repos: %+v", got)
	}
	if want := "based on 7 past M items across all repos (4 in o/r, too few on their own): median $4.50, p75 $6.00"; got.Detail != want {
		t.Fatalf("detail %q", got.Detail)
	}

	// A fifth here: the repo's own.
	e.past(t, "o/r", 14, "M", 5, 0.5)
	got, _ = e.p.estimate(ctx, it)
	if got.Basis != "repo" || got.Samples != 5 || !near(got.Median, 3.5) || !near(got.P75, 4.5) {
		t.Fatalf("repo: %+v", got)
	}
	if want := "based on 5 past M items in o/r: median $3.50, p75 $4.50"; got.Detail != want {
		t.Fatalf("detail %q", got.Detail)
	}
	// An item is never its own history.
	self, _ := e.p.Store.ItemByIssue(ctx, "o/r", 14)
	it.ID = self.ID
	if got, _ = e.p.estimate(ctx, it); got.Basis != "all" {
		t.Fatalf("counted itself: %+v", got)
	}
}

// Work on a subscription isn't billed: the estimate says so, and only the
// dollars count against the threshold.
func TestEstimateOnAPlan(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	e.p.PlanBilled = map[string]bool{"claude": true}
	for i := range 5 {
		e.past(t, "o/r", 10+i, "M", 4, 1)
	}
	e.p.Store.SetLimit(ctx, store.Limit{Harness: "claude", Window: "5h", UsedPercent: 42})
	it := &store.WorkItem{ID: 999, Repo: "o/r", Complexity: "M", Risk: "low"}
	got, _ := e.p.estimate(ctx, it)
	if !near(got.P75, 1) || !near(got.TotalP75, 5) {
		t.Fatalf("estimate %+v", got)
	}
	for _, want := range []string{"median $1.00, p75 $1.00", "on your plan (claude): about $4.00–$4.00 more", "usage now: claude 5h 42%"} {
		if !strings.Contains(got.Detail, want) {
			t.Errorf("detail %q lacks %q", got.Detail, want)
		}
	}
	// Each past job is billed as its own harness was: routing reviews to
	// the plan now doesn't make the past codex reviews free.
	e.p.Cfg.Routing["review"] = e.p.Cfg.Routing["implement.L"]
	if got, _ = e.p.estimate(ctx, it); !near(got.P75, 1) {
		t.Fatalf("history read by today's routing: %+v", got)
	}
	// The reviewer on a plan too: nothing is billed, so nothing waits.
	e.p.PlanBilled["codex"] = true
	got, _ = e.p.estimate(ctx, it)
	if got.P75 != 0 || !strings.Contains(got.Detail, "nothing billed in dollars") {
		t.Fatalf("all on the plan: %+v", got)
	}
	e.p.Cfg.Budgets.ConfirmOver = 0.5
	e.addIssue(1)
	e.claude.builds = []func(string) error{goodBuild}
	e.codex.reviews = []string{pass}
	e.claude.reviews = []string{pass}
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if est, _ := e.p.Store.ItemEstimate(ctx, e.item(t, 1).ID); est == nil || est.Status != store.EstimateUnder || est.Range() != "on your plan" {
		t.Fatalf("estimate %+v", est)
	}
}

// heldItem triages issue 1 over a $1 threshold and checks that it waits.
func heldItem(t *testing.T, e *env) *store.WorkItem {
	t.Helper()
	ctx := context.Background()
	e.p.Cfg.Budgets.ConfirmOver = 1
	e.addIssue(1)
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	it := e.item(t, 1)
	if it.Stage != state.Blocked || e.claude.calls["implement"] != 0 {
		t.Fatalf("stage %s, %d builds", it.Stage, e.claude.calls["implement"])
	}
	if !hasLabel(e.gh.issues[1].Labels, LabelAwaitingYou) {
		t.Fatalf("labels %v", e.gh.issues[1].Labels)
	}
	c := e.gh.comments[1]
	if len(c) != 1 || !strings.Contains(c[0], "Estimated cost: **$4.50–$9.00**") || !strings.Contains(c[0], "over the $1.00 limit") || !strings.Contains(c[0], "`factory:go`") {
		t.Fatalf("comments %q", c)
	}
	// Nothing frees it but an OK: not a poll, not its dependencies.
	e.p.lastPoll = e.p.lastPoll.AddDate(0, 0, -1)
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if it = e.item(t, 1); it.Stage != state.Blocked || len(e.gh.comments[1]) != 1 {
		t.Fatalf("after a poll: %s, %q", it.Stage, e.gh.comments[1])
	}
	return it
}

// Over the threshold, an item waits; a maintainer's factory:go builds it.
func TestEstimateOverThresholdWaitsForGo(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	var notes []string
	e.p.Notify = func(_ context.Context, m string) { notes = append(notes, m) }
	heldItem(t, e)
	if len(notes) != 1 || !strings.Contains(notes[0], "#1 waits for your OK: estimated $4.50–$9.00") {
		t.Fatalf("notes %q", notes)
	}
	e.gh.issues[1].Labels = append(e.gh.issues[1].Labels, LabelGo)
	e.claude.builds = []func(string) error{goodBuild}
	e.codex.reviews = []string{pass}
	e.p.lastPoll = e.p.lastPoll.AddDate(0, 0, -1)
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	it := e.item(t, 1)
	if it.Stage != state.AwaitingHuman {
		t.Fatalf("stage %s", it.Stage)
	}
	est, _ := e.p.Store.ItemEstimate(ctx, it.ID)
	if est.Status != store.EstimateApproved || est.ApprovedBy != "maintainer" {
		t.Fatalf("estimate %+v", est)
	}
	if hasLabel(e.gh.issues[1].Labels, LabelGo) {
		t.Fatal("factory:go is still on")
	}
	if c := e.gh.comments[1]; !strings.Contains(strings.Join(c, "\n"), "approved by @maintainer; building it") {
		t.Fatalf("comments %q", c)
	}
}

// factory:go from someone who can't approve work comes off and frees nothing.
func TestEstimateGoFromOutsiderRefused(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	heldItem(t, e)
	e.gh.issues[1].Labels = append(e.gh.issues[1].Labels, LabelGo)
	e.gh.labeledBy = map[int]string{1: "drive-by"}
	e.gh.outsiders = map[string]bool{"drive-by": true}
	e.p.lastPoll = e.p.lastPoll.AddDate(0, 0, -1)
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if it := e.item(t, 1); it.Stage != state.Blocked || e.claude.calls["implement"] != 0 {
		t.Fatalf("stage %s", it.Stage)
	}
	if hasLabel(e.gh.issues[1].Labels, LabelGo) {
		t.Fatal("the outsider's label is still on")
	}
	if c := e.gh.comments[1]; !strings.Contains(c[len(c)-1], "added by @drive-by, who can't approve work") {
		t.Fatalf("comments %q", c)
	}
}

// The dashboard's OK builds it; a second OK, or one for an item not
// waiting, is refused.
func TestEstimateApprovedFromDashboard(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	it := heldItem(t, e)
	e.claude.builds = []func(string) error{goodBuild}
	e.codex.reviews = []string{pass}
	if err := e.p.ApproveEstimate(ctx, it.ID, 0); err != nil {
		t.Fatal(err)
	}
	if err := e.p.ApproveEstimate(ctx, it.ID, 0); err == nil {
		t.Fatal("approved twice")
	}
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if it = e.item(t, 1); it.Stage != state.AwaitingHuman || hasLabel(e.gh.issues[1].Labels, LabelGo) {
		t.Fatalf("stage %s", it.Stage)
	}
	if est, _ := e.p.Store.ItemEstimate(ctx, it.ID); est.ApprovedBy != "the dashboard" {
		t.Fatalf("estimate %+v", est)
	}
}

// An OK is for the text and the estimate it was given: a re-triage of the
// same text doesn't ask again, an edit approved with factory:retry does.
func TestEstimateAsksAgainAfterAnEdit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	it := heldItem(t, e)
	// Approved, then the build fails on every tier and parks.
	e.claude.builds = []func(string) error{badBuild, badBuild, badBuild, badBuild}
	if err := e.p.ApproveEstimate(ctx, it.ID, 0); err != nil {
		t.Fatal(err)
	}
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if it = e.item(t, 1); it.Stage != state.Parked {
		t.Fatalf("stage %s", it.Stage)
	}
	// factory:retry on the same text: re-triaged, re-estimated, not held.
	retry := func() {
		e.gh.issues[1].Labels = append(e.gh.issues[1].Labels, LabelRetry)
		e.p.lastPoll = e.p.lastPoll.AddDate(0, 0, -1)
		if err := e.p.Drain(ctx); err != nil {
			t.Fatal(err)
		}
	}
	e.claude.calls["implement"] = 0
	e.claude.builds = []func(string) error{badBuild, badBuild, badBuild, badBuild}
	retry()
	if it = e.item(t, 1); it.Stage != state.Parked || e.claude.calls["implement"] == 0 {
		t.Fatalf("same text: stage %s after %d builds", it.Stage, e.claude.calls["implement"])
	}
	// The issue is edited; a retry approves the new text, and the cost is
	// asked about again.
	e.gh.issues[1].Body = "Add func Add(a, b int) int, and Sub too."
	e.claude.calls["implement"] = 0
	retry()
	if it = e.item(t, 1); it.Stage != state.Blocked || e.claude.calls["implement"] != 0 {
		t.Fatalf("edited: stage %s after %d builds", it.Stage, e.claude.calls["implement"])
	}
	if est, _ := e.p.Store.ItemEstimate(ctx, it.ID); est.Status != store.EstimateWaiting || est.ApprovedBy != "" {
		t.Fatalf("estimate %+v", est)
	}
	// Retrying an old build from the dashboard doesn't skip the OK.
	impl, _ := e.p.Store.ListJobs(ctx, store.JobFilter{ItemID: it.ID, Kind: "implement", Limit: 1})
	if err := e.p.Retry(ctx, impl[0].ID, false); err == nil || !strings.Contains(err.Error(), "approve that first") {
		t.Fatalf("retry: %v", err)
	}
}

// A main health fix is never held for its cost: a red main is urgent.
func TestEstimateExemptsMainHealthFixes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	e.p.Cfg.Budgets.ConfirmOver = 1
	e.addIssue(1)
	e.p.Store.SetHealthIssue(ctx, store.HealthIssue{Repo: "o/r", Check: "test", Kind: store.HealthRed, HeadSHA: "abc", IssueNumber: 1})
	e.claude.builds = []func(string) error{goodBuild}
	e.codex.reviews = []string{pass}
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	it := e.item(t, 1)
	if it.Stage != state.AwaitingHuman {
		t.Fatalf("stage %s", it.Stage)
	}
	if est, _ := e.p.Store.ItemEstimate(ctx, it.ID); est.Status != store.EstimateExempt {
		t.Fatalf("estimate %+v", est)
	}
}

// A repo's own confirm_over replaces the top level's; 0 turns it off.
func TestEstimateRepoOverride(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	e.p.Cfg.Budgets.ConfirmOver = 1
	off := 0.0
	e.p.Cfg.Repos[0].ConfirmOver = &off
	e.addIssue(1)
	e.claude.builds = []func(string) error{goodBuild}
	e.codex.reviews = []string{pass}
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if est, _ := e.p.Store.ItemEstimate(ctx, e.item(t, 1).ID); est.Status != store.EstimateOff || e.item(t, 1).Stage != state.AwaitingHuman {
		t.Fatalf("estimate %+v", est)
	}
}

// No road to a build skips a waiting estimate: not the orchestrator's
// enqueue, not a direct call, and a wait isn't "stuck" work for it.
func TestEstimateWaitingRefusesEveryEnqueue(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	it := heldItem(t, e)
	tools := toolsByName(e.p, 0)
	if _, err := callTool(t, tools, "enqueue", map[string]any{"item": it.ID, "stage": "implement", "reason": "r"}); err == nil || !strings.Contains(err.Error(), "needs a human's OK") {
		t.Fatalf("orchestrator enqueue: %v", err)
	}
	if err := e.p.enqueueStage(ctx, it.ID, "implement", "S"); !errors.Is(err, errEstimateWaiting) {
		t.Fatalf("enqueueStage: %v", err)
	}
	if _, err := e.p.enqueueImplement(ctx, it, 1, ImplementInput{}); !errors.Is(err, errEstimateWaiting) {
		t.Fatalf("enqueueImplement: %v", err)
	}
	if n, _ := e.p.Store.CountJobs(ctx, state.Queued, "implement"); n != 0 {
		t.Fatalf("%d implement jobs queued", n)
	}
	if it = e.item(t, 1); it.Complexity != "M" {
		t.Fatalf("complexity changed to %s", it.Complexity)
	}
	e.p.lastDrain = time.Time{}
	e.p.drainedTrigger(ctx)
	if seen, _ := e.p.Store.HasEvent(ctx, "drain_seen", store.Today()+" "+e.p.ref("o/r", 1)); seen {
		t.Fatal("a wait on a human was reported as stuck")
	}
}

// Priority 100 is not what exempts a fix: the orchestrator can't set it,
// and an item given it otherwise still waits.
func TestEstimatePriorityIsNoExemption(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	e.p.Cfg.Budgets.ConfirmOver = 1
	e.addIssue(1)
	if err := e.p.Intake(ctx); err != nil {
		t.Fatal(err)
	}
	e.p.lastPoll = time.Now()
	it := e.item(t, 1)
	tools := toolsByName(e.p, 0)
	if _, err := callTool(t, tools, "set_priority", map[string]any{"item": it.ID, "priority": HealthPriority, "reason": "r"}); err == nil || !strings.Contains(err.Error(), "kept for main health fixes") {
		t.Fatalf("set_priority: %v", err)
	}
	if _, err := callTool(t, tools, "set_priority", map[string]any{"item": it.ID, "priority": HealthPriority - 1, "reason": "r"}); err != nil {
		t.Fatal(err)
	}
	e.p.Store.SetPriority(ctx, it.ID, HealthPriority)
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if est, _ := e.p.Store.ItemEstimate(ctx, it.ID); est == nil || est.Status != store.EstimateWaiting || e.item(t, 1).Stage != state.Blocked {
		t.Fatalf("estimate %+v", est)
	}
}

// A main health fix stays exempt after main is green again and its
// health issue is cleared: the item was marked when it came in.
func TestEstimateHealthMarkOutlivesTheHealthIssue(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	e.p.Cfg.Budgets.ConfirmOver = 1
	e.addIssue(1)
	e.p.Store.SetHealthIssue(ctx, store.HealthIssue{Repo: "o/r", Check: "test", Kind: store.HealthRed, HeadSHA: "abc", IssueNumber: 1})
	if err := e.p.Intake(ctx); err != nil {
		t.Fatal(err)
	}
	e.p.lastPoll = time.Now()
	e.p.Store.ClearHealthIssue(ctx, "o/r", "test")
	e.claude.builds = []func(string) error{goodBuild}
	e.codex.reviews = []string{pass}
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if est, _ := e.p.Store.ItemEstimate(ctx, e.item(t, 1).ID); est == nil || est.Status != store.EstimateExempt || e.item(t, 1).Stage != state.AwaitingHuman {
		t.Fatalf("estimate %+v", est)
	}
}

// factory:go on an item that isn't waiting comes off with a note and
// approves nothing later.
func TestEstimateGoOnAnItemNotWaiting(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	e.p.Cfg.Budgets.ConfirmOver = 1
	e.addIssue(1)
	e.gh.issues[1].Labels = append(e.gh.issues[1].Labels, LabelGo)
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if hasLabel(e.gh.issues[1].Labels, LabelGo) {
		t.Fatal("factory:go is still on")
	}
	if c := strings.Join(e.gh.comments[1], "\n"); !strings.Contains(c, "`factory:go` was taken off: it only approves an item that's waiting") {
		t.Fatalf("comments %q", e.gh.comments[1])
	}
	e.p.lastPoll = e.p.lastPoll.AddDate(0, 0, -1)
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if it := e.item(t, 1); it.Stage != state.Blocked || e.claude.calls["implement"] != 0 {
		t.Fatalf("stage %s after %d builds", it.Stage, e.claude.calls["implement"])
	}
}

// factory:go added while the item was in triage, before the wait was
// announced, comes off with the announcement: only one added after counts.
func TestEstimateGoBeforeTheWaitDoesNotCount(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	e.p.Cfg.Budgets.ConfirmOver = 1
	e.addIssue(1)
	if err := e.p.Intake(ctx); err != nil {
		t.Fatal(err)
	}
	e.p.lastPoll = time.Now()
	e.gh.issues[1].Labels = append(e.gh.issues[1].Labels, LabelGo)
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if hasLabel(e.gh.issues[1].Labels, LabelGo) {
		t.Fatal("the early factory:go is still on")
	}
	if c := e.gh.comments[1]; len(c) != 1 || !strings.Contains(c[0], "has to be added after this comment") {
		t.Fatalf("comments %q", c)
	}
	e.p.lastPoll = e.p.lastPoll.AddDate(0, 0, -1)
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if it := e.item(t, 1); it.Stage != state.Blocked || e.claude.calls["implement"] != 0 {
		t.Fatalf("stage %s after %d builds", it.Stage, e.claude.calls["implement"])
	}
}

// A wait that ends in a park still needs the OK: a retry of a build is
// refused whatever the stage.
func TestEstimateParkedWhileWaitingStillNeedsOK(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	it := heldItem(t, e)
	id, _ := e.p.Store.EnqueueJob(ctx, store.Job{ItemID: &it.ID, Kind: "implement", Harness: "claude", Model: "m", BudgetUSD: 9})
	e.p.Store.ClaimJob(ctx)
	e.p.Store.FinishJob(ctx, id, store.Result{State: state.Failed})
	if err := e.p.park(ctx, it, "parked by hand"); err != nil {
		t.Fatal(err)
	}
	if est, _ := e.p.Store.ItemEstimate(ctx, it.ID); est.Status != store.EstimateParked {
		t.Fatalf("estimate %+v", est)
	}
	if hasLabel(e.gh.issues[1].Labels, LabelAwaitingYou) {
		t.Fatal("factory:awaiting-you is still on")
	}
	if err := e.p.Retry(ctx, id, false); err == nil || !strings.Contains(err.Error(), "approve that first") {
		t.Fatalf("retry: %v", err)
	}
	if err := e.p.ApproveEstimate(ctx, it.ID, 0); err == nil {
		t.Fatal("approved a parked item")
	}
}

// A re-triage that needn't wait takes factory:awaiting-you off.
func TestEstimateRetriageUnderClearsTheLabel(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	it := heldItem(t, e)
	if err := e.p.park(ctx, it, "parked by hand"); err != nil {
		t.Fatal(err)
	}
	e.gh.issues[1].Labels = append(e.gh.issues[1].Labels, LabelAwaitingYou, LabelRetry)
	e.p.Cfg.Budgets.ConfirmOver = 100
	// The builds fail, so nothing that reaches review puts the label back.
	e.claude.builds = []func(string) error{badBuild, badBuild, badBuild, badBuild}
	e.p.lastPoll = e.p.lastPoll.AddDate(0, 0, -1)
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if est, _ := e.p.Store.ItemEstimate(ctx, it.ID); est.Status != store.EstimateUnder || e.item(t, 1).Stage != state.Parked || e.claude.calls["implement"] == 0 {
		t.Fatalf("estimate %+v", est)
	}
	if hasLabel(e.gh.issues[1].Labels, LabelAwaitingYou) {
		t.Fatal("factory:awaiting-you is still on")
	}
}

// A wait whose comment didn't go out is announced on a later poll, and
// until then no factory:go counts.
func TestEstimateWaitAnnouncedAfterAFailure(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	var notes []string
	e.p.Notify = func(_ context.Context, m string) { notes = append(notes, m) }
	e.p.Cfg.Budgets.ConfirmOver = 1
	e.addIssue(1)
	e.gh.failComment = errors.New("forge down")
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	it := e.item(t, 1)
	if est, _ := e.p.Store.ItemEstimate(ctx, it.ID); it.Stage != state.Blocked || est.Status != store.EstimateWaiting || est.Announced || len(notes) != 0 {
		t.Fatalf("stage %s, estimate %+v, notes %q", it.Stage, est, notes)
	}
	// A go now is from before anyone was told: it doesn't count.
	e.gh.issues[1].Labels = append(e.gh.issues[1].Labels, LabelGo)
	e.p.lastPoll = e.p.lastPoll.AddDate(0, 0, -1)
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if it = e.item(t, 1); it.Stage != state.Blocked || hasLabel(e.gh.issues[1].Labels, LabelGo) {
		t.Fatalf("approved before the wait was announced: %s, %v", it.Stage, e.gh.issues[1].Labels)
	}
	e.gh.failComment = nil
	e.p.lastPoll = e.p.lastPoll.AddDate(0, 0, -1)
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if est, _ := e.p.Store.ItemEstimate(ctx, it.ID); !est.Announced || e.item(t, 1).Stage != state.Blocked {
		t.Fatalf("estimate %+v", est)
	}
	if c := e.gh.comments[1]; len(c) != 1 || !strings.Contains(c[0], "waiting for your OK on the cost") {
		t.Fatalf("comments %q", c)
	}
	if len(notes) != 1 || hasLabel(e.gh.issues[1].Labels, LabelGo) || !hasLabel(e.gh.issues[1].Labels, LabelAwaitingYou) {
		t.Fatalf("notes %q, labels %v", notes, e.gh.issues[1].Labels)
	}
	// Announced: a go now builds it.
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
}

// Failing to read factory:go labels doesn't stop the rest of intake.
func TestEstimateGoFailureKeepsIntakeGoing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	e.addIssue(1)
	e.claude.builds = []func(string) error{badBuild, badBuild, badBuild, badBuild}
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if it := e.item(t, 1); it.Stage != state.Parked {
		t.Fatalf("stage %s", it.Stage)
	}
	e.gh.failList = map[string]error{LabelGo: errors.New("forge down")}
	e.gh.issues[1].Labels = append(e.gh.issues[1].Labels, LabelRetry)
	e.claude.calls["implement"] = 0
	e.claude.builds = []func(string) error{goodBuild}
	e.codex.reviews = []string{pass}
	e.p.lastPoll = e.p.lastPoll.AddDate(0, 0, -1)
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if it := e.item(t, 1); it.Stage != state.AwaitingHuman {
		t.Fatalf("the retry wasn't taken: %s", it.Stage)
	}
}

// A size raised after triage is estimated again: the orchestrator can't
// enqueue an item under the limit as M with complexity L, over it.
func TestEstimateRaisedSizeIsEstimatedAgain(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	m, _ := e.p.estimate(ctx, &store.WorkItem{ID: 999, Repo: "o/r", Complexity: "M", Risk: "low"})
	l, _ := e.p.estimate(ctx, &store.WorkItem{ID: 999, Repo: "o/r", Complexity: "L", Risk: "low"})
	if l.P75 <= m.P75 {
		t.Fatalf("M %+v, L %+v", m, l)
	}
	e.p.Cfg.Budgets.ConfirmOver = (m.P75 + l.P75) / 2
	// Held back by a dependency after triage, so it's the orchestrator's
	// to queue once that's done.
	dep, _, _ := e.p.Store.UpsertItem(ctx, "o/r", 50, "dep")
	e.addIssue(50)
	e.gh.issues[50].Labels = nil
	e.addIssue(1)
	if err := e.p.Intake(ctx); err != nil {
		t.Fatal(err)
	}
	e.p.lastPoll = time.Now()
	it := e.item(t, 1)
	e.p.Store.SetDependencies(ctx, it.ID, []int64{dep.ID}, "orchestrator", "r")
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if est, _ := e.p.Store.ItemEstimate(ctx, it.ID); est == nil || est.Status != store.EstimateUnder || est.Size != "M" || e.item(t, 1).Stage != state.Blocked {
		t.Fatalf("after triage: %+v, %s", est, e.item(t, 1).Stage)
	}
	for _, to := range []state.Stage{state.Ready, state.Implementing, state.Reviewing, state.AwaitingHuman, state.Done} {
		e.p.Store.SetStage(ctx, dep.ID, to, "daemon", "")
	}
	tools := toolsByName(e.p, 0)
	if _, err := callTool(t, tools, "enqueue", map[string]any{"item": it.ID, "stage": "implement", "complexity": "L", "reason": "r"}); err == nil || !strings.Contains(err.Error(), "needs a human's OK") {
		t.Fatalf("enqueue as L: %v", err)
	}
	if n, _ := e.p.Store.CountJobs(ctx, state.Queued, "implement"); n != 0 {
		t.Fatalf("%d implement jobs queued", n)
	}
	est, _ := e.p.Store.ItemEstimate(ctx, it.ID)
	if est.Status != store.EstimateWaiting || est.Size != "L" || !est.Announced || e.item(t, 1).Stage != state.Blocked {
		t.Fatalf("as L: %+v, %s", est, e.item(t, 1).Stage)
	}
	if c := e.gh.comments[1]; len(c) == 0 || !strings.Contains(c[len(c)-1], "Triage rated this L") {
		t.Fatalf("comments %q", c)
	}
	// The same size again changes nothing to estimate: the wait holds.
	if _, err := callTool(t, tools, "enqueue", map[string]any{"item": it.ID, "stage": "implement", "complexity": "L", "reason": "r"}); err == nil {
		t.Fatal("queued while waiting")
	}
}

// A run on a subscription costs no dollars: it records none, so it never
// counts toward the daily cap; a run on an API key still does.
func TestPlanRunsCostNoDollars(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	e.p.PlanBilled = map[string]bool{"claude": true} // codex reviews on its API key
	e.addIssue(1)
	e.claude.builds = []func(string) error{goodBuild}
	e.codex.reviews = []string{pass}
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	jobs, _ := e.p.Store.ListJobs(ctx, store.JobFilter{ItemID: e.item(t, 1).ID})
	var plan, billed float64
	for _, j := range jobs {
		if j.Harness == "claude" {
			plan += j.CostUSD
		} else {
			billed += j.CostUSD
		}
	}
	spent, _ := e.p.Store.SpendOn(ctx, store.Today())
	if plan != 0 || billed == 0 || spent != billed {
		t.Fatalf("plan $%.2f, billed $%.2f, spent today $%.2f", plan, billed, spent)
	}
}
