package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/claptimes5/factory/internal/config"
	"github.com/claptimes5/factory/internal/gates"
	"github.com/claptimes5/factory/internal/state"
	"github.com/claptimes5/factory/internal/store"
	"github.com/claptimes5/factory/internal/verdict"
)

// A PR's description: what it closes, a summary, how to review it (the
// risk), what was tested, the blast radius, how to revert it, then the
// jobs and the reviewer's verdict. When the repo has a PR template on its
// base branch, its headings set the layout: the factory fills each one it
// has a section for and says n/a under the rest. The template is layout
// only: its text never reaches a prompt, and only its headings, cleaned,
// reach the PR.

const (
	// maxPRBody keeps a description under GitHub's 65536-character limit
	// (GitLab's is higher), with room to spare.
	maxPRBody = 60_000
	// maxTemplate is the most of a PR template the factory reads; a bigger
	// one is ignored, as is one that's still this big without comments.
	maxTemplate     = 8 << 10
	maxTemplateFile = 64 << 10
	// The end of each passing check's output the PR shows.
	outputLines = 20
	outputBytes = 1500
	// maxChanged is the most changed paths kept for the revert notes,
	// counting only those a note is about.
	maxChanged = 1000
	// The repo's files the redactor reads, at most, to learn their
	// secrets: past either, nothing is quoted.
	maxSecretFiles = 2000
	maxSecretFile  = 8 << 20
	// minSecret is the shortest known secret value taken out.
	minSecret = 6
)

// templatePaths are where a repo keeps its PR template, in the order the
// factory looks.
var (
	githubTemplates = []string{".github/pull_request_template.md", ".github/PULL_REQUEST_TEMPLATE.md", "PULL_REQUEST_TEMPLATE.md", "pull_request_template.md", "docs/pull_request_template.md", "docs/PULL_REQUEST_TEMPLATE.md"}
	gitlabTemplates = []string{".gitlab/merge_request_templates/Default.md", ".gitlab/merge_request_templates/default.md"}
)

