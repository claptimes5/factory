package pipeline

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/claptimes5/factory/internal/github"
	"github.com/claptimes5/factory/internal/store"
)

// Sources find work on their own: unresolved Sentry errors, open
// Dependabot security alerts, and comments marked TODO(factory). On its
// cadence each reads its repos and files one issue per thing it found,
// labeled for the source and never with the intake label: nothing is
// built until someone with write access adds that label, through the
// usual approval gate. When what an issue was filed for goes away (the
// error resolved, the alert fixed, the comment deleted) while nobody has
// approved the issue, the factory comments and closes it.

// Sources.
const (
	SourceSentry     = "sentry"
	SourceDependabot = "dependabot"
	SourceTODOs      = "todos"
)

// Labels on the issues each source files.
const (
	LabelFromSentry     = "factory:from-sentry"
	LabelFromDependabot = "factory:from-dependabot"
	LabelFromTODO       = "factory:from-todo"
)

// SourceNames lists the sources in the order they're shown.
var SourceNames = []string{SourceSentry, SourceDependabot, SourceTODOs}

// SourceLabel is the label on the issues a source files.
func SourceLabel(src string) string {
	switch src {
	case SourceSentry:
		return LabelFromSentry
	case SourceDependabot:
		return LabelFromDependabot
	}
	return LabelFromTODO
}

// sourceTitle names a source for people.
func sourceTitle(src string) string {
	switch src {
	case SourceSentry:
		return "Sentry"
	case SourceDependabot:
		return "Dependabot"
	}
	return "TODOs"
}

// finding is one thing a source found to file an issue for.
type finding struct {
	Key   string // what it is, to the source: its dedupe key
	Title string
	URL   string // where it came from
	// Body is the issue's text, made only when it's filed (Sentry's reads
	// the latest event).
	Body func(ctx context.Context) string
}

// scan is what one read of a source found in a repo.
type scan struct {
	found []finding // best first
	// gone says whether what a tracked issue was filed for went away,
	// and says why in a sentence; an error means it can't tell now.
	gone   func(ctx context.Context, si store.SourceIssue) (bool, string, error)
	detail string
	maxNew int // most issues to file this read; 0 for no limit
}

// errUnreadable is a source the factory can't read for a repo, for a
// reason other than an HTTP status.
type errUnreadable string

func (e errUnreadable) Error() string { return string(e) }

// SourceRepos are the repos a source reads.
func (p *Pipeline) SourceRepos(src string) []string {
	switch src {
	case SourceSentry:
		return p.Cfg.SentryRepos()
	case SourceDependabot:
		return p.Cfg.DependabotRepos()
	}
	return p.Cfg.TODORepos()
}

// SourceEnabled says whether a source is on.
func (p *Pipeline) SourceEnabled(src string) bool {
	s := p.Cfg.Sources
	switch src {
	case SourceSentry:
		return s.Sentry.Enabled
	case SourceDependabot:
		return s.Dependabot.Enabled
	}
	return s.TODOs.Enabled
}

func (p *Pipeline) sourceEvery(src string) time.Duration {
	s := p.Cfg.Sources
	switch src {
	case SourceSentry:
		return s.Sentry.Every.Duration
	case SourceDependabot:
		return s.Dependabot.Every.Duration
	}
	return s.TODOs.Every.Duration
}

// sourcesTrigger reads each source that's on and due. A restart doesn't
// make one due: the last read is in the store. With async, each read runs
// on its own, so a slow API or a clone doesn't hold up the daemon's loop;
// a source still reading isn't started again.
func (p *Pipeline) sourcesTrigger(ctx context.Context, now time.Time, async bool) {
	for _, src := range SourceNames {
		if !p.SourceEnabled(src) {
			continue
		}
		if p.sourceAt == nil {
			p.sourceAt = map[string]time.Time{}
		}
		if _, seen := p.sourceAt[src]; !seen {
			p.sourceAt[src] = p.lastSourceRead(ctx, src)
		}
		if now.Sub(p.sourceAt[src]) < p.sourceEvery(src) {
			continue
		}
		if !async {
			p.sourceAt[src] = now
			p.PollSource(ctx, src)
			continue
		}
		if _, busy := p.sourceBusy.LoadOrStore(src, true); busy {
			continue
		}
		p.sourceAt[src] = now
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			defer p.sourceBusy.Delete(src)
			p.PollSource(ctx, src)
		}()
	}
}

