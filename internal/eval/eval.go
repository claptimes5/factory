// Package eval runs the replay set: past issues with known-good fixes, each
// frozen at its base commit with a hidden acceptance test. Every case runs
// through the real pipeline (triage, implement, gates, review) against a
// private copy of the repository and an in-memory GitHub, so nothing is
// pushed or posted anywhere. The report gives pass rate, cost and time.
package eval

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/claptimes5/factory/internal/config"
	"github.com/claptimes5/factory/internal/github"
	"github.com/claptimes5/factory/internal/harness"
	"github.com/claptimes5/factory/internal/pipeline"
	"github.com/claptimes5/factory/internal/runner"
	"github.com/claptimes5/factory/internal/state"
	"github.com/claptimes5/factory/internal/store"
	"gopkg.in/yaml.v3"
)

// Case is one replay: evals/<name>/case.yaml, plus evals/<name>/accept/,
// files laid over the factory's result before the acceptance command runs
// (typically the known-good fix's tests). The builder never sees them.
type Case struct {
	Name string `yaml:"-"`
	Dir  string `yaml:"-"`
	// Repo is a configured repo's owner/name (its gates and base branch
	// apply), or a clone URL or path; default the first configured repo.
	Repo    string `yaml:"repo,omitempty"`
	Source  string `yaml:"source,omitempty"` // where the case came from, e.g. PR #14
	BaseSHA string `yaml:"base_sha"`
	Title   string `yaml:"title"`
	Body    string `yaml:"body"`
	Accept  string `yaml:"accept"` // e.g. go test -run TestAdd ./...
	// Kind is empty for a builder case; a reviewer case (see ReviewCase)
	// says planted, clean or revert, and Load leaves it out.
	Kind string `yaml:"kind,omitempty"`
}

// Load reads every case under dir.
func Load(dir string) ([]Case, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var cases []Case
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
		var c Case
		if err := yaml.Unmarshal(b, &c); err != nil {
			return nil, fmt.Errorf("%s: %w", e.Name(), err)
		}
		c.Name, c.Dir = e.Name(), filepath.Join(dir, e.Name())
		if c.Kind != "" {
			continue // the reviewer's, for LoadReview
		}
		if c.BaseSHA == "" || c.Title == "" || c.Accept == "" {
			return nil, fmt.Errorf("%s: base_sha, title and accept are required", c.Name)
		}
		cases = append(cases, c)
	}
	sort.Slice(cases, func(a, b int) bool { return cases[a].Name < cases[b].Name })
	return cases, nil
}

// Result is one case's outcome.
type Result struct {
	Case    string  `json:"case"`
	Passed  bool    `json:"passed"`
	Stage   string  `json:"stage"`  // where the pipeline left the item
	Reason  string  `json:"reason"` // why it failed, when it did
	CostUSD float64 `json:"cost_usd"`
	Seconds float64 `json:"seconds"`
	Jobs    int     `json:"jobs"`
	// Rounds is how many review rounds the case took (0 when it never
	// reached review); Findings and Refuted count its blocking findings
	// and the ones the verifier refuted.
	Rounds   int `json:"review_rounds"`
	Findings int `json:"blocking_findings"`
	Refuted  int `json:"refuted_findings"`
}

// Report is the whole run.
type Report struct {
	Config      string  `json:"config"`
	Cases       int     `json:"cases"`
	Passed      int     `json:"passed"`
	PassRate    float64 `json:"pass_rate"`
	CostUSD     float64 `json:"cost_usd"`
	CostPerPass float64 `json:"cost_per_pass_usd"` // total cost / passes; 0 when none passed
	Seconds     float64 `json:"seconds"`
	// MeanRounds is the mean review rounds of the cases that were reviewed.
	MeanRounds float64  `json:"mean_review_rounds"`
	Results    []Result `json:"results"`
}

// Options is what a run needs from the daemon's wiring.
type Options struct {
	Cfg       *config.Config
	Harnesses map[string]harness.Harness
	Runner    runner.Runner
	Secrets   map[string]map[string]string
	Tokens    github.TokenSource // for cloning the source repository
	// TokensFor, when set, gives the tokens for a configured repo instead.
	TokensFor func(repo string) github.TokenSource
	// GateExec overrides how gates and acceptance run (tests).
	GateExec func(ctx context.Context, workdir, command string) (int, string, error)
	WorkDir  string // scratch space; each case gets a subdirectory
	Parallel int
	// MaxCostUSD stops a reviewer eval run starting cases once it has
	// spent this much (0: no cap); one that stops for it is an error.
	MaxCostUSD float64
	Log        *slog.Logger
}

