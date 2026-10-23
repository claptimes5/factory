package pipeline

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/claptimes5/factory/internal/config"
	"github.com/claptimes5/factory/internal/harness"
	"github.com/claptimes5/factory/internal/state"
	"github.com/claptimes5/factory/internal/store"
)

// singleVendorConfig is a GitLab workspace of a Rails API and a Vite app,
// every route on one vendor: the routes of docs/examples, with test gates.
func singleVendorConfig(vendor string) string {
	routing := `
routing:
  orchestrate:       {harness: claude, model: opus}
  triage:            {harness: claude, model: haiku}
  implement.XS:      {harness: claude, model: haiku}
  implement.S:       {harness: claude, model: haiku}
  implement.M:       {harness: claude, model: sonnet}
  implement.L:       {harness: claude, model: sonnet}
  review:            {harness: claude, model: opus}
  review.high_risk:  {harness: claude, model: opus, effort: high}
  verify:            {harness: claude, model: opus, effort: low}
escalation: {implement: [haiku, sonnet], max_attempts_per_tier: 2, max_review_rounds: 2}
`
	if vendor == "codex" {
		routing = `
routing:
  orchestrate:       {harness: codex, model: sol}
  triage:            {harness: codex, model: luna}
  implement.XS:      {harness: codex, model: sol, effort: low}
  implement.S:       {harness: codex, model: sol, effort: medium}
  implement.M:       {harness: codex, model: sol, effort: high}
  implement.L:       {harness: codex, model: sol, effort: high}
  review:            {harness: codex, model: luna, effort: medium}
  review.high_risk:  {harness: codex, model: luna, effort: high}
  verify:            {harness: codex, model: luna}
  security:          {harness: codex, model: sol, effort: high}
escalation: {implement: [sol], max_attempts_per_tier: 3, max_review_rounds: 2}
pricing:
  sol:  {input_per_mtok: 2, cached_input_per_mtok: 0.1, output_per_mtok: 10}
  luna: {input_per_mtok: 0.1, cached_input_per_mtok: 0.01, output_per_mtok: 0.5}
`
	}
	return `
repos:
  - name: gitlab.com/acme/api
    gates: {required: [test], commands: {test: test -f app/models/order.rb}, proves_fix: none, coverage: none}
  - name: gitlab.com/acme/web
    gates: {required: [build], commands: {build: test -f src/api.ts}, proves_fix: none, coverage: none}
workspaces:
  shop: [gitlab.com/acme/api, gitlab.com/acme/web]
base_branch: main
runner: {kind: local}
budgets: {per_job: {XS: 0.5, S: 2, M: 6, L: 15}, triage: 0.5, review: 3, daily_total: 60}
` + routing
}