// lastSourceRead is when a source last read any of its repos, from the
// store, or zero.
func (p *Pipeline) lastSourceRead(ctx context.Context, src string) time.Time {
	sts, err := p.Store.SourceStates(ctx)
	p.warn(err, "SourceStates")
	var last time.Time
	for _, s := range sts {
		if t, err := time.Parse(store.TimeFormat, s.PolledAt); s.Source == src && err == nil && t.After(last) {
			last = t
		}
	}
	return last
}

// sweepSourceClones removes TODO scans' clones a stopped daemon left.
func (p *Pipeline) sweepSourceClones() {
	old, _ := filepath.Glob(filepath.Join(p.Cfg.Path(p.Cfg.DataDir), "todos-*"))
	for _, d := range old {
		p.warn(os.RemoveAll(d), "remove "+d)
	}
}

// PollSource reads a source once for each of its repos and files issues
// for what it finds.
func (p *Pipeline) PollSource(ctx context.Context, src string) {
	for _, repo := range p.SourceRepos(src) {
		if err := p.pollSource(ctx, src, repo); err != nil {
			p.log().Warn("source", "source", src, "repo", repo, "err", err)
		}
	}
}

func (p *Pipeline) readSource(ctx context.Context, src, repo string) (*scan, error) {
	switch src {
	case SourceSentry:
		return p.readSentry(ctx, repo)
	case SourceDependabot:
		return p.readDependabot(ctx, repo)
	}
	return p.readTODOs(ctx, repo)
}

func (p *Pipeline) pollSource(ctx context.Context, src, repo string) error {
	prev, err := p.Store.SourceState(ctx, repo, src)
	if err != nil {
		return err
	}
	st := store.SourceState{Repo: repo, Source: src, State: store.SourceOK}
	if prev != nil {
		st.Notified = prev.Notified
	}
	var sc *scan
	err = p.retried(ctx, func() error {
		var err error
		sc, err = p.readSource(ctx, src, repo)
		return err
	})
	if unreadable(err) {
		return p.sourceUnreadable(ctx, st, err.Error())
	}
	if err != nil {
		return err // an outage: read again next time
	}
	if st.Notified != "" {
		st.Notified = ""
		p.notify(ctx, fmt.Sprintf("%s: the factory can read %s again.", sourceTitle(src), repo))
	}
	st.Found, st.Detail = len(sc.found), sc.detail
	tracked, err := p.Store.SourceIssues(ctx, repo, src)
	if err != nil {
		return err
	}
	pr, err := p.prepare(ctx, src, repo, sc, tracked)
	if err != nil {
		return err
	}
	p.act.Lock()
	err = p.fileFindings(ctx, src, repo, tracked, pr)
	p.act.Unlock()
	if err != nil {
		return err
	}
	return p.Store.SetSourceState(ctx, st)
}

// unreadable is an error saying the factory may not read a source: a
// bad or missing token, no permission, or nothing there (main health's
// denied). A rate limit isn't one.
func unreadable(err error) bool {
	var u errUnreadable
	return errors.As(err, &u) || denied(err)
}

// rateLimited is a refusal for a rate limit, as the client that made the
// request reads it (GitHub's 403s and 429s, Sentry's 429s).
func rateLimited(err error) bool {
	var rl interface{ RateLimit() bool }
	return errors.As(err, &rl) && rl.RateLimit()
}

