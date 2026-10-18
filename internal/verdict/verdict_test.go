package verdict

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestReviewParse(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		raw    string
		ok     bool
		blocks bool
	}{
		{"pass", `{"decision":"pass","findings":[],"challenged_assumptions":[]}`, true, false},
		{"block", `{"decision":"block","findings":[{"severity":"major","file":"a.go","line":3,"claim":"x","evidence":"y"}],"challenged_assumptions":[]}`, true, true},
		{"critical overrides pass", `{"decision":"pass","findings":[{"severity":"critical","file":"a.go","line":1,"claim":"x","evidence":"y"}],"challenged_assumptions":[]}`, true, true},
		{"fenced", "Here you go:\n```json\n{\"decision\":\"pass\",\"findings\":[],\"challenged_assumptions\":[]}\n```", true, false},
		{"bad decision", `{"decision":"lgtm","findings":[],"challenged_assumptions":[]}`, false, false},
		{"missing findings", `{"decision":"pass"}`, false, false},
		{"unknown field", `{"decision":"pass","findings":[],"challenged_assumptions":[],"approve":true}`, false, false},
		{"empty", ``, false, false},
		{"prose", `looks good to me`, false, false},
		{"bad severity", `{"decision":"block","findings":[{"severity":"high","file":"","line":0,"claim":"x","evidence":""}],"challenged_assumptions":[]}`, false, false},
	}
	for _, tc := range cases {
		var r Review
		err := Parse([]byte(tc.raw), &r)
		if (err == nil) != tc.ok {
			t.Errorf("%s: err=%v want ok=%v", tc.name, err, tc.ok)
			continue
		}
		if tc.ok && r.Blocks() != tc.blocks {
			t.Errorf("%s: Blocks()=%v", tc.name, r.Blocks())
		}
	}
}

func TestTriageParkNeedsQuestion(t *testing.T) {
	t.Parallel()
	var tr Triage
	if err := Parse([]byte(`{"decision":"park","complexity":"S","risk":"low","question":"","summary":""}`), &tr); err == nil {
		t.Fatal("park without question must fail")
	}
	if err := Parse([]byte(`{"decision":"ready","complexity":"M","risk":"low","question":"","summary":"ok"}`), &tr); err != nil {
		t.Fatal(err)
	}
}

func TestLoadPrefersFileThenFinal(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := filepath.Join(dir, "verdict.json")
	var r Review
	if _, err := Load(p, `{"decision":"pass","findings":[],"challenged_assumptions":[]}`, &r); err != nil {
		t.Fatalf("fallback to final message: %v", err)
	}
	os.WriteFile(p, []byte(`{"decision":"block","findings":[],"challenged_assumptions":[]}`), 0o644)
	if _, err := Load(p, `{"decision":"pass","findings":[],"challenged_assumptions":[]}`, &r); err != nil || r.Decision != "block" {
		t.Fatalf("file should win: %v %s", err, r.Decision)
	}
	if _, err := Load(filepath.Join(dir, "nope.json"), "", &r); err == nil {
		t.Fatal("no file and no final message must fail")
	}
}

func TestSchemasAreJSON(t *testing.T) {
	t.Parallel()
	for k, s := range schemas {
		var v map[string]any
		if err := json.Unmarshal([]byte(s), &v); err != nil {
			t.Errorf("%s schema: %v", k, err)
		}
	}
}

