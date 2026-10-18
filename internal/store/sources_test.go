package store

import (
	"context"
	"testing"
)

func TestSources(t *testing.T) {
	ctx := context.Background()
	s, _ := openTest(t)
	if st, err := s.SourceState(ctx, "o/r", "sentry"); st != nil || err != nil {
		t.Fatalf("before any read: %+v %v", st, err)
	}
	s.SetSourceState(ctx, SourceState{Repo: "o/r", Source: "sentry", State: SourceUnreadable, Detail: "401", Notified: SourceUnreadable})
	s.SetSourceState(ctx, SourceState{Repo: "o/r", Source: "sentry", State: SourceOK, Found: 3})
	s.SetSourceState(ctx, SourceState{Repo: "o/r", Source: "todos", State: SourceOK})
	if st, _ := s.SourceState(ctx, "o/r", "sentry"); st == nil || st.State != SourceOK || st.Found != 3 || st.Notified != "" || st.PolledAt == "" {
		t.Fatalf("state %+v", st)
	}
	if sts, _ := s.SourceStates(ctx); len(sts) != 2 {
		t.Fatalf("states %+v", sts)
	}
	// One row per key: filing again updates it.
	s.SetSourceIssue(ctx, SourceIssue{Repo: "o/r", Source: "sentry", Key: "42", IssueNumber: 4})
	s.SetSourceIssue(ctx, SourceIssue{Repo: "o/r", Source: "sentry", Key: "42", IssueNumber: 5, Title: "t", BodyHash: "h"})
	s.SetSourceIssue(ctx, SourceIssue{Repo: "o/r", Source: "todos", Key: "k", IssueNumber: 6})
	if si, _ := s.SourceIssues(ctx, "o/r", "sentry"); len(si) != 1 || si[0].IssueNumber != 5 || si[0].Title != "t" || si[0].BodyHash != "h" {
		t.Fatalf("sentry %+v", si)
	}
	if si, _ := s.SourceIssues(ctx, "", ""); len(si) != 2 {
		t.Fatalf("all %+v", si)
	}
	s.ClearSourceIssue(ctx, "o/r", "sentry", "42")
	if si, _ := s.SourceIssues(ctx, "o/r", ""); len(si) != 1 || si[0].Source != "todos" {
		t.Fatalf("after clear %+v", si)
	}
}
