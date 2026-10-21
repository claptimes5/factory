package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"github.com/claptimes5/factory/internal/readiness"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	if err := setYAML(path, "base_branch", "@odd"); err != nil {
		t.Fatalf("a value YAML needs quoted: %v", err)
	}
	if cfg, err := config.Load(path); err != nil || cfg.BaseBranch != "@odd" {
		t.Fatalf("base_branch = %v, %v", cfg, err)
	}
}

func TestSetupStopsWhenInputEnds(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "ghp_from_gh")
	srv := httptest.NewServer(&fakeGitHub{})
	t.Cleanup(srv.Close)
	oldAPI, oldRun := githubAPI, runCmd
	githubAPI = srv.URL
	runCmd = func(io.Writer, string, ...string) error { return errors.New("no docker") }
	t.Cleanup(func() { githubAPI, runCmd = oldAPI, oldRun })
	cfgPath := testConfig(t, "o/r")
	cfg, _ := config.Load(cfgPath)
	var b strings.Builder
	// Repos kept, Docker not checked again: input ends at GitHub.
	p := &prompter{in: bufio.NewReader(strings.NewReader("\nn\n")), out: &b}
	err := setup(context.Background(), cfgPath, cfg, filepath.Join(t.TempDir(), "factory.env"), p)
	if !errors.Is(err, errInputClosed) {
		t.Fatalf("err = %v, want errInputClosed", err)
	}
	if !strings.Contains(b.String(), "3. Use a personal access token") {
		t.Fatalf("didn't reach GitHub's question:\n%s", b.String())
	}
	if strings.Contains(b.String(), "[3]") {
		t.Fatal("an exported GITHUB_TOKEN must not become the default")
	}
}

