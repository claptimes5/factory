package pipeline

import (
	"context"
	"os"
	"slices"
	"sort"
	"strings"

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

// signedOutNote marks a job ended because its harness couldn't sign in.
const signedOutNote = " (queued again for when it can sign in)"

// maxSignedOut is how many times in a row a job is queued again for a
// sign-in failure. The job can fake one, so after that it fails as usual.
const maxSignedOut = 2

// requeueSignedOut ends a job whose harness couldn't sign in and queues it
// again at the same attempt: the task didn't fail, so it uses no retry and
// parks nothing. The harness holds its jobs until it can sign in.
func (p *Pipeline) requeueSignedOut(ctx context.Context, j *store.Job, run agentRun) bool {
	if run.res == nil || !run.res.SignedOut || j.ItemID == nil {
		return false
	}
	ctx = context.WithoutCancel(ctx)
	prev, err := p.Store.ListJobs(ctx, store.JobFilter{ItemID: *j.ItemID, Kind: j.Kind})
	if err != nil {
		p.warn(err, "ListJobs")
		return false
	}
	n := 0
	for _, o := range prev {
		if o.ID != j.ID && o.Attempt == j.Attempt && o.State == state.Cancelled && strings.HasSuffix(o.Error, signedOutNote) {
			n++
		}
	}
	if n >= maxSignedOut {
		return false
	}
	// Queued first: if that fails, the job fails as usual instead of
	// leaving its item with no job.
	if _, err := p.Store.EnqueueJob(ctx, store.Job{ItemID: j.ItemID, Kind: j.Kind, Harness: j.Harness, Model: j.Model, Effort: j.Effort,
		Attempt: j.Attempt, Priority: j.Priority, BudgetUSD: j.BudgetUSD, Branch: j.Branch, Skillset: j.Skillset, CompareID: j.CompareID,
		Input: j.Input, Route: j.Route, Fast: j.Fast}); err != nil {
		p.warn(err, "requeue signed-out job")
		return false
	}
	// Cancelled, not failed: it says nothing about the task, so it doesn't
	// count toward "failed twice in a row".
	p.finish(ctx, j, run, state.Cancelled, "", run.res.Error+signedOutNote)
	p.heldHarnesses(ctx) // says so now, not on the next pass
	return true
}

// recoverSignIns lets each harness clean up after runs a crash cut short
// (Codex's copies of its ChatGPT sign-in).
func (p *Pipeline) recoverSignIns() {
	for _, h := range p.Harnesses {
		if r, ok := h.(interface{ Recover() error }); ok {
			p.warn(r.Recover(), "recover "+h.Name())
		}
	}
}

// scrubLog takes the secrets a run held besides its env (a ChatGPT
// sign-in's tokens, refreshed ones included) out of its log.
func (p *Pipeline) scrubLog(path string, res *harness.Result) {
	if res == nil || len(res.Redact) == 0 {
		return
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	out := string(b)
	for _, v := range res.Redact {
		if len(v) >= 8 {
			out = strings.ReplaceAll(out, v, "[REDACTED]")
		}
	}
	if out != string(b) {
		p.warn(os.WriteFile(path, []byte(out), 0o644), "scrub log")
	}
}
