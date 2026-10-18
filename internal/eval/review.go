package eval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/claptimes5/factory/internal/github"
	"github.com/claptimes5/factory/internal/pipeline"
	"github.com/claptimes5/factory/internal/state"
	"github.com/claptimes5/factory/internal/store"
	"gopkg.in/yaml.v3"
)

// Reviewer evals: past merged changes, reviewed alone, scored on what the
// reviewer blocked. A planted case is a change with a known bug put in it
// (see PlantBug), which counts as caught only when a blocking finding is
// at the bug; a clean case is a change that merged and stood, where any
// block is a false positive; a revert case is a change that merged and was
// later reverted, which the reviewer should have blocked.

// Reviewer case kinds.
const (
	KindPlanted = "planted"
	KindClean   = "clean"
	KindRevert  = "revert"
)

// PatchFile holds a reviewer case's change, as a diff from its base.
const PatchFile = "change.patch"

// ReviewCase is evals/<name>/case.yaml with a kind, plus change.patch.
type ReviewCase struct {
	Name    string `yaml:"-"`
	Dir     string `yaml:"-"`
	Kind    string `yaml:"kind"`
	Repo    string `yaml:"repo,omitempty"`
	Source  string `yaml:"source,omitempty"`
	BaseSHA string `yaml:"base_sha"`
	Title   string `yaml:"title"`
	Body    string `yaml:"body,omitempty"`
	Risk    string `yaml:"risk,omitempty"`  // low, medium or high; default low
	Plant   *Plant `yaml:"plant,omitempty"` // a planted case's bug
}

// LoadReview reads every reviewer case under dir.
func LoadReview(dir string) ([]ReviewCase, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var cases []ReviewCase
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name(), "case.yaml"))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		} else if err != nil {
			return nil, err
		}
		var c ReviewCase
		if err := yaml.Unmarshal(b, &c); err != nil {
			return nil, fmt.Errorf("%s: %w", e.Name(), err)
		}
		if c.Kind == "" {
			continue // the builder's
		}
		c.Name, c.Dir = e.Name(), filepath.Join(dir, e.Name())
		switch {
		case c.Kind != KindPlanted && c.Kind != KindClean && c.Kind != KindRevert:
			return nil, fmt.Errorf("%s: kind must be planted, clean or revert", c.Name)
		case c.BaseSHA == "" || c.Title == "":
			return nil, fmt.Errorf("%s: base_sha and title are required", c.Name)
		case c.Kind == KindPlanted && (c.Plant == nil || c.Plant.File == "" || c.Plant.Line < 1):
			return nil, fmt.Errorf("%s: a planted case needs plant: {mutation, file, line}", c.Name)
		case c.Risk != "" && c.Risk != "low" && c.Risk != "medium" && c.Risk != "high":
			return nil, fmt.Errorf("%s: risk must be low, medium or high", c.Name)
		}
		if _, err := os.Stat(filepath.Join(c.Dir, PatchFile)); err != nil {
			return nil, fmt.Errorf("%s: %w", c.Name, err)
		}
		cases = append(cases, c)
	}
	sort.Slice(cases, func(a, b int) bool { return cases[a].Name < cases[b].Name })
	return cases, nil
}

// Sample picks at most n cases, alike from run to run: in name order,
// one of each kind in turn (planted, clean, revert) while each lasts, so
// a small sample still holds every kind.
func Sample(cases []ReviewCase, n int) []ReviewCase {
	if n <= 0 || len(cases) <= n {
		return cases
	}
	by := map[string][]ReviewCase{}
	for _, c := range cases {
		by[c.Kind] = append(by[c.Kind], c)
	}
	var out []ReviewCase
	for len(out) < n {
		for _, k := range []string{KindPlanted, KindClean, KindRevert} {
			if len(by[k]) > 0 && len(out) < n {
				out = append(out, by[k][0])
				by[k] = by[k][1:]
			}
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Name < out[b].Name })
	return out
}

