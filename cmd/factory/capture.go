package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/claptimes5/factory/internal/config"
	"github.com/claptimes5/factory/internal/eval"
	"github.com/claptimes5/factory/internal/pipeline"
)

// runCapture turns a merged PR into a replay case: the issue it closed,
// frozen at the commit the fix was built on, with the fix's own tests as
// the hidden acceptance files.
func runCapture(ctx context.Context, cfg *config.Config, args []string) error {
	fs := flag.NewFlagSet("eval capture", flag.ContinueOnError)
	set := fs.String("set", "evals", "directory of replay cases")
	name := fs.String("name", "", "the case's directory name; default from the issue title")
	accept := fs.String("accept", "", "the acceptance command; default the repo's test gate")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: factory eval capture [-set evals] [-name NAME] [-accept CMD] PR")
	}
	repo, n, err := parseIssue(cfg, fs.Arg(0))
	if err != nil {
		return fmt.Errorf("capture: %w", err)
	}
	ts, err := tokens(cfg, repo)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.Path(cfg.DataDir), 0o755); err != nil {
		return err
	}
	work, err := os.MkdirTemp(cfg.Path(cfg.DataDir), "capture-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	c, err := capture(ctx, cfg, repo, n, newForge(cfg, repo, ts), repoGit(cfg, repo, ts), work, cfg.Path(*set), *name, *accept)
	if err != nil {
		return err
	}
	fmt.Printf("Wrote %s\n", c.Dir)
	for _, n := range c.Notes {
		fmt.Println("Note: " + n)
	}
	if len(c.Tests) == 0 {
		fmt.Println("The fix changed no test files, so accept/ is empty and the case only checks the suite still passes.\nAdd a test that fails on the factory's version to accept/ to make the case catch this again.")
		return nil
	}
	fmt.Printf("Acceptance files from the fix: %s\n", strings.Join(c.Tests, ", "))
	return nil
}

func capture(ctx context.Context, cfg *config.Config, repo string, n int, f forge, git *pipeline.Git, work, set, name, accept string) (*eval.Captured, error) {
	rc, ok := cfg.RepoConf(repo)
	if !ok {
		// eval runs a case against a configured repo's remote and gates.
		return nil, fmt.Errorf("capture: %s isn't in the config", repo)
	}
	pr, err := f.GetPR(ctx, n)
	if err != nil {
		return nil, fmt.Errorf("capture: PR %d: %w", n, err)
	}
	if !pr.Merged {
		return nil, fmt.Errorf("capture: PR %d isn't merged; a case needs a known-good fix", n)
	}
	if accept == "" {
		accept = cfg.Resolve(rc).Gates.Commands["test"]
		if accept == "" {
			return nil, fmt.Errorf("capture: %s has no test gate; pass -accept", repo)
		}
	}
	var notes []string
	title, body := pr.Title, pr.Body
	if is := eval.ClosedIssue(pr.Body); is > 0 {
		i, err := f.GetIssue(ctx, is)
		switch {
		case err != nil:
			notes = append(notes, fmt.Sprintf("couldn't read issue #%d (%v), so the task is the PR's own title and description; edit case.yaml if they give the fix away", is, err))
		case i.IsPR:
			notes = append(notes, fmt.Sprintf("#%d is a PR, not an issue, so the task is this PR's own title and description; edit case.yaml if they give the fix away", is))
		default:
			title, body = i.Title, i.Body
		}
	} else {
		notes = append(notes, "the PR names no issue it closes, so the task is its own title and description; edit case.yaml if they give the fix away")
	}
	if name == "" {
		name = eval.CaseName(fmt.Sprintf("pr-%d", n), title)
	}
	if eval.Exists(set, name) {
		return nil, fmt.Errorf("capture: %s already exists; pass -name", filepath.Join(set, name))
	}

	bare := filepath.Join(work, "repo.git")
	if err := git.CloneBare(ctx, bare); err != nil {
		return nil, err
	}
	ref := fmt.Sprintf("refs/pull/%d/head", n)
	if rc.GitLab() {
		ref = fmt.Sprintf("refs/merge-requests/%d/head", n)
	}
	head, err := git.FetchRef(ctx, bare, ref)
	if err != nil && pr.Head.SHA != "" {
		// GitLab can clean up an old merge request's refs; the commit
		// itself is still there when it was merged.
		head, err = git.FetchRef(ctx, bare, pr.Head.SHA)
	}
	if err != nil {
		return nil, err
	}
	if pr.Head.SHA != "" && head != pr.Head.SHA {
		return nil, fmt.Errorf("capture: %s is %s, but the PR's head is %s", ref, head, pr.Head.SHA)
	}
	// The fix was built where its branch left the base branch: the merge
	// base with the base branch just before the merge (the merge commit's
	// first parent; for a squash or rebase too). A fast-forward made no
	// commit of its own, so there it's the merge base with the PR's base.
	from := pr.Base.SHA
	if pr.MergeCommit != "" && pr.MergeCommit != head && !git.IsAncestor(ctx, bare, head, pr.MergeCommit+"^1") {
		from = pr.MergeCommit + "^1"
	}
	if from == "" {
		return nil, fmt.Errorf("capture: can't tell what PR %d was built on", n)
	}
	base, err := git.MergeBase(ctx, bare, head, from)
	if err != nil {
		return nil, err
	}
	if base == head {
		return nil, fmt.Errorf("capture: can't tell what PR %d was built on: its head is already on the base branch", n)
	}
	caseRepo := ""
	if len(cfg.Repos) > 1 {
		caseRepo = rc.Name
	}
	c, err := eval.Capture(ctx, git, bare, set, name, eval.Fix{Repo: caseRepo, Source: prRef(cfg, repo, n), Title: title, Body: body, Base: base, Head: head, Accept: accept})
	if c != nil {
		c.Notes = append(notes, c.Notes...)
	}
	return c, err
}

// prRef names a PR for people, like issueRef.
func prRef(cfg *config.Config, repo string, n int) string {
	if repoFor(cfg, repo).GitLab() {
		return strings.Replace(issueRef(cfg, repo, n), "#", "!", 1)
	}
	return "PR " + issueRef(cfg, repo, n)
}