// transient is an error worth trying again soon: a server error or a
// rate limit.
func transient(err error) bool {
	var sc interface{ StatusCode() int }
	return errors.As(err, &sc) && sc.StatusCode() >= 500 || rateLimited(err)
}

// retried runs f up to three times while it fails with a transient
// error, quietly.
func (p *Pipeline) retried(ctx context.Context, f func() error) error {
	wait := p.retryWait
	if wait == 0 {
		wait = 2 * time.Second
	}
	var err error
	for i := 1; i <= 3; i++ {
		if err = f(); !transient(err) || i == 3 {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(i) * wait):
		}
	}
	return err
}

// sourceUnreadable records a source the factory can't read for a repo,
// and says so once.
func (p *Pipeline) sourceUnreadable(ctx context.Context, st store.SourceState, why string) error {
	st.State, st.Detail = store.SourceUnreadable, firstLine(why)
	if st.Notified != store.SourceUnreadable {
		st.Notified = store.SourceUnreadable
		p.notify(ctx, fmt.Sprintf("%s: can't read it for %s (%s), so the factory files no issues from it.", sourceTitle(st.Source), st.Repo, truncate(firstLine(why), 200)))
	}
	return p.Store.SetSourceState(ctx, st)
}

// markerRE finds the line the factory puts at the end of each issue it
// files from a source, to find it again.
var markerRE = regexp.MustCompile(`<!-- factory-source: ([a-z]+) ([A-Za-z0-9._-]+) -->`)

// keyRE is what a source's key may hold.
var keyRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

func sourceMarker(src, key string) string {
	return fmt.Sprintf("<!-- factory-source: %s %s -->", src, key)
}

// selfer is a client that knows who the factory acts as; both real ones
// do.
type selfer interface {
	Self(ctx context.Context) (string, error)
}

// prepared is what a read leads to, worked out before the factory takes
// the lock it acts under: which tracked issues are done with, which
// issues on the forge it filed before, and the new ones' text.
type prepared struct {
	self    string
	gone    map[string]string // key -> why
	onForge map[string]github.Issue
	file    []finding
	bodies  map[string]string
	held    int // left for the cap
}

// prepare reads what it needs to act on a scan: the gone checks, the
// open issues and the new issues' text (Sentry's reads the latest event).
func (p *Pipeline) prepare(ctx context.Context, src, repo string, sc *scan, tracked []store.SourceIssue) (*prepared, error) {
	gh := p.gh(repo)
	pr := &prepared{gone: map[string]string{}, onForge: map[string]github.Issue{}, bodies: map[string]string{}}
	if s, ok := gh.(selfer); ok {
		self, err := s.Self(ctx)
		if err != nil {
			return nil, err
		}
		pr.self = self
	}
	known := map[string]bool{}
	for _, si := range tracked {
		known[si.Key] = true
		gone, why, err := sc.gone(ctx, si)
		if err != nil {
			p.warn(err, src+": is it resolved")
			continue
		}
		if gone {
			pr.gone[si.Key] = why
		}
	}
	open, err := gh.ListIssues(ctx, SourceLabel(src))
	if err != nil {
		return nil, err
	}
	for _, is := range open {
		// Only the factory's own, by the last line: the text above it may
		// quote anything, and anyone who can label could file a marker.
		body := strings.TrimSpace(is.Body)
		if m := markerRE.FindStringSubmatch(body[strings.LastIndex(body, "\n")+1:]); m != nil && m[1] == src && !known[m[2]] && pr.self != "" && is.Author == pr.self {
			pr.onForge[m[2]] = is
		}
	}
	room, err := p.capRoom(ctx, repo)
	if err != nil {
		return nil, err
	}
	var fresh []finding
	for _, f := range sc.found {
		if _, there := pr.onForge[f.Key]; !known[f.Key] && !there {
			known[f.Key] = true
			fresh = append(fresh, f)
		}
	}
	if sc.maxNew > 0 && len(fresh) > sc.maxNew {
		fresh = fresh[:sc.maxNew] // the next read files the rest
	}
	if len(fresh) > room {
		pr.held, fresh = len(fresh)-room, fresh[:room]
	}
	for _, f := range fresh {
		pr.bodies[f.Key] = f.Body(ctx) + "\n" + approveLine(src, p.Cfg.Intake.Label) + "\n" + sourceMarker(src, f.Key) + "\n"
	}
	pr.file = fresh
	return pr, nil
}

