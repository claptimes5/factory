package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/claptimes5/factory/internal/config"
	"github.com/claptimes5/factory/internal/harness"
	"github.com/claptimes5/factory/internal/state"
	"github.com/claptimes5/factory/internal/store"
	"github.com/claptimes5/factory/internal/verdict"
)

// This file is how a review checks itself and remembers: each finding gets
// an id that holds across the item's rounds, a verifier rules on each
// blocking one before it blocks, the builder answers each in its fix
// round, and the next review rules on those answers while it reads only
// what changed since it last looked.

// reviewRound is what a review knows of the item's earlier rounds.
type reviewRound struct {
	n      int        // this round: 1 plus the blocks since the latest triage
	triage int64      // the latest triage job, where this run of rounds began
	prior  *store.Job // this reviewer's last finished review of the item, if any
	// open are blocking findings no review has settled: this one rules on
	// each. seen are the rest raised before, which it shouldn't repeat.
	open, seen []store.ReviewFinding
}

// round reads the item's earlier rounds for review job j.
func (p *Pipeline) round(ctx context.Context, j *store.Job, it *store.WorkItem) (*reviewRound, error) {
	rr := &reviewRound{n: 1}
	blocks, err := p.countBlocks(ctx, it.ID)
	if err != nil {
		return nil, err
	}
	rr.n += blocks
	jobs, err := p.Store.ListJobs(ctx, store.JobFilter{ItemID: it.ID})
	if err != nil {
		return nil, err
	}
	for _, o := range jobs { // newest first
		if o.Kind == "triage" {
			rr.triage = o.ID
			break
		}
		if rr.prior == nil && o.ID < j.ID && o.Kind == j.Kind && o.State == state.Succeeded && o.BaseSHA != "" {
			rr.prior = o
		}
	}
	fs, err := p.Store.ItemFindings(ctx, it.ID, rr.triage)
	if err != nil {
		return nil, err
	}
	for _, f := range fs {
		// Not this job's own, from a run that didn't finish, nor what
		// another reviewer raised this round (the reviewer a critic follows).
		if f.JobID == j.ID || f.Round >= rr.n {
			continue
		}
		if stillOpen(f) {
			rr.open = append(rr.open, f)
		} else {
			rr.seen = append(rr.seen, f)
		}
	}
	return rr, nil
}

// stillOpen says whether a finding blocks until a review rules on it: it
// blocked, the verifier didn't refute it, and no review has found it gone
// or wrong.
func stillOpen(f store.ReviewFinding) bool {
	return f.Blocking && f.Check != verdict.Refuted && f.Ruling != "resolved" && f.Ruling != "withdrawn"
}

// incremental points a re-review at what changed since the reviewer last
// looked: the diff from the commit it reviewed to the head, with the whole
// change as context when it is small and as a list of files when not. It
// leaves d alone, for a full review, on the first round, in a workspace,
// or when that commit isn't behind the head.
func (p *Pipeline) incremental(ctx context.Context, it *store.WorkItem, pt *part, rr *reviewRound, full string, d *PromptData) {
	if !p.Cfg.Reviews.IncrementalOn() || rr.prior == nil {
		return
	}
	from, g := rr.prior.BaseSHA, p.git(pt.repo)
	if from == pt.head || !g.IsAncestor(ctx, pt.ws.Repo, from, pt.head) || !g.IsAncestor(ctx, pt.ws.Repo, pt.base, from) {
		return
	}
	inc, err := g.Diff(ctx, pt.ws.Repo, from, pt.head)
	if err != nil || strings.TrimSpace(inc) == "" {
		return
	}
	inc = truncate(inc, 200_000)
	d.Diff, d.Fence, d.Since = inc, fence(inc), short(from)
	if len(full) <= p.Cfg.Reviews.ContextBytes {
		d.Context, d.ContextFence = full, fence(full)
		return
	}
	files, err := g.ChangedFiles(ctx, pt.ws.Repo, pt.base, pt.head)
	if err != nil {
		files = []string{"(the file list couldn't be read: " + err.Error() + ")"}
	}
	if len(files) > 300 {
		files = append(files[:300], fmt.Sprintf("(and %d more)", len(files)-300))
	}
	for i, f := range files {
		files[i] = SafeText(f, 300)
	}
	d.ContextFiles = fenced(strings.Join(files, "\n"))
}

