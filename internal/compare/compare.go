// Package compare runs one issue once per skill set, from the same base
// commit, through the same implement, gates and review, and reports the
// arms side by side: the diff, gate results, the reviewer's verdict, cost
// and time. Each arm has its own store and an in-memory GitHub, so no PR is
// opened; the arms' branches (factory/compare-<issue>-<set>) are pushed to
// the real remote at the end so you can read them there.
package compare

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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
	"github.com/claptimes5/factory/internal/verdict"
)

// None is the skill set name for "no skills mounted".
const None = "none"

// Options is what a comparison needs from the daemon's wiring.
type Options struct {
	Cfg       *config.Config
	Repo      string // owner/name of the issue's repo; default the first configured repo
	Harnesses map[string]harness.Harness
	Runner    runner.Runner
	Secrets   map[string]map[string]string
	GH        interface {
		GetIssue(ctx context.Context, n int) (*github.Issue, error)
	}
	Git      *pipeline.Git // the real remote; used to clone the base and push the arms
	GateExec func(ctx context.Context, workdir, command string) (int, string, error)
	Dir      string // where the run keeps its files and the report
	Push     bool   // push the arms' branches to the real remote
	// Budget is what the arms may spend together (the daemon's remaining
	// daily budget); 0 means no limit. Each arm gets an equal share.
	Budget float64
	Log    *slog.Logger
}

// Arm is one skill set's run.
type Arm struct {
	Skillset   string             `json:"skillset"`
	Branch     string             `json:"branch"`
	Stage      string             `json:"stage"` // awaiting_human means it passed review
	Head       string             `json:"head,omitempty"`
	Gates      []store.GateResult `json:"gates"`
	Review     string             `json:"review"` // pass, block, questions, or "" when none ran
	Findings   []string           `json:"findings,omitempty"`
	Implements int                `json:"implement_jobs"`
	CostUSD    float64            `json:"cost_usd"`
	Seconds    float64            `json:"seconds"`
	DiffStat   string             `json:"diff_stat"`
	Diff       string             `json:"-"`
	Error      string             `json:"error,omitempty"`
}

// Report is the whole comparison.
type Report struct {
	Repo       string    `json:"repo"`
	Issue      int       `json:"issue"`
	Title      string    `json:"title"`
	BaseSHA    string    `json:"base_sha"`
	Complexity string    `json:"complexity"`
	Started    time.Time `json:"started"`
	Arms       []Arm     `json:"arms"`
}

func (o Options) repoConf() config.Repo {
	if r, ok := o.Cfg.RepoConf(o.Repo); ok {
		return r
	}
	return o.Cfg.Primary()
}

var setName = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// Run compares the skill sets on one issue.
func Run(ctx context.Context, o Options, issue int, sets []string, complexity, risk string) (*Report, error) {
	if len(sets) < 2 {
		return nil, fmt.Errorf("compare needs at least two skill sets")
	}
	seen := map[string]bool{}
	for _, s := range sets {
		if !setName.MatchString(s) || seen[s] {
			return nil, fmt.Errorf("bad or repeated skill set %q", s)
		}
		seen[s] = true
		if s != None {
			if _, err := os.Stat(filepath.Join(o.Cfg.Path(o.Cfg.SkillsDir), s)); err != nil {
				return nil, fmt.Errorf("skill set %q: %w", s, err)
			}
		}
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	is, err := o.GH.GetIssue(ctx, issue)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(o.Dir, 0o755); err != nil {
		return nil, err
	}
	// Every arm builds from one frozen copy of the base, so a commit landing
	// on the base mid-run cannot make the comparison unfair, and holding
	// nothing but the base, so no arm can read an earlier solution (the
	// daemon's PR branch or a previous comparison's).
	rc := o.repoConf()
	frozen := filepath.Join(o.Dir, "remote.git")
	if err := o.Git.CloneBare(ctx, frozen); err != nil {
		return nil, err
	}
	base, err := gitOut(ctx, frozen, "rev-parse", "refs/heads/"+rc.BaseBranch)
	if err != nil {
		return nil, err
	}
	if err := o.Git.Freeze(ctx, frozen, "refs/heads/"+rc.BaseBranch); err != nil {
		return nil, err
	}
	rep := &Report{Repo: rc.Name, Issue: issue, Title: is.Title, BaseSHA: base, Complexity: complexity, Started: time.Now().UTC()}
	rep.Arms = make([]Arm, len(sets))
	remotes := make([]string, len(sets))
	share := 0.0
	if o.Budget > 0 {
		share = o.Budget / float64(len(sets))
	}
	var wg sync.WaitGroup
	for i, set := range sets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Each arm pushes to its own copy, so arms never see each other.
			remotes[i] = filepath.Join(o.Dir, set, "remote.git")
			arm := Arm{Skillset: set}
			err := (&pipeline.Git{RemoteURL: frozen}).CloneBare(ctx, remotes[i])
			if err == nil {
				arm, err = runArm(ctx, o, is, set, complexity, risk, remotes[i], i, share)
			}
			if err != nil {
				arm.Error = err.Error()
			}
			rep.Arms[i] = arm
		}()
	}
	wg.Wait()
	for i := range rep.Arms {
		a := &rep.Arms[i]
		if a.Head == "" {
			continue
		}
		a.DiffStat, _ = gitOut(ctx, remotes[i], "diff", "--no-ext-diff", "--no-textconv", "--stat", base, a.Head)
		a.Diff, _ = gitOut(ctx, remotes[i], "diff", "--no-ext-diff", "--no-textconv", base, a.Head)
		if o.Push {
			if err := o.Git.Push(ctx, remotes[i], a.Head, a.Branch, true); err != nil {
				a.Error = strings.TrimSpace(a.Error + "; push failed: " + err.Error())
			}
		}
	}
	return rep, ctx.Err()
}

