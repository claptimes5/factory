// Package pipeline is the daemon's deterministic core: it takes labeled
// issues in, runs triage -> implement -> gates -> review jobs in fresh
// containers, and opens draft PRs. Agents only propose (via verdicts); every
// state change, budget check and GitHub write happens here.
package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"text/template"
	"time"

	"github.com/claptimes5/factory/internal/config"
	"github.com/claptimes5/factory/internal/gates"
	"github.com/claptimes5/factory/internal/github"
	"github.com/claptimes5/factory/internal/harness"
	"github.com/claptimes5/factory/internal/runner"
	"github.com/claptimes5/factory/internal/state"
	"github.com/claptimes5/factory/internal/store"
	"github.com/claptimes5/factory/internal/verdict"
)

// Labels the daemon manages on issues and PRs.
const (
	LabelRunning     = "factory:running"
	LabelParked      = "factory:parked"
	LabelAwaitingYou = "factory:awaiting-you"
	LabelReview      = "factory:review"
	LabelRetry       = "factory:retry" // a human adds it to send a parked item back to triage
	LabelAutoMerge   = "factory:auto-merge"
)

// Label is one GitHub label the factory reads or sets.
type Label struct{ Name, Color, Description string }

// Labels lists every label the factory uses, for `factory setup` to create.
func Labels(intake string) []Label {
	return []Label{
		{intake, "0e8a16", "Factory: pick this up"},
		{LabelRunning, "fbca04", "Factory: a job is running"},
		{LabelParked, "d93f0b", "Factory: needs a human; remove to retry"},
		{LabelAwaitingYou, "1d76db", "Factory: draft PR ready for your review"},
		{LabelAutoMerge, "0e8a16", "Factory: merges itself once its checks pass (auto_merge)"},
		{LabelRetry, "5319e7", "Factory: send a parked issue back to triage"},
		{LabelEpic, "c5def5", "Factory: split into child issues"},
		{LabelReadiness, "bfdadc", "Factory: makes the repo verifiable (from the readiness check)"},
		{riskLabel("high"), "b60205", "Factory: high risk; reviewer and critic passed it, read all of it"},
		{riskLabel("medium"), "fbca04", "Factory: medium risk; read the changes to the features it touches"},
		{riskLabel("low"), "c2e0c6", "Factory: low risk; gates and a reviewer passed it, a skim is enough"},
	}
}

// LabelReadiness marks the issues the readiness check filed, so running it
// again doesn't file them twice.
const LabelReadiness = "factory:readiness"

// GitHub is the subset of the GitHub client the pipeline uses. The GitLab
// client has the same methods, with merge requests for pull requests.
type GitHub interface {
	ListIssues(ctx context.Context, label string) ([]github.Issue, error)
	GetIssue(ctx context.Context, n int) (*github.Issue, error)
	AddLabels(ctx context.Context, n int, labels ...string) error
	LabelPR(ctx context.Context, n int, labels ...string) error
	UnlabelPR(ctx context.Context, n int, label string) error
	RemoveLabel(ctx context.Context, n int, label string) error
	Comment(ctx context.Context, n int, body string) error
	FindPR(ctx context.Context, branch string) (*github.PR, error)
	GetPR(ctx context.Context, n int) (*github.PR, error)
	CreatePR(ctx context.Context, title, body, head, base string, draft bool) (*github.PR, error)
	UpdatePRBody(ctx context.Context, n int, body string) error
	CreateIssue(ctx context.Context, title, body string, labels []string) (int, error)
	// Approver is who last added label to issue n, whether they may
	// approve work, and the text they approved (see approval.go).
	Approver(ctx context.Context, n int, label string) (github.Approval, error)
}

// AutoMerger is a GitHub (or GitLab) client that can merge a PR once its
// checks pass, if its head is still sha, and call that off.
type AutoMerger interface {
	AutoMerge(ctx context.Context, n int, sha, method string) error
	CancelAutoMerge(ctx context.Context, n int) error
}

// RepoClients are one repository's GitHub client and git remote.
type RepoClients struct {
	GH  GitHub
	Git *Git
}

// Pipeline wires the store, GitHub, git, the runner and the harnesses.
type Pipeline struct {
	Cfg   *config.Config
	Store *store.Store
	// ClientsFor makes a repo's clients, once per repo. Without it GH and
	// Git serve every repo, which is how tests run one.
	ClientsFor func(repo string) RepoClients
	GH         GitHub
	Git        *Git
	Runner     runner.Runner
	Harnesses  map[string]harness.Harness
	// Secrets maps a harness name to the env vars injected into its jobs.
	Secrets map[string]map[string]string
	Log     *slog.Logger
	// GateExec runs one gate command against a checkout. Defaults to
	// running it in the job image through Runner.
	GateExec func(ctx context.Context, workdir, command string) (int, string, error)
	// Notify sends a message to the configured channel (may be nil).
	Notify func(ctx context.Context, msg string)
	// JobPrefix keeps container names unique when several pipelines with
	// their own stores run at once (factory eval and factory compare).
	JobPrefix string
	// ConfigPath is the factory.yaml the tuning loop proposes changes to.
	ConfigPath string

	mu         sync.Mutex
	clientsMu  sync.Mutex
	clientsBy  map[string]RepoClients
	running    int
	lastPoll   time.Time
	budgetHalt string            // day the daily cap halted dequeuing, to notify once
	limitHalt  map[string]string // harness -> the window that paused it, to notify once
	signInHalt map[string]string // harness -> why it can't sign in, to notify once
	wg         sync.WaitGroup
	jobCtx     context.Context // outlives Serve's ctx so jobs get a shutdown grace
	gateSeq    atomic.Int64    // unique suffix for gate container names
	svcMu      sync.Mutex
	svcNets    map[int64]string // job ID -> its services network
	inflight   sync.Map         // item id -> job id while a job for it, and its follow-ups, run
	cancelled  sync.Map         // job id -> true for running jobs a human cancelled
	jobCancel  sync.Map         // job id -> context.CancelFunc of a running job
	// act serializes state-changing actions that check and then act:
	// intake, human actions and the orchestrator's tools.
	act    sync.Mutex
	orchMu sync.Mutex // guards coalescing of orchestrator requests

	diskMu   sync.Mutex
	diskAt   time.Time // when the free space was last measured
	diskMsg  string    // why there isn't room to start a job, or ""
	diskHalt bool      // the low disk was announced

	lastOrch    time.Time // last scheduled orchestrator request
	lastSummary string    // day the daily summary was requested
	lastDrain   time.Time // last drained-queue orchestrator request
	tuneDue     time.Time // when the tuning cadence last fired
	tuneHeld    string    // day a due tuning run waited for budget, to notify once

	restart atomic.Bool // a restart was asked for: claim nothing more
}

