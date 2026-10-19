package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"

	"golang.org/x/term"
	"gopkg.in/yaml.v3"

	"github.com/claptimes5/factory/internal/config"
	"github.com/claptimes5/factory/internal/github"
	"github.com/claptimes5/factory/internal/notify"
	"github.com/claptimes5/factory/internal/pipeline"
)

// envKeys are the settings `factory setup` asks for, in file order.
var envKeys = []string{
	"FACTORY_GITHUB_APP_ID", "FACTORY_GITHUB_INSTALLATION_ID", "FACTORY_GITHUB_APP_KEY", "GITHUB_TOKEN", "GITLAB_TOKEN",
	"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "OPENAI_API_KEY", "CODEX_API_KEY", "FACTORY_CODEX_AUTH", "CODEX_ACCESS_TOKEN",
	"FACTORY_NOTIFY_URL", "SENTRY_AUTH_TOKEN",
}

// prompter asks questions on a terminal, or reads answers line by line
// from a pipe.
type prompter struct {
	in  *bufio.Reader
	out io.Writer
	// hidden reads a line without echoing it; nil reads it like any other.
	hidden func() (string, error)
}

func newPrompter() *prompter {
	p := &prompter{in: bufio.NewReader(os.Stdin), out: os.Stdout}
	if fd := int(os.Stdin.Fd()); term.IsTerminal(fd) {
		// Ctrl-C exits right away, turning echo back on if a hidden prompt
		// had it off. Each section is saved as it finishes.
		if st, err := term.GetState(fd); err == nil {
			sig := make(chan os.Signal, 1)
			signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
			go func() {
				<-sig
				_ = term.Restore(fd, st)
				fmt.Fprintln(p.out, "\nStopped.")
				os.Exit(130)
			}()
		}
		p.hidden = func() (string, error) {
			b, err := term.ReadPassword(fd)
			fmt.Fprintln(p.out)
			return string(b), err
		}
	}
	return p
}

// errInputClosed stops setup when its input ends, so a closed or piped
// stdin can't leave a "try again?" loop spinning.
var errInputClosed = errors.New("input ended before setup finished; run `factory setup` in a terminal")

type inputClosed struct{}

func (p *prompter) line() string {
	s, err := p.in.ReadString('\n')
	if err != nil && s == "" {
		panic(inputClosed{})
	}
	return strings.TrimSpace(s)
}

func (p *prompter) say(f string, a ...any) { fmt.Fprintf(p.out, f+"\n", a...) }

// ask returns the answer, or def when the answer is empty.
func (p *prompter) ask(label, def string) string {
	if def != "" {
		fmt.Fprintf(p.out, "%s [%s]: ", label, def)
	} else {
		fmt.Fprintf(p.out, "%s: ", label)
	}
	if s := p.line(); s != "" {
		return s
	}
	return def
}

// secret is ask without echo, showing only a hint of the saved value.
func (p *prompter) secret(label, def string) string {
	if def != "" {
		fmt.Fprintf(p.out, "%s [current %s, Enter keeps it, - clears it]: ", label, mask(def))
	} else {
		fmt.Fprintf(p.out, "%s: ", label)
	}
	s := ""
	if p.hidden != nil {
		var err error
		if s, err = p.hidden(); err != nil {
			panic(inputClosed{})
		}
		s = strings.TrimSpace(s)
	} else {
		s = p.line()
	}
	switch s {
	case "":
		return def
	case "-":
		return ""
	}
	return s
}

func (p *prompter) yes(q string, def bool) bool {
	hint := "Y/n"
	if !def {
		hint = "y/N"
	}
	fmt.Fprintf(p.out, "%s [%s] ", q, hint)
	switch strings.ToLower(p.line()) {
	case "":
		return def
	case "y", "yes":
		return true
	}
	return false
}

func mask(s string) string {
	if len(s) <= 12 {
		return "value"
	}
	return s[:4] + "…" + s[len(s)-4:]
}

// githubAPI is the API root setup talks to; tests point it elsewhere.
var githubAPI = ""

// runCmd runs a command with its output on the terminal; tests replace it.
var runCmd = func(out io.Writer, name string, args ...string) error {
	c := exec.Command(name, args...)
	c.Stdout, c.Stderr = out, out
	return c.Run()
}

