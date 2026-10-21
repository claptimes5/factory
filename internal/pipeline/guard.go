package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/claptimes5/factory/internal/github"
	"github.com/claptimes5/factory/internal/metrics"
	"github.com/claptimes5/factory/internal/state"
	"github.com/claptimes5/factory/internal/store"
)

// Guard rails on auto-merge.
//
// The revert watch reads the base branch of each repo with recent factory
// merges, on main health's cadence and only when its head has moved, for
// commits and merged PRs that revert one. A revert, or a check main
// health first saw red on a factory PR's merge commit, is a strike
// against the repo. Enough strikes in a rolling window trip the repo's
// breaker: auto-merge stops there, the auto-merges waiting are cancelled,
// and you're told once. It stays off until you reset it (or a cool-off,
// if set, passes).
//
// Separately, a PR only merges itself once it was tested against the
// current base branch: through the forge's merge queue (or GitLab's
// merge train) where there is one, or else by bringing the branch up to
// date and waiting for CI on the new head first.

// RevertWindow is how far back the revert watch looks, and the period the
// revert rate is measured over.
const RevertWindow = metrics.RevertWindow

const (
	// maxRevertCommits and maxRevertPRs bound what one reading lists.
	maxRevertCommits = 1000
	maxRevertPRs     = 50
	// watchSlack reads a little before the last reading, for a commit
	// made before it but pushed after.
	watchSlack = 24 * time.Hour
	// syncWait is how long a PR brought up to date may wait for CI on its
	// new head before the factory gives up and leaves it for you.
	syncWait = 2 * time.Hour
	// maxSyncs is how many times the factory brings one PR up to date
	// before main moving under it leaves the PR for you.
	maxSyncs = 3
)

// guardNow is the guard rails' clock; tests move it.
var guardNow = time.Now

// RevertForge is a client that can read a base branch's commits and the
// PRs merged into it.
type RevertForge interface {
	BranchSHA(ctx context.Context, branch string) (string, error)
	BranchCommits(ctx context.Context, branch string, since time.Time, max int) ([]github.Commit, bool, error)
	MergedPRs(ctx context.Context, branch string, since time.Time, max int) ([]github.MergedPR, error)
}

var (
	_ RevertForge = (*github.Client)(nil)
	_ MergeQueuer = (*github.Client)(nil)
)

// MergeQueuer is a client that knows whether a branch has a merge queue
// (GitHub's, or GitLab's merge trains) and can put a PR in it.
type MergeQueuer interface {
	MergeQueue(ctx context.Context, branch string) (bool, error)
	QueueMerge(ctx context.Context, n int, sha, method string) error
}

// --- what reverts what ---

var (
	revertSubject = regexp.MustCompile(`^Revert "(.*)"(?: \((?:#|!)\d+\))?$`)
	revertsCommit = regexp.MustCompile(`(?i)This reverts commit ([0-9a-f]{7,40})\b`)
	revertsMR     = regexp.MustCompile(`(?i)This reverts merge request !(\d+)\b`)
	revertsPR     = regexp.MustCompile(`(?im)^Reverts (?:([\w.-]+/[\w.-]+))?#(\d+)\b`)
	revertBranch  = regexp.MustCompile(`^revert-(\d+)-`)
	mergedRevert  = regexp.MustCompile(`^Merge pull request #\d+ from [\w.-]+/revert-(\d+)-`)
	mergedPR      = regexp.MustCompile(`^Merge pull request #(\d+) from `)
)

// revertRefs are what a commit or PR says it reverts. It's forge and
// author text, so it's only matched against, never shown.
type revertRefs struct {
	inner   string   // the subject it reverts: Revert "<inner>"
	shas    []string // "This reverts commit <sha>"
	numbers []int    // PRs it names: "Reverts o/r#12", "!12", a revert-12- branch
}

func (r revertRefs) empty() bool { return r.inner == "" && len(r.shas) == 0 && len(r.numbers) == 0 }

// specific says whether the refs name a commit or a PR, not only a title.
func (r revertRefs) specific() bool { return len(r.shas) > 0 || len(r.numbers) > 0 }

// parseRevert reads what text (a commit message, or a PR's title and
// body) and a PR's head branch say they revert, in repo. On GitLab, #12
// is an issue, so only !12 names a merge request; on GitHub, !12 means
// nothing. A revert of a revert puts the change back, so it reverts
// nothing. The number in a revert-12- branch is believed only when the
// text says it's a revert too: anyone can name a branch that way.
func parseRevert(repo, text, headRef string, gitlab bool) revertRefs {
	var r revertRefs
	subject, _, _ := strings.Cut(strings.TrimSpace(text), "\n")
	subject = strings.TrimSpace(subject)
	if strings.HasPrefix(subject, `Revert "Revert `) || strings.HasPrefix(subject, `Reapply "`) {
		return r
	}
	if m := revertSubject.FindStringSubmatch(subject); m != nil {
		r.inner = strings.TrimSpace(m[1])
	}
	for _, m := range revertsCommit.FindAllStringSubmatch(text, -1) {
		r.shas = append(r.shas, strings.ToLower(m[1]))
	}
	num := func(s string) {
		if n, err := strconv.Atoi(s); err == nil && n > 0 && !slices.Contains(r.numbers, n) {
			r.numbers = append(r.numbers, n)
		}
	}
	if gitlab {
		for _, m := range revertsMR.FindAllStringSubmatch(text, -1) {
			num(m[1])
		}
		return r
	}
	saysReverts := r.inner != ""
	for _, m := range revertsPR.FindAllStringSubmatch(text, -1) {
		if m[1] == "" || strings.EqualFold(m[1], repo) || strings.HasSuffix(strings.ToLower(repo), "/"+strings.ToLower(m[1])) {
			num(m[2])
			saysReverts = true
		}
	}
	if m := mergedPR.FindStringSubmatch(r.inner); m != nil {
		num(m[1]) // Revert "Merge pull request #12 from …"
	}
	if m := revertBranch.FindStringSubmatch(headRef); m != nil && saysReverts {
		num(m[1])
	}
	if m := mergedRevert.FindStringSubmatch(subject); m != nil {
		// The merge of a revert PR: its body is the PR's title.
		for _, line := range strings.Split(text, "\n")[1:] {
			if revertSubject.MatchString(strings.TrimSpace(line)) {
				num(m[1])
				break
			}
		}
	}
	return r
}

