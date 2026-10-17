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
)

// GitHub is the subset of the GitHub client the pipeline uses.
type GitHub interface {
	ListIssues(ctx context.Context, label string) ([]github.Issue, error)
	GetIssue(ctx context.Context, n int) (*github.Issue, error)
	AddLabels(ctx context.Context, n int, labels ...string) error
	RemoveLabel(ctx context.Context, n int, label string) error
	Comment(ctx context.Context, n int, body string) error
	FindPR(ctx context.Context, branch string) (*github.PR, error)
	GetPR(ctx context.Context, n int) (*github.PR, error)
	CreatePR(ctx context.Context, title, body, head, base string, draft bool) (*github.PR, error)
	UpdatePRBody(ctx context.Context, n int, body string) error
	CreateIssue(ctx context.Context, title, body string, labels []string) (int, error)
}

// Pipeline wires the store, GitHub, git, the runner and the harnesses.
type Pipeline struct {
	Cfg       *config.Config
	Store     *store.Store
	GH        GitHub
	Git       *Git
	Runner    runner.Runner
	Harnesses map[string]harness.Harness
	// Secrets maps a harness name to the env vars injected into its jobs.
	Secrets map[string]map[string]string
	Log     *slog.Logger
	// GateExec runs one gate command against a checkout. Defaults to
	// running it in the job image through Runner.
	GateExec func(ctx context.Context, workdir, command string) (int, string, error)
	// Notify sends a message to the configured channel (may be nil).
	Notify func(ctx context.Context, msg string)

	mu         sync.Mutex
	running    int
	lastPoll   time.Time
	budgetHalt string // day the daily cap halted dequeuing, to notify once
	wg         sync.WaitGroup
	jobCtx     context.Context // outlives Serve's ctx so jobs get a shutdown grace
	gateSeq    atomic.Int64    // unique suffix for gate container names
	inflight   sync.Map        // item id -> true while a job for it, and its follow-ups, run
	cancelled  sync.Map        // job id -> true for running jobs a human cancelled

	lastOrch    time.Time // last scheduled orchestrator request
	lastSummary string    // day the daily summary was requested
	lastDrain   time.Time // last drained-queue orchestrator request
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
	n, err := p.Store.RequeueRunning(ctx, func(j *store.Job) bool { return false })
	if n > 0 {
		p.log().Info("requeued interrupted jobs", "count", n)
	}
	if err != nil {
		return err
	}
	return p.coalesceOrchestrate(ctx)
}

