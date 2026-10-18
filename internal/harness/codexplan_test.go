package harness

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/claptimes5/factory/internal/runner"
)

// codexRunner plays Codex: it checks the run's CODEX_HOME and may
// refresh the sign-in there, or fail on stderr.
type codexRunner struct {
	fakeRunner
	home    string // host CODEX_HOME, from the request's OutDir
	refresh string // new auth.json to write, "" for none
	stderr  string
	seen    string // the auth.json the run was given
	config  string
}

func (f *codexRunner) Run(ctx context.Context, s runner.Spec, w io.Writer) (int, error) {
	b, _ := os.ReadFile(filepath.Join(f.home, "auth.json"))
	f.seen = string(b)
	c, _ := os.ReadFile(filepath.Join(f.home, "config.toml"))
	f.config = string(c)
	if f.refresh != "" {
		os.WriteFile(filepath.Join(f.home, "auth.json"), []byte(f.refresh), 0o600)
	}
	if f.stderr != "" {
		os.WriteFile(filepath.Join(f.home, codexStderrFile), []byte(f.stderr), 0o600)
	}
	return f.fakeRunner.Run(ctx, s, w)
}

const (
	oldAuth = `{"auth_mode":"chatgpt","tokens":{"access_token":"a1","refresh_token":"r1"},"last_refresh":"2026-10-01T00:00:00Z"}`
	newAuth = `{"auth_mode":"chatgpt","tokens":{"access_token":"a2","refresh_token":"r2"},"last_refresh":"2026-10-18T00:00:00Z"}`
)

func planSetup(t *testing.T) (RunRequest, string) {
	t.Helper()
	r := req(t, true)
	r.Env = map[string]string{"OPENAI_API_KEY": "sk-x", "CODEX_API_KEY": "sk-x", "OTHER": "1"}
	file := filepath.Join(t.TempDir(), "codex", "auth.json")
	if err := SaveAuth(file, []byte(oldAuth)); err != nil {
		t.Fatal(err)
	}
	return r, file
}

func TestCodexPlanCopiesTheSignInAndSavesTheRefresh(t *testing.T) {
	r, file := planSetup(t)
	fr := &codexRunner{fakeRunner: fakeRunner{out: codexStream}, home: filepath.Join(r.OutDir, codexHomeDir), refresh: newAuth}
	h := NewCodexPlan(fr, file)
	res, err := h.Run(context.Background(), r, nil)
	if err != nil {
		t.Fatal(err)
	}
	if fr.seen != oldAuth {
		t.Fatalf("run got auth %q", fr.seen)
	}
	if !strings.Contains(fr.config, `cli_auth_credentials_store = "file"`) || !strings.Contains(fr.config, `forced_login_method = "chatgpt"`) {
		t.Fatalf("config.toml = %q", fr.config)
	}
	if b, _ := os.ReadFile(file); string(b) != newAuth {
		t.Fatalf("master copy = %s", b)
	}
	if st, _ := os.Stat(file); st.Mode().Perm() != 0o600 {
		t.Fatalf("master mode %v", st.Mode().Perm())
	}
	if _, err := os.Stat(fr.home); !os.IsNotExist(err) {
		t.Fatalf("the job's copy outlived the run: %v", err)
	}
	// No API key reaches the job, which runs in its own CODEX_HOME.
	env := fr.spec.Env
	if env["OPENAI_API_KEY"] != "" || env["CODEX_API_KEY"] != "" || env["OTHER"] != "1" || env["CODEX_HOME"] != "/out/codex-home" {
		t.Fatalf("env = %v", env)
	}
	if r.Env["OPENAI_API_KEY"] == "" {
		t.Fatal("the request's own env map was changed")
	}
	if fr.spec.Mounts[0].Container != "/out/codex-home/skills" {
		t.Fatalf("skills mount = %+v", fr.spec.Mounts)
	}
	cmd := strings.Join(fr.spec.Cmd, " ")
	if fr.spec.Cmd[0] != "sh" || !strings.Contains(cmd, `2>"$CODEX_HOME/stderr"`) {
		t.Fatalf("cmd = %s", cmd)
	}
	// Plan usage records tokens and no dollars.
	if !res.CostKnown || res.CostUSD != 0 || res.TokensIn != 1000 {
		t.Fatalf("res = %+v", res)
	}
	g := h.(Gated)
	if !g.OneAtATime() || g.Unavailable() != "" {
		t.Fatalf("gated %v %q", g.OneAtATime(), g.Unavailable())
	}
}