// Run runs every case and reports.
func Run(ctx context.Context, o Options, cases []Case) (*Report, error) {
	if o.Parallel < 1 {
		o.Parallel = 1
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	results := make([]Result, len(cases))
	sem := make(chan struct{}, o.Parallel)
	var wg sync.WaitGroup
	for i, c := range cases {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			o.Log.Info("eval case", "case", c.Name)
			r, err := runCase(ctx, o, c, filepath.Join(o.WorkDir, fmt.Sprintf("%02d-%s", i, c.Name)), fmt.Sprintf("eval%d-%d-", os.Getpid(), i))
			if err != nil {
				r = Result{Case: c.Name, Reason: "eval error: " + err.Error()}
			}
			results[i] = r
		}()
	}
	wg.Wait()
	rep := &Report{Cases: len(cases), Results: results}
	reviewed := 0
	for _, r := range results {
		if r.Rounds > 0 {
			reviewed++
			rep.MeanRounds += float64(r.Rounds)
		}
		rep.CostUSD += r.CostUSD
		rep.Seconds += r.Seconds
		if r.Passed {
			rep.Passed++
		}
	}
	if rep.Cases > 0 {
		rep.PassRate = float64(rep.Passed) / float64(rep.Cases)
	}
	if rep.Passed > 0 {
		rep.CostPerPass = rep.CostUSD / float64(rep.Passed)
	}
	if reviewed > 0 {
		rep.MeanRounds /= float64(reviewed)
	}
	return rep, ctx.Err()
}

func runCase(ctx context.Context, o Options, c Case, dir, prefix string) (Result, error) {
	res := Result{Case: c.Name}
	if err := os.RemoveAll(dir); err != nil {
		return res, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return res, err
	}
	cfg, rc, known := caseConfig(o, c.Repo, dir)
	base := rc.BaseBranch
	remote := filepath.Join(dir, "remote.git")
	if err := freeze(ctx, o, c, rc.Name, known || c.Repo == "", remote, base); err != nil {
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
	start := time.Now()
	if err := p.Drain(ctx); err != nil {
		return res, err
	}
	res.Seconds = time.Since(start).Seconds()
	it, err := st.ItemByIssue(ctx, rc.Name, 1)
	if err != nil {
		return res, err
	}
	jobs, err := st.ListJobs(ctx, store.JobFilter{ItemID: it.ID})
	if err != nil {
		return res, err
	}
	res.Jobs = len(jobs)
	for _, j := range jobs {
		res.CostUSD += j.CostUSD
		if j.Kind == "review" && j.State == state.Succeeded {
			res.Rounds++
		}
	}
	if fs, err := st.ItemFindings(ctx, it.ID, 0); err == nil {
		for _, f := range fs {
			if f.Blocking {
				res.Findings++
				if f.Outcome == store.OutcomeRefuted {
					res.Refuted++
				}
			}
		}
	}
	res.Stage = string(it.Stage)
	if it.Stage != state.AwaitingHuman {
		res.Reason = "pipeline stopped in " + res.Stage
		if evs, _ := st.Events(ctx, it.ID); len(evs) > 0 {
			res.Reason += ": " + firstLine(evs[len(evs)-1].Reason)
		}
		return res, nil
	}
	// The factory's PR head, with the hidden acceptance files laid over it.
	check := filepath.Join(dir, "accept")
	if err := export(ctx, remote, "refs/heads/factory/issue-1", check); err != nil {
		return res, err
	}
	if err := overlay(filepath.Join(c.Dir, "accept"), check); err != nil {
		return res, err
	}
	if err := p.CopyFiles(ctx, rc.Name, check); err != nil {
		return res, err
	}
	code, out, err := p.Check(ctx, rc.Name, check, c.Accept)
	switch {
	case err != nil:
		res.Reason = "acceptance could not run: " + err.Error()
	case code != 0:
		res.Reason = fmt.Sprintf("acceptance failed (exit %d): %s", code, lastLines(out, 5))
	default:
		res.Passed = true
	}
	return res, nil
}

// caseConfig is the config one case runs on, in dir: the candidate's,
// with everything that acts beyond the run turned off, and only the
// case's repo (repo: a configured repo's name, else the primary's gates).
// known says whether repo is a configured one.
func caseConfig(o Options, repo, dir string) (*config.Config, config.Repo, bool) {
	cfg := *o.Cfg
	cfg.DataDir = filepath.Join(dir, "data")
	cfg.Intake.PollInterval = config.Duration{}
	cfg.Orchestrator.On = false
	cfg.Learning.Enabled = new(bool) // nothing to learn from a test run
	cfg.MainHealth.Enabled = false   // main is the real repo's, not the run's
	cfg.Sources.Off()                // nor are its errors, alerts and TODOs
	cfg.Hunt.Enabled = false         // nor is its code hunted for bugs
	cfg.Previews.Off()               // nor is there a PR to show them on
	cfg.Budgets.ConfirmOver = 0      // nobody is there to say go
	cfg.Grooming.Off()               // nor is its backlog the run's
	cfg.Plan.Off()                   // nor anyone to approve a plan
	cfg.NoGuard = true               // nor are its reverts, nor its breaker
	rc, known := o.Cfg.RepoConf(repo)
	if !known {
		rc = o.Cfg.Primary()
		rc.Files = nil // the primary repo's files aren't this one's
	}
	rc.ConfirmOver = nil
	cfg.Repos = []config.Repo{rc}
	return &cfg, rc, known
}

// freeze makes a private bare remote whose base branch is the case's base.
func freeze(ctx context.Context, o Options, c Case, repo string, onGitHub bool, remote, base string) error {
	src, tokens, user := c.Repo, o.Tokens, ""
	if onGitHub {
		// On its host: GitHub, or GitLab for a host/group/project name.
		host, name := config.SplitRepo("", repo)
		r := config.Repo{Name: name, Host: host}
		src = r.CloneURL()
		if r.GitLab() {
			user = "oauth2"
		}
		if o.TokensFor != nil {
			tokens = o.TokensFor(repo)
		}
	}
	g := &pipeline.Git{RemoteURL: src, Tokens: tokens, AskPassDir: filepath.Dir(remote), User: user}
	if err := g.CloneBare(ctx, remote); err != nil {
		return err
	}
	if err := gitRun(ctx, "--git-dir="+remote, "update-ref", "refs/heads/"+base, c.BaseSHA+"^{commit}"); err != nil {
		return err
	}
	// Keep only what is reachable from the base, so the builder's clone
	// cannot contain the known-good fix (or anything after it).
	return g.Freeze(ctx, remote, "refs/heads/"+base)
}

func gitRun(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false"}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// export writes a ref's tree as plain files (see pipeline.Git.Export).
func export(ctx context.Context, gitDir, ref, dst string) error {
	abs, err := filepath.Abs(dst)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return err
	}
	idx, err := os.MkdirTemp("", "factory-eval-index-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(idx)
	for _, args := range [][]string{{"read-tree", ref}, {"checkout-index", "-a", "-f", "--prefix=" + abs + "/"}} {
		cmd := exec.CommandContext(ctx, "git", append([]string{"--git-dir=" + gitDir, "--work-tree=" + abs}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_INDEX_FILE="+filepath.Join(idx, "index"), "GIT_CONFIG_NOSYSTEM=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git %s: %v: %s", args[0], err, out)
		}
	}
	return nil
}

// overlay copies src over dst (a missing src is fine).
func overlay(src, dst string) error {
	if _, err := os.Stat(src); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o644)
	})
}

