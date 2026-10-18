package pipeline

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/claptimes5/factory/internal/github"
	"github.com/claptimes5/factory/internal/state"
	"github.com/claptimes5/factory/internal/store"
	"github.com/claptimes5/factory/internal/verdict"
)

// Backlog grooming: every grooming.every, a read-only groom job per repo
// reads the open issues the factory isn't working on, fenced as untrusted,
// and proposes duplicates, stale issues and splits. The daemon checks each
// proposal against what the job was shown, then comments once on each
// issue it flags and labels it. It never closes, edits or splits anything
// itself: someone with write access confirms with factory:close or
// factory:split, checked as an approval is, or dismisses by taking the
// question-mark label off.

// Labels grooming puts on issues, and the ones a human confirms with.
const (
	LabelDuplicateQ = "factory:duplicate?"
	LabelStaleQ     = "factory:stale?"
	LabelSplitQ     = "factory:split?"
	LabelSplit      = "factory:split" // a human confirms a split
	LabelClose      = "factory:close" // a human confirms a duplicate or stale issue
)

// Limits on a groom run.
const (
	MaxGroomFlags = 10   // issues one run flags at most
	groomRead     = 1000 // open issues read from the forge
	groomBody     = 1500 // runes of each issue's body the job reads
	groomBusyMax  = 100  // issues listed as the factory's own, by title
)

// markGroomDue is when the grooming cadence last fired.
const markGroomDue = "grooming.due"

// GroomInput is carried by a groom job: the repo it grooms.
type GroomInput struct {
	Repo string `json:"repo"`
}

// ErrGroomQueued is returned when a repo's groom job is already queued or
// running.
var ErrGroomQueued = errors.New("a grooming run for that repo is already queued or running")

// groomLabel is the label a proposal of a kind puts on its issue.
func groomLabel(kind string) string {
	switch kind {
	case store.GroomDuplicate:
		return LabelDuplicateQ
	case store.GroomStale:
		return LabelStaleQ
	}
	return LabelSplitQ
}

// confirmLabel is the label that confirms a proposal of a kind.
func confirmLabel(kind string) string {
	if kind == store.GroomSplit {
		return LabelSplit
	}
	return LabelClose
}

// openIssueLister is a client that lists all open issues; both real ones
// can.
type openIssueLister interface {
	OpenIssues(ctx context.Context, max int) ([]github.OpenIssue, error)
}

// commentLister is a client that lists an issue's comments and says who
// the factory is; both real ones can.
type commentLister interface {
	IssueComments(ctx context.Context, n int) ([]github.Comment, error)
	Self(ctx context.Context) (string, error)
}

// issueDetailer reads one issue with its age and linked PRs; both real
// clients can.
type issueDetailer interface {
	IssueDetail(ctx context.Context, n int) (*github.OpenIssue, error)
}

// CheckGroomPrompt fails when grooming is on and its prompt is missing.
func (p *Pipeline) CheckGroomPrompt() error {
	if p.Cfg.Grooming.Every <= 0 {
		return nil
	}
	if _, err := os.Stat(filepath.Join(p.Cfg.Path(p.Cfg.PromptsDir), "groom.md")); err != nil {
		return fmt.Errorf("grooming is on (grooming.every: %s) but its prompt is missing: %w; restore it, or set grooming.every: 0", p.Cfg.Grooming.Every, err)
	}
	return nil
}

// Groom queues a groom job for a repo, whatever the cadence. At most one
// per repo is queued or running.
func (p *Pipeline) Groom(ctx context.Context, repo, reason string) (int64, error) {
	rc, ok := p.Cfg.RepoConf(repo)
	if !ok {
		return 0, fmt.Errorf("%s isn't one of the factory's repos (%s)", repo, strings.Join(p.Cfg.RepoNames(), ", "))
	}
	r, ok := p.Cfg.Routing["groom"]
	if !ok {
		return 0, errors.New("routing.groom is missing (it defaults from routing.triage)")
	}
	b, _ := json.Marshal(GroomInput{Repo: rc.Name})
	fast := r.FastFor(r.Harness, r.Model)
	id, queued, err := p.Store.EnqueueSoleFor(ctx, store.Job{Kind: "groom", Harness: r.Harness, Model: r.Model, Effort: r.Effort, Fast: fast,
		Skillset: r.Skills, Route: "groom", Priority: -1, BudgetUSD: p.Cfg.FastBudget(p.Cfg.GroomBudget(), fast), Input: string(b)}, rc.Name)
	if err != nil {
		return 0, err
	}
	if !queued {
		return 0, ErrGroomQueued
	}
	p.warn(p.Store.AddEvent(ctx, store.Event{Actor: "daemon", JobID: &id, Type: "groom_queued", Reason: rc.Name + ": " + reason}), "AddEvent")
	return id, nil
}

// groomBudget says why a groom run can't spend now, or "". It counts as
// building, like learn.
func (p *Pipeline) groomBudget(ctx context.Context) string {
	spent, err := p.buildSpend(ctx)
	switch {
	case err != nil:
		return "today's spend can't be read (" + err.Error() + ")"
	case spent >= p.Cfg.Budgets.DailyTotal:
		return fmt.Sprintf("today's budget is spent ($%.2f of $%.2f)", spent, p.Cfg.Budgets.DailyTotal)
	}
	return ""
}