// capRoom is how many more issues the sources may file for a repo today.
func (p *Pipeline) capRoom(ctx context.Context, repo string) (int, error) {
	since := time.Now().UTC().Format("2006-01-02") + "T00:00:00"
	n, err := p.Store.CountEventsFor(ctx, "source_filed", repo, since)
	return max(0, p.Cfg.Sources.DailyIssueCap-n), err
}

// fileFindings closes the issues whose findings went away, takes back
// the ones it filed before the store knew of them, and files the new
// ones within the day's cap.
func (p *Pipeline) fileFindings(ctx context.Context, src, repo string, tracked []store.SourceIssue, pr *prepared) error {
	gh := p.gh(repo)
	for _, si := range tracked {
		if why, ok := pr.gone[si.Key]; ok {
			p.warn(p.retireSource(ctx, si, pr.self, why), src+": close")
		}
	}
	for key, is := range pr.onForge {
		if err := p.Store.SetSourceIssue(ctx, store.SourceIssue{Repo: repo, Source: src, Key: key, IssueNumber: is.Number, Title: is.Title, BodyHash: IssueHash(is)}); err != nil {
			return err
		}
	}
	for _, f := range pr.file {
		// Checked again under the lock: another read may have filed since.
		if room, err := p.capRoom(ctx, repo); err != nil || room == 0 {
			pr.held++
			if err != nil {
				return err
			}
			continue
		}
		body := pr.bodies[f.Key]
		num, err := gh.CreateIssue(ctx, f.Title, body, []string{SourceLabel(src)})
		if err != nil {
			return err
		}
		if err := p.Store.SetSourceIssue(ctx, store.SourceIssue{Repo: repo, Source: src, Key: f.Key, IssueNumber: num, Title: f.Title, URL: f.URL,
			BodyHash: IssueHash(github.Issue{Title: f.Title, Body: body})}); err != nil {
			return err
		}
		// Counted, or no more is filed: the cap must hold.
		data, _ := json.Marshal(map[string]any{"source": src, "key": f.Key, "issue": num})
		if err := p.Store.AddEvent(ctx, store.Event{Actor: "daemon", Type: "source_filed", Reason: repo, Data: string(data)}); err != nil {
			return err
		}
		// The title is untrusted, so the notice leaves it out.
		p.notify(ctx, fmt.Sprintf("Filed %s from %s. It waits for you: add `%s` to build it.", p.ref(repo, num), sourceFrom(src), p.Cfg.Intake.Label))
	}
	if pr.held > 0 {
		return p.capHeld(ctx, repo, pr.held)
	}
	return nil
}

// capHeld says, once a day per repo, that the cap held issues back.
func (p *Pipeline) capHeld(ctx context.Context, repo string, held int) error {
	since := time.Now().UTC().Format("2006-01-02") + "T00:00:00"
	if n, err := p.Store.CountEventsFor(ctx, "source_capped", repo, since); err != nil || n > 0 {
		return err
	}
	p.warn(p.Store.AddEvent(ctx, store.Event{Actor: "daemon", Type: "source_capped", Reason: repo}), "AddEvent")
	p.notify(ctx, fmt.Sprintf("Sources: %d %s for %s not filed today: %d were filed (sources.daily_issue_cap). They're filed later if they're still there.",
		held, plural(held, "issue", "issues"), repo, p.Cfg.Sources.DailyIssueCap))
	return nil
}