// earlierText is a re-review's view of the earlier rounds, fenced: the
// open findings with the builder's answers, then what was already raised.
// The answers are the builder's words and the findings a model's.
func earlierText(rr *reviewRound) string {
	if len(rr.open) == 0 && len(rr.seen) == 0 {
		return ""
	}
	var b strings.Builder
	if len(rr.open) > 0 {
		b.WriteString("Open findings (rule on each by id):\n\n")
	}
	for _, f := range rr.open {
		fmt.Fprintf(&b, "%s (%s) %s, raised in round %d\n  Claim: %s\n", f.FID, f.Severity, findingLoc(f.File, f.Line), f.Round, oneLine(f.Claim))
		if f.Evidence != "" {
			fmt.Fprintf(&b, "  Evidence: %s\n", oneLine(truncate(f.Evidence, 1000)))
		}
		if f.Ruling == "upheld" {
			fmt.Fprintf(&b, "  Upheld by a review: %s\n", oneLine(truncate(f.RulingReason, 1000)))
		}
		switch f.Answer {
		case "":
			b.WriteString("  Builder's answer: none yet\n")
		case "unanswered":
			b.WriteString("  Builder's answer: it didn't answer this one\n")
		default:
			fmt.Fprintf(&b, "  Builder's answer: %s: %s\n", f.Answer, oneLine(truncate(f.AnswerWhy, 1000)))
		}
		b.WriteString("\n")
	}
	if len(rr.seen) > 0 {
		b.WriteString("Already raised, and not open (don't raise these again without new evidence):\n\n")
	}
	for i, f := range rr.seen {
		if i == 40 {
			fmt.Fprintf(&b, "(and %d more)\n", len(rr.seen)-i)
			break
		}
		fmt.Fprintf(&b, "%s (%s) %s: %s [%s]\n", f.FID, f.Severity, findingLoc(f.File, f.Line), oneLine(truncate(f.Claim, 300)), seenWhy(f))
	}
	return fenced(b.String())
}

func seenWhy(f store.ReviewFinding) string {
	switch {
	case f.Check == verdict.Refuted:
		return "refuted by the verifier: " + oneLine(truncate(f.CheckReason, 300))
	case f.Ruling == "resolved":
		return "resolved"
	case f.Ruling == "withdrawn":
		return "withdrawn: " + oneLine(truncate(f.RulingReason, 300))
	case f.Severity == verdict.PreExisting:
		return "pre-existing, a note"
	case stillOpen(f) && f.Kind == store.KindSecurity:
		return "open: the security review rules on it"
	case stillOpen(f):
		return "open: the reviewer rules on it"
	}
	return "a note"
}

func findingLoc(file string, line int) string {
	if file == "" {
		return "(no file)"
	}
	if line > 0 {
		return fmt.Sprintf("%s:%d", file, line)
	}
	return file
}