// names says whether the refs' commits or PR numbers name the merge m.
func (r revertRefs) names(m store.FactoryMerge) bool {
	for _, sha := range r.shas {
		for _, of := range []string{m.MergeSHA, m.SquashSHA, m.HeadSHA} {
			if of != "" && strings.HasPrefix(strings.ToLower(of), sha) {
				return true
			}
		}
	}
	return slices.Contains(r.numbers, m.PR)
}

// titled says whether the subject the refs revert is the merge m's: its
// title, its squash commit's ("title (#12)", or "(!12)" on GitLab), or
// the merge of its branch. Titles repeat (each rebuild of an issue opens
// a PR with the same one), so a title is matched only when nothing more
// specific was given, and only when one merge has it.
func (r revertRefs) titled(m store.FactoryMerge, gitlab bool) bool {
	in, t := r.inner, strings.TrimSpace(m.Title)
	if in == "" {
		return false
	}
	mark := "#"
	if gitlab {
		mark = "!"
	}
	switch {
	case t != "" && (in == t || in == fmt.Sprintf("%s (%s%d)", t, mark, m.PR)):
		return true
	case m.Branch != "" && strings.HasPrefix(in, fmt.Sprintf("Merge branch '%s' into ", m.Branch)):
		return true
	}
	return false
}

// reverted are the merges among open that refs revert: the ones its
// commits or numbers name, or else the one merge with its title.
func (r revertRefs) reverted(open []store.FactoryMerge, gitlab bool) []store.FactoryMerge {
	var out []store.FactoryMerge
	if r.specific() {
		for _, m := range open {
			if r.names(m) {
				out = append(out, m)
			}
		}
		return out
	}
	for _, m := range open {
		if r.titled(m, gitlab) {
			out = append(out, m)
		}
	}
	if len(out) != 1 {
		return nil // no title, or one that more than one merge has
	}
	return out
}

// --- the revert watch ---

// revertHit is one merge a commit or PR reverted.
type revertHit struct {
	merge store.FactoryMerge
	by    string // "commit abc1234" or "PR #12"
}

// revertReading is what one repo's base branch said, read without the lock.
type revertReading struct {
	repo, head string
	hits       []revertHit
	err        error
	more       bool // there were more commits than one reading lists
}

// openMerges are a repo's factory merges in the revert window that stand.
func (p *Pipeline) openMerges(ctx context.Context, repo string, now time.Time) ([]store.FactoryMerge, error) {
	ms, err := p.Store.FactoryMerges(ctx, repo, stamp(now.Add(-RevertWindow)))
	if err != nil {
		return nil, err
	}
	var open []store.FactoryMerge
	for _, m := range ms {
		if m.RevertedAt == "" {
			open = append(open, m)
		}
	}
	return open, nil
}

// readReverts lists what was merged into a repo's base branch since its
// last reading, when its head is no longer head, and finds the factory
// merges it reverts. A repo with no factory merge to revert costs no call.
func (p *Pipeline) readReverts(ctx context.Context, repo, head string) *revertReading {
	r := &revertReading{repo: repo, head: head}
	if p.Cfg.NoGuard || head == "" {
		return r
	}
	f, ok := p.gh(repo).(RevertForge)
	if !ok {
		return r
	}
	now := guardNow()
	open, err := p.openMerges(ctx, repo, now)
	if err != nil || len(open) == 0 {
		r.err = err
		return r
	}
	g, err := p.Store.Guard(ctx, repo)
	if err != nil {
		r.err = err
		return r
	}
	if g != nil && g.WatchedSHA == head {
		return r // nothing new on the branch
	}
	since, _ := time.Parse(store.TimeFormat, open[0].MergedAt)
	since = since.Add(-time.Hour)
	if g != nil {
		if at, err := time.Parse(store.TimeFormat, g.WatchedAt); err == nil && at.Add(-watchSlack).After(since) {
			since = at.Add(-watchSlack)
		}
	}
	branch, gitlab := p.repoConf(repo).BaseBranch, p.repoConf(repo).GitLab()
	seen := map[int]bool{}
	match := func(refs revertRefs, by string) {
		if refs.empty() {
			return
		}
		for _, m := range refs.reverted(open, gitlab) {
			if !seen[m.PR] {
				seen[m.PR] = true
				r.hits = append(r.hits, revertHit{merge: m, by: by})
			}
		}
	}
	commits, more, err := f.BranchCommits(ctx, branch, since, maxRevertCommits)
	if err != nil {
		r.err = err
		return r
	}
	r.more = more
	// Oldest first: the first revert of a merge is the one recorded.
	for i := len(commits) - 1; i >= 0; i-- {
		c := commits[i]
		match(parseRevert(repo, c.Message, "", gitlab), "commit "+short(c.SHA))
	}
	prs, err := f.MergedPRs(ctx, branch, since, maxRevertPRs)
	if err != nil {
		r.err = err
		return r
	}
	for i := len(prs) - 1; i >= 0; i-- {
		pr := prs[i]
		by := fmt.Sprintf("PR #%d", pr.Number)
		if gitlab {
			by = fmt.Sprintf("MR !%d", pr.Number)
		}
		match(parseRevert(repo, pr.Title+"\n\n"+pr.Body, pr.HeadRef, gitlab), by)
	}
	return r
}

