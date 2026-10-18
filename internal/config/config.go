// Package config loads and validates factory.yaml.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/claptimes5/factory/internal/egress"
)

// Route says which harness and model run a stage.
type Route struct {
	Harness string `yaml:"harness"`
	Model   string `yaml:"model"`
	Effort  string `yaml:"effort"`
	Skills  string `yaml:"skills"` // skill set folder under skills/
	// Fast runs Claude in fast mode: quicker, at a higher price per token.
	// Only Opus models have it; on an escalation tier that isn't Opus the
	// job runs at normal speed.
	Fast bool `yaml:"fast,omitempty"`
	// Use names skills in that set the worker must use, in order.
	Use []string `yaml:"use,omitempty"`
	// Instructions, and the file InstructionsFile names (relative to the
	// config), are added to the step's prompt.
	Instructions     string `yaml:"instructions,omitempty"`
	InstructionsFile string `yaml:"instructions_file,omitempty"`
}

// AutoMerge says which of the factory's PRs merge without you.
type AutoMerge struct {
	// UpTo is the highest risk that merges on its own: low or medium, or
	// none. High-risk changes always wait for you.
	UpTo string `yaml:"up_to"`
	// MaxSize is the largest triage size that qualifies: XS, S (the
	// default), M or L.
	MaxSize string `yaml:"max_size,omitempty"`
	// Method is merge (the default), squash or rebase.
	Method string `yaml:"method,omitempty"`
}

// Allows says whether a change of this risk and size merges on its own.
func (a *AutoMerge) Allows(risk, size string) bool {
	if a == nil {
		return false
	}
	rank := func(list []string, v string) int { return slices.Index(list, v) }
	r, max := rank([]string{"low", "medium"}, risk), rank([]string{"low", "medium"}, a.UpTo)
	sizes := []string{"XS", "S", "M", "L"}
	ms := a.MaxSize
	if ms == "" {
		ms = "S"
	}
	return r >= 0 && max >= 0 && r <= max && rank(sizes, size) >= 0 && rank(sizes, size) <= rank(sizes, ms)
}

// MergeMethod is how an auto-merge merges.
func (a *AutoMerge) MergeMethod() string {
	if a == nil || a.Method == "" {
		return "merge"
	}
	return a.Method
}

// Gates are the checks a change must pass before review.
type Gates struct {
	// Install runs first, on a writable export with the network on, to
	// fetch dependencies (npm ci, bundle install). If it fails, nothing
	// else runs.
	Install string `yaml:"install,omitempty"`
	// Required nil inherits the top level's; empty means no checks.
	Required []string          `yaml:"required,omitempty"`
	Commands map[string]string `yaml:"commands,omitempty"`
	// ProvesFix runs the changed tests against the base code; it must
	// fail. {pkgs} is replaced by the packages holding changed tests.
	ProvesFix string `yaml:"proves_fix,omitempty"`
	// Coverage runs, with coverage, the tests of the packages whose code
	// the change touches, on the base and on the change; a package may
	// not end up less covered. {pkgs} is replaced by those packages. It
	// reads Go's "coverage: N% of statements" lines, or "name N%".
	Coverage string `yaml:"coverage,omitempty"`
	// CoverageSlack is how many points a package may drop (default 0).
	CoverageSlack float64 `yaml:"coverage_slack,omitempty"`
	// ScenariosDir holds the hidden end-to-end scenarios. It must be
	// outside the target repository so builders never see it.
	ScenariosDir string `yaml:"scenarios_dir,omitempty"`
	DiffGuard    struct {
		MaxChangedLines int      `yaml:"max_changed_lines,omitempty"`
		ProtectedPaths  []string `yaml:"protected_paths,omitempty"`
	} `yaml:"diff_guard,omitempty"`
}

// Repo is one repository the factory works on.
type Repo struct {
	// Name is owner/name on GitHub. On GitLab it starts with the host:
	// gitlab.com/group/project, or gitlab.example.com/group/sub/project.
	Name string `yaml:"name,omitempty"`
	// Host is the GitLab host, taken from Name; empty for GitHub. Writing
	// host: with a bare group/project name works too.
	Host       string `yaml:"host,omitempty"`
	BaseBranch string `yaml:"base_branch,omitempty"`
	// Image is the job image for this repo's agents and gates; default
	// runner.image. ImageFrom is the base `factory images` builds it on
	// (for example ruby:3.4-bookworm), which then names it
	// factory-job-<owner>__<name> unless Image says otherwise.
	Image     string `yaml:"image,omitempty"`
	ImageFrom string `yaml:"image_from,omitempty"`
	// Env is set in this repo's agent and gate containers, on top of the
	// top-level env. It is not for secrets: agents can read it.
	Env map[string]string `yaml:"env,omitempty"`
	// Services are containers this repo's builds and checks need, such as
	// a database; they replace the top-level services. See Service.
	Services map[string]Service `yaml:"services,omitempty"`
	// Gates overrides the top-level gates field by field: what it leaves
	// out (the diff guard's protected paths, say) is inherited, and its
	// commands replace the top level's. install, proves_fix, coverage or
	// scenarios_dir "none" turns one off.
	Gates *Gates `yaml:"gates,omitempty"`
	// AutoMerge replaces the top level's for this repo; up_to: none
	// turns it off.
	AutoMerge *AutoMerge `yaml:"auto_merge,omitempty"`
	// Instructions are added to this repo's prompts, by step: triage,
	// implement, review or critic.
	Instructions map[string]string `yaml:"instructions,omitempty"`
}

