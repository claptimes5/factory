package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/claptimes5/factory/internal/config"
	"github.com/claptimes5/factory/internal/github"
	"github.com/claptimes5/factory/internal/pipeline"
	"github.com/claptimes5/factory/internal/readiness"
	"github.com/claptimes5/factory/internal/state"
	"github.com/claptimes5/factory/internal/store"
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
	// Removing the factory, from any process, waits while this runs.
	unlock, err := runLock(dir)
	if err != nil {
		return nil, err
	}
	defer unlock()
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
	r := readiness.Assess(ctx, readiness.Input{Repo: rc, SHA: ws.BaseSHA, Dir: export, Claude: p.ClaudeBuilds(),
		Exec: func(ctx context.Context, dir, cmd string, install bool) (int, string, error) {
			fmt.Fprintf(out, "  running %s\n", cmd)
			return p.Exec(ctx, repo, dir, cmd, install)
		}})
	if _, err := readiness.Save(readinessDir(cfg), r); err != nil {
		return r, err
	}
	return r, nil
}

// readinessDir holds each repo's latest report, which the dashboard and
// status read.
func readinessDir(cfg *config.Config) string {
	return filepath.Join(cfg.Path(cfg.DataDir), "readiness")
}

// linkFiled records on each of the report's findings the issue filed for
// it, so the dashboard can link it and follow the factory's work on it.
// Of several, an open one wins, then the newest.
func linkFiled(r *readiness.Report, filed []github.Issue) {
	r.Filed = nil
	for _, o := range filed {
		r.Filed = append(r.Filed, o.Number)
	}
	for _, f := range r.Findings {
		if f.Issue == nil {
			continue
		}
		var best *github.Issue
		for i, o := range filed {
			if !strings.Contains(o.Body, marker(f.Issue.Key)) && !strings.EqualFold(o.Title, f.Issue.Title) {
				continue
			}
			if best == nil {
				best = &filed[i]
				continue
			}
			open, bestOpen := o.State == "open", best.State == "open"
			if open && !bestOpen || open == bestOpen && o.Number > best.Number {
				best = &filed[i]
			}
		}
		if best != nil {
			f.Issue.Number, f.Issue.State = best.Number, best.State
		}
	}
}

// relink reads the issues the check filed on repo and saves the report
// with them linked. A report that can't be linked is still right about
// the repo, so failing to read them only goes to the log.
func relink(ctx context.Context, cfg *config.Config, gh forge, st *store.Store, r *readiness.Report) error {
	filed, err := gh.ListIssuesIn(ctx, pipeline.LabelReadiness, "all")
	if err != nil {
		return fmt.Errorf("reading the issues it filed: %w", err)
	}
	closeFixed(ctx, gh, st, r, filed)
	linkFiled(r, filed)
	_, err = readiness.Save(readinessDir(cfg), r)
	return err
}

// closeFixed closes each open issue the check filed for a required check
// that passes on the base branch now, and the open issues it was split
// into: they're done. The factory stops working on their items when it
// sees them closed. filed is updated to match.
func closeFixed(ctx context.Context, gh forge, st *store.Store, r *readiness.Report, filed []github.Issue) {
	closer, ok := gh.(interface {
		CloseIssue(ctx context.Context, n int) error
	})
	if r.Incomplete || !ok {
		return
	}
	passed := map[string]bool{}
	for _, run := range r.Runs {
		passed[run.Gate] = run.Passed
	}
	sha := r.SHA
	if len(sha) > 7 {
		sha = sha[:7]
	}
	for i, o := range filed {
		m := markerRE.FindStringSubmatch(o.Body)
		if o.State != "open" || m == nil {
			continue
		}
		gate, ok := strings.CutPrefix(m[1], "check:")
		if !ok || !passed[gate] {
			continue
		}
		if err := closer.CloseIssue(ctx, o.Number); err != nil {
			slog.Warn("readiness", "repo", r.Repo, "close", o.Number, "err", err)
			continue
		}
		filed[i].State = "closed"
		why := fmt.Sprintf("`%s` passes on `%s` now, at %s", gate, r.Branch, sha)
		if err := gh.Comment(ctx, o.Number, "**Factory:** "+why+", so this is done."); err != nil {
			slog.Warn("readiness", "repo", r.Repo, "comment", o.Number, "err", err)
		}
		if st == nil {
			continue
		}
		it, err := st.ItemByIssue(ctx, r.Repo, o.Number)
		if err != nil {
			continue
		}
		items, _ := st.ListItems(ctx)
		for _, c := range items {
			if c.ParentID == nil || *c.ParentID != it.ID || c.Stage == state.Done {
				continue
			}
			if is, err := gh.GetIssue(ctx, c.IssueNumber); err != nil || is.State != "open" {
				continue
			}
			if err := closer.CloseIssue(ctx, c.IssueNumber); err != nil {
				slog.Warn("readiness", "repo", r.Repo, "close", c.IssueNumber, "err", err)
				continue
			}
			_ = gh.Comment(ctx, c.IssueNumber, fmt.Sprintf("**Factory:** closed with #%d: %s.", o.Number, why))
		}
	}
}