// applyReverts records a reading's reverts, notifies once for each, and
// trips the repo's breaker if they're too many. It holds the action lock.
func (p *Pipeline) applyReverts(ctx context.Context, r *revertReading) {
	if r.err != nil {
		p.log().Warn("revert watch", "repo", r.repo, "err", r.err)
		return
	}
	if r.more {
		// The oldest weren't read: the watch's place stays where it was,
		// head and time, so the next reading reads from there again.
		p.log().Warn("revert watch: more commits than one reading lists; the oldest weren't read", "repo", r.repo, "max", maxRevertCommits)
	}
	now := guardNow()
	struck := false
	for _, h := range r.hits {
		news, err := p.Store.MarkReverted(ctx, r.repo, h.merge.PR, h.by, stamp(now))
		if err != nil {
			p.log().Warn("revert watch", "repo", r.repo, "err", err)
			continue
		}
		if !news {
			continue
		}
		struck = true
		var item *int64
		if h.merge.ItemID != 0 {
			id := h.merge.ItemID
			item = &id
		}
		data, _ := json.Marshal(map[string]any{"pr": h.merge.PR, "by": h.by, "auto": h.merge.Auto})
		p.warn(p.Store.AddEvent(ctx, store.Event{Actor: "daemon", ItemID: item, Type: "pr_reverted", Reason: fmt.Sprintf("PR #%d", h.merge.PR), Data: string(data)}), "AddEvent")
		p.alert(ctx, fmt.Sprintf("Factory PR %s (%s) was reverted on %s by %s.", p.ref(r.repo, h.merge.PR), SafeText(h.merge.Title, 100), p.repoConf(r.repo).BaseBranch, h.by))
	}
	if r.head != "" && !p.Cfg.NoGuard && !r.more {
		p.warn(p.Store.SetWatched(ctx, r.repo, r.head, stamp(now)), "SetWatched")
	}
	if struck {
		p.checkBreaker(ctx, r.repo)
	}
}

// WatchMain reads each repo's base branch for reverts, when main health,
// which does it as it reads CI, is off.
func (p *Pipeline) WatchMain(ctx context.Context) {
	if p.Cfg.NoGuard {
		return
	}
	for _, repo := range p.Cfg.RepoNames() {
		f, ok := p.gh(repo).(RevertForge)
		if !ok {
			continue
		}
		if open, err := p.openMerges(ctx, repo, guardNow()); err != nil || len(open) == 0 {
			continue // nothing to watch for: no call
		}
		head, err := f.BranchSHA(ctx, p.repoConf(repo).BaseBranch)
		if err != nil {
			p.log().Warn("revert watch", "repo", repo, "err", err)
			continue
		}
		r := p.readReverts(ctx, repo, head)
		p.act.Lock()
		p.applyReverts(ctx, r)
		p.act.Unlock()
	}
}

// ParentForge is a client that can read a commit's first parent.
type ParentForge interface {
	FirstParent(ctx context.Context, sha string) (string, error)
}

var _ ParentForge = (*github.Client)(nil)

// readParent reads, without the lock, the checks on the first parent of
// the base branch's head when the head is a factory PR's merge and a
// check is newly red on it: a check red there already isn't the PR's.
func (p *Pipeline) readParent(ctx context.Context, r *healthReading, filed map[string]store.HealthIssue) {
	if p.Cfg.NoGuard || r.sha == "" {
		return
	}
	fresh := false
	for k, c := range r.checks {
		if _, ok := filed[k]; !ok && c.State == store.HealthRed {
			fresh = true
		}
	}
	if !fresh {
		return
	}
	m, err := p.Store.MergeBySHA(ctx, r.repo, r.sha)
	if err != nil || m == nil {
		p.warn(err, "MergeBySHA")
		return
	}
	pf, ok := r.f.(ParentForge)
	if !ok {
		return
	}
	parent, err := pf.FirstParent(ctx, r.sha)
	if err != nil || parent == "" {
		p.warn(err, "FirstParent")
		return
	}
	runs, err := r.f.CheckHistory(ctx, parent)
	if err != nil {
		p.warn(err, "CheckHistory")
		return
	}
	r.parent, r.parentChecks = parent, readChecks(runs)
}

