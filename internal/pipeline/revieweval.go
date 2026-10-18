package pipeline

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/claptimes5/factory/internal/config"
	"github.com/claptimes5/factory/internal/state"
	"github.com/claptimes5/factory/internal/store"
)

// Reviewer evals: the review stage alone, on a change already on a
// branch, as if its gates had passed. internal/eval builds the cases and
// scores them; the tuning loop runs them (through ReviewEval) before it
// proposes a change to how review works.

// ReviewOnly reviews head, already on the issue's branch, the way a
// build that passed its gates is reviewed: the review route for risk,
// the verifier on each blocking finding, and a high-risk change's critic.
// It stops there, never building or fixing, and returns the item: it is
// awaiting a human when review passed it, implementing when it blocked,
// and parked when the reviewer had questions or no verdict. For reviewer
// evals, on a store and forge of their own.
func (p *Pipeline) ReviewOnly(ctx context.Context, repo string, issue int, risk, head string) (*store.WorkItem, error) {
	is, err := p.gh(repo).GetIssue(ctx, issue)
	if err != nil {
		return nil, err
	}
	it, _, err := p.Store.UpsertItemIn(ctx, repo, issue, is.Title, state.Reviewing)
	if err != nil {
		return nil, err
	}
	if err := p.approve(ctx, it, *is, "the reviewer eval"); err != nil {
		return nil, err
	}
	size, branch := "M", p.branchFor(it)
	if err := p.Store.UpdateItem(ctx, it.ID, store.ItemUpdate{Risk: &risk, Complexity: &size, Branch: &branch}); err != nil {
		return nil, err
	}
	it.Risk, it.Complexity, it.Branch = risk, size, branch
	if err := p.enqueueReview(ctx, it, "review", 1, ReviewInput{Head: head, Gates: "All required gates passed."}); err != nil {
		return nil, err
	}
	// Only review and critic jobs run: a block queues a fix round, which
	// an eval of the reviewer leaves queued.
	skip := []string{"triage", "implement", "orchestrate", "tune", "learn", "groom", store.KindVerify, store.KindParallelCritic}
	for {
		j, err := p.Store.ClaimJobSkipping(ctx, skip, nil)
		if err != nil {
			return nil, err
		}
		if j == nil {
			break
		}
		p.runJob(ctx, j)
	}
	return p.Store.GetItem(ctx, it.ID)
}

// ReviewScore is what a reviewer eval run measured, for the tuning gate.
// A case the review gave no ruling on (an error, a broken verdict) counts
// in Errors and in no other count.
type ReviewScore struct {
	Planted int     `json:"planted"` // cases with a planted bug
	Caught  int     `json:"caught"`  // of those, blocked with a finding at the bug
	Recall  float64 `json:"recall"`
	Clean   int     `json:"clean"`           // known-good merged changes
	FalsePo int     `json:"false_positives"` // of those, blocked
	FPRate  float64 `json:"false_positive_rate"`
	Reverts int     `json:"reverts"`         // merged changes later reverted
	Stopped int     `json:"reverts_blocked"` // of those, blocked
	Errors  int     `json:"errors"`
	CostUSD float64 `json:"cost_usd"`
	// Outcomes is each case's result by name: caught, missed, ok,
	// false-positive or error.
	Outcomes map[string]string `json:"outcomes"`
}

// Rates fills in the rates from the counts.
func (s *ReviewScore) Rates() {
	s.Recall, s.FPRate = 0, 0
	if s.Planted > 0 {
		s.Recall = float64(s.Caught) / float64(s.Planted)
	}
	if s.Clean > 0 {
		s.FPRate = float64(s.FalsePo) / float64(s.Clean)
	}
}

// Review case outcomes, best first for each kind.
const (
	OutcomeCaught   = "caught"
	OutcomeMissed   = "missed"
	OutcomeOK       = "ok"
	OutcomeFalsePos = "false-positive"
	OutcomeError    = "error"
)

