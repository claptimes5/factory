package pipeline

import (
	"context"
	"slices"
	"sort"

	"github.com/claptimes5/factory/internal/harness"
	"github.com/claptimes5/factory/internal/state"
	"github.com/claptimes5/factory/internal/store"
)

// heldHarnesses are the harnesses whose jobs must stay queued for now:
// those paused at a usage limit, those that can't sign in, and those that
// run one job at a time (Codex on a ChatGPT plan) with one running.
func (p *Pipeline) heldHarnesses(ctx context.Context) []string {
	out := p.pausedHarnesses(ctx)
	names := make([]string, 0, len(p.Harnesses))
	for n := range p.Harnesses {
		names = append(names, n)
	}
	sort.Strings(names)
	why := map[string]string{}
	var busy map[string]bool // harnesses with a running job, listed once
	for _, n := range names {
		g, ok := p.Harnesses[n].(harness.Gated)
		if !ok {
			continue
		}
		if msg := g.Unavailable(); msg != "" {
			why[n] = msg
			if !slices.Contains(out, n) {
				out = append(out, n)
			}
			continue
		}
		if !g.OneAtATime() || slices.Contains(out, n) {
			continue
		}
		if busy == nil {
			jobs, err := p.Store.ListJobs(ctx, store.JobFilter{State: state.Running})
			if err != nil {
				out = append(out, n) // can't tell: wait rather than share the sign-in
				continue
			}
			busy = map[string]bool{}
			for _, j := range jobs {
				busy[j.Harness] = true
			}
		}
		if busy[n] {
			out = append(out, n)
		}
	}
	// Once per sign-in problem, and once when it's fixed.
	var notes []string
	p.mu.Lock()
	if p.signInHalt == nil {
		p.signInHalt = map[string]string{}
	}
	for _, n := range names {
		switch msg, was := why[n], p.signInHalt[n]; {
		case msg != "" && msg != was:
			p.signInHalt[n] = msg
			notes = append(notes, msg)
		case msg == "" && was != "":
			delete(p.signInHalt, n)
			notes = append(notes, n+" can sign in again; its jobs are starting.")
		}
	}
	p.mu.Unlock()
	for _, msg := range notes {
		p.notify(ctx, msg)
	}
	return out
}

// requeueSignedOut ends a job whose harness couldn't sign in and queues it
// again at the same attempt: the task didn't fail, so it uses no retry and
// parks nothing. The harness holds its jobs until it can sign in.
func (p *Pipeline) requeueSignedOut(ctx context.Context, j *store.Job, run agentRun) bool {
	if run.res == nil || !run.res.SignedOut {
		return false
	}
	ctx = context.WithoutCancel(ctx)
	p.finish(ctx, j, run, state.Failed, "", run.res.Error+" (queued again for when it can sign in)")
	_, err := p.Store.EnqueueJob(ctx, store.Job{ItemID: j.ItemID, Kind: j.Kind, Harness: j.Harness, Model: j.Model, Effort: j.Effort,
		Attempt: j.Attempt, Priority: j.Priority, BudgetUSD: j.BudgetUSD, Branch: j.Branch, Skillset: j.Skillset, CompareID: j.CompareID,
		Input: j.Input, Route: j.Route, Fast: j.Fast})
	p.warn(err, "requeue signed-out job")
	p.heldHarnesses(ctx) // says so now, not on the next pass
	return true
}
