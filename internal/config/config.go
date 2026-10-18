// Package config loads and validates factory.yaml.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Route says which harness and model run a stage.
type Route struct {
	Harness string `yaml:"harness"`
	Model   string `yaml:"model"`
	Effort  string `yaml:"effort"`
	Skills  string `yaml:"skills"` // skill set folder under skills/
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
	// Gates overrides the top-level gates field by field: what it leaves
	// out (the diff guard's protected paths, say) is inherited, and its
	// commands replace the top level's. install, proves_fix or scenarios_dir "none"
	// turns one off.
	Gates *Gates `yaml:"gates,omitempty"`
}

// MarshalYAML writes Required even when it is empty but not nil, since an
// empty list (no checks) and a missing one (inherit) differ.
func (g Gates) MarshalYAML() (any, error) {
	out := struct {
		Install      string            `yaml:"install,omitempty"`
		Required     *[]string         `yaml:"required,omitempty"`
		Commands     map[string]string `yaml:"commands,omitempty"`
		ProvesFix    string            `yaml:"proves_fix,omitempty"`
		ScenariosDir string            `yaml:"scenarios_dir,omitempty"`
		DiffGuard    any               `yaml:"diff_guard,omitempty"`
	}{Install: g.Install, Commands: g.Commands, ProvesFix: g.ProvesFix, ScenariosDir: g.ScenariosDir}
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
	if r.BaseBranch == "" && r.Image == "" && r.ImageFrom == "" && len(r.Env) == 0 && r.Gates == nil {
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

type Config struct {
	// SingleRepo is the short form for a factory with one repo; Repos is
	// filled from it. Use RepoNames and RepoConf rather than either field.
	SingleRepo string `yaml:"repo"`
	Repos      []Repo `yaml:"repos"`
	// BaseBranch, Env and Gates are the defaults for every repo.
	BaseBranch string            `yaml:"base_branch"`
	Env        map[string]string `yaml:"env"`
	DataDir    string            `yaml:"data_dir"`
	SkillsDir  string            `yaml:"skills_dir"`
	PromptsDir string            `yaml:"prompts_dir"`

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
	} `yaml:"runner"`

	Web struct {
		// Listen is the control room address. Keep it on localhost or a
		// Tailscale address: there is no login in v1.
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
	} `yaml:"budgets"`

	Pricing map[string]Price `yaml:"pricing"`

	Gates Gates `yaml:"gates"`

	dir string
}

// Price is dollars per million tokens.
type Price struct {
	InputPerMTok  float64 `yaml:"input_per_mtok"`
	OutputPerMTok float64 `yaml:"output_per_mtok"`
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
	c, err := Parse(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	abs, _ := filepath.Abs(filepath.Dir(path))
	c.dir = abs
	return c, nil
}

// Parse decodes and validates config bytes, filling defaults.
func Parse(b []byte) (*Config, error) {
	var c Config
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
	if first, rest, ok := strings.Cut(name, "/"); ok && strings.Contains(first, ".") {
		host, name = first, rest
	}
	host = strings.ToLower(strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(host, "https://"), "http://"), "/"))
	if host == "" || host == "github.com" || host == "www.github.com" {
		return "", name
	}
	return host, host + "/" + name
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
	seen := map[string]bool{}
	for _, r := range c.Repos {
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
	}
	for _, size := range []string{"S", "M", "L"} {
		b, ok := c.Routing["implement."+size]
		if !ok {
			continue
		}
		for _, rk := range []string{"review", "review.high_risk"} {
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
		if c.Budgets.PerJob[size] <= 0 {
			bad("budgets.per_job.%s must be > 0", size)
		}
	}
	if r, ok := c.Routing["orchestrate"]; ok && r.Harness != "claude" {
		bad("routing.orchestrate must use the claude harness (it drives the daemon's MCP tools)")
	}
	if len(c.Escalation.Implement) == 0 {
		bad("escalation.implement needs at least one model tier")
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
	// A required gate without a command is allowed here: it reports
	// MISCONFIGURED at run time and fails the job, so the gap is visible.
	return errors.Join(errs...)
}

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

// Primary is the first repo: the one commands use when you name none.
func (c *Config) Primary() Repo { return c.resolve(c.Repos[0]) }

func (c *Config) resolve(r Repo) Repo {
	if r.BaseBranch == "" {
		r.BaseBranch = c.BaseBranch
	}
	if r.Image == "" && r.ImageFrom != "" {
		r.Image = "factory-job-" + strings.ToLower(strings.ReplaceAll(r.Name, "/", "__")) + ":latest"
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
	r.Gates = mergeGates(c.Gates, r.Gates)
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
	if g.ScenariosDir == "" {
		g.ScenariosDir = top.ScenariosDir
	}
	if g.Install == "" {
		g.Install = top.Install
	}
	for _, s := range []*string{&g.ProvesFix, &g.ScenariosDir, &g.Install} {
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
		for _, f := range []string{"opus", "sonnet", "haiku"} {
			if strings.Contains(a, f) && strings.Contains(b, f) {
				return true
			}
		}
	}
	return false
}

// Review returns the reviewer route for an item's risk.
func (c *Config) Review(risk string) Route {
	if risk == "high" {
		if r, ok := c.Routing["review.high_risk"]; ok {
			return r
		}
	}
	return c.Routing["review"]
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
	return c.Budgets.PerJob["M"]
}

// Timeout returns the wall-clock limit for a stage.
func (c *Config) Timeout(stage string) time.Duration {
	if d, ok := c.Timeouts[stage]; ok && d.Duration > 0 {
		return d.Duration
	}
	return 30 * time.Minute
}

// CostOf prices token usage for models that do not report dollars.
func (c *Config) CostOf(model string, in, out int64) float64 {
	p := c.Pricing[model]
	return float64(in)/1e6*p.InputPerMTok + float64(out)/1e6*p.OutputPerMTok
}
