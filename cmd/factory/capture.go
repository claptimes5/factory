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
	"github.com/claptimes5/factory/internal/store"
)

// runCapture turns a merged PR into a replay case: the issue it closed,
// frozen at the commit the fix was built on, with the fix's own tests as
// the hidden acceptance files. With -review it writes reviewer cases
// instead, and with -reverts one for each factory PR the revert watch saw
// reverted.
func runCapture(ctx context.Context, cfg *config.Config, st *store.Store, args []string) error {
	fs := flag.NewFlagSet("eval capture", flag.ContinueOnError)
	set := fs.String("set", "evals", "directory of replay cases")
	name := fs.String("name", "", "the case's directory name; default from the issue title")
	accept := fs.String("accept", "", "the acceptance command; default the repo's test gate")
	review := fs.Bool("review", false, "write reviewer cases: a clean one and bugs planted in it, or a revert case if the factory saw it reverted")
	reverts := fs.Bool("reverts", false, "write a reviewer revert case for every reverted factory PR not captured yet")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *reverts {
		if fs.NArg() != 0 {
			return errors.New("usage: factory eval capture -reverts [-set evals]")
		}
		return captureReverts(ctx, cfg, st, cfg.Path(*set))
	}
	if fs.NArg() != 1 {
		return errors.New("usage: factory eval capture [-set evals] [-name NAME] [-accept CMD] [-review] PR")
	}
	repo, n, err := parseIssue(cfg, fs.Arg(0))
	if err != nil {
		return fmt.Errorf("capture: %w", err)
	}
	ts, err := tokens(cfg, repo)
	if err != nil {
		return err
	}
	work, err := captureWork(cfg)
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	if *review {
		revert, err := reverted(ctx, st, repo, n)
		if err != nil {
			return err
		}
		if revert && *name == "" {
			*name = revertCaseName(cfg, store.FactoryMerge{Repo: repo, PR: n}) // as -reverts names it
		}
		c, err := captureReview(ctx, cfg, repo, n, newForge(cfg, repo, ts), repoGit(cfg, repo, ts), work, cfg.Path(*set), *name, revert)
		printReview(c)
		return err
	}
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

func captureWork(cfg *config.Config) (string, error) {
	if err := os.MkdirAll(cfg.Path(cfg.DataDir), 0o755); err != nil {
		return "", err
	}
	return os.MkdirTemp(cfg.Path(cfg.DataDir), "capture-")
}

func printReview(c *eval.ReviewCaptured) {
	if c == nil {
		return
	}
	for _, d := range c.Dirs {
		fmt.Printf("Wrote %s\n", d)
	}
	for _, n := range c.Notes {
		fmt.Println("Note: " + n)
	}
}

// reverted says whether the revert watch saw the factory PR reverted.
func reverted(ctx context.Context, st *store.Store, repo string, n int) (bool, error) {
	ms, err := st.FactoryMerges(ctx, repo, "")
	if err != nil {
		return false, err
	}
	for _, m := range ms {
		if m.PR == n && m.RevertedAt != "" {
			return true, nil
		}
	}
	return false, nil
}

// captureReverts writes a revert case for each reverted factory PR that
// has none yet. One that can't be captured is said and skipped.
func captureReverts(ctx context.Context, cfg *config.Config, st *store.Store, set string) error {
	ms, err := st.FactoryMerges(ctx, "", "")
	if err != nil {
		return err
	}
	found := 0
	for _, m := range ms {
		if m.RevertedAt == "" {
			continue
		}
		if _, ok := cfg.RepoConf(m.Repo); !ok {
			continue
		}
		found++
		name := revertCaseName(cfg, m)
		if eval.Exists(set, name+"-revert") {
			continue
		}
		ts, err := tokens(cfg, m.Repo)
		if err != nil {
			return err
		}
		work, err := captureWork(cfg)
		if err != nil {
			return err
		}
		c, err := captureReview(ctx, cfg, m.Repo, m.PR, newForge(cfg, m.Repo, ts), repoGit(cfg, m.Repo, ts), work, set, name, true)
		_ = os.RemoveAll(work)
		printReview(c)
		if err != nil {
			fmt.Printf("Skipped %s: %v\n", prRef(cfg, m.Repo, m.PR), err)
		}
	}
	if found == 0 {
		fmt.Println("The revert watch has seen no factory PR reverted.")
	}
	return nil
}

// revertCaseName names a reverted PR's case by its number, so a second
// run finds it, with the repo's short name when there are several.
func revertCaseName(cfg *config.Config, m store.FactoryMerge) string {
	if len(cfg.Repos) > 1 {
		return fmt.Sprintf("pr-%s-%d", strings.ToLower(config.ShortName(m.Repo)), m.PR)
	}
	return fmt.Sprintf("pr-%d", m.PR)
}

func capture(ctx context.Context, cfg *config.Config, repo string, n int, f forge, git *pipeline.Git, work, set, name, accept string) (*eval.Captured, error) {
	rc, ok := cfg.RepoConf(repo)
	if !ok {
		// eval runs a case against a configured repo's remote and gates.
		return nil, fmt.Errorf("capture: %s isn't in the config", repo)
	}
	if accept == "" {
		accept = cfg.Resolve(rc).Gates.Commands["test"]
		if accept == "" {
			return nil, fmt.Errorf("capture: %s has no test gate; pass -accept", repo)
		}
	}
	ch, err := prChange(ctx, cfg, repo, n, f, git, work, set, name, true)
	if err != nil {
		return nil, err
	}
	ch.fix.Accept = accept
	c, err := eval.Capture(ctx, git, ch.bare, set, ch.name, ch.fix)
	if c != nil {
		c.Notes = append(ch.notes, c.Notes...)
	}
	return c, err
}

// captureReview writes reviewer cases from a merged PR (see
// eval.CaptureReview): a revert case when revert is set, else a clean
// case and planted ones.
func captureReview(ctx context.Context, cfg *config.Config, repo string, n int, f forge, git *pipeline.Git, work, set, name string, revert bool) (*eval.ReviewCaptured, error) {
	if _, ok := cfg.RepoConf(repo); !ok {
		return nil, fmt.Errorf("capture: %s isn't in the config", repo)
	}
	ch, err := prChange(ctx, cfg, repo, n, f, git, work, set, name, false)
	if err != nil {
		return nil, err
	}
	c, err := eval.CaptureReview(ctx, git, ch.bare, set, ch.name, ch.fix, revert)
	if c != nil {
		c.Notes = append(ch.notes, c.Notes...)
	}
	return c, err
}

// change is a merged PR, fetched into bare, as a case starts from it.
type change struct {
	bare, name string
	fix        eval.Fix
	notes      []string
}

// prChange reads a merged PR: the task (the issue it closed, else its
// own title and body), and the commits it was built on and merged as.
// With unique set, a case already called name is an error.
func prChange(ctx context.Context, cfg *config.Config, repo string, n int, f forge, git *pipeline.Git, work, set, name string, unique bool) (*change, error) {
	rc, _ := cfg.RepoConf(repo)
	pr, err := f.GetPR(ctx, n)
	if err != nil {
		return nil, fmt.Errorf("capture: PR %d: %w", n, err)
	}
	if !pr.Merged {
		return nil, fmt.Errorf("capture: PR %d isn't merged; a case needs a known-good fix", n)
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
	if unique && eval.Exists(set, name) {
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
	return &change{bare: bare, name: name, notes: notes,
		fix: eval.Fix{Repo: caseRepo, Source: prRef(cfg, repo, n), Title: title, Body: body, Base: base, Head: head}}, nil
}

// prRef names a PR for people, like issueRef.
func prRef(cfg *config.Config, repo string, n int) string {
	if repoFor(cfg, repo).GitLab() {
		return strings.Replace(issueRef(cfg, repo, n), "#", "!", 1)
	}
	return "PR " + issueRef(cfg, repo, n)
}