// groomTrigger runs on each tick: once grooming.every has passed since the
// last run, it queues a groom job per repo. The clock is kept in the store
// like tuning's; over budget, the run waits for budget, said once a day.
func (p *Pipeline) groomTrigger(ctx context.Context, now time.Time) {
	every := p.Cfg.Grooming.Every
	if every <= 0 {
		return
	}
	if p.groomDue.IsZero() {
		at, err := p.Store.Marked(ctx, markGroomDue)
		if err != nil {
			return
		}
		t, err := time.Parse(store.TimeFormat, at)
		if err != nil || t.After(now) || now.Sub(t) > 2*every {
			// The first run is one cadence after grooming is turned on.
			t = now
			if err := p.Store.Mark(ctx, markGroomDue, now.UTC().Format(store.TimeFormat)); err != nil {
				return
			}
		}
		p.groomDue = t
	}
	if now.Sub(p.groomDue) < every {
		return
	}
	if why := p.groomBudget(ctx); why != "" {
		if day := now.UTC().Format("2006-01-02"); p.groomHeld != day {
			p.groomHeld = day
			p.notify(ctx, "Backlog grooming is due, but "+why+". It runs once there's budget again.")
		}
		return
	}
	if err := p.Store.Mark(ctx, markGroomDue, now.UTC().Format(store.TimeFormat)); err != nil {
		p.warn(err, "groom")
		return
	}
	p.groomDue = now
	for _, repo := range p.Cfg.GroomRepos() {
		if _, err := p.Groom(ctx, repo, "scheduled run"); !errors.Is(err, ErrGroomQueued) {
			p.warn(err, "groom")
		}
	}
}

// --- gathering ---

// GroomPrompt is what prompts/groom.md is given.
type GroomPrompt struct {
	Repo, Today        string
	StaleDays          int
	MinSplit, MaxSplit int
	Issues             []GroomPromptIssue
	Busy               []GroomPromptIssue // by number and title only
	Fence              string
}

// GroomPromptIssue is one issue in the groom job's input.
type GroomPromptIssue struct {
	N              int
	Title          string
	Opened, Active string // dates
	Idle           bool   // no activity for grooming.stale_after
	Labels         string
	Body           string
}

// groomSet is what a groom job reads, and what it may flag.
type groomSet struct {
	byNum      map[int]github.OpenIssue
	candidates []github.OpenIssue
	scope      verdict.GroomScope
	prompt     GroomPrompt
}

// groomBusy says whether grooming must leave an issue alone: the factory
// has an item for it, an open PR would close it, or it carries a factory
// label (in progress, filed by a source or main health, an epic, or
// flagged already). It may still be a duplicate's canonical issue.
func (p *Pipeline) groomBusy(ctx context.Context, repo string, is github.OpenIssue) (bool, error) {
	if is.LinkedPRs > 0 || hasLabel(is.Labels, p.Cfg.Intake.Label) {
		return true, nil
	}
	for _, l := range is.Labels {
		if strings.HasPrefix(strings.ToLower(l), "factory:") {
			return true, nil
		}
	}
	_, err := p.Store.ItemByIssue(ctx, repo, is.Number)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// gatherGroom sorts a repo's open issues into those the job may flag, at
// most grooming.max_issues (half the newest, half the longest idle), and
// those it only sees by title.
func (p *Pipeline) gatherGroom(ctx context.Context, repo string, open []github.OpenIssue, now time.Time) (*groomSet, error) {
	g := &groomSet{byNum: map[int]github.OpenIssue{}, scope: verdict.GroomScope{Flaggable: map[int]bool{}, Old: map[int]bool{}, Known: map[int]bool{}, NotCanonical: map[int]bool{}}}
	props, err := p.Store.OpenGroomProposals(ctx, repo)
	if err != nil {
		return nil, err
	}
	flagged := map[int]bool{}
	for _, pr := range props {
		flagged[pr.IssueNumber] = true
		if pr.Kind != store.GroomSplit {
			g.scope.NotCanonical[pr.IssueNumber] = true // proposed closed already
		}
	}
	children, err := p.Store.GroomChildren(ctx, repo, 0)
	if err != nil {
		return nil, err
	}
	for _, c := range children {
		flagged[c.Issue] = true // the factory's own split, waiting for approval
	}
	var free, busy []github.OpenIssue
	for _, is := range open {
		if is.IsPR {
			continue
		}
		g.byNum[is.Number] = is
		b, err := p.groomBusy(ctx, repo, is)
		if err != nil {
			return nil, err
		}
		if b || flagged[is.Number] {
			busy = append(busy, is)
		} else {
			free = append(free, is)
		}
	}
	max := p.Cfg.Grooming.MaxIssues
	if len(free) > max {
		sort.SliceStable(free, func(a, b int) bool { return free[a].CreatedAt.After(free[b].CreatedAt) })
		pick := append([]github.OpenIssue{}, free[:max/2]...)
		rest := append([]github.OpenIssue{}, free[max/2:]...)
		sort.SliceStable(rest, func(a, b int) bool { return rest[a].UpdatedAt.Before(rest[b].UpdatedAt) })
		free = append(pick, rest[:max-len(pick)]...)
	}
	sort.SliceStable(free, func(a, b int) bool { return free[a].Number < free[b].Number })
	g.candidates = free
	sort.SliceStable(busy, func(a, b int) bool { return busy[a].CreatedAt.After(busy[b].CreatedAt) })
	if len(busy) > groomBusyMax {
		busy = busy[:groomBusyMax]
	}
	sort.SliceStable(busy, func(a, b int) bool { return busy[a].Number < busy[b].Number })

	stale := p.Cfg.Grooming.StaleAfter
	day := func(t time.Time) string {
		if t.IsZero() {
			return "unknown"
		}
		return fmt.Sprintf("%s (%d days ago)", t.UTC().Format("2006-01-02"), int(now.Sub(t).Hours()/24))
	}
	d := GroomPrompt{Repo: repo, Today: now.UTC().Format("2006-01-02"), StaleDays: int(stale.Hours() / 24), MinSplit: verdict.MinSplit, MaxSplit: verdict.MaxSplit}
	var all strings.Builder
	for _, is := range free {
		old := !is.UpdatedAt.IsZero() && now.Sub(is.UpdatedAt) >= stale
		g.scope.Flaggable[is.Number], g.scope.Known[is.Number] = true, true
		if old {
			g.scope.Old[is.Number] = true
		}
		pi := GroomPromptIssue{N: is.Number, Title: clip(is.Title, 200), Opened: day(is.CreatedAt), Active: day(is.UpdatedAt), Idle: old,
			Labels: clip(strings.Join(is.Labels, ", "), 300), Body: capRunes(strings.TrimSpace(is.Body), groomBody)}
		if pi.Body == "" {
			pi.Body = "(no description)"
		}
		d.Issues = append(d.Issues, pi)
		all.WriteString(pi.Title + pi.Labels + pi.Body)
	}
	for _, is := range busy {
		g.scope.Known[is.Number] = true
		pi := GroomPromptIssue{N: is.Number, Title: clip(is.Title, 200)}
		d.Busy = append(d.Busy, pi)
		all.WriteString(pi.Title)
	}
	d.Fence = fence(all.String())
	g.prompt = d
	return g, nil
}

// capRunes keeps at most n runes of s, saying when it cut.
func capRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "\n… (cut)"
	}
	return s
}