// settle applies a review's rulings on the open findings it was shown,
// carries each one it upheld into this round with its id (an upheld
// finding blocks again), and names the findings it raised. An open
// finding it didn't rule on stands as if upheld: only a ruling can lift
// a block. It returns the earlier findings it changed, for finishReview.
func (p *Pipeline) settle(ctx context.Context, j *store.Job, it *store.WorkItem, v *verdict.Review, rr *reviewRound) (map[string]*store.ReviewFinding, error) {
	// One ruling per finding it was shown; of two, an upheld one stands.
	rulings := map[string]verdict.Ruling{}
	for _, e := range v.Earlier {
		if !slices.ContainsFunc(rr.open, func(f store.ReviewFinding) bool { return f.FID == e.ID }) {
			continue
		}
		if prev, seen := rulings[e.ID]; !seen || (e.Ruling == "upheld" && prev.Ruling != "upheld") {
			rulings[e.ID] = e
		}
	}
	changed := map[string]*store.ReviewFinding{}
	var kept []verdict.Ruling
	var carried []verdict.Finding
	yes := true
	for i := range rr.open {
		f := &rr.open[i]
		e, ok := rulings[f.FID]
		if !ok {
			e = verdict.Ruling{ID: f.FID, Ruling: "upheld", Reason: noRuling}
		}
		kept = append(kept, e)
		f.Ruling, f.RulingReason, f.RulingJob = e.Ruling, truncate(e.Reason, 1000), j.ID
		switch e.Ruling {
		case "resolved":
			f.Outcome = store.OutcomeResolved
			if f.Answer == "fixed" {
				f.Outcome = store.OutcomeFixed
			}
		case "withdrawn":
			f.Outcome = store.OutcomeWithdrawn
		case "upheld":
			f.Outcome = store.OutcomeOpen
			ev := f.Evidence
			switch {
			case !ok:
				ev = strings.TrimSpace(noRuling + ", so it stands. " + f.Evidence)
			case strings.TrimSpace(e.Reason) != "":
				ev = e.Reason
			}
			by := ""
			if f.Kind == store.KindSecurity {
				by = store.KindSecurity
			}
			carried = append(carried, verdict.Finding{Severity: f.Severity, File: f.File, Line: f.Line, Claim: f.Claim, Evidence: ev, ID: f.FID, By: by, Carried: true, Blocks: &yes})
		}
		changed[f.FID] = f
	}
	v.Earlier = kept
	// An upheld finding still stands, so the round can't pass over it.
	if len(carried) > 0 && v.Decision == "pass" {
		v.Decision = "block"
	}
	seq, err := p.Store.NextFindingSeq(ctx, it.ID, j.ID)
	if err != nil {
		return nil, err
	}
	for i := range v.Findings {
		v.Findings[i].ID = store.FindingID(seq)
		seq++
	}
	v.Findings = append(v.Findings, carried...)
	return changed, nil
}

// noRuling is the reason an open finding the review didn't rule on stands.
const noRuling = "the review didn't rule on it"

// reviewer names who raised findings in a review: the job's own reviewer,
// and in a paired review the critic beside it.
type reviewer struct{ kind, route, harness, model string }

// finishReview ends a review that produced verdict v, recording this
// round's findings and the earlier ones it changed, with how each stands
// now, in the same transaction. If that fails, nothing is recorded and the
// review must go no further.
func (p *Pipeline) finishReview(ctx context.Context, j *store.Job, it *store.WorkItem, run agentRun, v *verdict.Review, rr *reviewRound, changed map[string]*store.ReviewFinding, by map[string]reviewer) error {
	blocking := v.Blocking()
	var raised, updated []store.ReviewFinding
	for i, f := range v.Findings {
		blocks := slices.Contains(blocking, i)
		outcome := store.OutcomeOpen
		switch {
		case f.Severity == verdict.PreExisting:
			outcome = store.OutcomePreExisting
		case !blocks:
			outcome = store.OutcomeNote
		case f.Check != nil && f.Check.Status == verdict.Refuted:
			outcome = store.OutcomeRefuted
		}
		var st, reason, cby string
		if f.Check != nil {
			st, reason, cby = f.Check.Status, truncate(f.Check.Reason, 1000), f.Check.By
		}
		if f.Carried {
			row := changed[f.ID]
			if row == nil {
				continue
			}
			row.Blocking, row.Check, row.CheckReason, row.CheckBy = true, st, reason, cby
			row.Confirmed = row.Confirmed || st == verdict.Confirmed
			row.Outcome = outcome
			continue
		}
		who := by[f.By]
		raised = append(raised, store.ReviewFinding{ItemID: it.ID, FID: f.ID, Seq: seqOf(f.ID), Kind: who.kind, Route: who.route, Harness: who.harness, Model: who.model,
			Round: rr.n, Severity: f.Severity, File: truncate(f.File, 500), Line: f.Line, Claim: truncate(f.Claim, 2000), Evidence: truncate(f.Evidence, 2000),
			Blocking: blocks, Check: st, CheckReason: reason, CheckBy: cby, Confirmed: st == verdict.Confirmed, Outcome: outcome})
	}
	for _, f := range changed {
		updated = append(updated, *f)
	}
	slices.SortFunc(updated, func(a, b store.ReviewFinding) int { return a.Seq - b.Seq })
	norm, _ := json.Marshal(v)
	if err := p.Store.FinishReview(ctx, j.ID, p.result(ctx, j, run, state.Succeeded, string(norm), ""), raised, updated); err != nil {
		return fmt.Errorf("finish review %d: %w", j.ID, err)
	}
	return nil
}

