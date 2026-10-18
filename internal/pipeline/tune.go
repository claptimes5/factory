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
	"strings"
	"time"

	"github.com/claptimes5/factory/internal/config"
	"github.com/claptimes5/factory/internal/metrics"
	"github.com/claptimes5/factory/internal/state"
	"github.com/claptimes5/factory/internal/store"
)

// The tuning loop: on a cadence, a read-only agent reads the metrics for
// the period since the last run (and the one before, for trends) and
// proposes changes to the settings the dashboard may edit. The daemon
// keeps only the changes config.Proposed accepts, and stores the proposal
// for a human to apply or dismiss. It never changes factory.yaml itself.

// Marks the tuning loop keeps in the store.
const (
	markTuneDue    = "tuning.due"    // when the cadence last fired
	markTunePeriod = "tuning.period" // where the next run's period starts
)

// TuneInput is carried by a tune job: why it runs and the period it reads.
type TuneInput struct {
	Reason string `json:"reason"`
	From   string `json:"from"` // TimeFormat
	To     string `json:"to"`
}

// ErrTuneQueued is returned when a tune job is already queued or running.
var ErrTuneQueued = errors.New("a tuning run is already queued or running")

// CheckTunePrompt fails when tuning is on and its prompt is missing, so
// the daemon says so at start rather than a month later.
func (p *Pipeline) CheckTunePrompt() error {
	if p.Cfg.Tuning.Every <= 0 {
		return nil
	}
	path := filepath.Join(p.Cfg.Path(p.Cfg.PromptsDir), "tune.md")
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("tuning is on (tuning.every: %s) but its prompt is missing: %w; restore it, or set tuning.every: 0", p.Cfg.Tuning.Every, err)
	}
	return nil
}

// tuneEvery is the cadence, or the default one when tuning is off (for
// `factory tune`, which still needs a period).
func (p *Pipeline) tuneEvery() time.Duration {
	if p.Cfg.Tuning.Every > 0 {
		return p.Cfg.Tuning.Every
	}
	return 720 * time.Hour
}

// Tune queues a tuning run for the period since the last one. Unless
// force is set, a period with fewer than tuning.min_items builds is
// skipped, with a notification saying so. It returns the job's id, or 0
// when it skipped. At most one tune job is queued or running, whichever
// process asks (the daemon, or `factory tune`).
func (p *Pipeline) Tune(ctx context.Context, reason string, force bool) (int64, error) {
	to := time.Now().UTC()
	from, err := p.tunePeriodStart(ctx, to)
	if err != nil {
		return 0, err
	}
	if !force {
		_, b, err := metrics.Period(ctx, p.Store, from, to)
		if err != nil {
			return 0, err
		}
		if b.Implemented < p.Cfg.Tuning.MinItems {
			msg := fmt.Sprintf("Tuning skipped: %d %s built since %s, fewer than tuning.min_items (%d). It looks again in %s.",
				b.Implemented, plural(b.Implemented, "item was", "items were"), from.Format("Jan 2"), p.Cfg.Tuning.MinItems, p.Cfg.Tuning.Every)
			p.warn(p.Store.AddEvent(ctx, store.Event{Actor: "daemon", Type: "tune_skipped", Reason: msg}), "AddEvent")
			p.notify(ctx, msg)
			return 0, nil
		}
	}
	r := p.Cfg.Routing["tune"]
	b, _ := json.Marshal(TuneInput{Reason: reason, From: from.Format(store.TimeFormat), To: to.Format(store.TimeFormat)})
	id, queued, err := p.Store.EnqueueSole(ctx, store.Job{Kind: "tune", Harness: r.Harness, Model: r.Model, Effort: r.Effort, Fast: r.FastFor(r.Harness, r.Model),
		Skillset: r.Skills, Route: "tune", BudgetUSD: p.Cfg.FastBudget(p.Cfg.Tuning.Budget, r.FastFor(r.Harness, r.Model)), Priority: 10, Input: string(b)})
	if err == nil && !queued {
		err = ErrTuneQueued
	}
	return id, err
}

