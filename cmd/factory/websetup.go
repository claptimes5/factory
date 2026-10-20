package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/claptimes5/factory/internal/config"
	"github.com/claptimes5/factory/internal/github"
	"github.com/claptimes5/factory/internal/harness"
	"github.com/claptimes5/factory/internal/notify"
	"github.com/claptimes5/factory/internal/pipeline"
	"github.com/claptimes5/factory/internal/stack"
	"github.com/claptimes5/factory/internal/web"
)

// wizard is the dashboard's get-started page (web.Wizard): what `factory
// setup` asks in the terminal, saved the same way, to factory.yaml and
// factory.env. Secrets go in and never come back out, except masked.
type wizard struct {
	cfgPath, envPath string
	keyDir           string // where a new app's key is saved, next to factory.env
	setupMode        bool
	// restart applies saved settings once running jobs finish (the
	// daemon's RequestRestart); nil in setup mode, where Finish closes done.
	restart  func(context.Context) int
	done     chan struct{}
	finish   sync.Once
	bg       context.Context // the daemon's: an image build stops with it
	exported map[string]string
	// machineCheck runs the doctor's checks of this machine; tests
	// replace it.
	machineCheck func(context.Context, *config.Config) []finding

	act sync.Mutex // one action at a time, each reading what the last saved

	mu        sync.Mutex // guards everything below
	cfg       *config.Config
	started   string // the files' version when the factory started
	looked    bool   // the first look at this machine and the repos has started
	machine   []finding
	machineAt time.Time
	images    map[string]bool // job image tag: built
	access    map[string]web.APIWizardAccess
	// detected is what each reachable repo's files suggest for its checks,
	// read with the access check; tries are the last "Try these checks".
	detected  map[string]*stack.Suggestion
	tries     map[string]*web.APIWizardTry
	credGen   int // bumped by each credential change, so stale checks are dropped
	labels    []web.APIWizardLabel
	missing   []string // accounts the app isn't installed on, from the last check
	instErr   string
	checkedAt time.Time  // when the access, install and label checks last ran
	refresh   sync.Mutex // one live check at a time
	pending   map[string]pendingApp
	builder   *imageBuilder // shared with serve's own build of missing images
	signIn    pageSignIn
	// signInDone counts the running sign-in, so Cancel can wait for it.
	signInDone sync.WaitGroup
}

// pageSignIn is the ChatGPT sign-in started from the page.
type pageSignIn struct {
	running   bool
	log       *tailBuffer
	result    string
	failed    bool
	cancel    context.CancelFunc // stops Codex
	cancelled bool               // by Cancel, not a timeout or shutdown
}

// runCmdContext runs a command that ctx can stop; tests replace it.
var runCmdContext = func(ctx context.Context, out io.Writer, name string, args ...string) error {
	c := exec.CommandContext(ctx, name, args...)
	c.Stdout, c.Stderr = out, out
	c.WaitDelay = 5 * time.Second
	return c.Run()
}

// pendingApp is a new-app flow started from the page, by its state.
type pendingApp struct {
	owner string
	at    time.Time
}

// accessWait bounds one run of the access checks.
var accessWait = 30 * time.Second

// appFlowTTL is how long a started app flow can come back.
const appFlowTTL = time.Hour

// newWizard makes the page's backend. In setup mode Finish closes done;
// otherwise it asks restart to apply what changed. builder is serve's,
// so the page shows its builds; nil gives the page one of its own.
func newWizard(ctx context.Context, cfgPath, envPath string, setupMode bool, restart func(context.Context) int, builder *imageBuilder) (*wizard, error) {
	envPath, err := filepath.Abs(envPath)
	if err != nil {
		return nil, err
	}
	cfgPath, err = filepath.Abs(cfgPath)
	if err != nil {
		return nil, err
	}
	// Its own copy: the daemon's config stays as it started until a restart.
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, err
	}
	w := &wizard{cfgPath: cfgPath, envPath: envPath, keyDir: filepath.Dir(envPath), setupMode: setupMode, restart: restart,
		done: make(chan struct{}), bg: ctx, exported: envMap(startEnv), cfg: cfg,
		access: map[string]web.APIWizardAccess{}, pending: map[string]pendingApp{}}
	if builder == nil {
		builder = &imageBuilder{}
	}
	w.builder = builder
	w.machineCheck = func(ctx context.Context, cfg *config.Config) []finding {
		return machineFindings(ctx, cfg, builder.Building)
	}
	// Whoever started a build, the page's image list is checked again after.
	builder.onDone(func() { w.checkMachine(w.bg) })
	w.started = w.version()
	return w, nil
}

func envMap(env []string) map[string]string {
	m := map[string]string{}
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			m[k] = v
		}
	}
	return m
}

// machineFindings are the doctor's checks of this machine, as setup
// shows them, with Docker's version and the disk.
func machineFindings(ctx context.Context, cfg *config.Config, building func() []string) []finding {
	d := newDoctor(cfg)
	d.run = runOutput
	d.building = building
	out := d.checkAll(ctx, true)
	out = append(out, d.dockerVersion(ctx)...)
	out = append(out, d.images(ctx)...)
	return append(out, d.disk(ctx)...)
}

// version changes whenever factory.yaml or factory.env does.
func (w *wizard) version() string {
	h := sha256.New()
	for _, p := range []string{w.cfgPath, w.envPath} {
		b, _ := os.ReadFile(p)
		s := sha256.Sum256(b)
		h.Write(s[:])
	}
	return hex.EncodeToString(h.Sum(nil))
}

// vals reads the settings as the factory will run with them, as
// loadEnvFile does: what the factory was started with wins when it is
// set, else the settings file.
func (w *wizard) vals() map[string]string {
	vals, err := readEnvFile(w.envPath)
	if err != nil {
		vals = map[string]string{}
	}
	for _, k := range envKeys {
		if v, ok := w.exported[k]; ok {
			vals[k] = v
		}
	}
	return vals
}

// overridden are the keys saved in the settings file that an exported
// variable overrides, so saving them here changes nothing.
func (w *wizard) overridden() []string {
	saved, err := readEnvFile(w.envPath)
	if err != nil {
		return nil
	}
	var out []string
	for _, k := range envKeys {
		if v, ok := w.exported[k]; ok {
			if s, in := saved[k]; in && s != v {
				out = append(out, k)
			}
		}
	}
	return out
}

// save sets keys in factory.env and comments out drop, as `factory
// codex-login` does, keeping every other line. A new file is readable
// only by you.
func (w *wizard) save(set map[string]string, drop ...string) error {
	if _, err := os.Stat(w.envPath); os.IsNotExist(err) {
		if err := writeEnvFile(w.envPath, "# Written by `factory setup`. Values set in the environment override these.\n", nil, nil); err != nil {
			return err
		}
	}
	_, err := updateEnvFile(w.envPath, set, drop)
	return err
}

// credentialsChanged forgets what was checked with the old ones.
func (w *wizard) credentialsChanged() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.credGen++
	w.access = map[string]web.APIWizardAccess{}
	w.labels = nil
}

func (w *wizard) config() *config.Config {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.cfg
}

// reload reads factory.yaml again after the page changed it.
func (w *wizard) reload() error {
	c, err := config.Load(w.cfgPath)
	if err != nil {
		return err
	}
	w.mu.Lock()
	w.cfg = c
	w.images = nil
	w.mu.Unlock()
	w.credentialsChanged()
	return nil
}

// liveFor is how long a live check of the forges stands for the page.
const liveFor = 30 * time.Second

// State is the page, from what was saved and the last checks. The first
// look starts the machine and image checks in the background. With live,
// the access, install and label checks run again if the last are older
// than liveFor, so a revoked token, an uninstalled app or a deleted label
// shows on the page.
func (w *wizard) State(ctx context.Context, live bool) web.APIWizard {
	w.mu.Lock()
	first := !w.looked
	w.looked = true
	w.mu.Unlock()
	if first {
		go func() {
			ctx, cancel := context.WithTimeout(w.bg, 2*time.Minute)
			defer cancel()
			w.checkMachine(ctx)
		}()
	}
	if live {
		w.refresh.Lock()
		w.mu.Lock()
		stale := time.Since(w.checkedAt) > liveFor
		w.mu.Unlock()
		if stale {
			w.checkAccess(ctx)
		}
		w.refresh.Unlock()
	}
	vals := w.vals()
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.stateLocked(vals)
}