// CheckRun is one check's outcome as the PR's Testing section shows it.
// Output is the end of a passing command's output, already redacted.
type CheckRun struct {
	Repo   string `json:"repo,omitempty"` // the repo it ran in
	Gate   string `json:"gate"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
	Output string `json:"output,omitempty"`
}

// checkRuns keeps what the Testing section needs from one repo's results.
// Output and detail are redacted whole, and only then cut down.
func (p *Pipeline) checkRuns(ctx context.Context, repo string, rs []gates.Result) []CheckRun {
	red := p.redactorFor(ctx, repo)
	out := make([]CheckRun, 0, len(rs))
	for _, r := range rs {
		c := CheckRun{Repo: repo, Gate: r.Gate, Status: r.Status}
		if r.Output != "" {
			c.Output = red.output(r.Output)
		}
		if shownDetail(r.Gate) {
			c.Detail = red.detail(r.Detail)
		}
		out = append(out, c)
	}
	return out
}

// output is a check's output fit to quote: redacted, then its last lines;
// nothing when the redactor couldn't read every secret.
func (r *redactor) output(s string) string {
	if r.blind {
		return ""
	}
	return tailLines(r.clean(s), outputLines, outputBytes)
}

// detail is a check's detail fit to quote: redacted, then its first
// lines. Blind, only its first line, the factory's own words.
func (r *redactor) detail(s string) string {
	s = r.clean(s)
	if r.blind {
		s = firstLine(s)
	}
	return headLines(s, outputBytes)
}

// tailLines is the last lines of s, at most size bytes, in whole lines.
func tailLines(s string, lines, size int) string {
	s = strings.TrimRight(s, "\n ")
	if ls := strings.Split(s, "\n"); len(ls) > lines {
		s = strings.Join(ls[len(ls)-lines:], "\n")
	}
	return gates.TailLines(s, size)
}

// headLines is the start of s, at most size bytes, in whole lines.
func headLines(s string, size int) string {
	if len(s) <= size {
		return s
	}
	s = s[:size]
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[:i] + "\n…"
	}
	return "…"
}

// shownDetail says a check's detail is shown on the PR: the test-proves-fix
// check's and coverage's say what they found. A scenario's is hidden from
// the builder, and the rest say only why a check failed.
func shownDetail(gate string) bool {
	_, name, ok := strings.Cut(gate, ": ") // a workspace's "folder: gate"
	if !ok {
		name = gate
	}
	return name == "proves_fix" || name == "coverage"
}

// prFacts reads, for each repo the change touches, what the PR needs from
// the repository: the files changed, for the revert notes, and the PR
// template on the base branch.
func (p *Pipeline) prFacts(ctx context.Context, parts []*part, in *ReviewInput) {
	in.Changed, in.Templates, in.Unchecked = map[string][]string{}, map[string]string{}, map[string]int{}
	for _, pt := range parts {
		if pt.head == "" || pt.base == "" {
			continue
		}
		if files, err := p.git(pt.repo).ChangedFiles(ctx, pt.ws.Repo, pt.base, pt.head); err == nil {
			// Only the files a revert note is about, so a big change
			// keeps its migrations.
			keep := []string{}
			for _, f := range files {
				if isMigration(f) || isDependency(f) || isConfig(f) {
					keep = append(keep, f)
				}
			}
			if len(keep) > maxChanged {
				in.Unchecked[pt.repo] = len(keep) - maxChanged
				keep = keep[:maxChanged]
			}
			in.Changed[pt.repo] = keep
		} else {
			p.warn(err, "ChangedFiles")
		}
		if t := p.prTemplate(ctx, pt.repo, pt.ws.Repo, pt.ws.BaseSHA); t != "" {
			in.Templates[pt.repo] = t
		}
	}
}

// prTemplate is the repo's PR template on the base branch, with its
// comments taken out, or "" when it has none the factory will use.
func (p *Pipeline) prTemplate(ctx context.Context, repo, dir, sha string) string {
	paths := githubTemplates
	if p.repoConf(repo).Host != "" {
		paths = gitlabTemplates
	}
	for _, name := range paths {
		body, ok, err := p.git(repo).FileAtMost(ctx, dir, sha, name, maxTemplateFile)
		if errors.Is(err, errTooBig) {
			p.log().Info("PR template too big; using the factory's layout", "repo", repo, "file", name)
			return ""
		}
		if err != nil {
			p.warn(err, "PR template")
			return ""
		}
		if ok {
			return cleanTemplate(body)
		}
	}
	return ""
}

var commentRE = regexp.MustCompile(`(?s)<!--.*?-->`)

// cleanTemplate takes the HTML comments out of a template, and refuses
// one that's still too big.
func cleanTemplate(s string) string {
	s = commentRE.ReplaceAllString(strings.ToValidUTF8(s, ""), "")
	if i := strings.Index(s, "<!--"); i >= 0 {
		s = s[:i] // an unclosed comment hides the rest
	}
	if len(s) > maxTemplate {
		return ""
	}
	return s
}

// heading is one of a template's headings.
type heading struct {
	level int
	text  string
}

var (
	atxRE   = regexp.MustCompile(`^ {0,3}(#{1,6})[ \t]+(.*?)(?:[ \t]+#+)?[ \t]*$`)
	fenceRE = regexp.MustCompile("^ {0,3}(```+|~~~+)")
	tagRE   = regexp.MustCompile(`<[^>]*>`)
)

// templateHeadings are a template's headings, outside its code blocks,
// as text fit for a heading of the factory's own: one line, no HTML, no
// backticks, no reference to close or person to mention.
func templateHeadings(s string) []heading {
	var hs []heading
	open := ""
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimRight(line, "\r")
		if m := fenceRE.FindStringSubmatch(line); m != nil {
			switch {
			case open == "":
				open = m[1]
			case m[1][0] == open[0] && len(m[1]) >= len(open):
				open = ""
			}
			continue
		}
		if open != "" {
			continue
		}
		m := atxRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		t := strings.Trim(titleText(tagRE.ReplaceAllString(m[2], ""), 100), " <>")
		t = strings.NewReplacer("<", "", ">", "").Replace(t)
		if strings.TrimFunc(t, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) == "" {
			continue
		}
		hs = append(hs, heading{len(m[1]), t})
		if len(hs) == 50 {
			break
		}
	}
	return hs
}

// The factory's sections, in the order of its own layout.
const (
	secSummary = "summary"
	secIssue   = "issue"
	secRisk    = "risk"
	secTesting = "testing"
	secBlast   = "blast"
	secRevert  = "revert"
	secNotes   = "notes"
)

var sectionTitles = map[string]string{
	secSummary: "Summary", secRisk: "How to review", secTesting: "Testing",
	secBlast: "Blast radius", secRevert: "How to revert", secNotes: "Reviewer notes",
}

// matchSection is the factory section a template heading asks for, or ""
// for one it can't fill.
func matchSection(h string) string {
	words := strings.FieldsFunc(strings.ToLower(h), func(r rune) bool { return !unicode.IsLetter(r) })
	has := func(prefixes ...string) bool {
		for _, w := range words {
			for _, pfx := range prefixes {
				if strings.HasPrefix(w, pfx) {
					return true
				}
			}
		}
		return false
	}
	is := func(names ...string) bool {
		for _, w := range words {
			for _, n := range names {
				if w == n {
					return true
				}
			}
		}
		return false
	}
	joined := " " + strings.Join(words, " ") + " "
	switch {
	case has("revert", "rollback", "undo") || strings.Contains(joined, " roll back ") || strings.Contains(joined, " back out "):
		return secRevert
	case has("test", "verif") || is("qa"):
		return secTesting
	case has("risk", "impact", "blast") || strings.Contains(joined, " how to review ") || is("review", "reviewers"):
		return secRisk
	case is("type", "types", "kind", "screenshot", "screenshots", "checklist"):
		return ""
	case is("issue", "issues", "ticket", "tickets", "related", "closes", "fixes", "links"):
		return secIssue
	case is("summary", "description", "overview", "what", "why", "changes", "changed", "motivation", "context", "purpose", "details", "background"):
		return secSummary
	}
	return ""
}