// tunePeriodStart is where the last tuning run's period ended, or one
// cadence back when none has run. It reaches back two cadences at most,
// so a factory that went untuned for long reads its recent work, and a
// start in the future (the clock moved back) is one cadence back.
func (p *Pipeline) tunePeriodStart(ctx context.Context, to time.Time) (time.Time, error) {
	every := p.tuneEvery()
	at, err := p.Store.Marked(ctx, markTunePeriod)
	if err != nil {
		return time.Time{}, err
	}
	t, err := time.Parse(store.TimeFormat, at)
	switch {
	case err != nil, !t.Before(to):
		return to.Add(-every), nil
	case t.Before(to.Add(-2 * every)):
		return to.Add(-2 * every), nil
	}
	return t, nil
}

// tuneBudget says why a tuning run can't spend now, or "": it waits for
// both today's build budget and the orchestration budget, which its spend
// counts toward.
func (p *Pipeline) tuneBudget(ctx context.Context) string {
	if spent, err := p.buildSpend(ctx); err != nil || spent >= p.Cfg.Budgets.DailyTotal {
		return fmt.Sprintf("today's budget is spent ($%.2f of $%.2f)", spent, p.Cfg.Budgets.DailyTotal)
	}
	if spent, err := p.orchSpend(ctx); err != nil || spent >= p.Cfg.Orchestrator.DailyBudget {
		return fmt.Sprintf("today's orchestration budget is spent ($%.2f of $%.2f)", spent, p.Cfg.Orchestrator.DailyBudget)
	}
	return ""
}

// tuneTrigger runs on each tick: a tuning run is due once tuning.every
// has passed since the last one. The clock starts the first time the
// daemon sees tuning on, and is kept in the store across restarts. A
// clock more than two cadences old (tuning was off, or the daemon was
// down) or in the future (the clock moved back) starts again from now.
func (p *Pipeline) tuneTrigger(ctx context.Context, now time.Time) {
	every := p.Cfg.Tuning.Every
	if every <= 0 {
		return
	}
	if p.tuneDue.IsZero() {
		at, err := p.Store.Marked(ctx, markTuneDue)
		if err != nil {
			return
		}
		t, err := time.Parse(store.TimeFormat, at)
		if err != nil || t.After(now) || now.Sub(t) > 2*every {
			t = now
			if err := p.Store.Mark(ctx, markTuneDue, now.UTC().Format(store.TimeFormat)); err != nil {
				return
			}
		}
		p.tuneDue = t
	}
	if now.Sub(p.tuneDue) < every {
		return
	}
	// Over budget, it waits for the budget rather than a whole cadence.
	if why := p.tuneBudget(ctx); why != "" {
		if day := now.UTC().Format("2006-01-02"); p.tuneHeld != day {
			p.tuneHeld = day
			p.notify(ctx, "Tuning is due, but "+why+". It runs once there's budget again.")
		}
		return
	}
	// Marked first: a run that fails, or skips, waits for the next cadence.
	if err := p.Store.Mark(ctx, markTuneDue, now.UTC().Format(store.TimeFormat)); err != nil {
		p.warn(err, "tune")
		return
	}
	p.tuneDue = now
	if _, err := p.Tune(ctx, "scheduled run", false); !errors.Is(err, ErrTuneQueued) {
		p.warn(err, "tune")
	}
}

// TunePrompt is what prompts/tune.md is given, each part as JSON.
type TunePrompt struct {
	From, To  string // the period, YYYY-MM-DD
	Current   string // the metrics report for the period
	Previous  string // the same for the period before it
	Breakdown string // the period's numbers per size and per starting model, and park reasons
	PrevBreak string // the same for the period before
	Settings  string // what may change, with current values
}

// tuneSchema is the shape the tune job's answer must have.
const tuneSchema = `{"type":"object","additionalProperties":false,"required":["summary","changes","ideas"],"properties":{` +
	`"summary":{"type":"string"},` +
	`"changes":{"type":"array","items":{"type":"object","additionalProperties":false,"required":["path","value","why"],"properties":{"path":{"type":"array","items":{"type":"string"}},"value":{},"why":{"type":"string"}}}},` +
	`"ideas":{"type":"array","items":{"type":"string"}}}}`

