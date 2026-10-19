package store

import (
	"context"

	"github.com/claptimes5/factory/internal/state"
)

// Plan events, each on the plan job it's about. Nothing else about a plan
// is stored: the plan is the plan job's verdict, and where it stands is
// in these events.
const (
	EvPlanWaiting   = "plan_waiting"
	EvPlanAnnounced = "plan_announced"
	EvPlanApproved  = "plan_approved"
	EvPlanReplan    = "plan_replan"
)

// PlanStatus is where the plan of an item's current triage cycle stands.
type PlanStatus struct {
	Job       *Job
	Waiting   bool // for a human's OK
	Announced bool // the comment asking for it went out
	Approved  bool
	By        string // how it was approved: "by @x", "by the critic"
}

// CurrentPlan is the item's plan job since its last triage and where it
// stands, or nil when it has none this cycle.
func (s *Store) CurrentPlan(ctx context.Context, itemID int64) (*PlanStatus, error) {
	js, err := s.ListJobs(ctx, JobFilter{ItemID: itemID, Kind: "plan", Limit: 1})
	if err != nil || len(js) == 0 {
		return nil, err
	}
	tr, err := s.ListJobs(ctx, JobFilter{ItemID: itemID, Kind: "triage", Limit: 1})
	if err != nil {
		return nil, err
	}
	if len(tr) > 0 && tr[0].ID > js[0].ID {
		return nil, nil // an earlier cycle's: a factory:retry plans again
	}
	p := &PlanStatus{Job: js[0]}
	evs, err := s.Events(ctx, itemID)
	if err != nil {
		return nil, err
	}
	replanned := false
	for _, e := range evs {
		if e.JobID == nil || *e.JobID != p.Job.ID {
			continue
		}
		switch e.Type {
		case EvPlanWaiting:
			p.Waiting = true
		case EvPlanAnnounced:
			p.Announced = true
		case EvPlanApproved:
			p.Approved, p.By = true, e.Reason
		case EvPlanReplan:
			replanned = true
		}
	}
	p.Waiting = p.Waiting && !p.Approved && !replanned
	return p, nil
}

// PlanWaiting says whether a blocked item waits for a human's OK on its
// plan.
func (s *Store) PlanWaiting(ctx context.Context, it *WorkItem) bool {
	if it.Stage != state.Blocked {
		return false
	}
	p, err := s.CurrentPlan(ctx, it.ID)
	return err == nil && p != nil && p.Waiting
}
