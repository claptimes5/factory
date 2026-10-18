package pipeline

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/claptimes5/factory/internal/config"
	"github.com/claptimes5/factory/internal/github"
	"github.com/claptimes5/factory/internal/state"
	"github.com/claptimes5/factory/internal/store"
)

// Workspaces are repos that change together, like an API and the web app
// that calls it. An issue in any member is one item: its builder gets every
// member checked out side by side, the gates run in each repo the change
// touches, the reviewer reads one diff across them, and each changed repo
// gets its own draft PR, linked to the others and merged in the order the
// workspace lists its repos. The item waits on you once every repo passes.

// part is one repo of an item's change: the item's own repo, or one member
// of its workspace.
type part struct {
	repo   string
	folder string // the repo's folder in a workspace checkout; "" on its own
	ws     *Workspace
	base   string // where the change starts on the base branch
	head   string // the commit under test
}

// sfx keeps one part's job directories apart from another's.
func (pt *part) sfx() string {
	if pt.folder == "" {
		return ""
	}
	return "-" + pt.folder
}

// gate names a gate result after the repo it ran in, in a workspace.
func (pt *part) gate(name string) string {
	if pt.folder == "" {
		return name
	}
	return pt.folder + ": " + name
}

// members are the repos an item's change may span, in merge order: its
// workspace's, or only its own.
func (p *Pipeline) members(it *store.WorkItem) []string {
	if _, m := p.Cfg.Workspace(it.Repo); len(m) > 1 {
		return m
	}
	return []string{it.Repo}
}

func (p *Pipeline) inWorkspace(it *store.WorkItem) bool { return len(p.members(it)) > 1 }

// branchFor is the branch an item's change goes on. In a workspace it is
// the same in every repo and names the issue's own repo, since issue 12 of
// the API and issue 12 of the web app are different changes.
func (p *Pipeline) branchFor(it *store.WorkItem) string {
	if p.inWorkspace(it) {
		// Never a branch the item used before its repo joined the
		// workspace: factory/issue-12 in the other repos is their own
		// issue 12's.
		return fmt.Sprintf("factory/%s-%d", config.ShortName(it.Repo), it.IssueNumber)
	}
	if it.Branch != "" {
		return it.Branch
	}
	return fmt.Sprintf("factory/issue-%d", it.IssueNumber)
}

// prepareParts clones every repo of an item's change for a job; branch
// says what to check out in each ("" for the base branch). A workspace's
// repos go in dir/repos/<folder>, and the builder's in dir/work/<folder>,
// so an agent sees them side by side.
func (p *Pipeline) prepareParts(ctx context.Context, dir string, it *store.WorkItem, branch func(repo string) string, fresh, builder bool) ([]*part, error) {
	ms := p.members(it)
	if len(ms) == 1 {
		ws, err := p.git(it.Repo).Prepare(ctx, dir, p.repoConf(it.Repo).BaseBranch, branch(it.Repo), fresh, builder)
		if err != nil {
			return nil, err
		}
		return []*part{{repo: it.Repo, ws: ws}}, nil
	}
	var parts []*part
	for _, m := range ms {
		f := config.ShortName(m)
		work := ""
		if builder {
			work = filepath.Join(dir, "work", f)
		}
		ws, err := p.git(m).PrepareAt(ctx, filepath.Join(dir, "repos", f), work, p.repoConf(m).BaseBranch, branch(m), fresh)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", m, err)
		}
		parts = append(parts, &part{repo: m, folder: f, ws: ws})
	}
	return parts, nil
}

// agentDir is where an agent works on parts: the one checkout, or the
// folder that holds a workspace's.
func agentDir(dir string, parts []*part, builder bool) string {
	if parts[0].folder == "" {
		if builder {
			return parts[0].ws.Work
		}
		return parts[0].ws.Repo
	}
	if builder {
		return filepath.Join(dir, "work")
	}
	return filepath.Join(dir, "repos")
}

