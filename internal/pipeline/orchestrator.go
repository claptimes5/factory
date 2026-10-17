package pipeline

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/claptimes5/factory/internal/mcp"
	"github.com/claptimes5/factory/internal/runner"
	"github.com/claptimes5/factory/internal/state"
	"github.com/claptimes5/factory/internal/store"
)

// The orchestrator is a short-lived, read-only Claude run that plans and
// steers through the daemon's MCP tools. It never edits code, pushes or
// merges; every tool call is checked here against policy and written to the
// audit trail with actor "orchestrator" and the reason it gave.

// LabelEpic marks an issue for the orchestrator to decompose.
const LabelEpic = "factory:epic"

// OrchestrateInput is carried by an orchestrate job: why it runs.
type OrchestrateInput struct {
	Reasons []string `json:"reasons"`
	Summary bool     `json:"summary,omitempty"` // write the daily summary
}

// Orchestrate requests an orchestrator run. Requests coalesce: at most one
// orchestrate job is ever queued, and reasons accumulate on it.
func (p *Pipeline) Orchestrate(ctx context.Context, reason string, summary bool) error {
	spent, err := p.Store.SpendByKind(ctx, store.Today(), "orchestrate")
	if err != nil {
		return err
	}
	if spent >= p.Cfg.Orchestrator.DailyBudget {
		return p.Store.AddEvent(ctx, store.Event{Actor: "daemon", Type: "orchestrate_skipped",
			Reason: fmt.Sprintf("orchestration budget spent ($%.2f of $%.2f): %s", spent, p.Cfg.Orchestrator.DailyBudget, reason)})
	}
	queued, err := p.Store.ListJobs(ctx, store.JobFilter{Kind: "orchestrate", State: state.Queued})
	if err != nil {
		return err
	}
	if len(queued) > 0 {
		var in OrchestrateInput
		_ = json.Unmarshal([]byte(queued[0].Input), &in)
		in.Reasons = append(in.Reasons, reason)
		in.Summary = in.Summary || summary
		b, _ := json.Marshal(in)
		return p.Store.SetJobInput(ctx, queued[0].ID, string(b))
	}
	r := p.Cfg.Routing["orchestrate"]
	b, _ := json.Marshal(OrchestrateInput{Reasons: []string{reason}, Summary: summary})
	_, err = p.Store.EnqueueJob(ctx, store.Job{Kind: "orchestrate", Harness: r.Harness, Model: r.Model, Effort: r.Effort,
		Skillset: r.Skills, BudgetUSD: p.Cfg.Orchestrator.RunBudget, Priority: 10, Input: string(b)})
	return err
}

// coalesceOrchestrate merges every queued orchestrate job into the oldest,
// so triggers that piled up while the daemon was down (including a run it
// interrupted) fire as one fresh run.
func (p *Pipeline) coalesceOrchestrate(ctx context.Context) error {
	queued, err := p.Store.ListJobs(ctx, store.JobFilter{Kind: "orchestrate", State: state.Queued})
	if err != nil || len(queued) < 2 {
		return err
	}
	sort.Slice(queued, func(a, b int) bool { return queued[a].ID < queued[b].ID })
	var in OrchestrateInput
	for _, j := range queued {
		var x OrchestrateInput
		_ = json.Unmarshal([]byte(j.Input), &x)
		in.Reasons = append(in.Reasons, x.Reasons...)
		in.Summary = in.Summary || x.Summary
	}
	for _, j := range queued[1:] {
		if err := p.Store.CancelQueued(ctx, j.ID, "daemon", fmt.Sprintf("coalesced into job %d", queued[0].ID)); err != nil {
			return err
		}
	}
	b, _ := json.Marshal(in)
	return p.Store.SetJobInput(ctx, queued[0].ID, string(b))
}