// attributeRed records a strike when main health first sees a check red
// on a factory PR's merge commit, and it passed on the commit before (the
// merge's first parent). A check missing there, still running, or with no
// result is no strike: unsure, it's none. It holds the action lock.
func (p *Pipeline) attributeRed(ctx context.Context, r *healthReading, check string) {
	if p.Cfg.NoGuard || r.parent == "" {
		return
	}
	if c := r.parentChecks[check]; c == nil || c.State != store.HealthGreen {
		return // red before the merge too, or not known to have passed
	}
	repo, sha := r.repo, r.sha
	m, err := p.Store.MarkRed(ctx, repo, sha, check, stamp(guardNow()))
	if err != nil || m == nil {
		p.warn(err, "MarkRed")
		return
	}
	var item *int64
	if m.ItemID != 0 {
		id := m.ItemID
		item = &id
	}
	data, _ := json.Marshal(map[string]any{"pr": m.PR, "sha": sha, "check": check, "auto": m.Auto})
	p.warn(p.Store.AddEvent(ctx, store.Event{Actor: "daemon", ItemID: item, Type: "pr_broke_main", Reason: fmt.Sprintf("PR #%d", m.PR), Data: string(data)}), "AddEvent")
	p.alert(ctx, fmt.Sprintf("Main health: %s went red on %s at %s, the merge of factory PR %s (%s); it passed on the commit before.", SafeName(check), repo, short(sha), p.ref(repo, m.PR), SafeText(m.Title, 100)))
	p.checkBreaker(ctx, repo)
}

// --- the breaker ---

// BreakerState is a repo's auto-merge breaker, for status and the dashboard.
type BreakerState struct {
	Repo      string
	On        bool // the repo's auto_merge lets PRs merge themselves
	Tripped   bool
	TrippedAt string
	Reason    string
	Strikes   int
	Threshold int // 0 when it never trips
	Window    time.Duration
	CoolOff   time.Duration // 0 when only a human resets it
	ResetAt   string
	ResetBy   string
}

// Breaker reads a repo's breaker and its strikes now.
func (p *Pipeline) Breaker(ctx context.Context, repo string) (BreakerState, error) {
	am := p.repoConf(repo).AutoMerge
	b := BreakerState{Repo: repo, On: am.On(), Threshold: am.BreakerThreshold(), Window: am.BreakerWindow(), CoolOff: am.BreakerCoolOff()}
	g, err := p.Store.Guard(ctx, repo)
	if err != nil {
		return b, err
	}
	if end, ok := p.coolOffEnd(repo, g); ok {
		// Past its cool-off: back on, as the next look at it will record.
		cp := *g
		cp.TrippedAt, cp.Reason, cp.ResetAt, cp.ResetBy = "", "", stamp(end), "cool-off"
		g = &cp
	}
	since := stamp(guardNow().Add(-b.Window))
	if g != nil {
		b.Tripped, b.TrippedAt, b.Reason, b.ResetAt, b.ResetBy = g.Tripped(), g.TrippedAt, g.Reason, g.ResetAt, g.ResetBy
		if g.ResetAt > since {
			since = g.ResetAt
		}
	}
	s, err := p.Store.Strikes(ctx, repo, since)
	b.Strikes = len(s)
	return b, err
}

// Breakers reads every repo's breaker.
func (p *Pipeline) Breakers(ctx context.Context) []BreakerState {
	var out []BreakerState
	for _, repo := range p.Cfg.RepoNames() {
		b, err := p.Breaker(ctx, repo)
		p.warn(err, "Breaker")
		out = append(out, b)
	}
	return out
}

// checkBreaker trips a repo's breaker once its strikes reach the
// threshold: auto-merge stops there, the auto-merges waiting are
// cancelled, and you're told, once. It holds the action lock.
func (p *Pipeline) checkBreaker(ctx context.Context, repo string) {
	if p.Cfg.NoGuard {
		return
	}
	am := p.repoConf(repo).AutoMerge
	th := am.BreakerThreshold()
	if !am.On() || th == 0 {
		return
	}
	// Against startMerge and enableAt: a PR they turn auto-merge on for is
	// on record before the breaker lists the PRs to hand back, or they see
	// it tripped.
	mu := p.guardLock(repo)
	mu.Lock()
	defer mu.Unlock()
	now := guardNow()
	g, err := p.coolOff(ctx, repo)
	if err != nil || g.Tripped() {
		p.warn(err, "Guard")
		return
	}
	since := stamp(now.Add(-am.BreakerWindow()))
	if g != nil && g.ResetAt > since {
		since = g.ResetAt
	}
	strikes, err := p.Store.Strikes(ctx, repo, since)
	if err != nil || len(strikes) < th {
		p.warn(err, "Strikes")
		return
	}
	var what []string
	for _, s := range strikes {
		w := fmt.Sprintf("%s reverted by %s", p.ref(repo, s.PR), s.RevertedBy)
		switch {
		case s.RevertedAt != "" && s.RedAt != "":
			w = fmt.Sprintf("%s turned main red and was reverted", p.ref(repo, s.PR))
		case s.RevertedAt == "":
			w = fmt.Sprintf("%s turned main red (%s)", p.ref(repo, s.PR), SafeName(s.RedCheck))
		}
		what = append(what, w)
	}
	reason := fmt.Sprintf("%d factory PRs in %s: %s", len(strikes), days(am.BreakerWindow()), strings.Join(what, "; "))
	tripped, err := p.Store.TripBreaker(ctx, repo, reason, stamp(now))
	if err != nil || !tripped {
		p.warn(err, "TripBreaker")
		return
	}
	data, _ := json.Marshal(map[string]any{"strikes": len(strikes), "threshold": th})
	p.warn(p.Store.AddEvent(ctx, store.Event{Actor: "daemon", Type: "auto_merge_breaker", Reason: repo, Data: string(data)}), "AddEvent")
	var cancelled, stuck []string
	items, err := p.Store.ListItems(ctx, state.AwaitingHuman)
	p.warn(err, "ListItems")
	for _, it := range items {
		if !strings.EqualFold(it.Repo, repo) || !p.autoMerged(ctx, it) {
			continue
		}
		if err := p.handBack(ctx, it, "the auto-merge breaker tripped", false); err != nil {
			stuck = append(stuck, p.ref(repo, it.PRNumber))
			continue
		}
		cancelled = append(cancelled, p.ref(repo, it.PRNumber))
	}
	msg := fmt.Sprintf("Auto-merge is off for %s: %s. New PRs there wait for you.", repo, reason)
	if len(cancelled) > 0 {
		msg += fmt.Sprintf(" Cancelled auto-merge on %s; %s for you.", strings.Join(cancelled, ", "), plural(len(cancelled), "it waits", "they wait"))
	}
	if len(stuck) > 0 {
		msg += fmt.Sprintf(" Couldn't cancel it on %s: turn it off on the PR yourself.", strings.Join(stuck, ", "))
	}
	msg += fmt.Sprintf(" Turn it back on with `factory automerge reset %s` or the dashboard's Reset button", repo)
	if c := am.BreakerCoolOff(); c > 0 {
		msg += fmt.Sprintf("; it comes back on its own after %s", days(c))
	}
	p.alert(ctx, msg+".")
}

