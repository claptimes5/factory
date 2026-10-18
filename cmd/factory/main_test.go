package main

import (
	"context"
	"os"
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
	st.SetHealthIssue(ctx, store.HealthIssue{Repo: "o/r", Check: "test\x1b[2J\u202e", Kind: store.HealthRed, State: store.HealthRed, HeadSHA: "abcdef1234", IssueNumber: 12})
	st.SetHealthIssue(ctx, store.HealthIssue{Repo: "o/r", Check: "lint", Kind: store.HealthRed, State: store.HealthRed, HeadSHA: "abcdef1234"})
	st.SetHealthIssue(ctx, store.HealthIssue{Repo: "o/r", Check: "old", Kind: store.HealthFlaky, State: store.HealthGreen, HeadSHA: "abcdef1234", IssueNumber: 9})
	got := strings.Join(mainHealthLines(ctx, st, cfg), "\n")
	// No escape reaches the terminal, and a check passing now isn't listed.
	want := "main o/r:\tred on main at abcdef1: test [2J (red, o/r#12), lint (red, not filed yet)\nmain o/web:\tnot read yet"
	if got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
}

func TestSourceLines(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "f.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := &config.Config{Repos: []config.Repo{{Name: "o/r"}, {Name: "o/web"}}}
	if l := sourceLines(ctx, st, cfg); l != nil {
		t.Fatalf("all off and never read: %q", l)
	}
	cfg.Sources.Sentry.Enabled = true
	cfg.Sources.Sentry.Org, cfg.Sources.Sentry.Projects = "acme", map[string]string{"o/r": "api"}
	st.SetSourceState(ctx, store.SourceState{Repo: "o/r", Source: "sentry", State: store.SourceOK, Found: 2, Detail: "4 unresolved read, 2 over the thresholds"})
	st.SetSourceIssue(ctx, store.SourceIssue{Repo: "o/r", Source: "sentry", Key: "7", IssueNumber: 12})
	st.SetSourceState(ctx, store.SourceState{Repo: "o/web", Source: "todos", State: store.SourceUnreadable, Detail: "git clone: denied"})
	got := strings.Join(sourceLines(ctx, st, cfg), "\n")
	for _, want := range []string{"sentry o/r:\tread ", ": 2 found (4 unresolved read, 2 over the thresholds); issues o/r#12",
		"todos o/web:\tread ", ", can't read it: git clone: denied (sources.todos is off)"} {
		if !strings.Contains(got, want) {
			t.Errorf("lines lack %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "sentry o/web") {
		t.Errorf("o/web has no Sentry project:\n%s", got)
	}
}

func TestSentryTokenLeavesTheEnvironment(t *testing.T) {
	t.Setenv("SENTRY_AUTH_TOKEN", " sntrys_abc ")
	takeSentryToken()
	t.Cleanup(func() { sentryToken = "" })
	if _, set := os.LookupEnv("SENTRY_AUTH_TOKEN"); set || sentryToken != "sntrys_abc" {
		t.Fatalf("still in the environment, or lost: %q", sentryToken)
	}
	if sentryClient(&config.Config{}) == nil {
		t.Fatal("no client with a token")
	}
}
