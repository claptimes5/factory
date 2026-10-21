package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/claptimes5/factory/internal/config"
	"github.com/claptimes5/factory/internal/pipeline"
)

// fakeGitLab is a GitLab API with one project, team/web.
type fakeGitLab struct {
	mu     sync.Mutex
	tokens []string
	labels []string
}

func (f *fakeGitLab) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokens = append(f.tokens, r.Header.Get("Authorization"))
	if r.Header.Get("Authorization") != "Bearer glpat-abc" {
		w.WriteHeader(401)
		return
	}
	const p = "/projects/team%2Fweb"
	j := func(v any) { json.NewEncoder(w).Encode(v) }
	switch path := r.URL.EscapedPath(); {
	case path == p+"/repository/branches/main":
		j(map[string]any{"commit": map[string]string{"id": "abc1234567"}})
	case path == p:
		j(map[string]string{"default_branch": "main"})
	case path == p+"/repository/tree":
		j([]any{})
	case strings.HasPrefix(path, p+"/labels/"):
		w.WriteHeader(404)
	case path == p+"/labels" && r.Method == "POST":
		var l struct{ Name string }
		json.NewDecoder(r.Body).Decode(&l)
		f.labels = append(f.labels, l.Name)
		w.WriteHeader(201)
		j(l)
	default:
		w.WriteHeader(404)
	}
}

func TestSetupWithGitLab(t *testing.T) {
	gl := &fakeGitLab{}
	srv := httptest.NewServer(gl)
	t.Cleanup(srv.Close)
	old := gitlabAPI
	gitlabAPI = srv.URL
	t.Cleanup(func() { gitlabAPI = old })

	f := &fakeGitHub{labels: map[string]bool{}}
	cfgPath, envPath, out := runSetup(t, f,
		"o/api, https://gitlab.com/team/web.git",
		"3", "ghp_token123", // GitHub
		"", "glpat-wrong", "y", "glpat-abc", // GitLab: a legacy token, a bad one then a good one
		"y", // create labels on both
		"",  // run the readiness check
		"",  // Claude, Codex or both: both
		"sk-ant-api03-abc",
		"", "", // Codex on an API key, but none
		"",
	)
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(cfg.RepoNames(), ","); got != "o/api,gitlab.com/team/web" {
		t.Fatalf("repos = %s", got)
	}
	for _, want := range []string{"== 3b. GitLab access", "GitLab didn't accept the token", "OK: the factory can reach gitlab.com/team/web.", "OK: the factory can reach o/api."} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q:\n%s", want, out)
		}
	}
	if len(gl.labels) != len(pipeline.Labels(cfg.Intake.Label)) || len(f.created) != len(gl.labels) {
		t.Fatalf("labels: gitlab %v, github %v", gl.labels, f.created)
	}
	vals, _ := readEnvFile(envPath)
	if vals["GITLAB_TOKEN"] != "glpat-abc" || vals["GITHUB_TOKEN"] != "ghp_token123" {
		t.Fatalf("saved %v", vals)
	}
}

