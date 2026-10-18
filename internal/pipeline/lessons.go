package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/claptimes5/factory/internal/github"
	"github.com/claptimes5/factory/internal/state"
	"github.com/claptimes5/factory/internal/store"
	"github.com/claptimes5/factory/internal/verdict"
)

// The learn loop: what a human changed in a factory PR before merging it,
// and blocking findings that repeat across items, become signals. Once a
// day per repo, when there's enough to go on, a read-only learn job turns
// them into lines for the repo's AGENTS.md, and the daemon proposes those
// in a draft PR of its own on LessonsBranch. That PR is no item's, so it
// never auto-merges and no builder ever runs on its branch.

// LessonsBranch is the branch of the lessons PR.
const LessonsBranch = "factory/lessons"

// lessonsHeading is the AGENTS.md section the lessons go under.
const lessonsHeading = "## Lessons from the factory"

// Limits that keep a learn job's input small.
const (
	maxEditDiff   = 20_000              // bytes of one human edit's diff
	maxEdits      = 5                   // human edits per learn job
	maxGroups     = 10                  // finding groups per learn job
	maxGroupSize  = 10                  // findings per group
	signalHorizon = 30 * 24 * time.Hour // older unused signals are left out
)

// learnDay is the day a learn job is counted against; tests move it.
var learnDay = store.Today

// LearnInput is carried by a learn job: its repo and the signals it reads.
type LearnInput struct {
	Repo    string  `json:"repo"`
	Signals []int64 `json:"signals"`
}

// --- signals ---

// recordHumanEdit saves what a human pushed to a factory PR after the
// factory's last push (from), up to the head it merged at. Without the
// diff there's nothing to learn from, so a failure records nothing.
func (p *Pipeline) recordHumanEdit(ctx context.Context, it *store.WorkItem, pr *github.PR, from string) {
	if !p.Cfg.Learning.On() || from == "" || pr.Head.SHA == "" || from == pr.Head.SHA {
		return
	}
	diff, err := p.humanDiff(ctx, it.Repo, pr, from)
	if err != nil {
		p.log().Warn("lessons: human edit", "pr", pr.Number, "err", err)
		return
	}
	if strings.TrimSpace(diff) == "" {
		return
	}
	p.warn(p.Store.AddSignal(ctx, store.Signal{Repo: it.Repo, Kind: store.SignalHumanEdit, ItemID: it.ID, IssueNumber: it.IssueNumber,
		PRNumber: pr.Number, Diff: diff}), "AddSignal")
}

// humanDiff is the patch from the factory's head to a merged PR's head,
// read from a bare clone that is deleted after.
func (p *Pipeline) humanDiff(ctx context.Context, repo string, pr *github.PR, from string) (string, error) {
	root := p.Cfg.Path(p.Cfg.DataDir)
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp(root, "lessons-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	g := p.git(repo)
	bare := filepath.Join(dir, "repo.git")
	if err := g.CloneBare(ctx, bare); err != nil {
		return "", err
	}
	// The branch may be gone after the merge; the PR's own ref isn't.
	ref := fmt.Sprintf("refs/pull/%d/head", pr.Number)
	if p.repoConf(repo).GitLab() {
		ref = fmt.Sprintf("refs/merge-requests/%d/head", pr.Number)
	}
	for _, sha := range []string{pr.Head.SHA, from} {
		if g.hasCommit(ctx, bare, sha) {
			continue
		}
		got, err := g.FetchRef(ctx, bare, ref)
		if err != nil || got != sha {
			if _, err := g.FetchRef(ctx, bare, sha); err != nil {
				return "", err
			}
		}
		if !g.hasCommit(ctx, bare, sha) {
			return "", fmt.Errorf("commit %s isn't on the remote", short(sha))
		}
	}
	diff, err := g.Diff(ctx, bare, from, pr.Head.SHA)
	if err != nil {
		return "", err
	}
	return capDiff(diff, maxEditDiff), nil
}

// hasCommit reports whether sha is a commit in repo.
func (g *Git) hasCommit(ctx context.Context, repo, sha string) bool {
	_, err := g.run(ctx, repo, nil, "cat-file", "-e", sha+"^{commit}")
	return err == nil
}

// capDiff keeps a patch under max bytes, whole hunks only, and says how
// many it left out.
func capDiff(diff string, max int) string {
	if len(diff) <= max {
		return diff
	}
	var b strings.Builder
	left := 0
	for _, file := range splitBefore(diff, "diff --git ") {
		header, hunks := file, []string(nil)
		if i := strings.Index(file, "\n@@"); i >= 0 {
			header, hunks = file[:i+1], splitBefore(file[i+1:], "@@")
		}
		wrote := false
		for _, h := range hunks {
			need := len(h)
			if !wrote {
				need += len(header)
			}
			if b.Len()+need > max {
				left++
				continue
			}
			if !wrote {
				b.WriteString(header)
				wrote = true
			}
			b.WriteString(h)
		}
		if len(hunks) == 0 && b.Len()+len(header) <= max {
			b.WriteString(header) // a binary file, a rename: no hunks
		}
	}
	if left > 0 {
		fmt.Fprintf(&b, "\n… %d more %s left out (the diff is over %d KB)\n", left, plural(left, "hunk", "hunks"), max/1000)
	}
	return b.String()
}

// splitBefore splits s before each line that starts with prefix.
func splitBefore(s, prefix string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); {
		j := strings.IndexByte(s[i:], '\n')
		next := len(s)
		if j >= 0 {
			next = i + j + 1
		}
		if i > start && strings.HasPrefix(s[i:], prefix) {
			out = append(out, s[start:i])
			start = i
		}
		i = next
	}
	return append(out, s[start:])
}

