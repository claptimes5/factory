package pipeline

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/claptimes5/factory/internal/config"
	"github.com/claptimes5/factory/internal/features"
	"github.com/claptimes5/factory/internal/gates"
	"github.com/claptimes5/factory/internal/store"
	"github.com/claptimes5/factory/internal/verdict"
)

// This file is the security pass: a review of its own, with
// prompts/security.md on routing.security, of a change that touches code
// where a mistake is an exploit. It reads the change beside the reviewer,
// pinned to the same verified commit, and its findings join the
// reviewer's: they get ids, go to the verifier, are answered by the
// builder and ruled on in the next round like any other. Its critical
// findings block whatever the reviewer decides, and a pass that can't run
// or says nothing usable fails the review: it never passes silently.

// CheckSecurityPrompt fails when the security pass is on and its prompt
// is missing, so the daemon says so at start rather than at the first
// change that needs one.
func (p *Pipeline) CheckSecurityPrompt() error {
	if !p.Cfg.Security.On() {
		return nil
	}
	if _, err := os.Stat(filepath.Join(p.Cfg.Path(p.Cfg.PromptsDir), "security.md")); err != nil {
		return fmt.Errorf("the security pass is on but its prompt is missing: %w; restore it, or set security: {enabled: false}", err)
	}
	return nil
}

// securityWhy says why review job j's change gets a security pass, or nil
// when it doesn't: a changed file matches security.paths, it touches a
// high-risk feature in the feature map on the base branch, the code it
// adds does something risky (security.content), or an earlier pass's
// finding is still open, which only a pass may rule on. What can't be
// read is a reason too: the pass runs rather than be skipped on a guess.
func (p *Pipeline) securityWhy(ctx context.Context, j *store.Job, parts []*part, diff string, rr *reviewRound) []string {
	sec := p.Cfg.Security
	if j.Kind != "review" || !sec.On() {
		return nil
	}
	var why []string
	for _, pt := range parts {
		if pt.head == "" {
			continue // a workspace repo the change doesn't touch
		}
		in := ""
		if pt.folder != "" {
			in = pt.folder + "/"
		}
		changed, err := p.git(pt.repo).ChangedFiles(ctx, pt.ws.Repo, pt.base, pt.head)
		if err != nil {
			why = append(why, "the changed files couldn't be listed: "+err.Error())
			continue
		}
		why = append(why, securityPaths(sec.Match, changed, in)...)
		m, err := p.loadMap(ctx, pt.repo, pt.ws.Repo, pt.base)
		if err != nil {
			why = append(why, "the feature map couldn't be read, so its high-risk features can't be ruled out: "+err.Error())
			continue
		}
		why = append(why, highRiskFeatures(m, changed, in)...)
	}
	if sec.ContentOn() {
		why = append(why, riskyCode(diff)...)
	}
	if n := len(securityOpen(rr)); n > 0 {
		why = append(why, fmt.Sprintf("%d of its findings from an earlier round are still open", n))
	}
	return why
}

// securityPaths are the changed files a security glob matches, other than
// tests and documents, as reasons for a pass.
func securityPaths(match func(string) (string, bool), changed []string, in string) []string {
	var hit []string
	for _, f := range changed {
		if gates.IsTest(f) || doc(f) {
			continue
		}
		if g, ok := match(f); ok {
			hit = append(hit, fmt.Sprintf("%s (matches %s)", SafeText(in+f, 200), SafeText(g, 100)))
		}
	}
	if len(hit) == 0 {
		return nil
	}
	if len(hit) > 10 {
		hit = append(hit[:10], fmt.Sprintf("and %d more", len(hit)-10))
	}
	return []string{"it changes " + strings.Join(hit, ", ")}
}

// highRiskFeatures are the high-risk features whose code (not only their
// tests) the change touches, as reasons for a pass.
func highRiskFeatures(m *features.Map, changed []string, in string) []string {
	if m == nil {
		return nil
	}
	var code []string
	for _, f := range changed {
		if !gates.IsTest(f) {
			code = append(code, f)
		}
	}
	hits, _ := m.Touched(code)
	var out []string
	for _, h := range hits {
		if h.Feature.Risk == "high" {
			out = append(out, fmt.Sprintf("it touches %s, a high-risk feature (%s)", SafeText(h.Feature.Name, 100), SafeText(in+h.Files[0], 200)))
		}
	}
	return out
}