// days writes a duration in days when it's whole days, else as is.
func days(d time.Duration) string {
	if d >= 24*time.Hour && d%(24*time.Hour) == 0 {
		n := int(d / (24 * time.Hour))
		return fmt.Sprintf("%d %s", n, plural(n, "day", "days"))
	}
	return d.String()
}

// guardLock is a repo's lock between the breaker tripping and a PR's
// auto-merge being turned on. It's held across forge calls (turning
// auto-merge on, or off for each PR the breaker hands back), which the
// clients' HTTP timeout bounds; nothing that holds it takes another lock
// that waits on it.
func (p *Pipeline) guardLock(repo string) *sync.Mutex {
	mu, _ := p.guardMu.LoadOrStore(strings.ToLower(repo), &sync.Mutex{})
	return mu.(*sync.Mutex)
}

// coolOffEnd is when a tripped breaker's cool-off ended, if it has.
func (p *Pipeline) coolOffEnd(repo string, g *store.Guard) (time.Time, bool) {
	c := p.repoConf(repo).AutoMerge.BreakerCoolOff()
	if !g.Tripped() || c <= 0 {
		return time.Time{}, false
	}
	at, err := time.Parse(store.TimeFormat, g.TrippedAt)
	if err != nil || guardNow().Before(at.Add(c)) {
		return time.Time{}, false
	}
	return at.Add(c), true
}

// coolOff reads a repo's breaker, resetting it first if its cool-off has
// ended, as of when it ended: strikes since then count. You're told once.
func (p *Pipeline) coolOff(ctx context.Context, repo string) (*store.Guard, error) {
	g, err := p.Store.Guard(ctx, repo)
	if err != nil {
		return nil, err
	}
	end, ok := p.coolOffEnd(repo, g)
	if !ok {
		return g, nil
	}
	reset, err := p.resetBreaker(ctx, repo, "cool-off", end)
	if err != nil {
		return nil, err
	}
	if reset {
		p.notify(ctx, fmt.Sprintf("Auto-merge is back on for %s: its breaker's cool-off (%s) passed. PRs that were waiting still wait for you.", repo, days(p.repoConf(repo).AutoMerge.BreakerCoolOff())))
	}
	return p.Store.Guard(ctx, repo)
}

// breakerTripped says whether a repo's breaker keeps its PRs from
// merging themselves. A breaker past its cool-off is reset, and you're
// told. Unsure, it says tripped.
func (p *Pipeline) breakerTripped(ctx context.Context, repo string) bool {
	if p.Cfg.NoGuard {
		return false
	}
	g, err := p.coolOff(ctx, repo)
	if err != nil {
		p.warn(err, "Guard")
		return true
	}
	return g.Tripped()
}

// errBreakerTripped is why a PR doesn't merge itself after all: the
// repo's breaker tripped while it was being opened.
var errBreakerTripped = errors.New("auto-merge's breaker for the repo tripped")

// ResetAutoMerge turns a repo's tripped breaker off, so its PRs may merge
// themselves again. Strikes before now no longer count. The PRs whose
// auto-merge it cancelled still wait for you.
func (p *Pipeline) ResetAutoMerge(ctx context.Context, repo, by string) error {
	name, ok := p.repoName(repo)
	if !ok {
		return fmt.Errorf("%s isn't one of the factory's repos", repo)
	}
	reset, err := p.resetBreaker(ctx, name, by, guardNow())
	if err != nil {
		return err
	}
	if !reset {
		return fmt.Errorf("auto-merge's breaker isn't tripped for %s", name)
	}
	return nil
}

// resetBreaker resets a repo's breaker as of at: strikes before it no
// longer count.
func (p *Pipeline) resetBreaker(ctx context.Context, repo, by string, at time.Time) (bool, error) {
	reset, err := p.Store.ResetBreaker(ctx, repo, by, stamp(at))
	if err != nil || !reset {
		return reset, err
	}
	actor := "human"
	if by == "cool-off" {
		actor = "daemon"
	}
	p.warn(p.Store.AddEvent(ctx, store.Event{Actor: actor, Type: "auto_merge_breaker_reset", Reason: repo, Data: fmt.Sprintf(`{"by":%q}`, by)}), "AddEvent")
	return true, nil
}