// bodyParts are a PR description's sections, rendered.
type bodyParts struct {
	top      string            // what it closes, and the factory's line
	sections map[string]string // by sec*; "" or absent for none
	tail     string            // the jobs and the reviewer's verdict
	footer   string
}

// layout puts the parts together: in the factory's order, or under the
// template's headings with the sections they don't name after them.
func (bp bodyParts) layout(tmpl []heading) string {
	var b strings.Builder
	b.WriteString(bp.top)
	used := map[string]bool{}
	if len(tmpl) > 0 {
		for _, h := range tmpl {
			fmt.Fprintf(&b, "%s %s\n\n", strings.Repeat("#", h.level), h.text)
			sec := matchSection(h.text)
			body := bp.sections[sec]
			if sec == secRisk && bp.sections[secBlast] != "" && !used[secBlast] {
				body += "\n**Blast radius**\n\n" + bp.sections[secBlast]
				used[secBlast] = true
			}
			if sec == "" || used[sec] || strings.TrimSpace(body) == "" {
				b.WriteString("_n/a_\n\n")
				continue
			}
			used[sec] = true
			b.WriteString(strings.TrimRight(body, "\n") + "\n\n")
		}
	}
	for _, sec := range []string{secSummary, secRisk, secTesting, secBlast, secNotes, secRevert} {
		if body := bp.sections[sec]; !used[sec] && strings.TrimSpace(body) != "" {
			fmt.Fprintf(&b, "## %s\n\n%s\n\n", sectionTitles[sec], strings.TrimRight(body, "\n"))
		}
	}
	b.WriteString(bp.tail)
	b.WriteString(bp.footer)
	return b.String()
}

// prBody is a PR's description in repo: top (what it closes, and in a
// workspace the linked PRs), then the evidence, within the forge's size
// limit.
func (p *Pipeline) prBody(ctx context.Context, it *store.WorkItem, repo string, in ReviewInput, top string, auto bool) (string, error) {
	jobs, err := p.Store.ListJobs(ctx, store.JobFilter{ItemID: it.ID})
	if err != nil {
		return "", err
	}
	tmpl := templateHeadings(lookup(in.Templates, repo))
	private := false
	if c := p.Cfg.PR.CheckOutput; c == "" || c == config.CheckOutputPrivate {
		private = p.repoPrivate(ctx, repo) // asked only when it matters
	}
	show := p.Cfg.PR.ShowOutput(private)
	var red *redactor
	if len(in.Checks) == 0 && in.ImplementJob != 0 {
		red = p.redactorFor(ctx, repo)
	}
	for _, o := range []bodyOpts{{}, {noOutput: true}, {noOutput: true, noVerdict: true}, {noOutput: true, noVerdict: true, short: true}} {
		o.hidden, o.red = !show, red
		body := p.bodyParts(ctx, it, repo, in, jobs, top, auto, o).layout(tmpl)
		if utf8.RuneCountInString(body) <= maxPRBody {
			return body, nil
		}
	}
	// Still too big: the factory's sections, cut.
	bp := p.bodyParts(ctx, it, repo, in, jobs, top, auto, bodyOpts{noOutput: true, noVerdict: true, short: true, hidden: !show, red: red})
	footer := bp.footer
	bp.footer = ""
	body := bp.layout(tmpl)
	note := "\n_The rest is cut to fit the forge's size limit; the dashboard has it all._\n\n"
	keep := maxPRBody - utf8.RuneCountInString(note+footer) - 32 // room to close a block
	return closeBlocks(cutLines(body, keep)) + note + footer, nil
}

// cutLines is s cut to at most n runes, at a line's end.
func cutLines(s string, n int) string {
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	s = string(rs[:n])
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		s = s[:i+1]
	}
	return s
}

// closeBlocks closes a code block or a <details> a cut left open, so the
// note after it isn't swallowed.
func closeBlocks(s string) string {
	open, details := "", 0
	for _, line := range strings.Split(s, "\n") {
		if m := fenceRE.FindStringSubmatch(line); m != nil {
			switch {
			case open == "":
				open = m[1]
			case m[1][0] == open[0] && len(m[1]) >= len(open) && strings.TrimSpace(line) == m[1]:
				open = ""
			}
			continue
		}
		if open == "" {
			details += strings.Count(line, "<details>") - strings.Count(line, "</details>")
		}
	}
	if !strings.HasSuffix(s, "\n") {
		s += "\n"
	}
	if open != "" {
		s += open + "\n"
	}
	for ; details > 0; details-- {
		s += "</details>\n"
	}
	return s
}

// RepoPrivacy is a forge that says whether a repo is private.
type RepoPrivacy interface {
	Private(ctx context.Context) (bool, error)
}

// privacyTTL is how long the forge's answer on a repo's visibility is
// trusted: a repo made public shows output for at most this long.
const privacyTTL = 5 * time.Minute

type privacyAnswer struct {
	private bool
	at      time.Time
}

