package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"golang.org/x/term"

	"github.com/claptimes5/factory/internal/config"
	"github.com/claptimes5/factory/internal/github"
	"github.com/claptimes5/factory/internal/notify"
	"github.com/claptimes5/factory/internal/pipeline"
)

// envKeys are the settings `factory setup` asks for, in file order.
var envKeys = []string{
	"FACTORY_GITHUB_APP_ID", "FACTORY_GITHUB_INSTALLATION_ID", "FACTORY_GITHUB_APP_KEY", "GITHUB_TOKEN",
	"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "OPENAI_API_KEY", "CODEX_API_KEY", "FACTORY_NOTIFY_URL",
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
		p.hidden = func() (string, error) {
			b, err := term.ReadPassword(fd)
			fmt.Fprintln(p.out)
			return string(b), err
		}
	}
	return p
}

func (p *prompter) line() string {
	s, _ := p.in.ReadString('\n')
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
		fmt.Fprintf(p.out, "%s [saved %s, Enter keeps it, - clears it]: ", label, mask(def))
	} else {
		fmt.Fprintf(p.out, "%s: ", label)
	}
	s := ""
	if p.hidden != nil {
		s, _ = p.hidden()
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
func setup(ctx context.Context, cfgPath string, cfg *config.Config, envPath string, p *prompter) error {
	vals, err := readEnvFile(envPath)
	if err != nil {
		return err
	}
	for _, k := range envKeys {
		if _, ok := vals[k]; !ok {
			vals[k] = os.Getenv(k)
		}
	}
	save := func() error {
		return writeEnvFile(envPath, "# Written by `factory setup`. Values set in the environment override these.\n", envKeys, vals)
	}
	p.say("Factory setup. Your answers are saved to %s, readable only by you.", envPath)
	p.say("Press Enter to keep a value shown in [brackets]. Ctrl-C stops without saving.")

	p.say("\n== 1. Repository")
	for {
		repo := p.ask("GitHub repository the factory works on (owner/name)", cfg.Repo)
		repo = strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(repo, "https://github.com/"), "github.com/"), ".git")
		if o, n, ok := strings.Cut(repo, "/"); !ok || o == "" || n == "" || strings.Contains(n, "/") {
			p.say("Write it as owner/name, for example octocat/hello-world.")
			continue
		}
		if repo != cfg.Repo {
			if err := setYAML(cfgPath, "repo", repo); err != nil {
				return err
			}
			cfg.Repo = repo
			p.say("Updated repo in %s.", cfgPath)
		}
		break
	}

	if cfg.Runner.Kind == "docker" {
		p.say("\n== 2. Docker")
		if err := checkDocker(p, cfg); err != nil {
			return err
		}
	}

	p.say("\n== 3. GitHub access")
	gh, err := setupGitHub(ctx, p, cfg, vals, filepath.Dir(envPath))
	if err != nil {
		return err
	}
	if err := save(); err != nil {
		return err
	}
	if gh != nil {
		if b, err := gh.DefaultBranch(ctx); err == nil && b != "" && b != cfg.BaseBranch {
			if p.yes(fmt.Sprintf("%s's default branch is %s, but factory.yaml says %s. Use %s?", cfg.Repo, b, cfg.BaseBranch, b), true) {
				if err := setYAML(cfgPath, "base_branch", b); err != nil {
					return err
				}
				cfg.BaseBranch = b
			}
		}
		labels := pipeline.Labels(cfg.Intake.Label)
		p.say("\nThe factory uses %d labels on %s, such as %s.", len(labels), cfg.Repo, labels[0].Name)
		if p.yes("Create the ones that are missing? Existing labels are left as they are.", true) {
			for _, l := range labels {
				created, err := gh.EnsureLabel(ctx, l.Name, l.Color, l.Description)
				switch {
				case err != nil:
					p.say("  %s: %v", l.Name, err)
				case created:
					p.say("  %s: created", l.Name)
				default:
					p.say("  %s: already there", l.Name)
				}
			}
		}
	}

	p.say("\n== 4. Claude (writes the code)")
	p.say("Paste an API key from https://console.anthropic.com/settings/keys, or, to use")
	p.say("a Claude subscription, run `claude setup-token` and paste the token it prints.")
	for {
		v := p.secret("Anthropic API key or subscription token", firstOf(vals["ANTHROPIC_API_KEY"], vals["CLAUDE_CODE_OAUTH_TOKEN"]))
		vals["ANTHROPIC_API_KEY"], vals["CLAUDE_CODE_OAUTH_TOKEN"] = "", ""
		switch {
		case strings.HasPrefix(v, "sk-ant-oat"):
			vals["CLAUDE_CODE_OAUTH_TOKEN"] = v
		case strings.HasPrefix(v, "sk-ant-"):
			vals["ANTHROPIC_API_KEY"] = v
		case v == "":
			p.say("Every job needs Claude, so the factory can't run without one.")
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

	if usesHarness(cfg, "codex") {
		p.say("\n== 5. OpenAI (reviews the code)")
		p.say("factory.yaml has Codex review Claude's work. Paste an API key from")
		p.say("https://platform.openai.com/api-keys. A ChatGPT plan isn't needed.")
		p.say("No OpenAI account? Leave it empty and see \"Claude-only review\" in the README.")
		vals["OPENAI_API_KEY"] = p.secret("OpenAI API key", firstOf(vals["OPENAI_API_KEY"], vals["CODEX_API_KEY"]))
		vals["CODEX_API_KEY"] = ""
		if vals["OPENAI_API_KEY"] == "" {
			p.say("Without it, reviews will fail until you add one or switch to Claude-only review.")
		}
	}

	p.say("\n== 6. Phone notifications (optional)")
	p.say("An ntfy topic URL (https://ntfy.sh/<topic>), or a Slack or Discord webhook URL.")
	for {
		u := p.ask("Notification URL (Enter to skip, - to clear)", vals["FACTORY_NOTIFY_URL"])
		if u == "-" {
			u = ""
		}
		if _, err := notify.New(u); err != nil {
			p.say("%v", err)
			continue
		}
		vals["FACTORY_NOTIFY_URL"] = u
		break
	}

	if err := save(); err != nil {
		return err
	}
	p.say("\nAll set. Saved %s.", envPath)
	p.say("Start the factory with:\n\n    ./factory serve\n")
	p.say("Then open http://%s/app/ and label an issue on %s with %s.", cfg.Web.Listen, cfg.Repo, cfg.Intake.Label)
	return nil
}

// setupGitHub connects to GitHub one of three ways and checks that it can
// read the repo. It returns nil if you chose to skip the check.
func setupGitHub(ctx context.Context, p *prompter, cfg *config.Config, vals map[string]string, keyDir string) (*github.Client, error) {
	def := "1"
	switch {
	case vals["FACTORY_GITHUB_APP_ID"] != "":
		def = "2"
	case vals["GITHUB_TOKEN"] != "":
		def = "3"
	}
	p.say("The factory acts on GitHub through a GitHub App limited to this one repo.")
	p.say("  1. Create a new app for me (recommended; takes about a minute in the browser)")
	p.say("  2. Use an app I already made")
	p.say("  3. Use a personal access token (only for a quick try)")
	for {
		var gh *github.Client
		switch p.ask("Choose 1, 2 or 3", def) {
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
			gh, _ = appClient(cfg, vals)
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
			owner, _, _ := strings.Cut(cfg.Repo, "/")
			found, err := (&github.AppTokens{AppID: vals["FACTORY_GITHUB_APP_ID"], Key: key, BaseURL: githubAPI}).FindInstallation(ctx, owner)
			if err != nil {
				p.say("GitHub refused that app ID and key: %v", err)
				continue
			}
			if found == "" {
				p.say("The app isn't installed on %s. Install it from the app's settings page (Install App), then try again.", owner)
				continue
			}
			vals["FACTORY_GITHUB_INSTALLATION_ID"], vals["GITHUB_TOKEN"] = found, ""
			gh, _ = appClient(cfg, vals)
		case "3":
			p.say("Create a fine-grained token at %s/settings/personal-access-tokens/new", githubWeb)
			p.say("for %s only, with Contents, Issues and Pull requests set to Read and write.", cfg.Repo)
			vals["GITHUB_TOKEN"] = p.secret("Token", vals["GITHUB_TOKEN"])
			vals["FACTORY_GITHUB_APP_ID"], vals["FACTORY_GITHUB_INSTALLATION_ID"], vals["FACTORY_GITHUB_APP_KEY"] = "", "", ""
			gh = &github.Client{Repo: cfg.Repo, Tokens: github.StaticToken(vals["GITHUB_TOKEN"]), BaseURL: githubAPI}
		default:
			continue
		}
		p.say("Checking access to %s ...", cfg.Repo)
		sha, err := gh.BranchSHA(ctx, cfg.BaseBranch)
		if err == nil && sha == "" {
			// The branch may just be named differently; reading the repo is what matters.
			_, err = gh.DefaultBranch(ctx)
		}
		if err == nil {
			p.say("OK: the factory can reach %s.", cfg.Repo)
			return gh, nil
		}
		p.say("That didn't work: %v", err)
		if strings.Contains(err.Error(), "422") || strings.Contains(err.Error(), "404") {
			p.say("Check that the app or token has access to %s itself, not only to its owner.", cfg.Repo)
		}
		if !p.yes("Try again?", true) {
			p.say("Saving what you entered; run `factory setup` again to fix it.")
			return nil, nil
		}
		def = ""
	}
}

func appClient(cfg *config.Config, vals map[string]string) (*github.Client, error) {
	key, err := github.LoadAppKey(vals["FACTORY_GITHUB_APP_KEY"])
	if err != nil {
		return nil, err
	}
	return &github.Client{Repo: cfg.Repo, BaseURL: githubAPI, Tokens: &github.AppTokens{
		AppID: vals["FACTORY_GITHUB_APP_ID"], InstallationID: vals["FACTORY_GITHUB_INSTALLATION_ID"],
		Key: key, Repo: cfg.Repo, BaseURL: githubAPI}}, nil
}

// checkDocker makes sure Docker works for this user and the job image
// exists, offering to build it.
func checkDocker(p *prompter, cfg *config.Config) error {
	for runCmd(io.Discard, "docker", "info") != nil {
		p.say("Docker isn't reachable. Start it, and if it says permission denied, run")
		p.say("    sudo usermod -aG docker $USER")
		p.say("then log out and back in (on WSL, close and reopen the terminal).")
		if !p.yes("Check again?", true) {
			p.say("Skipping; jobs can't run until Docker works.")
			return nil
		}
	}
	p.say("Docker works.")
	if runCmd(io.Discard, "docker", "image", "inspect", cfg.Runner.Image) == nil {
		p.say("The job image %s is built.", cfg.Runner.Image)
		return nil
	}
	p.say("Jobs run in the image %s, which isn't built yet. Building it takes a few minutes.", cfg.Runner.Image)
	if !p.yes("Build it now?", true) {
		p.say("Build it later with: docker build -t %s deploy/job-image", cfg.Runner.Image)
		return nil
	}
	if err := runCmd(p.out, "docker", "build", "-t", cfg.Runner.Image, cfg.Path("deploy/job-image")); err != nil {
		p.say("The build failed: %v. Fix it and run `factory setup` again.", err)
	}
	return nil
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
	out := re.ReplaceAllLiteral(b, []byte(key+": "+value))
	if _, err := config.Parse(out); err != nil {
		return fmt.Errorf("updating %s: %w", path, err)
	}
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	return os.WriteFile(path, out, st.Mode().Perm())
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
