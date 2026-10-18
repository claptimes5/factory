package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/claptimes5/factory/internal/github"
	"github.com/claptimes5/factory/internal/pathglob"
	"github.com/claptimes5/factory/internal/readiness"
	"github.com/claptimes5/factory/internal/reviewmd"
	"github.com/claptimes5/factory/internal/state"
	"github.com/claptimes5/factory/internal/store"
	"github.com/claptimes5/factory/internal/verdict"
)

// The learn loop: what a human changed in a factory PR before merging it,
// and blocking findings that repeat across items, become signals. Once a
// day per repo, when there's enough to go on, a read-only learn job turns
// them into lines for the instructions file the repo's builders read
// (AGENTS.md, which CLAUDE.md loads; see lessonsTarget), and the daemon proposes
// those in a draft PR of its own on LessonsBranch. That PR is no item's,
// so it never auto-merges and no builder ever runs on its branch.

// LessonsBranch is the branch of the lessons PR. When it has commits the
// factory didn't make and no open PR, the PR goes on LessonsBranch-2, -3...
const LessonsBranch = "factory/lessons"

// isLessonsBranch says whether a branch is one of the lessons PR's.
func isLessonsBranch(b string) bool {
	return b == LessonsBranch || strings.HasPrefix(b, LessonsBranch+"-")
}

// lessonsHeading is the section the lessons go under.
const lessonsHeading = "## Lessons from the factory"

// Limits that keep a learn job's input small.
const (
	maxEditDiff   = 20_000              // bytes of one human edit's diff
	maxEdits      = 5                   // human edits per learn job
	maxGroups     = 10                  // finding groups per learn job
	maxGroupSize  = 10                  // findings per group
	maxWhy        = 500                 // bytes of a lesson's why, in the PR
	signalHorizon = 30 * 24 * time.Hour // older unused signals are left out
)

// learnDay is the day a learn job is counted against.
func (p *Pipeline) learnDay() string {
	if p.learnDayFn != nil {
		return p.learnDayFn()
	}
	return store.Today()
}

// LearnInput is carried by a learn job: its repo and the signals it reads.
// Trimmed are the ones the caps left out, set aside with the rest so they
// don't start a job on old news the next day.
type LearnInput struct {
	Repo    string  `json:"repo"`
	Signals []int64 `json:"signals"`
	Trimmed []int64 `json:"trimmed,omitempty"`
}

// --- signals ---

// recordHumanEdit notes that a human pushed to a factory PR after the
// factory's last push (from), up to the head it merged at. It runs under
// the intake lock, so it only records the two commits: the learn job reads
// the diff, and tries again the next day if it can't.
func (p *Pipeline) recordHumanEdit(ctx context.Context, it *store.WorkItem, pr *github.PR, from string) {
	if !p.Cfg.Learning.On() || from == "" || pr.Head.SHA == "" || from == pr.Head.SHA {
		return
	}
	p.warn(p.Store.AddSignal(ctx, store.Signal{Repo: it.Repo, Kind: store.SignalHumanEdit, ItemID: it.ID, IssueNumber: it.IssueNumber,
		PRNumber: pr.Number, FromSHA: from, HeadSHA: pr.Head.SHA}), "AddSignal")
}

// recordOverruled notes, as a human merges an item's PR, each blocking
// finding they overruled: one the verifier refuted, or one the builder
// called wrong and a later review withdrew. from is the factory's last
// push, and changed says the human pushed after it: the learn job then
// checks they didn't fix the finding's file after all. Like
// recordHumanEdit, it runs under the intake lock and only records.
func (p *Pipeline) recordOverruled(ctx context.Context, it *store.WorkItem, pr *github.PR, from string, changed bool) {
	if !p.Cfg.Learning.On() || (changed && from == "") {
		return
	}
	fs, err := p.Store.ItemFindings(ctx, it.ID, 0)
	if err != nil {
		p.warn(err, "ItemFindings")
		return
	}
	for _, f := range fs {
		why := overruled(f)
		if why == "" {
			continue
		}
		sig := store.Signal{Repo: it.Repo, Kind: store.SignalOverruled, ItemID: it.ID, IssueNumber: it.IssueNumber, JobID: f.JobID, PRNumber: pr.Number,
			File: truncate(f.File, 500), Claim: truncate(f.Claim, 500), Severity: f.Severity, Evidence: truncate(why, 500)}
		if changed {
			sig.FromSHA, sig.HeadSHA = from, pr.Head.SHA
		}
		p.warn(p.Store.AddSignal(ctx, sig), "AddSignal")
	}
}

// riskyLine are words a learned REVIEW.md line may not use: one that
// tells reviewers to look away from security or correctness, or to stop
// flagging a whole class of thing, does more harm than a false alarm.
var (
	riskyLine   = regexp.MustCompile(`(?i)secur|inject|validat|sanitiz|escap|xss|csrf|ssrf|privilege|permission|authori|authent|\bauth|\bacl\b|rbac`)
	blanketLine = regexp.MustCompile(`(?i)\b(any|all|every|anything|everything|never flag|always approve|approve|skip|ignore)\b`)
)

// riskyReviewLine says why a learned line can't go in REVIEW.md, or "":
// it names security-sensitive code, by its words or by a path in it that
// the sensitive list (areas) has.
func riskyReviewLine(l string, areas []pathglob.Area) string {
	switch {
	case reviewmd.Sensitive(l) || riskyLine.MatchString(l) || slices.ContainsFunc(linePaths(l), func(f string) bool { return reviewmd.SensitiveIn(areas, f) }):
		return "it's about security-sensitive code"
	case blanketLine.MatchString(l):
		return "it's a blanket rule, not one false alarm"
	}
	return ""
}

// linePaths are the words of a line that look like paths: with a / or a
// dot inside, such as internal/upload/ or jwt.go.
func linePaths(l string) []string {
	var out []string
	for _, w := range strings.FieldsFunc(l, func(r rune) bool { return unicode.IsSpace(r) || strings.ContainsRune("`'\"()[]{}<>,;", r) }) {
		w = strings.TrimRight(w, ".:!?")
		if strings.Trim(w, "./") != "" && strings.ContainsAny(w, "/.") {
			out = append(out, w)
		}
	}
	return out
}

