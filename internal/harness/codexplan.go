package harness

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/claptimes5/factory/internal/runner"
)

// Gated is a harness whose jobs may have to wait. Codex on a ChatGPT plan
// runs one job at a time, since two runs refreshing one sign-in break it,
// and none while it is signed out.
type Gated interface {
	// OneAtATime reports that only one of its jobs may run at once.
	OneAtATime() bool
	// Unavailable says why its jobs can't start now, or "".
	Unavailable() string
}

// NewCodexPlan returns the Codex adapter signed in to a ChatGPT plan
// rather than with an API key. With authFile (an auth.json from `codex
// login`), each run gets a copy of it in its own CODEX_HOME and a copy
// Codex refreshed is saved back. With "", the job's env carries
// CODEX_ACCESS_TOKEN (Business and Enterprise plans). Either way, plan
// usage costs no dollars, so runs report $0.
func NewCodexPlan(r runner.Runner, authFile string) Harness {
	return &cliHarness{name: "codex", a: codexAdapter{plan: &codexPlan{file: authFile}}, runner: r}
}

// codexPlan is a ChatGPT sign-in shared by every run of the harness.
type codexPlan struct {
	file string // the daemon's auth.json; "" for CODEX_ACCESS_TOKEN

	mu  sync.Mutex // one run at a time with the file
	out sync.Mutex // guards outAt
	// outAt is the sign-in's digest when Codex last couldn't sign in with
	// it, so its jobs wait until a new sign-in replaces it.
	outAt  string
	outSet bool
}

const (
	codexHomeDir    = "codex-home" // CODEX_HOME, under the job's out directory
	codexStderrFile = "stderr"
	// codexConfig makes Codex keep its sign-in in auth.json, where the
	// daemon can read a refreshed copy back, and sign in only with it.
	codexConfig = "cli_auth_credentials_store = \"file\"\nforced_login_method = \"chatgpt\"\n"
)

// apiKeys are the variables that would make Codex use an API key instead.
var apiKeys = []string{"CODEX_API_KEY", "OPENAI_API_KEY"}

// home is the CODEX_HOME a run with the auth file uses, as the process
// sees it: the job's out directory is mounted, so the daemon can read it.
func (c *codexPlan) home(p runner.Paths) string { return path.Join(p.Out, codexHomeDir) }

func (c *codexPlan) withFile() bool { return c != nil && c.file != "" }

// OneAtATime reports whether runs share the auth file.
func (a codexAdapter) OneAtATime() bool { return a.plan.withFile() }

// Unavailable says why Codex can't sign in, or "".
func (a codexAdapter) Unavailable() string {
	c := a.plan
	if c == nil {
		return ""
	}
	if c.file == "" {
		c.out.Lock()
		defer c.out.Unlock()
		if c.outSet {
			return "Codex couldn't sign in with CODEX_ACCESS_TOKEN; it may have expired. Put a new one in factory.env and restart the factory. Codex jobs wait until then."
		}
		return ""
	}
	sum, err := fileSum(c.file)
	if err != nil {
		return fmt.Sprintf("Codex isn't signed in to ChatGPT (no %s). Run `factory codex-login`; Codex jobs wait until then.", c.file)
	}
	c.out.Lock()
	defer c.out.Unlock()
	if c.outSet && c.outAt == sum {
		return "Codex's ChatGPT sign-in has expired or was used elsewhere. Run `factory codex-login` again; Codex jobs wait until then."
	}
	return ""
}

func (c *codexPlan) signedOut(sum string) {
	c.out.Lock()
	c.outAt, c.outSet = sum, true
	c.out.Unlock()
}

func fileSum(name string) (string, error) {
	b, err := os.ReadFile(name)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(b)
	return string(h[:]), nil
}

