package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
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
// keeps only the changes on that list that still leave a valid
// factory.yaml, and stores the proposal for a human to apply or dismiss.
// It never changes factory.yaml itself.

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

// Tune queues a tuning run for the period since the last one. Unless
// force is set, a period with fewer than tuning.min_items builds is
// skipped, with a notification saying so. It returns the job's id, or 0
// when it skipped.
func (p *Pipeline) Tune(ctx context.Context, reason string, force bool) (int64, error) {
	p.tuneMu.Lock()
	defer p.tuneMu.Unlock()
	for _, st := range []state.JobState{state.Queued, state.Running} {
		if n, err := p.Store.CountJobs(ctx, st, "tune"); err != nil || n > 0 {
			if err == nil {
				err = ErrTuneQueued
			}
			return 0, err
		}
	}
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
	return p.Store.EnqueueJob(ctx, store.Job{Kind: "tune", Harness: r.Harness, Model: r.Model, Effort: r.Effort, Fast: r.FastFor(r.Harness, r.Model),
		Skillset: r.Skills, Route: "tune", BudgetUSD: p.Cfg.FastBudget(p.Cfg.Tuning.Budget, r.FastFor(r.Harness, r.Model)), Priority: 10, Input: string(b)})
}

// tunePeriodStart is where the last tuning run's period ended, or one
// cadence back when none has run.
func (p *Pipeline) tunePeriodStart(ctx context.Context, to time.Time) (time.Time, error) {
	at, err := p.Store.Marked(ctx, markTunePeriod)
	if err != nil {
		return time.Time{}, err
	}
	if t, err := time.Parse(store.TimeFormat, at); err == nil {
		return t, nil
	}
	every := p.Cfg.Tuning.Every
	if every <= 0 {
		every = 720 * time.Hour
	}
	return to.Add(-every), nil
}