// --- the groom job ---

func (p *Pipeline) runGroom(ctx context.Context, j *store.Job) error {
	dir := p.jobDir(j.ID)
	out := filepath.Join(dir, "out")
	var in GroomInput
	_ = json.Unmarshal([]byte(j.Input), &in)
	fail := func(run agentRun, st state.JobState, msg string) error {
		if p.stopIfCancelled(ctx, j, nil, run) {
			return nil
		}
		p.finish(ctx, j, run, st, "", msg)
		if in.Repo != "" {
			p.warn(p.Store.SetGroomRun(ctx, store.GroomRun{Repo: in.Repo, JobID: &j.ID, Note: "failed: " + truncate(firstLine(msg), 200)}), "SetGroomRun")
		}
		return nil // the next cadence tries again
	}
	rc, ok := p.Cfg.RepoConf(in.Repo)
	if !ok {
		return fail(agentRun{}, state.Failed, fmt.Sprintf("bad job input: %q isn't a configured repo", in.Repo))
	}
	in.Repo = rc.Name
	lister, ok := p.gh(in.Repo).(openIssueLister)
	if !ok {
		return fail(agentRun{}, state.Failed, in.Repo+"'s client can't list open issues")
	}
	var open []github.OpenIssue
	if err := p.retried(ctx, func() error {
		var err error
		open, err = lister.OpenIssues(ctx, groomRead)
		return err
	}); err != nil {
		return fail(agentRun{}, state.Failed, "listing open issues: "+err.Error())
	}
	g, err := p.gatherGroom(ctx, in.Repo, open, time.Now())
	if err != nil {
		return fail(agentRun{}, state.Failed, err.Error())
	}
	if len(g.candidates) == 0 {
		p.finish(ctx, j, agentRun{}, state.Succeeded, `{"skipped":"no open issues to groom"}`, "")
		p.warn(p.Store.SetGroomRun(ctx, store.GroomRun{Repo: in.Repo, JobID: &j.ID, Note: "no open issues to groom"}), "SetGroomRun")
		return nil
	}
	prompt, err := p.renderStep(j, "groom", []string{in.Repo}, g.prompt)
	if err != nil {
		return fail(agentRun{}, state.Failed, err.Error())
	}
	// It reads only what the prompt gives it: an empty directory to run in.
	work := filepath.Join(dir, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		return fail(agentRun{}, state.Failed, err.Error())
	}
	schema, _ := verdict.Schema("groom")
	// No repo: it needs none of the repo's env, and gets the default image.
	run := p.runAgentWith(ctx, j, work, out, prompt, schema, p.Cfg.Timeout("groom"), agentExtras{})
	if p.stopIfCancelled(ctx, j, nil, run) {
		return nil
	}
	if st, msg, bad := runFailure(run); bad {
		return fail(run, st, msg)
	}
	var v verdict.Groom
	if err := verdict.Parse([]byte(run.final), &v); err != nil {
		return fail(run, state.Failed, err.Error())
	}
	kept, dropped := v.Sift(g.scope)
	p.act.Lock()
	counts := p.flagIssues(ctx, j, in.Repo, g, kept)
	p.act.Unlock()
	if dropped == nil {
		dropped = []string{}
	}
	norm, _ := json.Marshal(map[string]any{"kept": kept, "dropped": dropped})
	p.finish(ctx, j, run, state.Succeeded, string(norm), "")
	n := counts[store.GroomDuplicate] + counts[store.GroomStale] + counts[store.GroomSplit]
	note := ""
	if len(dropped) > 0 {
		note = fmt.Sprintf("%d %s dropped", len(dropped), plural(len(dropped), "proposal", "proposals"))
	}
	p.warn(p.Store.SetGroomRun(ctx, store.GroomRun{Repo: in.Repo, JobID: &j.ID, Read: len(g.candidates), Flagged: n, Note: note}), "SetGroomRun")
	if n > 0 {
		var parts []string
		for _, k := range []string{store.GroomDuplicate, store.GroomStale, store.GroomSplit} {
			if c := counts[k]; c > 0 {
				parts = append(parts, fmt.Sprintf("%d %s", c, groomNoun(k, c)))
			}
		}
		p.notify(ctx, fmt.Sprintf("Backlog grooming flagged %d %s in %s: %s. Each has a comment; add `%s` or `%s` to act on one, or take its question-mark label off to dismiss it.",
			n, plural(n, "issue", "issues"), in.Repo, strings.Join(parts, ", "), LabelClose, LabelSplit))
	}
	return nil
}