func (p *Pipeline) clients(repo string) RepoClients {
	if p.ClientsFor == nil {
		return RepoClients{GH: p.GH, Git: p.Git}
	}
	key := strings.ToLower(repo)
	p.clientsMu.Lock()
	defer p.clientsMu.Unlock()
	c, ok := p.clientsBy[key]
	if !ok {
		if p.clientsBy == nil {
			p.clientsBy = map[string]RepoClients{}
		}
		c = p.ClientsFor(repo)
		p.clientsBy[key] = c
	}
	return c
}

// GitHubFor returns the GitHub client for a repo.
func (p *Pipeline) GitHubFor(repo string) GitHub { return p.gh(repo) }

// GitFor returns the git remote for a repo.
func (p *Pipeline) GitFor(repo string) *Git { return p.git(repo) }

// gh returns the GitHub client for an item's repo.
func (p *Pipeline) gh(repo string) GitHub { return p.clients(repo).GH }

// git returns the git remote for an item's repo.
func (p *Pipeline) git(repo string) *Git { return p.clients(repo).Git }

// repoConf returns an item's repo settings. A repo dropped from the config
// keeps the defaults, so its items can still finish.
func (p *Pipeline) repoConf(repo string) config.Repo {
	if r, ok := p.Cfg.RepoConf(repo); ok {
		return r
	}
	// Resolved like a listed repo, so a GitLab one keeps its host and the
	// diff guard's GitLab paths.
	host, name := config.SplitRepo("", repo)
	r := p.Cfg.Resolve(config.Repo{Name: name, Host: host})
	r.AutoMerge = nil // its own setting is gone: never merge for it
	return r
}

// ref names an issue for people: #12 with one repo, owner/name#12 with several.
func (p *Pipeline) ref(repo string, n int) string {
	if len(p.Cfg.Repos) > 1 {
		return fmt.Sprintf("%s#%d", repo, n)
	}
	return fmt.Sprintf("#%d", n)
}

// ShutdownGrace is how long running jobs may continue after SIGTERM.
var ShutdownGrace = 10 * time.Minute

func (p *Pipeline) log() *slog.Logger {
	if p.Log == nil {
		return slog.Default()
	}
	return p.Log
}

// warn logs a failed best-effort side effect (a label, a comment) that
// must not fail the job.
func (p *Pipeline) warn(err error, what string) {
	if err != nil {
		p.log().Warn(what, "err", err)
	}
}

func (p *Pipeline) notify(ctx context.Context, msg string) {
	p.log().Info("notify", "msg", msg)
	if p.Notify != nil {
		p.Notify(ctx, msg)
	}
}

// Recover is the startup reconciliation: containers of jobs that were
// running when the daemon stopped are killed and the jobs requeued
// without using a retry.
func (p *Pipeline) Recover(ctx context.Context) error {
	// Nothing runs yet, so every factory container (jobs and gates) is stale.
	p.warn(p.Runner.KillAll(ctx), "kill stale containers")
	if d, ok := p.Runner.(*runner.Docker); ok {
		p.warn(d.StaleEgressProxies(ctx), "remove stale egress proxies")
	}
	p.recoverSignIns()
	n, err := p.Store.RequeueRunning(ctx, func(j *store.Job) bool { return false })
	if n > 0 {
		p.log().Info("requeued interrupted jobs", "count", n)
	}
	if err != nil {
		return err
	}
	return p.coalesceOrchestrate(ctx)
}

// ErrRestart is what Serve returns once a restart was asked for and no
// job is left running. The caller starts the daemon again, which reads
// factory.yaml afresh.
var ErrRestart = errors.New("restart to apply new settings")

// RequestRestart stops new jobs from starting so the daemon can restart
// once the running ones finish (see ErrRestart). It returns how many are
// still running. Asking again changes nothing.
func (p *Pipeline) RequestRestart(ctx context.Context) int {
	first := p.restart.CompareAndSwap(false, true)
	n := p.Running() // after the flag, so no job starts uncounted
	if first {
		msg := "Restarting to apply new settings."
		if n > 0 {
			msg = fmt.Sprintf("Restarting to apply new settings once %d running %s.", n, plural(n, "job finishes", "jobs finish"))
		}
		p.notify(ctx, msg)
	}
	return n
}

// CancelRestart calls off a restart that is still waiting, and says
// whether there was one.
func (p *Pipeline) CancelRestart(ctx context.Context) bool {
	if !p.restart.CompareAndSwap(true, false) {
		return false
	}
	p.notify(ctx, "Restart called off; jobs are starting again.")
	return true
}

// RestartPending says whether a restart is waiting on running jobs.
func (p *Pipeline) RestartPending() bool { return p.restart.Load() }

// Running is how many jobs this daemon is running.
func (p *Pipeline) Running() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.running
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// Serve runs the daemon loop until ctx is cancelled, then waits for
// running jobs to stop. After RequestRestart it returns ErrRestart once
// they have.
func (p *Pipeline) Serve(ctx context.Context) error {
	if err := p.Recover(ctx); err != nil {
		return err
	}
	jobCtx, cancelJobs := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelJobs()
	p.jobCtx = jobCtx
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		err := p.Tick(ctx, true)
		if errors.Is(err, ErrRestart) {
			p.wg.Wait() // nothing runs, but a job may be finishing up
			return err
		}
		if err != nil && !errors.Is(err, context.Canceled) {
			p.log().Error("tick", "err", err)
		}
		select {
		case <-ctx.Done():
			// Stop claiming; give running jobs a grace period, then kill them.
			// Killed jobs are requeued on the next start.
			done := make(chan struct{})
			go func() { p.wg.Wait(); close(done) }()
			p.log().Info("shutting down", "grace", ShutdownGrace)
			select {
			case <-done:
			case <-time.After(ShutdownGrace):
				cancelJobs()
				<-done
			}
			return nil
		case <-t.C:
		}
	}
}