func seqOf(fid string) int {
	var n int
	_, _ = fmt.Sscanf(fid, "F%d", &n)
	return n
}

// --- the verifier ---

// verifyFindings sends a review's blocking findings to the verifier, which
// confirms or refutes each against the code. Whatever it doesn't rule on,
// because it couldn't run, ran out of budget or skipped one, is marked
// unverified and blocks: the verifier can only take a block away.
//
// The security pass's findings are held to more: a critical one, or one
// the pass upheld from an earlier round, isn't sent to a verifier at all
// and blocks; the rest are checked on sec, the pass's own route, never the
// cheap verifier's. sec is nil when no pass read the change this round.
func (p *Pipeline) verifyFindings(ctx context.Context, j *store.Job, it *store.WorkItem, v *verdict.Review, work, diff string, d PromptData, sec *config.Route) {
	idx := v.Blocking()
	if len(idx) == 0 {
		return
	}
	v.Verified = true
	var plain, secure []int
	for _, i := range idx {
		f := &v.Findings[i]
		switch {
		case f.By != store.KindSecurity:
			plain = append(plain, i)
		case f.Carried:
			f.Check = &verdict.Check{Status: verdict.Unverified, Reason: "the security pass upheld it, so it isn't checked again"}
		case f.Severity == verdict.Critical:
			f.Check = &verdict.Check{Status: verdict.Unverified, Reason: "a critical security finding isn't sent to a verifier: it blocks"}
		case sec == nil:
			f.Check = &verdict.Check{Status: verdict.Unverified, Reason: "no security pass ran this round to check it"}
		default:
			secure = append(secure, i)
		}
	}
	p.verifyOn(ctx, j, it, v, plain, p.Cfg.VerifyRoute(), p.Cfg.JobBudget("verify", ""), work, diff, d)
	if sec != nil {
		p.verifyOn(ctx, j, it, v, secure, *sec, p.Cfg.JobBudget("review", ""), work, diff, d)
	}
}

// verifyOn sends findings idx of v to a verifier on route r, capped at
// budget. Whatever it doesn't rule on is marked unverified, and blocks.
func (p *Pipeline) verifyOn(ctx context.Context, j *store.Job, it *store.WorkItem, v *verdict.Review, idx []int, r config.Route, budget float64, work, diff string, d PromptData) {
	if len(idx) == 0 {
		return
	}
	mark := func(why string) {
		for _, i := range idx {
			if v.Findings[i].Check == nil {
				v.Findings[i].Check = &verdict.Check{Status: verdict.Unverified, Reason: why}
			}
		}
	}
	sub, why := p.startSub(ctx, j, store.Job{ItemID: &it.ID, Kind: store.KindVerify, Harness: r.Harness, Model: r.Model, Effort: r.Effort,
		Priority: it.Priority, BudgetUSD: budget, Branch: j.Branch, Route: "verify"})
	if sub == nil {
		mark(why)
		return
	}
	out := filepath.Join(p.jobDir(sub.ID), "out")
	if err := os.MkdirAll(out, 0o755); err != nil {
		p.finishSub(ctx, sub, agentRun{}, state.Failed, "", err.Error())
		mark("the verifier couldn't start: " + err.Error())
		return
	}
	defer func() { _ = os.RemoveAll(out) }()
	var b strings.Builder
	for _, i := range idx {
		f := v.Findings[i]
		fmt.Fprintf(&b, "%s (%s) %s\n  Claim: %s\n  Evidence: %s\n", f.ID, f.Severity, findingLoc(f.File, f.Line), oneLine(f.Claim), oneLine(truncate(f.Evidence, 2000)))
		if f.Carried {
			b.WriteString("  (raised in an earlier round; the reviewer says it is still there)\n")
		}
		b.WriteString("\n")
	}
	d.Findings = fenced(b.String())
	// The whole change, whatever part of it a re-review read.
	d.Screenshots, d.Earlier, d.Diff, d.Fence = nil, "", diff, fence(diff)
	prompt, err := p.renderStep(sub, "verify", []string{it.Repo}, d)
	if err != nil {
		p.finishSub(ctx, sub, agentRun{}, state.Failed, "", err.Error())
		mark("the verifier's prompt failed: " + err.Error())
		return
	}
	schema, _ := verdict.Schema("verify")
	run := p.runAgentWith(ctx, sub, work, out, prompt, schema, p.Cfg.Timeout("verify"), agentExtras{repo: p.members(it)[0]})
	if st, msg, bad := runFailure(run); bad {
		if ctx.Err() != nil {
			st = state.Cancelled
		}
		p.finishSub(ctx, sub, run, st, "", msg)
		mark("the verifier failed: " + truncate(msg, 300))
		return
	}
	var vv verdict.Verify
	if err := verdict.Parse([]byte(run.final), &vv); err != nil {
		p.finishSub(ctx, sub, run, state.Failed, "", err.Error())
		mark("the verifier's verdict was unusable: " + truncate(err.Error(), 300))
		return
	}
	norm, _ := json.Marshal(vv)
	p.finishSub(ctx, sub, run, state.Succeeded, string(norm), "")
	by := r.Harness + "/" + r.Model
	for _, c := range vv.Checks {
		for _, i := range idx {
			if f := &v.Findings[i]; f.ID == c.ID && f.Check == nil {
				f.Check = &verdict.Check{Status: c.Verdict, Reason: truncate(c.Reason, 1000), By: by}
			}
		}
	}
	mark("the verifier didn't rule on it")
}

