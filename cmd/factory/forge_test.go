package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	f.tokens = append(f.tokens, r.Header.Get("PRIVATE-TOKEN"))
	if r.Header.Get("PRIVATE-TOKEN") != "glpat-abc" {
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
		"glpat-wrong", "y", "glpat-abc", // GitLab: a bad token, then a good one
		"y", // create labels on both
		"",  // run the readiness check
		"sk-ant-api03-abc",
		"",
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