// MarshalYAML writes Required even when it is empty but not nil, since an
// empty list (no checks) and a missing one (inherit) differ.
func (g Gates) MarshalYAML() (any, error) {
	out := struct {
		Install       string            `yaml:"install,omitempty"`
		Required      *[]string         `yaml:"required,omitempty"`
		Commands      map[string]string `yaml:"commands,omitempty"`
		ProvesFix     string            `yaml:"proves_fix,omitempty"`
		Coverage      string            `yaml:"coverage,omitempty"`
		CoverageSlack float64           `yaml:"coverage_slack,omitempty"`
		ScenariosDir  string            `yaml:"scenarios_dir,omitempty"`
		DiffGuard     any               `yaml:"diff_guard,omitempty"`
	}{Install: g.Install, Commands: g.Commands, ProvesFix: g.ProvesFix, Coverage: g.Coverage, CoverageSlack: g.CoverageSlack, ScenariosDir: g.ScenariosDir}
	if g.Required != nil {
		out.Required = &g.Required
	}
	if g.DiffGuard.MaxChangedLines != 0 || g.DiffGuard.ProtectedPaths != nil {
		out.DiffGuard = g.DiffGuard
	}
	return out, nil
}

// MarshalYAML writes a repo with no settings of its own as a bare name.
func (r Repo) MarshalYAML() (any, error) {
	if r.BaseBranch == "" && r.Image == "" && r.ImageFrom == "" && len(r.Env) == 0 && r.Gates == nil && len(r.Services) == 0 && len(r.Instructions) == 0 && r.AutoMerge == nil {
		return r.Name, nil
	}
	type plain Repo
	r.Host = "" // it's in the name
	return plain(r), nil
}

// UnmarshalYAML accepts a bare "owner/name" as well as a mapping.
func (r *Repo) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		r.Name = n.Value
		return nil
	}
	type plain Repo
	return n.Decode((*plain)(r))
}

// Service is a container started for each build and check of a repo,
// reachable by its name as a host name (postgres:5432), for example:
//
//	services:
//	  postgres:
//	    image: postgres:16
//	    env: {POSTGRES_PASSWORD: factory}
//	    ready: pg_isready -U postgres
//
// Its network has no way out, and it is removed when the job ends. Env is
// not for secrets: it is on the docker command line.
type Service struct {
	Image string            `yaml:"image"`
	Env   map[string]string `yaml:"env,omitempty"`
	// Ready runs in the service until it succeeds, before anything else
	// starts; leave it out for services ready as soon as they start.
	Ready string `yaml:"ready,omitempty"`
}

