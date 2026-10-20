// Package state defines the work-item stages and job states and the only
// transitions the daemon may make between them. Agents never change state;
// they report verdicts, and the daemon applies a transition checked here.
package state

import "fmt"

// Stage is a work item's position in the pipeline.
type Stage string

const (
	Triage        Stage = "triage"
	Spec          Stage = "spec"
	Ready         Stage = "ready"
	Implementing  Stage = "implementing"
	Reviewing     Stage = "reviewing"
	AwaitingHuman Stage = "awaiting_human"
	Done          Stage = "done"
	Parked        Stage = "parked"
	Blocked       Stage = "blocked"
	Epic          Stage = "epic" // split by the orchestrator, never built itself
)

// An item whose issue was closed is done from any stage where nothing
// runs; one being built is cancelled, which parks it, first.
var stageEdges = map[Stage][]Stage{
	Triage:        {Spec, Ready, Parked, Blocked, Epic, Done},
	Spec:          {Ready, Parked, Blocked, Done},
	Ready:         {Implementing, Parked, Blocked, Done},
	Implementing:  {Reviewing, Parked, Blocked},
	Reviewing:     {Implementing, AwaitingHuman, Parked},
	AwaitingHuman: {Done, Implementing, Parked},
	Parked:        {Triage, Ready, Epic, Done},
	Blocked:       {Ready, Parked, Done},
	Epic:          {Done, Parked},
	Done:          {},
}

// CheckStage reports whether a work item may move from one stage to another.
func CheckStage(from, to Stage) error {
	edges, ok := stageEdges[from]
	if !ok {
		return fmt.Errorf("unknown stage %q", from)
	}
	if _, ok := stageEdges[to]; !ok {
		return fmt.Errorf("unknown stage %q", to)
	}
	for _, e := range edges {
		if e == to {
			return nil
		}
	}
	return fmt.Errorf("stage transition %s -> %s not allowed", from, to)
}

// JobState is one agent run's lifecycle state.
type JobState string

const (
	Queued    JobState = "queued"
	Running   JobState = "running"
	Succeeded JobState = "succeeded"
	Failed    JobState = "failed"
	Cancelled JobState = "cancelled"
	TimedOut  JobState = "timed_out"
)

var jobEdges = map[JobState][]JobState{
	Queued:  {Running, Cancelled},
	Running: {Succeeded, Failed, Cancelled, TimedOut, Queued},
}

// Terminal reports whether no further transition is possible.
func (s JobState) Terminal() bool {
	switch s {
	case Succeeded, Failed, Cancelled, TimedOut:
		return true
	}
	return false
}

// CheckJob reports whether a job may move from one state to another.
// A retry is a new job row, so terminal states have no outgoing edges.
func CheckJob(from, to JobState) error {
	for _, e := range jobEdges[from] {
		if e == to {
			return nil
		}
	}
	return fmt.Errorf("job transition %s -> %s not allowed", from, to)
}