// setup walks through everything the first run needs, checking each answer
// as it goes, and saves the settings to envPath. Running it again keeps the
// saved answers as defaults, so it is also how you change one.
func setup(ctx context.Context, cfgPath string, cfg *config.Config, envPath string, p *prompter) (err error) {
	defer func() {
		if r := recover(); r != nil {
			if _, ok := r.(inputClosed); !ok {
				panic(r)
			}
			p.say("")
			err = errInputClosed
		}
	}()
	if envPath, err = filepath.Abs(envPath); err != nil {
		return err
	}
	vals, err := readEnvFile(envPath)
	if err != nil {
		return err
	}
	for _, k := range envKeys {
		// A GITHUB_TOKEN exported for other tools (gh, for one) is usually a
		// broad personal token, so it is never picked up without asking.
		if _, ok := vals[k]; !ok && k != "GITHUB_TOKEN" {
			vals[k] = os.Getenv(k)
		}
	}
	save := func() error {
		return writeEnvFile(envPath, "# Written by `factory setup`. Values set in the environment override these.\n", envKeys, vals)
	}
	p.say("Factory setup. Your answers are saved to %s, readable only by you.", envPath)
	p.say("Press Enter to keep a value shown in [brackets]. Each section is saved")
	p.say("as you finish it; Ctrl-C stops, and running setup again starts from your answers.")
	checkMachine(ctx, p, cfg)

	p.say("\n== 1. Repositories")
	p.say("The factory can work on several repos, each on its own. List them separated by commas:")
	p.say("owner/name for GitHub, and the project's URL for GitLab (https://gitlab.com/group/project).")
	for {
		names, err := parseRepoList(p.ask("Repositories", strings.Join(cfg.RepoNames(), ", ")))
		if err != nil {
			p.say("%v", err)
			continue
		}
		if !slices.Equal(names, cfg.RepoNames()) {
			if err := setRepos(cfgPath, names); err != nil {
				return err
			}
			c, err := config.Load(cfgPath)
			if err != nil {
				return err
			}
			*cfg = *c
			p.say("Updated %s.", cfgPath)
		}
		break
	}

	if cfg.Runner.Kind == "docker" {
		p.say("\n== 2. Docker")
		if err := checkDocker(ctx, p, cfg); err != nil {
			return err
		}
	}

	var ghClient, glClient clientFor
	if len(githubRepos(cfg)) > 0 {
		p.say("\n== 3. GitHub access")
		if ghClient, err = setupGitHub(ctx, p, cfg, vals, filepath.Dir(envPath)); err != nil {
			return err
		}
		if err := save(); err != nil {
			return err
		}
	}
	if len(gitlabRepos(cfg)) > 0 {
		header := "\n== 3. GitLab access"
		if ghClient != nil || len(githubRepos(cfg)) > 0 {
			header = "\n== 3b. GitLab access"
		}
		p.say("%s", header)
		glClient = setupGitLab(ctx, p, cfg, vals)
		if err := save(); err != nil {
			return err
		}
	}
	client := bothHosts(cfg, ghClient, glClient)
	if client != nil {
		checkBaseBranches(ctx, p, cfgPath, cfg, client)
		labels := pipeline.Labels(cfg.Intake.Label)
		p.say("\nThe factory uses %d labels on each repo, such as %s.", len(labels), labels[0].Name)
		if p.yes("Create the ones that are missing? Existing labels are left as they are.", true) {
			for _, repo := range cfg.RepoNames() {
				gh, _, err := client(repo)
				if err != nil {
					p.say("%s: %v", repo, err)
					continue
				}
				for _, l := range labels {
					created, err := gh.EnsureLabel(ctx, l.Name, l.Color, l.Description)
					switch {
					case err != nil:
						p.say("  %s %s: %v", repo, l.Name, err)
					case created:
						p.say("  %s %s: created", repo, l.Name)
					default:
						p.say("  %s %s: already there", repo, l.Name)
					}
				}
			}
		}
	}

	if client != nil {
		p.say("\n== 4. Checks for each repo")
		p.say("Each repo's changes must pass its checks (install, lint, test, build) before review.")
		if err := setupChecks(ctx, p, cfgPath, cfg, client); err != nil {
			return err
		}
	}
	images := true
	if cfg.Runner.Kind == "docker" {
		p.say("\n== 5. Job images")
		images = setupImages(p, cfg)
	}

	if client != nil {
		p.say("\n== 6. How ready each repo is for the factory")
		if images {
			setupReadiness(ctx, p, cfg, client)
		} else {
			p.say("The check runs in the job images, so it waits until they're built.")
			p.say("Run it then with: ./factory readiness")
		}
	}

	// A Codex-only factory needs no Claude sign-in.
	if usesHarness(cfg, "claude") {
		p.say("\n== 7. Claude (writes the code)")
		p.say("Paste an API key from https://console.anthropic.com/settings/keys, or, to use")
		p.say("a Claude subscription, run `claude setup-token` and paste the token it prints.")
		setupClaude(p, vals)
	}
	if err := save(); err != nil {
		return err
	}

	if usesHarness(cfg, "codex") {
		p.say("\n== 8. OpenAI (Codex)")
		setupCodex(p, cfg, vals)
		if err := save(); err != nil {
			return err
		}
	}

	p.say("\n== 9. Phone notifications (optional)")
	p.say("An ntfy topic URL (https://ntfy.sh/<topic>), or a Slack or Discord webhook URL.")
	for {
		// Webhook URLs work as passwords, so this is read like a key.
		u := p.secret("Notification URL (Enter to skip)", vals["FACTORY_NOTIFY_URL"])
		if _, err := notify.New(u); err != nil {
			p.say("%v", err)
			continue
		}
		vals["FACTORY_NOTIFY_URL"] = u
		break
	}

	if cfg.Sources.Sentry.Enabled {
		p.say("\n== 10. Sentry (optional)")
		p.say("sources.sentry files an issue for each unresolved Sentry error. It reads Sentry with")
		p.say("an auth token: an internal integration's, or a personal one, with the project:read")
		p.say("and event:read scopes (Settings > Developer Settings on your Sentry).")
		vals["SENTRY_AUTH_TOKEN"] = p.secret("Sentry auth token (Enter to skip)", vals["SENTRY_AUTH_TOKEN"])
		if vals["SENTRY_AUTH_TOKEN"] == "" {
			p.say("Without it the factory files nothing from Sentry, and tells you so once.")
		}
	}

	if err := save(); err != nil {
		return err
	}
	p.say("\nAll set. Saved %s.", envPath)
	p.say("Start the factory with:\n\n    ./factory serve\n")
	p.say("Then open http://%s/app/ and label an issue on %s with %s.", cfg.Web.Listen, strings.Join(cfg.RepoNames(), " or "), cfg.Intake.Label)
	return nil
}