// ReviewResult is one reviewer case's outcome.
type ReviewResult struct {
	Case    string `json:"case"`
	Kind    string `json:"kind"`
	Blocked bool   `json:"blocked"`
	// Caught: a planted case blocked with a finding at the bug. False
	// positive: a clean case blocked.
	Caught   bool    `json:"caught"`
	FalsePos bool    `json:"false_positive"`
	Findings int     `json:"blocking_findings"` // that the verifier didn't refute
	Stage    string  `json:"stage"`
	Error    bool    `json:"error,omitempty"` // no ruling: an eval error, or no valid verdict
	Reason   string  `json:"reason,omitempty"`
	CostUSD  float64 `json:"cost_usd"`
	Seconds  float64 `json:"seconds"`
}

// ReviewReport is a whole reviewer eval run.
type ReviewReport struct {
	Config string `json:"config"`
	pipeline.ReviewScore
	Seconds float64        `json:"seconds"`
	Results []ReviewResult `json:"results"`
}

// RunReview reviews every reviewer case and reports.
func RunReview(ctx context.Context, o Options, cases []ReviewCase) (*ReviewReport, error) {
	if o.Parallel < 1 {
		o.Parallel = 1
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	results := make([]ReviewResult, len(cases))
	sem := make(chan struct{}, o.Parallel)
	var wg sync.WaitGroup
	var mu sync.Mutex
	spent, capped := 0.0, false
	for i, c := range cases {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			mu.Lock()
			over := o.MaxCostUSD > 0 && spent >= o.MaxCostUSD
			capped = capped || over
			mu.Unlock()
			if over {
				results[i] = ReviewResult{Case: c.Name, Kind: c.Kind, Error: true, Reason: fmt.Sprintf("not run: the run's $%.2f cap is spent", o.MaxCostUSD)}
				return
			}
			o.Log.Info("reviewer eval case", "case", c.Name, "kind", c.Kind)
			r, err := runReviewCase(ctx, o, c, filepath.Join(o.WorkDir, fmt.Sprintf("r%02d-%s", i, c.Name)), fmt.Sprintf("reval%d-%d-", os.Getpid(), i))
			if err != nil {
				r = ReviewResult{Case: c.Name, Kind: c.Kind, Error: true, Reason: "eval error: " + err.Error(), CostUSD: r.CostUSD}
			}
			mu.Lock()
			spent += r.CostUSD
			mu.Unlock()
			results[i] = r
		}()
	}
	wg.Wait()
	rep := &ReviewReport{Results: results}
	rep.Outcomes = map[string]string{}
	for _, r := range results {
		rep.CostUSD += r.CostUSD
		rep.Seconds += r.Seconds
		rep.Outcomes[r.Case] = r.Outcome()
		if r.Error {
			rep.Errors++ // in no denominator
			continue
		}
		switch r.Kind {
		case KindPlanted:
			rep.Planted++
			if r.Caught {
				rep.Caught++
			}
		case KindClean:
			rep.Clean++
			if r.FalsePos {
				rep.FalsePo++
			}
		case KindRevert:
			rep.Reverts++
			if r.Blocked {
				rep.Stopped++
			}
		}
	}
	rep.Rates()
	if capped {
		return rep, fmt.Errorf("stopped at its $%.2f cap with cases left to run", o.MaxCostUSD)
	}
	return rep, ctx.Err()
}

// Outcome is the case's result in a word (see pipeline.OutcomeCaught).
func (r ReviewResult) Outcome() string {
	switch {
	case r.Error:
		return pipeline.OutcomeError
	case r.Kind == KindClean && r.FalsePos:
		return pipeline.OutcomeFalsePos
	case r.Kind == KindClean:
		return pipeline.OutcomeOK
	case r.Caught, r.Kind == KindRevert && r.Blocked:
		return pipeline.OutcomeCaught
	}
	return pipeline.OutcomeMissed
}

