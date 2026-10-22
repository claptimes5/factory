package pipeline

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/claptimes5/factory/internal/config"
	"github.com/claptimes5/factory/internal/state"
	"github.com/claptimes5/factory/internal/store"
)

// Human actions from the control room. Each is checked like any other
// request: the daemon decides whether it is allowed.

// Retry queues a new run of a finished job. With tierUp, an implement job
// moves to the first attempt of the next model tier.
func (p *Pipeline) Retry(ctx context.Context, jobID int64, tierUp bool) error {
	p.act.Lock()
	defer p.act.Unlock()
	return p.retry(ctx, "human", jobID, tierUp)
}

// Cancel stops a queued or running job. A cancelled job does not retry;
// its item parks so a human decides what happens next.
func (p *Pipeline) Cancel(ctx context.Context, jobID int64) error {
	p.act.Lock()
	defer p.act.Unlock()
	return p.cancel(ctx, "human", jobID)
}

// Park moves an item to parked and cancels its active job.
func (p *Pipeline) Park(ctx context.Context, itemID int64, reason string) error {
	p.act.Lock()
	defer p.act.Unlock()
	return p.parkItem(ctx, "human", itemID, reason)
}

// The lower-case actions assume p.act is held.

// retryPlan is what a retry of job jobID would run, once every check
// that could refuse it passed. It changes nothing, so the dashboard can
// ask it what Retry and Tier up would do.
type retryPlan struct {
	j     *store.Job
	it    *store.WorkItem
	tier  bool
	in    ImplementInput // an implement job's input
	size  string         // its route's size
	route config.Route   // the route it would run on
	next  int            // the attempt it would run as
}

func (p *Pipeline) planRetry(ctx context.Context, actor string, jobID int64, tierUp bool) (*retryPlan, error) {
	j, err := p.Store.GetJob(ctx, jobID)
	if err != nil {
		return nil, err
	}
	if !j.State.Terminal() {
		return nil, fmt.Errorf("job %d is still %s", jobID, j.State)
	}
	if j.ItemID == nil {
		return nil, errors.New("only item jobs can be retried")
	}
	it, err := p.Store.GetItem(ctx, *j.ItemID)
	if err != nil {
		return nil, err
	}
	if it.Stage == state.Done || it.Stage == state.AwaitingHuman {
		return nil, fmt.Errorf("item is %s", it.Stage)
	}
	if active, err := p.activeJob(ctx, it.ID); err != nil || active != nil {
		if active != nil {
			return nil, fmt.Errorf("item already has job %d %s", active.ID, active.State)
		}
		return nil, err
	}
	// Whatever the stage: one parked while it waited still needs the OK.
	if j.Kind == "implement" && p.estimateNeedsOK(ctx, it.ID) {
		return nil, fmt.Errorf("item %d needs an OK on its cost estimate before it's built; approve that first, or send it back through triage with %s", it.ID, LabelRetry)
	}
	if tierUp && j.Kind != "implement" {
		return nil, errors.New("only implement jobs have model tiers")
	}
	rp := &retryPlan{j: j, it: it, tier: tierUp, next: j.Attempt}
	switch j.Kind {
	case "triage":
		rp.route = p.Cfg.Routing["triage"]
	case "implement":
		if tierUp {
			per := p.Cfg.Escalation.MaxAttemptsPerTier
			rp.next = ((j.Attempt-1)/per+1)*per + 1
		}
		if j.Input != "" {
			_ = jsonUnmarshal(j.Input, &rp.in)
		}
		rp.in.Continue = nil // a retry starts over
		// It hears why this job failed, not the one before it.
		if j.Error != "" && j.State != state.Cancelled {
			rp.in.PreviousFailure = p.priorFailure(j.ID, j.Error)
		}
		rp.size = it.Complexity
		if rp.in.Size != "" {
			rp.size = rp.in.Size // a rethink's build, or a fix round after one
		}
		r, ok := p.Cfg.Implement(rp.size, rp.next)
		if !ok {
			if tierUp {
				return nil, errors.New("already on the top model tier")
			}
			return nil, fmt.Errorf("no implement route for size %q at attempt %d", rp.size, rp.next)
		}
		rp.route = r
	case "review", "critic":
		// Parked: the review couldn't finish (its harness failed, say), and the build it read is still there.
		if it.Stage != state.Reviewing && it.Stage != state.Parked {
			return nil, fmt.Errorf("item is %s; retry its implement job instead", it.Stage)
		}
		// Today's route, not the failed job's: a reviewer changed in factory.yaml since takes over.
		rp.route = p.Cfg.Review(it.Risk)
		if j.Kind == "critic" {
			rp.route = p.Cfg.CriticRoute()
		}
	default:
		return nil, fmt.Errorf("cannot retry a %s job", j.Kind)
	}
	return rp, nil
}

