package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/claptimes5/factory/internal/config"
	"github.com/claptimes5/factory/internal/github"
	"github.com/claptimes5/factory/internal/pipeline"
	"github.com/claptimes5/factory/internal/readiness"
)

// assess clones a repo's base branch and runs the readiness check on it,
// printing each command as it starts. Tests replace it.
var assess = func(ctx context.Context, cfg *config.Config, p *pipeline.Pipeline, repo string, out io.Writer) (*readiness.Report, error) {
	rc, ok := cfg.RepoConf(repo)
	if !ok {
		return nil, fmt.Errorf("%s isn't in the config", repo)
	}
	data := cfg.Path(cfg.DataDir)
	if err := os.MkdirAll(data, 0o755); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(data, "readiness-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	g := p.GitFor(repo)
	ws, err := g.Prepare(ctx, dir, rc.BaseBranch, "", false, false)
	if err != nil {
		return nil, fmt.Errorf("cloning %s: %w", repo, err)
	}
	export := filepath.Join(dir, "export")
	if err := g.Export(ctx, ws.Repo, ws.BaseSHA, export); err != nil {
		return nil, err
	}
	if err := p.CopyFiles(ctx, repo, export); err != nil {
		return nil, err
	}
	stop, err := p.StartServices(ctx, 0, repo)
	defer stop()
	if err != nil {
		return nil, err
	}
	r := readiness.Assess(ctx, readiness.Input{Repo: rc, SHA: ws.BaseSHA, Dir: export,
		Exec: func(ctx context.Context, dir, cmd string, install bool) (int, string, error) {
			fmt.Fprintf(out, "  running %s\n", cmd)
			return p.Exec(ctx, repo, dir, cmd, install)
		}})
	if _, err := readiness.Save(filepath.Join(data, "readiness"), r); err != nil {
		return r, err
	}
	return r, nil
}

// marker is hidden in the body of each issue the check files, so a later
// run knows the gap is already filed even if its title changed.
func marker(key string) string { return "<!-- factory-readiness: " + key + " -->" }

// planIssues picks the report's findings that need an issue. It skips a
// gap whose issue is still open, or was closed for any reason but done
// (not planned, a duplicate, none given): someone decided. A gap whose
// issue was closed as done is back, so it is filed again; on GitLab,
// which gives no reason, a closed issue stays closed. Once the repo is ready, waiting are the open issues
// filed without the intake label while it had blockers.
func planIssues(ctx context.Context, gh forge, r *readiness.Report, intake string) (todo []readiness.Finding, waiting []int, skipped int, err error) {
	if r.Incomplete {
		return nil, nil, 0, nil
	}
	filed, err := gh.ListIssuesIn(ctx, pipeline.LabelReadiness, "all")
	if err != nil {
		return nil, nil, 0, err
	}
	if r.Ready {
		for _, o := range filed {
			if o.State == "open" && untouched(o, intake) {
				waiting = append(waiting, o.Number)
			}
		}
	}
	for _, f := range r.Findings {
		if f.Issue == nil {
			continue
		}
		dup := false
		for _, o := range filed {
			same := strings.Contains(o.Body, marker(f.Issue.Key)) || strings.EqualFold(o.Title, f.Issue.Title)
			// Only a close known to be done (GitHub's "completed") lets it
			// back in; GitLab gives no reason, so a closed issue stays closed.
			dup = dup || (same && (o.State != "closed" || o.StateReason != "completed"))
		}
		if dup {
			skipped++
			continue
		}
		todo = append(todo, f)
	}
	return todo, waiting, skipped, nil
}

// fileIssues opens an issue for each finding. With queue they get the
// intake label, so the factory starts on them; while the repo has
// blockers only those are queued, since every other job would fail until
// they are fixed.
func fileIssues(ctx context.Context, gh forge, r *readiness.Report, todo []readiness.Finding, intake string, queue bool) (filed []int, err error) {
	for _, f := range todo {
		labels := []string{pipeline.LabelReadiness}
		if queue && (r.Ready || f.Level == readiness.Blocker) {
			labels = append(labels, intake)
		}
		body := f.Issue.Body + "\n\n---\nFiled by the factory's readiness check of `" + r.Branch + "` at " + r.SHA + ".\n" + marker(f.Issue.Key)
		n, err := gh.CreateIssue(ctx, f.Issue.Title, body, labels)
		if err != nil {
			return filed, err
		}
		filed = append(filed, n)
	}
	return filed, nil
}

// untouched is an issue the check filed that nothing has picked up: its
// only factory label is the check's own.
func untouched(o github.Issue, intake string) bool {
	if !strings.Contains(o.Body, "<!-- factory-readiness: ") {
		return false
	}
	return !slices.ContainsFunc(o.Labels, func(l string) bool {
		return l == intake || (strings.HasPrefix(l, "factory:") && l != pipeline.LabelReadiness)
	})
}

// queueIssues gives issues the intake label.
func queueIssues(ctx context.Context, gh forge, issues []int, intake string) error {
	for _, n := range issues {
		if err := gh.AddLabels(ctx, n, intake); err != nil {
			return err
		}
	}
	return nil
}

// readinessPrefix keeps the check's containers apart from a running
// daemon's, which it would otherwise kill as leftovers.
func readinessPrefix() string { return fmt.Sprintf("ready%d-", os.Getpid()) }

// runReadiness is `factory readiness [-file] [-queue] [owner/name]`.
func runReadiness(ctx context.Context, cfg *config.Config, p *pipeline.Pipeline, args []string) error {
	fs := flag.NewFlagSet("readiness", flag.ContinueOnError)
	file := fs.Bool("file", false, "open an issue for each gap")
	queue := fs.Bool("queue", false, "open the issues with the intake label, so the factory works them (implies -file)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	repos := cfg.RepoNames()
	if fs.NArg() == 1 {
		r, ok := cfg.RepoConf(fs.Arg(0))
		if !ok {
			return fmt.Errorf("%s isn't in the config", fs.Arg(0))
		}
		repos = []string{r.Name}
	} else if fs.NArg() > 1 {
		return usage()
	}
	p.JobPrefix = readinessPrefix()
	var failed []string
	for _, repo := range repos {
		if err := readinessFor(ctx, cfg, p, repo, *file || *queue, *queue); err != nil {
			fmt.Printf("%s: %v\n", repo, err)
			failed = append(failed, repo)
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("couldn't check %s", joinAnd(failed))
	}
	return nil
}

func readinessFor(ctx context.Context, cfg *config.Config, p *pipeline.Pipeline, repo string, file, queue bool) error {
	fmt.Printf("Checking %s...\n", repo)
	r, err := assess(ctx, cfg, p, repo, os.Stdout)
	if err != nil {
		return err
	}
	fmt.Print(r.Text())
	if r.Incomplete {
		return nil
	}
	if !file {
		if n := len(r.Issues()); n > 0 {
			fmt.Printf("Run with -file to open %d issue(s) for the gaps, or -queue to have the factory work them.\n", n)
		}
		return nil
	}
	gh, ok := p.GitHubFor(repo).(forge)
	if !ok {
		return fmt.Errorf("no client for its host")
	}
	todo, waiting, skipped, err := planIssues(ctx, gh, r, cfg.Intake.Label)
	if err != nil {
		return err
	}
	filed, err := fileIssues(ctx, gh, r, todo, cfg.Intake.Label, queue)
	if err != nil {
		return err
	}
	fmt.Printf("Opened %d issue(s) on %s%s.\n", len(filed), repo, skippedNote(skipped))
	if !queue || len(waiting) == 0 {
		return nil
	}
	if err := queueIssues(ctx, gh, waiting, cfg.Intake.Label); err != nil {
		return err
	}
	fmt.Printf("Labeled %d issue(s) filed earlier %s, now that the blockers are fixed.\n", len(waiting), cfg.Intake.Label)
	return nil
}

func skippedNote(n int) string {
	if n == 0 {
		return ""
	}
	return fmt.Sprintf(" (%d already filed)", n)
}

// setupReadiness runs the readiness check on each repo and offers to file
// the work that makes it verifiable.
func setupReadiness(ctx context.Context, p *prompter, cfg *config.Config, client clientFor) {
	rn, err := newRunner(cfg, "")
	if err != nil {
		p.say("Skipping the readiness check: %v", err)
		return
	}
	pl := &pipeline.Pipeline{Cfg: cfg, Runner: rn, JobPrefix: readinessPrefix(), ClientsFor: func(repo string) pipeline.RepoClients {
		gh, ts, err := client(repo)
		if err != nil {
			gh, ts = newForge(cfg, repo, github.StaticToken("")), nil
		}
		return pipeline.RepoClients{GH: gh, Git: repoGit(cfg, repo, ts)}
	}}
	p.say("This clones each repo, runs its install and checks on its base branch, runs the")
	p.say("tests twice to catch flakiness, and looks for end-to-end tests, a feature map and CI.")
	if !p.yes("Check now? It can take a few minutes per repo", true) {
		p.say("Run it later with: ./factory readiness")
		return
	}
	for _, repo := range cfg.RepoNames() {
		p.say("\nChecking %s...", repo)
		r, err := assess(ctx, cfg, pl, repo, p.out)
		if err != nil {
			p.say("Couldn't check %s: %v", repo, err)
			continue
		}
		p.say("%s", strings.TrimRight(r.Text(), "\n"))
		if r.Incomplete {
			p.say("Fix that and run: ./factory readiness %s", repo)
			continue
		}
		if len(r.Issues()) == 0 {
			continue
		}
		gh, _, err := client(repo)
		if err != nil {
			p.say("%s: %v", repo, err)
			continue
		}
		todo, waiting, skipped, err := planIssues(ctx, gh, r, cfg.Intake.Label)
		if err != nil {
			p.say("Couldn't list %s's issues: %v", repo, err)
			continue
		}
		if len(waiting) > 0 && p.yes(fmt.Sprintf("%d issue(s) filed earlier wait on blockers that are now fixed. Label them %s so the factory starts on them?", len(waiting), cfg.Intake.Label), true) {
			if err := queueIssues(ctx, gh, waiting, cfg.Intake.Label); err != nil {
				p.say("Labeling them failed: %v", err)
			}
		}
		if len(todo) == 0 {
			p.say("Issues for these are already filed on %s.", repo)
			continue
		}
		if r.Ready {
			p.say("The factory can start now. These would verify more of what it changes:")
		} else {
			p.say("Every job would fail until the blockers are fixed. The factory can fix them first:")
		}
		for i, f := range todo {
			p.say("  %d. %s", i+1, f.Issue.Title)
		}
		if skipped > 0 {
			p.say("(%d more already filed.)", skipped)
		}
		if !p.yes(fmt.Sprintf("Open these %d issues on %s?", len(todo), repo), true) {
			p.say("Open them later with: ./factory readiness -file %s", repo)
			continue
		}
		ask := fmt.Sprintf("Label them %s so the factory starts on them when it runs?", cfg.Intake.Label)
		if !r.Ready {
			ask = fmt.Sprintf("Label the blockers %s so the factory starts on them when it runs? The rest are filed for later", cfg.Intake.Label)
		}
		queue := p.yes(ask, true)
		filed, err := fileIssues(ctx, gh, r, todo, cfg.Intake.Label, queue)
		if err != nil {
			p.say("Opening issues on %s failed: %v", repo, err)
			continue
		}
		p.say("Opened %d issue(s) on %s.", len(filed), repo)
		if queue && !r.Ready {
			p.say("Once the blockers are fixed, run ./factory readiness -queue %s to queue the rest.", repo)
		}
	}
}