func runReviewCase(ctx context.Context, o Options, c ReviewCase, dir, prefix string) (ReviewResult, error) {
	res := ReviewResult{Case: c.Name, Kind: c.Kind}
	if err := os.RemoveAll(dir); err != nil {
		return res, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return res, err
	}
	cfg, rc, known := caseConfig(o, c.Repo, dir)
	remote := filepath.Join(dir, "remote.git")
	if err := freeze(ctx, o, Case{Repo: c.Repo, BaseSHA: c.BaseSHA}, rc.Name, known || c.Repo == "", remote, rc.BaseBranch); err != nil {
		return res, err
	}
	head, err := c.headOn(ctx, remote, rc.BaseBranch, filepath.Join(dir, "change"))
	if err != nil {
		return res, err
	}
	st, err := store.Open(filepath.Join(dir, "eval.db"))
	if err != nil {
		return res, err
	}
	defer func() { _ = st.Close() }()
	gh := github.NewMemory()
	gh.AddIssue(1, c.Title, c.Body, cfg.Intake.Label)
	p := &pipeline.Pipeline{
		Cfg: cfg, Store: st, GH: gh, Runner: o.Runner, Harnesses: o.Harnesses, Secrets: o.Secrets, Log: o.Log.With("case", c.Name),
		Git:      &pipeline.Git{RemoteURL: remote, Author: "factory-eval <eval@localhost>", AskPassDir: dir},
		GateExec: o.GateExec, JobPrefix: prefix,
	}
	risk := c.Risk
	if risk == "" {
		risk = "low"
	}
	start := time.Now()
	it, err := p.ReviewOnly(ctx, rc.Name, 1, risk, head)
	if err != nil {
		return res, err
	}
	res.Seconds = time.Since(start).Seconds()
	jobs, err := st.ListJobs(ctx, store.JobFilter{ItemID: it.ID})
	if err != nil {
		return res, err
	}
	for _, j := range jobs {
		res.CostUSD += j.CostUSD
	}
	res.Stage = string(it.Stage)
	last := ""
	if evs, _ := st.Events(ctx, it.ID); len(evs) > 0 {
		last = firstLine(evs[len(evs)-1].Reason)
	}
	switch {
	case it.Stage == state.Implementing:
		res.Blocked = true
	case it.Stage == state.Parked && lastDecision(jobs) == "questions":
		res.Blocked, res.Reason = true, "the reviewer asked questions"
	case it.Stage != state.AwaitingHuman:
		// A park for any other reason (a review with no valid verdict)
		// is no ruling at all.
		res.Error = true
		res.Reason = "review stopped in " + res.Stage + ": " + last
	}
	fs, err := st.ItemFindings(ctx, it.ID, 0)
	if err != nil {
		return res, err
	}
	for _, f := range fs {
		if !f.Blocking || f.Outcome == store.OutcomeRefuted || f.Check == "refuted" {
			continue
		}
		res.Findings++
		if c.Plant != nil && atPlant(f, *c.Plant) {
			res.Caught = res.Blocked
		}
	}
	res.FalsePos = c.Kind == KindClean && res.Blocked
	if c.Kind == KindPlanted && res.Blocked && !res.Caught && res.Reason == "" {
		res.Reason = fmt.Sprintf("blocked, but no finding at %s:%d", c.Plant.File, c.Plant.Line)
	}
	return res, nil
}

// lastDecision is the decision of the newest review or critic job that
// succeeded, or "".
func lastDecision(jobs []*store.Job) string {
	var newest *store.Job
	for _, j := range jobs {
		if (j.Kind == "review" || j.Kind == "critic") && j.State == state.Succeeded && (newest == nil || j.ID > newest.ID) {
			newest = j
		}
	}
	if newest == nil {
		return ""
	}
	var v struct {
		Decision string `json:"decision"`
	}
	_ = json.Unmarshal([]byte(newest.Verdict), &v)
	return v.Decision
}

// plantSlack is how many lines from the bug a finding may point and
// still be at it: the hunk around it, about.
const plantSlack = 6