// cantRun says why a sub-job of job j can't run on harness h now, or "":
// a harness that isn't there, one harnessHeld holds, or a daily budget its
// cap would go over. It is the scheduler's check for a sub-job, which the
// scheduler doesn't claim: the caller holds claimMu from the check to the
// sub-job's start, so the scheduler can't claim a job for h in between.
func (p *Pipeline) cantRun(ctx context.Context, j *store.Job, h string, budget float64) string {
	if _, ok := p.Harnesses[h]; !ok {
		return fmt.Sprintf("no %s harness", h)
	}
	if why := p.harnessHeld(ctx, j, h); why != "" {
		return why
	}
	spent, err := p.buildSpend(ctx)
	if err != nil {
		return err.Error()
	}
	if spent+budget > p.Cfg.Budgets.DailyTotal {
		return fmt.Sprintf("the daily budget can't cover it ($%.2f of $%.2f spent)", spent, p.Cfg.Budgets.DailyTotal)
	}
	return ""
}

// harnessHeld says why harness h can't take a sub-job of job j now, as
// the scheduler would hold a job of its own, or "": it can't sign in, its
// usage limit is reached, or it runs one job at a time and another job
// holds it. Job j and the sub-jobs it runs don't count: one that must
// wait for them runs after them (see startBeside).
func (p *Pipeline) harnessHeld(ctx context.Context, j *store.Job, h string) string {
	if slices.Contains(p.pausedHarnesses(ctx), h) {
		return h + "'s usage limit is reached"
	}
	g, ok := p.Harnesses[h].(harness.Gated)
	if !ok {
		return ""
	}
	if msg := g.Unavailable(); msg != "" {
		return msg
	}
	if g.OneAtATime() {
		jobs, err := p.Store.ListJobs(ctx, store.JobFilter{State: state.Running})
		if err != nil {
			return err.Error()
		}
		within := fmt.Sprintf(`{"within":%d}`, j.ID)
		for _, o := range jobs {
			if o.Harness == h && o.ID != j.ID && o.Input != within {
				return h + " is running another job, and runs one at a time"
			}
		}
	}
	return ""
}

// startSub starts sub-job sub of job j once cantRun allows it, or says
// why it can't.
func (p *Pipeline) startSub(ctx context.Context, j *store.Job, sub store.Job) (*store.Job, string) {
	lctx, unlock := p.lockClaims(ctx)
	defer unlock()
	if why := p.cantRun(lctx, j, sub.Harness, sub.BudgetUSD); why != "" {
		return nil, why
	}
	started, err := p.Store.StartSubJob(lctx, j.ID, sub)
	if err != nil {
		return nil, "it couldn't start: " + err.Error()
	}
	return started, ""
}

// finishSub ends a sub-job, whose parent may already be cancelled.
func (p *Pipeline) finishSub(ctx context.Context, j *store.Job, run agentRun, st state.JobState, verdict, msg string) {
	p.finish(context.WithoutCancel(ctx), j, run, st, verdict, msg)
}

// --- a paired review ---

