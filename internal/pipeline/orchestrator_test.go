package pipeline

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/claptimes5/factory/internal/config"
	"github.com/claptimes5/factory/internal/github"
	"github.com/claptimes5/factory/internal/harness"
	"github.com/claptimes5/factory/internal/mcp"
	"github.com/claptimes5/factory/internal/state"
	"github.com/claptimes5/factory/internal/store"
)

// mcpClient speaks to the daemon's MCP socket the way the agent's bridge does.
type mcpClient struct {
	t    *testing.T
	conn net.Conn
	sc   *bufio.Scanner
	id   int
}

func dialMCP(t *testing.T, req harness.RunRequest) *mcpClient {
	t.Helper()
	var cfg struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(req.MCPConfig), &cfg); err != nil {
		t.Fatalf("mcp config %q: %v", req.MCPConfig, err)
	}
	srv := cfg.MCPServers["factory"]
	if len(srv.Args) != 2 || srv.Args[0] != "mcp-bridge" {
		t.Fatalf("bridge args = %v", srv.Args)
	}
	if !req.ReadOnly || len(req.Tools) != 1 || req.Tools[0] != "mcp__factory" {
		t.Fatalf("orchestrator must run read-only with only the factory tools: %+v", req)
	}
	c, err := net.Dial("unix", srv.Args[1])
	if err != nil {
		t.Fatal(err)
	}
	m := &mcpClient{t: t, conn: c, sc: bufio.NewScanner(c)}
	m.sc.Buffer(make([]byte, 1<<20), 16<<20)
	m.rpc("initialize", map[string]any{"protocolVersion": mcp.ProtocolVersion})
	return m
}

func (m *mcpClient) rpc(method string, params any) json.RawMessage {
	m.t.Helper()
	m.id++
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": m.id, "method": method, "params": params})
	if _, err := m.conn.Write(append(b, '\n')); err != nil {
		m.t.Fatal(err)
	}
	if !m.sc.Scan() {
		m.t.Fatalf("%s: no response: %v", method, m.sc.Err())
	}
	var r struct {
		Result json.RawMessage `json:"result"`
		Error  *struct{ Message string }
	}
	if err := json.Unmarshal(m.sc.Bytes(), &r); err != nil || r.Error != nil {
		m.t.Fatalf("%s: %s %v", method, m.sc.Text(), err)
	}
	return r.Result
}