func groomNoun(kind string, n int) string {
	switch kind {
	case store.GroomDuplicate:
		return plural(n, "likely duplicate", "likely duplicates")
	case store.GroomStale:
		return "likely stale"
	}
	return "to split"
}

// groomFlag is one issue to flag.
type groomFlag struct {
	kind      string
	issue     int
	canonical int
	why       string
	children  []verdict.GroomChild
}

// groomKey names what a proposal proposes, apart from its wording: the
// kind, a duplicate's canonical issue, a split's titles.
func groomKey(f groomFlag) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\x00%d", f.kind, f.canonical)
	for _, c := range f.children {
		b.WriteString("\x00" + strings.ToLower(oneLine(c.Title)))
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:6])
}

// groomMarker ends a flag's comment: the proposal, and the issue's text
// it was made for, so a comment the record lost is found again, but not
// one made for text the issue no longer has.
func groomMarker(kind, key, hash string) string {
	if len(hash) > 12 {
		hash = hash[:12]
	}
	return fmt.Sprintf("<!-- factory-groom: %s %s %s -->", kind, key, hash)
}

// flagIssues comments on and labels each issue the verdict kept, at most
// MaxGroomFlags, and counts them by kind. Called with p.act held.
func (p *Pipeline) flagIssues(ctx context.Context, j *store.Job, repo string, g *groomSet, v verdict.Groom) map[string]int {
	var flags []groomFlag
	for _, d := range v.Duplicates {
		for _, n := range d.Duplicates {
			flags = append(flags, groomFlag{kind: store.GroomDuplicate, issue: n, canonical: d.Canonical, why: d.Why})
		}
	}
	for _, s := range v.Stale {
		flags = append(flags, groomFlag{kind: store.GroomStale, issue: s.Issue, why: s.Why})
	}
	for _, t := range v.TooBig {
		flags = append(flags, groomFlag{kind: store.GroomSplit, issue: t.Issue, why: t.Why, children: t.Split})
	}
	counts := map[string]int{}
	n := 0
	for _, f := range flags {
		if n >= MaxGroomFlags {
			break
		}
		hash := IssueHash(g.byNum[f.issue].Issue)
		key := groomKey(f)
		seen, err := p.groomSeen(ctx, repo, f, key, hash)
		if err != nil {
			p.warn(err, "grooming")
			continue
		}
		if seen {
			continue
		}
		// The job may have run for minutes: flag the issue only if it
		// still reads as the job saw it, and nobody has taken it up.
		if why := p.groomStillFree(ctx, repo, f.issue, hash); why != "" {
			p.log().Info("grooming: not flagging", "repo", repo, "issue", f.issue, "why", why)
			continue
		}
		// With no open record for it (a lost database, or a failure after
		// the comment), the factory's marker still says it was flagged.
		repair := false
		switch p.groomCommented(ctx, repo, f.issue, groomMarker(f.kind, key, hash)) {
		case markerUnsure:
			continue // flag it next time rather than twice
		case markerFound:
			repair = true // put the record and label back, without a second comment
		}
		if !p.postFlag(ctx, j, repo, f, key, hash, repair) {
			continue
		}
		if !repair {
			counts[f.kind]++
			n++
		}
	}
	return counts
}

// postFlag records a proposal, then comments (unless repairing one already
// posted) and labels. Should either fail, the record goes, so nothing is
// left half done: a comment without its label is put right by the next
// run, which finds the marker.
func (p *Pipeline) postFlag(ctx context.Context, j *store.Job, repo string, f groomFlag, key, hash string, repair bool) bool {
	gh := p.gh(repo)
	var children []store.GroomChild
	for _, c := range f.children {
		children = append(children, store.GroomChild{Title: c.Title, Body: c.Body})
	}
	id, err := p.Store.AddGroomProposal(ctx, store.GroomProposal{Repo: repo, IssueNumber: f.issue, Kind: f.kind, Key: key, IssueHash: hash,
		Canonical: f.canonical, Why: f.why, Children: children, JobID: &j.ID})
	if err != nil {
		p.warn(err, "AddGroomProposal")
		return false
	}
	undo := func(err error, what string) bool {
		p.warn(err, what)
		p.warn(p.Store.DeleteGroomProposal(ctx, id), "DeleteGroomProposal")
		return false
	}
	if !repair {
		if err := gh.Comment(ctx, f.issue, p.groomComment(f, key, hash)); err != nil {
			return undo(err, "grooming: comment")
		}
	}
	if err := gh.AddLabels(ctx, f.issue, groomLabel(f.kind)); err != nil {
		return undo(err, "grooming: label")
	}
	typ := "groom_flagged"
	if repair {
		typ = "groom_repaired"
	}
	b, _ := json.Marshal(map[string]any{"issue": f.issue, "kind": f.kind, "canonical": f.canonical})
	p.warn(p.Store.AddEvent(ctx, store.Event{Actor: "daemon", JobID: &j.ID, Type: typ, Reason: repo, Data: string(b)}), "AddEvent")
	return true
}