// Serve runs the daemon loop until ctx is cancelled, then waits for
// running jobs to stop.
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
		if err := p.Tick(ctx, true); err != nil && !errors.Is(err, context.Canceled) {
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
func (p *Pipeline) Tick(ctx context.Context, async bool) error {
	if time.Since(p.lastPoll) >= p.Cfg.Intake.PollInterval.Duration {
		p.lastPoll = time.Now()
		if err := p.Intake(ctx); err != nil {
			p.log().Error("intake", "err", err)
		}
	}
	if p.Cfg.Orchestrator.On {
		p.orchestratorTriggers(ctx, time.Now())
	}
	for claimed := false; ; claimed = true {
		j, err := p.claimNext(ctx)
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
	if running >= p.Cfg.Budgets.ConcurrentJobs {
		return nil, nil
	}
	spent, err := p.Store.SpendOn(ctx, store.Today())
	if err != nil {
		return nil, err
	}
	if spent >= p.Cfg.Budgets.DailyTotal {
		if p.budgetHalt != store.Today() {
			p.budgetHalt = store.Today()
			p.notify(ctx, fmt.Sprintf("Daily budget reached ($%.2f of $%.2f). No new jobs start until midnight UTC.", spent, p.Cfg.Budgets.DailyTotal))
		}
		return nil, nil
	}
	waiting, err := p.Store.ListItems(ctx, state.AwaitingHuman)
	if err != nil {
		return nil, err
	}
	var except []string
	if len(waiting) >= p.Cfg.Budgets.AwaitingHumanMax {
		except = append(except, "implement")
	}
	// One orchestrator at a time: its tools act on the whole queue.
	if n, err := p.Store.CountJobs(ctx, state.Running, "orchestrate"); err != nil {
		return nil, err
	} else if n > 0 {
		except = append(except, "orchestrate")
	}
	if len(except) > 0 {
		return p.Store.ClaimJobExcept(ctx, except...)
	}
	return p.Store.ClaimJob(ctx)
}

// Intake turns labeled issues into work items, sends items a human marked
// factory:retry back to triage, closes out items whose PR merged or closed,
// and reconciles items left with no job.
func (p *Pipeline) Intake(ctx context.Context) error {
	issues, err := p.GH.ListIssues(ctx, p.Cfg.Intake.Label)
	if err != nil {
		return err
	}
	for _, is := range issues {
		it, created, err := p.Store.UpsertItem(ctx, p.Cfg.Repo, is.Number, is.Title)
		if err != nil {
			return err
		}
		if created {
			if err := p.enqueueTriage(ctx, it, 1, ""); err != nil {
				return err
			}
		}
	}
	if p.Cfg.Orchestrator.On {
		p.warn(p.intakeEpics(ctx), "epics")
	}
	retries, err := p.GH.ListIssues(ctx, LabelRetry)
	if err != nil {
		return err
	}
	for _, is := range retries {
		it, err := p.Store.ItemByIssue(ctx, p.Cfg.Repo, is.Number)
		if err != nil || it.Stage != state.Parked {
			continue
		}
		// Remove the signal first so a failure below cannot loop.
		if err := p.GH.RemoveLabel(ctx, is.Number, LabelRetry); err != nil {
			p.warn(err, "RemoveLabel")
			continue
		}
		p.warn(p.GH.RemoveLabel(ctx, is.Number, LabelParked), "RemoveLabel")
		if err := p.Store.SetStage(ctx, it.ID, state.Triage, "human", "factory:retry label added"); err != nil {
			return err
		}
		if err := p.enqueueTriage(ctx, it, 1, ""); err != nil {
			return err
		}
	}
	waiting, err := p.Store.ListItems(ctx, state.AwaitingHuman)
	if err != nil {
		return err
	}
	for _, it := range waiting {
		if it.PRNumber == 0 {
			continue
		}
		pr, err := p.GH.GetPR(ctx, it.PRNumber)
		if err != nil {
			p.log().Warn("get pr", "pr", it.PRNumber, "err", err)
			continue
		}
		if pr.Merged {
			p.warn(p.Store.SetStage(ctx, it.ID, state.Done, "human", fmt.Sprintf("PR #%d merged", pr.Number)), "SetStage")
			p.warn(p.GH.RemoveLabel(ctx, it.IssueNumber, LabelAwaitingYou), "RemoveLabel")
		} else if pr.State == "closed" {
			p.warn(p.GH.RemoveLabel(ctx, it.IssueNumber, LabelAwaitingYou), "RemoveLabel")
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
		return p.enqueueReview(ctx, it, "review", 1, ReviewInput{ImplementJob: last.ID, Head: last.HeadSHA, Gates: gates.VerdictLine(rs) + "\n\n" + gateTable(rs)})
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

func hasLabel(ls []string, l string) bool {
	for _, x := range ls {
		if x == l {
			return true
		}
	}
	return false
}

func containerName(id int64) string { return fmt.Sprintf("factory-job-%d", id) }

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
	_, err := p.Store.EnqueueJob(ctx, store.Job{ItemID: &it.ID, Kind: "triage", Harness: r.Harness, Model: r.Model, Effort: r.Effort,
		Skillset: r.Skills, Attempt: attempt, Priority: it.Priority, BudgetUSD: p.Cfg.JobBudget("triage", ""), Input: input})
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
	_, err := p.Store.EnqueueJob(ctx, store.Job{ItemID: &it.ID, Kind: "implement", Harness: r.Harness, Model: r.Model, Effort: r.Effort,
		Skillset: r.Skills, Attempt: attempt, Priority: it.Priority, BudgetUSD: p.Cfg.JobBudget("implement", it.Complexity),
		Branch: branchFor(it), Input: string(b)})
	return true, err
}

// ReviewInput is carried to review and critic jobs.
type ReviewInput struct {
	ImplementJob int64  `json:"implement_job"`
	Head         string `json:"head"` // the commit the gates verified
	Gates        string `json:"gates"`
}

func (p *Pipeline) enqueueReview(ctx context.Context, it *store.WorkItem, kind string, attempt int, in ReviewInput) error {
	r := p.Cfg.Review(it.Risk)
	if kind == "critic" {
		r = p.Cfg.Routing["implement.L"] // the strongest Claude tier
		r.Skills = ""
		if c, ok := p.Cfg.Routing["critic"]; ok {
			r = c
		}
	}
	b, _ := json.Marshal(in)
	_, err := p.Store.EnqueueJob(ctx, store.Job{ItemID: &it.ID, Kind: kind, Harness: r.Harness, Model: r.Model, Effort: r.Effort,
		Skillset: r.Skills, Attempt: attempt, Priority: it.Priority, BudgetUSD: p.Cfg.JobBudget("review", ""),
		Branch: branchFor(it), Input: string(b)})
	return err
}

func branchFor(it *store.WorkItem) string {
	if it.Branch != "" {
		return it.Branch
	}
	return fmt.Sprintf("factory/issue-%d", it.IssueNumber)
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
func (p *Pipeline) runAgent(ctx context.Context, j *store.Job, workdir, outDir, prompt, schema string, readOnly bool, timeout time.Duration) agentRun {
	return p.runAgentWith(ctx, j, workdir, outDir, prompt, schema, timeout, agentExtras{writable: !readOnly})
}

// agentExtras are the less common knobs of an agent run.
type agentExtras struct {
	writable  bool
	mounts    []runner.Mount
	mcpConfig string
	tools     []string
}

func (p *Pipeline) runAgentWith(ctx context.Context, j *store.Job, workdir, outDir, prompt, schema string, timeout time.Duration, x agentExtras) agentRun {
	h, ok := p.Harnesses[j.Harness]
	if !ok {
		return agentRun{err: fmt.Errorf("no harness %q", j.Harness)}
	}
	logPath := filepath.Join(p.jobDir(j.ID), "log.jsonl")
	p.warn(p.Store.SetJobRuntime(ctx, j.ID, containerName(j.ID), logPath, ""), "Store.SetJobRuntime")
	lf, err := os.Create(logPath)
	if err != nil {
		return agentRun{err: err}
	}
	defer lf.Close()
	secrets := p.Secrets[j.Harness]
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
		Name: containerName(j.ID), Stage: j.Kind, Model: j.Model, Effort: j.Effort, Prompt: prompt,
		Workdir: workdir, OutDir: outDir, SkillsDir: p.skillsDir(j.Skillset), ReadOnly: !x.writable, Schema: schema,
		BudgetUSD: j.BudgetUSD, MaxTurns: 80, Env: secrets, Timeout: timeout, Image: p.Cfg.Runner.Image,
		Mounts: x.mounts, MCPConfig: x.mcpConfig, Tools: x.tools,
	}, events)
	close(events)
	<-done
	run := agentRun{res: res, err: err}
	if errors.Is(err, runner.ErrTimeout) {
		run.timeout, run.err = true, nil
	}
	if res != nil {
		run.final = res.Final
		run.cost = res.CostUSD
		if !res.CostKnown {
			run.cost = p.Cfg.CostOf(j.Model, res.TokensIn, res.TokensOut)
		}
	}
	return run
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
	if (st == state.Failed || st == state.TimedOut) && j.Kind != "orchestrate" && p.Cfg.Orchestrator.On {
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
		for _, d := range []string{"repo", "work", "gate", "proves"} {
			_ = os.RemoveAll(filepath.Join(dir, d))
		}
	}()
	var it *store.WorkItem
	if j.ItemID != nil {
		p.inflight.Store(*j.ItemID, true)
		defer p.inflight.Delete(*j.ItemID)
		var err error
		if it, err = p.Store.GetItem(ctx, *j.ItemID); err != nil {
			p.finish(ctx, j, agentRun{}, state.Failed, "", err.Error())
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
	Repo            string
	Issue           github.Issue
	OutDir          string
	Findings        string
	PreviousFailure string
	Gates           string
	Diff            string
	Fence           string   // a code fence the diff cannot close
	Reasons         []string // why the orchestrator runs
	Summary         bool     // the orchestrator writes the daily summary
	Status          string   // queue snapshot for the orchestrator
}

func (p *Pipeline) render(stage string, d PromptData) (string, error) {
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

// gateExec runs one gate command in the job image against a read-only
// export of the commit under test.
func (p *Pipeline) gateExec(ctx context.Context, jobID int64, workdir, command string) (int, string, error) {
	if p.GateExec != nil {
		return p.GateExec(ctx, workdir, command)
	}
	var out bytes.Buffer
	outDir, err := os.MkdirTemp(p.jobDir(jobID), "gate-out-")
	if err != nil {
		return -1, "", err
	}
	defer os.RemoveAll(outDir)
	code, err := p.Runner.Run(ctx, runner.Spec{
		Name: fmt.Sprintf("%s-gate-%d", containerName(jobID), p.gateSeq.Add(1)), Image: p.Cfg.Runner.Image, ReadOnly: true,
		Workdir: workdir, OutDir: outDir, Cmd: []string{"sh", "-c", command + " 2>&1"}, Timeout: p.Cfg.Timeout("gates"),
	}, &out)
	return code, out.String(), err
}

func specFor(work, out string) runner.Spec { return runner.Spec{Workdir: work, OutDir: out} }

func jsonUnmarshal(s string, v any) error { return json.Unmarshal([]byte(s), v) }