func secretOf(v string) web.APIWizardSecret {
	if v == "" {
		return web.APIWizardSecret{}
	}
	return web.APIWizardSecret{Set: true, Masked: mask(v)}
}

var findingStatus = [...]string{"pass", "warn", "fail"}

func (w *wizard) stateLocked(vals map[string]string) web.APIWizard {
	cfg := w.cfg
	out := web.APIWizard{ConfigPath: w.cfgPath, EnvPath: w.envPath, Repos: cfg.RepoNames(), Machine: []web.APIWizardFinding{},
		Access: []web.APIWizardAccess{}, Labels: []web.APIWizardLabel{}, Warnings: []string{}, Steps: []web.APIWizardStep{},
		Notify: secretOf(vals["FACTORY_NOTIFY_URL"]), Changed: w.version() != w.started}
	if out.Labels = w.labels; out.Labels == nil {
		out.Labels = []web.APIWizardLabel{}
	}
	for _, l := range pipeline.Labels(cfg.Intake.Label) {
		out.LabelNames = append(out.LabelNames, l.Name)
	}
	// Names only: the values are secrets.
	for _, k := range w.overridden() {
		out.Warnings = append(out.Warnings, k+" is set in the environment the factory starts with, so it overrides the one saved in "+filepath.Base(w.envPath)+". Unset it there for the saved one to apply.")
	}
	step := func(key, title, state, summary string) {
		out.Steps = append(out.Steps, web.APIWizardStep{Key: key, Title: title, State: state, Summary: summary})
	}

	// This machine.
	fails := 0
	for _, f := range w.machine {
		af := web.APIWizardFinding{Status: findingStatus[f.status], What: f.what, Fix: f.fix}
		if f.fix == fixBuildImages {
			// The page builds them itself: a button, not a command.
			af.Fix, af.Action = "", "build_images"
		}
		out.Machine = append(out.Machine, af)
		if f.status == fail {
			fails++
		}
	}
	switch {
	case w.machineAt.IsZero():
		step("machine", "This machine", "attention", "Checking what this machine has…")
	case fails > 0:
		out.MachineCheckedAt = w.machineAt.UTC().Format(time.RFC3339)
		step("machine", "This machine", "attention", plural(fails, "thing", "things")+" to fix")
	default:
		out.MachineCheckedAt = w.machineAt.UTC().Format(time.RFC3339)
		step("machine", "This machine", "done", "Everything the factory needs is here")
	}
	step("repos", "Repositories", "done", joinAnd(cfg.RepoNames()))

	// Each repo's access, in config order.
	reach := map[string]int{} // host: repos reached
	for _, n := range cfg.RepoNames() {
		rc, _ := cfg.RepoConf(n)
		a, ok := w.access[n]
		if !ok {
			a = web.APIWizardAccess{Repo: n, Host: "github", State: "unchecked", Hints: []string{}}
			if rc.GitLab() {
				a.Host = "gitlab"
			}
		}
		a.BaseBranch, a.CanUseDefault = rc.BaseBranch, len(cfg.Repos) == 1
		if a.MissingLabels == nil {
			a.MissingLabels = []string{} // unchecked, or unreachable: a list, never null
		}
		if a.DefaultBranch == a.BaseBranch {
			a.DefaultBranch = ""
		}
		if a.State == "ok" {
			reach[a.Host]++
		}
		out.Access = append(out.Access, a)
	}
	forgeStep := func(key, title string, repos []string, connected bool) {
		var unlabeled []string
		for _, a := range out.Access {
			if a.Host == key && len(a.MissingLabels) > 0 {
				unlabeled = append(unlabeled, a.Repo)
			}
		}
		switch {
		case reach[key] == len(repos) && len(unlabeled) > 0:
			step(key, title, "attention", "Labels missing on "+joinAnd(unlabeled))
			out.Warnings = append(out.Warnings, "Labels the factory uses are missing on "+joinAnd(unlabeled)+", so it can't mark its work there.")
		case reach[key] == len(repos):
			step(key, title, "done", "The factory can reach "+joinAnd(repos))
		case !connected:
			step(key, title, "attention", "Not connected yet")
		default:
			var bad []string
			for _, a := range out.Access {
				if a.Host == key && a.State == "failed" {
					bad = append(bad, a.Repo)
				}
			}
			if len(bad) > 0 {
				step(key, title, "attention", "Can't reach "+joinAnd(bad)+" yet")
			} else {
				step(key, title, "attention", "Not checked yet")
			}
		}
	}
	if repos := githubRepos(cfg); len(repos) > 0 {
		g := &web.APIWizardGitHub{Method: "none", AppID: vals["FACTORY_GITHUB_APP_ID"], AppSlug: vals["FACTORY_GITHUB_APP_SLUG"],
			KeyPath: vals["FACTORY_GITHUB_APP_KEY"], InstallationID: vals["FACTORY_GITHUB_INSTALLATION_ID"], Token: secretOf(vals["GITHUB_TOKEN"]),
			TokenURL: githubWeb + "/settings/personal-access-tokens/new", Missing: []web.APIWizardOwner{}, InstallSteps: []string{}, InstallError: w.instErr}
		g.AppOwner, _, _ = strings.Cut(repos[0], "/")
		switch {
		case g.AppID != "":
			g.Method, g.Token = "app", web.APIWizardSecret{}
		case g.Token.Set:
			g.Method = "token"
		}
		if g.Method == "app" {
			if g.AppSlug != "" {
				g.InstallURL = installURL(g.AppSlug)
			}
			byOwner := owners(cfg)
			for _, o := range w.missing {
				g.Missing = append(g.Missing, web.APIWizardOwner{Owner: o, Repos: byOwner[strings.ToLower(o)]})
			}
			if len(w.missing) > 0 {
				g.InstallSteps = installSteps(cfg, w.missing)
			}
		}
		out.GitHub = g
		forgeStep("github", "GitHub", repos, g.Method != "none")
	}
	if repos := gitlabRepos(cfg); len(repos) > 0 {
		g := &web.APIWizardGitLab{Token: secretOf(vals["GITLAB_TOKEN"]), FineGrained: gitlabFineGrained, Hosts: []web.APIWizardHost{}}
		for _, h := range gitlabHosts(cfg) {
			g.Hosts = append(g.Hosts, web.APIWizardHost{Host: h, LegacyURL: gitlabLegacyURL(h), TokensURL: gitlabTokensURL(h)})
		}
		out.GitLab = g
		forgeStep("gitlab", "GitLab", repos, g.Token.Set)
	}
	out.CanFinish = reach["github"]+reach["gitlab"] == len(cfg.Repos)

	// Each repo's checks, saved or suggested: what every change must pass.
	var unsaved, disagree []string
	for _, r := range cfg.Repos {
		c := wizardChecks(r, w.detected[r.Name])
		if t := w.tries[r.Name]; t != nil {
			cp := *t
			cp.Steps = slices.Clone(t.Steps)
			c.Try = &cp
		}
		out.Checks = append(out.Checks, c)
		switch {
		case !c.Saved:
			unsaved = append(unsaved, r.Name)
		case slices.ContainsFunc(c.Gates, func(g web.APIWizardGate) bool { return g.Suggested != "" && g.Source != "" }):
			disagree = append(disagree, r.Name)
		}
	}
	switch {
	case len(unsaved) > 0:
		step("checks", "Checks", "attention", "Confirm the checks for "+joinAnd(unsaved))
		out.Warnings = append(out.Warnings, joinAnd(unsaved)+" has no checks of its own confirmed, so every change there runs the top-level ones in factory.yaml.")
	case len(disagree) > 0:
		step("checks", "Checks", "attention", "The repo's agent instructions suggest other commands for "+joinAnd(disagree))
	default:
		step("checks", "Checks", "done", "Every repo's checks are set")
	}

	if usesHarness(cfg, "claude") {
		c := &web.APIWizardClaude{}
		switch {
		case vals["ANTHROPIC_API_KEY"] != "":
			c.Kind, c.Key = "api_key", secretOf(vals["ANTHROPIC_API_KEY"])
			step("claude", "Claude", "done", "API key saved")
		case vals["CLAUDE_CODE_OAUTH_TOKEN"] != "":
			c.Kind, c.Key = "subscription", secretOf(vals["CLAUDE_CODE_OAUTH_TOKEN"])
			step("claude", "Claude", "done", "Subscription token saved")
		default:
			step("claude", "Claude", "attention", "No key yet")
			out.Warnings = append(out.Warnings, "Claude has no key, so jobs routed to Claude will fail.")
		}
		out.Claude = c
	}
	if usesHarness(cfg, "codex") {
		c := &web.APIWizardCodex{Mode: codexAPIKey, APIKey: secretOf(firstOf(vals["OPENAI_API_KEY"], vals["CODEX_API_KEY"])),
			AccessToken: secretOf(vals["CODEX_ACCESS_TOKEN"]), SignIn: w.signInState(), ReviewOnly: usesHarness(cfg, "claude")}
		if b, err := os.ReadFile(codexAuthFile(cfg)); err == nil && harness.ValidAuth(b) {
			c.SignedIn = true
		}
		switch {
		case c.AccessToken.Set:
			c.Mode = codexAccessToken
		case vals["FACTORY_CODEX_AUTH"] == codexChatGPT:
			c.Mode = codexChatGPT
		}
		switch {
		case c.Mode == codexAccessToken:
			step("codex", "OpenAI (Codex)", "done", "Access token saved")
		case c.Mode == codexChatGPT && c.SignedIn:
			step("codex", "OpenAI (Codex)", "done", "ChatGPT plan, signed in")
		case c.Mode == codexChatGPT:
			step("codex", "OpenAI (Codex)", "attention", "ChatGPT plan, not signed in yet")
			out.Warnings = append(out.Warnings, "Codex isn't signed in to ChatGPT, so Codex jobs wait until you sign in.")
		case c.APIKey.Set:
			step("codex", "OpenAI (Codex)", "done", "API key saved")
		default:
			step("codex", "OpenAI (Codex)", "attention", "No key or sign-in yet")
			out.Warnings = append(out.Warnings, "Codex has no key or sign-in, so jobs routed to Codex will fail.")
		}
		out.Codex = c
	}
	if out.Notify.Set {
		step("notify", "Notifications", "done", "Saved")
	} else {
		step("notify", "Notifications", "optional", "Optional: get a message when something needs you")
	}
	if cfg.Sources.Sentry.Enabled {
		s := secretOf(vals["SENTRY_AUTH_TOKEN"])
		out.Sentry = &s
		if s.Set {
			step("sentry", "Sentry", "done", "Token saved")
		} else {
			step("sentry", "Sentry", "optional", "Optional: without a token nothing is filed from Sentry")
		}
	}
	if cfg.Runner.Kind == "docker" {
		running, log, result, failed := w.builder.status()
		im := &web.APIWizardImages{Images: []web.APIWizardImage{}, Building: running, Log: log, Result: result, Failed: failed}
		missing := 0
		for _, j := range jobImages(cfg) {
			built := w.images[j.tag]
			if !built {
				missing++
			}
			im.Images = append(im.Images, web.APIWizardImage{Tag: j.tag, For: joinAnd(j.repos), Built: built})
		}
		switch {
		case im.Building:
			step("images", "Job images", "attention", "Building…")
		case w.images == nil:
			step("images", "Job images", "attention", "Checking which are built…")
		case missing > 0:
			step("images", "Job images", "attention", plural(missing, "image", "images")+" to build")
			out.Warnings = append(out.Warnings, "Jobs can't start until the job images are built.")
		default:
			step("images", "Job images", "done", "Built")
		}
		out.Images = im
	}
	if fails > 0 {
		out.Warnings = append(out.Warnings, "This machine has "+plural(fails, "problem", "problems")+" that can stop jobs.")
	}
	switch {
	case !out.CanFinish:
		step("finish", "Finish", "attention", "Once the factory can reach every repo")
	case len(out.Warnings) > 0:
		step("finish", "Finish", "done", "Ready, with "+plural(len(out.Warnings), "warning", "warnings"))
	default:
		step("finish", "Finish", "done", "Ready")
	}
	return out
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}