// tuneTrigger runs on each tick: a tuning run is due once tuning.every
// has passed since the last one. The clock starts the first time the
// daemon sees tuning on, and is kept in the store across restarts.
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
		if err != nil {
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

// tuneVerdict is the tune job's answer.
type tuneVerdict struct {
	Summary string `json:"summary"`
	Changes []struct {
		Path  []string `json:"path"`
		Value any      `json:"value"`
		Why   string   `json:"why"`
	} `json:"changes"`
	Ideas []string `json:"ideas"`
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

	asked := make([]store.ProposedChange, len(v.Changes))
	for i, c := range v.Changes {
		asked[i] = store.ProposedChange{Path: c.Path, Value: c.Value, Why: strings.TrimSpace(c.Why)}
	}
	cfgDir, _ := filepath.Abs(filepath.Dir(p.ConfigPath))
	edited, kept, dropped := ProposeEdit(src, cfgDir, asked)
	ideas := []string{}
	for _, idea := range v.Ideas {
		if s := strings.TrimSpace(idea); s != "" {
			ideas = append(ideas, s)
		}
	}
	prop := store.Proposal{JobID: &j.ID, Summary: strings.TrimSpace(v.Summary), Changes: kept, Dropped: dropped, Ideas: ideas,
		Diff: config.UnifiedDiff(filepath.Base(p.ConfigPath), src, edited), PeriodFrom: in.From, PeriodTo: in.To}
	id, err := p.Store.AddProposal(ctx, prop)
	if err != nil {
		p.notify(ctx, "Tuning proposal could not be saved: "+err.Error())
		return err
	}
	prURL := ""
	if p.Cfg.Tuning.Repo != "" && len(kept) > 0 {
		prURL, err = p.tuningPR(ctx, j, prop, edited)
		if err != nil {
			p.log().Warn("tuning PR", "err", err)
			prURL = ""
			prop.Summary += "\n\n(The draft PR could not be opened: " + truncate(err.Error(), 200) + ")"
		} else if prURL != "" {
			p.warn(p.Store.SetProposalPR(ctx, id, prURL), "SetProposalPR")
		}
	}
	p.notify(ctx, tuneMessage(prop, prURL))
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
			if tunable(s.Path) {
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

// tunable says whether the tuning loop may propose a change at path: any
// setting the dashboard may change, except the loop's own.
func tunable(path []string) bool { return len(path) > 0 && path[0] != "tuning" }

// ProposeEdit applies proposed changes to a factory.yaml's bytes, one at
// a time, keeping each only if it is a setting the dashboard may change
// (see config.Settings), its value fits, and the file still loads. It
// returns the edited file, the changes kept and the ones dropped, each
// with the reason. dir is the file's directory, for validation.
func ProposeEdit(src []byte, dir string, changes []store.ProposedChange) (out []byte, kept, dropped []store.ProposedChange) {
	out = src
	kept, dropped = []store.ProposedChange{}, []store.ProposedChange{}
	drop := func(c store.ProposedChange, why string) {
		c.Reason = why
		dropped = append(dropped, c)
	}
	for _, c := range changes {
		groups, err := config.Settings(out)
		if err != nil {
			drop(c, err.Error())
			continue
		}
		s, ok := config.FindSetting(groups, c.Path)
		if !ok || !tunable(c.Path) {
			drop(c, strings.Join(c.Path, ".")+" isn't a setting tuning may change")
			continue
		}
		v, err := s.Coerce(c.Value)
		if err != nil {
			drop(c, err.Error())
			continue
		}
		next, err := config.Edit(out, []config.Change{{Path: c.Path, Value: v}})
		if err != nil {
			drop(c, err.Error())
			continue
		}
		if bytes.Equal(next, out) {
			drop(c, "factory.yaml already says that")
			continue
		}
		if _, err := config.ParseIn(next, dir); err != nil {
			drop(c, "factory.yaml would not load: "+truncate(err.Error(), 300))
			continue
		}
		out = next
		kept = append(kept, c)
	}
	return out, kept, dropped
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
func tuneMessage(prop store.Proposal, prURL string) string {
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
	if prURL != "" {
		b.WriteString("\nDraft PR: " + prURL)
	}
	return b.String()
}

// tuningBody is the draft PR's description.
func tuningBody(prop store.Proposal) string {
	var b strings.Builder
	b.WriteString(prop.Summary + "\n\n## Changes\n\n")
	for _, c := range prop.Changes {
		fmt.Fprintf(&b, "- `%s`: `%s`. %s\n", strings.Join(c.Path, "."), showValue(c.Value), c.Why)
	}
	if len(prop.Ideas) > 0 {
		b.WriteString("\n## Ideas for prompts and skills\n\n")
		for _, idea := range prop.Ideas {
			b.WriteString("- " + idea + "\n")
		}
	}
	if len(prop.Dropped) > 0 {
		b.WriteString("\n## Dropped\n\n")
		for _, c := range prop.Dropped {
			fmt.Fprintf(&b, "- `%s`: %s\n", strings.Join(c.Path, "."), c.Reason)
		}
	}
	b.WriteString("\n_Proposed by the factory's tuning loop from its metrics. Check model changes with `factory eval` first. The factory never merges this PR._\n")
	return b.String()
}

// tuningPR opens (or updates) a draft PR with the proposal applied to the
// repo's copy of factory.yaml at tuning.path, on a branch of the day.
// Where the repo has no such file, it gets the edited local one.
func (p *Pipeline) tuningPR(ctx context.Context, j *store.Job, prop store.Proposal, edited []byte) (string, error) {
	repo := p.Cfg.Tuning.Repo
	rc := p.repoConf(repo)
	path, err := config.CleanFilePath(p.Cfg.Tuning.Path)
	if err != nil {
		return "", err
	}
	branch := "factory/tuning-" + time.Now().UTC().Format("2006-01-02")
	g := p.git(repo)
	ws, err := g.Prepare(ctx, p.jobDir(j.ID), rc.BaseBranch, branch, true, false)
	if err != nil {
		return "", err
	}
	file := filepath.Join(ws.Repo, filepath.FromSlash(path))
	want := edited
	if theirs, err := os.ReadFile(file); err == nil {
		changes := make([]config.Change, 0, len(prop.Changes))
		groups, err := config.Settings(theirs)
		if err != nil {
			return "", fmt.Errorf("%s in %s: %w", path, repo, err)
		}
		for _, c := range prop.Changes {
			s, ok := config.FindSetting(groups, c.Path)
			if !ok {
				return "", fmt.Errorf("%s in %s has no %s", path, repo, strings.Join(c.Path, "."))
			}
			v, err := s.Coerce(c.Value)
			if err != nil {
				return "", err
			}
			changes = append(changes, config.Change{Path: c.Path, Value: v})
		}
		if want, err = config.Edit(theirs, changes); err != nil {
			return "", err
		}
		if bytes.Equal(want, theirs) {
			return "", nil // the repo already says this
		}
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(file, want, 0o644); err != nil {
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
		if pr, err = gh.CreatePR(ctx, "Tuning: "+firstLine(truncate(prop.Summary, 80)), body, branch, rc.BaseBranch, true); err != nil {
			return "", err
		}
	} else {
		p.warn(gh.UpdatePRBody(ctx, pr.Number, body), "UpdatePRBody")
	}
	p.warn(gh.LabelPR(ctx, pr.Number, LabelAwaitingYou), "LabelPR")
	return pr.URL, nil
}