// Tick polls GitHub when due and starts as many queued jobs as the caps
// allow. With async=false, jobs run inline (tests and `factory run-once`).
// Once a restart is asked for it starts nothing, and returns ErrRestart
// when no job is left running.
func (p *Pipeline) Tick(ctx context.Context, async bool) error {
	if p.restart.Load() {
		if p.Running() == 0 {
			return ErrRestart
		}
		return nil
	}
	if time.Since(p.lastPoll) >= p.Cfg.Intake.PollInterval.Duration {
		p.lastPoll = time.Now()
		if err := p.Intake(ctx); err != nil {
			p.log().Error("intake", "err", err)
		}
		p.learnTriggers(ctx)
	}
	if p.Cfg.Orchestrator.On {
		p.orchestratorTriggers(ctx, time.Now())
	}
	p.tuneTrigger(ctx, time.Now())
	for claimed := false; ; claimed = true {
		j, err := p.claimNext(ctx)
		if errors.Is(err, ErrDiskLow) && async {
			return nil // said once already; the next tick looks again
		}
		if err == nil && j == nil && !claimed && p.Cfg.Orchestrator.On {
			p.drainedTrigger(ctx)
			j, err = p.claimNext(ctx) // it may have queued an orchestrator run
		}
		if err != nil || j == nil {
			return err
		}
		if !async {
			p.runJob(ctx, j)
			continue
		}
		p.mu.Lock()
		p.running++
		p.mu.Unlock()
		jctx := ctx
		if p.jobCtx != nil {
			jctx = p.jobCtx
		}
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			defer func() { p.mu.Lock(); p.running--; p.mu.Unlock() }()
			p.runJob(jctx, j)
		}()
	}
}

// Drain runs ticks inline until no queued job is left.
func (p *Pipeline) Drain(ctx context.Context) error {
	return p.Tick(ctx, false)
}

// claimNext applies every daemon-enforced cap, then claims one job.
func (p *Pipeline) claimNext(ctx context.Context) (*store.Job, error) {
	p.mu.Lock()
	running := p.running
	p.mu.Unlock()
	if running >= p.Cfg.Budgets.ConcurrentJobs || p.restart.Load() {
		return nil, nil
	}
	// One orchestrator at a time: its tools act on the whole queue. One
	// tuning run at a time, too: each reads the file the last proposed for.
	// And one learn job: two for a repo would push over each other's lines.
	var except []string
	for _, kind := range []string{"orchestrate", "tune", "learn"} {
		if n, err := p.Store.CountJobs(ctx, state.Running, kind); err != nil {
			return nil, err
		} else if n > 0 {
			except = append(except, kind)
		}
	}
	// A tuning run waits for both today's budget and the orchestrator's.
	if !slices.Contains(except, "tune") && p.tuneBudget(ctx) != "" {
		except = append(except, "tune")
	}
	if msg := p.diskLow(ctx); msg != "" {
		if !p.diskHalt {
			p.diskHalt = true
			p.notify(ctx, msg)
		}
		// The orchestrator writes next to nothing; builds wait.
		if j, err := p.Store.ClaimJobSkipping(ctx, append(except, "triage", "implement", "review", "critic", "learn"), p.heldHarnesses(ctx)); j != nil || err != nil {
			return j, err
		}
		return nil, ErrDiskLow
	} else if p.diskHalt {
		p.diskHalt = false
		p.notify(ctx, "There's room on disk again; jobs are starting.")
	}
	spent, err := p.buildSpend(ctx)
	if err != nil {
		return nil, err
	}
	if spent >= p.Cfg.Budgets.DailyTotal {
		if p.budgetHalt != store.Today() {
			p.budgetHalt = store.Today()
			p.notify(ctx, fmt.Sprintf("Daily budget reached ($%.2f of $%.2f). No new jobs start until midnight UTC.", spent, p.Cfg.Budgets.DailyTotal))
		}
		// The orchestrator has its own budget, checked when its run starts.
		return p.Store.ClaimJobSkipping(ctx, append(except, "triage", "implement", "review", "critic", "learn"), p.heldHarnesses(ctx))
	}
	waiting, err := p.Store.ListItems(ctx, state.AwaitingHuman)
	if err != nil {
		return nil, err
	}
	if len(waiting) >= p.Cfg.Budgets.AwaitingHumanMax {
		except = append(except, "implement")
	}
	return p.Store.ClaimJobSkipping(ctx, except, p.heldHarnesses(ctx))
}

// pausedHarnesses are the harnesses whose subscription window is used up
// to budgets.pause_at_usage: their jobs wait in the queue until it resets,
// rather than fail and burn retries.
func (p *Pipeline) pausedHarnesses(ctx context.Context) []string {
	limits, err := p.Store.Limits(ctx)
	if err != nil {
		return nil
	}
	at := p.Cfg.PauseAtUsage()
	now := time.Now().UTC()
	var out []string
	why := map[string]string{}
	window := map[string]string{}
	for _, l := range limits {
		// Only the windows every model shares: a model's own weekly
		// window, or overage, shouldn't hold the harness's other jobs.
		if l.Window != "5h" && l.Window != "weekly" {
			continue
		}
		resets, err := time.Parse(store.TimeFormat, l.ResetsAt)
		if l.UsedPercent < at || err != nil || !resets.After(now) {
			continue // a window with no reset time can't be waited out
		}
		if _, ok := why[l.Harness]; !ok {
			out = append(out, l.Harness)
			window[l.Harness] = l.Window
			why[l.Harness] = fmt.Sprintf("%s's %s limit is %.0f%% used; its jobs wait until it resets at %s UTC. `factory limits clear %s` starts them sooner.", l.Harness, l.Window, l.UsedPercent, resets.Format("Jan 2 15:04"), l.Harness)
		}
	}
	var notes []string
	p.mu.Lock()
	if p.limitHalt == nil {
		p.limitHalt = map[string]string{}
	}
	// Once per pause, not per reading: jobs still running keep reporting
	// a fuller window.
	for h, msg := range why {
		if p.limitHalt[h] != window[h] {
			p.limitHalt[h] = window[h]
			notes = append(notes, msg)
		}
	}
	for h := range p.limitHalt {
		if _, ok := why[h]; !ok {
			delete(p.limitHalt, h)
			notes = append(notes, h+"'s usage limit has reset; its jobs are starting.")
		}
	}
	p.mu.Unlock()
	sort.Strings(notes)
	for _, n := range notes {
		p.notify(ctx, n)
	}
	return out
}

