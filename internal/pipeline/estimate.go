package pipeline

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/claptimes5/factory/internal/state"
	"github.com/claptimes5/factory/internal/store"
)

// The cost estimate before implement. Triage rates an item's size; the
// estimate is what items of that size have cost the factory to build
// (every implement attempt, the gates, review and critic), from its own
// history: the same repo's when it has enough of them, else every repo's,
// else the budgets' caps. Above budgets.confirm_over (its p75 against it)
// the item waits for a human's OK.

// LabelGo approves an item whose estimate waits for a human.
const LabelGo = "factory:go"

// MinEstimateSamples is how many past items of a size the estimate needs
// before it trusts their costs over the budgets.
const MinEstimateSamples = 5

// costEstimate is an estimate before it's stored. Median and P75 are
// billed dollars; the totals hold work on a subscription too.
type costEstimate struct {
	Median, P75           float64
	TotalMedian, TotalP75 float64
	Basis                 string // repo | all | default
	Samples               int
	Detail                string
}

// onPlan says whether a harness's jobs draw on a subscription plan.
func (p *Pipeline) onPlan(harness string) bool { return p.PlanBilled[harness] }

// split is what a past build billed in dollars and drew on a plan, job by
// job on the harness each ran on, and the plans it drew on.
func (p *Pipeline) split(c store.ItemCost) (billed, plan float64, plans []string) {
	for _, pt := range c.Parts {
		if !p.onPlan(pt.Harness) {
			billed += pt.USD
			continue
		}
		plan += pt.USD
		if !slices.Contains(plans, pt.Harness) {
			plans = append(plans, pt.Harness)
		}
	}
	return billed, plan, plans
}

// estimate works out what building an item will likely cost.
func (p *Pipeline) estimate(ctx context.Context, it *store.WorkItem) (costEstimate, error) {
	hist, err := p.Store.ItemCosts(ctx)
	if err != nil {
		return costEstimate{}, err
	}
	var repo, all []store.ItemCost
	for _, c := range hist {
		if c.ItemID == it.ID || c.Size != it.Complexity {
			continue
		}
		all = append(all, c)
		if strings.EqualFold(c.Repo, it.Repo) {
			repo = append(repo, c)
		}
	}
	var plan []string
	var e costEstimate
	var from []store.ItemCost
	switch {
	case len(repo) >= MinEstimateSamples:
		e.Basis, from = "repo", repo
	case len(all) >= MinEstimateSamples:
		e.Basis, from = "all", all
	default:
		e, plan = p.budgetEstimate(it, len(all))
	}
	var planLo, planHi float64
	if from != nil {
		var billed, plans, totals []float64
		for _, c := range from {
			bl, pl, hs := p.split(c)
			for _, h := range hs {
				if !slices.Contains(plan, h) {
					plan = append(plan, h)
				}
			}
			billed, plans, totals = append(billed, bl), append(plans, pl), append(totals, c.Total())
		}
		e.Samples = len(from)
		e.Median, e.P75 = quantile(billed, 0.5), quantile(billed, 0.75)
		e.TotalMedian, e.TotalP75 = quantile(totals, 0.5), quantile(totals, 0.75)
		planLo, planHi = quantile(plans, 0.5), quantile(plans, 0.75)
		where := "in " + it.Repo
		if e.Basis == "all" {
			where = fmt.Sprintf("across all repos (%d in %s, too few on their own)", len(repo), it.Repo)
		}
		e.Detail = fmt.Sprintf("based on %d past %s items %s: ", len(from), it.Complexity, where)
		if e.P75 > 0 || planHi == 0 {
			e.Detail += fmt.Sprintf("median %s, p75 %s", usd(e.Median), usd(e.P75))
		} else {
			e.Detail += "nothing billed in dollars"
		}
	} else {
		planLo, planHi = e.TotalMedian-e.Median, e.TotalP75-e.P75
	}
	if planHi > 0 {
		e.Detail += fmt.Sprintf("; on your plan (%s): about %s–%s more, at the cost the CLI reports", strings.Join(plan, ", "), usd(planLo), usd(planHi))
		if ls := p.limitsOf(ctx, plan); ls != "" {
			e.Detail += "; usage now: " + ls
		}
	}
	return e, nil
}

