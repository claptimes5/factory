package harness

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"

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
	return &cliHarness{name: "codex", a: codexAdapter{plan: &codexPlan{file: authFile, gate: make(chan struct{}, 1)}}, runner: r}
}

// codexPlan is a ChatGPT sign-in shared by every run of the harness.
type codexPlan struct {
	file string        // the daemon's auth.json; "" for CODEX_ACCESS_TOKEN
	gate chan struct{} // one run at a time with the file, in this process

	out sync.Mutex // guards outAt and outSet
	// outAt is the sign-in's digest when Codex last couldn't sign in with
	// it, so its jobs wait until a new sign-in replaces it.
	outAt  string
	outSet bool
}

const (
	codexHomeDir = "codex-home" // CODEX_HOME, under the job's out directory
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

// Recover finishes the runs a crash cut short: it keeps a sign-in one of
// them refreshed and deletes their copies.
func (a codexAdapter) Recover() error {
	if !a.plan.withFile() {
		return nil
	}
	return recoverRuns(a.plan.file)
}

// prepare gives a run its own CODEX_HOME with a copy of the sign-in, and
// returns what to do once it ends: save a refreshed sign-in, and notice
// one Codex couldn't use. Codex's stderr is kept by the runner, outside
// anything the job can write.
func (a codexAdapter) prepare(ctx context.Context, req RunRequest, p runner.Paths, spec *runner.Spec) (func(*Result), []string, error) {
	c := a.plan
	if c == nil {
		return func(*Result) {}, nil, nil
	}
	env := map[string]string{}
	for k, v := range spec.Env {
		env[k] = v
	}
	for _, k := range apiKeys {
		delete(env, k) // codex exec would pick an API key over the plan
	}
	spec.Env = env
	stderr := &tailBuffer{max: maxJobFile}
	spec.Stderr = stderr
	if c.file == "" {
		return func(res *Result) {
			if signInFailed(res, stderr.String()) {
				c.signedOut("")
			}
			planCost(res)
		}, nil, nil
	}
	release, err := c.acquire(ctx)
	if err != nil {
		return nil, nil, err
	}
	auth, err := os.ReadFile(c.file)
	if err != nil {
		release()
		return nil, nil, fmt.Errorf("codex: no ChatGPT sign-in: run `factory codex-login` (%w)", err)
	}
	e := runEntry{Out: req.OutDir, Was: digest(auth)}
	dir := filepath.Join(req.OutDir, codexHomeDir)
	end, err := startRun(c.file, e)
	if err == nil {
		err = os.RemoveAll(dir)
	}
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
		if end != nil {
			end()
		}
		release()
		return nil, nil, err
	}
	env["CODEX_HOME"] = c.home(p)
	redact := authSecrets(auth)
	return func(res *Result) {
		defer release()
		defer end()
		// The copy, Codex's sessions and anything else in the job's
		// CODEX_HOME go when the run ends.
		got, saved, err := finishRun(c.file, e, auth)
		if err != nil && res != nil && !res.IsError {
			res.IsError, res.Error = true, "codex: saving its refreshed sign-in: "+err.Error()
		}
		if res != nil {
			res.Redact = append(redact, authSecrets(got)...)
		}
		if signInFailed(res, stderr.String()) {
			// Wait for a sign-in other than the one that failed: the one
			// it started with, or the one it saved while failing.
			if saved {
				c.signedOut(digest(got))
			} else {
				c.signedOut(e.Was)
			}
		}
		if res != nil {
			res.Error = scrub(res.Error, res.Redact)
		}
		planCost(res)
	}, redact, nil
}

// acquire waits, until ctx ends, to be the only run with the sign-in: in
// this process, and across processes (a `factory eval` or `factory
// compare` beside the daemon), since two runs refreshing it at once would
// sign both out.
func (c *codexPlan) acquire(ctx context.Context) (func(), error) {
	select {
	case c.gate <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	unlock, err := lockFile(ctx, c.file+".run")
	if err != nil {
		<-c.gate
		return nil, err
	}
	return func() { unlock(); <-c.gate }, nil
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	mu  sync.Mutex
	max int
	b   []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, p...)
	if len(t.b) > t.max {
		t.b = append([]byte(nil), t.b[len(t.b)-t.max:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.b)
}

// scrub replaces each secret in s.
func scrub(s string, secrets []string) string {
	for _, v := range secrets {
		if len(v) >= 8 {
			s = strings.ReplaceAll(s, v, "[REDACTED]")
		}
	}
	return s
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