// realDir refuses a builder's checkout that is no longer a plain
// directory. A workspace's checkouts sit inside the folder the builder can
// write, so it could swap one for a symlink to anywhere on the host, and
// the daemon would commit what is there.
func realDir(dir string) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("the builder replaced %s with something other than a directory", filepath.Base(dir))
	}
	return nil
}

// homePart is the part in the item's own repo.
func homePart(parts []*part, repo string) *part {
	for _, pt := range parts {
		if strings.EqualFold(pt.repo, repo) {
			return pt
		}
	}
	return parts[0]
}

// headOf finds repo's commit in heads.
func headOf(heads []store.RepoHead, repo string) (string, bool) {
	for _, h := range heads {
		if strings.EqualFold(h.Repo, repo) {
			return h.SHA, true
		}
	}
	return "", false
}

// jobHeads is what an implement job verified: a commit per repo it changed
// in a workspace, else its one head.
func (p *Pipeline) jobHeads(ctx context.Context, it *store.WorkItem, j *store.Job) []store.RepoHead {
	if hs, err := p.Store.JobHeads(ctx, j.ID); err == nil && len(hs) > 0 {
		return hs
	}
	if j.HeadSHA == "" {
		return nil
	}
	return []store.RepoHead{{Repo: it.Repo, SHA: j.HeadSHA}}
}

// lastHeads is what the item's latest successful implement job verified.
func (p *Pipeline) lastHeads(ctx context.Context, it *store.WorkItem) []store.RepoHead {
	jobs, err := p.Store.ListJobs(ctx, store.JobFilter{ItemID: it.ID, Kind: "implement", State: state.Succeeded, Limit: 1})
	if err != nil || len(jobs) == 0 {
		return nil
	}
	return p.jobHeads(ctx, it, jobs[0])
}

// requiredChecks are a repo's required gate commands, for the builder.
func (p *Pipeline) requiredChecks(repo string) []string {
	g := p.repoConf(repo).Gates
	var checks []string
	for _, name := range g.Required {
		if c := g.Commands[name]; c != "" {
			checks = append(checks, c)
		}
	}
	return checks
}

// PromptMember is one repo of a workspace, for the builder and reviewer.
type PromptMember struct {
	Dir     string // its folder in the checkout
	Repo    string
	Install string
	Checks  []string
}

func (p *Pipeline) promptMembers(parts []*part) []PromptMember {
	if parts[0].folder == "" {
		return nil
	}
	out := make([]PromptMember, len(parts))
	for i, pt := range parts {
		out[i] = PromptMember{Dir: pt.folder, Repo: pt.repo, Install: p.repoConf(pt.repo).Gates.Install, Checks: p.requiredChecks(pt.repo)}
	}
	return out
}

// combinedDiff is the change across a workspace's repos, every path under
// its repo's folder, so the reviewer reads it as one.
func (p *Pipeline) combinedDiff(ctx context.Context, parts []*part) (string, error) {
	var b strings.Builder
	for _, pt := range parts {
		if pt.head == "" {
			continue
		}
		d, err := p.git(pt.repo).DiffIn(ctx, pt.ws.Repo, pt.base, pt.head, pt.folder)
		if err != nil {
			return "", fmt.Errorf("%s: %w", pt.repo, err)
		}
		if d != "" {
			b.WriteString(d + "\n")
		}
	}
	return b.String(), nil
}

// issueRef names an issue so a PR in another repo can link it.
func (p *Pipeline) issueRef(repo string, n int) string {
	return fmt.Sprintf("%s#%d", p.repoConf(repo).Path(), n)
}

// prRef names a PR (a merge request on GitLab) from another repo.
func (p *Pipeline) prRef(repo string, n int) string {
	rc := p.repoConf(repo)
	if rc.GitLab() {
		return fmt.Sprintf("%s!%d", rc.Path(), n)
	}
	return fmt.Sprintf("%s#%d", rc.Path(), n)
}

type openedPR struct {
	repo string
	pr   *github.PR
}