// repoPrivate says whether the forge reports repo as private, remembered
// for privacyTTL. Unknown is public.
func (p *Pipeline) repoPrivate(ctx context.Context, repo string) bool {
	key := strings.ToLower(repo)
	if v, ok := p.privacy.Load(key); ok && time.Since(v.(privacyAnswer).at) < privacyTTL {
		return v.(privacyAnswer).private
	}
	f, ok := p.gh(repo).(RepoPrivacy)
	if !ok {
		return false
	}
	private, err := f.Private(ctx)
	if err != nil {
		p.log().Warn("repo visibility unknown; quoting no check output", "repo", repo, "err", err)
		return false // asked again next time
	}
	p.privacy.Store(key, privacyAnswer{private, time.Now()})
	return private
}

// bodyOpts shrink a description that's too big, a step at a time.
type bodyOpts struct {
	noOutput  bool // the checks' output
	noVerdict bool // the reviewer's JSON
	short     bool // the blast radius and job tables
	hidden    bool // the checks' output, by pr.check_output
	// red redacts the stored results the Testing section falls back on;
	// built once per description.
	red *redactor
}

func (p *Pipeline) bodyParts(ctx context.Context, it *store.WorkItem, repo string, in ReviewInput, jobs []*store.Job, top string, auto bool, o bodyOpts) bodyParts {
	verified := fmt.Sprintf("Verified commit `%s`.", short(in.Head))
	if len(in.Heads) > 0 {
		var cs []string
		for _, h := range in.Heads {
			cs = append(cs, fmt.Sprintf("%s `%s`", h.Repo, short(h.SHA)))
		}
		verified = "Verified commits: " + strings.Join(cs, ", ") + "."
	}
	bp := bodyParts{sections: map[string]string{}}
	bp.top = fmt.Sprintf("%s\n\nBuilt by the factory. Complexity **%s**, risk **%s**. %s\n\n", strings.TrimRight(top, "\n"), it.Complexity, riskOrLow(it.Risk), verified)
	if rj := rethinkJob(jobs); rj != nil {
		bp.top += fmt.Sprintf("This is the factory's second approach: the first hit the review round cap, so it started over from the base branch on routing.%s (job %d).\n\n", rj.Route, rj.ID)
	}
	impl := implementVerdict(jobs, in.ImplementJob)
	if s := strings.TrimSpace(impl.Summary); s != "" {
		bp.sections[secSummary] = "The builder's summary, in its words:\n\n" + fenced(inert(safeBlock(truncate(s, 3000))))
	}
	bp.sections[secIssue] = firstLine(top) + "\n" // "Closes #12", or a workspace's "Part of o/r#12"
	bp.sections[secRisk] = p.reviewGuide(ctx, it, in.Features != "")
	if in.ImplementJob != 0 {
		bp.sections[secRisk] += "\n" + p.diffRiskSection(ctx, it, in.ImplementJob)
	}
	if note := p.securityRefutedNote(ctx, it.ID); note != "" {
		bp.sections[secRisk] += "\n" + note
	}
	bp.sections[secTesting] = p.testingSection(ctx, repo, in, impl, o)
	if in.Features != "" {
		blast := "What this change touches, from the feature map on `" + p.repoConf(it.Repo).BaseBranch + "`.\n\n" + in.Features
		if o.short {
			blast = "What this change touches is on the item's page in the dashboard (left out here to fit the forge's size limit).\n"
		}
		bp.sections[secBlast] = blast
	}
	if notes := p.passNotes(jobs); notes != "" && !o.noVerdict {
		bp.sections[secNotes] = "Findings that didn't block: minor ones, problems the change didn't introduce, and ones a second model checked and refuted.\n\n" + notes
	}
	bp.sections[secRevert] = p.revertSection(it, repo, in)

	var t strings.Builder
	var total float64
	t.WriteString("## Jobs\n\n")
	if !o.short {
		t.WriteString("| Job | Kind | Harness | Model | Skills | Attempt | State | Cost |\n| --- | --- | --- | --- | --- | --- | --- | --- |\n")
	}
	for i := len(jobs) - 1; i >= 0; i-- {
		j := jobs[i]
		total += j.CostUSD
		sk := j.Skillset
		if sk == "" {
			sk = "-"
		}
		if !o.short {
			fmt.Fprintf(&t, "| %d | %s | %s | %s | %s | %d | %s | $%.2f |\n", j.ID, j.Kind, j.Harness, j.ModelLabel(), sk, j.Attempt, j.State, j.CostUSD)
		}
	}
	if o.short {
		fmt.Fprintf(&t, "%d jobs; the dashboard lists them.\n", len(jobs))
	}
	fmt.Fprintf(&t, "\n**Total cost:** $%.2f\n\n", total)
	for _, j := range jobs {
		if o.noVerdict {
			break
		}
		if (j.Kind == "review" || j.Kind == "critic") && j.State == state.Succeeded {
			var pretty any
			if json.Unmarshal([]byte(j.Verdict), &pretty) != nil {
				continue
			}
			pv, _ := json.MarshalIndent(pretty, "", "  ")
			f := fence(string(pv)) // a finding may quote backticks
			fmt.Fprintf(&t, "<details><summary>%s verdict (job %d, %s)</summary>\n\n%sjson\n%s\n%s\n</details>\n\n", j.Kind, j.ID, j.Model, f, inert(string(pv)), f)
			break
		}
	}
	bp.tail = t.String()
	bp.footer = humanFooter
	if auto {
		bp.footer = autoFooter
	}
	return bp
}