// paired says whether review job j reads a high-risk item with its critic
// beside it.
func (p *Pipeline) paired(j *store.Job, it *store.WorkItem) bool {
	return j.Kind == "review" && it.Risk == "high" && p.Cfg.Reviews.PairedHighRisk
}

// pairedRun is the run of a sub-job beside the reviewer: a paired
// critic's, or the security pass's. skipped says it didn't run: a critic
// then follows the review, and a security pass fails it.
type pairedRun struct {
	job     *store.Job
	run     agentRun
	v       *verdict.Review
	err     string
	skipped string
}

// startCritic starts the critic of a paired review on the reviewer's
// checkout and returns a wait for its verdict, or nil when it can't run
// now (see cantRun), so the review isn't paired.
func (p *Pipeline) startCritic(ctx context.Context, j *store.Job, it *store.WorkItem, work string, repos []string, d PromptData, taken []string) func(goOn bool) pairedRun {
	wait, why := p.startBeside(ctx, j, it, work, repos, d, beside{kind: store.KindParallelCritic, route: "critic", r: p.Cfg.CriticRoute(), prompt: "review", as: "critic"}, taken)
	if wait == nil {
		p.log().Info("review not paired", "job", j.ID, "why", why)
	}
	return wait
}

// beside is an agent run that reads a change beside its reviewer, as a
// sub-job of the review: a paired critic, or the security pass.
type beside struct {
	kind   string       // the sub-job's kind
	route  string       // its routing entry
	r      config.Route // and what that entry resolves to
	prompt string       // its prompt, under prompts/
	as     string       // the step whose instructions it gets
}

// startBeside starts sub-job b of review job j on the reviewer's checkout
// and returns a wait for its verdict, or nil and why when it can't run
// now (see cantRun). When it needs a harness that runs one job at a time
// and that the review's agents already hold (taken: the reviewer's, and
// a paired critic's), it runs when wait is called, after them, instead of
// beside them; so a review waits on its sub-jobs in the order it started
// them. wait(false) means the reviewer failed: a sub-job beside it is
// stopped, and one after it doesn't run.
func (p *Pipeline) startBeside(ctx context.Context, j *store.Job, it *store.WorkItem, work string, repos []string, d PromptData, b beside, taken []string) (func(goOn bool) pairedRun, string) {
	r := b.r
	want := store.Job{ItemID: &it.ID, Kind: b.kind, Harness: r.Harness, Model: r.Model, Effort: r.Effort,
		Priority: it.Priority, BudgetUSD: p.Cfg.JobBudget("review", ""), Branch: j.Branch, Route: b.route}
	// Its own instructions: its route's, and the repos' for its step.
	as := want
	as.Kind = b.as
	d.Screenshots = nil // they are in the reviewer's output directory
	run := func(ctx context.Context, sub *store.Job) pairedRun {
		res := pairedRun{job: sub}
		out := filepath.Join(p.jobDir(sub.ID), "out")
		if err := os.MkdirAll(out, 0o755); err != nil {
			res.err = err.Error()
			p.finishSub(ctx, sub, agentRun{}, state.Failed, "", res.err)
			return res
		}
		defer func() { _ = os.RemoveAll(out) }()
		as.ID = sub.ID
		prompt, err := p.renderStep(&as, b.prompt, repos, d)
		if err != nil {
			res.err = err.Error()
			p.finishSub(ctx, sub, agentRun{}, state.Failed, "", res.err)
			return res
		}
		schema, _ := verdict.Schema("review")
		ar := p.runAgentWith(ctx, sub, work, out, prompt, schema, p.Cfg.Timeout("review"), agentExtras{repo: p.members(it)[0]})
		res.run = ar
		if st, msg, bad := runFailure(ar); bad {
			switch {
			case ctx.Err() != nil:
				st = state.Cancelled
			case ar.res != nil && ar.res.SignedOut:
				st, msg = state.Cancelled, msg+signedOutNote
			}
			res.err = msg
			p.finishSub(ctx, sub, ar, st, "", msg)
			return res
		}
		v, err := verdict.ParseReview([]byte(ar.final))
		if err != nil {
			res.err = err.Error()
			p.finishSub(ctx, sub, ar, state.Failed, "", res.err)
			return res
		}
		norm, _ := json.Marshal(v)
		p.finishSub(ctx, sub, ar, state.Succeeded, string(norm), "")
		res.v = v
		return res
	}
	g, gated := p.Harnesses[r.Harness].(harness.Gated)
	if slices.Contains(taken, r.Harness) && gated && g.OneAtATime() {
		// It runs after the reviewer (and critic), in the harness they hold;
		// whether it can is checked now, and again when it runs.
		if why := p.cantRun(ctx, j, r.Harness, want.BudgetUSD); why != "" {
			return nil, why
		}
		return func(goOn bool) pairedRun {
			if !goOn {
				return pairedRun{skipped: "the reviewer failed"}
			}
			sub, why := p.startSub(ctx, j, want)
			if sub == nil {
				return pairedRun{skipped: why}
			}
			return run(ctx, sub)
		}, ""
	}
	sub, why := p.startSub(ctx, j, want)
	if sub == nil {
		return nil, why
	}
	cctx, stop := context.WithCancel(ctx)
	ch := make(chan pairedRun, 1)
	go func() { ch <- run(cctx, sub) }()
	return func(goOn bool) pairedRun {
		if !goOn {
			stop()
		}
		res := <-ch
		stop()
		return res
	}, ""
}

