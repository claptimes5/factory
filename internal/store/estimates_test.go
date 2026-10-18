package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/claptimes5/factory/internal/state"
)

func TestEstimates(t *testing.T) {
	ctx := context.Background()
	s, _ := openTest(t)
	it, _, _ := s.UpsertItem(ctx, "o/r", 1, "t")
	if e, err := s.ItemEstimate(ctx, it.ID); e != nil || err != nil {
		t.Fatalf("before any: %+v %v", e, err)
	}
	s.SetEstimate(ctx, Estimate{ItemID: it.ID, Size: "M", MedianUSD: 2, P75USD: 6, Basis: "repo", Detail: "d", Status: EstimateWaiting})
	if ok, _ := s.ApproveEstimate(ctx, it.ID, "maintainer"); !ok {
		t.Fatal("a waiting estimate wasn't approved")
	}
	if ok, _ := s.ApproveEstimate(ctx, it.ID, "someone"); ok {
		t.Fatal("approved twice")
	}
	e, _ := s.ItemEstimate(ctx, it.ID)
	if e.Status != EstimateApproved || e.ApprovedBy != "maintainer" || e.ApprovedUSD != 6 || e.DecidedAt == "" || e.CreatedAt == "" {
		t.Fatalf("estimate %+v", e)
	}
	// A re-triage replaces it.
	s.SetEstimate(ctx, Estimate{ItemID: it.ID, Size: "S", Basis: "default", Detail: "d", Status: EstimateUnder})
	if es, _ := s.Estimates(ctx); len(es) != 1 || es[0].Size != "S" || es[0].ApprovedBy != "" {
		t.Fatalf("estimates %+v", es)
	}
}

func TestItemCosts(t *testing.T) {
	ctx := context.Background()
	s, _ := openTest(t)
	harness := "claude"
	job := func(item int64, kind string, cost float64, end state.JobState) {
		id, _ := s.EnqueueJob(ctx, Job{ItemID: &item, Kind: kind, Harness: harness, Model: "m", BudgetUSD: 9})
		if end == state.Queued {
			return
		}
		s.ClaimJob(ctx)
		s.FinishJob(ctx, id, Result{State: end, CostUSD: cost})
	}
	item := func(n int, size string, stages ...state.Stage) int64 {
		it, _, _ := s.UpsertItem(ctx, "o/r", n, "t")
		s.UpdateItem(ctx, it.ID, ItemUpdate{Complexity: &size})
		for _, st := range stages {
			if err := s.SetStage(ctx, it.ID, st, "daemon", ""); err != nil {
				t.Fatal(err)
			}
		}
		return it.ID
	}
	done := item(1, "M", state.Ready, state.Implementing, state.Reviewing, state.AwaitingHuman, state.Done)
	job(done, "triage", 0.1, state.Succeeded)
	job(done, "implement", 1, state.Failed)
	job(done, "implement", 2, state.Succeeded)
	job(done, "review", 0.5, state.Succeeded)
	job(done, "critic", 0.25, state.Succeeded)
	parked := item(2, "S", state.Ready, state.Implementing, state.Parked)
	job(parked, "implement", 3, state.Failed)
	triagedOnly := item(3, "S", state.Parked)
	job(triagedOnly, "triage", 0.1, state.Succeeded)
	// Re-triaged after a first cycle: only the latest cycle counts.
	again := item(5, "L", state.Ready, state.Implementing, state.Parked)
	job(again, "triage", 0.1, state.Succeeded)
	job(again, "implement", 10, state.Failed)
	job(again, "review", 4, state.Succeeded)
	job(again, "triage", 0.1, state.Succeeded)
	job(again, "implement", 2, state.Succeeded)
	harness = "codex"
	job(again, "review", 1, state.Succeeded)
	harness = "claude"
	// Queued last: a claim takes the oldest queued job.
	building := item(4, "S", state.Ready, state.Implementing)
	job(building, "implement", 1, state.Queued)

	cs, err := s.ItemCosts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 3 {
		t.Fatalf("costs %+v", cs)
	}
	c, err := s.ItemCostOf(ctx, again)
	if err != nil || c == nil || c.Implement != 2 || c.Review != 1 || c.Total() != 3 || c.Size != "L" {
		t.Fatalf("again %+v %v", c, err)
	}
	if cs[2].ItemID != again || cs[2].Total() != 3 {
		t.Fatalf("again in the list %+v", cs[2])
	}
	// Each part keeps its job's harness, for billing.
	parts := map[string]float64{}
	for _, p := range c.Parts {
		parts[p.Kind+"/"+p.Harness] += p.USD
	}
	if len(parts) != 2 || parts["implement/claude"] != 2 || parts["review/codex"] != 1 {
		t.Fatalf("parts %+v", c.Parts)
	}
	if c, err := s.ItemCostOf(ctx, building); err != nil || c != nil {
		t.Fatalf("building %+v %v", c, err)
	}
	if c := cs[0]; c.ItemID != done || c.Implement != 3 || c.Review != 0.5 || c.Critic != 0.25 || c.Total() != 3.75 || c.Size != "M" {
		t.Fatalf("done %+v", c)
	}
	if c := cs[1]; c.ItemID != parked || c.Total() != 3 || c.Stage != "parked" {
		t.Fatalf("parked %+v", c)
	}
}

// Upgrading marks the health fixes already in flight, so their cost
// isn't held once the health issue is gone.
func TestEstimatesMigrationMarksHealthFixes(t *testing.T) {
	ctx := context.Background()
	s, path := openTest(t)
	fix, _, _ := s.UpsertItem(ctx, "o/r", 7, "Main is red: test")
	other, _, _ := s.UpsertItem(ctx, "o/r", 8, "t")
	s.SetHealthIssue(ctx, HealthIssue{Repo: "o/r", Check: "test", Kind: HealthRed, HeadSHA: "abc", IssueNumber: 7})
	s.SetHealthIssue(ctx, HealthIssue{Repo: "o/r", Check: "lint", Kind: HealthRed, HeadSHA: "abc"}) // not filed yet
	// Back to before 0013, undoing the later migrations too.
	for _, q := range []string{
		`DROP TABLE groom_proposals`, `DROP TABLE groom_children`, `DROP TABLE groom_runs`, // 0014
		`DROP TABLE estimates`, `ALTER TABLE work_items DROP COLUMN main_health`,
		`DELETE FROM schema_migrations WHERE version >= 13`,
	} {
		if _, err := s.write.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()
	// The first open's backups may share this second's name.
	baks, _ := filepath.Glob(path + ".bak-*")
	for _, b := range baks {
		os.Remove(b)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if ok, _ := s.IsHealthItem(ctx, fix.ID); !ok {
		t.Fatal("the health fix in flight wasn't marked")
	}
	if ok, _ := s.IsHealthItem(ctx, other.ID); ok {
		t.Fatal("marked an item that isn't a health fix")
	}
}