// recordFindings saves a blocking verdict's critical and major findings.
func (p *Pipeline) recordFindings(ctx context.Context, j *store.Job, it *store.WorkItem, v *verdict.Review) {
	if !p.Cfg.Learning.On() || !v.Blocks() {
		return
	}
	for _, f := range v.Findings {
		if f.Severity == "minor" {
			continue
		}
		p.warn(p.Store.AddSignal(ctx, store.Signal{Repo: it.Repo, Kind: store.SignalFinding, ItemID: it.ID, IssueNumber: it.IssueNumber,
			JobID: j.ID, PRNumber: it.PRNumber, File: f.File, Claim: truncate(f.Claim, 500), Severity: f.Severity, Evidence: truncate(f.Evidence, 500)}), "AddSignal")
	}
}

// --- when to learn ---

// learnTriggers queues a learn job for each repo with something to learn
// from, at most once a day per repo.
func (p *Pipeline) learnTriggers(ctx context.Context) {
	if !p.Cfg.Learning.On() {
		return
	}
	r, ok := p.Cfg.Routing["learn"]
	if !ok {
		return
	}
	since := time.Now().Add(-signalHorizon).UTC().Format(store.TimeFormat)
	for _, repo := range p.Cfg.RepoNames() {
		key := learnDay() + " " + repo
		if seen, err := p.Store.HasEvent(ctx, "learn_queued", key); err != nil || seen {
			continue
		}
		sigs, err := p.Store.UnusedSignals(ctx, repo, since)
		if err != nil {
			p.warn(err, "UnusedSignals")
			continue
		}
		var ids []int64
		for _, s := range learnable(sigs, p.Cfg.Learning.MinFindings) {
			ids = append(ids, s.ID)
		}
		if len(ids) == 0 {
			continue
		}
		b, _ := json.Marshal(LearnInput{Repo: repo, Signals: ids})
		fast := r.FastFor(r.Harness, r.Model)
		if _, err := p.Store.EnqueueJob(ctx, store.Job{Kind: "learn", Harness: r.Harness, Model: r.Model, Effort: r.Effort, Fast: fast,
			Skillset: r.Skills, Route: "learn", Priority: -1, BudgetUSD: p.Cfg.FastBudget(p.Cfg.LearnBudget(), fast), Input: string(b)}); err != nil {
			p.warn(err, "enqueue learn")
			continue
		}
		p.warn(p.Store.AddEvent(ctx, store.Event{Actor: "daemon", Type: "learn_queued", Reason: key, Data: fmt.Sprintf(`{"signals":%d}`, len(ids))}), "AddEvent")
	}
}