// orchestratorTriggers runs on each tick: the schedule, the daily summary
// and a drained queue while ready work exists. (Epics and repeated failures
// trigger from Intake and finish.)
func (p *Pipeline) orchestratorTriggers(ctx context.Context, now time.Time) {
	if p.lastOrch.IsZero() {
		p.lastOrch = now // the first scheduled run is one interval after start
	}
	if now.Sub(p.lastOrch) >= p.Cfg.Orchestrator.Interval.Duration {
		p.lastOrch = now
		p.warn(p.Orchestrate(ctx, "scheduled run", false), "orchestrate")
	}
	day := now.Format("2006-01-02")
	if now.Hour() >= p.Cfg.Orchestrator.SummaryHour && p.lastSummary != day {
		p.lastSummary = day
		p.warn(p.Orchestrate(ctx, "daily summary", true), "orchestrate")
	}
}

// drainedTrigger asks the orchestrator for help when the queue is empty but
// work is stuck: an item is ready with no job, or blocked on dependencies
// none of which is moving. Waiting on a human's PR review is not stuck. Each
// distinct set of stuck items triggers once.
func (p *Pipeline) drainedTrigger(ctx context.Context) {
	if time.Since(p.lastDrain) < 30*time.Minute {
		return
	}
	queued, _ := p.Store.CountJobs(ctx, state.Queued, "")
	running, _ := p.Store.CountJobs(ctx, state.Running, "")
	if queued+running > 0 {
		return
	}
	p.lastDrain = time.Now()
	items, err := p.Store.ListItems(ctx, state.Ready, state.Blocked)
	if err != nil {
		return
	}
	var stuck []string
	for _, it := range items {
		if it.Stage == state.Blocked && p.depsMoving(ctx, it.ID) {
			continue
		}
		stuck = append(stuck, fmt.Sprintf("#%d", it.IssueNumber))
	}
	if len(stuck) == 0 {
		return
	}
	key := strings.Join(stuck, ",")
	if seen, err := p.Store.HasEvent(ctx, "drain_seen", key); err != nil || seen {
		return
	}
	p.warn(p.Store.AddEvent(ctx, store.Event{Actor: "daemon", Type: "drain_seen", Reason: key}), "AddEvent")
	p.warn(p.Orchestrate(ctx, "queue drained; stuck items: "+key, false), "orchestrate")
}

// depsMoving reports whether any dependency of an item is still progressing
// (in the pipeline or waiting on a human), as opposed to parked or blocked.
func (p *Pipeline) depsMoving(ctx context.Context, itemID int64) bool {
	deps, err := p.Store.Dependencies(ctx, itemID)
	if err != nil {
		return false
	}
	for _, d := range deps {
		it, err := p.Store.GetItem(ctx, d)
		if err != nil {
			continue
		}
		switch it.Stage {
		case state.Triage, state.Spec, state.Ready, state.Implementing, state.Reviewing, state.AwaitingHuman:
			return true
		}
	}
	return false
}

// failedTwice triggers the orchestrator when an item's last two jobs failed.
func (p *Pipeline) failedTwice(ctx context.Context, j *store.Job) {
	if j.ItemID == nil {
		return
	}
	jobs, err := p.Store.ListJobs(ctx, store.JobFilter{ItemID: *j.ItemID, Limit: 2})
	if err != nil || len(jobs) < 2 {
		return
	}
	bad := func(s state.JobState) bool { return s == state.Failed || s == state.TimedOut }
	if bad(jobs[0].State) && bad(jobs[1].State) {
		it, err := p.Store.GetItem(ctx, *j.ItemID)
		if err != nil {
			return
		}
		p.warn(p.Orchestrate(ctx, fmt.Sprintf("item %d (issue #%d) failed twice in a row; last error: %s", it.ID, it.IssueNumber, truncate(jobs[0].Error, 300)), false), "orchestrate")
	}
}