// checkMachine runs the doctor's machine checks and sees which job
// images are built.
func (w *wizard) checkMachine(ctx context.Context) {
	cfg := w.config()
	fs := w.machineCheck(ctx, cfg)
	images := map[string]bool{}
	if cfg.Runner.Kind == "docker" {
		for _, im := range jobImages(cfg) {
			_, err := runOutput(ctx, "docker", "image", "inspect", im.tag) // gives up on a hung Docker
			images[im.tag] = err == nil
		}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.machine, w.machineAt = fs, time.Now()
	if w.cfg == cfg {
		w.images = images
	}
}

// clients reach each repo with the saved credentials, or are nil when
// none are saved.
func (w *wizard) clients(cfg *config.Config, vals map[string]string) clientFor {
	var gh, gl clientFor
	switch {
	case vals["FACTORY_GITHUB_APP_ID"] != "":
		gh = appClients(vals)
	case vals["GITHUB_TOKEN"] != "":
		tok := github.StaticToken(vals["GITHUB_TOKEN"])
		gh = func(repo string) (forge, github.TokenSource, error) {
			return &github.Client{Repo: repo, Tokens: tok, BaseURL: githubAPI}, tok, nil
		}
	}
	if vals["GITLAB_TOKEN"] != "" {
		gl = func(repo string) (forge, github.TokenSource, error) {
			ts := github.StaticToken(vals["GITLAB_TOKEN"])
			return newForge(cfg, repo, ts), ts, nil
		}
	}
	return bothHosts(cfg, gh, gl)
}

// checkAccess checks each repo with the saved credentials, as setup
// does, and reads its default branch.
func (w *wizard) checkAccess(ctx context.Context) {
	// A closed request mustn't leave "can't reach" on every repo, so the
	// check runs on its own clock. Repos are checked side by side; one a
	// slow forge doesn't answer in time is marked as timed out, and the
	// rest keep what they found.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), accessWait)
	defer cancel()
	vals := w.vals()
	w.mu.Lock()
	cfg, gen := w.cfg, w.credGen
	w.mu.Unlock()
	var (
		mu      sync.Mutex
		wg      sync.WaitGroup
		missing []string
		instErr string
		results = map[string]web.APIWizardAccess{}
		found   = map[string]*stack.Suggestion{}
		slots   = make(chan struct{}, accessParallel)
	)
	// Where the app is installed, so an uninstalled one shows too.
	if id := vals["FACTORY_GITHUB_APP_ID"]; id != "" && len(githubRepos(cfg)) > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			key, err := github.LoadAppKey(vals["FACTORY_GITHUB_APP_KEY"])
			var miss []string
			if err == nil {
				_, miss, err = installations(ctx, &github.AppTokens{AppID: id, Key: key, BaseURL: githubAPI}, githubRepos(cfg))
			}
			mu.Lock()
			defer mu.Unlock()
			switch {
			case isContextErr(err):
				instErr = "timed out"
			case err != nil:
				instErr = err.Error()
			default:
				missing = miss
			}
		}()
	}
	labels := pipeline.Labels(cfg.Intake.Label)
	client := w.clients(cfg, vals)
	for _, n := range cfg.RepoNames() {
		rc, _ := cfg.RepoConf(n)
		a := web.APIWizardAccess{Repo: n, Host: "github", State: "unchecked", Hints: []string{}}
		hasCreds := vals["FACTORY_GITHUB_APP_ID"] != "" || vals["GITHUB_TOKEN"] != ""
		if rc.GitLab() {
			a.Host, hasCreds = "gitlab", vals["GITLAB_TOKEN"] != ""
		}
		if client == nil || !hasCreds {
			results[n] = a
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			case <-ctx.Done():
				mu.Lock()
				results[n] = timedOutAccess(a)
				mu.Unlock()
				return
			}
			a, err := w.checkOne(ctx, client, cfg, rc, n, a, labels, vals)
			// Only a check cut off by the clock; a real answer such as a
			// 401 stays as it is.
			if isContextErr(err) {
				a = timedOutAccess(a)
			}
			var s *stack.Suggestion
			if a.State == "ok" {
				if gh, _, err := client(n); err == nil {
					s, _ = stack.Detect(ctx, ghFiles{gh})
				}
			}
			mu.Lock()
			results[n] = a
			if s != nil {
				found[n] = s
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	w.mu.Lock()
	defer w.mu.Unlock()
	// Only checks made with the credentials and repos still saved count.
	if w.credGen == gen && w.cfg == cfg {
		w.access, w.checkedAt, w.detected = results, time.Now(), found
		w.missing, w.instErr = missing, instErr
	}
}

// accessParallel is how many repos are checked at once.
const accessParallel = 4

func timedOutAccess(a web.APIWizardAccess) web.APIWizardAccess {
	a.State, a.Error, a.Hints = "failed", "timed out: the forge didn't answer in time; check again", []string{}
	return a
}

// checkOne checks the factory reaches one repo, and its default branch
// and labels.
func (w *wizard) checkOne(ctx context.Context, client clientFor, cfg *config.Config, rc config.Repo, n string, a web.APIWizardAccess, labels []pipeline.Label, vals map[string]string) (web.APIWizardAccess, error) {
	if err := checkRepo(ctx, client, cfg, n); err != nil {
		a.State, a.Error = "failed", err.Error()
		if rc.GitLab() {
			if h := gitlabHint(err, n); h != "" {
				a.Hints = append(a.Hints, h)
			}
		} else {
			a.Hints = append(a.Hints, githubHint(err, n, vals["FACTORY_GITHUB_APP_ID"] != "")...)
		}
		return a, err
	}
	a.State = "ok"
	if gh, _, err := client(n); err == nil {
		if b, err := gh.DefaultBranch(ctx); err == nil {
			a.DefaultBranch = b
		}
		a.MissingLabels, a.LabelsError = missingLabels(ctx, gh, labels)
	}
	return a, nil
}

// isContextErr says whether err is a context's end: a timeout, not an
// answer from the forge.
func isContextErr(err error) bool {
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
}

// missingLabels are the factory's labels a repo doesn't have, or why it
// couldn't tell.
func missingLabels(ctx context.Context, gh forge, want []pipeline.Label) ([]string, string) {
	have, err := gh.ListLabels(ctx)
	if err != nil {
		return []string{}, err.Error()
	}
	out := []string{}
	for _, l := range want {
		if !slices.ContainsFunc(have, func(h string) bool { return strings.EqualFold(h, l.Name) }) {
			out = append(out, l.Name)
		}
	}
	return out, ""
}

// Do runs one of the page's actions.
func (w *wizard) Do(ctx context.Context, action string, in web.WizardInput) (string, error) {
	w.act.Lock()
	defer w.act.Unlock()
	cfg := w.config()
	switch action {
	case "machine/check":
		w.checkMachine(ctx)
		return "Checked this machine again.", nil
	case "repos":
		return w.saveRepos(ctx, in.Repos)
	case "access/check":
		w.checkAccess(ctx)
		return w.accessMessage(), nil
	case "github/install-check":
		return w.checkInstall(ctx)
	case "github/existing-app":
		return w.useApp(ctx, in.AppID, in.KeyPath)
	case "github/token":
		t := strings.TrimSpace(in.Token)
		if t == "" {
			return "", errors.New("paste the token first")
		}
		if err := w.save(map[string]string{"GITHUB_TOKEN": t}, githubAppKeys...); err != nil {
			return "", err
		}
		w.mu.Lock()
		w.missing, w.instErr = nil, ""
		w.mu.Unlock()
		w.credentialsChanged()
		w.checkAccess(ctx)
		if v, ok := w.exported["GITHUB_TOKEN"]; ok && v != t {
			return "Saved the token, but the GITHUB_TOKEN exported where the factory starts wins, so the factory uses that one. This check is of that one: " + w.accessMessage(), nil
		}
		return "Saved the token. " + w.accessMessage(), nil
	case "gitlab/token":
		t := strings.TrimSpace(in.Token)
		if t == "" {
			return "", errors.New("paste the token first")
		}
		if err := w.save(map[string]string{"GITLAB_TOKEN": t}); err != nil {
			return "", err
		}
		w.credentialsChanged()
		w.checkAccess(ctx)
		return "Saved the token. " + w.accessMessage(), nil
	case "base-branch":
		return w.useBranch(ctx, cfg, in.Repo, in.Branch)
	case "checks/save":
		return w.saveChecks(ctx, cfg, in)
	case "checks/try":
		return w.tryChecks(cfg, in)
	case "labels":
		return w.createLabels(ctx, cfg)
	case "claude":
		return w.saveClaude(in.Key, in.Anyway)
	case "codex":
		return w.saveCodex(cfg, in.Mode, in.Key)
	case "notify":
		u := strings.TrimSpace(in.URL)
		if _, err := notify.New(u); err != nil {
			return "", errors.New("that isn't an ntfy, Slack or Discord address")
		}
		if u == "" {
			return "Notifications are off.", w.save(nil, "FACTORY_NOTIFY_URL")
		}
		return "Saved. Send a test to check it works.", w.save(map[string]string{"FACTORY_NOTIFY_URL": u})
	case "notify/test":
		n, err := notify.New(w.vals()["FACTORY_NOTIFY_URL"])
		if err != nil || n == nil {
			return "", errors.New("save a notification address first")
		}
		if err := n.Send(ctx, "Factory", "A test from the factory's setup. Notifications work."); err != nil {
			return "", fmt.Errorf("sending failed: %w", err)
		}
		return "Sent. Check your phone or channel.", nil
	case "sentry":
		t := strings.TrimSpace(in.Token)
		if t == "" {
			return "Removed the Sentry token.", w.save(nil, "SENTRY_AUTH_TOKEN")
		}
		return "Saved the Sentry token.", w.save(map[string]string{"SENTRY_AUTH_TOKEN": t})
	case "codex/sign-in":
		return w.startCodexSignIn(cfg)
	case "codex/sign-in/cancel":
		return w.cancelCodexSignIn()
	case "images/build":
		return w.buildImages(cfg)
	case "finish":
		return w.finishSetup(ctx)
	}
	return "", fmt.Errorf("unknown action %q", action)
}

// accessMessage sums up the last access check.
func (w *wizard) accessMessage() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	var ok, bad, none []string
	for _, n := range w.cfg.RepoNames() {
		switch w.access[n].State {
		case "ok":
			ok = append(ok, n)
		case "failed":
			bad = append(bad, n)
		default:
			none = append(none, n)
		}
	}
	switch {
	case len(bad) > 0:
		return "Couldn't reach " + joinAnd(bad) + "."
	case len(none) > 0 && len(ok) > 0:
		return "The factory can reach " + joinAnd(ok) + "; " + joinAnd(none) + " still need access."
	case len(none) > 0:
		return "Connect the factory to " + joinAnd(none) + " first."
	}
	return "The factory can reach " + joinAnd(ok) + "."
}