// setupClaude asks for an Anthropic API key or a subscription token.
func setupClaude(p *prompter, vals map[string]string) {
	for {
		v := p.secret("Anthropic API key or subscription token", firstOf(vals["ANTHROPIC_API_KEY"], vals["CLAUDE_CODE_OAUTH_TOKEN"]))
		vals["ANTHROPIC_API_KEY"], vals["CLAUDE_CODE_OAUTH_TOKEN"] = "", ""
		switch {
		case strings.HasPrefix(v, "sk-ant-oat"):
			vals["CLAUDE_CODE_OAUTH_TOKEN"] = v
		case strings.HasPrefix(v, "sk-ant-"):
			vals["ANTHROPIC_API_KEY"] = v
		case v == "":
			p.say("factory.yaml routes jobs to Claude, so they can't run without one.")
			if p.yes("Skip it for now?", false) {
				break
			}
			continue
		default:
			p.say("That doesn't look like an Anthropic key (they start with sk-ant-).")
			if !p.yes("Use it anyway?", false) {
				continue
			}
			vals["ANTHROPIC_API_KEY"] = v
		}
		break
	}
}

// setupCodex asks how Codex signs in: an API key, a ChatGPT plan, or a
// Business or Enterprise access token.
func setupCodex(p *prompter, cfg *config.Config, vals map[string]string) {
	if usesHarness(cfg, "claude") {
		p.say("factory.yaml has Codex review Claude's work. Codex can sign in with:")
	} else {
		p.say("factory.yaml runs every job on Codex. Codex can sign in with:")
	}
	p.say("  1. An OpenAI API key, billed per use. OpenAI recommends this for automation.")
	p.say("  2. A ChatGPT plan (Plus, Pro, Business or Enterprise), using the plan's limits.")
	p.say("  3. A Codex access token from a ChatGPT Business or Enterprise workspace.")
	p.say("No OpenAI account? Choose 1, leave the key empty, and see")
	p.say("docs/configuration.md#claude-only-review.")
	def := "1"
	switch {
	case vals["CODEX_ACCESS_TOKEN"] != "":
		def = "3"
	case vals["FACTORY_CODEX_AUTH"] == codexChatGPT:
		def = "2"
	}
	for {
		switch p.ask("API key or ChatGPT plan? 1, 2 or 3", def) {
		case "1":
			p.say("Paste an API key from https://platform.openai.com/api-keys.")
			vals["OPENAI_API_KEY"] = p.secret("OpenAI API key", firstOf(vals["OPENAI_API_KEY"], vals["CODEX_API_KEY"]))
			vals["CODEX_API_KEY"], vals["FACTORY_CODEX_AUTH"], vals["CODEX_ACCESS_TOKEN"] = "", "", ""
			if vals["OPENAI_API_KEY"] == "" {
				p.say("Without it, reviews will fail until you add one or switch to Claude-only review.")
			}
			return
		case "2":
			p.say("On a ChatGPT plan:")
			p.say("  - Don't use it for public repos, where anyone can put text in front of Codex.")
			p.say("  - Codex runs one job at a time, since two jobs can't share one sign-in.")
			p.say("  - Codex refreshes the sign-in about every 8 days, and the factory keeps the")
			p.say("    new one. If it stops working, run `./factory codex-login` again.")
			p.say("  - Jobs cost $0 in the factory's budget; the plan's 5-hour and weekly limits")
			p.say("    show in `./factory status` and on the dashboard's Spend page, and Codex")
			p.say("    jobs wait when one is nearly used up.")
			vals["FACTORY_CODEX_AUTH"] = codexChatGPT
			vals["OPENAI_API_KEY"], vals["CODEX_API_KEY"], vals["CODEX_ACCESS_TOKEN"] = "", "", ""
			if !signInCodex(p, cfg) {
				p.say("Codex jobs wait until you sign in with: ./factory codex-login")
			}
			return
		case "3":
			p.say("See https://learn.chatgpt.com/codex/enterprise/access-tokens for how to make")
			p.say("one. When it expires, make a new one and run setup again.")
			vals["CODEX_ACCESS_TOKEN"] = p.secret("Codex access token", vals["CODEX_ACCESS_TOKEN"])
			vals["OPENAI_API_KEY"], vals["CODEX_API_KEY"], vals["FACTORY_CODEX_AUTH"] = "", "", ""
			if vals["CODEX_ACCESS_TOKEN"] == "" {
				p.say("Without it, reviews will fail until you add one.")
			}
			return
		}
		p.say("Answer 1, 2 or 3.")
	}
}