// A workspace item on GitLab goes through triage, the build, the gates in
// each repo, a blocked review, the verifier, a fix round and a passing
// review to a merge request in each project, with every job on Claude
// alone or on Codex alone.
func TestSingleVendorGitLabWorkspace(t *testing.T) {
	for _, vendor := range []string{"claude", "codex"} {
		t.Run(vendor, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			e := setup(t)
			cfg, err := config.Parse([]byte(singleVendorConfig(vendor)))
			if err != nil {
				t.Fatal(err)
			}
			cfg.DataDir, cfg.PromptsDir, cfg.SkillsDir = e.p.Cfg.DataDir, e.p.Cfg.PromptsDir, e.p.Cfg.SkillsDir
			cfg.Budgets.MinFreeDiskGB = -1
			e.p.Cfg = cfg
			web := setup(t)
			clients := map[string]RepoClients{
				"gitlab.com/acme/api": {GH: e.gh, Git: e.p.Git},
				"gitlab.com/acme/web": {GH: web.gh, Git: web.p.Git},
			}
			e.p.ClientsFor = func(repo string) RepoClients { return clients[strings.ToLower(repo)] }
			e.addIssue(7)

			h, other := e.claude, e.codex
			if vendor == "codex" {
				h, other = e.codex, e.claude
			}
			h.triage = `{"decision":"ready","complexity":"M","risk":"low","question":"","summary":"orders API and its client"}`
			h.builds = []func(string) error{
				func(work string) error {
					api, app := filepath.Join(work, "api"), filepath.Join(work, "web")
					if err := writeFile(api, "app/models/order.rb", "class Order < ApplicationRecord\nend\n"); err != nil {
						return err
					}
					if err := commitIn(api, "order"); err != nil {
						return err
					}
					if err := writeFile(app, "src/api.ts", "export const orders = () => fetch('/orders')\n"); err != nil {
						return err
					}
					return commitIn(app, "client")
				},
				webBuild("checked\n"),
			}
			h.reviews = []string{block, resolveF1}
			h.verifies = []string{confirmF1}

			if err := e.p.Drain(ctx); err != nil {
				t.Fatal(err)
			}
			it, err := e.p.Store.ItemByIssue(ctx, "gitlab.com/acme/api", 7)
			if err != nil {
				t.Fatal(err)
			}
			if it.Stage != state.AwaitingHuman {
				evs, _ := e.p.Store.ListJobs(ctx, store.JobFilter{ItemID: it.ID})
				for _, j := range evs {
					t.Logf("job %d %s %s/%s %s: %s", j.ID, j.Kind, j.Harness, j.Model, j.State, j.Error)
				}
				t.Fatalf("stage = %s", it.Stage)
			}
			if other.calls != nil {
				t.Errorf("the other vendor ran: %v", other.calls)
			}
			jobs, _ := e.p.Store.ListJobs(ctx, store.JobFilter{ItemID: it.ID})
			kinds := map[string]bool{}
			for _, j := range jobs {
				kinds[j.Kind] = true
				if j.Harness != vendor {
					t.Errorf("job %d (%s) ran on %s/%s", j.ID, j.Kind, j.Harness, j.Model)
				}
			}
			for _, k := range []string{"triage", "implement", "review"} {
				if !kinds[k] {
					t.Errorf("no %s job: %v", k, kinds)
				}
			}
			if h.calls["verify"] != 1 {
				t.Errorf("verify ran %d times", h.calls["verify"])
			}

			// A merge request in each project, linked by GitLab references,
			// and only the last to merge closes the issue.
			branch := "factory/api-7"
			api, app := e.gh.prs[branch], web.gh.prs[branch]
			if api == nil || app == nil {
				t.Fatalf("merge requests: api %v web %v", api, app)
			}
			if b := e.gh.bodies[api.Number]; !strings.HasPrefix(b, "Part of acme/api#7\n") || !strings.Contains(b, "1. [acme/api!101](https://example/pr) (this PR)\n2. [acme/web!101]") {
				t.Errorf("API merge request:\n%s", b)
			}
			if b := web.gh.bodies[app.Number]; !strings.HasPrefix(b, "Closes acme/api#7\n") {
				t.Errorf("web merge request:\n%s", b)
			}
		})
	}
}

// The orchestrator runs on Codex too, with the daemon's MCP tools.
func TestOrchestratorOnCodex(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	orchestratorOn(e)
	e.p.Cfg.Routing["orchestrate"] = config.Route{Harness: "codex", Model: "gpt-test"}
	called := false
	e.codex.orch = func(req harness.RunRequest) (string, error) {
		m := dialMCP(t, req)
		defer m.conn.Close()
		if out, _ := m.call("queue_status", map[string]any{}); out == "" {
			t.Error("queue_status said nothing")
		}
		called = true
		return `{"summary":"all quiet"}`, nil
	}
	if err := e.p.Orchestrate(ctx, "r", "test", false); err != nil {
		t.Fatal(err)
	}
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	jobs, _ := e.p.Store.ListJobs(ctx, store.JobFilter{Kind: "orchestrate"})
	if !called || len(jobs) != 1 || jobs[0].State != state.Succeeded || jobs[0].Harness != "codex" {
		t.Fatalf("called %v, jobs %+v", called, jobs)
	}
}