// implementVerdict is the builder's report from the implement job, or an
// empty one.
func implementVerdict(jobs []*store.Job, id int64) verdict.Implement {
	var v verdict.Implement
	for _, j := range jobs {
		if j.ID == id {
			_ = json.Unmarshal([]byte(j.Verdict), &v)
		}
	}
	return v
}

// testingSection says what ran and how it went: every check with its
// result, the test-proves-fix check and coverage, and the end of each
// passing command's output, folded away.
func (p *Pipeline) testingSection(ctx context.Context, repo string, in ReviewInput, impl verdict.Implement, o bodyOpts) string {
	ws := len(in.Heads) > 0
	checks := in.Checks
	if len(checks) == 0 && in.ImplementJob != 0 {
		// Queued before the PR kept its checks (or reconciled after a
		// restart): the stored results, without output. A workspace's
		// don't say which repo they ran in, so they show pass or fail only.
		gr, _ := p.Store.GateResults(ctx, in.ImplementJob)
		red := o.red
		if red == nil {
			red = p.redactorFor(ctx, repo)
		}
		for _, g := range gr {
			c := CheckRun{Gate: g.Gate, Status: g.Status}
			if !ws {
				c.Repo = repo
				if shownDetail(g.Gate) {
					c.Detail = red.detail(g.Detail)
				}
			}
			checks = append(checks, c)
		}
	}
	// Only this PR's repo's checks are quoted: another repo's output
	// belongs on its own PR, which may be more private than this one.
	own := func(c CheckRun) bool { return c.Repo == "" && !ws || strings.EqualFold(c.Repo, repo) }
	var b strings.Builder
	if line := firstLine(in.Gates); line != "" {
		b.WriteString(SafeText(line, 500) + "\n\n")
	}
	if len(checks) == 0 {
		if rest := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(in.Gates), firstLine(in.Gates))); rest != "" {
			b.WriteString(rest + "\n")
		}
		return b.String()
	}
	b.WriteString("| Check | Result |\n| --- | --- |\n")
	for _, c := range checks {
		fmt.Fprintf(&b, "| `%s` | %s |\n", cell(inert(clip(c.Gate, 100))), cell(clip(c.Status, 20)))
	}
	b.WriteString("\n")
	for _, c := range checks {
		if !shownDetail(c.Gate) || c.Detail == "" || !own(c) {
			continue
		}
		label := "Test proves the fix"
		if strings.HasSuffix(c.Gate, "coverage") {
			label = "Coverage"
		}
		if pfx, _, ok := strings.Cut(c.Gate, ": "); ok {
			label = clip(pfx, 60) + ": " + label
		}
		first, rest, _ := strings.Cut(c.Detail, "\n")
		fmt.Fprintf(&b, "**%s:** %s, %s.\n", label, clip(c.Status, 20), strings.TrimRight(inert(clip(first, 200)), "."))
		if strings.TrimSpace(rest) != "" && !o.noOutput && !o.hidden {
			b.WriteString("\n" + fenced(inert(safeBlock(rest))))
		}
		b.WriteString("\n")
	}
	if len(impl.TestsAdded) > 0 {
		var ts []string
		for i, t := range impl.TestsAdded {
			if i == 10 {
				ts = append(ts, fmt.Sprintf("and %d more", len(impl.TestsAdded)-i))
				break
			}
			ts = append(ts, "`"+inert(clip(t, 100))+"`")
		}
		b.WriteString("The builder says it added or changed these tests: " + strings.Join(ts, ", ") + ".\n\n")
	}
	var out strings.Builder
	for _, c := range checks {
		if c.Output == "" || c.Status != gates.Pass || !own(c) {
			continue
		}
		fmt.Fprintf(&out, "`%s`\n\n%s\n", inert(clip(c.Gate, 100)), fenced(inert(safeBlock(c.Output))))
	}
	switch {
	case out.Len() == 0:
	case o.hidden:
		b.WriteString("_The checks' output isn't quoted here: `pr.check_output` shows it only on a repo the forge reports as private. The job's page in the dashboard has it._\n")
	case o.noOutput:
		b.WriteString("_The checks' output is left out to fit the forge's size limit; the job's page in the dashboard has it._\n")
	default:
		b.WriteString("<details><summary>The end of each passing check's output (lines that may hold a secret are left out)</summary>\n\n" + out.String() + "</details>\n")
	}
	return b.String()
}

var mentionRE = regexp.MustCompile(`@(\w)`)

// inert is untrusted text that can't act once it's on the PR: no issue
// reference to close and no @name to mention, even outside a code block.
func inert(s string) string { return noClose(mentionRE.ReplaceAllString(s, "@ $1")) }

// lookup finds repo's entry, matching its name as the forge does.
func lookup[V any](m map[string]V, repo string) V {
	if v, ok := m[repo]; ok {
		return v
	}
	for k, v := range m {
		if strings.EqualFold(k, repo) {
			return v
		}
	}
	var zero V
	return zero
}