// clientFor makes the client for one repo on its host (GitHub or GitLab)
// with the saved credentials, and returns the token git uses.
type clientFor func(repo string) (forge, github.TokenSource, error)

// githubRepos and gitlabRepos split the configured repos by host.
func githubRepos(cfg *config.Config) (out []string) {
	for _, r := range cfg.Repos {
		if !r.GitLab() {
			out = append(out, r.Name)
		}
	}
	return out
}

func gitlabRepos(cfg *config.Config) (out []string) {
	for _, r := range cfg.Repos {
		if r.GitLab() {
			out = append(out, r.Name)
		}
	}
	return out
}

// bothHosts sends each repo to the client for its host. Either may be nil
// when that host's access isn't set up; it returns nil when both are.
func bothHosts(cfg *config.Config, gh, gl clientFor) clientFor {
	if gh == nil && gl == nil {
		return nil
	}
	return func(repo string) (forge, github.TokenSource, error) {
		rc, _ := cfg.RepoConf(repo)
		c, host := gh, "GitHub"
		if rc.GitLab() {
			c, host = gl, "GitLab"
		}
		if c == nil {
			return nil, nil, fmt.Errorf("%s access isn't set up; run setup again", host)
		}
		return c(repo)
	}
}

// setupGitLab asks for a GitLab token and checks that it reaches every
// GitLab repo. It returns nil if you chose to skip the check.
func setupGitLab(ctx context.Context, p *prompter, cfg *config.Config, vals map[string]string) clientFor {
	repos := gitlabRepos(cfg)
	hosts := map[string]bool{}
	for _, r := range repos {
		rc, _ := cfg.RepoConf(r)
		hosts[rc.Host] = true
	}
	var sorted []string
	for h := range hosts {
		sorted = append(sorted, h)
	}
	slices.Sort(sorted)
	gitlabTokenHelp(p, sorted)
	client := clientFor(func(repo string) (forge, github.TokenSource, error) {
		ts := github.StaticToken(vals["GITLAB_TOKEN"])
		return newForge(cfg, repo, ts), ts, nil
	})
	for {
		vals["GITLAB_TOKEN"] = p.secret("GitLab token", vals["GITLAB_TOKEN"])
		var failed []string
		for _, repo := range repos {
			p.say("Checking access to %s ...", repo)
			if err := checkRepo(ctx, client, cfg, repo); err != nil {
				failed = append(failed, repo)
				p.say("That didn't work: %v", err)
				if strings.Contains(err.Error(), "401") {
					p.say("GitLab didn't accept the token. Check it was copied whole and hasn't expired.")
				} else if strings.Contains(err.Error(), "404") {
					p.say("The token can't see %s. A project token only reaches its own project; check the path too.", repo)
				}
				continue
			}
			p.say("OK: the factory can reach %s.", repo)
		}
		if len(failed) == 0 {
			return client
		}
		if !p.yes("Try again?", true) {
			p.say("Saving what you entered; run `factory setup` again to fix it.")
			return nil
		}
	}
}