// learnable picks the signals worth a learn job: every human edit, and
// each group of alike findings with at least min of them from two or more
// items. With neither there's nothing to learn yet. Findings left out wait
// for one like them.
func learnable(sigs []store.Signal, min int) []store.Signal {
	var edits, findings []store.Signal
	for _, s := range sigs {
		switch s.Kind {
		case store.SignalHumanEdit:
			edits = append(edits, s)
		case store.SignalFinding:
			findings = append(findings, s)
		}
	}
	if len(edits) > maxEdits {
		edits = edits[len(edits)-maxEdits:] // the newest
	}
	out := edits
	n := 0
	for _, g := range groupFindings(findings) {
		items := map[int64]bool{}
		for _, f := range g {
			items[f.ItemID] = true
		}
		if len(g) < min || len(items) < 2 || n == maxGroups {
			continue
		}
		n++
		if len(g) > maxGroupSize {
			g = g[len(g)-maxGroupSize:]
		}
		out = append(out, g...)
	}
	return out
}

// groupFindings puts findings that may be the same mistake together: in
// files that match one glob (same folder and extension), or with claims
// that share most of their words. The learn job judges whether they are.
func groupFindings(fs []store.Signal) [][]store.Signal {
	parent := make([]int, len(fs))
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(i int) int {
		if parent[i] != i {
			parent[i] = find(parent[i])
		}
		return parent[i]
	}
	words := make([]map[string]bool, len(fs))
	for i, f := range fs {
		words[i] = claimWords(f.Claim)
	}
	for i := range fs {
		for k := i + 1; k < len(fs); k++ {
			if g := fileGlob(fs[i].File); (g != "" && g == fileGlob(fs[k].File)) || alikeWords(words[i], words[k]) {
				parent[find(k)] = find(i)
			}
		}
	}
	var out [][]store.Signal
	at := map[int]int{}
	for i, f := range fs {
		r := find(i)
		if _, ok := at[r]; !ok {
			at[r] = len(out)
			out = append(out, nil)
		}
		out[at[r]] = append(out[at[r]], f)
	}
	return out
}

