package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/claptimes5/factory/internal/config"
	"github.com/claptimes5/factory/internal/harness"
	"github.com/claptimes5/factory/internal/runner"
)

// How Codex signs in. FACTORY_CODEX_AUTH=chatgpt picks the ChatGPT plan
// sign-in `factory codex-login` saves; CODEX_ACCESS_TOKEN (Business and
// Enterprise plans) wins over both it and an API key, as it does in Codex.
const (
	codexAPIKey      = "api-key"
	codexChatGPT     = "chatgpt"
	codexAccessToken = "access-token"
)

// codexAuth is how Codex jobs sign in, from the environment.
func codexAuth() (string, error) {
	if os.Getenv("CODEX_ACCESS_TOKEN") != "" {
		return codexAccessToken, nil
	}
	switch v := os.Getenv("FACTORY_CODEX_AUTH"); v {
	case "", codexAPIKey:
		return codexAPIKey, nil
	case codexChatGPT:
		return codexChatGPT, nil
	default:
		return "", fmt.Errorf("FACTORY_CODEX_AUTH is %q; use chatgpt for a ChatGPT plan, or leave it out for an API key", v)
	}
}

// codexAuthMode is codexAuth's mode; an invalid one fails the build.
func codexAuthMode() string {
	m, _ := codexAuth()
	return m
}

// codexAuthFile is the factory's own copy of Codex's ChatGPT sign-in.
// Jobs get a copy of it, and the daemon saves back the one Codex refreshes.
func codexAuthFile(cfg *config.Config) string {
	return filepath.Join(cfg.Path(cfg.DataDir), "codex", "auth.json")
}

// newCodex makes the Codex harness for how Codex signs in.
func newCodex(cfg *config.Config, r runner.Runner) harness.Harness {
	mode, _ := codexAuth() // checked in build
	switch mode {
	case codexChatGPT:
		return harness.NewCodexPlan(r, codexAuthFile(cfg))
	case codexAccessToken:
		return harness.NewCodexPlan(r, "")
	}
	return harness.NewCodex(r)
}

// codexSignIn describes the saved ChatGPT sign-in for `factory status`.
func codexSignIn(cfg *config.Config) string {
	mode, err := codexAuth()
	switch {
	case err != nil:
		return err.Error()
	case mode == codexAccessToken:
		return "ChatGPT plan, with CODEX_ACCESS_TOKEN"
	case mode == codexAPIKey:
		return ""
	}
	b, err := os.ReadFile(codexAuthFile(cfg))
	if err != nil || !harness.ValidAuth(b) {
		return "ChatGPT plan, not signed in: run `factory codex-login`"
	}
	var a struct {
		LastRefresh time.Time `json:"last_refresh"`
	}
	if json.Unmarshal(b, &a) == nil && !a.LastRefresh.IsZero() {
		return "ChatGPT plan, sign-in refreshed " + a.LastRefresh.UTC().Format("Jan 2 15:04") + " UTC"
	}
	return "ChatGPT plan, signed in"
}

