package main

import (
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/claptimes5/factory/internal/config"
	"github.com/claptimes5/factory/internal/github"
	"github.com/claptimes5/factory/internal/pipeline"
	"github.com/claptimes5/factory/internal/readiness"
)

// With -queue, a ready repo's gaps all go to the factory; a repo that
// can't be checked doesn't stop the others.
func TestRunReadinessQueue(t *testing.T) {
	f := &fakeGitHub{labels: map[string]bool{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	cfg, err := config.Load(testConfig(t, "o/r"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Repos = []config.Repo{{Name: "o/broken"}, {Name: "o/r"}}
	p := &pipeline.Pipeline{Cfg: cfg, ClientsFor: func(repo string) pipeline.RepoClients {
		return pipeline.RepoClients{GH: &github.Client{Repo: repo, BaseURL: srv.URL, Tokens: github.StaticToken("t")}}
	}}
	oldAssess := assess
	t.Cleanup(func() { assess = oldAssess })
	assess = func(_ context.Context, _ *config.Config, p *pipeline.Pipeline, repo string, _ io.Writer) (*readiness.Report, error) {
		if !strings.HasPrefix(p.JobPrefix, "ready") {
			t.Errorf("job prefix %q would collide with the daemon's containers", p.JobPrefix)
		}
		if repo == "o/broken" {
			return nil, errors.New("clone failed")
		}
		return &readiness.Report{Repo: repo, Branch: "main", Ready: true, Findings: []readiness.Finding{
			{Level: readiness.Gap, Issue: &readiness.Issue{Key: "e2e", Title: "Add end-to-end smoke tests"}},
		}}, nil
	}
	err = runReadiness(context.Background(), cfg, p, []string{"-queue"})
	if err == nil || !strings.Contains(err.Error(), "o/broken") {
		t.Fatalf("err %v", err)
	}
	if strings.Join(f.issues, "|") != "/repos/o/r/issues Add end-to-end smoke tests factory:readiness,factory:ready" {
		t.Fatalf("issues: %v", f.issues)
	}

	// An incomplete report files nothing.
	f.issues = nil
	assess = func(context.Context, *config.Config, *pipeline.Pipeline, string, io.Writer) (*readiness.Report, error) {
		return &readiness.Report{Repo: "o/r", Incomplete: true, Findings: []readiness.Finding{
			{Level: readiness.Gap, Issue: &readiness.Issue{Key: "e2e", Title: "Add end-to-end smoke tests"}},
		}}, nil
	}
	if err := runReadiness(context.Background(), cfg, p, []string{"-file", "o/r"}); err != nil || len(f.issues) != 0 {
		t.Fatalf("err %v, issues %v", err, f.issues)
	}
}
