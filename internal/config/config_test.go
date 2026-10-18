package config

import (
	"gopkg.in/yaml.v3"
	"os"
	"regexp"
	"strings"
	"testing"
)

func load(t *testing.T) *Config {
	t.Helper()
	c, err := Load("../../factory.yaml")
	if err != nil {
		t.Fatalf("repo factory.yaml must load: %v", err)
	}
	return c
}

func TestRepoConfigLoads(t *testing.T) {
	c := load(t)
	if names := c.RepoNames(); len(names) != 1 || names[0] != "claptimes5/factory" || c.Budgets.ConcurrentJobs != 3 {
		t.Fatalf("unexpected: %+v", c)
	}
	if r, ok := c.RepoConf("ClapTimes5/Factory"); !ok || r.BaseBranch != "main" || r.Gates == nil || len(r.Gates.Required) == 0 {
		t.Fatalf("repo settings: %+v %v", r, ok)
	}
}

func TestSeveralRepos(t *testing.T) {
	b, _ := os.ReadFile("../../factory.yaml")
	y := regexp.MustCompile(`(?m)^repo: .*$`).ReplaceAllString(string(b), `repos:
  - o/api
  - name: o/web
    base_branch: develop
    gates:
      required: [test]
      commands: {test: npm test}`)
	c, err := Parse([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	api, _ := c.RepoConf("o/api")
	web, _ := c.RepoConf("o/web")
	if api.BaseBranch != "main" || len(api.Gates.Required) < 2 {
		t.Fatalf("o/api should take the defaults: %+v", api)
	}
	if web.BaseBranch != "develop" || web.Gates.Commands["test"] != "npm test" || len(web.Gates.Required) != 1 {
		t.Fatalf("o/web: %+v", web.Gates)
	}
	// What o/web leaves out is inherited: above all the diff guard.
	if web.Gates.DiffGuard.MaxChangedLines != c.Gates.DiffGuard.MaxChangedLines || len(web.Gates.DiffGuard.ProtectedPaths) == 0 ||
		web.Gates.DiffGuard.MaxChangedLines == 0 || web.Gates.ProvesFix != c.Gates.ProvesFix {
		t.Fatalf("o/web should inherit the diff guard and proves_fix: %+v", web.Gates)
	}
	for name, bad := range map[string]string{
		"both forms":     "repo: o/x\n" + y,
		"duplicate":      strings.Replace(y, "  - name: o/web", "  - O/API\n  - name: o/web", 1),
		"not owner/name": strings.Replace(y, "  - o/api", "  - api", 1),
	} {
		if _, err := Parse([]byte(bad)); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func TestReviewerMustDifferFromBuilder(t *testing.T) {
	b, _ := os.ReadFile("../../factory.yaml")
	claudeReview := func(model, tiers string) string {
		y := strings.Replace(string(b), "review:            {harness: codex,  model: gpt-5-codex}", "review: {harness: claude, model: "+model+"}", 1)
		y = strings.Replace(y, "review.high_risk:  {harness: codex,  model: gpt-5-codex, effort: high}", "", 1)
		y = strings.Replace(y, "implement.L:       {harness: claude, model: opus}", "implement.L:       {harness: claude, model: sonnet}", 1)
		return strings.Replace(y, "implement: [sonnet, opus]", "implement: "+tiers, 1)
	}
	cases := []struct {
		name, model, tiers string
		ok                 bool
	}{
		{"same model", "sonnet", "[sonnet]", false},
		{"model a retry escalates to", "opus", "[sonnet, opus]", false},
		{"different model at every tier", "opus", "[sonnet]", true},
		{"names compare without case", "Sonnet", "[sonnet]", false},
		{"full id of a builder model", "claude-sonnet-4-5", "[sonnet]", false},
	}
	for _, tc := range cases {
		_, err := Parse([]byte(claudeReview(tc.model, tc.tiers)))
		if tc.ok && err != nil {
			t.Errorf("%s: want ok, got %v", tc.name, err)
		}
		if !tc.ok && (err == nil || !strings.Contains(err.Error(), "differ from the builder")) {
			t.Errorf("%s: want builder/reviewer clash, got %v", tc.name, err)
		}
	}
}

func TestRejectsUnknownFields(t *testing.T) {
	b, _ := os.ReadFile("../../factory.yaml")
	if _, err := Parse(append(b, []byte("\nbogus: 1\n")...)); err == nil {
		t.Fatal("unknown top-level field should be rejected")
	}
}

func TestEscalation(t *testing.T) {
	c := load(t) // implement: [sonnet, opus], 2 per tier
	cases := []struct {
		size    string
		attempt int
		model   string
		ok      bool
	}{
		{"S", 1, "sonnet", true},
		{"S", 2, "sonnet", true},
		{"S", 3, "opus", true},
		{"S", 4, "opus", true},
		{"S", 5, "", false},
		{"L", 1, "opus", true},
		{"L", 2, "opus", true},
		{"L", 3, "", false},
	}
	for _, tc := range cases {
		r, ok := c.Implement(tc.size, tc.attempt)
		if ok != tc.ok || (ok && r.Model != tc.model) {
			t.Errorf("Implement(%s,%d) = %s,%v want %s,%v", tc.size, tc.attempt, r.Model, ok, tc.model, tc.ok)
		}
	}
}

func TestReviewRouteByRisk(t *testing.T) {
	c := load(t)
	if c.Review("high").Effort != "high" || c.Review("low").Effort != "" {
		t.Fatal("high risk should use review.high_risk")
	}
}

func TestGatesRoundTrip(t *testing.T) {
	for _, req := range [][]string{nil, {}, {"test"}} {
		b, err := yaml.Marshal(Repo{Name: "o/r", Gates: &Gates{Install: "npm ci", Required: req, Commands: map[string]string{"test": "npm test"}}})
		if err != nil {
			t.Fatal(err)
		}
		var r Repo
		if err := yaml.Unmarshal(b, &r); err != nil {
			t.Fatal(err)
		}
		if (r.Gates.Required == nil) != (req == nil) || len(r.Gates.Required) != len(req) || r.Gates.Install != "npm ci" || r.Gates.Commands["test"] != "npm test" {
			t.Errorf("%v round-tripped as %+v:\n%s", req, r.Gates, b)
		}
	}
}

func TestRepoImagesAndEnvAreChecked(t *testing.T) {
	b, _ := os.ReadFile("../../factory.yaml")
	base := regexp.MustCompile(`(?m)^repo: .*\n`).ReplaceAllString(string(b), "")
	for name, c := range map[string]struct{ repos, err string }{
		"two bases":   {"repos:\n  - {name: o/a, image: shared, image_from: ruby:3.4-bookworm}\n  - {name: o/b, image: shared, image_from: python:3.13-bookworm}\n", "different bases"},
		"default too": {"repos:\n  - o/a\n  - {name: o/b, image: factory-job:latest, image_from: ruby:3.4-bookworm}\n", "different bases"},
		"own image":   {"repos:\n  - {name: o/a, image: mine}\n  - {name: o/b, image: mine}\n", ""},
		"home":        {"repos:\n  - {name: o/a, env: {HOME: /x}}\n", "can't set HOME"},
		"docker host": {"env: {DOCKER_HOST: tcp://x}\nrepos: [o/a]\n", "can't set DOCKER_HOST"},
		"fine":        {"env: {RAILS_ENV: test}\nrepos: [o/a]\n", ""},
	} {
		_, err := Parse([]byte(c.repos + base))
		if (c.err == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), c.err)) {
			t.Errorf("%s: err = %v, want %q", name, err, c.err)
		}
	}
}
