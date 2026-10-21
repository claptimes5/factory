package pipeline

import (
	"context"
	"strings"
	"testing"

	"github.com/claptimes5/factory/internal/config"
	"github.com/claptimes5/factory/internal/github"
	"github.com/claptimes5/factory/internal/state"
	"github.com/claptimes5/factory/internal/store"
)

func TestSelfSource(t *testing.T) {
	t.Parallel()
	for by, want := range map[string]string{
		HealthApprover:                       "main_health",
		OrchestratorApprover:                 "orchestrator",
		SelfApprover(SourceDependabot):       "dependabot",
		SelfApprover(SourceHunt):             "hunt",
		"the factory's chat":                 "", // a person asked for it
		"a person on the dashboard":          "",
		"octocat":                            "",
		"(approved before the factory knew)": "",
	} {
		if got := SelfSource(by); got != want {
			t.Errorf("%q: %q, want %q", by, got, want)
		}
	}
}

// At trust level 6 a Dependabot finding starts on its own, within the
// day's cap; past it, and below level 6, it waits for a person.
func TestDependabotStartsOnItsOwnAtLevelSix(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e, gh, _, msgs := srcSetup(t, nil)
	e.p.Cfg.Repos[0].SelfStart = &config.SelfStart{Sources: []string{"dependabot"}, Daily: 1}
	gh.alerts[1] = &github.DependabotAlert{Number: 1, State: "open", Severity: "critical", Package: "requests", Manifest: "requirements.txt"}
	gh.alerts[2] = &github.DependabotAlert{Number: 2, State: "open", Severity: "high", Package: "lodash", Manifest: "package-lock.json"}
	e.p.PollSource(ctx, SourceDependabot)
	filed := labeled(e, LabelFromDependabot)
	if len(filed) != 2 {
		t.Fatalf("filed %d", len(filed))
	}
	started := 0
	for _, is := range filed {
		it, err := e.p.Store.ItemByIssue(ctx, "o/r", is.Number)
		if err != nil {
			continue
		}
		started++
		if a, _ := e.p.Store.ItemApproval(ctx, it.ID); a == nil || a.ApprovedBy != "the factory (dependabot)" {
			t.Fatalf("approval %+v", a)
		}
		if e.p.mergesItself(ctx, it) != true {
			t.Fatal("at level 6 it may merge itself")
		}
	}
	if started != 1 || count(*msgs, "and started it, as trust level 6") != 1 || count(*msgs, "It waits for you, as 1 started on their own today") != 1 {
		t.Fatalf("started %d; notifications %q", started, *msgs)
	}

	// Below level 6, work the factory started waits for a person to merge.
	e.p.Cfg.Repos[0].SelfStart = nil
	it, _, _ := e.p.Store.UpsertItem(ctx, "o/r", 90, "Fix the flaky test")
	e.p.Store.SetApproval(ctx, it.ID, store.Approval{IssueHash: "h", ApprovedBy: HealthApprover})
	if e.p.mergesItself(ctx, it) {
		t.Fatal("a main health fix merged itself below level 6")
	}
	e.p.Cfg.Repos[0].SelfStart = &config.SelfStart{Sources: []string{"main_health"}}
	if !e.p.mergesItself(ctx, it) {
		t.Fatal("at level 6 for main health, its fix may merge itself")
	}
	person, _, _ := e.p.Store.UpsertItem(ctx, "o/r", 91, "Add a button")
	e.p.Store.SetApproval(ctx, person.ID, store.Approval{IssueHash: "h", ApprovedBy: "octocat"})
	e.p.Cfg.Repos[0].SelfStart = nil
	if !e.p.mergesItself(ctx, person) {
		t.Fatal("work a person approved follows auto_merge alone")
	}
}

// Work the factory started on its own stops starting once it has spent
// its share of the day's budget.
func TestSelfStartBudget(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	e.p.Cfg.Budgets.DailyTotal = 10
	e.p.Cfg.Repos[0].SelfStart = &config.SelfStart{Sources: []string{"dependabot"}}
	if why, err := e.p.selfStartRoom(ctx, "o/r"); why != "" || err != nil {
		t.Fatalf("room before any spend: %q %v", why, err)
	}
	it, _, _ := e.p.Store.UpsertItem(ctx, "o/r", 5, "Bump requests")
	e.p.Store.SetApproval(ctx, it.ID, store.Approval{IssueHash: "h", ApprovedBy: SelfApprover(SourceDependabot)})
	id, _ := e.p.Store.EnqueueJob(ctx, store.Job{ItemID: &it.ID, Kind: "implement", Harness: "claude", Model: "sonnet", BudgetUSD: 5})
	e.p.Store.ClaimJob(ctx)
	e.p.Store.FinishJob(ctx, id, store.Result{State: state.Succeeded, CostUSD: 2.5})
	why, err := e.p.selfStartRoom(ctx, "o/r")
	if err != nil || !strings.Contains(why, "$2.50 of its $2.00 a day") {
		t.Fatalf("room after a fifth of the budget: %q %v", why, err)
	}
	// A tripped breaker stops it too, whatever the budget.
	e.p.Cfg.Budgets.DailyTotal = 100
	e.p.Store.TripBreaker(ctx, "o/r", "2 reverts", store.Now())
	if why, _ := e.p.selfStartRoom(ctx, "o/r"); !strings.Contains(why, "auto-merge is off") {
		t.Fatalf("room with the breaker tripped: %q", why)
	}
}