// gitlabTokenHelp asks which kind of GitLab token you want to make and
// prints only the steps for that kind.
func gitlabTokenHelp(p *prompter, hosts []string) {
	p.say("The factory acts on GitLab with an access token. Its owner needs the Developer")
	p.say("role on each project (Maintainer if the base branch is protected from Developers).")
	p.say("Which kind of token will you make?")
	p.say("  1. Legacy, the quick one: one link opens the form with its scopes already ticked.")
	p.say("  2. Fine-grained: limited to these projects, but about 20 boxes to tick by hand")
	p.say("     (GitLab 18.10 beta, GA in 19.2).")
	for {
		switch p.ask("Legacy or fine-grained? 1 or 2", "1") {
		case "1":
			for _, h := range hosts {
				p.say("Open this link, set an expiry date and click Generate token:")
				p.say("  https://%s/-/user_settings/personal_access_tokens?name=factory&scopes=api,write_repository", h)
			}
			p.say("It makes a personal token with the api and write_repository scopes. A project,")
			p.say("group or bot account token with those scopes works too.")
			return
		case "2":
			for _, h := range hosts {
				p.say("Open https://%s/-/user_settings/personal_access_tokens and choose", h)
				p.say("Generate token > Fine-grained token.")
			}
			p.say("On the Group and project tab, add each project and give it:")
			p.say("  Code: Download, Push")
			p.say("  Project, Repository, Branch, Commit, Pipeline, Job, Member: Read")
			p.say("  Work Item: Read, Create, Update")
			p.say("  Merge Request: Read, Create, Update, Merge")
			p.say("  Label: Read, Create")
			p.say("  Markdown Upload: Create, Delete")
			p.say("On the User tab, User: Read. On the Global tab, Metadata: Read.")
			p.say("The search box finds each one; docs/configuration.md#gitlab-repos has the list.")
			return
		}
		p.say("Type 1 or 2.")
	}
}

