package pipeline

import (
	"context"
	"encoding/json"
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
	resolveF1 = `{"decision":"pass","findings":[],"earlier":[{"id":"F1","ruling":"resolved","reason":"the check is there"}],"challenged_assumptions":[]}`
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	ctx := context.Background()
	t.Run("error", func(t *testing.T) {
		e := setup(t)
		e.addIssue(1)
		e.claude.builds = []func(string) error{goodBuild, fixer()}
		e.codex.reviews = []string{block, resolveF1}
		// No verifier verdict scripted: its run fails.
		e.p.Drain(ctx)
		if n := len(e.claude.prompts["implement"]); n != 2 {
			t.Fatalf("the unverified finding didn't block: %d builds", n)
		}
		fs := e.findings(t, 1)
		if len(fs) != 1 || fs[0].Check != verdict.Unverified || !strings.Contains(fs[0].CheckReason, "the verifier failed") || fs[0].Outcome != store.OutcomeResolved {
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
		e.codex.reviews = []string{block, resolveF1}
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
		e.codex.reviews = []string{block, resolveF1}
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
		e.codex.reviews = []string{block, resolveF1}
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
	t.Parallel()
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
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	e.addIssue(1)
	e.p.Cfg.Reviews.ContextBytes = 10
	e.claude.builds = []func(string) error{goodBuild, fixer()}
	e.claude.verifies = []string{confirmF1}
	e.codex.reviews = []string{block, resolveF1}
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
	e2.codex.reviews = []string{block, resolveF1}
	e2.p.Drain(ctx)
	if re := e2.codex.prompts["review"][1]; strings.Contains(re, "Changes since your last review") || !strings.Contains(re, "add_test.go") {
		t.Fatalf("full re-review prompt:\n%s", re)
	}
}

// A paired high-risk review runs the critic beside the reviewer and
// blocks on what either raises that the verifier confirms.
func TestPairedHighRiskReview(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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

// An open finding the re-review doesn't rule on stands, as if upheld: only
// a ruling lifts a block. Of two rulings on one finding, the upheld one
// stands.
func TestAnOpenFindingWithoutARulingStillBlocks(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for name, round2 := range map[string]string{
		"no ruling":   pass,
		"two rulings": `{"decision":"pass","findings":[],"earlier":[{"id":"F1","ruling":"resolved","reason":"gone"},{"id":"F1","ruling":"upheld","reason":"still there"}],"challenged_assumptions":[]}`,
	} {
		t.Run(name, func(t *testing.T) {
			e := setup(t)
			e.addIssue(1)
			f := fixer()
			e.claude.builds = []func(string) error{goodBuild, f, f}
			e.claude.implVerdict = answerF1
			e.claude.verifies = []string{confirmF1, confirmF1}
			e.codex.reviews = []string{block, round2, `{"decision":"pass","findings":[],"earlier":[{"id":"F1","ruling":"resolved","reason":"the check is there"}],"challenged_assumptions":[]}`}
			if err := e.p.Drain(ctx); err != nil {
				t.Fatal(err)
			}
			if st := e.item(t, 1).Stage; st != state.AwaitingHuman {
				t.Fatalf("stage %s", st)
			}
			if n := len(e.claude.prompts["implement"]); n != 3 {
				t.Fatalf("F1 didn't block round 2: %d builds", n)
			}
			var v verdict.Review
			verdict.Parse([]byte(e.jobsOf(t, 1, "review")[1].Verdict), &v)
			if v.Decision != "block" || len(v.Findings) != 1 || v.Findings[0].ID != "F1" || !v.Findings[0].Carried || !v.Blocks() ||
				len(v.Earlier) != 1 || v.Earlier[0].Ruling != "upheld" {
				t.Fatalf("round 2 verdict %+v", v)
			}
			if name == "no ruling" && !strings.Contains(v.Findings[0].Evidence, "the review didn't rule on it") {
				t.Fatalf("evidence %q", v.Findings[0].Evidence)
			}
			if fs := e.findings(t, 1); len(fs) != 1 || fs[0].Ruling != "resolved" || fs[0].Outcome != store.OutcomeFixed {
				t.Fatalf("findings %+v", fs)
			}
		})
	}
}

// What blocks is judged against each reviewer's own decision when it
// raises a finding: merged with a critic's block, a reviewer's passing
// note still doesn't block, and the critic's minor finding it blocked on
// does.
func TestPairKeepsEachReviewersBlockingFindings(t *testing.T) {
	t.Parallel()
	rv, err := verdict.ParseReview([]byte(`{"decision":"pass","findings":[{"severity":"major","file":"a.go","line":1,"claim":"note","evidence":"e"}],"challenged_assumptions":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	cr, err := verdict.ParseReview([]byte(`{"decision":"block","findings":[{"severity":"minor","file":"b.go","line":2,"claim":"blocker","evidence":"e"}],"challenged_assumptions":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	v, ok := pair(rv, cr)
	if !ok || v.Decision != "block" {
		t.Fatalf("paired %+v", v)
	}
	if b := v.Blocking(); len(b) != 1 || v.Findings[b[0]].Claim != "blocker" {
		t.Fatalf("blocking %v of %+v", b, v.Findings)
	}
}

// A paired critic that can't run now (here, the daily budget can't cover
// it) isn't paired: the critic follows the reviewer as usual.
func TestAPairedCriticThatCantRunFollowsTheReview(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	e.addIssue(1)
	e.p.Cfg.Reviews.PairedHighRisk = true
	e.p.Cfg.Budgets.DailyTotal = 2 // below what's spent plus a critic's $3 cap
	e.claude.triage = `{"decision":"ready","complexity":"M","risk":"high","question":"","summary":"add Add"}`
	e.claude.builds = []func(string) error{goodBuild}
	e.codex.reviews = []string{pass}
	e.claude.reviews = []string{pass}
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if st := e.item(t, 1).Stage; st != state.AwaitingHuman {
		t.Fatalf("stage %s", st)
	}
	if e.claude.calls["parallel_critic"] != 0 || e.claude.calls["critic"] != 1 || len(e.jobsOf(t, 1, store.KindParallelCritic)) != 0 {
		t.Fatalf("calls %v", e.claude.calls)
	}
}

// A paired critic that can't sign in sends the review back to the queue
// without using a retry, as the reviewer's own harness would; the next run
// isn't paired while the critic still can't sign in.
func TestAPairedCriticSignedOutRequeuesTheReview(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	g := &gatedHarness{fakeHarness: e.claude, signOuts: 1, stage: "parallel_critic"}
	e.p.Harnesses["claude"] = g
	e.addIssue(1)
	e.p.Cfg.Reviews.PairedHighRisk = true
	e.claude.triage = `{"decision":"ready","complexity":"M","risk":"high","question":"","summary":"add Add"}`
	e.claude.builds = []func(string) error{goodBuild}
	e.codex.reviews = []string{pass, pass}
	e.claude.reviews = []string{pass}
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	var states []string
	for _, r := range e.jobsOf(t, 1, "review") {
		states = append(states, fmt.Sprintf("%d:%s", r.Attempt, r.State))
	}
	if want := "1:succeeded 1:cancelled"; strings.Join(states, " ") != want {
		t.Fatalf("reviews %s, want %s", strings.Join(states, " "), want)
	}
	pc := e.jobsOf(t, 1, store.KindParallelCritic)
	if len(pc) != 1 || pc[0].State != state.Cancelled || !strings.Contains(pc[0].Error, signedOutNote) {
		t.Fatalf("paired critic jobs %+v", pc)
	}
	g.signIn()
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if st := e.item(t, 1).Stage; st != state.AwaitingHuman || e.claude.calls["critic"] != 1 {
		t.Fatalf("stage %s, calls %v", st, e.claude.calls)
	}
}

// A paired critic that runs after the reviewer, on a harness they can't
// share, doesn't run when the reviewer failed.
func TestADeferredCriticSkipsAFailedReview(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	e.p.Harnesses["codex"] = &gatedHarness{fakeHarness: e.codex, one: true}
	e.p.Cfg.Routing["critic"] = e.p.Cfg.Routing["review"]
	e.addIssue(1)
	e.p.Cfg.Reviews.PairedHighRisk = true
	e.claude.triage = `{"decision":"ready","complexity":"M","risk":"high","question":"","summary":"add Add"}`
	e.claude.builds = []func(string) error{goodBuild}
	e.codex.critics = []string{pass, pass}
	// No review scripted: the reviewer fails, twice, and the item parks.
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if st := e.item(t, 1).Stage; st != state.Parked || e.codex.calls["review"] != maxReviewAttempts {
		t.Fatalf("stage %s, calls %v", st, e.codex.calls)
	}
	if e.codex.calls["parallel_critic"] != 0 || len(e.jobsOf(t, 1, store.KindParallelCritic)) != 0 {
		t.Fatalf("the critic ran after a failed review: %v", e.codex.calls)
	}
}

// The PR's reviewer notes are the passing review's and, on a high-risk
// item, the critic's that followed it, each under its own heading.
func TestPassNotesIncludeReviewerAndCritic(t *testing.T) {
	t.Parallel()
	note := func(claim string) string {
		return `{"decision":"pass","findings":[{"severity":"minor","file":"a.go","line":1,"claim":"` + claim + `","evidence":"e","id":"F1","blocks":false}],"challenged_assumptions":[]}`
	}
	jobs := []*store.Job{ // newest first
		{Kind: "critic", State: state.Succeeded, Verdict: note("from the critic")},
		{Kind: "review", State: state.Succeeded, Verdict: note("from the reviewer")},
		{Kind: "critic", State: state.Succeeded, Verdict: note("an older critic")},
		{Kind: "triage", State: state.Succeeded},
	}
	e := setup(t)
	got := e.p.passNotes(jobs)
	for _, want := range []string{"From the reviewer:", "from the reviewer", "From the critic:", "from the critic"} {
		if !strings.Contains(got, want) {
			t.Errorf("notes missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "an older critic") {
		t.Errorf("notes include a critic from before the review:\n%s", got)
	}
	if got := e.p.passNotes(jobs[1:]); strings.Contains(got, "From the") || !strings.Contains(got, "from the reviewer") {
		t.Errorf("a review alone:\n%s", got)
	}
}

// In a paired review, one side's block without a finding stands, even
// when the other side raised only notes; either way round.
func TestPairKeepsABlockWithoutFindings(t *testing.T) {
	t.Parallel()
	bare := `{"decision":"block","findings":[],"challenged_assumptions":["the design is wrong"]}`
	note := `{"decision":"pass","findings":[{"severity":"minor","file":"a.go","line":1,"claim":"note","evidence":"e"}],"challenged_assumptions":[]}`
	for _, order := range [][2]string{{bare, note}, {note, bare}} {
		a, err := verdict.ParseReview([]byte(order[0]))
		if err != nil {
			t.Fatal(err)
		}
		b, err := verdict.ParseReview([]byte(order[1]))
		if err != nil {
			t.Fatal(err)
		}
		v, ok := pair(a, b)
		if ok || v.Decision != "block" || !v.BlockedWithoutFinding || !v.Blocks() {
			t.Fatalf("paired %+v, ok %v", v, ok)
		}
		// It survives the round trip through the store.
		raw, _ := json.Marshal(v)
		var back verdict.Review
		if err := verdict.Parse(raw, &back); err != nil || !back.Blocks() {
			t.Fatalf("stored %v %s", err, raw)
		}
	}
}

// End to end: the reviewer blocks with no finding, the paired critic
// leaves a note, and the round goes back to the builder.
func TestPairedReviewBlockWithoutFindingsSendsItBack(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	e.addIssue(1)
	e.p.Cfg.Reviews.PairedHighRisk = true
	e.claude.triage = `{"decision":"ready","complexity":"M","risk":"high","question":"","summary":"add Add"}`
	e.claude.builds = []func(string) error{goodBuild, fixer()}
	e.codex.reviews = []string{`{"decision":"block","findings":[],"challenged_assumptions":["the design is wrong"]}`, pass}
	e.claude.critics = []string{`{"decision":"pass","findings":[{"severity":"minor","file":"a.go","line":1,"claim":"note","evidence":"e"}],"challenged_assumptions":[]}`, pass}
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(e.claude.prompts["implement"]); n != 2 {
		t.Fatalf("the block was lost: %d builds", n)
	}
	if e.claude.calls["verify"] != 0 {
		t.Fatal("a block without a finding went to the verifier")
	}
}

// Notifications raised while the claim lock is held are sent after it's
// let go: sending one can take seconds.
func TestNotesWaitForTheClaimLock(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	e.p.Cfg.Budgets.DailyTotal = 0
	var sent []string
	e.p.Notify = func(_ context.Context, m string) {
		if !e.p.claimMu.TryLock() {
			t.Errorf("%q sent under the claim lock", m)
			return
		}
		e.p.claimMu.Unlock()
		sent = append(sent, m)
	}
	if _, err := e.p.claimNext(ctx); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 1 || !strings.Contains(sent[0], "Daily budget reached") {
		t.Fatalf("sent %q", sent)
	}
}
