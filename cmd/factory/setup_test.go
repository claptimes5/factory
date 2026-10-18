package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/claptimes5/factory/internal/config"
)

func TestEnvFileRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "factory.env")
	vals := map[string]string{"A": "plain", "B": `has "quotes" and \ and # and spaces`, "C": ""}
	if err := writeEnvFile(path, "# hi\n", []string{"A", "B", "C"}, vals); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(path)
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", st.Mode().Perm())
	}
	got, err := readEnvFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got["A"] != "plain" || got["B"] != vals["B"] {
		t.Fatalf("round trip: %q", got)
	}
	if _, ok := got["C"]; ok {
		t.Fatal("empty values are left out")
	}
	if err := writeEnvFile(path, "", []string{"A"}, map[string]string{"A": "x\ny"}); err == nil {
		t.Fatal("a value with a line break must be refused")
	}
}

func TestLoadEnvFileKeepsExplicitEnv(t *testing.T) {
	path := filepath.Join(t.TempDir(), "factory.env")
	os.WriteFile(path, []byte("FACTORY_T_ONE=file\nFACTORY_T_TWO=file\n"), 0o600)
	t.Setenv("FACTORY_T_ONE", "exported")
	os.Unsetenv("FACTORY_T_TWO")
	t.Cleanup(func() { os.Unsetenv("FACTORY_T_TWO") })
	if err := loadEnvFile(path); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("FACTORY_T_ONE") != "exported" || os.Getenv("FACTORY_T_TWO") != "file" {
		t.Fatalf("got %q %q", os.Getenv("FACTORY_T_ONE"), os.Getenv("FACTORY_T_TWO"))
	}
	if err := loadEnvFile(filepath.Join(t.TempDir(), "missing")); err != nil {
		t.Fatalf("a missing file is fine: %v", err)
	}
}

func TestSetYAMLKeepsComments(t *testing.T) {
	path := testConfig(t, "x/y")
	if err := setYAML(path, "repo", "octo/hello"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), "repo: octo/hello\n") || !strings.Contains(string(b), "# Factory configuration.") {
		t.Fatalf("rewrite lost something:\n%.300s", b)
	}
	if err := setYAML(path, "repo", "not a repo: [x"); err == nil {
		t.Fatal("a value that breaks the config must be refused")
	}
}

// fakeGitHub serves the endpoints setup uses, for repo o/r.
type fakeGitHub struct {
	mu       sync.Mutex
	key      *rsa.PrivateKey
	labels   map[string]bool
	created  []string
	manifest map[string]any
	auth     []string
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.auth = append(f.auth, r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization"))
	j := func(v any) { json.NewEncoder(w).Encode(v) }
	switch {
	case r.URL.Path == "/users/o":
		j(map[string]string{"type": "User"})
	case r.URL.Path == "/app-manifests/CODE/conversions" && r.Method == "POST":
		der := x509.MarshalPKCS1PrivateKey(f.key)
		j(map[string]any{"id": 42, "slug": "factory-r-ab12", "html_url": "https://github.com/apps/factory-r-ab12",
			"pem": string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: der}))})
	case r.URL.Path == "/app/installations":
		j([]map[string]any{{"id": 77, "account": map[string]string{"login": "someone-else"}}, {"id": 55, "account": map[string]string{"login": "O"}}})
	case r.URL.Path == "/app/installations/55/access_tokens":
		j(map[string]any{"token": "ghs_inst", "expires_at": time.Now().Add(time.Hour)})
	case r.URL.Path == "/repos/o/r/branches/main":
		j(map[string]any{"commit": map[string]string{"sha": "abc1234567"}})
	case r.URL.Path == "/repos/o/r":
		j(map[string]string{"default_branch": "main"})
	case strings.HasPrefix(r.URL.Path, "/repos/o/r/labels/") && r.Method == "GET":
		name, _ := url.PathUnescape(strings.TrimPrefix(r.URL.EscapedPath(), "/repos/o/r/labels/"))
		if !f.labels[name] {
			w.WriteHeader(404)
			return
		}
		j(map[string]string{"name": name})
	case r.URL.Path == "/repos/o/r/labels" && r.Method == "POST":
		var l struct{ Name string }
		json.NewDecoder(r.Body).Decode(&l)
		f.created = append(f.created, l.Name)
		w.WriteHeader(201)
		j(l)
	default:
		w.WriteHeader(404)
	}
}

func testConfig(t *testing.T, repo string) string {
	t.Helper()
	b, err := os.ReadFile("../../factory.yaml")
	if err != nil {
		t.Fatal(err)
	}
	y := regexp.MustCompile(`(?m)^repo: .*$`).ReplaceAllString(string(b), "repo: "+repo)
	path := filepath.Join(t.TempDir(), "factory.yaml")
	os.WriteFile(path, []byte(y), 0o644)
	return path
}