// setupGitHub connects to GitHub one of three ways and checks that it can
// read every repo. It returns nil if you chose to skip the check.
func setupGitHub(ctx context.Context, p *prompter, cfg *config.Config, vals map[string]string, keyDir string) (clientFor, error) {
	def := func() string {
		switch {
		case vals["FACTORY_GITHUB_APP_ID"] != "":
			return "2"
		case vals["GITHUB_TOKEN"] != "":
			return "3"
		}
		return "1"
	}
	repos := githubRepos(cfg)
	list := strings.Join(repos, ", ")
	p.say("The factory acts on GitHub through a GitHub App limited to %s.", list)
	p.say("  1. Create a new app for me (recommended; takes about a minute in the browser)")
	p.say("  2. Use an app I already made")
	p.say("  3. Use a personal access token (only for a quick try)")
	for {
		var client clientFor
		switch p.ask("Choose 1, 2 or 3", def()) {
		case "1":
			id, slug, keyPath, err := createApp(ctx, p, cfg, keyDir)
			if err != nil {
				p.say("%v", err)
				continue
			}
			inst, err := installApp(ctx, p, cfg, id, slug, keyPath)
			if err != nil {
				p.say("%v", err)
				continue
			}
			vals["FACTORY_GITHUB_APP_ID"], vals["FACTORY_GITHUB_INSTALLATION_ID"], vals["FACTORY_GITHUB_APP_KEY"] = id, inst, keyPath
			vals["GITHUB_TOKEN"] = ""
			client = appClients(vals)
		case "2":
			vals["FACTORY_GITHUB_APP_ID"] = p.ask("App ID (under About on the app's settings page)", vals["FACTORY_GITHUB_APP_ID"])
			for {
				path := expandPath(p.ask("Private key file (.pem)", vals["FACTORY_GITHUB_APP_KEY"]))
				if _, err := github.LoadAppKey(path); err != nil {
					p.say("Can't use that key: %v", err)
					continue
				}
				vals["FACTORY_GITHUB_APP_KEY"] = path
				break
			}
			key, _ := github.LoadAppKey(vals["FACTORY_GITHUB_APP_KEY"])
			app := &github.AppTokens{AppID: vals["FACTORY_GITHUB_APP_ID"], Key: key, BaseURL: githubAPI}
			first, missing, err := installations(ctx, app, repos)
			if err != nil {
				p.say("GitHub refused that app ID and key: %v", err)
				continue
			}
			if len(missing) > 0 {
				p.say("The app isn't installed on %s. Install it from the app's settings page (Install App), then try again.", strings.Join(missing, " or "))
				continue
			}
			vals["FACTORY_GITHUB_INSTALLATION_ID"], vals["GITHUB_TOKEN"] = first, ""
			client = appClients(vals)
		case "3":
			p.say("Create a fine-grained token at %s/settings/personal-access-tokens/new", githubWeb)
			p.say("for %s only, with Contents, Issues and Pull requests set to Read and write.", list)
			vals["GITHUB_TOKEN"] = p.secret("Token", vals["GITHUB_TOKEN"])
			vals["FACTORY_GITHUB_APP_ID"], vals["FACTORY_GITHUB_INSTALLATION_ID"], vals["FACTORY_GITHUB_APP_KEY"] = "", "", ""
			tok := github.StaticToken(vals["GITHUB_TOKEN"])
			client = func(repo string) (forge, github.TokenSource, error) {
				return &github.Client{Repo: repo, Tokens: tok, BaseURL: githubAPI}, tok, nil
			}
		default:
			continue
		}
		var failed []string
		for _, repo := range repos {
			p.say("Checking access to %s ...", repo)
			err := checkRepo(ctx, client, cfg, repo)
			if err == nil {
				p.say("OK: the factory can reach %s.", repo)
				continue
			}
			failed = append(failed, repo)
			p.say("That didn't work: %v", err)
			if strings.Contains(err.Error(), "422") || strings.Contains(err.Error(), "404") || strings.Contains(err.Error(), "not installed") {
				owner, _, _ := strings.Cut(repo, "/")
				p.say("Check that the app or token has access to %s itself, not only to its owner.", repo)
				if vals["FACTORY_GITHUB_APP_ID"] != "" {
					p.say("For the app: open its settings, Install App, the gear next to %s, and add %s.", owner, repo)
				}
			}
		}
		if len(failed) == 0 {
			return client, nil
		}
		if vals["FACTORY_GITHUB_APP_ID"] != "" {
			p.say("Then choose 2 to check again; that keeps the app you already have.")
		}
		if !p.yes("Try again?", true) {
			p.say("Saving what you entered; run `factory setup` again to fix it.")
			return nil, nil
		}
	}
}