// safeBlock is untrusted text fit for a code block: its lines kept, with
// no control characters or invisible ones (bidi overrides, zero-width
// spaces), and tabs as spaces.
func safeBlock(s string) string {
	s = strings.ToValidUTF8(strings.ReplaceAll(s, "\r\n", "\n"), "")
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n':
			return r
		case r == '\t':
			return ' '
		case unicode.IsControl(r), unicode.Is(unicode.Zl, r), unicode.Is(unicode.Zp, r):
			return ' '
		case unicode.Is(unicode.Cf, r):
			return -1
		}
		return r
	}, s)
}

// --- how to revert ---

// revertNote is a step a revert needs besides git revert.
type revertNote struct {
	kind  string // migration|config|dependency
	paths []string
}

var (
	depFiles = map[string]bool{
		"go.mod": true, "go.sum": true, "package.json": true, "package-lock.json": true, "npm-shrinkwrap.json": true,
		"yarn.lock": true, "pnpm-lock.yaml": true, "bun.lockb": true, "bun.lock": true, "gemfile": true, "gemfile.lock": true,
		"pipfile": true, "pipfile.lock": true, "pyproject.toml": true, "poetry.lock": true, "uv.lock": true, "setup.py": true,
		"setup.cfg": true, "cargo.toml": true, "cargo.lock": true, "composer.json": true, "composer.lock": true, "pom.xml": true,
		"build.gradle": true, "build.gradle.kts": true, "gradle.lockfile": true, "mix.exs": true, "mix.lock": true,
		"package.swift": true, "package.resolved": true, "podfile": true, "podfile.lock": true, "deno.json": true, "deno.lock": true,
		"go.work": true, "go.work.sum": true,
	}
	reqRE      = regexp.MustCompile(`^requirements[\w.-]*\.(txt|in)$`)
	configExts = map[string]bool{".yaml": true, ".yml": true, ".toml": true, ".ini": true, ".conf": true, ".cfg": true, ".properties": true, ".env": true}
	configDirs = map[string]bool{"config": true, "configs": true, "conf": true, "settings": true, "deploy": true, "k8s": true, "helm": true, "terraform": true}
)

// isMigration says a path is a database migration: under a migrations
// folder (Django's, Prisma's, golang-migrate's, Flyway's db/migration and
// the like), Rails' db/migrate, Alembic's versions or Liquibase's
// db/changelog, or a Flyway-style versioned SQL file.
func isMigration(p string) bool {
	lp := "/" + strings.ToLower(p)
	segs := strings.Split(lp, "/")
	for _, s := range segs[:len(segs)-1] {
		if s == "migrations" || s == "migration" {
			return true
		}
	}
	for _, d := range []string{"/db/migrate/", "/alembic/versions/", "/db/changelog/"} {
		if strings.Contains(lp, d) {
			return true
		}
	}
	return flywayRE.MatchString(segs[len(segs)-1])
}

var flywayRE = regexp.MustCompile(`^v\d+(_\d+)*__.*\.sql$`)

func isDependency(p string) bool {
	base := strings.ToLower(path.Base(p))
	return depFiles[base] || reqRE.MatchString(base)
}

func isConfig(p string) bool {
	if isDependency(p) || isMigration(p) || gates.IsTest(p) {
		return false
	}
	lp := strings.ToLower(p)
	if strings.HasPrefix(lp, ".github/") || strings.HasPrefix(lp, ".gitlab/") || lp == ".gitlab-ci.yml" {
		return false // CI, not what a deploy reads
	}
	base := path.Base(lp)
	if base == ".env" || strings.HasPrefix(base, ".env.") || configExts[path.Ext(base)] {
		return true
	}
	segs := strings.Split(lp, "/")
	for _, s := range segs[:len(segs)-1] {
		if configDirs[s] {
			return true
		}
	}
	return false
}

// revertNotes are the steps besides git revert that reverting the change
// to files takes.
func revertNotes(files []string) []revertNote {
	var mig, cfg, dep []string
	for _, f := range files {
		switch {
		case isMigration(f):
			mig = append(mig, f)
		case isDependency(f):
			dep = append(dep, f)
		case isConfig(f):
			cfg = append(cfg, f)
		}
	}
	var out []revertNote
	for _, n := range []revertNote{{"migration", mig}, {"config", cfg}, {"dependency", dep}} {
		if len(n.paths) > 0 {
			out = append(out, n)
		}
	}
	return out
}

// migrationHint is how the tool behind a migration path rolls one back.
func migrationHint(paths []string) string {
	for _, p := range paths {
		lp := strings.ToLower(p)
		switch {
		case strings.Contains(lp, "db/migrate/"):
			return "`bin/rails db:rollback` (one step per migration)"
		case strings.Contains(lp, "alembic/"):
			return "`alembic downgrade -1` (one step per migration)"
		case strings.Contains(lp, "prisma/migrations/"):
			return "a new migration that undoes it (Prisma has no down step)"
		case strings.HasSuffix(lp, ".py") && strings.Contains(lp, "/migrations/"):
			return "`python manage.py migrate <app> <the migration before it>`"
		}
	}
	return "the migration tool's down step, or a new migration that undoes it if the tool only goes forward"
}

