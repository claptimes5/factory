package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/claptimes5/factory/internal/gates"
	"github.com/claptimes5/factory/internal/github"
	"github.com/claptimes5/factory/internal/state"
	"github.com/claptimes5/factory/internal/store"
	"github.com/claptimes5/factory/internal/verdict"
)

// maxTriageAttempts and maxReviewAttempts bound retries of stages whose
// failures are usually transient (unparseable output, a crashed CLI).
const (
	maxTriageAttempts = 2
	maxReviewAttempts = 2
)

func (p *Pipeline) issue(ctx context.Context, it *store.WorkItem) (github.Issue, error) {
	is, err := p.GH.GetIssue(ctx, it.IssueNumber)
	if err != nil {
		return github.Issue{}, err
	}
	return *is, nil
}

// park moves an item to parked, labels the issue and tells a human why.
func (p *Pipeline) park(ctx context.Context, it *store.WorkItem, reason string) error {
	if err := p.Store.SetStage(ctx, it.ID, state.Parked, "daemon", reason); err != nil {
		return err
	}
	p.warn(p.GH.RemoveLabel(ctx, it.IssueNumber, LabelRunning), "GH.RemoveLabel")
	p.warn(p.GH.AddLabels(ctx, it.IssueNumber, LabelParked), "GH.AddLabels")
	p.warn(p.GH.Comment(ctx, it.IssueNumber, "**Factory parked this issue.**\n\n"+reason+"\n\nRemove the `"+LabelParked+"` label to send it back through triage."), "GH.Comment")
	p.notify(ctx, fmt.Sprintf("Parked #%d: %s", it.IssueNumber, firstLine(reason)))
	return nil
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	return s
}

// --- triage ---

func (p *Pipeline) runTriage(ctx context.Context, j *store.Job, it *store.WorkItem) error {
	dir := p.jobDir(j.ID)
	work, out := filepath.Join(dir, "work"), filepath.Join(dir, "out")
	fail := func(run agentRun, st state.JobState, msg string) error {
		p.finish(ctx, j, run, st, "", msg)
		if j.Attempt < maxTriageAttempts {
			return p.enqueueTriage(ctx, it, j.Attempt+1, "")
		}
		return p.park(ctx, it, "Triage failed twice: "+msg)
	}
	is, err := p.issue(ctx, it)
	if err != nil {
		return fail(agentRun{}, state.Failed, err.Error())
	}
	p.warn(p.GH.AddLabels(ctx, it.IssueNumber, LabelRunning), "GH.AddLabels")
	sha, _, err := p.Git.Checkout(ctx, work, p.Cfg.BaseBranch, "")
	if err != nil {
		return fail(agentRun{}, state.Failed, err.Error())
	}
	p.warn(p.Store.SetJobRuntime(ctx, j.ID, "", "", sha), "Store.SetJobRuntime")
	prompt, err := p.render("triage", PromptData{Repo: p.Cfg.Repo, Issue: is, OutDir: "/out"})
	if err != nil {
		return fail(agentRun{}, state.Failed, err.Error())
	}
	schema, _ := verdict.Schema("triage")
	run := p.runAgent(ctx, j, work, out, prompt, schema, true, p.Cfg.Timeout("triage"))
	if st, msg, bad := runFailure(run); bad {
		return fail(run, st, msg)
	}
	var v verdict.Triage
	norm, err := verdict.Load(filepath.Join(out, "verdict.json"), run.final, &v)
	if err != nil {
		return fail(run, state.Failed, err.Error())
	}
	p.finish(ctx, j, run, state.Succeeded, string(norm), "")
	if v.Decision == "park" {
		return p.park(ctx, it, "Triage needs more information:\n\n> "+v.Question)
	}
	if err := p.Store.UpdateItem(ctx, it.ID, store.ItemUpdate{Complexity: &v.Complexity, Risk: &v.Risk}); err != nil {
		return err
	}
	if err := p.Store.SetStage(ctx, it.ID, state.Ready, "daemon", fmt.Sprintf("triaged %s/%s: %s", v.Complexity, v.Risk, v.Summary)); err != nil {
		return err
	}
	it.Complexity, it.Risk = v.Complexity, v.Risk
	if _, err := p.enqueueImplement(ctx, it, 1, ImplementInput{}); err != nil {
		return err
	}
	return p.Store.SetStage(ctx, it.ID, state.Implementing, "daemon", "implement queued")
}