// intakeEpics triggers one orchestrator run per new factory:epic issue.
func (p *Pipeline) intakeEpics(ctx context.Context) error {
	epics, err := p.GH.ListIssues(ctx, LabelEpic)
	if err != nil {
		return err
	}
	for _, is := range epics {
		key := fmt.Sprintf("%s#%d", p.Cfg.Repo, is.Number)
		seen, err := p.Store.HasEvent(ctx, "epic_seen", key)
		if err != nil || seen {
			continue
		}
		if err := p.Store.AddEvent(ctx, store.Event{Actor: "daemon", Type: "epic_seen", Reason: key}); err != nil {
			return err
		}
		p.warn(p.Orchestrate(ctx, fmt.Sprintf("new epic #%d: %s", is.Number, is.Title), false), "orchestrate")
	}
	return nil
}

// --- running the orchestrator ---

func (p *Pipeline) runOrchestrate(ctx context.Context, j *store.Job) error {
	dir := p.jobDir(j.ID)
	out := filepath.Join(dir, "out")
	var in OrchestrateInput
	_ = json.Unmarshal([]byte(j.Input), &in)
	fail := func(run agentRun, st state.JobState, msg string) error {
		if p.stopIfCancelled(ctx, j, nil, run) {
			return nil
		}
		p.finish(ctx, j, run, st, "", msg)
		return nil // the next trigger starts a fresh run; nothing to retry
	}
	ws, err := p.Git.Prepare(ctx, dir, p.Cfg.BaseBranch, "", false, false)
	if err != nil {
		return fail(agentRun{}, state.Failed, err.Error())
	}
	sockDir := filepath.Join(dir, "run")
	if err := os.MkdirAll(sockDir, 0o700); err != nil {
		return fail(agentRun{}, state.Failed, err.Error())
	}
	sctx, stop := context.WithCancel(ctx)
	defer stop()
	srv := &mcp.Server{Name: "factory", Tools: p.OrchestratorTools(j.ID)}
	if _, err := srv.ListenUnix(sctx, filepath.Join(sockDir, "mcp.sock")); err != nil {
		return fail(agentRun{}, state.Failed, err.Error())
	}
	bin, err := os.Executable()
	if err != nil {
		return fail(agentRun{}, state.Failed, err.Error())
	}
	sock, binPath := filepath.Join(sockDir, "mcp.sock"), bin
	var mounts []runner.Mount
	if p.Cfg.Runner.Kind == "docker" {
		mounts = append(mounts, runner.Mount{Host: sockDir, Container: "/run/factory"}, runner.Mount{Host: bin, Container: "/opt/factory/factory", ReadOnly: true})
		sock, binPath = "/run/factory/mcp.sock", "/opt/factory/factory"
	}
	mcpCfg, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{
		"factory": map[string]any{"type": "stdio", "command": binPath, "args": []string{"mcp-bridge", sock}},
	}})

	snapshot, _ := p.statusText(ctx)
	prompt, err := p.render("orchestrate", PromptData{Repo: p.Cfg.Repo, Reasons: in.Reasons, Summary: in.Summary, Status: snapshot})
	if err != nil {
		return fail(agentRun{}, state.Failed, err.Error())
	}
	run := p.runAgentWith(ctx, j, ws.Repo, out, prompt, `{"type":"object","additionalProperties":false,"required":["summary"],"properties":{"summary":{"type":"string"}}}`,
		p.Cfg.Timeout("orchestrate"), agentExtras{mounts: mounts, mcpConfig: string(mcpCfg), tools: []string{"mcp__factory"}})
	if p.stopIfCancelled(ctx, j, nil, run) {
		return nil
	}
	if st, msg, bad := runFailure(run); bad {
		return fail(run, st, msg)
	}
	var v struct {
		Summary string `json:"summary"`
	}
	if err := json.Unmarshal([]byte(extractJSON(run.final)), &v); err != nil || strings.TrimSpace(v.Summary) == "" {
		return fail(run, state.Failed, "orchestrator verdict: missing summary")
	}
	norm, _ := json.Marshal(v)
	p.finish(ctx, j, run, state.Succeeded, string(norm), "")
	p.warn(p.Store.AddEvent(ctx, store.Event{Actor: "orchestrator", JobID: &j.ID, Type: "run_summary", Reason: truncate(v.Summary, 2000)}), "AddEvent")
	if in.Summary {
		p.notify(ctx, "Daily summary\n\n"+v.Summary)
	}
	return nil
}

