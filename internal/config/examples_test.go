package config

import (
	"strings"
	"testing"
)

// The single-vendor examples in docs/examples load, and every step,
// including the ones whose routes default from others, runs on the one
// vendor. The reviewer may share a builder's model (it reviews in a fresh
// session); the security pass still never runs on a reviewer's.
func TestSingleVendorExamples(t *testing.T) {
	for file, vendor := range map[string]string{
		"gitlab-workspace-claude-only.yaml": "claude",
		"gitlab-workspace-codex-only.yaml":  "codex",
	} {
		t.Run(vendor, func(t *testing.T) {
			c, err := Load("../../docs/examples/" + file)
			if err != nil {
				t.Fatal(err)
			}
			for _, r := range c.Repos {
				if !r.GitLab() || r.Host != "gitlab.com" {
					t.Errorf("repo %s isn't on gitlab.com", r.Name)
				}
			}
			if _, m := c.Workspace("gitlab.com/acme/web"); len(m) != 2 || m[0] != "gitlab.com/acme/api" {
				t.Errorf("workspace = %v", m)
			}
			routes := map[string]Route{"verify (resolved)": c.VerifyRoute(), "critic (resolved)": c.CriticRoute(), "security (resolved)": c.SecurityRoute()}
			for k, r := range c.Routing {
				routes[k] = r
			}
			for _, k := range []string{"orchestrate", "tune", "learn", "groom", "hunt"} {
				if _, ok := c.Routing[k]; !ok {
					t.Errorf("routing.%s has no default", k)
				}
			}
			for k, r := range routes {
				if r.Harness != vendor {
					t.Errorf("%s runs on %s/%s", k, r.Harness, r.Model)
				}
			}
			if s := c.SecurityRoute(); sameAsAny(s, c.reviewers()) {
				t.Errorf("security runs on a reviewer's model, %s", s.Model)
			}
		})
	}
}

// The Claude-only recipe in docs/configuration.md still loads with every
// route the factory has gained since: the security pass, verifier, plan
// check, hunt and the rest default to routes it allows.
func TestClaudeOnlyRecipe(t *testing.T) {
	c, err := Parse([]byte(`
repo: o/r
runner: {kind: local}
routing:
  triage:            {harness: claude, model: haiku}
  implement.S:       {harness: claude, model: haiku}
  implement.M:       {harness: claude, model: sonnet}
  implement.L:       {harness: claude, model: sonnet}
  review:            {harness: claude, model: opus}
  review.high_risk:  {harness: claude, model: opus, effort: high}
  review.medium_risk: {harness: claude, model: opus}
escalation: {implement: [haiku, sonnet]}
budgets: {per_job: {S: 2, M: 6, L: 15}, triage: 0.5, review: 3, daily_total: 60}
hunt: {enabled: true}
plan: {medium: critic}
`))
	if err != nil {
		t.Fatal(err)
	}
	if v := c.VerifyRoute(); v.Model != "opus" {
		t.Errorf("verify = %+v, want the reviewer's model (triage's builds)", v)
	}
	if s := c.SecurityRoute(Route{Harness: "claude", Model: "sonnet"}); s.Model != "haiku" {
		t.Errorf("security on a sonnet build = %+v, want the tier that didn't build it", s)
	}
	if o := c.Routing["orchestrate"]; o.Harness != "claude" {
		t.Errorf("orchestrate = %+v", o)
	}
}

