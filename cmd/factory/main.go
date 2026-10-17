// Command factory turns labeled GitHub issues into reviewed draft PRs.
//
//	factory serve      run the daemon
//	factory run-once   poll once and run queued jobs to completion, then exit
//	factory status     print items, the queue and today's spend
//	factory migrate    apply database migrations and exit
//	factory backup F   write a consistent copy of the database to F
package main

import (
	"context"
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
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/claptimes5/factory/internal/config"
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
	return fmt.Errorf("usage: factory [-config factory.yaml] serve|run-once|status|migrate|backup <file>|mcp-bridge <socket>")
}

func run(args []string) error {
	// The orchestrator's MCP bridge runs inside the job container, where there
	// is no config or database: it only joins stdio to the daemon's socket.
	if len(args) == 2 && args[0] == "mcp-bridge" {
		return mcp.Bridge(args[1], os.Stdin, os.Stdout)
	}
	fs := flag.NewFlagSet("factory", flag.ContinueOnError)
	cfgPath := fs.String("config", envOr("FACTORY_CONFIG", "factory.yaml"), "path to factory.yaml")
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
			slog.Info("factory serving", "repo", cfg.Repo, "runner", cfg.Runner.Kind, "control_room", "http://"+cfg.Web.Listen)
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

func tokens(cfg *config.Config) (github.TokenSource, error) {
	if app := os.Getenv("FACTORY_GITHUB_APP_ID"); app != "" {
		key, err := github.LoadAppKey(os.Getenv("FACTORY_GITHUB_APP_KEY"))
		if err != nil {
			return nil, fmt.Errorf("FACTORY_GITHUB_APP_KEY: %w", err)
		}
		return &github.AppTokens{AppID: app, InstallationID: os.Getenv("FACTORY_GITHUB_INSTALLATION_ID"), Key: key, Repo: cfg.Repo}, nil
	}
	if t := os.Getenv("GITHUB_TOKEN"); t != "" {
		return github.StaticToken(t), nil
	}
	return nil, fmt.Errorf("set FACTORY_GITHUB_APP_ID, FACTORY_GITHUB_INSTALLATION_ID and FACTORY_GITHUB_APP_KEY (or GITHUB_TOKEN for development)")
}

// secrets collects model credentials from the daemon's environment. They are
// injected into job containers per job and never written to disk.
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

func build(cfg *config.Config, st *store.Store) (*pipeline.Pipeline, error) {
	ts, err := tokens(cfg)
	if err != nil {
		return nil, err
	}
	if cfg.Orchestrator.On && cfg.Runner.Kind == "docker" && runtime.GOOS != "linux" {
		// Its container runs this binary as the MCP bridge.
		slog.Warn("orchestrator disabled: with the docker runner it needs a Linux host")
		cfg.Orchestrator.On = false
	}
	var r runner.Runner = runner.Local{}
	if cfg.Runner.Kind == "docker" {
		r = &runner.Docker{CPUs: cfg.Runner.CPUs, Memory: cfg.Runner.Memory, Network: cfg.Runner.Network, UID: os.Getuid(), GID: os.Getgid()}
	}
	return &pipeline.Pipeline{
		Cfg:    cfg,
		Store:  st,
		GH:     &github.Client{Repo: cfg.Repo, Tokens: ts},
		Git:    &pipeline.Git{RemoteURL: "https://github.com/" + cfg.Repo + ".git", Tokens: ts, Author: "factory[bot] <factory@users.noreply.github.com>", AskPassDir: cfg.Path(cfg.DataDir)},
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
	if err := exec.CommandContext(ctx, "docker", "image", "inspect", cfg.Runner.Image).Run(); err != nil {
		msg := fmt.Sprintf("Job image %s is missing or Docker is down: %v", cfg.Runner.Image, err)
		slog.Error(msg)
		if p.Notify != nil {
			p.Notify(ctx, msg)
		}
	}
}

func serveWeb(ctx context.Context, cfg *config.Config, st *store.Store, p *pipeline.Pipeline) error {
	host, _, err := net.SplitHostPort(cfg.Web.Listen)
	if err != nil {
		return fmt.Errorf("web.listen: %w", err)
	}
	srv := &web.Server{Store: st, Actions: p, Repo: cfg.Repo, DailyCap: cfg.Budgets.DailyTotal, AwaitCap: cfg.Budgets.AwaitingHumanMax,
		Hosts: append([]string{host}, cfg.Web.Hosts...)}
	if cfg.Runner.Kind == "docker" {
		srv.Attach = web.TTYDAttach(st, host)
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
		fmt.Fprintf(w, "%d\t#%d\t%s\t%s\t%s\t%s\t%s\n", it.ID, it.IssueNumber, it.Stage, dash(it.Complexity), dash(it.Risk), pr, it.Title)
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