func extractJSON(s string) string {
	s = strings.TrimSpace(s)
	i, j := strings.IndexByte(s, '{'), strings.LastIndexByte(s, '}')
	if i < 0 || j < i {
		return s
	}
	return s[i : j+1]
}

// statusText is the deterministic snapshot given to the orchestrator and
// used by queue_status.
func (p *Pipeline) statusText(ctx context.Context) (string, error) {
	items, err := p.Store.ListItems(ctx)
	if err != nil {
		return "", err
	}
	byStage := map[state.Stage]int{}
	for _, it := range items {
		byStage[it.Stage]++
	}
	var stages []string
	for st, n := range byStage {
		stages = append(stages, fmt.Sprintf("%s=%d", st, n))
	}
	sort.Strings(stages)
	q, _ := p.Store.CountJobs(ctx, state.Queued, "")
	r, _ := p.Store.CountJobs(ctx, state.Running, "")
	spent, _ := p.Store.SpendOn(ctx, store.Today())
	orch, _ := p.Store.SpendByKind(ctx, store.Today(), "orchestrate")
	return fmt.Sprintf("items: %s\njobs: queued=%d running=%d (max %d concurrent)\nspend today: $%.2f of $%.2f (orchestration $%.2f of $%.2f)\nawaiting human: %d of max %d",
		strings.Join(stages, " "), q, r, p.Cfg.Budgets.ConcurrentJobs, spent, p.Cfg.Budgets.DailyTotal, orch, p.Cfg.Orchestrator.DailyBudget,
		byStage[state.AwaitingHuman], p.Cfg.Budgets.AwaitingHumanMax), nil
}

// --- tools ---

func schema(props string, required ...string) json.RawMessage {
	req, _ := json.Marshal(required)
	return json.RawMessage(fmt.Sprintf(`{"type":"object","additionalProperties":false,"properties":{%s},"required":%s}`, props, req))
}

const reasonProp = `"reason":{"type":"string","description":"Why you are doing this. Recorded in the audit trail."}`