// overruled says how a finding that blocked was overruled, or "" if it
// wasn't: the verifier refuted it, or the builder answered wrong and a
// review withdrew it, when the verifier hadn't confirmed it.
func overruled(f store.ReviewFinding) string {
	switch {
	case !f.Blocking:
	case f.Check == verdict.Refuted:
		return "the verifier refuted it: " + oneLine(f.CheckReason)
	case f.Answer == "wrong" && f.Ruling == "withdrawn" && f.Check != verdict.Confirmed && !f.Confirmed:
		return "the builder said it was wrong (" + oneLine(f.AnswerWhy) + ") and the review withdrew it: " + oneLine(f.RulingReason)
	}
	return ""
}

// humanDiff is what the human changed on a merged PR after the factory's
// last push: the patches of their own commits, in the checkout ws. Not a
// diff of the two heads, which would hold whatever an "Update branch"
// merge or a rebase brought in from base, and not the factory's own
// commits rebased.
func (p *Pipeline) humanDiff(ctx context.Context, repo string, ws *Workspace, s store.Signal) (string, error) {
	out, err := p.humanLog(ctx, repo, ws, s, "-p")
	if err != nil {
		return "", err
	}
	return capDiff(out, maxEditDiff), nil
}

// humanFiles are the files the human's own commits changed (see
// humanDiff), whole, where humanDiff is capped.
func (p *Pipeline) humanFiles(ctx context.Context, repo string, ws *Workspace, s store.Signal) (map[string]bool, error) {
	out, err := p.humanLog(ctx, repo, ws, s, "--name-only")
	if err != nil {
		return nil, err
	}
	files := map[string]bool{}
	for _, f := range strings.Split(out, "\n") {
		if f = strings.TrimSpace(f); f != "" {
			files[f] = true
		}
	}
	return files, nil
}

// humanLog runs git log over the human's own commits on a merged PR,
// with what (-p, --name-only) it should show of each.
func (p *Pipeline) humanLog(ctx context.Context, repo string, ws *Workspace, s store.Signal, what string) (string, error) {
	g := p.git(repo)
	// The branch may be gone after the merge; the PR's own ref isn't.
	ref := fmt.Sprintf("refs/pull/%d/head", s.PRNumber)
	if p.repoConf(repo).GitLab() {
		ref = fmt.Sprintf("refs/merge-requests/%d/head", s.PRNumber)
	}
	for _, sha := range []string{s.HeadSHA, s.FromSHA} {
		if g.hasCommit(ctx, ws.Repo, sha) {
			continue
		}
		got, err := g.FetchRef(ctx, ws.Repo, ref)
		if err != nil || got != sha {
			if _, err := g.FetchRef(ctx, ws.Repo, sha); err != nil {
				return "", err
			}
		}
		if !g.hasCommit(ctx, ws.Repo, sha) {
			return "", fmt.Errorf("commit %s isn't on the remote", short(sha))
		}
	}
	before, err := g.baseBefore(ctx, ws.Repo, "refs/remotes/origin/"+p.repoConf(repo).BaseBranch, s.HeadSHA)
	if err != nil {
		return "", err
	}
	// Commits on head and not on from, less what base had before the
	// merge, less those with a twin on from (the factory's, rebased).
	args := []string{"log", what, "--reverse", "--no-merges", "--right-only", "--cherry-pick", "--format=",
		"--no-ext-diff", "--no-textconv", s.FromSHA + "..." + s.HeadSHA}
	if before != "" {
		args = append(args, "^"+before)
	}
	var b strings.Builder
	if _, err := g.run(ctx, ws.Repo, &b, append(args, "--")...); err != nil {
		return "", err
	}
	return b.String(), nil
}

// baseBefore is base as it was before head merged into it: the first
// parent of the merge commit on base's mainline, or base itself when no
// commit there has head (a squash or a rebase merge). It is "" when head
// was fast-forwarded onto base, which leaves nothing apart to tell by.
func (g *Git) baseBefore(ctx context.Context, repo, base, head string) (string, error) {
	if !g.IsAncestor(ctx, repo, head, base) {
		return base, nil
	}
	out, err := g.run(ctx, repo, nil, "rev-list", "--first-parent", "--ancestry-path", "--reverse", head+".."+base)
	if err != nil {
		return "", err
	}
	first, _, _ := strings.Cut(out, "\n")
	if first == "" {
		return "", nil
	}
	parent, err := g.run(ctx, repo, nil, "rev-parse", first+"^1")
	if err != nil || parent == head {
		return "", err
	}
	return parent, nil
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

// recordFindings saves the critical and major findings a verdict blocked
// on: not the ones the verifier refuted, nor pre-existing ones.
func (p *Pipeline) recordFindings(ctx context.Context, j *store.Job, it *store.WorkItem, v *verdict.Review) {
	if !p.Cfg.Learning.On() || !v.Blocks() {
		return
	}
	for i, f := range v.Findings {
		if f.Severity != verdict.Critical && f.Severity != verdict.Major || !v.BlocksOn(i) {
			continue
		}
		p.warn(p.Store.AddSignal(ctx, store.Signal{Repo: it.Repo, Kind: store.SignalFinding, ItemID: it.ID, IssueNumber: it.IssueNumber,
			JobID: j.ID, PRNumber: it.PRNumber, File: f.File, Claim: truncate(f.Claim, 500), Severity: f.Severity, Evidence: truncate(f.Evidence, 500)}), "AddSignal")
	}
}

// --- when to learn ---

// learnTriggers queues a learn job for each repo with something to learn
// from, at most once a day per repo, and never while one for the repo is
// queued or running: two would push over each other's lines.
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
		key := p.learnDay() + " " + repo
		if seen, err := p.Store.HasEvent(ctx, "learn_queued", key); err != nil || seen {
			continue
		}
		sigs, err := p.Store.UnusedSignals(ctx, repo, since)
		if err != nil {
			p.warn(err, "UnusedSignals")
			continue
		}
		use, trimmed := learnable(sigs, p.Cfg.Learning.MinFindings)
		if len(use) == 0 {
			continue
		}
		in := LearnInput{Repo: repo}
		for _, s := range use {
			in.Signals = append(in.Signals, s.ID)
		}
		for _, s := range trimmed {
			in.Trimmed = append(in.Trimmed, s.ID)
		}
		b, _ := json.Marshal(in)
		fast := r.FastFor(r.Harness, r.Model)
		_, queued, err := p.Store.EnqueueSoleFor(ctx, store.Job{Kind: "learn", Harness: r.Harness, Model: r.Model, Effort: r.Effort, Fast: fast,
			Skillset: r.Skills, Route: "learn", Priority: -1, BudgetUSD: p.Cfg.FastBudget(p.Cfg.LearnBudget(), fast), Input: string(b)}, repo)
		if err != nil || !queued {
			p.warn(err, "enqueue learn")
			continue
		}
		p.warn(p.Store.AddEvent(ctx, store.Event{Actor: "daemon", Type: "learn_queued", Reason: key, Data: fmt.Sprintf(`{"signals":%d}`, len(in.Signals))}), "AddEvent")
	}
}

