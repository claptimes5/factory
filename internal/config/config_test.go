package config

import (
	"gopkg.in/yaml.v3"
	"maps"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
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

// A reviewer may run on a builder's harness and model, escalation
// included: it reviews in a session of its own (see the pipeline's
// TestReviewNeverResumesTheBuildersSession), so it isn't refused.
func TestReviewerMayShareTheBuildersModel(t *testing.T) {
	b, _ := os.ReadFile("../../factory.yaml")
	claudeReview := func(model, tiers string) string {
		y := strings.Replace(string(b), "review:            {harness: codex,  model: gpt-6.1-sol}", "review: {harness: claude, model: "+model+"}", 1)
		y = strings.Replace(y, "review.high_risk:  {harness: codex,  model: gpt-6.1-sol, effort: high}", "", 1)
		y = strings.Replace(y, "security:          {harness: claude, model: opus}", "", 1)
		y = strings.Replace(y, "implement.L:       {harness: claude, model: opus}", "implement.L:       {harness: claude, model: sonnet}", 1)
		return strings.Replace(y, "implement: [haiku, sonnet, opus]", "implement: "+tiers, 1)
	}
	for name, tc := range map[string]struct{ model, tiers string }{
		"same model":                    {"sonnet", "[sonnet]"},
		"model a retry escalates to":    {"opus", "[sonnet, opus]"},
		"different model at every tier": {"opus", "[sonnet]"},
		"full id of a builder model":    {"claude-sonnet-4-5", "[sonnet]"},
	} {
		c, err := Parse([]byte(claudeReview(tc.model, tc.tiers)))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		// A verifier on a builder's model is allowed too.
		c.Routing["verify"] = c.Routing["implement.M"]
		if err := c.Validate(); err != nil {
			t.Errorf("%s: verify on a builder's model: %v", name, err)
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

func TestStepInstructions(t *testing.T) {
	c := load(t)
	dir := t.TempDir()
	c.SkillsDir = dir
	if err := os.MkdirAll(dir+"/go/tdd", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir+"/go/tdd/SKILL.md", []byte("---\nname: tdd\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir+"/style.md", []byte("Keep functions short.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := c.Routing["implement.M"]
	r.Skills, r.Use, r.Instructions, r.InstructionsFile = "go", []string{"tdd"}, "Prefer table tests.", dir+"/style.md"
	c.Routing["implement.M"] = r
	c.Repos[0].Instructions = map[string]string{"implement": "Run make generate after editing the schema."}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	got, err := c.StepInstructions("implement.M", "implement", "claptimes5/factory")
	want := "Use these skills from your skills directory, in this order: tdd.\n\nPrefer table tests.\n\nKeep functions short.\n\nRun make generate after editing the schema."
	if err != nil || got != want {
		t.Fatalf("got %q, %v", got, err)
	}
	if got, _ := c.StepInstructions("review", "review", "claptimes5/factory"); got != "" {
		t.Fatalf("review got %q", got)
	}

	for name, mutate := range map[string]func(c *Config){
		"missing skill": func(c *Config) { r := c.Routing["implement.M"]; r.Use = []string{"nope"}; c.Routing["implement.M"] = r },
		"use without set": func(c *Config) {
			r := c.Routing["implement.M"]
			r.Skills = ""
			c.Routing["implement.M"] = r
		},
		"missing file": func(c *Config) {
			r := c.Routing["implement.M"]
			r.InstructionsFile = dir + "/gone.md"
			c.Routing["implement.M"] = r
		},
		"unknown step": func(c *Config) { c.Repos[0].Instructions = map[string]string{"deploy": "x"} },
	} {
		bad := *c
		bad.Routing = maps.Clone(c.Routing)
		bad.Repos = slices.Clone(c.Repos)
		mutate(&bad)
		if err := bad.Validate(); err == nil {
			t.Errorf("%s: want a validation error", name)
		}
	}
}

func TestStepFilesResolveAgainstTheConfig(t *testing.T) {
	dir := t.TempDir()
	b, err := os.ReadFile("../../factory.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cfg := strings.Replace(string(b), "routing:\n", "routing:\n  critic: {harness: claude, model: opus, instructions_file: notes/critic.md}\n", 1)
	if err := os.MkdirAll(dir+"/notes", 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"factory.yaml", "notes/critic.md"} {
		body := "Check the money paths.\n"
		if f == "factory.yaml" {
			body = cfg
		}
		if err := os.WriteFile(dir+"/"+f, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Run from elsewhere: the file is next to the config, not here.
	c, err := Load(dir + "/factory.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := c.StepInstructions("critic", "critic"); err != nil || got != "Check the money paths." {
		t.Fatalf("got %q, %v", got, err)
	}
	// A directory passes a stat but can't be read.
	if err := os.WriteFile(dir+"/factory.yaml", []byte(strings.Replace(cfg, "notes/critic.md", "notes", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir + "/factory.yaml"); err == nil || !strings.Contains(err.Error(), "instructions_file") {
		t.Fatalf("a directory as instructions_file: %v", err)
	}
}

func TestRepoWithOnlyInstructionsKeepsThem(t *testing.T) {
	out, err := yaml.Marshal([]Repo{{Name: "a/b", Instructions: map[string]string{"review": "x"}}, {Name: "c/d"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "review: x") || !strings.Contains(string(out), "- c/d") {
		t.Fatalf("marshaled:\n%s", out)
	}
}

func TestWorkspaceInstructionsAreLabeled(t *testing.T) {
	c := load(t)
	c.Repos = []Repo{{Name: "o/api", Instructions: map[string]string{"implement": "Regenerate the client."}}, {Name: "o/web"}, {Name: "o/docs", Instructions: map[string]string{"implement": "Update the changelog."}}}
	got, _ := c.StepInstructions("implement.M", "implement", "o/api", "o/web", "o/docs")
	if want := "In o/api:\n\nRegenerate the client.\n\nIn o/docs:\n\nUpdate the changelog."; got != want {
		t.Fatalf("got %q", got)
	}
}

func TestAutoMergeLevels(t *testing.T) {
	var off *AutoMerge
	low, med := &AutoMerge{UpTo: "low"}, &AutoMerge{UpTo: "medium", MaxSize: "M", Method: "squash"}
	for _, c := range []struct {
		a          *AutoMerge
		risk, size string
		want       bool
	}{
		{off, "low", "XS", false},
		{low, "low", "XS", true},
		{low, "low", "S", true},
		{low, "low", "M", false}, // max_size defaults to S
		{low, "medium", "S", false},
		{med, "medium", "M", true},
		{med, "high", "S", false},
		{med, "", "S", false}, // no rating, no merge
		{&AutoMerge{UpTo: "none"}, "low", "XS", false},
	} {
		if got := c.a.Allows(c.risk, c.size); got != c.want {
			t.Errorf("%+v allows %s %s = %v", c.a, c.risk, c.size, got)
		}
	}
	if off.MergeMethod() != "merge" || med.MergeMethod() != "squash" {
		t.Fatal("merge method")
	}
	c := load(t)
	c.Repos[0].AutoMerge = &AutoMerge{UpTo: "high"}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "always wait for you") {
		t.Fatalf("up_to high: %v", err)
	}
	c.Repos[0].AutoMerge = &AutoMerge{UpTo: "low", Method: "fast-forward"}
	if err := c.Validate(); err == nil {
		t.Fatal("unknown method accepted")
	}
	c.Repos[0].AutoMerge, c.AutoMerge = nil, low
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if r, _ := c.RepoConf(c.Repos[0].Name); r.AutoMerge != low {
		t.Fatal("repo should inherit the top-level auto_merge")
	}
}

func TestAutoMergeBreaker(t *testing.T) {
	var none *AutoMerge
	if none.BreakerThreshold() != 2 || none.BreakerWindow() != 7*24*time.Hour || none.BreakerCoolOff() != 0 || none.On() {
		t.Fatal("defaults")
	}
	never := &AutoMerge{UpTo: "low", Breaker: &Breaker{Threshold: -1, Window: Duration{72 * time.Hour}, CoolOff: Duration{24 * time.Hour}}}
	if never.BreakerThreshold() != 0 || never.BreakerWindow() != 72*time.Hour || never.BreakerCoolOff() != 24*time.Hour || !never.On() {
		t.Fatalf("set: %+v", never.Breaker)
	}
	b, _ := os.ReadFile("../../factory.yaml")
	c, err := ParseIn([]byte(strings.Replace(string(b), "\nrouting:", "\nauto_merge: {up_to: low, breaker: {threshold: 3, window: 48h}}\nrouting:", 1)), "../..")
	if err != nil || c.AutoMerge.BreakerThreshold() != 3 || c.AutoMerge.BreakerWindow() != 48*time.Hour {
		t.Fatalf("parse: %v", err)
	}
	for _, b := range []*Breaker{{Threshold: -2}, {CoolOff: Duration{time.Minute}}, {Window: Duration{-time.Hour}}} {
		c := load(t)
		c.AutoMerge = &AutoMerge{UpTo: "low", Breaker: b}
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "breaker") {
			t.Errorf("%+v: %v", b, err)
		}
	}
}

func TestFastMode(t *testing.T) {
	c := load(t)
	r := c.Routing["implement.L"] // opus
	r.Fast = true
	c.Routing["implement.L"] = r
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if !r.FastFor("claude", "opus") || r.FastFor("claude", "sonnet") || r.FastFor("codex", "opus") {
		t.Fatal("FastFor")
	}
	for m, want := range map[string]bool{"opus": true, "claude-opus-4-8": true, "claude-opus-5": true, "claude-opus-5-5": true, "claude-opus-4-1": false, "claude-opus-4-7": false, "sonnet": false} {
		if IsOpus(m) != want {
			t.Errorf("IsOpus(%s) = %v", m, !want)
		}
	}
	if c.FastBudget(15, true) != 30 || c.FastBudget(15, false) != 15 {
		t.Fatal("fast budget")
	}
	s := c.Routing["implement.S"] // haiku
	s.Fast = true
	c.Routing["implement.S"] = s
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "Opus 4.8 or Opus 5") {
		t.Fatalf("fast on haiku: %v", err)
	}
	s.Fast = false
	c.Routing["implement.S"] = s
	rv := c.Routing["review"]
	rv.Fast = true
	c.Routing["review"] = rv
	if err := c.Validate(); err == nil {
		t.Fatal("fast on a codex reviewer accepted")
	}
}

func TestTuning(t *testing.T) {
	c := load(t)
	if tu := c.Tuning; tu.Every != 720*time.Hour || tu.MinItems != 10 || tu.Budget != c.Orchestrator.RunBudget || tu.Path != "factory.yaml" || tu.Repo != "" {
		t.Fatalf("defaults = %+v", tu)
	}
	if r := c.Routing["tune"]; !reflect.DeepEqual(r, c.Routing["orchestrate"]) {
		t.Fatalf("routing.tune = %+v, want the orchestrate route", r)
	}
	b, _ := os.ReadFile("../../factory.yaml")
	with := func(block string) (*Config, error) { return Parse([]byte(string(b) + "\ntuning: " + block + "\n")) }
	for block, every := range map[string]time.Duration{"{every: 0}": 0, "{every: 0s}": 0, "{every: none}": 0, "{every: 168h}": 168 * time.Hour, "{every: 1h}": time.Hour} {
		c, err := with(block)
		if err != nil || c.Tuning.Every != every {
			t.Errorf("%s: every = %v, %v", block, c.Tuning.Every, err)
		}
	}
	c, err := with("{repo: claptimes5/factory, path: deploy/factory.yaml, min_items: 3, budget: 1.5}")
	if err != nil || c.Tuning.Repo != "claptimes5/factory" || c.Tuning.MinItems != 3 || c.Tuning.Budget != 1.5 {
		t.Fatalf("tuning = %+v, %v", c.Tuning, err)
	}
	for block, want := range map[string]string{
		"{every: monthly}":    "tuning.every must be a duration",
		"{every: -1h}":        "tuning.every must be a duration",
		"{min_items: -1}":     "tuning.min_items",
		"{budget: -2}":        "tuning.budget",
		"{repo: o/elsewhere}": "tuning.repo o/elsewhere isn't in repos",
		"{path: ../x.yaml}":   "tuning.path",
	} {
		if _, err := with(block); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", block, err, want)
		}
	}
}

func TestUnifiedDiff(t *testing.T) {
	a := []byte("a\nb\nc\nd\ne\nf\ng\nh\ni\nj\nk\n")
	b := []byte("a\nB\nc\nd\ne\nf\ng\nh\ni\nj\nk\nl\n")
	want := "--- a/f.yaml\n+++ b/f.yaml\n@@ -1,5 +1,5 @@\n a\n-b\n+B\n c\n d\n e\n@@ -9,3 +9,4 @@\n i\n j\n k\n+l\n"
	if got := UnifiedDiff("f.yaml", a, b); got != want {
		t.Fatalf("diff:\n%s\nwant:\n%s", got, want)
	}
	if UnifiedDiff("f", a, a) != "" {
		t.Fatal("no change, no diff")
	}
}

func TestTuningEverySetting(t *testing.T) {
	groups, err := Settings([]byte("repo: o/r\n"))
	if err != nil {
		t.Fatal(err)
	}
	s, ok := FindSetting(groups, []string{"tuning", "every"})
	if !ok {
		t.Fatal("no tuning.every setting")
	}
	for in, want := range map[string]string{"0": "0", "0s": "0s", "none": "none", "1h": "1h", "720h": "720h"} {
		if v, err := s.Coerce(in); err != nil || v != want {
			t.Errorf("%s: %v, %v", in, v, err)
		}
	}
	for _, in := range []string{"30m", "10s"} {
		if _, err := s.Coerce(in); err == nil || !strings.Contains(err.Error(), "at least 1h") {
			t.Errorf("%s: %v", in, err)
		}
	}
	if s, _ := FindSetting(groups, []string{"orchestrator", "interval"}); s.Path == nil {
		t.Fatal("no orchestrator.interval")
	} else if _, err := s.Coerce("0"); err == nil {
		t.Error("an interval of 0 isn't off")
	}
}

func TestTunable(t *testing.T) {
	for path, want := range map[string]bool{
		"budgets.daily_total": true, "routing.implement.S.model": true, "routing.review.effort": true, "escalation.implement": true,
		"tuning.every": false, "auto_merge.up_to": false, "orchestrator.enabled": false, "orchestrator.run_budget": true,
		"routing.tune.model": false, "routing.orchestrate.model": false, "": false,
	} {
		p := strings.Split(path, ".")
		if strings.HasPrefix(path, "routing.") {
			p = []string{"routing", strings.TrimSuffix(strings.TrimPrefix(path, "routing."), "."+p[len(p)-1]), p[len(p)-1]}
		}
		if path == "" {
			p = nil
		}
		if got := Tunable(p); got != want {
			t.Errorf("%s: %v", path, got)
		}
	}
}

func TestReviewSettings(t *testing.T) {
	c := load(t)
	if !c.Reviews.VerifyOn() || !c.Reviews.IncrementalOn() || c.Reviews.PairedHighRisk || c.Reviews.ContextBytes != 20000 || c.Timeout("verify") != 10*time.Minute {
		t.Fatalf("defaults %+v", c.Reviews)
	}
	if r := c.VerifyRoute(); r.Harness != "codex" || r.Model != "gpt-6-luna" || c.JobBudget("verify", "") != 0.5 {
		t.Fatalf("verify route %+v", r)
	}
	// Left out, it is triage's cheap tier, unless a builder runs on it:
	// here triage's haiku builds S items, so it is the reviewer's.
	delete(c.Routing, "verify")
	if r := c.VerifyRoute(); r.Harness != "codex" || r.Model != "gpt-6.1-sol" {
		t.Fatalf("default verify route %+v", r)
	}
	c.Routing["triage"] = Route{Harness: "codex", Model: "gpt-6-luna"}
	if r := c.VerifyRoute(); r.Model != "gpt-6-luna" {
		t.Fatalf("triage's tier %+v", r)
	}
	// Set, it is used even when a builder can run on it: each verifier
	// run is a fresh session.
	c.Routing["verify"] = Route{Harness: "claude", Model: "sonnet"}
	if r := c.VerifyRoute(); r.Model != "sonnet" || c.Validate() != nil {
		t.Fatalf("verify on a builder's model: %+v %v", r, c.Validate())
	}
	c.Routing["verify"] = Route{Harness: "codex", Model: "gpt-6-luna"}
	c.Budgets.Verify = 0
	if c.JobBudget("verify", "") != c.Budgets.Triage {
		t.Fatal("budgets.verify should default to budgets.triage")
	}
	if cr := c.CriticRoute(); cr.Model != "opus" || cr.Skills != "" {
		t.Fatalf("critic %+v", cr)
	}
}

func TestRiskSettings(t *testing.T) {
	c := load(t)
	r := c.RiskRules()
	if r.MediumLines != 150 || r.HighLines != 300 || r.MediumFiles != 10 || r.HighFiles != 25 || len(r.HighPaths) != 0 ||
		!slices.Equal(areaNames(r.Auth), authAreas) || !slices.Equal(areaNames(r.Migrations), []string{"migrations"}) || !slices.Contains(r.MediumPaths, "Dockerfile") ||
		!slices.Equal(r.NeedsPerson, []string{"migrations", "new_dependencies", "auth", "deleted_tests"}) {
		t.Fatalf("defaults %+v", r)
	}
	b, _ := os.ReadFile("../../factory.yaml")
	set := func(risk string) (*Config, error) {
		return ParseIn([]byte(strings.Replace(string(b), "\nrouting:", "\nrisk: "+risk+"\nrouting:", 1)), "../..")
	}
	c, err := set("{high_lines: -1, medium_files: 5, high_paths: [billing/], auth_paths: [login*], needs_person: []}")
	if err != nil {
		t.Fatal(err)
	}
	if r := c.RiskRules(); r.HighLines != 0 || r.MediumLines != 150 || r.MediumFiles != 5 || !slices.Equal(r.HighPaths, []string{"billing/"}) ||
		len(r.Auth) != 5 || r.Auth[4].Name != "auth" || !slices.Equal(r.Auth[4].Globs, []string{"login*"}) || r.NeedsPerson == nil || len(r.NeedsPerson) != 0 {
		t.Fatalf("set: %+v", r)
	}
	for _, bad := range []struct{ risk, err string }{
		{"{needs_person: [schema]}", `risk.needs_person: "schema" isn't a kind of change`},
		{"{auth_paths: ['[']}", "risk.auth_paths"},
		{"{medium_lines: 500}", "risk.medium_lines (500) is above risk.high_lines (300)"},
		{"{high_files: -2}", "risk.high_files must be a positive number"},
		{"{migration_paths: [/]}", `risk.migration_paths: glob "/": is empty`},
	} {
		if _, err := set(bad.risk); err == nil || !strings.Contains(err.Error(), bad.err) {
			t.Errorf("%s: %v", bad.risk, err)
		}
	}
}
