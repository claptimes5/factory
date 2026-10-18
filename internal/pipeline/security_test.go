package pipeline

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/claptimes5/factory/internal/config"
	"github.com/claptimes5/factory/internal/metrics"
	"github.com/claptimes5/factory/internal/state"
	"github.com/claptimes5/factory/internal/store"
	"github.com/claptimes5/factory/internal/verdict"
)

// authBuild is goodBuild plus a file the default security globs match.
func authBuild(work string) error {
	writeFile(work, "auth.go", "package fixture\n\nfunc Allowed(user string) bool { return true }\n")
	return goodBuild(work)
}

// authFixer changes auth.go again, on top of the branch.
func authFixer() func(string) error {
	round := 0
	return func(w string) error {
		round++
		writeFile(w, "auth.go", fmt.Sprintf("package fixture\n\n// Allowed checks the user (fix %d).\nfunc Allowed(user string) bool { return user == \"admin\" }\n", round))
		return commitIn(w, "fix")
	}
}

const (
	secCritical  = `{"decision":"pass","findings":[{"severity":"critical","file":"auth.go","line":3,"claim":"Allowed lets anyone in","evidence":"Allowed(\"mallory\") is true"}],"challenged_assumptions":["callers check the user first"]}`
	secResolveF1 = `{"decision":"pass","findings":[],"earlier":[{"id":"F1","ruling":"resolved","reason":"it compares the user now"}],"challenged_assumptions":[]}`
)

// The reviewer passes the change, but the security pass beside it finds
// a critical hole: the round blocks on it all the same. The finding gets
// an id, is verified, answered by the builder and ruled on by the next
// security pass, which alone sees it among its open findings.
func TestSecurityPassBlocksWhenTheReviewerPasses(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	e.addIssue(1)
	e.claude.builds = []func(string) error{authBuild, authFixer()}
	e.claude.implVerdict = answerF1
	e.codex.reviews = []string{pass, pass}
	e.claude.security = []string{secCritical, secResolveF1}
	e.claude.reviews = []string{pass, pass} // auth code is high risk: the critic reads it too
	// A verifier would refute it, but a critical security finding isn't
	// sent to one.
	e.claude.verifies = []string{`{"checks":[{"id":"F1","verdict":"refuted","reason":"callers check first"}]}`}
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	it := e.item(t, 1)
	if it.Stage != state.AwaitingHuman {
		t.Fatalf("stage %s", it.Stage)
	}
	if n := len(e.claude.prompts["implement"]); n != 2 {
		t.Fatalf("the security finding didn't block: %d builds", n)
	}
	// The pass ran on its own route, as a sub-job of each review.
	sj := e.jobsOf(t, 1, store.KindSecurity)
	if len(sj) != 2 || sj[0].Route != "security" || sj[0].Harness != "claude" || sj[0].Model != "opus" || sj[0].State != state.Succeeded {
		t.Fatalf("security jobs %+v", sj)
	}
	sp := e.claude.prompts["security"]
	for _, want := range []string{"it changes auth.go (matches auth: *auth*)", "+func Allowed", "Leave `earlier` empty"} {
		if !strings.Contains(sp[0], want) {
			t.Errorf("first security prompt missing %q:\n%s", want, sp[0])
		}
	}
	if strings.Contains(sp[0], "add_test.go (matches") {
		t.Error("a test file started the pass")
	}
	// Round two: the pass rules on its own finding; the reviewer isn't
	// asked to, though it is told the finding was raised.
	if !strings.Contains(sp[1], "Open findings (rule on each by id)") || !strings.Contains(sp[1], "F1 (critical) auth.go:3") {
		t.Fatalf("second security prompt:\n%s", sp[1])
	}
	// It is among what the reviewer is told was raised, not what it rules on.
	if rp := e.codex.prompts["review"][1]; strings.Contains(rp, "Open findings (rule on each by id)") || !strings.Contains(rp, "[open: the security review rules on it]") {
		t.Fatalf("the reviewer's view of the security finding:\n%s", rp)
	}
	// The builder was told who raised it, and that it blocks unchecked.
	if b := e.claude.prompts["implement"][1]; !strings.Contains(b, "Raised by the security review.") || !strings.Contains(b, "a critical security finding isn't sent to a verifier") {
		t.Fatalf("fix round prompt:\n%s", b)
	}
	if e.claude.calls["verify"] != 0 || len(e.jobsOf(t, 1, store.KindVerify)) != 0 {
		t.Fatalf("the verifier ruled on a security critical: %v", e.claude.calls)
	}
	fs := e.findings(t, 1)
	if len(fs) != 1 || fs[0].FID != "F1" || fs[0].Kind != store.KindSecurity || fs[0].Route != "security" || fs[0].Model != "opus" ||
		!fs[0].Blocking || fs[0].Check != verdict.Unverified || fs[0].Ruling != "resolved" || fs[0].Outcome != store.OutcomeFixed {
		t.Fatalf("findings %+v", fs)
	}
	var v verdict.Review
	verdict.Parse([]byte(e.jobsOf(t, 1, "review")[1].Verdict), &v)
	if v.Decision != "pass" || !v.Blocks() || len(v.Security) == 0 || strings.Join(v.Reviewers, ",") != "review,security" {
		t.Fatalf("round 1 verdict %+v", v)
	}
	r, err := metrics.Compute(ctx, e.p.Store)
	if err != nil {
		t.Fatal(err)
	}
	if r.SecurityPasses != 2 || r.SecurityBlocks != 1 || r.ReviewBlocks != 1 {
		t.Fatalf("metrics: %d passes, %d security blocks, %d blocks", r.SecurityPasses, r.SecurityBlocks, r.ReviewBlocks)
	}
}