// tuneVerdict is the tune job's answer. A value is kept raw, so a
// missing one isn't read as null, which would put the default back.
type tuneVerdict struct {
	Summary string `json:"summary"`
	Changes []struct {
		Path  []string        `json:"path"`
		Value json.RawMessage `json:"value"`
		Why   string          `json:"why"`
	} `json:"changes"`
	Ideas []string `json:"ideas"`
}

// proposed reads the verdict's changes: one per path (the last), each
// with a path of non-empty keys and a value. The others come back
// dropped, with why.
func (v tuneVerdict) proposed() (asked, dropped []store.ProposedChange) {
	last := map[string]int{}
	for i, c := range v.Changes {
		last[strings.Join(c.Path, "\x00")] = i
	}
	for i, c := range v.Changes {
		pc := store.ProposedChange{Path: c.Path, Why: strings.TrimSpace(c.Why)}
		if pc.Path == nil {
			pc.Path = []string{}
		}
		if len(c.Value) > 0 {
			_ = json.Unmarshal(c.Value, &pc.Value)
		}
		switch {
		case len(c.Path) == 0 || strings.Contains("\x00"+strings.Join(c.Path, "\x00")+"\x00", "\x00\x00"):
			pc.Reason = "the change names no setting"
		case len(c.Value) == 0:
			pc.Reason = "the change gives no value"
		case last[strings.Join(c.Path, "\x00")] != i:
			pc.Reason = "a later change to the same setting replaces it"
		default:
			asked = append(asked, pc)
			continue
		}
		dropped = append(dropped, pc)
	}
	return asked, dropped
}

func (p *Pipeline) runTune(ctx context.Context, j *store.Job) error {
	dir := p.jobDir(j.ID)
	out := filepath.Join(dir, "out")
	var in TuneInput
	_ = json.Unmarshal([]byte(j.Input), &in)
	fail := func(run agentRun, st state.JobState, msg string) error {
		if p.stopIfCancelled(ctx, j, nil, run) {
			return nil
		}
		p.finish(ctx, j, run, st, "", msg)
		p.notify(ctx, "Tuning run failed: "+truncate(msg, 300))
		return nil // the next cadence starts a fresh run
	}
	if p.ConfigPath == "" {
		return fail(agentRun{}, state.Failed, "the daemon has no factory.yaml path to tune")
	}
	src, err := os.ReadFile(p.ConfigPath)
	if err != nil {
		return fail(agentRun{}, state.Failed, err.Error())
	}
	from, err1 := time.Parse(store.TimeFormat, in.From)
	to, err2 := time.Parse(store.TimeFormat, in.To)
	if err1 != nil || err2 != nil || !from.Before(to) {
		return fail(agentRun{}, state.Failed, "tune job has no period")
	}
	data, err := p.tunePrompt(ctx, src, from, to)
	if err != nil {
		return fail(agentRun{}, state.Failed, err.Error())
	}
	prompt, err := p.renderStep(j, "tune", nil, PromptData{Tune: data})
	if err != nil {
		return fail(agentRun{}, state.Failed, err.Error())
	}
	// It reads only what the prompt gives it: an empty directory to run in.
	work := filepath.Join(dir, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		return fail(agentRun{}, state.Failed, err.Error())
	}
	run := p.runAgentWith(ctx, j, work, out, prompt, tuneSchema, p.Cfg.Timeout("tune"), agentExtras{})
	if p.stopIfCancelled(ctx, j, nil, run) {
		return nil
	}
	if st, msg, bad := runFailure(run); bad {
		return fail(run, st, msg)
	}
	var v tuneVerdict
	if err := json.Unmarshal([]byte(extractJSON(run.final)), &v); err != nil || strings.TrimSpace(v.Summary) == "" {
		return fail(run, state.Failed, "tune verdict: missing summary")
	}
	norm, _ := json.Marshal(v)
	p.finish(ctx, j, run, state.Succeeded, string(norm), "")
	p.warn(p.Store.Mark(ctx, markTunePeriod, in.To), "Mark")

	asked, dropped := v.proposed()
	changes := make([]config.Change, len(asked))
	for i, c := range asked {
		changes[i] = config.Change{Path: c.Path, Value: c.Value}
	}
	cfgDir, _ := filepath.Abs(filepath.Dir(p.ConfigPath))
	edited, why := config.Proposed(src, cfgDir, changes)
	kept := []store.ProposedChange{}
	for i, c := range asked {
		if why[i] == "" {
			kept = append(kept, c)
		} else {
			c.Reason = why[i]
			dropped = append(dropped, c)
		}
	}
	// A change to how review judges must not make it judge worse.
	n := len(kept)
	kept, dropped, evalLine := p.gateReview(ctx, src, cfgDir, kept, dropped)
	if len(kept) != n {
		changes = changes[:0]
		for _, c := range kept {
			changes = append(changes, config.Change{Path: c.Path, Value: c.Value})
		}
		edited, _ = config.Proposed(src, cfgDir, changes)
	}
	ideas := []string{}
	for _, idea := range v.Ideas {
		if s := strings.TrimSpace(idea); s != "" {
			ideas = append(ideas, s)
		}
	}
	summary := strings.TrimSpace(v.Summary)
	if evalLine != "" {
		summary += "\n\n" + evalLine
	}
	prop := store.Proposal{JobID: &j.ID, Summary: summary, Changes: kept, Dropped: dropped, Ideas: ideas,
		Diff: config.UnifiedDiff(filepath.Base(p.ConfigPath), src, edited), PeriodFrom: in.From, PeriodTo: in.To}
	if dropped == nil {
		prop.Dropped = []store.ProposedChange{}
	}
	// The PR first, so the proposal is stored with its link, or with why
	// there is none.
	if p.Cfg.Tuning.Repo != "" && len(kept) > 0 {
		url, err := p.tuningPR(ctx, j, prop)
		if err != nil {
			p.log().Warn("tuning PR", "err", err)
			prop.Summary += "\n\nThe draft PR on " + p.Cfg.Tuning.Repo + " was not opened: " + truncate(err.Error(), 300)
		}
		prop.PRURL = url
	}
	if _, err := p.Store.AddProposal(ctx, prop); err != nil {
		p.notify(ctx, "Tuning proposal could not be saved: "+err.Error())
		return err
	}
	p.notify(ctx, tuneMessage(prop))
	return nil
}