// recordLimits saves the usage-limit windows a run reported.
func (p *Pipeline) recordLimits(ctx context.Context, harnessName string, res *harness.Result) {
	if res == nil {
		return
	}
	for _, l := range res.Limits {
		resets := ""
		if !l.ResetsAt.IsZero() {
			resets = l.ResetsAt.UTC().Format(store.TimeFormat)
		}
		p.warn(p.Store.SetLimit(ctx, store.Limit{Harness: harnessName, Window: l.Window, UsedPercent: l.UsedPercent, ResetsAt: resets}), "SetLimit")
	}
}

// buildSpend is today's spend on building, without orchestration, which
// has its own daily cap.
func (p *Pipeline) buildSpend(ctx context.Context) (float64, error) {
	all, err := p.Store.SpendOn(ctx, store.Today())
	if err != nil {
		return 0, err
	}
	orch, err := p.orchSpend(ctx)
	return all - orch, err
}

// orchSpend is today's spend on orchestration: the orchestrator's runs
// and the tuning loop's, which share orchestrator.daily_budget.
func (p *Pipeline) orchSpend(ctx context.Context) (float64, error) {
	orch, err := p.Store.SpendByKind(ctx, store.Today(), "orchestrate")
	if err != nil {
		return 0, err
	}
	tune, err := p.Store.SpendByKind(ctx, store.Today(), "tune")
	return orch + tune, err
}

// Intake turns labeled issues into work items, sends items a human marked
// factory:retry back to triage, closes out items whose PR merged or closed,
// and reconciles items left with no job.
func (p *Pipeline) Intake(ctx context.Context) error {
	p.act.Lock()
	defer p.act.Unlock()
	// One repo GitHub can't reach must not stop the others.
	var errs []error
	for _, repo := range p.Cfg.RepoNames() {
		if err := p.intakeRepo(ctx, repo); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", repo, err))
		}
	}
	if err := p.closeOut(ctx); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// intakeRepo picks up one repo's labeled and retried issues.
func (p *Pipeline) intakeRepo(ctx context.Context, repo string) error {
	gh := p.gh(repo)
	issues, err := gh.ListIssues(ctx, p.Cfg.Intake.Label)
	if err != nil {
		return err
	}
	for _, is := range issues {
		if hasLabel(is.Labels, LabelEpic) {
			continue // epics are split by the orchestrator, never built whole
		}
		fresh, err := p.isNew(ctx, repo, is.Number)
		if err != nil {
			return err
		}
		who, text := "", is
		if fresh {
			ok := false
			if who, text, ok = p.approvedBy(ctx, repo, is.Number, p.Cfg.Intake.Label); !ok {
				continue
			}
		}
		it, created, err := p.Store.UpsertItem(ctx, repo, is.Number, text.Title)
		if err != nil {
			return err
		}
		if created {
			// Should this fail, the item parks at triage for lack of one.
			if err := p.approve(ctx, it, text, who); err != nil {
				return err
			}
			if err := p.enqueueTriage(ctx, it, 1, ""); err != nil {
				return err
			}
		}
	}
	if p.Cfg.Orchestrator.On {
		p.warn(p.intakeEpics(ctx, repo), "epics")
	}
	retries, err := gh.ListIssues(ctx, LabelRetry)
	if err != nil {
		return err
	}
	for _, is := range retries {
		it, err := p.Store.ItemByIssue(ctx, repo, is.Number)
		if err != nil || it.Stage != state.Parked {
			continue
		}
		// A retry approves the issue as it reads now.
		who, text, ok := p.approvedBy(ctx, repo, is.Number, LabelRetry)
		if !ok {
			continue
		}
		if err := p.approve(ctx, it, text, who); err != nil {
			return err
		}
		// Remove the signal first so a failure below cannot loop.
		if err := gh.RemoveLabel(ctx, is.Number, LabelRetry); err != nil {
			p.warn(err, "RemoveLabel")
			continue
		}
		p.warn(gh.RemoveLabel(ctx, is.Number, LabelParked), "RemoveLabel")
		if hasLabel(is.Labels, LabelEpic) {
			// An unparked epic goes back to the orchestrator, never to triage.
			if err := p.Store.SetStage(ctx, it.ID, state.Epic, "human", "factory:retry label added"); err != nil {
				return err
			}
			p.warn(p.Orchestrate(ctx, fmt.Sprintf("epic %s (item %d) was unparked: %s", p.ref(repo, is.Number), it.ID, is.Title), false), "orchestrate")
			continue
		}
		if err := p.Store.SetStage(ctx, it.ID, state.Triage, "human", "factory:retry label added"); err != nil {
			return err
		}
		if err := p.enqueueTriage(ctx, it, 1, ""); err != nil {
			return err
		}
	}
	return nil
}