// repoName is the configured spelling of a repo.
func (p *Pipeline) repoName(repo string) (string, bool) {
	for _, r := range p.Cfg.RepoNames() {
		if strings.EqualFold(r, repo) {
			return r, true
		}
	}
	return "", false
}

// --- tested against the current base branch ---

// mergeState is where the factory's auto-merge of the item's PR stands:
// the latest of its auto_merge (on), auto_merge_wait (brought up to date,
// waiting for CI on the new head) and auto_merge_off events.
type mergeState struct {
	Kind  string `json:"-"`     // auto_merge, auto_merge_wait, auto_merge_off, or ""
	Head  string `json:"head"`  // the commit it merges, or waits on
	Base  string `json:"base"`  // the base branch head it was tested against
	Queue bool   `json:"queue"` // the forge's merge queue keeps it current
	At    string `json:"-"`
	// Syncs is how many times it was brought up to date since auto-merge
	// was last turned on for a head the factory didn't make (a new build's)
	// or turned off.
	Syncs int `json:"-"`
	// Made are the heads the factory made by bringing the branch up to
	// date, ever, even after auto-merge was turned off: a human merging one
	// merged the factory's commit, not a change of theirs.
	Made []string `json:"-"`
}

func (p *Pipeline) mergeState(ctx context.Context, it *store.WorkItem) mergeState {
	var ms mergeState
	if it.PRNumber == 0 {
		return ms
	}
	evs, err := p.Store.Events(ctx, it.ID)
	if err != nil {
		return ms
	}
	ref := fmt.Sprintf("PR #%d", it.PRNumber)
	for _, e := range evs {
		if e.Reason != ref {
			continue
		}
		switch e.Type {
		case "auto_merge", "auto_merge_wait":
			prev := ms
			ms = mergeState{Kind: e.Type, At: e.At, Syncs: prev.Syncs, Made: prev.Made}
			_ = json.Unmarshal([]byte(e.Data), &ms)
			switch {
			case e.Type == "auto_merge_wait":
				ms.Syncs++
				ms.Made = append(ms.Made, ms.Head)
			case !slices.Contains(prev.Made, ms.Head):
				ms.Syncs = 0 // on for a new build's head: it starts over
			}
		case "auto_merge_off":
			ms = mergeState{Kind: e.Type, At: e.At, Made: ms.Made}
			var off struct {
				Made string `json:"made"` // a head made as it was turned off
			}
			if json.Unmarshal([]byte(e.Data), &off) == nil && off.Made != "" {
				ms.Made = append(ms.Made, off.Made)
			}
		}
	}
	return ms
}

// syncBase brings the item's branch up to date with its base, if it's
// behind: the daemon merges the base into it, in a clone of its own, and
// pushes exactly that commit. It returns the branch's head after, and the
// base commit it holds. The branch must still be at head. The clone
// fetches no file contents but the ones the merge reads.
func (p *Pipeline) syncBase(ctx context.Context, it *store.WorkItem, head string) (string, string, error) {
	g := p.git(it.Repo)
	if g == nil {
		return "", "", fmt.Errorf("no git remote to bring the branch up to date with")
	}
	root := filepath.Join(p.Cfg.Path(p.Cfg.DataDir), "sync")
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", "", err
	}
	dir, err := os.MkdirTemp(root, fmt.Sprintf("item-%d-", it.ID))
	if err != nil {
		return "", "", err
	}
	defer os.RemoveAll(dir)
	base, branch := p.repoConf(it.Repo).BaseBranch, p.branchFor(it)
	defer g.DoneSync(filepath.Join(dir, "repo"))
	ws, err := g.PrepareSync(ctx, filepath.Join(dir, "repo"), base, branch)
	if err != nil {
		return "", "", err
	}
	if !ws.Existed || ws.Start != head {
		return "", "", fmt.Errorf("its branch moved from %s", short(head))
	}
	if g.IsAncestor(ctx, ws.Repo, ws.BaseSHA, head) {
		return head, ws.BaseSHA, nil
	}
	msg := fmt.Sprintf("Merge %s into %s\n\nThe factory brought this branch up to date with %s before it merges itself, so CI tests it against the current %s.", base, branch, base, base)
	sha, err := g.MergeInto(ctx, ws.Repo, "refs/remotes/origin/"+base, msg)
	if err != nil {
		return "", "", fmt.Errorf("%s doesn't merge into it cleanly", base)
	}
	if err := g.Push(ctx, ws.Repo, sha, branch, false); err != nil {
		return "", "", err
	}
	return sha, ws.BaseSHA, nil
}

// handBack turns the factory's auto-merge off on the item's PR and leaves
// it for you, labeled as any PR waiting on you, its description no longer
// saying it merges itself. With tell, you're told why.
func (p *Pipeline) handBack(ctx context.Context, it *store.WorkItem, why string, tell bool) error {
	if err := p.cancelAutoMerge(ctx, it, why); err != nil {
		return err // it notified
	}
	gh := p.gh(it.Repo)
	if pr, err := gh.GetPR(ctx, it.PRNumber); err != nil {
		p.warn(err, "GetPR")
	} else if strings.Contains(pr.Body, autoFooter) {
		p.warn(gh.UpdatePRBody(ctx, it.PRNumber, strings.Replace(pr.Body, autoFooter, humanFooter, 1)), "UpdatePRBody")
	}
	p.warn(gh.LabelPR(ctx, it.PRNumber, LabelAwaitingYou), "LabelPR")
	p.warn(gh.AddLabels(ctx, it.IssueNumber, LabelAwaitingYou), "AddLabels")
	if tell {
		p.notify(ctx, fmt.Sprintf("%s won't merge itself: %s. It waits for you.", p.ref(it.Repo, it.PRNumber), why))
	}
	return nil
}