// call runs a tool and returns its text and whether it was a tool error.
func (m *mcpClient) call(name string, args any) (string, bool) {
	m.t.Helper()
	var r struct {
		Content []struct{ Text string }
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(m.rpc("tools/call", map[string]any{"name": name, "arguments": args}), &r); err != nil {
		m.t.Fatal(err)
	}
	var txt []string
	for _, c := range r.Content {
		txt = append(txt, c.Text)
	}
	return strings.Join(txt, "\n"), r.IsError
}

func orchestratorOn(e *env) {
	e.p.Cfg.Orchestrator.On = true
	// Keep the clock triggers quiet unless a test wants them.
	e.p.lastOrch = time.Now()
	e.p.lastSummary = time.Now().Format("2006-01-02")
}

// An epic is split into issues through the MCP tools; the dependent child
// waits until its dependency's PR merges, then builds.
func TestEpicSplitsIntoOrderedIssues(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	orchestratorOn(e)
	// Labeled ready too: an epic must still never be built whole.
	e.gh.issues[50] = &github.Issue{Number: 50, Title: "Arithmetic", Body: "Add and Sub.", Labels: []string{LabelEpic, "factory:ready"}}
	e.p.Cfg.Orchestrator.Labels = []string{"math"}
	runs := 0
	e.claude.orch = func(req harness.RunRequest) (string, error) {
		runs++
		if runs > 1 {
			return `{"summary":"nothing to do"}`, nil
		}
		if !strings.Contains(req.Prompt, "new epic #50") {
			t.Errorf("prompt lacks the trigger reason:\n%s", req.Prompt)
		}
		m := dialMCP(t, req)
		defer m.conn.Close()
		var list struct {
			Tools []struct{ Name string }
		}
		json.Unmarshal(m.rpc("tools/list", nil), &list)
		var names []string
		for _, tl := range list.Tools {
			names = append(names, tl.Name)
		}
		for _, banned := range []string{"merge", "push", "close_pr"} {
			if strings.Contains(strings.Join(names, ","), banned) {
				t.Errorf("tool list exposes %s: %v", banned, names)
			}
		}
		out, _ := m.call("list_items", map[string]any{"stage": "epic"})
		var epics []struct{ ID int64 }
		json.Unmarshal([]byte(out), &epics)
		if len(epics) != 1 {
			t.Fatalf("epic items = %s", out)
		}
		epic := epics[0].ID
		if out, _ := m.call("get_issue", map[string]any{"issue": 50}); !strings.Contains(out, "Add and Sub.") {
			t.Fatalf("get_issue = %s", out)
		}
		create := func(args map[string]any) int64 {
			out, isErr := m.call("create_issue", args)
			if isErr {
				t.Fatalf("create_issue: %s", out)
			}
			var r struct{ Item int64 }
			json.Unmarshal([]byte(strings.SplitN(out, "\n", 2)[0]), &r)
			return r.Item
		}
		first := create(map[string]any{"title": "Add an Add function", "body": "Part of #50.", "parent": epic,
			"labels": []string{"Factory:Running", "math", "automerge"}, "reason": "split epic #50"})
		second := create(map[string]any{"title": "Add a Sub function", "body": "Part of #50.", "parent": epic,
			"depends_on": []int64{first}, "reason": "Sub reuses Add's file"})
		if out, isErr := m.call("set_dependencies", map[string]any{"item": first, "depends_on": []int64{second}, "reason": "oops"}); !isErr {
			t.Fatalf("a cycle must be refused, got %s", out)
		}
		if out, isErr := m.call("create_issue", map[string]any{"title": "x", "body": "y", "parent": 999, "reason": "r"}); !isErr {
			t.Fatalf("a missing parent must be refused, got %s", out)
		}
		return `{"summary":"split #50 into two issues"}`, nil
	}
	e.claude.builds = []func(string) error{goodBuild, func(work string) error {
		// The fake remote never merges, so this build carries Add too.
		writeFile(work, "sub.go", "package fixture\n\nfunc Sub(a, b int) int { return a - b }\n")
		return goodBuild(work)
	}}
	e.codex.reviews = []string{pass, pass}

	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if runs != 1 {
		js, _ := e.p.Store.ListJobs(ctx, store.JobFilter{Kind: "orchestrate"})
		for _, j := range js {
			t.Logf("job %d %s %s", j.ID, j.State, j.Error)
		}
		t.Fatalf("orchestrator runs = %d", runs)
	}
	first, second := e.item(t, 51), e.item(t, 52)
	if first.Stage != state.AwaitingHuman || second.Stage != state.Blocked {
		t.Fatalf("stages = %s, %s", first.Stage, second.Stage)
	}
	if ls := e.gh.issues[51].Labels; !hasLabel(ls, "factory:ready") || !hasLabel(ls, "math") || hasLabel(ls, "Factory:Running") || hasLabel(ls, "automerge") {
		t.Fatalf("labels = %v", ls)
	}
	evs, _ := e.p.Store.EventsByActor(ctx, "orchestrator", 50)
	audited := map[string]bool{}
	for _, ev := range evs {
		audited[ev.Type] = ev.Reason != ""
	}
	if !audited["create_issue"] || !audited["set_dependencies"] || !audited["run_summary"] {
		t.Fatalf("audit trail = %+v", evs)
	}

	// The human merges the first PR; the second is unblocked and built.
	e.gh.prs["factory/issue-51"].Merged = true
	e.p.lastPoll = time.Time{}
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if st := e.item(t, 52).Stage; st != state.AwaitingHuman {
		t.Fatalf("second item = %s", st)
	}
	if e.claude.calls["triage"] != 2 {
		t.Fatalf("triage runs = %d; the epic itself must not be triaged", e.claude.calls["triage"])
	}
	// Both children merged: the epic is done.
	e.gh.prs["factory/issue-52"].Merged = true
	e.p.lastPoll = time.Time{}
	e.p.Drain(ctx)
	if st := e.item(t, 50).Stage; st != state.Done {
		t.Fatalf("epic = %s", st)
	}
	// The epic is seen once, however often intake runs.
	e.p.lastPoll = time.Time{}
	e.p.Drain(ctx)
	if runs != 1 {
		t.Fatalf("orchestrator runs after re-intake = %d", runs)
	}
}

func toolsByName(p *Pipeline, jobID int64) map[string]mcp.Tool {
	m := map[string]mcp.Tool{}
	for _, t := range p.OrchestratorTools(jobID) {
		m[t.Name] = t
	}
	return m
}

func callTool(t *testing.T, tools map[string]mcp.Tool, name string, args any) (string, error) {
	t.Helper()
	b, _ := json.Marshal(args)
	return tools[name].Handler(context.Background(), b)
}

func TestOrchestratorToolPolicy(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	e.p.Cfg.Orchestrator.DailyIssueCap = 1
	e.p.Cfg.Orchestrator.NotifyPerHour = 1
	var sent []string
	e.p.Notify = func(_ context.Context, msg string) { sent = append(sent, msg) }
	tools := toolsByName(e.p, 0)

	if _, err := callTool(t, tools, "create_issue", map[string]any{"title": "x", "body": "y"}); err == nil {
		t.Fatal("a mutating call without a reason must be refused")
	}
	out, err := callTool(t, tools, "create_issue", map[string]any{"title": "x", "body": "y", "reason": "r"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := callTool(t, tools, "create_issue", map[string]any{"title": "x2", "body": "y", "reason": "r"}); err == nil || !strings.Contains(err.Error(), "daily issue cap") {
		t.Fatalf("issue cap: %v", err)
	}
	var r struct{ Item int64 }
	json.Unmarshal([]byte(out), &r)

	// Implement is refused while triage is queued, and for unmet dependencies.
	if _, err := callTool(t, tools, "enqueue", map[string]any{"item": r.Item, "stage": "implement", "complexity": "S", "reason": "r"}); err == nil {
		t.Fatal("enqueue over an active job must be refused")
	}
	parked, _, _ := e.p.Store.UpsertItem(ctx, "o/r", 98, "parked")
	e.p.Store.SetStage(ctx, parked.ID, state.Parked, "test", "")
	if _, err := callTool(t, tools, "enqueue", map[string]any{"item": parked.ID, "stage": "triage", "reason": "r"}); err == nil || !strings.Contains(err.Error(), "parked") {
		t.Fatalf("unparking is for humans: %v", err)
	}
	other, _, _ := e.p.Store.UpsertItem(ctx, "o/r", 99, "other")
	e.p.Store.SetStage(ctx, other.ID, state.Ready, "test", "")
	e.p.Store.SetDependencies(ctx, other.ID, []int64{r.Item}, "test", "")
	if _, err := callTool(t, tools, "enqueue", map[string]any{"item": other.ID, "stage": "implement", "complexity": "S", "reason": "r"}); err == nil || !strings.Contains(err.Error(), "dependencies") {
		t.Fatalf("unmet dependencies: %v", err)
	}
	if _, err := callTool(t, tools, "enqueue", map[string]any{"item": other.ID, "stage": "review", "reason": "r"}); err == nil {
		t.Fatal("only triage and implement may be enqueued")
	}

	if _, err := callTool(t, tools, "notify_human", map[string]any{"message": "hi"}); err != nil {
		t.Fatal(err)
	}
	if _, err := callTool(t, tools, "notify_human", map[string]any{"message": "again"}); err == nil {
		t.Fatal("notifications over the hourly limit must be refused")
	}
	if len(sent) != 1 {
		t.Fatalf("sent = %v", sent)
	}

	// Spend at the daily cap: anything that would start work is refused.
	e.p.Cfg.Budgets.DailyTotal = 0
	if _, err := callTool(t, tools, "retry", map[string]any{"job": 1, "reason": "r"}); err == nil || !strings.Contains(err.Error(), "budget") {
		t.Fatalf("retry over budget: %v", err)
	}
	if _, err := callTool(t, tools, "enqueue", map[string]any{"item": r.Item, "stage": "triage", "reason": "r"}); err == nil {
		t.Fatalf("enqueue over budget: %v", err)
	}

	evs, _ := e.p.Store.EventsByActor(ctx, "orchestrator", 50)
	got := map[string]int{}
	for _, ev := range evs {
		got[ev.Type]++
	}
	if got["create_issue"] != 1 || got["notify_human"] != 1 || got["retry"] != 0 {
		t.Fatalf("only allowed calls are audited as actions: %v", got)
	}
}

func TestOrchestrateRequestsCoalesce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	for i := 0; i < 3; i++ {
		if err := e.p.Orchestrate(ctx, fmt.Sprintf("reason %d", i), i == 1); err != nil {
			t.Fatal(err)
		}
	}
	jobs, _ := e.p.Store.ListJobs(ctx, store.JobFilter{Kind: "orchestrate"})
	if len(jobs) != 1 {
		t.Fatalf("jobs = %d", len(jobs))
	}
	var in OrchestrateInput
	json.Unmarshal([]byte(jobs[0].Input), &in)
	if len(in.Reasons) != 3 || !in.Summary {
		t.Fatalf("input = %+v", in)
	}

	// The orchestration budget is separate and, once spent, skips requests.
	e.p.Cfg.Orchestrator.DailyBudget = 0
	if err := e.p.Orchestrate(ctx, "more", false); err != nil {
		t.Fatal(err)
	}
	if ok, _ := e.p.Store.HasEvent(ctx, "orchestrate_skipped", "orchestration budget spent ($0.00 of $0.00): more"); !ok {
		t.Fatal("a skipped request must be recorded")
	}
}

// Two failures in a row on an item ask the orchestrator to look at it, and
// the daily summary goes to the notification channel.
func TestFailuresAndSummaryTriggerOrchestrator(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	orchestratorOn(e)
	e.p.lastSummary = "" // the summary hour has passed today
	e.p.Cfg.Orchestrator.SummaryHour = 0
	var sent []string
	e.p.Notify = func(_ context.Context, msg string) { sent = append(sent, msg) }
	var prompts []string
	e.claude.orch = func(req harness.RunRequest) (string, error) {
		prompts = append(prompts, req.Prompt)
		return `{"summary":"looked at it"}`, nil
	}
	e.addIssue(60)
	e.claude.builds = []func(string) error{badBuild, badBuild, badBuild, badBuild}
	e.p.Drain(ctx)

	all := strings.Join(prompts, "\n---\n")
	if !strings.Contains(all, "daily summary") || !strings.Contains(all, "failed twice in a row") {
		t.Fatalf("orchestrator prompts:\n%s", all)
	}
	var summary bool
	for _, m := range sent {
		summary = summary || strings.HasPrefix(m, "Daily summary")
	}
	if !summary {
		t.Fatalf("notifications = %v", sent)
	}
}

// Triggers that piled up while the daemon was down, plus the run it
// interrupted, fire as one fresh orchestrator run after a restart.
func TestRestartCoalescesOrchestratorRuns(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	for _, in := range []string{`{"reasons":["interrupted"]}`, `{"reasons":["epic"]}`, `{"reasons":["summary"],"summary":true}`} {
		if _, err := e.p.Store.EnqueueJob(ctx, store.Job{Kind: "orchestrate", Harness: "claude", Model: "opus", Input: in}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.p.Store.ClaimJob(ctx); err != nil { // the first was running at shutdown
		t.Fatal(err)
	}
	if err := e.p.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	queued, _ := e.p.Store.ListJobs(ctx, store.JobFilter{Kind: "orchestrate", State: state.Queued})
	if len(queued) != 1 {
		t.Fatalf("queued = %d", len(queued))
	}
	var in OrchestrateInput
	json.Unmarshal([]byte(queued[0].Input), &in)
	if strings.Join(in.Reasons, ",") != "interrupted,epic,summary" || !in.Summary {
		t.Fatalf("input = %+v", in)
	}
}

// Dependencies set after a child was triaged hold back its queued build.
func TestLateDependencyHoldsQueuedBuild(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	a, _, _ := e.p.Store.UpsertItem(ctx, "o/r", 1, "a")
	b, _, _ := e.p.Store.UpsertItem(ctx, "o/r", 2, "b")
	e.p.Store.SetStage(ctx, b.ID, state.Ready, "test", "")
	cx := "S"
	e.p.Store.UpdateItem(ctx, b.ID, store.ItemUpdate{Complexity: &cx})
	b, _ = e.p.Store.GetItem(ctx, b.ID)
	e.p.enqueueImplement(ctx, b, 1, ImplementInput{})
	e.p.Store.SetStage(ctx, b.ID, state.Implementing, "test", "")

	tools := toolsByName(e.p, 0)
	out, err := callTool(t, tools, "set_dependencies", map[string]any{"item": b.ID, "depends_on": []int64{a.ID}, "reason": "b needs a"})
	if err != nil || !strings.Contains(out, "held") {
		t.Fatalf("%s %v", out, err)
	}
	if st := e.item(t, 2).Stage; st != state.Blocked {
		t.Fatalf("stage = %s", st)
	}
	if active, _ := e.p.activeJob(ctx, b.ID); active != nil {
		t.Fatalf("queued build still active: %+v", active)
	}
}

func TestOrchestratorRetryCap(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	e.addIssue(80)
	e.claude.builds = []func(string) error{badBuild, badBuild, badBuild, badBuild, badBuild, badBuild, badBuild}
	e.p.Drain(ctx) // fails on every tier and parks
	jobs, _ := e.p.Store.ListJobs(ctx, store.JobFilter{ItemID: e.item(t, 80).ID, Kind: "implement"})
	tools := toolsByName(e.p, 0)
	if _, err := callTool(t, tools, "retry", map[string]any{"job": jobs[0].ID, "reason": "r"}); err == nil || !strings.Contains(err.Error(), "parked") {
		t.Fatalf("retrying a parked item: %v", err)
	}
	// Unparked into triage by a human; the orchestrator may retry twice.
	e.p.Store.SetStage(ctx, e.item(t, 80).ID, state.Triage, "human", "")
	for i := 0; i < MaxOrchestratorRetries; i++ {
		if _, err := callTool(t, tools, "retry", map[string]any{"job": jobs[len(jobs)-1].ID, "reason": "r"}); err != nil {
			t.Fatalf("retry %d: %v", i, err)
		}
		e.p.Drain(ctx)
		e.p.Store.SetStage(ctx, e.item(t, 80).ID, state.Triage, "human", "")
	}
	if _, err := callTool(t, tools, "retry", map[string]any{"job": jobs[len(jobs)-1].ID, "reason": "r"}); err == nil || !strings.Contains(err.Error(), "already retried") {
		t.Fatalf("third retry: %v", err)
	}
}

func TestEpicEdgeCases(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	orchestratorOn(e)
	// A mixed-case label is still an epic: never built whole.
	e.gh.issues[7] = &github.Issue{Number: 7, Title: "Big", Labels: []string{"Factory:Epic", "factory:ready"}}
	e.claude.orch = func(harness.RunRequest) (string, error) { return `{"summary":"later"}`, nil }
	e.p.Drain(ctx)
	ep := e.item(t, 7)
	if ep.Stage != state.Epic || e.claude.calls["triage"] != 0 {
		t.Fatalf("epic = %s, triage runs %d", ep.Stage, e.claude.calls["triage"])
	}
	tools := toolsByName(e.p, 0)
	if _, err := callTool(t, tools, "create_issue", map[string]any{"title": "c", "body": "b", "parent": ep.ID, "depends_on": []int64{ep.ID}, "reason": "r"}); err == nil || !strings.Contains(err.Error(), "epic") {
		t.Fatalf("depending on an epic deadlocks: %v", err)
	}
	// Parked, then unparked by a human: back to the orchestrator, not triage.
	if _, err := callTool(t, tools, "park", map[string]any{"item": ep.ID, "reason": "unclear"}); err != nil {
		t.Fatal(err)
	}
	e.gh.issues[7].Labels = append(e.gh.issues[7].Labels, LabelRetry)
	e.p.lastPoll = time.Time{}
	e.p.Drain(ctx)
	if st := e.item(t, 7).Stage; st != state.Epic || e.claude.calls["triage"] != 0 {
		t.Fatalf("unparked epic = %s, triage runs %d", st, e.claude.calls["triage"])
	}
}

// Over the build budget only orchestrator runs start, and still one at a time.
func TestOneOrchestratorAtATimeOverBudget(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	e.p.Cfg.Budgets.DailyTotal = 0
	for i := 0; i < 2; i++ {
		e.p.Store.EnqueueJob(ctx, store.Job{Kind: "orchestrate", Harness: "claude", Model: "opus"})
	}
	if j, _ := e.p.claimNext(ctx); j == nil || j.Kind != "orchestrate" {
		t.Fatalf("first claim = %+v", j)
	}
	if j, _ := e.p.claimNext(ctx); j != nil {
		t.Fatalf("a second orchestrator started: %+v", j)
	}
}

// Claude Code drops every tool of a server when one schema is invalid, as
// "required": null is.
func TestToolSchemasHaveRequiredList(t *testing.T) {
	for _, s := range []json.RawMessage{schema(""), schema(`"stage":{"type":"string"}`), schema(`"item":{"type":"integer"}`, "item")} {
		var v struct {
			Required []string `json:"required"`
		}
		if err := json.Unmarshal(s, &v); err != nil || v.Required == nil {
			t.Fatalf("schema %s: required must be a list (err %v)", s, err)
		}
	}
}

// A GitLab repo may be named without its host; an ambiguous name is refused.
func TestPickRepoWithoutHost(t *testing.T) {
	p := &Pipeline{Cfg: &config.Config{Repos: []config.Repo{{Name: "gitlab.com/acme/api"}, {Name: "gitlab.example.com/acme/web"}, {Name: "git.example.org/acme/web"}}}}
	if got, err := p.pickRepo("acme/api"); err != nil || got != "gitlab.com/acme/api" {
		t.Fatalf("acme/api: %q %v", got, err)
	}
	if _, err := p.pickRepo("acme/web"); err == nil {
		t.Fatal("acme/web matches two repos and should be refused")
	}
}

// notPlannedGH closes issues as not planned, as GitHub's client does.
type notPlannedGH struct {
	*fakeGH
	closed []int
}

func (f *notPlannedGH) CloseNotPlanned(_ context.Context, n int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.issues[n].State = "closed"
	f.closed = append(f.closed, n)
	return nil
}

// Splitting an item from chat opens its children, then closes its issue in
// favor of them and makes it their epic.
func TestChatClosesASplitItem(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	gh := &notPlannedGH{fakeGH: e.gh}
	e.p.GH = gh
	e.gh.issues[9] = &github.Issue{Number: 9, Title: "Make lint pass", State: "open"}
	it, _, _ := e.p.Store.UpsertItem(ctx, "o/r", 9, "Make lint pass")
	e.p.Store.SetStage(ctx, it.ID, state.Parked, "test", "")
	tools := map[string]mcp.Tool{}
	for _, tl := range e.p.ChatTools(1) {
		tools[tl.Name] = tl
	}
	if _, err := callTool(t, tools, "close_split", map[string]any{"item": it.ID, "reason": "r"}); err == nil || !strings.Contains(err.Error(), "no child issues") {
		t.Fatalf("closing with no children = %v", err)
	}
	for _, title := range []string{"Lint app/", "Lint test/"} {
		if out, err := callTool(t, tools, "create_issue", map[string]any{"title": title, "body": "b", "parent": it.ID, "reason": "split"}); err != nil {
			t.Fatalf("create_issue: %s %v", out, err)
		}
	}
	if _, err := callTool(t, tools, "close_split", map[string]any{"item": it.ID, "reason": "Too big for one build."}); err != nil {
		t.Fatal(err)
	}
	if len(gh.closed) != 1 || gh.closed[0] != 9 {
		t.Fatalf("closed = %v", gh.closed)
	}
	if cs := e.gh.comments[9]; len(cs) != 1 || !strings.Contains(cs[0], "split into #") || !strings.Contains(cs[0], "Too big") {
		t.Fatalf("comments = %q", cs)
	}
	if got, _ := e.p.Store.GetItem(ctx, it.ID); got.Stage != state.Epic {
		t.Fatalf("stage = %s", got.Stage)
	}
	if n, _ := e.p.Store.CountEventsSince(ctx, "chat", "create_issue", "2000-01-01"); n != 2 {
		t.Errorf("chat's issues audited = %d", n)
	}
	// A building item isn't closed under its build.
	b, _, _ := e.p.Store.UpsertItem(ctx, "o/r", 10, "b")
	e.p.Store.SetStage(ctx, b.ID, state.Ready, "test", "")
	if _, err := callTool(t, tools, "close_split", map[string]any{"item": b.ID, "reason": "r"}); err == nil || !strings.Contains(err.Error(), "only a parked") {
		t.Fatalf("closing a ready item = %v", err)
	}
}