type Config struct {
	// SingleRepo is the short form for a factory with one repo; Repos is
	// filled from it. Use RepoNames and RepoConf rather than either field.
	SingleRepo string `yaml:"repo"`
	Repos      []Repo `yaml:"repos"`
	// Workspaces group repos that change together, like an API and the
	// web app that calls it: an issue in any member becomes one change
	// across all of them, with a PR in each repo it touches. Members are
	// listed in the order their PRs should merge.
	Workspaces map[string][]string `yaml:"workspaces,omitempty"`
	// AutoMerge lets the factory merge its own PRs up to a risk level and
	// size, once CI passes. Unset, every PR waits for you.
	AutoMerge *AutoMerge `yaml:"auto_merge,omitempty"`
	// BaseBranch, Env and Gates are the defaults for every repo.
	BaseBranch string             `yaml:"base_branch"`
	Env        map[string]string  `yaml:"env"`
	Services   map[string]Service `yaml:"services,omitempty"`
	DataDir    string             `yaml:"data_dir"`
	SkillsDir  string             `yaml:"skills_dir"`
	PromptsDir string             `yaml:"prompts_dir"`

	Intake struct {
		Label        string   `yaml:"label"`
		PollInterval Duration `yaml:"poll_interval"`
	} `yaml:"intake"`

	Runner struct {
		Kind    string `yaml:"kind"`
		Image   string `yaml:"image"`
		Network string `yaml:"network"`
		CPUs    string `yaml:"cpus"`
		Memory  string `yaml:"memory"`
		// WSLDrive is where WSL mounts the Windows drive that holds its
		// virtual disks (and Docker Desktop's): default /mnt/c.
		WSLDrive string `yaml:"wsl_drive"`
		// Egress is where jobs may connect. With the docker runner they
		// reach only allowlisted hosts, through the egress proxy.
		Egress struct {
			// Mode is allowlist (the default) or open: Docker's usual
			// network, the whole internet.
			Mode string `yaml:"mode,omitempty"`
			// Allow adds hosts ("registry.example.com", "*.example.com",
			// either with ":port") to the built-in list.
			Allow []string `yaml:"allow,omitempty"`
			// Private are hosts on a private network, such as a
			// self-hosted package mirror; the repos' own hosts are added.
			Private []string `yaml:"private,omitempty"`
			// NoDefaults drops the built-in list: only Allow, Private and
			// the repos' hosts are reachable.
			NoDefaults bool `yaml:"no_defaults,omitempty"`
		} `yaml:"egress"`
	} `yaml:"runner"`

	Web struct {
		// Listen is the control room address. Anywhere but localhost or a
		// Tailscale address needs FACTORY_WEB_PASSWORD.
		Listen string `yaml:"listen"`
		// Hosts are extra names the control room answers to (for example
		// a Tailscale name); anything else is refused.
		Hosts []string `yaml:"hosts"`
	} `yaml:"web"`

	Routing map[string]Route `yaml:"routing"`

	Escalation struct {
		Implement          []string `yaml:"implement"`
		MaxAttemptsPerTier int      `yaml:"max_attempts_per_tier"`
		MaxReviewRounds    int      `yaml:"max_review_rounds"`
	} `yaml:"escalation"`

	Timeouts map[string]Duration `yaml:"timeouts"`

	Orchestrator struct {
		On            bool     `yaml:"enabled"`         // off unless set: it spends on its own schedule
		RunBudget     float64  `yaml:"run_budget"`      // dollars per orchestrator run
		DailyBudget   float64  `yaml:"daily_budget"`    // dollars per day, separate from build spend
		DailyIssueCap int      `yaml:"daily_issue_cap"` // issues it may open per day
		Interval      Duration `yaml:"interval"`        // scheduled run period
		SummaryHour   int      `yaml:"summary_hour"`    // local hour of the daily summary
		NotifyPerHour int      `yaml:"notify_per_hour"`
		// Labels the orchestrator may put on issues it opens (besides the
		// intake label). factory:* labels are never allowed.
		Labels []string `yaml:"labels"`
	} `yaml:"orchestrator"`

	Budgets struct {
		PerJob           map[string]float64 `yaml:"per_job"`
		Triage           float64            `yaml:"triage"`
		Review           float64            `yaml:"review"`
		DailyTotal       float64            `yaml:"daily_total"`
		ConcurrentJobs   int                `yaml:"concurrent_jobs"`
		AwaitingHumanMax int                `yaml:"awaiting_human_max"`
		// MinFreeDiskGB is the free space no new job starts without, on
		// the data directory and wherever Docker stores containers;
		// default 10, negative to turn the check off.
		MinFreeDiskGB float64 `yaml:"min_free_disk_gb"`
		// FastMultiplier scales the dollar cap of a job in fast mode;
		// default 2.
		FastMultiplier float64 `yaml:"fast_multiplier,omitempty"`
		// PauseAtUsage holds a harness's jobs in the queue once a
		// subscription usage window it reports is this full, in percent,
		// until the window resets. Default 95; 100 waits for a hard limit.
		PauseAtUsage float64 `yaml:"pause_at_usage,omitempty"`
	} `yaml:"budgets"`

	Pricing map[string]Price `yaml:"pricing"`

	Gates Gates `yaml:"gates"`

	dir string
}

// Price is dollars per million tokens.
type Price struct {
	InputPerMTok  float64 `yaml:"input_per_mtok"`
	OutputPerMTok float64 `yaml:"output_per_mtok"`
	// CachedPerMTok prices input read from the prompt cache; 0 prices it
	// as other input.
	CachedPerMTok float64 `yaml:"cached_input_per_mtok"`
}

// Duration parses Go duration strings such as "10m".
type Duration struct{ time.Duration }

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	v, err := time.ParseDuration(n.Value)
	if err != nil {
		return fmt.Errorf("line %d: %w", n.Line, err)
	}
	d.Duration = v
	return nil
}

// Load reads and validates a config file. Relative paths in it resolve
// against the file's directory.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	abs, _ := filepath.Abs(filepath.Dir(path))
	c, err := ParseIn(b, abs)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// Parse decodes and validates config bytes, filling defaults. Relative
// paths resolve against the current directory.
func Parse(b []byte) (*Config, error) { return ParseIn(b, "") }

// ParseIn is Parse for a config that lives in dir: relative paths in it,
// including the files validation checks, resolve against dir.
func ParseIn(b []byte, dir string) (*Config, error) {
	c := Config{dir: dir}
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, err
	}
	c.defaults()
	return &c, c.Validate()
}

