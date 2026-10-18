package harness

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/claptimes5/factory/internal/runner"
)

// codexRunner plays Codex: it checks the run's CODEX_HOME and may
// refresh the sign-in there, write to stderr, or do what a hostile job
// could do to its CODEX_HOME.
type codexRunner struct {
	fakeRunner
	home    string // host CODEX_HOME, from the request's OutDir
	refresh string // new auth.json to write, "" for none
	stderr  string // to the process's stderr
	during  func() // runs mid-run, e.g. a `factory codex-login`
	plant   func(home string)
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
	if f.stderr != "" && s.Stderr != nil {
		s.Stderr.Write([]byte(f.stderr))
	}
	if f.during != nil {
		f.during()
	}
	if f.plant != nil {
		f.plant(f.home)
	}
	return f.fakeRunner.Run(ctx, s, w)
}

const (
	oldAuth   = `{"auth_mode":"chatgpt","tokens":{"access_token":"access-old-1","refresh_token":"refresh-old-1","account_id":"acct-1"},"last_refresh":"2026-10-01T00:00:00Z"}`
	newAuth   = `{"auth_mode":"chatgpt","tokens":{"access_token":"access-new-2","refresh_token":"refresh-new-2","account_id":"acct-1"},"last_refresh":"2026-10-18T00:00:00Z"}`
	loginAuth = `{"auth_mode":"chatgpt","tokens":{"access_token":"access-login-3","refresh_token":"refresh-login-3","account_id":"acct-1"},"last_refresh":"2026-10-18T01:00:00Z"}`
	otherAuth = `{"auth_mode":"chatgpt","tokens":{"access_token":"access-evil-9","refresh_token":"refresh-evil-9","account_id":"acct-evil"}}`
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

func newRunner(r RunRequest, out string) *codexRunner {
	return &codexRunner{fakeRunner: fakeRunner{out: out}, home: filepath.Join(r.OutDir, codexHomeDir)}
}

func master(t *testing.T, file string) string {
	t.Helper()
	b, _ := os.ReadFile(file)
	return string(b)
}

func TestCodexPlanCopiesTheSignInAndSavesTheRefresh(t *testing.T) {
	r, file := planSetup(t)
	fr := newRunner(r, codexStream)
	fr.refresh = newAuth
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
	if got := master(t, file); got != newAuth {
		t.Fatalf("master copy = %s", got)
	}
	if st, _ := os.Stat(file); st.Mode().Perm() != 0o600 {
		t.Fatalf("master mode %v", st.Mode().Perm())
	}
	if _, err := os.Stat(fr.home); !os.IsNotExist(err) {
		t.Fatalf("the job's copy outlived the run: %v", err)
	}
	if left, _ := filepath.Glob(filepath.Join(runsDir(file), "*")); len(left) != 0 {
		t.Fatalf("run records left: %v", left)
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
	// Stderr is the runner's, not a file in the job's CODEX_HOME.
	if fr.spec.Stderr == nil || strings.Contains(strings.Join(fr.spec.Cmd, " "), "2>") {
		t.Fatalf("stderr %v, cmd %q", fr.spec.Stderr, fr.spec.Cmd)
	}
	// Plan usage records tokens and no dollars.
	if !res.CostKnown || res.CostUSD != 0 || res.TokensIn != 1000 {
		t.Fatalf("res = %+v", res)
	}
	// Both sign-ins' tokens are to be kept out of the log.
	for _, s := range []string{"access-old-1", "refresh-old-1", "access-new-2", "refresh-new-2"} {
		if !strings.Contains(strings.Join(res.Redact, " "), s) {
			t.Fatalf("redact %q lacks %s", res.Redact, s)
		}
	}
	g := h.(Gated)
	if !g.OneAtATime() || g.Unavailable() != "" {
		t.Fatalf("gated %v %q", g.OneAtATime(), g.Unavailable())
	}
}

func TestCodexPlanScrubsTheSignInFromItsEvents(t *testing.T) {
	r, file := planSetup(t)
	fr := newRunner(r, `{"type":"item.completed","item":{"type":"agent_message","text":"token access-old-1"}}`+"\n")
	ev := make(chan Event, 10)
	if _, err := NewCodexPlan(fr, file).Run(context.Background(), r, ev); err != nil {
		t.Fatal(err)
	}
	close(ev)
	for e := range ev {
		if e.Kind == "raw" && strings.Contains(string(e.Payload), "access-old-1") {
			t.Fatalf("raw event kept the token: %s", e.Payload)
		}
	}
}

func TestCodexPlanKeepsTheSignInWhenTheCopyIsBroken(t *testing.T) {
	for name, copy := range map[string]string{
		"half written":      `{"tokens":`,
		"empty token":       `{"tokens":{"refresh_token":"","account_id":"acct-1"}}`,
		"not a string":      `{"tokens":{"refresh_token":{"x":1},"account_id":"acct-1"}}`,
		"another account":   otherAuth,
		"no account at all": `{"tokens":{"refresh_token":"refresh-x-123456"}}`,
	} {
		r, file := planSetup(t)
		fr := newRunner(r, codexStream)
		fr.refresh = copy
		if _, err := NewCodexPlan(fr, file).Run(context.Background(), r, nil); err != nil {
			t.Fatal(err)
		}
		if got := master(t, file); got != oldAuth {
			t.Fatalf("%s: master copy = %s", name, got)
		}
	}
}

func TestCodexPlanReadsNothingTheJobPlanted(t *testing.T) {
	for name, plant := range map[string]func(home, host string){
		"fifo": func(home, _ string) {
			os.Remove(filepath.Join(home, "auth.json"))
			syscall.Mkfifo(filepath.Join(home, "auth.json"), 0o600)
		},
		"link to a host file": func(home, host string) {
			os.Remove(filepath.Join(home, "auth.json"))
			os.Symlink(host, filepath.Join(home, "auth.json"))
		},
		"link to /dev/zero": func(home, _ string) {
			os.Remove(filepath.Join(home, "auth.json"))
			os.Symlink("/dev/zero", filepath.Join(home, "auth.json"))
		},
		"home linked away": func(home, host string) {
			os.RemoveAll(home)
			os.Symlink(filepath.Dir(host), home)
		},
		"limits fifo": func(home, _ string) {
			syscall.Mkfifo(filepath.Join(filepath.Dir(home), codexLimitsFile), 0o600)
		},
		"limits linked to /dev/zero": func(home, _ string) {
			os.Symlink("/dev/zero", filepath.Join(filepath.Dir(home), codexLimitsFile))
		},
	} {
		// The operator's own sign-in, which the job must not hand over.
		host := filepath.Join(t.TempDir(), "auth.json")
		if err := os.WriteFile(host, []byte(newAuth), 0o600); err != nil {
			t.Fatal(err)
		}
		r, file := planSetup(t)
		r.CaptureLimits = true
		fr := newRunner(r, codexStream)
		fr.plant = func(home string) { plant(home, host) }
		done := make(chan error, 1)
		go func() {
			_, err := NewCodexPlan(fr, file).Run(context.Background(), r, nil)
			done <- err
		}()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: the run hung", name)
		}
		if got := master(t, file); got != oldAuth {
			t.Fatalf("%s: master copy = %s", name, got)
		}
		if _, err := os.Stat(host); err != nil {
			t.Fatalf("%s: the host file went: %v", name, err)
		}
	}
}

func TestCodexPlanDoesntOverwriteANewerSignIn(t *testing.T) {
	r, file := planSetup(t)
	fr := newRunner(r, codexStream)
	fr.refresh = newAuth
	fr.during = func() {
		// `factory codex-login` while the job runs.
		if err := SaveAuth(file, []byte(loginAuth)); err != nil {
			t.Error(err)
		}
	}
	if _, err := NewCodexPlan(fr, file).Run(context.Background(), r, nil); err != nil {
		t.Fatal(err)
	}
	if got := master(t, file); got != loginAuth {
		t.Fatalf("master copy = %s", got)
	}
}

func TestSaveAuthWaitsForTheLock(t *testing.T) {
	_, file := planSetup(t)
	unlock, err := lockFile(context.Background(), file+".lock")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- SaveAuth(file, []byte(loginAuth)) }()
	select {
	case <-done:
		t.Fatal("saved while a save-back held the lock")
	case <-time.After(500 * time.Millisecond):
	}
	unlock()
	if err := <-done; err != nil || master(t, file) != loginAuth {
		t.Fatalf("%v %s", err, master(t, file))
	}
}