func runArm(ctx context.Context, o Options, is *github.Issue, set, complexity, risk, remote string, i int, budget float64) (Arm, error) {
	arm := Arm{Skillset: set, Branch: fmt.Sprintf("factory/compare-%d-%s", is.Number, set)}
	dir := filepath.Join(o.Dir, set)
	cfg := *o.Cfg
	cfg.Repos = []config.Repo{o.repoConf()}
	cfg.DataDir = filepath.Join(dir, "data")
	cfg.Orchestrator.On = false
	if budget > 0 {
		cfg.Budgets.DailyTotal = budget
	}
	cfg.Routing = map[string]config.Route{}
	for k, r := range o.Cfg.Routing {
		if strings.HasPrefix(k, "implement") {
			r.Skills = set
			if set == None {
				r.Skills = ""
			}
		}
		cfg.Routing[k] = r
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return arm, err
	}
	st, err := store.Open(filepath.Join(dir, "arm.db"))
	if err != nil {
		return arm, err
	}
	defer func() { _ = st.Close() }()
	gh := github.NewMemory()
	gh.AddIssue(is.Number, is.Title, is.Body)
	p := &pipeline.Pipeline{
		Cfg: &cfg, Store: st, GH: gh, Runner: o.Runner, Harnesses: o.Harnesses, Secrets: o.Secrets,
		Git:      &pipeline.Git{RemoteURL: remote, Author: o.Git.Author, AskPassDir: dir},
		GateExec: o.GateExec, JobPrefix: fmt.Sprintf("cmp%d-%d-", os.Getpid(), i), Log: o.Log.With("skillset", set),
	}
	it, _, err := st.UpsertItem(ctx, cfg.Repos[0].Name, is.Number, is.Title)
	if err != nil {
		return arm, err
	}
	if err := st.UpdateItem(ctx, it.ID, store.ItemUpdate{Branch: &arm.Branch, Complexity: &complexity, Risk: &risk}); err != nil {
		return arm, err
	}
	if err := st.SetStage(ctx, it.ID, state.Ready, "daemon", "compare arm "+set); err != nil {
		return arm, err
	}
	if it, err = st.GetItem(ctx, it.ID); err != nil {
		return arm, err
	}
	start := time.Now()
	if err := p.StartImplement(ctx, it); err != nil {
		return arm, err
	}
	if err := p.Drain(ctx); err != nil {
		return arm, err
	}
	arm.Seconds = time.Since(start).Seconds()
	if it, err = st.GetItem(ctx, it.ID); err != nil {
		return arm, err
	}
	arm.Stage = string(it.Stage)
	jobs, err := st.ListJobs(ctx, store.JobFilter{ItemID: it.ID})
	if err != nil {
		return arm, err
	}
	var lastImpl, lastReview *store.Job
	for _, j := range jobs { // newest first
		arm.CostUSD += j.CostUSD
		switch j.Kind {
		case "implement":
			arm.Implements++
			if lastImpl == nil {
				lastImpl = j
			}
		case "review":
			if lastReview == nil && j.State == state.Succeeded {
				lastReview = j
			}
		}
	}
	if lastImpl != nil {
		arm.Gates, _ = st.GateResults(ctx, lastImpl.ID)
	}
	for _, j := range jobs {
		if j.Kind == "implement" && j.State == state.Succeeded && j.HeadSHA != "" {
			arm.Head = j.HeadSHA
			break
		}
	}
	if lastReview != nil {
		var v verdict.Review
		if verdict.Parse([]byte(lastReview.Verdict), &v) == nil {
			arm.Review = v.Decision
			for _, f := range v.Findings {
				arm.Findings = append(arm.Findings, fmt.Sprintf("%s %s:%d %s", f.Severity, f.File, f.Line, f.Claim))
			}
		}
	}
	if arm.Head == "" && it.Stage == state.Parked {
		evs, _ := st.Events(ctx, it.ID)
		if len(evs) > 0 {
			arm.Error = "parked: " + firstLine(evs[len(evs)-1].Reason)
		}
	}
	return arm, nil
}