// learnable picks the signals worth a learn job: every human edit, and
// each group of alike findings with at least min of them from two or more
// items. With neither there's nothing to learn yet. Findings left out wait
// for one like them. trimmed are the older edits and findings of a picked
// group past the caps: the job sets them aside with the rest.
func learnable(sigs []store.Signal, min int) (use, trimmed []store.Signal) {
	var edits, findings, over []store.Signal
	for _, s := range sigs {
		switch s.Kind {
		case store.SignalHumanEdit:
			edits = append(edits, s)
		case store.SignalFinding:
			findings = append(findings, s)
		case store.SignalOverruled:
			over = append(over, s)
		}
	}
	if len(edits) > maxEdits {
		trimmed = append(trimmed, edits[:len(edits)-maxEdits]...)
		edits = edits[len(edits)-maxEdits:] // the newest
	}
	use = edits
	// Findings and overruled findings are grouped apart: one teaches the
	// builders, the other the reviewers.
	for _, kind := range [][]store.Signal{findings, over} {
		n := 0
		for _, g := range repeats(kind, min) {
			if n == maxGroups {
				break
			}
			n++
			if len(g) > maxGroupSize {
				trimmed = append(trimmed, g[:len(g)-maxGroupSize]...)
				g = g[len(g)-maxGroupSize:]
			}
			use = append(use, g...)
		}
	}
	if len(use) == 0 {
		return nil, nil
	}
	return use, trimmed
}

