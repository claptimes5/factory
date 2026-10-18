package store

import (
	"context"
	"testing"
)

// A signal recorded twice is kept once; a used one is no longer unused.
func TestSignals(t *testing.T) {
	ctx := context.Background()
	s, _ := openTest(t)
	a, b := newItem(t, s, 1), newItem(t, s, 2)
	f := Signal{Repo: "o/r", Kind: SignalFinding, ItemID: a.ID, IssueNumber: 1, JobID: 5, File: "add.go", Claim: "overflow", Severity: "major"}
	for _, sig := range []Signal{f, f, {Repo: "o/r", Kind: SignalHumanEdit, ItemID: b.ID, IssueNumber: 2, PRNumber: 101, Diff: "+x"}, {Repo: "o/other", Kind: SignalFinding, ItemID: b.ID, JobID: 6, Claim: "x"}} {
		if err := s.AddSignal(ctx, sig); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.UnusedSignals(ctx, "O/R", "")
	if err != nil || len(got) != 2 || got[0].Claim != "overflow" || got[1].Diff != "+x" || got[1].PRNumber != 101 {
		t.Fatalf("unused = %+v, %v", got, err)
	}
	if err := s.UseSignals(ctx, []int64{got[0].ID}, 9); err != nil {
		t.Fatal(err)
	}
	left, _ := s.UnusedSignals(ctx, "o/r", "")
	if len(left) != 1 || left[0].Kind != SignalHumanEdit {
		t.Fatalf("after use = %+v", left)
	}
	byID, _ := s.SignalsByID(ctx, []int64{got[0].ID, got[1].ID})
	if len(byID) != 2 || byID[0].LearnJob != 9 || byID[1].LearnJob != 0 {
		t.Fatalf("by id = %+v", byID)
	}
	if later, _ := s.UnusedSignals(ctx, "o/r", "9999"); len(later) != 0 {
		t.Fatalf("since = %+v", later)
	}
}