func (w *wizard) saveRepos(ctx context.Context, in string) (string, error) {
	names, err := parseRepoList(in)
	if err != nil {
		return "", err
	}
	if slices.Equal(names, w.config().RepoNames()) {
		return "Nothing changed.", nil
	}
	if err := setRepos(w.cfgPath, names); err != nil {
		return "", err
	}
	if err := w.reload(); err != nil {
		return "", err
	}
	w.mu.Lock()
	w.missing, w.instErr = nil, ""
	w.mu.Unlock()
	w.checkAccess(ctx)
	go w.checkMachine(context.WithoutCancel(ctx)) // the images can differ
	return "Saved " + joinAnd(names) + " to factory.yaml.", nil
}

// StartGitHubApp prepares the app manifest the page posts to GitHub.
func (w *wizard) StartGitHubApp(ctx context.Context, callback string) (web.APIWizardManifest, error) {
	w.act.Lock()
	defer w.act.Unlock()
	cfg := w.config()
	if len(githubRepos(cfg)) == 0 {
		return web.APIWizardManifest{}, errors.New("no GitHub repos to make an app for")
	}
	m, err := appManifest(ctx, cfg, callback)
	if err != nil {
		return web.APIWizardManifest{}, err
	}
	state := randHex(16)
	w.mu.Lock()
	for s, p := range w.pending {
		if time.Since(p.at) > appFlowTTL {
			delete(w.pending, s)
		}
	}
	w.pending[state] = pendingApp{owner: m.owner, at: time.Now()}
	w.mu.Unlock()
	return web.APIWizardManifest{URL: m.newURL + "?state=" + state, State: state, Manifest: string(m.manifest)}, nil
}