// atPlant says whether a finding points at the planted bug: its file, and
// a line near it or none.
func atPlant(f store.ReviewFinding, pl Plant) bool {
	file := path.Clean(strings.TrimPrefix(filepath.ToSlash(f.File), "./"))
	if file != pl.File && !strings.HasSuffix(file, "/"+pl.File) {
		return false
	}
	d := f.Line - pl.Line
	return f.Line == 0 || d >= -plantSlack && d <= plantSlack
}

// headOn makes the case's change, and a planted case's bug, a commit on
// the frozen base and pushes it to the branch the review reads. It
// returns the commit.
func (c ReviewCase) headOn(ctx context.Context, remote, base, work string) (string, error) {
	if err := gitRun(ctx, "clone", "-q", "-b", base, remote, work); err != nil {
		return "", err
	}
	patch, err := filepath.Abs(filepath.Join(c.Dir, PatchFile))
	if err != nil {
		return "", err
	}
	if err := gitRun(ctx, "-C", work, "apply", "--whitespace=nowarn", patch); err != nil {
		return "", fmt.Errorf("%s no longer applies to its base: %w", PatchFile, err)
	}
	if c.Kind == KindPlanted {
		diff, err := os.ReadFile(patch)
		if err != nil {
			return "", err
		}
		if !safePath(c.Plant.File) {
			return "", fmt.Errorf("plant.file %q isn't a usable path", c.Plant.File)
		}
		file := filepath.Join(work, filepath.FromSlash(c.Plant.File))
		src, err := os.ReadFile(file)
		if err != nil {
			return "", err
		}
		out, line, err := PlantBug(string(src), AddedLines(string(diff))[c.Plant.File], c.Plant.Mutation)
		if err != nil {
			return "", err
		}
		if line != c.Plant.Line {
			return "", fmt.Errorf("%s now lands on line %d, not %d: capture the case again", c.Plant.Mutation, line, c.Plant.Line)
		}
		if err := os.WriteFile(file, []byte(out), 0o644); err != nil {
			return "", err
		}
	}
	if err := gitRun(ctx, "-C", work, "add", "-A"); err != nil {
		return "", err
	}
	if err := gitRun(ctx, "-C", work, "-c", "user.name=factory-eval", "-c", "user.email=eval@localhost", "-c", "commit.gpgsign=false",
		"commit", "-q", "--allow-empty", "-m", "the change under review"); err != nil {
		return "", err
	}
	if err := gitRun(ctx, "-C", work, "push", "-q", "origin", "HEAD:refs/heads/factory/issue-1"); err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, "git", "-C", work, "rev-parse", "HEAD")
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

// CompareReview says why a candidate's reviewer evals are worse than the
// baseline's, by more than tolerance (see pipeline.ReviewRegressions).
func CompareReview(cur, base *ReviewReport, tolerance float64) []string {
	return pipeline.ReviewRegressions(cur.ReviewScore, base.ReviewScore, tolerance)
}

// Table renders a reviewer report for the terminal.
func (r *ReviewReport) Table() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%-32s %-8s %-8s %-14s %8s %9s  %s\n", "CASE", "KIND", "BLOCKED", "RESULT", "FINDINGS", "COST", "REASON")
	for _, x := range r.Results {
		blocked := "no"
		if x.Blocked {
			blocked = "yes"
		}
		result := x.Outcome()
		fmt.Fprintf(&b, "%-32s %-8s %-8s %-14s %8d %9s  %s\n", x.Case, x.Kind, blocked, result, x.Findings, fmt.Sprintf("$%.2f", x.CostUSD), firstLine(x.Reason))
	}
	fmt.Fprintf(&b, "\nrecall %.0f%% (%d of %d planted) · false positives %.0f%% (%d of %d clean) · reverts blocked %d of %d · cost $%.2f · time %.0fs",
		r.Recall*100, r.Caught, r.Planted, r.FPRate*100, r.FalsePo, r.Clean, r.Stopped, r.Reverts, r.CostUSD, r.Seconds)
	if r.Errors > 0 {
		fmt.Fprintf(&b, " · %d with no ruling", r.Errors)
	}
	b.WriteString("\n")
	return b.String()
}