// revertSection is the PR's "How to revert" section for repo's PR.
func (p *Pipeline) revertSection(it *store.WorkItem, repo string, in ReviewInput) string {
	base := p.repoConf(repo).BaseBranch
	branch := "revert-" + strings.ReplaceAll(p.branchFor(it), "/", "-")
	cmds := fmt.Sprintf("git fetch origin %s\ngit switch -c %s origin/%s\ngit revert --no-edit -m 1 <merge commit>    # merged with a merge commit\n# squash-merged: git revert --no-edit <squash commit>\ngit push origin %s\n", base, branch, base, branch)
	var b strings.Builder
	b.WriteString("Once it's merged, the forge's **Revert** button on this PR opens one that undoes it. By hand:\n\n")
	b.WriteString(fence(cmds) + "sh\n" + cmds + fence(cmds) + "\n\n")
	if len(in.Heads) > 1 {
		b.WriteString("This change spans several repos: revert their PRs in the reverse of the order they merged in.\n\n")
	}
	files, known := in.Changed[repo]
	if !known {
		for k, v := range in.Changed {
			if strings.EqualFold(k, repo) {
				files, known = v, true
			}
		}
	}
	if !known {
		b.WriteString("_The factory couldn't list the changed files, so check the diff for migrations, configuration or dependencies before you revert._\n")
		return b.String()
	}
	for _, n := range revertNotes(files) {
		switch n.kind {
		case "migration":
			fmt.Fprintf(&b, "- **It changes database migrations** (%s). A revert takes the files out but doesn't undo the schema: a database they already ran on keeps the change. Roll them back first with %s, then revert.\n", pathList(n.paths), migrationHint(n.paths))
		case "config":
			fmt.Fprintf(&b, "- **It changes configuration** (%s). A revert restores the files; anything already running with the new values needs a redeploy or restart to drop them.\n", pathList(n.paths))
		case "dependency":
			fmt.Fprintf(&b, "- **It changes dependencies** (%s). After the revert, do a clean install from the reverted lock file so the dropped packages leave builds, images and caches.\n", pathList(n.paths))
		}
	}
	if n := lookup(in.Unchecked, repo); n > 0 {
		fmt.Fprintf(&b, "- _And %d more migration, configuration or dependency files not listed here: read the diff for them._\n", n)
	}
	return b.String()
}

// pathList names up to five paths as inline code.
func pathList(ps []string) string {
	var cs []string
	for i, f := range ps {
		if i == 5 {
			cs = append(cs, fmt.Sprintf("and %d more", len(ps)-i))
			break
		}
		cs = append(cs, "`"+inert(clip(f, 120))+"`")
	}
	return strings.Join(cs, ", ")
}

// --- redaction ---

// redactor takes secrets out of a check's output before the PR quotes
// it: the values the factory knows are secret (every harness's keys, the
// repo's env and services' env, the values in the repo's files), common
// token shapes, and whole lines that name a token, secret, password, key
// or credential, or set an environment variable.
type redactor struct {
	values []string
	rep    *strings.Replacer // every value, longest first
	// blind is a redactor that couldn't read all of the repo's files, so
	// it doesn't know every secret: nothing is quoted with it.
	blind bool
}

var (
	tokenRE      = regexp.MustCompile(`([sr]k_(?:live|test)_[A-Za-z0-9]{10,}|AIza[0-9A-Za-z_-]{35}|gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|glpat-[A-Za-z0-9_-]{20,}|sk-[A-Za-z0-9_-]{20,}|xox[abeprs]-[A-Za-z0-9-]{10,}|AKIA[0-9A-Z]{16}|eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,})`)
	secretLineRE = regexp.MustCompile(`(?i)(token|secret|passw|passwd|pwd|api[_-]?key|access[_-]?key|private[_-]?key|credential|authoriz|bearer|cookie|session[_-]?id|BEGIN [A-Z ]*PRIVATE)`)
	envLineRE    = regexp.MustCompile(`^\s*(export\s+|declare\s+-x\s+)?[A-Za-z_][A-Za-z0-9_]*=`)
	kvRE         = regexp.MustCompile(`^\s*(?:export\s+)?["']?([\w.-]+)["']?\s*[=:]\s*(.+?)\s*,?\s*$`)
	commentRE2   = regexp.MustCompile(`\s+#.*$`)
	longWordRE   = regexp.MustCompile(`[A-Za-z0-9+/=_\-.]{20,}`)
)

// secretValue is a value the redactor may take out, with the name it
// was set under ("" for none).
type secretValue struct{ key, value string }

// secretKeyRE is a name whose value is a secret even when it's short.
var secretKeyRE = regexp.MustCompile(`(?i)(token|secret|key|pass|pwd|auth|cred|private|cert|salt|sign|api|dsn|session|cookie)`)