// FinishGitHubApp exchanges GitHub's code for the new app, saves its key
// next to factory.env and its ID in it, then checks where it's installed.
func (w *wizard) FinishGitHubApp(ctx context.Context, code, state string) (string, error) {
	w.act.Lock()
	defer w.act.Unlock()
	w.mu.Lock()
	p, ok := w.pending[state]
	delete(w.pending, state) // each state works once
	w.mu.Unlock()
	if !ok || state == "" || time.Since(p.at) > appFlowTTL {
		return "", errors.New("that link is from another or an earlier setup; start again with \"Create an app for me\"")
	}
	if code == "" {
		return "", errors.New("GitHub sent no code back; start again with \"Create an app for me\"")
	}
	app, err := convertApp(ctx, code, p.owner)
	if err != nil {
		return "", err
	}
	keyPath, err := saveAppKey(w.keyDir, app)
	if err != nil {
		return "", err
	}
	if err := w.save(map[string]string{"FACTORY_GITHUB_APP_ID": strconv.FormatInt(app.ID, 10), "FACTORY_GITHUB_APP_KEY": keyPath,
		"FACTORY_GITHUB_APP_SLUG": app.Slug}, "FACTORY_GITHUB_INSTALLATION_ID", "GITHUB_TOKEN"); err != nil {
		return "", err
	}
	w.credentialsChanged()
	_, _ = w.checkInstall(ctx)
	return "Created the app " + app.Slug + ". Its key is saved in " + keyPath + ". Now install it.", nil
}

// checkInstall finds the app's installations; once it's on every
// account, it saves the first repo's and checks access.
func (w *wizard) checkInstall(ctx context.Context) (string, error) {
	vals := w.vals()
	cfg := w.config()
	if vals["FACTORY_GITHUB_APP_ID"] == "" {
		return "", errors.New("make or pick a GitHub App first")
	}
	key, err := github.LoadAppKey(vals["FACTORY_GITHUB_APP_KEY"])
	if err != nil {
		return "", fmt.Errorf("can't read the app's key: %w", err)
	}
	app := &github.AppTokens{AppID: vals["FACTORY_GITHUB_APP_ID"], Key: key, BaseURL: githubAPI}
	first, missing, err := installations(ctx, app, githubRepos(cfg))
	w.mu.Lock()
	w.missing, w.instErr = missing, ""
	if err != nil {
		w.missing, w.instErr = nil, err.Error()
	}
	w.mu.Unlock()
	if err != nil {
		return "", fmt.Errorf("couldn't check: %w", err)
	}
	if len(missing) > 0 {
		return "It isn't installed on " + joinOr(missing) + " yet.", nil
	}
	if first != vals["FACTORY_GITHUB_INSTALLATION_ID"] {
		if err := w.save(map[string]string{"FACTORY_GITHUB_INSTALLATION_ID": first}); err != nil {
			return "", err
		}
	}
	w.credentialsChanged()
	w.checkAccess(ctx)
	return "Installed. " + w.accessMessage(), nil
}

// useApp switches to an app made before, once GitHub takes its ID and key.
func (w *wizard) useApp(ctx context.Context, id, keyPath string) (string, error) {
	id, keyPath = strings.TrimSpace(id), strings.TrimSpace(keyPath)
	if _, err := strconv.ParseInt(id, 10, 64); err != nil {
		return "", errors.New("the app ID is a number, under About on the app's settings page")
	}
	if keyPath == "" {
		return "", errors.New("give the path of the app's private key (.pem) on the factory's machine")
	}
	keyPath = expandPath(keyPath)
	key, err := github.LoadAppKey(keyPath)
	if err != nil {
		return "", fmt.Errorf("can't use that key: %w", err)
	}
	app := &github.AppTokens{AppID: id, Key: key, BaseURL: githubAPI}
	first, missing, err := installations(ctx, app, githubRepos(w.config()))
	if err != nil {
		return "", fmt.Errorf("GitHub refused that app ID and key: %w", err)
	}
	set := map[string]string{"FACTORY_GITHUB_APP_ID": id, "FACTORY_GITHUB_APP_KEY": keyPath}
	drop := []string{"GITHUB_TOKEN", "FACTORY_GITHUB_APP_SLUG", "FACTORY_GITHUB_INSTALLATION_ID"}
	if len(missing) == 0 {
		set["FACTORY_GITHUB_INSTALLATION_ID"], drop = first, drop[:2]
	}
	if err := w.save(set, drop...); err != nil {
		return "", err
	}
	w.mu.Lock()
	w.missing, w.instErr = missing, ""
	w.mu.Unlock()
	w.credentialsChanged()
	if len(missing) > 0 {
		return "Saved. The app isn't installed on " + joinOr(missing) + ": install it from the app's settings page (Install App), then press Check.", nil
	}
	w.checkAccess(ctx)
	return "Saved the app. " + w.accessMessage(), nil
}

// useBranch sets the one repo's base_branch to its default branch.
func (w *wizard) useBranch(ctx context.Context, cfg *config.Config, repo, branch string) (string, error) {
	if len(cfg.Repos) != 1 || !strings.EqualFold(cfg.Repos[0].Name, repo) {
		return "", fmt.Errorf("with several repos, give each its own base_branch under repos: in %s", w.cfgPath)
	}
	// Only the branch the access check found the repo's default, so the
	// page can't write anything else into factory.yaml.
	w.mu.Lock()
	def := w.access[cfg.Repos[0].Name].DefaultBranch
	w.mu.Unlock()
	if def == "" {
		return "", errors.New("check access first, so the factory knows the repo's default branch")
	}
	if branch != def {
		return "", fmt.Errorf("only the repo's default branch, %s, can be picked here; set base_branch in %s for another", def, w.cfgPath)
	}
	if err := setYAML(w.cfgPath, "base_branch", branch); err != nil {
		return "", fmt.Errorf("couldn't update it: %w. Set base_branch in %s yourself", err, w.cfgPath)
	}
	if err := w.reload(); err != nil {
		return "", err
	}
	w.checkAccess(ctx)
	return "The factory now works on " + branch + ".", nil
}

// createLabels makes the labels the factory uses on each repo it can
// reach, leaving ones already there as they are.
func (w *wizard) createLabels(ctx context.Context, cfg *config.Config) (string, error) {
	client := w.clients(cfg, w.vals())
	if client == nil {
		return "", errors.New("connect the factory to your repos first")
	}
	var out []web.APIWizardLabel
	created, failed := 0, 0
	labels := pipeline.Labels(cfg.Intake.Label)
	for _, repo := range cfg.RepoNames() {
		gh, _, cerr := client(repo)
		// Only the missing ones are made; if the list can't be read, each
		// is looked up on its own.
		var missing []string
		listed := false
		if cerr == nil {
			var lerr string
			missing, lerr = missingLabels(ctx, gh, labels)
			listed = lerr == ""
		}
		for _, l := range labels {
			r := web.APIWizardLabel{Repo: repo, Name: l.Name, Result: "exists"}
			made, err := false, cerr
			if err == nil && (!listed || slices.Contains(missing, l.Name)) {
				made, err = gh.EnsureLabel(ctx, l.Name, l.Color, l.Description)
			}
			switch {
			case err != nil:
				r.Result, r.Error = "failed", err.Error()
				failed++
			case made:
				r.Result = "created"
				created++
			}
			out = append(out, r)
		}
	}
	w.mu.Lock()
	w.labels = out
	w.mu.Unlock()
	w.checkAccess(ctx)
	msg := fmt.Sprintf("Created %s; the rest were already there.", plural(created, "label", "labels"))
	if failed > 0 {
		msg = fmt.Sprintf("Created %s; %s failed.", plural(created, "label", "labels"), plural(failed, "label", "labels"))
	}
	return msg, nil
}