// OrchestratorTools are the daemon's MCP tools for one orchestrate job.
// Not available: merge, push, closing PRs, editing factory.yaml or prompts,
// marking anything verified.
func (p *Pipeline) OrchestratorTools(jobID int64) []mcp.Tool {
	// audit records a mutating call before it takes effect.
	audit := func(ctx context.Context, tool string, itemID *int64, reason string, args json.RawMessage) error {
		if strings.TrimSpace(reason) == "" {
			return errors.New("reason is required")
		}
		return p.Store.AddEvent(ctx, store.Event{Actor: "orchestrator", JobID: &jobID, ItemID: itemID, Type: tool, Reason: reason, Data: string(args)})
	}
	// Reads are audited too, so the control room shows everything it looked at.
	logRead := func(ctx context.Context, tool string, args json.RawMessage) {
		p.warn(p.Store.AddEvent(ctx, store.Event{Actor: "orchestrator", JobID: &jobID, Type: "read", Reason: tool, Data: truncate(string(args), 500)}), "AddEvent")
	}
	type itemArgs struct {
		Item   int64  `json:"item"`
		Reason string `json:"reason"`
	}
	return []mcp.Tool{
		{Name: "list_items", Description: "List work items, optionally only those in one stage.",
			InputSchema: schema(`"stage":{"type":"string"}`),
			Handler: func(ctx context.Context, raw json.RawMessage) (string, error) {
				logRead(ctx, "list_items", raw)
				var a struct {
					Stage string `json:"stage"`
				}
				if err := json.Unmarshal(raw, &a); err != nil {
					return "", err
				}
				var stages []state.Stage
				if a.Stage != "" {
					stages = append(stages, state.Stage(a.Stage))
				}
				items, err := p.Store.ListItems(ctx, stages...)
				if err != nil {
					return "", err
				}
				type row struct {
					ID         int64  `json:"id"`
					Issue      int    `json:"issue"`
					Title      string `json:"title"`
					Stage      string `json:"stage"`
					Complexity string `json:"complexity,omitempty"`
					Risk       string `json:"risk,omitempty"`
					Priority   int    `json:"priority"`
					PR         int    `json:"pr,omitempty"`
					Parent     *int64 `json:"parent,omitempty"`
				}
				rows := []row{}
				for _, it := range items {
					rows = append(rows, row{it.ID, it.IssueNumber, it.Title, string(it.Stage), it.Complexity, it.Risk, it.Priority, it.PRNumber, it.ParentID})
				}
				b, _ := json.Marshal(rows)
				return string(b), nil
			}},
		{Name: "get_item", Description: "One work item with its dependencies, jobs and recent audit trail.",
			InputSchema: schema(`"item":{"type":"integer"}`, "item"),
			Handler: func(ctx context.Context, raw json.RawMessage) (string, error) {
				logRead(ctx, "get_item", raw)
				var a itemArgs
				if err := json.Unmarshal(raw, &a); err != nil {
					return "", err
				}
				it, err := p.Store.GetItem(ctx, a.Item)
				if err != nil {
					return "", fmt.Errorf("no item %d", a.Item)
				}
				deps, _ := p.Store.Dependencies(ctx, a.Item)
				jobs, _ := p.Store.ListJobs(ctx, store.JobFilter{ItemID: a.Item, Limit: 20})
				evs, _ := p.Store.Events(ctx, a.Item)
				if len(evs) > 20 {
					evs = evs[len(evs)-20:]
				}
				type jrow struct {
					ID      int64   `json:"id"`
					Kind    string  `json:"kind"`
					State   string  `json:"state"`
					Model   string  `json:"model"`
					Attempt int     `json:"attempt"`
					Cost    float64 `json:"cost_usd"`
					Error   string  `json:"error,omitempty"`
				}
				var js []jrow
				for _, j := range jobs {
					js = append(js, jrow{j.ID, j.Kind, string(j.State), j.Model, j.Attempt, j.CostUSD, truncate(j.Error, 500)})
				}
				var es []string
				for _, e := range evs {
					es = append(es, fmt.Sprintf("%s %s %s: %s", e.At[:19], e.Actor, e.Type, truncate(e.Reason, 200)))
				}
				b, _ := json.Marshal(map[string]any{"item": it, "depends_on": deps, "jobs": js, "events": es})
				return string(b), nil
			}},
		{Name: "queue_status", Description: "Queue depth, spend against budgets, and caps.",
			InputSchema: schema(""),
			Handler: func(ctx context.Context, raw json.RawMessage) (string, error) {
				logRead(ctx, "queue_status", raw)
				return p.statusText(ctx)
			}},
		{Name: "get_job_log", Description: "The last lines of a job's log.",
			InputSchema: schema(`"job":{"type":"integer"},"lines":{"type":"integer"}`, "job"),
			Handler: func(ctx context.Context, raw json.RawMessage) (string, error) {
				logRead(ctx, "get_job_log", raw)
				var a struct {
					Job   int64 `json:"job"`
					Lines int   `json:"lines"`
				}
				if err := json.Unmarshal(raw, &a); err != nil {
					return "", err
				}
				j, err := p.Store.GetJob(ctx, a.Job)
				if err != nil || j.LogPath == "" {
					return "", fmt.Errorf("no log for job %d", a.Job)
				}
				if a.Lines <= 0 || a.Lines > 200 {
					a.Lines = 50
				}
				return tailFile(j.LogPath, a.Lines)
			}},
		{Name: "create_issue", Description: "Open a GitHub issue that the factory will pick up, optionally as a child of an epic item.",
			InputSchema: schema(`"title":{"type":"string"},"body":{"type":"string"},"parent":{"type":"integer"},"labels":{"type":"array","items":{"type":"string"}},`+reasonProp, "title", "body", "reason"),
			Handler: func(ctx context.Context, raw json.RawMessage) (string, error) {
				var a struct {
					Title, Body, Reason string
					Parent              int64
					Labels              []string
				}
				if err := json.Unmarshal(raw, &a); err != nil {
					return "", err
				}
				since := time.Now().UTC().Format("2006-01-02") + "T00:00:00"
				n, err := p.Store.CountEventsSince(ctx, "orchestrator", "create_issue", since)
				if err != nil {
					return "", err
				}
				if n >= p.Cfg.Orchestrator.DailyIssueCap {
					return "", fmt.Errorf("refused: daily issue cap (%d) reached", p.Cfg.Orchestrator.DailyIssueCap)
				}
				labels := []string{p.Cfg.Intake.Label}
				for _, l := range a.Labels {
					if strings.HasPrefix(l, "factory:") {
						continue // only the daemon sets factory labels
					}
					labels = append(labels, l)
				}
				if err := audit(ctx, "create_issue", nil, a.Reason, raw); err != nil {
					return "", err
				}
				num, err := p.GH.CreateIssue(ctx, a.Title, a.Body+"\n\n_Opened by the factory orchestrator._", labels)
				if err != nil {
					return "", err
				}
				it, _, err := p.Store.UpsertItem(ctx, p.Cfg.Repo, num, a.Title)
				if err != nil {
					return "", err
				}
				if a.Parent != 0 {
					p.warn(p.Store.SetParent(ctx, it.ID, a.Parent), "SetParent")
				}
				if err := p.enqueueTriage(ctx, it, 1, ""); err != nil {
					return "", err
				}
				return fmt.Sprintf(`{"item":%d,"issue":%d}`, it.ID, num), nil
			}},
		{Name: "set_dependencies", Description: "Make an item wait until other items are done. Cycles are refused.",
			InputSchema: schema(`"item":{"type":"integer"},"depends_on":{"type":"array","items":{"type":"integer"}},`+reasonProp, "item", "depends_on", "reason"),
			Handler: func(ctx context.Context, raw json.RawMessage) (string, error) {
				var a struct {
					Item      int64   `json:"item"`
					DependsOn []int64 `json:"depends_on"`
					Reason    string  `json:"reason"`
				}
				if err := json.Unmarshal(raw, &a); err != nil {
					return "", err
				}
				if strings.TrimSpace(a.Reason) == "" {
					return "", errors.New("reason is required")
				}
				for _, d := range append([]int64{a.Item}, a.DependsOn...) {
					if _, err := p.Store.GetItem(ctx, d); err != nil {
						return "", fmt.Errorf("no item %d", d)
					}
				}
				if err := p.Store.SetDependencies(ctx, a.Item, a.DependsOn, "orchestrator", a.Reason); err != nil {
					return "", err
				}
				return "ok", nil
			}},
		{Name: "set_priority", Description: "Reorder the queue: higher runs first.",
			InputSchema: schema(`"item":{"type":"integer"},"priority":{"type":"integer"},`+reasonProp, "item", "priority", "reason"),
			Handler: func(ctx context.Context, raw json.RawMessage) (string, error) {
				var a struct {
					Item     int64  `json:"item"`
					Priority int    `json:"priority"`
					Reason   string `json:"reason"`
				}
				if err := json.Unmarshal(raw, &a); err != nil {
					return "", err
				}
				if err := audit(ctx, "set_priority", &a.Item, a.Reason, raw); err != nil {
					return "", err
				}
				return "ok", p.Store.SetPriority(ctx, a.Item, a.Priority)
			}},
		{Name: "enqueue", Description: "Queue a stage for an item: triage, or implement (optionally setting complexity S, M or L).",
			InputSchema: schema(`"item":{"type":"integer"},"stage":{"type":"string","enum":["triage","implement"]},"complexity":{"type":"string","enum":["S","M","L"]},`+reasonProp, "item", "stage", "reason"),
			Handler: func(ctx context.Context, raw json.RawMessage) (string, error) {
				var a struct {
					Item       int64  `json:"item"`
					Stage      string `json:"stage"`
					Complexity string `json:"complexity"`
					Reason     string `json:"reason"`
				}
				if err := json.Unmarshal(raw, &a); err != nil {
					return "", err
				}
				if err := p.checkEnqueue(ctx, a.Item, a.Stage); err != nil {
					return "", err
				}
				if err := audit(ctx, "enqueue", &a.Item, a.Reason, raw); err != nil {
					return "", err
				}
				return "ok", p.enqueueStage(ctx, a.Item, a.Stage, a.Complexity)
			}},
		{Name: "retry", Description: "Requeue a finished job, optionally one model tier up.",
			InputSchema: schema(`"job":{"type":"integer"},"tier_up":{"type":"boolean"},`+reasonProp, "job", "reason"),
			Handler: func(ctx context.Context, raw json.RawMessage) (string, error) {
				var a struct {
					Job    int64  `json:"job"`
					TierUp bool   `json:"tier_up"`
					Reason string `json:"reason"`
				}
				if err := json.Unmarshal(raw, &a); err != nil {
					return "", err
				}
				if err := p.checkBudget(ctx); err != nil {
					return "", err
				}
				if err := audit(ctx, "retry", nil, a.Reason, raw); err != nil {
					return "", err
				}
				return "ok", p.Retry(ctx, a.Job, a.TierUp)
			}},
		{Name: "park", Description: "Park an item for a human, with the reason they will read.",
			InputSchema: schema(`"item":{"type":"integer"},`+reasonProp, "item", "reason"),
			Handler: func(ctx context.Context, raw json.RawMessage) (string, error) {
				var a itemArgs
				if err := json.Unmarshal(raw, &a); err != nil {
					return "", err
				}
				if err := audit(ctx, "park", &a.Item, a.Reason, raw); err != nil {
					return "", err
				}
				return "ok", p.Park(ctx, a.Item, "(orchestrator) "+a.Reason)
			}},
		{Name: "notify_human", Description: "Send a short message to the human's notification channel.",
			InputSchema: schema(`"message":{"type":"string"},"item":{"type":"integer"}`, "message"),
			Handler: func(ctx context.Context, raw json.RawMessage) (string, error) {
				var a struct {
					Message string `json:"message"`
					Item    int64  `json:"item"`
				}
				if err := json.Unmarshal(raw, &a); err != nil {
					return "", err
				}
				since := time.Now().UTC().Add(-time.Hour).Format(store.TimeFormat)
				n, err := p.Store.CountEventsSince(ctx, "orchestrator", "notify_human", since)
				if err != nil {
					return "", err
				}
				if n >= p.Cfg.Orchestrator.NotifyPerHour {
					return "", fmt.Errorf("refused: notification rate limit (%d per hour)", p.Cfg.Orchestrator.NotifyPerHour)
				}
				var item *int64
				if a.Item != 0 {
					item = &a.Item
				}
				if err := audit(ctx, "notify_human", item, truncate(a.Message, 500), raw); err != nil {
					return "", err
				}
				p.notify(ctx, truncate(a.Message, 2000))
				return "sent", nil
			}},
	}
}