func (p *Pipeline) redactorFor(ctx context.Context, repo string) *redactor {
	var strong, vals []secretValue
	for _, s := range p.Secrets {
		for k, v := range s {
			strong = append(strong, secretValue{k, v})
		}
	}
	rc := p.repoConf(repo)
	for k, v := range rc.Env {
		vals = append(vals, secretValue{k, v})
	}
	for _, sv := range rc.Services {
		for k, v := range sv.Env {
			vals = append(vals, secretValue{k, v})
		}
	}
	blind := false
	from, rel, err := p.hostFiles(ctx, repo)
	switch {
	case err != nil:
		blind = true
	case from != "":
		fv, complete := fileSecrets(from, rel)
		blind = !complete
		vals = append(vals, fv...)
	}
	r := newRedactor(strong, vals)
	r.blind = blind
	return r
}

// newRedactor takes out the strong values (the harnesses' keys) from
// minSecret characters, and the others as keepSecret says.
func newRedactor(strong, vals []secretValue) *redactor {
	seen := map[string]bool{}
	var values []string
	var add func(sv secretValue, strong bool)
	add = func(sv secretValue, strong bool) {
		v := strings.TrimSpace(sv.value)
		if seen[v] || !keepSecret(sv.key, v, strong) {
			return
		}
		seen[v] = true
		values = append(values, v)
		// A multi-line value (a PEM key) is also taken out line by line
		// and word by word, so no line of it survives a cut.
		if strings.Contains(v, "\n") {
			for _, l := range strings.Split(v, "\n") {
				add(secretValue{sv.key, l}, strong)
				for _, w := range longWordRE.FindAllString(l, -1) {
					add(secretValue{sv.key, w}, strong)
				}
			}
		}
	}
	for _, sv := range strong {
		add(sv, true)
	}
	for _, sv := range vals {
		add(sv, false)
	}
	// Longest first: a Replacer tries its pairs in order at each place,
	// so a secret holding another is taken out whole.
	sort.SliceStable(values, func(i, j int) bool { return len(values[i]) > len(values[j]) })
	pairs := make([]string, 0, 2*len(values))
	for _, v := range values {
		pairs = append(pairs, v, "[REDACTED]")
	}
	return &redactor{values: values, rep: strings.NewReplacer(pairs...)}
}

// keepSecret says whether a known value is worth taking out of output:
// any of minSecret characters or more set under a name that sounds secret
// (or a harness's key), and otherwise only a long or random-looking one,
// so HOST=localhost doesn't blank every line that names localhost.
func keepSecret(key, v string, strong bool) bool {
	switch {
	case len(v) < minSecret:
		return false
	case strong || secretKeyRE.MatchString(key):
		return true
	case len(v) >= 12:
		return true
	}
	return len(v) >= 8 && looksRandom(v)
}

// looksRandom says a value mixes at least three of lower case, upper
// case, digits and symbols, as generated secrets do.
func looksRandom(v string) bool {
	var lower, upper, digit, other int
	for _, r := range v {
		switch {
		case unicode.IsLower(r):
			lower = 1
		case unicode.IsUpper(r):
			upper = 1
		case unicode.IsDigit(r):
			digit = 1
		default:
			other = 1
		}
	}
	return lower+upper+digit+other >= 3
}

// fileSecrets are the values in the repo's files (a .env, a credentials
// file): each KEY=value's value, with and without a trailing comment, and
// every long word. Each file is opened as the copy into a checkout opens
// it, through links. complete is false when a file couldn't be read in
// full or there are too many: then some secret may be missing.
func fileSecrets(from string, rel []string) (vals []secretValue, complete bool) {
	if len(rel) > maxSecretFiles {
		return nil, false
	}
	for _, r := range rel {
		b, ok := readSecretFile(filepath.Join(from, filepath.FromSlash(r)))
		if !ok {
			return nil, false
		}
		for _, line := range strings.Split(string(b), "\n") {
			if m := kvRE.FindStringSubmatch(line); m != nil {
				vals = append(vals, secretValue{m[1], strings.Trim(m[2], `"' `)},
					secretValue{m[1], strings.Trim(commentRE2.ReplaceAllString(m[2], ""), `"' `)})
			}
			for _, w := range longWordRE.FindAllString(line, -1) {
				vals = append(vals, secretValue{"", w})
			}
		}
	}
	return vals, true
}

// readSecretFile reads one of the repo's files as placeFile copies it.
// Something that isn't a regular file isn't copied, so it's fine to skip.
func readSecretFile(path string) ([]byte, bool) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	fi, err := f.Stat()
	switch {
	case err != nil:
		return nil, false
	case !fi.Mode().IsRegular():
		return nil, true
	case fi.Size() > maxSecretFile:
		return nil, false
	}
	b, err := io.ReadAll(io.LimitReader(f, maxSecretFile+1))
	if err != nil || len(b) > maxSecretFile {
		return nil, false
	}
	return b, true
}

// clean is s with the secrets taken out.
func (r *redactor) clean(s string) string {
	if r.rep != nil {
		s = r.rep.Replace(s)
	}
	s = tokenRE.ReplaceAllString(s, "[REDACTED]")
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if secretLineRE.MatchString(l) || envLineRE.MatchString(l) {
			lines[i] = "[line left out: it may hold a secret]"
		}
	}
	return strings.Join(lines, "\n")
}
