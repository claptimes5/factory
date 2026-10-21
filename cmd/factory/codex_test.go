package main

import (
	"bufio"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/claptimes5/factory/internal/config"
)

const testAuth = `{"auth_mode":"chatgpt","tokens":{"access_token":"a","refresh_token":"r"},"last_refresh":"2026-10-18T09:30:00Z"}`

func TestCodexAuthModes(t *testing.T) {
	for _, tc := range []struct {
		auth, token, want string
		keys              bool // API keys reach Codex jobs
	}{
		{"", "", codexAPIKey, true},
		{"api-key", "", codexAPIKey, true},
		{"chatgpt", "", codexChatGPT, false},
		{"chatgpt", "tok", codexAccessToken, false},
		{"", "tok", codexAccessToken, false},
	} {
		t.Setenv("FACTORY_CODEX_AUTH", tc.auth)
		t.Setenv("CODEX_ACCESS_TOKEN", tc.token)
		t.Setenv("OPENAI_API_KEY", "sk-openai")
		t.Setenv("CODEX_API_KEY", "")
		mode, err := codexAuth()
		if err != nil || mode != tc.want {
			t.Fatalf("%+v: mode %q %v", tc, mode, err)
		}
		codex := secrets()["codex"]
		if got := codex["CODEX_API_KEY"] != "" || codex["OPENAI_API_KEY"] != ""; got != tc.keys {
			t.Fatalf("%+v: codex env %v", tc, codex)
		}
		if (codex["CODEX_ACCESS_TOKEN"] != "") != (tc.want == codexAccessToken) || (tc.want == codexChatGPT && len(codex) != 0) {
			t.Fatalf("%+v: codex env %v", tc, codex)
		}
	}
	t.Setenv("CODEX_ACCESS_TOKEN", "")
	t.Setenv("FACTORY_CODEX_AUTH", "plan")
	if _, err := codexAuth(); err == nil || !strings.Contains(err.Error(), "chatgpt") {
		t.Fatalf("a bad FACTORY_CODEX_AUTH: %v", err)
	}
}

// fakeLogin plays `codex login --device-auth` in the job image: it saves
// a sign-in in the CODEX_HOME mounted in.
func fakeLogin(t *testing.T, auth string) *[]string {
	t.Helper()
	var calls []string
	old := runTTY
	runTTY = func(env []string, name string, args ...string) error {
		calls = append(calls, name+" "+strings.Join(args, " "))
		for i, a := range args {
			if a == "-v" && strings.HasSuffix(args[i+1], ":/codex") {
				dir := strings.TrimSuffix(args[i+1], ":/codex")
				if c, err := os.ReadFile(filepath.Join(dir, "config.toml")); err != nil || !strings.Contains(string(c), `cli_auth_credentials_store = "file"`) {
					t.Errorf("config.toml %q %v", c, err)
				}
				return os.WriteFile(filepath.Join(dir, "auth.json"), []byte(auth), 0o600)
			}
		}
		return nil
	}
	t.Cleanup(func() { runTTY = old })
	t.Setenv("CODEX_HOME", t.TempDir()) // Codex on this machine isn't signed in
	return &calls
}

func TestSetupSignsCodexInToAChatGPTPlan(t *testing.T) {
	calls := fakeLogin(t, testAuth)
	cfgPath, envPath, out := runSetup(t, &fakeGitHub{labels: map[string]bool{}},
		"o/r",
		"3", "ghp_token123",
		"n", "n",
		"", // Claude, Codex or both: both
		"sk-ant-api03-abc",
		"2", // a ChatGPT plan
		"",
	)
	vals, _ := readEnvFile(envPath)
	if vals["FACTORY_CODEX_AUTH"] != "chatgpt" || vals["OPENAI_API_KEY"] != "" || vals["CODEX_ACCESS_TOKEN"] != "" {
		t.Fatalf("env %v", vals)
	}
	if len(*calls) != 1 || !strings.Contains((*calls)[0], "docker run --rm -i") || !strings.Contains((*calls)[0], "codex login --device-auth") {
		t.Fatalf("login %q", *calls)
	}
	cfg, _ := config.Load(cfgPath)
	file := codexAuthFile(cfg)
	if b, err := os.ReadFile(file); err != nil || string(b) != testAuth {
		t.Fatalf("saved %s %v", b, err)
	}
	if st, _ := os.Stat(file); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode().Perm())
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(file), "login-*")); len(left) != 0 {
		t.Fatalf("login dirs left: %v", left)
	}
	for _, want := range []string{"public repos", "one job at a time", "8 days", "Spend page", "Signed in."} {
		if !strings.Contains(out, want) {
			t.Errorf("setup didn't say %q:\n%s", want, out)
		}
	}
	t.Setenv("FACTORY_CODEX_AUTH", "chatgpt")
	t.Setenv("CODEX_ACCESS_TOKEN", "")
	if s := codexSignIn(cfg); s != "ChatGPT plan, sign-in refreshed Oct 18 09:30 UTC" {
		t.Fatalf("status %q", s)
	}
}

