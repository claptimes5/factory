package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func repoYAML(t *testing.T) ([]byte, string) {
	t.Helper()
	b, err := os.ReadFile("../../factory.yaml")
	if err != nil {
		t.Fatal(err)
	}
	dir, _ := filepath.Abs("../..")
	return b, dir
}

func TestEditKeepsTheRestOfTheFile(t *testing.T) {
	src, dir := repoYAML(t)
	changes := []Change{
		{Path: []string{"budgets", "daily_total"}, Value: 80.0},
		{Path: []string{"budgets", "pause_at_usage"}, Value: 90.0},                    // a new key in a block mapping
		{Path: []string{"budgets", "per_job", "XS"}, Value: 0.25},                     // in a flow mapping
		{Path: []string{"routing", "implement.S", "model"}, Value: "sonnet"},          // a dotted route
		{Path: []string{"routing", "implement.M", "effort"}, Value: "high"},           // a new key in a flow mapping
		{Path: []string{"routing", "review.high_risk", "effort"}, Value: nil},         // removed: back to the default
		{Path: []string{"escalation", "implement"}, Value: []any{"sonnet", "opus"}},   // a list
		{Path: []string{"orchestrator", "run_budget"}, Value: 12.5},                   // before a comment
		{Path: []string{"intake", "poll_interval"}, Value: nil},                       // a whole line
		{Path: []string{"auto_merge", "up_to"}, Value: "low"},                         // a new section
		{Path: []string{"auto_merge", "method"}, Value: "squash"},                     // and a key in it
		{Path: []string{"routing", "review.medium_risk", "effort"}, Value: nil},       // nothing to remove
		{Path: []string{"orchestrator", "interval"}, Value: "30m"},                    // a duration
		{Path: []string{"budgets", "concurrent_jobs"}, Value: 4.0},                    // JSON numbers are floats
		{Path: []string{"routing", "orchestrate", "harness"}, Value: "claude"},        // the same value
		{Path: []string{"routing", "triage", "fast"}, Value: nil},                     // absent already
		{Path: []string{"routing", "implement.L", "fast"}, Value: true},               // a bool
		{Path: []string{"budgets", "fast_multiplier"}, Value: 1.5},                    // another new key
		{Path: []string{"orchestrator", "daily_issue_cap"}, Value: 3.0},               // plain integer
		{Path: []string{"escalation", "max_attempts_per_tier"}, Value: 3.0},           // after a list
		{Path: []string{"routing", "implement.XS", "model"}, Value: "claude-haiku-5"}, // keeps its comment
	}
	// Each change is spliced into the text, not written out from the tree.
	cur := src
	for _, c := range changes {
		want, err := expected(cur, c)
		if err != nil {
			t.Fatal(err)
		}
		next, err := splice(cur, c)
		if err != nil || !sameDoc(next, want) {
			t.Fatalf("splice %v: %v\n%s", c.Path, err, next)
		}
		cur = next
	}
	out, err := Edit(src, changes)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != string(cur) {
		t.Fatal("Edit and splice differ")
	}
	c, err := ParseIn(out, dir)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if c.Budgets.DailyTotal != 80 || c.Budgets.PauseAtUsage != 90 || c.Budgets.PerJob["XS"] != 0.25 || c.Budgets.PerJob["L"] != 15 ||
		c.Routing["implement.S"].Model != "sonnet" || c.Routing["implement.M"].Effort != "high" || c.Routing["review.high_risk"].Effort != "" ||
		strings.Join(c.Escalation.Implement, ",") != "sonnet,opus" || c.Orchestrator.RunBudget != 12.5 || c.Intake.PollInterval.Minutes() != 1 ||
		c.AutoMerge == nil || c.AutoMerge.UpTo != "low" || c.AutoMerge.Method != "squash" || c.Orchestrator.Interval.Minutes() != 30 ||
		c.Budgets.ConcurrentJobs != 4 || !c.Routing["implement.L"].Fast || c.Escalation.MaxAttemptsPerTier != 3 {
		t.Fatalf("config = %+v", c)
	}
	// Every comment survives, and every line nothing changed is as it was.
	lines := strings.Split(string(out), "\n")
	have := map[string]bool{}
	for _, l := range lines {
		have[l] = true
	}
	touched := []string{"daily_total:", "per_job:", "implement.S:", "implement.M:", "review.high_risk:", "implement: [", "run_budget:",
		"poll_interval:", "interval:", "concurrent_jobs:", "implement.L:", "daily_issue_cap:", "max_attempts_per_tier:", "implement.XS:"}
	for _, l := range strings.Split(string(src), "\n") {
		if i := strings.Index(l, "#"); i >= 0 && !strings.Contains(string(out), l[i:]) {
			t.Errorf("comment lost: %q", l[i:])
		}
		skip := false
		for _, k := range touched {
			skip = skip || strings.Contains(l, k)
		}
		if !skip && !have[l] {
			t.Errorf("line changed: %q", l)
		}
	}
	for _, want := range []string{
		"  run_budget: 12.5            # dollars per run", // the comment stays in its column
		"  implement.XS:      {harness: claude, model: claude-haiku-5}   # text only: copy, docs, config values",
		"  implement.M:       {harness: claude, model: sonnet, effort: high}",
		"  review.high_risk:  {harness: codex,  model: gpt-6.1-sol}",
		"  implement: [sonnet, opus]",
		"  per_job: {XS: 0.25, S: 2, M: 6, L: 15}",
		"  concurrent_jobs: 4\n",
		"\nauto_merge:\n  up_to: low\n  method: squash\n",
	} {
		if !strings.Contains(string(out), want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(string(out), "poll_interval") {
		t.Error("poll_interval was not removed")
	}
}

func TestEditWithNoChangeParsesTheSame(t *testing.T) {
	src, dir := repoYAML(t)
	before, err := ParseIn(src, dir)
	if err != nil {
		t.Fatal(err)
	}
	out, err := Edit(src, nil)
	if err != nil || string(out) != string(src) {
		t.Fatalf("no changes changed the file: %v", err)
	}
	// Setting every value to what it already is keeps the file and the config.
	same := []Change{
		{Path: []string{"budgets", "daily_total"}, Value: 60.0},
		{Path: []string{"budgets", "per_job", "M"}, Value: 6.0},
		{Path: []string{"routing", "review", "model"}, Value: "gpt-6.1-sol"},
		{Path: []string{"escalation", "implement"}, Value: []any{"haiku", "sonnet", "opus"}},
		{Path: []string{"orchestrator", "enabled"}, Value: true},
		{Path: []string{"intake", "poll_interval"}, Value: "60s"},
	}
	out, err = Edit(src, same)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != string(src) {
		t.Errorf("setting values to themselves changed the text:\n%s", out)
	}
	after, err := ParseIn(out, dir)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("config changed:\n%+v\n%+v", before, after)
	}
}

func TestEditRemovesEmptiedMappings(t *testing.T) {
	src := []byte("repo: o/r\nauto_merge: {up_to: low}   # on\nbudgets:\n  per_job:\n    S: 2\n  review: 3\n")
	out, err := Edit(src, []Change{{Path: []string{"auto_merge", "up_to"}, Value: nil}, {Path: []string{"budgets", "per_job", "S"}, Value: nil}})
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "repo: o/r\nbudgets:\n  review: 3\n" {
		t.Fatalf("got:\n%s", out)
	}
	// The last key of a flow mapping takes the comma before it.
	out, err = Edit([]byte("r: {a: 1, b: 2}\n"), []Change{{Path: []string{"r", "b"}, Value: nil}})
	if err != nil || string(out) != "r: {a: 1}\n" {
		t.Fatalf("got %q, %v", out, err)
	}
	out, err = Edit([]byte("r: {a: 1, b: 2}\n"), []Change{{Path: []string{"r", "a"}, Value: nil}})
	if err != nil || string(out) != "r: {b: 2}\n" {
		t.Fatalf("got %q, %v", out, err)
	}
}

func TestEditFallsBackToTheTree(t *testing.T) {
	// A block scalar and an empty mapping key are shapes the splice leaves
	// alone; the tree edit still makes the change and keeps comments.
	src := []byte("# top\nnote: |\n  two\n  lines\nauto_merge:\nlist:\n  - a\n  - b   # b\n")
	out, err := Edit(src, []Change{
		{Path: []string{"note"}, Value: "one"},
		{Path: []string{"auto_merge", "up_to"}, Value: "low"},
		{Path: []string{"list"}, Value: []any{"c"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# top", "note: one", "up_to: low", "list: [c]"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if _, err := Edit([]byte("repo: o/r\n"), []Change{{Path: []string{"repo", "x"}, Value: 1.0}}); err == nil {
		t.Fatal("a key under a string must be refused")
	}
	// Values a flow mapping can't hold plainly are quoted.
	out, err = Edit([]byte("r: {model: a}\n"), []Change{{Path: []string{"r", "model"}, Value: "a, b"}})
	if err != nil || string(out) != "r: {model: \"a, b\"}\n" {
		t.Fatalf("got %q, %v", out, err)
	}
}

func TestSettings(t *testing.T) {
	src, _ := repoYAML(t)
	groups, err := Settings(src)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, g := range groups {
		names = append(names, g.Name)
	}
	if strings.Join(names, ",") != "Budgets,Models,Review,Escalation,Auto-merge,Orchestrator,Tuning,Intake" {
		t.Fatalf("groups = %v", names)
	}
	get := func(p ...string) Setting {
		s, ok := FindSetting(groups, p)
		if !ok {
			t.Fatalf("no setting %v", p)
		}
		return s
	}
	if s := get("budgets", "daily_total"); s.Value != 60.0 || s.Default != nil || s.Kind != "number" {
		t.Errorf("daily_total = %+v", s)
	}
	if s := get("budgets", "pause_at_usage"); s.Value != nil || s.Default != 95 {
		t.Errorf("pause_at_usage = %+v", s)
	}
	if s := get("routing", "review.high_risk", "effort"); s.Value != "high" || s.Row != "review.high_risk" {
		t.Errorf("effort = %+v", s)
	}
	if s := get("escalation", "implement"); !reflect.DeepEqual(s.Value, []string{"haiku", "sonnet", "opus"}) {
		t.Errorf("tiers = %+v", s)
	}
	if s := get("auto_merge", "up_to"); s.Value != nil || s.Default != "none" {
		t.Errorf("up_to = %+v", s)
	}
	if s := get("orchestrator", "enabled"); s.Value != true {
		t.Errorf("enabled = %+v", s)
	}
	// One row per routing entry in the file, in its order.
	var rows []string
	for _, s := range groups[1].Settings {
		if len(rows) == 0 || rows[len(rows)-1] != s.Row {
			rows = append(rows, s.Row)
		}
	}
	if strings.Join(rows, ",") != "orchestrate,triage,implement.XS,implement.S,implement.M,implement.L,review,review.high_risk,verify" {
		t.Errorf("rows = %v", rows)
	}
	// What can run commands or open the network isn't offered.
	for _, p := range [][]string{{"runner", "kind"}, {"gates", "commands", "test"}, {"repo"}, {"routing", "review.medium_risk", "model"}, {"routing", "review", "instructions_file"}} {
		if _, ok := FindSetting(groups, p); ok {
			t.Errorf("%v must not be editable", p)
		}
	}

	for _, c := range []struct {
		s    Setting
		in   any
		want any
		err  string
	}{
		{get("budgets", "concurrent_jobs"), 4.0, 4.0, ""},
		{get("budgets", "concurrent_jobs"), 4.5, nil, "whole number"},
		{get("budgets", "daily_total"), "60", nil, "must be a number"},
		{get("auto_merge", "up_to"), "high", nil, "none, low, medium"},
		{get("auto_merge", "up_to"), "", nil, ""},
		{get("orchestrator", "interval"), "soon", nil, "invalid duration"},
		{get("orchestrator", "enabled"), false, nil, ""}, // the default: left out
		{get("routing", "triage", "fast"), true, true, ""},
		{get("routing", "triage", "model"), " opus\n4 ", nil, "one line"},
		{get("escalation", "implement"), []any{" sonnet", "", "opus"}, []any{"sonnet", "opus"}, ""},
		{get("escalation", "implement"), []any{1.0}, nil, "list of names"},
	} {
		got, err := c.s.Coerce(c.in)
		if c.err != "" {
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("%v %v: err = %v, want %q", c.s.Path, c.in, err, c.err)
			}
			continue
		}
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("%v %v = %v, %v; want %v", c.s.Path, c.in, got, err, c.want)
		}
	}
}

func TestReviewSettingsAreEditable(t *testing.T) {
	src, _ := os.ReadFile("../../factory.yaml")
	groups, err := Settings(src)
	if err != nil {
		t.Fatal(err)
	}
	s, ok := FindSetting(groups, []string{"review", "verify"})
	if !ok || s.Default != true || s.Value != nil {
		t.Fatalf("review.verify %+v", s)
	}
	if v, err := s.Coerce(false); err != nil || v != false {
		t.Fatalf("coerce %v %v", v, err)
	}
	if _, ok := FindSetting(groups, []string{"budgets", "verify"}); !ok {
		t.Fatal("budgets.verify isn't editable")
	}
}