// ReviewRegressions says how cur is worse than base: a different set of
// cases (they can't be compared), more cases with no ruling, recall fallen
// or the false-positive rate risen by more than tolerance (a share, 0.05
// for five points), or fewer reverted changes blocked. Each case that got
// worse is named. Empty means no worse.
func ReviewRegressions(cur, base ReviewScore, tolerance float64) []string {
	if diff := caseDiff(cur.Outcomes, base.Outcomes); diff != "" {
		return []string{"the runs reviewed different cases (" + diff + "), so they can't be compared"}
	}
	var why []string
	if cur.Errors > base.Errors {
		why = append(why, fmt.Sprintf("%d cases got no ruling, up from %d", cur.Errors, base.Errors))
	}
	if base.Planted > 0 && cur.Recall < base.Recall-tolerance-1e-9 {
		why = append(why, fmt.Sprintf("reviewer recall fell from %.0f%% to %.0f%%", base.Recall*100, cur.Recall*100))
	}
	if base.Clean > 0 && cur.FPRate > base.FPRate+tolerance+1e-9 {
		why = append(why, fmt.Sprintf("reviewer false-positive rate rose from %.0f%% to %.0f%%", base.FPRate*100, cur.FPRate*100))
	}
	if base.Reverts > 0 && cur.Stopped < base.Stopped {
		why = append(why, fmt.Sprintf("the reviewer blocked %d of %d later-reverted changes, down from %d", cur.Stopped, cur.Reverts, base.Stopped))
	}
	if len(why) == 0 {
		return nil
	}
	var worse []string
	for name, was := range base.Outcomes {
		if now := cur.Outcomes[name]; now != was && (was == OutcomeCaught || was == OutcomeOK || now == OutcomeError) {
			worse = append(worse, name+" "+was+" → "+now)
		}
	}
	if len(worse) > 0 {
		sort.Strings(worse)
		why = append(why, "worse: "+strings.Join(worse, ", "))
	}
	return why
}

// caseDiff names the cases only one of a and b holds, or "".
func caseDiff(a, b map[string]string) string {
	var only []string
	for n := range a {
		if _, ok := b[n]; !ok {
			only = append(only, n+" only in the candidate")
		}
	}
	for n := range b {
		if _, ok := a[n]; !ok {
			only = append(only, n+" only in the baseline")
		}
	}
	sort.Strings(only)
	if len(only) > 3 {
		only = append(only[:3], fmt.Sprintf("%d more", len(only)-3))
	}
	return strings.Join(only, ", ")
}

// ReviewEvaluator runs the reviewer eval set on a config, spending at most
// about capUSD (0 for no cap): a case starts only while there's budget
// left, and a run that stops for it is an error. The daemon's wiring sets
// one (internal/eval can't be imported from here).
type ReviewEvaluator func(ctx context.Context, cfg *config.Config, capUSD float64) (ReviewScore, error)

// reviewBehaviour is everything that decides how a change is reviewed,
// resolved: the routes each risk, the verifier, the critic and the
// security pass run on (fallbacks included), their settings, budgets and
// timeouts, and where the prompts live. Two configs with equal ones
// review alike.
type reviewBehaviour struct {
	Low, Medium, High, Verify, Critic, Security config.Route
	Reviews                                     config.Reviews
	SecurityConf                                config.Security
	ReviewBudget, VerifyBudget, FastBudget      float64
	Timeouts                                    []time.Duration
	PromptsDir                                  string
}

func behaviourOf(c *config.Config) reviewBehaviour {
	b := reviewBehaviour{
		Low: c.Review("low"), Medium: c.Review("medium"), High: c.Review("high"),
		Verify: c.VerifyRoute(), Critic: c.CriticRoute(), Security: c.SecurityRoute(),
		Reviews: c.Reviews, SecurityConf: c.Security,
		ReviewBudget: c.JobBudget("review", ""), VerifyBudget: c.JobBudget("verify", ""), FastBudget: c.FastBudget(1, true),
		PromptsDir: c.Path(c.PromptsDir),
	}
	for _, stage := range []string{"review", "critic", "verify", "security"} {
		b.Timeouts = append(b.Timeouts, c.Timeout(stage))
	}
	return b
}