// riskySignals are what code a change adds can do that gets a security
// pass whatever file it is in. Each is narrow, to keep false starts rare:
// a call that runs a command, not the word exec.
var riskySignals = []struct {
	what   string
	re     *regexp.Regexp
	unless *regexp.Regexp // a line that also matches this is safe
	ext    string         // only in files of this extension, if set
}{
	{what: "runs a command", re: regexp.MustCompile(`"os/exec"|\bexec\.Command(Context)?\(|\bsyscall\.Exec\(|\bsubprocess\.[a-z_]+\(|\b(from|import) subprocess\b|\bos\.(system|popen|exec[lv]p?e?)\(|\bchild_process\b|\bProcessBuilder\(|Runtime\.getRuntime\(\)\.exec\(|\bCommand::new\(|(^|[^\w.$>:])(system|exec|shell_exec|proc_open|passthru|popen)\(`)},
	{what: "runs a command", re: regexp.MustCompile("`[^`]*`|%x[({\\[]"), unless: regexp.MustCompile(`^\s*#`), ext: ".rb"}, // Ruby's backticks, outside comments
	{what: "builds SQL from strings", re: sqlBuilt},
	{what: "evaluates code", re: regexp.MustCompile(`(^|[^\w.])eval\(|\bnew Function\(`)},
	{what: "deserializes an unsafe format", re: regexp.MustCompile(`\bpickle\.loads?\(|\bmarshal\.loads?\(|\bMarshal\.load\(|\b(yaml|YAML)\.(load|unsafe_load)\(|\bunserialize\(|\bObjectInputStream\(|\bBinaryFormatter\b`),
		unless: regexp.MustCompile(`SafeLoader|safe_load|permitted_classes`)},
	{what: "writes raw HTML", re: regexp.MustCompile(`dangerouslySetInnerHTML|\.(inner|outer)HTML\s*\+?=[^=]|\binsertAdjacentHTML\(|\bdocument\.write(ln)?\(|\bv-html\b|\btemplate\.HTML\(|\.html_safe\b|\bmark_safe\(|\bMarkup\(|\|\s*safe\s*(\}\}|\||%)`)},
	{what: "turns off TLS checks", re: regexp.MustCompile(`InsecureSkipVerify\s*[:=]\s*true|\bverify\s*=\s*False\b|rejectUnauthorized:\s*false|NODE_TLS_REJECT_UNAUTHORIZED|CURLOPT_SSL_VERIFY(PEER|HOST),\s*(0|false)|\bCERT_NONE\b|\bcheck_hostname\s*=\s*False\b|_create_unverified_context|\bcurl\b.*\s(-k|--insecure)(\s|$)|--no-check-certificate`)},
}

// sqlBuilt is a quoted SQL statement joined to something, or with a
// value formatted into it, rather than a placeholder.
var sqlBuilt = func() *regexp.Regexp {
	q, nq := "[\"'`]", "[^\"'`]"
	return regexp.MustCompile(`(?i)` + q + `\s*(select\s` + nq + `*?\bfrom\b|insert\s+into\b|update\s+\w+\s+set\b|delete\s+from\b)` + nq +
		`*(` + q + `+\s*(\+|\|\||\.\s*format\()|%[sv]|\$\{|\{[a-z_][\w.]*\})`)
}()