// A change that touches nothing the security globs, the feature map or
// the risky-code signals name gets no pass.
func TestNoSecurityPassForAnUnrelatedChange(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	e.addIssue(1)
	e.claude.builds = []func(string) error{goodBuild}
	e.codex.reviews = []string{pass}
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if st := e.item(t, 1).Stage; st != state.AwaitingHuman {
		t.Fatalf("stage %s", st)
	}
	if e.claude.calls["security"] != 0 || len(e.jobsOf(t, 1, store.KindSecurity)) != 0 {
		t.Fatalf("calls %v", e.claude.calls)
	}
	var v verdict.Review
	verdict.Parse([]byte(e.jobsOf(t, 1, "review")[0].Verdict), &v)
	if len(v.Security) != 0 || len(v.Reviewers) != 0 {
		t.Fatalf("verdict %+v", v)
	}
}

// Turned off, a change that would get a pass doesn't.
func TestSecurityPassOff(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	off := false
	e.p.Cfg.Security.Enabled = &off
	e.addIssue(1)
	e.claude.builds = []func(string) error{authBuild}
	e.codex.reviews = []string{pass}
	e.claude.reviews = []string{pass} // the critic, on high-risk auth code
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if st := e.item(t, 1).Stage; st != state.AwaitingHuman || e.claude.calls["security"] != 0 {
		t.Fatalf("stage %s, calls %v", st, e.claude.calls)
	}
}

// What the code adds starts a pass too, in a file no glob names, and the
// pass runs on routing.security when it is set.
func TestRiskyCodeStartsASecurityPassOnItsRoute(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	e.p.Cfg.Routing["security"] = config.Route{Harness: "claude", Model: "haiku", Effort: "high"}
	e.addIssue(1)
	e.claude.builds = []func(string) error{func(w string) error {
		writeFile(w, "run.go", "package fixture\n\nimport \"os/exec\"\n\nfunc Run(name string) error { return exec.Command(\"sh\", \"-c\", name).Run() }\n")
		return goodBuild(w)
	}}
	e.codex.reviews = []string{pass}
	e.claude.security = []string{pass}
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if st := e.item(t, 1).Stage; st != state.AwaitingHuman {
		t.Fatalf("stage %s", st)
	}
	sj := e.jobsOf(t, 1, store.KindSecurity)
	if len(sj) != 1 || sj[0].Model != "haiku" || sj[0].Effort != "high" {
		t.Fatalf("security jobs %+v", sj)
	}
	if p := e.claude.prompts["security"][0]; !strings.Contains(p, "code it adds runs a command, in run.go") {
		t.Fatalf("security prompt:\n%s", p)
	}
}

