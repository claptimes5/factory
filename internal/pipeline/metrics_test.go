package pipeline

import (
	"context"
	"testing"
	"time"

	"github.com/claptimes5/factory/internal/metrics"
)

// The derived views over a first-pass item, an escalated one with a
// review block, and one whose PR a human changed before merging.
func TestMetricsFromRealRuns(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	fix := func(w string) error {
		writeFile(w, "add.go", "package fixture\n\n// Add adds, now checked.\nfunc Add(a, b int) int { return a + b }\n")
		return commitIn(w, "fix")
	}
	// 1 passes first time; 2 fails twice on sonnet, passes on opus, then
	// gets one review block and a fix; 3 passes first time.
	runs := []struct {
		builds  []func(string) error
		reviews []string
	}{
		{[]func(string) error{goodBuild}, []string{pass}},
		{[]func(string) error{badBuild, badBuild, goodBuild, fix}, []string{block, pass}},
		{[]func(string) error{goodBuild}, []string{pass}},
	}
	for i, r := range runs {
		e.addIssue(i + 1)
		e.claude.builds, e.claude.calls = r.builds, nil
		e.codex.reviews, e.codex.calls = r.reviews, nil
		e.p.lastPoll = time.Time{}
		if err := e.p.Drain(ctx); err != nil {
			t.Fatal(err)
		}
	}
	for n, head := range map[int]string{1: "", 2: "", 3: "human-edited"} {
		pr := e.gh.prs[e.p.branchFor(e.item(t, n))]
		pr.Merged = true
		if head != "" {
			pr.Head.SHA = head
		} else {
			pr.Head.SHA, _ = headOf(e.p.lastHeads(ctx, e.item(t, n)), e.item(t, n).Repo)
		}
	}
	e.p.lastPoll = time.Time{}
	e.p.Drain(ctx)

	r, err := metrics.Compute(ctx, e.p.Store)
	if err != nil {
		t.Fatal(err)
	}
	if r.Merged != 3 || r.CostPerMerged <= 0 {
		t.Fatalf("report = %+v", r)
	}
	if r.FirstPass < 0.66 || r.FirstPass > 0.67 || r.Escalation < 0.33 || r.Escalation > 0.34 {
		t.Fatalf("first pass %.2f escalation %.2f", r.FirstPass, r.Escalation)
	}
	if r.ReviewBlocks != 1 || r.ReviewEscapes < 0.33 || r.ReviewEscapes > 0.34 {
		t.Fatalf("blocks %d escapes %.2f", r.ReviewBlocks, r.ReviewEscapes)
	}
	if len(r.ByComplexity) != 1 || r.ByComplexity[0].Key != "M" || r.ByComplexity[0].Merged != 3 {
		t.Fatalf("by complexity = %+v", r.ByComplexity)
	}
	// Item 2 escalated to opus, but its fix round (the job that landed) ran on sonnet.
	if len(r.ByModel) != 1 || r.ByModel[0].Key != "claude/sonnet" || r.AwaitingHours <= 0 {
		t.Fatalf("by model = %+v, awaiting %.6f", r.ByModel, r.AwaitingHours)
	}
}