// tunePrompt gathers the tune job's input: the reports for the period and
// the one before it, and the settings it may change.
func (p *Pipeline) tunePrompt(ctx context.Context, src []byte, from, to time.Time) (TunePrompt, error) {
	cur, curB, err := metrics.Period(ctx, p.Store, from, to)
	if err != nil {
		return TunePrompt{}, err
	}
	prev, prevB, err := metrics.Period(ctx, p.Store, from.Add(-to.Sub(from)), from)
	if err != nil {
		return TunePrompt{}, err
	}
	// The prompt says -1 is no data: nothing merged has no cost per merge.
	for _, r := range []*metrics.Report{cur, prev} {
		if r.Merged == 0 {
			r.CostPerMerged = -1
		}
		for i := range r.Weekly {
			if r.Weekly[i].Merged == 0 {
				r.Weekly[i].CostPerMerged = -1
			}
		}
	}
	groups, err := config.Settings(src)
	if err != nil {
		return TunePrompt{}, err
	}
	type setting struct {
		Path    []string `json:"path"`
		Label   string   `json:"label"`
		Help    string   `json:"help"`
		Kind    string   `json:"kind"`
		Options []string `json:"options,omitempty"`
		Value   any      `json:"value"`
		Default any      `json:"default"`
	}
	var settings []setting
	for _, g := range groups {
		for _, s := range g.Settings {
			if config.Tunable(s.Path) {
				settings = append(settings, setting{s.Path, s.Label, s.Help, s.Kind, s.Options, s.Value, s.Default})
			}
		}
	}
	js := func(v any) string {
		b, _ := json.MarshalIndent(v, "", "  ")
		return string(b)
	}
	return TunePrompt{
		From: from.Format("2006-01-02"), To: to.Format("2006-01-02"),
		Current: js(cur), Previous: js(prev), Breakdown: js(curB), PrevBreak: js(prevB), Settings: js(settings),
	}, nil
}