// prepare gives a run its own CODEX_HOME with a copy of the sign-in, and
// returns what to do once it ends: save a refreshed sign-in, and notice
// one Codex couldn't use.
func (a codexAdapter) prepare(ctx context.Context, req RunRequest, p runner.Paths, spec *runner.Spec) (func(*Result), error) {
	c := a.plan
	if c == nil {
		return func(*Result) {}, nil
	}
	env := map[string]string{}
	for k, v := range spec.Env {
		env[k] = v
	}
	for _, k := range apiKeys {
		delete(env, k) // codex exec would pick an API key over the plan
	}
	spec.Env = env
	if c.file == "" {
		return func(res *Result) {
			if signInFailed(res, "") {
				c.signedOut("")
			}
			planCost(res)
		}, nil
	}
	c.mu.Lock()
	unlock, err := lockFile(ctx, c.file+".lock")
	if err != nil {
		c.mu.Unlock()
		return nil, err
	}
	release := func() { unlock(); c.mu.Unlock() }
	auth, err := os.ReadFile(c.file)
	if err != nil {
		release()
		return nil, fmt.Errorf("codex: no ChatGPT sign-in: run `factory codex-login` (%w)", err)
	}
	dir := filepath.Join(req.OutDir, codexHomeDir)
	err = os.RemoveAll(dir)
	if err == nil {
		err = os.MkdirAll(dir, 0o700)
	}
	if err == nil {
		err = os.WriteFile(filepath.Join(dir, "auth.json"), auth, 0o600)
	}
	if err == nil {
		err = os.WriteFile(filepath.Join(dir, "config.toml"), []byte(codexConfig), 0o600)
	}
	if err == nil && req.SkillsDir != "" {
		// The skills mount's mount point, made here so it is ours.
		err = os.MkdirAll(filepath.Join(dir, "skills"), 0o700)
	}
	if err != nil {
		_ = os.RemoveAll(dir)
		release()
		return nil, err
	}
	env["CODEX_HOME"] = c.home(p)
	sum := sha256.Sum256(auth)
	return func(res *Result) {
		defer release()
		// The copy, Codex's sessions and its stderr go with the job's
		// CODEX_HOME: nothing of the sign-in outlives the run.
		defer func() { _ = os.RemoveAll(dir) }()
		stderr, _ := os.ReadFile(filepath.Join(dir, codexStderrFile))
		if b, err := os.ReadFile(filepath.Join(dir, "auth.json")); err == nil && !bytes.Equal(b, auth) && ValidAuth(b) {
			if err := writeAtomic(c.file, b); err != nil && res != nil && !res.IsError {
				res.IsError, res.Error = true, "codex: saving its refreshed sign-in: "+err.Error()
			}
		} else if signInFailed(res, string(stderr)) {
			c.signedOut(string(sum[:]))
		}
		planCost(res)
	}, nil
}

// planCost prices a plan run at nothing: it uses the plan's limits, not
// dollars. The tokens are still recorded.
func planCost(res *Result) {
	if res != nil {
		res.CostUSD, res.CostKnown = 0, true
	}
}

// signInFailed reports whether a run failed because Codex couldn't sign
// in, and marks the result so: its job isn't the builder's failure.
func signInFailed(res *Result, stderr string) bool {
	if res == nil || (!res.IsError && res.ExitCode == 0) {
		return false
	}
	text := res.Error + "\n" + stderr
	lower := strings.ToLower(text)
	for _, s := range []string{
		"refresh token", "refresh_token", "sign in again", "log in again", "log out and sign in",
		"not logged in", "codex login", "401 unauthorized", "status 401",
	} {
		if strings.Contains(lower, s) {
			res.SignedOut, res.IsError = true, true
			res.Error = "codex: its ChatGPT sign-in was refused"
			if line := signInLine(text, s); line != "" {
				res.Error += ": " + line
			}
			return true
		}
	}
	return false
}

// signInLine is the line of text that mentions s, trimmed.
func signInLine(text, s string) string {
	for _, l := range strings.Split(text, "\n") {
		if strings.Contains(strings.ToLower(l), s) {
			return truncate(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(l), "codex:")), 300)
		}
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// ValidAuth reports whether b is an auth.json with ChatGPT tokens in it,
// so a half-written or emptied file never replaces a working one.
func ValidAuth(b []byte) bool {
	var a struct {
		Tokens map[string]any `json:"tokens"`
	}
	return json.Unmarshal(b, &a) == nil && a.Tokens["refresh_token"] != nil
}

// SaveAuth replaces name with b atomically, readable only by its owner.
func SaveAuth(name string, b []byte) error {
	if !ValidAuth(b) {
		return errors.New("not a ChatGPT sign-in (no refresh token in it)")
	}
	if err := os.MkdirAll(filepath.Dir(name), 0o700); err != nil {
		return err
	}
	return writeAtomic(name, b)
}

// writeAtomic replaces name with b through a temporary file in the same
// directory, mode 0600.
func writeAtomic(name string, b []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(name), ".auth-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }() // no-op after the rename
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), name)
}

// lockFile takes an exclusive lock on name, so a `factory eval` or
// `factory compare` running beside the daemon can't refresh the sign-in
// at the same time as it.
func lockFile(ctx context.Context, name string) (func(), error) {
	f, err := os.OpenFile(name, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			f.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}