// budgetEstimate is the estimate with too little history: half to all of
// one build's cap and one review's (and the critic's, for high risk).
func (p *Pipeline) budgetEstimate(it *store.WorkItem, have int) (costEstimate, []string) {
	r, _ := p.Cfg.Implement(it.Complexity, 1)
	build := p.Cfg.FastBudget(p.Cfg.JobBudget("implement", it.Complexity), r.FastFor(r.Harness, r.Model))
	review := p.Cfg.JobBudget("review", "")
	c := store.ItemCost{Implement: build, Review: review, Parts: []store.CostPart{
		{Kind: "implement", Harness: r.Harness, USD: build}, {Kind: "review", Harness: p.Cfg.Review(it.Risk).Harness, USD: review}}}
	if it.Risk == "high" {
		critic := p.Cfg.Routing["implement.L"]
		if cr, ok := p.Cfg.Routing["critic"]; ok {
			critic = cr
		}
		c.Critic = review
		c.Parts = append(c.Parts, store.CostPart{Kind: "critic", Harness: critic.Harness, USD: review})
	}
	billed, _, plans := p.split(c)
	e := costEstimate{Basis: "default", Samples: have, Median: billed / 2, P75: billed, TotalMedian: c.Total() / 2, TotalP75: c.Total()}
	e.Detail = fmt.Sprintf("from your budgets, with too few past %s items to go by (%d; it takes %d): half to all of one %s build's %s cap and one review's %s",
		it.Complexity, have, MinEstimateSamples, it.Complexity, usd(build), usd(review))
	if c.Critic > 0 {
		e.Detail += " and the critic's " + usd(c.Critic)
	}
	return e, plans
}

// limitsOf is the usage windows the harnesses last reported, in a line.
func (p *Pipeline) limitsOf(ctx context.Context, harnesses []string) string {
	ls, err := p.Store.Limits(ctx)
	if err != nil {
		return ""
	}
	var mine []store.Limit
	for _, l := range ls {
		if slices.Contains(harnesses, l.Harness) {
			mine = append(mine, l)
		}
	}
	return store.FormatLimits(mine)
}

// quantile interpolates between the nearest ranks, as a spreadsheet's
// PERCENTILE does; 0 for no values.
func quantile(vs []float64, q float64) float64 {
	if len(vs) == 0 {
		return 0
	}
	s := slices.Clone(vs)
	sort.Float64s(s)
	pos := q * float64(len(s)-1)
	lo := int(pos)
	if lo+1 >= len(s) {
		return s[lo]
	}
	return s[lo] + (pos-float64(lo))*(s[lo+1]-s[lo])
}

func usd(v float64) string { return fmt.Sprintf("$%.2f", v) }

// checkEstimate estimates a triaged item and holds it for a human when
// the estimate is over its repo's confirm_over. It reports whether the
// item now waits. A main health fix is never held: a red main is urgent.
// An OK carries over a re-triage of the same text that comes out no
// higher; an edit, or a higher estimate, asks again.
func (p *Pipeline) checkEstimate(ctx context.Context, it *store.WorkItem) (bool, error) {
	var triage int64
	if js, err := p.Store.ListJobs(ctx, store.JobFilter{ItemID: it.ID, Kind: "triage", Limit: 1}); err != nil {
		return false, err
	} else if len(js) > 0 {
		triage = js[0].ID
	}
	prev, err := p.Store.ItemEstimate(ctx, it.ID)
	if err != nil {
		return false, err
	}
	if prev != nil && triage != 0 && prev.TriageJob == triage {
		// Decided after this triage already (a resumed item): only once.
		if prev.Status == store.EstimateWaiting {
			if err := p.Store.SetStage(ctx, it.ID, state.Blocked, "daemon", p.waitingReason(prev)); err != nil {
				return true, err
			}
			if !prev.Announced {
				p.announceWait(ctx, it, prev)
			}
			return true, nil
		}
		return false, nil
	}
	e, err := p.estimate(ctx, it)
	if err != nil {
		return false, err
	}
	hash := ""
	if a, err := p.Store.ItemApproval(ctx, it.ID); err == nil && a != nil {
		hash = a.IssueHash
	}
	threshold := p.Cfg.ConfirmOver(it.Repo)
	est := store.Estimate{ItemID: it.ID, TriageJob: triage, IssueHash: hash, Size: it.Complexity, MedianUSD: e.Median, P75USD: e.P75,
		TotalMedianUSD: e.TotalMedian, TotalP75USD: e.TotalP75, Basis: e.Basis, Samples: e.Samples, Detail: e.Detail, ThresholdUSD: threshold}
	health, err := p.isHealthFix(ctx, it)
	if err != nil {
		return false, err
	}
	switch {
	case threshold <= 0:
		est.Status = store.EstimateOff
	case e.P75 <= threshold:
		est.Status = store.EstimateUnder
	case health:
		est.Status = store.EstimateExempt
	case prev != nil && prev.Status == store.EstimateApproved && prev.IssueHash == hash && hash != "" && e.P75 <= prev.ApprovedUSD+0.005:
		est.Status, est.ApprovedBy, est.ApprovedUSD, est.DecidedAt = store.EstimateApproved, prev.ApprovedBy, prev.ApprovedUSD, prev.DecidedAt
	default:
		est.Status = store.EstimateWaiting
	}
	if err := p.Store.SetEstimate(ctx, est); err != nil {
		return false, err
	}
	p.warn(p.Store.AddEvent(ctx, store.Event{Actor: "daemon", ItemID: &it.ID, Type: "estimate", Reason: est.Range() + ", " + est.Detail,
		Data: fmt.Sprintf(`{"median_usd":%g,"p75_usd":%g,"threshold_usd":%g,"status":%q}`, est.MedianUSD, est.P75USD, threshold, est.Status)}), "AddEvent")
	if est.Status != store.EstimateWaiting {
		if prev != nil {
			// A re-triage that needn't wait: an earlier wait's label goes.
			p.warn(p.gh(it.Repo).RemoveLabel(ctx, it.IssueNumber, LabelAwaitingYou), "RemoveLabel")
		}
		return false, nil
	}
	if err := p.Store.SetStage(ctx, it.ID, state.Blocked, "daemon", p.waitingReason(&est)); err != nil {
		return false, err
	}
	p.announceWait(ctx, it, &est)
	return true, nil
}

