// Package config loads and validates factory.yaml.
package config

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strconv"
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
	// Breaker turns auto-merge off for a repo once its merged PRs are
	// reverted, or turn main red, too often. See Breaker.
	Breaker *Breaker `yaml:"breaker,omitempty"`
}

// Breaker is auto-merge's circuit breaker. A strike is a factory PR that
// was reverted on the base branch, or whose merge commit main health saw
// a check go red on. Once a repo has Threshold strikes within Window, the
// factory stops auto-merging there, cancels the auto-merges waiting, and
// tells you; it stays off until you reset it, or CoolOff passes.
type Breaker struct {
	// Threshold is how many strikes trip it; default 2, -1 never trips.
	Threshold int `yaml:"threshold,omitempty"`
	// Window is the rolling period strikes count in; default 168h.
	Window Duration `yaml:"window,omitempty"`
	// CoolOff, when set, resets a tripped breaker after that long; unset,
	// only a human resets it.
	CoolOff Duration `yaml:"cool_off,omitempty"`
}

// BreakerThreshold is how many strikes trip the breaker, or 0 for never.
func (a *AutoMerge) BreakerThreshold() int {
	switch {
	case a == nil || a.Breaker == nil || a.Breaker.Threshold == 0:
		return 2
	case a.Breaker.Threshold < 0:
		return 0
	}
	return a.Breaker.Threshold
}

// BreakerWindow is the rolling period the breaker counts strikes in.
func (a *AutoMerge) BreakerWindow() time.Duration {
	if a == nil || a.Breaker == nil || a.Breaker.Window.Duration <= 0 {
		return 7 * 24 * time.Hour
	}
	return a.Breaker.Window.Duration
}

// BreakerCoolOff is how long a tripped breaker stays tripped on its own,
// or 0 for until a human resets it.
func (a *AutoMerge) BreakerCoolOff() time.Duration {
	if a == nil || a.Breaker == nil {
		return 0
	}
	return a.Breaker.CoolOff.Duration
}

// On says whether the setting lets any PR merge itself.
func (a *AutoMerge) On() bool { return a != nil && (a.UpTo == "low" || a.UpTo == "medium") }

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
	// ImageBuild builds the base from the repo's own Dockerfile instead,
	// at its base branch, so the checks run on what the app runs on. It
	// takes the place of image_from. See ImageBuild.
	ImageBuild *ImageBuild `yaml:"image_build,omitempty"`
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
	// SelfStart is trust level 6: the factory's own findings in this repo
	// start without a label (see SelfStart). Unset, they wait for you.
	SelfStart *SelfStart `yaml:"self_start,omitempty"`
	// Instructions are added to this repo's prompts, by step: triage,
	// implement, review or critic.
	Instructions map[string]string `yaml:"instructions,omitempty"`
	// Files are copied from a checkout on this machine into each of this
	// repo's job checkouts, and never committed. See Files.
	Files *Files `yaml:"files,omitempty"`
	// Preview screenshots the repo's web app for each PR. See Preview.
	Preview *Preview `yaml:"preview,omitempty"`
	// ConfirmOver replaces budgets.confirm_over for this repo; 0 turns
	// it off here.
	ConfirmOver *float64 `yaml:"confirm_over,omitempty"`
}

// ImageBuild is a Dockerfile in the repo that `factory images` builds a
// repo's job image on: Dockerfile and Context are paths in the repo
// (default Dockerfile and its root), Target a stage in it (default the
// last). The stage must be Debian or Ubuntu, like image_from's images.
// Args are its --build-arg values. Secrets are its build secrets (RUN
// --mount=type=secret), by id, each a file: a path in files.from's
// checkout, or an absolute one. They're never in the image or the repo.
type ImageBuild struct {
	Dockerfile string            `yaml:"dockerfile,omitempty" json:"dockerfile"`
	Target     string            `yaml:"target,omitempty" json:"target"`
	Context    string            `yaml:"context,omitempty" json:"context"`
	Args       map[string]string `yaml:"args,omitempty" json:"args"`
	Secrets    map[string]string `yaml:"secrets,omitempty" json:"secrets"`
}