// recordMergeOn records that auto-merge is on (or waits) for the PR at
// ms.Head, under the repo's guard lock and only while its breaker hasn't
// tripped, then has enable turn it on. The lock is held across enable's
// forge calls, so a breaker tripping waits for them: they're bounded by
// the clients' HTTP timeout (30 seconds a request, in github and
// gitlab). Recorded first: if the daemon
// stops between the forge turning it on and the record, a later park or
// build must turn it off; and a breaker tripping after it hands it back.
func (p *Pipeline) recordMergeOn(ctx context.Context, it *store.WorkItem, prNumber int, typ string, ms mergeState, enable func() error) error {
	mu := p.guardLock(it.Repo)
	mu.Lock()
	defer mu.Unlock()
	if p.breakerTripped(ctx, it.Repo) {
		return errBreakerTripped
	}
	am := p.repoConf(it.Repo).AutoMerge
	data, _ := json.Marshal(map[string]any{"head": ms.Head, "base": ms.Base, "queue": ms.Queue, "method": am.MergeMethod()})
	if err := p.Store.AddEvent(ctx, store.Event{Actor: "daemon", ItemID: &it.ID, Type: typ, Reason: fmt.Sprintf("PR #%d", prNumber), Data: string(data)}); err != nil {
		return err
	}
	if enable == nil {
		return nil
	}
	return enable()
}

// startMerge hands the PR at head to the forge to merge, once it's tested
// against the current base: into the merge queue where the base branch
// has one; else, when it's up to date, to auto-merge pinned to head; and
// when it's behind, it's brought up to date first and waits for CI there
// (see followMerge). It reports whether the PR is on its way to merge. A
// breaker that tripped since the PR was opened stops it.
func (p *Pipeline) startMerge(ctx context.Context, it *store.WorkItem, prNumber int, head string) (bool, error) {
	am := p.repoConf(it.Repo).AutoMerge
	base := p.repoConf(it.Repo).BaseBranch
	if q, ok := p.gh(it.Repo).(MergeQueuer); ok {
		queued, err := q.MergeQueue(ctx, base)
		if err != nil {
			return false, fmt.Errorf("couldn't tell whether %s has a merge queue: %w", base, err)
		}
		if queued {
			err := p.recordMergeOn(ctx, it, prNumber, "auto_merge", mergeState{Head: head, Queue: true}, func() error {
				return q.QueueMerge(ctx, prNumber, head, am.MergeMethod())
			})
			return err == nil, err
		}
	}
	if p.breakerTripped(ctx, it.Repo) {
		return false, errBreakerTripped // before pushing anything
	}
	now, tip, err := p.syncBase(ctx, it, head)
	if err != nil {
		return false, fmt.Errorf("couldn't bring it up to date with %s: %w", base, err)
	}
	if now != head {
		err := p.recordMergeOn(ctx, it, prNumber, "auto_merge_wait", mergeState{Head: now, Base: tip}, nil)
		if errors.Is(err, errBreakerTripped) {
			// It tripped while the branch was brought up to date: the head
			// pushed is still the factory's, should you merge it.
			data, _ := json.Marshal(map[string]any{"why": "the auto-merge breaker tripped", "made": now})
			p.warn(p.Store.AddEvent(ctx, store.Event{Actor: "daemon", ItemID: &it.ID, Type: "auto_merge_off", Reason: fmt.Sprintf("PR #%d", prNumber), Data: string(data)}), "AddEvent")
		}
		return err == nil, err
	}
	err = p.recordMergeOn(ctx, it, prNumber, "auto_merge", mergeState{Head: head, Base: tip}, func() error {
		return p.gh(it.Repo).(AutoMerger).AutoMerge(ctx, prNumber, head, am.MergeMethod())
	})
	return err == nil, err
}