// isHealthFix says whether an item was filed for a red or flaky main:
// marked so when it came in, or still tracked as such.
func (p *Pipeline) isHealthFix(ctx context.Context, it *store.WorkItem) (bool, error) {
	if ok, err := p.Store.IsHealthItem(ctx, it.ID); err != nil || ok {
		return ok, err
	}
	return p.Store.IsHealthIssue(ctx, it.Repo, it.IssueNumber)
}

// announceWait tells people an item waits on its estimate: a factory:go
// from before the wait comes off first, so only one added after this
// comment approves it. Recorded once it's all out; until then intake
// tries again and no factory:go counts.
func (p *Pipeline) announceWait(ctx context.Context, it *store.WorkItem, est *store.Estimate) {
	gh := p.gh(it.Repo)
	is, err := gh.GetIssue(ctx, it.IssueNumber)
	if err != nil {
		p.warn(err, "GetIssue")
		return
	}
	stale := hasLabel(is.Labels, LabelGo)
	if stale {
		if err := gh.RemoveLabel(ctx, it.IssueNumber, LabelGo); err != nil {
			p.warn(err, "RemoveLabel")
			return
		}
	}
	p.warn(gh.RemoveLabel(ctx, it.IssueNumber, LabelRunning), "RemoveLabel")
	if err := gh.AddLabels(ctx, it.IssueNumber, LabelAwaitingYou); err != nil {
		p.warn(err, "AddLabels")
		return
	}
	note := ""
	if stale {
		note = fmt.Sprintf("\n\nThe `%s` label that was already on was taken off: it has to be added after this comment, for this estimate.", LabelGo)
	}
	if err := gh.Comment(ctx, it.IssueNumber, fmt.Sprintf("**Factory: waiting for your OK on the cost.**\n\n"+
		"Triage rated this %s. Estimated cost: **%s** (%s). That's over the %s limit (`confirm_over`), so nothing is built until someone approves.\n\n"+
		"To go ahead, someone with write access adds the `%s` label, or presses **Approve estimate** on the item in the dashboard.%s",
		it.Complexity, est.Range(), est.Detail, usd(est.ThresholdUSD), LabelGo, note)); err != nil {
		p.warn(err, "Comment")
		return
	}
	p.notify(ctx, fmt.Sprintf("%s waits for your OK: estimated %s, over the %s limit. Add %s to build it.", p.ref(it.Repo, it.IssueNumber), est.Range(), usd(est.ThresholdUSD), LabelGo))
	p.warn(p.Store.SetEstimateAnnounced(ctx, it.ID), "SetEstimateAnnounced")
}

func (p *Pipeline) waitingReason(e *store.Estimate) string {
	return fmt.Sprintf("Waiting for your OK on the cost: estimated %s, over the %s limit. Add %s or approve it here.", e.Range(), usd(e.ThresholdUSD), LabelGo)
}

// errEstimateWaiting refuses to build an item whose estimate needs an OK.
var errEstimateWaiting = errors.New("refused: the item needs a human's OK on its cost estimate first")

// estimateWaiting says whether an item's estimate waits for a human.
func (p *Pipeline) estimateWaiting(ctx context.Context, itemID int64) bool {
	e, err := p.Store.ItemEstimate(ctx, itemID)
	return err == nil && e != nil && e.Status == store.EstimateWaiting
}

// estimateNeedsOK is estimateWaiting, or parked while it waited: either
// way nothing may build it until someone approves.
func (p *Pipeline) estimateNeedsOK(ctx context.Context, itemID int64) bool {
	e, err := p.Store.ItemEstimate(ctx, itemID)
	return err != nil || e != nil && store.EstimateNeedsOK(e.Status)
}

