package pipeline

import (
	"context"
	"fmt"
	"strings"

	"github.com/claptimes5/factory/internal/readiness"
	"github.com/claptimes5/factory/internal/state"
	"github.com/claptimes5/factory/internal/store"
)

// Lint that fails across the whole base branch isn't a builder's to fix:
// a change that fixes years of offenses is too big for one build, and
// pieces of it each fail a lint of the whole repo. So when the factory
// picks up the issue the readiness check filed for it, on a repo whose
// linter can be baselined and has no baseline yet, its first job makes
// the baseline (see LintBaseline) instead of triaging it. The baseline is
// the item's pull request, reviewed and merged like any other.

// lintIssueMarker is in the body of the issue the readiness check files
// for lint failing on the base branch.
const lintIssueMarker = "<!-- factory-readiness: check:lint -->"

// baselineOver is how many offenses make a baseline the fix: fewer, and a
// builder fixes them as usual.
const baselineOver = 50

// fixesByBaseline says whether issue body, on repo, is lint to fix with a
// baseline.
func (p *Pipeline) fixesByBaseline(repo, body string) bool {
	if !strings.Contains(body, lintIssueMarker) || !p.CanBaseline(repo) {
		return false
	}
	r, _ := readiness.Load(ReadinessDir(p.Cfg), repo)
	if r == nil || r.Baselined {
		return r == nil
	}
	n := r.Offenses("lint")
	return n < 0 || n > baselineOver
}

// runBaseline does item it's work as a lint baseline, in job j, and hands
// its pull request to a person.
func (p *Pipeline) runBaseline(ctx context.Context, j *store.Job, it *store.WorkItem) error {
	p.warn(p.gh(it.Repo).AddLabels(ctx, it.IssueNumber, LabelRunning), "AddLabels")
	url, err := p.LintBaseline(ctx, it.Repo)
	if err != nil {
		p.finish(ctx, j, agentRun{}, state.Failed, "", err.Error())
		p.warn(p.gh(it.Repo).RemoveLabel(ctx, it.IssueNumber, LabelRunning), "RemoveLabel")
		return p.park(ctx, it, "Lint fails across the whole base branch, so the factory tried to fix it with a baseline of today's offenses, and that failed:\n\n```\n"+truncate(err.Error(), 3000)+"\n```")
	}
	gh := p.gh(it.Repo)
	pr, err := gh.FindPR(ctx, BaselineBranch)
	if err != nil || pr == nil {
		p.finish(ctx, j, agentRun{}, state.Failed, "", fmt.Sprintf("the baseline's pull request %s can't be read: %v", url, err))
		return p.park(ctx, it, "The factory opened a lint baseline, "+url+", but can't follow it. Merge it, and the next check closes this.")
	}
	n, branch := pr.Number, BaselineBranch
	if err := p.Store.UpdateItem(ctx, it.ID, store.ItemUpdate{PRNumber: &n, Branch: &branch}); err != nil {
		return err
	}
	// Through the stages a build takes, so its history reads as one.
	for _, st := range []struct {
		stage state.Stage
		why   string
	}{
		{state.Ready, "lint fails across the base branch: fixed with a baseline"},
		{state.Implementing, "made the lint baseline"},
		{state.Reviewing, "lint passes with the baseline"},
		{state.AwaitingHuman, fmt.Sprintf("lint baseline PR #%d ready", n)},
	} {
		if err := p.Store.SetStage(ctx, it.ID, st.stage, "daemon", st.why); err != nil {
			return err
		}
	}
	p.warn(gh.LabelPR(ctx, n, LabelAwaitingYou), "LabelPR")
	p.warn(gh.RemoveLabel(ctx, it.IssueNumber, LabelRunning), "RemoveLabel")
	p.warn(gh.AddLabels(ctx, it.IssueNumber, LabelAwaitingYou), "AddLabels")
	p.warn(gh.Comment(ctx, it.IssueNumber, fmt.Sprintf("**Factory:** lint fails across the whole of `%s`, too much for one change, and pieces of it would each fail a lint of the whole repo. "+
		"So this is fixed with a baseline instead: %s records today's offenses as accepted, changing only the linter's own files. "+
		"Merge it and lint passes; only new offenses fail a change. Pay the old ones down later, a rule at a time.", p.repoConf(it.Repo).BaseBranch, url)), "Comment")
	p.finish(ctx, j, agentRun{}, state.Succeeded, "", "")
	p.notify(ctx, fmt.Sprintf("Lint baseline ready for review: %s %s", p.ref(it.Repo, n), url))
	return nil
}
