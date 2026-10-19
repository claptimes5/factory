package pipeline

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/claptimes5/factory/internal/config"
	"github.com/claptimes5/factory/internal/github"
	"github.com/claptimes5/factory/internal/state"
	"github.com/claptimes5/factory/internal/store"
	"github.com/claptimes5/factory/internal/verdict"
)

// The plan check for big items. After triage, an item of a size in
// plan.sizes (L by default) gets a plan job first: the builder's route for
// its size reads the repo, read-only, and writes a short plan (approach,
// files, risks, tests), which goes on the issue. Nothing is built until
// someone approves it, with the factory:go that approves a cost estimate:
// when the estimate waits too, one comment asks for both and one OK
// covers both. Someone who wants changes comments, then adds
// factory:replan: the builder plans once more with their comment, and a
// second factory:replan parks the item. With plan.medium: critic an M
// item's plan goes to the critic instead, which approves it or raises
// concerns that send it back once; the build then goes on by itself.
// Whatever plan was approved goes, fenced, to the builder and the reviewer.
//
// Nothing new is stored: the plan is the plan job's verdict, and where
// it stands is in events on that job (plan_waiting, plan_announced,
// plan_approved, plan_replan).

// LabelReplan asks for a new plan, with the labeler's last comment.
const LabelReplan = "factory:replan"

// maxPlanRounds is a first plan and the one re-plan.
const maxPlanRounds = 2

// Plan events, each on the plan job it's about.
const (
	evPlanWaiting   = store.EvPlanWaiting
	evPlanAnnounced = store.EvPlanAnnounced
	evPlanApproved  = store.EvPlanApproved
	evPlanReplan    = store.EvPlanReplan
)

// PlanInput is carried to plan jobs.
type PlanInput struct {
	Round    int    `json:"round,omitempty"`    // 2 for the re-plan
	Feedback string `json:"feedback,omitempty"` // the earlier plan and what to change, fenced
	From     string `json:"from,omitempty"`     // who asked: @login, or the critic
}

// PlanCheckInput is carried to plan_check jobs.
type PlanCheckInput struct {
	PlanJob int64 `json:"plan_job"`
}

// planState is the plan of an item's current triage cycle.
type planState struct {
	job       *store.Job
	in        PlanInput
	plan      *verdict.Plan // nil until the job succeeded
	waiting   bool          // for a human's OK
	announced bool          // the comment asking for it went out
	approved  bool
	by        string // how it was approved: "by @x", "by the critic"
}

// currentPlan is the item's plan since its last triage, or nil.
func (p *Pipeline) currentPlan(ctx context.Context, itemID int64) (*planState, error) {
	ps, err := p.Store.CurrentPlan(ctx, itemID)
	if err != nil || ps == nil {
		return nil, err
	}
	s := &planState{job: ps.Job, waiting: ps.Waiting, announced: ps.Announced, approved: ps.Approved, by: ps.By}
	_ = json.Unmarshal([]byte(s.job.Input), &s.in)
	if s.job.State == state.Succeeded {
		var v verdict.Plan
		if verdict.Parse([]byte(s.job.Verdict), &v) == nil {
			s.plan = &v
		}
	}
	return s, nil
}

// planNeedsOK says whether the item's plan keeps it from being built: one
// was asked for this cycle and isn't approved yet, or its size needs one
// and it has none since its last triage. A plan once asked for holds the
// build even if the config drops its size later: someone may be reading
// it, and only an OK, or factory:retry, moves on from it. Main health
// fixes never need one.
func (p *Pipeline) planNeedsOK(ctx context.Context, it *store.WorkItem) bool {
	s, err := p.currentPlan(ctx, it.ID)
	if err != nil {
		return true
	}
	if s != nil {
		return !s.approved
	}
	if p.Cfg.PlanFor(it.Complexity) == "" {
		return false
	}
	health, err := p.isHealthFix(ctx, it)
	return err != nil || !health
}