// issueNow reads an issue afresh: with its linked PRs when the client
// can say, else as GetIssue reads it.
func (p *Pipeline) issueNow(ctx context.Context, repo string, n int) (*github.OpenIssue, error) {
	gh := p.gh(repo)
	if d, ok := gh.(issueDetailer); ok {
		return d.IssueDetail(ctx, n)
	}
	is, err := gh.GetIssue(ctx, n)
	if err != nil {
		return nil, err
	}
	return &github.OpenIssue{Issue: *is}, nil
}

// groomStillFree says why an issue may no longer be flagged, or "": it
// closed, its text changed since hash was taken, or it's busy now.
func (p *Pipeline) groomStillFree(ctx context.Context, repo string, n int, hash string) string {
	is, err := p.issueNow(ctx, repo, n)
	switch {
	case err != nil:
		return "can't read it: " + err.Error()
	case is.State == "closed":
		return "it closed"
	case IssueHash(is.Issue) != hash:
		return "it changed since the job read it"
	}
	if busy, err := p.groomBusy(ctx, repo, *is); err != nil || busy {
		return "the factory, a PR or a label has taken it up"
	}
	return ""
}

// groomSeen says whether a flag was made before: the issue has an open
// proposal, a human dismissed one of this kind while the issue read as it
// does now, or this very proposal was made for this text before.
func (p *Pipeline) groomSeen(ctx context.Context, repo string, f groomFlag, key, hash string) (bool, error) {
	props, err := p.Store.GroomProposals(ctx, repo, f.issue)
	if err != nil {
		return false, err
	}
	for _, g := range props {
		if g.Status == store.GroomOpen {
			return true, nil
		}
		if g.Kind == f.kind && g.IssueHash == hash && (g.Status == store.GroomDismissed || g.Key == key) {
			return true, nil
		}
	}
	return false, nil
}

// markerSeen is what an issue's comments say about a marker.
type markerSeen int

const (
	markerNone   markerSeen = iota
	markerFound             // in a comment by the factory itself
	markerUnsure            // the comments, or who the factory is, can't be read
)

// groomCommented looks for the marker at the end of one of the factory's
// own comments on the issue: anyone else's doesn't count, or an outsider
// could keep an issue from ever being flagged.
func (p *Pipeline) groomCommented(ctx context.Context, repo string, n int, marker string) markerSeen {
	cl, ok := p.gh(repo).(commentLister)
	if !ok {
		return markerNone
	}
	self, err := cl.Self(ctx)
	if err != nil || self == "" {
		p.warn(err, "grooming: who the factory is")
		return markerUnsure
	}
	cs, err := cl.IssueComments(ctx, n)
	if err != nil {
		p.warn(err, "grooming: comments")
		return markerUnsure
	}
	for _, c := range cs {
		if !strings.EqualFold(c.Author, self) {
			continue
		}
		b := strings.TrimSpace(c.Body)
		if b[strings.LastIndex(b, "\n")+1:] == marker {
			return markerFound
		}
	}
	return markerNone
}

// quoted puts checked model text in a blockquote, line by line.
func quoted(s string) string {
	lines := strings.Split(strings.TrimSpace(strings.ReplaceAll(s, "\r\n", "\n")), "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight("> "+l, " ")
	}
	return strings.Join(lines, "\n")
}

// groomComment is the comment on a flagged issue. The daemon's words say
// what to do; the model's, checked by Sift, are quoted.
func (p *Pipeline) groomComment(f groomFlag, key, hash string) string {
	var b strings.Builder
	q := groomLabel(f.kind)
	switch f.kind {
	case store.GroomDuplicate:
		fmt.Fprintf(&b, "**Factory backlog grooming: this looks like a duplicate of #%d.**\n\n%s\n\n", f.canonical, quoted(f.why))
		fmt.Fprintf(&b, "If it is, someone with write access can add `%s`: the factory closes this issue with a note pointing to #%d. ", LabelClose, f.canonical)
	case store.GroomStale:
		fmt.Fprintf(&b, "**Factory backlog grooming: this may be stale.** It has had no activity for %d days.\n\n%s\n\n", int(p.Cfg.Grooming.StaleAfter.Hours()/24), quoted(f.why))
		fmt.Fprintf(&b, "If it's no longer needed, someone with write access can add `%s`: the factory closes it with a note. ", LabelClose)
	default:
		fmt.Fprintf(&b, "**Factory backlog grooming: this may be too big for one change.**\n\n%s\n\nIt could be split into:\n\n", quoted(f.why))
		for i, c := range f.children {
			fmt.Fprintf(&b, "%d. **%s**\n\n%s\n\n", i+1, oneLine(c.Title), indent(quoted(c.Body), "   "))
		}
		fmt.Fprintf(&b, "To split it, someone with write access adds `%s`: the factory opens these %d issues, each waiting for `%s` like any other, and makes this issue their epic. ",
			LabelSplit, len(f.children), p.Cfg.Intake.Label)
	}
	fmt.Fprintf(&b, "If not, take `%s` off and the factory won't suggest this again unless the issue changes. The factory never closes, edits or splits an issue on its own.\n\n%s\n", q, groomMarker(f.kind, key, hash))
	return b.String()
}

func indent(s, by string) string {
	return by + strings.ReplaceAll(s, "\n", "\n"+by)
}

// --- confirming and dismissing ---

// How often the daemon looks at the labels of issues with open proposals,
// and at sub-issues nobody has approved yet.
const (
	groomPollEvery  = 10 * time.Minute
	groomChildEvery = 6 * time.Hour
)