// runFailure classifies a run that did not complete normally.
func runFailure(run agentRun) (state.JobState, string, bool) {
	switch {
	case run.timeout:
		return state.TimedOut, "timed out", true
	case run.err != nil:
		return state.Failed, run.err.Error(), true
	case run.res != nil && run.res.IsError:
		return state.Failed, run.res.Error, true
	}
	return "", "", false
}

// --- implement ---

func (p *Pipeline) runImplement(ctx context.Context, j *store.Job, it *store.WorkItem) error {
	dir := p.jobDir(j.ID)
	work, out := filepath.Join(dir, "work"), filepath.Join(dir, "out")
	var in ImplementInput
	if j.Input != "" {
		if err := json.Unmarshal([]byte(j.Input), &in); err != nil {
			p.finish(ctx, j, agentRun{}, state.Failed, "", "bad job input: "+err.Error())
			return err
		}
	}

	fail := func(run agentRun, st state.JobState, msg string) error {
		p.finish(ctx, j, run, st, "", msg)
		next := in
		next.PreviousFailure = msg
		ok, err := p.enqueueImplement(ctx, it, j.Attempt+1, next)
		if err != nil {
			return err
		}
		if !ok {
			return p.park(ctx, it, fmt.Sprintf("Implementation failed on every model tier (%d attempts). Last failure:\n\n```\n%s\n```", j.Attempt, truncate(msg, 3000)))
		}
		return nil
	}

	is, err := p.issue(ctx, it)
	if err != nil {
		return fail(agentRun{}, state.Failed, err.Error())
	}
	p.warn(p.GH.AddLabels(ctx, it.IssueNumber, LabelRunning), "GH.AddLabels")
	start, existed, err := p.Git.Checkout(ctx, work, p.Cfg.BaseBranch, j.Branch)
	if err != nil {
		return fail(agentRun{}, state.Failed, err.Error())
	}
	if existed && !in.FixRound {
		// A fresh attempt starts from the base branch, not a stale branch.
		if _, err := p.Git.run(ctx, work, "reset", "--quiet", "--hard", "origin/"+p.Cfg.BaseBranch); err != nil {
			return fail(agentRun{}, state.Failed, err.Error())
		}
		start, _ = p.Git.run(ctx, work, "rev-parse", "HEAD")
	}
	p.warn(p.Store.SetJobRuntime(ctx, j.ID, "", "", start), "Store.SetJobRuntime")

	prompt, err := p.render("implement", PromptData{Repo: p.Cfg.Repo, Issue: is, OutDir: p.Runner.Paths(specFor(work, out)).Out,
		Findings: in.Findings, PreviousFailure: in.PreviousFailure})
	if err != nil {
		return fail(agentRun{}, state.Failed, err.Error())
	}
	run := p.runAgent(ctx, j, work, out, prompt, "", false, p.Cfg.Timeout("implement"))
	if st, msg, bad := runFailure(run); bad {
		return fail(run, st, msg)
	}
	var v verdict.Implement
	norm, err := verdict.Load(filepath.Join(out, "verdict.json"), "", &v)
	if err != nil {
		return fail(run, state.Failed, "builder verdict: "+err.Error())
	}
	if v.Status == "blocked" {
		return fail(run, state.Failed, "builder reported blocked: "+v.Blocker)
	}
	if _, err := p.Git.CommitAll(ctx, work, fmt.Sprintf("factory: uncommitted builder changes for #%d", it.IssueNumber)); err != nil {
		p.log().Warn("commit leftovers", "err", err)
	}
	if n, err := p.Git.CommitsSince(ctx, work, start); err != nil || n == 0 {
		return fail(run, state.Failed, "builder made no commits")
	}

	// Deterministic verification, outside the agent.
	base, err := p.Git.MergeBase(ctx, work, p.Cfg.BaseBranch)
	if err != nil {
		return fail(run, state.Failed, err.Error())
	}
	results := []gates.Result{gates.DiffGuard(ctx, work, base, gates.Guard{
		MaxChangedLines: p.Cfg.Gates.DiffGuard.MaxChangedLines, ProtectedPaths: p.Cfg.Gates.DiffGuard.ProtectedPaths})}
	results = append(results, gates.Run(ctx, p.Cfg.Gates.Required, p.Cfg.Gates.Commands, func(ctx context.Context, cmd string) (int, string, error) {
		return p.gateExec(ctx, work, cmd)
	})...)
	p.warn(p.Store.SetGateResults(ctx, j.ID, toStore(results)), "Store.SetGateResults")
	line := gates.VerdictLine(results)
	if !gates.Passed(results) {
		return fail(run, state.Failed, line+"\n\n"+gateDetails(results))
	}

	if err := p.Git.Push(ctx, work, j.Branch, !in.FixRound); err != nil {
		return fail(run, state.Failed, "push: "+err.Error())
	}
	branch := j.Branch
	p.warn(p.Store.UpdateItem(ctx, it.ID, store.ItemUpdate{Branch: &branch}), "Store.UpdateItem")
	p.finish(ctx, j, run, state.Succeeded, string(norm), "")
	if err := p.Store.SetStage(ctx, it.ID, state.Reviewing, "daemon", line); err != nil {
		return err
	}
	return p.enqueueReview(ctx, it, "review", 1, ReviewInput{ImplementJob: j.ID, Gates: line + "\n\n" + gateTable(results)})
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func toStore(rs []gates.Result) []store.GateResult {
	out := make([]store.GateResult, len(rs))
	for i, r := range rs {
		out[i] = store.GateResult{Gate: r.Gate, Status: r.Status, Detail: r.Detail}
	}
	return out
}

func gateTable(rs []gates.Result) string {
	var b strings.Builder
	b.WriteString("| Gate | Result |\n| --- | --- |\n")
	for _, r := range rs {
		fmt.Fprintf(&b, "| %s | %s |\n", r.Gate, r.Status)
	}
	return b.String()
}

func gateDetails(rs []gates.Result) string {
	var b strings.Builder
	for _, r := range rs {
		if r.Status != gates.Pass {
			fmt.Fprintf(&b, "### %s: %s\n%s\n\n", r.Gate, r.Status, truncate(r.Detail, 2000))
		}
	}
	return b.String()
}

// --- review ---

func (p *Pipeline) runReview(ctx context.Context, j *store.Job, it *store.WorkItem) error {
	dir := p.jobDir(j.ID)
	work, out := filepath.Join(dir, "work"), filepath.Join(dir, "out")
	var in ReviewInput
	if err := json.Unmarshal([]byte(j.Input), &in); err != nil {
		p.finish(ctx, j, agentRun{}, state.Failed, "", "bad job input: "+err.Error())
		return err
	}
	fail := func(run agentRun, st state.JobState, msg string) error {
		p.finish(ctx, j, run, st, "", msg)
		if j.Attempt < maxReviewAttempts {
			return p.enqueueReview(ctx, it, j.Kind, j.Attempt+1, in)
		}
		// Fail closed: an unusable review never passes.
		return p.park(ctx, it, fmt.Sprintf("The %s stage could not produce a valid verdict (%s).", j.Kind, msg))
	}
	is, err := p.issue(ctx, it)
	if err != nil {
		return fail(agentRun{}, state.Failed, err.Error())
	}
	head, _, err := p.Git.Checkout(ctx, work, p.Cfg.BaseBranch, j.Branch)
	if err != nil {
		return fail(agentRun{}, state.Failed, err.Error())
	}
	p.warn(p.Store.SetJobRuntime(ctx, j.ID, "", "", head), "Store.SetJobRuntime")
	base, err := p.Git.MergeBase(ctx, work, p.Cfg.BaseBranch)
	if err != nil {
		return fail(agentRun{}, state.Failed, err.Error())
	}
	diff, err := p.Git.Diff(ctx, work, base)
	if err != nil {
		return fail(agentRun{}, state.Failed, err.Error())
	}
	// The reviewer sees the issue, the diff and gate results only: never the
	// builder's own summary.
	prompt, err := p.render("review", PromptData{Repo: p.Cfg.Repo, Issue: is, Gates: in.Gates, Diff: truncate(diff, 200_000)})
	if err != nil {
		return fail(agentRun{}, state.Failed, err.Error())
	}
	schema, _ := verdict.Schema("review")
	run := p.runAgent(ctx, j, work, out, prompt, schema, true, p.Cfg.Timeout("review"))
	if st, msg, bad := runFailure(run); bad {
		return fail(run, st, msg)
	}
	var v verdict.Review
	norm, err := verdict.Load(filepath.Join(out, "verdict.json"), run.final, &v)
	if err != nil {
		return fail(run, state.Failed, err.Error())
	}
	p.finish(ctx, j, run, state.Succeeded, string(norm), "")

	switch {
	case v.Decision == "questions":
		return p.park(ctx, it, "The reviewer has questions it needs a human to answer:\n\n"+formatFindings(&v))
	case v.Blocks():
		return p.reviewBlocked(ctx, it, &v)
	case j.Kind == "review" && it.Risk == "high":
		// High-risk items get a second, independent Claude critic.
		return p.enqueueReview(ctx, it, "critic", 1, in)
	}
	return p.openPR(ctx, it, is, in)
}

func (p *Pipeline) reviewBlocked(ctx context.Context, it *store.WorkItem, v *verdict.Review) error {
	blocks, err := p.countBlocks(ctx, it.ID)
	if err != nil {
		return err
	}
	findings := formatFindings(v)
	if blocks > p.Cfg.Escalation.MaxReviewRounds {
		return p.park(ctx, it, fmt.Sprintf("The reviewer blocked this %d times. Latest findings:\n\n%s", blocks, findings))
	}
	if err := p.Store.SetStage(ctx, it.ID, state.Implementing, "daemon", fmt.Sprintf("review blocked (round %d)", blocks)); err != nil {
		return err
	}
	p.warn(p.GH.Comment(ctx, it.IssueNumber, fmt.Sprintf("**Reviewer blocked round %d.** Sending it back to the builder.\n\n%s", blocks, findings)), "GH.Comment")
	_, err = p.enqueueImplement(ctx, it, 1, ImplementInput{Findings: findings, FixRound: true})
	return err
}

// countBlocks counts succeeded review and critic jobs whose verdict blocked.
func (p *Pipeline) countBlocks(ctx context.Context, itemID int64) (int, error) {
	jobs, err := p.Store.ListJobs(ctx, store.JobFilter{ItemID: itemID, State: state.Succeeded})
	if err != nil {
		return 0, err
	}
	n := 0
	for _, j := range jobs {
		if j.Kind != "review" && j.Kind != "critic" {
			continue
		}
		var v verdict.Review
		if verdict.Parse([]byte(j.Verdict), &v) == nil && v.Blocks() {
			n++
		}
	}
	return n, nil
}

func formatFindings(v *verdict.Review) string {
	var b strings.Builder
	if len(v.Findings) == 0 {
		b.WriteString("_No individual findings._\n")
	}
	for _, f := range v.Findings {
		loc := f.File
		if f.Line > 0 {
			loc = fmt.Sprintf("%s:%d", f.File, f.Line)
		}
		fmt.Fprintf(&b, "- **%s** `%s`: %s\n  Evidence: %s\n", f.Severity, loc, f.Claim, f.Evidence)
	}
	if len(v.ChallengedAssumptions) > 0 {
		b.WriteString("\nChallenged assumptions:\n")
		for _, a := range v.ChallengedAssumptions {
			fmt.Fprintf(&b, "- %s\n", a)
		}
	}
	return b.String()
}

// openPR opens (or refreshes) the draft PR carrying the evidence and hands
// the item to a human. Merge is always human.
func (p *Pipeline) openPR(ctx context.Context, it *store.WorkItem, is github.Issue, in ReviewInput) error {
	body, err := p.prBody(ctx, it, in)
	if err != nil {
		return err
	}
	branch := branchFor(it)
	pr, err := p.GH.FindPR(ctx, branch)
	if err != nil {
		return err
	}
	if pr == nil {
		pr, err = p.GH.CreatePR(ctx, fmt.Sprintf("%s (#%d)", is.Title, is.Number), body, branch, p.Cfg.BaseBranch, true)
		if err != nil {
			return err
		}
	} else if err := p.GH.UpdatePRBody(ctx, pr.Number, body); err != nil {
		return err
	}
	n := pr.Number
	p.warn(p.Store.UpdateItem(ctx, it.ID, store.ItemUpdate{PRNumber: &n}), "Store.UpdateItem")
	p.warn(p.GH.AddLabels(ctx, pr.Number, LabelAwaitingYou), "GH.AddLabels")
	p.warn(p.GH.RemoveLabel(ctx, it.IssueNumber, LabelRunning), "GH.RemoveLabel")
	p.warn(p.GH.AddLabels(ctx, it.IssueNumber, LabelAwaitingYou), "GH.AddLabels")
	if err := p.Store.SetStage(ctx, it.ID, state.AwaitingHuman, "daemon", fmt.Sprintf("draft PR #%d ready", pr.Number)); err != nil {
		return err
	}
	p.notify(ctx, fmt.Sprintf("PR ready for review: #%d %s %s", pr.Number, is.Title, pr.URL))
	return nil
}

func (p *Pipeline) prBody(ctx context.Context, it *store.WorkItem, in ReviewInput) (string, error) {
	jobs, err := p.Store.ListJobs(ctx, store.JobFilter{ItemID: it.ID})
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Closes #%d\n\nBuilt by the factory. Triage: complexity **%s**, risk **%s**.\n\n", it.IssueNumber, it.Complexity, it.Risk)
	b.WriteString("## Gates\n\n" + in.Gates + "\n")
	var total float64
	b.WriteString("## Jobs\n\n| Job | Kind | Harness | Model | Skills | Attempt | State | Cost |\n| --- | --- | --- | --- | --- | --- | --- | --- |\n")
	for i := len(jobs) - 1; i >= 0; i-- {
		j := jobs[i]
		total += j.CostUSD
		sk := j.Skillset
		if sk == "" {
			sk = "-"
		}
		fmt.Fprintf(&b, "| %d | %s | %s | %s | %s | %d | %s | $%.2f |\n", j.ID, j.Kind, j.Harness, j.Model, sk, j.Attempt, j.State, j.CostUSD)
	}
	fmt.Fprintf(&b, "\n**Total cost:** $%.2f\n\n", total)
	for _, j := range jobs {
		if (j.Kind == "review" || j.Kind == "critic") && j.State == state.Succeeded {
			var pretty any
			if json.Unmarshal([]byte(j.Verdict), &pretty) != nil {
				continue
			}
			pv, _ := json.MarshalIndent(pretty, "", "  ")
			fmt.Fprintf(&b, "<details><summary>%s verdict (job %d, %s)</summary>\n\n```json\n%s\n```\n</details>\n\n", j.Kind, j.ID, j.Model, pv)
			break
		}
	}
	b.WriteString("_Merge is always human. Review the diff before merging._\n")
	return b.String(), nil
}
