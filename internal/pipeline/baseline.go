package pipeline

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// A lint baseline makes a lint check that fails on the base branch pass
// there by recording today's offenses as accepted, so only new ones fail
// a change. The factory makes it without a model: it runs the linter's
// own baseline command on the base branch and opens a pull request with
// just the files that command writes, for a person to merge.

// BaselineBranch is the branch a lint baseline is pushed to.
const BaselineBranch = "factory/lint-baseline"

// linter is one linter the factory can baseline.
type linter struct {
	name  string
	cmd   string   // writes the baseline, given the lint command's runner
	files []string // all it may change
}

// baselineFor is how to baseline the lint command, or nil when its
// linter has no baseline the factory knows.
func baselineFor(lint string) *linter {
	if !strings.Contains(lint, "rubocop") {
		return nil
	}
	runner := "bundle exec rubocop"
	if strings.HasPrefix(strings.TrimSpace(lint), "bin/rubocop") {
		runner = "bin/rubocop"
	}
	// --no-exclude-limit lists every file, so no cop is turned off for
	// the whole repo; no counts or timestamp, so a rerun changes nothing.
	return &linter{name: "RuboCop", cmd: runner + " --auto-gen-config --no-exclude-limit --no-offense-counts --no-auto-gen-timestamp",
		files: []string{".rubocop.yml", ".rubocop_todo.yml"}}
}

// CanBaseline says whether the repo's lint check is one the factory can
// baseline.
func (p *Pipeline) CanBaseline(repo string) bool {
	g := p.repoConf(repo).Gates
	return g != nil && baselineFor(g.Commands["lint"]) != nil
}

// LintBaseline records the lint offenses on repo's base branch as a
// baseline and opens a pull request with it; it returns the pull
// request's address. The lint check must pass with the baseline, or
// nothing is pushed.
func (p *Pipeline) LintBaseline(ctx context.Context, repo string) (string, error) {
	rc := p.repoConf(repo)
	if rc.Gates == nil {
		return "", errors.New("the repo has no checks of its own")
	}
	lint := rc.Gates.Commands["lint"]
	l := baselineFor(lint)
	if l == nil {
		return "", fmt.Errorf("the factory can baseline RuboCop for now, not `%s`", lint)
	}
	data := p.Cfg.Path(p.Cfg.DataDir)
	if err := os.MkdirAll(data, 0o755); err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp(data, "readiness-baseline-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	g := p.git(repo)
	// A fresh branch off the base, with a clean clone to commit from.
	ws, err := g.Prepare(ctx, dir, rc.BaseBranch, BaselineBranch, true, true)
	if err != nil {
		return "", fmt.Errorf("cloning %s: %w", repo, err)
	}
	// The linter runs in an export, so what install leaves behind never
	// reaches the commit.
	export := filepath.Join(dir, "export")
	if err := g.Export(ctx, ws.Repo, ws.BaseSHA, export); err != nil {
		return "", err
	}
	if err := p.CopyFiles(ctx, repo, export); err != nil {
		return "", err
	}
	if rc.Gates.Install != "" {
		if code, out, err := p.Exec(ctx, repo, export, rc.Gates.Install, true); err != nil || code != 0 {
			return "", runErr("install", code, out, err)
		}
	}
	if code, out, err := p.Exec(ctx, repo, export, l.cmd, false); err != nil || code > 1 {
		return "", runErr(l.name+"'s baseline", code, out, err) // 1 is "offenses found", as it records them
	}
	if code, out, err := p.Exec(ctx, repo, export, lint, false); err != nil || code != 0 {
		return "", fmt.Errorf("`%s` still fails with the baseline, so nothing was pushed: %w", lint, runErr("lint", code, out, err))
	}
	for _, f := range l.files {
		b, err := os.ReadFile(filepath.Join(export, f))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		if err := os.WriteFile(filepath.Join(ws.Work, f), b, 0o644); err != nil {
			return "", err
		}
	}
	sha, changed, err := g.Ingest(ctx, ws, "Accept today's "+l.name+" offenses as a baseline\n\nMade by the factory with `"+l.cmd+"`.")
	if err != nil {
		return "", err
	}
	if !changed {
		return "", fmt.Errorf("the baseline is already on %s", rc.BaseBranch)
	}
	if err := g.Push(ctx, ws.Repo, sha, BaselineBranch, true); err != nil {
		return "", fmt.Errorf("pushing %s: %w", BaselineBranch, err)
	}
	gh := p.gh(repo)
	if pr, err := gh.FindPR(ctx, BaselineBranch); err == nil && pr != nil {
		return pr.URL, nil // the push updated it
	}
	pr, err := gh.CreatePR(ctx, "Accept today's "+l.name+" offenses as a baseline", baselineBody(l, lint, rc.BaseBranch), BaselineBranch, rc.BaseBranch, false)
	if err != nil {
		return "", err
	}
	return pr.URL, nil
}

func baselineBody(l *linter, lint, base string) string {
	return fmt.Sprintf("`%s` fails on `%s` today, and the factory requires it to pass, so every change would fail it.\n\n"+
		"This records today's offenses as accepted with `%s`, so `%s` passes and only new offenses fail a change. "+
		"It changes only %s; no code changes, and no cop is turned off for the whole repo.\n\n"+
		"To pay the old offenses down later, delete a cop's entry from `.rubocop_todo.yml` and fix what it flags, one cop at a time.\n\n"+
		"_Opened by the factory from its Setup page. It ran the baseline on `%s` and checked `%s` passes with it._",
		lint, base, l.cmd, lint, "`"+strings.Join(l.files, "` and `")+"`", base, lint)
}

func runErr(what string, code int, out string, err error) error {
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	return fmt.Errorf("%s exited %d:\n%s", what, code, tail(out, 2000))
}