// repeats are the groups of alike findings (see groupFindings) with at
// least min of them, from two or more items: a repeat, not a one-off.
func repeats(fs []store.Signal, min int) [][]store.Signal {
	var out [][]store.Signal
	for _, g := range groupFindings(fs) {
		items := map[int64]bool{}
		for _, f := range g {
			items[f.ItemID] = true
		}
		if len(g) >= min && len(items) >= 2 {
			out = append(out, g)
		}
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
	File     string      // where the lines go, such as AGENTS.md
	Current  string      // File on the base branch, or ""
	Others   []learnFile // the other instruction files builders read
	Proposed []string    // lines in the open lessons PR not on the base yet
	Declined []string    // lines a human turned down: never again
	Edits    []learnEdit
	Groups   []learnGroup
	// Overruled are groups of findings a human overruled, which teach
	// lines for ReviewFile, the REVIEW.md the repo's reviewers read.
	Overruled      []learnGroup
	ReviewFile     string
	ReviewCurrent  string   // ReviewFile on the base branch, or ""
	ReviewProposed []string // REVIEW.md lines in the open lessons PR
	ReviewDeclined []string // REVIEW.md lines a human turned down
	Fence          string   // a code fence none of the above can close
}

type learnFile struct{ Name, Text string }

// refusal is why the factory won't write a repo's lessons, found before
// the agent runs, that a later run would meet again: such as a symlink out
// of the repo. The job's signals are set aside rather than retried daily.
type refusal struct{ msg string }

func (r refusal) Error() string { return r.msg }

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
	// Everything that could refuse the lines is checked before the agent
	// is paid for.
	plan, err := p.lessonsPlan(ctx, in.Repo, ws)
	if err == nil {
		err = checkWritable(ws.Repo, plan)
	}
	if r := (refusal{}); errors.As(err, &r) {
		p.refuseLearn(ctx, j, in, r)
		return nil
	} else if err != nil {
		return fail(agentRun{}, state.Failed, err.Error())
	}
	// The diffs of human edits are read here, not at merge time. One that
	// can't be read now stays unused for the next run. So are the files a
	// human changed before merging past a finding they overruled: one
	// whose file they changed after all may have been right, and is set
	// aside.
	var use []int64
	var kept []store.Signal
	touched := map[int]map[string]bool{} // by PR
	for _, s := range sigs {
		switch {
		case s.Kind == store.SignalHumanEdit && s.Diff == "" && s.FromSHA != "":
			diff, err := p.humanDiff(ctx, in.Repo, ws, s)
			if err != nil {
				p.log().Warn("lessons: human edit", "pr", s.PRNumber, "err", err)
				continue
			}
			use = append(use, s.ID)
			if strings.TrimSpace(diff) == "" {
				continue // only base's changes, or the factory's own
			}
			p.warn(p.Store.SetSignalDiff(ctx, s.ID, diff), "SetSignalDiff")
			s.Diff = diff
		case s.Kind == store.SignalOverruled && s.FromSHA != "":
			files, ok := touched[s.PRNumber]
			if !ok {
				var err error
				if files, err = p.humanFiles(ctx, in.Repo, ws, s); err != nil {
					p.log().Warn("lessons: overruled finding", "pr", s.PRNumber, "err", err)
					continue
				}
				touched[s.PRNumber] = files
			}
			use = append(use, s.ID)
			if files[s.File] {
				continue // the human changed its file after all
			}
		default:
			use = append(use, s.ID)
		}
		kept = append(kept, s)
	}
	kept, use = p.overruledRepeats(ctx, j, in.Repo, plan, kept, use)
	if len(kept) == 0 {
		p.finish(ctx, j, agentRun{}, state.Succeeded, `{"lessons":[],"skip_reason":"nothing left to learn from"}`, "")
		p.warn(p.Store.UseSignals(ctx, append(use, in.Trimmed...), j.ID), "UseSignals")
		return nil
	}
	base, _, err := g.FileAt(ctx, ws.Repo, ws.BaseSHA, plan.File)
	if err != nil {
		return fail(agentRun{}, state.Failed, err.Error())
	}
	data := learnPrompt{Repo: in.Repo, File: plan.File, Current: truncate(base, 30_000)}
	all := base
	for _, name := range []string{"AGENTS.md", "CLAUDE.md"} {
		if f, err := instructionsFile(ctx, g, ws.Repo, ws.BaseSHA, name); err == nil && f != "" && f != plan.File {
			text, _, _ := g.FileAt(ctx, ws.Repo, ws.BaseSHA, f)
			data.Others = append(data.Others, learnFile{Name: f, Text: truncate(text, 30_000)})
			all += "\n" + text
		}
	}
	if plan.Open != nil {
		if branch, ok := instructionsText(ctx, g, ws.Repo, "refs/remotes/origin/"+plan.Branch); ok {
			data.Proposed = newLines(all, branch)
		}
	}
	data.Declined, data.ReviewDeclined = p.settleLessons(ctx, in.Repo, ws, plan, all)
	var findings, over []store.Signal
	for _, s := range kept {
		switch s.Kind {
		case store.SignalHumanEdit:
			data.Edits = append(data.Edits, learnEdit{Ref: p.ref(in.Repo, s.IssueNumber), PR: s.PRNumber, Diff: s.Diff})
		case store.SignalFinding:
			findings = append(findings, s)
		case store.SignalOverruled:
			over = append(over, s)
		}
	}
	groups := func(fs []store.Signal) []learnGroup {
		var out []learnGroup
		for _, grp := range groupFindings(fs) {
			lg := learnGroup{N: len(out) + 1}
			for _, f := range grp {
				lg.Findings = append(lg.Findings, learnFinding{Ref: p.ref(in.Repo, f.IssueNumber), Severity: f.Severity, File: oneLine(f.File), Claim: oneLine(f.Claim), Evidence: oneLine(f.Evidence)})
			}
			out = append(out, lg)
		}
		return out
	}
	data.Groups, data.Overruled = groups(findings), groups(over)
	if len(over) > 0 {
		cur, _, err := g.FileAt(ctx, ws.Repo, ws.BaseSHA, plan.Review)
		if err != nil {
			return fail(agentRun{}, state.Failed, err.Error())
		}
		data.ReviewFile, data.ReviewCurrent = plan.Review, truncate(cur, 30_000)
		if plan.Open != nil {
			data.ReviewProposed = newLines(reviewText(ctx, g, ws.Repo, ws.BaseSHA), reviewText(ctx, g, ws.Repo, "refs/remotes/origin/"+plan.Branch))
		}
	}
	var text strings.Builder
	text.WriteString(data.Current + data.ReviewCurrent)
	for _, o := range data.Others {
		text.WriteString(o.Text)
	}
	for _, ls := range [][]string{data.Proposed, data.Declined, data.ReviewProposed, data.ReviewDeclined} {
		for _, l := range ls {
			text.WriteString(l)
		}
	}
	for _, e := range data.Edits {
		text.WriteString(e.Diff)
	}
	for _, grp := range append(append([]learnGroup{}, data.Groups...), data.Overruled...) {
		for _, f := range grp.Findings {
			text.WriteString(f.File + f.Claim + f.Evidence)
		}
	}
	data.Fence = fence(text.String())
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
	// Each kind of line needs its own evidence: lessons for builders come
	// from human edits and repeated findings, lines for reviewers only
	// from findings a human overruled.
	if len(data.Edits) == 0 && len(data.Groups) == 0 {
		v.Lessons = nil
	}
	if len(data.Overruled) == 0 {
		v.ReviewLines = nil
	}
	v.ReviewLines = slices.DeleteFunc(v.ReviewLines, func(l verdict.Lesson) bool {
		if why := riskyReviewLine(l.Line, p.Cfg.SensitiveAreas()); why != "" {
			p.log().Info("lessons: REVIEW.md line refused", "line", oneLine(l.Line), "why", why)
			return true
		}
		return false
	})
	if err := p.proposeLessons(ctx, j, in.Repo, plan, v.Lessons, v.ReviewLines, kept); err != nil {
		return fail(run, state.Failed, "lessons PR: "+err.Error())
	}
	norm, _ := json.Marshal(v)
	p.finish(ctx, j, run, state.Succeeded, string(norm), "")
	p.warn(p.Store.UseSignals(ctx, append(use, in.Trimmed...), j.ID), "UseSignals")
	return nil
}

// refuseLearn ends a learn job the factory can't act on: its signals are
// set aside, and you hear why once, not every day it happens again.
func (p *Pipeline) refuseLearn(ctx context.Context, j *store.Job, in LearnInput, r refusal) {
	p.finish(ctx, j, agentRun{}, state.Failed, "", r.msg)
	p.warn(p.Store.UseSignals(ctx, append(append([]int64{}, in.Signals...), in.Trimmed...), j.ID), "UseSignals")
	p.notifyRefusal(ctx, j, in.Repo, r.msg, fmt.Sprintf("Lessons for %s can't be proposed: %s. What the factory had to learn from is set aside; once that's fixed, new edits and findings start again.", in.Repo, r.msg))
}

// notifyRefusal tells you why the factory won't write some of a repo's
// lines, once for each reason, not every day it happens again.
func (p *Pipeline) notifyRefusal(ctx context.Context, j *store.Job, repo, why, msg string) {
	key := strings.ToLower(repo) + ": " + why
	if seen, err := p.Store.HasEvent(ctx, "learn_refused", key); err != nil || seen {
		return
	}
	p.warn(p.Store.AddEvent(ctx, store.Event{Actor: "daemon", JobID: &j.ID, Type: "learn_refused", Reason: key}), "AddEvent")
	p.notify(ctx, msg)
}