// holdForPlan reports whether the item's plan holds its build, and starts
// the plan when its size needs one and it has none this cycle (an item
// re-sized, or retried from an old job, after triage). A parked or
// blocked item goes back to ready while it's planned.
func (p *Pipeline) holdForPlan(ctx context.Context, it *store.WorkItem, actor string) (bool, error) {
	if !p.planNeedsOK(ctx, it) {
		return false, nil
	}
	s, err := p.currentPlan(ctx, it.ID)
	if err != nil || s != nil {
		return true, err
	}
	if it.Stage == state.Parked || it.Stage == state.Blocked {
		if err := p.Store.SetStage(ctx, it.ID, state.Ready, actor, "planning before the build"); err != nil {
			return true, err
		}
	}
	return true, p.enqueuePlan(ctx, it, PlanInput{Round: 1}, 1)
}

// errPlanWaiting refuses to build an item whose plan isn't approved.
var errPlanWaiting = errors.New("refused: the item's plan isn't approved yet")

// approvedPlan is the approved plan, fenced, and how it was approved;
// empty when there is none.
func (p *Pipeline) approvedPlan(ctx context.Context, it *store.WorkItem) (string, string) {
	s, err := p.currentPlan(ctx, it.ID)
	if err != nil || s == nil || !s.approved || s.plan == nil {
		return "", ""
	}
	return fenced(planText(s.plan)), s.by
}

// planText is a plan as people read it.
func planText(v *verdict.Plan) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Approach: %s\n", strings.TrimSpace(v.Approach))
	if len(v.Files) > 0 {
		b.WriteString("\nFiles:\n")
		for _, f := range v.Files {
			fmt.Fprintf(&b, "- %s\n", oneLine(f))
		}
	}
	if len(v.Risks) > 0 {
		b.WriteString("\nRisks:\n")
		for _, r := range v.Risks {
			fmt.Fprintf(&b, "- %s\n", oneLine(r))
		}
	}
	fmt.Fprintf(&b, "\nTests: %s\n", strings.TrimSpace(v.Tests))
	return b.String()
}

// issueText is the issue as a prompt quotes it: fenced, so its text
// can't close the block and pass for the factory's own words.
func issueText(is github.Issue) string {
	return fenced(fmt.Sprintf("Issue #%d: %s\n\n%s", is.Number, is.Title, is.Body))
}

// checkPlan starts an item's plan when its size needs one, and reports
// whether the build waits on it. A main health fix is never held.
func (p *Pipeline) checkPlan(ctx context.Context, it *store.WorkItem) (bool, error) {
	if p.Cfg.PlanFor(it.Complexity) == "" {
		return false, nil
	}
	if health, err := p.isHealthFix(ctx, it); err != nil || health {
		return false, err
	}
	s, err := p.currentPlan(ctx, it.ID)
	if err != nil {
		return false, err
	}
	if s != nil {
		return !s.approved, nil
	}
	return true, p.enqueuePlan(ctx, it, PlanInput{Round: 1}, 1)
}

// proceed takes a triaged item, its plan approved or not needed, on to
// implement: past its cost estimate, and its dependencies.
func (p *Pipeline) proceed(ctx context.Context, it *store.WorkItem) error {
	if held, err := p.checkEstimate(ctx, it); err != nil || held {
		return err
	}
	if ok, err := p.depsDone(ctx, it.ID); err != nil {
		return err
	} else if !ok {
		// unblock queues the implement once every dependency is done.
		return p.Store.SetStage(ctx, it.ID, state.Blocked, "daemon", "waiting on dependencies")
	}
	if _, err := p.enqueueImplement(ctx, it, 1, ImplementInput{}); err != nil {
		return err
	}
	return p.Store.SetStage(ctx, it.ID, state.Implementing, "daemon", "implement queued")
}

