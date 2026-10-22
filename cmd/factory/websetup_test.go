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
	"regexp"
	"slices"
	"strings"
	"sync"
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
	oldAPI, oldWeb, oldRun, oldCtx := githubAPI, githubWeb, runCmd, runCmdContext
	githubAPI, githubWeb, runCmd = srv.URL, "https://github.example", run
	runCmdContext = func(_ context.Context, out io.Writer, name string, args ...string) error {
		return run(out, name, args...)
	}
	t.Cleanup(func() { githubAPI, githubWeb, runCmd, runCmdContext = oldAPI, oldWeb, oldRun, oldCtx })
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
	if !st.CanFinish || !strings.Contains(strings.Join(st.Warnings, " "), "Neither Claude nor Codex is connected") {
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

	// Codex comes second, so it reviews what Claude builds.
	if msg, err := w.Do(ctx, "codex", web.WizardInput{Mode: codexAPIKey, Key: "sk-openai-123"}); err != nil || !strings.Contains(msg, "reviews what it builds") {
		t.Fatalf("codex: %q %v", msg, err)
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
	if st := w.State(ctx, false); st.Codex.Mode != codexChatGPT || st.Codex.SignedIn || st.Codex.Role != "reviews" || st.Claude.Role != "builds" {
		t.Fatalf("codex state = %+v, claude %+v", st.Codex, st.Claude)
	}
	if _, err := w.Do(ctx, "codex", web.WizardInput{Mode: codexAccessToken}); err == nil {
		t.Fatal("an access token is needed")
	}
	// Disconnecting Codex leaves Claude doing everything.
	if msg, err := w.Do(ctx, "codex/disconnect", web.WizardInput{}); err != nil || !strings.Contains(msg, "Claude now builds and reviews") {
		t.Fatalf("codex/disconnect: %q %v", msg, err)
	}
	if st := w.State(ctx, false); st.Codex.Role != "" || st.Claude.Role != "builds and reviews" || step(st, "codex").State != "optional" {
		t.Fatalf("after codex/disconnect: codex %+v, claude %+v", st.Codex, st.Claude)
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

// The page reads the settings as the factory runs with them: an exported
// variable wins over the saved one, and the page says which it overrides,
// by name only.
func TestWizardReadsTheEnvironmentAsTheFactoryDoes(t *testing.T) {
	w := testWizard(t, &fakeGitHub{}, "o/r", false, noDocker)
	if err := w.save(map[string]string{"ANTHROPIC_API_KEY": "sk-ant-api03-savedsaved", "FACTORY_NOTIFY_URL": "https://ntfy.sh/saved-topic"}); err != nil {
		t.Fatal(err)
	}
	w.exported = map[string]string{"GITHUB_TOKEN": "ghp_from_gh_cli_123", "ANTHROPIC_API_KEY": "sk-ant-api03-exported", "FACTORY_NOTIFY_URL": "https://ntfy.sh/saved-topic"}
	st := w.State(context.Background(), false)
	if st.GitHub.Method != "token" || st.Claude.Key.Masked != mask("sk-ant-api03-exported") {
		t.Fatalf("github %+v claude %+v", st.GitHub, st.Claude)
	}
	var over []string
	for _, x := range st.Warnings {
		if strings.Contains(x, "overrides") {
			over = append(over, x)
		}
	}
	// Only the key whose exported value differs, and never a value.
	if len(over) != 1 || !strings.HasPrefix(over[0], "ANTHROPIC_API_KEY is set in the environment") || strings.Contains(over[0], "sk-ant") {
		t.Fatalf("warnings %q", st.Warnings)
	}
}

func TestWizardBuildsImages(t *testing.T) {
	built := make(chan struct{})
	run := func(out io.Writer, name string, args ...string) error {
		switch {
		case len(args) > 1 && args[0] == "image" && args[1] == "inspect":
			select {
			case <-built:
				io.WriteString(out, "from=") // built on the default base
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
	f := fakeCodexLogin()
	w := testWizard(t, &fakeGitHub{}, "o/r", false, f.run)
	f.install(t)
	ctx := context.Background()
	if _, err := w.Do(ctx, "codex/sign-in", web.WizardInput{}); err != nil {
		t.Fatal(err)
	}
	entered := f.entered
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
	// Finish would restart the factory under it.
	if _, err := w.Do(ctx, "finish", web.WizardInput{}); err == nil || !strings.Contains(err.Error(), "sign-in is running") {
		t.Fatalf("finish while signing in: %v", err)
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

// codexLogin fakes `codex login --device-auth` in the job image: it prints
// the link and code, then saves a sign-in once entered is closed, or
// stops when its context does.
type codexLoginFake struct {
	entered chan struct{}
	late    bool // it signs in even once stopped, as if the code was in first
	fail    bool // with late, Codex exits saving no sign-in
	mu      sync.Mutex
	cmds    []string
	homes   []string
}

func fakeCodexLogin() *codexLoginFake {
	return &codexLoginFake{entered: make(chan struct{})}
}

// install puts the fake in place of Codex, after testWizard set the rest.
func (f *codexLoginFake) install(t *testing.T) {
	t.Helper()
	old := runCmdContext
	runCmdContext = func(ctx context.Context, out io.Writer, name string, args ...string) error {
		if slices.Contains(args, "-t") {
			t.Errorf("no terminal here, yet -t in %q", args)
		}
		home := ""
		for i, a := range args {
			if a == "-v" && strings.HasSuffix(args[i+1], ":/codex") {
				home = strings.TrimSuffix(args[i+1], ":/codex")
			}
		}
		f.mu.Lock()
		f.cmds = append(f.cmds, name+" "+strings.Join(args, " "))
		f.homes = append(f.homes, home)
		f.mu.Unlock()
		io.WriteString(out, "Follow these steps to sign in with ChatGPT using device code authorization:\n\n"+
			"1. Open this link in your browser and sign in to your account\n   \x1b[94mhttps://auth.openai.com/codex/device\x1b[0m\n\n"+
			"2. Enter this one-time code \x1b[90m(expires in 15 minutes)\x1b[0m\n   \x1b[94mABCD-12345\x1b[0m\n")
		if f.late {
			<-f.entered // Codex already saw the code: it finishes anyway
			if f.fail {
				return nil
			}
		} else {
			select {
			case <-f.entered:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if home == "" {
			return errors.New("no CODEX_HOME mounted")
		}
		return os.WriteFile(filepath.Join(home, "auth.json"), []byte(testAuth), 0o600)
	}
	t.Cleanup(func() { runCmdContext = old })
}

// run is the rest of Docker: the image is built, and rm is recorded.
func (f *codexLoginFake) run(_ io.Writer, name string, args ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cmds = append(f.cmds, name+" "+strings.Join(args, " "))
	return nil
}

func (f *codexLoginFake) ran(prefix string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.cmds {
		if strings.HasPrefix(c, prefix) {
			out = append(out, c)
		}
	}
	return out
}

// Cancel stops Codex and its container, and removes its folder.
func TestWizardCancelsCodexSignIn(t *testing.T) {
	f := fakeCodexLogin()
	w := testWizard(t, &fakeGitHub{}, "o/r", false, f.run)
	f.install(t)
	ctx := context.Background()
	if _, err := w.Do(ctx, "codex/sign-in/cancel", web.WizardInput{}); err == nil {
		t.Fatal("cancelled a sign-in that isn't running")
	}
	if _, err := w.Do(ctx, "codex/sign-in", web.WizardInput{}); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); w.State(ctx, false).Codex.SignIn.Code == ""; {
		if time.Now().After(deadline) {
			t.Fatal("Codex never showed a code")
		}
		time.Sleep(5 * time.Millisecond)
	}
	msg, err := w.Do(ctx, "codex/sign-in/cancel", web.WizardInput{})
	if err != nil || msg != "Sign-in cancelled." {
		t.Fatalf("cancel = %q %v", msg, err)
	}
	si := w.State(ctx, false).Codex
	if si.SignIn.Running || si.SignIn.Failed || si.SignIn.Result != "Sign-in cancelled." || si.SignedIn {
		t.Fatalf("after cancel = %+v", si)
	}
	run := f.ran("docker run")
	if len(run) != 1 {
		t.Fatalf("ran %q", f.cmds)
	}
	name := regexp.MustCompile(`--name (\S+)`).FindStringSubmatch(run[0])
	if name == nil || !slices.Contains(f.ran("docker rm"), "docker rm -f "+name[1]) {
		t.Fatalf("container not removed: %q", f.cmds)
	}
	if _, err := os.Stat(f.homes[0]); !os.IsNotExist(err) {
		t.Fatalf("its folder %s is still there: %v", f.homes[0], err)
	}
	// Nothing running now, so Finish isn't held.
	if why := w.busy(); why != "" {
		t.Fatal(why)
	}
}

// A page closed mid-check doesn't leave "can't reach" on every repo: the
// check runs on. A repo a slow forge doesn't answer for in time is marked
// timed out, and the others keep what they found, so Finish can pass.
func TestWizardAccessCheckOutlivesTheRequest(t *testing.T) {
	w := testWizard(t, &fakeGitHub{labels: map[string]bool{}, slow: "o/slow", denied: "o/denied"}, "o/r", false, noDocker)
	if err := w.save(map[string]string{"GITHUB_TOKEN": "ghp_token123"}); err != nil {
		t.Fatal(err)
	}
	gone, cancel := context.WithCancel(context.Background())
	cancel()
	if a := w.State(gone, true).Access; len(a) != 1 || a[0].State != "ok" {
		t.Fatalf("access after a closed request = %+v", a)
	}

	if err := setRepos(w.cfgPath, []string{"o/slow", "o/r", "o/denied"}); err != nil {
		t.Fatal(err)
	}
	if err := w.reload(); err != nil {
		t.Fatal(err)
	}
	old := accessWait
	accessWait = 300 * time.Millisecond
	t.Cleanup(func() { accessWait = old })
	start := time.Now()
	w.checkAccess(context.Background())
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("took %s", d)
	}
	a := w.State(context.Background(), false).Access
	// The slow one comes first, so o/r was checked beside it, not after.
	if len(a) != 3 || a[1].State != "ok" || a[0].State != "failed" || !strings.Contains(a[0].Error, "timed out") {
		t.Fatalf("access = %+v", a)
	}
	// A real refusal keeps its own reason, even when the clock ran out.
	if a[2].State != "failed" || strings.Contains(a[2].Error, "timed out") {
		t.Fatalf("o/denied = %+v", a[2])
	}
}

// Finish waits while an image builds, since the restart would cut it off.
func TestWizardFinishWaitsForAnImageBuild(t *testing.T) {
	d := &fakeDocker{gate: make(chan struct{}), building: make(chan string, 1), built: map[string]bool{}}
	w := testWizard(t, &fakeGitHub{}, "o/r", false, d.run)
	if _, err := w.Do(context.Background(), "images/build", web.WizardInput{}); err != nil {
		t.Fatal(err)
	}
	<-d.building
	if _, err := w.Do(context.Background(), "finish", web.WizardInput{}); err == nil || !strings.Contains(err.Error(), "being built") {
		t.Fatalf("finish while building: %v", err)
	}
	close(d.gate)
	w.builder.wait()
	if why := w.busy(); why != "" {
		t.Fatal(why)
	}
}

// The page may only switch base_branch to the default the access check
// found, not write any branch it is sent.
func TestWizardBaseBranchIsOnlyTheDefault(t *testing.T) {
	w := testWizard(t, &fakeGitHub{}, "o/r", false, noDocker)
	ctx := context.Background()
	in := web.WizardInput{Repo: "o/r", Branch: "trunk"}
	if _, err := w.Do(ctx, "base-branch", in); err == nil || !strings.Contains(err.Error(), "check access first") {
		t.Fatalf("before a check: %v", err)
	}
	w.mu.Lock()
	w.access["o/r"] = web.APIWizardAccess{Repo: "o/r", State: "ok", DefaultBranch: "trunk"}
	w.mu.Unlock()
	if _, err := w.Do(ctx, "base-branch", web.WizardInput{Repo: "o/r", Branch: "trunk\nevil: 1"}); err == nil {
		t.Fatal("took a branch that isn't the default")
	}
	if _, err := w.Do(ctx, "base-branch", in); err != nil {
		t.Fatal(err)
	}
	if rc, _ := w.config().RepoConf("o/r"); rc.BaseBranch != "trunk" {
		t.Fatalf("base branch %q", rc.BaseBranch)
	}
}

func pollSignIn(t *testing.T, w *wizard, until func(web.APIWizardSignIn) bool) web.APIWizardSignIn {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); ; {
		si := w.State(context.Background(), false).Codex.SignIn
		if until(si) {
			return si
		}
		if time.Now().After(deadline) {
			t.Fatalf("sign-in = %+v", si)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A Cancel that lands after Codex got its code reports the sign-in.
func TestWizardCancelAfterSignInSaysSignedIn(t *testing.T) {
	f := fakeCodexLogin()
	f.late = true
	w := testWizard(t, &fakeGitHub{}, "o/r", false, f.run)
	f.install(t)
	ctx := context.Background()
	if _, err := w.Do(ctx, "codex/sign-in", web.WizardInput{}); err != nil {
		t.Fatal(err)
	}
	pollSignIn(t, w, func(si web.APIWizardSignIn) bool { return si.Code != "" })
	got := make(chan string, 1)
	go func() {
		msg, _ := w.cancelCodexSignIn()
		got <- msg
	}()
	for {
		w.mu.Lock()
		asked := w.signIn.cancelled
		w.mu.Unlock()
		if asked {
			break
		}
		time.Sleep(time.Millisecond)
	}
	close(f.entered)
	if msg := <-got; msg != "Signed in. Codex uses your ChatGPT plan." {
		t.Fatalf("cancel said %q", msg)
	}
	if !w.State(ctx, false).Codex.SignedIn {
		t.Fatal("the sign-in wasn't kept")
	}
	if rm := f.ran("docker rm"); len(rm) != 0 {
		t.Fatalf("removed a container that finished: %q", rm)
	}
}

// A sign-in that failed just before Cancel landed is an error, not a success.
func TestWizardCancelAfterFailedSignInErrs(t *testing.T) {
	f := fakeCodexLogin()
	f.late, f.fail = true, true
	w := testWizard(t, &fakeGitHub{}, "o/r", false, f.run)
	f.install(t)
	ctx := context.Background()
	if _, err := w.Do(ctx, "codex/sign-in", web.WizardInput{}); err != nil {
		t.Fatal(err)
	}
	pollSignIn(t, w, func(si web.APIWizardSignIn) bool { return si.Code != "" })
	type answer struct {
		msg string
		err error
	}
	got := make(chan answer, 1)
	go func() {
		msg, err := w.cancelCodexSignIn()
		got <- answer{msg, err}
	}()
	for {
		w.mu.Lock()
		asked := w.signIn.cancelled
		w.mu.Unlock()
		if asked {
			break
		}
		time.Sleep(time.Millisecond)
	}
	close(f.entered)
	a := <-got
	if a.err == nil || a.msg != "" {
		t.Fatalf("cancel said %q, %v", a.msg, a.err)
	}
	if w.State(ctx, false).Codex.SignedIn {
		t.Fatal("signed in after a failure")
	}
}

// A code nobody enters ends the sign-in, so it can't hold a restart.
func TestWizardSignInTimesOut(t *testing.T) {
	old := signInWait
	signInWait = 50 * time.Millisecond
	t.Cleanup(func() { signInWait = old })
	f := fakeCodexLogin()
	w := testWizard(t, &fakeGitHub{}, "o/r", false, f.run)
	f.install(t)
	if _, err := w.Do(context.Background(), "codex/sign-in", web.WizardInput{}); err != nil {
		t.Fatal(err)
	}
	si := pollSignIn(t, w, func(si web.APIWizardSignIn) bool { return !si.Running })
	if !si.Failed || !strings.Contains(si.Result, "timed out") {
		t.Fatalf("sign-in = %+v", si)
	}
	if w.waitingOn() != "" || len(f.ran("docker rm -f factory-codex-login-")) != 1 {
		t.Fatalf("still waiting on %q, ran %q", w.waitingOn(), f.cmds)
	}
}

// A container Docker hadn't made yet when the sign-in stopped is still
// removed.
func TestRemoveContainerRetries(t *testing.T) {
	oldRun, oldWait := runCmd, rmRetryWait
	rmRetryWait = time.Millisecond
	var tries int
	runCmd = func(out io.Writer, _ string, _ ...string) error {
		tries++
		if tries < 3 {
			io.WriteString(out, "Error response from daemon: No such container: x\n")
			return errors.New("exit status 1")
		}
		return nil
	}
	t.Cleanup(func() { runCmd, rmRetryWait = oldRun, oldWait })
	removeContainer("x")
	if tries != 3 {
		t.Fatalf("tries = %d", tries)
	}
}

// A pasted token an exported GITHUB_TOKEN overrides says the exported
// one is what the factory uses, and what was checked.
func TestWizardTokenSaveNamesTheExportedOne(t *testing.T) {
	w := testWizard(t, &fakeGitHub{labels: map[string]bool{}}, "o/r", false, noDocker)
	w.exported = map[string]string{"GITHUB_TOKEN": "ghp_from_gh_cli_123"}
	msg, err := w.Do(context.Background(), "github/token", web.WizardInput{Token: "ghp_pasted_456"})
	if err != nil || !strings.Contains(msg, "GITHUB_TOKEN exported where the factory starts wins") || strings.Contains(msg, "ghp_") {
		t.Fatalf("msg %q %v", msg, err)
	}
	w.exported = map[string]string{"GITHUB_TOKEN": "ghp_pasted_456"}
	if msg, _ := w.Do(context.Background(), "github/token", web.WizardInput{Token: "ghp_pasted_456"}); strings.Contains(msg, "exported") {
		t.Fatalf("the same token: %q", msg)
	}
}

// The access check reads each repo's files for its checks: the page shows
// the suggestion, with the command the repo's AGENTS.md gives, and saving
// writes them to factory.yaml.
func TestWizardChecks(t *testing.T) {
	f := &fakeGitHub{labels: map[string]bool{}, files: map[string]map[string]string{"o/r": {
		"Gemfile":             "gem \"rails\"\ngem \"rspec-rails\"\ngem \"rubocop\"\n",
		"spec/factories/x.rb": "",
		"AGENTS.md":           "Tests are Minitest: run `bin/rails test <path>`.",
	}}}
	w := testWizard(t, f, "o/r", false, noDocker)
	ctx := context.Background()
	if _, err := w.Do(ctx, "github/token", web.WizardInput{Token: "ghp_token_123456789"}); err != nil {
		t.Fatal(err)
	}
	st := w.State(ctx, false)
	if s := step(st, "checks"); s.State != "attention" || !strings.Contains(s.Summary, "Confirm the checks for o/r") {
		t.Fatalf("checks step = %+v", s)
	}
	c := st.Checks[0]
	byGate := map[string]web.APIWizardGate{}
	for _, g := range c.Gates {
		byGate[g.Gate] = g
	}
	if c.Saved || c.Stack != "Ruby on Rails" || byGate["test"].Command != "bin/rails db:prepare test" || byGate["test"].Source != "AGENTS.md" || byGate["lint"].Command != "bundle exec rubocop" {
		t.Fatalf("checks = %+v", c)
	}
	if _, err := w.Do(ctx, "checks/save", web.WizardInput{Repo: "o/r", Commands: map[string]string{"test": " "}}); err == nil {
		t.Fatal("saving no checks must be refused")
	}
	msg, err := w.Do(ctx, "checks/save", web.WizardInput{Repo: "o/r", Install: "bundle install", ImageFrom: "ruby:3.4-bookworm",
		Commands: map[string]string{"test": "bin/rails db:prepare test", "lint": "bundle exec rubocop", "types": ""}})
	if err != nil || !strings.Contains(msg, "lint, test") {
		t.Fatalf("save: %q %v", msg, err)
	}
	cfg, _ := config.Load(w.cfgPath)
	rc, _ := cfg.RepoConf("o/r")
	if rc.Gates.Commands["test"] != "bin/rails db:prepare test" || strings.Join(rc.Gates.Required, ",") != "lint,test" || rc.ImageFrom != "ruby:3.4-bookworm" || cfg.Repos[0].Gates.ProvesFix != "none" {
		t.Fatalf("saved = %+v image %q", rc.Gates, rc.ImageFrom)
	}
	after := w.State(ctx, false)
	if s := step(after, "checks"); s.State != "done" {
		t.Fatalf("after saving: %+v", s)
	}
	// Saving reloads the config: access is checked again, and no list
	// goes to the page as null.
	if after.Access[0].State != "ok" {
		t.Fatalf("access after saving = %+v", after.Access)
	}
	w.credentialsChanged() // as a reload does, before the check runs again
	if b, _ := json.Marshal(w.State(ctx, false)); strings.Contains(string(b), `"missing_labels":null`) {
		t.Fatalf("an unchecked repo's labels went as null: %s", b)
	}
	// A saved command the repo's instructions disagree with is flagged.
	if _, err := w.Do(ctx, "checks/save", web.WizardInput{Repo: "o/r", Install: "bundle install", Commands: map[string]string{"test": "bundle exec rspec"}}); err != nil {
		t.Fatal(err)
	}
	st = w.State(ctx, false)
	if s := step(st, "checks"); s.State != "attention" || !strings.Contains(s.Summary, "agent instructions") {
		t.Fatalf("disagreeing: %+v", s)
	}
	if g := st.Checks[0].Gates; !slices.ContainsFunc(g, func(g web.APIWizardGate) bool {
		return g.Gate == "test" && g.Command == "bundle exec rspec" && g.Suggested == "bin/rails db:prepare test"
	}) {
		t.Fatalf("gates = %+v", g)
	}
}

// The page tries its checks before saving them: the install and each check
// run with the commands and services on the page, and it follows each step.
// Saving keeps the services picked and their environment.
func TestWizardTriesAndSavesServices(t *testing.T) {
	f := &fakeGitHub{labels: map[string]bool{}, files: map[string]map[string]string{"o/r": {
		"Gemfile":             "gem \"rails\"\ngem \"pg\"\ngem \"redis\"\n",
		"test/test_helper.rb": "",
	}}}
	w := testWizard(t, f, "o/r", false, noDocker)
	ctx := context.Background()
	if _, err := w.Do(ctx, "github/token", web.WizardInput{Token: "ghp_token_123456789"}); err != nil {
		t.Fatal(err)
	}
	c := w.State(ctx, false).Checks[0]
	if len(c.Services) != 2 || c.Services[0].Name != "postgres" || c.Services[0].Saved || c.Env["DATABASE_URL"] == "" || c.Env["REDIS_URL"] == "" || c.Try != nil {
		t.Fatalf("suggested = %+v env %v", c.Services, c.Env)
	}
	release := make(chan struct{})
	var ran *config.Config
	old := tryRun
	t.Cleanup(func() { tryRun = old })
	tryRun = func(_ context.Context, cfg *config.Config, repo string, _ clientFor, update func(func(*web.APIWizardTry))) error {
		ran = cfg
		update(func(t *web.APIWizardTry) { t.Steps[0].State = "pass" })
		<-release
		update(func(t *web.APIWizardTry) { t.Steps[1].State, t.Steps[1].Output = "fail", "PG::ConnectionBad" })
		return nil
	}
	in := web.WizardInput{Repo: "o/r", Install: "bundle install", Commands: map[string]string{"test": "bin/rails db:prepare test"},
		Services: []string{"postgres"}, Env: map[string]string{"DATABASE_URL": "postgres://postgres:factory@postgres:5432/test"}}
	if msg, err := w.Do(ctx, "checks/try", in); err != nil || !strings.Contains(msg, "Trying o/r's checks") {
		t.Fatalf("try: %q %v", msg, err)
	}
	if _, err := w.Do(ctx, "checks/try", in); err == nil {
		t.Fatal("a second try while one runs")
	}
	try := w.State(ctx, false).Checks[0].Try
	if try == nil || !try.Running || len(try.Steps) != 2 || try.Steps[0].Gate != "install" {
		t.Fatalf("while trying = %+v", try)
	}
	close(release)
	for deadline := time.Now().Add(5 * time.Second); w.State(ctx, false).Checks[0].Try.Running; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the try never ended")
		}
	}
	try = w.State(ctx, false).Checks[0].Try
	if try.Steps[1].State != "fail" || try.Steps[1].Output != "PG::ConnectionBad" {
		t.Fatalf("after = %+v", try)
	}
	// It ran with what was on the page, not saved yet.
	rc, _ := ran.RepoConf("o/r")
	if rc.Gates.Commands["test"] != "bin/rails db:prepare test" || rc.Services["postgres"].Image != "postgres:16" || rc.Services["redis"].Image != "" || rc.Env["DATABASE_URL"] == "" {
		t.Fatalf("tried %+v services %v env %v", rc.Gates, rc.Services, rc.Env)
	}
	if cfg, _ := config.Load(w.cfgPath); cfg.Repos[0].Gates != nil {
		t.Fatal("trying saved the checks")
	}
	if _, err := w.Do(ctx, "checks/save", in); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(w.cfgPath)
	if r := cfg.Repos[0]; r.Services["postgres"].Ready != "pg_isready -U postgres" || len(r.Services) != 1 || r.Env["DATABASE_URL"] == "" {
		t.Fatalf("saved services %v env %v", r.Services, r.Env)
	}
	if c := w.State(ctx, false).Checks[0]; len(c.Services) != 2 || !c.Services[0].Saved || c.Services[1].Saved {
		t.Fatalf("after saving = %+v", c.Services)
	}
	if _, err := w.Do(ctx, "checks/save", web.WizardInput{Repo: "o/r", Commands: map[string]string{"test": "x"}, Services: []string{"mongo"}}); err == nil {
		t.Fatal("a service the page didn't show was saved")
	}
}

// A new factory names no repo: the page asks for them first, shows no
// host until one is chosen, and can't finish.
func TestWizardStartsWithNoRepos(t *testing.T) {
	w := testWizard(t, &fakeGitHub{}, "o/r", true, noDocker)
	b, _ := os.ReadFile(w.cfgPath)
	os.WriteFile(w.cfgPath, []byte(strings.Replace(string(b), "repo: o/r\n", "# repo: octocat/hello-world\n", 1)), 0o644)
	if err := w.reload(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	st := w.State(ctx, false)
	if s := step(st, "repos"); s.State != "attention" || st.GitHub != nil || st.GitLab != nil || st.CanFinish || len(st.Repos) != 0 {
		t.Fatalf("repos %+v, github %v, gitlab %v, finish %v", s, st.GitHub, st.GitLab, st.CanFinish)
	}
	if _, err := w.Do(ctx, "finish", web.WizardInput{}); err == nil {
		t.Fatal("finish with no repos must be refused")
	}
	if _, err := w.Do(ctx, "repos", web.WizardInput{Repos: "https://gitlab.com/g/p"}); err != nil {
		t.Fatal(err)
	}
	// Saving repos checks the machine again in the background: let it end before the test does.
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		w.mu.Lock()
		checked := !w.machineAt.IsZero()
		w.mu.Unlock()
		if checked || time.Now().After(deadline) {
			break
		}
	}
	if st = w.State(ctx, false); st.GitLab == nil || st.GitHub != nil || step(st, "repos").State != "done" {
		t.Fatalf("after choosing a GitLab project: github %v, gitlab %v", st.GitHub, st.GitLab)
	}
}

// A token pasted in the wrong host's field is refused, and Remove takes
// a saved token out of factory.env altogether.
func TestWizardRemovesAToken(t *testing.T) {
	w := testWizard(t, &fakeGitHub{labels: map[string]bool{}}, "o/r", true, noDocker)
	ctx := context.Background()
	if _, err := w.Do(ctx, "github/token", web.WizardInput{Token: "glpat-abcdefghijklmnop"}); err == nil || !strings.Contains(err.Error(), "GitLab token") {
		t.Fatalf("a GitLab token in GitHub's field: %v", err)
	}
	if _, err := w.Do(ctx, "gitlab/token", web.WizardInput{Token: "github_pat_abcdefghij"}); err == nil || !strings.Contains(err.Error(), "GitHub token") {
		t.Fatalf("a GitHub token in GitLab's field: %v", err)
	}
	if _, err := w.Do(ctx, "github/token", web.WizardInput{Token: "ghp_secret_token_1234"}); err != nil {
		t.Fatal(err)
	}
	msg, err := w.Do(ctx, "github/disconnect", web.WizardInput{})
	if err != nil || !strings.Contains(msg, "Removed GitHub's saved credentials") {
		t.Fatalf("disconnect: %q %v", msg, err)
	}
	b, _ := os.ReadFile(w.envPath)
	if strings.Contains(string(b), "ghp_secret_token_1234") {
		t.Fatalf("the token is still in factory.env:\n%s", b)
	}
	st := w.State(ctx, false)
	if st.GitHub.Method != "none" || st.Access[0].State != "unchecked" || step(st, "github").State != "attention" {
		t.Fatalf("after disconnect: %+v %+v", st.GitHub, st.Access)
	}
	// One exported where the factory starts still wins: the message says so.
	w.exported = map[string]string{"GITHUB_TOKEN": "ghp_exported"}
	if msg, _ := w.Do(ctx, "github/disconnect", web.WizardInput{}); !strings.Contains(msg, "GITHUB_TOKEN is exported") {
		t.Fatalf("disconnect with an exported token: %q", msg)
	}
}

// A token that reads the repo but can't open pull requests fails the
// access check, saying what's missing, rather than every job at its end.
func TestWizardAccessNeedsPullRequests(t *testing.T) {
	f := &fakeGitHub{labels: map[string]bool{}, noPulls: true}
	w := testWizard(t, f, "o/r", true, noDocker)
	ctx := context.Background()
	w.Do(ctx, "github/token", web.WizardInput{Token: "ghp_secret_token_1234"})
	a := w.State(ctx, false).Access[0]
	if a.State != "failed" || !strings.Contains(a.Error, "can't open pull requests") || !slices.ContainsFunc(a.Hints, func(h string) bool { return strings.Contains(h, "not accessible") }) {
		t.Fatalf("access = %+v", a)
	}
}
