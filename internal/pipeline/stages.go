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

// issue fetches the item's issue: the text that was approved, or errEdited
// (or errUnapproved).
func (p *Pipeline) issue(ctx context.Context, it *store.WorkItem) (github.Issue, error) {
	is, err := p.gh(it.Repo).GetIssue(ctx, it.IssueNumber)
	if err != nil {
		return github.Issue{}, err
	}
	if err := p.unchanged(ctx, it, *is); err != nil {
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
	_ = p.cancelAutoMerge(ctx, it, "the item was parked") // it notifies on failure
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
	if notApproved(err) {
		p.finish(ctx, j, agentRun{}, state.Cancelled, "", err.Error())
		p.parkEdited(ctx, it, err)
		return nil
	}
	if err != nil {
		return fail(agentRun{}, state.Failed, err.Error())
	}
	p.warn(p.gh(it.Repo).AddLabels(ctx, it.IssueNumber, LabelRunning), "AddLabels")
	ws, err := p.git(it.Repo).Prepare(ctx, dir, p.repoConf(it.Repo).BaseBranch, "", false, false)
	if err != nil {
		return fail(agentRun{}, state.Failed, err.Error())
	}
	p.warn(p.Store.SetJobRuntime(ctx, j.ID, "", "", ws.Start), "SetJobRuntime")
	prompt, err := p.renderStep(j, "triage", []string{it.Repo}, PromptData{Repo: it.Repo, Issue: is})
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
	// This build will push to the PR's branch: nothing it pushes may merge
	// before the gates and reviewer pass it.
	if err := p.cancelAutoMerge(ctx, it, "the factory is building on it again"); err != nil {
		p.finish(ctx, j, agentRun{}, state.Cancelled, "", err.Error())
		return p.park(ctx, it, "Auto-merge is on for its PR and the factory couldn't turn it off, so it won't push a new commit that could merge unchecked: "+err.Error())
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
	if notApproved(err) {
		p.finish(ctx, j, agentRun{}, state.Cancelled, "", err.Error())
		p.parkEdited(ctx, it, err)
		return nil
	}
	if err != nil {
		return fail(agentRun{}, state.Failed, err.Error())
	}
	// The builder runs the checks too, so the services run for the whole job.
	members := p.members(it)
	stop, err := p.StartServices(ctx, j.ID, members...)
	defer stop()
	if err != nil {
		// Another model tier can't start a database: say so and stop.
		p.finish(ctx, j, agentRun{}, state.Failed, "", err.Error())
		return p.park(ctx, it, fmt.Sprintf("The repo's services didn't start, so nothing was built:\n\n```\n%s\n```", truncate(err.Error(), 3000)))
	}
	p.warn(p.gh(it.Repo).AddLabels(ctx, it.IssueNumber, LabelRunning), "AddLabels")
	// A fresh attempt starts from the base branch; a fix round builds on the
	// branch the reviewer saw, in the repos the reviewer saw it in. A
	// workspace repo the last change left alone starts from base, so a
	// stale branch from an earlier run can't creep in.
	prev := map[string]bool{}
	if in.FixRound && len(members) > 1 {
		for _, h := range p.lastHeads(ctx, it) {
			prev[strings.ToLower(h.Repo)] = true
		}
	}
	onBranch := func(repo string) bool { return len(members) == 1 || !in.FixRound || prev[strings.ToLower(repo)] }
	parts, err := p.prepareParts(ctx, dir, it, func(repo string) string {
		if onBranch(repo) {
			return j.Branch
		}
		return ""
	}, !in.FixRound, true)
	if err != nil {
		return fail(agentRun{}, state.Failed, err.Error())
	}
	own := homePart(parts, it.Repo)
	p.warn(p.Store.SetJobRuntime(ctx, j.ID, "", "", own.ws.Start), "SetJobRuntime")

	work := agentDir(dir, parts, true)
	data := PromptData{Repo: it.Repo, Issue: is, OutDir: p.Runner.Paths(specFor(work, out)).Out,
		Findings: in.Findings, PreviousFailure: in.PreviousFailure, Risk: riskOrLow(it.Risk), Members: p.promptMembers(parts),
		TextOnly: textOnly(it)}
	for _, pt := range parts {
		for _, f := range p.riskyFeatures(ctx, pt.repo, pt.ws.Repo, pt.ws.BaseSHA) {
			if pt.folder != "" {
				f = pt.folder + ": " + f
			}
			data.Risky = append(data.Risky, f)
		}
	}
	if data.Members == nil {
		data.Install, data.Checks = p.repoConf(it.Repo).Gates.Install, p.requiredChecks(it.Repo)
	}
	for _, pt := range parts {
		data.Coverage = data.Coverage || p.repoConf(pt.repo).Gates.Coverage != ""
	}
	prompt, err := p.renderStep(j, "implement", partRepos(parts), data)
	if err != nil {
		return fail(agentRun{}, state.Failed, err.Error())
	}
	x := p.sessionFor(ctx, j, in.FixRound)
	x.repo = members[0]
	run := p.runAgentWith(ctx, j, work, out, prompt, "", p.Cfg.Timeout("implement"), x)
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

	// From here on the builder's checkouts are only read as files. Everything
	// below runs in the daemon's own repositories.
	msg := fmt.Sprintf("factory: %s %s\n\n%s\n\nJob %d, %s/%s, attempt %d.",
		p.ref(it.Repo, it.IssueNumber), is.Title, v.Summary, j.ID, j.Harness, j.Model, j.Attempt)
	if len(members) == 1 {
		msg = fmt.Sprintf("factory: #%d %s\n\n%s\n\nJob %d, %s/%s, attempt %d.", it.IssueNumber, is.Title, v.Summary, j.ID, j.Harness, j.Model, j.Attempt)
	}
	changed := false
	for _, pt := range parts {
		if err := realDir(pt.ws.Work); err != nil {
			return fail(run, state.Failed, err.Error())
		}
		head, ch, err := p.git(pt.repo).Ingest(ctx, pt.ws, msg)
		if err != nil {
			return fail(run, state.Failed, err.Error())
		}
		pt.head, changed = head, changed || ch
	}
	if !changed {
		return fail(run, state.Failed, "builder made no changes")
	}
	// The change is every repo whose branch is ahead of base: what this
	// round changed, and in a fix round what the last one did.
	var touched []*part
	for _, pt := range parts {
		base, err := p.git(pt.repo).MergeBase(ctx, pt.ws.Repo, pt.ws.BaseSHA, pt.head)
		if err != nil {
			return fail(run, state.Failed, err.Error())
		}
		if pt.base = base; base != pt.head {
			touched = append(touched, pt)
		}
	}
	if len(touched) == 0 {
		return fail(run, state.Failed, "builder made no changes")
	}
	var results []gates.Result
	var blasts []partBlast
	for _, pt := range touched {
		rs, blast, err := p.verify(ctx, j.ID, pt)
		if err != nil {
			return fail(run, state.Failed, err.Error())
		}
		for i := range rs {
			rs[i].Gate = pt.gate(rs[i].Gate)
		}
		results = append(results, rs...)
		blasts = append(blasts, partBlast{pt.folder, blast})
	}
	p.warn(p.Store.SetGateResults(ctx, j.ID, toStore(results)), "SetGateResults")
	line := gates.VerdictLine(results)
	if !gates.Passed(results) {
		return fail(run, state.Failed, line+"\n\n"+gateDetails(results))
	}

	primary := touched[0]
	var heads []store.RepoHead
	for _, pt := range touched {
		if pt == own {
			primary = pt
		}
		heads = append(heads, store.RepoHead{Repo: pt.repo, SHA: pt.head})
	}
	p.warn(p.Store.SetJobHead(ctx, j.ID, primary.head), "SetJobHead")
	if len(members) > 1 {
		p.warn(p.Store.SetJobHeads(ctx, j.ID, heads), "SetJobHeads")
	} else {
		heads = nil
	}
	// Before the job is marked done, so a restart that resumes at review
	// finds both: the risk raised and the blast radius saved.
	blastMD := p.applyBlast(ctx, it, j.ID, blasts)
	// Push exactly the commits that were verified. A workspace repo new to
	// a fix round replaces whatever an earlier run left on its branch.
	for _, pt := range touched {
		if err := p.git(pt.repo).Push(ctx, pt.ws.Repo, pt.head, j.Branch, !onBranch(pt.repo) || !in.FixRound); err != nil {
			if pt.folder != "" {
				err = fmt.Errorf("%s: %w", pt.repo, err)
			}
			return fail(run, state.Failed, "push: "+err.Error())
		}
	}
	branch := j.Branch
	p.warn(p.Store.UpdateItem(ctx, it.ID, store.ItemUpdate{Branch: &branch}), "UpdateItem")
	p.finish(ctx, j, run, state.Succeeded, string(norm), "")
	if err := p.Store.SetStage(ctx, it.ID, state.Reviewing, "daemon", line); err != nil {
		return err
	}
	return p.enqueueReview(ctx, it, "review", 1, ReviewInput{ImplementJob: j.ID, Head: primary.head, Heads: heads, Gates: line + "\n\n" + gateTable(results), Features: blastMD})
}

// verify runs the deterministic checks on one repo's commit under test:
// the diff guard on the daemon's repository, then install, the gates and
// the test-proves-fix check on exports, with a check after each that the
// commit's files are untouched.
func (p *Pipeline) verify(ctx context.Context, jobID int64, pt *part) ([]gates.Result, *blastRadius, error) {
	ws, base, head, repo := pt.ws, pt.base, pt.head, pt.repo
	g := p.repoConf(repo).Gates
	results := []gates.Result{gates.DiffGuard(ctx, ws.Repo, base, head, gates.Guard{
		MaxChangedLines: g.DiffGuard.MaxChangedLines, ProtectedPaths: g.DiffGuard.ProtectedPaths})}
	gateDir := filepath.Join(p.jobDir(jobID), "gate"+pt.sfx())
	if err := p.git(repo).Export(ctx, ws.Repo, head, gateDir); err != nil {
		return nil, nil, err
	}
	snap, err := takeSnapshot(gateDir)
	if err != nil {
		return nil, nil, err
	}
	// After the snapshot: they aren't the commit's, so a check may change them.
	if _, err := p.copyFilesInto(ctx, repo, gateDir); err != nil {
		return nil, nil, err
	}
	if g.Install != "" {
		r := gates.Result{Gate: "install", Status: gates.Pass}
		code, out, err := p.gateExec(ctx, jobID, gateDir, g.Install, gateRun{repo: repo, install: true})
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
		code, out, err := p.gateExec(ctx, jobID, gateDir, cmd, gateRun{repo: repo})
		if note := snap.tamperNote(gateDir); err == nil && note != "" {
			return 1, out + "\n\nfactory: " + note, nil
		}
		return code, out, err
	})...)
	if g.ProvesFix != "" {
		results = append(results, p.provesFix(ctx, jobID, pt, g.ProvesFix))
	}
	if g.Coverage != "" && gates.Passed(results) {
		results = append(results, p.coverage(ctx, jobID, pt, gateDir, snap))
	}
	// The checks of the features the change touches, then the scenarios:
	// slow end-to-end checks, run only on code that passes everything else.
	var blast *blastRadius
	if gates.Passed(results) {
		var fr []gates.Result
		fr, blast = p.featureChecks(ctx, jobID, pt, base, head, gateDir, snap)
		results = append(results, fr...)
	}
	if gates.Passed(results) {
		results = append(results, p.runScenarios(ctx, repo, jobID, gateDir, g.ScenariosDir, pt.sfx())...)
		if note := snap.tamperNote(gateDir); note != "" {
			results = append(results, gates.Result{Gate: "scenarios", Status: gates.Fail, Detail: "a hidden scenario run: " + note})
		}
	}
	return results, blast, nil
}