// linkedTop opens the body of PR i of a workspace change: what it closes,
// and every PR of the change in the order they merge. Only the PR that
// merges last closes the issue, so it stays open until all of it is in.
func (p *Pipeline) linkedTop(it *store.WorkItem, prs []openedPR, i int) string {
	var b strings.Builder
	issue := p.issueRef(it.Repo, it.IssueNumber)
	if len(prs) == 1 {
		return "Closes " + issue
	}
	if i == len(prs)-1 {
		b.WriteString("Closes " + issue)
	} else {
		b.WriteString("Part of " + issue)
	}
	fmt.Fprintf(&b, "\n\nThis change spans %d repos. Merge them in this order:\n\n", len(prs))
	for k, o := range prs {
		ref := p.prRef(o.repo, o.pr.Number)
		if o.pr.URL != "" {
			ref = fmt.Sprintf("[%s](%s)", ref, o.pr.URL)
		}
		if k == i {
			ref += " (this PR)"
		}
		fmt.Fprintf(&b, "%d. %s\n", k+1, ref)
	}
	return b.String()
}

// openWorkspacePRs opens (or refreshes) a draft PR in each repo the change
// touches, links them to each other and hands the item to a human.
func (p *Pipeline) openWorkspacePRs(ctx context.Context, it *store.WorkItem, is github.Issue, in ReviewInput) error {
	branch := p.branchFor(it)
	title := fmt.Sprintf("%s (%s)", is.Title, p.issueRef(it.Repo, is.Number))
	var prs []openedPR
	for _, h := range in.Heads {
		gh := p.gh(h.Repo)
		pr, err := gh.FindPR(ctx, branch)
		if err != nil {
			return fmt.Errorf("%s: %w", h.Repo, err)
		}
		if pr == nil {
			// The body links the other PRs, so it is written once they all exist.
			if pr, err = gh.CreatePR(ctx, title, "Opening the linked PRs…", branch, p.repoConf(h.Repo).BaseBranch, true); err != nil {
				return fmt.Errorf("%s: %w", h.Repo, err)
			}
		}
		prs = append(prs, openedPR{h.Repo, pr})
	}
	if len(prs) == 0 {
		return fmt.Errorf("no repo of the change has a verified commit")
	}
	rows := make([]store.RepoPR, len(prs))
	for i, o := range prs {
		body, err := p.prBody(ctx, it, in, p.linkedTop(it, prs, i), false)
		if err != nil {
			return err
		}
		if err := p.gh(o.repo).UpdatePRBody(ctx, o.pr.Number, body); err != nil {
			return fmt.Errorf("%s: %w", o.repo, err)
		}
		rows[i] = store.RepoPR{Repo: o.repo, Number: o.pr.Number}
	}
	p.retire(ctx, it, branch, prs)
	if err := p.Store.SetItemPRs(ctx, it.ID, rows); err != nil {
		return err
	}
	// The item's own PR number is its repo's PR, when the change has one.
	main := prs[0]
	for _, o := range prs {
		if strings.EqualFold(o.repo, it.Repo) {
			main = o
		}
	}
	n := main.pr.Number
	if err := p.Store.UpdateItem(ctx, it.ID, store.ItemUpdate{PRNumber: &n}); err != nil {
		return err
	}
	refs := make([]string, len(prs))
	urls := make([]string, len(prs))
	for i, o := range prs {
		refs[i], urls[i] = p.prRef(o.repo, o.pr.Number), o.pr.URL
	}
	if err := p.Store.SetStage(ctx, it.ID, state.AwaitingHuman, "daemon", "draft PRs "+strings.Join(refs, ", ")+" ready"); err != nil {
		return err
	}
	for _, o := range prs {
		p.warn(p.gh(o.repo).LabelPR(ctx, o.pr.Number, LabelAwaitingYou), "LabelPR")
		p.labelRisk(ctx, it, o.repo, o.pr.Number)
	}
	p.warn(p.gh(it.Repo).RemoveLabel(ctx, it.IssueNumber, LabelRunning), "RemoveLabel")
	p.warn(p.gh(it.Repo).AddLabels(ctx, it.IssueNumber, LabelAwaitingYou), "AddLabels")
	p.notify(ctx, fmt.Sprintf("%s-risk change ready for review across %d repos: %s %s. Merge in order: %s",
		capitalize(riskOrLow(it.Risk)), len(prs), p.ref(it.Repo, it.IssueNumber), is.Title, strings.Join(urls, " then ")))
	return nil
}