// runSetup drives setup with scripted answers against a fake GitHub.
func runSetup(t *testing.T, f *fakeGitHub, answers ...string) (cfgPath, envPath, out string) {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	oldAPI, oldWeb, oldRun := githubAPI, githubWeb, runCmd
	githubAPI, githubWeb = srv.URL, "https://github.example"
	runCmd = func(io.Writer, string, ...string) error { return nil } // Docker works, image built
	t.Cleanup(func() { githubAPI, githubWeb, runCmd = oldAPI, oldWeb, oldRun })
	for _, k := range envKeys {
		t.Setenv(k, "")
	}
	cfgPath = testConfig(t, "x/y")
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	envPath = filepath.Join(t.TempDir(), "factory.env")
	var b strings.Builder
	p := &prompter{in: bufio.NewReader(strings.NewReader(strings.Join(answers, "\n") + "\n")), out: &b}
	if err := setup(context.Background(), cfgPath, cfg, envPath, p); err != nil {
		t.Fatalf("setup: %v\n%s", err, b.String())
	}
	return cfgPath, envPath, b.String()
}

func TestSetupCreatesTheAppAndSavesEverything(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	f := &fakeGitHub{key: key, labels: map[string]bool{"factory:ready": true}}
	browse = func(base string) {
		// What the browser does: load the form, post it to GitHub, come back.
		resp, err := http.Get(base + "/")
		if err != nil {
			t.Error(err)
			return
		}
		page, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		m := regexp.MustCompile(`action="([^"]+)"[\s\S]*value="([^"]+)"`).FindStringSubmatch(string(page))
		if m == nil {
			t.Errorf("no manifest form in:\n%s", page)
			return
		}
		action, _ := url.Parse(strings.ReplaceAll(m[1], "&amp;", "&"))
		if action.Path != "/settings/apps/new" {
			t.Errorf("form posts to %s", action)
		}
		json.Unmarshal([]byte(strings.NewReplacer("&#34;", `"`, "&quot;", `"`).Replace(m[2])), &f.manifest)
		resp, err = http.Get(base + "/callback?code=CODE&state=" + action.Query().Get("state"))
		if err == nil {
			resp.Body.Close()
		}
	}
	t.Cleanup(func() { browse = nil })

	cfgPath, envPath, out := runSetup(t, f,
		"github.com/o/r", // 1. repo, as pasted from the browser
		"1",              // 3. create a new app
		"",               // the browser said it was created
		"y",              // create labels
		"sk-ant-api03-abc",
		"sk-openai-xyz",
		"", // no notifications
	)
	vals, _ := readEnvFile(envPath)
	if vals["FACTORY_GITHUB_APP_ID"] != "42" || vals["FACTORY_GITHUB_INSTALLATION_ID"] != "55" {
		t.Fatalf("app settings: %v\n%s", vals, out)
	}
	if _, err := os.Stat(vals["FACTORY_GITHUB_APP_KEY"]); err != nil {
		t.Fatalf("key not saved: %v", err)
	}
	if st, _ := os.Stat(vals["FACTORY_GITHUB_APP_KEY"]); st.Mode().Perm() != 0o600 {
		t.Fatalf("key mode %v", st.Mode().Perm())
	}
	if vals["ANTHROPIC_API_KEY"] != "sk-ant-api03-abc" || vals["OPENAI_API_KEY"] != "sk-openai-xyz" {
		t.Fatalf("model keys: %v", vals)
	}
	perms, _ := f.manifest["default_permissions"].(map[string]any)
	if perms["contents"] != "write" || perms["checks"] != "read" || perms["workflows"] != nil {
		t.Fatalf("manifest permissions: %v", f.manifest)
	}
	if len(f.created) != 5 {
		t.Fatalf("created labels %v; factory:ready already existed", f.created)
	}
	b, _ := os.ReadFile(cfgPath)
	if !strings.Contains(string(b), "repo: o/r\n") {
		t.Fatal("repo not written to factory.yaml")
	}
	if !strings.Contains(out, "./factory serve") {
		t.Fatalf("no next step:\n%s", out)
	}
}

func TestSetupWithTokenSortsClaudeCredentials(t *testing.T) {
	f := &fakeGitHub{labels: map[string]bool{}}
	_, envPath, out := runSetup(t, f,
		"o/r",
		"3", "ghp_token123", // personal token
		"n",                         // don't create labels
		"sk-ant-oat01-subscription", // a subscription token goes in its own variable
		"",                          // no OpenAI key
		"https://example.com/hook",  // not a supported notifier
		"https://ntfy.sh/my-topic",
	)
	vals, _ := readEnvFile(envPath)
	if vals["GITHUB_TOKEN"] != "ghp_token123" || vals["FACTORY_GITHUB_APP_ID"] != "" {
		t.Fatalf("github: %v", vals)
	}
	if vals["CLAUDE_CODE_OAUTH_TOKEN"] != "sk-ant-oat01-subscription" || vals["ANTHROPIC_API_KEY"] != "" {
		t.Fatalf("claude: %v", vals)
	}
	if vals["FACTORY_NOTIFY_URL"] != "https://ntfy.sh/my-topic" {
		t.Fatalf("notify: %v\n%s", vals, out)
	}
	if !strings.Contains(out, "reviews will fail") {
		t.Fatalf("no warning about the missing OpenAI key:\n%s", out)
	}
	if len(f.created) != 0 {
		t.Fatal("labels created without permission")
	}
}