func TestRepoGitForGitLab(t *testing.T) {
	cfg, err := config.Load(testConfig(t, "o/r"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Repos = []config.Repo{{Name: "o/r"}, {Name: "git.example.com/a/b/c", Host: "git.example.com"}}
	if g := repoGit(cfg, "git.example.com/a/b/c", nil); g.RemoteURL != "https://git.example.com/a/b/c.git" || g.User != "oauth2" {
		t.Fatalf("%+v", g)
	}
	if g := repoGit(cfg, "o/r", nil); g.RemoteURL != "https://github.com/o/r.git" || g.User != "" {
		t.Fatalf("%+v", g)
	}
	// A repo dropped from the config keeps its host.
	if g := repoGit(cfg, "gitlab.com/old/one", nil); g.RemoteURL != "https://gitlab.com/old/one.git" || g.User != "oauth2" {
		t.Fatalf("%+v", g)
	}
	t.Setenv("GITLAB_TOKEN", "")
	if _, err := tokens(cfg, "git.example.com/a/b/c"); err == nil || !strings.Contains(err.Error(), "GITLAB_TOKEN") {
		t.Fatalf("err %v", err)
	}
}

func TestParseRepoListGitLab(t *testing.T) {
	names, err := parseRepoList("o/a, https://gitlab.com/g/sub/p.git gitlab.example.com/g/p")
	if err != nil || strings.Join(names, ",") != "o/a,gitlab.com/g/sub/p,gitlab.example.com/g/p" {
		t.Fatalf("%v %v", names, err)
	}
	if _, err := parseRepoList("https://gitlab.com/onlyone"); err == nil {
		t.Fatal("want an error")
	}
}

// Names from the browser go into factory.yaml, so only real repo names
// are taken.
func TestParseRepoListRefusesOddNames(t *testing.T) {
	for _, bad := range []string{
		"o/r#x", "o/r:\n  evil: 1", "-o/r", "o/..", "o/r/x", "o_/r'", `o/"r"`, "o/r\t#",
		"gitlab.com/g/../p", "gitlab.com/g/p?x=1", "git_lab.com/g/p", "gitlab.com/g//p", "gitlab.com/g/p#frag",
	} {
		if names, err := parseRepoList(bad); err == nil {
			t.Errorf("%q was taken as %q", bad, names)
		}
	}
	names, err := parseRepoList("my-org/my.repo_2, octo_user/x, gitlab.example.com:8443/team.a/sub/proj-1")
	if err != nil || strings.Join(names, ",") != "my-org/my.repo_2,octo_user/x,gitlab.example.com:8443/team.a/sub/proj-1" {
		t.Fatalf("%v %v", names, err)
	}
}

// One repo is written as repo: name, quoted when YAML needs it.
func TestWriteReposQuotesOneRepo(t *testing.T) {
	path := testConfig(t, "o/r")
	for _, name := range []string{"o/r", "o/yes", "gitlab.com/g/p"} {
		if err := writeRepos(path, []config.Repo{{Name: name}}); err != nil {
			t.Fatal(err)
		}
		cfg, err := config.Load(path)
		if err != nil || strings.Join(cfg.RepoNames(), ",") != name {
			t.Fatalf("%s: %v %v", name, cfg.RepoNames(), err)
		}
	}
}

// A repo pasted the way GitHub or GitLab shows it is taken: a clone
// address over SSH, or a link to a page inside the repo.
func TestParseRepoListTakesWhatYouPaste(t *testing.T) {
	for in, want := range map[string]string{
		"git@github.com:octocat/hello-world.git":           "octocat/hello-world",
		"https://github.com/octocat/hello-world/tree/main": "octocat/hello-world",
		"github.com/octocat/hello-world/pull/3":            "octocat/hello-world",
		"git@gitlab.com:group/sub/project.git":             "gitlab.com/group/sub/project",
		"ssh://git@gitlab.example.com:2222/g/p.git":        "gitlab.example.com/g/p",
		"https://gitlab.com/group/project/-/tree/main":     "gitlab.com/group/project",
	} {
		names, err := parseRepoList(in)
		if err != nil || strings.Join(names, ",") != want {
			t.Errorf("%s: %v %v, want %s", in, names, err, want)
		}
	}
}

// A new factory.yaml names no repo: the first one chosen takes the place
// of the example's commented-out line.
func TestWriteReposIntoANewConfig(t *testing.T) {
	path := testConfig(t, "o/x")
	b, _ := os.ReadFile(path)
	os.WriteFile(path, []byte(strings.Replace(string(b), "repo: o/x\n", "# repo: octocat/hello-world  # an example\n", 1)), 0o644)
	if cfg, err := config.Load(path); err != nil || !cfg.NoRepos() {
		t.Fatalf("the new config names %v, %v", cfg.RepoNames(), err)
	}
	if err := writeRepos(path, []config.Repo{{Name: "o/r"}}); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(path)
	if !strings.Contains(string(b), "\nrepo: o/r\n") || strings.Contains(string(b), "# repo:") {
		t.Fatalf("factory.yaml:\n%s", b)
	}
	cfg, err := config.Load(path)
	if err != nil || strings.Join(cfg.RepoNames(), ",") != "o/r" {
		t.Fatalf("%v %v", cfg.RepoNames(), err)
	}
}