// gateReview runs the reviewer evals on the current config and on the
// candidate when tuning.review_eval is on and a kept change alters how
// review works (see reviewBehaviour), each change judged on its own
// against the current file. A candidate worse than
// tuning.review_eval_tolerance allows, or evals that can't run, lose
// those changes, each dropped with why. It returns the changes still
// kept, the dropped ones added, and a line for the summary.
func (p *Pipeline) gateReview(ctx context.Context, src []byte, cfgDir string, kept, dropped []store.ProposedChange) ([]store.ProposedChange, []store.ProposedChange, string) {
	t := p.Cfg.Tuning
	if !t.ReviewEval || p.ReviewEval == nil || len(kept) == 0 {
		return kept, dropped, ""
	}
	cur, err := config.ParseIn(src, cfgDir)
	if err != nil {
		return kept, dropped, "" // Proposed kept nothing it couldn't load, so this isn't reached
	}
	was := behaviourOf(cur)
	review := map[int]bool{}
	for i, c := range kept {
		one, why := config.Proposed(src, cfgDir, []config.Change{{Path: c.Path, Value: c.Value}})
		next, err := config.ParseIn(one, cfgDir)
		if why[0] != "" || err != nil || !reflect.DeepEqual(behaviourOf(next), was) {
			review[i] = true
		}
	}
	if len(review) == 0 {
		return kept, dropped, ""
	}
	drop := func(why string) ([]store.ProposedChange, []store.ProposedChange) {
		still := []store.ProposedChange{}
		for i, c := range kept {
			if review[i] {
				c.Reason = why
				dropped = append(dropped, c)
			} else {
				still = append(still, c)
			}
		}
		return still, dropped
	}
	changes := make([]config.Change, len(kept))
	for i, c := range kept {
		changes[i] = config.Change{Path: c.Path, Value: c.Value}
	}
	edited, _ := config.Proposed(src, cfgDir, changes)
	cand, err := config.ParseIn(edited, cfgDir)
	if err != nil {
		k, d := drop("the reviewer evals could not load the proposal: " + truncate(err.Error(), 200))
		return k, d, ""
	}
	// It spends like the tuning run it belongs to, from what is left of
	// today's budget, half for each run.
	if why := p.tuneBudget(ctx); why != "" {
		k, d := drop("the reviewer evals could not run: " + why)
		return k, d, ""
	}
	spent, err := p.buildSpend(ctx)
	if err != nil {
		k, d := drop("the reviewer evals could not run: " + err.Error())
		return k, d, ""
	}
	capUSD := (p.Cfg.Budgets.DailyTotal - spent) / 2
	spend := func(s ReviewScore) {
		if s.CostUSD > 0 {
			p.warn(p.Store.AddSpend(ctx, s.CostUSD, "daemon", "reviewer evals for a tuning proposal"), "AddSpend")
		}
	}
	base, err := p.ReviewEval(ctx, cur, capUSD)
	spend(base)
	if err != nil {
		k, d := drop("the reviewer evals could not run: " + truncate(err.Error(), 200))
		return k, d, fmt.Sprintf("Reviewer evals: $%.2f spent.", base.CostUSD)
	}
	next, err := p.ReviewEval(ctx, cand, capUSD)
	spend(next)
	cost := base.CostUSD + next.CostUSD
	if err != nil {
		k, d := drop("the reviewer evals could not run: " + truncate(err.Error(), 200))
		return k, d, fmt.Sprintf("Reviewer evals: $%.2f spent.", cost)
	}
	line := fmt.Sprintf("Reviewer evals: recall %.0f%% → %.0f%%, false positives %.0f%% → %.0f%%, $%.2f spent.",
		base.Recall*100, next.Recall*100, base.FPRate*100, next.FPRate*100, cost)
	if why := ReviewRegressions(next, base, t.ReviewTolerance()); len(why) > 0 {
		k, d := drop("the reviewer evals got worse: " + strings.Join(why, "; "))
		return k, d, line
	}
	return kept, dropped, line
}
