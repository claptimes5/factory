package readiness

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/claptimes5/factory/internal/config"
)

func TestSteps(t *testing.T) {
	state := func(r *Report) (out []string) {
		for _, s := range r.Steps() {
			out = append(out, s.Key+":"+s.State)
		}
		return out
	}
	// A failed install means the checks never ran; no tests at all shows
	// without them.
	r := (&Report{Branch: "main"})
	r.add(Finding{Level: Blocker, Area: "install", Title: "Install fails on main"})
	r.add(Finding{Level: Blocker, Area: "tests", Title: "No tests"})
	r.add(Finding{Level: Gap, Area: "install", Title: "No lockfile"})
	r.add(Finding{Level: Good, Area: "ci", Title: "CI runs on the repo"})
	r.finish()
	if got := state(r); len(got) != 3 || got[0] != "install:failing" || got[1] != "checks:unchecked" || got[2] != "tests:failing" {
		t.Fatalf("steps %v", got)
	}
	if done, total := r.Progress(); done != 0 || total != 3 {
		t.Fatalf("progress %d/%d", done, total)
	}
	if ex := r.Extras(); len(ex) != 2 || ex[0].Title != "No lockfile" || ex[1].Title != "CI runs on the repo" {
		t.Fatalf("extras %+v", ex)
	}

	// No install command: no install step. A check that couldn't run
	// leaves what comes after it unchecked.
	r = &Report{Branch: "main", Incomplete: true}
	r.add(Finding{Level: Blocker, Area: "checks", Title: "Couldn't run `test`"})
	r.finish()
	if got := state(r); len(got) != 2 || got[0] != "checks:failing" || got[1] != "tests:unchecked" {
		t.Fatalf("steps %v", got)
	}

	// All in place.
	r = &Report{Branch: "main"}
	r.add(Finding{Level: Good, Area: "install", Title: "Installs from a clean checkout"})
	r.add(Finding{Level: Good, Area: "checks", Title: "Every required check passes on main: test"})
	r.add(Finding{Level: Good, Area: "tests", Title: "3 test files, passing twice in a row"})
	r.finish()
	s := r.Steps()
	if done, total := r.Progress(); done != 3 || total != 3 || s[1].Title != "Every required check passes on main" || s[2].Passed != "3 test files, passing twice in a row" {
		t.Fatalf("progress %d/%d, steps %+v", done, total, s)
	}
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	if r, err := Load(dir, "o/r"); r != nil || err != nil {
		t.Fatalf("never checked: %v %v", r, err)
	}
	if _, err := Save(dir, &Report{Repo: "o/r", Score: 70, Findings: []Finding{{Issue: &Issue{Key: "e2e", Number: 4}}}}); err != nil {
		t.Fatal(err)
	}
	r, err := Load(dir, "o/r")
	if err != nil || r.Score != 70 || r.Findings[0].Issue.Number != 4 {
		t.Fatalf("%+v %v", r, err)
	}
	os.WriteFile(filepath.Join(dir, "o__x.json"), []byte("{"), 0o644)
	if _, err := Load(dir, "o/x"); err == nil {
		t.Fatal("a broken report must say so")
	}
}

// Work waits for setup unless it fixes a blocker, directly or as a piece
// of one; a ready, unchecked or unfinished check holds nothing.
func TestHolds(t *testing.T) {
	r := &Report{Findings: []Finding{
		{Level: Blocker, Area: "checks", Title: "lint", Issue: &Issue{Key: "check:lint", Number: 9}},
		{Level: Blocker, Area: "tests", Title: "flaky", Issue: &Issue{Key: "flaky"}}, // not filed
		{Level: Gap, Area: "feature map", Title: "map", Issue: &Issue{Key: "feature-map", Number: 11}},
	}}
	for _, c := range []struct {
		issue, parent int
		held          bool
	}{{16, 0, true}, {9, 0, false}, {14, 9, false}, {11, 0, true}, {0, 0, true}} {
		if got := r.Holds(c.issue, c.parent); got != c.held {
			t.Errorf("Holds(%d, %d) = %v", c.issue, c.parent, got)
		}
	}
	if f := r.Fixes(14, 9); f == nil || f.Issue.Key != "check:lint" {
		t.Fatalf("Fixes = %+v", f)
	}
	for _, r := range []*Report{nil, {Ready: true}, {Incomplete: true}} {
		if r.Holds(16, 0) {
			t.Errorf("%+v holds", r)
		}
	}
}

// A token that can't open merge requests is a blocker of its own, the
// first step, with what the host said and where to fix it; a host that
// couldn't be asked shows no such step.
func TestAccessStep(t *testing.T) {
	gitlab := config.Repo{Name: "gitlab.com/team/api", Host: "gitlab.com", BaseBranch: "master"}
	r := &Report{Branch: "master"}
	r.access(Input{Repo: gitlab, AccessChecked: true, Denied: []Denied{{What: "open merge requests", Says: "requires [Merge Request: Create]"}}})
	r.finish()
	steps := r.Steps()
	if len(steps) != 3 || steps[0].Key != "access" || steps[0].State != StepFailing || r.Ready {
		t.Fatalf("steps %+v, ready %v", steps, r.Ready)
	}
	f := steps[0].Blockers[0]
	if f.Title != "The factory's token can't open merge requests" || f.Issue != nil ||
		!strings.Contains(f.Detail, "Merge Request: Create") || !strings.Contains(f.Detail, "GITLAB_TOKEN") {
		t.Fatalf("finding %+v", f)
	}

	r = &Report{Branch: "master"}
	r.access(Input{Repo: gitlab, AccessChecked: true})
	r.finish()
	if s := r.Steps()[0]; s.Key != "access" || s.State != StepDone || s.Passed != "The factory's token can open merge requests, issues and labels" {
		t.Fatalf("step %+v", s)
	}
	r = &Report{Branch: "master"}
	r.access(Input{Repo: gitlab})
	r.finish()
	if s := r.Steps()[0]; s.Key == "access" {
		t.Fatalf("an unasked host has a step: %+v", s)
	}
}

// A long failure keeps its start, where the error is, and its end.
func TestExcerptKeepsTheError(t *testing.T) {
	out := "bin/rails aborted!\nActiveRecord::ConnectionNotEstablished: no database\n" + strings.Repeat("/gems/rake/task.rb:241:in 'each'\n", 400) + "Tasks: TOP => db:prepare\n"
	got := excerpt(out, 3000)
	if len(got) > 3010 || !strings.Contains(got, "ConnectionNotEstablished: no database") || !strings.HasSuffix(got, "Tasks: TOP => db:prepare\n") {
		t.Fatalf("excerpt = %q", got)
	}
}
