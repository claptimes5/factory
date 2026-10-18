package store

import (
	"context"
	"testing"
)

func TestFactoryMergesRevertsAndStrikes(t *testing.T) {
	ctx := context.Background()
	s, _ := openTest(t)
	at := func(day string) string { return "2026-10-" + day + "T00:00:00.000000000Z" }
	for _, m := range []FactoryMerge{
		{Repo: "o/r", PR: 1, MergeSHA: "aaa1111", Title: "one", MergedAt: at("01")},
		{Repo: "o/r", PR: 2, MergeSHA: "bbb2222", SquashSHA: "sss2222", Title: "two", Auto: true, MergedAt: at("02")},
		{Repo: "o/x", PR: 3, MergeSHA: "ccc3333", MergedAt: at("03")},
	} {
		if err := s.RecordMerge(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	// A second record of the same PR keeps the first.
	s.RecordMerge(ctx, FactoryMerge{Repo: "o/r", PR: 1, MergeSHA: "zzz", MergedAt: at("09")})
	ms, err := s.FactoryMerges(ctx, "o/r", at("01"))
	if err != nil || len(ms) != 2 || ms[0].MergeSHA != "aaa1111" || !ms[1].Auto || ms[1].Auto == ms[0].Auto {
		t.Fatalf("merges %+v %v", ms, err)
	}
	if ms[1].SquashSHA != "sss2222" {
		t.Fatalf("squash sha %+v", ms[1])
	}
	if all, _ := s.FactoryMerges(ctx, "", at("02")); len(all) != 2 {
		t.Fatalf("since the 2nd, every repo: %+v", all)
	}

	// A revert is news once.
	if news, err := s.MarkReverted(ctx, "o/r", 1, "commit abc", at("05")); !news || err != nil {
		t.Fatalf("first revert: %v %v", news, err)
	}
	if news, _ := s.MarkReverted(ctx, "o/r", 1, "PR #9", at("06")); news {
		t.Fatal("a second revert of the same PR was news")
	}
	if m, err := s.MergeBySHA(ctx, "o/r", "bbb2222"); err != nil || m == nil || m.PR != 2 {
		t.Fatalf("by sha: %+v %v", m, err)
	}
	if m, _ := s.MergeBySHA(ctx, "o/r", "not-ours"); m != nil {
		t.Fatalf("by sha, not ours: %+v", m)
	}
	// A red on a merge commit is news once, and only for a factory merge.
	if m, err := s.MarkRed(ctx, "o/r", "bbb2222", "test", at("05")); err != nil || m == nil || m.PR != 2 || m.RedCheck != "test" {
		t.Fatalf("red: %+v %v", m, err)
	}
	if m, _ := s.MarkRed(ctx, "o/r", "bbb2222", "lint", at("06")); m != nil {
		t.Fatal("a second red on the same merge was news")
	}
	if m, _ := s.MergeBySHA(ctx, "o/r", "bbb2222"); m != nil {
		t.Fatal("a merge already red is still looked up")
	}
	if m, _ := s.MarkRed(ctx, "o/r", "not-ours", "test", at("06")); m != nil {
		t.Fatal("a commit no factory PR made was attributed")
	}
	// PR 1 reverted on the 5th, PR 2 red on the 5th: two strikes since the
	// 4th, none since the 6th.
	if st, _ := s.Strikes(ctx, "o/r", at("04")); len(st) != 2 {
		t.Fatalf("strikes %+v", st)
	}
	if st, _ := s.Strikes(ctx, "o/r", at("06")); len(st) != 0 {
		t.Fatalf("strikes after: %+v", st)
	}
	// Reverted and red is still one strike.
	s.MarkReverted(ctx, "o/r", 2, "commit def", at("05"))
	if st, _ := s.Strikes(ctx, "o/r", at("04")); len(st) != 2 {
		t.Fatalf("a PR both reverted and red counted twice: %+v", st)
	}
}

func TestBreakerTripsOnceAndResets(t *testing.T) {
	ctx := context.Background()
	s, _ := openTest(t)
	if g, err := s.Guard(ctx, "o/r"); g != nil || err != nil || g.Tripped() {
		t.Fatalf("no guard yet: %+v %v", g, err)
	}
	if err := s.SetWatched(ctx, "o/r", "head1", Now()); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.TripBreaker(ctx, "o/r", "two reverts", Now()); !ok || err != nil {
		t.Fatalf("trip: %v %v", ok, err)
	}
	if ok, _ := s.TripBreaker(ctx, "o/r", "again", Now()); ok {
		t.Fatal("tripped twice")
	}
	g, _ := s.Guard(ctx, "o/r")
	if !g.Tripped() || g.Reason != "two reverts" || g.WatchedSHA != "head1" {
		t.Fatalf("guard %+v", g)
	}
	if ok, err := s.ResetBreaker(ctx, "o/r", "me", Now()); !ok || err != nil {
		t.Fatalf("reset: %v %v", ok, err)
	}
	if ok, _ := s.ResetBreaker(ctx, "o/r", "me", Now()); ok {
		t.Fatal("reset an untripped breaker")
	}
	g, _ = s.Guard(ctx, "o/r")
	if g.Tripped() || g.ResetBy != "me" || g.ResetAt == "" {
		t.Fatalf("after reset %+v", g)
	}
	if gs, _ := s.Guards(ctx); len(gs) != 1 {
		t.Fatalf("guards %+v", gs)
	}
}
