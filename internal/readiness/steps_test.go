package readiness

import (
	"os"
	"path/filepath"
	"testing"
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
