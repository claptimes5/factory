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
	ctx := context.Background()
	e := setup(t)
	orchestratorOn(e)
	e.gh.issues[50] = &github.Issue{Number: 50, Title: "Arithmetic", Body: "Add and Sub.", Labels: []string{LabelEpic}}
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
		var ids [2]int64
		for i, title := range []string{"Add an Add function", "Add a Sub function"} {
			out, isErr := m.call("create_issue", map[string]any{"title": title, "body": "Part of #50.", "labels": []string{"factory:running", "math"}, "reason": "split epic #50"})
			if isErr {
				t.Fatalf("create_issue: %s", out)
			}
			var r struct{ Item int64 }
			json.Unmarshal([]byte(out), &r)
			ids[i] = r.Item
		}
		if out, isErr := m.call("set_dependencies", map[string]any{"item": ids[1], "depends_on": []int64{ids[0]}, "reason": "Sub reuses Add's file"}); isErr {
			t.Fatalf("set_dependencies: %s", out)
		}
		if out, isErr := m.call("set_dependencies", map[string]any{"item": ids[0], "depends_on": []int64{ids[1]}, "reason": "oops"}); !isErr {
			t.Fatalf("a cycle must be refused, got %s", out)
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
	if ls := e.gh.issues[51].Labels; !hasLabel(ls, "factory:ready") || !hasLabel(ls, "math") || hasLabel(ls, "factory:running") {
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
	other, _, _ := e.p.Store.UpsertItem(ctx, "o/r", 99, "other")
	e.p.Store.SetStage(ctx, other.ID, state.Parked, "test", "")
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
	if _, err := callTool(t, tools, "enqueue", map[string]any{"item": other.ID, "stage": "triage", "reason": "r"}); err == nil || !strings.Contains(err.Error(), "budget") {
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