// markerRE finds the key in an issue the check filed.
var markerRE = regexp.MustCompile(`<!-- factory-readiness: (\S+) -->`)

// recheckReadiness is the dashboard's Check again: the readiness check on
// repo's base branch, with its issues linked. It only reads: it files
// and labels nothing. Its containers are named apart from the daemon's
// jobs, which would otherwise take them for leftovers.
func recheckReadiness(ctx context.Context, cfg *config.Config, daemon *pipeline.Pipeline, repo string) error {
	pl := &pipeline.Pipeline{Cfg: cfg, Runner: daemon.Runner, JobPrefix: readinessPrefix(), ClientsFor: daemon.ClientsFor}
	r, err := assess(ctx, cfg, pl, repo, io.Discard)
	if err != nil {
		return err
	}
	if gh, ok := daemon.GitHubFor(repo).(forge); ok {
		if err := relink(ctx, cfg, gh, daemon.Store, r); err != nil {
			slog.Warn("readiness", "repo", repo, "err", err)
		}
	}
	return nil
}

// relinkSaved links each saved report to the issues filed for it, without
// running the check again: a report saved before the check recorded its
// issues, or before they were filed elsewhere, still shows them.
func relinkSaved(ctx context.Context, cfg *config.Config, p *pipeline.Pipeline) {
	for _, repo := range cfg.RepoNames() {
		r, err := readiness.Load(readinessDir(cfg), repo)
		gh, ok := p.GitHubFor(repo).(forge)
		if err != nil || r == nil || !ok {
			continue
		}
		if err := relink(ctx, cfg, gh, p.Store, r); err != nil {
			slog.Warn("readiness", "repo", repo, "err", err)
		}
	}
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
	linkFiled(r, filed)
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
		// f.Issue is the report's own, so this links it there.
		f.Issue.Number, f.Issue.State = n, "open"
		r.Filed = append(r.Filed, n)
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
	gh, ok := p.GitHubFor(repo).(forge)
	if !file {
		if ok {
			if err := relink(ctx, cfg, gh, p.Store, r); err != nil {
				fmt.Printf("Couldn't link the report to its issues: %v\n", err)
			}
		}
		if n := len(r.Issues()); n > 0 {
			fmt.Printf("Run with -file to open %d issue(s) for the gaps, or -queue to have the factory work them.\n", n)
		}
		return nil
	}
	if !ok {
		return fmt.Errorf("no client for its host")
	}
	msg, err := fileReport(ctx, cfg, gh, r, queue)
	if msg != "" {
		fmt.Println(msg)
	}
	return err
}