func (c *Config) defaults() {
	if c.BaseBranch == "" {
		c.BaseBranch = "main"
	}
	if c.DataDir == "" {
		c.DataDir = "./data"
	}
	if c.SkillsDir == "" {
		c.SkillsDir = "./skills"
	}
	if c.PromptsDir == "" {
		c.PromptsDir = "./prompts"
	}
	if c.Intake.Label == "" {
		c.Intake.Label = "factory:ready"
	}
	if c.Intake.PollInterval.Duration == 0 {
		c.Intake.PollInterval.Duration = time.Minute
	}
	if c.Web.Listen == "" {
		c.Web.Listen = "127.0.0.1:8080"
	}
	if c.Runner.Kind == "" {
		c.Runner.Kind = "docker"
	}
	if c.Escalation.MaxAttemptsPerTier == 0 {
		c.Escalation.MaxAttemptsPerTier = 2
	}
	if c.Escalation.MaxReviewRounds == 0 {
		c.Escalation.MaxReviewRounds = 2
	}
	if c.Timeouts == nil {
		c.Timeouts = map[string]Duration{}
	}
	for k, v := range map[string]time.Duration{"triage": 10 * time.Minute, "implement": 60 * time.Minute, "review": 20 * time.Minute, "gates": 20 * time.Minute, "orchestrate": 20 * time.Minute} {
		if c.Timeouts[k].Duration == 0 {
			c.Timeouts[k] = Duration{v}
		}
	}
	o := &c.Orchestrator
	if o.RunBudget == 0 {
		o.RunBudget = 3
	}
	if o.DailyBudget == 0 {
		o.DailyBudget = 15
	}
	if o.DailyIssueCap == 0 {
		o.DailyIssueCap = 10
	}
	if o.Interval.Duration == 0 {
		o.Interval.Duration = time.Hour
	}
	if o.SummaryHour == 0 {
		o.SummaryHour = 8
	}
	if o.NotifyPerHour == 0 {
		o.NotifyPerHour = 6
	}
	if _, ok := c.Routing["orchestrate"]; !ok && c.Routing != nil {
		c.Routing["orchestrate"] = Route{Harness: "claude", Model: "opus"}
	}
	if c.Budgets.ConcurrentJobs == 0 {
		c.Budgets.ConcurrentJobs = 3
	}
	if c.Budgets.MinFreeDiskGB == 0 {
		c.Budgets.MinFreeDiskGB = 10
	}
	if c.Runner.WSLDrive == "" {
		c.Runner.WSLDrive = "/mnt/c"
	}
	if c.Budgets.AwaitingHumanMax == 0 {
		c.Budgets.AwaitingHumanMax = 5
	}
	if c.Gates.DiffGuard.MaxChangedLines == 0 {
		c.Gates.DiffGuard.MaxChangedLines = 400
	}
	if c.SingleRepo != "" && len(c.Repos) == 0 {
		c.Repos = []Repo{{Name: c.SingleRepo}}
	}
	for i := range c.Repos {
		r := &c.Repos[i]
		r.Host, r.Name = SplitRepo(r.Host, r.Name)
		// Unset base_branch and gates follow the top level (see RepoConf).
	}
}

// SplitRepo reads a repo as written in the config or typed in setup
// ("owner/name", "github.com/owner/name", "https://gitlab.com/group/project.git",
// or a bare path with host set) into its GitLab host ("" for GitHub) and
// its name as the factory knows it: owner/name, or host/path on GitLab.
func SplitRepo(host, name string) (string, string) {
	name = strings.TrimSpace(name)
	for _, scheme := range []string{"https://", "http://"} {
		name = strings.TrimPrefix(name, scheme)
	}
	name = strings.TrimSuffix(strings.TrimSuffix(name, "/"), ".git")
	host = strings.ToLower(strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(host), "https://"), "http://"), "/"))
	if first, rest, ok := strings.Cut(name, "/"); ok && strings.Contains(first, ".") && (host == "" || strings.EqualFold(first, host)) {
		// A dotted first segment is a host, unless host: names another
		// one: then it's a group like my.team.
		host, name = strings.ToLower(first), rest
	}
	if host == "" || host == "github.com" || host == "www.github.com" {
		return "", name
	}
	return host, host + "/" + name
}

// EgressAllowlist reports whether jobs reach only allowlisted hosts.
func (c *Config) EgressAllowlist() bool {
	return c.Runner.Kind == "docker" && c.Runner.Egress.Mode != "open"
}

// EgressRules are the hosts jobs may reach: the built-in list unless
// dropped, plus runner.egress.allow; and the private ones, which include
// each repo's own git host (a self-hosted GitLab may be on the LAN).
func (c *Config) EgressRules() (allow, private []string) {
	if !c.Runner.Egress.NoDefaults {
		allow = append(allow, egress.Defaults...)
	}
	allow = append(allow, c.Runner.Egress.Allow...)
	private = append(private, c.Runner.Egress.Private...)
	seen := map[string]bool{}
	for _, r := range c.Repos {
		if r.Host != "" && !seen[r.Host] && r.Host != "gitlab.com" {
			seen[r.Host] = true
			private = append(private, r.Host)
		}
	}
	return allow, private
}

// GitLab reports whether the repo is on GitLab.
func (r Repo) GitLab() bool { return r.Host != "" }

// Path is the repo's path on its host: owner/name, or group/project.
func (r Repo) Path() string { return strings.TrimPrefix(r.Name, r.Host+"/") }

// WebURL is the repo's page.
func (r Repo) WebURL() string {
	if r.GitLab() {
		return "https://" + r.Name
	}
	return "https://github.com/" + r.Name
}

// CloneURL is the repo's HTTPS git remote.
func (r Repo) CloneURL() string { return r.WebURL() + ".git" }

// IssueURL is the page of issue n.
func (r Repo) IssueURL(n int) string {
	if r.GitLab() {
		return fmt.Sprintf("%s/-/issues/%d", r.WebURL(), n)
	}
	return fmt.Sprintf("%s/issues/%d", r.WebURL(), n)
}

// PRURL is the page of pull request (on GitLab, merge request) n.
func (r Repo) PRURL(n int) string {
	if r.GitLab() {
		return fmt.Sprintf("%s/-/merge_requests/%d", r.WebURL(), n)
	}
	return fmt.Sprintf("%s/pull/%d", r.WebURL(), n)
}

var reservedEnv = map[string]bool{"HOME": true, "PATH": true, "USER": true}

var harnesses = map[string]bool{"claude": true, "codex": true}

