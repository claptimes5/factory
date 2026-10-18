// Command factory turns labeled GitHub issues into reviewed draft PRs.
//
//	factory setup      ask for keys and settings, save them, create labels
//	factory serve      run the daemon
//	factory run-once   poll once and run queued jobs to completion, then exit
//	factory status     print items, the queue and today's spend
//	factory migrate    apply database migrations and exit
//	factory backup F   write a consistent copy of the database to F
package main

import (
	"context"
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
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/claptimes5/factory/internal/compare"
	"github.com/claptimes5/factory/internal/config"
	"github.com/claptimes5/factory/internal/eval"
	"github.com/claptimes5/factory/internal/github"
	"github.com/claptimes5/factory/internal/harness"
	"github.com/claptimes5/factory/internal/mcp"
	"github.com/claptimes5/factory/internal/notify"
	"github.com/claptimes5/factory/internal/pipeline"
	"github.com/claptimes5/factory/internal/runner"
	"github.com/claptimes5/factory/internal/state"
	"github.com/claptimes5/factory/internal/store"
	"github.com/claptimes5/factory/internal/web"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "factory:", err)
		os.Exit(1)
	}
}

func usage() error {
	return fmt.Errorf("usage: factory [-config factory.yaml] [-env factory.env] setup|images [--rebuild]|readiness [-file|-queue] [repo]|serve|run-once|status|migrate|backup <file>|pull <issue>|last-job|wait-job <after-id> <timeout>|eval [flags]|compare <issue> -skills a,b|mcp-bridge <socket>")
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
	fs := flag.NewFlagSet("factory", flag.ContinueOnError)
	cfgPath := fs.String("config", envOr("FACTORY_CONFIG", "factory.yaml"), "path to factory.yaml")
	envPath := fs.String("env", os.Getenv("FACTORY_ENV_FILE"), "settings file `factory setup` writes (default: factory.env next to the config)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return usage()
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
		return runEval(ctx, cfg, fs.Args()[1:])
	case "serve", "run-once":
		p, err := build(cfg, st)
		if err != nil {
			return err
		}
		if cmd == "serve" {
			if err := serveWeb(ctx, cfg, st, p); err != nil {
				return err
			}
			healthCheck(ctx, cfg, p)
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
	return map[string]map[string]string{
		"claude": pick("ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN"),
		"codex":  codex,
	}
}

func newRunner(cfg *config.Config) (runner.Runner, error) {
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
		d.Egress = &runner.Egress{Allow: allow, Private: private, Binary: bin}
	}
	return d, nil
}

func build(cfg *config.Config, st *store.Store) (*pipeline.Pipeline, error) {
	// Fail at start, not on the first job, when credentials are missing.
	for _, repos := range [][]string{githubRepos(cfg), gitlabRepos(cfg)} {
		if len(repos) == 0 {
			continue
		}
		if _, err := tokens(cfg, repos[0]); err != nil {
			return nil, err
		}
	}
	if cfg.Orchestrator.On && cfg.Runner.Kind == "docker" && runtime.GOOS != "linux" {
		// Its container runs this binary as the MCP bridge.
		slog.Warn("orchestrator disabled: with the docker runner it needs a Linux host")
		cfg.Orchestrator.On = false
	}
	r, err := newRunner(cfg)
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
			"codex":  harness.NewCodex(r),
		},
		Secrets: secrets(),
		Log:     slog.Default(),
		Notify:  notifier(),
	}, nil
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

// healthCheck warns when the job image is missing, since every job would fail.
func healthCheck(ctx context.Context, cfg *config.Config, p *pipeline.Pipeline) {
	if cfg.Runner.Kind != "docker" {
		return
	}
	seen := map[string]bool{}
	for _, n := range cfg.RepoNames() {
		r, _ := cfg.RepoConf(n)
		if seen[r.Image] {
			continue
		}
		seen[r.Image] = true
		if err := exec.CommandContext(ctx, "docker", "image", "inspect", r.Image).Run(); err != nil {
			msg := fmt.Sprintf("Job image %s (for %s) is missing or Docker is down: %v. Build it with ./factory images", r.Image, n, err)
			slog.Error(msg)
			if p.Notify != nil {
				p.Notify(ctx, msg)
			}
		}
	}
}