// riskyCode reads the lines a diff adds, outside tests and prose, for
// riskySignals, as reasons for a pass. A file's name is read only from its
// header, between its diff --git line and its first hunk, so an added
// line that looks like a header can't rename the file it is in.
func riskyCode(diff string) []string {
	found := map[string][]string{} // what -> files
	file, header := "", false
	for _, l := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(l, "diff --git "):
			// Until the +++ line, if any: a file with no hunks adds nothing.
			file, header = "", true
			continue
		case header && strings.HasPrefix(l, "+++ "):
			file = strings.TrimPrefix(strings.Trim(strings.TrimPrefix(l, "+++ "), `"`), "b/")
			continue
		case header && strings.HasPrefix(l, "@@"):
			header = false
			continue
		case header:
			continue
		}
		if !strings.HasPrefix(l, "+") || file == "" || file == "/dev/null" || gates.IsTest(file) || prose(file) {
			continue
		}
		for _, s := range riskySignals {
			if s.ext != "" && !strings.EqualFold(path.Ext(file), s.ext) {
				continue
			}
			if s.re.MatchString(l[1:]) && (s.unless == nil || !s.unless.MatchString(l[1:])) && !slices.Contains(found[s.what], file) {
				found[s.what] = append(found[s.what], file)
			}
		}
	}
	var out []string
	seen := map[string]bool{}
	for _, s := range riskySignals {
		fs := found[s.what]
		if len(fs) == 0 || seen[s.what] {
			continue
		}
		seen[s.what] = true
		for i, f := range fs {
			fs[i] = SafeText(f, 200)
		}
		if len(fs) > 5 {
			fs = append(fs[:5], fmt.Sprintf("and %d more", len(fs)-5))
		}
		out = append(out, fmt.Sprintf("code it adds %s, in %s", s.what, strings.Join(fs, ", ")))
	}
	return out
}

// prose is a file of text to read, whose code samples run nowhere.
func prose(file string) bool {
	return doc(file) || strings.EqualFold(path.Ext(file), ".txt")
}

// doc is a document, which no path glob makes a reason for a pass (a
// requirements.txt is code; an AUTHORS.md isn't).
func doc(file string) bool {
	switch strings.ToLower(path.Ext(file)) {
	case ".md", ".markdown", ".rst", ".adoc":
		return true
	}
	return false
}

// securityOpen are the open findings an earlier security pass raised:
// only a security pass rules on them.
func securityOpen(rr *reviewRound) []store.ReviewFinding {
	var out []store.ReviewFinding
	for _, f := range rr.open {
		if f.Kind == store.KindSecurity {
			out = append(out, f)
		}
	}
	return out
}

// splitRound gives the reviewer and the security pass each their own open
// findings to rule on. Each sees the other's among what was already
// raised, so it doesn't raise them again, and everything else raised
// before.
func splitRound(rr *reviewRound) (review, security *reviewRound) {
	review, security = &reviewRound{n: rr.n, triage: rr.triage, prior: rr.prior}, &reviewRound{n: rr.n, triage: rr.triage}
	for _, f := range rr.open {
		if f.Kind == store.KindSecurity {
			security.open = append(security.open, f)
			review.seen = append(review.seen, f)
		} else {
			review.open = append(review.open, f)
			security.seen = append(security.seen, f)
		}
	}
	review.seen = append(review.seen, rr.seen...)
	security.seen = append(security.seen, rr.seen...)
	return review, security
}

// startSecurity starts the security pass beside review job j on route r,
// on the whole change and with its own open findings, and returns a wait
// for its verdict, or nil and why when it can't run now.
func (p *Pipeline) startSecurity(ctx context.Context, j *store.Job, it *store.WorkItem, work string, repos []string, d PromptData, diff string, rr *reviewRound, why []string, r config.Route, taken []string) (func(goOn bool) pairedRun, string) {
	// The whole change each round, however little of it a re-review reads.
	d.Diff, d.Fence, d.Since, d.Context, d.ContextFence, d.ContextFiles = diff, fence(diff), "", "", "", ""
	d.Earlier = earlierText(rr)
	var b strings.Builder
	for _, w := range why {
		fmt.Fprintf(&b, "- %s\n", oneLine(w))
	}
	d.Security = fenced(b.String())
	return p.startBeside(ctx, j, it, work, repos, d, beside{kind: store.KindSecurity, route: "security", r: r, prompt: "security", as: store.KindSecurity}, taken)
}

