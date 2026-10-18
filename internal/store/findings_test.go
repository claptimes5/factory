package store

import (
	"context"
	"testing"

	"github.com/claptimes5/factory/internal/state"
)

func TestFindingsKeepTheirIDsAndOutcomes(t *testing.T) {
	ctx := context.Background()
	s, _ := openTest(t)
	it := newItem(t, s, 1)
	review, _ := s.EnqueueJob(ctx, Job{ItemID: &it.ID, Kind: "review", Harness: "codex", Model: "m", BudgetUSD: 1})
	seq, err := s.NextFindingSeq(ctx, it.ID, review)
	if err != nil || seq != 1 {
		t.Fatalf("first seq %d %v", seq, err)
	}
	raised := []ReviewFinding{
		{ItemID: it.ID, FID: FindingID(seq), Seq: seq, Kind: "review", Route: "review", Harness: "codex", Model: "m", Round: 1, Severity: "major", Claim: "a", Blocking: true, Check: "confirmed", Confirmed: true},
		{ItemID: it.ID, FID: FindingID(seq + 1), Seq: seq + 1, Kind: "review", Round: 1, Severity: "minor", Claim: "b", Outcome: OutcomeNote},
	}
	if err := s.SaveReview(ctx, review, raised, nil); err != nil {
		t.Fatal(err)
	}
	// The same job saving again replaces its own findings, and reuses their ids.
	if seq, _ := s.NextFindingSeq(ctx, it.ID, review); seq != 1 {
		t.Fatalf("a rerun's seq %d", seq)
	}
	if err := s.SaveReview(ctx, review, raised, nil); err != nil {
		t.Fatal(err)
	}
	fs, err := s.ItemFindings(ctx, it.ID, 0)
	if err != nil || len(fs) != 2 || fs[0].FID != "F1" || fs[0].Outcome != OutcomeOpen || !fs[0].Confirmed || fs[1].Outcome != OutcomeNote {
		t.Fatalf("%v %+v", err, fs)
	}
	// Another job's findings go on from there.
	next, _ := s.EnqueueJob(ctx, Job{ItemID: &it.ID, Kind: "review", Harness: "codex", Model: "m", BudgetUSD: 1})
	if seq, _ := s.NextFindingSeq(ctx, it.ID, next); seq != 3 {
		t.Fatalf("next seq %d", seq)
	}
	f := fs[0]
	f.Answer, f.AnswerWhy, f.AnswerJob, f.Outcome = "fixed", "checked", 9, OutcomeFixed
	if err := s.UpdateFindings(ctx, []ReviewFinding{f}); err != nil {
		t.Fatal(err)
	}
	f.Ruling, f.RulingJob = "resolved", next
	if err := s.SaveReview(ctx, next, nil, []ReviewFinding{f}); err != nil {
		t.Fatal(err)
	}
	all, _ := s.AllFindings(ctx)
	if len(all) != 2 || all[0].Answer != "fixed" || all[0].Ruling != "resolved" || all[0].Outcome != OutcomeFixed || all[0].JobID != review {
		t.Fatalf("%+v", all)
	}
	// Findings before a fresh triage are left behind.
	if fs, _ := s.ItemFindings(ctx, it.ID, next); len(fs) != 0 {
		t.Fatalf("since %d: %+v", next, fs)
	}
}

// A sub-job runs from the start, and a restart cancels it rather than
// queue it: the job it ran inside runs again.
func TestSubJobsNeverQueue(t *testing.T) {
	ctx := context.Background()
	s, _ := openTest(t)
	it := newItem(t, s, 1)
	parent, _ := s.EnqueueJob(ctx, Job{ItemID: &it.ID, Kind: "review", Harness: "codex", Model: "m", BudgetUSD: 1})
	if j, _ := s.ClaimJob(ctx); j == nil || j.ID != parent {
		t.Fatal("claim")
	}
	sub, err := s.StartSubJob(ctx, parent, Job{ItemID: &it.ID, Kind: KindVerify, Harness: "claude", Model: "haiku", BudgetUSD: 0.5, Route: "verify"})
	if err != nil || sub.State != state.Running || sub.Input != `{"within":1}` || sub.Route != "verify" {
		t.Fatalf("%v %+v", err, sub)
	}
	if j, _ := s.ClaimJob(ctx); j != nil {
		t.Fatalf("claimed %+v", j)
	}
	n, err := s.RequeueRunning(ctx, func(*Job) bool { return false })
	if err != nil || n != 1 {
		t.Fatalf("requeued %d %v", n, err)
	}
	if j, _ := s.GetJob(ctx, sub.ID); j.State != state.Cancelled {
		t.Fatalf("sub-job %s", j.State)
	}
	if j, _ := s.ClaimJob(ctx); j == nil || j.ID != parent {
		t.Fatal("the parent wasn't requeued")
	}
}

// A review's findings and its finish are one transaction: if the job
// can't finish, its rulings aren't applied either.
func TestFinishReviewIsAllOrNothing(t *testing.T) {
	ctx := context.Background()
	s, _ := openTest(t)
	it := newItem(t, s, 1)
	first, _ := s.EnqueueJob(ctx, Job{ItemID: &it.ID, Kind: "review", Harness: "codex", Model: "m", BudgetUSD: 1})
	s.ClaimJob(ctx)
	f := ReviewFinding{ItemID: it.ID, FID: "F1", Seq: 1, Kind: "review", Round: 1, Severity: "major", Claim: "a", Blocking: true, Outcome: OutcomeOpen}
	if err := s.FinishReview(ctx, first, Result{State: state.Succeeded, Verdict: "{}"}, []ReviewFinding{f}, nil); err != nil {
		t.Fatal(err)
	}
	// Queued, not running: it can't finish, so its ruling is rolled back.
	next, _ := s.EnqueueJob(ctx, Job{ItemID: &it.ID, Kind: "review", Harness: "codex", Model: "m", BudgetUSD: 1})
	f.Ruling, f.RulingJob, f.Outcome = "withdrawn", next, OutcomeWithdrawn
	if err := s.FinishReview(ctx, next, Result{State: state.Succeeded}, nil, []ReviewFinding{f}); err == nil {
		t.Fatal("a queued job finished")
	}
	fs, _ := s.ItemFindings(ctx, it.ID, 0)
	if len(fs) != 1 || fs[0].Ruling != "" || fs[0].Outcome != OutcomeOpen {
		t.Fatalf("findings %+v", fs)
	}
	if j, _ := s.GetJob(ctx, first); j.State != state.Succeeded {
		t.Fatalf("first review %s", j.State)
	}
}