// saveClaude saves an API key or subscription token in its variable,
// clearing the other, as setupClaude does.
func (w *wizard) saveClaude(v string, anyway bool) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", errors.New("paste an API key or a subscription token first")
	}
	k := claudeKey(v)
	if k == "" && !anyway {
		return "", errors.New("that doesn't look like an Anthropic key (they start with sk-ant-); check it, or save it anyway")
	}
	if k == "" {
		k = "ANTHROPIC_API_KEY"
	}
	other := "CLAUDE_CODE_OAUTH_TOKEN"
	if k == other {
		other = "ANTHROPIC_API_KEY"
	}
	if err := w.save(map[string]string{k: v}, other); err != nil {
		return "", err
	}
	if k == "CLAUDE_CODE_OAUTH_TOKEN" {
		return "Saved the subscription token.", nil
	}
	return "Saved the API key.", nil
}

// saveCodex saves how Codex signs in, clearing the keys Codex would use
// instead, as setupCodex does. An empty key keeps the saved one.
func (w *wizard) saveCodex(cfg *config.Config, mode, key string) (string, error) {
	vals := w.vals()
	key = strings.TrimSpace(key)
	switch mode {
	case codexAPIKey:
		v := firstOf(key, vals["OPENAI_API_KEY"], vals["CODEX_API_KEY"])
		set := map[string]string{}
		if v != "" {
			set["OPENAI_API_KEY"] = v
		}
		if err := w.save(set, codexClears[codexAPIKey]...); err != nil {
			return "", err
		}
		if v == "" {
			return "Without a key, Codex jobs fail until you add one or switch to Claude-only review.", nil
		}
		return "Saved the API key.", nil
	case codexChatGPT:
		if err := w.save(map[string]string{"FACTORY_CODEX_AUTH": codexChatGPT}, codexClears[codexChatGPT]...); err != nil {
			return "", err
		}
		if b, err := os.ReadFile(codexAuthFile(cfg)); err == nil && harness.ValidAuth(b) {
			return "Saved. Codex is signed in to ChatGPT.", nil
		}
		return "Saved. Now sign in with ChatGPT.", nil
	case codexAccessToken:
		v := firstOf(key, vals["CODEX_ACCESS_TOKEN"])
		if v == "" {
			return "", errors.New("paste the access token first")
		}
		if err := w.save(map[string]string{"CODEX_ACCESS_TOKEN": v}, codexClears[codexAccessToken]...); err != nil {
			return "", err
		}
		return "Saved the access token.", nil
	}
	return "", errors.New("pick an API key, a ChatGPT plan or an access token")
}

// buildImages builds the missing job images in the background, on the
// builder serve shares, so an image is never built twice at once.
func (w *wizard) buildImages(cfg *config.Config) (string, error) {
	if cfg.Runner.Kind != "docker" {
		return "", errors.New("jobs don't run in images with this runner")
	}
	n, err := w.builder.start(cfg, nil)
	if err != nil {
		return "", err
	}
	if n == 0 {
		w.checkMachine(w.bg)
		return "Every job image is built.", nil
	}
	return "Building. It takes a few minutes per image.", nil
}

// startCodexSignIn runs `codex login --device-auth` in the background, as
// `factory codex-login` does. Codex prints a link and a code, which the
// page shows; once someone enters the code, the sign-in is saved and the
// factory set to the ChatGPT plan.
func (w *wizard) startCodexSignIn(cfg *config.Config) (string, error) {
	w.mu.Lock()
	running := w.signIn.running
	w.mu.Unlock()
	if running {
		return "", errors.New("a sign-in is already running: open the link and enter the code")
	}
	tmp, err := codexLoginDir(cfg)
	if err != nil {
		return "", err
	}
	name, args, container := "env", []string{"CODEX_HOME=" + tmp, "codex", "login", "--device-auth"}, ""
	if cfg.Runner.Kind == "docker" {
		// Named, so Cancel can stop the container and not only the client.
		name, container = "docker", fmt.Sprintf("factory-codex-login-%d-%d", os.Getpid(), time.Now().UnixNano())
		if args, err = dockerLoginArgs(cfg, tmp, false, container); err != nil {
			os.RemoveAll(tmp)
			return "", fmt.Errorf("%w; build it in the job images step first", err)
		}
	}
	w.mu.Lock()
	if w.signIn.running {
		w.mu.Unlock()
		os.RemoveAll(tmp)
		return "", errors.New("a sign-in is already running: open the link and enter the code")
	}
	// The daemon's context, so it stops with the daemon too, and a
	// deadline, so a code nobody enters doesn't hold a restart for long.
	ctx, cancel := context.WithTimeout(w.bg, signInWait)
	log := &tailBuffer{max: 8 << 10}
	w.signIn = pageSignIn{running: true, log: log, cancel: cancel}
	w.signInDone.Add(1)
	w.mu.Unlock()
	go func() {
		defer w.signInDone.Done()
		defer os.RemoveAll(tmp)
		defer cancel()
		runErr := runCmdContext(ctx, log, name, args...)
		// Codex that finished before a Cancel landed is a sign-in all the same.
		stopped := runErr != nil && ctx.Err() != nil
		if stopped && container != "" {
			removeContainer(container)
		}
		res, err := "", error(nil)
		if !stopped {
			res, err = w.finishCodexSignIn(runErr, filepath.Join(tmp, "auth.json"), cfg)
		}
		w.mu.Lock()
		defer w.mu.Unlock()
		cancelled := w.signIn.cancelled
		w.signIn.running, w.signIn.cancel, w.signIn.cancelled = false, nil, false
		switch {
		case stopped && cancelled:
			w.signIn.result = "Sign-in cancelled."
		case stopped && errors.Is(ctx.Err(), context.DeadlineExceeded):
			w.signIn.result, w.signIn.failed = fmt.Sprintf("The sign-in timed out: nobody entered the code within %s. Start it again.", signInWait.Round(time.Minute)), true
		case stopped:
			w.signIn.result, w.signIn.failed = "The sign-in stopped because the factory is shutting down.", true
		case err != nil:
			w.signIn.result, w.signIn.failed = err.Error(), true
		default:
			w.signIn.result = res
		}
	}()
	return "Started. Open the link and enter the code that appear here.", nil
}

// cancelCodexSignIn stops a running sign-in. Its folder is removed once
// Codex has stopped.
func (w *wizard) cancelCodexSignIn() (string, error) {
	w.mu.Lock()
	cancel := w.signIn.cancel
	if !w.signIn.running || cancel == nil {
		w.mu.Unlock()
		return "", errors.New("no sign-in is running")
	}
	w.signIn.cancelled = true
	w.mu.Unlock()
	cancel()
	w.signInDone.Wait()
	// It may have finished first: say what happened, failure included.
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.signIn.failed {
		return "", errors.New(w.signIn.result)
	}
	return w.signIn.result, nil
}

// signInWait is how long a sign-in waits for its code to be entered.
var signInWait = 20 * time.Minute

// rmRetries and rmRetryWait bound removing a sign-in's container: one
// cancelled just as Docker made it may not be there on the first try.
var (
	rmRetries   = 5
	rmRetryWait = time.Second
)