// followMerge keeps a PR the factory set to merge itself tested against
// the current base, on each poll until it merges. Brought up to date, it
// waits for CI on its new head, then turns auto-merge on for that commit;
// red CI there, or none within syncWait, leaves it for you. If the base
// moves before it merges, auto-merge is turned off and it's brought up to
// date again, up to maxSyncs times. A merge queue needs none of this.
// tips holds each repo's base branch head, read once for a closeOut pass.
func (p *Pipeline) followMerge(ctx context.Context, it *store.WorkItem, pr *github.PR, tips map[string]string) {
	ms := p.mergeState(ctx, it)
	if ms.Kind != "auto_merge" && ms.Kind != "auto_merge_wait" || ms.Queue || ms.Base == "" {
		return
	}
	f, ok := p.gh(it.Repo).(HealthForge)
	if !ok {
		return // can't read the base or CI: GitHub's own rules decide
	}
	if ms.Kind == "auto_merge_wait" && pr.Head.SHA != "" && pr.Head.SHA != ms.Head {
		p.warn(p.handBack(ctx, it, fmt.Sprintf("its branch moved to %s while it waited for CI", short(pr.Head.SHA)), true), "handBack")
		return
	}
	tip, ok := tips[it.Repo]
	if !ok {
		var err error
		tip, err = f.BranchSHA(ctx, p.repoConf(it.Repo).BaseBranch)
		if err != nil {
			p.warn(err, "BranchSHA")
		}
		tips[it.Repo] = tip
	}
	if tip == "" {
		return
	}
	if tip != ms.Base {
		// The base moved: perhaps by merging this PR, since it was read.
		if now, err := p.gh(it.Repo).GetPR(ctx, it.PRNumber); err != nil || now.Merged || now.State == "closed" {
			p.warn(err, "GetPR")
			return // the next poll closes it out
		}
		p.resync(ctx, it, ms)
		return
	}
	if ms.Kind != "auto_merge_wait" {
		return // on, and up to date
	}
	runs, err := f.CheckHistory(ctx, ms.Head)
	if err != nil {
		p.warn(err, "CheckHistory")
		return
	}
	st, detail := store.HealthPending, ""
	if checks := readChecks(runs); len(checks) > 0 {
		st, detail = overall(checks)
		passed := false
		for _, c := range checks {
			passed = passed || c.State == store.HealthGreen
		}
		if st == store.HealthGreen && !passed {
			st = store.HealthPending // only cancelled or skipped: nothing passed yet
		}
	}
	switch st {
	case store.HealthGreen:
		p.enableAt(ctx, it, ms)
	case store.HealthRed, store.HealthFlaky:
		p.warn(p.handBack(ctx, it, fmt.Sprintf("CI failed on it merged with %s (%s at %s)", p.repoConf(it.Repo).BaseBranch, SafeText(detail, 200), short(ms.Head)), true), "handBack")
	default:
		if since, err := time.Parse(store.TimeFormat, ms.At); err == nil && guardNow().Sub(since) > syncWait {
			p.warn(p.handBack(ctx, it, fmt.Sprintf("CI didn't finish on %s within %s of bringing it up to date", short(ms.Head), syncWait), true), "handBack")
		}
	}
}

// enableAt turns auto-merge on for the up-to-date head CI passed, unless
// the repo's breaker tripped; either way short of on, the PR is handed
// back to you.
func (p *Pipeline) enableAt(ctx context.Context, it *store.WorkItem, ms mergeState) {
	am := p.repoConf(it.Repo).AutoMerge
	err := p.recordMergeOn(ctx, it, it.PRNumber, "auto_merge", mergeState{Head: ms.Head, Base: ms.Base}, func() error {
		return p.gh(it.Repo).(AutoMerger).AutoMerge(ctx, it.PRNumber, ms.Head, am.MergeMethod())
	})
	switch {
	case errors.Is(err, errBreakerTripped):
		p.warn(p.handBack(ctx, it, "the auto-merge breaker for "+it.Repo+" tripped", true), "handBack")
	case err != nil:
		p.warn(p.handBack(ctx, it, "turning on auto-merge failed: "+SafeText(err.Error(), 200), true), "handBack")
	}
}

// resync brings a PR whose base moved up to date again, with auto-merge
// off until CI passes there.
func (p *Pipeline) resync(ctx context.Context, it *store.WorkItem, ms mergeState) {
	base := p.repoConf(it.Repo).BaseBranch
	if ms.Syncs >= maxSyncs {
		p.warn(p.handBack(ctx, it, fmt.Sprintf("%s kept moving under it (brought up to date %d times)", base, ms.Syncs), true), "handBack")
		return
	}
	if ms.Kind == "auto_merge" {
		// Off before the push: GitHub keeps auto-merge on through a push.
		if err := p.gh(it.Repo).(AutoMerger).CancelAutoMerge(ctx, it.PRNumber); err != nil {
			p.warn(p.handBack(ctx, it, "turning off auto-merge to bring it up to date failed", true), "handBack")
			return
		}
	}
	now, tip, err := p.syncBase(ctx, it, ms.Head)
	if err != nil {
		p.warn(p.handBack(ctx, it, fmt.Sprintf("%s moved and it couldn't be brought up to date: %s", base, SafeText(err.Error(), 200)), true), "handBack")
		return
	}
	if err := p.recordMergeOn(ctx, it, it.PRNumber, "auto_merge_wait", mergeState{Head: now, Base: tip}, nil); err != nil {
		why := "the auto-merge breaker for " + it.Repo + " tripped"
		if !errors.Is(err, errBreakerTripped) {
			why = "recording it failed: " + SafeText(err.Error(), 200)
		}
		p.warn(p.handBack(ctx, it, why, true), "handBack")
		return
	}
	if now == ms.Head && ms.Kind == "auto_merge" {
		// Already current after all (a base reset, say): back on as it was.
		p.enableAt(ctx, it, mergeState{Head: now, Base: tip})
	}
}

// recordMerge records a factory PR's merge, for the revert watch and the
// revert rate, as of when the forge says it merged.
func (p *Pipeline) recordMerge(ctx context.Context, it *store.WorkItem, repo string, pr *github.PR, auto bool) {
	branch := pr.Head.Ref
	if branch == "" {
		branch = p.branchFor(it)
	}
	at := guardNow()
	if pr.MergedAt != nil && !pr.MergedAt.IsZero() {
		at = *pr.MergedAt
	}
	p.warn(p.Store.RecordMerge(ctx, store.FactoryMerge{Repo: repo, PR: pr.Number, ItemID: it.ID, MergeSHA: pr.MergeCommit, SquashSHA: pr.SquashCommit, HeadSHA: pr.Head.SHA,
		Branch: branch, Title: pr.Title, Auto: auto, MergedAt: stamp(at)}), "RecordMerge")
}