func (p *Pipeline) enqueuePlan(ctx context.Context, it *store.WorkItem, in PlanInput, attempt int) error {
	r, _ := p.Cfg.Implement(it.Complexity, 1)
	fast := r.FastFor(r.Harness, r.Model)
	b, _ := json.Marshal(in)
	_, err := p.Store.EnqueueJob(ctx, store.Job{ItemID: &it.ID, Kind: "plan", Harness: r.Harness, Model: r.Model, Effort: r.Effort, Fast: fast,
		Skillset: r.Skills, Route: p.Cfg.ImplementKey(it.Complexity), Attempt: attempt, Priority: it.Priority,
		BudgetUSD: p.Cfg.FastBudget(p.Cfg.JobBudget("review", ""), fast), Input: string(b)})
	return err
}

func (p *Pipeline) enqueuePlanCheck(ctx context.Context, it *store.WorkItem, planJob int64, attempt int) error {
	r := p.Cfg.CriticRoute()
	fast := r.FastFor(r.Harness, r.Model)
	b, _ := json.Marshal(PlanCheckInput{PlanJob: planJob})
	_, err := p.Store.EnqueueJob(ctx, store.Job{ItemID: &it.ID, Kind: "plan_check", Harness: r.Harness, Model: r.Model, Effort: r.Effort, Fast: fast,
		Skillset: r.Skills, Route: "critic", Attempt: attempt, Priority: it.Priority,
		BudgetUSD: p.Cfg.FastBudget(p.Cfg.JobBudget("review", ""), fast), Input: string(b)})
	return err
}

// planEvent records where a plan stands, on its job.
func (p *Pipeline) planEvent(ctx context.Context, it *store.WorkItem, jobID int64, actor, typ, reason string) error {
	return p.Store.AddEvent(ctx, store.Event{Actor: actor, ItemID: &it.ID, JobID: &jobID, Type: typ, Reason: reason})
}

// --- the plan job ---

func (p *Pipeline) runPlan(ctx context.Context, j *store.Job, it *store.WorkItem) error {
	dir := p.jobDir(j.ID)
	out := filepath.Join(dir, "out")
	var in PlanInput
	_ = json.Unmarshal([]byte(j.Input), &in)
	fail := func(run agentRun, st state.JobState, msg string) error {
		if p.stopIfCancelled(ctx, j, it, run) {
			return nil
		}
		p.finish(ctx, j, run, st, "", msg)
		if j.Attempt < maxTriageAttempts {
			return p.enqueuePlan(ctx, it, in, j.Attempt+1)
		}
		return p.park(ctx, it, "Planning failed twice: "+msg)
	}
	is, err := p.issue(ctx, it)
	if notApproved(err) {
		p.finish(ctx, j, agentRun{}, state.Cancelled, "", err.Error())
		p.parkEdited(ctx, it, err)
		return nil
	}
	if err != nil {
		return fail(agentRun{}, state.Failed, err.Error())
	}
	p.warn(p.gh(it.Repo).AddLabels(ctx, it.IssueNumber, LabelRunning), "AddLabels")
	ws, err := p.git(it.Repo).Prepare(ctx, dir, p.repoConf(it.Repo).BaseBranch, "", false, false)
	if err != nil {
		return fail(agentRun{}, state.Failed, err.Error())
	}
	p.warn(p.Store.SetJobRuntime(ctx, j.ID, "", "", ws.Start), "SetJobRuntime")
	prompt, err := p.renderStep(j, "plan", []string{it.Repo}, PromptData{Repo: it.Repo, Issue: is, IssueText: issueText(is),
		PlanFeedback: in.Feedback, PlanFrom: in.From})
	if err != nil {
		return fail(agentRun{}, state.Failed, err.Error())
	}
	schema, _ := verdict.Schema("plan")
	run := p.runAgent(ctx, it.Repo, j, ws.Repo, out, prompt, schema, true, p.Cfg.Timeout("plan"))
	if p.stopIfCancelled(ctx, j, it, run) {
		return nil
	}
	if st, msg, bad := runFailure(run); bad {
		return fail(run, st, msg)
	}
	var v verdict.Plan
	if err := verdict.Parse([]byte(run.final), &v); err != nil {
		return fail(run, state.Failed, err.Error())
	}
	norm, _ := json.Marshal(v)
	p.finish(ctx, j, run, state.Succeeded, string(norm), "")
	return p.afterPlan(ctx, it, j, &v)
}

