package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
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

// fileIssues opens the report's issues on GitHub, skipping any the check
// filed before that are still open. With queue they get the intake label,
// so the factory starts on them.
func fileIssues(ctx context.Context, gh *github.Client, r *readiness.Report, intake string, queue bool) (filed []int, skipped int, err error) {
	open, err := gh.ListIssues(ctx, pipeline.LabelReadiness)
	if err != nil {
		return nil, 0, err
	}
	labels := []string{pipeline.LabelReadiness}
	if queue {
		labels = append(labels, intake)
	}
	for _, is := range r.Issues() {
		dup := false
		for _, o := range open {
			dup = dup || strings.EqualFold(o.Title, is.Title)
		}
		if dup {
			skipped++
			continue
		}
		body := is.Body + "\n\n---\nFiled by the factory's readiness check of `" + r.Branch + "` at " + r.SHA + "."
		n, err := gh.CreateIssue(ctx, is.Title, body, labels)
		if err != nil {
			return filed, skipped, err
		}
		filed = append(filed, n)
	}
	return filed, skipped, nil
}

// runReadiness is `factory readiness [owner/name] [-file] [-queue]`.
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
	for _, repo := range repos {
		fmt.Printf("Checking %s...\n", repo)
		r, err := assess(ctx, cfg, p, repo, os.Stdout)
		if err != nil {
			return err
		}
		fmt.Print(r.Text())
		if !*file && !*queue {
			if n := len(r.Issues()); n > 0 {
				fmt.Printf("Run with -file to open %d issue(s) for the gaps, or -queue to have the factory work them.\n", n)
			}
			continue
		}
		gh, ok := p.GitHubFor(repo).(*github.Client)
		if !ok {
			return fmt.Errorf("%s: no GitHub client", repo)
		}
		filed, skipped, err := fileIssues(ctx, gh, r, cfg.Intake.Label, *queue)
		if err != nil {
			return err
		}
		fmt.Printf("Opened %d issue(s) on %s%s.\n", len(filed), repo, skippedNote(skipped))
	}
	return nil
}

func skippedNote(n int) string {
	if n == 0 {
		return ""
	}
	return fmt.Sprintf(" (%d already open)", n)
}

// setupReadiness runs the readiness check on each repo and offers to file
// the work that makes it verifiable.
func setupReadiness(ctx context.Context, p *prompter, cfg *config.Config, client clientFor) {
	pl := &pipeline.Pipeline{Cfg: cfg, Runner: newRunner(cfg), ClientsFor: func(repo string) pipeline.RepoClients {
		gh, err := client(repo)
		if err != nil {
			gh = &github.Client{Repo: repo, BaseURL: githubAPI, Tokens: github.StaticToken("")}
		}
		return pipeline.RepoClients{GH: gh, Git: repoGit(cfg, repo, gh.Tokens)}
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
		issues := r.Issues()
		if len(issues) == 0 {
			continue
		}
		if r.Ready {
			p.say("The factory can start now. These would verify more of what it changes:")
		} else {
			p.say("Every job would fail until the blockers are fixed. The factory can fix them first:")
		}
		for i, is := range issues {
			p.say("  %d. %s", i+1, is.Title)
		}
		if !p.yes(fmt.Sprintf("Open these %d issues on %s?", len(issues), repo), true) {
			p.say("Open them later with: ./factory readiness -file %s", repo)
			continue
		}
		queue := p.yes(fmt.Sprintf("Label them %s so the factory starts on them when it runs?", cfg.Intake.Label), true)
		gh, err := client(repo)
		if err != nil {
			p.say("%s: %v", repo, err)
			continue
		}
		filed, skipped, err := fileIssues(ctx, gh, r, cfg.Intake.Label, queue)
		if err != nil {
			p.say("Opening issues on %s failed: %v", repo, err)
			continue
		}
		p.say("Opened %d issue(s) on %s%s.", len(filed), repo, skippedNote(skipped))
	}
}

// repoGit is how the factory reaches a repo with git.
func repoGit(cfg *config.Config, repo string, ts github.TokenSource) *pipeline.Git {
	return &pipeline.Git{RemoteURL: "https://github.com/" + repo + ".git", Tokens: ts, Author: "factory[bot] <factory@users.noreply.github.com>", AskPassDir: cfg.Path(cfg.DataDir)}
}