func TestPreExistingNeverBlocks(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{
		`{"decision":"block","findings":[{"severity":"pre-existing","file":"a.go","line":1,"claim":"old bug","evidence":"y"}],"challenged_assumptions":[]}`,
		`{"decision":"pass","findings":[{"severity":"pre-existing","file":"a.go","line":1,"claim":"old bug","evidence":"y"}],"challenged_assumptions":[]}`,
	} {
		r, err := ParseReview([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		if r.Blocks() || len(r.Blocking()) != 0 {
			t.Fatalf("a pre-existing finding blocked: %s", raw)
		}
	}
	// Next to a real one, only the real one blocks.
	r, _ := ParseReview([]byte(`{"decision":"block","findings":[{"severity":"pre-existing","file":"a.go","line":1,"claim":"old","evidence":""},{"severity":"major","file":"b.go","line":2,"claim":"new","evidence":""}],"challenged_assumptions":[]}`))
	if !r.Blocks() || len(r.Blocking()) != 1 || r.Blocking()[0] != 1 || r.BlocksOn(0) {
		t.Fatalf("blocking %v", r.Blocking())
	}
}

func TestBlockingFindings(t *testing.T) {
	t.Parallel()
	f := func(sev string) Finding { return Finding{Severity: sev, Claim: "c"} }
	cases := []struct {
		name     string
		r        Review
		blocking []int
	}{
		{"block: critical and major", Review{Decision: "block", Findings: []Finding{f(Minor), f(Major), f(Critical)}}, []int{1, 2}},
		{"block on minors only", Review{Decision: "block", Findings: []Finding{f(Minor), f(PreExisting), f(Minor)}}, []int{0, 2}},
		{"pass: only critical", Review{Decision: "pass", Findings: []Finding{f(Major), f(Critical)}}, []int{1}},
		{"pass: nothing", Review{Decision: "pass", Findings: []Finding{f(Major), f(Minor)}}, nil},
	}
	for _, tc := range cases {
		got := tc.r.Blocking()
		if len(got) != len(tc.blocking) {
			t.Errorf("%s: %v want %v", tc.name, got, tc.blocking)
			continue
		}
		for i := range got {
			if got[i] != tc.blocking[i] {
				t.Errorf("%s: %v want %v", tc.name, got, tc.blocking)
			}
		}
	}
}

// Only a refuted finding stops blocking: one the verifier confirmed, or
// couldn't check, still blocks.
func TestVerifierRulingsDecideTheBlock(t *testing.T) {
	t.Parallel()
	r := Review{Decision: "block", Verified: true, Findings: []Finding{{Severity: Major, Claim: "a", Check: &Check{Status: Refuted, Reason: "handled"}}}}
	if r.Blocks() {
		t.Fatal("a refuted finding blocked")
	}
	r.Findings = append(r.Findings, Finding{Severity: Critical, Claim: "b", Check: &Check{Status: Unverified, Reason: "budget"}})
	if !r.Blocks() || r.BlocksOn(0) || !r.BlocksOn(1) {
		t.Fatal("an unverified finding must block")
	}
	r.Findings[1].Check.Status = Confirmed
	if !r.Blocks() {
		t.Fatal("a confirmed finding must block")
	}
	// A block with nothing to check blocks as it always did.
	if !(&Review{Decision: "block", Findings: []Finding{}, Verified: true}).Blocks() {
		t.Fatal("an empty block must block")
	}
	b, _ := json.Marshal(r)
	var back Review
	if err := Parse(b, &back); err != nil || !back.Blocks() || back.Findings[0].Check.Status != Refuted {
		t.Fatalf("stored verdict: %v %+v", err, back)
	}
}

// A reviewer can't set what only the daemon sets.
func TestParseReviewRefusesDaemonFields(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{
		`{"decision":"block","findings":[{"severity":"major","file":"","line":0,"claim":"x","evidence":"","check":{"status":"refuted","reason":"r"}}],"challenged_assumptions":[]}`,
		`{"decision":"block","findings":[{"severity":"major","file":"","line":0,"claim":"x","evidence":"","id":"F9"}],"challenged_assumptions":[]}`,
		`{"decision":"pass","findings":[],"challenged_assumptions":[],"verified":true}`,
		`{"decision":"pass","findings":[],"challenged_assumptions":[],"reviewers":["critic"]}`,
		`{"decision":"pass","findings":[],"earlier":[{"id":"F1","ruling":"forgotten","reason":""}],"challenged_assumptions":[]}`,
	} {
		if _, err := ParseReview([]byte(raw)); err == nil {
			t.Errorf("accepted %s", raw)
		}
	}
	r, err := ParseReview([]byte(`{"decision":"pass","findings":[],"earlier":[{"id":"F1","ruling":"resolved","reason":"fixed in add.go"}],"challenged_assumptions":[]}`))
	if err != nil || len(r.Earlier) != 1 {
		t.Fatalf("%v %+v", err, r)
	}
}

func TestVerifyAndAnswersParse(t *testing.T) {
	t.Parallel()
	var v Verify
	if err := Parse([]byte(`{"checks":[{"id":"F1","verdict":"refuted","reason":"the nil check is on line 3"}]}`), &v); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{`{"checks":[{"id":"F1","verdict":"maybe","reason":"r"}]}`, `{"checks":[{"id":"F1","verdict":"confirmed","reason":""}]}`, `{}`} {
		if err := Parse([]byte(raw), &Verify{}); err == nil {
			t.Errorf("accepted %s", raw)
		}
	}
	var im Implement
	if err := Parse([]byte(`{"status":"done","summary":"s","tests_added":[],"blocker":"","answers":[{"id":"F1","outcome":"wrong","why":"Add can't overflow: inputs are bytes"}]}`), &im); err != nil || im.Answers[0].Outcome != "wrong" {
		t.Fatalf("%v %+v", err, im)
	}
	if err := Parse([]byte(`{"status":"done","summary":"s","tests_added":[],"blocker":"","answers":[{"id":"F1","outcome":"ignored","why":""}]}`), &Implement{}); err == nil {
		t.Fatal("accepted a bad outcome")
	}
}

// Which findings block is judged when each is raised, against its own
// reviewer's decision, and carried findings always block: a decision the
// daemon changes later doesn't move it.
func TestBlockingIsMarkedWhenRaised(t *testing.T) {
	t.Parallel()
	r, err := ParseReview([]byte(`{"decision":"pass","findings":[{"severity":"major","file":"a.go","line":1,"claim":"note","evidence":"e"}],"challenged_assumptions":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if r.Findings[0].Blocks == nil || *r.Findings[0].Blocks {
		t.Fatalf("a major finding under a pass is marked %v", r.Findings[0].Blocks)
	}
	// An upheld minor finding carried into the round turns it into a block;
	// the reviewer's major note still doesn't block, and the carried one does.
	yes := true
	r.Findings = append(r.Findings, Finding{Severity: Minor, File: "b.go", Claim: "upheld", ID: "F1", Carried: true, Blocks: &yes})
	r.Decision = "block"
	if b := r.Blocking(); len(b) != 1 || b[0] != 1 {
		t.Fatalf("blocking %v", b)
	}
	// Refuting the major finding can't hide the upheld minor one.
	r.Findings[0].Check = &Check{Status: Refuted}
	if !r.Blocks() {
		t.Fatal("an upheld finding didn't block")
	}
	// A model can't mark its own findings.
	if _, err := ParseReview([]byte(`{"decision":"block","findings":[{"severity":"minor","file":"a.go","line":1,"claim":"c","evidence":"e","blocks":false}],"challenged_assumptions":[]}`)); err == nil {
		t.Fatal("a reviewer set blocks")
	}
	// A verdict stored before the marks falls back to the rule.
	var old Review
	if err := Parse([]byte(`{"decision":"block","findings":[{"severity":"major","file":"a.go","line":1,"claim":"c","evidence":"e"},{"severity":"minor","file":"a.go","line":2,"claim":"m","evidence":"e"}],"challenged_assumptions":[]}`), &old); err != nil {
		t.Fatal(err)
	}
	if b := old.Blocking(); len(b) != 1 || b[0] != 0 {
		t.Fatalf("unmarked blocking %v", b)
	}
}

// A block without a finding is recorded when it's read, so findings added
// later (carried from an earlier round, or a paired reviewer's) can't
// hide it.
func TestABlockWithoutFindingsIsKept(t *testing.T) {
	t.Parallel()
	r, err := ParseReview([]byte(`{"decision":"block","findings":[],"challenged_assumptions":["it's untested"]}`))
	if err != nil || !r.BlockedWithoutFinding {
		t.Fatalf("%v %+v", err, r)
	}
	yes := true
	r.Findings = append(r.Findings, Finding{Severity: Major, File: "a.go", Claim: "upheld", ID: "F1", Carried: true, Blocks: &yes, Check: &Check{Status: Refuted}})
	if !r.Blocks() {
		t.Fatal("refuting a carried finding lifted a block that had none")
	}
	if _, err := ParseReview([]byte(`{"decision":"pass","findings":[],"challenged_assumptions":[],"blocked_without_finding":true}`)); err == nil {
		t.Fatal("a reviewer set blocked_without_finding")
	}
	if r, _ := ParseReview([]byte(`{"decision":"pass","findings":[],"challenged_assumptions":[]}`)); r.BlockedWithoutFinding {
		t.Fatal("a pass marked as a block")
	}
}
