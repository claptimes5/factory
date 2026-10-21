// Command factory turns labeled GitHub issues into reviewed draft PRs.
//
//	factory setup      ask for keys and settings, save them, create labels
//	factory doctor     check what the factory needs is installed and set up
//	factory serve      run the daemon
//	factory run-once   poll once and run queued jobs to completion, then exit
//	factory status     print items, the queue and today's spend
//	factory codex-login sign Codex in to a ChatGPT plan
//	factory uninstall  remove the factory's labels, job images and (--data) data
//	factory migrate    apply database migrations and exit
//	factory backup F   write a consistent copy of the database to F
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/claptimes5/factory/internal/compare"
	"github.com/claptimes5/factory/internal/config"
	"github.com/claptimes5/factory/internal/eval"
	"github.com/claptimes5/factory/internal/github"
	"github.com/claptimes5/factory/internal/harness"
	"github.com/claptimes5/factory/internal/mcp"
	"github.com/claptimes5/factory/internal/metrics"
	"github.com/claptimes5/factory/internal/notify"
	"github.com/claptimes5/factory/internal/pipeline"
	"github.com/claptimes5/factory/internal/runner"
	"github.com/claptimes5/factory/internal/sentry"
	"github.com/claptimes5/factory/internal/state"
	"github.com/claptimes5/factory/internal/store"
	"github.com/claptimes5/factory/internal/web"
)

func main() {
	err := run(os.Args[1:])
	if errors.Is(err, pipeline.ErrRestart) {
		// run's deferred cleanup (the database above all) is done.
		err = restart()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "factory:", err)
		os.Exit(1)
	}
}

// startEnv is the environment the process started with, before the
// settings file filled it in, so a restart reads that file afresh too.
var startEnv = os.Environ()

// restart replaces this process with a fresh copy of the same binary,
// arguments and environment, which reads factory.yaml again. No job is
// running; were one, Recover would requeue it.
func restart() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	slog.Info("restarting to apply new settings")
	return syscall.Exec(exe, os.Args, startEnv)
}

// buildVersion is the release this binary was built from, set by the
// release workflow with -ldflags "-X main.buildVersion=v1.2.3"; dev for a
// local build.
var buildVersion = "dev"

func usage() error {
	return fmt.Errorf("usage: factory [-config factory.yaml] [-env factory.env] version|setup|doctor|codex-login|images [--rebuild]|uninstall [--data] [--dry-run]|readiness [-file|-queue] [repo]|serve|run-once|status|automerge [reset <repo>]|tune|groom [repo]|limits clear [harness]|migrate|backup <file>|pull <issue>|last-job|wait-job <after-id> <timeout>|eval [review] [flags]|compare <issue> -skills a,b|mcp-bridge <socket>")
}