func gitOut(ctx context.Context, gitDir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"--git-dir=" + gitDir, "-c", "core.hooksPath=/dev/null"}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w", args[0], err)
	}
	return strings.TrimSpace(string(out)), nil
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	return s
}

// Save writes report.json and report.md into dir.
func (r *Report) Save(dir string) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "report.json"), b, 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "report.md"), []byte(r.Markdown()), 0o644)
}

// Markdown renders the side-by-side report.
func (r *Report) Markdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Skill comparison: #%d %s\n\nBase `%s`, complexity %s, started %s.\n\n", r.Issue, r.Title, short(r.BaseSHA), r.Complexity, r.Started.Format(time.RFC3339))
	row := func(label string, f func(a Arm) string) {
		fmt.Fprintf(&b, "| %s |", label)
		for _, a := range r.Arms {
			fmt.Fprintf(&b, " %s |", strings.ReplaceAll(f(a), "|", "\\|"))
		}
		b.WriteString("\n")
	}
	row("", func(a Arm) string { return "**" + a.Skillset + "**" })
	b.WriteString("| --- |" + strings.Repeat(" --- |", len(r.Arms)) + "\n")
	row("Outcome", func(a Arm) string {
		if a.Stage == string(state.AwaitingHuman) {
			return "passed review"
		}
		return a.Stage
	})
	row("Review", func(a Arm) string { return dash(a.Review) + fmt.Sprintf(" (%d findings)", len(a.Findings)) })
	row("Gates", func(a Arm) string {
		var bad []string
		for _, g := range a.Gates {
			if g.Status != "pass" {
				bad = append(bad, g.Gate+"="+g.Status)
			}
		}
		if len(a.Gates) == 0 {
			return "–"
		}
		if len(bad) == 0 {
			return "all pass"
		}
		return strings.Join(bad, ", ")
	})
	row("Implement jobs", func(a Arm) string { return fmt.Sprint(a.Implements) })
	row("Cost", func(a Arm) string { return fmt.Sprintf("$%.2f", a.CostUSD) })
	row("Time", func(a Arm) string { return fmt.Sprintf("%.0fs", a.Seconds) })
	row("Diff", func(a Arm) string { return lastLine(a.DiffStat) })
	row("Branch", func(a Arm) string { return "`" + a.Branch + "`" })
	for _, a := range r.Arms {
		fmt.Fprintf(&b, "\n## %s\n\n", a.Skillset)
		if a.Error != "" {
			fmt.Fprintf(&b, "Error: %s\n\n", a.Error)
		}
		for _, f := range a.Findings {
			fmt.Fprintf(&b, "- %s\n", f)
		}
		if a.Diff != "" {
			fence := "```"
			for strings.Contains(a.Diff, fence) {
				fence += "`"
			}
			fmt.Fprintf(&b, "\n<details><summary>Diff</summary>\n\n%sdiff\n%s\n%s\n\n</details>\n", fence, a.Diff, fence)
		}
	}
	return b.String()
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

func dash(s string) string {
	if s == "" {
		return "–"
	}
	return s
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}
