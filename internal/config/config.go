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

type Config struct {
	Repo       string `yaml:"repo"`
	BaseBranch string `yaml:"base_branch"`
	DataDir    string `yaml:"data_dir"`
	SkillsDir  string `yaml:"skills_dir"`
	PromptsDir string `yaml:"prompts_dir"`

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

	Routing map[string]Route `yaml:"routing"`

	Escalation struct {
		Implement          []string `yaml:"implement"`
		MaxAttemptsPerTier int      `yaml:"max_attempts_per_tier"`
		MaxReviewRounds    int      `yaml:"max_review_rounds"`
	} `yaml:"escalation"`

	Timeouts map[string]Duration `yaml:"timeouts"`

	Budgets struct {
		PerJob           map[string]float64 `yaml:"per_job"`
		Triage           float64            `yaml:"triage"`
		Review           float64            `yaml:"review"`
		DailyTotal       float64            `yaml:"daily_total"`
		ConcurrentJobs   int                `yaml:"concurrent_jobs"`
		AwaitingHumanMax int                `yaml:"awaiting_human_max"`
	} `yaml:"budgets"`

	Pricing map[string]Price `yaml:"pricing"`

	Gates struct {
		Required  []string          `yaml:"required"`
		Commands  map[string]string `yaml:"commands"`
		DiffGuard struct {
			MaxChangedLines int      `yaml:"max_changed_lines"`
			ProtectedPaths  []string `yaml:"protected_paths"`
		} `yaml:"diff_guard"`
	} `yaml:"gates"`

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
	for k, v := range map[string]time.Duration{"triage": 10 * time.Minute, "implement": 60 * time.Minute, "review": 20 * time.Minute, "gates": 20 * time.Minute} {
		if c.Timeouts[k].Duration == 0 {
			c.Timeouts[k] = Duration{v}
		}
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
}

var harnesses = map[string]bool{"claude": true, "codex": true}

// Validate checks the rules in SPEC.md, including that the builder and the
// reviewer never share a harness.
func (c *Config) Validate() error {
	var errs []error
	bad := func(f string, a ...any) { errs = append(errs, fmt.Errorf(f, a...)) }
	if c.Repo == "" || !strings.Contains(c.Repo, "/") {
		bad("repo must be owner/name")
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
			if r, ok := c.Routing[rk]; ok && r.Harness == b.Harness {
				bad("routing.implement.%s and routing.%s both use %s: the builder and reviewer must be different harnesses", size, rk, b.Harness)
			}
		}
		if c.Budgets.PerJob[size] <= 0 {
			bad("budgets.per_job.%s must be > 0", size)
		}
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
	// A required gate without a command is allowed here: it reports
	// MISCONFIGURED at run time and fails the job, so the gap is visible.
	return errors.Join(errs...)
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
