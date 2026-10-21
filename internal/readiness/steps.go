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

// Step is one thing a repo must get right before the factory can work on
// it: the blockers, grouped the way a person fixes them.
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
	{"install", "Dependencies install from a clean checkout",
		"Every job starts by installing dependencies on a fresh copy of {branch}.", []string{"install"}},
	{"checks", "Every required check passes on {branch}",
		"Every change runs these checks. One that already fails on {branch} fails every change, whatever it does.", []string{"checks", "services"}},
	{"tests", "There are tests, and they pass every time",
		"The factory proves each change with the tests, so there must be some, and they must give the same answer on every run.", []string{"tests"}},
}

// Steps are the setup steps the report covers, in order. The install
// step is left out when the repo has no install command.
func (r *Report) Steps() []Step {
	var out []Step
	stuck := r.Incomplete
	for _, g := range steps {
		s := Step{Key: g.key, Title: strings.ReplaceAll(g.title, "{branch}", r.Branch), Why: strings.ReplaceAll(g.why, "{branch}", r.Branch), Blockers: []Finding{}}
		var passed []string
		seen := false
		for _, f := range r.Findings {
			if !slices.Contains(g.areas, f.Area) {
				continue
			}
			switch f.Level {
			case Blocker:
				s.Blockers = append(s.Blockers, f)
				seen = true
			case Good:
				passed = append(passed, f.Title)
				seen = true
			}
		}
		if g.key == "install" && !seen {
			continue
		}
		switch {
		case len(s.Blockers) > 0:
			s.State = StepFailing
		case len(passed) > 0 || !stuck:
			s.State = StepDone
			s.Passed = strings.Join(passed, "; ")
		default:
			s.State = StepUnchecked
		}
		// A failed install means nothing after it ran.
		stuck = stuck || g.key == "install" && s.State == StepFailing
		out = append(out, s)
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

// Follow records that a person asked the factory to work through repo's
// setup by itself (File and start work): it checks the repo again as its
// base branch moves, and starts the next issues once they can pass.
func Follow(dir, repo string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(beside(dir, repo, "follow"), nil, 0o644)
}

// Followed says whether Follow was called for repo.
func Followed(dir, repo string) bool {
	_, err := os.Stat(beside(dir, repo, "follow"))
	return err == nil
}

// SaveBaselinePR records the pull request a lint baseline opened on repo.
func SaveBaselinePR(dir, repo, url string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(beside(dir, repo, "baseline"), []byte(url), 0o644)
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
