package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

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
// finding blocks again), and names the findings it raised. It returns the
// earlier findings it changed, for saveFindings.
func (p *Pipeline) settle(ctx context.Context, j *store.Job, it *store.WorkItem, v *verdict.Review, rr *reviewRound) (map[string]*store.ReviewFinding, error) {
	open := map[string]*store.ReviewFinding{}
	for i := range rr.open {
		open[rr.open[i].FID] = &rr.open[i]
	}
	changed := map[string]*store.ReviewFinding{}
	var kept []verdict.Ruling
	var carried []verdict.Finding
	for _, e := range v.Earlier {
		f, ok := open[e.ID]
		if !ok || changed[e.ID] != nil {
			continue // not one it was shown, or ruled on twice
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
			if strings.TrimSpace(e.Reason) != "" {
				ev = e.Reason
			}
			carried = append(carried, verdict.Finding{Severity: f.Severity, File: f.File, Line: f.Line, Claim: f.Claim, Evidence: ev, ID: f.FID, Carried: true})
		}
		changed[e.ID] = f
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

// reviewer names who raised findings in a review: the job's own reviewer,
// and in a paired review the critic beside it.
type reviewer struct{ kind, route, harness, model string }

// saveFindings records this round's findings and the earlier ones it
// changed, with how each stands now.
func (p *Pipeline) saveFindings(ctx context.Context, j *store.Job, it *store.WorkItem, v *verdict.Review, rr *reviewRound, changed map[string]*store.ReviewFinding, by map[string]reviewer) {
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
	p.warn(p.Store.SaveReview(ctx, j.ID, raised, updated), "SaveReview")
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
func (p *Pipeline) verifyFindings(ctx context.Context, j *store.Job, it *store.WorkItem, v *verdict.Review, work, diff string, d PromptData) {
	idx := v.Blocking()
	if len(idx) == 0 {
		return
	}
	v.Verified = true
	mark := func(why string) {
		for _, i := range idx {
			if v.Findings[i].Check == nil {
				v.Findings[i].Check = &verdict.Check{Status: verdict.Unverified, Reason: why}
			}
		}
	}
	r := p.Cfg.VerifyRoute()
	if why := p.cantVerify(ctx, j, r.Harness); why != "" {
		mark(why)
		return
	}
	sub, err := p.Store.StartSubJob(ctx, j.ID, store.Job{ItemID: &it.ID, Kind: store.KindVerify, Harness: r.Harness, Model: r.Model, Effort: r.Effort,
		Priority: it.Priority, BudgetUSD: p.Cfg.JobBudget("verify", ""), Branch: j.Branch, Route: "verify"})
	if err != nil {
		mark("the verifier couldn't start: " + err.Error())
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

// cantVerify says why the verifier can't run now, or "": a harness that
// isn't there, can't sign in, is held by its usage limit or is in use by
// another job it can't share a sign-in with, or a daily budget its cap
// would go over.
func (p *Pipeline) cantVerify(ctx context.Context, j *store.Job, h string) string {
	hn, ok := p.Harnesses[h]
	if !ok {
		return fmt.Sprintf("no %s harness", h)
	}
	if slices.Contains(p.pausedHarnesses(ctx), h) {
		return h + "'s usage limit is reached"
	}
	if g, ok := hn.(harness.Gated); ok {
		if msg := g.Unavailable(); msg != "" {
			return msg
		}
		if g.OneAtATime() {
			jobs, err := p.Store.ListJobs(ctx, store.JobFilter{State: state.Running})
			if err != nil {
				return err.Error()
			}
			for _, o := range jobs {
				if o.Harness == h && o.ID != j.ID {
					return h + " is running another job, and runs one at a time"
				}
			}
		}
	}
	spent, err := p.buildSpend(ctx)
	if err != nil {
		return err.Error()
	}
	if spent+p.Cfg.JobBudget("verify", "") > p.Cfg.Budgets.DailyTotal {
		return fmt.Sprintf("the daily budget can't cover it ($%.2f of $%.2f spent)", spent, p.Cfg.Budgets.DailyTotal)
	}
	return ""
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

// pairedRun is the critic's half of a paired review.
type pairedRun struct {
	job *store.Job
	v   *verdict.Review
	err string
}

// startCritic starts the critic of a paired review on the reviewer's
// checkout and returns a wait for its verdict. When it shares a harness
// that runs one job at a time with the reviewer, it runs when wait is
// called, after the reviewer, instead of beside it.
func (p *Pipeline) startCritic(ctx context.Context, j *store.Job, it *store.WorkItem, work string, d PromptData) func() pairedRun {
	r := p.Cfg.CriticRoute()
	sub, err := p.Store.StartSubJob(ctx, j.ID, store.Job{ItemID: &it.ID, Kind: store.KindParallelCritic, Harness: r.Harness, Model: r.Model, Effort: r.Effort,
		Priority: it.Priority, BudgetUSD: p.Cfg.JobBudget("review", ""), Branch: j.Branch, Route: "critic"})
	if err != nil {
		return func() pairedRun { return pairedRun{err: err.Error()} }
	}
	run := func() pairedRun {
		res := pairedRun{job: sub}
		out := filepath.Join(p.jobDir(sub.ID), "out")
		if err := os.MkdirAll(out, 0o755); err != nil {
			res.err = err.Error()
			p.finishSub(ctx, sub, agentRun{}, state.Failed, "", res.err)
			return res
		}
		defer func() { _ = os.RemoveAll(out) }()
		// The critic's own instructions: its route's, and the repos' for critics.
		as := *sub
		as.Kind = "critic"
		d.Screenshots = nil // they are in the reviewer's output directory
		prompt, err := p.renderStep(&as, "review", []string{it.Repo}, d)
		if err != nil {
			res.err = err.Error()
			p.finishSub(ctx, sub, agentRun{}, state.Failed, "", res.err)
			return res
		}
		schema, _ := verdict.Schema("review")
		ar := p.runAgentWith(ctx, sub, work, out, prompt, schema, p.Cfg.Timeout("review"), agentExtras{repo: p.members(it)[0]})
		if st, msg, bad := runFailure(ar); bad {
			if ctx.Err() != nil {
				st = state.Cancelled
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
	if r.Harness == j.Harness && gated && g.OneAtATime() {
		return run
	}
	ch := make(chan pairedRun, 1)
	go func() { ch <- run() }()
	return func() pairedRun { return <-ch }
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
		if x.r.Decision == "block" && len(x.r.Findings) == 0 {
			ok = false
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
