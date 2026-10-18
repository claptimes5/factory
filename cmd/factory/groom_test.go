package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/claptimes5/factory/internal/config"
	"github.com/claptimes5/factory/internal/store"
	"github.com/claptimes5/factory/internal/store/storetest"
)

func TestGroomCommand(t *testing.T) {
	ctx := context.Background()
	st := storetest.Open(t, filepath.Join(t.TempDir(), "f.db"))
	cfg := &config.Config{Repos: []config.Repo{{Name: "o/r"}, {Name: "o/web"}},
		Routing: map[string]config.Route{"groom": {Harness: "claude", Model: "sonnet"}}}
	cfg.Budgets.Triage = 0.5
	// Grooming is off, but the command runs it now.
	var out bytes.Buffer
	if err := groom(ctx, &out, cfg, st, ""); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); strings.Count(got, "queued grooming run") != 2 {
		t.Fatalf("output %q", got)
	}
	out.Reset()
	if err := groom(ctx, &out, cfg, st, "o/web"); err != nil || !strings.Contains(out.String(), "o/web: a grooming run is already queued") {
		t.Fatalf("again: %q %v", out.String(), err)
	}
	if err := groom(ctx, &out, cfg, st, "o/elsewhere"); err == nil {
		t.Fatal("groomed a repo not in the config")
	}
	jobs, _ := st.ListJobs(ctx, store.JobFilter{Kind: "groom"})
	if len(jobs) != 2 || jobs[0].BudgetUSD != 0.5 || jobs[0].Model != "sonnet" {
		t.Fatalf("jobs %+v", jobs)
	}
}

func TestGroomLines(t *testing.T) {
	ctx := context.Background()
	st := storetest.Open(t, filepath.Join(t.TempDir(), "f.db"))
	cfg := &config.Config{Repos: []config.Repo{{Name: "o/r"}, {Name: "o/web"}}}
	if l := groomLines(ctx, st, cfg); l != nil {
		t.Fatalf("off and never ran: %q", l)
	}
	cfg.Grooming.Every = 168 * time.Hour
	_ = st.SetGroomRun(ctx, store.GroomRun{Repo: "o/r", Read: 40, Flagged: 3, Note: "1 proposal dropped"})
	_, _ = st.AddGroomProposal(ctx, store.GroomProposal{Repo: "o/r", IssueNumber: 4, Kind: store.GroomSplit, Key: "k", IssueHash: "h"})
	_, _ = st.AddGroomProposal(ctx, store.GroomProposal{Repo: "o/r", IssueNumber: 2, Kind: store.GroomDuplicate, Key: "k", IssueHash: "h"})
	got := strings.Join(groomLines(ctx, st, cfg), "\n")
	for _, want := range []string{"grooming o/r:\tlast run " + store.Today() + ", read 40, flagged 3 (1 proposal dropped); 2 open: duplicate o/r#2; split o/r#4",
		"grooming o/web:\tnot run yet"} {
		if !strings.Contains(got, want) {
			t.Errorf("lines lack %q:\n%s", want, got)
		}
	}
}