func (p *Pipeline) checkBudget(ctx context.Context) error {
	spent, err := p.Store.SpendOn(ctx, store.Today())
	if err != nil {
		return err
	}
	if spent >= p.Cfg.Budgets.DailyTotal {
		return fmt.Errorf("refused: daily budget spent ($%.2f of $%.2f)", spent, p.Cfg.Budgets.DailyTotal)
	}
	return nil
}

// checkEnqueue enforces which stages may be queued for an item in its
// current state, plus the caps and the budget.
func (p *Pipeline) checkEnqueue(ctx context.Context, itemID int64, stage string) error {
	it, err := p.Store.GetItem(ctx, itemID)
	if err != nil {
		return fmt.Errorf("no item %d", itemID)
	}
	if active, err := p.activeJob(ctx, itemID); err != nil || active != nil {
		if active != nil {
			return fmt.Errorf("refused: item already has job %d (%s %s)", active.ID, active.Kind, active.State)
		}
		return err
	}
	if err := p.checkBudget(ctx); err != nil {
		return err
	}
	switch stage {
	case "triage":
		if it.Stage != state.Triage && it.Stage != state.Parked {
			return fmt.Errorf("refused: triage needs an item in triage or parked, this one is %s", it.Stage)
		}
	case "implement":
		switch it.Stage {
		case state.Ready, state.Parked, state.Blocked:
		default:
			return fmt.Errorf("refused: implement needs an item that is ready, parked or blocked, this one is %s", it.Stage)
		}
		if ok, err := p.depsDone(ctx, itemID); err != nil || !ok {
			return errors.New("refused: the item's dependencies are not done")
		}
		waiting, _ := p.Store.ListItems(ctx, state.AwaitingHuman)
		if len(waiting) >= p.Cfg.Budgets.AwaitingHumanMax {
			return fmt.Errorf("refused: %d items already await human review", len(waiting))
		}
	default:
		return fmt.Errorf("refused: unknown stage %q", stage)
	}
	return nil
}