// showValue writes a proposed value for people: null as "the default".
func showValue(v any) string {
	if v == nil {
		return "the default"
	}
	b, _ := json.Marshal(v)
	return strings.Trim(string(b), `"`)
}

// tuneMessage is the notification for a new proposal.
func tuneMessage(prop store.Proposal) string {
	var b strings.Builder
	b.WriteString("Tuning proposal\n\n" + truncate(prop.Summary, 600))
	if len(prop.Changes) == 0 {
		b.WriteString("\n\nNo setting changes.")
	} else {
		fmt.Fprintf(&b, "\n\n%d %s:", len(prop.Changes), plural(len(prop.Changes), "change", "changes"))
		for _, c := range prop.Changes {
			fmt.Fprintf(&b, "\n- %s: %s", strings.Join(c.Path, "."), showValue(c.Value))
		}
	}
	if n := len(prop.Dropped); n > 0 {
		fmt.Fprintf(&b, "\n%d more %s dropped.", n, plural(n, "was", "were"))
	}
	if n := len(prop.Ideas); n > 0 {
		fmt.Fprintf(&b, "\n%d %s for prompts or skills.", n, plural(n, "idea", "ideas"))
	}
	b.WriteString("\n\nApply or dismiss it on the dashboard's Settings screen (/app/settings).")
	if prop.PRURL != "" {
		b.WriteString("\nDraft PR: " + prop.PRURL)
	}
	return b.String()
}

// fenced puts model text in a code block, where GitHub and GitLab link no
// issues, close none and mention no one.
func fenced(s string) string {
	f := fence(s)
	return f + "text\n" + strings.TrimRight(s, "\n") + "\n" + f + "\n"
}

// tuningBody is the draft PR's description. Everything the model wrote is
// fenced: a "Closes #12" or an @mention in it must not act.
func tuningBody(prop store.Proposal) string {
	var b strings.Builder
	b.WriteString("The factory's tuning loop proposes these changes to its settings, from its metrics. Check model changes with `factory eval` before you merge. The factory never merges this PR.\n\n## Summary\n\n")
	b.WriteString(fenced(prop.Summary))
	var changes strings.Builder
	for _, c := range prop.Changes {
		fmt.Fprintf(&changes, "%s: %s\n  why: %s\n", strings.Join(c.Path, "."), showValue(c.Value), c.Why)
	}
	b.WriteString("\n## Changes\n\n" + fenced(changes.String()))
	if len(prop.Ideas) > 0 {
		b.WriteString("\n## Ideas for prompts and skills\n\n" + fenced("- "+strings.Join(prop.Ideas, "\n- ")))
	}
	if len(prop.Dropped) > 0 {
		var d strings.Builder
		for _, c := range prop.Dropped {
			fmt.Fprintf(&d, "%s: %s\n", strings.Join(c.Path, "."), c.Reason)
		}
		b.WriteString("\n## Dropped\n\n" + fenced(d.String()))
	}
	return b.String()
}

// tuningBranch is the day's branch for the PR: factory/tuning-<date>, or
// with a suffix when that branch has commits the factory didn't make,
// which a force push would throw away.
func (p *Pipeline) tuningBranch(ctx context.Context, g *Git, ws *Workspace) (string, error) {
	base := "factory/tuning-" + time.Now().UTC().Format("2006-01-02")
	for n := 1; n <= 20; n++ {
		branch := base
		if n > 1 {
			branch = fmt.Sprintf("%s-%d", base, n)
		}
		if !g.HasBranch(ctx, ws.Repo, branch) {
			return branch, nil
		}
		if mine, err := g.onlyMine(ctx, ws.Repo, ws.BaseSHA, "refs/remotes/origin/"+branch); err != nil || mine {
			return branch, err
		}
	}
	return "", fmt.Errorf("every %s branch has commits of someone else's", base)
}

// onlyMine says whether every commit on to after from is the daemon's.
func (g *Git) onlyMine(ctx context.Context, repo, from, to string) (bool, error) {
	authors, err := g.Authors(ctx, repo, from, to)
	if err != nil {
		return false, err
	}
	for _, a := range authors {
		if !strings.EqualFold(a, g.AuthorEmail()) {
			return false, nil
		}
	}
	return true, nil
}