// checkRepo reads a repo's base branch, or failing that the repo itself:
// the branch may just be named differently.
func checkRepo(ctx context.Context, client clientFor, cfg *config.Config, repo string) error {
	gh, _, err := client(repo)
	if err != nil {
		return err
	}
	rc, _ := cfg.RepoConf(repo)
	sha, err := gh.BranchSHA(ctx, rc.BaseBranch)
	if err == nil && sha == "" {
		_, err = gh.DefaultBranch(ctx)
	}
	return err
}

// installations finds the app's installation on each repo's owner. It
// returns the first repo's, and the owners it isn't installed on.
func installations(ctx context.Context, app *github.AppTokens, repos []string) (first string, missing []string, err error) {
	seen := map[string]bool{}
	for i, repo := range repos {
		owner, _, _ := strings.Cut(repo, "/")
		if seen[strings.ToLower(owner)] {
			continue
		}
		seen[strings.ToLower(owner)] = true
		id, err := app.FindInstallation(ctx, owner)
		if err != nil {
			return "", nil, err
		}
		if id == "" {
			missing = append(missing, owner)
		}
		if i == 0 {
			first = id
		}
	}
	return first, missing, nil
}

// appClients makes clients that act as the saved app. The saved
// installation is the first repo's owner's; others are looked up.
func appClients(vals map[string]string) clientFor {
	return func(repo string) (forge, github.TokenSource, error) {
		key, err := github.LoadAppKey(vals["FACTORY_GITHUB_APP_KEY"])
		if err != nil {
			return nil, nil, err
		}
		ts := &github.AppTokens{AppID: vals["FACTORY_GITHUB_APP_ID"], Key: key, Repo: repo, BaseURL: githubAPI}
		return &github.Client{Repo: repo, BaseURL: githubAPI, Tokens: ts}, ts, nil
	}
}

// setupImages builds the job images that aren't built yet. It reports
// whether they all are now.
func setupImages(p *prompter, cfg *config.Config) bool {
	var missing []jobImage
	for _, im := range jobImages(cfg) {
		if runCmd(io.Discard, "docker", "image", "inspect", im.tag) != nil {
			missing = append(missing, im)
		}
	}
	if len(missing) == 0 {
		p.say("The job images are built.")
		return true
	}
	for _, im := range missing {
		p.say("Jobs for %s run in the image %s, which isn't built yet.", joinAnd(im.repos), im.tag)
	}
	p.say("Building takes a few minutes per image.")
	if !p.yes("Build now?", true) {
		p.say("Build them later with: ./factory images")
		return false
	}
	if err := buildImages(p.out, cfg, false); err != nil {
		p.say("%v. Fix it and run `./factory images` (or setup) again.", err)
		return false
	}
	return true
}

// checkBaseBranches compares each repo's base branch in factory.yaml with
// its default branch on GitHub. With one repo it offers to fix the config.
func checkBaseBranches(ctx context.Context, p *prompter, cfgPath string, cfg *config.Config, client clientFor) {
	for _, name := range cfg.RepoNames() {
		r, _ := cfg.RepoConf(name)
		gh, _, err := client(r.Name)
		if err != nil {
			continue
		}
		b, err := gh.DefaultBranch(ctx)
		if err != nil || b == "" || b == r.BaseBranch {
			continue
		}
		if len(cfg.Repos) > 1 {
			p.say("%s's default branch is %s, but the factory will use %s. To change it, give", r.Name, b, r.BaseBranch)
			p.say("that repo its own base_branch under repos: in %s.", cfgPath)
			continue
		}
		if p.yes(fmt.Sprintf("%s's default branch is %s, but factory.yaml says %s. Use %s?", r.Name, b, r.BaseBranch, b), true) {
			if err := setYAML(cfgPath, "base_branch", b); err != nil {
				p.say("Couldn't update it: %v. Set base_branch in %s yourself.", err, cfgPath)
			} else if c, err := config.Load(cfgPath); err == nil {
				*cfg = *c
			}
		}
	}
}