// overruledRepeats keeps the overruled findings a learn job reads to
// those still a repeat (see repeats), once the ones a human addressed
// are gone; the others leave kept and use, to wait for one like them.
// When the factory won't write REVIEW.md (see lessonsPlan), they are all
// set aside: used, and not read.
func (p *Pipeline) overruledRepeats(ctx context.Context, j *store.Job, repo string, plan *lessonsPlan, kept []store.Signal, use []int64) ([]store.Signal, []int64) {
	var over []store.Signal
	for _, s := range kept {
		if s.Kind == store.SignalOverruled {
			over = append(over, s)
		}
	}
	if len(over) == 0 {
		return kept, use
	}
	if plan.Review == "" {
		p.notifyRefusal(ctx, j, repo, plan.ReviewRefused, fmt.Sprintf("Lines for %s's REVIEW.md can't be proposed: %s. The findings you overruled are set aside; lessons for AGENTS.md go on.", repo, plan.ReviewRefused))
		return slices.DeleteFunc(kept, func(s store.Signal) bool { return s.Kind == store.SignalOverruled }), use
	}
	repeat := map[int64]bool{}
	for _, g := range repeats(over, p.Cfg.Learning.MinFindings) {
		for _, s := range g {
			repeat[s.ID] = true
		}
	}
	wait := map[int64]bool{}
	for _, s := range over {
		if !repeat[s.ID] {
			wait[s.ID] = true
		}
	}
	kept = slices.DeleteFunc(kept, func(s store.Signal) bool { return wait[s.ID] })
	use = slices.DeleteFunc(use, func(id int64) bool { return wait[id] })
	return kept, use
}

// lessonsPlan is where a repo's lessons go: the file and the PR's branch.
type lessonsPlan struct {
	File   string     // the instructions file, such as AGENTS.md
	Import string     // the CLAUDE.md to add "@AGENTS.md" to, so Claude reads it too; or ""
	Branch string     // LessonsBranch, or one with a suffix
	Open   *github.PR // the open lessons PR on Branch, or nil
	// Review is the REVIEW.md that lines for reviewers go in, or "" when
	// the factory won't write it, for the reason in ReviewRefused. Unlike
	// File, that refuses only those lines.
	Review        string
	ReviewRefused string
}

// lessonsPlan finds the branch and file for a repo's lessons in the
// checkout ws: the file on the open PR's branch, or on base.
func (p *Pipeline) lessonsPlan(ctx context.Context, repo string, ws *Workspace) (*lessonsPlan, error) {
	g := p.git(repo)
	branch, open, err := lessonsBranch(ctx, g, p.gh(repo), ws)
	if err != nil {
		return nil, err
	}
	rev := ws.BaseSHA
	if open != nil {
		rev = "refs/remotes/origin/" + branch
	}
	file, imp, err := lessonsTarget(ctx, g, ws.Repo, rev, p.ClaudeBuilds())
	if err != nil {
		return nil, err
	}
	if imp != "" && rev != ws.BaseSHA {
		// Someone may have added the import on base since the PR opened.
		if _, onBase, err := lessonsTarget(ctx, g, ws.Repo, ws.BaseSHA, true); err == nil && onBase == "" {
			imp = ""
		}
	}
	plan := &lessonsPlan{File: file, Import: imp, Branch: branch, Open: open}
	plan.Review, err = reviewTarget(ctx, g, ws.Repo, rev)
	if r := (refusal{}); errors.As(err, &r) {
		plan.ReviewRefused = r.msg
	} else if err != nil {
		return nil, err
	}
	return plan, nil
}

// reviewTarget is the file at rev that lines for reviewers go in: the
// first REVIEW.md there is (see reviewmd.Files), or the tracked file it
// links to; REVIEW.md, new, when there's none.
func reviewTarget(ctx context.Context, g *Git, repo, rev string) (string, error) {
	for _, name := range reviewmd.Files {
		if f, err := instructionsFile(ctx, g, repo, rev, name); err != nil || f != "" {
			return f, err
		}
	}
	return reviewmd.Files[0], nil
}

// reviewText is every REVIEW.md at rev, as one text.
func reviewText(ctx context.Context, g *Git, repo, rev string) string {
	var b strings.Builder
	seen := map[string]bool{}
	for _, name := range reviewmd.Files {
		f, err := instructionsFile(ctx, g, repo, rev, name)
		if err != nil || f == "" || seen[f] {
			continue
		}
		seen[f] = true
		if text, ok, _ := g.FileAt(ctx, repo, rev, f); ok {
			b.WriteString(text + "\n")
		}
	}
	return b.String()
}

// lessonsBranch is the branch for the lessons PR: the one with the open
// PR, or else the first that is missing or has only the factory's commits
// past base, which a force push would lose nothing of.
func lessonsBranch(ctx context.Context, g *Git, gh GitHub, ws *Workspace) (string, *github.PR, error) {
	for n := 1; n <= 20; n++ {
		branch := LessonsBranch
		if n > 1 {
			branch = fmt.Sprintf("%s-%d", LessonsBranch, n)
		}
		pr, err := gh.FindPR(ctx, branch)
		if err != nil {
			return "", nil, err
		}
		if pr != nil && pr.State == "open" && !pr.Merged {
			return branch, pr, nil
		}
		if !g.HasBranch(ctx, ws.Repo, branch) {
			return branch, nil, nil
		}
		if mine, err := g.onlyMine(ctx, ws.Repo, ws.BaseSHA, "refs/remotes/origin/"+branch); err != nil || mine {
			return branch, nil, err
		}
	}
	return "", nil, refusal{fmt.Sprintf("every %s branch has commits of someone else's", LessonsBranch)}
}

// ClaudeBuilds says whether any build runs on Claude Code, which reads
// CLAUDE.md and not AGENTS.md.
func (p *Pipeline) ClaudeBuilds() bool {
	for k, r := range p.Cfg.Routing {
		if strings.HasPrefix(k, "implement") && r.Harness == "claude" {
			return true
		}
	}
	return false
}

// instructionsText is AGENTS.md and CLAUDE.md at rev, as one text, and
// whether either is there.
func instructionsText(ctx context.Context, g *Git, repo, rev string) (string, bool) {
	var b strings.Builder
	found := false
	seen := map[string]bool{}
	for _, name := range []string{"AGENTS.md", "CLAUDE.md"} {
		f, err := instructionsFile(ctx, g, repo, rev, name)
		if err != nil || f == "" || seen[f] {
			continue
		}
		seen[f] = true
		if text, ok, _ := g.FileAt(ctx, repo, rev, f); ok {
			found = true
			b.WriteString(text + "\n")
		}
	}
	return b.String(), found
}