// SecretFile is where build secret id's file is on this machine; "" when
// the repo doesn't name one.
func (c *Config) SecretFile(r Repo, id string) (string, error) {
	p := r.ImageBuild.Secrets[id]
	if p == "" || filepath.IsAbs(p) {
		return p, nil
	}
	if r.Files == nil {
		return "", fmt.Errorf("image_build.secrets.%s is a path in files.from's checkout, and the repo has no files.from", id)
	}
	from, err := c.FilesFrom(r.Files)
	if err != nil {
		return "", err
	}
	return filepath.Join(from, filepath.FromSlash(p)), nil
}

// BaseImage is the tag `factory images` gives the image built from a
// repo's Dockerfile, before the factory's tools go on top.
func (r Repo) BaseImage() string {
	return "factory-base-" + repoTag(r.Name) + ":latest"
}

func repoTag(name string) string {
	return strings.ToLower(strings.NewReplacer("/", "__", ":", "-").Replace(name))
}

// Files are files a repo needs to build or test but doesn't commit, such
// as a .env: they're copied from From, a checkout of the repo on this
// machine. Paths lists them (files or folders, relative to the repo);
// without it, the .worktreeinclude file in From picks them, as Claude Code
// does for its worktrees: the files that match it and are gitignored.
type Files struct {
	From  string   `yaml:"from"`
	Paths []string `yaml:"paths,omitempty"`
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
	if r.BaseBranch == "" && r.Image == "" && r.ImageFrom == "" && r.ImageBuild == nil && len(r.Env) == 0 && r.Gates == nil && len(r.Services) == 0 && len(r.Instructions) == 0 && r.AutoMerge == nil && r.SelfStart == nil && r.Files == nil && r.Preview == nil && r.ConfirmOver == nil {
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
	// Workspaces are projects: the repos that belong together, and what
	// the dashboard, the orchestrator, trust and budgets are scoped to.
	// A repo in none has one of its own. See Workspace.
	Workspaces map[string]Workspace `yaml:"workspaces,omitempty"`
	// AutoMerge lets the factory merge its own PRs up to a risk level and
	// size, once CI passes. Unset, every PR waits for you.
	AutoMerge *AutoMerge `yaml:"auto_merge,omitempty"`
	// BaseBranch, Env and Gates are the defaults for every repo. Unset
	// here and on a repo, the repo's default branch on its host is used
	// (see SetDefaultBranch), and main when that isn't known.
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

	// Notify is when notifications go out: where is FACTORY_NOTIFY_URL
	// and the browsers that turned on the dashboard's push.
	Notify struct {
		QuietHours QuietHours `yaml:"quiet_hours"`
	} `yaml:"notify"`

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

	// Tuning is the loop that reads the metrics on a cadence and proposes
	// changes to the settings the dashboard may edit. See Tuning.
	Tuning   Tuning   `yaml:"tuning"`
	Learning Learning `yaml:"learning"`
	// MainHealth files fix issues when CI goes red or flakes on a repo's
	// base branch. See MainHealth.
	MainHealth MainHealth `yaml:"main_health"`
	// Sources file issues from Sentry, Dependabot and TODO comments, for
	// you to approve. See Sources.
	Sources Sources `yaml:"sources"`
	// Hunt reviews one feature area at a time for old bugs, and files
	// them for you to approve. See Hunt.
	Hunt Hunt `yaml:"hunt"`
	// Grooming flags duplicate, stale and too-big issues on a cadence, for
	// you to confirm. See Grooming.
	Grooming Grooming `yaml:"grooming"`
	// Previews are the defaults and limits of each repo's preview.
	Previews Previews `yaml:"previews"`
	// Reviews says how review checks its own findings. See Reviews.
	Reviews Reviews `yaml:"review"`
	// Risk says how a built change's diff raises its risk, and which
	// kinds of change always wait for you. See Risk.
	Risk Risk `yaml:"risk"`
	// Sensitive is the one list of sensitive code the security pass, risk
	// and REVIEW.md's skip all read. See Sensitive.
	Sensitive Sensitive `yaml:"sensitive"`
	// Security is the security pass for changes that touch auth, input
	// handling, commands, SQL or dependencies. See Security.
	Security Security `yaml:"security"`
	// Plan is the plan check for big items before the build. See Plan.
	Plan Plan `yaml:"plan"`
	// ConciseAsks shapes what the factory writes for a person to act on
	// (plans and chat replies) with prompts/concise.md. On unless false.
	ConciseAsks *bool `yaml:"concise_asks,omitempty"`
	// NoGuard turns off auto-merge's guard rails' polling and breaker: the
	// revert watch and the strikes that trip it. eval and compare set it,
	// since the base branch they'd read is the real repo's, not the run's.
	NoGuard bool `yaml:"-"`
	// PR shapes the factory's PR descriptions. See PR.
	PR PR `yaml:"pr,omitempty"`

	Budgets struct {
		PerJob map[string]float64 `yaml:"per_job"`
		Triage float64            `yaml:"triage"`
		Review float64            `yaml:"review"`
		// Verify is dollars per verifier run; 0 uses budgets.triage.
		Verify           float64 `yaml:"verify,omitempty"`
		DailyTotal       float64 `yaml:"daily_total"`
		ConcurrentJobs   int     `yaml:"concurrent_jobs"`
		AwaitingHumanMax int     `yaml:"awaiting_human_max"`
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
		// ConfirmOver holds an item before implement, for a human's OK,
		// when triage's cost estimate (its p75) is over this many
		// dollars. 0, the default, never holds one.
		ConfirmOver float64 `yaml:"confirm_over,omitempty"`
		// MaxTurns caps the turns (model calls) of one build by size, as
		// Claude's --max-turns; see DefaultMaxTurns. One number sets every
		// size. Other steps keep a cap of 80. A build that runs out stops,
		// keeps its work and waits for a person to continue it.
		MaxTurns TurnCaps `yaml:"max_turns,omitempty"`
	} `yaml:"budgets"`

	Pricing map[string]Price `yaml:"pricing"`

	Gates Gates `yaml:"gates"`

	dir string
	// defaultBranches are repos' default branches on their hosts, by
	// lower-cased name (see SetDefaultBranch).
	defaultBranches map[string]string
	// noOrchestrator turns every orchestrator off (see DisableOrchestrator).
	noOrchestrator bool
}

// MinTuningEvery is the shortest tuning cadence: each run costs a model
// run, and a period shorter than this holds too few builds to read.
const MinTuningEvery = time.Hour

// Tuning says how often the factory proposes changes from its metrics,
// and where its factory.yaml lives in a repo it works on, if anywhere.
type Tuning struct {
	// EveryText is the cadence as written: a duration such as 720h (the
	// default, about monthly), or 0 or none for off. Every is it parsed.
	EveryText string        `yaml:"every"`
	Every     time.Duration `yaml:"-"`
	// MinItems skips a run when fewer items were built since the last
	// one; default 10.
	MinItems int `yaml:"min_items"`
	// Budget is dollars per run; default orchestrator.run_budget.
	Budget float64 `yaml:"budget"`
	// Repo and Path name this file in one of the factory's repos; with
	// Repo set, each proposal also opens a draft PR there. Path defaults
	// to factory.yaml.
	Repo string `yaml:"repo"`
	Path string `yaml:"path"`
	// ReviewEval runs the reviewer evals (the kind: planted, clean and
	// revert cases in ReviewEvalSet) on the current config and on the
	// proposal before keeping a change to how review works; a proposal
	// worse by more than ReviewEvalTolerance (a share, default 0.05)
	// loses those changes. Off by default: it costs two runs of the set.
	ReviewEval          bool     `yaml:"review_eval,omitempty"`
	ReviewEvalSet       string   `yaml:"review_eval_set,omitempty"`       // default evals
	ReviewEvalTolerance *float64 `yaml:"review_eval_tolerance,omitempty"` // default 0.05; 0 allows no drop
	// ReviewEvalCases is the most cases each run reviews, the first by
	// name; default 20.
	ReviewEvalCases int `yaml:"review_eval_cases,omitempty"`

	everyErr error
}

// ReviewTolerance is tuning.review_eval_tolerance, 0.05 when left out.
func (t Tuning) ReviewTolerance() float64 {
	if t.ReviewEvalTolerance == nil {
		return 0.05
	}
	return *t.ReviewEvalTolerance
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

// MarshalYAML writes a duration as it's read back: 2m0s, 1h30m0s.
func (d Duration) MarshalYAML() (any, error) { return d.String(), nil }

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

// autoCPUs is runner.cpus left out: this machine's cores shared by the
// jobs that run at once, so a full line can't starve it.
func autoCPUs(cores, jobs int) string {
	return strconv.Itoa(max(1, cores/max(1, jobs)))
}

func (c *Config) defaults() {
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
	for k, v := range map[string]time.Duration{"triage": 10 * time.Minute, "implement": 60 * time.Minute, "review": 20 * time.Minute, "gates": 20 * time.Minute, "orchestrate": 20 * time.Minute, "tune": 20 * time.Minute} {
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
		c.Routing["orchestrate"] = c.defaultOrchestrate()
	}
	t := &c.Tuning
	switch strings.TrimSpace(t.EveryText) {
	case "":
		t.Every = 720 * time.Hour
	case "none":
		t.Every = 0
	default:
		t.Every, t.everyErr = time.ParseDuration(strings.TrimSpace(t.EveryText))
	}
	if t.MinItems == 0 {
		t.MinItems = 10
	}
	if t.Budget == 0 {
		t.Budget = o.RunBudget
	}
	if t.Path == "" {
		t.Path = "factory.yaml"
	}
	if t.ReviewEvalSet == "" {
		t.ReviewEvalSet = "evals"
	}
	if t.ReviewEvalCases == 0 {
		t.ReviewEvalCases = 20
	}
	if _, ok := c.Routing["tune"]; !ok && c.Routing != nil {
		c.Routing["tune"] = c.Routing["orchestrate"]
	}
	c.learningDefaults()
	c.healthDefaults()
	c.sourcesDefaults()
	c.groomingDefaults()
	c.previewDefaults()
	c.reviewDefaults()
	c.riskDefaults()
	c.huntDefaults()
	c.planDefaults()
	if c.Budgets.ConcurrentJobs == 0 {
		c.Budgets.ConcurrentJobs = 3
	}
	if c.Runner.CPUs == "" {
		c.Runner.CPUs = autoCPUs(runtime.NumCPU(), c.Budgets.ConcurrentJobs)
	}
	if c.Budgets.MinFreeDiskGB == 0 {
		c.Budgets.MinFreeDiskGB = 10
	}
	if c.Budgets.MaxTurns == nil {
		c.Budgets.MaxTurns = TurnCaps{}
	}
	for size, n := range DefaultMaxTurns {
		if c.Budgets.MaxTurns[size] == 0 {
			c.Budgets.MaxTurns[size] = n
		}
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

// defaultOrchestrate is the orchestrator's route when routing leaves it
// out: Opus on Claude, or, when no route names Claude (a Codex-only
// factory), the strongest builder's harness and model, so a factory that
// has no Claude sign-in never needs one for it, or for tuning.
func (c *Config) defaultOrchestrate() Route {
	for _, r := range c.Routing {
		if r.Harness == "claude" {
			return Route{Harness: "claude", Model: "opus"}
		}
	}
	for _, k := range []string{"implement.L", "implement.M", "implement.S", "review"} {
		if r, ok := c.Routing[k]; ok {
			return Route{Harness: r.Harness, Model: r.Model}
		}
	}
	return Route{Harness: "claude", Model: "opus"}
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
	if err := c.Notify.QuietHours.check(); err != nil {
		errs = append(errs, err)
	}
	for _, r := range c.Repos {
		if err := r.SelfStart.check("repos[" + r.Name + "]"); err != nil {
			errs = append(errs, err)
		}
	}
	// No repos yet is a factory not set up: setup and serve's get-started
	// page ask for them, and the other commands refuse (see NoRepos).
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
		if b := a.Breaker; b != nil {
			if b.Threshold < -1 {
				bad("%sauto_merge.breaker.threshold is a number of strikes, or -1 to never trip", where)
			}
			if b.Window.Duration < 0 || b.CoolOff.Duration < 0 {
				bad("%sauto_merge.breaker: window and cool_off can't be negative", where)
			}
			if b.CoolOff.Duration > 0 && b.CoolOff.Duration < time.Hour {
				bad("%sauto_merge.breaker.cool_off must be at least 1h, or left out so only you reset it", where)
			}
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
			if !slices.Contains([]string{"triage", "implement", "review", "critic", "security"}, step) {
				bad("repo %s: instructions.%s: the steps are triage, implement, review, critic and security", r.Name, step)
			}
		}
		if b := r.ImageBuild; b != nil {
			if r.ImageFrom != "" {
				bad("repo %s: set image_build or image_from, not both", r.Name)
			}
			for _, p := range []string{b.Dockerfile, b.Context} {
				if c := path.Clean(p); p != "" && (path.IsAbs(c) || c == ".." || strings.HasPrefix(c, "../")) {
					bad("repo %s: image_build: %s must be a path inside the repo", r.Name, p)
				}
			}
			if b.Target != "" && !stageName.MatchString(b.Target) {
				bad("repo %s: image_build.target %q isn't a Dockerfile stage name", r.Name, b.Target)
			}
			for _, id := range sortedKeys(b.Secrets) {
				p := b.Secrets[id]
				switch {
				case !stageName.MatchString(id):
					bad("repo %s: image_build.secrets: %q isn't a secret id", r.Name, id)
				case !filepath.IsAbs(p) && r.Files == nil:
					bad("repo %s: image_build.secrets.%s is a path in files.from's checkout, so the repo needs files.from", r.Name, id)
				case !filepath.IsAbs(p):
					if _, err := CleanFilePath(p); err != nil {
						bad("repo %s: image_build.secrets.%s: %v", r.Name, id, err)
					}
				}
			}
		}
		if f := r.Files; f != nil {
			if err := c.checkFiles(f); err != nil {
				bad("repo %s: files: %v", r.Name, err)
			}
		}
		if r.ConfirmOver != nil && *r.ConfirmOver < 0 {
			bad("repo %s: confirm_over is dollars, or 0 to turn it off for this repo", r.Name)
		}
	}
	c.checkWorkspaces(seen, bad, checkAutoMerge)
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
		// Escalation tiers are model names on the route's own harness.
		if b.Harness == "codex" {
			for _, m := range c.escalatesTo(b) {
				if _, ok := c.Pricing[m]; !ok {
					bad("routing.implement.%s runs on codex and escalates to %q, which has no pricing entry: escalation.implement names models of the route's harness, so list Codex models there", size, m)
					break
				}
			}
		}
		if v, ok := c.Budgets.PerJob[size]; (ok || size != "XS") && v <= 0 { // a missing XS falls back to S's
			bad("budgets.per_job.%s must be > 0", size)
		}
	}
	if len(c.Escalation.Implement) == 0 {
		bad("escalation.implement needs at least one model tier")
	}
	if t := c.Tuning; t.everyErr != nil || t.Every < 0 {
		bad("tuning.every must be a duration such as 720h, or 0 or none to turn tuning off")
	} else if t.Every > 0 && t.Every < MinTuningEvery {
		bad("tuning.every must be at least 1h, or 0 or none to turn tuning off")
	}
	if c.Tuning.MinItems < 0 {
		bad("tuning.min_items can't be negative")
	}
	if c.Tuning.Budget < 0 {
		bad("tuning.budget must be > 0, or left out for orchestrator.run_budget")
	}
	if r := c.Tuning.Repo; r != "" {
		if _, ok := c.RepoConf(r); !ok {
			bad("tuning.repo %s isn't in repos", r)
		}
	}
	if t := c.Tuning.ReviewTolerance(); t < 0 || t >= 1 {
		bad("tuning.review_eval_tolerance must be a share from 0 to 1, such as 0.05")
	}
	if c.Tuning.ReviewEvalCases < 0 {
		bad("tuning.review_eval_cases can't be negative")
	}
	if _, err := CleanFilePath(c.Tuning.Path); err != nil {
		bad("tuning.path: %v", err)
	}
	errs = append(errs, c.checkLearning()...)
	errs = append(errs, c.checkHealth()...)
	errs = append(errs, c.checkSources()...)
	errs = append(errs, c.checkGrooming()...)
	errs = append(errs, c.checkPreviews()...)
	errs = append(errs, c.checkReview()...)
	errs = append(errs, c.checkRisk()...)
	errs = append(errs, c.checkPlan()...)
	errs = append(errs, c.checkSensitive()...)
	errs = append(errs, c.checkSecurity()...)
	errs = append(errs, c.checkHunt()...)
	switch c.PR.CheckOutput {
	case "", CheckOutputPrivate, CheckOutputAlways, CheckOutputNever:
	default:
		bad("pr.check_output must be private, always or never")
	}
	if c.Budgets.PauseAtUsage < 0 || c.Budgets.PauseAtUsage > 100 {
		bad("budgets.pause_at_usage is a percentage up to 100, or 0 for the default")
	}
	for size, n := range c.Budgets.MaxTurns {
		if n < 0 {
			bad("budgets.max_turns.%s must be > 0, or left out for %d", size, DefaultMaxTurns[size])
		}
	}
	if c.Budgets.DailyTotal <= 0 {
		bad("budgets.daily_total must be > 0")
	}
	if c.Budgets.ConfirmOver < 0 {
		bad("budgets.confirm_over is dollars, or 0 (the default) to turn it off")
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
			if !ServiceName.MatchString(name) {
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

var stageName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// ServiceName is what a service may be called: its host name on the job's network.
var ServiceName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

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

// BaseBranchUnset says factory.yaml names no base branch for repo, at the
// top level or on the repo, so its default branch on its host is used.
func (c *Config) BaseBranchUnset(repo string) bool {
	for _, r := range c.Repos {
		if strings.EqualFold(r.Name, repo) {
			return r.BaseBranch == "" && c.BaseBranch == ""
		}
	}
	return false
}

// SetDefaultBranch records repo's default branch on its host, the base
// branch for a repo whose base_branch is unset.
func (c *Config) SetDefaultBranch(repo, branch string) {
	if c.defaultBranches == nil {
		c.defaultBranches = map[string]string{}
	}
	c.defaultBranches[strings.ToLower(repo)] = branch
}

// NoRepos says no repo is chosen yet, as a new factory.yaml starts.
func (c *Config) NoRepos() bool { return len(c.Repos) == 0 }

// Primary is the first repo: the one commands use when you name none.
func (c *Config) Primary() Repo { return c.resolve(c.Repos[0]) }

// Resolve fills a repo's unset settings from the top level, as RepoConf
// does for listed repos.
func (c *Config) Resolve(r Repo) Repo { return c.resolve(r) }

func (c *Config) resolve(r Repo) Repo {
	if r.BaseBranch == "" {
		r.BaseBranch = c.BaseBranch
	}
	if r.BaseBranch == "" {
		r.BaseBranch = c.defaultBranches[strings.ToLower(r.Name)]
	}
	if r.BaseBranch == "" {
		r.BaseBranch = "main"
	}
	if r.Image == "" && (r.ImageFrom != "" || r.ImageBuild != nil) {
		r.Image = "factory-job-" + repoTag(r.Name) + ":latest"
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
	// Trust is the workspace's; a repo's own may only be stricter.
	w := c.Workspaces[c.WorkspaceOf(r.Name)]
	r.AutoMerge = trustOf(r.AutoMerge, w.AutoMerge, c.AutoMerge)
	r.SelfStart = selfStartOf(r.SelfStart, w.SelfStart)
	r.Gates = mergeGates(c.Gates, r.Gates)
	// REVIEW.md tells the reviewers what to look for and what to skip, so
	// the builder they review never may change it, whatever the config.
	always := AlwaysProtected
	if r.GitLab() {
		// A pushed branch runs its own .gitlab-ci.yml with the project's
		// runners and CI variables, and a GitLab token, unlike GitHub's,
		// can push pipeline changes. So builders never may.
		always = append(slices.Clone(always), ".gitlab-ci.yml", ".gitlab/")
	}
	pp := slices.Clone(r.Gates.DiffGuard.ProtectedPaths)
	for _, p := range always {
		if !slices.Contains(pp, p) {
			pp = append(pp, p)
		}
	}
	r.Gates.DiffGuard.ProtectedPaths = pp
	return r
}

// AlwaysProtected are protected paths every repo has, whatever its
// gates.diff_guard.protected_paths say: the repo's review guidance. A bare
// name protects that file name in any folder.
var AlwaysProtected = []string{"REVIEW.md", ".github/REVIEW.md"}

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

// FilesFrom is where a repo's files are copied from: files.from with ~
// expanded to the home of the user the daemon runs as, resolved against
// the config file's directory.
func (c *Config) FilesFrom(f *Files) (string, error) {
	from := f.From
	if from == "~" || strings.HasPrefix(from, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("files.from %s: %w", from, err)
		}
		from = filepath.Join(home, strings.TrimPrefix(from, "~"))
	}
	return c.Path(from), nil
}

// CleanFilePath checks a path under files.paths: relative, inside the
// repo, and not in .git. It returns it cleaned, with forward slashes.
func CleanFilePath(p string) (string, error) {
	c := filepath.ToSlash(filepath.Clean(p))
	switch {
	case p == "" || filepath.IsAbs(p) || strings.HasPrefix(p, "/"):
		return "", fmt.Errorf("%q must be a path inside the repo", p)
	case c == "." || c == ".." || strings.HasPrefix(c, "../"):
		return "", fmt.Errorf("%q must be a path inside the repo", p)
	case c == ".git" || strings.HasPrefix(c, ".git/"):
		return "", fmt.Errorf("%q is inside .git", p)
	}
	return c, nil
}

func (c *Config) checkFiles(f *Files) error {
	// Only the syntax: a checkout that's moved or not mounted yet fails
	// that repo's jobs, with the path, rather than every command.
	if f.From == "" {
		return errors.New("from is required: a checkout of the repo on this machine")
	}
	for _, p := range f.Paths {
		if _, err := CleanFilePath(p); err != nil {
			return err
		}
	}
	return nil
}

// Path resolves p against the config file's directory.
func (c *Config) Path(p string) string {
	if filepath.IsAbs(p) || c.dir == "" {
		return p
	}
	return filepath.Join(c.dir, p)
}

// DefaultMaxTurns are the builds' turn caps by size. They're a starting
// point, not a measurement: the dollar cap (budgets.per_job) is what a
// build can't go past, and stops it without warning, while running out of
// turns warns it to commit and note what's left so it can be continued.
// So these sit above what a build of each size usually takes.
var DefaultMaxTurns = TurnCaps{"XS": 40, "S": 80, "M": 150, "L": 300}

// TurnCaps are turn caps by size. In YAML, one number sets every size.
type TurnCaps map[string]int

func (t *TurnCaps) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		var v int
		if err := n.Decode(&v); err != nil {
			return err
		}
		*t = TurnCaps{}
		for size := range DefaultMaxTurns {
			(*t)[size] = v
		}
		return nil
	}
	var m map[string]int
	if err := n.Decode(&m); err != nil {
		return err
	}
	*t = m
	return nil
}

// BuildTurns is the turn cap of a build of the given size; an unknown
// size gets M's, as its route does.
func (c *Config) BuildTurns(size string) int {
	if n := c.Budgets.MaxTurns[size]; n > 0 {
		return n
	}
	return c.Budgets.MaxTurns["M"]
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

// escalatesTo lists the models a retry moves an implement route to, as
// Implement picks them: the tiers after the one it starts on, which is its
// own model's tier, or the first when its model isn't a tier.
func (c *Config) escalatesTo(r Route) []string {
	tiers := c.Escalation.Implement
	start := 0
	for i, m := range tiers {
		if m == r.Model {
			start = i
		}
	}
	if start+1 >= len(tiers) {
		return nil
	}
	return tiers[start+1:]
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

// ConfirmOver is the estimate, in dollars, over which a repo's items wait
// for a human's OK before implement: the repo's confirm_over, else
// budgets.confirm_over. 0 is off.
func (c *Config) ConfirmOver(repo string) float64 {
	if r, ok := c.RepoConf(repo); ok && r.ConfirmOver != nil {
		return *r.ConfirmOver
	}
	return c.Budgets.ConfirmOver
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
	case "verify":
		if c.Budgets.Verify > 0 {
			return c.Budgets.Verify
		}
		return c.Budgets.Triage
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

// PR shapes the factory's PR descriptions.
type PR struct {
	// CheckOutput says when the Testing section quotes the end of each
	// passing check's output (redacted): private (the default) only on a
	// repo the forge reports as private, always, or never. Each check's
	// pass or fail is always shown.
	CheckOutput string `yaml:"check_output,omitempty"`
}

// The values of pr.check_output.
const (
	CheckOutputPrivate = "private"
	CheckOutputAlways  = "always"
	CheckOutputNever   = "never"
)

// ShowOutput says whether a PR quotes the checks' output, given whether
// its repo is private (false when the forge didn't say).
func (p PR) ShowOutput(private bool) bool {
	switch p.CheckOutput {
	case CheckOutputAlways:
		return true
	case CheckOutputNever:
		return false
	}
	return private
}