// A pass that fails, says nothing usable, or can't start never lets the
// change through: the review fails, is retried once, and parks.
func TestSecurityPassFailsSafe(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for name, c := range map[string]struct {
		verdicts []string
		budget   float64
		why      string
	}{
		"error":        {why: "the security pass: no scripted security pass"},
		"unparseable":  {verdicts: []string{"looks fine to me", `{"decision":"maybe","findings":[],"challenged_assumptions":[]}`}, why: "the security pass: verdict:"},
		"daily budget": {budget: 2.5, why: "the security pass couldn't start: the daily budget"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := setup(t)
			e.addIssue(1)
			e.claude.builds = []func(string) error{authBuild}
			e.codex.reviews = []string{pass, pass}
			e.claude.security = c.verdicts
			if c.budget > 0 {
				e.p.Cfg.Budgets.DailyTotal = c.budget // below what's spent plus the pass's $3 cap
			}
			if err := e.p.Drain(ctx); err != nil {
				t.Fatal(err)
			}
			it := e.item(t, 1)
			if it.Stage != state.Parked || it.PRNumber != 0 {
				t.Fatalf("item %+v", it)
			}
			rs := e.jobsOf(t, 1, "review")
			if len(rs) != maxReviewAttempts || rs[0].State != state.Failed || !strings.Contains(rs[0].Error, c.why) {
				t.Fatalf("reviews %+v", rs)
			}
			if c.budget > 0 && e.codex.calls["review"] != 0 {
				t.Fatal("the reviewer ran though the security pass couldn't")
			}
		})
	}
}

// A pass that can't sign in sends the review back to the queue without
// using a retry. While its harness still can't sign in, the review waits
// in the queue, held as that harness's own jobs are, and runs once it can.
func TestASecurityPassSignedOut(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	g := &gatedHarness{fakeHarness: e.claude, signOuts: 1, stage: store.KindSecurity}
	e.p.Harnesses["claude"] = g
	e.addIssue(1)
	e.claude.builds = []func(string) error{authBuild}
	e.codex.reviews = []string{pass, pass}
	e.claude.reviews = []string{pass, pass, pass} // the critic, on high-risk auth code
	e.claude.security = []string{pass}
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	var states []string
	for _, r := range e.jobsOf(t, 1, "review") {
		states = append(states, fmt.Sprintf("%d:%s", r.Attempt, r.State))
	}
	if want := "1:queued 1:cancelled 1:cancelled"; strings.Join(states, " ") != want {
		t.Fatalf("reviews %s, want %s", strings.Join(states, " "), want)
	}
	rs := e.jobsOf(t, 1, "review")
	if !strings.Contains(rs[2].Error, "the security pass: ") || !strings.Contains(rs[1].Error, heldNote) || !strings.Contains(rs[0].Input, `"waits_on":"claude"`) {
		t.Fatalf("reviews %+v", rs)
	}
	if st := e.item(t, 1).Stage; st != state.Reviewing || e.codex.calls["review"] != 1 {
		t.Fatalf("stage %s, calls %v", st, e.codex.calls)
	}
	g.signIn()
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if st := e.item(t, 1).Stage; st != state.AwaitingHuman || e.claude.calls["security"] != 1 {
		t.Fatalf("stage %s, calls %v", st, e.claude.calls)
	}
}

