package store

import (
	"context"
	"testing"
)

func TestHealth(t *testing.T) {
	ctx := context.Background()
	s, _ := openTest(t)
	if h, err := s.RepoHealth(ctx, "o/r"); h != nil || err != nil {
		t.Fatalf("before any reading: %+v %v", h, err)
	}
	if err := s.SetRepoHealth(ctx, RepoHealth{Repo: "o/r", Branch: "main", HeadSHA: "a", State: HealthRed, Detail: "test"}); err != nil {
		t.Fatal(err)
	}
	s.SetRepoHealth(ctx, RepoHealth{Repo: "o/r", Branch: "main", HeadSHA: "b", State: HealthGreen})
	if hs, _ := s.RepoHealths(ctx); len(hs) != 1 || hs[0].State != HealthGreen || hs[0].HeadSHA != "b" {
		t.Fatalf("healths %+v", hs)
	}
	// One row per check: filing later updates it.
	s.SetHealthIssue(ctx, HealthIssue{Repo: "o/r", Check: "test", Kind: HealthRed, HeadSHA: "a"})
	s.SetHealthIssue(ctx, HealthIssue{Repo: "o/r", Check: "test", Kind: HealthRed, HeadSHA: "a", IssueNumber: 4})
	s.SetHealthIssue(ctx, HealthIssue{Repo: "o/x", Check: "lint", Kind: HealthFlaky, HeadSHA: "c", IssueNumber: 5})
	s.SetHealthIssue(ctx, HealthIssue{Repo: "o/r", Check: "test", Kind: HealthRed, HeadSHA: "a", IssueNumber: 4, State: HealthAbsent, AbsentSince: "t1", MutedUntil: "t2", Refiles: 1})
	if hs, _ := s.HealthIssues(ctx, "o/r"); len(hs) != 1 || hs[0].IssueNumber != 4 || hs[0].State != HealthAbsent || hs[0].AbsentSince != "t1" || hs[0].MutedUntil != "t2" || hs[0].Refiles != 1 {
		t.Fatalf("o/r %+v", hs)
	}
	if hs, _ := s.HealthIssues(ctx, ""); len(hs) != 2 {
		t.Fatalf("all %+v", hs)
	}
	if ok, _ := s.IsHealthIssue(ctx, "o/r", 4); !ok {
		t.Fatal("#4 is a health issue")
	}
	if ok, _ := s.IsHealthIssue(ctx, "o/r", 5); ok {
		t.Fatal("#5 is o/x's")
	}
	s.ClearHealthIssue(ctx, "o/r", "test")
	if ok, _ := s.IsHealthIssue(ctx, "o/r", 4); ok {
		t.Fatal("cleared, still tracked")
	}
	s.AddEvent(ctx, Event{Actor: "daemon", Type: "main_health_filed", Reason: "o/r"})
	s.AddEvent(ctx, Event{Actor: "daemon", Type: "main_health_filed", Reason: "o/x"})
	if n, _ := s.CountEventsFor(ctx, "main_health_filed", "o/r", "2000-01-01"); n != 1 {
		t.Fatalf("count %d", n)
	}
	if n, _ := s.CountEventsFor(ctx, "main_health_filed", "o/r", "2999-01-01"); n != 0 {
		t.Fatalf("count since later %d", n)
	}
}
