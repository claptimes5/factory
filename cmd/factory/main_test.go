package main

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/claptimes5/factory/internal/config"
	"github.com/claptimes5/factory/internal/state"
	"github.com/claptimes5/factory/internal/store"
)

func TestWaitJob(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "f.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	finish := func(r store.Result) int64 {
		id, _ := st.EnqueueJob(ctx, store.Job{Kind: "triage", Harness: "claude", Model: "m", BudgetUSD: 1})
		st.ClaimJob(ctx)
		st.FinishJob(ctx, id, r)
		return id
	}
	first := finish(store.Result{State: state.Succeeded, CostUSD: 0.1})
	if err := waitJob(ctx, st, "0", "1s"); err != nil {
		t.Fatalf("a finished job: %v", err)
	}
	finish(store.Result{State: state.Failed, Error: "no such image"})
	if err := waitJob(ctx, st, itoa(first), "1s"); err == nil {
		t.Fatal("a job that failed before doing any work must fail the upgrade")
	}
	if err := waitJob(ctx, st, "99", "1ms"); err != nil {
		t.Fatalf("an idle factory is not a failed upgrade: %v", err)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestMainHealthLines(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "f.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := &config.Config{Repos: []config.Repo{{Name: "o/r"}, {Name: "o/web"}}}
	if l := mainHealthLines(ctx, st, cfg); l != nil {
		t.Fatalf("off and never read: %q", l)
	}
	cfg.MainHealth.Enabled = true
	st.SetRepoHealth(ctx, store.RepoHealth{Repo: "o/r", Branch: "main", HeadSHA: "abcdef1234", State: store.HealthRed, Detail: "test, lint"})
	st.SetHealthIssue(ctx, store.HealthIssue{Repo: "o/r", Check: "test", Kind: store.HealthRed, HeadSHA: "abcdef1234", IssueNumber: 12})
	st.SetHealthIssue(ctx, store.HealthIssue{Repo: "o/r", Check: "lint", Kind: store.HealthRed, HeadSHA: "abcdef1234"})
	got := strings.Join(mainHealthLines(ctx, st, cfg), "\n")
	want := "main o/r:\tred on main at abcdef1: test (red, o/r#12), lint (red, not filed yet)\nmain o/web:\tnot read yet"
	if got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
}