// parseRepoList reads "owner/a, https://github.com/owner/b,
// https://gitlab.com/group/c" into names.
func parseRepoList(in string) ([]string, error) {
	var names []string
	seen := map[string]bool{}
	for _, f := range strings.FieldsFunc(in, func(r rune) bool { return r == ',' || r == ' ' }) {
		host, name := config.SplitRepo("", f)
		path := strings.TrimPrefix(name, host+"/")
		if host != "" {
			if o, n, ok := strings.Cut(path, "/"); !ok || o == "" || n == "" || strings.Contains(path, "//") {
				return nil, fmt.Errorf("write %q as %s/group/project", f, host)
			}
		} else if o, n, ok := strings.Cut(name, "/"); !ok || o == "" || n == "" || strings.Contains(n, "/") {
			return nil, fmt.Errorf("write %q as owner/name, for example octocat/hello-world", f)
		}
		f = name
		if !seen[strings.ToLower(f)] {
			seen[strings.ToLower(f)] = true
			names = append(names, f)
		}
	}
	if len(names) == 0 {
		return nil, errors.New("name at least one repository, as owner/name")
	}
	return names, nil
}

// setRepos changes which repos the config lists, keeping the settings
// of the ones that stay.
func setRepos(path string, names []string) error {
	cur, err := config.Load(path)
	if err != nil {
		return err
	}
	repos := make([]config.Repo, len(names))
	for i, n := range names {
		repos[i] = config.Repo{Name: n}
		for _, r := range cur.Repos {
			if strings.EqualFold(r.Name, n) {
				repos[i] = r
				repos[i].Name = n
			}
		}
	}
	return writeRepos(path, repos)
}

// writeRepos replaces the config's top-level repo: or repos: entry, and
// whatever belongs to it, with repos. Comments inside that entry are
// dropped; the rest of the file, comments included, stays as it is.
func writeRepos(path string, repos []config.Repo) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var block string
	if bare, _ := repos[0].MarshalYAML(); len(repos) == 1 && bare == repos[0].Name {
		block = "repo: " + repos[0].Name + "\n"
	} else {
		var buf bytes.Buffer
		enc := yaml.NewEncoder(&buf)
		enc.SetIndent(2)
		if err := enc.Encode(repos); err != nil {
			return err
		}
		block = "repos:\n"
		for _, l := range strings.SplitAfter(strings.TrimRight(buf.String(), "\n")+"\n", "\n") {
			if l != "" {
				block += "  " + l
			}
		}
	}
	lines := strings.SplitAfter(string(b), "\n")
	var out strings.Builder
	found := false
	for i := 0; i < len(lines); i++ {
		l := lines[i]
		isRepo := strings.HasPrefix(l, "repo:") || strings.HasPrefix(l, "repos:")
		if found || !isRepo {
			out.WriteString(l)
			continue
		}
		found = true
		out.WriteString(block)
		// Skip the entry's own lines, and blank or comment lines between
		// them, up to the next top-level key.
		for i+1 < len(lines) && continues(lines[i+1:]) {
			i++
		}
	}
	if !found {
		return fmt.Errorf("%s has no repo: line to update", path)
	}
	return writeConfig(path, []byte(out.String()))
}

// continues reports whether lines[0] still belongs to the YAML entry
// above it: it is indented or a "- item", or it is blank or a comment
// with such a line after it.
func continues(lines []string) bool {
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		return strings.HasPrefix(l, " ") || strings.HasPrefix(l, "\t") || strings.HasPrefix(l, "-")
	}
	return false
}

// setYAML rewrites one top-level `key: value` line of the config file,
// keeping everything else, comments included.
func setYAML(path, key, value string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(key) + `:[ \t]*[^\s#]*`)
	if !re.Match(b) {
		return fmt.Errorf("%s has no top-level %s: line to update", path, key)
	}
	quoted, err := yaml.Marshal(value)
	if err != nil {
		return err
	}
	return writeConfig(path, re.ReplaceAllLiteral(b, append([]byte(key+": "), bytes.TrimSpace(quoted)...)))
}

// writeConfig replaces the config file in one step, after checking that
// the new text is a valid config.
func writeConfig(path string, out []byte) error {
	dir, _ := filepath.Abs(filepath.Dir(path))
	if _, err := config.ParseIn(out, dir); err != nil {
		return fmt.Errorf("updating %s: %w", path, err)
	}
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	tmp := path + ".setup-tmp"
	if err := os.WriteFile(tmp, out, st.Mode().Perm()); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func expandPath(p string) string {
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(home, p[2:])
		}
	}
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}

func firstOf(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

func usesHarness(cfg *config.Config, h string) bool {
	for _, r := range cfg.Routing {
		if r.Harness == h {
			return true
		}
	}
	return false
}