// lessonsTarget is the file at rev that lessons go in: AGENTS.md, or the
// tracked file it links to, new when there's none. Claude Code reads
// CLAUDE.md, not AGENTS.md, so when Claude builds, imp names the CLAUDE.md
// (or the file it links to) that needs an "@AGENTS.md" line to load it;
// "" when it already does, or is the same file.
func lessonsTarget(ctx context.Context, g *Git, repo, rev string, claude bool) (file, imp string, err error) {
	file, err = instructionsFile(ctx, g, repo, rev, "AGENTS.md")
	if err != nil {
		return "", "", err
	}
	if file == "" {
		file = "AGENTS.md"
	}
	if !claude {
		return file, "", nil
	}
	cl, err := instructionsFile(ctx, g, repo, rev, "CLAUDE.md")
	switch {
	case err != nil:
		return "", "", err
	case cl == "":
		return file, "CLAUDE.md", nil
	case cl == file:
		return file, "", nil
	}
	md, _, err := g.FileAt(ctx, repo, rev, cl)
	if err != nil {
		return "", "", err
	}
	if readiness.ImportsAgents(md) {
		return file, "", nil
	}
	return file, cl, nil
}

// withImport is a CLAUDE.md that loads AGENTS.md: md with the line added.
func withImport(md string) string {
	if md == "" {
		return "@AGENTS.md\n"
	}
	return strings.TrimRight(md, "\n") + "\n\n@AGENTS.md\n"
}

// instructionsFile is the tracked plain file that name is at rev: name
// itself, or the file a symlink there points to inside the repo. It is ""
// when name isn't there, and a refusal when it is anything else.
func instructionsFile(ctx context.Context, g *Git, repo, rev, name string) (string, error) {
	mode, err := g.Mode(ctx, repo, rev, name)
	if err != nil {
		return "", err
	}
	plain := func(m string) bool { return m == "100644" || m == "100755" }
	switch {
	case mode == "":
		return "", nil
	case plain(mode):
		return name, nil
	case mode != "120000":
		return "", refusal{fmt.Sprintf("%s isn't a regular file (mode %s), so the factory won't write to it", name, mode)}
	}
	link, _, err := g.FileAt(ctx, repo, rev, name)
	if err != nil {
		return "", err
	}
	link = strings.TrimRight(link, "\n")
	target := path.Clean(path.Join(path.Dir(name), link))
	if path.IsAbs(link) || target == "." || target == ".." || strings.HasPrefix(target, "../") {
		return "", refusal{fmt.Sprintf("%s is a symlink out of the repo (to %s), so the factory won't write through it", name, oneLine(link))}
	}
	if m, err := g.Mode(ctx, repo, rev, target); err != nil {
		return "", err
	} else if !plain(m) {
		return "", refusal{fmt.Sprintf("%s is a symlink to %s, which isn't a plain file tracked in the repo, so the factory won't write through it", name, oneLine(link))}
	}
	return target, nil
}

// checkWritable checks the checkout at repo for what git can't tell: on
// a case-insensitive disk, an agents.md symlink is what AGENTS.md opens.
// A REVIEW.md it can't write only takes Review out of the plan.
func checkWritable(repo string, plan *lessonsPlan) error {
	root, err := os.OpenRoot(repo)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	if plan.Review != "" {
		if _, err := root.Lstat(plan.Review); !errors.Is(err, fs.ErrNotExist) {
			if err := plainFile(root, plan.Review); err != nil {
				plan.Review, plan.ReviewRefused = "", err.Error()
			}
		}
	}
	files := []string{plan.File}
	if plan.Import != "" {
		files = append(files, plan.Import)
	}
	for _, f := range files {
		if _, err := root.Lstat(f); errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err := plainFile(root, f); err != nil {
			return refusal{err.Error()}
		}
	}
	return nil
}