// afterPlan asks for the plan's approval: a human's, with the estimate's
// when that waits too, or the critic's.
func (p *Pipeline) afterPlan(ctx context.Context, it *store.WorkItem, j *store.Job, v *verdict.Plan) error {
	var in PlanInput
	_ = json.Unmarshal([]byte(j.Input), &in)
	p.warn(p.gh(it.Repo).RemoveLabel(ctx, it.IssueNumber, LabelRunning), "RemoveLabel")
	mode := p.Cfg.PlanFor(it.Complexity)
	if mode == "" {
		// Plans were turned off for its size since it started: it was
		// asked for, so it still waits for an OK (see planNeedsOK).
		mode = config.PlanHuman
	}
	switch mode {
	case config.PlanCritic:
		if in.Round >= maxPlanRounds {
			// The one re-plan is built without another check.
			return p.approvePlan(ctx, it, j.ID, "daemon", "after the critic's concerns")
		}
		p.warn(p.gh(it.Repo).Comment(ctx, it.IssueNumber, "**Factory: the plan for this build.** The critic checks it before the build goes on; nothing to do.\n\n"+
			fenced(planText(v))), "Comment")
		return p.enqueuePlanCheck(ctx, it, j.ID, 1)
	case config.PlanHuman:
		if err := p.planEvent(ctx, it, j.ID, "daemon", evPlanWaiting, "waiting for an OK on the plan"); err != nil {
			return err
		}
		// One ask for both: a cost estimate that waits too goes out with
		// the plan, in the same comment.
		held, err := p.checkEstimate(ctx, it)
		if err != nil {
			return err
		}
		var est *store.Estimate
		if held {
			if est, err = p.Store.ItemEstimate(ctx, it.ID); err != nil {
				return err
			}
		} else if err := p.Store.SetStage(ctx, it.ID, state.Blocked, "daemon", planWaitingReason); err != nil {
			return err
		}
		if s, err := p.currentPlan(ctx, it.ID); err == nil && s != nil && !s.announced {
			p.announceWait(ctx, it, est)
		}
		return nil
	}
	return p.proceed(ctx, it)
}

// planWaitingReason is why the item is blocked, where the dashboard and
// the inbox show it.
const planWaitingReason = "Waiting for your OK on the plan. Read it on this item's page or on the issue, then: " +
	"1. To build it, press Build it, or add the " + LabelGo + " label on the issue. " +
	"2. For another plan, use Ask for another plan, or comment what to change on the issue and then add the " + LabelReplan + " label."

// planSteps is what the comment asking for the plan's OK says to do, with
// the cost estimate's OK in the same step when both wait.
func planSteps(withCost bool) string {
	build := ""
	if withCost {
		build = " This approves the cost and the plan at once."
	}
	return fmt.Sprintf("**To build it**, either:\n\n"+
		"1. add the `%s` label to this issue (someone with write access), or\n"+
		"2. press **Build it** on this item's page in the factory dashboard.%s\n\n"+
		"**To get another plan**, either:\n\n"+
		"1. comment on this issue what to change, then add the `%s` label: the builder plans once more with your comment, or\n"+
		"2. press **Ask for another plan** on this item's page in the factory dashboard and say what to change there.", LabelGo, build, LabelReplan)
}