func serveWeb(ctx context.Context, cfg *config.Config, st *store.Store, p *pipeline.Pipeline) error {
	host, _, err := net.SplitHostPort(cfg.Web.Listen)
	if err != nil {
		return fmt.Errorf("web.listen: %w", err)
	}
	srv := &web.Server{Store: st, Actions: p, Repos: cfg.RepoNames(), DailyCap: cfg.Budgets.DailyTotal, AwaitCap: cfg.Budgets.AwaitingHumanMax,
		Hosts: append([]string{host}, cfg.Web.Hosts...), CompareDir: filepath.Join(cfg.Path(cfg.DataDir), "compare")}
	srv.JobsDir = filepath.Join(cfg.Path(cfg.DataDir), "jobs")
	srv.DiskLow = p.DiskLow
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
	h, err := srv.Handler()
	if err != nil {
		return err
	}
	l, err := net.Listen("tcp", cfg.Web.Listen)
	if err != nil {
		return err
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
	return nil
}

func status(ctx context.Context, st *store.Store, cfg *config.Config) error {
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	defer w.Flush()
	items, err := st.ListItems(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintln(w, "ITEM\tISSUE\tSTAGE\tSIZE\tRISK\tPR\tTITLE")
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
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\t%s\n", it.ID, issueRef(cfg, it.Repo, it.IssueNumber), it.Stage, dash(it.Complexity), dash(it.Risk), pr, it.Title)
	}
	fmt.Fprintln(w)
	for _, s := range []state.JobState{state.Queued, state.Running} {
		n, _ := st.CountJobs(ctx, s, "")
		fmt.Fprintf(w, "jobs %s:\t%d\n", s, n)
	}
	spent, _ := st.SpendOn(ctx, store.Today())
	fmt.Fprintf(w, "spend today:\t$%.2f of $%.2f\n", spent, cfg.Budgets.DailyTotal)
	jobs, err := st.ListJobs(ctx, store.JobFilter{Limit: 10})
	if err != nil {
		return err
	}
	if len(jobs) > 0 {
		fmt.Fprintln(w, "\nJOB\tKIND\tSTATE\tHARNESS\tMODEL\tSKILLS\tATTEMPT\tCOST\tERROR")
		for _, j := range jobs {
			fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\t%d\t$%.2f\t%s\n", j.ID, j.Kind, j.State, j.Harness, j.Model, dash(j.Skillset), j.Attempt, j.CostUSD, trunc(j.Error, 60))
		}
	}
	return nil
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
func runEval(ctx context.Context, cfg *config.Config, args []string) error {
	if len(args) > 0 && args[0] == "capture" {
		return runCapture(ctx, cfg, args[1:])
	}
	fs := flag.NewFlagSet("eval", flag.ContinueOnError)
	candidate := fs.String("config", "", "candidate factory.yaml (routing, prompts, skills) to evaluate; default the main config")
	set := fs.String("set", "evals", "directory of replay cases")
	baseline := fs.String("baseline", "", "report JSON of the current config to compare against")
	out := fs.String("out", "", "write the report JSON here")
	parallel := fs.Int("parallel", 1, "cases to run at once")
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
	cases, err := eval.Load(cfg.Path(*set))
	if err != nil {
		return err
	}
	if len(cases) == 0 {
		return fmt.Errorf("no cases in %s", *set)
	}
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
	r, err := newRunner(cfg)
	if err != nil {
		return err
	}
	work, err := os.MkdirTemp(cfg.Path(cfg.DataDir), "eval-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	rep, err := eval.Run(ctx, eval.Options{
		Cfg: cfg, Runner: r, Secrets: secrets(), Tokens: ts, TokensFor: tokensFor, WorkDir: work, Parallel: *parallel,
		Harnesses: map[string]harness.Harness{"claude": harness.NewClaude(r), "codex": harness.NewCodex(r)},
	}, cases)
	if err != nil {
		return err
	}
	rep.Config = *candidate
	fmt.Print(rep.Table())
	if *out != "" {
		b, _ := json.MarshalIndent(rep, "", "  ")
		if err := os.WriteFile(*out, b, 0o644); err != nil {
			return err
		}
	}
	if *baseline != "" {
		b, err := os.ReadFile(*baseline)
		if err != nil {
			return err
		}
		var base eval.Report
		if err := json.Unmarshal(b, &base); err != nil {
			return fmt.Errorf("baseline: %w", err)
		}
		if why := eval.Compare(rep, &base); len(why) > 0 {
			return fmt.Errorf("rejected: %s", strings.Join(why, "; "))
		}
		fmt.Println("accepted: no worse than the baseline")
	}
	return nil
}

// pull hands an item over for local work: it fetches the item's branch into
// the git repository in the current directory and copies the item's job
// transcripts (and Claude sessions, when kept) next to it.
// remoteIs reports whether a git remote URL (https or ssh) points at the
// GitHub repo owner/name.
func remoteIs(url, repo string) bool {
	u := strings.ToLower(strings.TrimSuffix(strings.TrimSuffix(url, "/"), ".git"))
	r := strings.ToLower(repo)
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
		case <-time.After(10 * time.Second):
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
	complexity := fs.String("complexity", "", "S, M or L (default: the item's triage, else M)")
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