// runTTY runs an interactive command on this terminal; tests replace it.
var runTTY = func(env []string, name string, args ...string) error {
	c := exec.Command(name, args...)
	c.Env = append(os.Environ(), env...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	return c.Run()
}

// codexLogin is `factory codex-login`: sign Codex in to a ChatGPT plan
// again, and switch the factory to it.
func codexLogin(cfg *config.Config, envPath string, p *prompter) (err error) {
	defer func() {
		if r := recover(); r != nil {
			if _, ok := r.(inputClosed); !ok {
				panic(r)
			}
			err = errInputClosed
		}
	}()
	if !signInCodex(p, cfg) {
		return errors.New("codex isn't signed in")
	}
	vals, err := readEnvFile(envPath)
	if err != nil {
		return err
	}
	if vals["FACTORY_CODEX_AUTH"] == codexChatGPT && vals["CODEX_ACCESS_TOKEN"] == "" {
		p.say("A running factory picks up the new sign-in for its next Codex job.")
		return nil
	}
	// The keys Codex would use instead of the plan are commented out, not
	// deleted, so switching back is an edit.
	off, err := updateEnvFile(envPath, map[string]string{"FACTORY_CODEX_AUTH": codexChatGPT},
		[]string{"CODEX_ACCESS_TOKEN", "OPENAI_API_KEY", "CODEX_API_KEY"})
	if err != nil {
		return err
	}
	p.say("Set FACTORY_CODEX_AUTH=chatgpt in %s.", envPath)
	if len(off) > 0 {
		p.say("Commented out %s there: on a ChatGPT plan Codex doesn't use them.", strings.Join(off, ", "))
	}
	p.say("Restart the factory to use the plan.")
	return nil
}

// signInCodex gets a ChatGPT sign-in for Codex and saves it as the
// factory's copy. It copies the sign-in of Codex on this machine, if you
// want, or else runs `codex login --device-auth` in the job image. It
// reports whether a sign-in was saved.
func signInCodex(p *prompter, cfg *config.Config) bool {
	dst := codexAuthFile(cfg)
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		p.say("%v", err)
		return false
	}
	home := os.Getenv("CODEX_HOME")
	if home == "" {
		if h, err := os.UserHomeDir(); err == nil {
			home = filepath.Join(h, ".codex")
		}
	}
	if b, err := os.ReadFile(filepath.Join(home, "auth.json")); err == nil && harness.ValidAuth(b) {
		p.say("Codex on this machine is signed in to ChatGPT (%s).", filepath.Join(home, "auth.json"))
		p.say("The factory can copy that sign-in, but then the two share it, and whichever")
		p.say("refreshes it second is signed out. If you copy it, run `codex login` here")
		p.say("again afterward so this machine has its own.")
		if p.yes("Copy it?", false) {
			if err := harness.SaveAuth(dst, b); err != nil {
				p.say("Couldn't save it: %v", err)
				return false
			}
			p.say("Saved the sign-in to %s.", dst)
			return true
		}
	}
	tmp, err := os.MkdirTemp(filepath.Dir(dst), "login-")
	if err != nil {
		p.say("%v", err)
		return false
	}
	defer os.RemoveAll(tmp)
	if err := os.WriteFile(filepath.Join(tmp, "config.toml"), []byte("cli_auth_credentials_store = \"file\"\nforced_login_method = \"chatgpt\"\n"), 0o600); err != nil {
		p.say("%v", err)
		return false
	}
	p.say("Signing in with a device code: open the link Codex shows, sign in to ChatGPT,")
	p.say("and enter the code. Turn on device code sign-in in ChatGPT's security settings")
	p.say("(or your workspace's) if Codex says it's off.")
	for {
		err := loginCmd(cfg, tmp)
		b, rerr := os.ReadFile(filepath.Join(tmp, "auth.json"))
		if err == nil && rerr == nil && harness.ValidAuth(b) {
			if err := harness.SaveAuth(dst, b); err != nil {
				p.say("Couldn't save the sign-in: %v", err)
				return false
			}
			p.say("Signed in. Saved to %s, readable only by you.", dst)
			return true
		}
		if err == nil {
			err = errors.New("codex didn't save a sign-in")
		}
		p.say("Sign-in failed: %v", err)
		if !p.yes("Try again?", true) {
			return false
		}
	}
}

// loginCmd runs `codex login --device-auth` with CODEX_HOME set to dir:
// in the first repo's job image with the Docker runner, else on this
// machine.
func loginCmd(cfg *config.Config, dir string) error {
	if cfg.Runner.Kind != "docker" {
		return runTTY([]string{"CODEX_HOME=" + dir}, "codex", "login", "--device-auth")
	}
	image := ""
	if names := cfg.RepoNames(); len(names) > 0 {
		r, _ := cfg.RepoConf(names[0])
		image = r.Image
	}
	if image == "" || runCmd(io.Discard, "docker", "image", "inspect", image) != nil {
		return fmt.Errorf("the job image %s isn't built; build it with ./factory images", image)
	}
	args := []string{"run", "--rm", "-i"}
	if term.IsTerminal(int(os.Stdin.Fd())) {
		args = append(args, "-t")
	}
	args = append(args, "--security-opt", "no-new-privileges", "--cap-drop", "ALL",
		"--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()),
		"-e", "HOME=/tmp", "-e", "CODEX_HOME=/codex", "-v", dir+":/codex", image, "codex", "login", "--device-auth")
	return runTTY(nil, "docker", args...)
}