// With no route on Claude, the orchestrator's and tuning's routes default
// to the strongest builder's, not to a Claude sign-in nobody has.
func TestOrchestrateDefaultsToTheOneVendor(t *testing.T) {
	c, err := Parse([]byte(`
repo: o/r
runner: {kind: local}
routing:
  triage:      {harness: codex, model: luna}
  implement.S: {harness: codex, model: sol}
  implement.M: {harness: codex, model: sol}
  implement.L: {harness: codex, model: sol, effort: high}
  review:      {harness: codex, model: luna, effort: high}
escalation: {implement: [sol]}
budgets: {per_job: {S: 2, M: 6, L: 15}, triage: 0.5, review: 3, daily_total: 60}
pricing: {sol: {input_per_mtok: 1, output_per_mtok: 1}, luna: {input_per_mtok: 1, output_per_mtok: 1}}
`))
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"orchestrate", "tune", "learn", "groom", "hunt"} {
		if r := c.Routing[k]; r.Harness != "codex" {
			t.Errorf("routing.%s = %+v", k, r)
		}
	}
	if o := c.Routing["orchestrate"]; o.Model != "sol" {
		t.Errorf("orchestrate = %+v, want implement.L's model", o)
	}
	// An orchestrator on Codex is allowed: it gets the MCP tools too.
	if _, err := Parse([]byte(`
repo: o/r
runner: {kind: local}
routing:
  orchestrate: {harness: codex, model: sol}
  triage:      {harness: claude, model: haiku}
  implement.S: {harness: claude, model: sonnet}
  implement.M: {harness: claude, model: sonnet}
  implement.L: {harness: claude, model: opus}
  review:      {harness: codex, model: luna}
escalation: {implement: [sonnet, opus]}
budgets: {per_job: {S: 2, M: 6, L: 15}, triage: 0.5, review: 3, daily_total: 60}
pricing: {sol: {input_per_mtok: 1, output_per_mtok: 1}, luna: {input_per_mtok: 1, output_per_mtok: 1}}
`)); err != nil {
		t.Errorf("codex orchestrator: %v", err)
	}
}

// A Codex builder can't escalate to the Claude tiers the default
// factory.yaml lists: the tiers are models of the route's own harness.
func TestCodexBuilderEscalatesOnCodex(t *testing.T) {
	_, err := Parse([]byte(`
repo: o/r
runner: {kind: local}
routing:
  triage:      {harness: codex, model: luna}
  implement.S: {harness: codex, model: sol}
  implement.M: {harness: codex, model: sol}
  implement.L: {harness: codex, model: sol}
  review:      {harness: codex, model: luna}
escalation: {implement: [haiku, sonnet, opus]}
budgets: {per_job: {S: 2, M: 6, L: 15}, triage: 0.5, review: 3, daily_total: 60}
pricing: {sol: {input_per_mtok: 1, output_per_mtok: 1}, luna: {input_per_mtok: 1, output_per_mtok: 1}}
`))
	if err == nil || !strings.Contains(err.Error(), `escalates to "sonnet"`) {
		t.Fatalf("err = %v", err)
	}
}

// A tier Implement never moves a route to needs no pricing: a codex
// implement.L whose model isn't a tier starts on the first and escalates
// only to the ones after it.
func TestCodexTiersAreTheOnesItReaches(t *testing.T) {
	cfg := func(tiers string) string {
		return `
repo: o/r
runner: {kind: local}
routing:
  triage:      {harness: claude, model: haiku}
  implement.S: {harness: claude, model: haiku}
  implement.M: {harness: claude, model: sonnet}
  implement.L: {harness: codex, model: sol}
  review:      {harness: codex, model: luna}
escalation: {implement: ` + tiers + `}
budgets: {per_job: {S: 2, M: 6, L: 15}, triage: 0.5, review: 3, daily_total: 60}
pricing: {sol: {input_per_mtok: 1, output_per_mtok: 1}, luna: {input_per_mtok: 1, output_per_mtok: 1}}
`
	}
	if _, err := Parse([]byte(cfg("[opus]"))); err != nil {
		t.Errorf("one tier, never reached: %v", err)
	}
	if _, err := Parse([]byte(cfg("[haiku, sonnet]"))); err == nil || !strings.Contains(err.Error(), `implement.L runs on codex and escalates to "sonnet"`) {
		t.Errorf("a Claude tier it reaches: %v", err)
	}
}