func (p *Pipeline) retry(ctx context.Context, actor string, jobID int64, tierUp bool) error {
	rp, err := p.planRetry(ctx, actor, jobID, tierUp)
	if err != nil {
		return err
	}
	j, it := rp.j, rp.it
	if j.Kind == "implement" {
		if held, err := p.holdForPlan(ctx, it, actor); err != nil {
			return err
		} else if held {
			return fmt.Errorf("item %d needs its plan approved before it's built; it's on the issue (planning it first if it had none)", it.ID)
		}
	}
	p.warn(p.Store.AddEvent(ctx, store.Event{Actor: actor, ItemID: &it.ID, JobID: &j.ID, Type: "retry",
		Reason: map[bool]string{true: "retry one tier up", false: "retry"}[tierUp]}), "AddEvent")

	switch j.Kind {
	case "triage":
		if err := p.Store.SetStage(ctx, it.ID, state.Triage, actor, "retry triage"); err != nil {
			return err
		}
		return p.enqueueTriage(ctx, it, j.Attempt, "")
	case "implement":
		if err := p.toImplementing(ctx, it, actor, "retry implement"); err != nil {
			return err
		}
		ok, err := p.enqueueImplement(ctx, it, rp.next, rp.in)
		if err == nil && !ok {
			return fmt.Errorf("no implement route for size %q at attempt %d", rp.size, rp.next)
		}
		return err
	default: // review, critic
		var in ReviewInput
		if err := jsonUnmarshal(j.Input, &in); err != nil {
			return err
		}
		if it.Stage == state.Parked {
			if err := p.Store.SetStage(ctx, it.ID, state.Reviewing, actor, "retry "+j.Kind); err != nil {
				return err
			}
		}
		return p.enqueueReview(ctx, it, j.Kind, j.Attempt, in)
	}
}

// toImplementing moves an item back to implementing for a build a person
// asked for, by way of ready when it waits.
func (p *Pipeline) toImplementing(ctx context.Context, it *store.WorkItem, actor, why string) error {
	if it.Stage == state.Parked || it.Stage == state.Blocked || it.Stage == state.Triage {
		if err := p.Store.SetStage(ctx, it.ID, state.Ready, actor, why); err != nil {
			return err
		}
	}
	return p.Store.SetStage(ctx, it.ID, state.Implementing, actor, why)
}

func (p *Pipeline) cancel(ctx context.Context, actor string, jobID int64) error {
	j, err := p.Store.GetJob(ctx, jobID)
	if err != nil {
		return err
	}
	if j.State.Terminal() {
		return fmt.Errorf("job %d is already %s", jobID, j.State)
	}
	if store.IsSubKind(j.Kind) {
		// Cancelling the verifier alone would only make its findings block.
		var in struct{ Within int64 }
		if err := jsonUnmarshal(j.Input, &in); err != nil || in.Within == 0 {
			return fmt.Errorf("job %d runs inside another job, which it doesn't name", jobID)
		}
		return p.cancel(ctx, actor, in.Within)
	}
	if err := p.stopJob(ctx, actor, j, "cancelled from the control room"); err != nil {
		return err
	}
	if j.ItemID != nil {
		if it, err := p.Store.GetItem(ctx, *j.ItemID); err == nil && it.Stage != state.Parked {
			why := fmt.Sprintf("Job %d (%s) was cancelled from the dashboard.", j.ID, j.Kind)
			if actor != "human" {
				why = fmt.Sprintf("%s cancelled job %d (%s).", capital(who(actor)), j.ID, j.Kind)
			}
			return p.park(ctx, it, why+" Nothing runs until you choose what's next: retry it, or leave it parked.")
		}
	}
	return nil
}

