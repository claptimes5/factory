package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/claptimes5/factory/internal/config"
	"github.com/claptimes5/factory/internal/store"
)

func TestAutoMergeCommand(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "f.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := &config.Config{Repos: []config.Repo{{Name: "o/r"}, {Name: "o/web"}}, AutoMerge: &config.AutoMerge{UpTo: "low"}}
	cfg.Repos[1].AutoMerge = &config.AutoMerge{UpTo: "none"}
	st.RecordMerge(ctx, store.FactoryMerge{Repo: "o/r", PR: 3, Title: "t"})
	st.MarkReverted(ctx, "o/r", 3, "commit abc", store.Now())
	st.TripBreaker(ctx, "o/r", "2 factory PRs in 7 days: #3 reverted\x1b[31m", store.Now())

	got := strings.Join(autoMergeLines(ctx, st, cfg), "\n")
	for _, want := range []string{"auto-merge o/r:\toff since", "#3 reverted [31m", "factory automerge reset o/r", "reverts 100% (1 of 1 factory PRs merged in 30 days)"} {
		if !strings.Contains(got, want) {
			t.Fatalf("lines lack %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "o/web") || strings.Contains(got, "\x1b") {
		t.Fatalf("lines:\n%q", got)
	}

	var out bytes.Buffer
	if err := automerge(ctx, &out, cfg, st, []string{"reset", "o/r"}); err != nil || !strings.Contains(out.String(), "reset") {
		t.Fatalf("reset: %q %v", out.String(), err)
	}
	if g, _ := st.Guard(ctx, "o/r"); g.Tripped() || !strings.HasPrefix(g.ResetBy, "factory automerge reset") {
		t.Fatalf("guard %+v", g)
	}
	if err := automerge(ctx, &out, cfg, st, []string{"reset", "o/r"}); err == nil {
		t.Fatal("reset an untripped breaker")
	}
	if err := automerge(ctx, &out, cfg, st, []string{"reset", "o/elsewhere"}); err == nil {
		t.Fatal("reset a repo not in the config")
	}
	got = strings.Join(autoMergeLines(ctx, st, cfg), "\n")
	if !strings.Contains(got, "auto-merge o/r:\ton; 0 of 2 strikes in 7d trip its breaker") {
		t.Fatalf("after reset:\n%s", got)
	}
}
