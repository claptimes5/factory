package config

import (
	"gopkg.in/yaml.v3"
	"os"
	"regexp"
	"slices"
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
		y := strings.Replace(string(b), "review:            {harness: codex,  model: gpt-6.1-sol}", "review: {harness: claude, model: "+model+"}", 1)
		y = strings.Replace(y, "review.high_risk:  {harness: codex,  model: gpt-6.1-sol, effort: high}", "", 1)
		y = strings.Replace(y, "implement.L:       {harness: claude, model: opus}", "implement.L:       {harness: claude, model: sonnet}", 1)
		return strings.Replace(y, "implement: [haiku, sonnet, opus]", "implement: "+tiers, 1)
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
	c := load(t) // implement: [haiku, sonnet, opus], 2 per tier
	cases := []struct {
		size    string
		attempt int
		model   string
		ok      bool
	}{
		{"S", 1, "haiku", true},
		{"S", 2, "haiku", true},
		{"S", 3, "sonnet", true},
		{"S", 4, "sonnet", true},
		{"S", 5, "opus", true},
		{"S", 6, "opus", true},
		{"S", 7, "", false},
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
	if c.Review("high").Effort != "high" || c.Review("low").Effort != "" || c.Review("medium").Effort != "" {
		t.Fatal("high risk should use review.high_risk")
	}
	c.Routing["review.medium_risk"] = Route{Harness: "codex", Model: "m", Effort: "medium"}
	if c.Review("medium").Effort != "medium" || c.Review("low").Effort != "" {
		t.Fatal("medium risk should use review.medium_risk when set")
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

func TestGitLabRepos(t *testing.T) {
	b, _ := os.ReadFile("../../factory.yaml")
	base := regexp.MustCompile(`(?m)^repo: .*\n`).ReplaceAllString(string(b), "")
	c, err := Parse([]byte("repos:\n  - o/api\n  - https://gitlab.com/team/web.git\n  - {name: platform/infra/deploy, host: git.example.com}\n  - github.com/o/cli\n" + base))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(c.RepoNames(), ","); got != "o/api,gitlab.com/team/web,git.example.com/platform/infra/deploy,o/cli" {
		t.Fatalf("names %s", got)
	}
	web, _ := c.RepoConf("GitLab.com/team/web")
	if !web.GitLab() || web.Host != "gitlab.com" || web.Path() != "team/web" || web.CloneURL() != "https://gitlab.com/team/web.git" ||
		web.IssueURL(3) != "https://gitlab.com/team/web/-/issues/3" || web.PRURL(4) != "https://gitlab.com/team/web/-/merge_requests/4" {
		t.Fatalf("%+v", web)
	}
	api, _ := c.RepoConf("o/api")
	if api.GitLab() || api.Path() != "o/api" || api.PRURL(4) != "https://github.com/o/api/pull/4" {
		t.Fatalf("%+v", api)
	}
	deploy, _ := c.RepoConf("git.example.com/platform/infra/deploy")
	if out, _ := yaml.Marshal(deploy); strings.TrimSpace(string(out)) == "" || strings.Contains(string(out), "host:") {
		t.Fatalf("marshal: %s", out)
	}
	if pp := web.Gates.DiffGuard.ProtectedPaths; !slices.Contains(pp, ".gitlab-ci.yml") || !slices.Contains(pp, ".gitlab/") {
		t.Fatalf("protected: %v", pp)
	}
	// A dotted group under an explicit host, and a host with a port.
	for _, c := range [][4]string{
		{"git.example.com", "my.team/proj", "git.example.com", "git.example.com/my.team/proj"},
		{"https://gitlab.com", "gitlab.com/g/p", "gitlab.com", "gitlab.com/g/p"},
		{"gitlab.com/", "gitlab.com/g/p", "gitlab.com", "gitlab.com/g/p"},
		{"http://git.x.io:8443", "git.x.io:8443/g/p", "git.x.io:8443", "git.x.io:8443/g/p"},
		{"", "GitLab.com/g/p", "gitlab.com", "gitlab.com/g/p"},
	} {
		if h, n := SplitRepo(c[0], c[1]); h != c[2] || n != c[3] {
			t.Errorf("SplitRepo(%q, %q) = %s %s", c[0], c[1], h, n)
		}
	}
	c2, err := Parse([]byte("repos:\n  - {name: \"git.example.com:8443/g/p\", image_from: ruby:3.4-bookworm}\n" + base))
	if err != nil {
		t.Fatal(err)
	}
	if r := c2.Primary(); r.Image != "factory-job-git.example.com-8443__g__p:latest" {
		t.Fatalf("image %s", r.Image)
	}
	for _, bad := range []string{"gitlab.com/onlyone", "o/a/b"} {
		if _, err := Parse([]byte("repos: [" + bad + "]\n" + base)); err == nil {
			t.Errorf("%s: want an error", bad)
		}
	}
}

// Services at the top level apply to every repo that names none of its
// own; a repo's replace them. Names must be host names, each needs an
// image, and only the docker runner runs them.
func TestServices(t *testing.T) {
	b, _ := os.ReadFile("../../factory.yaml")
	base := regexp.MustCompile(`(?m)^repo: .*\n`).ReplaceAllString(string(b), "")
	c, err := Parse([]byte(`repos:
  - o/api
  - name: o/web
    services:
      cache: {image: "redis:7", ready: redis-cli ping}
services:
  postgres: {image: "postgres:16", env: {POSTGRES_PASSWORD: factory}, ready: pg_isready -U postgres}
` + base))
	if err != nil {
		t.Fatal(err)
	}
	api, _ := c.RepoConf("o/api")
	web, _ := c.RepoConf("o/web")
	if s := api.Services["postgres"]; len(api.Services) != 1 || s.Image != "postgres:16" || s.Env["POSTGRES_PASSWORD"] != "factory" || s.Ready != "pg_isready -U postgres" {
		t.Fatalf("api services = %+v", api.Services)
	}
	if len(web.Services) != 1 || web.Services["cache"].Image != "redis:7" {
		t.Fatalf("web services = %+v", web.Services)
	}
	for bad, want := range map[string]string{
		"services:\n  Bad_Name: {image: x}\n": "must be a host name",
		"services:\n  db: {ready: x}\n":       "services.db needs an image",
	} {
		if _, err := Parse([]byte("repo: o/api\n" + base + bad)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err = %v, want %q", bad, err, want)
		}
	}
	local := strings.Replace(base, "kind: docker", "kind: local", 1)
	if _, err := Parse([]byte("repo: o/api\n" + local + "services:\n  db: {image: x}\n")); err == nil || !strings.Contains(err.Error(), "need the docker runner") {
		t.Errorf("local runner: err = %v", err)
	}
	host := strings.Replace(base, `network: ""`, "network: host", 1)
	if _, err := Parse([]byte("repo: o/api\n" + host + "services:\n  db: {image: x}\n")); err == nil || !strings.Contains(err.Error(), "network: host") {
		t.Errorf("host network: err = %v", err)
	}
}

func TestWorkspaces(t *testing.T) {
	b, _ := os.ReadFile("../../factory.yaml")
	base := regexp.MustCompile(`(?m)^repo: .*\n`).ReplaceAllString(string(b), "")
	repos := "repos: [o/api, o/web, o/docs, p/web]\n"
	c, err := Parse([]byte(repos + "workspaces:\n  shop: [O/API, o/web]\n" + base))
	if err != nil {
		t.Fatal(err)
	}
	if name, m := c.Workspace("o/WEB"); name != "shop" || !slices.Equal(m, []string{"o/api", "o/web"}) {
		t.Fatalf("workspace = %s %v", name, m)
	}
	if name, m := c.Workspace("o/docs"); name != "" || m != nil {
		t.Fatalf("o/docs is on its own: %s %v", name, m)
	}
	if ShortName("group/sub/Web") != "web" {
		t.Fatal("ShortName")
	}
	for ws, want := range map[string]string{
		"Shop: [o/api, o/web]":                    "names are lowercase",
		"shop: [o/api]":                           "at least two repos",
		"shop: [o/api, o/nope]":                   "o/nope isn't in repos",
		"a: [o/api, o/web]\n  b: [o/api, o/docs]": "o/api is already in workspace a",
		"shop: [o/web, p/web]":                    "two repos are named web",
	} {
		if _, err := Parse([]byte(repos + "workspaces:\n  " + ws + "\n" + base)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err = %v, want %q", ws, err, want)
		}
	}
}

func TestCostOfPricesCachedInput(t *testing.T) {
	c := load(t)
	// gpt-6.1-sol: $2 input, $0.10 cached, $10 output per million.
	got := c.CostOf("gpt-6.1-sol", 1_000_000, 800_000, 100_000)
	if want := 0.4 + 0.08 + 1.0; got < want-1e-9 || got > want+1e-9 {
		t.Errorf("CostOf = %v, want %v", got, want)
	}
	// No cached price: cached input costs as much as other input.
	c.Pricing["plain"] = Price{InputPerMTok: 1, OutputPerMTok: 2}
	if got := c.CostOf("plain", 1_000_000, 500_000, 0); got != 1 {
		t.Errorf("CostOf without a cached price = %v, want 1", got)
	}
}
