package main

import (
	"context"
	"fmt"
	"os"

	"github.com/claptimes5/factory/internal/config"
	"github.com/claptimes5/factory/internal/github"
	"github.com/claptimes5/factory/internal/gitlab"
	"github.com/claptimes5/factory/internal/pipeline"
)

// forge is a repo's host as setup, the readiness check and the dashboard
// use it: GitHub or GitLab.
type forge interface {
	pipeline.GitHub
	ListIssuesIn(ctx context.Context, label, state string) ([]github.Issue, error)
	EnsureLabel(ctx context.Context, name, color, description string) (bool, error)
	BranchSHA(ctx context.Context, branch string) (string, error)
	DefaultBranch(ctx context.Context) (string, error)
	ListDir(ctx context.Context, dir string) ([]string, error)
	ReadFile(ctx context.Context, path string) (string, error)
	CheckRuns(ctx context.Context, sha string) ([]github.CheckRun, error)
}

var (
	_ forge                = (*github.Client)(nil)
	_ forge                = (*gitlab.Client)(nil)
	_ pipeline.HealthForge = (*github.Client)(nil)
	_ pipeline.HealthForge = (*gitlab.Client)(nil)
	_ pipeline.Previewer   = (*github.Client)(nil)
	_ pipeline.Previewer   = (*gitlab.Client)(nil)
)

// gitlabAPI overrides the GitLab API base for every host; tests set it.
var gitlabAPI = ""

// repoFor is a repo's settings, or for one since dropped from the config
// (its work items can still be open) just its host and name.
func repoFor(cfg *config.Config, repo string) config.Repo {
	if rc, ok := cfg.RepoConf(repo); ok {
		return rc
	}
	host, name := config.SplitRepo("", repo)
	return config.Repo{Name: name, Host: host}
}

// newForge makes the client for a repo on its host.
func newForge(cfg *config.Config, repo string, ts github.TokenSource) forge {
	if rc := repoFor(cfg, repo); rc.GitLab() {
		return &gitlab.Client{Project: rc.Path(), Host: rc.Host, BaseURL: gitlabAPI, Tokens: ts}
	}
	return &github.Client{Repo: repo, BaseURL: githubAPI, WebURL: githubWeb, Tokens: ts}
}

// gitlabToken is the token for GitLab repos.
func gitlabToken() (github.TokenSource, error) {
	if t := os.Getenv("GITLAB_TOKEN"); t != "" {
		return github.StaticToken(t), nil
	}
	return nil, fmt.Errorf("no GitLab token: run `factory setup`, or set GITLAB_TOKEN to a token with the api and write_repository scopes")
}

// repoGit is how the factory reaches a repo with git.
func repoGit(cfg *config.Config, repo string, ts github.TokenSource) *pipeline.Git {
	rc := repoFor(cfg, repo)
	g := &pipeline.Git{RemoteURL: rc.CloneURL(), Tokens: ts, Author: "factory[bot] <factory@users.noreply.github.com>", AskPassDir: cfg.Path(cfg.DataDir)}
	if rc.GitLab() {
		g.User = "oauth2"
	}
	return g
}