// approvePlan records the plan's approval and goes on to the build.
func (p *Pipeline) approvePlan(ctx context.Context, it *store.WorkItem, planJob int64, actor, by string) error {
	if err := p.planEvent(ctx, it, planJob, actor, evPlanApproved, by); err != nil {
		return err
	}
	return p.proceed(ctx, it)
}

// planSection is the plan in the comment that asks for its OK.
func planSection(s *planState) string {
	if s == nil || s.plan == nil {
		return ""
	}
	who := ""
	if s.in.Round >= maxPlanRounds {
		who = fmt.Sprintf(" It's the new plan %s asked for.", s.in.From)
	}
	return "**The plan** the builder wrote, before building anything." + who + " It's a model's words, so it's in a block:\n\n" + fenced(planText(s.plan)) + "\n"
}

// --- the critic's check ---

func (p *Pipeline) runPlanCheck(ctx context.Context, j *store.Job, it *store.WorkItem) error {
	dir := p.jobDir(j.ID)
	out := filepath.Join(dir, "out")
	var in PlanCheckInput
	_ = json.Unmarshal([]byte(j.Input), &in)
	s, err := p.currentPlan(ctx, it.ID)
	if err != nil {
		p.finish(ctx, j, agentRun{}, state.Failed, "", err.Error())
		return err
	}
	if s == nil || s.job.ID != in.PlanJob || s.plan == nil || s.approved {
		p.finish(ctx, j, agentRun{}, state.Cancelled, "", "the plan it checks was superseded")
		return nil
	}
	fail := func(run agentRun, st state.JobState, msg string) error {
		if p.stopIfCancelled(ctx, j, it, run) {
			return nil
		}
		p.finish(ctx, j, run, st, "", msg)
		return p.planCheckFailed(ctx, it, s, j.Attempt, msg)
	}
	is, err := p.issue(ctx, it)
	if notApproved(err) {
		p.finish(ctx, j, agentRun{}, state.Cancelled, "", err.Error())
		p.parkEdited(ctx, it, err)
		return nil
	}
	if err != nil {
		return fail(agentRun{}, state.Failed, err.Error())
	}
	ws, err := p.git(it.Repo).Prepare(ctx, dir, p.repoConf(it.Repo).BaseBranch, "", false, false)
	if err != nil {
		return fail(agentRun{}, state.Failed, err.Error())
	}
	p.warn(p.Store.SetJobRuntime(ctx, j.ID, "", "", ws.Start), "SetJobRuntime")
	prompt, err := p.renderStep(j, "plan_check", []string{it.Repo}, PromptData{Repo: it.Repo, Issue: is, IssueText: issueText(is), Plan: fenced(planText(s.plan))})
	if err != nil {
		return fail(agentRun{}, state.Failed, err.Error())
	}
	schema, _ := verdict.Schema("plan_check")
	run := p.runAgent(ctx, it.Repo, j, ws.Repo, out, prompt, schema, true, p.Cfg.Timeout("plan"))
	if p.stopIfCancelled(ctx, j, it, run) {
		return nil
	}
	if st, msg, bad := runFailure(run); bad {
		return fail(run, st, msg)
	}
	var v verdict.PlanCheck
	if err := verdict.Parse([]byte(run.final), &v); err != nil {
		return fail(run, state.Failed, err.Error())
	}
	norm, _ := json.Marshal(v)
	p.finish(ctx, j, run, state.Succeeded, string(norm), "")
	return p.afterPlanCheck(ctx, it, s, &v)
}

// planCheckFailed retries a check that failed once; after that the build
// goes on without it, as it would with plan.medium off: the gates and the
// review still hold the change.
func (p *Pipeline) planCheckFailed(ctx context.Context, it *store.WorkItem, s *planState, attempt int, msg string) error {
	if attempt < maxReviewAttempts {
		return p.enqueuePlanCheck(ctx, it, s.job.ID, attempt+1)
	}
	return p.approvePlan(ctx, it, s.job.ID, "daemon", "without the critic's check, which failed twice: "+firstLine(msg))
}

