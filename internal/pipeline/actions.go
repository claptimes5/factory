package pipeline

import (
	"context"
	"errors"
	"fmt"

	"github.com/claptimes5/factory/internal/state"
	"github.com/claptimes5/factory/internal/store"
)

// Human actions from the control room. Each is checked like any other
// request: the daemon decides whether it is allowed.

// Retry queues a new run of a finished job. With tierUp, an implement job
// moves to the first attempt of the next model tier.
func (p *Pipeline) Retry(ctx context.Context, jobID int64, tierUp bool) error {
	j, err := p.Store.GetJob(ctx, jobID)
	if err != nil {
		return err
	}
	if !j.State.Terminal() {
		return fmt.Errorf("job %d is still %s", jobID, j.State)
	}
	if j.ItemID == nil {
		return errors.New("only item jobs can be retried")
	}
	it, err := p.Store.GetItem(ctx, *j.ItemID)
	if err != nil {
		return err
	}
	if it.Stage == state.Done || it.Stage == state.AwaitingHuman {
		return fmt.Errorf("item is %s", it.Stage)
	}
	if active, err := p.activeJob(ctx, it.ID); err != nil || active != nil {
		if active != nil {
			return fmt.Errorf("item already has job %d %s", active.ID, active.State)
		}
		return err
	}
	if tierUp && j.Kind != "implement" {
		return errors.New("only implement jobs have model tiers")
	}
	p.warn(p.Store.AddEvent(ctx, store.Event{Actor: "human", ItemID: &it.ID, JobID: &j.ID, Type: "retry",
		Reason: map[bool]string{true: "retry one tier up", false: "retry"}[tierUp]}), "AddEvent")

	switch j.Kind {
	case "triage":
		if err := p.Store.SetStage(ctx, it.ID, state.Triage, "human", "retry triage"); err != nil {
			return err
		}
		return p.enqueueTriage(ctx, it, j.Attempt, "")
	case "implement":
		attempt := j.Attempt
		if tierUp {
			per := p.Cfg.Escalation.MaxAttemptsPerTier
			attempt = ((j.Attempt-1)/per+1)*per + 1
		}
		if _, ok := p.Cfg.Implement(it.Complexity, attempt); !ok {
			return errors.New("already on the top model tier")
		}
		if it.Stage == state.Parked || it.Stage == state.Blocked || it.Stage == state.Triage {
			if err := p.Store.SetStage(ctx, it.ID, state.Ready, "human", "retry implement"); err != nil {
				return err
			}
		}
		if err := p.Store.SetStage(ctx, it.ID, state.Implementing, "human", "retry implement"); err != nil {
			return err
		}
		var in ImplementInput
		if j.Input != "" {
			_ = jsonUnmarshal(j.Input, &in)
		}
		_, err := p.enqueueImplement(ctx, it, attempt, in)
		return err
	case "review", "critic":
		if it.Stage != state.Reviewing {
			return fmt.Errorf("item is %s; retry its implement job instead", it.Stage)
		}
		var in ReviewInput
		if err := jsonUnmarshal(j.Input, &in); err != nil {
			return err
		}
		return p.enqueueReview(ctx, it, j.Kind, j.Attempt, in)
	}
	return fmt.Errorf("cannot retry a %s job", j.Kind)
}

// Cancel stops a queued or running job. A cancelled job does not retry;
// its item parks so a human decides what happens next.
func (p *Pipeline) Cancel(ctx context.Context, jobID int64) error {
	j, err := p.Store.GetJob(ctx, jobID)
	if err != nil {
		return err
	}
	switch j.State {
	case state.Queued:
		if err := p.Store.CancelQueued(ctx, jobID, "human", "cancelled from the control room"); err != nil {
			return err
		}
		if j.ItemID != nil {
			if it, err := p.Store.GetItem(ctx, *j.ItemID); err == nil {
				return p.park(ctx, it, fmt.Sprintf("A human cancelled job %d (%s).", j.ID, j.Kind))
			}
		}
		return nil
	case state.Running:
		p.cancelled.Store(jobID, true)
		return p.Runner.Kill(ctx, containerName(jobID))
	}
	return fmt.Errorf("job %d is already %s", jobID, j.State)
}

// Park moves an item to parked and cancels its active job.
func (p *Pipeline) Park(ctx context.Context, itemID int64, reason string) error {
	it, err := p.Store.GetItem(ctx, itemID)
	if err != nil {
		return err
	}
	if it.Stage == state.Done || it.Stage == state.Parked {
		return fmt.Errorf("item is %s", it.Stage)
	}
	if active, err := p.activeJob(ctx, itemID); err == nil && active != nil {
		switch active.State {
		case state.Queued:
			p.warn(p.Store.CancelQueued(ctx, active.ID, "human", "item parked"), "CancelQueued")
		case state.Running:
			p.cancelled.Store(active.ID, true)
			p.warn(p.Runner.Kill(ctx, containerName(active.ID)), "Kill")
		}
	}
	return p.park(ctx, it, "Parked by a human: "+reason)
}

func (p *Pipeline) activeJob(ctx context.Context, itemID int64) (*store.Job, error) {
	jobs, err := p.Store.ListJobs(ctx, store.JobFilter{ItemID: itemID})
	if err != nil {
		return nil, err
	}
	for _, j := range jobs {
		if !j.State.Terminal() {
			return j, nil
		}
	}
	return nil, nil
}

// stopIfCancelled ends a job a human cancelled mid-run. It returns true
// when the caller must not retry.
func (p *Pipeline) stopIfCancelled(ctx context.Context, j *store.Job, it *store.WorkItem, run agentRun) bool {
	if _, ok := p.cancelled.LoadAndDelete(j.ID); !ok {
		return false
	}
	p.finish(ctx, j, run, state.Cancelled, "", "cancelled by a human")
	if it != nil && it.Stage != state.Parked {
		if cur, err := p.Store.GetItem(ctx, it.ID); err == nil && cur.Stage != state.Parked {
			p.warn(p.park(ctx, cur, fmt.Sprintf("A human cancelled job %d (%s).", j.ID, j.Kind)), "park")
		}
	}
	return true
}