// writePlain writes rel under root, over a plain file or as a new one,
// never through a symlink.
func writePlain(root *os.Root, rel string, b []byte) error {
	flag := os.O_WRONLY | os.O_TRUNC
	if _, err := root.Lstat(rel); errors.Is(err, fs.ErrNotExist) {
		flag = os.O_WRONLY | os.O_CREATE | os.O_EXCL
	} else if err := plainFile(root, rel); err != nil {
		return err
	}
	f, err := root.OpenFile(rel, flag, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// settleLessons records what became of the lines the factory proposed:
// merged, or declined when their PR closed unmerged, when a human took
// them out of the PR before merging it, or out of the file after. It
// returns every declined line, for AGENTS.md and for REVIEW.md. base is
// the base branch's instruction files, in the checkout ws. A REVIEW.md
// line is looked for in the REVIEW.md files.
func (p *Pipeline) settleLessons(ctx context.Context, repo string, ws *Workspace, plan *lessonsPlan, base string) (declined, reviewDeclined []string) {
	ls, err := p.Store.Lessons(ctx, repo)
	if err != nil {
		p.warn(err, "Lessons")
		return nil, nil
	}
	g := p.git(repo)
	reviewBase := reviewText(ctx, g, ws.Repo, ws.BaseSHA)
	has := func(md, norm string) bool {
		for _, l := range strings.Split(md, "\n") {
			if normLine(l) == norm {
				return true
			}
		}
		return false
	}
	open := plan.Open
	branch, onBranch := "", false
	if open != nil {
		// Both files: a PR opened before lessons moved to AGENTS.md
		// has them in CLAUDE.md.
		branch, onBranch = instructionsText(ctx, g, ws.Repo, "refs/remotes/origin/"+plan.Branch)
	}
	reviewBranch := ""
	if open != nil {
		reviewBranch = reviewText(ctx, g, ws.Repo, "refs/remotes/origin/"+plan.Branch)
	}
	prs := map[int]*github.PR{}
	for _, l := range ls {
		base, branch, onBranch, norm := base, branch, onBranch, l.Norm
		rn, forReview := l.ForReview()
		if forReview {
			// As for AGENTS.md, a branch with no REVIEW.md settles nothing.
			base, branch, onBranch, norm = reviewBase, reviewBranch, reviewBranch != "", rn
		}
		status := ""
		switch {
		case l.Status == store.LessonMerged && !has(base, norm):
			status = store.LessonDeclined // taken out after it merged
		case l.Status != store.LessonProposed:
		case open != nil && l.PRNumber == open.Number:
			if onBranch && !has(branch, norm) && !has(base, norm) {
				status = store.LessonDeclined // taken out of the open PR
			}
		default:
			pr, ok := prs[l.PRNumber]
			if !ok {
				if pr, err = p.gh(repo).GetPR(ctx, l.PRNumber); err != nil {
					pr = nil
				}
				prs[l.PRNumber] = pr
			}
			switch {
			case pr == nil:
			case pr.Merged && has(base, norm):
				status = store.LessonMerged
			case pr.Merged || pr.State == "closed":
				status = store.LessonDeclined // closed, or merged without it
			}
		}
		if status != "" {
			p.warn(p.Store.DecideLesson(ctx, l.ID, status), "DecideLesson")
			l.Status = status
		}
		switch {
		case l.Status != store.LessonDeclined:
		case forReview:
			reviewDeclined = append(reviewDeclined, l.Line)
		default:
			declined = append(declined, l.Line)
		}
	}
	return declined, reviewDeclined
}

// oneLine joins a text's lines, for a list item.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// --- the lessons PR ---

// proposeLessons adds the lessons not already in the file, on the base
// branch or in the open lessons PR, to that PR, opening it if there is
// none, and the lines for reviewers to the repo's REVIEW.md the same way.
// It never merges: the PR waits for a human, whatever auto_merge says.
// want is the plan the job checked before its agent ran.
func (p *Pipeline) proposeLessons(ctx context.Context, j *store.Job, repo string, want *lessonsPlan, lessons, reviewLines []verdict.Lesson, sigs []store.Signal) error {
	if want.Review == "" {
		reviewLines = nil
	}
	if len(lessons) == 0 && len(reviewLines) == 0 {
		return nil
	}
	rc := p.repoConf(repo)
	g, gh := p.git(repo), p.gh(repo)
	// With no open PR, a branch left by a closed one starts over from base.
	ws, err := g.PrepareAt(ctx, filepath.Join(p.jobDir(j.ID), "lessons", "repo"), "", rc.BaseBranch, want.Branch, want.Open == nil)
	if err != nil {
		return err
	}
	plan, err := p.lessonsPlan(ctx, repo, ws)
	if err != nil {
		return err
	}
	if plan.Branch != want.Branch || (plan.Open == nil) != (want.Open == nil) || (len(reviewLines) > 0 && plan.Review != want.Review) {
		return fmt.Errorf("the lessons PR changed while the job ran")
	}
	known, err := p.Store.Lessons(ctx, repo)
	if err != nil {
		return err
	}
	// fresh are the lines not in the files yet, nor settled before.
	fresh := func(ls []verdict.Lesson, text string, norm func(string) string) []verdict.Lesson {
		seen := map[string]bool{}
		for _, l := range strings.Split(text, "\n") {
			seen[norm(normLine(l))] = true
		}
		for _, l := range known {
			if l.Status != store.LessonProposed {
				seen[l.Norm] = true // declined, or merged once: settled
			}
		}
		var add []verdict.Lesson
		for _, l := range ls {
			l.Line = cleanLine(l.Line)
			if k := normLine(l.Line); k != "" && !seen[norm(k)] {
				seen[norm(k)] = true
				add = append(add, l)
			}
		}
		return add
	}
	var add, addR []verdict.Lesson
	var cur string
	if len(lessons) > 0 {
		base, _, err := g.FileAt(ctx, ws.Repo, ws.BaseSHA, plan.File)
		if err != nil {
			return err
		}
		if cur, _, err = g.FileAt(ctx, ws.Repo, "HEAD", plan.File); err != nil {
			return err
		}
		others, _ := instructionsText(ctx, g, ws.Repo, "HEAD")
		add = fresh(lessons, base+"\n"+cur+"\n"+others, func(n string) string { return n })
	}
	if len(reviewLines) > 0 {
		addR = fresh(reviewLines, reviewText(ctx, g, ws.Repo, ws.BaseSHA)+"\n"+reviewText(ctx, g, ws.Repo, "HEAD"), store.ReviewNorm)
	}
	if len(add) == 0 && len(addR) == 0 {
		return nil
	}
	lines, linesR := lessonLines(add), lessonLines(addR)
	root, err := os.OpenRoot(ws.Repo)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	var paths, whats []string
	imp := ""
	if len(add) > 0 {
		if err := writePlain(root, plan.File, []byte(appendLessons(cur, lines))); err != nil {
			return err
		}
		paths = append(paths, plan.File)
		whats = append(whats, fmt.Sprintf("%d %s for %s", len(add), plural(len(add), "lesson", "lessons"), plan.File))
		if imp = plan.Import; imp != "" {
			md, _, err := g.FileAt(ctx, ws.Repo, "HEAD", imp)
			if err != nil {
				return err
			}
			if err := writePlain(root, imp, []byte(withImport(md))); err != nil {
				return err
			}
			paths = append(paths, imp)
		}
	}
	if len(addR) > 0 {
		md, _, err := g.FileAt(ctx, ws.Repo, "HEAD", plan.Review)
		if err != nil {
			return err
		}
		if err := writePlain(root, plan.Review, []byte(appendSection(md, reviewHeading, linesR))); err != nil {
			return err
		}
		paths = append(paths, plan.Review)
		whats = append(whats, fmt.Sprintf("%d %s for %s", len(addR), plural(len(addR), "line", "lines"), plan.Review))
	}
	what := strings.Join(whats, " and ")
	msg := fmt.Sprintf("factory: %s\n\nLearned by job %d.", what, j.ID)
	if imp != "" {
		msg += "\n\n" + importNote(imp)
	}
	sha, err := g.commitFiles(ctx, ws.Repo, msg, paths...)
	if err != nil {
		return err
	}
	if err := g.Push(ctx, ws.Repo, sha, plan.Branch, plan.Open == nil); err != nil {
		return fmt.Errorf("push: %w", err)
	}
	// Recorded before the PR, so a line pushed is a line tracked; a new
	// PR's number is filled in once it's open.
	record := func(n int) error {
		for _, l := range lines {
			if err := p.Store.AddLesson(ctx, store.Lesson{Repo: repo, Line: l, Norm: normLine(l), PRNumber: n}); err != nil {
				return fmt.Errorf("record lessons: %w", err)
			}
		}
		for _, l := range linesR {
			if err := p.Store.AddLesson(ctx, store.Lesson{Repo: repo, Line: l, Norm: store.ReviewNorm(normLine(l)), PRNumber: n}); err != nil {
				return fmt.Errorf("record lessons: %w", err)
			}
		}
		return nil
	}
	pr := plan.Open
	n := 0
	if pr != nil {
		n = pr.Number
	}
	if err := record(n); err != nil {
		return err
	}
	entries := p.lessonEntries(repo, add, sigs)
	if len(addR) > 0 {
		entries += "For `" + plan.Review + "`, which the factory's reviewers read, from findings you overruled:\n\n" + p.lessonEntries(repo, addR, sigs)
	}
	if pr == nil {
		files := plan.File
		switch {
		case len(add) == 0:
			files = plan.Review
		case len(addR) > 0:
			files += " and " + plan.Review
		}
		if pr, err = gh.CreatePR(ctx, "Lessons from the factory for "+files, lessonsIntro(plan.File, imp, len(addR) > 0)+entries+"\n"+lessonsFooter, plan.Branch, rc.BaseBranch, true); err != nil {
			return err
		}
		if err := record(pr.Number); err != nil {
			return err
		}
	} else {
		body := pr.Body
		if i := strings.Index(body, lessonsFooter); i >= 0 {
			body = body[:i] + entries + "\n" + body[i:]
		} else {
			body = strings.TrimRight(body, "\n") + "\n\n" + entries
		}
		if err := gh.UpdatePRBody(ctx, pr.Number, body); err != nil {
			return err
		}
	}
	p.warn(gh.LabelPR(ctx, pr.Number, LabelAwaitingYou), "LabelPR")
	data := map[string]any{"file": plan.File, "lines": lines}
	if len(linesR) > 0 {
		data["review_file"], data["review_lines"] = plan.Review, linesR
	}
	b, _ := json.Marshal(data)
	p.warn(p.Store.AddEvent(ctx, store.Event{Actor: "daemon", JobID: &j.ID, Type: "lessons_pr", Reason: fmt.Sprintf("%s PR #%d", repo, pr.Number), Data: string(b)}), "AddEvent")
	if plan.Open == nil {
		p.notify(ctx, fmt.Sprintf("Opened a lessons PR with %s: %s %s", what, p.ref(repo, pr.Number), pr.URL))
	} else {
		p.notify(ctx, fmt.Sprintf("Added %s to the lessons PR: %s %s", what, p.ref(repo, pr.Number), pr.URL))
	}
	return nil
}

// lessonLines are the lessons' lines.
func lessonLines(ls []verdict.Lesson) []string {
	var out []string
	for _, l := range ls {
		out = append(out, l.Line)
	}
	return out
}

// lessonsIntro opens the lessons PR's description. review says it also
// has lines for REVIEW.md.
func lessonsIntro(file, imp string, review bool) string {
	note := ""
	if imp != "" {
		note = importNote(imp) + "\n\n"
	}
	if review {
		note += "Lines for REVIEW.md come from reviewer findings that the verifier refuted, or that the builder disputed and the reviewer withdrew, on PRs that were then merged anyway, more than once. That isn't proof the findings were wrong: check each line before you merge. The factory's reviewers read REVIEW.md, so each tells them what not to flag in this repo.\n\n"
	}
	return "The factory learned these from the changes you made to its PRs before merging them, and from findings its reviewers repeated. " +
		"Each is a line for `" + file + "`, which the factory's builders read before they start.\n\n" +
		"Drop any you don't want from this branch before you merge. If you reword one, the factory counts the original as declined and doesn't track your version. " +
		"New lessons are added here until you merge or close it.\n\n" + note + "## Lessons\n\n"
}

// importNote says why a lessons PR also changes CLAUDE.md.
func importNote(imp string) string {
	return "It also adds an `@AGENTS.md` line to `" + imp + "`, so Claude builders, which read only CLAUDE.md, read AGENTS.md too."
}

const lessonsFooter = "_Merge is always human. This PR never merges itself._\n"

// lessonEntries lists lessons for the PR body: each line and why, fenced
// since the model wrote them (a "fixed #12" there must close nothing),
// and links to the issues and PRs it came from.
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
		b.WriteString(fenced(l.Line + "\n\nWhy: " + truncate(oneLine(l.Why), maxWhy)))
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
			b.WriteString("From " + strings.Join(links, ", ") + ".\n")
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
		if k := normLine(l); k != "" && !had[k] && !strings.HasPrefix(strings.TrimSpace(l), "#") && !readiness.ImportsAgents(l) {
			out = append(out, cleanLine(l))
		}
	}
	return out
}