func run(args []string) error {
	// The orchestrator's MCP bridge runs inside the job container, where there
	// is no config or database: it only joins stdio to the daemon's socket.
	if len(args) == 2 && args[0] == "mcp-bridge" {
		return mcp.Bridge(args[1], os.Stdin, os.Stdout)
	}
	// The egress proxy runs in its own container, also without either.
	if len(args) > 0 && args[0] == "egress-proxy" {
		return egressProxy(args[1:])
	}
	if len(args) == 1 && args[0] == "version" {
		fmt.Println("factory", buildVersion)
		return nil
	}
	fs := flag.NewFlagSet("factory", flag.ContinueOnError)
	cfgPath := fs.String("config", envOr("FACTORY_CONFIG", "factory.yaml"), "path to factory.yaml")
	envPath := fs.String("env", os.Getenv("FACTORY_ENV_FILE"), "settings file `factory setup` writes (default: factory.env next to the config)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return usage()
	}
	// Now, so codex-login and the rest don't hand on a token the shell exported.
	takeSentryToken()
	if err := firstConfig(*cfgPath); err != nil {
		return err
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	if *envPath == "" {
		*envPath = cfg.Path("factory.env")
	}
	if fs.Arg(0) == "setup" {
		// Setup waits on the terminal, so Ctrl-C ends it at once (see newPrompter).
		return setup(context.Background(), *cfgPath, cfg, *envPath, newPrompter())
	}
	if fs.Arg(0) == "codex-login" {
		return codexLogin(cfg, *envPath, newPrompter())
	}
	if fs.Arg(0) == "images" {
		// Build the job images: the missing ones, or all with --rebuild.
		force := fs.NArg() == 2 && fs.Arg(1) == "--rebuild"
		if fs.NArg() > 2 || (fs.NArg() == 2 && !force) {
			return usage()
		}
		return buildImages(os.Stdout, cfg, force)
	}
	if err := loadEnvFile(*envPath); err != nil {
		return err
	}
	takeSentryToken() // and the one factory.env sets
	if fs.Arg(0) == "doctor" {
		return runDoctor(context.Background(), os.Stdout, *cfgPath, cfg, *envPath)
	}
	if fs.Arg(0) == "uninstall" {
		// Before the database opens, so --data can remove it.
		return uninstall(context.Background(), os.Stdout, os.Stdin, *cfgPath, cfg, *envPath, fs.Args()[1:])
	}
	if cfg.NoRepos() && fs.Arg(0) != "serve" {
		// serve opens the get-started page, which asks for them.
		return fmt.Errorf("%s names no repositories yet: run ./factory setup, or ./factory serve and open the get-started page", *cfgPath)
	}
	dataDir := cfg.Path(cfg.DataDir)
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return err
	}
	st, err := store.Open(filepath.Join(dataDir, "factory.db"))
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch cmd := fs.Arg(0); cmd {
	case "migrate":
		v, err := st.SchemaVersion(ctx)
		fmt.Println("schema version", v)
		return err
	case "backup":
		if fs.NArg() != 2 {
			return usage()
		}
		return st.Backup(ctx, fs.Arg(1))
	case "status":
		return status(ctx, st, cfg)
	case "last-job":
		jobs, err := st.ListJobs(ctx, store.JobFilter{Limit: 1})
		if err != nil {
			return err
		}
		var id int64
		if len(jobs) > 0 {
			id = jobs[0].ID
		}
		fmt.Println(id)
		return nil
	case "limits":
		if fs.NArg() < 2 || fs.NArg() > 3 || fs.Arg(1) != "clear" {
			return usage()
		}
		n, err := st.ClearLimits(ctx, fs.Arg(2))
		if err == nil {
			fmt.Printf("cleared %d usage-limit readings\n", n)
		}
		return err
	case "db-path":
		fmt.Println(filepath.Join(dataDir, "factory.db"))
		return nil
	case "wait-job":
		if fs.NArg() != 3 {
			return usage()
		}
		return waitJob(ctx, st, fs.Arg(1), fs.Arg(2))
	case "pull":
		if fs.NArg() != 2 {
			return usage()
		}
		return pull(ctx, cfg, st, fs.Arg(1))
	case "readiness":
		p, err := build(cfg, st)
		if err != nil {
			return err
		}
		return runReadiness(ctx, cfg, p, fs.Args()[1:])
	case "compare":
		return runCompare(ctx, cfg, st, fs.Args()[1:])
	case "eval":
		return runEval(ctx, cfg, st, fs.Args()[1:])
	case "tune":
		// Queued for the daemon, which runs it on its next pass, however
		// few items were built: you asked for it.
		p := &pipeline.Pipeline{Cfg: cfg, Store: st}
		id, err := p.Tune(ctx, "factory tune", true)
		if err != nil {
			return err
		}
		fmt.Printf("queued tuning run (job %d); its proposal shows on the dashboard's Settings screen\n", id)
		return nil
	case "automerge":
		return automerge(ctx, os.Stdout, cfg, st, fs.Args()[1:])
	case "groom":
		// Queued for the daemon, like tune, whatever grooming.every says.
		if fs.NArg() > 2 {
			return usage()
		}
		return groom(ctx, os.Stdout, cfg, st, fs.Arg(1))
	case "serve", "run-once":
		p, err := build(cfg, st)
		if ns := (needsSetup{}); cmd == "serve" && errors.As(err, &ns) {
			// Not an error to stop on: the dashboard can finish setup.
			return serveSetup(ctx, *cfgPath, cfg, *envPath, ns.err)
		}
		if err != nil {
			return err
		}
		p.ConfigPath, _ = filepath.Abs(*cfgPath)
		if err := p.CheckTunePrompt(); err != nil {
			return err
		}
		if err := p.CheckGroomPrompt(); err != nil {
			return err
		}
		if err := p.CheckSecurityPrompt(); err != nil {
			return err
		}
		if err := p.CheckHuntPrompt(); err != nil {
			return err
		}
		if cmd == "serve" {
			// Missing job images build in the background; jobs that need
			// one wait for it rather than fail.
			builder := &imageBuilder{}
			p.Building = builder.Building
			if err := serveWeb(ctx, *cfgPath, *envPath, cfg, st, p, builder); err != nil {
				return err
			}
			autoBuild(ctx, cfg, builder, p.Notify)
			if cfg.Runner.Kind == "docker" {
				go pruneLoop(ctx, cfg)
			}
			healthCheck(ctx, cfg, p, builder)
			slog.Info("factory serving", "repos", strings.Join(cfg.RepoNames(), ","), "runner", cfg.Runner.Kind, "control_room", "http://"+cfg.Web.Listen)
			return p.Serve(ctx)
		}
		if err := p.Recover(ctx); err != nil {
			return err
		}
		return p.Drain(ctx)
	default:
		return usage()
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// tokens returns GitHub credentials for one repo. App tokens are scoped to
// that repo alone.
func tokens(cfg *config.Config, repo string) (github.TokenSource, error) {
	if repoFor(cfg, repo).GitLab() {
		return gitlabToken()
	}
	if app := os.Getenv("FACTORY_GITHUB_APP_ID"); app != "" {
		key, err := github.LoadAppKey(os.Getenv("FACTORY_GITHUB_APP_KEY"))
		if err != nil {
			return nil, fmt.Errorf("FACTORY_GITHUB_APP_KEY: %w", err)
		}
		// The saved installation is the app's install on the first repo's
		// account. A repo under any other account (repos under several, or
		// one since dropped from the config) has its own looked up instead.
		inst := os.Getenv("FACTORY_GITHUB_INSTALLATION_ID")
		owner, _, _ := strings.Cut(repo, "/")
		for _, r := range append(githubRepos(cfg), repo) {
			if o, _, _ := strings.Cut(r, "/"); !strings.EqualFold(o, owner) {
				inst = ""
			}
		}
		return &github.AppTokens{AppID: app, InstallationID: inst, Key: key, Repo: repo}, nil
	}
	if t := os.Getenv("GITHUB_TOKEN"); t != "" {
		return github.StaticToken(t), nil
	}
	return nil, fmt.Errorf("no GitHub credentials: run `factory setup`, or set FACTORY_GITHUB_APP_ID, FACTORY_GITHUB_INSTALLATION_ID and FACTORY_GITHUB_APP_KEY (or GITHUB_TOKEN for development)")
}

// secrets collects model credentials from the daemon's environment. They are
// injected into job containers per job and never written into a job's files.
func secrets() map[string]map[string]string {
	pick := func(names ...string) map[string]string {
		m := map[string]string{}
		for _, n := range names {
			if v := os.Getenv(n); v != "" {
				m[n] = v
			}
		}
		return m
	}
	codex := pick("OPENAI_API_KEY", "CODEX_API_KEY")
	if v, ok := codex["OPENAI_API_KEY"]; ok && codex["CODEX_API_KEY"] == "" {
		codex["CODEX_API_KEY"] = v
	}
	// On a ChatGPT plan no API key reaches Codex: codex exec would use it
	// instead of the plan.
	switch mode, _ := codexAuth(); mode {
	case codexAccessToken:
		codex = pick("CODEX_ACCESS_TOKEN")
	case codexChatGPT:
		codex = map[string]string{}
	}
	return map[string]map[string]string{
		"claude": pick("ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN"),
		"codex":  codex,
	}
}

// newRunner makes the jobs' runner. owner labels the egress proxies it
// makes; only the daemon's own owner's stale ones are removed at start.
func newRunner(cfg *config.Config, owner string) (runner.Runner, error) {
	if cfg.Runner.Kind != "docker" {
		return runner.Local{}, nil
	}
	d := &runner.Docker{CPUs: cfg.Runner.CPUs, Memory: cfg.Runner.Memory, Network: cfg.Runner.Network, UID: os.Getuid(), GID: os.Getgid()}
	if cfg.EgressAllowlist() {
		if runtime.GOOS != "linux" {
			// The proxy's container runs this binary.
			return nil, errors.New("the egress allowlist needs a Linux host (WSL counts); set runner.egress.mode: open to run jobs with open network access")
		}
		bin, err := os.Executable()
		if err != nil {
			return nil, err
		}
		allow, private := cfg.EgressRules()
		d.Egress = &runner.Egress{Allow: allow, Private: private, Binary: bin, Owner: owner}
	}
	return d, nil
}

// daemonOwner names the daemon of this data directory, for the egress
// proxies it makes.
func daemonOwner(cfg *config.Config) string {
	dir, err := filepath.Abs(cfg.DataDir)
	if err != nil {
		dir = cfg.DataDir
	}
	h := sha256.Sum256([]byte(dir))
	return "daemon-" + hex.EncodeToString(h[:8])
}

// needsSetup is why the factory can't start: the forge credentials setup
// saves are missing or don't load. serve opens the get-started page then.
type needsSetup struct{ err error }

func (e needsSetup) Error() string { return e.err.Error() }
func (e needsSetup) Unwrap() error { return e.err }

func build(cfg *config.Config, st *store.Store) (*pipeline.Pipeline, error) {
	if cfg.NoRepos() {
		return nil, needsSetup{errors.New("no repositories chosen yet")}
	}
	// Fail at start, not on the first job, when credentials are missing.
	for _, repos := range [][]string{githubRepos(cfg), gitlabRepos(cfg)} {
		if len(repos) == 0 {
			continue
		}
		if _, err := tokens(cfg, repos[0]); err != nil {
			return nil, needsSetup{err}
		}
	}
	if cfg.Orchestrator.On && cfg.Runner.Kind == "docker" && runtime.GOOS != "linux" {
		// Its container runs this binary as the MCP bridge.
		slog.Warn("orchestrator disabled: with the docker runner it needs a Linux host")
		cfg.Orchestrator.On = false
	}
	if _, err := codexAuth(); err != nil {
		return nil, err
	}
	r, err := newRunner(cfg, daemonOwner(cfg))
	if err != nil {
		return nil, err
	}
	return &pipeline.Pipeline{
		Cfg:   cfg,
		Store: st,
		ClientsFor: func(repo string) pipeline.RepoClients {
			ts, err := tokens(cfg, repo)
			if err != nil {
				ts = github.StaticToken("") // checked above; fails each call if the env changed
			}
			return pipeline.RepoClients{
				GH:  newForge(cfg, repo, ts),
				Git: repoGit(cfg, repo, ts),
			}
		},
		Runner: r,
		Harnesses: map[string]harness.Harness{
			"claude": harness.NewClaude(r),
			"codex":  newCodex(cfg, r),
		},
		Secrets:    secrets(),
		PlanBilled: planBilled(secrets(), os.Getenv, codexAuthMode()),
		Log:        slog.Default(),
		Notify:     notifier(),
		Sentry:     sentryClient(cfg),
		ReviewEval: reviewEvaluator(cfg),
	}, nil
}

// planHarnesses are the harnesses the routing uses that run on a
// subscription plan, Claude first.
func planHarnesses(cfg *config.Config, billed map[string]bool) []string {
	used := map[string]bool{}
	for _, r := range cfg.Routing {
		used[r.Harness] = true
	}
	var out []string
	for _, h := range []string{"claude", "codex"} {
		if used[h] && billed[h] {
			out = append(out, h)
		}
	}
	return out
}

// noReadingLines are status's lines on each harness on a plan that has
// reported no usage yet, so its dollar spend isn't mistaken for the limit.
func noReadingLines(plans []string, limits []store.Limit) []string {
	var out []string
	for _, h := range plans {
		if slices.ContainsFunc(limits, func(l store.Limit) bool { return l.Harness == h }) {
			continue
		}
		when := "Claude reports usage when it gets close to a limit"
		if h == "codex" {
			when = "Codex reports usage after each job it runs with the Docker runner"
		}
		out = append(out, fmt.Sprintf("usage limits:\t%s runs on a plan; no reading yet (%s). Its dollar figures are API-price estimates.", h, when))
	}
	return out
}

// planBilled names the harnesses on a subscription plan: Claude signed in
// with a subscription token and no API key, which Claude Code would use
// first, nor an auth token or Bedrock or Vertex, all billed by use; and
// Codex signed in to a ChatGPT plan (codexMode is codexAuth's) rather
// than run with an API key.
func planBilled(sec map[string]map[string]string, getenv func(string) string, codexMode string) map[string]bool {
	c := sec["claude"]
	set := func(k string) bool { return c[k] != "" || getenv(k) != "" }
	billed := set("ANTHROPIC_API_KEY") || set("ANTHROPIC_AUTH_TOKEN") || set("CLAUDE_CODE_USE_BEDROCK") || set("CLAUDE_CODE_USE_VERTEX")
	return map[string]bool{
		"claude": set("CLAUDE_CODE_OAUTH_TOKEN") && !billed,
		"codex":  codexMode == codexChatGPT || codexMode == codexAccessToken,
	}
}

// sentryToken is SENTRY_AUTH_TOKEN, taken out of the environment.
var sentryToken string

// takeSentryToken moves SENTRY_AUTH_TOKEN out of the process's
// environment as soon as factory.env is loaded, before any command can
// start, so nothing the factory runs (git, docker, a job or an eval on
// the local runner) inherits it.
func takeSentryToken() {
	if v, ok := os.LookupEnv("SENTRY_AUTH_TOKEN"); ok {
		sentryToken = strings.TrimSpace(v)
		_ = os.Unsetenv("SENTRY_AUTH_TOKEN")
	}
}

// sentryClient reads Sentry with the token; nil without one.
func sentryClient(cfg *config.Config) pipeline.SentryAPI {
	if sentryToken == "" {
		return nil
	}
	return &sentry.Client{BaseURL: cfg.Sources.Sentry.BaseURL, Token: sentryToken}
}

// notifier sends to FACTORY_NOTIFY_URL (ntfy, Slack or Discord), at most
// 20 messages an hour.
func notifier() func(ctx context.Context, msg string) {
	n, err := notify.New(os.Getenv("FACTORY_NOTIFY_URL"))
	if err != nil {
		slog.Warn("notifications disabled", "err", err)
		return nil
	}
	if n == nil {
		return nil
	}
	rl := &notify.RateLimited{Next: n, N: 20, Window: time.Hour}
	return func(ctx context.Context, msg string) {
		if err := rl.Send(ctx, "Factory", msg); err != nil {
			slog.Warn("notify", "err", err)
		}
	}
}

func serveWeb(ctx context.Context, cfgPath, envPath string, cfg *config.Config, st *store.Store, p *pipeline.Pipeline, builder *imageBuilder) error {
	host, _, err := net.SplitHostPort(cfg.Web.Listen)
	if err != nil {
		return fmt.Errorf("web.listen: %w", err)
	}
	srv := &web.Server{Store: st, Actions: p, Repos: cfg.RepoNames(), DailyCap: cfg.Budgets.DailyTotal, AwaitCap: cfg.Budgets.AwaitingHumanMax,
		Hosts: append([]string{host}, cfg.Web.Hosts...), CompareDir: filepath.Join(cfg.Path(cfg.DataDir), "compare")}
	srv.JobsDir = filepath.Join(cfg.Path(cfg.DataDir), "jobs")
	srv.DiskLow = p.DiskLow
	// The setup screen reads the readiness check's reports, and can run it
	// again: on the daemon's context, so it stops when the daemon does.
	srv.ReadinessDir, srv.IntakeLabel = readinessDir(cfg), cfg.Intake.Label
	// A check a crash or restart cut off left its folder behind.
	sweepReadiness(cfg.Path(cfg.DataDir))
	// Readiness runs count as busy for removing the factory, and none
	// starts while it is removed.
	var readinessRuns atomic.Int32
	var removing atomic.Bool
	readinessRun := func() (func(), error) {
		// Counted first, so a removal that starts now sees it.
		readinessRuns.Add(1)
		if removing.Load() {
			readinessRuns.Add(-1)
			return nil, errors.New("the factory is being removed")
		}
		return func() { readinessRuns.Add(-1) }, nil
	}
	srv.CheckReadiness = func(_ context.Context, repo string) error {
		done, err := readinessRun()
		if err != nil {
			return err
		}
		defer done()
		return recheckReadiness(ctx, cfg, p, repo)
	}
	srv.FileReadiness = func(_ context.Context, repo string, queue bool) (string, error) {
		done, err := readinessRun()
		if err != nil {
			return "", err
		}
		defer done()
		return fileReadiness(ctx, cfg, p, repo, queue)
	}
	// A lint baseline runs the repo's install and linter like a check, in
	// containers named apart from the daemon's jobs.
	srv.CanBaseline = p.CanBaseline
	srv.LintBaseline = func(_ context.Context, repo string) (string, error) {
		done, err := readinessRun()
		if err != nil {
			return "", err
		}
		defer done()
		pl := &pipeline.Pipeline{Cfg: cfg, Runner: p.Runner, JobPrefix: readinessPrefix(), ClientsFor: p.ClientsFor}
		return pl.LintBaseline(ctx, repo)
	}
	go relinkSaved(ctx, cfg, p)
	srv.Plans, srv.PauseAtUsage = planHarnesses(cfg, p.PlanBilled), cfg.PauseAtUsage()
	r := cfg.Routing["orchestrate"]
	srv.Chat = p
	srv.Orchestrator = web.OrchestratorInfo{On: cfg.Orchestrator.On, Harness: r.Harness, Model: r.Model, Every: cfg.Orchestrator.Interval.String(),
		RunBudget: cfg.Orchestrator.RunBudget, DailyBudget: cfg.Orchestrator.DailyBudget}
	// The Settings screen edits the config file; saved changes apply when
	// the daemon restarts, which it does once running jobs finish.
	srv.ConfigPath, _ = filepath.Abs(cfgPath)
	srv.Listen = cfg.Web.Listen
	srv.Restarter = p
	srv.SetTrust = func(_ context.Context, repo string, a *config.AutoMerge) error {
		return config.SetRepoAutoMerge(srv.ConfigPath, repo, a)
	}
	srv.Breakers = func(ctx context.Context) []web.APIBreaker {
		var out []web.APIBreaker
		for _, b := range p.Breakers(ctx) {
			out = append(out, web.APIBreaker{Repo: b.Repo, On: b.On, Tripped: b.Tripped, TrippedAt: b.TrippedAt, Reason: pipeline.SafeText(b.Reason, 500),
				Strikes: b.Strikes, Threshold: b.Threshold, WindowHours: int(b.Window.Hours()), CoolOffHours: int(b.CoolOff.Hours())})
		}
		return out
	}
	srv.Choices = func(ctx context.Context, jobID int64) []web.APIChoice {
		var out []web.APIChoice
		for _, c := range p.Choices(ctx, jobID) {
			out = append(out, web.APIChoice{Action: c.Action, Label: c.Label, Model: c.Model, Explain: c.Explain, Available: c.Available, Why: c.Why})
		}
		return out
	}
	srv.SteerUnsupported = pipeline.SteerUnsupported
	if b, err := os.ReadFile(cfgPath); err == nil {
		srv.StartedConfig = web.ConfigVersion(b)
	}
	// A repo since dropped from the config still links to its host.
	links := func(repo string) config.Repo {
		h, n := config.SplitRepo("", repo)
		return config.Repo{Name: n, Host: h}
	}
	srv.IssueURL = func(repo string, n int) string { return links(repo).IssueURL(n) }
	srv.PRURL = func(repo string, n int) string { return links(repo).PRURL(n) }
	srv.Issue = web.CachedIssue(func(ctx context.Context, repo string, n int) (*github.Issue, error) {
		return p.GitHubFor(repo).GetIssue(ctx, n)
	}, 5*time.Minute)
	srv.Checks = web.CachedChecks(func(ctx context.Context, repo, sha string) ([]github.CheckRun, error) {
		c, ok := p.GitHubFor(repo).(forge)
		if !ok {
			return nil, nil
		}
		return c.CheckRuns(ctx, sha)
	}, 30*time.Second)
	if cfg.Runner.Kind == "docker" {
		srv.Attach = web.TTYDAttach(st, host)
		srv.Exec = web.DockerExec(st)
	}
	// The get-started page, for changing what setup saved; Finish
	// restarts the factory to apply it.
	wz, err := newWizard(ctx, cfgPath, envPath, false, p.RequestRestart, builder)
	if err != nil {
		return err
	}
	srv.Wizard = wz
	// A restart waits for an image build, a sign-in or a readiness
	// check, as for a job.
	p.Busy = func() string {
		if w := wz.waitingOn(); w != "" {
			return w
		}
		if readinessRuns.Load() > 0 {
			return "the readiness check"
		}
		return ""
	}
	// Settings' "Remove the factory" waits for jobs, readiness checks and
	// compare runs (in any process, by their run locks) and that work too,
	// and holds off new work while it removes.
	un := newUninstaller(cfg, cfgPath, envPath, true, func(ctx context.Context) string {
		if runs := liveRuns(cfg.Path(cfg.DataDir)); len(runs) > 0 {
			return "Not now: " + joinAnd(runs) + " is running. Wait for it to finish, then try again."
		}
		n := p.Running()
		if m, err := st.CountJobs(ctx, state.Running, ""); err == nil && m > n {
			n = m
		}
		if n > 0 {
			return fmt.Sprintf("Not now: %d job(s) are running. Wait for them to finish, or cancel them, then try again.", n)
		}
		if readinessRuns.Load() > 0 {
			return "Not now: a readiness check is running. Wait for it to finish, then try again."
		}
		if w := wz.waitingOn(); w != "" {
			return "Not now: " + w + " is running. Wait for it to finish, then try again."
		}
		return ""
	})
	un.hold = func() func() {
		release := p.Hold()
		removing.Store(true)
		return func() { removing.Store(false); release() }
	}
	srv.Uninstaller = un
	_, err = listenWeb(ctx, cfg, srv)
	return err
}

// webLogin is the control room's password, if FACTORY_WEB_PASSWORD sets
// one. Without one it may only listen on this machine or a tailnet.
func webLogin(cfg *config.Config) (*web.Login, error) {
	if pw := os.Getenv("FACTORY_WEB_PASSWORD"); pw != "" {
		if len(pw) < web.MinPassword {
			return nil, fmt.Errorf("FACTORY_WEB_PASSWORD is %d characters; use at least %d", len(pw), web.MinPassword)
		}
		secret, err := web.LoadSecret(filepath.Join(cfg.Path(cfg.DataDir), "web.secret"))
		if err != nil {
			return nil, err
		}
		return &web.Login{Password: pw, Secret: secret}, nil
	} else if web.NeedsLogin(cfg.Web.Listen) {
		return nil, fmt.Errorf("web.listen %s reaches beyond this machine and a tailnet: set FACTORY_WEB_PASSWORD in factory.env so the control room asks for it", cfg.Web.Listen)
	}
	return nil, nil
}

// listenWeb serves srv on web.listen, behind its password if one is set,
// until ctx ends.
func listenWeb(ctx context.Context, cfg *config.Config, srv *web.Server) (*http.Server, error) {
	login, err := webLogin(cfg)
	if err != nil {
		return nil, err
	}
	srv.Login = login
	h, err := srv.Handler()
	if err != nil {
		return nil, err
	}
	l, err := net.Listen("tcp", cfg.Web.Listen)
	if err != nil {
		return nil, err
	}
	hs := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		_ = hs.Close()
	}()
	go func() {
		if err := hs.Serve(l); err != nil && err != http.ErrServerClosed {
			slog.Error("control room", "err", err)
		}
	}()
	return hs, nil
}

// serveSetup runs the dashboard with only its get-started page, for a
// factory that can't start yet (why). Finish there restarts the factory,
// which reads the new settings.
func serveSetup(ctx context.Context, cfgPath string, cfg *config.Config, envPath string, why error) error {
	wz, err := newWizard(ctx, cfgPath, envPath, true, nil, nil)
	if err != nil {
		return err
	}
	host, _, err := net.SplitHostPort(cfg.Web.Listen)
	if err != nil {
		return fmt.Errorf("web.listen: %w", err)
	}
	srv := &web.Server{Wizard: wz, SetupMode: true, SetupReason: why.Error(), Listen: cfg.Web.Listen, Hosts: append([]string{host}, cfg.Web.Hosts...)}
	srv.ConfigPath, _ = filepath.Abs(cfgPath)
	hs, err := listenWeb(ctx, cfg, srv)
	if err != nil {
		return err
	}
	slog.Warn("the factory isn't set up yet", "why", why)
	fmt.Printf("\nThe factory isn't set up yet: %v\nOpen http://%s/app/get-started to finish setup.\n\n", why, cfg.Web.Listen)
	select {
	case <-ctx.Done():
		return nil
	case <-wz.done:
	}
	// Let the Finish press get its answer before the process is replaced.
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = hs.Shutdown(sctx)
	slog.Info("setup finished; starting the factory")
	return pipeline.ErrRestart
}

func status(ctx context.Context, st *store.Store, cfg *config.Config) error {
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	defer w.Flush()
	items, err := st.ListItems(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintln(w, "ITEM\tISSUE\tSTAGE\tSIZE\tRISK\tESTIMATE\tPR\tTITLE")
	for _, it := range items {
		pr := "-"
		if it.PRNumber > 0 {
			pr = fmt.Sprintf("#%d", it.PRNumber)
		}
		if prs, _ := st.ItemPRs(ctx, it.ID); len(prs) > 0 {
			refs := make([]string, len(prs))
			for i, p := range prs {
				refs[i] = fmt.Sprintf("%s#%d", config.ShortName(p.Repo), p.Number)
			}
			pr = strings.Join(refs, ",")
		}
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", it.ID, issueRef(cfg, it.Repo, it.IssueNumber), it.Stage, dash(it.Complexity), dash(it.Risk), estimateCell(ctx, st, it.ID), pr, it.Title)
	}
	fmt.Fprintln(w)
	for _, l := range waitingLines(ctx, st, cfg, items) {
		fmt.Fprintln(w, l)
	}
	for _, s := range []state.JobState{state.Queued, state.Running} {
		n, _ := st.CountJobs(ctx, s, "")
		fmt.Fprintf(w, "jobs %s:\t%d\n", s, n)
	}
	spent, _ := st.SpendOn(ctx, store.Today())
	fmt.Fprintf(w, "spend today:\t$%.2f of $%.2f\n", spent, cfg.Budgets.DailyTotal)
	limits, _ := st.Limits(ctx)
	if len(limits) > 0 {
		fmt.Fprintf(w, "usage limits:\t%s\n", store.FormatLimits(limits))
	}
	// By this shell's settings, as codex sign-in below.
	for _, l := range noReadingLines(planHarnesses(cfg, planBilled(secrets(), os.Getenv, codexAuthMode())), limits) {
		fmt.Fprintln(w, l)
	}
	if rep, err := metrics.Compute(ctx, st); err == nil {
		fmt.Fprintf(w, "review:\t%s\n", reviewLine(rep))
	}
	// From this command's own settings, which are the daemon's unless its
	// environment overrides factory.env.
	if s := codexSignIn(cfg); s != "" {
		fmt.Fprintf(w, "codex sign-in (by this shell's settings):\t%s\n", s)
	}
	for _, l := range readinessLines(ctx, st, cfg) {
		fmt.Fprintln(w, l)
	}
	for _, l := range mainHealthLines(ctx, st, cfg) {
		fmt.Fprintln(w, l)
	}
	for _, l := range sourceLines(ctx, st, cfg) {
		fmt.Fprintln(w, l)
	}
	for _, l := range groomLines(ctx, st, cfg) {
		fmt.Fprintln(w, l)
	}
	for _, l := range huntLines(ctx, st, cfg) {
		fmt.Fprintln(w, l)
	}
	for _, l := range autoMergeLines(ctx, st, cfg) {
		fmt.Fprintln(w, l)
	}
	jobs, err := st.ListJobs(ctx, store.JobFilter{Limit: 10})
	if err != nil {
		return err
	}
	if len(jobs) > 0 {
		fmt.Fprintln(w, "\nJOB\tKIND\tSTATE\tHARNESS\tMODEL\tSKILLS\tATTEMPT\tCOST\tERROR")
		for _, j := range jobs {
			fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\t%d\t$%.2f\t%s\n", j.ID, j.Kind, j.State, j.Harness, j.ModelLabel(), dash(j.Skillset), j.Attempt, j.CostUSD, trunc(j.Error, 60))
		}
	}
	return nil
}

// reviewLine is status's line on how review is doing: escapes, the
// precision of its blocking findings and the rounds an item takes.
func reviewLine(r *metrics.Report) string {
	pct := func(v float64) string {
		if v < 0 {
			return "-"
		}
		return fmt.Sprintf("%.0f%%", v*100)
	}
	rounds := "-"
	if r.RoundsToPass >= 0 {
		rounds = fmt.Sprintf("%.1f", r.RoundsToPass)
	}
	return fmt.Sprintf("escape rate %s, precision %s, verifier refuted %s, rounds to pass %s", pct(r.ReviewEscapes), pct(r.ReviewPrecision), pct(r.VerifierRefuted), rounds)
}

// estimateCell is an item's cost estimate for status: its range, and
// whether it waits for your OK.
func estimateCell(ctx context.Context, st *store.Store, itemID int64) string {
	e, err := st.ItemEstimate(ctx, itemID)
	if err != nil || e == nil {
		return "-"
	}
	switch e.Status {
	case store.EstimateWaiting:
		return e.Range() + " (needs your OK)"
	case store.EstimateParked:
		return e.Range() + " (parked; needs an OK)"
	case store.EstimateApproved:
		return e.Range() + " (approved)"
	}
	return e.Range()
}

// waitingLines are status's lines on each item that waits for your OK on
// its plan or its cost: what to do, and where.
func waitingLines(ctx context.Context, st *store.Store, cfg *config.Config, items []*store.WorkItem) []string {
	var out []string
	for _, it := range items {
		if it.Stage != state.Blocked {
			continue
		}
		e, _ := st.ItemEstimate(ctx, it.ID)
		cost := e != nil && e.Status == store.EstimateWaiting
		plan := st.PlanWaiting(ctx, it)
		if !cost && !plan {
			continue
		}
		rc, ok := cfg.RepoConf(it.Repo)
		if !ok {
			h, n := config.SplitRepo("", it.Repo)
			rc = config.Repo{Name: n, Host: h}
		}
		issue := rc.IssueURL(it.IssueNumber)
		page := fmt.Sprintf("/app/items/%d", it.ID)
		ref := issueRef(cfg, it.Repo, it.IssueNumber)
		var line string
		switch {
		case plan:
			what := "the plan"
			if cost {
				what = fmt.Sprintf("the plan and the cost (est. %s, over the $%.2f limit)", e.Range(), e.ThresholdUSD)
			}
			line = fmt.Sprintf("%s needs your OK on %s.\t1. Read the plan on the issue, %s, or on its dashboard page, %s. "+
				"2. To build it, press Build it on that page, or add the %s label on the issue. "+
				"3. For another plan, press Ask for another plan on that page, or comment on the issue what to change, then add the %s label.",
				ref, what, issue, page, pipeline.LabelGo, pipeline.LabelReplan)
		default:
			line = fmt.Sprintf("%s needs your OK on the cost (est. %s, over the $%.2f limit).\tTo build it, press Approve estimate on its dashboard page, %s, or add the %s label on the issue, %s.",
				ref, e.Range(), e.ThresholdUSD, page, pipeline.LabelGo, issue)
		}
		out = append(out, line)
	}
	return out
}

// mainHealthLines are status's lines on each repo's base branch CI, as
// main health last read it; none while it's off and never ran.
func mainHealthLines(ctx context.Context, st *store.Store, cfg *config.Config) []string {
	hs, _ := st.RepoHealths(ctx)
	if !cfg.MainHealth.Enabled && len(hs) == 0 {
		return nil
	}
	by := map[string]store.RepoHealth{}
	for _, h := range hs {
		by[strings.ToLower(h.Repo)] = h
	}
	issues, _ := st.HealthIssues(ctx, "")
	var out []string
	for _, repo := range cfg.RepoNames() {
		h, ok := by[strings.ToLower(repo)]
		if !ok {
			out = append(out, fmt.Sprintf("main %s:\tnot read yet", repo))
			continue
		}
		// Check names and Detail come from CI: no escapes reach the terminal.
		line := fmt.Sprintf("main %s:\t%s on %s", repo, strings.ReplaceAll(h.State, "_", " "), pipeline.SafeText(h.Branch, 100))
		if h.HeadSHA != "" {
			line += " at " + pipeline.SafeText(h.HeadSHA[:min(7, len(h.HeadSHA))], 7)
		}
		var checks []string
		for _, is := range issues {
			if !strings.EqualFold(is.Repo, repo) || is.State != store.HealthRed && is.State != store.HealthFlaky {
				continue // only what's failing now
			}
			c := pipeline.SafeName(is.Check) + " (" + is.State + ", not filed yet)"
			if is.IssueNumber > 0 {
				c = fmt.Sprintf("%s (%s, %s)", pipeline.SafeName(is.Check), is.State, issueRef(cfg, repo, is.IssueNumber))
			}
			checks = append(checks, c)
		}
		switch {
		case len(checks) > 0:
			line += ": " + strings.Join(checks, ", ")
		case h.Detail != "":
			line += ": " + pipeline.SafeText(h.Detail, 200)
		}
		if !cfg.MainHealth.Enabled {
			line += " (main_health is off)"
		}
		out = append(out, line)
	}
	return out
}

// sourceLines are status's lines on each source that's on, or that ran:
// for each of its repos, its last read and the issues it has open.
func sourceLines(ctx context.Context, st *store.Store, cfg *config.Config) []string {
	sts, _ := st.SourceStates(ctx)
	by := map[string]store.SourceState{}
	for _, s := range sts {
		by[s.Source+" "+strings.ToLower(s.Repo)] = s
	}
	issues, _ := st.SourceIssues(ctx, "", "")
	p := &pipeline.Pipeline{Cfg: cfg}
	var out []string
	for _, src := range pipeline.SourceNames {
		on := p.SourceEnabled(src)
		repos := p.SourceRepos(src)
		if !on {
			repos = nil
			for _, s := range sts {
				if s.Source == src {
					repos = append(repos, s.Repo)
				}
			}
		}
		for _, repo := range repos {
			s, ok := by[src+" "+strings.ToLower(repo)]
			if !ok {
				out = append(out, fmt.Sprintf("%s %s:\tnot read yet", src, repo))
				continue
			}
			line := fmt.Sprintf("%s %s:\tread %s", src, repo, s.PolledAt)
			if s.State == store.SourceUnreadable {
				line += ", can't read it: " + s.Detail
			} else {
				line += fmt.Sprintf(": %d found", s.Found)
				if s.Detail != "" {
					line += " (" + s.Detail + ")"
				}
			}
			var filed []string
			for _, is := range issues {
				if is.Source == src && strings.EqualFold(is.Repo, repo) {
					filed = append(filed, issueRef(cfg, repo, is.IssueNumber))
				}
			}
			if len(filed) > 0 {
				line += "; issues " + strings.Join(filed, ", ")
			}
			if !on {
				line += " (sources." + src + " is off)"
			}
			out = append(out, line)
		}
	}
	return out
}

// parseIssue reads an issue as 12, #12 or owner/name#12. A bare number
// means the only repo; with several, the repo must be named.
func parseIssue(cfg *config.Config, arg string) (string, int, error) {
	repo, num, named := strings.Cut(arg, "#")
	if !named {
		repo, num = "", arg
	}
	n, err := strconv.Atoi(num)
	if err != nil || n <= 0 {
		return "", 0, fmt.Errorf("%q is not an issue number (write 12, #12 or owner/name#12)", arg)
	}
	if repo == "" {
		if len(cfg.Repos) > 1 {
			return "", 0, fmt.Errorf("the factory works on several repos; name one, as in %s#%d", cfg.Repos[0].Name, n)
		}
		return cfg.Repos[0].Name, n, nil
	}
	if r, ok := cfg.RepoConf(repo); ok {
		repo = r.Name
	}
	return repo, n, nil
}

// issueRef names an issue for people: #12, or owner/name#12 with several repos.
func issueRef(cfg *config.Config, repo string, n int) string {
	if len(cfg.Repos) > 1 {
		return fmt.Sprintf("%s#%d", repo, n)
	}
	return fmt.Sprintf("#%d", n)
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func trunc(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// runEval runs the replay set against a candidate config and, with a
// baseline, rejects a candidate that passes less or costs more per pass.
// `eval review` runs the reviewer cases instead.
func runEval(ctx context.Context, cfg *config.Config, st *store.Store, args []string) error {
	if len(args) > 0 && args[0] == "capture" {
		return runCapture(ctx, cfg, st, args[1:])
	}
	reviewer := len(args) > 0 && args[0] == "review"
	if reviewer {
		args = args[1:]
	}
	if _, err := codexAuth(); err != nil {
		return err // as build checks it for the daemon
	}
	fs := flag.NewFlagSet("eval", flag.ContinueOnError)
	candidate := fs.String("config", "", "candidate factory.yaml (routing, prompts, skills) to evaluate; default the main config")
	set := fs.String("set", "evals", "directory of replay cases")
	baseline := fs.String("baseline", "", "report JSON of the current config to compare against")
	out := fs.String("out", "", "write the report JSON here")
	parallel := fs.Int("parallel", 1, "cases to run at once")
	tolerance := fs.Float64("tolerance", -1, "eval review: how far recall may fall or false positives rise, as a share; default tuning.review_eval_tolerance")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *candidate != "" {
		c, err := config.Load(*candidate)
		if err != nil {
			return err
		}
		cfg = c
	}
	if reviewer {
		cases, err := eval.LoadReview(cfg.Path(*set))
		if err != nil {
			return err
		}
		if len(cases) == 0 {
			return fmt.Errorf("no reviewer cases in %s; `factory eval capture -review PR` writes some", *set)
		}
		rep, err := runReviewEval(ctx, cfg, cases, *parallel, 0)
		if err != nil {
			return err
		}
		rep.Config = *candidate
		fmt.Print(rep.Table())
		if *tolerance < 0 {
			*tolerance = cfg.Tuning.ReviewTolerance()
		}
		return report(rep, *out, *baseline, func(b []byte) ([]string, error) {
			var base eval.ReviewReport
			if err := json.Unmarshal(b, &base); err != nil {
				return nil, err
			}
			return eval.CompareReview(rep, &base, *tolerance), nil
		})
	}
	cases, err := eval.Load(cfg.Path(*set))
	if err != nil {
		return err
	}
	if len(cases) == 0 {
		return fmt.Errorf("no cases in %s", *set)
	}
	o, done, err := evalOptions(cfg, *parallel)
	if err != nil {
		return err
	}
	defer done()
	rep, err := eval.Run(ctx, o, cases)
	if err != nil {
		return err
	}
	rep.Config = *candidate
	fmt.Print(rep.Table())
	return report(rep, *out, *baseline, func(b []byte) ([]string, error) {
		var base eval.Report
		if err := json.Unmarshal(b, &base); err != nil {
			return nil, err
		}
		return eval.Compare(rep, &base), nil
	})
}

// report writes rep to out, when set, and with a baseline file fails
// with why compare finds it worse.
func report(rep any, out, baseline string, compare func([]byte) ([]string, error)) error {
	if out != "" {
		b, _ := json.MarshalIndent(rep, "", "  ")
		if err := os.WriteFile(out, b, 0o644); err != nil {
			return err
		}
	}
	if baseline == "" {
		return nil
	}
	b, err := os.ReadFile(baseline)
	if err != nil {
		return err
	}
	why, err := compare(b)
	if err != nil {
		return fmt.Errorf("baseline: %w", err)
	}
	if len(why) > 0 {
		return fmt.Errorf("rejected: %s", strings.Join(why, "; "))
	}
	fmt.Println("accepted: no worse than the baseline")
	return nil
}

// evalOptions wires an eval run: the runner, the harnesses, each repo's
// credentials, and scratch space, which done removes.
func evalOptions(cfg *config.Config, parallel int) (eval.Options, func(), error) {
	ts, err := tokens(cfg, cfg.RepoNames()[0])
	if err != nil {
		slog.Warn("no GitHub credentials; cases must name a local or public repo", "err", err)
		ts = nil
	}
	tokensFor := func(repo string) github.TokenSource {
		// Each repo's own host's credentials, whichever are set up.
		t, err := tokens(cfg, repo)
		if err != nil {
			return nil
		}
		return t
	}
	r, err := newRunner(cfg, "")
	if err != nil {
		return eval.Options{}, nil, err
	}
	if err := os.MkdirAll(cfg.Path(cfg.DataDir), 0o755); err != nil {
		return eval.Options{}, nil, err
	}
	work, err := os.MkdirTemp(cfg.Path(cfg.DataDir), "eval-")
	if err != nil {
		return eval.Options{}, nil, err
	}
	return eval.Options{
		Cfg: cfg, Runner: r, Secrets: secrets(), Tokens: ts, TokensFor: tokensFor, WorkDir: work, Parallel: parallel,
		Harnesses: map[string]harness.Harness{"claude": harness.NewClaude(r), "codex": newCodex(cfg, r)},
	}, func() { _ = os.RemoveAll(work) }, nil
}

// runReviewEval runs reviewer cases on cfg.
func runReviewEval(ctx context.Context, cfg *config.Config, cases []eval.ReviewCase, parallel int, capUSD float64) (*eval.ReviewReport, error) {
	o, done, err := evalOptions(cfg, parallel)
	if err != nil {
		return nil, err
	}
	defer done()
	o.MaxCostUSD = capUSD
	return eval.RunReview(ctx, o, cases)
}

// reviewEvaluator is the daemon's pipeline.ReviewEval: at most
// tuning.review_eval_cases reviewer cases of tuning.review_eval_set (see
// eval.Sample), read from the daemon's own config's directory, on the
// config given, one at a time within capUSD.
func reviewEvaluator(daemon *config.Config) pipeline.ReviewEvaluator {
	return func(ctx context.Context, cfg *config.Config, capUSD float64) (pipeline.ReviewScore, error) {
		cases, err := eval.LoadReview(daemon.Path(daemon.Tuning.ReviewEvalSet))
		if err != nil {
			return pipeline.ReviewScore{}, err
		}
		if len(cases) == 0 {
			return pipeline.ReviewScore{}, fmt.Errorf("no reviewer cases in %s", daemon.Tuning.ReviewEvalSet)
		}
		cases = eval.Sample(cases, daemon.Tuning.ReviewEvalCases)
		cfg.DataDir = daemon.DataDir
		rep, err := runReviewEval(ctx, cfg, cases, 1, capUSD)
		if rep == nil {
			return pipeline.ReviewScore{}, err
		}
		return rep.ReviewScore, err
	}
}

// pull hands an item over for local work: it fetches the item's branch into
// the git repository in the current directory and copies the item's job
// transcripts (and Claude sessions, when kept) next to it.
// remoteIs reports whether a git remote URL (https or ssh) points at the
// GitHub repo owner/name, or the GitLab project host/group/project.
func remoteIs(url, repo string) bool {
	u := strings.ToLower(strings.TrimSuffix(strings.TrimSuffix(url, "/"), ".git"))
	r := strings.ToLower(repo)
	// A GitLab repo's name starts with its host: gitlab.com/group/project.
	// Its URL may name a port: ssh://git@host:2222/group/project.
	if host, path, _ := strings.Cut(r, "/"); strings.Contains(host, ".") && strings.Contains(path, "/") {
		re := regexp.MustCompile(`(//|@)` + regexp.QuoteMeta(host) + `(:[0-9]+)?/` + regexp.QuoteMeta(path) + `$`)
		return re.MatchString(u) || strings.HasSuffix(u, "@"+host+":"+path)
	}
	return strings.HasSuffix(u, "/"+r) || strings.HasSuffix(u, ":"+r)
}

func pull(ctx context.Context, cfg *config.Config, st *store.Store, arg string) error {
	repo, n, err := parseIssue(cfg, arg)
	if err != nil {
		return fmt.Errorf("pull: %w", err)
	}
	it, err := st.ItemByIssue(ctx, repo, n)
	if err != nil {
		return fmt.Errorf("no work item for issue %s", issueRef(cfg, repo, n))
	}
	branch := it.Branch
	if branch == "" {
		branch = fmt.Sprintf("factory/issue-%d", n)
	}
	// The user's own git and credentials: this runs in their checkout,
	// which with several repos must be the checkout of this one.
	if len(cfg.Repos) > 1 {
		out, err := exec.CommandContext(ctx, "git", "remote", "get-url", "origin").Output()
		if err != nil {
			return fmt.Errorf("pull: reading this checkout's origin: %w", err)
		}
		if !remoteIs(strings.TrimSpace(string(out)), repo) {
			return fmt.Errorf("pull: this checkout's origin is %s, not %s; run it in a checkout of %s", strings.TrimSpace(string(out)), repo, repo)
		}
	}
	fetch := exec.CommandContext(ctx, "git", "fetch", "origin", "+refs/heads/"+branch+":refs/heads/"+branch)
	fetch.Stdout, fetch.Stderr = os.Stdout, os.Stderr
	if err := fetch.Run(); err != nil {
		return fmt.Errorf("git fetch %s: %w", branch, err)
	}
	dst := filepath.Join(".factory", fmt.Sprintf("issue-%d", n))
	if len(cfg.Repos) > 1 {
		dst = filepath.Join(".factory", strings.ReplaceAll(repo, "/", "__")+fmt.Sprintf("-issue-%d", n))
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	jobs, err := st.ListJobs(ctx, store.JobFilter{ItemID: it.ID})
	if err != nil {
		return err
	}
	var lastSession string
	for i := len(jobs) - 1; i >= 0; i-- {
		j := jobs[i]
		if j.LogPath == "" {
			continue
		}
		b, err := os.ReadFile(j.LogPath)
		if err != nil {
			continue
		}
		if err := os.WriteFile(filepath.Join(dst, fmt.Sprintf("job-%d-%s.jsonl", j.ID, j.Kind)), b, 0o600); err != nil {
			return err
		}
		if s, err := os.ReadFile(filepath.Join(filepath.Dir(j.LogPath), "session")); err == nil {
			lastSession = strings.TrimSpace(string(s))
		}
	}
	sessions := filepath.Join(cfg.Path(cfg.DataDir), "sessions", fmt.Sprintf("item-%d", it.ID))
	if _, err := os.Stat(sessions); err == nil {
		if err := exec.CommandContext(ctx, "cp", "-R", sessions, filepath.Join(dst, "claude-projects")).Run(); err != nil {
			return err
		}
	}
	fmt.Printf("Branch %s fetched; %d job transcripts in %s\n", branch, len(jobs), dst)
	if lastSession != "" {
		fmt.Printf("Last Claude session: %s (its files are in %s/claude-projects; copy them under ~/.claude/projects to `claude --resume` it)\n", lastSession, dst)
	}
	return nil
}

// waitJob waits for the first job after a given id to finish, for
// deploy/upgrade.sh. It fails when that job failed before its agent did
// any work (cost 0), which points at the daemon or the image rather than
// at the task. Nothing finishing in time is not a failure.
// waitJobEvery is how often waitJob looks again; tests shorten it.
var waitJobEvery = 10 * time.Second

func waitJob(ctx context.Context, st *store.Store, afterArg, timeoutArg string) error {
	after, err := strconv.ParseInt(afterArg, 10, 64)
	if err != nil {
		return err
	}
	timeout, err := time.ParseDuration(timeoutArg)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		jobs, err := st.ListJobs(ctx, store.JobFilter{Limit: 50})
		if err != nil {
			return err
		}
		for i := len(jobs) - 1; i >= 0; i-- {
			j := jobs[i]
			if j.ID <= after || !j.State.Terminal() {
				continue
			}
			if (j.State == state.Failed || j.State == state.TimedOut) && j.CostUSD == 0 {
				return fmt.Errorf("job %d (%s) %s before doing any work: %s", j.ID, j.Kind, j.State, j.Error)
			}
			fmt.Printf("job %d (%s) finished: %s\n", j.ID, j.Kind, j.State)
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(waitJobEvery):
		}
	}
	// An idle factory runs no job; upgrade.sh then relies on the daemon
	// still being up.
	fmt.Printf("no job finished within %s\n", timeout)
	return nil
}

// runCompare runs one issue once per skill set and writes a side-by-side
// report under data/compare/.
func runCompare(ctx context.Context, cfg *config.Config, st *store.Store, args []string) error {
	fs := flag.NewFlagSet("compare", flag.ContinueOnError)
	skills := fs.String("skills", "", "comma-separated skill sets to compare (\"none\" for no skills)")
	complexity := fs.String("complexity", "", "XS, S, M or L (default: the item's triage, else M)")
	noPush := fs.Bool("no-push", false, "keep the arms' branches local instead of pushing factory/compare-* branches")
	if len(args) == 0 {
		return fmt.Errorf("usage: factory compare <issue> -skills a,b")
	}
	repo, issue, err := parseIssue(cfg, args[0])
	if err != nil {
		return fmt.Errorf("compare: %w", err)
	}
	if _, ok := cfg.RepoConf(repo); !ok {
		return fmt.Errorf("compare: %s is not in factory.yaml", repo)
	}
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	risk := "low"
	if it, err := st.ItemByIssue(ctx, repo, issue); err == nil {
		if *complexity == "" {
			*complexity = it.Complexity
		}
		if it.Risk != "" {
			risk = it.Risk
		}
	}
	if *complexity == "" {
		*complexity = "M"
	}
	p, err := build(cfg, st)
	if err != nil {
		return err
	}
	// The arms share what is left of today's budget and their spend counts
	// against it, so a comparison cannot run past the daily cap.
	spent, err := st.SpendOn(ctx, store.Today())
	if err != nil {
		return err
	}
	left := cfg.Budgets.DailyTotal - spent
	if cfg.Budgets.DailyTotal > 0 && left <= 0 {
		return fmt.Errorf("today's budget is spent ($%.2f of $%.2f)", spent, cfg.Budgets.DailyTotal)
	}
	id := fmt.Sprintf("%d-%s", issue, time.Now().UTC().Format("20060102T150405Z"))
	if len(cfg.Repos) > 1 {
		id = strings.ReplaceAll(repo, "/", "__") + "-" + id
	}
	dir := filepath.Join(cfg.Path(cfg.DataDir), "compare", id)
	// Removing the factory waits while this runs: it uses a database of
	// its own, which the main one never sees.
	unlock, err := runLock(dir)
	if err != nil {
		return err
	}
	defer unlock()
	rep, err := compare.Run(ctx, compare.Options{
		Cfg: cfg, Repo: repo, Harnesses: p.Harnesses, Runner: p.Runner, Secrets: p.Secrets, GH: p.GitHubFor(repo), Git: p.GitFor(repo),
		Dir: dir, Push: !*noPush, Budget: max(left, 0),
	}, issue, strings.Split(*skills, ","), *complexity, risk)
	if rep != nil {
		var cost float64
		for _, a := range rep.Arms {
			cost += a.CostUSD
		}
		if cost > 0 {
			if serr := st.AddSpend(context.WithoutCancel(ctx), cost, "human", fmt.Sprintf("compare %s: %s", issueRef(cfg, repo, issue), *skills)); serr != nil {
				slog.Error("recording compare spend", "err", serr)
			}
		}
	}
	if err != nil {
		return err
	}
	if err := rep.Save(dir); err != nil {
		return err
	}
	_ = st.AddEvent(ctx, store.Event{Actor: "human", Type: "compare_done", Reason: fmt.Sprintf("%s: %s", issueRef(cfg, repo, issue), *skills), Data: dir})
	for _, a := range rep.Arms {
		fmt.Printf("%-16s %-16s review=%-9s cost=$%.2f time=%.0fs %s\n", a.Skillset, a.Stage, a.Review, a.CostUSD, a.Seconds, a.Error)
	}
	fmt.Printf("\nReport: %s (also in the control room under Compare)\n", filepath.Join(dir, "report.md"))
	return nil
}

// firstConfig makes the config from factory.example.yaml beside it when
// there's none yet. factory.yaml is yours, and git ignores it, so pulling
// a new version of the factory never conflicts with your settings.
func firstConfig(path string) error {
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		return nil
	}
	example := filepath.Join(filepath.Dir(path), "factory.example.yaml")
	b, err := os.ReadFile(example)
	if err != nil {
		return nil // loading says what's missing
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "Made %s from %s. It's yours to edit; git leaves it alone.\n", path, filepath.Base(example))
	return nil
}
