package eval

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/claptimes5/factory/internal/config"
	"github.com/claptimes5/factory/internal/harness"
	"github.com/claptimes5/factory/internal/pipeline"
	"github.com/claptimes5/factory/internal/runner"
)

// prCode is what the merged PR in these tests added.
const prCode = `package fixture

import "os"

// Max is the larger of a and b.
func Max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// Read reads a file.
func Read(p string) (string, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// Sum adds up xs.
func Sum(xs []int) int {
	n := 0
	for i := 0; i < len(xs); i++ {
		n += xs[i]
	}
	return n
}
`

func allLines(n int) map[int]bool {
	m := map[int]bool{}
	for i := 1; i <= n; i++ {
		m[i] = true
	}
	return m
}

func TestPlantBug(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		mutation string
		line     int
		want     string // a line of the result
		gone     string // a line no longer there
	}{
		{"flip-comparison", 7, "\tif a <= b {", "\tif a > b {"},
		{"off-by-one", 25, "\tfor i := 0; i <= len(xs); i++ {", "\tfor i := 0; i < len(xs); i++ {"},
		{"drop-err-check", 16, "\treturn string(b), nil", "\tif err != nil {"},
	} {
		out, line, err := PlantBug(prCode, allLines(40), tc.mutation)
		if err != nil || line != tc.line {
			t.Fatalf("%s: line %d, %v", tc.mutation, line, err)
		}
		lines := strings.Split(out, "\n")
		if !strings.Contains(out, tc.want+"\n") || strings.Contains(out, tc.gone+"\n") || lines[line-1] != tc.want && tc.mutation != "drop-err-check" {
			t.Fatalf("%s:\n%s", tc.mutation, out)
		}
		if tc.mutation == "drop-err-check" && lines[line-1] != tc.want {
			t.Fatalf("drop-err-check points at %q", lines[line-1])
		}
	}
	// Only lines the change added are touched.
	if _, _, err := PlantBug(prCode, map[int]bool{1: true, 2: true}, "flip-comparison"); err == nil {
		t.Fatal("planted in a line the change didn't add")
	}
	if _, _, err := PlantBug(prCode, allLines(40), "drop-nil-check"); err == nil {
		t.Fatal("no nil check to drop, but it planted one")
	}
	src := "package x\n\nfunc F(p *T) int {\n\tif p == nil {\n\t\treturn 0\n\t}\n\treturn p.n\n}\n"
	if out, line, err := PlantBug(src, allLines(9), "drop-nil-check"); err != nil || line != 4 || strings.Contains(out, "nil") {
		t.Fatalf("drop-nil-check: %d %v\n%s", line, err, out)
	}
	if _, _, err := PlantBug(prCode, allLines(40), "nonsense"); err == nil {
		t.Fatal("an unknown mutation")
	}
	if !Plantable("a/b.go") || Plantable("a/b_test.go") || Plantable("a/b.py") {
		t.Fatal("Plantable")
	}
}

func TestAddedLines(t *testing.T) {
	t.Parallel()
	diff := "diff --git a/f.go b/f.go\n--- a/f.go\n+++ b/f.go\n@@ -1,3 +1,4 @@\n package x\n-old\n+new\n+more\n keep\n@@ -10,1 +11,2 @@\n ctx\n+late\ndiff --git a/g.go b/g.go\n--- a/g.go\n+++ /dev/null\n@@ -1 +0,0 @@\n-gone\n"
	got := AddedLines(diff)
	if len(got) != 1 || !got["f.go"][2] || !got["f.go"][3] || got["f.go"][4] || !got["f.go"][12] || len(got["f.go"]) != 3 {
		t.Fatalf("added = %v", got)
	}
}

// fakeReviewer blocks what it can see in the diff: a flipped comparison
// and an off-by-one at their lines, never a dropped error check. Its
// noisy model blocks everything, somewhere else.
type fakeReviewer struct{ name string }

func (f *fakeReviewer) Name() string { return f.name }
func (f *fakeReviewer) Run(_ context.Context, req harness.RunRequest, _ chan<- harness.Event) (*harness.Result, error) {
	res := &harness.Result{CostUSD: 0.5, CostKnown: true, Final: `{"decision":"pass","findings":[],"challenged_assumptions":[]}`}
	if req.Stage != "review" && req.Stage != "critic" {
		return res, nil
	}
	block := func(file string, line int) {
		res.Final = `{"decision":"block","findings":[{"severity":"critical","file":"` + file + `","line":` + strconv.Itoa(line) + `,"claim":"wrong","evidence":"see the diff"}],"challenged_assumptions":[]}`
	}
	switch {
	case req.Model == "gpt-broken":
		res.Final = "not a verdict"
	case req.Model == "gpt-asks":
		res.Final = `{"decision":"questions","findings":[{"severity":"major","file":"f.go","line":7,"claim":"which order?","evidence":"e"}],"challenged_assumptions":[]}`
	case req.Model == "gpt-noisy":
		block("other.go", 1)
	case strings.Contains(req.Prompt, "+\tif a <= b {"):
		block("f.go", 7)
	case strings.Contains(req.Prompt, "i <= len(xs)"):
		block("./f.go", 27)
	}
	return res, nil
}

