package store

import (
	"context"
	"strings"
	"testing"
)

// A signal recorded twice is kept once; a used one is no longer unused.
func TestSignals(t *testing.T) {
	t.Parallel()
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

// A proposed line is kept once, moves to the PR that proposes it again,
// and remembers what became of it.
func TestLessons(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := openTest(t)
	for _, l := range []Lesson{{Repo: "o/R", Line: "Check X.", Norm: "check x"}, {Repo: "o/r", Line: "check x", Norm: "check x", PRNumber: 6}} {
		if err := s.AddLesson(ctx, l); err != nil {
			t.Fatal(err)
		}
	}
	ls, err := s.Lessons(ctx, "O/r")
	if err != nil || len(ls) != 1 || ls[0].PRNumber != 6 || ls[0].Status != LessonProposed {
		t.Fatalf("lessons = %+v, %v", ls, err)
	}
	if err := s.DecideLesson(ctx, ls[0].ID, LessonDeclined); err != nil {
		t.Fatal(err)
	}
	if err := s.AddLesson(ctx, Lesson{Repo: "o/r", Line: "Check X", Norm: "check x", PRNumber: 7}); err != nil {
		t.Fatal(err)
	}
	if ls, _ = s.Lessons(ctx, "o/r"); ls[0].Status != LessonDeclined || ls[0].DecidedAt == "" || ls[0].PRNumber != 6 {
		t.Fatalf("after decide = %+v", ls)
	}
}

// A finding a retried review job records again is kept once.
func TestSignalOncePerFinding(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := openTest(t)
	a := newItem(t, s, 1)
	f := Signal{Repo: "o/r", Kind: SignalFinding, ItemID: a.ID, JobID: 5, File: "add.go", Claim: "overflow", Severity: "major"}
	for _, job := range []int64{5, 6} {
		f.JobID = job
		if err := s.AddSignal(ctx, f); err != nil {
			t.Fatal(err)
		}
	}
	if got, _ := s.UnusedSignals(ctx, "o/r", ""); len(got) != 1 {
		t.Fatalf("unused = %+v", got)
	}
}

// Unused signals are found through their index, whatever the repo's case.
func TestUnusedSignalsUseTheIndex(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := openTest(t)
	rows, err := s.read.QueryContext(ctx, `EXPLAIN QUERY PLAN SELECT `+signalCols+` FROM lesson_signals `+unusedSignals, "o/r", "")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan += detail + "\n"
	}
	if !strings.Contains(plan, "lesson_signals_unused") {
		t.Fatalf("plan:\n%s", plan)
	}
	if err := s.AddSignal(ctx, Signal{Repo: "O/R", Kind: SignalFinding, ItemID: newItem(t, s, 1).ID, Claim: "x"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.UnusedSignals(ctx, "o/R", ""); len(got) != 1 || got[0].Repo != "o/r" {
		t.Fatalf("unused = %+v", got)
	}
}

// A human edit is recorded with its commits first and its diff later.
func TestSignalDiffLater(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := openTest(t)
	a := newItem(t, s, 1)
	if err := s.AddSignal(ctx, Signal{Repo: "o/r", Kind: SignalHumanEdit, ItemID: a.ID, PRNumber: 101, FromSHA: "aaa", HeadSHA: "bbb"}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.UnusedSignals(ctx, "o/r", "")
	if len(got) != 1 || got[0].FromSHA != "aaa" || got[0].HeadSHA != "bbb" || got[0].Diff != "" {
		t.Fatalf("unused = %+v", got)
	}
	if err := s.SetSignalDiff(ctx, got[0].ID, "+x"); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.SignalsByID(ctx, []int64{got[0].ID}); got[0].Diff != "+x" {
		t.Fatalf("after = %+v", got)
	}
}

// Only one learn job per repo is queued or running at a time.
func TestEnqueueSoleFor(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := openTest(t)
	learn := func(repo string) Job {
		return Job{Kind: "learn", Harness: "claude", Model: "sonnet", BudgetUSD: 1, Input: `{"repo":"` + repo + `"}`}
	}
	if _, err := s.EnqueueJob(ctx, Job{Kind: "triage", Harness: "claude", Model: "haiku", BudgetUSD: 1, Input: "not json"}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.EnqueueSoleFor(ctx, learn("o/r"), "o/r"); err != nil || !ok {
		t.Fatalf("first = %v, %v", ok, err)
	}
	if _, ok, err := s.EnqueueSoleFor(ctx, learn("O/R"), "O/R"); err != nil || ok {
		t.Fatalf("same repo = %v, %v", ok, err)
	}
	if _, ok, err := s.EnqueueSoleFor(ctx, learn("o/other"), "o/other"); err != nil || !ok {
		t.Fatalf("another repo = %v, %v", ok, err)
	}
}
