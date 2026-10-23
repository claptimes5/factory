package pipeline

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/claptimes5/factory/internal/config"
	"github.com/claptimes5/factory/internal/mcp"
	"github.com/claptimes5/factory/internal/store"
)

// scopedEnv is three repos in two workspaces: app (o/r and o/web) and
// docs (o/docs, its own). extra goes in app's settings.
type scopedEnv struct {
	*env
	web, docs *env
}

func setupScoped(t *testing.T, extra string) *scopedEnv {
	t.Helper()
	e := setup(t)
	cfg, err := config.Parse([]byte(strings.Replace(testConfig, "repo: o/r\n", "repos: [o/r, o/web, o/docs]\nworkspaces:\n  app:\n    repos: [o/r, o/web]\n"+extra, 1)))
	if err != nil {
		t.Fatal(err)
	}
	cfg.DataDir, cfg.PromptsDir, cfg.SkillsDir = e.p.Cfg.DataDir, e.p.Cfg.PromptsDir, e.p.Cfg.SkillsDir
	cfg.Budgets.MinFreeDiskGB = e.p.Cfg.Budgets.MinFreeDiskGB
	cfg.Orchestrator.On = true
	e.p.Cfg = cfg
	web, docs := setup(t), setup(t)
	clients := map[string]RepoClients{
		"o/r":    {GH: e.gh, Git: e.p.Git},
		"o/web":  {GH: web.gh, Git: web.p.Git},
		"o/docs": {GH: docs.gh, Git: docs.p.Git},
	}
	e.p.ClientsFor = func(repo string) RepoClients { return clients[strings.ToLower(repo)] }
	e.p.lastOrch = map[string]time.Time{"app": time.Now(), "docs": time.Now()}
	e.p.lastSummary = map[string]string{"app": time.Now().Format("2006-01-02"), "docs": time.Now().Format("2006-01-02")}
	return &scopedEnv{e, web, docs}
}

func toolsIn(p *Pipeline, jobID int64, ws string, chat bool) map[string]mcp.Tool {
	ts := p.OrchestratorTools(jobID, ws)
	if chat {
		ts = p.ChatTools(jobID, ws)
	}
	m := map[string]mcp.Tool{}
	for _, t := range ts {
		m[t.Name] = t
	}
	return m
}

// An orchestrator run sees and changes its workspace's items and repos
// only, and in a workspace of several repos must say which one an issue
// goes in.
func TestOrchestratorStaysInItsWorkspace(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setupScoped(t, "")
	api, _, _ := e.p.Store.UpsertItem(ctx, "o/r", 1, "API change")
	doc, _, _ := e.p.Store.UpsertItem(ctx, "o/docs", 2, "Docs change")
	app := toolsIn(e.p, 1, "app", false)

	out, err := callTool(t, app, "list_items", map[string]any{})
	if err != nil || !strings.Contains(out, `"repo":"o/r"`) || strings.Contains(out, "o/docs") {
		t.Fatalf("list_items = %s %v", out, err)
	}
	for name, args := range map[string]map[string]any{
		"get_item":         {"item": doc.ID},
		"set_priority":     {"item": doc.ID, "priority": 5, "reason": "r"},
		"park":             {"item": doc.ID, "reason": "r"},
		"set_dependencies": {"item": api.ID, "depends_on": []int64{doc.ID}, "reason": "r"},
	} {
		if _, err := callTool(t, app, name, args); err == nil || !strings.Contains(err.Error(), "workspace docs") {
			t.Errorf("%s on docs' item = %v", name, err)
		}
	}
	issue := func(tools map[string]mcp.Tool, repo string) (string, error) {
		args := map[string]any{"title": "Add it", "body": "b", "reason": "r"}
		if repo != "" {
			args["repo"] = repo
		}
		return callTool(t, tools, "create_issue", args)
	}
	if _, err := issue(app, ""); err == nil || !strings.Contains(err.Error(), "name the repo") {
		t.Fatalf("no repo in a workspace of two = %v", err)
	}
	if _, err := issue(app, "o/docs"); err == nil || !strings.Contains(err.Error(), "o/docs is in workspace docs") {
		t.Fatalf("another workspace's repo = %v", err)
	}
	if out, err := issue(app, "o/web"); err != nil || len(e.web.gh.issues) != 1 {
		t.Fatalf("o/web = %s %v, its issues %d", out, err, len(e.web.gh.issues))
	}
	// A workspace of one repo needs no name.
	if out, err := issue(toolsIn(e.p, 2, "docs", false), ""); err != nil || len(e.docs.gh.issues) != 1 {
		t.Fatalf("docs = %s %v", out, err)
	}
	// Ask is fenced the same way.
	if _, err := issue(toolsIn(e.p, 3, "docs", true), "o/r"); err == nil || !strings.Contains(err.Error(), "workspace app") {
		t.Fatalf("chat in docs filing in o/r = %v", err)
	}
}

// Each workspace has its own orchestrator: requests coalesce per
// workspace, one that's off queues nothing, and each runs on its own
// interval.
func TestOrchestratorRunsPerWorkspace(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setupScoped(t, "    orchestrator: {interval: 2h}\n  docs:\n    repos: [o/docs]\n    orchestrator: {enabled: false}\n")
	for _, r := range []struct{ ws, why string }{{"app", "a"}, {"docs", "b"}, {"app", "c"}} {
		if err := e.p.Orchestrate(ctx, r.ws, r.why, false); err != nil {
			t.Fatal(err)
		}
	}
	jobs, _ := e.p.Store.ListJobs(ctx, store.JobFilter{Kind: "orchestrate"})
	if len(jobs) != 1 {
		t.Fatalf("jobs = %d", len(jobs))
	}
	var in OrchestrateInput
	json.Unmarshal([]byte(jobs[0].Input), &in)
	if in.Workspace != "app" || strings.Join(in.Reasons, ",") != "a,c" {
		t.Fatalf("input = %+v", in)
	}
	if got := e.p.Cfg.OrchestratorFor("app").Interval; got != 2*time.Hour {
		t.Fatalf("app's interval = %v", got)
	}
	// An hour on, app's isn't due yet: nothing more is queued.
	e.p.orchestratorTriggers(ctx, time.Now().Add(time.Hour))
	if jobs, _ := e.p.Store.ListJobs(ctx, store.JobFilter{Kind: "orchestrate"}); len(jobs) != 1 {
		t.Fatalf("after an hour, jobs = %d", len(jobs))
	}
	if !e.p.Cfg.AnyOrchestrator() {
		t.Fatal("app's orchestrator is on")
	}
	e.p.Cfg.DisableOrchestrator()
	if e.p.Cfg.AnyOrchestrator() {
		t.Fatal("DisableOrchestrator turns every workspace's off")
	}
}

// A new chat needs a workspace once there are several; one already
// started keeps its own.
func TestChatNeedsAWorkspace(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setupScoped(t, "")
	e.p.openChat(ctx)
	if _, _, err := e.p.Chat(ctx, "", 0, "Hello"); err == nil || !strings.Contains(err.Error(), "say which workspace") {
		t.Fatalf("no workspace = %v", err)
	}
	if _, _, err := e.p.Chat(ctx, "nope", 0, "Hello"); err == nil || !strings.Contains(err.Error(), "no workspace") {
		t.Fatalf("unknown workspace = %v", err)
	}
	old, _ := e.p.Store.NewChat(ctx, "from before workspaces")
	if _, _, err := e.p.Chat(ctx, "docs", old, "Hello"); err == nil || !strings.Contains(err.Error(), "before workspaces") {
		t.Fatalf("an old chat with several workspaces = %v", err)
	}
}