func TestSetupTakesACodexAccessToken(t *testing.T) {
	_, envPath, _ := runSetup(t, &fakeGitHub{labels: map[string]bool{}},
		"o/r",
		"3", "ghp_token123",
		"n", "n",
		"", // Claude, Codex or both: both
		"sk-ant-api03-abc",
		"4", "3", "codex-access-token", // a wrong answer, then a token
		"",
	)
	vals, _ := readEnvFile(envPath)
	if vals["CODEX_ACCESS_TOKEN"] != "codex-access-token" || vals["FACTORY_CODEX_AUTH"] != "" || vals["OPENAI_API_KEY"] != "" {
		t.Fatalf("env %v", vals)
	}
}

func TestCodexLoginCopiesThisMachinesSignIn(t *testing.T) {
	calls := fakeLogin(t, "")
	if err := os.WriteFile(filepath.Join(os.Getenv("CODEX_HOME"), "auth.json"), []byte(testAuth), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(testConfig(t, "o/r"))
	if err != nil {
		t.Fatal(err)
	}
	// factory.env is a link to the real file, which has comments.
	dir := t.TempDir()
	real := filepath.Join(dir, "real.env")
	if err := os.WriteFile(real, []byte("# my notes\nOPENAI_API_KEY=sk-x\nFACTORY_WEB_PASSWORD=long-enough-pw\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	envPath := filepath.Join(dir, "factory.env")
	if err := os.Symlink(real, envPath); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	p := &prompter{in: bufio.NewReader(strings.NewReader("y\n")), out: &b}
	if err := codexLogin(cfg, envPath, p); err != nil {
		t.Fatalf("%v\n%s", err, b.String())
	}
	if len(*calls) != 0 {
		t.Fatalf("ran a login after copying: %q", *calls)
	}
	if got, _ := os.ReadFile(codexAuthFile(cfg)); string(got) != testAuth {
		t.Fatalf("saved %s", got)
	}
	if st, err := os.Lstat(envPath); err != nil || st.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("factory.env is no longer a link: %v", err)
	}
	got, _ := os.ReadFile(real)
	if want := "# my notes\n# OPENAI_API_KEY=sk-x\nFACTORY_WEB_PASSWORD=long-enough-pw\nFACTORY_CODEX_AUTH=chatgpt\n"; string(got) != want {
		t.Fatalf("factory.env = %q, want %q", got, want)
	}
	if !strings.Contains(b.String(), "Commented out OPENAI_API_KEY") {
		t.Fatalf("didn't say what it commented out:\n%s", b.String())
	}
	if !strings.Contains(b.String(), "codex login") {
		t.Fatalf("no advice to sign this machine in again:\n%s", b.String())
	}
}

func TestCodexLoginRefusesAFileWithNoSignIn(t *testing.T) {
	fakeLogin(t, `{"OPENAI_API_KEY":"sk-x"}`)
	cfg, err := config.Load(testConfig(t, "o/r"))
	if err != nil {
		t.Fatal(err)
	}
	old := runCmd
	runCmd = func(io.Writer, string, ...string) error { return nil } // the image is built
	t.Cleanup(func() { runCmd = old })
	var b strings.Builder
	p := &prompter{in: bufio.NewReader(strings.NewReader("n\n")), out: &b}
	if err := codexLogin(cfg, filepath.Join(t.TempDir(), "factory.env"), p); err == nil {
		t.Fatal("saved a file with no sign-in")
	}
	if _, err := os.Stat(codexAuthFile(cfg)); !os.IsNotExist(err) {
		t.Fatalf("auth file: %v", err)
	}
}

func TestEvalRefusesABadCodexAuth(t *testing.T) {
	t.Setenv("CODEX_ACCESS_TOKEN", "")
	t.Setenv("FACTORY_CODEX_AUTH", "plan")
	cfg, err := config.Load(testConfig(t, "o/r"))
	if err != nil {
		t.Fatal(err)
	}
	if err := runEval(context.Background(), cfg, nil, []string{"-set", t.TempDir()}); err == nil || !strings.Contains(err.Error(), "FACTORY_CODEX_AUTH") {
		t.Fatalf("eval ran with a bad FACTORY_CODEX_AUTH: %v", err)
	}
}