// Joined to the reviewer's verdict, a security pass's critical finding
// blocks though both decided pass, the reviewer's passing note still
// doesn't, and each side's rulings count only on its own findings.
func TestWithSecurityKeepsEachSidesBlocks(t *testing.T) {
	t.Parallel()
	rv, err := verdict.ParseReview([]byte(`{"decision":"pass","findings":[{"severity":"major","file":"a.go","line":1,"claim":"note","evidence":"e"}],
		"earlier":[{"id":"F1","ruling":"withdrawn","reason":"not mine to judge"},{"id":"F2","ruling":"resolved","reason":"gone"}],"challenged_assumptions":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	sv, err := verdict.ParseReview([]byte(`{"decision":"pass","findings":[{"severity":"critical","file":"b.go","line":2,"claim":"injection","evidence":"e"}],
		"earlier":[{"id":"F1","ruling":"upheld","reason":"still there"},{"id":"F2","ruling":"withdrawn","reason":"x"}],"challenged_assumptions":["input is trusted"]}`))
	if err != nil {
		t.Fatal(err)
	}
	own := []store.ReviewFinding{{FID: "F1", Kind: store.KindSecurity}}
	v, ok := withSecurity(rv, sv, own, []string{"it changes b.go"})
	if !ok || v.Decision != "pass" || !v.Blocks() || len(v.Security) != 1 {
		t.Fatalf("joined %+v", v)
	}
	if b := v.Blocking(); len(b) != 1 || v.Findings[b[0]].Claim != "injection" || v.Findings[b[0]].By != store.KindSecurity {
		t.Fatalf("blocking %v of %+v", b, v.Findings)
	}
	rulings := map[string]string{}
	for _, e := range v.Earlier {
		rulings[e.ID] = e.Ruling
	}
	if len(v.Earlier) != 2 || rulings["F1"] != "upheld" || rulings["F2"] != "resolved" {
		t.Fatalf("earlier %+v", v.Earlier)
	}
	// A pass that blocks without a finding blocks, with nothing to verify.
	rv, _ = verdict.ParseReview([]byte(pass))
	sv, _ = verdict.ParseReview([]byte(`{"decision":"block","findings":[],"challenged_assumptions":["the token is never logged"]}`))
	if v, ok := withSecurity(rv, sv, nil, []string{"x"}); ok || !v.Blocks() || v.Decision != "block" {
		t.Fatalf("block without a finding: ok %v, %+v", ok, v)
	}
}

func TestSecurityPathsSkipTests(t *testing.T) {
	t.Parallel()
	c := &config.Config{}
	got := securityPaths(c.SecurityMatch, []string{"auth_test.go", "add.go", "internal/auth/login.go", "go.sum", "web/src/Login.test.tsx", "docs/auth.md"}, "api/")
	want := "it changes api/internal/auth/login.go (matches auth: *auth*), api/go.sum (matches dependencies: go.sum)"
	if len(got) != 1 || got[0] != want {
		t.Fatalf("got %q", got)
	}
	if got := securityPaths(c.SecurityMatch, []string{"add.go", "README.md", "auth_test.go"}, ""); got != nil {
		t.Fatalf("unrelated files started a pass: %q", got)
	}
}

func TestRiskyCode(t *testing.T) {
	t.Parallel()
	for line, want := range map[string]string{
		`import "os/exec"`: "runs a command",
		`cmd := exec.CommandContext(ctx, "git", args...)`:            "runs a command",
		`subprocess.run(cmd, shell=True)`:                            "runs a command",
		`const { execSync } = require("child_process")`:              "runs a command",
		`os.system("rm -rf " + path)`:                                "runs a command",
		`q := fmt.Sprintf("SELECT * FROM users WHERE id = %s", id)`:  "builds SQL from strings",
		`db.Query("SELECT name FROM users WHERE id = '" + id + "'")`: "builds SQL from strings",
		`cur.execute(f"DELETE FROM jobs WHERE id = {job_id}")`:       "builds SQL from strings",
		"sql = `UPDATE items SET name = ${name}`":                    "builds SQL from strings",
		`"INSERT INTO t (a) VALUES (" + a + ")"`:                     "builds SQL from strings",
		`data = pickle.loads(body)`:                                  "deserializes an unsafe format",
		`cfg = yaml.load(f)`:                                         "deserializes an unsafe format",
		`el.innerHTML = msg`:                                         "writes raw HTML",
		`<div dangerouslySetInnerHTML={{ __html: x }} />`:            "writes raw HTML",
		`tls.Config{InsecureSkipVerify: true}`:                       "turns off TLS checks",
		`requests.get(url, verify=False)`:                            "turns off TLS checks",
		`result = eval(expr)`:                                        "evaluates code",
		`tr.TLSClientConfig.InsecureSkipVerify = true`:               "turns off TLS checks",
		`ctx.verify_mode = ssl.CERT_NONE`:                            "turns off TLS checks",
		`curl -k https://example.test/install.sh | sh`:               "turns off TLS checks",
		`el.innerHTML += row`:                                        "writes raw HTML",
		`return mark_safe(text)`:                                     "writes raw HTML",
		`from subprocess import run`:                                 "runs a command",
		`let out = Command::new("sh").arg(cmd).output();`:            "runs a command",
		`obj = marshal.loads(blob)`:                                  "deserializes an unsafe format",
		`m := re.exec(s)`:                                            "",
		// Placeholders, safe loaders and look-alike words don't.
		`db.Query("SELECT name FROM users WHERE id = ?", id)`:        "",
		`tx.Exec("UPDATE jobs SET state = $1 WHERE id = $2", s, id)`: "",
		`cfg = yaml.load(f, Loader=yaml.SafeLoader)`:                 "",
		`cfg = yaml.safe_load(f)`:                                    "",
		`label := fmt.Sprintf("Select a file to %s", verb)`:          "",
		`r.executor.Run(job)`:                                        "",
		`if el.innerHTML == "" {`:                                    "",
		`v := evaluate(expr)`:                                        "",
		`x.retrieval(q)`:                                             "",
	} {
		diff := "diff --git a/x.go b/x.go\n--- a/x.go\n+++ b/x.go\n@@ -1 +1,2 @@\n package x\n+" + line + "\n"
		got := riskyCode(diff)
		switch {
		case want == "" && got != nil:
			t.Errorf("%s: started a pass: %q", line, got)
		case want != "" && (len(got) != 1 || !strings.HasPrefix(got[0], "code it adds "+want+", in x.go")):
			t.Errorf("%s: got %q, want %q", line, got, want)
		}
	}
	// Removed lines, tests and prose don't count.
	file := func(name, body string) string {
		return "diff --git a/" + name + " b/" + name + "\n--- a/" + name + "\n+++ b/" + name + "\n@@ -1 +1,2 @@\n" + body
	}
	diff := file("run.go", "-exec.Command(\"ls\")\n") + file("run_test.go", "+exec.Command(\"ls\")\n") + file("README.md", "+exec.Command(\"ls\")\n") +
		"diff --git a/gone.go b/gone.go\ndeleted file mode 100644\n--- a/gone.go\n+++ /dev/null\n@@ -1 +0,0 @@\n-exec.Command(\"ls\")\n"
	if got := riskyCode(diff); got != nil {
		t.Fatalf("got %q", got)
	}
	// An added line that looks like a file header doesn't rename the file
	// it is in: "+++ b/docs/x.md" in a hunk is the line "++ b/docs/x.md".
	diff = file("run.go", "+++ b/docs/x.md\n+exec.Command(\"ls\")\n+++ b/run_test.go\n+os.system(cmd)\n")
	if got := riskyCode(diff); len(got) != 1 || got[0] != "code it adds runs a command, in run.go" {
		t.Fatalf("header bypass: got %q", got)
	}
	// Ruby's backticks run a command; in a comment they don't.
	if got := riskyCode(file("app/sync.rb", "+  out = `git pull ${branch}`\n")); len(got) != 1 {
		t.Fatalf("backticks: %q", got)
	}
	if got := riskyCode(file("app/sync.rb", "+  # Call `pull` first\n")); got != nil {
		t.Fatalf("a comment: %q", got)
	}
}

func TestCheckSecurityPrompt(t *testing.T) {
	t.Parallel()
	e := setup(t)
	if err := e.p.CheckSecurityPrompt(); err != nil {
		t.Fatal(err)
	}
	e.p.Cfg.PromptsDir = t.TempDir()
	if err := e.p.CheckSecurityPrompt(); err == nil || !strings.Contains(err.Error(), "security: {enabled: false}") {
		t.Fatalf("missing prompt: %v", err)
	}
	off := false
	e.p.Cfg.Security.Enabled = &off
	if err := e.p.CheckSecurityPrompt(); err != nil {
		t.Fatal(err)
	}
}

const (
	secMajor     = `{"decision":"block","findings":[{"severity":"major","file":"auth.go","line":3,"claim":"no rate limit on Allowed","evidence":"a loop can guess users"}],"challenged_assumptions":[]}`
	secUpholdF1  = `{"decision":"pass","findings":[],"earlier":[{"id":"F1","ruling":"upheld","reason":"still unlimited at auth.go:3"}],"challenged_assumptions":[]}`
	refuteSecF1  = `{"checks":[{"id":"F1","verdict":"refuted","reason":"a proxy limits it"}]}`
	confirmSecF1 = `{"checks":[{"id":"F1","verdict":"confirmed","reason":"nothing limits it"}]}`
)

// Questions from the reviewer reach a human even when the security pass
// blocks: nothing is verified away from under them, and the park message
// carries the security finding too.
func TestReviewerQuestionsWinOverASecurityBlock(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	rv, _ := verdict.ParseReview([]byte(`{"decision":"questions","findings":[],"challenged_assumptions":["which users count?"]}`))
	sv, _ := verdict.ParseReview([]byte(secCritical))
	if v, _ := withSecurity(rv, sv, nil, []string{"x"}); v.Decision != "questions" {
		t.Fatalf("joined %+v", v)
	}
	e := setup(t)
	e.addIssue(1)
	e.claude.builds = []func(string) error{authBuild}
	e.codex.reviews = []string{`{"decision":"questions","findings":[],"challenged_assumptions":["which users count?"]}`}
	e.claude.security = []string{secMajor}
	e.claude.verifies = []string{refuteSecF1}
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	it := e.item(t, 1)
	if it.Stage != state.Parked || it.PRNumber != 0 || e.claude.calls["verify"] != 0 {
		t.Fatalf("item %+v, calls %v", it, e.claude.calls)
	}
	evs, _ := e.p.Store.Events(ctx, it.ID)
	parked := ""
	for _, ev := range evs {
		if ev.Type == "stage" && strings.Contains(ev.Reason, "questions") {
			parked = ev.Reason
		}
	}
	if !strings.Contains(parked, "no rate limit on Allowed") {
		t.Fatalf("park reason %q", parked)
	}
}

// A security pass's major finding is checked on the security route, never
// the cheap verifier's. Refuted, it doesn't block, but the PR says so and
// doesn't merge itself.
func TestARefutedSecurityFindingStopsAutoMerge(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	e.p.Cfg.Repos[0].AutoMerge = &config.AutoMerge{UpTo: "low"}
	// Auth code alone neither raises the risk nor needs a person here,
	// but still gets a pass, so only the refuted finding keeps auto-merge
	// off.
	e.p.Cfg.Sensitive, e.p.Cfg.Security.ExtraPaths, e.p.Cfg.Risk.NeedsPerson = config.Sensitive{"auth": {}}, []string{"*auth*"}, []string{}
	e.claude.triage = `{"decision":"ready","complexity":"S","risk":"low","question":"","summary":"add Add"}`
	e.addIssue(1)
	e.claude.builds = []func(string) error{authBuild}
	e.codex.reviews = []string{pass}
	e.claude.security = []string{secMajor}
	e.claude.verifies = []string{refuteSecF1}
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	it := e.item(t, 1)
	if it.Stage != state.AwaitingHuman || len(e.claude.prompts["implement"]) != 1 {
		t.Fatalf("item %+v", it)
	}
	vs := e.jobsOf(t, 1, store.KindVerify)
	if len(vs) != 1 || vs[0].Model != "opus" || vs[0].Model == e.p.Cfg.VerifyRoute().Model {
		t.Fatalf("verify jobs %+v", vs)
	}
	if _, on := e.gh.autoMerged[it.PRNumber]; on {
		t.Fatal("auto-merged past a refuted security finding")
	}
	body := e.gh.bodies[it.PRNumber]
	if !strings.Contains(body, "The security review raised findings a verifier refuted") || !strings.Contains(body, "a proxy limits it") ||
		!strings.Contains(body, "Raised by the security review.") {
		t.Fatalf("PR body:\n%s", body)
	}
}

// A security finding the pass upholds in a later round blocks again, and
// isn't sent to a verifier again.
func TestAnUpheldSecurityFindingIsntVerifiedAgain(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	e.addIssue(1)
	f := authFixer()
	e.claude.builds = []func(string) error{authBuild, f, f}
	e.claude.implVerdict = answerF1
	e.codex.reviews = []string{pass, pass, pass}
	e.claude.security = []string{secMajor, secUpholdF1, secResolveF1}
	e.claude.reviews = []string{pass, pass, pass} // the critic, on high-risk auth code
	e.claude.verifies = []string{confirmSecF1, refuteSecF1}
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if st := e.item(t, 1).Stage; st != state.AwaitingHuman || len(e.claude.prompts["implement"]) != 3 {
		t.Fatalf("stage %s, %d builds", st, len(e.claude.prompts["implement"]))
	}
	if n := e.claude.calls["verify"]; n != 1 {
		t.Fatalf("%d verifier runs", n)
	}
	var v verdict.Review
	verdict.Parse([]byte(e.jobsOf(t, 1, "review")[1].Verdict), &v)
	if len(v.Findings) != 1 || !v.Findings[0].Carried || v.Findings[0].By != store.KindSecurity || !v.Blocks() ||
		!strings.Contains(v.Findings[0].Check.Reason, "the security pass upheld it") {
		t.Fatalf("round 2 verdict %+v", v)
	}
}

// What decides the pass is the whole diff, not the part the reviewer is
// shown: risky code past the first 200 KB still starts one.
func TestSecurityPassReadsTheWholeDiff(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	e.addIssue(1)
	e.claude.builds = []func(string) error{func(w string) error {
		writeFile(w, "a_big.go", "package fixture\n\n// "+strings.Repeat("x", 250_000)+"\n")
		writeFile(w, "run.go", "package fixture\n\nimport \"os/exec\"\n\nvar _ = exec.Command\n")
		return goodBuild(w)
	}}
	e.codex.reviews = []string{pass}
	e.claude.security = []string{pass}
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if st := e.item(t, 1).Stage; st != state.AwaitingHuman || e.claude.calls["security"] != 1 {
		t.Fatalf("stage %s, calls %v", st, e.claude.calls)
	}
	if p := e.claude.prompts["security"][0]; !strings.Contains(p, "code it adds runs a command, in run.go") {
		t.Fatalf("security prompt:\n%.2000s", p)
	}
}

// In a workspace, a change to any member repo starts the pass, which names
// the file with the repo's folder.
func TestSecurityPassInAWorkspace(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	w := setupWorkspace(t)
	w.addIssue(1)
	w.claude.builds = []func(string) error{func(work string) error {
		if err := goodBuild(filepath.Join(work, "r")); err != nil {
			return err
		}
		dir := filepath.Join(work, "web")
		writeFile(dir, "client.txt", "calls Add\n")
		writeFile(dir, "package.json", "{\"dependencies\":{\"left-pad\":\"1.0.0\"}}\n")
		return commitIn(dir, "client")
	}}
	w.codex.reviews = []string{pass}
	w.claude.security = []string{pass}
	if err := w.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if st := w.item(t, 1).Stage; st != state.AwaitingHuman {
		t.Fatalf("stage %s", st)
	}
	if sp := w.claude.prompts["security"]; len(sp) != 1 || !strings.Contains(sp[0], "it changes web/package.json (matches dependencies: package.json)") {
		t.Fatalf("security prompts %q", sp)
	}
}