func TestCodexPlanKeepsTheSignInWhenTheCopyIsBroken(t *testing.T) {
	r, file := planSetup(t)
	fr := &codexRunner{fakeRunner: fakeRunner{out: codexStream}, home: filepath.Join(r.OutDir, codexHomeDir), refresh: `{"tokens":`}
	if _, err := NewCodexPlan(fr, file).Run(context.Background(), r, nil); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(file); string(b) != oldAuth {
		t.Fatalf("master copy = %s", b)
	}
}

func TestCodexPlanSignedOutHoldsItsJobsUntilANewSignIn(t *testing.T) {
	r, file := planSetup(t)
	fr := &codexRunner{fakeRunner: fakeRunner{code: 1}, home: filepath.Join(r.OutDir, codexHomeDir),
		stderr: "Error: Your access token could not be refreshed because your refresh token was already used. Please log out and sign in again.\n"}
	h := NewCodexPlan(fr, file)
	res, err := h.Run(context.Background(), r, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !res.SignedOut || !res.IsError || !strings.Contains(res.Error, "refresh token was already used") {
		t.Fatalf("res = %+v", res)
	}
	g := h.(Gated)
	if why := g.Unavailable(); !strings.Contains(why, "factory codex-login") {
		t.Fatalf("unavailable = %q", why)
	}
	// `factory codex-login` saves a new sign-in: jobs may start again.
	if err := SaveAuth(file, []byte(newAuth)); err != nil {
		t.Fatal(err)
	}
	if why := g.Unavailable(); why != "" {
		t.Fatalf("still unavailable: %q", why)
	}
	os.Remove(file)
	if why := g.Unavailable(); !strings.Contains(why, "isn't signed in") {
		t.Fatalf("no file: %q", why)
	}
}

func TestCodexPlanIgnoresSignInWordsInASuccessfulRun(t *testing.T) {
	r, file := planSetup(t)
	fr := &codexRunner{fakeRunner: fakeRunner{out: codexStream}, home: filepath.Join(r.OutDir, codexHomeDir), stderr: "warn: refresh token retry\n"}
	res, err := NewCodexPlan(fr, file).Run(context.Background(), r, nil)
	if err != nil || res.SignedOut || res.IsError {
		t.Fatalf("res = %+v %v", res, err)
	}
}

func TestCodexAccessTokenRunsInParallelWithoutAPIKeys(t *testing.T) {
	r := req(t, true)
	r.Env = map[string]string{"OPENAI_API_KEY": "sk-x", "CODEX_ACCESS_TOKEN": "tok"}
	fr := &fakeRunner{out: `{"type":"error","message":"unexpected status 401 Unauthorized: token expired"}` + "\n", code: 1}
	h := NewCodexPlan(fr, "")
	g := h.(Gated)
	if g.OneAtATime() || g.Unavailable() != "" {
		t.Fatalf("gated %v %q", g.OneAtATime(), g.Unavailable())
	}
	res, err := h.Run(context.Background(), r, nil)
	if err != nil {
		t.Fatal(err)
	}
	if fr.spec.Env["OPENAI_API_KEY"] != "" || fr.spec.Env["CODEX_ACCESS_TOKEN"] != "tok" || fr.spec.Env["CODEX_HOME"] != "" {
		t.Fatalf("env = %v", fr.spec.Env)
	}
	if fr.spec.Cmd[0] != "codex" {
		t.Fatalf("cmd = %q", fr.spec.Cmd)
	}
	if !res.SignedOut || !strings.Contains(g.Unavailable(), "CODEX_ACCESS_TOKEN") {
		t.Fatalf("res = %+v, unavailable %q", res, g.Unavailable())
	}
}

func TestCodexWithAnAPIKeyIsNotGated(t *testing.T) {
	g := NewCodex(&fakeRunner{}).(Gated)
	if g.OneAtATime() || g.Unavailable() != "" {
		t.Fatal("an API key needs no gate")
	}
}