// pair merges a paired review's two verdicts into one: either one's block
// blocks, and every finding of either goes to the verifier. ok is false
// when one blocked without a finding to check, so nothing is verified
// and the block stands.
func pair(a, b *verdict.Review) (v *verdict.Review, ok bool) {
	v = &verdict.Review{Findings: []verdict.Finding{}, Reviewers: []string{"review", "critic"}}
	ok = true
	for _, x := range []struct {
		r  *verdict.Review
		by string
	}{{a, "review"}, {b, "critic"}} {
		for _, f := range x.r.Findings {
			f.By = x.by
			v.Findings = append(v.Findings, f)
		}
		v.ChallengedAssumptions = append(v.ChallengedAssumptions, x.r.ChallengedAssumptions...)
		if x.r.BlockedWithoutFinding || (x.r.Decision == "block" && len(x.r.Findings) == 0) {
			// Its block stands, whatever the other side's findings.
			ok, v.BlockedWithoutFinding = false, true
		}
	}
	switch {
	case a.Decision == "block" || b.Decision == "block":
		v.Decision = "block"
	case a.Decision == "questions" || b.Decision == "questions":
		v.Decision = "questions"
	default:
		v.Decision = "pass"
	}
	// One ruling per earlier finding; an upheld one stands.
	at := map[string]int{}
	for _, e := range append(slices.Clone(a.Earlier), b.Earlier...) {
		if i, seen := at[e.ID]; !seen {
			at[e.ID] = len(v.Earlier)
			v.Earlier = append(v.Earlier, e)
		} else if e.Ruling == "upheld" {
			v.Earlier[i] = e
		}
	}
	return v, ok
}

// --- what the builder, the issue and the PR are shown ---

// fixFindings is a fix round's findings for the builder, fenced, and the
// ids it must answer: the ones the round blocks on, then the notes.
func fixFindings(v *verdict.Review) (string, []string) {
	var b strings.Builder
	var ids []string
	b.WriteString("Blocking findings (answer each by id):\n\n")
	for i, f := range v.Findings {
		if !v.BlocksOn(i) {
			continue
		}
		ids = append(ids, f.ID)
		writeFinding(&b, f)
	}
	if len(ids) == 0 {
		b.WriteString("(The reviewer blocked without a specific finding: see its challenged assumptions.)\n\n")
	}
	notes := false
	for i, f := range v.Findings {
		if v.BlocksOn(i) {
			continue
		}
		if !notes {
			b.WriteString("Notes that don't block (fix them if it's cheap; no answer needed):\n\n")
			notes = true
		}
		writeFinding(&b, f)
	}
	if len(v.ChallengedAssumptions) > 0 {
		b.WriteString("Challenged assumptions:\n")
		for _, a := range v.ChallengedAssumptions {
			fmt.Fprintf(&b, "- %s\n", oneLine(a))
		}
	}
	return fenced(b.String()), ids
}