// groomActions runs on each tick, but looks at the forge at most every
// groomPollEvery, and only while proposals are open: a confirming label
// acts on its proposal, a question-mark label taken off dismisses it, and
// an issue the factory has taken up drops its proposal. Every
// groomChildEvery it also forgets sub-issues that closed before anyone
// approved them.
func (p *Pipeline) groomActions(ctx context.Context, now time.Time) {
	if now.Sub(p.groomPollAt) < groomPollEvery {
		return
	}
	p.groomPollAt = now
	open, err := p.Store.OpenGroomProposals(ctx, "")
	if err != nil {
		p.warn(err, "grooming")
		return
	}
	children := now.Sub(p.groomChildAt) >= groomChildEvery
	if len(open) == 0 && !children {
		return
	}
	p.act.Lock()
	defer p.act.Unlock()
	byRepo := map[string][]store.GroomProposal{}
	var repos []string
	for _, g := range open {
		if _, ok := byRepo[g.Repo]; !ok {
			repos = append(repos, g.Repo)
		}
		byRepo[g.Repo] = append(byRepo[g.Repo], g)
	}
	for _, repo := range repos {
		if err := p.groomRepoActions(ctx, repo, byRepo[repo]); err != nil {
			p.log().Warn("grooming", "repo", repo, "err", err)
		}
	}
	if children {
		p.groomChildAt = now
		p.settleGroomChildren(ctx)
	}
}

func (p *Pipeline) groomRepoActions(ctx context.Context, repo string, props []store.GroomProposal) error {
	gh := p.gh(repo)
	labels := []string{LabelSplit, LabelClose}
	for _, g := range props {
		if l := groomLabel(g.Kind); !slices.Contains(labels, l) {
			labels = append(labels, l)
		}
	}
	on := map[string]map[int]bool{}
	for _, l := range labels {
		is, err := gh.ListIssues(ctx, l)
		if err != nil {
			return err
		}
		on[l] = map[int]bool{}
		for _, i := range is {
			if i.State != "closed" {
				on[l][i.Number] = true
			}
		}
	}
	for _, g := range props {
		n := g.IssueNumber
		confirmed := on[confirmLabel(g.Kind)][n]
		// Taken up since it was flagged (approved, or filed by a source):
		// the proposal no longer applies.
		if _, err := p.Store.ItemByIssue(ctx, repo, n); !errors.Is(err, sql.ErrNoRows) {
			if err != nil {
				p.warn(err, "grooming")
				continue
			}
			why := ""
			if confirmed {
				why = "the factory has since taken this issue up, so the grooming proposal no longer applies"
			}
			p.warn(p.dropGroom(ctx, g, confirmed, store.GroomGone, "taken up", why), "grooming")
			continue
		}
		switch {
		case confirmed:
			p.warn(p.confirmGroom(ctx, g), "grooming: confirm")
		case !on[groomLabel(g.Kind)][n]:
			is, err := gh.GetIssue(ctx, n)
			switch {
			case github.IsNotFound(err) || err == nil && is.State == "closed":
				p.warn(p.Store.DecideGroomProposal(ctx, g.ID, store.GroomGone, "the issue closed"), "DecideGroomProposal")
			case err != nil:
				p.warn(err, "grooming: GetIssue")
			case !hasLabel(is.Labels, groomLabel(g.Kind)):
				p.warn(p.Store.DecideGroomProposal(ctx, g.ID, store.GroomDismissed, "human"), "DecideGroomProposal")
				p.warn(p.Store.AddEvent(ctx, store.Event{Actor: "human", Type: "groom_dismissed", Reason: repo,
					Data: fmt.Sprintf(`{"issue":%d,"kind":%q}`, n, g.Kind)}), "AddEvent")
			}
		}
	}
	return nil
}

// dropGroom ends a proposal without acting on it: its question-mark label
// comes off, and the confirming label too when it was added, with a
// comment saying why the factory isn't acting (when why isn't "").
func (p *Pipeline) dropGroom(ctx context.Context, g store.GroomProposal, confirmed bool, status, by, why string) error {
	gh := p.gh(g.Repo)
	if confirmed {
		if err := gh.RemoveLabel(ctx, g.IssueNumber, confirmLabel(g.Kind)); err != nil {
			return err // tried again on the next poll
		}
	}
	p.warn(gh.RemoveLabel(ctx, g.IssueNumber, groomLabel(g.Kind)), "RemoveLabel")
	if why != "" {
		p.warn(gh.Comment(ctx, g.IssueNumber, fmt.Sprintf("**Factory: not acting on `%s`.** %s.", confirmLabel(g.Kind), upperFirst(why))), "Comment")
	}
	p.warn(p.Store.AddEvent(ctx, store.Event{Actor: "daemon", Type: "groom_withdrawn", Reason: g.Repo,
		Data: fmt.Sprintf(`{"issue":%d,"kind":%q,"why":%q}`, g.IssueNumber, g.Kind, by)}), "AddEvent")
	return p.Store.DecideGroomProposal(ctx, g.ID, status, by)
}

func upperFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// groomRefusal words approvedFor's comment for a confirming label.
func groomRefusal(kind string) labelRefusal {
	if kind == store.GroomSplit {
		return labelRefusal{"not splitting this issue", "confirm a split", "split it", "split it"}
	}
	return labelRefusal{"not closing this issue", "confirm closing it", "close it", "close it"}
}