// An exported GITHUB_TOKEN is never the default nor written to
// factory.env, and a saved key an export overrides stays saved, with its
// name (never a value) said.
func TestSetupKeepsSavedValuesOverExports(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "ghp_from_gh")
	old := startEnv
	startEnv = append(slices.Clone(old), "GITHUB_TOKEN=ghp_from_gh", "ANTHROPIC_API_KEY=sk-ant-api03-exported")
	t.Cleanup(func() { startEnv = old })
	envPath := filepath.Join(t.TempDir(), "factory.env")
	if err := os.WriteFile(envPath, []byte("ANTHROPIC_API_KEY=sk-ant-api03-saved\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(&fakeGitHub{})
	t.Cleanup(srv.Close)
	oldAPI, oldRun := githubAPI, runCmd
	githubAPI = srv.URL
	runCmd = func(io.Writer, string, ...string) error { return errors.New("no docker") }
	t.Cleanup(func() { githubAPI, runCmd = oldAPI, oldRun })
	cfgPath := testConfig(t, "o/r")
	cfg, _ := config.Load(cfgPath)
	var b strings.Builder
	p := &prompter{in: bufio.NewReader(strings.NewReader("\nn\n")), out: &b}
	if err := setup(context.Background(), cfgPath, cfg, envPath, p); !errors.Is(err, errInputClosed) {
		t.Fatalf("err = %v", err)
	}
	out := b.String()
	if !strings.Contains(out, "ANTHROPIC_API_KEY is set in the environment the factory starts with") || strings.Contains(out, "exported\n") || strings.Contains(out, "sk-ant-api03-exported") {
		t.Fatalf("output:\n%s", out)
	}
	if strings.Contains(out, "GITHUB_TOKEN is set") {
		t.Fatal("warned about a GITHUB_TOKEN that isn't saved")
	}
	vals, err := readEnvFile(envPath)
	if err != nil {
		t.Fatal(err)
	}
	if vals["GITHUB_TOKEN"] != "" || vals["ANTHROPIC_API_KEY"] != "sk-ant-api03-saved" {
		t.Fatalf("saved %v", vals)
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
	files    map[string]map[string]string // repo -> path -> contents
	runCmd   func(io.Writer, string, ...string) error
	report   *readiness.Report // what the readiness check finds; ready if nil
	issues   []string          // path, title and labels of issues created
	bodies   []string
	filed    []map[string]any    // issues the check filed before, if set
	labeled  []string            // path and labels added to issues
	yaml     func(string) string // edits factory.yaml before setup reads it
	gone     bool                // the app was uninstalled
	slow     string              // a repo whose requests never get an answer
	denied   string              // a repo whose requests get a 401
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if f.slow != "" && (r.URL.Path == "/repos/"+f.slow || strings.HasPrefix(r.URL.Path, "/repos/"+f.slow+"/")) {
		<-r.Context().Done()
		return
	}
	if f.denied != "" && (r.URL.Path == "/repos/"+f.denied || strings.HasPrefix(r.URL.Path, "/repos/"+f.denied+"/")) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"message":"Bad credentials"}`))
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.auth = append(f.auth, r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization"))
	if repo, path, ok := strings.Cut(strings.TrimPrefix(r.URL.Path, "/repos/"), "/contents/"); ok && f.files[repo] != nil {
		fakeContents(w, f.files[repo], path)
		return
	}
	if strings.HasSuffix(r.URL.Path, "/issues") && r.Method == "POST" {
		var body struct {
			Title, Body string
			Labels      []string
		}
		json.NewDecoder(r.Body).Decode(&body)
		f.issues = append(f.issues, r.URL.Path+" "+body.Title+" "+strings.Join(body.Labels, ","))
		f.bodies = append(f.bodies, body.Body)
		json.NewEncoder(w).Encode(map[string]int{"number": 100 + len(f.issues)})
		return
	}
	if strings.Contains(r.URL.Path, "/issues/") && strings.HasSuffix(r.URL.Path, "/labels") && r.Method == "POST" {
		var body struct{ Labels []string }
		json.NewDecoder(r.Body).Decode(&body)
		f.labeled = append(f.labeled, r.URL.Path+" "+strings.Join(body.Labels, ","))
		w.Write([]byte("[]"))
		return
	}
	if strings.HasSuffix(r.URL.Path, "/issues") && r.URL.Query().Get("labels") == "factory:readiness" && r.URL.Query().Get("state") == "all" && f.filed != nil {
		json.NewEncoder(w).Encode(f.filed)
		return
	}
	if strings.HasSuffix(r.URL.Path, "/issues") && r.URL.Query().Get("labels") == "factory:readiness" && r.URL.Query().Get("state") == "all" {
		// The check filed before: one still open (from before issues had
		// keys), one done, one declined.
		json.NewEncoder(w).Encode([]map[string]any{
			{"number": 7, "title": "Start a feature map", "state": "open"},
			{"number": 8, "title": "Add e2e tests", "body": "x\n<!-- factory-readiness: e2e -->", "state": "closed", "state_reason": "completed"},
			{"number": 9, "title": "Lockfile", "body": "<!-- factory-readiness: lockfile -->", "state": "closed", "state_reason": "not_planned"},
		})
		return
	}
	// Every repo under o looks like o/r.
	r.URL.Path = anyRepo.ReplaceAllString(r.URL.Path, "/repos/o/r")
	r.URL.RawPath = anyRepo.ReplaceAllString(r.URL.RawPath, "/repos/o/r")
	j := func(v any) { json.NewEncoder(w).Encode(v) }
	switch {
	case r.URL.Path == "/users/o":
		j(map[string]string{"type": "User"})
	case r.URL.Path == "/app-manifests/CODE/conversions" && r.Method == "POST":
		der := x509.MarshalPKCS1PrivateKey(f.key)
		j(map[string]any{"id": 42, "slug": "factory-r-ab12", "html_url": "https://github.com/apps/factory-r-ab12", "owner": map[string]string{"login": "o"},
			"pem": string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: der}))})
	case f.gone && (r.URL.Path == "/app/installations" || r.URL.Path == "/repos/o/r/installation"):
		if r.URL.Path == "/app/installations" {
			j([]any{})
		} else {
			w.WriteHeader(404)
		}
	case r.URL.Path == "/app/installations":
		j([]map[string]any{{"id": 77, "account": map[string]string{"login": "someone-else"}}, {"id": 55, "account": map[string]string{"login": "O"}}})
	case r.URL.Path == "/repos/o/r/installation":
		j(map[string]any{"id": 55})
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
	case r.URL.Path == "/repos/o/r/labels" && r.Method == "GET":
		var names []map[string]string
		for n, ok := range f.labels {
			if ok {
				names = append(names, map[string]string{"name": n})
			}
		}
		for _, n := range f.created {
			names = append(names, map[string]string{"name": n})
		}
		j(names)
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

var anyRepo = regexp.MustCompile(`^/repos/o/[^/]+`)

func TestSetupWithSeveralRepos(t *testing.T) {
	f := &fakeGitHub{labels: map[string]bool{}}
	cfgPath, _, out := runSetup(t, f,
		"o/api, https://github.com/o/web.git",
		"3", "ghp_token123",
		"y", // create labels on both
		"",  // run the readiness check
		"sk-ant-api03-abc",
		"", "", // Codex on an API key, but none
		"",
	)
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(cfg.RepoNames(), ","); got != "o/api,o/web" {
		t.Fatalf("repos = %s", got)
	}
	if len(f.created) != 2*len(pipeline.Labels(cfg.Intake.Label)) {
		t.Fatalf("labels created: %v", f.created)
	}
	if !strings.Contains(out, "OK: the factory can reach o/web.") {
		t.Fatalf("o/web not checked:\n%s", out)
	}
}

func TestSetReposTwice(t *testing.T) {
	path := testConfig(t, "o/r")
	for _, want := range [][]string{{"o/a", "o/b"}, {"o/b"}, {"o/a", "o/c", "o/b"}, {"o/c", "o/a"}} {
		if err := setRepos(path, want); err != nil {
			t.Fatalf("%v: %v", want, err)
		}
		cfg, err := config.Load(path)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(cfg.RepoNames(), want) {
			t.Fatalf("repos = %v, want %v", cfg.RepoNames(), want)
		}
		if cfg.Gates.DiffGuard.MaxChangedLines == 0 || cfg.Intake.Label == "" {
			t.Fatalf("lost the rest of the file: %+v", cfg)
		}
	}
	// A repo's own settings survive a change to the list.
	b, _ := os.ReadFile(path)
	os.WriteFile(path, []byte(strings.Replace(string(b), "  - o/a\n", "  - name: o/a\n    base_branch: dev\n    gates: {install: npm ci}\n", 1)), 0o644)
	if err := setRepos(path, []string{"o/a", "o/d"}); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if a, _ := cfg.RepoConf("o/a"); a.BaseBranch != "dev" || a.Gates.Install != "npm ci" {
		t.Fatalf("o/a lost its settings: %+v", a)
	}
	if err := setRepos(path, []string{"o/d"}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); !strings.Contains(string(b), "\nrepo: o/d\n") {
		t.Fatalf("one bare repo should use the short form:\n%s", b)
	}
}

func fakeContents(w http.ResponseWriter, files map[string]string, path string) {
	if c, ok := files[path]; ok {
		json.NewEncoder(w).Encode(map[string]string{"encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte(c))})
		return
	}
	var list []map[string]string
	seen := map[string]bool{}
	for p := range files {
		rest, ok := strings.CutPrefix(p, path)
		if !ok || (path != "" && !strings.HasPrefix(rest, "/") && path[len(path)-1] != '/') {
			continue
		}
		rest = strings.TrimPrefix(rest, "/")
		name, _, dir := strings.Cut(rest, "/")
		if seen[name] {
			continue
		}
		seen[name] = true
		typ := "file"
		if dir {
			typ = "dir"
		}
		list = append(list, map[string]string{"name": name, "type": typ})
	}
	if len(list) == 0 {
		w.WriteHeader(404)
		return
	}
	json.NewEncoder(w).Encode(list)
}

// Setup suggests checks and an image per repo from its files, and writes
// the ones you accept.
func TestSetupSuggestsChecks(t *testing.T) {
	f := &fakeGitHub{labels: map[string]bool{}, files: map[string]map[string]string{
		"o/api": {"Gemfile": "gem \"rails\"\ngem \"rspec-rails\"\n", ".ruby-version": "3.3.6"},
		"o/web": {"package.json": `{"scripts":{"build":"vite build","test":"vitest"},"devDependencies":{"vite":"^8"}}`, "package-lock.json": "{}"},
	}}
	var built []string
	f.runCmd = func(_ io.Writer, name string, args ...string) error {
		cmd := strings.Join(args, " ")
		switch {
		case strings.HasPrefix(cmd, "image inspect factory-job-o__api"):
			return errors.New("no such image")
		case strings.HasPrefix(cmd, "build"):
			built = append(built, cmd)
		}
		return nil
	}
	cfgPath, _, out := runSetup(t, f,
		"o/api, o/web",
		"3", "ghp_token123",
		"n", // no labels
		"y", // o/api's checks
		"y", // o/web's checks
		"y", // build the images
		"",  // run the readiness check
		"sk-ant-api03-abc",
		"", "", // Codex on an API key, but none
		"",
	)
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	api, _ := cfg.RepoConf("o/api")
	if api.Image != "factory-job-o__api:latest" || api.ImageFrom != "ruby:3.3.6-bookworm" || api.Gates.Install != "bundle install" || api.Gates.Commands["test"] != "bundle exec rspec" || api.Gates.ProvesFix != "" {
		t.Fatalf("o/api = %+v %+v\n%s", api, api.Gates, out)
	}
	web, _ := cfg.RepoConf("o/web")
	if web.Image != cfg.Runner.Image || strings.Join(web.Gates.Required, ",") != "test,build" || web.Gates.DiffGuard.MaxChangedLines == 0 {
		t.Fatalf("o/web = %+v %+v", web, web.Gates)
	}
	if len(built) != 1 || !strings.HasPrefix(built[0], "build -t factory-job-o__api:latest --label io.factory.built=1 --build-arg BASE=ruby:3.3.6-bookworm ") {
		t.Fatalf("built %q", built)
	}
	if !strings.Contains(out, "o/api looks like Ruby on Rails") || !strings.Contains(out, "Jobs for o/api run in the image factory-job-o__api:latest") {
		t.Fatalf("output:\n%s", out)
	}
}

// A repos list with blank lines and comments inside it is replaced whole.
func TestWriteReposSkipsBlankLines(t *testing.T) {
	path := testConfig(t, "o/r")
	b, _ := os.ReadFile(path)
	y := strings.Replace(string(b), "repo: o/r", "repos:\n  - name: o/a\n    base_branch: dev\n\n# the frontend\n  - o/b\n\n", 1)
	os.WriteFile(path, []byte(y), 0o644)
	if err := setRepos(path, []string{"o/a"}); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.RepoNames(); !slices.Equal(got, []string{"o/a"}) {
		t.Fatalf("repos = %v", got)
	}
	if b, _ := os.ReadFile(path); !strings.Contains(string(b), "\nbase_branch: main") {
		t.Fatalf("lost what follows the list:\n%s", b)
	}
}

// Setup shows each repo's readiness and opens the issues that close its
// gaps, skipping ones still open from an earlier run.
func TestSetupFilesReadinessIssues(t *testing.T) {
	f := &fakeGitHub{labels: map[string]bool{}, report: &readiness.Report{Branch: "main", SHA: "abc1234", Score: 59,
		Findings: []readiness.Finding{
			{Level: readiness.Blocker, Title: "`test` fails on main", Issue: &readiness.Issue{Key: "check:test", Title: "Make the test check pass on main", Body: "fix it"}},
			{Level: readiness.Gap, Title: "No feature map", Issue: &readiness.Issue{Key: "feature-map", Title: "Start a feature map", Body: "map it"}},
			{Level: readiness.Gap, Title: "No end-to-end tests", Issue: &readiness.Issue{Key: "e2e", Title: "Add end-to-end smoke tests", Body: "e2e"}},
			{Level: readiness.Gap, Title: "No lockfile", Issue: &readiness.Issue{Key: "lockfile", Title: "Commit a dependency lockfile", Body: "lock"}},
		}}}
	_, _, out := runSetup(t, f,
		"o/r",
		"3", "ghp_token123",
		"n", // no labels
		"",  // run the readiness check
		"y", // open the issues
		"y", // and queue them
		"sk-ant-api03-abc",
		"", "", // Codex on an API key, but none
		"",
	)
	// While the repo has blockers, only they are queued.
	want := []string{"/repos/o/r/issues Make the test check pass on main factory:readiness,factory:ready", "/repos/o/r/issues Add end-to-end smoke tests factory:readiness"}
	if strings.Join(f.issues, "|") != strings.Join(want, "|") {
		t.Fatalf("issues: %v\n%s", f.issues, out)
	}
	if !strings.HasSuffix(f.bodies[0], "<!-- factory-readiness: check:test -->") {
		t.Fatalf("body: %s", f.bodies[0])
	}
	for _, want := range []string{"o/r at main (abc1234): 59/100, not ready yet", "✗ `test` fails on main", "Every job would fail until the blockers are fixed",
		"Open these 2 issues on o/r?", "(2 more already filed.)", "Opened 2 issue(s) on o/r."} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q:\n%s", want, out)
		}
	}
}

func TestRemoteIs(t *testing.T) {
	for url, want := range map[string]bool{
		"https://github.com/o/Web.git": true,
		"git@github.com:o/web.git":     true,
		"ssh://git@github.com/o/web":   true,
		"https://github.com/o/api.git": false,
		"https://github.com/xo/web":    false,
	} {
		if remoteIs(url, "o/web") != want {
			t.Errorf("remoteIs(%s) = %v", url, !want)
		}
	}
}

func TestRemoteIsGitLab(t *testing.T) {
	for url, want := range map[string]bool{
		"https://gitlab.com/acme/web.git":        true,
		"git@gitlab.com:acme/web.git":            true,
		"ssh://git@gitlab.com/acme/web":          true,
		"https://oauth2@gitlab.com/acme/web":     true,
		"ssh://git@gitlab.com:2222/acme/web.git": true,
		"https://gitlab.com:8443/acme/web":       true,
		"ssh://git@gitlab.com:2222/x/acme/web":   false,
		"git@gitlab.example.com:acme/web.git":    false,
		"https://gitlab.com/other/acme/web.git":  false,
		"https://gitlab.com/acme/web-old.git":    false,
	} {
		if remoteIs(url, "gitlab.com/acme/web") != want {
			t.Errorf("remoteIs(%s) = %v", url, !want)
		}
	}
}

func TestParseIssue(t *testing.T) {
	one, _ := config.Load(testConfig(t, "o/r"))
	if repo, n, err := parseIssue(one, "#12"); err != nil || repo != "o/r" || n != 12 {
		t.Fatalf("one repo: %s %d %v", repo, n, err)
	}
	two := *one
	two.Repos = []config.Repo{{Name: "o/r"}, {Name: "o/Web"}}
	if _, _, err := parseIssue(&two, "12"); err == nil {
		t.Fatal("a bare number is ambiguous with several repos")
	}
	if repo, n, err := parseIssue(&two, "o/web#7"); err != nil || repo != "o/Web" || n != 7 {
		t.Fatalf("named: %s %d %v", repo, n, err)
	}
	if _, _, err := parseIssue(&two, "o/web#x"); err == nil {
		t.Fatal("want an error for a bad number")
	}
}

func testConfig(t *testing.T, repo string) string {
	t.Helper()
	b, err := os.ReadFile("../../factory.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	y := regexp.MustCompile(`(?m)^(# )?repo: .*$`).ReplaceAllString(string(b), "repo: "+repo)
	dir := t.TempDir()
	path := filepath.Join(dir, "factory.yaml")
	os.WriteFile(path, []byte(y), 0o644)
	os.MkdirAll(filepath.Join(dir, "deploy", "job-image"), 0o755)
	os.WriteFile(filepath.Join(dir, "deploy", "job-image", "Dockerfile"), []byte("FROM scratch\n"), 0o644)
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
	if f.runCmd != nil {
		runCmd = f.runCmd
	}
	oldAssess := assess
	assess = func(_ context.Context, _ *config.Config, _ *pipeline.Pipeline, repo string, _ io.Writer) (*readiness.Report, error) {
		if f.report != nil {
			r := *f.report
			r.Repo = repo
			return &r, nil
		}
		return &readiness.Report{Repo: repo, Branch: "main", Score: 100, Ready: true}, nil
	}
	t.Cleanup(func() { assess = oldAssess })
	t.Cleanup(func() { githubAPI, githubWeb, runCmd = oldAPI, oldWeb, oldRun })
	for _, k := range envKeys {
		t.Setenv(k, "")
	}
	cfgPath = testConfig(t, "x/y")
	if f.yaml != nil {
		b, _ := os.ReadFile(cfgPath)
		os.WriteFile(cfgPath, []byte(f.yaml(string(b))), 0o644)
	}
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
		"",               // run the readiness check
		"sk-ant-api03-abc",
		"", // Codex on an API key
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
	if perms["contents"] != "write" || perms["checks"] != "read" || perms["actions"] != "read" || perms["workflows"] != nil {
		t.Fatalf("manifest permissions: %v", f.manifest)
	}
	if len(f.created) != len(pipeline.Labels("factory:ready"))-1 {
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
		"n",                         // skip the readiness check
		"sk-ant-oat01-subscription", // a subscription token goes in its own variable
		"", "",                      // an API key for Codex, but none
		"https://example.com/hook", // not a supported notifier
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

// With every route on Codex, setup doesn't ask for a Claude credential.
func TestSetupCodexOnlySkipsClaude(t *testing.T) {
	f := &fakeGitHub{labels: map[string]bool{}, yaml: func(y string) string {
		y = regexp.MustCompile(`harness: claude, model: [a-z]+`).ReplaceAllString(y, "harness: codex, model: gpt-6.1-sol")
		y = strings.Replace(y, "review:            {harness: codex,  model: gpt-6.1-sol}", "review: {harness: codex, model: gpt-6-luna}", 1)
		y = strings.Replace(y, "review.high_risk:  {harness: codex,  model: gpt-6.1-sol, effort: high}", "", 1)
		y = strings.Replace(y, "verify:            {harness: codex,  model: gpt-6-luna}", "", 1)
		y = strings.Replace(y, "security:          {harness: codex, model: gpt-6.1-sol}", "", 1)
		return strings.Replace(y, "implement: [haiku, sonnet, opus]", "implement: [gpt-6.1-sol]", 1)
	}}
	_, envPath, out := runSetup(t, f,
		"o/r",
		"3", "ghp_token123", // personal token
		"n",           // don't create labels
		"n",           // skip the readiness check
		"1", "sk-xyz", // Codex on an API key
		"", // no notifications
	)
	if strings.Contains(out, "Anthropic") || strings.Contains(out, "== 7. Claude") {
		t.Fatalf("setup asked for Claude:\n%s", out)
	}
	if !strings.Contains(out, "runs every job on Codex") {
		t.Fatalf("no Codex-only wording:\n%s", out)
	}
	if vals, _ := readEnvFile(envPath); vals["OPENAI_API_KEY"] != "sk-xyz" {
		t.Fatalf("codex key: %v\n%s", vals, out)
	}
}

// A repo that takes screenshots gets Chromium in its image.
func TestImagesForPreviews(t *testing.T) {
	path := testConfig(t, "o/r")
	b, _ := os.ReadFile(path)
	y := strings.Replace(string(b), "repo: o/r", "repos:\n  - name: o/web\n    preview: {start: npm start}\n  - name: o/api\n    image_from: ruby:3.4-bookworm\n", 1)
	cfg, err := config.Parse([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	ims := jobImages(cfg)
	if len(ims) != 2 || !strings.Contains(strings.Join(ims[0].buildArgs("d"), " "), "--build-arg PLAYWRIGHT=1") || strings.Contains(strings.Join(ims[1].buildArgs("d"), " "), "PLAYWRIGHT") {
		t.Fatalf("images %+v", ims)
	}
}

func TestGitLabTokenHelpShowsOnlyTheChosenKind(t *testing.T) {
	for _, c := range []struct{ answer, want, not string }{
		{"\n", "scopes=api,write_repository", "Merge Request: Read"},
		{"2\n", "Merge Request: Read, Create, Update, Merge", "scopes=api"},
		{"3\n2\n", "Type 1 or 2.", "scopes=api"},
	} {
		var b strings.Builder
		p := &prompter{in: bufio.NewReader(strings.NewReader(c.answer)), out: &b}
		gitlabTokenHelp(p, []string{"gitlab.example.com"})
		if out := b.String(); !strings.Contains(out, c.want) || strings.Contains(out, c.not) {
			t.Errorf("answer %q:\n%s", c.answer, out)
		}
	}
}

// The orchestrator, tuning and grooming run in runner.image, so it's built
// even when every repo has an image of its own, while any of them is on.
func TestImagesIncludeTheOrchestrators(t *testing.T) {
	path := testConfig(t, "o/r")
	b, _ := os.ReadFile(path)
	y := strings.Replace(string(b), "repo: o/r", "repos:\n  - name: o/api\n    image_from: ruby:3.4-bookworm\n", 1)
	cfg, err := config.Parse([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Orchestrator.On, cfg.Tuning.Every, cfg.Grooming.Every = false, 0, 0
	if ims := jobImages(cfg); len(ims) != 1 {
		t.Fatalf("with nothing outside a repo on: images %+v", ims)
	}
	cfg.Orchestrator.On = true
	ims := jobImages(cfg)
	if len(ims) != 2 || ims[1].tag != cfg.Runner.Image || joinAnd(ims[1].repos) != runnerImageUsers {
		t.Fatalf("images %+v", ims)
	}
}