// retire marks the PRs an earlier run of the item opened in repos this
// change no longer touches: they hold old commits, and merging one would
// ship half of a change. The factory never closes a PR; it says so in the
// body and takes off its awaiting-you label.
func (p *Pipeline) retire(ctx context.Context, it *store.WorkItem, branch string, keep []openedPR) {
	old, err := p.Store.ItemPRs(ctx, it.ID)
	if err != nil {
		p.warn(err, "ItemPRs")
		return
	}
	for _, o := range old {
		kept := false
		for _, k := range keep {
			kept = kept || strings.EqualFold(k.repo, o.Repo)
		}
		if kept {
			continue
		}
		body := fmt.Sprintf("**Superseded: don't merge this.** The factory rebuilt %s and the change no longer touches this repo. Close this PR and delete `%s`.",
			p.issueRef(it.Repo, it.IssueNumber), branch)
		p.warn(p.gh(o.Repo).UpdatePRBody(ctx, o.Number, body), "UpdatePRBody")
		p.warn(p.gh(o.Repo).UnlabelPR(ctx, o.Number, LabelAwaitingYou), "UnlabelPR")
	}
}

// closeOutWorkspace finishes a workspace item once every PR of its change
// merged, and parks it when any was closed without merging.
func (p *Pipeline) closeOutWorkspace(ctx context.Context, it *store.WorkItem, prs []store.RepoPR) {
	pushed := p.lastHeads(ctx, it)
	merged, changed := 0, false
	var refs []string
	for _, rp := range prs {
		pr, err := p.gh(rp.Repo).GetPR(ctx, rp.Number)
		if err != nil {
			p.log().Warn("get pr", "repo", rp.Repo, "pr", rp.Number, "err", err)
			return
		}
		ref := p.prRef(rp.Repo, rp.Number)
		refs = append(refs, ref)
		switch {
		case pr.Merged:
			merged++
			want, _ := headOf(pushed, rp.Repo)
			changed = changed || pr.Head.SHA != "" && pr.Head.SHA != want
		case pr.State == "closed":
			p.warn(p.Store.AddEvent(ctx, store.Event{Actor: "human", ItemID: &it.ID, Type: "pr_closed", Reason: "PR " + ref}), "AddEvent")
			p.warn(p.gh(it.Repo).RemoveLabel(ctx, it.IssueNumber, LabelAwaitingYou), "RemoveLabel")
			p.warn(p.park(ctx, it, fmt.Sprintf("PR %s was closed without merging, so the change across the workspace is incomplete. Close or revert the others if they shouldn't stay.", ref)), "park")
			return
		}
	}
	if merged < len(prs) {
		return // still waiting on some
	}
	all := strings.Join(refs, ", ")
	p.warn(p.Store.AddEvent(ctx, store.Event{Actor: "human", ItemID: &it.ID, Type: "pr_merged",
		Reason: "PRs " + all, Data: fmt.Sprintf(`{"changed":%t}`, changed)}), "AddEvent")
	p.warn(p.Store.SetStage(ctx, it.ID, state.Done, "human", "PRs "+all+" merged"), "SetStage")
	if changed {
		p.notify(ctx, fmt.Sprintf("The PRs for %s were merged with your changes. If they fixed something the factory got wrong, it's worth a replay case.", p.ref(it.Repo, it.IssueNumber)))
	}
	p.warn(p.gh(it.Repo).RemoveLabel(ctx, it.IssueNumber, LabelAwaitingYou), "RemoveLabel")
}