func writeFinding(b *strings.Builder, f verdict.Finding) {
	id := f.ID
	if id == "" {
		id = "-"
	}
	fmt.Fprintf(b, "%s (%s) %s\n  Claim: %s\n", id, f.Severity, findingLoc(f.File, f.Line), oneLine(f.Claim))
	if f.Evidence != "" {
		fmt.Fprintf(b, "  Evidence: %s\n", oneLine(truncate(f.Evidence, 2000)))
	}
	if f.By == store.KindSecurity {
		b.WriteString("  Raised by the security review.\n")
	}
	if f.Carried {
		b.WriteString("  Raised in an earlier round and upheld by this review.\n")
	}
	if c := f.Check; c != nil {
		switch c.Status {
		case verdict.Confirmed:
			fmt.Fprintf(b, "  Verifier: confirmed: %s\n", oneLine(c.Reason))
		case verdict.Refuted:
			fmt.Fprintf(b, "  Verifier: refuted, so it doesn't block: %s\n", oneLine(c.Reason))
		default:
			fmt.Fprintf(b, "  Verifier: not checked (%s), so it blocks\n", oneLine(c.Reason))
		}
	}
	b.WriteString("\n")
}

// formatFindings is a verdict's findings for an issue comment or a park
// message. The text is the reviewer's, so it is fenced, where it can't
// link, close or mention anything.
func formatFindings(v *verdict.Review) string {
	if len(v.Findings) == 0 && len(v.ChallengedAssumptions) == 0 {
		return "_No individual findings._\n"
	}
	var block, notes strings.Builder
	for i, f := range v.Findings {
		if v.BlocksOn(i) {
			writeFinding(&block, f)
		} else {
			writeFinding(&notes, f)
		}
	}
	var b strings.Builder
	if block.Len() > 0 {
		b.WriteString("Blocking:\n\n" + fenced(block.String()) + "\n")
	}
	if notes.Len() > 0 {
		b.WriteString("Notes that don't block:\n\n" + fenced(notes.String()) + "\n")
	}
	if len(v.ChallengedAssumptions) > 0 {
		var a strings.Builder
		for _, x := range v.ChallengedAssumptions {
			fmt.Fprintf(&a, "- %s\n", oneLine(x))
		}
		b.WriteString("Challenged assumptions:\n\n" + fenced(a.String()))
	}
	return b.String()
}

// reviewNotes are the non-blocking findings of the review that passed the
// item, for its PR: worth a look, though none held it back.
func reviewNotes(v *verdict.Review) string {
	var b strings.Builder
	for i, f := range v.Findings {
		if !v.BlocksOn(i) {
			writeFinding(&b, f)
		}
	}
	if b.Len() == 0 {
		return ""
	}
	return fenced(b.String())
}

// recordAnswers saves the builder's answers to a fix round's findings.
// A finding it didn't answer is marked so, and stays open.
func (p *Pipeline) recordAnswers(ctx context.Context, j *store.Job, it *store.WorkItem, ids []string, answers []verdict.Answer) {
	if len(ids) == 0 {
		return
	}
	triage := int64(0)
	if ts, err := p.Store.ListJobs(ctx, store.JobFilter{ItemID: it.ID, Kind: "triage", Limit: 1}); err == nil && len(ts) > 0 {
		triage = ts[0].ID
	}
	fs, err := p.Store.ItemFindings(ctx, it.ID, triage)
	if err != nil {
		p.warn(err, "ItemFindings")
		return
	}
	by := map[string]verdict.Answer{}
	for _, a := range answers {
		if _, dup := by[a.ID]; !dup {
			by[a.ID] = a
		}
	}
	var upd []store.ReviewFinding
	for _, f := range fs {
		if !slices.Contains(ids, f.FID) {
			continue
		}
		f.AnswerJob = j.ID
		a, ok := by[f.FID]
		if !ok {
			f.Answer, f.AnswerWhy, f.Outcome = "unanswered", "", store.OutcomeOpen
			upd = append(upd, f)
			continue
		}
		f.Answer, f.AnswerWhy = a.Outcome, truncate(a.Why, 1000)
		switch a.Outcome {
		case "fixed":
			f.Outcome = store.OutcomeFixed
		case "wrong":
			f.Outcome = store.OutcomeDisputed
		default:
			f.Outcome = store.OutcomeOutOfScope
		}
		upd = append(upd, f)
	}
	p.warn(p.Store.UpdateFindings(ctx, upd), "UpdateFindings")
}