// Reviewer cases from a merged PR and a reverted one: the planted bugs it
// sees are caught, the one it can't is missed, the clean change passes,
// and a noisier reviewer is rejected against the baseline.
func TestReviewerEvals(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	src := filepath.Join(root, "src")
	os.MkdirAll(src, 0o755)
	git(t, src, "init", "-q", "-b", "main")
	write(src, "go.mod", "module fixture\n\ngo 1.23\n")
	write(src, "f.go", "package fixture\n")
	git(t, src, "add", "-A")
	git(t, src, "commit", "-qm", "base")
	base := git(t, src, "rev-parse", "HEAD")
	write(src, "f.go", prCode)
	git(t, src, "add", "-A")
	git(t, src, "commit", "-qm", "the PR")
	head := git(t, src, "rev-parse", "HEAD")

	set := filepath.Join(root, "evals")
	g := &pipeline.Git{RemoteURL: src}
	fix := Fix{Repo: src, Source: "PR #7", Title: "Add Max, Read and Sum", Body: "Helpers.", Base: base, Head: head}
	c, err := CaptureReview(ctx, g, src, set, "pr-7", fix, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Dirs) != 4 || len(c.Notes) != 1 || !strings.Contains(c.Notes[0], "drop-nil-check") {
		t.Fatalf("captured %v, notes %v", c.Dirs, c.Notes)
	}
	// Captured again: nothing is overwritten.
	if again, err := CaptureReview(ctx, g, src, set, "pr-7", fix, false); err != nil || len(again.Dirs) != 0 {
		t.Fatalf("again: %+v %v", again, err)
	}
	// A reverted PR is a case the reviewer should have blocked.
	if r, err := CaptureReview(ctx, g, src, set, "pr-8", fix, true); err != nil || len(r.Dirs) != 1 || !strings.HasSuffix(r.Dirs[0], "pr-8-revert") {
		t.Fatalf("revert: %+v %v", r, err)
	}
	// A builder case beside them: each loader reads only its own.
	write(set, "build/case.yaml", "title: t\nbase_sha: "+base+"\naccept: true\n")
	if bc, err := Load(set); err != nil || len(bc) != 1 {
		t.Fatalf("builder cases %v %v", bc, err)
	}
	cases, err := LoadReview(set)
	if err != nil || len(cases) != 5 {
		t.Fatalf("reviewer cases %v %v", cases, err)
	}
	if cases[1].Name != "pr-7-plant-drop-err-check" || cases[1].Plant.Line != 16 || cases[1].Plant.File != "f.go" {
		t.Fatalf("planted %+v", cases[1].Plant)
	}

	run := func(model string, cases []ReviewCase, capUSD float64) (*ReviewReport, error) {
		t.Helper()
		cfg, err := config.Parse([]byte(strings.ReplaceAll(cfgYAML, "gpt-test", model) + "review: {verify: false}\n"))
		if err != nil {
			t.Fatal(err)
		}
		cfg.Budgets.MinFreeDiskGB = -1
		wd, _ := os.Getwd()
		cfg.PromptsDir = filepath.Join(wd, "..", "..", "prompts")
		parallel := 3
		if capUSD > 0 {
			parallel = 1
		}
		return RunReview(ctx, Options{
			Cfg: cfg, Runner: runner.Local{}, WorkDir: filepath.Join(root, fmt.Sprintf("work-%s-%d-%g", model, len(cases), capUSD)), Parallel: parallel, MaxCostUSD: capUSD,
			Harnesses: map[string]harness.Harness{"claude": &fakeReviewer{"claude"}, "codex": &fakeReviewer{"codex"}},
		}, cases)
	}
	rep, err := run("gpt-test", cases, 0)
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]ReviewResult{}
	for _, r := range rep.Results {
		by[r.Case] = r
	}
	if r := by["pr-7-plant-flip-comparison"]; !r.Caught || !r.Blocked {
		t.Fatalf("flip: %+v\n%s", r, rep.Table())
	}
	if r := by["pr-7-plant-off-by-one"]; !r.Caught {
		t.Fatalf("off-by-one, found at ./f.go two lines off: %+v\n%s", r, rep.Table())
	}
	if r := by["pr-7-plant-drop-err-check"]; r.Caught || r.Blocked {
		t.Fatalf("drop-err-check: %+v", r)
	}
	if r := by["pr-7-clean"]; r.Blocked || r.FalsePos || r.Stage != "awaiting_human" {
		t.Fatalf("clean: %+v\n%s", r, rep.Table())
	}
	if r := by["pr-8-revert"]; r.Blocked {
		t.Fatalf("revert: %+v", r)
	}
	if rep.Planted != 3 || rep.Caught != 2 || rep.Clean != 1 || rep.FalsePo != 0 || rep.Reverts != 1 || rep.Stopped != 0 || rep.Errors != 0 {
		t.Fatalf("report:\n%s", rep.Table())
	}
	if rep.CostUSD != 2.5 || !strings.Contains(rep.Table(), "recall 67% (2 of 3 planted) · false positives 0% (0 of 1 clean) · reverts blocked 0 of 1") {
		t.Fatalf("cost %.2f\n%s", rep.CostUSD, rep.Table())
	}

	// A reviewer that blocks everything stops the revert, catches no bug
	// at its place, and blocks the clean change: rejected.
	noisy, err := run("gpt-noisy", cases, 0)
	if err != nil || noisy.Caught != 0 || noisy.FalsePo != 1 || noisy.Stopped != 1 {
		t.Fatalf("noisy: %v\n%s", err, noisy.Table())
	}
	why := CompareReview(noisy, rep, 0.05)
	if len(why) != 3 || !strings.Contains(why[0], "recall fell from 67% to 0%") || !strings.Contains(why[1], "false-positive rate rose from 0% to 100%") ||
		!strings.Contains(why[2], "pr-7-clean ok → false-positive") || !strings.Contains(why[2], "pr-7-plant-flip-comparison caught → missed") {
		t.Fatalf("compare: %v", why)
	}
	if why := CompareReview(rep, rep, 0.05); len(why) != 0 {
		t.Fatalf("the same report: %v", why)
	}
	// Runs of different cases can't be compared.
	fewer, err := run("gpt-test", cases[:4], 0)
	if err != nil {
		t.Fatal(err)
	}
	if why := CompareReview(fewer, rep, 0.05); len(why) != 1 || !strings.Contains(why[0], "pr-8-revert only in the baseline") {
		t.Fatalf("different cases: %v", why)
	}

	// A reviewer with no valid verdict made no ruling: errors, in no
	// denominator, and worse than the baseline however its rates look.
	broken, err := run("gpt-broken", cases, 0)
	if err != nil || broken.Errors != 5 || broken.Planted != 0 || broken.Clean != 0 || broken.FalsePo != 0 || broken.Stopped != 0 {
		t.Fatalf("broken: %v\n%s", err, broken.Table())
	}
	if r := broken.Results[0]; r.Blocked || !r.Error || r.Outcome() != "error" {
		t.Fatalf("a broken verdict: %+v", r)
	}
	if why := CompareReview(broken, rep, 0.05); len(why) == 0 || !strings.Contains(why[0], "5 cases got no ruling, up from 0") {
		t.Fatalf("broken compare: %v", why)
	}
	// Questions stop the change: blocked, not an error.
	asks, err := run("gpt-asks", cases[:1], 0)
	if err != nil || asks.Errors != 0 || !asks.Results[0].Blocked || !asks.Results[0].FalsePos {
		t.Fatalf("questions: %v\n%s", err, asks.Table())
	}

	// A cap stops the run once spent, as an error.
	capped, err := run("gpt-test", cases, 0.75)
	if err == nil || !strings.Contains(err.Error(), "$0.75 cap") || capped.Errors != 3 || capped.CostUSD != 1 {
		t.Fatalf("capped: %v\n%s", err, capped.Table())
	}
}

func TestSampleTakesEveryKind(t *testing.T) {
	t.Parallel()
	var cases []ReviewCase
	for _, n := range []string{"a1", "a2", "a3", "a4", "a5"} {
		cases = append(cases, ReviewCase{Name: n, Kind: KindPlanted})
	}
	cases = append(cases, ReviewCase{Name: "b1", Kind: KindClean}, ReviewCase{Name: "b2", Kind: KindClean}, ReviewCase{Name: "c1", Kind: KindRevert})
	var names []string
	for _, c := range Sample(cases, 5) {
		names = append(names, c.Name)
	}
	if strings.Join(names, ",") != "a1,a2,b1,b2,c1" {
		t.Fatalf("sample = %v", names)
	}
	if got := Sample(cases, 20); len(got) != len(cases) {
		t.Fatalf("all fit: %d", len(got))
	}
}