func TestCodexPlanWaitForATurnEndsWithItsContext(t *testing.T) {
	r, file := planSetup(t)
	block, started := make(chan struct{}), make(chan struct{})
	fr := newRunner(r, codexStream)
	fr.during = func() { close(started); <-block }
	h := NewCodexPlan(fr, file)
	go h.Run(context.Background(), r, nil)
	<-started
	defer close(block)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := h.Run(ctx, req(t, true), nil); err == nil {
		t.Fatal("a second run started beside the first")
	}
	// Nor in another process: the run lock is held.
	if _, err := lockFile(ctx, file+".run"); err == nil {
		t.Fatal("run lock not held")
	}
}

func TestCodexPlanSignedOutHoldsItsJobsUntilANewSignIn(t *testing.T) {
	r, file := planSetup(t)
	fr := newRunner(r, "")
	fr.code = 1
	fr.stderr = "Error: Your access token could not be refreshed because your refresh token was already used. Please log out and sign in again.\n"
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
	if err := SaveAuth(file, []byte(loginAuth)); err != nil {
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

func TestCodexPlanSignedOutAfterARefreshIsStillSignedOut(t *testing.T) {
	r, file := planSetup(t)
	fr := newRunner(r, "")
	fr.code, fr.refresh = 1, newAuth
	fr.stderr = "error: 401 Unauthorized\n"
	h := NewCodexPlan(fr, file)
	res, err := h.Run(context.Background(), r, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !res.SignedOut || master(t, file) != newAuth {
		t.Fatalf("res %+v, master %s", res, master(t, file))
	}
	// Held on the sign-in it saved, until a new one.
	if why := h.(Gated).Unavailable(); why == "" {
		t.Fatal("not held after failing with the refreshed sign-in")
	}
}

func TestCodexPlanIgnoresSignInWordsTheJobWrote(t *testing.T) {
	r, file := planSetup(t)
	fr := newRunner(r, "")
	fr.code = 1
	fr.plant = func(home string) {
		os.WriteFile(filepath.Join(home, "stderr"), []byte("401 Unauthorized: run codex login\n"), 0o600)
	}
	res, err := NewCodexPlan(fr, file).Run(context.Background(), r, nil)
	if err != nil || res.SignedOut {
		t.Fatalf("res = %+v %v", res, err)
	}
}

func TestCodexPlanIgnoresSignInWordsInASuccessfulRun(t *testing.T) {
	r, file := planSetup(t)
	fr := newRunner(r, codexStream)
	fr.stderr = "warn: refresh token retry\n"
	res, err := NewCodexPlan(fr, file).Run(context.Background(), r, nil)
	if err != nil || res.SignedOut || res.IsError {
		t.Fatalf("res = %+v %v", res, err)
	}
}

func TestCodexPlanRecoversARunACrashCutShort(t *testing.T) {
	r, file := planSetup(t)
	home := filepath.Join(r.OutDir, codexHomeDir)
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(home, "auth.json"), []byte(newAuth), 0o600)
	// A record no process holds any more, as a crash leaves it.
	end, err := startRun(file, runEntry{Out: r.OutDir, Was: digest([]byte(oldAuth))})
	if err != nil {
		t.Fatal(err)
	}
	names, _ := filepath.Glob(filepath.Join(runsDir(file), "*.json"))
	b, _ := os.ReadFile(names[0])
	end()
	os.WriteFile(names[0], b, 0o600)
	// And one a live run holds, in another directory.
	live := filepath.Join(t.TempDir(), "out")
	os.MkdirAll(filepath.Join(live, codexHomeDir), 0o700)
	endLive, err := startRun(file, runEntry{Out: live, Was: digest([]byte(oldAuth))})
	if err != nil {
		t.Fatal(err)
	}
	defer endLive()

	h := NewCodexPlan(&fakeRunner{}, file).(interface{ Recover() error })
	if err := h.Recover(); err != nil {
		t.Fatal(err)
	}
	if got := master(t, file); got != newAuth {
		t.Fatalf("master copy = %s", got)
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Fatalf("the crashed run's copy is still there: %v", err)
	}
	if _, err := os.Stat(filepath.Join(live, codexHomeDir)); err != nil {
		t.Fatalf("the live run's CODEX_HOME was removed: %v", err)
	}
}

func TestCodexAccessTokenRunsInParallelWithoutAPIKeys(t *testing.T) {
	r := req(t, true)
	r.Env = map[string]string{"OPENAI_API_KEY": "sk-x", "CODEX_ACCESS_TOKEN": "tok"}
	fr := &codexRunner{fakeRunner: fakeRunner{code: 1}, home: t.TempDir(), stderr: "Error: unexpected status 401 Unauthorized: token expired\n"}
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
	// Seen on the process's stderr, with no error event.
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