func (p *Pipeline) afterPlanCheck(ctx context.Context, it *store.WorkItem, s *planState, v *verdict.PlanCheck) error {
	if v.Decision == "approve" {
		return p.approvePlan(ctx, it, s.job.ID, "critic", "by the critic")
	}
	if err := p.planEvent(ctx, it, s.job.ID, "critic", evPlanReplan, "the critic raised concerns"); err != nil {
		return err
	}
	return p.enqueuePlan(ctx, it, PlanInput{Round: maxPlanRounds, From: "the critic",
		Feedback: "Your earlier plan:\n\n" + fenced(planText(s.plan)) + "\nThe critic's concerns:\n\n" + fenced("- "+strings.Join(v.Concerns, "\n- "))}, 1)
}

// resumePlan picks an item up after its last job, a plan or its check.
func (p *Pipeline) resumePlan(ctx context.Context, it *store.WorkItem, last *store.Job) error {
	s, err := p.currentPlan(ctx, it.ID)
	if err != nil {
		return err
	}
	switch {
	case s == nil:
		return p.enqueueTriage(ctx, it, 1, "")
	case s.approved:
		return p.proceed(ctx, it)
	case last.Kind == "plan" && s.plan != nil:
		return p.afterPlan(ctx, it, s.job, s.plan)
	case last.Kind == "plan" && last.Attempt < maxTriageAttempts:
		return p.enqueuePlan(ctx, it, s.in, last.Attempt+1)
	case last.Kind == "plan":
		return p.park(ctx, it, "Planning failed twice.")
	}
	var v verdict.PlanCheck
	if last.State == state.Succeeded && verdict.Parse([]byte(last.Verdict), &v) == nil {
		return p.afterPlanCheck(ctx, it, s, &v)
	}
	return p.planCheckFailed(ctx, it, s, last.Attempt, last.Error)
}

// --- asking for changes ---

// intakeReplan sends back the plans people asked to change with
// factory:replan, added after they commented, by someone who may approve
// work. The builder plans once more with their last comment; a second
// ask parks the item. Anywhere else the label comes off, with a note.
func (p *Pipeline) intakeReplan(ctx context.Context, repo string) error {
	gh := p.gh(repo)
	issues, err := gh.ListIssues(ctx, LabelReplan)
	if err != nil {
		return err
	}
	for _, is := range issues {
		var s *planState
		it, err := p.Store.ItemByIssue(ctx, repo, is.Number)
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			p.warn(err, "ItemByIssue")
			continue
		default:
			if s, err = p.currentPlan(ctx, it.ID); err != nil {
				p.warn(err, "currentPlan")
				continue
			}
		}
		waiting := it != nil && it.Stage == state.Blocked && s != nil && s.waiting
		if waiting && !s.announced {
			continue // asked about once the plan is out
		}
		if !waiting {
			if err := gh.RemoveLabel(ctx, is.Number, LabelReplan); err != nil {
				p.warn(err, "RemoveLabel")
				continue
			}
			p.warn(gh.Comment(ctx, is.Number, fmt.Sprintf("**Factory:** `%s` was taken off: it only asks for a new plan for an item whose plan waits for an OK, and this one's doesn't.", LabelReplan)), "Comment")
			continue
		}
		who, _, ok := p.approvedFor(ctx, repo, is.Number, LabelReplan, labelRefusal{"not re-planning", "ask for a new plan", "ask for one", "ask for one"})
		if !ok {
			continue
		}
		// Off first, so a failure below cannot loop.
		if err := gh.RemoveLabel(ctx, is.Number, LabelReplan); err != nil {
			p.warn(err, "RemoveLabel")
			continue
		}
		p.warn(p.replan(ctx, it, s, "@"+who, p.lastCommentBy(ctx, it.Repo, it.IssueNumber, who)), "replan")
	}
	return nil
}