func (p *Pipeline) enqueueStage(ctx context.Context, itemID int64, stage, complexity string) error {
	it, err := p.Store.GetItem(ctx, itemID)
	if err != nil {
		return err
	}
	if stage == "triage" {
		if err := p.Store.SetStage(ctx, it.ID, state.Triage, "orchestrator", "enqueue triage"); err != nil {
			return err
		}
		return p.enqueueTriage(ctx, it, 1, "")
	}
	if complexity != "" {
		if err := p.Store.UpdateItem(ctx, it.ID, store.ItemUpdate{Complexity: &complexity}); err != nil {
			return err
		}
		it.Complexity = complexity
	}
	if it.Complexity == "" {
		return errors.New("refused: the item has no complexity; triage it or pass one")
	}
	if it.Stage != state.Ready {
		if err := p.Store.SetStage(ctx, it.ID, state.Ready, "orchestrator", "enqueue implement"); err != nil {
			return err
		}
	}
	if _, err := p.enqueueImplement(ctx, it, 1, ImplementInput{}); err != nil {
		return err
	}
	return p.Store.SetStage(ctx, it.ID, state.Implementing, "orchestrator", "implement queued")
}

// depsDone reports whether every dependency of an item is done.
func (p *Pipeline) depsDone(ctx context.Context, itemID int64) (bool, error) {
	deps, err := p.Store.Dependencies(ctx, itemID)
	if err != nil {
		return false, err
	}
	for _, d := range deps {
		it, err := p.Store.GetItem(ctx, d)
		if err != nil {
			return false, err
		}
		if it.Stage != state.Done {
			return false, nil
		}
	}
	return true, nil
}

// unblock moves blocked items whose dependencies are done on to implement.
func (p *Pipeline) unblock(ctx context.Context) error {
	blocked, err := p.Store.ListItems(ctx, state.Blocked)
	if err != nil {
		return err
	}
	for _, it := range blocked {
		ok, err := p.depsDone(ctx, it.ID)
		if err != nil || !ok || it.Complexity == "" {
			continue
		}
		if err := p.Store.SetStage(ctx, it.ID, state.Ready, "daemon", "dependencies done"); err != nil {
			return err
		}
		if _, err := p.enqueueImplement(ctx, it, 1, ImplementInput{}); err != nil {
			return err
		}
		if err := p.Store.SetStage(ctx, it.ID, state.Implementing, "daemon", "implement queued"); err != nil {
			return err
		}
	}
	return nil
}

func tailFile(path string, n int) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	var lines []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		lines = append(lines, truncate(sc.Text(), 600))
		if len(lines) > n {
			lines = lines[1:]
		}
	}
	return strings.Join(lines, "\n"), sc.Err()
}