// Validate checks the rules in SPEC.md, including that a reviewer never runs
// the same harness and model as the builder it reviews.
func (c *Config) Validate() error {
	var errs []error
	bad := func(f string, a ...any) { errs = append(errs, fmt.Errorf(f, a...)) }
	if _, single := SplitRepo("", c.SingleRepo); c.SingleRepo != "" && (len(c.Repos) != 1 || c.Repos[0].Name != single) {
		bad("use repo: for one repository or repos: for several, not both")
	}
	if len(c.Repos) == 0 {
		bad("repos needs at least one owner/name")
	}
	checkAutoMerge := func(where string, a *AutoMerge) {
		if a == nil {
			return
		}
		switch {
		case a.UpTo == "high":
			bad("%sauto_merge.up_to: high-risk changes always wait for you; use low or medium", where)
		case !slices.Contains([]string{"none", "low", "medium"}, a.UpTo):
			bad("%sauto_merge.up_to must be none, low or medium", where)
		}
		if a.MaxSize != "" && !slices.Contains([]string{"XS", "S", "M", "L"}, a.MaxSize) {
			bad("%sauto_merge.max_size must be XS, S, M or L", where)
		}
		if a.Method != "" && !slices.Contains([]string{"merge", "squash", "rebase"}, a.Method) {
			bad("%sauto_merge.method must be merge, squash or rebase", where)
		}
	}
	checkAutoMerge("", c.AutoMerge)
	seen := map[string]bool{}
	for _, r := range c.Repos {
		checkAutoMerge("repo "+r.Name+": ", r.AutoMerge)
		switch o, n, ok := strings.Cut(r.Path(), "/"); {
		case r.GitLab() && (!ok || o == "" || n == "" || strings.Contains(r.Name, "//")):
			bad("repo %s must be %s/group/project", r.Name, r.Host)
		case !r.GitLab() && (!ok || o == "" || n == "" || strings.Contains(n, "/")):
			bad("repo %q must be owner/name", r.Name)
		}
		if seen[strings.ToLower(r.Name)] {
			bad("repo %s is listed twice", r.Name)
		}
		seen[strings.ToLower(r.Name)] = true
		for _, step := range sortedKeys(r.Instructions) {
			if !slices.Contains([]string{"triage", "implement", "review", "critic"}, step) {
				bad("repo %s: instructions.%s: the steps are triage, implement, review and critic", r.Name, step)
			}
		}
	}
	inWS := map[string]string{}
	for _, name := range sortedKeys(c.Workspaces) {
		members := c.Workspaces[name]
		if !wsName.MatchString(name) {
			bad("workspace %q: names are lowercase letters, digits and -", name)
		}
		if len(members) < 2 {
			bad("workspace %s needs at least two repos", name)
		}
		short := map[string]bool{}
		for _, m := range members {
			key := strings.ToLower(m)
			switch {
			case !seen[key]:
				bad("workspace %s: %s isn't in repos", name, m)
			case inWS[key] != "":
				bad("workspace %s: %s is already in workspace %s", name, m, inWS[key])
			}
			inWS[key] = name
			// Each member is checked out in a folder named for it.
			if sn := ShortName(m); short[sn] {
				bad("workspace %s: two repos are named %s; a workspace's repos need different names", name, sn)
			} else {
				short[sn] = true
			}
		}
	}
	for _, k := range []string{"triage", "implement.S", "implement.M", "implement.L", "review"} {
		if _, ok := c.Routing[k]; !ok {
			bad("routing.%s is required", k)
		}
	}
	for k, r := range c.Routing {
		if !harnesses[r.Harness] {
			bad("routing.%s: unknown harness %q", k, r.Harness)
		}
		if r.Model == "" {
			bad("routing.%s: model is required", k)
		}
		if r.Harness == "codex" {
			if _, ok := c.Pricing[r.Model]; !ok {
				bad("routing.%s: codex model %q needs a pricing entry (codex reports tokens, not dollars)", k, r.Model)
			}
		}
		if strings.ContainsAny(r.Skills, `/\.`) {
			bad("routing.%s: skills must be a folder name under skills_dir", k)
		}
		if r.Fast && (r.Harness != "claude" || !IsOpus(r.Model)) {
			bad("routing.%s: fast mode is Claude's, on Opus 4.8 or Opus 5 and later", k)
		}
		if len(r.Use) > 0 && r.Skills == "" {
			bad("routing.%s: use names skills, so it needs a skill set (skills:)", k)
		}
		for _, u := range r.Use {
			if u == "" || strings.ContainsAny(u, `/\`) || u == "." || u == ".." {
				bad("routing.%s: use %q must be a skill folder name", k, u)
			} else if r.Skills != "" && !strings.ContainsAny(r.Skills, `/\.`) {
				if _, err := os.Stat(filepath.Join(c.Path(c.SkillsDir), r.Skills, u, "SKILL.md")); err != nil {
					bad("routing.%s: use %q: no %s", k, u, filepath.Join(c.SkillsDir, r.Skills, u, "SKILL.md"))
				}
			}
		}
		if r.InstructionsFile != "" {
			// Read it now: a directory or unreadable file would otherwise
			// fail every job on this route.
			if _, err := os.ReadFile(c.Path(r.InstructionsFile)); err != nil {
				bad("routing.%s: instructions_file: %v", k, err)
			}
		}
	}
	for _, size := range []string{"XS", "S", "M", "L"} {
		b, ok := c.Routing["implement."+size]
		if !ok {
			continue
		}
		for _, rk := range []string{"review", "review.medium_risk", "review.high_risk"} {
			r, ok := c.Routing[rk]
			if !ok || r.Harness != b.Harness {
				continue
			}
			// Same harness is allowed only with a different model, at every
			// tier a retry can move the builder to.
			for _, m := range c.builderModels(b) {
				if sameModel(r.Harness, m, r.Model) {
					bad("routing.%s reviews with %s/%s, which routing.implement.%s can build with (escalation included): a reviewer must differ from the builder in harness or model", rk, r.Harness, r.Model, size)
					break
				}
			}
		}
		if v, ok := c.Budgets.PerJob[size]; (ok || size != "XS") && v <= 0 { // a missing XS falls back to S's
			bad("budgets.per_job.%s must be > 0", size)
		}
	}
	if r, ok := c.Routing["orchestrate"]; ok && r.Harness != "claude" {
		bad("routing.orchestrate must use the claude harness (it drives the daemon's MCP tools)")
	}
	if len(c.Escalation.Implement) == 0 {
		bad("escalation.implement needs at least one model tier")
	}
	if c.Budgets.PauseAtUsage < 0 || c.Budgets.PauseAtUsage > 100 {
		bad("budgets.pause_at_usage is a percentage up to 100, or 0 for the default")
	}
	if c.Budgets.DailyTotal <= 0 {
		bad("budgets.daily_total must be > 0")
	}
	if c.Budgets.Triage <= 0 || c.Budgets.Review <= 0 {
		bad("budgets.triage and budgets.review must be > 0")
	}
	switch c.Runner.Kind {
	case "docker":
		if c.Runner.Image == "" {
			bad("runner.image is required for the docker runner")
		}
	case "local":
	default:
		bad("runner.kind must be docker or local")
	}
	// One image tag built on two bases would give one repo the other's
	// toolchain.
	from := map[string]string{}
	for _, n := range c.RepoNames() {
		r, _ := c.RepoConf(n)
		base := r.ImageFrom
		if base == "" && r.Image != c.Runner.Image {
			continue // an image you build yourself
		}
		if prev, ok := from[r.Image]; ok && prev != base {
			bad("repos share the image %s but name different bases for it (%q and %q); give one its own image", r.Image, prev, base)
		}
		from[r.Image] = base
	}
	envs := []map[string]string{c.Env}
	for _, r := range c.Repos {
		envs = append(envs, r.Env)
	}
	for _, e := range envs {
		for k := range e {
			if reservedEnv[k] || strings.HasPrefix(k, "DOCKER_") {
				bad("env can't set %s: the factory sets it, or the docker command reads it", k)
			}
		}
	}
	svcs := []map[string]Service{c.Services}
	for _, r := range c.Repos {
		svcs = append(svcs, r.Services)
	}
	for _, m := range svcs {
		for name, s := range m {
			if !serviceName.MatchString(name) {
				bad("services: %q must be a host name: lowercase letters, digits and dashes", name)
			}
			if s.Image == "" {
				bad("services.%s needs an image", name)
			}
		}
	}
	if len(c.Services) > 0 || slices.ContainsFunc(c.Repos, func(r Repo) bool { return len(r.Services) > 0 }) {
		if c.Runner.Kind != "docker" {
			bad("services need the docker runner")
		}
		if c.Runner.Network == "host" {
			// On the host's network, a job reaches the host and not the
			// services' private network.
			bad("services can't be used with runner.network: host")
		}
	}
	switch c.Runner.Egress.Mode {
	case "", "allowlist":
		if c.Runner.Kind == "docker" && c.Runner.Network == "host" {
			bad("runner.network: host leaves the egress allowlist nothing to control; set runner.egress.mode: open to run that way")
		}
		allow, private := c.EgressRules()
		if c.Runner.Kind == "docker" && len(allow)+len(private) == 0 {
			bad("runner.egress: with no_defaults, list the hosts jobs may reach in allow")
		}
		for _, r := range append(allow, private...) {
			if _, err := egress.ParseRule(r); err != nil {
				bad("runner.egress: %v", err)
			}
		}
	case "open":
	default:
		bad("runner.egress.mode must be allowlist or open")
	}
	// A required gate without a command is allowed here: it reports
	// MISCONFIGURED at run time and fails the job, so the gap is visible.
	return errors.Join(errs...)
}

var serviceName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

// RepoNames lists the repos the factory works on, the first being the one
// commands use when you don't name one.
func (c *Config) RepoNames() []string {
	ns := make([]string, len(c.Repos))
	for i, r := range c.Repos {
		ns[i] = r.Name
	}
	return ns
}

// RepoConf returns a repo's settings by name, ignoring case as GitHub
// does, with the top-level base_branch and gates filling what it leaves out.
func (c *Config) RepoConf(name string) (Repo, bool) {
	for _, r := range c.Repos {
		if strings.EqualFold(r.Name, name) {
			return c.resolve(r), true
		}
	}
	return Repo{}, false
}

// Workspace returns the workspace a repo belongs to and its members, in
// merge order, as listed in repos; "" and nil for a repo on its own.
func (c *Config) Workspace(repo string) (string, []string) {
	for _, name := range sortedKeys(c.Workspaces) {
		for _, m := range c.Workspaces[name] {
			if strings.EqualFold(m, repo) {
				out := make([]string, 0, len(c.Workspaces[name]))
				for _, x := range c.Workspaces[name] {
					if r, ok := c.RepoConf(x); ok {
						x = r.Name
					}
					out = append(out, x)
				}
				return name, out
			}
		}
	}
	return "", nil
}

// ShortName is a repo's name without its owner or group: the folder it is
// checked out in within a workspace.
func ShortName(repo string) string {
	return strings.ToLower(repo[strings.LastIndex(repo, "/")+1:])
}

var wsName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

func sortedKeys[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// Primary is the first repo: the one commands use when you name none.
func (c *Config) Primary() Repo { return c.resolve(c.Repos[0]) }

// Resolve fills a repo's unset settings from the top level, as RepoConf
// does for listed repos.
func (c *Config) Resolve(r Repo) Repo { return c.resolve(r) }

func (c *Config) resolve(r Repo) Repo {
	if r.BaseBranch == "" {
		r.BaseBranch = c.BaseBranch
	}
	if r.Image == "" && r.ImageFrom != "" {
		r.Image = "factory-job-" + strings.ToLower(strings.NewReplacer("/", "__", ":", "-").Replace(r.Name)) + ":latest"
	}
	if r.Image == "" {
		r.Image = c.Runner.Image
	}
	env := map[string]string{}
	for k, v := range c.Env {
		env[k] = v
	}
	for k, v := range r.Env {
		env[k] = v
	}
	r.Env = env
	if r.Services == nil {
		r.Services = c.Services
	}
	if r.AutoMerge == nil {
		r.AutoMerge = c.AutoMerge
	}
	r.Gates = mergeGates(c.Gates, r.Gates)
	if r.GitLab() {
		// A pushed branch runs its own .gitlab-ci.yml with the project's
		// runners and CI variables, and a GitLab token, unlike GitHub's,
		// can push pipeline changes. So builders never may.
		pp := slices.Clone(r.Gates.DiffGuard.ProtectedPaths)
		for _, p := range []string{".gitlab-ci.yml", ".gitlab/"} {
			if !slices.Contains(pp, p) {
				pp = append(pp, p)
			}
		}
		r.Gates.DiffGuard.ProtectedPaths = pp
	}
	return r
}

// mergeGates lays a repo's gates over the top-level ones.
func mergeGates(top Gates, own *Gates) *Gates {
	if own == nil {
		own = &Gates{}
	}
	g := *own
	if g.Required == nil {
		g.Required = top.Required
	}
	// Commands replace the top level's rather than add to them: a
	// TypeScript repo shouldn't run the Go repo's test command.
	if g.Commands == nil {
		g.Commands = top.Commands
	}
	if g.ProvesFix == "" {
		g.ProvesFix = top.ProvesFix
	}
	if g.Coverage == "" {
		g.Coverage = top.Coverage
	}
	if g.CoverageSlack == 0 {
		g.CoverageSlack = top.CoverageSlack
	}
	if g.ScenariosDir == "" {
		g.ScenariosDir = top.ScenariosDir
	}
	if g.Install == "" {
		g.Install = top.Install
	}
	for _, s := range []*string{&g.ProvesFix, &g.Coverage, &g.ScenariosDir, &g.Install} {
		if *s == "none" {
			*s = ""
		}
	}
	if g.DiffGuard.MaxChangedLines == 0 {
		g.DiffGuard.MaxChangedLines = top.DiffGuard.MaxChangedLines
	}
	if g.DiffGuard.ProtectedPaths == nil {
		g.DiffGuard.ProtectedPaths = top.DiffGuard.ProtectedPaths
	}
	return &g
}

// Path resolves p against the config file's directory.
func (c *Config) Path(p string) string {
	if filepath.IsAbs(p) || c.dir == "" {
		return p
	}
	return filepath.Join(c.dir, p)
}

// Implement returns the route for an implement job of the given complexity
// and attempt history. Models follow escalation.implement: attempts
// 1..max_attempts_per_tier use the route's model, then each further block of
// attempts moves one tier up. ok is false once the top tier is exhausted.
func (c *Config) Implement(complexity string, attempt int) (Route, bool) {
	r, found := c.Routing["implement."+complexity]
	if !found && complexity == "XS" {
		r, found = c.Routing["implement.S"]
	}
	if !found {
		r = c.Routing["implement.M"]
	}
	tiers := c.Escalation.Implement
	start := 0
	for i, m := range tiers {
		if m == r.Model {
			start = i
		}
	}
	tier := start + (attempt-1)/c.Escalation.MaxAttemptsPerTier
	if tier >= len(tiers) {
		return r, false
	}
	if (attempt-1)/c.Escalation.MaxAttemptsPerTier > 0 {
		r.Model = tiers[tier]
	}
	return r, true
}

// builderModels lists every model an implement route can run with: its own,
// then each escalation tier above it (see Implement).
func (c *Config) builderModels(r Route) []string {
	ms := []string{r.Model}
	tiers := c.Escalation.Implement
	start := 0
	for i, m := range tiers {
		if m == r.Model {
			start = i
		}
	}
	return append(ms, tiers[min(start, len(tiers)):]...)
}

// sameModel reports whether two model names on one harness may be the same
// model. Claude accepts both aliases ("sonnet") and full ids
// ("claude-sonnet-4-5"), so Claude models compare by family.
func sameModel(harness, a, b string) bool {
	a, b = strings.ToLower(a), strings.ToLower(b)
	if a == b {
		return true
	}
	if harness == "claude" {
		for _, f := range []string{"fable", "opus", "sonnet", "haiku"} {
			if strings.Contains(a, f) && strings.Contains(b, f) {
				return true
			}
		}
	}
	return false
}

// IsOpus says whether a Claude model has fast mode: the opus alias, Opus
// 4.8, and Opus 5 and later.
func IsOpus(model string) bool {
	m := strings.ToLower(model)
	if m == "opus" || strings.Contains(m, "opus-4-8") {
		return true
	}
	if _, after, ok := strings.Cut(m, "opus-"); ok && after != "" && after[0] >= '5' && after[0] <= '9' {
		return true
	}
	return false
}

// FastBudget scales a job's dollar cap for fast mode, which costs more per
// token: by budgets.fast_multiplier, 2 unless set.
func (c *Config) FastBudget(usd float64, fast bool) float64 {
	if !fast {
		return usd
	}
	m := c.Budgets.FastMultiplier
	if m <= 0 {
		m = 2
	}
	return usd * m
}

// PauseAtUsage is budgets.pause_at_usage with its default.
func (c *Config) PauseAtUsage() float64 {
	if c.Budgets.PauseAtUsage <= 0 {
		return 95
	}
	return c.Budgets.PauseAtUsage
}

// FastFor says whether a job on route r with this model runs in fast mode.
func (r Route) FastFor(harness, model string) bool {
	return r.Fast && harness == "claude" && IsOpus(model)
}

// Review returns the reviewer route for an item's risk: review.high_risk
// or review.medium_risk when set, else review.
func (c *Config) Review(risk string) Route {
	return c.Routing[c.ReviewKey(risk)]
}

// ReviewKey is the routing entry Review picks.
func (c *Config) ReviewKey(risk string) string {
	if risk == "high" || risk == "medium" {
		if _, ok := c.Routing["review."+risk+"_risk"]; ok {
			return "review." + risk + "_risk"
		}
	}
	return "review"
}

// ImplementKey is the routing entry Implement starts from.
func (c *Config) ImplementKey(complexity string) string {
	if _, ok := c.Routing["implement."+complexity]; ok {
		return "implement." + complexity
	}
	if _, ok := c.Routing["implement.S"]; ok && complexity == "XS" {
		return "implement.S"
	}
	return "implement.M"
}

// StepInstructions is what a step's prompt gets on top of its template:
// the route's skills to use and instructions, then the repo's for the
// step. kind is the job kind (triage, implement, review, critic,
// orchestrate). With several repos, as in a workspace, each repo's
// instructions are labeled with its name.
func (c *Config) StepInstructions(route, kind string, repos ...string) (string, error) {
	var parts []string
	r := c.Routing[route]
	if len(r.Use) > 0 {
		parts = append(parts, "Use these skills from your skills directory, in this order: "+strings.Join(r.Use, ", ")+".")
	}
	if s := strings.TrimSpace(r.Instructions); s != "" {
		parts = append(parts, s)
	}
	if r.InstructionsFile != "" {
		b, err := os.ReadFile(c.Path(r.InstructionsFile))
		if err != nil {
			return "", fmt.Errorf("routing.%s instructions_file: %w", route, err)
		}
		if s := strings.TrimSpace(string(b)); s != "" {
			parts = append(parts, s)
		}
	}
	for _, repo := range repos {
		if rc, ok := c.RepoConf(repo); ok {
			if s := strings.TrimSpace(rc.Instructions[kind]); s != "" {
				if len(repos) > 1 {
					s = "In " + repo + ":\n\n" + s
				}
				parts = append(parts, s)
			}
		}
	}
	return strings.Join(parts, "\n\n"), nil
}

// JobBudget returns the dollar cap for a job.
func (c *Config) JobBudget(kind, complexity string) float64 {
	switch kind {
	case "triage":
		return c.Budgets.Triage
	case "review":
		return c.Budgets.Review
	}
	if v, ok := c.Budgets.PerJob[complexity]; ok {
		return v
	}
	if v, ok := c.Budgets.PerJob["S"]; ok && complexity == "XS" {
		return v
	}
	return c.Budgets.PerJob["M"]
}

// Timeout returns the wall-clock limit for a stage.
func (c *Config) Timeout(stage string) time.Duration {
	if d, ok := c.Timeouts[stage]; ok && d.Duration > 0 {
		return d.Duration
	}
	return 30 * time.Minute
}

// CostOf prices token usage for models that do not report dollars. in
// includes cached, the input read from the prompt cache.
func (c *Config) CostOf(model string, in, cached, out int64) float64 {
	p := c.Pricing[model]
	cachedPrice := p.CachedPerMTok
	if cachedPrice == 0 {
		cachedPrice = p.InputPerMTok
	}
	cached = min(max(cached, 0), in)
	return float64(in-cached)/1e6*p.InputPerMTok + float64(cached)/1e6*cachedPrice + float64(out)/1e6*p.OutputPerMTok
}
