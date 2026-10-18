package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
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
	is, err := p.gh(it.Repo).GetIssue(ctx, it.IssueNumber)
	if err != nil {
		return github.Issue{}, err
	}
	return *is, nil
}

// park moves an item to parked, labels the issue and tells a human why.
// Only an explicit factory:retry label brings it back (see Intake).
func (p *Pipeline) park(ctx context.Context, it *store.WorkItem, reason string) error {
	if err := p.Store.SetStage(ctx, it.ID, state.Parked, "daemon", reason); err != nil {
		return err
	}
	p.warn(p.gh(it.Repo).RemoveLabel(ctx, it.IssueNumber, LabelRunning), "RemoveLabel")
	p.warn(p.gh(it.Repo).AddLabels(ctx, it.IssueNumber, LabelParked), "AddLabels")
	p.warn(p.gh(it.Repo).Comment(ctx, it.IssueNumber, "**Factory parked this issue.**\n\n"+reason+
		"\n\nAdd the `"+LabelRetry+"` label to send it back through triage."), "Comment")
	p.notify(ctx, fmt.Sprintf("Parked %s: %s", p.ref(it.Repo, it.IssueNumber), firstLine(reason)))
	return nil
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	return s
}

// --- triage ---

func (p *Pipeline) runTriage(ctx context.Context, j *store.Job, it *store.WorkItem) error {
	dir := p.jobDir(j.ID)
	out := filepath.Join(dir, "out")
	fail := func(run agentRun, st state.JobState, msg string) error {
		if p.stopIfCancelled(ctx, j, it, run) {
			return nil
		}
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
	p.warn(p.gh(it.Repo).AddLabels(ctx, it.IssueNumber, LabelRunning), "AddLabels")
	ws, err := p.git(it.Repo).Prepare(ctx, dir, p.repoConf(it.Repo).BaseBranch, "", false, false)
	if err != nil {
		return fail(agentRun{}, state.Failed, err.Error())
	}
	p.warn(p.Store.SetJobRuntime(ctx, j.ID, "", "", ws.Start), "SetJobRuntime")
	prompt, err := p.render("triage", PromptData{Repo: it.Repo, Issue: is})
	if err != nil {
		return fail(agentRun{}, state.Failed, err.Error())
	}
	schema, _ := verdict.Schema("triage")
	run := p.runAgent(ctx, it.Repo, j, ws.Repo, out, prompt, schema, true, p.Cfg.Timeout("triage"))
	if p.stopIfCancelled(ctx, j, it, run) {
		return nil
	}
	if st, msg, bad := runFailure(run); bad {
		return fail(run, st, msg)
	}
	// Read-only stages report only through their final message.
	var v verdict.Triage
	if err := verdict.Parse([]byte(run.final), &v); err != nil {
		return fail(run, state.Failed, err.Error())
	}
	norm, _ := json.Marshal(v)
	p.finish(ctx, j, run, state.Succeeded, string(norm), "")
	return p.afterTriage(ctx, it, &v)
}

func (p *Pipeline) afterTriage(ctx context.Context, it *store.WorkItem, v *verdict.Triage) error {
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
	if ok, err := p.depsDone(ctx, it.ID); err != nil {
		return err
	} else if !ok {
		// unblock queues the implement once every dependency is done.
		return p.Store.SetStage(ctx, it.ID, state.Blocked, "daemon", "waiting on dependencies")
	}
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
	out := filepath.Join(dir, "out")
	var in ImplementInput
	if j.Input != "" {
		if err := json.Unmarshal([]byte(j.Input), &in); err != nil {
			p.finish(ctx, j, agentRun{}, state.Failed, "", "bad job input: "+err.Error())
			return err
		}
	}

	fail := func(run agentRun, st state.JobState, msg string) error {
		if p.stopIfCancelled(ctx, j, it, run) {
			return nil
		}
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
	p.warn(p.gh(it.Repo).AddLabels(ctx, it.IssueNumber, LabelRunning), "AddLabels")
	// A fresh attempt starts from the base branch; a fix round builds on the
	// branch the reviewer saw.
	ws, err := p.git(it.Repo).Prepare(ctx, dir, p.repoConf(it.Repo).BaseBranch, j.Branch, !in.FixRound, true)
	if err != nil {
		return fail(agentRun{}, state.Failed, err.Error())
	}
	p.warn(p.Store.SetJobRuntime(ctx, j.ID, "", "", ws.Start), "SetJobRuntime")

	g := p.repoConf(it.Repo).Gates
	var checks []string
	for _, name := range g.Required {
		if c := g.Commands[name]; c != "" {
			checks = append(checks, c)
		}
	}
	prompt, err := p.render("implement", PromptData{Repo: it.Repo, Issue: is, OutDir: p.Runner.Paths(specFor(ws.Work, out)).Out,
		Install: g.Install, Checks: checks, Findings: in.Findings, PreviousFailure: in.PreviousFailure})
	if err != nil {
		return fail(agentRun{}, state.Failed, err.Error())
	}
	x := p.sessionFor(ctx, j, in.FixRound)
	x.repo = it.Repo
	run := p.runAgentWith(ctx, j, ws.Work, out, prompt, "", p.Cfg.Timeout("implement"), x)
	if p.stopIfCancelled(ctx, j, it, run) {
		return nil
	}
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

	// From here on the builder's checkout is only read as files. Everything
	// below runs in the daemon's own repository.
	head, changed, err := p.git(it.Repo).Ingest(ctx, ws, fmt.Sprintf("factory: #%d %s\n\n%s\n\nJob %d, %s/%s, attempt %d.",
		it.IssueNumber, is.Title, v.Summary, j.ID, j.Harness, j.Model, j.Attempt))
	if err != nil {
		return fail(run, state.Failed, err.Error())
	}
	if !changed {
		return fail(run, state.Failed, "builder made no changes")
	}
	base, err := p.git(it.Repo).MergeBase(ctx, ws.Repo, ws.BaseSHA, head)
	if err != nil {
		return fail(run, state.Failed, err.Error())
	}
	results, blast, err := p.verify(ctx, j.ID, it, ws, base, head)
	if err != nil {
		return fail(run, state.Failed, err.Error())
	}
	p.warn(p.Store.SetGateResults(ctx, j.ID, toStore(results)), "SetGateResults")
	line := gates.VerdictLine(results)
	if !gates.Passed(results) {
		return fail(run, state.Failed, line+"\n\n"+gateDetails(results))
	}

	p.warn(p.Store.SetJobHead(ctx, j.ID, head), "SetJobHead")
	// Before the job is marked done, so a restart that resumes at review
	// finds both: the risk raised and the blast radius saved.
	blastMD := p.applyBlast(ctx, it, j.ID, blast)
	// Push exactly the commit that was verified.
	if err := p.git(it.Repo).Push(ctx, ws.Repo, head, j.Branch, !in.FixRound); err != nil {
		return fail(run, state.Failed, "push: "+err.Error())
	}
	branch := j.Branch
	p.warn(p.Store.UpdateItem(ctx, it.ID, store.ItemUpdate{Branch: &branch}), "UpdateItem")
	p.finish(ctx, j, run, state.Succeeded, string(norm), "")
	if err := p.Store.SetStage(ctx, it.ID, state.Reviewing, "daemon", line); err != nil {
		return err
	}
	return p.enqueueReview(ctx, it, "review", 1, ReviewInput{ImplementJob: j.ID, Head: head, Gates: line + "\n\n" + gateTable(results), Features: blastMD})
}

// verify runs the deterministic checks on the commit under test: the diff
// guard on the daemon's repository, then install, the gates and the
// test-proves-fix check on exports, with a check after each that the
// commit's files are untouched.
func (p *Pipeline) verify(ctx context.Context, jobID int64, it *store.WorkItem, ws *Workspace, base, head string) ([]gates.Result, *blastRadius, error) {
	g := p.repoConf(it.Repo).Gates
	results := []gates.Result{gates.DiffGuard(ctx, ws.Repo, base, head, gates.Guard{
		MaxChangedLines: g.DiffGuard.MaxChangedLines, ProtectedPaths: g.DiffGuard.ProtectedPaths})}
	gateDir := filepath.Join(p.jobDir(jobID), "gate")
	if err := p.git(it.Repo).Export(ctx, ws.Repo, head, gateDir); err != nil {
		return nil, nil, err
	}
	snap, err := takeSnapshot(gateDir)
	if err != nil {
		return nil, nil, err
	}
	if g.Install != "" {
		r := gates.Result{Gate: "install", Status: gates.Pass}
		code, out, err := p.gateExec(ctx, jobID, gateDir, g.Install, gateRun{repo: it.Repo, install: true})
		switch {
		case err != nil:
			r.Status, r.Detail = gates.Fail, "could not run: "+err.Error()
		case code != 0:
			r.Status, r.Detail = gates.Fail, fmt.Sprintf("exit %d\n%s", code, tail(out, 3000))
		default:
			if note := snap.tamperNote(gateDir); note != "" {
				r.Status, r.Detail = gates.Fail, note
			}
		}
		if results = append(results, r); r.Status != gates.Pass {
			return results, nil, nil // nothing else can run without the dependencies
		}
	}
	results = append(results, gates.Run(ctx, g.Required, g.Commands, func(ctx context.Context, cmd string) (int, string, error) {
		code, out, err := p.gateExec(ctx, jobID, gateDir, cmd, gateRun{repo: it.Repo})
		if note := snap.tamperNote(gateDir); err == nil && note != "" {
			return 1, out + "\n\nfactory: " + note, nil
		}
		return code, out, err
	})...)
	if g.ProvesFix != "" {
		results = append(results, p.provesFix(ctx, jobID, it, g.ProvesFix, ws.Repo, base, head))
	}
	// The checks of the features the change touches, then the scenarios:
	// slow end-to-end checks, run only on code that passes everything else.
	var blast *blastRadius
	if gates.Passed(results) {
		var fr []gates.Result
		fr, blast = p.featureChecks(ctx, jobID, it, ws, base, head, gateDir, snap)
		results = append(results, fr...)
	}
	if gates.Passed(results) {
		results = append(results, p.runScenarios(ctx, it.Repo, jobID, gateDir, g.ScenariosDir)...)
		if note := snap.tamperNote(gateDir); note != "" {
			results = append(results, gates.Result{Gate: "scenarios", Status: gates.Fail, Detail: "a hidden scenario run: " + note})
		}
	}
	return results, blast, nil
}

// provesFix reverts the non-test code to base, keeps the changed tests, and
// requires them to fail: a test that passes without the fix proves nothing.
func (p *Pipeline) provesFix(ctx context.Context, jobID int64, it *store.WorkItem, command, repo, base, head string) gates.Result {
	r := gates.Result{Gate: "proves_fix"}
	files, err := p.git(it.Repo).ChangedFiles(ctx, repo, base, head)
	if err != nil {
		r.Status, r.Detail = gates.Fail, err.Error()
		return r
	}
	var tests, code []string
	for _, f := range files {
		switch {
		case gates.IsTest(f):
			tests = append(tests, f)
		case strings.HasSuffix(f, ".go"):
			code = append(code, f)
		}
	}
	if len(code) == 0 {
		r.Status, r.Detail = gates.Pass, "no non-test Go code changed"
		return r
	}
	pkgSet := map[string]bool{}
	for _, f := range tests {
		if strings.HasSuffix(f, "_test.go") {
			pkgSet["./"+filepath.Dir(f)] = true
		}
	}
	if len(pkgSet) == 0 {
		r.Status, r.Detail = gates.Fail, "the change touches Go code but adds or changes no test"
		return r
	}
	dir := filepath.Join(p.jobDir(jobID), "proves")
	if err := p.git(it.Repo).Export(ctx, repo, base, dir); err != nil {
		r.Status, r.Detail = gates.Fail, err.Error()
		return r
	}
	for _, f := range tests {
		// A test file deleted at head is skipped; the diff guard reports it.
		if err := p.git(it.Repo).Show(ctx, repo, head, f, filepath.Join(dir, f)); err != nil {
			continue
		}
	}
	var pkgs []string
	for k := range pkgSet {
		pkgs = append(pkgs, k)
	}
	sort.Strings(pkgs)
	cmd := strings.ReplaceAll(command, "{pkgs}", strings.Join(pkgs, " "))
	exit, output, err := p.gateExec(ctx, jobID, dir, cmd, gateRun{repo: it.Repo})
	switch {
	case err != nil:
		r.Status, r.Detail = gates.Fail, "could not run: "+err.Error()
	case exit == 0:
		r.Status, r.Detail = gates.Fail, "the changed tests pass without the fix, so they do not prove it\n"+truncate(output, 2000)
	default:
		r.Status, r.Detail = gates.Pass, fmt.Sprintf("changed tests fail without the fix (exit %d)", exit)
	}
	return r
}

// tail keeps the last n bytes of s, where a failing command says why.
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
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

// fence returns a backtick fence longer than any backtick run in s, so the
// diff cannot close its code block and write into the reviewer's prompt.
func fence(s string) string {
	longest, run := 0, 0
	for _, c := range s {
		if c == '`' {
			run++
			if run > longest {
				longest = run
			}
		} else {
			run = 0
		}
	}
	if longest < 3 {
		longest = 2
	}
	return strings.Repeat("`", longest+1)
}

// --- review ---

func (p *Pipeline) runReview(ctx context.Context, j *store.Job, it *store.WorkItem) error {
	dir := p.jobDir(j.ID)
	out := filepath.Join(dir, "out")
	var in ReviewInput
	if err := json.Unmarshal([]byte(j.Input), &in); err != nil {
		p.finish(ctx, j, agentRun{}, state.Failed, "", "bad job input: "+err.Error())
		return err
	}
	fail := func(run agentRun, st state.JobState, msg string) error {
		if p.stopIfCancelled(ctx, j, it, run) {
			return nil
		}
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
	ws, err := p.git(it.Repo).Prepare(ctx, dir, p.repoConf(it.Repo).BaseBranch, j.Branch, false, false)
	if err != nil {
		return fail(agentRun{}, state.Failed, err.Error())
	}
	if in.Head != "" && ws.Start != in.Head {
		// Someone pushed after the gates ran: those commits were never verified.
		p.finish(ctx, j, agentRun{}, state.Failed, "", "branch moved after the gates ran")
		return p.park(ctx, it, fmt.Sprintf("Branch `%s` moved to %s after the gates verified %s. Push changes through the factory, or review them yourself.",
			j.Branch, short(ws.Start), short(in.Head)))
	}
	p.warn(p.Store.SetJobRuntime(ctx, j.ID, "", "", ws.Start), "SetJobRuntime")
	base, err := p.git(it.Repo).MergeBase(ctx, ws.Repo, ws.BaseSHA, ws.Start)
	if err != nil {
		return fail(agentRun{}, state.Failed, err.Error())
	}
	diff, err := p.git(it.Repo).Diff(ctx, ws.Repo, base, ws.Start)
	if err != nil {
		return fail(agentRun{}, state.Failed, err.Error())
	}
	diff = truncate(diff, 200_000)
	// The control room's diff viewer shows exactly what the reviewer saw.
	p.warn(os.WriteFile(filepath.Join(dir, "diff.patch"), []byte(diff), 0o644), "save diff")
	// The reviewer sees the issue, the diff and gate results only: never the
	// builder's own summary.
	prompt, err := p.render("review", PromptData{Repo: it.Repo, Issue: is, Gates: in.Gates, Features: in.Features, Risk: riskOrLow(it.Risk), Diff: diff, Fence: fence(diff)})
	if err != nil {
		return fail(agentRun{}, state.Failed, err.Error())
	}
	schema, _ := verdict.Schema("review")
	run := p.runAgent(ctx, it.Repo, j, ws.Repo, out, prompt, schema, true, p.Cfg.Timeout("review"))
	if p.stopIfCancelled(ctx, j, it, run) {
		return nil
	}
	if st, msg, bad := runFailure(run); bad {
		return fail(run, st, msg)
	}
	// Only the final message counts: a file in the checkout or the output
	// directory could have been planted by the builder's code.
	var v verdict.Review
	if err := verdict.Parse([]byte(run.final), &v); err != nil {
		return fail(run, state.Failed, err.Error())
	}
	norm, _ := json.Marshal(v)
	p.finish(ctx, j, run, state.Succeeded, string(norm), "")
	return p.afterReview(ctx, it, j.Kind, &v, in)
}

func short(sha string) string {
	if len(sha) > 10 {
		return sha[:10]
	}
	return sha
}

func (p *Pipeline) afterReview(ctx context.Context, it *store.WorkItem, kind string, v *verdict.Review, in ReviewInput) error {
	switch {
	case v.Decision == "questions":
		return p.park(ctx, it, "The reviewer has questions it needs a human to answer:\n\n"+formatFindings(v))
	case v.Blocks():
		return p.reviewBlocked(ctx, it, v)
	case kind == "review" && it.Risk == "high":
		// High-risk items get a second, independent Claude critic.
		return p.enqueueReview(ctx, it, "critic", 1, in)
	}
	is, err := p.issue(ctx, it)
	if err != nil {
		return err
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
	p.warn(p.gh(it.Repo).Comment(ctx, it.IssueNumber, fmt.Sprintf("**Reviewer blocked round %d.** Sending it back to the builder.\n\n%s", blocks, findings)), "Comment")
	_, err = p.enqueueImplement(ctx, it, 1, ImplementInput{Findings: findings, FixRound: true})
	return err
}

// countBlocks counts blocking review and critic verdicts since the item's
// latest triage, so a human retry starts with fresh rounds.
func (p *Pipeline) countBlocks(ctx context.Context, itemID int64) (int, error) {
	jobs, err := p.Store.ListJobs(ctx, store.JobFilter{ItemID: itemID})
	if err != nil {
		return 0, err
	}
	n := 0
	for _, j := range jobs { // newest first
		if j.Kind == "triage" {
			break
		}
		if (j.Kind != "review" && j.Kind != "critic") || j.State != state.Succeeded {
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
	pr, err := p.gh(it.Repo).FindPR(ctx, branch)
	if err != nil {
		return err
	}
	if pr == nil {
		pr, err = p.gh(it.Repo).CreatePR(ctx, fmt.Sprintf("%s (#%d)", is.Title, is.Number), body, branch, p.repoConf(it.Repo).BaseBranch, true)
		if err != nil {
			return err
		}
	} else if err := p.gh(it.Repo).UpdatePRBody(ctx, pr.Number, body); err != nil {
		return err
	}
	n := pr.Number
	if err := p.Store.UpdateItem(ctx, it.ID, store.ItemUpdate{PRNumber: &n}); err != nil {
		return err
	}
	if err := p.Store.SetStage(ctx, it.ID, state.AwaitingHuman, "daemon", fmt.Sprintf("draft PR #%d ready", pr.Number)); err != nil {
		return err
	}
	p.warn(p.gh(it.Repo).LabelPR(ctx, pr.Number, LabelAwaitingYou), "LabelPR")
	p.labelRisk(ctx, it, pr.Number)
	p.warn(p.gh(it.Repo).RemoveLabel(ctx, it.IssueNumber, LabelRunning), "RemoveLabel")
	p.warn(p.gh(it.Repo).AddLabels(ctx, it.IssueNumber, LabelAwaitingYou), "AddLabels")
	p.notify(ctx, fmt.Sprintf("%s-risk PR ready for review: %s %s %s", capitalize(riskOrLow(it.Risk)), p.ref(it.Repo, pr.Number), is.Title, pr.URL))
	return nil
}

func (p *Pipeline) prBody(ctx context.Context, it *store.WorkItem, in ReviewInput) (string, error) {
	jobs, err := p.Store.ListJobs(ctx, store.JobFilter{ItemID: it.ID})
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Closes #%d\n\nBuilt by the factory. Triage: complexity **%s**, risk **%s**. Verified commit `%s`.\n\n", it.IssueNumber, it.Complexity, it.Risk, short(in.Head))
	b.WriteString("## How to review\n\n" + p.reviewGuide(ctx, it, in.Features != "") + "\n")
	b.WriteString("## Gates\n\n" + in.Gates + "\n")
	if in.Features != "" {
		b.WriteString("## Blast radius\n\nWhat this change touches, from the feature map on `" + p.repoConf(it.Repo).BaseBranch + "`.\n\n" + in.Features + "\n")
	}
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