// builtWith are the harnesses and models the item's builds since its
// latest triage ran on, which its security pass mustn't be.
func (p *Pipeline) builtWith(ctx context.Context, itemID, triage int64) []config.Route {
	jobs, err := p.Store.ListJobs(ctx, store.JobFilter{ItemID: itemID, Kind: "implement"})
	if err != nil {
		p.warn(err, "ListJobs")
	}
	var out []config.Route
	for _, b := range jobs {
		if b.ID > triage {
			out = append(out, config.Route{Harness: b.Harness, Model: b.Model})
		}
	}
	return out
}

// securityRefuted are the security pass's findings a verifier refuted
// since the item's latest triage. Each is a security hole two models
// disagree on, so the item's PR says so and never merges itself.
func (p *Pipeline) securityRefuted(ctx context.Context, itemID int64) []store.ReviewFinding {
	triage := int64(0)
	if ts, err := p.Store.ListJobs(ctx, store.JobFilter{ItemID: itemID, Kind: "triage", Limit: 1}); err == nil && len(ts) > 0 {
		triage = ts[0].ID
	}
	fs, err := p.Store.ItemFindings(ctx, itemID, triage)
	if err != nil {
		p.warn(err, "ItemFindings")
		// Can't tell: as if one were, so nothing merges on a guess.
		return []store.ReviewFinding{{FID: "?", Claim: "the findings couldn't be read"}}
	}
	var out []store.ReviewFinding
	for _, f := range fs {
		if f.Kind == store.KindSecurity && f.Check == verdict.Refuted {
			out = append(out, f)
		}
	}
	return out
}

// securityRefutedNote is the PR's word on refuted security findings, or "".
func (p *Pipeline) securityRefutedNote(ctx context.Context, itemID int64) string {
	fs := p.securityRefuted(ctx, itemID)
	if len(fs) == 0 {
		return ""
	}
	var b strings.Builder
	for _, f := range fs {
		fmt.Fprintf(&b, "%s (%s) %s: %s\n  Refuted: %s\n", f.FID, f.Severity, findingLoc(f.File, f.Line), oneLine(truncate(f.Claim, 300)), oneLine(truncate(f.CheckReason, 300)))
	}
	return "**The security review raised findings a verifier refuted**, so they didn't block. Check each yourself; this PR doesn't merge itself.\n\n" + fenced(b.String())
}

// withSecurity joins the security pass's verdict s to the reviewer's v:
// either one's questions park the item, either one's block blocks, and each finding keeps whether it can block
// as its own reviewer judged it, so a critical security finding blocks
// whatever the reviewer decided. Each rules only on its own open
// findings (own names the pass's). ok is false when the pass blocked
// without a finding to check, so nothing is verified and the block stands.
func withSecurity(v, s *verdict.Review, own []store.ReviewFinding, why []string) (out *verdict.Review, ok bool) {
	ours := func(id string) bool {
		return slices.ContainsFunc(own, func(f store.ReviewFinding) bool { return f.FID == id })
	}
	out = v
	out.Security = why
	if len(out.Reviewers) == 0 {
		out.Reviewers = []string{"review"}
	}
	out.Reviewers = append(out.Reviewers, "security")
	for _, f := range s.Findings {
		f.By = store.KindSecurity
		out.Findings = append(out.Findings, f)
	}
	out.ChallengedAssumptions = append(out.ChallengedAssumptions, s.ChallengedAssumptions...)
	out.Earlier = slices.DeleteFunc(out.Earlier, func(e verdict.Ruling) bool { return ours(e.ID) })
	for _, e := range s.Earlier {
		if ours(e.ID) {
			out.Earlier = append(out.Earlier, e)
		}
	}
	ok = true
	if s.BlockedWithoutFinding || (s.Decision == "block" && len(s.Findings) == 0) {
		ok, out.BlockedWithoutFinding = false, true
	}
	// Questions from either side go to a human, with every finding: a
	// block isn't verified away from under them.
	switch {
	case v.Decision == "questions" || s.Decision == "questions":
		out.Decision = "questions"
	case v.Decision == "block" || s.Decision == "block":
		out.Decision = "block"
	}
	return out, ok
}