// closeOut finishes items whose PR was merged or closed, unblocks their
// dependents and reconciles items left with no job.
func (p *Pipeline) closeOut(ctx context.Context) error {
	waiting, err := p.Store.ListItems(ctx, state.AwaitingHuman)
	if err != nil {
		return err
	}
	for _, it := range waiting {
		if prs, err := p.Store.ItemPRs(ctx, it.ID); err != nil {
			return err
		} else if len(prs) > 0 {
			p.closeOutWorkspace(ctx, it, prs)
			continue
		}
		if it.PRNumber == 0 {
			continue
		}
		gh := p.gh(it.Repo)
		pr, err := gh.GetPR(ctx, it.PRNumber)
		if err != nil {
			p.log().Warn("get pr", "pr", it.PRNumber, "err", err)
			continue
		}
		if pr.Merged {
			// Metrics: did the human change the PR before merging it?
			want, _ := headOf(p.lastHeads(ctx, it), it.Repo)
			changed := pr.Head.SHA != "" && pr.Head.SHA != want
			by := "human"
			if !changed && p.autoMerged(ctx, it) {
				by = "daemon"
			}
			p.warn(p.Store.AddEvent(ctx, store.Event{Actor: by, ItemID: &it.ID, Type: "pr_merged",
				Reason: fmt.Sprintf("PR #%d", pr.Number), Data: fmt.Sprintf(`{"changed":%t}`, changed)}), "AddEvent")
			p.warn(p.Store.SetStage(ctx, it.ID, state.Done, by, fmt.Sprintf("PR #%d merged", pr.Number)), "SetStage")
			if changed {
				// What the human changed is what the factory missed: worth
				// a replay case, so the next config is measured against it.
				arg := strings.TrimPrefix(p.ref(it.Repo, pr.Number), "#") // a bare # starts a shell comment
				p.recordHumanEdit(ctx, it, pr, want)
				p.notify(ctx, fmt.Sprintf("PR #%d for %s was merged with your changes. If they fixed something the factory got wrong, `factory eval capture %s` turns it into a replay case.",
					pr.Number, p.ref(it.Repo, it.IssueNumber), arg))
			}
			p.warn(gh.RemoveLabel(ctx, it.IssueNumber, LabelAwaitingYou), "RemoveLabel")
			if p.autoMerged(ctx, it) {
				p.warn(gh.RemoveLabel(ctx, it.IssueNumber, LabelAutoMerge), "RemoveLabel")
			}
		} else if pr.State == "closed" {
			p.warn(p.Store.AddEvent(ctx, store.Event{Actor: "human", ItemID: &it.ID, Type: "pr_closed", Reason: fmt.Sprintf("PR #%d", pr.Number)}), "AddEvent")
			p.warn(gh.RemoveLabel(ctx, it.IssueNumber, LabelAwaitingYou), "RemoveLabel")
			p.warn(p.park(ctx, it, fmt.Sprintf("PR #%d was closed without merging.", pr.Number)), "park")
		}
	}
	if err := p.unblock(ctx); err != nil {
		return err
	}
	return p.Reconcile(ctx, ReconcileAfter)
}

// ReconcileAfter is how long an in-flight item may sit with no queued or
// running job before Reconcile picks it up again.
var ReconcileAfter = 2 * time.Minute

// Reconcile finds items in an active stage with no queued or running job
// (a follow-up step failed after a job finished, for example a GitHub error
// while opening the PR) and resumes them from their last job's outcome.
func (p *Pipeline) Reconcile(ctx context.Context, quiet time.Duration) error {
	items, err := p.Store.ListItems(ctx, state.Triage, state.Ready, state.Implementing, state.Reviewing)
	if err != nil {
		return err
	}
	for _, it := range items {
		jobs, err := p.Store.ListJobs(ctx, store.JobFilter{ItemID: it.ID})
		if err != nil {
			return err
		}
		if len(jobs) > 0 && !jobs[0].State.Terminal() {
			continue
		}
		if _, busy := p.inflight.Load(it.ID); busy {
			continue // a job finished but its follow-up steps are still running
		}
		if len(jobs) > 0 {
			if t, err := time.Parse(store.TimeFormat, jobs[0].FinishedAt); err == nil && time.Since(t) < quiet {
				continue
			}
		}
		p.warn(p.Store.AddEvent(ctx, store.Event{Actor: "daemon", ItemID: &it.ID, Type: "reconcile", Reason: "no active job in stage " + string(it.Stage)}), "AddEvent")
		if err := p.resume(ctx, it, jobs); err != nil {
			p.log().Error("reconcile", "item", it.ID, "err", err)
		}
	}
	return nil
}

func (p *Pipeline) resume(ctx context.Context, it *store.WorkItem, jobs []*store.Job) error {
	var last *store.Job
	if len(jobs) > 0 {
		last = jobs[0]
	}
	switch {
	case last == nil || it.Stage == state.Triage && last.Kind != "triage":
		return p.enqueueTriage(ctx, it, 1, "")
	case last.Kind == "triage" && last.State == state.Succeeded:
		var v verdict.Triage
		if err := verdict.Parse([]byte(last.Verdict), &v); err != nil {
			return p.enqueueTriage(ctx, it, 1, "")
		}
		return p.afterTriage(ctx, it, &v)
	case last.Kind == "triage":
		return p.enqueueTriage(ctx, it, last.Attempt+1, "")
	case (last.Kind == "review" || last.Kind == "critic") && last.State == state.Succeeded:
		var v verdict.Review
		var in ReviewInput
		if verdict.Parse([]byte(last.Verdict), &v) != nil || json.Unmarshal([]byte(last.Input), &in) != nil {
			return p.enqueueReview(ctx, it, last.Kind, 1, in)
		}
		return p.afterReview(ctx, it, last.Kind, &v, in)
	case last.Kind == "review" || last.Kind == "critic":
		var in ReviewInput
		_ = json.Unmarshal([]byte(last.Input), &in)
		return p.enqueueReview(ctx, it, last.Kind, last.Attempt+1, in)
	case last.Kind == "implement" && last.State == state.Succeeded:
		gr, _ := p.Store.GateResults(ctx, last.ID)
		rs := make([]gates.Result, len(gr))
		for i, g := range gr {
			rs[i] = gates.Result{Gate: g.Gate, Status: g.Status, Detail: g.Detail}
		}
		if it.Stage != state.Reviewing {
			if err := p.Store.SetStage(ctx, it.ID, state.Reviewing, "daemon", "reconciled after implement"); err != nil {
				return err
			}
		}
		var heads []store.RepoHead
		if p.inWorkspace(it) {
			heads = p.jobHeads(ctx, it, last)
		}
		return p.enqueueReview(ctx, it, "review", 1, ReviewInput{ImplementJob: last.ID, Head: last.HeadSHA, Heads: heads, Gates: gates.VerdictLine(rs) + "\n\n" + gateTable(rs), Features: p.savedBlast(ctx, it.ID, last.ID)})
	default:
		var in ImplementInput
		_ = json.Unmarshal([]byte(last.Input), &in)
		ok, err := p.enqueueImplement(ctx, it, last.Attempt+1, in)
		if err == nil && !ok {
			return p.park(ctx, it, "Implementation failed on every model tier.")
		}
		return err
	}
}

// hasLabel matches like GitHub does: case-insensitively.
func hasLabel(ls []string, l string) bool {
	for _, x := range ls {
		if strings.EqualFold(x, l) {
			return true
		}
	}
	return false
}

