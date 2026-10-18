package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/claptimes5/factory/internal/config"
	"github.com/claptimes5/factory/internal/state"
	"github.com/claptimes5/factory/internal/store"
	"github.com/claptimes5/factory/internal/verdict"
)

// Rethink before parking. An item whose reviewer blocks it past
// escalation.max_review_rounds gets one more try before it parks: a fresh
// build from the base branch on the next size's implement route (S, then
// M, then L, which is the top), handed the reviewer's unresolved findings
// and told the approach itself may be wrong. The rethink's implement job
// carries Rethink in its input, so it is its own record: rounds and
// findings count from it, and an item has one per triage, across
// restarts. A human retry re-triages, which allows another.

// isRethink says whether a job is a rethink's build, or a retry of one.
func isRethink(j *store.Job) bool {
	if j.Kind != "implement" || !strings.Contains(j.Input, `"rethink"`) {
		return false
	}
	var in ImplementInput
	return json.Unmarshal([]byte(j.Input), &in) == nil && in.Rethink
}

// rethinkJob is the newest rethink build since the item's latest triage,
// or nil. jobs are the item's, newest first.
func rethinkJob(jobs []*store.Job) *store.Job {
	for _, j := range jobs {
		if j.Kind == "triage" {
			break
		}
		if isRethink(j) {
			return j
		}
	}
	return nil
}

// rethinkSize is the implement size a rethink since the latest triage
// builds on, so its fix rounds stay there, or "" for the item's own.
func (p *Pipeline) rethinkSize(ctx context.Context, itemID int64) string {
	jobs, err := p.Store.ListJobs(ctx, store.JobFilter{ItemID: itemID})
	if err != nil {
		p.warn(err, "ListJobs")
		return ""
	}
	if rj := rethinkJob(jobs); rj != nil {
		var in ImplementInput
		if json.Unmarshal([]byte(rj.Input), &in) == nil {
			return in.Size
		}
	}
	return ""
}

// nextSize is the implement size one up from an item's: S (and XS) go to
// M, M and L to L.
func nextSize(size string) string {
	switch size {
	case "XS", "S":
		return "M"
	}
	return "L"
}

// rethinkTo is the size a rethink builds on: the next one up, or the
// item's own when the next has no route; why says when neither has one.
func (p *Pipeline) rethinkTo(it *store.WorkItem) (string, string) {
	next := nextSize(it.Complexity)
	if _, ok := p.Cfg.Routing["implement."+next]; ok {
		return next, ""
	}
	if _, ok := p.Cfg.Routing[p.Cfg.ImplementKey(it.Complexity)]; ok {
		return "", ""
	}
	return "", fmt.Sprintf("there is no routing.implement.%s (nor one for its own size) to build it on", next)
}

// roundCap is what happens when the reviewer blocks past the round cap:
// a rethink when the item has none since its triage and can afford one,
// else the park it has always been.
func (p *Pipeline) roundCap(ctx context.Context, it *store.WorkItem, v *verdict.Review, blocks int, findings string) error {
	jobs, err := p.Store.ListJobs(ctx, store.JobFilter{ItemID: it.ID})
	if err != nil {
		return err
	}
	parked := fmt.Sprintf("The reviewer blocked this %d times. Latest findings:\n\n%s", blocks, findings)
	if rj := rethinkJob(jobs); rj != nil {
		return p.park(ctx, it, fmt.Sprintf("The reviewer blocked this %d times on a second, fresh attempt (job %d, on routing.%s), after the first approach hit the round cap too. Latest findings:\n\n%s",
			blocks, rj.ID, rj.Route, findings))
	}
	if !p.Cfg.Reviews.RethinkOn() {
		return p.park(ctx, it, parked)
	}
	size, why := p.rethinkTo(it)
	if why == "" {
		why = p.cantRethink(ctx, it, jobs, size)
	}
	if why != "" {
		return p.park(ctx, it, parked+"\n\nNo fresh attempt first: "+why+".")
	}
	text, _ := fixFindings(v)
	text = strings.Replace(text, "Blocking findings (answer each by id):", "Blocking findings the last approach didn't resolve:", 1)
	in := ImplementInput{Findings: text, Rethink: true, Size: size}
	if size == "" {
		size = it.Complexity
	}
	route := p.Cfg.ImplementKey(size)
	if err := p.Store.SetStage(ctx, it.ID, state.Implementing, "daemon", fmt.Sprintf("round cap reached: rethinking on %s", route)); err != nil {
		return err
	}
	ok, err := p.enqueueImplement(ctx, it, 1, in)
	if errors.Is(err, errEstimateWaiting) || err == nil && !ok {
		return p.park(ctx, it, parked+"\n\nNo fresh attempt first: nothing may build it now.")
	}
	if err != nil {
		return err
	}
	p.warn(p.Store.AddEvent(ctx, store.Event{Actor: "daemon", ItemID: &it.ID, Type: "rethink",
		Reason: fmt.Sprintf("the reviewer blocked %d times: a fresh attempt from the base branch on routing.%s", blocks, route)}), "AddEvent")
	p.warn(p.gh(it.Repo).Comment(ctx, it.IssueNumber, fmt.Sprintf("**Reviewer blocked round %d, the last one.** Before parking it, the factory tries once more: "+
		"a fresh build from the base branch on routing.%s, told the approach itself may be wrong. It gets %d fix rounds of its own, then parks.\n\n%s",
		blocks, route, p.Cfg.Escalation.MaxReviewRounds, findings)), "Comment")
	return nil
}

// cantRethink says why the item can't afford a rethink, or "": with a
// cost estimate in force (budgets.confirm_over), what it has billed since
// triage plus the rethink's build and one round of review (its critic on
// a high-risk item, its security pass when the first approach had one)
// must stay within the estimate's p75, or what a human approved.
func (p *Pipeline) cantRethink(ctx context.Context, it *store.WorkItem, jobs []*store.Job, size string) string {
	e, err := p.Store.ItemEstimate(ctx, it.ID)
	if err != nil {
		return "its cost estimate couldn't be read: " + err.Error()
	}
	if e == nil || e.ThresholdUSD <= 0 {
		return ""
	}
	limit := e.TotalP75USD
	if e.Status == store.EstimateApproved && e.ApprovedUSD > 0 {
		limit = e.ApprovedUSD
	}
	spent, security := 0.0, false
	for _, j := range jobs {
		if j.Kind == "triage" {
			break
		}
		security = security || j.Kind == store.KindSecurity
		if !p.onPlan(j.Harness) {
			spent += j.CostUSD
		}
	}
	if size == "" {
		size = it.Complexity
	}
	next := 0.0
	add := func(r config.Route, budget float64) {
		if !p.onPlan(r.Harness) {
			next += p.Cfg.FastBudget(budget, r.FastFor(r.Harness, r.Model))
		}
	}
	if r, ok := p.Cfg.Implement(size, 1); ok {
		add(r, p.Cfg.JobBudget("implement", size))
	}
	review := p.Cfg.JobBudget("review", "")
	add(p.Cfg.Routing[p.Cfg.ReviewKey(it.Risk)], review)
	if it.Risk == "high" {
		add(p.Cfg.CriticRoute(), review)
	}
	if security && p.Cfg.Security.On() {
		next += review
	}
	if spent+next > limit {
		return fmt.Sprintf("it has billed $%.2f, and another build and round of review could bill $%.2f more, past the $%.2f it was estimated at", spent, next, limit)
	}
	return ""
}