// sourceFrom says where a source's issues come from.
func sourceFrom(src string) string {
	switch src {
	case SourceSentry:
		return "Sentry"
	case SourceDependabot:
		return "a Dependabot alert"
	}
	return "a TODO comment"
}

// approveLine is the issue's line on how to approve it.
func approveLine(src, intake string) string {
	return fmt.Sprintf("**To build it:** someone with write access adds `%s`. The factory filed this from %s and builds nothing until then.", intake, sourceFrom(src))
}

// approved says whether an issue was approved: it has the intake or the
// epic label, or the factory has an item for it.
func (p *Pipeline) approved(ctx context.Context, repo string, is *github.Issue) (bool, error) {
	if hasLabel(is.Labels, p.Cfg.Intake.Label) || hasLabel(is.Labels, LabelEpic) {
		return true, nil
	}
	_, err := p.Store.ItemByIssue(ctx, repo, is.Number)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// issueCloser is a client that can close issues; both real ones can.
// GitHub's can also say nobody built it.
type issueCloser interface {
	CloseIssue(ctx context.Context, n int) error
}

type notPlannedCloser interface {
	CloseNotPlanned(ctx context.Context, n int) error
}

// retireSource handles an issue whose finding went away. Still open,
// still labeled, still the factory's text and nobody approved it: it
// closes it and says why. Approved, it's left to its build and tracked
// until it closes, so the finding isn't filed twice. Closed, edited by
// someone, not filed by the factory, or with the label taken off, it's
// forgotten and left alone.
func (p *Pipeline) retireSource(ctx context.Context, si store.SourceIssue, self, why string) error {
	gh := p.gh(si.Repo)
	is, err := gh.GetIssue(ctx, si.IssueNumber)
	switch {
	case github.IsNotFound(err):
		is = &github.Issue{Number: si.IssueNumber, State: "closed"} // deleted
	case err != nil:
		return err
	}
	ours := self != "" && is.Author == self && si.BodyHash != "" && IssueHash(*is) == si.BodyHash
	if is.State != "closed" && hasLabel(is.Labels, SourceLabel(si.Source)) && ours {
		ok, err := p.approved(ctx, si.Repo, is)
		if err != nil || ok {
			return err
		}
		closeIt := func(context.Context, int) error { return errors.New("its client can't close issues") }
		if c, can := gh.(issueCloser); can {
			closeIt = c.CloseIssue
		}
		if c, can := gh.(notPlannedCloser); can {
			closeIt = c.CloseNotPlanned
		}
		// Closed first, so a close that keeps failing doesn't comment
		// every read.
		if err := closeIt(ctx, si.IssueNumber); err != nil {
			return fmt.Errorf("%s#%d: %w", si.Repo, si.IssueNumber, err) // tracked still: tried again next time
		}
		p.warn(gh.Comment(ctx, si.IssueNumber, "**Factory:** "+why+" Nobody approved this issue, so the factory closed it."), "Comment")
		p.log().Info("source issue closed", "source", si.Source, "repo", si.Repo, "issue", si.IssueNumber)
	}
	return p.Store.ClearSourceIssue(ctx, si.Repo, si.Source, si.Key)
}

// clip is untrusted text fit for inline code: one line, no backticks, no
// invisible or direction-changing characters, at most n runes.
func clip(s string, n int) string { return strings.ReplaceAll(SafeText(s, n), "`", "'") }

// refRE is an issue reference or a mention in a title.
var refRE = regexp.MustCompile(`([#@])([\w])`)

// titleText is untrusted text fit for an issue's title, which becomes
// the factory's commit message and PR title: clipped, and with no "#123"
// to close or "@name" to mention.
func titleText(s string, n int) string { return refRE.ReplaceAllString(clip(s, n), "$1 $2") }

// plainRE is what an id or a date from an API may hold in an issue's
// text outside a fence.
var plainRE = regexp.MustCompile(`[^A-Za-z0-9 :._+-]`)

func plain(s string) string { return clip(plainRE.ReplaceAllString(s, ""), 60) }