// fileGlob is the folder and extension of a path, such as
// internal/pipeline/*.go, or the path itself when it has no extension.
func fileGlob(path string) string {
	path = strings.TrimSpace(strings.ReplaceAll(path, `\`, "/"))
	if path == "" {
		return ""
	}
	ext := filepath.Ext(path)
	if ext == "" {
		return path
	}
	return filepath.ToSlash(filepath.Join(filepath.Dir(path), "*"+ext))
}

var nonWord = regexp.MustCompile(`[^a-z0-9_]+`)

var stopWords = map[string]bool{"the": true, "and": true, "for": true, "not": true, "but": true, "with": true, "this": true, "that": true,
	"are": true, "was": true, "its": true, "from": true, "into": true, "when": true, "has": true, "have": true, "can": true, "does": true,
	"which": true, "will": true, "would": true, "should": true, "any": true, "all": true, "isn": true, "doesn": true, "there": true}

// claimWords are a claim's words, lowercased and roughly singular, without
// the short and common ones.
func claimWords(claim string) map[string]bool {
	out := map[string]bool{}
	for _, w := range nonWord.Split(strings.ToLower(claim), -1) {
		if len(w) < 3 || stopWords[w] {
			continue
		}
		if len(w) > 3 && strings.HasSuffix(w, "s") && !strings.HasSuffix(w, "ss") {
			w = w[:len(w)-1]
		}
		out[w] = true
	}
	return out
}

// alikeWords says whether two claims share at least two words and half of
// all the words either uses.
func alikeWords(a, b map[string]bool) bool {
	shared := 0
	for w := range a {
		if b[w] {
			shared++
		}
	}
	all := len(a) + len(b) - shared
	return shared >= 2 && 2*shared >= all
}

// --- the learn job ---

type learnEdit struct {
	Ref  string // the issue, such as #12
	PR   int
	Diff string
}

type learnFinding struct {
	Ref, Severity, File, Claim, Evidence string
}

type learnGroup struct {
	N        int
	Findings []learnFinding
}

// learnPrompt is what prompts/learn.md can reference.
type learnPrompt struct {
	Repo     string
	AgentsMD string   // the base branch's AGENTS.md, or ""
	ClaudeMD string   // the base branch's CLAUDE.md, or ""
	Proposed []string // lines in the open lessons PR not on the base yet
	Declined []string // lines a human turned down: never again
	Edits    []learnEdit
	Groups   []learnGroup
	Fence    string // a code fence none of the above can close
}

func (p *Pipeline) runLearn(ctx context.Context, j *store.Job) error {
	dir := p.jobDir(j.ID)
	out := filepath.Join(dir, "out")
	defer os.RemoveAll(filepath.Join(dir, "lessons"))
	// The signals stay unused, so the next day's run reads them again.
	fail := func(run agentRun, st state.JobState, msg string) error {
		if p.stopIfCancelled(ctx, j, nil, run) {
			return nil
		}
		p.finish(ctx, j, run, st, "", msg)
		return nil
	}
	var in LearnInput
	if err := json.Unmarshal([]byte(j.Input), &in); err != nil || in.Repo == "" {
		return fail(agentRun{}, state.Failed, fmt.Sprintf("bad job input: %v", err))
	}
	sigs, err := p.Store.SignalsByID(ctx, in.Signals)
	if err != nil {
		return fail(agentRun{}, state.Failed, err.Error())
	}
	g := p.git(in.Repo)
	ws, err := g.Prepare(ctx, dir, p.repoConf(in.Repo).BaseBranch, "", false, false)
	if err != nil {
		return fail(agentRun{}, state.Failed, err.Error())
	}
	p.warn(p.Store.SetJobRuntime(ctx, j.ID, "", "", ws.Start), "SetJobRuntime")
	agents, _, err := g.FileAt(ctx, ws.Repo, ws.BaseSHA, "AGENTS.md")
	if err != nil {
		return fail(agentRun{}, state.Failed, err.Error())
	}
	claude, _, _ := g.FileAt(ctx, ws.Repo, ws.BaseSHA, "CLAUDE.md")
	data := learnPrompt{Repo: in.Repo, AgentsMD: truncate(agents, 30_000), ClaudeMD: truncate(claude, 30_000)}
	if pr, _ := p.gh(in.Repo).FindPR(ctx, LessonsBranch); pr != nil {
		if branch, ok, _ := g.FileAt(ctx, ws.Repo, "refs/remotes/origin/"+LessonsBranch, "AGENTS.md"); ok {
			data.Proposed = newLines(agents, branch)
		}
	}
	data.Declined = p.settleLessons(ctx, in.Repo, ws, agents)
	var findings []store.Signal
	for _, s := range sigs {
		switch s.Kind {
		case store.SignalHumanEdit:
			data.Edits = append(data.Edits, learnEdit{Ref: p.ref(in.Repo, s.IssueNumber), PR: s.PRNumber, Diff: s.Diff})
		case store.SignalFinding:
			findings = append(findings, s)
		}
	}
	for _, grp := range groupFindings(findings) {
		lg := learnGroup{N: len(data.Groups) + 1}
		for _, f := range grp {
			lg.Findings = append(lg.Findings, learnFinding{Ref: p.ref(in.Repo, f.IssueNumber), Severity: f.Severity, File: oneLine(f.File), Claim: oneLine(f.Claim), Evidence: oneLine(f.Evidence)})
		}
		data.Groups = append(data.Groups, lg)
	}
	var all strings.Builder
	all.WriteString(data.AgentsMD + data.ClaudeMD)
	for _, e := range data.Edits {
		all.WriteString(e.Diff)
	}
	for _, grp := range data.Groups {
		for _, f := range grp.Findings {
			all.WriteString(f.File + f.Claim + f.Evidence)
		}
	}
	data.Fence = fence(all.String())
	prompt, err := p.renderStep(j, "learn", []string{in.Repo}, data)
	if err != nil {
		return fail(agentRun{}, state.Failed, err.Error())
	}
	schema, _ := verdict.Schema("learn")
	run := p.runAgent(ctx, in.Repo, j, ws.Repo, out, prompt, schema, true, p.Cfg.Timeout("learn"))
	if p.stopIfCancelled(ctx, j, nil, run) {
		return nil
	}
	if st, msg, bad := runFailure(run); bad {
		return fail(run, st, msg)
	}
	var v verdict.Learn
	if err := verdict.Parse([]byte(run.final), &v); err != nil {
		return fail(run, state.Failed, err.Error())
	}
	if err := p.proposeLessons(ctx, j, in.Repo, v.Lessons, sigs); err != nil {
		return fail(run, state.Failed, "lessons PR: "+err.Error())
	}
	norm, _ := json.Marshal(v)
	p.finish(ctx, j, run, state.Succeeded, string(norm), "")
	p.warn(p.Store.UseSignals(ctx, in.Signals, j.ID), "UseSignals")
	return nil
}

// settleLessons records what became of the lines the factory proposed:
// merged, or declined when their PR closed unmerged, or when a human took
// them out of the PR before merging it. It returns every declined line.
// base is AGENTS.md on the base branch of the checkout ws.
func (p *Pipeline) settleLessons(ctx context.Context, repo string, ws *Workspace, base string) []string {
	ls, err := p.Store.Lessons(ctx, repo)
	if err != nil {
		p.warn(err, "Lessons")
		return nil
	}
	has := func(md, norm string) bool {
		for _, l := range strings.Split(md, "\n") {
			if normLine(l) == norm {
				return true
			}
		}
		return false
	}
	open, _ := p.gh(repo).FindPR(ctx, LessonsBranch)
	if open != nil && (open.State != "open" || open.Merged) {
		open = nil
	}
	branch, onBranch := "", false
	if open != nil {
		branch, onBranch, _ = p.git(repo).FileAt(ctx, ws.Repo, "refs/remotes/origin/"+LessonsBranch, "AGENTS.md")
	}
	prs := map[int]*github.PR{}
	var declined []string
	for _, l := range ls {
		if l.Status == store.LessonProposed {
			status := ""
			if open != nil && l.PRNumber == open.Number {
				if onBranch && !has(branch, l.Norm) && !has(base, l.Norm) {
					status = store.LessonDeclined // taken out of the open PR
				}
			} else {
				pr, ok := prs[l.PRNumber]
				if !ok {
					if pr, err = p.gh(repo).GetPR(ctx, l.PRNumber); err != nil {
						pr = nil
					}
					prs[l.PRNumber] = pr
				}
				switch {
				case pr == nil:
				case pr.Merged && has(base, l.Norm):
					status = store.LessonMerged
				case pr.Merged || pr.State == "closed":
					status = store.LessonDeclined // closed, or merged without it
				}
			}
			if status != "" {
				p.warn(p.Store.DecideLesson(ctx, l.ID, status), "DecideLesson")
				l.Status = status
			}
		}
		if l.Status == store.LessonDeclined {
			declined = append(declined, l.Line)
		}
	}
	return declined
}

// oneLine joins a text's lines, for a list item.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// --- the lessons PR ---

// proposeLessons adds the lessons not already in AGENTS.md, on the base
// branch or in the open lessons PR, to that PR, opening it if there is
// none. It never merges: the PR waits for a human, whatever auto_merge
// says.
func (p *Pipeline) proposeLessons(ctx context.Context, j *store.Job, repo string, lessons []verdict.Lesson, sigs []store.Signal) error {
	if len(lessons) == 0 {
		return nil
	}
	rc := p.repoConf(repo)
	g, gh := p.git(repo), p.gh(repo)
	open, err := gh.FindPR(ctx, LessonsBranch)
	if err != nil {
		return err
	}
	if open != nil && (open.State != "open" || open.Merged) {
		open = nil
	}
	// With no open PR, a branch left by a closed one starts over from base.
	ws, err := g.PrepareAt(ctx, filepath.Join(p.jobDir(j.ID), "lessons", "repo"), "", rc.BaseBranch, LessonsBranch, open == nil)
	if err != nil {
		return err
	}
	if mode, err := g.Mode(ctx, ws.Repo, "HEAD", "AGENTS.md"); err != nil {
		return err
	} else if mode != "" && mode != "100644" && mode != "100755" {
		return fmt.Errorf("AGENTS.md isn't a regular file (mode %s), so the factory won't write to it", mode)
	}
	base, _, err := g.FileAt(ctx, ws.Repo, ws.BaseSHA, "AGENTS.md")
	if err != nil {
		return err
	}
	cur, _, err := g.FileAt(ctx, ws.Repo, "HEAD", "AGENTS.md")
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, l := range strings.Split(base+"\n"+cur, "\n") {
		seen[normLine(l)] = true
	}
	known, err := p.Store.Lessons(ctx, repo)
	if err != nil {
		return err
	}
	for _, l := range known {
		if l.Status == store.LessonDeclined {
			seen[l.Norm] = true
		}
	}
	var add []verdict.Lesson
	for _, l := range lessons {
		l.Line = cleanLine(l.Line)
		if k := normLine(l.Line); k != "" && !seen[k] {
			seen[k] = true
			add = append(add, l)
		}
	}
	if len(add) == 0 {
		return nil
	}
	var lines []string
	for _, l := range add {
		lines = append(lines, l.Line)
	}
	if err := os.WriteFile(filepath.Join(ws.Repo, "AGENTS.md"), []byte(appendLessons(cur, lines)), 0o644); err != nil {
		return err
	}
	sha, err := g.commitFile(ctx, ws.Repo, "AGENTS.md", fmt.Sprintf("factory: %d %s for AGENTS.md\n\nLearned by job %d.", len(add), plural(len(add), "lesson", "lessons"), j.ID))
	if err != nil {
		return err
	}
	if err := g.Push(ctx, ws.Repo, sha, LessonsBranch, open == nil); err != nil {
		return fmt.Errorf("push: %w", err)
	}
	entries := p.lessonEntries(repo, add, sigs)
	pr, verb := open, "Added"
	if open == nil {
		verb = "Opened"
		pr, err = gh.CreatePR(ctx, "Lessons from the factory for AGENTS.md", lessonsIntro+entries+"\n"+lessonsFooter, LessonsBranch, rc.BaseBranch, true)
		if err != nil {
			return err
		}
	} else {
		body := open.Body
		if i := strings.Index(body, lessonsFooter); i >= 0 {
			body = body[:i] + entries + "\n" + body[i:]
		} else {
			body = strings.TrimRight(body, "\n") + "\n\n" + entries
		}
		if err := gh.UpdatePRBody(ctx, open.Number, body); err != nil {
			return err
		}
	}
	for _, l := range lines {
		p.warn(p.Store.AddLesson(ctx, store.Lesson{Repo: repo, Line: l, Norm: normLine(l), PRNumber: pr.Number}), "AddLesson")
	}
	p.warn(gh.LabelPR(ctx, pr.Number, LabelAwaitingYou), "LabelPR")
	b, _ := json.Marshal(map[string]any{"lines": lines})
	p.warn(p.Store.AddEvent(ctx, store.Event{Actor: "daemon", JobID: &j.ID, Type: "lessons_pr", Reason: fmt.Sprintf("%s PR #%d", repo, pr.Number), Data: string(b)}), "AddEvent")
	what := fmt.Sprintf("%d %s for AGENTS.md", len(add), plural(len(add), "lesson", "lessons"))
	if verb == "Opened" {
		p.notify(ctx, fmt.Sprintf("Opened a lessons PR with %s: %s %s", what, p.ref(repo, pr.Number), pr.URL))
	} else {
		p.notify(ctx, fmt.Sprintf("Added %s to the lessons PR: %s %s", what, p.ref(repo, pr.Number), pr.URL))
	}
	return nil
}

const (
	lessonsIntro = "The factory learned these from the changes you made to its PRs before merging them, and from findings its reviewers repeated. " +
		"Each is a line for `AGENTS.md`, which builders read before they start.\n\n" +
		"Edit or drop any of them on this branch before you merge. New lessons are added here until you merge or close it.\n\n## Lessons\n\n"
	lessonsFooter = "_Merge is always human. This PR never merges itself._\n"
)

// lessonEntries lists lessons for the PR body: each line, why, and links
// to the issues and PRs it came from.
func (p *Pipeline) lessonEntries(repo string, ls []verdict.Lesson, sigs []store.Signal) string {
	rc := p.repoConf(repo)
	issues, prs := map[int]bool{}, map[int]bool{}
	for _, s := range sigs {
		issues[s.IssueNumber] = true
		if s.PRNumber > 0 {
			prs[s.PRNumber] = true
		}
	}
	var b strings.Builder
	for _, l := range ls {
		fmt.Fprintf(&b, "- **%s**\n  %s", l.Line, oneLine(l.Why))
		var links []string
		for _, e := range l.Evidence {
			switch kind, n := evidenceRef(e); {
			case prs[n] && (kind == "pr" || kind == "either"):
				links = append(links, fmt.Sprintf("[PR %d](%s)", n, rc.PRURL(n)))
			case issues[n] && (kind == "issue" || kind == "either"):
				links = append(links, fmt.Sprintf("[#%d](%s)", n, rc.IssueURL(n)))
			}
		}
		if len(links) > 0 {
			b.WriteString(" (" + strings.Join(links, ", ") + ")")
		}
		b.WriteString("\n")
	}
	return b.String()
}

var (
	prEvidence    = regexp.MustCompile(`(?i)^(?:(?:pr|mr|pull request|merge request)\s*[#!]?|!)\s*(\d+)$`)
	issueEvidence = regexp.MustCompile(`^(?:[\w.-]+/[\w.-]+)?#(\d+)$`)
	bareEvidence  = regexp.MustCompile(`^(\d+)$`)
)

// evidenceRef reads "PR #101" (or !101) as a PR, "#12" as an issue, and a
// bare number as either.
func evidenceRef(e string) (string, int) {
	e = strings.TrimSpace(e)
	for _, r := range []struct {
		kind string
		re   *regexp.Regexp
	}{{"pr", prEvidence}, {"issue", issueEvidence}, {"either", bareEvidence}} {
		if m := r.re.FindStringSubmatch(e); m != nil {
			n, _ := strconv.Atoi(m[1])
			return r.kind, n
		}
	}
	return "", 0
}

// cleanLine makes a proposed lesson one plain line: no bullet, heading or
// line break, and not too long.
func cleanLine(s string) string {
	s = oneLine(s)
	s = strings.TrimSpace(strings.TrimLeft(s, "-*#> "))
	return truncate(s, 300)
}

// normLine is a line as dedup compares it: no bullet, case, spacing or
// final period.
func normLine(s string) string {
	s = strings.ToLower(oneLine(strings.TrimLeft(strings.TrimSpace(s), "-*+ ")))
	return strings.TrimRight(s, ".!;: ")
}

// newLines are the lines of after that before doesn't have, such as the
// lessons an open PR adds.
func newLines(before, after string) []string {
	had := map[string]bool{}
	for _, l := range strings.Split(before, "\n") {
		had[normLine(l)] = true
	}
	var out []string
	for _, l := range strings.Split(after, "\n") {
		if k := normLine(l); k != "" && !had[k] && !strings.HasPrefix(strings.TrimSpace(l), "#") {
			out = append(out, cleanLine(l))
		}
	}
	return out
}

// appendLessons adds bullets to the end of AGENTS.md's lessons section,
// making the section (and the file) if it isn't there.
func appendLessons(md string, lines []string) string {
	var bullets []string
	for _, l := range lines {
		bullets = append(bullets, "- "+l)
	}
	if strings.TrimSpace(md) == "" {
		return lessonsHeading + "\n\n" + strings.Join(bullets, "\n") + "\n"
	}
	ls := strings.Split(strings.TrimRight(md, "\n"), "\n")
	start, inCode := -1, false
	for i, l := range ls {
		if strings.HasPrefix(strings.TrimSpace(l), "```") {
			inCode = !inCode
		}
		if !inCode && strings.TrimSpace(l) == lessonsHeading {
			start = i
			break
		}
	}
	if start < 0 {
		return strings.Join(ls, "\n") + "\n\n" + lessonsHeading + "\n\n" + strings.Join(bullets, "\n") + "\n"
	}
	end := len(ls)
	inCode = false
	for i := start + 1; i < len(ls); i++ {
		if strings.HasPrefix(strings.TrimSpace(ls[i]), "```") {
			inCode = !inCode
		}
		if !inCode && (strings.HasPrefix(ls[i], "# ") || strings.HasPrefix(ls[i], "## ")) {
			end = i
			break
		}
	}
	at := end
	for at > start+1 && strings.TrimSpace(ls[at-1]) == "" {
		at--
	}
	out := append([]string{}, ls[:at]...)
	if at == start+1 {
		out = append(out, "") // a blank line after the heading
	}
	out = append(out, bullets...)
	if end < len(ls) {
		out = append(append(out, ""), ls[end:]...) // and one before the next
	}
	return strings.Join(out, "\n") + "\n"
}

// commitFile commits one changed file in the daemon's own repository and
// returns the new HEAD.
func (g *Git) commitFile(ctx context.Context, repo, path, msg string) (string, error) {
	if _, err := g.run(ctx, repo, nil, "add", "--", path); err != nil {
		return "", err
	}
	if _, err := g.run(ctx, repo, nil, "commit", "--quiet", "--no-verify", "-m", msg); err != nil {
		return "", err
	}
	return g.run(ctx, repo, nil, "rev-parse", "HEAD")
}