// ApproveEstimate is the dashboard's OK on an item whose estimate waits.
func (p *Pipeline) ApproveEstimate(ctx context.Context, itemID int64) error {
	p.act.Lock()
	defer p.act.Unlock()
	return p.approveEstimate(ctx, itemID, "the dashboard")
}

// approveEstimate lets a waiting item go on to implement (or to wait on
// its dependencies). p.act is held.
func (p *Pipeline) approveEstimate(ctx context.Context, itemID int64, by string) error {
	it, err := p.Store.GetItem(ctx, itemID)
	if err != nil {
		return err
	}
	if it.Stage != state.Blocked || !p.estimateWaiting(ctx, itemID) {
		return fmt.Errorf("item %d isn't waiting for an OK on its estimate", itemID)
	}
	if ok, err := p.Store.ApproveEstimate(ctx, itemID, by); err != nil || !ok {
		if err == nil {
			err = fmt.Errorf("item %d isn't waiting for an OK on its estimate", itemID)
		}
		return err
	}
	e, _ := p.Store.ItemEstimate(ctx, itemID)
	rng := ""
	if e != nil {
		rng = " (" + e.Range() + ")"
	}
	actor := "human"
	if by == "the dashboard" {
		by = "from the dashboard"
	} else {
		by = "by @" + by
	}
	p.warn(p.Store.AddEvent(ctx, store.Event{Actor: actor, ItemID: &it.ID, Type: "estimate_approved", Reason: "approved " + by + rng}), "AddEvent")
	gh := p.gh(it.Repo)
	p.warn(gh.RemoveLabel(ctx, it.IssueNumber, LabelAwaitingYou), "RemoveLabel")
	p.warn(gh.Comment(ctx, it.IssueNumber, "**Factory:** the estimate"+rng+" was approved "+by+"; building it."), "Comment")
	if err := p.Store.SetStage(ctx, it.ID, state.Ready, actor, "estimate approved "+by); err != nil {
		return err
	}
	if ok, err := p.depsDone(ctx, it.ID); err != nil {
		return err
	} else if !ok {
		return p.Store.SetStage(ctx, it.ID, state.Blocked, "daemon", "waiting on dependencies")
	}
	if _, err := p.enqueueImplement(ctx, it, 1, ImplementInput{}); err != nil {
		return err
	}
	return p.Store.SetStage(ctx, it.ID, state.Implementing, "daemon", "implement queued")
}

// intakeGo announces waits that didn't go out, and releases items a
// human approved with the factory:go label. The label counts only on an
// item already waiting, added after the wait was announced (announceWait
// takes an earlier one off), by someone who may approve work. Anywhere
// else it comes off, with a note.
func (p *Pipeline) intakeGo(ctx context.Context, repo string) error {
	blocked, err := p.Store.ListItems(ctx, state.Blocked)
	if err != nil {
		return err
	}
	for _, it := range blocked {
		if !strings.EqualFold(it.Repo, repo) {
			continue
		}
		if e, err := p.Store.ItemEstimate(ctx, it.ID); err == nil && e != nil && e.Status == store.EstimateWaiting && !e.Announced {
			p.announceWait(ctx, it, e)
		}
	}
	gh := p.gh(repo)
	issues, err := gh.ListIssues(ctx, LabelGo)
	if err != nil {
		return err
	}
	for _, is := range issues {
		var est *store.Estimate
		it, err := p.Store.ItemByIssue(ctx, repo, is.Number)
		if err == nil {
			est, _ = p.Store.ItemEstimate(ctx, it.ID)
		}
		waiting := err == nil && it.Stage == state.Blocked && est != nil && est.Status == store.EstimateWaiting
		if waiting && !est.Announced {
			continue // announceWait takes it off before it may count
		}
		if !waiting {
			if err := gh.RemoveLabel(ctx, is.Number, LabelGo); err != nil {
				p.warn(err, "RemoveLabel")
				continue
			}
			p.warn(gh.Comment(ctx, is.Number, fmt.Sprintf("**Factory:** `%s` was taken off: it only approves an item that's waiting for an OK on its cost estimate, and this one isn't. "+
				"If it waits later, add it again then.", LabelGo)), "Comment")
			continue
		}
		who, _, ok := p.approvedBy(ctx, repo, is.Number, LabelGo)
		if !ok {
			continue
		}
		// Off first, so a failure below cannot loop.
		if err := gh.RemoveLabel(ctx, is.Number, LabelGo); err != nil {
			p.warn(err, "RemoveLabel")
			continue
		}
		p.warn(p.approveEstimate(ctx, it.ID, who), "approve estimate")
	}
	return nil
}
