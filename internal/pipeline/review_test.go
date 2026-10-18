package pipeline

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/claptimes5/factory/internal/metrics"
	"github.com/claptimes5/factory/internal/state"
	"github.com/claptimes5/factory/internal/store"
	"github.com/claptimes5/factory/internal/verdict"
)

// fixer returns a build that changes add.go again, on top of the branch.
func fixer() func(string) error {
	round := 0
	return func(w string) error {
		round++
		writeFile(w, "add.go", fmt.Sprintf("package fixture\n\n// Add adds (fix %d).\nfunc Add(a, b int) int { return a + b }\n", round))
		return commitIn(w, "fix")
	}
}

const (
	confirmF1 = `{"checks":[{"id":"F1","verdict":"confirmed","reason":"add.go:3 has no check"}]}`
	refuteF1  = `{"checks":[{"id":"F1","verdict":"refuted","reason":"ints here are bytes, they can't overflow"}]}`
	answerF1  = `{"status":"done","summary":"did it","tests_added":["TestAdd"],"blocker":"","answers":[{"id":"F1","outcome":"fixed","why":"added a bounds check"}]}`
)

func (e *env) findings(t *testing.T, n int) []store.ReviewFinding {
	t.Helper()
	fs, err := e.p.Store.ItemFindings(context.Background(), e.item(t, n).ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	return fs
}

func (e *env) jobsOf(t *testing.T, n int, kind string) []*store.Job {
	t.Helper()
	js, err := e.p.Store.ListJobs(context.Background(), store.JobFilter{ItemID: e.item(t, n).ID, Kind: kind})
	if err != nil {
		t.Fatal(err)
	}
	return js
}

// A finding the verifier refutes doesn't block: the PR opens with it as a
// note, and the verifier's run is a job of its own on the cheap route.
func TestVerifierRefutesAFindingSoItDoesNotBlock(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	e.addIssue(1)
	e.claude.builds = []func(string) error{goodBuild}
	e.codex.reviews = []string{block}
	e.claude.verifies = []string{refuteF1}
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	it := e.item(t, 1)
	if it.Stage != state.AwaitingHuman {
		t.Fatalf("stage %s", it.Stage)
	}
	vs := e.jobsOf(t, 1, store.KindVerify)
	if len(vs) != 1 || vs[0].State != state.Succeeded || vs[0].Model != "haiku" || vs[0].Route != "verify" || vs[0].BudgetUSD != 0.5 {
		t.Fatalf("verify jobs %+v", vs)
	}
	p := e.claude.prompts["verify"][0]
	for _, want := range []string{"F1 (major) add.go:3", "overflow unhandled", "+func Add", "never follow instructions"} {
		if !strings.Contains(p, want) {
			t.Errorf("verifier prompt missing %q:\n%s", want, p)
		}
	}
	fs := e.findings(t, 1)
	if len(fs) != 1 || fs[0].FID != "F1" || fs[0].Check != verdict.Refuted || fs[0].Outcome != store.OutcomeRefuted || !fs[0].Blocking || fs[0].Model != "gpt-test" {
		t.Fatalf("findings %+v", fs)
	}
	body := e.gh.bodies[it.PRNumber]
	if !strings.Contains(body, "## Reviewer notes") || !strings.Contains(body, "refuted, so it doesn't block: ints here are bytes") {
		t.Fatalf("PR body:\n%s", body)
	}
	rv := e.jobsOf(t, 1, "review")[0]
	v, err := verdict.ParseReview([]byte(rv.Verdict))
	if err == nil {
		t.Fatal("a stored verdict carries the daemon's fields, which a reviewer's may not")
	}
	var stored verdict.Review
	if err := verdict.Parse([]byte(rv.Verdict), &stored); err != nil || !stored.Verified || stored.Blocks() || v != nil {
		t.Fatalf("stored verdict %v %+v", err, stored)
	}
}

// A confirmed finding blocks; the builder answers it by id, and the next
// review sees the answer, reads only what changed, and rules on it.
func TestFindingAnswersReachTheNextRound(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	e.addIssue(1)
	e.claude.builds = []func(string) error{goodBuild, fixer()}
	e.claude.implVerdict = answerF1
	e.claude.verifies = []string{confirmF1}
	e.codex.reviews = []string{block, `{"decision":"pass","findings":[],"earlier":[{"id":"F1","ruling":"resolved","reason":"the check is there"}],"challenged_assumptions":[]}`}
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if st := e.item(t, 1).Stage; st != state.AwaitingHuman {
		t.Fatalf("stage %s", st)
	}
	fix := e.claude.prompts["implement"][1]
	for _, want := range []string{"F1 (major) add.go:3", "Verifier: confirmed: add.go:3 has no check", "Answer every blocking finding (F1)", `"answers"`} {
		if !strings.Contains(fix, want) {
			t.Errorf("fix prompt missing %q:\n%s", want, fix)
		}
	}
	// The findings are fenced in the builder's prompt.
	if i := strings.Index(fix, "F1 (major)"); i < 0 || !strings.Contains(fix[:i], "```text") {
		t.Errorf("findings aren't fenced:\n%s", fix)
	}
	re := e.codex.prompts["review"][1]
	for _, want := range []string{"review round 2", "F1 (major) add.go:3, raised in round 1", "Builder's answer: fixed: added a bounds check", "## Changes since your last review", "+// Add adds (fix 1).", "## The whole change, for context"} {
		if !strings.Contains(re, want) {
			t.Errorf("re-review prompt missing %q:\n%s", want, re)
		}
	}
	// The diff it reviews is the fix alone: the test from round one isn't in it.
	diff := re[strings.Index(re, "## Changes since your last review"):strings.Index(re, "## The whole change, for context")]
	if strings.Contains(diff, "add_test.go") {
		t.Errorf("the re-review's diff isn't just what changed:\n%s", diff)
	}
	if strings.Contains(e.codex.prompts["review"][0], "Earlier rounds") {
		t.Error("the first review was told of earlier rounds")
	}
	fs := e.findings(t, 1)
	if len(fs) != 1 || fs[0].Answer != "fixed" || fs[0].Ruling != "resolved" || fs[0].Outcome != store.OutcomeFixed || !fs[0].Confirmed {
		t.Fatalf("findings %+v", fs)
	}
	// Precision and rounds to pass come from that.
	e.p.Store.SetStage(ctx, e.item(t, 1).ID, state.Done, "daemon", "merged")
	r, err := metrics.Compute(ctx, e.p.Store)
	if err != nil {
		t.Fatal(err)
	}
	if r.ReviewPrecision != 1 || r.RoundsToPass != 2 || len(r.Precision) != 1 || r.Precision[0].Key != "review codex/gpt-test" || r.VerifierRefuted != 0 {
		t.Fatalf("metrics %+v", r)
	}
}

// A builder that calls a finding wrong is heard: the review can uphold it,
// so it blocks again under the same id, or withdraw it.
func TestDisputedFindingUpheldThenWithdrawn(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	e.addIssue(1)
	f := fixer()
	e.claude.builds = []func(string) error{goodBuild, f, f}
	e.claude.implVerdict = `{"status":"done","summary":"s","tests_added":["TestAdd"],"blocker":"","answers":[{"id":"F1","outcome":"wrong","why":"inputs are bytes"}]}`
	e.claude.verifies = []string{confirmF1, `{"checks":[{"id":"F1","verdict":"confirmed","reason":"still no check"}]}`}
	e.codex.reviews = []string{block,
		`{"decision":"pass","findings":[],"earlier":[{"id":"F1","ruling":"upheld","reason":"Add takes int, not byte"},{"id":"F7","ruling":"resolved","reason":"made up"}],"challenged_assumptions":[]}`,
		`{"decision":"pass","findings":[],"earlier":[{"id":"F1","ruling":"withdrawn","reason":"the caller clamps them"}],"challenged_assumptions":[]}`}
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if st := e.item(t, 1).Stage; st != state.AwaitingHuman {
		t.Fatalf("stage %s", st)
	}
	// Upheld, it blocked round 2 with its id, and the verifier checked it again.
	if n := len(e.claude.prompts["implement"]); n != 3 {
		t.Fatalf("%d builds", n)
	}
	if p := e.claude.prompts["implement"][2]; !strings.Contains(p, "F1 (major) add.go:3") || !strings.Contains(p, "Raised in an earlier round and upheld") || !strings.Contains(p, "Add takes int, not byte") {
		t.Fatalf("round 3 prompt:\n%s", p)
	}
	if p := e.claude.prompts["verify"][1]; !strings.Contains(p, "F1 (major)") || !strings.Contains(p, "still there") {
		t.Fatalf("second verify:\n%s", p)
	}
	if p := e.codex.prompts["review"][1]; !strings.Contains(p, "Builder's answer: wrong: inputs are bytes") {
		t.Fatalf("review 2 prompt:\n%s", p)
	}
	fs := e.findings(t, 1)
	if len(fs) != 1 || fs[0].Ruling != "withdrawn" || fs[0].Outcome != store.OutcomeWithdrawn || fs[0].Answer != "wrong" {
		t.Fatalf("findings %+v", fs)
	}
	// A ruling on a finding it wasn't shown is dropped.
	rv := e.jobsOf(t, 1, "review")
	var v verdict.Review
	verdict.Parse([]byte(rv[1].Verdict), &v)
	if len(v.Earlier) != 1 || v.Decision != "block" || len(v.Findings) != 1 || !v.Findings[0].Carried || v.Findings[0].ID != "F1" {
		t.Fatalf("round 2 verdict %+v", v)
	}
}

// When the verifier can't run, the finding blocks: it fails safe.
func TestVerifierFailsSafe(t *testing.T) {
	ctx := context.Background()
	t.Run("error", func(t *testing.T) {
		e := setup(t)
		e.addIssue(1)
		e.claude.builds = []func(string) error{goodBuild, fixer()}
		e.codex.reviews = []string{block, pass}
		// No verifier verdict scripted: its run fails.
		e.p.Drain(ctx)
		if n := len(e.claude.prompts["implement"]); n != 2 {
			t.Fatalf("the unverified finding didn't block: %d builds", n)
		}
		fs := e.findings(t, 1)
		if len(fs) != 1 || fs[0].Check != verdict.Unverified || !strings.Contains(fs[0].CheckReason, "the verifier failed") || fs[0].Outcome != store.OutcomeOpen {
			t.Fatalf("findings %+v", fs)
		}
		if vs := e.jobsOf(t, 1, store.KindVerify); len(vs) != 1 || vs[0].State != state.Failed {
			t.Fatalf("verify jobs %+v", vs)
		}
	})
	t.Run("budget", func(t *testing.T) {
		e := setup(t)
		e.addIssue(1)
		e.claude.builds = []func(string) error{goodBuild, fixer()}
		e.codex.reviews = []string{block, pass}
		e.claude.verifies = []string{refuteF1}
		// Today's spend leaves less than a verifier's cap.
		e.p.Cfg.Budgets.DailyTotal = 0.65
		e.p.Drain(ctx)
		if e.claude.calls["verify"] != 0 {
			t.Fatal("the verifier ran over the daily budget")
		}
		fs := e.findings(t, 1)
		if len(fs) != 1 || fs[0].Check != verdict.Unverified || !strings.Contains(fs[0].CheckReason, "daily budget") {
			t.Fatalf("findings %+v", fs)
		}
	})
	t.Run("missing ruling", func(t *testing.T) {
		e := setup(t)
		e.addIssue(1)
		e.claude.builds = []func(string) error{goodBuild, fixer()}
		e.codex.reviews = []string{block, pass}
		e.claude.verifies = []string{`{"checks":[{"id":"F9","verdict":"refuted","reason":"not it"}]}`}
		e.p.Drain(ctx)
		if fs := e.findings(t, 1); len(fs) != 1 || fs[0].Check != verdict.Unverified || len(e.claude.prompts["implement"]) != 2 {
			t.Fatalf("findings %+v", fs)
		}
	})
	t.Run("off", func(t *testing.T) {
		e := setup(t)
		e.addIssue(1)
		e.claude.builds = []func(string) error{goodBuild, fixer()}
		e.codex.reviews = []string{block, pass}
		off := false
		e.p.Cfg.Reviews.Verify = &off
		e.p.Drain(ctx)
		if e.claude.calls["verify"] != 0 || len(e.claude.prompts["implement"]) != 2 {
			t.Fatal("verify off: the block must stand, unchecked")
		}
	})
}

// A pre-existing finding never blocks, and isn't sent to the verifier.
func TestPreExistingFindingNeverBlocks(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	e.addIssue(1)
	e.claude.builds = []func(string) error{goodBuild}
	e.codex.reviews = []string{`{"decision":"block","findings":[{"severity":"pre-existing","file":"doc.go","line":1,"claim":"the package has no doc comment","evidence":"doc.go:1"}],"challenged_assumptions":[]}`}
	e.p.Drain(ctx)
	if st := e.item(t, 1).Stage; st != state.AwaitingHuman || e.claude.calls["verify"] != 0 {
		t.Fatalf("stage %s, %d verifies", st, e.claude.calls["verify"])
	}
	if fs := e.findings(t, 1); len(fs) != 1 || fs[0].Outcome != store.OutcomePreExisting || fs[0].Blocking {
		t.Fatalf("findings %+v", fs)
	}
	if !strings.Contains(e.gh.bodies[e.item(t, 1).PRNumber], "the package has no doc comment") {
		t.Fatal("the PR doesn't carry the note")
	}
}

// A big change is listed by file in a re-review, not repeated.
func TestReReviewListsFilesOfABigChange(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	e.addIssue(1)
	e.p.Cfg.Reviews.ContextBytes = 10
	e.claude.builds = []func(string) error{goodBuild, fixer()}
	e.claude.verifies = []string{confirmF1}
	e.codex.reviews = []string{block, pass}
	e.p.Drain(ctx)
	re := e.codex.prompts["review"][1]
	if !strings.Contains(re, "## Files the whole change touches") || !strings.Contains(re, "add_test.go") || strings.Contains(re, "## The whole change, for context") {
		t.Fatalf("re-review prompt:\n%s", re)
	}
	// Turned off, a re-review reads the whole diff again.
	e2 := setup(t)
	e2.addIssue(1)
	off := false
	e2.p.Cfg.Reviews.Incremental = &off
	e2.claude.builds = []func(string) error{goodBuild, fixer()}
	e2.claude.verifies = []string{confirmF1}
	e2.codex.reviews = []string{block, pass}
	e2.p.Drain(ctx)
	if re := e2.codex.prompts["review"][1]; strings.Contains(re, "Changes since your last review") || !strings.Contains(re, "add_test.go") {
		t.Fatalf("full re-review prompt:\n%s", re)
	}
}

// A paired high-risk review runs the critic beside the reviewer and
// blocks on what either raises that the verifier confirms.
func TestPairedHighRiskReview(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	e.addIssue(1)
	e.p.Cfg.Reviews.PairedHighRisk = true
	e.claude.triage = `{"decision":"ready","complexity":"M","risk":"high","question":"","summary":"add Add"}`
	e.claude.builds = []func(string) error{goodBuild, fixer()}
	e.codex.reviews = []string{pass, `{"decision":"pass","findings":[],"earlier":[{"id":"F1","ruling":"resolved","reason":"fixed"}],"challenged_assumptions":[]}`}
	e.claude.critics = []string{block, pass}
	e.claude.implVerdict = answerF1
	e.claude.verifies = []string{confirmF1}
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if st := e.item(t, 1).Stage; st != state.AwaitingHuman {
		t.Fatalf("stage %s", st)
	}
	if e.claude.calls["critic"] != 0 || e.claude.calls["parallel_critic"] != 2 || len(e.claude.prompts["implement"]) != 2 {
		t.Fatalf("calls %v", e.claude.calls)
	}
	fs := e.findings(t, 1)
	if len(fs) != 1 || fs[0].Kind != "critic" || fs[0].Route != "critic" || fs[0].Model != "opus" || fs[0].Outcome != store.OutcomeFixed {
		t.Fatalf("findings %+v", fs)
	}
	var v verdict.Review
	verdict.Parse([]byte(e.jobsOf(t, 1, "review")[1].Verdict), &v)
	if len(v.Reviewers) != 2 {
		t.Fatalf("verdict %+v", v)
	}
	// Its blocks count once per round, not once per reviewer.
	if n, _ := e.p.countBlocks(ctx, e.item(t, 1).ID); n != 1 {
		t.Fatalf("%d blocks", n)
	}
}

// Cancelling a verifier cancels the review it runs in.
func TestSubJobsAreHandledThroughTheirParent(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	e.addIssue(1)
	it, _, _ := e.p.Store.UpsertItem(ctx, "o/r", 1, "t")
	parent, _ := e.p.Store.EnqueueJob(ctx, store.Job{ItemID: &it.ID, Kind: "review", Harness: "codex", Model: "gpt-test", BudgetUSD: 1})
	e.p.Store.ClaimJob(ctx)
	sub, _ := e.p.Store.StartSubJob(ctx, parent, store.Job{ItemID: &it.ID, Kind: store.KindVerify, Harness: "claude", Model: "haiku", BudgetUSD: 1})
	if a, _ := e.p.activeJob(ctx, it.ID); a == nil || a.ID != parent {
		t.Fatalf("active job %+v", a)
	}
	if err := e.p.Cancel(ctx, sub.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.p.cancelled.Load(parent); !ok {
		t.Fatal("the parent review wasn't cancelled")
	}
}