// reviewHeading is the section of REVIEW.md that learned lines go under.
const reviewHeading = "## Learned from findings you overruled"

// appendLessons adds bullets to the end of AGENTS.md's lessons section,
// making the section (and the file) if it isn't there.
func appendLessons(md string, lines []string) string { return appendSection(md, lessonsHeading, lines) }

// appendSection adds bullets to the end of a file's section under
// heading, making the section (and the file) if it isn't there.
func appendSection(md, heading string, lines []string) string {
	var bullets []string
	for _, l := range lines {
		bullets = append(bullets, "- "+l)
	}
	if strings.TrimSpace(md) == "" {
		return heading + "\n\n" + strings.Join(bullets, "\n") + "\n"
	}
	ls := strings.Split(strings.TrimRight(md, "\n"), "\n")
	start, inCode := -1, false
	for i, l := range ls {
		if strings.HasPrefix(strings.TrimSpace(l), "```") {
			inCode = !inCode
		}
		if !inCode && strings.TrimSpace(l) == heading {
			start = i
			break
		}
	}
	if start < 0 {
		return strings.Join(ls, "\n") + "\n\n" + heading + "\n\n" + strings.Join(bullets, "\n") + "\n"
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

// commitFiles commits changed files in the daemon's own repository and
// returns the new HEAD.
func (g *Git) commitFiles(ctx context.Context, repo, msg string, paths ...string) (string, error) {
	if _, err := g.run(ctx, repo, nil, append([]string{"add", "--"}, paths...)...); err != nil {
		return "", err
	}
	if _, err := g.run(ctx, repo, nil, "commit", "--quiet", "--no-verify", "-m", msg); err != nil {
		return "", err
	}
	return g.run(ctx, repo, nil, "rev-parse", "HEAD")
}