// fromDashboard is who asked, in the factory's words, when it was asked
// on the dashboard.
const fromDashboard = "someone on the dashboard"

// errPlanChanged refuses a dashboard action on a plan that isn't the
// current one: a newer plan came while the page was open.
var errPlanChanged = errors.New("the plan changed since you opened this page; reload to see the new one")

// Replan is the dashboard's ask for another plan, with what to change: what
// a comment and factory:replan on the issue do. planJob is the plan job
// the page showed; a plan that changed since is refused.
func (p *Pipeline) Replan(ctx context.Context, itemID, planJob int64, ask string) error {
	p.act.Lock()
	defer p.act.Unlock()
	ask = strings.TrimSpace(ask)
	if ask == "" {
		return errors.New("say what to change about the plan")
	}
	it, err := p.Store.GetItem(ctx, itemID)
	if err != nil {
		return err
	}
	s, err := p.currentPlan(ctx, itemID)
	if err != nil {
		return err
	}
	if s == nil || s.job.ID != planJob {
		return errPlanChanged
	}
	if it.Stage != state.Blocked || !s.waiting {
		return fmt.Errorf("item %d's plan isn't waiting for an OK", itemID)
	}
	return p.replan(ctx, it, s, fromDashboard, ask)
}

// replan sends a waiting plan back to the builder with what from ("@x",
// or fromDashboard) asked to change, or parks the item when its plan was
// already sent back once. p.act is held.
func (p *Pipeline) replan(ctx context.Context, it *store.WorkItem, s *planState, from, ask string) error {
	dashboard := from == fromDashboard
	if err := p.planEvent(ctx, it, s.job.ID, "human", evPlanReplan, from+" asked for changes"); err != nil {
		return err
	}
	who := from
	if dashboard {
		who = capitalize(from)
	}
	if s.in.Round >= maxPlanRounds {
		return p.park(ctx, it, fmt.Sprintf("%s asked for changes to the plan again. The factory plans again only once, so it stopped here rather than guess. "+
			"Edit the issue to say what you want built, then add `%s` to triage and plan it from the start.", who, LabelRetry))
	}
	if ask == "" {
		ask = "(No comment from them was found: they asked for another plan without saying what to change.)"
	}
	gh := p.gh(it.Repo)
	p.warn(gh.RemoveLabel(ctx, it.IssueNumber, LabelAwaitingYou), "RemoveLabel")
	said, comment := "their last comment.", from+"'s comment"
	if dashboard {
		// The ask isn't on the issue yet: it goes there, fenced, so it's on
		// the record and its words can't mention or close anything.
		said, comment = "what they asked:\n\n"+fenced(truncate(ask, 4000)), "What they asked to change, on the dashboard"
	}
	p.warn(gh.Comment(ctx, it.IssueNumber, "**Factory:** "+from+" asked for changes to the plan; the builder is writing a new one with "+said), "Comment")
	if err := p.Store.SetStage(ctx, it.ID, state.Ready, "human", from+" asked for a new plan"); err != nil {
		return err
	}
	return p.enqueuePlan(ctx, it, PlanInput{Round: maxPlanRounds, From: from,
		Feedback: "Your earlier plan:\n\n" + fenced(planText(s.plan)) + "\n" + comment + ":\n\n" + fenced(truncate(ask, 4000))}, 1)
}

// lastCommentBy is who's latest comment on the issue, or "" when there
// is none or the comments can't be read.
func (p *Pipeline) lastCommentBy(ctx context.Context, repo string, n int, who string) string {
	cl, ok := p.gh(repo).(commentLister)
	if !ok {
		return ""
	}
	cs, err := cl.IssueComments(ctx, n)
	if err != nil {
		p.warn(err, "IssueComments")
		return ""
	}
	for i := len(cs) - 1; i >= 0; i-- {
		if strings.EqualFold(cs[i].Author, who) {
			return strings.TrimSpace(cs[i].Body)
		}
	}
	return ""
}