// confirmGroom acts on a proposal someone confirmed, once their label is
// an approval: write access, and the issue unchanged since they added it,
// and since grooming flagged it.
func (p *Pipeline) confirmGroom(ctx context.Context, g store.GroomProposal) error {
	who, text, ok := p.approvedFor(ctx, g.Repo, g.IssueNumber, confirmLabel(g.Kind), groomRefusal(g.Kind))
	if !ok {
		return nil // it said why, and took the label off
	}
	if IssueHash(text) != g.IssueHash {
		return p.dropGroom(ctx, g, true, store.GroomGone, "changed",
			fmt.Sprintf("the issue's title or text changed since backlog grooming flagged it, so the proposal may no longer fit. Take `%s` off; the next grooming run looks at it afresh", groomLabel(g.Kind)))
	}
	if g.Kind == store.GroomSplit {
		return p.groomSplit(ctx, g, who, text)
	}
	return p.groomClose(ctx, g, who)
}

// groomCloseBlocked says why a confirmed close mustn't happen now, or "":
// an open PR would close the issue, or, for a duplicate, its canonical
// issue closed or is itself proposed closed.
func (p *Pipeline) groomCloseBlocked(ctx context.Context, g store.GroomProposal) (string, error) {
	is, err := p.issueNow(ctx, g.Repo, g.IssueNumber)
	if err != nil {
		return "", err
	}
	if is.LinkedPRs > 0 {
		return "an open pull request would close this issue, so it's being worked on", nil
	}
	if g.Kind != store.GroomDuplicate {
		return "", nil
	}
	c, err := p.issueNow(ctx, g.Repo, g.Canonical)
	switch {
	case github.IsNotFound(err):
		return fmt.Sprintf("#%d, which this duplicates, can't be found", g.Canonical), nil
	case err != nil:
		return "", err
	case c.State == "closed":
		return fmt.Sprintf("#%d, which this duplicates, is closed now, so this issue may be the one left to track it", g.Canonical), nil
	}
	props, err := p.Store.GroomProposals(ctx, g.Repo, g.Canonical)
	if err != nil {
		return "", err
	}
	for _, o := range props {
		if o.Status == store.GroomOpen && o.Kind != store.GroomSplit {
			return fmt.Sprintf("#%d, which this duplicates, is itself proposed closed, and closing both would lose the issue", g.Canonical), nil
		}
	}
	return "", nil
}

// groomClose closes a confirmed duplicate or stale issue, then says why.
func (p *Pipeline) groomClose(ctx context.Context, g store.GroomProposal, who string) error {
	why, err := p.groomCloseBlocked(ctx, g)
	if err != nil {
		return err // the label stays: tried again on the next poll
	}
	if why != "" {
		return p.dropGroom(ctx, g, true, store.GroomGone, "blocked", why)
	}
	gh := p.gh(g.Repo)
	switch c := gh.(type) {
	case notPlannedCloser:
		err = c.CloseNotPlanned(ctx, g.IssueNumber)
	case issueCloser:
		err = c.CloseIssue(ctx, g.IssueNumber)
	default:
		err = fmt.Errorf("%s's client can't close issues", g.Repo)
	}
	if err != nil {
		return err
	}
	note := fmt.Sprintf("**Factory:** closing this as stale, as @%s confirmed with `%s`. Reopen it if it still matters.", who, LabelClose)
	if g.Kind == store.GroomDuplicate {
		note = fmt.Sprintf("**Factory:** closing this as a duplicate of #%d, as @%s confirmed with `%s`. Follow #%d for it.", g.Canonical, who, LabelClose, g.Canonical)
	}
	p.warn(gh.Comment(ctx, g.IssueNumber, note), "Comment")
	p.warn(gh.RemoveLabel(ctx, g.IssueNumber, LabelClose), "RemoveLabel")
	p.warn(gh.RemoveLabel(ctx, g.IssueNumber, groomLabel(g.Kind)), "RemoveLabel")
	p.warn(p.Store.AddEvent(ctx, store.Event{Actor: "human", Type: "groom_closed", Reason: g.Repo,
		Data: fmt.Sprintf(`{"issue":%d,"kind":%q,"canonical":%d,"by":%q}`, g.IssueNumber, g.Kind, g.Canonical, who)}), "AddEvent")
	p.log().Info("grooming closed an issue", "repo", g.Repo, "issue", g.IssueNumber, "kind", g.Kind, "by", who)
	return p.Store.DecideGroomProposal(ctx, g.ID, store.GroomDone, who)
}

// childFiling marks a sub-issue whose filing began: it may exist on the
// forge without its number on record.
const childFiling = -1

func groomChildMarker(parent, i int) string {
	return fmt.Sprintf("<!-- factory-groom-child: %d %d -->", parent, i)
}

// filedChild finds a sub-issue the factory filed before a failure kept
// its number from the record: an open issue by the factory whose body
// ends with the marker. 0 when there's none.
func (p *Pipeline) filedChild(ctx context.Context, repo, marker string) (int, error) {
	gh := p.gh(repo)
	lister, ok1 := gh.(openIssueLister)
	cl, ok2 := gh.(commentLister)
	if !ok1 || !ok2 {
		return 0, fmt.Errorf("%s's client can't look for sub-issues it filed", repo)
	}
	self, err := cl.Self(ctx)
	if err != nil {
		return 0, err
	}
	open, err := lister.OpenIssues(ctx, groomRead)
	if err != nil {
		return 0, err
	}
	for _, is := range open {
		b := strings.TrimSpace(is.Body)
		if strings.EqualFold(is.Author, self) && b[strings.LastIndex(b, "\n")+1:] == marker {
			return is.Number, nil
		}
	}
	return 0, nil
}