// removeContainer removes a container, trying again a few times while
// Docker says there's no such container yet.
func removeContainer(name string) {
	for i := 0; i < rmRetries; i++ {
		out, err := runOutput(context.Background(), "docker", "rm", "-f", name)
		if err == nil && !strings.Contains(out, "No such container") {
			return
		}
		time.Sleep(rmRetryWait)
	}
}

// signingIn says whether a sign-in from the page is running.
func (w *wizard) signingIn() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.signIn.running
}

// busy says what a restart now would cut off, or "".
func (w *wizard) busy() string {
	switch w.waitingOn() {
	case waitBuild:
		return "A job image is being built. Finish once it's done, so the restart doesn't cut it off."
	case waitSignIn:
		return "A Codex sign-in is running. Finish it or cancel it first."
	}
	return ""
}

// What a restart waits for, besides jobs.
const (
	waitBuild  = "the job image build"
	waitSignIn = "the Codex sign-in"
)

// waitingOn names the work a restart must wait for, or "".
func (w *wizard) waitingOn() string {
	if running, _, _, _ := w.builder.status(); running {
		return waitBuild
	}
	if w.signingIn() {
		return waitSignIn
	}
	return ""
}

// finishCodexSignIn saves the sign-in Codex left in auth, as
// `factory codex-login` does.
func (w *wizard) finishCodexSignIn(runErr error, auth string, cfg *config.Config) (string, error) {
	b, rerr := os.ReadFile(auth)
	if runErr == nil && rerr == nil && harness.ValidAuth(b) {
		if err := harness.SaveAuth(codexAuthFile(cfg), b); err != nil {
			return "", fmt.Errorf("couldn't save the sign-in: %w", err)
		}
		if err := w.save(map[string]string{"FACTORY_CODEX_AUTH": codexChatGPT}, codexClears[codexChatGPT]...); err != nil {
			return "", err
		}
		return "Signed in. Codex uses your ChatGPT plan.", nil
	}
	if runErr == nil {
		runErr = errors.New("codex didn't save a sign-in")
	}
	return "", fmt.Errorf("sign-in failed: %w. Turn on device code sign-in in ChatGPT's security settings (or your workspace's) if Codex says it's off, then try again", runErr)
}

var (
	ansiRE       = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)
	signInURLRE  = regexp.MustCompile(`https://[^\s"'<>]+`)
	signInCodeRE = regexp.MustCompile(`\b[A-Z0-9]{4,}-[A-Z0-9]{4,}\b`)
)

// signInState is the page's view of the sign-in: the link and code Codex
// printed, picked out of its output. w.mu is held.
func (w *wizard) signInState() web.APIWizardSignIn {
	si := w.signIn
	out := web.APIWizardSignIn{Running: si.running, Result: si.result, Failed: si.failed}
	if si.log == nil {
		return out
	}
	out.Log = ansiRE.ReplaceAllString(si.log.String(), "")
	out.URL = signInURLRE.FindString(out.Log)
	out.Code = signInCodeRE.FindString(out.Log)
	return out
}

// finishSetup starts the factory: in setup mode by ending the setup
// server, otherwise by restarting once running jobs finish, if anything
// changed.
func (w *wizard) finishSetup(ctx context.Context) (string, error) {
	if why := w.busy(); why != "" {
		return "", errors.New(why)
	}
	w.checkAccess(ctx)
	vals := w.vals()
	w.mu.Lock()
	st := w.stateLocked(vals)
	w.mu.Unlock()
	if !st.CanFinish {
		return "", errors.New("the factory can't reach every repo yet: " + strings.TrimSuffix(w.accessMessage(), "."))
	}

	if w.setupMode {
		w.finish.Do(func() { close(w.done) })
		return "Starting the factory. This page opens the dashboard when it's up.", nil
	}
	if !st.Changed {
		return "Nothing changed, so there's nothing to restart. You're set.", nil
	}
	if w.restart == nil {
		return "Saved. Restart the factory to apply it.", nil
	}
	switch n := w.restart(ctx); n {
	case 0:
		return "Restarting now to apply the changes.", nil
	case 1:
		return "Restarting once the running job finishes. No new jobs start until then.", nil
	default:
		return fmt.Sprintf("Restarting once %d running jobs finish. No new jobs start until then.", n), nil
	}
}

func joinOr(s []string) string {
	j := joinAnd(s)
	if i := strings.LastIndex(j, " and "); i >= 0 {
		return j[:i] + " or " + j[i+5:]
	}
	return j
}

