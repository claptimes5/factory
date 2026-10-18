package metrics

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/claptimes5/factory/internal/state"
	"github.com/claptimes5/factory/internal/store"
)

// Precision counts settled blocking findings: refuted ones and ones a
// review ruled on, or any once the item is out of the loop; rounds to
// pass counts the review rounds of items that passed.
func TestReviewPrecisionAndRoundsToPass(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "f.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	review := func(it *store.WorkItem, v string) int64 {
		id, _ := st.EnqueueJob(ctx, store.Job{ItemID: &it.ID, Kind: "review", Harness: "codex", Model: "m", BudgetUSD: 1})
		st.ClaimJob(ctx)
		st.FinishJob(ctx, id, store.Result{State: state.Succeeded, Verdict: v})
		return id
	}
	const (
		blocked = `{"decision":"block","findings":[{"severity":"major","file":"a","line":1,"claim":"c","evidence":"e"}],"challenged_assumptions":[]}`
		passed  = `{"decision":"pass","findings":[],"challenged_assumptions":[]}`
	)
	f := func(it *store.WorkItem, fid, check, ruling, outcome string) store.ReviewFinding {
		return store.ReviewFinding{ItemID: it.ID, FID: fid, Seq: len(fid), Route: "review", Harness: "codex", Model: "m", Severity: "major", Claim: "c",
			Blocking: true, Check: check, Confirmed: check == "confirmed", Ruling: ruling, Outcome: outcome}
	}
	move := func(it *store.WorkItem, to state.Stage) {
		for _, s := range []state.Stage{state.Ready, state.Implementing, state.Reviewing, state.AwaitingHuman, state.Done} {
			if err := st.SetStage(ctx, it.ID, s, "daemon", "t"); err != nil {
				t.Fatal(err)
			}
			if s == to {
				return
			}
		}
	}
	// Done after two rounds: one finding fixed, one refuted.
	a, _, _ := st.UpsertItem(ctx, "o/r", 1, "a")
	j1 := review(a, blocked)
	review(a, passed)
	st.SaveReview(ctx, j1, []store.ReviewFinding{f(a, "F1", "confirmed", "resolved", store.OutcomeFixed), f(a, "F2", "refuted", "", store.OutcomeRefuted)}, nil)
	move(a, state.Done)
	// Still being fixed: its open finding isn't settled yet.
	b, _, _ := st.UpsertItem(ctx, "o/r", 2, "b")
	j2 := review(b, blocked)
	st.SaveReview(ctx, j2, []store.ReviewFinding{f(b, "F1", "confirmed", "", store.OutcomeFixed)}, nil)
	move(b, state.Implementing)
	// Passed the first time.
	c, _, _ := st.UpsertItem(ctx, "o/r", 3, "c")
	review(c, passed)
	move(c, state.AwaitingHuman)

	r, err := Compute(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	if r.ReviewPrecision != 0.5 || r.RoundsToPass != 1.5 || r.VerifierRefuted != 1.0/3 {
		t.Fatalf("precision %v, rounds %v, refuted %v", r.ReviewPrecision, r.RoundsToPass, r.VerifierRefuted)
	}
	if len(r.Precision) != 1 || r.Precision[0].Key != "review codex/m" || r.Precision[0].Findings != 2 || r.Precision[0].Fixed != 1 || r.Precision[0].Refuted != 1 {
		t.Fatalf("by reviewer %+v", r.Precision)
	}
	// No findings, no data.
	empty, _ := store.Open(filepath.Join(t.TempDir(), "e.db"))
	defer empty.Close()
	if r, _ := Compute(ctx, empty); r.ReviewPrecision != -1 || r.RoundsToPass != -1 || r.Precision == nil {
		t.Fatalf("empty %+v", r)
	}
}