// fileReport files an issue for each of the report's gaps that has none,
// and with queue labels the ones filed earlier, now the repo is ready. It
// saves the report with its issues linked and says what it did.
func fileReport(ctx context.Context, cfg *config.Config, gh forge, r *readiness.Report, queue bool) (string, error) {
	todo, waiting, skipped, err := planIssues(ctx, gh, r, cfg.Intake.Label)
	if err != nil {
		return "", err
	}
	filed, err := fileIssues(ctx, gh, r, todo, cfg.Intake.Label, queue)
	if _, serr := readiness.Save(readinessDir(cfg), r); err == nil {
		err = serr
	}
	if err != nil {
		return "", err
	}
	msg := fmt.Sprintf("Opened %d issue(s) on %s%s.", len(filed), r.Repo, skippedNote(skipped))
	if !queue || len(waiting) == 0 {
		return msg, nil
	}
	if err := queueIssues(ctx, gh, waiting, cfg.Intake.Label); err != nil {
		return msg, err
	}
	return msg + fmt.Sprintf("\nLabeled %d issue(s) filed earlier %s, now that the blockers are fixed.", len(waiting), cfg.Intake.Label), nil
}

// fileReadiness is the dashboard's File issues (and with queue, File and
// start work): the check again on repo, then its issues filed as
// `factory readiness -file` (or -queue) does.
func fileReadiness(ctx context.Context, cfg *config.Config, daemon *pipeline.Pipeline, repo string, queue bool) (string, error) {
	pl := &pipeline.Pipeline{Cfg: cfg, Runner: daemon.Runner, JobPrefix: readinessPrefix(), ClientsFor: daemon.ClientsFor}
	r, err := assess(ctx, cfg, pl, repo, io.Discard)
	if err != nil {
		return "", err
	}
	if r.Incomplete {
		return "The check couldn't finish, so nothing was filed. Fix what it found, then try again.", nil
	}
	gh, ok := daemon.GitHubFor(repo).(forge)
	if !ok {
		return "", fmt.Errorf("no client for %s's host", repo)
	}
	msg, err := fileReport(ctx, cfg, gh, r, queue)
	return strings.ReplaceAll(msg, "\n", " "), err
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
		if _, err := readiness.Save(readinessDir(cfg), r); err != nil {
			p.say("Couldn't save the report with its issues: %v", err)
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
		if _, serr := readiness.Save(readinessDir(cfg), r); err == nil && serr != nil {
			p.say("Couldn't save the report with its issues: %v", serr)
		}
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

// readinessLines are status's lines on how ready each repo is, from the
// readiness check's latest reports; none when no repo was ever checked.
func readinessLines(ctx context.Context, st *store.Store, cfg *config.Config) []string {
	var out []string
	checked := false
	for _, repo := range cfg.RepoNames() {
		r, err := readiness.Load(readinessDir(cfg), repo)
		if err != nil {
			out = append(out, fmt.Sprintf("setup %s:\tcan't read its readiness report: %v", repo, err))
			continue
		}
		if r == nil {
			out = append(out, fmt.Sprintf("setup %s:\tnot checked yet; run ./factory readiness %s", repo, repo))
			continue
		}
		checked = true
		done, total := r.Progress()
		line := fmt.Sprintf("setup %s:\t%d of %d setup steps done, ", repo, done, total)
		var blockers []string
		for _, s := range r.Steps() {
			for _, f := range s.Blockers {
				b := pipeline.SafeText(strings.ReplaceAll(f.Title, "`", ""), 120)
				if f.Issue != nil && f.Issue.Number > 0 {
					where := "not queued"
					if it, err := st.ItemByIssue(ctx, repo, f.Issue.Number); err == nil {
						where = strings.ReplaceAll(string(it.Stage), "_", " ")
					} else if f.Issue.State == "closed" {
						where = "closed"
					}
					b += fmt.Sprintf(" (%s, %s)", issueRef(cfg, repo, f.Issue.Number), where)
				}
				blockers = append(blockers, b)
			}
		}
		switch {
		case r.Incomplete:
			line += "the check couldn't finish: " + strings.Join(blockers, "; ") + "; fix that and run ./factory readiness " + repo
		case !r.Ready:
			line += "not ready: " + strings.Join(blockers, "; ")
		default:
			line += fmt.Sprintf("ready; add the %s label to an issue to start", cfg.Intake.Label)
		}
		out = append(out, line)
	}
	if !checked {
		return nil
	}
	return out
}