// tailBuffer keeps the end of what is written to it.
type tailBuffer struct {
	mu  sync.Mutex
	max int
	b   []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, p...)
	if over := len(t.b) - t.max; over > 0 {
		t.b = append([]byte(nil), t.b[over:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.b)
}

var _ web.Wizard = (*wizard)(nil)

// gateOrder is the order checks run and are shown in.
var gateOrder = []string{"types", "lint", "test", "build", "audit"}

// byGateOrder sorts gate names in gateOrder, then the rest by name.
func byGateOrder(a, b string) int {
	ia, ib := slices.Index(gateOrder, a), slices.Index(gateOrder, b)
	if ia < 0 {
		ia = len(gateOrder)
	}
	if ib < 0 {
		ib = len(gateOrder)
	}
	if ia != ib {
		return ia - ib
	}
	return strings.Compare(a, b)
}

// wizardChecks is a repo's checks for the page: the saved ones, each with
// the suggestion beside it when that differs, or the suggestion.
func wizardChecks(r config.Repo, s *stack.Suggestion) web.APIWizardChecks {
	c := web.APIWizardChecks{Repo: r.Name, Saved: r.Gates != nil, ImageFrom: r.ImageFrom, Gates: []web.APIWizardGate{}, Notes: []string{}}
	sug := map[string]string{}
	if s != nil {
		c.Stack, sug = s.Stack, s.Commands
		c.Notes = append(c.Notes, s.Notes...)
		if !c.Saved {
			c.ImageFrom, c.Install = s.ImageFrom, s.Install
		} else if s.Install != r.Gates.Install {
			c.SuggestedInstall = s.Install
		}
	}
	saved := map[string]string{}
	if c.Saved {
		c.Install, saved = r.Gates.Install, r.Gates.Commands
	}
	var names []string
	for _, m := range []map[string]string{saved, sug} {
		for g := range m {
			if !slices.Contains(names, g) {
				names = append(names, g)
			}
		}
	}
	slices.SortFunc(names, byGateOrder)
	for _, g := range names {
		row := web.APIWizardGate{Gate: g, Command: sug[g]}
		if s != nil {
			row.Source = s.Sources[g]
		}
		if c.Saved {
			row.Command = saved[g]
			if sug[g] != row.Command {
				row.Suggested = sug[g]
			}
		}
		c.Gates = append(c.Gates, row)
	}
	// The services: the saved ones, then the suggested it lacks.
	c.Services, c.Env = []web.APIWizardService{}, map[string]string{}
	for _, n := range slices.Sorted(maps.Keys(r.Services)) {
		c.Services = append(c.Services, web.APIWizardService{Name: n, Image: r.Services[n].Image, Saved: true})
	}
	maps.Copy(c.Env, r.Env)
	if s != nil {
		for _, n := range slices.Sorted(maps.Keys(s.Services)) {
			if _, ok := r.Services[n]; !ok {
				c.Services = append(c.Services, web.APIWizardService{Name: n, Image: s.Services[n].Image})
			}
		}
		for k, v := range s.Env {
			if _, ok := c.Env[k]; !ok {
				c.Env[k] = v
			}
		}
	}
	return c
}

// applyChecks is repo r with the install, base image, checks, services and
// environment on the page: what checks/save writes and checks/try runs.
// A service is one shown on the page: saved, or suggested in s.
func applyChecks(r config.Repo, in web.WizardInput, s *stack.Suggestion) (config.Repo, error) {
	var g config.Gates
	if r.Gates != nil {
		g = *r.Gates // keeps what the page doesn't show, such as the diff guard
	} else if s == nil || !s.Go {
		// proves_fix and coverage are Go-only for now.
		g.ProvesFix, g.Coverage = "none", "none"
	}
	g.Install = strings.TrimSpace(in.Install)
	g.Commands, g.Required = map[string]string{}, nil
	for name, cmd := range in.Commands {
		if cmd = strings.TrimSpace(cmd); cmd != "" {
			g.Commands[name] = cmd
			g.Required = append(g.Required, name)
		}
	}
	slices.SortFunc(g.Required, byGateOrder)
	if len(g.Required) == 0 {
		return r, errors.New("give at least one check: a change that runs none is checked by nothing")
	}
	svcs := map[string]config.Service{}
	for _, n := range in.Services {
		svc, ok := r.Services[n]
		if !ok && s != nil {
			svc, ok = s.Services[n]
		}
		if !ok {
			return r, fmt.Errorf("no service %q to start: pick from the ones shown", n)
		}
		svcs[n] = svc
	}
	r.Services = nil
	if len(svcs) > 0 {
		r.Services = svcs
	}
	r.Env = nil
	for k, v := range in.Env {
		if k = strings.TrimSpace(k); k != "" {
			if r.Env == nil {
				r.Env = map[string]string{}
			}
			r.Env[k] = strings.TrimSpace(v)
		}
	}
	r.Gates, r.ImageFrom = &g, strings.TrimSpace(in.ImageFrom)
	return r, nil
}

// saveChecks writes a repo's checks, as the person confirmed them on the
// page, to factory.yaml.
func (w *wizard) saveChecks(ctx context.Context, cfg *config.Config, in web.WizardInput) (string, error) {
	i := slices.IndexFunc(cfg.Repos, func(r config.Repo) bool { return strings.EqualFold(r.Name, in.Repo) })
	if i < 0 {
		return "", fmt.Errorf("%s isn't one of the factory's repos", in.Repo)
	}
	w.mu.Lock()
	s := w.detected[cfg.Repos[i].Name]
	w.mu.Unlock()
	r, err := applyChecks(cfg.Repos[i], in, s)
	if err != nil {
		return "", err
	}
	repos := slices.Clone(cfg.Repos)
	repos[i] = r
	old, err := os.ReadFile(w.cfgPath)
	if err != nil {
		return "", err
	}
	if err := writeRepos(w.cfgPath, repos); err != nil {
		return "", err
	}
	if err := w.reload(); err != nil {
		// What was saved doesn't load: put the file back.
		_ = os.WriteFile(w.cfgPath, old, 0o644)
		_ = w.reload()
		return "", fmt.Errorf("those checks aren't valid: %w", err)
	}
	// Reloading drops the last access check; the page shouldn't go back
	// to "not checked".
	w.checkAccess(ctx)
	return fmt.Sprintf("Saved %s's checks to %s: %s.", r.Name, filepath.Base(w.cfgPath), strings.Join(r.Gates.Required, ", ")), nil
}

// tryChecks starts a try of a repo's checks as on the page, saved or not,
// in the background: the page follows it.
func (w *wizard) tryChecks(cfg *config.Config, in web.WizardInput) (string, error) {
	i := slices.IndexFunc(cfg.Repos, func(r config.Repo) bool { return strings.EqualFold(r.Name, in.Repo) })
	if i < 0 {
		return "", fmt.Errorf("%s isn't one of the factory's repos", in.Repo)
	}
	name := cfg.Repos[i].Name
	w.mu.Lock()
	s := w.detected[name]
	busy := w.tries[name] != nil && w.tries[name].Running
	w.mu.Unlock()
	if busy {
		return "", fmt.Errorf("%s's checks are being tried already; wait for them", name)
	}
	r, err := applyChecks(cfg.Repos[i], in, s)
	if err != nil {
		return "", err
	}
	// The saved job image: a new base needs a save and a build first.
	r.ImageFrom = cfg.Repos[i].ImageFrom
	c := *cfg
	c.Repos = slices.Clone(cfg.Repos)
	c.Repos[i] = r
	t := &web.APIWizardTry{Running: true, Since: time.Now().UTC().Format(time.RFC3339), Steps: []web.APIWizardTryStep{}}
	if r.Gates.Install != "" {
		t.Steps = append(t.Steps, web.APIWizardTryStep{Gate: "install", Command: r.Gates.Install, State: "waiting"})
	}
	for _, g := range r.Gates.Required {
		t.Steps = append(t.Steps, web.APIWizardTryStep{Gate: g, Command: r.Gates.Commands[g], State: "waiting"})
	}
	w.mu.Lock()
	if w.tries == nil {
		w.tries = map[string]*web.APIWizardTry{}
	}
	w.tries[name] = t
	w.mu.Unlock()
	client := w.clients(&c, w.vals())
	go func() {
		err := tryRun(w.bg, &c, name, client, func(f func(*web.APIWizardTry)) {
			w.mu.Lock()
			defer w.mu.Unlock()
			f(t)
		})
		w.mu.Lock()
		defer w.mu.Unlock()
		t.Running = false
		if err != nil {
			t.Error = err.Error()
		}
		for i := range t.Steps {
			if t.Steps[i].State == "waiting" || t.Steps[i].State == "running" {
				t.Steps[i].State = "skipped"
			}
		}
	}()
	return "Trying " + name + "'s checks: it clones the base branch, starts the services and runs each command. It takes a few minutes; this page follows it.", nil
}

// tryRun is runTry; tests replace it.
var tryRun = runTry

// runTry clones repo's base branch, starts its services and runs the
// install and each check once, telling update as each starts and ends.
// An install that fails stops it: nothing else can run.
func runTry(ctx context.Context, cfg *config.Config, repo string, client clientFor, update func(func(*web.APIWizardTry))) error {
	if client == nil {
		return errors.New("connect the factory to the repo first")
	}
	rn, err := newRunner(cfg, "")
	if err != nil {
		return err
	}
	pl := &pipeline.Pipeline{Cfg: cfg, Runner: rn, JobPrefix: readinessPrefix(), ClientsFor: func(repo string) pipeline.RepoClients {
		gh, ts, err := client(repo)
		if err != nil {
			gh, ts = newForge(cfg, repo, github.StaticToken("")), nil
		}
		return pipeline.RepoClients{GH: gh, Git: repoGit(cfg, repo, ts)}
	}}
	rc, _ := cfg.RepoConf(repo)
	data := cfg.Path(cfg.DataDir)
	if err := os.MkdirAll(data, 0o755); err != nil {
		return err
	}
	dir, err := os.MkdirTemp(data, "readiness-try-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	g := pl.GitFor(repo)
	ws, err := g.Prepare(ctx, dir, rc.BaseBranch, "", false, false)
	if err != nil {
		return fmt.Errorf("cloning %s: %w", repo, err)
	}
	export := filepath.Join(dir, "export")
	if err := g.Export(ctx, ws.Repo, ws.BaseSHA, export); err != nil {
		return err
	}
	if err := pl.CopyFiles(ctx, repo, export); err != nil {
		return err
	}
	stop, err := pl.StartServices(ctx, 0, repo)
	defer stop()
	if err != nil {
		return fmt.Errorf("starting its services: %w", err)
	}
	var steps []web.APIWizardTryStep
	update(func(t *web.APIWizardTry) { steps = slices.Clone(t.Steps) })
	for i, st := range steps {
		update(func(t *web.APIWizardTry) { t.Steps[i].State = "running" })
		start := time.Now()
		code, out, err := pl.Exec(ctx, repo, export, st.Command, st.Gate == "install")
		state := "pass"
		if err != nil || code != 0 {
			state = "fail"
			if err != nil {
				out += "\n" + err.Error()
			}
		}
		update(func(t *web.APIWizardTry) {
			t.Steps[i].State, t.Steps[i].Seconds, t.Steps[i].Output = state, int(time.Since(start).Seconds()), tailLines(out, 60)
		})
		if state == "fail" && st.Gate == "install" {
			return nil // the rest need what it installs
		}
	}
	return nil
}

// tailLines is the last n lines of s.
func tailLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = append([]string{"…"}, lines[len(lines)-n:]...)
	}
	return strings.Join(lines, "\n")
}
