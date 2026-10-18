package store

import (
	"context"
	"testing"
)

func TestGroomProposals(t *testing.T) {
	t.Parallel()
	s, _ := openTest(t)
	ctx := context.Background()
	id, err := s.AddGroomProposal(ctx, GroomProposal{Repo: "o/r", IssueNumber: 7, Kind: GroomSplit, Key: "k", IssueHash: "h",
		Children: []GroomChild{{Title: "a", Body: "b"}, {Title: "c", Body: "d"}}})
	if err != nil {
		t.Fatal(err)
	}
	// One open per issue.
	if _, err := s.AddGroomProposal(ctx, GroomProposal{Repo: "o/r", IssueNumber: 7, Kind: GroomStale, Key: "stale", IssueHash: "h"}); err == nil {
		t.Fatal("two open proposals for one issue")
	}
	open, err := s.OpenGroomProposals(ctx, "O/R")
	if err != nil || len(open) != 1 || len(open[0].Children) != 2 || open[0].Status != GroomOpen {
		t.Fatalf("open %+v %v", open, err)
	}
	open[0].Children[0].Issue = 20
	if err := s.SetGroomChildren(ctx, id, open[0].Children); err != nil {
		t.Fatal(err)
	}
	if err := s.DecideGroomProposal(ctx, id, GroomDismissed, "human"); err != nil {
		t.Fatal(err)
	}
	all, _ := s.GroomProposals(ctx, "o/r", 7)
	if len(all) != 1 || all[0].Status != GroomDismissed || all[0].Children[0].Issue != 20 || all[0].DecidedAt == "" {
		t.Fatalf("after %+v", all)
	}
	// A decided one stays decided.
	if err := s.DecideGroomProposal(ctx, id, GroomDone, "x"); err != nil {
		t.Fatal(err)
	}
	if all, _ := s.GroomProposals(ctx, "", 0); all[0].Status != GroomDismissed {
		t.Fatal("re-decided")
	}
	if _, err := s.AddGroomProposal(ctx, GroomProposal{Repo: "o/r", IssueNumber: 7, Kind: GroomStale, Key: "stale", IssueHash: "h2"}); err != nil {
		t.Fatal(err)
	}

	if err := s.AddGroomChild(ctx, "o/r", 20, 7); err != nil {
		t.Fatal(err)
	}
	if p, _ := s.GroomParent(ctx, "o/r", 20); p != 7 {
		t.Fatalf("parent %d", p)
	}
	if cs, _ := s.GroomChildren(ctx, "", 7); len(cs) != 1 || cs[0].Issue != 20 {
		t.Fatalf("children %+v", cs)
	}
	_ = s.ClearGroomChild(ctx, "o/r", 20)
	if p, _ := s.GroomParent(ctx, "o/r", 20); p != 0 {
		t.Fatal("not cleared")
	}

	// A done split makes an epic; a deleted proposal is gone.
	sid, _ := s.AddGroomProposal(ctx, GroomProposal{Repo: "o/r", IssueNumber: 30, Kind: GroomSplit, Key: "k", IssueHash: "h"})
	if ok, _ := s.GroomSplit(ctx, "o/r", 30); ok {
		t.Fatal("an open split is no epic")
	}
	_ = s.DecideGroomProposal(ctx, sid, GroomDone, "m")
	if ok, _ := s.GroomSplit(ctx, "O/R", 30); !ok {
		t.Fatal("done split not found")
	}
	did, _ := s.AddGroomProposal(ctx, GroomProposal{Repo: "o/r", IssueNumber: 31, Kind: GroomStale, Key: "k", IssueHash: "h"})
	_ = s.DeleteGroomProposal(ctx, did)
	if ps, _ := s.GroomProposals(ctx, "o/r", 31); len(ps) != 0 {
		t.Fatal("not deleted")
	}

	if err := s.SetGroomRun(ctx, GroomRun{Repo: "o/r", Read: 10, Flagged: 2}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetGroomRun(ctx, GroomRun{Repo: "o/r", Read: 12, Flagged: 1, Note: "n"}); err != nil {
		t.Fatal(err)
	}
	if rs, _ := s.GroomRuns(ctx); len(rs) != 1 || rs[0].Read != 12 || rs[0].Note != "n" {
		t.Fatalf("runs %+v", rs)
	}
}
