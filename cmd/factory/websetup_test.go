package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/claptimes5/factory/internal/config"
	"github.com/claptimes5/factory/internal/pipeline"
	"github.com/claptimes5/factory/internal/web"
)

// testWizard makes the get-started page's backend for repo against a fake
// GitHub, with nothing saved or exported yet and Docker answering run.
func testWizard(t *testing.T, f *fakeGitHub, repo string, setupMode bool, run func(io.Writer, string, ...string) error) *wizard {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	oldAPI, oldWeb, oldRun := githubAPI, githubWeb, runCmd
	githubAPI, githubWeb, runCmd = srv.URL, "https://github.example", run
	t.Cleanup(func() { githubAPI, githubWeb, runCmd = oldAPI, oldWeb, oldRun })
	w, err := newWizard(context.Background(), testConfig(t, repo), filepath.Join(t.TempDir(), "factory.env"), setupMode, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	w.exported = map[string]string{}
	w.looked = true // no background checks: each test runs what it needs
	w.machineCheck = func(context.Context, *config.Config) []finding {
		return []finding{{pass, "git 2.50", ""}, {fail, "Docker isn't running", "sudo systemctl start docker"}}
	}
	return w
}

func noDocker(io.Writer, string, ...string) error { return errors.New("no docker") }

func step(st web.APIWizard, key string) web.APIWizardStep {
	for _, s := range st.Steps {
		if s.Key == key {
			return s
		}
	}
	return web.APIWizardStep{}
}

func TestWizardMakesTheGitHubApp(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	f := &fakeGitHub{key: key, labels: map[string]bool{"factory:ready": true}}
	w := testWizard(t, f, "o/r", true, noDocker)
	ctx := context.Background()

	if s := step(w.State(ctx, false), "github"); s.State != "attention" || s.Summary != "Not connected yet" {
		t.Fatalf("github step before = %+v", s)
	}
	m, err := w.StartGitHubApp(ctx, "http://127.0.0.1:8080/api/v1/setup/github/callback")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(m.URL, "https://github.example/settings/apps/new?state="+m.State) || len(m.State) != 32 {
		t.Fatalf("manifest = %+v", m)
	}
	var man map[string]any
	json.Unmarshal([]byte(m.Manifest), &man)
	if man["redirect_url"] != "http://127.0.0.1:8080/api/v1/setup/github/callback" {
		t.Fatalf("redirect_url = %v", man["redirect_url"])
	}
	if _, err := w.FinishGitHubApp(ctx, "CODE", "not-the-state"); err == nil {
		t.Fatal("a state this wizard didn't hand out must be refused")
	}
	msg, err := w.FinishGitHubApp(ctx, "CODE", m.State)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(msg, "factory-r-ab12") {
		t.Fatalf("msg = %q", msg)
	}
	if _, err := w.FinishGitHubApp(ctx, "CODE", m.State); err == nil {
		t.Fatal("a state works once")
	}
	vals, _ := readEnvFile(w.envPath)
	if vals["FACTORY_GITHUB_APP_ID"] != "42" || vals["FACTORY_GITHUB_APP_SLUG"] != "factory-r-ab12" || vals["FACTORY_GITHUB_INSTALLATION_ID"] != "55" {
		t.Fatalf("saved %v", vals)
	}
	if st, err := os.Stat(vals["FACTORY_GITHUB_APP_KEY"]); err != nil || st.Mode().Perm() != 0o600 || filepath.Dir(vals["FACTORY_GITHUB_APP_KEY"]) != filepath.Dir(w.envPath) {
		t.Fatalf("key %s: %v", vals["FACTORY_GITHUB_APP_KEY"], err)
	}
	if st, _ := os.Stat(w.envPath); st.Mode().Perm() != 0o600 {
		t.Fatalf("factory.env mode %v", st.Mode().Perm())
	}
	st := w.State(ctx, false)
	// Reached, but its labels aren't made yet.
	if s := step(st, "github"); s.State != "attention" || s.Summary != "Labels missing on o/r" {
		t.Fatalf("github step after = %+v", s)
	}
	if st.GitHub.Method != "app" || st.GitHub.InstallURL != "https://github.example/apps/factory-r-ab12/installations/new" || !st.CanFinish {
		t.Fatalf("github = %+v, can finish %v", st.GitHub, st.CanFinish)
	}

	// Labels: the one already there is left alone.
	if _, err := w.Do(ctx, "labels", web.WizardInput{}); err != nil {
		t.Fatal(err)
	}
	if len(f.created) != len(pipeline.Labels("factory:ready"))-1 {
		t.Fatalf("created %v", f.created)
	}
	st = w.State(ctx, false)
	if l := st.Labels; len(l) != len(pipeline.Labels("factory:ready")) || l[0].Result != "exists" || l[1].Result != "created" {
		t.Fatalf("labels = %+v", l)
	}
	if s := step(st, "github"); s.State != "done" {
		t.Fatalf("github step with labels = %+v", s)
	}

	// The page checks again as it loads: a deleted label shows, and only
	// it is made again.
	f.mu.Lock()
	f.created = slices.DeleteFunc(f.created, func(n string) bool { return n == pipeline.LabelParked })
	made := len(f.created)
	f.mu.Unlock()
	if s := step(w.State(ctx, true), "github"); s.State != "done" {
		t.Fatalf("checked moments ago, so no new check: %+v", s)
	}
	w.mu.Lock()
	w.checkedAt = time.Time{}
	w.mu.Unlock()
	st = w.State(ctx, true)
	if s := step(st, "github"); s.State != "attention" || s.Summary != "Labels missing on o/r" {
		t.Fatalf("github step = %+v, access %+v", s, st.Access)
	}
	if m := st.Access[0].MissingLabels; strings.Join(m, ",") != pipeline.LabelParked {
		t.Fatalf("missing labels = %v", m)
	}
	if _, err := w.Do(ctx, "labels", web.WizardInput{}); err != nil {
		t.Fatal(err)
	}
	if len(f.created) != made+1 || f.created[made] != pipeline.LabelParked {
		t.Fatalf("created %v", f.created)
	}
	if s := step(w.State(ctx, false), "github"); s.State != "done" {
		t.Fatalf("github step = %+v", s)
	}

	// An uninstalled app shows too.
	f.mu.Lock()
	f.gone = true
	f.mu.Unlock()
	w.mu.Lock()
	w.checkedAt = time.Time{}
	w.mu.Unlock()
	st = w.State(ctx, true)
	if s := step(st, "github"); s.State != "attention" || len(st.GitHub.Missing) != 1 || st.Access[0].State != "failed" {
		t.Fatalf("after uninstalling: %+v %+v %+v", s, st.GitHub.Missing, st.Access)
	}
	f.mu.Lock()
	f.gone = false
	f.mu.Unlock()
	w.mu.Lock()
	w.checkedAt = time.Time{}
	w.mu.Unlock()
	w.State(ctx, true)

	// Setup mode: Finish ends the setup server.
	if _, err := w.Do(ctx, "finish", web.WizardInput{}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-w.done:
	default:
		t.Fatal("finish didn't end setup mode")
	}
}

func TestWizardTokenAndAccess(t *testing.T) {
	f := &fakeGitHub{labels: map[string]bool{}}
	w := testWizard(t, f, "o/r", true, noDocker)
	ctx := context.Background()
	if _, err := w.Do(ctx, "finish", web.WizardInput{}); err == nil {
		t.Fatal("finish without access must be refused")
	}
	if _, err := w.Do(ctx, "github/token", web.WizardInput{Token: " "}); err == nil {
		t.Fatal("an empty token must be refused")
	}
	msg, err := w.Do(ctx, "github/token", web.WizardInput{Token: "ghp_secret_token_1234"})
	if err != nil || !strings.Contains(msg, "can reach o/r") {
		t.Fatalf("token: %q %v", msg, err)
	}
	st := w.State(ctx, false)
	if st.GitHub.Method != "token" || st.GitHub.Token.Masked != "ghp_…1234" || st.Access[0].State != "ok" {
		t.Fatalf("state = %+v %+v", st.GitHub, st.Access)
	}
	// No secret leaves, whole.
	b, _ := json.Marshal(st)
	if strings.Contains(string(b), "ghp_secret_token_1234") {
		t.Fatal("the token was sent back")
	}
	// Warnings say what won't work: no model keys yet.
	if !st.CanFinish || !strings.Contains(strings.Join(st.Warnings, " "), "Claude has no key") {
		t.Fatalf("finish = %v %v", st.CanFinish, st.Warnings)
	}
	if s := step(st, "machine"); s.State != "attention" {
		t.Fatalf("machine = %+v", s)
	}
}

func TestWizardReposReloadTheConfig(t *testing.T) {
	w := testWizard(t, &fakeGitHub{}, "o/r", false, noDocker)
	ctx := context.Background()
	if _, err := w.Do(ctx, "repos", web.WizardInput{Repos: "o/r, o/"}); err == nil {
		t.Fatal("a bad repo must be refused")
	}
	if _, err := w.Do(ctx, "repos", web.WizardInput{Repos: "o/api, https://gitlab.com/g/web"}); err != nil {
		t.Fatal(err)
	}
	st := w.State(ctx, false)
	if strings.Join(st.Repos, ",") != "o/api,gitlab.com/g/web" || st.GitHub == nil || st.GitLab == nil || !st.Changed {
		t.Fatalf("state = %v gitlab %v changed %v", st.Repos, st.GitLab, st.Changed)
	}
	if h := st.GitLab.Hosts; len(h) != 1 || !strings.Contains(h[0].LegacyURL, "gitlab.com/-/user_settings/personal_access_tokens?name=factory&scopes=api,write_repository") {
		t.Fatalf("gitlab hosts = %+v", h)
	}
	if cfg, _ := config.Load(w.cfgPath); len(cfg.Repos) != 2 {
		t.Fatalf("factory.yaml has %v", cfg.RepoNames())
	}
	// Finish in normal mode restarts to apply it.
	if _, err := w.Do(ctx, "repos", web.WizardInput{Repos: "o/api o/web"}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Do(ctx, "github/token", web.WizardInput{Token: "ghp_token_123456789"}); err != nil {
		t.Fatal(err)
	}
	restarts := 0
	w.restart = func(context.Context) int { restarts++; return 1 }
	msg, err := w.Do(ctx, "finish", web.WizardInput{})
	if err != nil || restarts != 1 || !strings.Contains(msg, "running job finishes") {
		t.Fatalf("finish: %q %v, %d restarts", msg, err, restarts)
	}
}

func TestWizardModelKeys(t *testing.T) {
	w := testWizard(t, &fakeGitHub{}, "o/r", false, noDocker)
	ctx := context.Background()
	if _, err := w.Do(ctx, "claude", web.WizardInput{Key: "hello"}); err == nil || !strings.Contains(err.Error(), "sk-ant-") {
		t.Fatalf("an odd key: %v", err)
	}
	if _, err := w.Do(ctx, "claude", web.WizardInput{Key: "sk-ant-api03-abcdefghijk"}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Do(ctx, "claude", web.WizardInput{Key: "sk-ant-oat01-subscription"}); err != nil {
		t.Fatal(err)
	}
	vals, _ := readEnvFile(w.envPath)
	if vals["CLAUDE_CODE_OAUTH_TOKEN"] != "sk-ant-oat01-subscription" || vals["ANTHROPIC_API_KEY"] != "" {
		t.Fatalf("claude: %v", vals)
	}
	if c := w.State(ctx, false).Claude; c.Kind != "subscription" || c.Key.Masked != "sk-a…tion" {
		t.Fatalf("claude state = %+v", c)
	}
	if _, err := w.Do(ctx, "claude", web.WizardInput{Key: "hello", Anyway: true}); err != nil {
		t.Fatal(err)
	}
	if vals, _ := readEnvFile(w.envPath); vals["ANTHROPIC_API_KEY"] != "hello" {
		t.Fatalf("anyway: %v", vals)
	}

	if _, err := w.Do(ctx, "codex", web.WizardInput{Mode: codexAPIKey, Key: "sk-openai-123"}); err != nil {
		t.Fatal(err)
	}
	msg, err := w.Do(ctx, "codex", web.WizardInput{Mode: codexChatGPT})
	if err != nil || !strings.Contains(msg, "sign in with ChatGPT") {
		t.Fatalf("chatgpt: %q %v", msg, err)
	}
	vals, _ = readEnvFile(w.envPath)
	if vals["FACTORY_CODEX_AUTH"] != codexChatGPT || vals["OPENAI_API_KEY"] != "" {
		t.Fatalf("codex: %v", vals)
	}
	if b, _ := os.ReadFile(w.envPath); !strings.Contains(string(b), "# OPENAI_API_KEY=sk-openai-123") {
		t.Fatalf("the API key is commented out, not lost:\n%s", b)
	}
	if c := w.State(ctx, false).Codex; c.Mode != codexChatGPT || c.SignedIn || !c.ReviewOnly {
		t.Fatalf("codex state = %+v", c)
	}
	if _, err := w.Do(ctx, "codex", web.WizardInput{Mode: codexAccessToken}); err == nil {
		t.Fatal("an access token is needed")
	}

	if _, err := w.Do(ctx, "notify", web.WizardInput{URL: "https://example.com/hook"}); err == nil {
		t.Fatal("an unknown notifier must be refused")
	}
	if _, err := w.Do(ctx, "notify", web.WizardInput{URL: "https://ntfy.sh/my-topic-123"}); err != nil {
		t.Fatal(err)
	}
	if n := w.State(ctx, false).Notify; !n.Set {
		t.Fatalf("notify = %+v", n)
	}
}

func TestWizardReadsTheEnvironmentButNotGitHubToken(t *testing.T) {
	w := testWizard(t, &fakeGitHub{}, "o/r", false, noDocker)
	w.exported = map[string]string{"GITHUB_TOKEN": "ghp_from_gh_cli_123", "ANTHROPIC_API_KEY": "sk-ant-api03-exported"}
	st := w.State(context.Background(), false)
	if st.GitHub.Method != "none" || st.Claude.Kind != "api_key" {
		t.Fatalf("github %+v claude %+v", st.GitHub, st.Claude)
	}
}

func TestWizardBuildsImages(t *testing.T) {
	built := make(chan struct{})
	run := func(out io.Writer, name string, args ...string) error {
		switch {
		case len(args) > 1 && args[0] == "image" && args[1] == "inspect":
			select {
			case <-built:
				return nil
			default:
				return errors.New("no such image")
			}
		case len(args) > 0 && args[0] == "build":
			io.WriteString(out, "Step 1/9 : FROM debian\n")
			close(built)
		}
		return nil
	}
	w := testWizard(t, &fakeGitHub{}, "o/r", false, run)
	ctx := context.Background()
	w.checkMachine(ctx)
	if s := step(w.State(ctx, false), "images"); s.State != "attention" || !strings.Contains(s.Summary, "to build") {
		t.Fatalf("images = %+v", s)
	}
	if _, err := w.Do(ctx, "images/build", web.WizardInput{}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		st := w.State(ctx, false)
		if !st.Images.Building && step(st, "images").State == "done" {
			if !strings.Contains(st.Images.Log, "Step 1/9") || st.Images.Failed {
				t.Fatalf("images = %+v", st.Images)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("build didn't finish: %+v", st.Images)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Sign in with ChatGPT runs Codex's device-code sign-in from the page: it
// shows the link and code Codex prints, then saves the sign-in.
func TestWizardSignsCodexIn(t *testing.T) {
	entered := make(chan struct{})
	run := func(out io.Writer, name string, args ...string) error {
		if name != "docker" || len(args) == 0 || args[0] != "run" {
			return nil // the job image is built
		}
		if slices.Contains(args, "-t") {
			t.Errorf("no terminal here, yet -t in %q", args)
		}
		io.WriteString(out, "Follow these steps to sign in with ChatGPT using device code authorization:\n\n"+
			"1. Open this link in your browser and sign in to your account\n   \x1b[94mhttps://auth.openai.com/codex/device\x1b[0m\n\n"+
			"2. Enter this one-time code \x1b[90m(expires in 15 minutes)\x1b[0m\n   \x1b[94mABCD-12345\x1b[0m\n")
		<-entered
		for i, a := range args {
			if a == "-v" && strings.HasSuffix(args[i+1], ":/codex") {
				return os.WriteFile(filepath.Join(strings.TrimSuffix(args[i+1], ":/codex"), "auth.json"), []byte(testAuth), 0o600)
			}
		}
		return errors.New("no CODEX_HOME mounted")
	}
	w := testWizard(t, &fakeGitHub{}, "o/r", false, run)
	ctx := context.Background()
	if _, err := w.Do(ctx, "codex/sign-in", web.WizardInput{}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	var si web.APIWizardSignIn
	for {
		si = w.State(ctx, false).Codex.SignIn
		if si.Code != "" || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !si.Running || si.URL != "https://auth.openai.com/codex/device" || si.Code != "ABCD-12345" || strings.Contains(si.Log, "\x1b") {
		t.Fatalf("sign-in = %+v", si)
	}
	if _, err := w.Do(ctx, "codex/sign-in", web.WizardInput{}); err == nil {
		t.Fatal("a second sign-in started")
	}
	close(entered)
	for {
		st := w.State(ctx, false)
		if !st.Codex.SignIn.Running {
			if !st.Codex.SignedIn || st.Codex.SignIn.Failed || st.Codex.Mode != codexChatGPT || step(st, "codex").State != "done" {
				t.Fatalf("codex = %+v", st.Codex)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("sign-in didn't finish")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if vals, _ := readEnvFile(w.envPath); vals["FACTORY_CODEX_AUTH"] != codexChatGPT {
		t.Fatalf("env %v", vals)
	}
}

// A job image the page can build shows a Build button, not a command.
func TestWizardImageFindingIsAButton(t *testing.T) {
	w := testWizard(t, &fakeGitHub{}, "o/r", false, noDocker)
	w.machineCheck = func(context.Context, *config.Config) []finding {
		return []finding{{fail, "Job image factory-job:latest (for o/r) isn't built", fixBuildImages}, {fail, "Job image mine:1 (for o/r) isn't built", "build it yourself"}}
	}
	ctx := context.Background()
	w.checkMachine(ctx)
	m := w.State(ctx, false).Machine
	if len(m) != 2 || m[0].Action != "build_images" || m[0].Fix != "" || m[1].Action != "" || m[1].Fix != "build it yourself" {
		t.Fatalf("machine = %+v", m)
	}
}
