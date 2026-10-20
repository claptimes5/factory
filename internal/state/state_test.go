package state

import "testing"

func TestStageTransitions(t *testing.T) {
	allowed := []struct{ from, to Stage }{
		{Triage, Ready},
		{Triage, Spec},
		{Triage, Parked},
		{Spec, Ready},
		{Ready, Implementing},
		{Implementing, Reviewing},
		{Implementing, Parked},
		{Reviewing, Implementing}, // reviewer blocked: fix round
		{Reviewing, AwaitingHuman},
		{Reviewing, Parked},
		{AwaitingHuman, Done},
		{Parked, Triage},
		{Parked, Ready},
		{Blocked, Ready},
		{Parked, Done}, // its issue was closed
		{Blocked, Done},
	}
	for _, c := range allowed {
		if err := CheckStage(c.from, c.to); err != nil {
			t.Errorf("%s -> %s should be allowed: %v", c.from, c.to, err)
		}
	}

	denied := []struct{ from, to Stage }{
		{Triage, Implementing},        // must be triaged first
		{Ready, AwaitingHuman},        // cannot skip implement and review
		{Implementing, AwaitingHuman}, // cannot skip review
		{Done, Implementing},          // done is terminal
		{Reviewing, Done},             // merge is human; only awaiting_human -> done
		{Triage, Triage},              // self loops are not transitions
		{Stage("bogus"), Ready},
		{Ready, Stage("bogus")},
	}
	for _, c := range denied {
		if err := CheckStage(c.from, c.to); err == nil {
			t.Errorf("%s -> %s should be denied", c.from, c.to)
		}
	}
}

func TestJobTransitions(t *testing.T) {
	allowed := []struct{ from, to JobState }{
		{Queued, Running},
		{Queued, Cancelled},
		{Running, Succeeded},
		{Running, Failed},
		{Running, Cancelled},
		{Running, TimedOut},
		{Running, Queued}, // daemon restart recovery
	}
	for _, c := range allowed {
		if err := CheckJob(c.from, c.to); err != nil {
			t.Errorf("%s -> %s should be allowed: %v", c.from, c.to, err)
		}
	}
	denied := []struct{ from, to JobState }{
		{Queued, Succeeded},
		{Succeeded, Running},
		{Failed, Queued}, // retries are new job rows, not resurrections
		{TimedOut, Running},
		{Cancelled, Queued},
	}
	for _, c := range denied {
		if err := CheckJob(c.from, c.to); err == nil {
			t.Errorf("%s -> %s should be denied", c.from, c.to)
		}
	}
}

func TestTerminal(t *testing.T) {
	for _, s := range []JobState{Succeeded, Failed, Cancelled, TimedOut} {
		if !s.Terminal() {
			t.Errorf("%s should be terminal", s)
		}
	}
	for _, s := range []JobState{Queued, Running} {
		if s.Terminal() {
			t.Errorf("%s should not be terminal", s)
		}
	}
}