// groomSplit files a confirmed split's sub-issues, each waiting for
// approval, and makes the issue their epic. Progress is saved before each
// sub-issue is filed, so one filed before a failure is found again by its
// marker rather than filed twice.
func (p *Pipeline) groomSplit(ctx context.Context, g store.GroomProposal, who string, text github.Issue) error {
	gh := p.gh(g.Repo)
	children := g.Children
	for i := range children {
		if children[i].Issue > 0 {
			continue
		}
		marker := groomChildMarker(g.IssueNumber, i+1)
		num := 0
		if children[i].Issue == childFiling {
			n, err := p.filedChild(ctx, g.Repo, marker)
			if err != nil {
				return err // the label stays: tried again on the next poll
			}
			num = n
		} else {
			children[i].Issue = childFiling
			if err := p.Store.SetGroomChildren(ctx, g.ID, children); err != nil {
				return err
			}
		}
		if num == 0 {
			body := strings.TrimSpace(children[i].Body) + fmt.Sprintf("\n\n_Part of #%d, split from it by the factory's backlog grooming, as %s confirmed._\n\n", g.IssueNumber, who) +
				fmt.Sprintf("**To build it:** someone with write access adds `%s`. The factory builds nothing until then.\n\n", p.Cfg.Intake.Label) + marker + "\n"
			n, err := gh.CreateIssue(ctx, oneLine(children[i].Title), body, nil)
			if err != nil {
				return err
			}
			num = n
		}
		if err := p.Store.AddGroomChild(ctx, g.Repo, num, g.IssueNumber); err != nil {
			return err
		}
		children[i].Issue = num
		if err := p.Store.SetGroomChildren(ctx, g.ID, children); err != nil {
			return err
		}
	}
	it, created, err := p.Store.UpsertItemIn(ctx, g.Repo, g.IssueNumber, text.Title, state.Epic)
	if err != nil {
		return err
	}
	if created {
		if err := p.approve(ctx, it, text, who); err != nil {
			return err
		}
	}
	refs := make([]string, len(children))
	for i, c := range children {
		refs[i] = fmt.Sprintf("#%d", c.Issue)
	}
	p.warn(gh.AddLabels(ctx, g.IssueNumber, LabelEpic), "AddLabels")
	p.warn(gh.RemoveLabel(ctx, g.IssueNumber, LabelSplit), "RemoveLabel")
	p.warn(gh.RemoveLabel(ctx, g.IssueNumber, LabelSplitQ), "RemoveLabel")
	p.warn(gh.Comment(ctx, g.IssueNumber, fmt.Sprintf("**Factory:** split into %s, as @%s confirmed with `%s`. Each waits for someone with write access to add `%s`. This issue is now their epic: it's done once each is built or closed.",
		strings.Join(refs, ", "), who, LabelSplit, p.Cfg.Intake.Label)), "Comment")
	b, _ := json.Marshal(map[string]any{"issue": g.IssueNumber, "children": refs, "by": who})
	p.warn(p.Store.AddEvent(ctx, store.Event{Actor: "human", ItemID: &it.ID, Type: "groom_split", Reason: g.Repo, Data: string(b)}), "AddEvent")
	p.notify(ctx, fmt.Sprintf("Split %s into %s, as %s confirmed. They wait for you: add `%s` to build each.", p.ref(g.Repo, g.IssueNumber), strings.Join(refs, ", "), who, p.Cfg.Intake.Label))
	return p.Store.DecideGroomProposal(ctx, g.ID, store.GroomDone, who)
}

// adoptGroomChild makes a new item its epic's child when a split filed
// its issue.
func (p *Pipeline) adoptGroomChild(ctx context.Context, it *store.WorkItem) {
	parent, err := p.Store.GroomParent(ctx, it.Repo, it.IssueNumber)
	if err != nil || parent == 0 {
		p.warn(err, "GroomParent")
		return
	}
	if ep, err := p.Store.ItemByIssue(ctx, it.Repo, parent); err == nil {
		p.warn(p.Store.SetParent(ctx, it.ID, ep.ID), "SetParent")
	}
	p.warn(p.Store.ClearGroomChild(ctx, it.Repo, it.IssueNumber), "ClearGroomChild")
}

// settleGroomChildren forgets sub-issues that were approved (an item
// adopts them) or closed unbuilt, so their epic can finish. Intake adopts
// approved ones as it finds them, so this mostly reads, one issue each,
// the sub-issues still waiting.
func (p *Pipeline) settleGroomChildren(ctx context.Context) {
	cs, err := p.Store.GroomChildren(ctx, "", 0)
	if err != nil {
		p.warn(err, "GroomChildren")
		return
	}
	for _, c := range cs {
		if it, err := p.Store.ItemByIssue(ctx, c.Repo, c.Issue); err == nil {
			p.adoptGroomChild(ctx, it)
			continue
		}
		is, err := p.gh(c.Repo).GetIssue(ctx, c.Issue)
		if github.IsNotFound(err) || err == nil && is.State == "closed" {
			p.warn(p.Store.ClearGroomChild(ctx, c.Repo, c.Issue), "ClearGroomChild")
		}
	}
}

// waitsOnGroomChildren says whether an epic has sub-issues nobody has
// approved or closed yet.
func (p *Pipeline) waitsOnGroomChildren(ctx context.Context, ep *store.WorkItem) bool {
	cs, err := p.Store.GroomChildren(ctx, ep.Repo, ep.IssueNumber)
	return err != nil || len(cs) > 0
}
