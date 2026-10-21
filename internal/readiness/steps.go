package readiness

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Step states.
const (
	StepDone      = "done"
	StepFailing   = "failing"
	StepUnchecked = "unchecked" // an earlier step failed, or the check stopped first
)

// Step is one thing a person must get right before the factory can work
// on a repo: the blockers only they can fix (a token, a service, a file
// git doesn't track), grouped the way they fix them. Blockers a change
// can fix (lint failing, flaky tests) are the factory's work instead: see
// Work.
type Step struct {
	Key   string `json:"key"`
	Title string `json:"title"`
	// Why says what the factory needs it for, in a sentence.
	Why   string `json:"why"`
	State string `json:"state"`
	// Passed is what the check saw when the step is done ("12 test
	// files, passing twice in a row"), or "".
	Passed   string    `json:"passed"`
	Blockers []Finding `json:"blockers"`
}

// steps are the groups of areas a blocker can be in.
var steps = []struct {
	key, title, why string
	areas           []string
}{
	{"access", "The factory's token can do its job",
		"Every change ends with the factory opening a pull request, and it labels and comments on issues as it works.", []string{"access"}},
	{"run", "The factory can install the repo and run its checks",
		"Every job installs a fresh copy of {branch} and runs the repo's checks there, with the services and files they need.", []string{"install", "checks", "services", "files", "tests"}},
}

// Steps are the setup steps the report covers, in order: what a person
// must do. The access step is left out when the host couldn't be asked.
func (r *Report) Steps() []Step {
	var out []Step
	for _, g := range steps {
		s := Step{Key: g.key, Title: strings.ReplaceAll(g.title, "{branch}", r.Branch), Why: strings.ReplaceAll(g.why, "{branch}", r.Branch), Blockers: []Finding{}}
		var passed []string
		seen := false
		for _, f := range r.Findings {
			if !slices.Contains(g.areas, f.Area) {
				continue
			}
			switch {
			case f.Level == Blocker && f.Issue != nil:
				seen = true // the factory's to fix: see Work
			case f.Level == Blocker:
				s.Blockers = append(s.Blockers, f)
				seen = true
			case f.Level == Good:
				passed = append(passed, f.Title)
				seen = true
			}
		}
		// Shown when it was looked at: a host that couldn't be asked skips it.
		if g.key == "access" && !seen {
			continue
		}
		switch {
		case len(s.Blockers) > 0:
			s.State = StepFailing
		case len(passed) > 0 || !r.Incomplete:
			s.State = StepDone
			s.Passed = strings.Join(passed, "; ")
		default:
			s.State = StepUnchecked
		}
		out = append(out, s)
	}
	return out
}

// Work are the blockers a change can fix, so the factory does, each
// filed as an issue: until they're merged, every other job would fail.
func (r *Report) Work() []Finding {
	out := []Finding{}
	for _, f := range r.Findings {
		if f.Level == Blocker && f.Issue != nil {
			out = append(out, f)
		}
	}
	return out
}

// Progress counts the setup steps done, of all of them.
func (r *Report) Progress() (done, total int) {
	for _, s := range r.Steps() {
		if s.State == StepDone {
			done++
		}
		total++
	}
	return done, total
}

// Extras are the findings outside the setup steps: gaps and tips, worth
// doing but not needed to start, and what's already in place beyond the
// steps.
func (r *Report) Extras() []Finding {
	in := map[string]bool{}
	for _, g := range steps {
		for _, a := range g.areas {
			in[a] = true
		}
	}
	out := []Finding{}
	for _, f := range r.Findings {
		if f.Level == Blocker || f.Level == Good && in[f.Area] {
			continue
		}
		out = append(out, f)
	}
	return out
}

// Fixes is the blocker that work on issue fixes, or nil: the blocker
// filed as issue, or as parent, the epic issue was split from (0 for
// none).
func (r *Report) Fixes(issue, parent int) *Finding {
	if r == nil {
		return nil
	}
	for i, f := range r.Findings {
		if f.Level == Blocker && f.Issue != nil && f.Issue.Number != 0 && (f.Issue.Number == issue || f.Issue.Number == parent) {
			return &r.Findings[i]
		}
	}
	return nil
}

// Holds says whether work on issue waits for the repo's setup: the check
// found blockers, so every job would fail on them, and the work isn't
// fixing one. An unchecked repo, or a check that couldn't finish, holds
// nothing.
func (r *Report) Holds(issue, parent int) bool {
	return r != nil && !r.Ready && !r.Incomplete && r.Fixes(issue, parent) == nil
}

// Path is where Save keeps repo's latest report in dir.
func Path(dir, repo string) string {
	return filepath.Join(dir, strings.ReplaceAll(repo, "/", "__")+".json")
}

// beside is a file kept next to repo's report in dir.
func beside(dir, repo, ext string) string {
	return strings.TrimSuffix(Path(dir, repo), ".json") + "." + ext
}

// SaveBaselinePR records the pull request a lint baseline opened on repo.
func SaveBaselinePR(dir, repo, url string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(beside(dir, repo, "baseline"), []byte(url), 0o644)
}

// ClearBaselinePR forgets the pull request SaveBaselinePR recorded.
func ClearBaselinePR(dir, repo string) {
	_ = os.Remove(beside(dir, repo, "baseline"))
}

// BaselinePR is the pull request SaveBaselinePR recorded, or "".
func BaselinePR(dir, repo string) string {
	b, _ := os.ReadFile(beside(dir, repo, "baseline"))
	return strings.TrimSpace(string(b))
}

// Load reads repo's latest report from dir; nil, with no error, when it
// was never checked.
func Load(dir, repo string) (*Report, error) {
	b, err := os.ReadFile(Path(dir, repo))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var r Report
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	return &r, nil
}