func (p *Pipeline) containerName(id int64) string {
	return fmt.Sprintf("factory-%sjob-%d", p.JobPrefix, id)
}

func (p *Pipeline) jobDir(id int64) string {
	return filepath.Join(p.Cfg.Path(p.Cfg.DataDir), "jobs", fmt.Sprint(id))
}

func (p *Pipeline) skillsDir(set string) string {
	if set == "" {
		return ""
	}
	return filepath.Join(p.Cfg.Path(p.Cfg.SkillsDir), set)
}

// --- enqueue helpers ---

func (p *Pipeline) enqueueTriage(ctx context.Context, it *store.WorkItem, attempt int, input string) error {
	r := p.Cfg.Routing["triage"]
	_, err := p.Store.EnqueueJob(ctx, store.Job{ItemID: &it.ID, Kind: "triage", Harness: r.Harness, Model: r.Model, Effort: r.Effort, Fast: r.FastFor(r.Harness, r.Model),
		Skillset: r.Skills, Route: "triage", Attempt: attempt, Priority: it.Priority, BudgetUSD: p.Cfg.FastBudget(p.Cfg.JobBudget("triage", ""), r.FastFor(r.Harness, r.Model)), Input: input})
	return err
}

// ImplementInput is carried to implement jobs.
type ImplementInput struct {
	Findings        string `json:"findings,omitempty"`         // reviewer findings for a fix round
	PreviousFailure string `json:"previous_failure,omitempty"` // why the last attempt failed
	FixRound        bool   `json:"fix_round,omitempty"`
}

func (p *Pipeline) enqueueImplement(ctx context.Context, it *store.WorkItem, attempt int, in ImplementInput) (bool, error) {
	r, ok := p.Cfg.Implement(it.Complexity, attempt)
	if !ok {
		return false, nil
	}
	b, _ := json.Marshal(in)
	_, err := p.Store.EnqueueJob(ctx, store.Job{ItemID: &it.ID, Kind: "implement", Harness: r.Harness, Model: r.Model, Effort: r.Effort, Fast: r.FastFor(r.Harness, r.Model),
		Skillset: r.Skills, Route: p.Cfg.ImplementKey(it.Complexity), Attempt: attempt, Priority: it.Priority, BudgetUSD: p.Cfg.FastBudget(p.Cfg.JobBudget("implement", it.Complexity), r.FastFor(r.Harness, r.Model)),
		Branch: p.branchFor(it), Input: string(b)})
	return true, err
}

// ReviewInput is carried to review and critic jobs.
type ReviewInput struct {
	ImplementJob int64  `json:"implement_job"`
	Head         string `json:"head"` // the commit the gates verified
	// Heads are the commits verified in each repo a workspace change
	// touches, in merge order; empty outside a workspace.
	Heads    []store.RepoHead `json:"heads,omitempty"`
	Gates    string           `json:"gates"`
	Features string           `json:"features,omitempty"` // the blast radius, from the feature map
}

func (p *Pipeline) enqueueReview(ctx context.Context, it *store.WorkItem, kind string, attempt int, in ReviewInput) error {
	route := p.Cfg.ReviewKey(it.Risk)
	r := p.Cfg.Routing[route]
	if kind == "critic" {
		route = "critic"
		r = p.Cfg.Routing["implement.L"] // the strongest Claude tier
		// Only what makes it the strongest model: not the builder's skills,
		// instructions or fast mode.
		r.Skills, r.Use, r.Instructions, r.InstructionsFile, r.Fast = "", nil, "", "", false
		if c, ok := p.Cfg.Routing["critic"]; ok {
			r = c
		}
	}
	b, _ := json.Marshal(in)
	_, err := p.Store.EnqueueJob(ctx, store.Job{ItemID: &it.ID, Kind: kind, Harness: r.Harness, Model: r.Model, Effort: r.Effort, Fast: r.FastFor(r.Harness, r.Model),
		Skillset: r.Skills, Route: route, Attempt: attempt, Priority: it.Priority, BudgetUSD: p.Cfg.FastBudget(p.Cfg.JobBudget("review", ""), r.FastFor(r.Harness, r.Model)),
		Branch: p.branchFor(it), Input: string(b)})
	return err
}

// --- running a job ---

type agentRun struct {
	res     *harness.Result
	final   string
	cost    float64
	timeout bool
	err     error
}

// runAgent runs one harness invocation for a job, streams its events to the
// job log (scrubbed of secrets) and returns the outcome.
func (p *Pipeline) runAgent(ctx context.Context, repo string, j *store.Job, workdir, outDir, prompt, schema string, readOnly bool, timeout time.Duration) agentRun {
	return p.runAgentWith(ctx, j, workdir, outDir, prompt, schema, timeout, agentExtras{repo: repo, writable: !readOnly})
}

// agentExtras are the less common knobs of an agent run.
type agentExtras struct {
	repo       string // picks the image and env; "" for the orchestrator
	writable   bool
	sessionDir string // keeps Claude sessions across an item's jobs
	resume     string // the session to continue
	mounts     []runner.Mount
	mcpConfig  string
	tools      []string
}