// provesFix reverts the non-test code to base, keeps the changed tests, and
// requires them to fail: a test that passes without the fix proves nothing.
func (p *Pipeline) provesFix(ctx context.Context, jobID int64, pt *part, command string) gates.Result {
	r := gates.Result{Gate: "proves_fix"}
	repo, base, head := pt.ws.Repo, pt.base, pt.head
	files, err := p.git(pt.repo).ChangedFiles(ctx, repo, base, head)
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
	dir := filepath.Join(p.jobDir(jobID), "proves"+pt.sfx())
	if err := p.git(pt.repo).Export(ctx, repo, base, dir); err != nil {
		r.Status, r.Detail = gates.Fail, err.Error()
		return r
	}
	for _, f := range tests {
		// A test file deleted at head is skipped; the diff guard reports it.
		if err := p.git(pt.repo).Show(ctx, repo, head, f, filepath.Join(dir, f)); err != nil {
			continue
		}
	}
	if _, err := p.copyFilesInto(ctx, pt.repo, dir); err != nil {
		r.Status, r.Detail = gates.Fail, err.Error()
		return r
	}
	var pkgs []string
	for k := range pkgSet {
		pkgs = append(pkgs, k)
	}
	sort.Strings(pkgs)
	cmd := strings.ReplaceAll(command, "{pkgs}", strings.Join(pkgs, " "))
	exit, output, err := p.gateExec(ctx, jobID, dir, cmd, gateRun{repo: pt.repo})
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
	if notApproved(err) {
		p.finish(ctx, j, agentRun{}, state.Cancelled, "", err.Error())
		p.parkEdited(ctx, it, err)
		return nil
	}
	if err != nil {
		return fail(agentRun{}, state.Failed, err.Error())
	}
	// A workspace repo the change doesn't touch is checked out at base, so
	// the reviewer can still read it.
	ws := len(in.Heads) > 0
	parts, err := p.prepareParts(ctx, dir, it, func(repo string) string {
		if _, ok := headOf(in.Heads, repo); ws && !ok {
			return ""
		}
		return j.Branch
	}, false, false)
	if err != nil {
		return fail(agentRun{}, state.Failed, err.Error())
	}
	for _, pt := range parts {
		want := in.Head
		if ws {
			var ok bool
			if want, ok = headOf(in.Heads, pt.repo); !ok {
				continue
			}
		}
		if want != "" && pt.ws.Start != want {
			// Someone pushed after the gates ran: those commits were never verified.
			p.finish(ctx, j, agentRun{}, state.Failed, "", "branch moved after the gates ran")
			where := ""
			if ws {
				where = " in " + pt.repo
			}
			return p.park(ctx, it, fmt.Sprintf("Branch `%s`%s moved to %s after the gates verified %s. Push changes through the factory, or review them yourself.",
				j.Branch, where, short(pt.ws.Start), short(want)))
		}
		if pt.base, err = p.git(pt.repo).MergeBase(ctx, pt.ws.Repo, pt.ws.BaseSHA, pt.ws.Start); err != nil {
			return fail(agentRun{}, state.Failed, err.Error())
		}
		pt.head = pt.ws.Start
	}
	own := homePart(parts, it.Repo)
	p.warn(p.Store.SetJobRuntime(ctx, j.ID, "", "", own.ws.Start), "SetJobRuntime")
	var diff string
	if ws {
		diff, err = p.combinedDiff(ctx, parts)
	} else {
		diff, err = p.git(it.Repo).Diff(ctx, own.ws.Repo, own.base, own.head)
	}
	if err != nil {
		return fail(agentRun{}, state.Failed, err.Error())
	}
	diff = truncate(diff, 200_000)
	// The control room's diff viewer shows exactly what the reviewer saw.
	p.warn(os.WriteFile(filepath.Join(dir, "diff.patch"), []byte(diff), 0o644), "save diff")
	// The reviewer sees the issue, the diff and gate results only: never the
	// builder's own summary.
	prompt, err := p.renderStep(j, "review", partRepos(parts), PromptData{Repo: it.Repo, Issue: is, Gates: in.Gates, Features: in.Features, Risk: riskOrLow(it.Risk), Diff: diff, Fence: fence(diff), Members: p.promptMembers(parts), TextOnly: textOnly(it)})
	if err != nil {
		return fail(agentRun{}, state.Failed, err.Error())
	}
	schema, _ := verdict.Schema("review")
	run := p.runAgent(ctx, p.members(it)[0], j, agentDir(dir, parts, false), out, prompt, schema, true, p.Cfg.Timeout("review"))
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
	p.recordFindings(ctx, j, it, &v)
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
	if p.parkEdited(ctx, it, err) {
		return nil
	}
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
// the item to a human, or to the forge's auto-merge when the repo's
// auto_merge covers it.
func (p *Pipeline) openPR(ctx context.Context, it *store.WorkItem, is github.Issue, in ReviewInput) error {
	if len(in.Heads) > 0 {
		return p.openWorkspacePRs(ctx, it, is, in)
	}
	auto := p.autoMergeWanted(ctx, it, in)
	body, err := p.prBody(ctx, it, in, fmt.Sprintf("Closes #%d", it.IssueNumber), auto)
	if err != nil {
		return err
	}
	branch := p.branchFor(it)
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
	if auto {
		if p.autoMerge(ctx, it, is, pr, in.Head) {
			return nil
		}
		// The body promised a merge that won't happen.
		p.warn(p.gh(it.Repo).UpdatePRBody(ctx, pr.Number, strings.Replace(body, autoFooter, humanFooter, 1)), "UpdatePRBody")
	}
	p.warn(p.gh(it.Repo).LabelPR(ctx, pr.Number, LabelAwaitingYou), "LabelPR")
	p.labelRisk(ctx, it, it.Repo, pr.Number)
	p.warn(p.gh(it.Repo).RemoveLabel(ctx, it.IssueNumber, LabelRunning), "RemoveLabel")
	p.warn(p.gh(it.Repo).AddLabels(ctx, it.IssueNumber, LabelAwaitingYou), "AddLabels")
	p.notify(ctx, fmt.Sprintf("%s-risk PR ready for review: %s %s %s", capitalize(riskOrLow(it.Risk)), p.ref(it.Repo, pr.Number), is.Title, pr.URL))
	return nil
}

// prBody is a PR's description: top (what it closes, and in a workspace
// the linked PRs), then the evidence.
func (p *Pipeline) prBody(ctx context.Context, it *store.WorkItem, in ReviewInput, top string, auto bool) (string, error) {
	jobs, err := p.Store.ListJobs(ctx, store.JobFilter{ItemID: it.ID})
	if err != nil {
		return "", err
	}
	var b strings.Builder
	verified := fmt.Sprintf("Verified commit `%s`.", short(in.Head))
	if len(in.Heads) > 0 {
		var cs []string
		for _, h := range in.Heads {
			cs = append(cs, fmt.Sprintf("%s `%s`", h.Repo, short(h.SHA)))
		}
		verified = "Verified commits: " + strings.Join(cs, ", ") + "."
	}
	fmt.Fprintf(&b, "%s\n\nBuilt by the factory. Complexity **%s**, risk **%s**. %s\n\n", strings.TrimRight(top, "\n"), it.Complexity, riskOrLow(it.Risk), verified)
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
		fmt.Fprintf(&b, "| %d | %s | %s | %s | %s | %d | %s | $%.2f |\n", j.ID, j.Kind, j.Harness, j.ModelLabel(), sk, j.Attempt, j.State, j.CostUSD)
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
	if auto {
		b.WriteString(autoFooter)
	} else {
		b.WriteString(humanFooter)
	}
	return b.String(), nil
}

// partRepos names the repos a change spans, in order.
func partRepos(parts []*part) []string {
	var out []string
	for _, pt := range parts {
		out = append(out, pt.repo)
	}
	return out
}

const (
	autoFooter  = "_This change is within the repo's `auto_merge` setting, so it merges itself once its required checks pass. To stop that, turn off auto-merge on this PR, park the item, or close the PR._\n"
	humanFooter = "_Merge is always human. Review the diff before merging._\n"
)

// autoMergeWanted says whether the item's PR should merge itself: the
// repo's auto_merge covers its risk and size, it is one repo's change, and
// the feature map that rates its risk could be read.
func (p *Pipeline) autoMergeWanted(ctx context.Context, it *store.WorkItem, in ReviewInput) bool {
	if len(in.Heads) > 0 || p.inWorkspace(it) || in.Head == "" {
		return false
	}
	if _, ok := p.gh(it.Repo).(AutoMerger); !ok {
		return false
	}
	return p.repoConf(it.Repo).AutoMerge.Allows(it.Risk, it.Complexity) && !p.blastUnread(ctx, it.ID, in.ImplementJob)
}

// autoMerge hands a PR to the forge to merge once its checks pass, pinned
// to the verified commit. It reports whether it did; otherwise the PR
// waits for a human as usual.
func (p *Pipeline) autoMerge(ctx context.Context, it *store.WorkItem, is github.Issue, pr *github.PR, head string) bool {
	am := p.repoConf(it.Repo).AutoMerge
	m := p.gh(it.Repo).(AutoMerger)
	// Recorded first: if the daemon stops between the forge turning it on
	// and the record, a later park or build must still turn it off.
	if err := p.Store.AddEvent(ctx, store.Event{Actor: "daemon", ItemID: &it.ID, Type: "auto_merge",
		Reason: fmt.Sprintf("PR #%d", pr.Number), Data: fmt.Sprintf(`{"head":%q,"method":%q}`, head, am.MergeMethod())}); err != nil {
		p.log().Warn("auto-merge", "pr", pr.Number, "err", err)
		return false
	}
	if err := m.AutoMerge(ctx, pr.Number, head, am.MergeMethod()); err != nil {
		p.log().Warn("auto-merge", "pr", pr.Number, "err", err)
		// It may have taken effect anyway (a timeout): make sure it's off.
		_ = p.cancelAutoMerge(ctx, it, "turning it on failed")
		p.notify(ctx, fmt.Sprintf("Couldn't turn on auto-merge for %s, so it waits for you: %v", p.ref(it.Repo, pr.Number), err))
		return false
	}
	p.warn(p.gh(it.Repo).LabelPR(ctx, pr.Number, LabelAutoMerge), "LabelPR")
	p.labelRisk(ctx, it, it.Repo, pr.Number)
	p.warn(p.gh(it.Repo).RemoveLabel(ctx, it.IssueNumber, LabelRunning), "RemoveLabel")
	p.warn(p.gh(it.Repo).AddLabels(ctx, it.IssueNumber, LabelAutoMerge), "AddLabels")
	p.notify(ctx, fmt.Sprintf("%s-risk PR will merge itself once its checks pass: %s %s %s", capitalize(it.Risk), p.ref(it.Repo, pr.Number), is.Title, pr.URL))
	return true
}

// autoMerged says whether auto-merge is on for the item's PR: the factory
// turned it on and hasn't turned it off since.
func (p *Pipeline) autoMerged(ctx context.Context, it *store.WorkItem) bool {
	if it.PRNumber == 0 {
		return false
	}
	evs, err := p.Store.Events(ctx, it.ID)
	if err != nil {
		return true // unsure: cancelling is harmless, a missed cancel isn't
	}
	on := false
	for _, e := range evs {
		if e.Reason == fmt.Sprintf("PR #%d", it.PRNumber) {
			switch e.Type {
			case "auto_merge":
				on = true
			case "auto_merge_off":
				on = false
			}
		}
	}
	return on
}

// cancelAutoMerge turns auto-merge off on the item's PR if the factory
// turned it on, so nothing merges that the gates and reviewer haven't
// passed.
func (p *Pipeline) cancelAutoMerge(ctx context.Context, it *store.WorkItem, why string) error {
	if !p.autoMerged(ctx, it) {
		return nil
	}
	gh := p.gh(it.Repo)
	m, ok := gh.(AutoMerger)
	if !ok {
		return nil
	}
	if err := m.CancelAutoMerge(ctx, it.PRNumber); err != nil {
		p.log().Warn("cancel auto-merge", "pr", it.PRNumber, "err", err)
		p.notify(ctx, fmt.Sprintf("Couldn't turn off auto-merge on %s (%s): %v. Turn it off on the PR yourself.", p.ref(it.Repo, it.PRNumber), why, err))
		return err
	}
	p.warn(p.Store.AddEvent(ctx, store.Event{Actor: "daemon", ItemID: &it.ID, Type: "auto_merge_off", Reason: fmt.Sprintf("PR #%d", it.PRNumber), Data: fmt.Sprintf(`{"why":%q}`, why)}), "AddEvent")
	p.warn(gh.UnlabelPR(ctx, it.PRNumber, LabelAutoMerge), "UnlabelPR")
	p.warn(gh.RemoveLabel(ctx, it.IssueNumber, LabelAutoMerge), "RemoveLabel")
	return nil
}