// Compare returns why a candidate report is worse than the baseline: a
// lower pass rate, or a higher cost per pass. Empty means accept.
func Compare(cur, base *Report) []string {
	var why []string
	if cur.PassRate < base.PassRate {
		why = append(why, fmt.Sprintf("pass rate fell from %.0f%% to %.0f%%", base.PassRate*100, cur.PassRate*100))
	}
	if base.CostPerPass > 0 && (cur.CostPerPass > base.CostPerPass || cur.Passed == 0) {
		why = append(why, fmt.Sprintf("cost per pass rose from $%.2f to $%.2f", base.CostPerPass, cur.CostPerPass))
	}
	return why
}

// Table renders a report for the terminal.
func (r *Report) Table() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%-24s %-6s %-16s %9s %8s %6s %8s  %s\n", "CASE", "PASS", "STAGE", "COST", "TIME", "ROUNDS", "FINDINGS", "REASON")
	for _, x := range r.Results {
		pass := "no"
		if x.Passed {
			pass = "yes"
		}
		fmt.Fprintf(&b, "%-24s %-6s %-16s %9s %7.0fs %6d %8s  %s\n", x.Case, pass, x.Stage, fmt.Sprintf("$%.2f", x.CostUSD), x.Seconds, x.Rounds,
			fmt.Sprintf("%d (%d refuted)", x.Findings, x.Refuted), firstLine(x.Reason))
	}
	fmt.Fprintf(&b, "\npass rate %.0f%% (%d of %d) · cost $%.2f · cost per pass $%.2f · time %.0fs · review rounds %.1f\n",
		r.PassRate*100, r.Passed, r.Cases, r.CostUSD, r.CostPerPass, r.Seconds, r.MeanRounds)
	return b.String()
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	if len(s) > 120 {
		s = s[:120] + "…"
	}
	return s
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " | ")
}