// tuningPR opens (or updates) a draft PR with the proposal applied to the
// repo's copy of factory.yaml at tuning.path. The file is read and
// written through an os.Root of the daemon's clone and must be a plain
// file there: a symlink in the repo can't send the write anywhere else.
// The result must load, as the daemon would load it from that checkout.
func (p *Pipeline) tuningPR(ctx context.Context, j *store.Job, prop store.Proposal) (string, error) {
	repo := p.Cfg.Tuning.Repo
	rc := p.repoConf(repo)
	rel, err := config.CleanFilePath(p.Cfg.Tuning.Path)
	if err != nil {
		return "", err
	}
	g := p.git(repo)
	ws, err := g.Prepare(ctx, p.jobDir(j.ID), rc.BaseBranch, "", true, false)
	if err != nil {
		return "", err
	}
	branch, err := p.tuningBranch(ctx, g, ws)
	if err != nil {
		return "", err
	}
	root, err := os.OpenRoot(ws.Repo)
	if err != nil {
		return "", err
	}
	defer func() { _ = root.Close() }()
	if err := plainFile(root, rel); err != nil {
		return "", fmt.Errorf("tuning.path %w", err)
	}
	theirs, err := root.ReadFile(rel)
	if err != nil {
		return "", err
	}
	changes := make([]config.Change, len(prop.Changes))
	for i, c := range prop.Changes {
		changes[i] = config.Change{Path: c.Path, Value: c.Value}
	}
	want, why := config.Proposed(theirs, filepath.Join(ws.Repo, filepath.FromSlash(path.Dir(rel))), changes)
	for i, w := range why {
		if w != "" && w != config.NoChange {
			return "", fmt.Errorf("%s in %s: %s: %s", rel, repo, strings.Join(changes[i].Path, "."), w)
		}
	}
	if string(want) == string(theirs) {
		return "", nil // the repo already says this
	}
	f, err := root.OpenFile(rel, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		return "", err
	}
	if _, err := f.Write(want); err != nil {
		f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	// The daemon's own clone is the work tree: no agent ever touched it.
	ws.Work = ws.Repo
	sha, changed, err := g.Ingest(ctx, ws, "factory: tuning proposal "+time.Now().UTC().Format("2006-01-02"))
	if err != nil || !changed {
		return "", err
	}
	if err := g.Push(ctx, ws.Repo, sha, branch, true); err != nil {
		return "", err
	}
	gh := p.gh(repo)
	body := tuningBody(prop)
	pr, err := gh.FindPR(ctx, branch)
	if err != nil {
		return "", err
	}
	if pr == nil {
		// The title is the daemon's own words: model text could mention or link.
		if pr, err = gh.CreatePR(ctx, "Tuning proposal "+time.Now().UTC().Format("2006-01-02"), body, branch, rc.BaseBranch, true); err != nil {
			return "", err
		}
	} else {
		p.warn(gh.UpdatePRBody(ctx, pr.Number, body), "UpdatePRBody")
	}
	p.warn(gh.LabelPR(ctx, pr.Number, LabelAwaitingYou), "LabelPR")
	return pr.URL, nil
}

// plainFile checks that rel is a regular file under root, reached through
// folders only: no symlink on the way or at the end.
func plainFile(root *os.Root, rel string) error {
	parts := strings.Split(rel, "/")
	for i := 1; i <= len(parts); i++ {
		at := strings.Join(parts[:i], "/")
		fi, err := root.Lstat(at)
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%s not found in the repo", rel)
		}
		if err != nil {
			return err
		}
		switch {
		case fi.Mode()&fs.ModeSymlink != 0:
			return fmt.Errorf("%s: %s is a symlink in the repo; refusing to write through it", rel, at)
		case i < len(parts) && !fi.IsDir():
			return fmt.Errorf("%s not found in the repo", rel)
		case i == len(parts) && !fi.Mode().IsRegular():
			return fmt.Errorf("%s isn't a regular file in the repo", rel)
		}
	}
	return nil
}