func (p *Pipeline) runAgentWith(ctx context.Context, j *store.Job, workdir, outDir, prompt, schema string, timeout time.Duration, x agentExtras) agentRun {
	h, ok := p.Harnesses[j.Harness]
	if !ok {
		return agentRun{err: fmt.Errorf("no harness %q", j.Harness)}
	}
	logPath := filepath.Join(p.jobDir(j.ID), "log.jsonl")
	p.warn(p.Store.SetJobRuntime(ctx, j.ID, p.containerName(j.ID), logPath, ""), "Store.SetJobRuntime")
	lf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return agentRun{err: err}
	}
	defer lf.Close()
	secrets := p.Secrets[j.Harness]
	rc := p.repoConf(x.repo)
	env := map[string]string{}
	for k, v := range rc.Env {
		env[k] = v
	}
	for k, v := range secrets {
		env[k] = v
	}
	events := make(chan harness.Event, 256)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for e := range events {
			if e.Kind != "raw" {
				continue
			}
			_, _ = lf.Write([]byte(scrub(string(e.Payload), secrets) + "\n"))
		}
	}()
	res, err := h.Run(ctx, harness.RunRequest{
		Name: p.containerName(j.ID), Stage: j.Kind, Model: j.Model, Effort: j.Effort, Fast: j.Fast, Prompt: prompt,
		Workdir: workdir, OutDir: outDir, SkillsDir: p.skillsDir(j.Skillset), ReadOnly: !x.writable, Schema: schema,
		BudgetUSD: j.BudgetUSD, MaxTurns: 80, Env: env, Timeout: timeout, Image: rc.Image, CaptureLimits: p.Cfg.Runner.Kind == "docker",
		Mounts: x.mounts, MCPConfig: x.mcpConfig, Tools: x.tools, SessionDir: x.sessionDir, Resume: x.resume, Services: p.servicesOf(j.ID),
	}, events)
	if x.resume != "" && err == nil && res != nil && res.IsError && strings.Contains(res.Error, "No conversation found") && ctx.Err() == nil {
		// Resuming is best effort: if the session is gone, start fresh,
		// keeping whatever the failed attempt cost.
		p.log().Warn("resume failed; starting a fresh session", "job", j.ID, "session", x.resume)
		x.resume = ""
		close(events)
		<-done
		_ = lf.Close()
		run := p.runAgentWith(ctx, j, workdir, outDir, prompt, schema, timeout, x)
		run.cost += res.CostUSD
		if !res.CostKnown {
			run.cost += p.Cfg.CostOf(j.Model, res.TokensIn, res.TokensCached, res.TokensOut)
		}
		return run
	}
	close(events)
	<-done
	p.scrubLog(logPath, res)
	p.recordLimits(ctx, j.Harness, res)
	run := agentRun{res: res, err: err}
	if res != nil && res.SessionID != "" {
		p.warn(os.WriteFile(filepath.Join(p.jobDir(j.ID), "session"), []byte(res.SessionID), 0o644), "save session")
	}
	if errors.Is(err, runner.ErrTimeout) {
		run.timeout, run.err = true, nil
	}
	if res != nil {
		run.final = res.Final
		run.cost = res.CostUSD
		if !res.CostKnown {
			run.cost = p.Cfg.CostOf(j.Model, res.TokensIn, res.TokensCached, res.TokensOut)
		}
	}
	return run
}

// sessionFor lets a retry of an implement job continue the previous
// attempt's Claude session (same item, same model), so it keeps what it
// learned. Only with the Docker runner, where the home is per job and the
// session directory is mounted in.
func (p *Pipeline) sessionFor(ctx context.Context, j *store.Job, fixRound bool) agentExtras {
	x := agentExtras{writable: true}
	if _, docker := p.Runner.(*runner.Docker); !docker || j.Harness != "claude" || j.ItemID == nil {
		return x
	}
	x.sessionDir = filepath.Join(p.Cfg.Path(p.Cfg.DataDir), "sessions", fmt.Sprintf("item-%d", *j.ItemID))
	jobs, err := p.Store.ListJobs(ctx, store.JobFilter{ItemID: *j.ItemID, Kind: "implement"})
	if err != nil {
		return x
	}
	// Only a fix round continues on the previous attempt's branch; any other
	// retry starts from a fresh checkout, which the old session would not know.
	for _, prev := range jobs {
		if prev.ID >= j.ID {
			continue
		}
		if fixRound && prev.Harness == j.Harness && prev.Model == j.Model {
			if b, err := os.ReadFile(filepath.Join(p.jobDir(prev.ID), "session")); err == nil {
				x.resume = strings.TrimSpace(string(b))
			}
		}
		break // only the attempt right before this one
	}
	return x
}

func scrub(s string, secrets map[string]string) string {
	for _, v := range secrets {
		if len(v) >= 8 {
			s = strings.ReplaceAll(s, v, "[REDACTED]")
		}
	}
	return s
}

func (p *Pipeline) finish(ctx context.Context, j *store.Job, run agentRun, st state.JobState, verdict, errMsg string) {
	r := store.Result{State: st, CostUSD: run.cost, Verdict: verdict, Error: errMsg}
	if run.res != nil {
		r.TokensIn, r.TokensOut = run.res.TokensIn, run.res.TokensOut
	}
	// Claude enforces the cap itself (--max-budget-usd); Codex reports usage
	// only at the end, so an overrun is recorded for the control room.
	if run.cost > j.BudgetUSD {
		p.warn(p.Store.AddEvent(ctx, store.Event{Actor: "daemon", ItemID: j.ItemID, JobID: &j.ID, Type: "budget_exceeded",
			Reason: fmt.Sprintf("job cost $%.2f over its $%.2f budget", run.cost, j.BudgetUSD)}), "AddEvent")
	}
	if err := p.Store.FinishJob(ctx, j.ID, r); err != nil {
		p.log().Error("finish job", "job", j.ID, "err", err)
	}
	if (st == state.Failed || st == state.TimedOut) && j.ItemID != nil && p.Cfg.Orchestrator.On {
		p.failedTwice(ctx, j)
	}
}

func (p *Pipeline) runJob(ctx context.Context, j *store.Job) {
	log := p.log().With("job", j.ID, "kind", j.Kind)
	log.Info("job started", "harness", j.Harness, "model", j.Model, "attempt", j.Attempt, "skillset", j.Skillset)
	dir := p.jobDir(j.ID)
	// A requeued job may find a previous run's checkout and verdict here.
	_ = os.RemoveAll(dir)
	if err := os.MkdirAll(filepath.Join(dir, "out"), 0o755); err != nil {
		p.finish(ctx, j, agentRun{}, state.Failed, "", err.Error())
		return
	}
	defer func() {
		for _, d := range []string{"repo", "repos", "work", "gate", "proves", "feature-base"} {
			_ = os.RemoveAll(filepath.Join(dir, d))
		}
	}()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	p.jobCancel.Store(j.ID, cancel)
	defer p.jobCancel.Delete(j.ID)
	var it *store.WorkItem
	// A cancel that landed after the job's last cancellation check (for
	// example while it opened the PR, or before the cancel func existed)
	// still ends the job and parks the item.
	defer func() { p.stopIfCancelled(ctx, j, it, agentRun{}) }()
	if _, ok := p.cancelled.Load(j.ID); ok {
		cancel()
	}
	if j.ItemID != nil {
		p.inflight.Store(*j.ItemID, j.ID)
		defer p.inflight.Delete(*j.ItemID)
		var err error
		if it, err = p.Store.GetItem(ctx, *j.ItemID); err != nil {
			p.finish(ctx, j, agentRun{}, state.Failed, "", err.Error())
			return
		}
		// Parked (or finished) between being queued and being claimed.
		if it.Stage == state.Parked || it.Stage == state.Done {
			p.finish(ctx, j, agentRun{}, state.Cancelled, "", "item is "+string(it.Stage))
			return
		}
	}
	var err error
	switch j.Kind {
	case "triage":
		err = p.runTriage(ctx, j, it)
	case "implement":
		err = p.runImplement(ctx, j, it)
	case "review", "critic":
		err = p.runReview(ctx, j, it)
	case "orchestrate":
		err = p.runOrchestrate(ctx, j)
	case "tune":
		err = p.runTune(ctx, j)
	case "learn":
		err = p.runLearn(ctx, j)
	default:
		p.finish(ctx, j, agentRun{}, state.Failed, "", "unknown job kind "+j.Kind)
		return
	}
	if err != nil {
		log.Error("job error", "err", err)
	}
	log.Info("job done")
}