// stopJob cancels a queued job, or stops a running one: its context is
// cancelled (ending whatever phase it is in) and its container killed. A job
// claimed between the read and the cancel is treated as running.
func (p *Pipeline) stopJob(ctx context.Context, actor string, j *store.Job, reason string) error {
	if j.State == state.Queued {
		if err := p.Store.CancelQueued(ctx, j.ID, actor, reason); err == nil {
			return nil
		}
	}
	p.cancelled.Store(j.ID, true)
	if c, ok := p.jobCancel.Load(j.ID); ok {
		c.(context.CancelFunc)()
	}
	// And the verifier or paired critic it is running, if any.
	if j.ItemID != nil {
		if subs, err := p.Store.ListJobs(ctx, store.JobFilter{ItemID: *j.ItemID, State: state.Running}); err == nil {
			for _, s := range subs {
				if store.IsSubKind(s.Kind) {
					p.warn(p.Runner.Kill(ctx, p.containerName(s.ID)), "kill sub-job")
				}
			}
		}
	}
	return p.Runner.Kill(ctx, p.containerName(j.ID))
}

func (p *Pipeline) parkItem(ctx context.Context, actor string, itemID int64, reason string) error {
	it, err := p.Store.GetItem(ctx, itemID)
	if err != nil {
		return err
	}
	if it.Stage == state.Done || it.Stage == state.Parked {
		return fmt.Errorf("item is %s", it.Stage)
	}
	if active, err := p.activeJob(ctx, itemID); err == nil && active != nil {
		p.warn(p.stopJob(ctx, actor, active, "item parked"), "stop job")
	}
	// A job that already finished may still be running its follow-up steps
	// (queueing the next stage, opening the PR); stop those too.
	if id, ok := p.inflight.Load(itemID); ok {
		p.cancelled.Store(id.(int64), true)
		if c, ok := p.jobCancel.Load(id.(int64)); ok {
			c.(context.CancelFunc)()
		}
	}
	return p.park(ctx, it, fmt.Sprintf("Parked by %s: %s", who(actor), reason))
}

func capital(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func who(actor string) string {
	if actor == "human" {
		return "a human"
	}
	return "the " + actor
}

func (p *Pipeline) activeJob(ctx context.Context, itemID int64) (*store.Job, error) {
	jobs, err := p.Store.ListJobs(ctx, store.JobFilter{ItemID: itemID})
	if err != nil {
		return nil, err
	}
	for _, j := range jobs {
		// A verifier or paired critic runs inside a review: that is the job.
		if !j.State.Terminal() && !store.IsSubKind(j.Kind) {
			return j, nil
		}
	}
	return nil, nil
}

// stopIfCancelled ends a job that was cancelled mid-run, or one whose
// harness couldn't sign in (see requeueSignedOut). It returns true
// when the caller must not retry. The job's own context may already be
// cancelled, so the bookkeeping runs without it.
func (p *Pipeline) stopIfCancelled(ctx context.Context, j *store.Job, it *store.WorkItem, run agentRun) bool {
	if _, ok := p.cancelled.LoadAndDelete(j.ID); !ok {
		return p.requeueSignedOut(ctx, j, run)
	}
	ctx = context.WithoutCancel(ctx)
	if cur, err := p.Store.GetJob(ctx, j.ID); err == nil && !cur.State.Terminal() {
		p.finish(ctx, j, run, state.Cancelled, "", "cancelled")
	}
	if j.ItemID != nil {
		if cur, err := p.Store.GetItem(ctx, *j.ItemID); err == nil && cur.Stage != state.Parked && cur.Stage != state.Done {
			p.warn(p.park(ctx, cur, fmt.Sprintf("Job %d (%s) was cancelled.", j.ID, j.Kind)), "park")
		}
	}
	return true
}

// StartImplement queues the first implement job for a ready item that
// already has a complexity, skipping triage (factory compare uses it so
// every arm builds at the same tier).
func (p *Pipeline) StartImplement(ctx context.Context, it *store.WorkItem) error {
	ok, err := p.enqueueImplement(ctx, it, 1, ImplementInput{})
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("no implement route for complexity %q", it.Complexity)
	}
	return p.Store.SetStage(ctx, it.ID, state.Implementing, "daemon", "implement queued")
}