// --- prompts ---

// PromptData is what stage templates can reference.
type PromptData struct {
	Repo            string   // owner/name, or the list of them for the orchestrator
	Repos           []string // the orchestrator's repos when there are several
	Issue           github.Issue
	OutDir          string
	Install         string   // the repo's install command, if any
	Checks          []string // the repo's required gate commands
	Coverage        bool     // a coverage floor holds the change
	Findings        string
	PreviousFailure string
	Gates           string
	Features        string   // the change's blast radius, when the repo has a feature map
	Risk            string   // low, medium or high
	TextOnly        bool     // triage rated it XS: text, not behavior
	Risky           []string // the high-risk features in the feature map, for the builder
	Diff            string
	Fence           string         // a code fence the diff cannot close
	Members         []PromptMember // a workspace's repos, side by side in the checkout
	Reasons         []string       // why the orchestrator runs
	Summary         bool           // the orchestrator writes the daily summary
	Status          string         // queue snapshot for the orchestrator
	Tune            TunePrompt     // the tuning loop's metrics and settings
}

func (p *Pipeline) render(stage string, d any) (string, error) {
	path := filepath.Join(p.Cfg.Path(p.Cfg.PromptsDir), stage+".md")
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	t, err := template.New(stage).Option("missingkey=error").Parse(string(b))
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, d); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// renderStep renders a job's prompt and adds the instructions its route
// and repos carry for the step. d is a PromptData, or a stage's own data.
func (p *Pipeline) renderStep(j *store.Job, stage string, repos []string, d any) (string, error) {
	prompt, err := p.render(stage, d)
	if err != nil {
		return "", err
	}
	extra, err := p.Cfg.StepInstructions(j.Route, j.Kind, repos...)
	if err != nil || extra == "" {
		return prompt, err
	}
	return strings.TrimRight(prompt, "\n") + "\n\n## Instructions for this step\n\n" + extra + "\n", nil
}

// gateRun says how a gate command runs.
type gateRun struct {
	repo    string         // picks the image, env and dependency cache
	install bool           // the install step: the only one that may write the cache
	noNet   bool           // no network at all (hidden scenarios)
	mounts  []runner.Mount // extra mounts (the scenarios directory)
}

// cacheEnv points package managers at the repo's dependency cache at
// /cache. Install writes it and lives on across jobs, so it holds only
// stores that check every package against the lockfile's hashes when read
// (npm, pnpm): a job can't plant a package a later job would trust.
var cacheEnv = map[string]string{"npm_config_cache": "/cache/npm", "PNPM_STORE_DIR": "/cache/pnpm"}

// exportEnv keeps what install builds inside the export, where the gates
// after it find it and the next commit's export starts without it.
var exportEnv = map[string]string{
	"BUNDLE_PATH": "vendor/bundle", "CARGO_HOME": "/work/.cargo-home", "COREPACK_HOME": "/work/.corepack",
	"COREPACK_ENABLE_DOWNLOAD_PROMPT": "0",
}

// gateExec runs one gate command in the repo's job image against an
// export of the commit under test, kept apart from what gets pushed.
func (p *Pipeline) gateExec(ctx context.Context, jobID int64, workdir, command string, g gateRun) (int, string, error) {
	if p.GateExec != nil {
		return p.GateExec(ctx, workdir, command)
	}
	var out bytes.Buffer
	outDir, err := os.MkdirTemp(p.jobDir(jobID), "gate-out-")
	if err != nil {
		return -1, "", err
	}
	defer os.RemoveAll(outDir)
	rc := p.repoConf(g.repo)
	env := map[string]string{}
	for k, v := range rc.Env {
		env[k] = v
	}
	mounts := g.mounts
	if _, docker := p.Runner.(*runner.Docker); docker && g.repo != "" {
		for k, v := range exportEnv {
			env[k] = v
		}
		// Only repos with an install step use the cache; Go fetches its
		// modules itself, into the container.
		if rc.Gates.Install != "" {
			cache := filepath.Join(p.Cfg.Path(p.Cfg.DataDir), "cache", strings.ReplaceAll(g.repo, "/", "__"))
			if err := os.MkdirAll(cache, 0o755); err != nil {
				return -1, "", err
			}
			mounts = append(slices.Clip(mounts), runner.Mount{Host: cache, Container: "/cache", ReadOnly: !g.install})
			for k, v := range cacheEnv {
				env[k] = v
			}
		}
	}
	timeout := p.Cfg.Timeout("gates")
	code, err := p.Runner.Run(ctx, runner.Spec{
		Name: fmt.Sprintf("%s-gate-%d", p.containerName(jobID), p.gateSeq.Add(1)), Image: rc.Image,
		Workdir: workdir, OutDir: outDir, Mounts: mounts, Env: env, NoNetwork: g.noNet, Services: p.servicesOf(jobID),
		Cmd: []string{"sh", "-c", command + " 2>&1"}, Timeout: timeout,
	}, &out)
	return code, out.String(), err
}

func specFor(work, out string) runner.Spec { return runner.Spec{Workdir: work, OutDir: out} }

func jsonUnmarshal(s string, v any) error { return json.Unmarshal([]byte(s), v) }
