package config

import (
	"strings"
	"testing"
	"time"
)

func TestSources(t *testing.T) {
	b, _ := exampleYAML()
	y := string(b)
	c, err := Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	s := c.Sources
	if s.Any() || s.DailyIssueCap != 3 || s.Sentry.Every.Duration != 15*time.Minute || s.Sentry.BaseURL != "https://sentry.io" || s.Sentry.MinEvents != 10 ||
		s.Dependabot.Every.Duration != time.Hour || s.Dependabot.MinSeverity != "high" ||
		s.TODOs.Every.Duration != 6*time.Hour || s.TODOs.MaxPerRun != 5 || strings.Join(s.TODOs.Markers, ",") != "TODO(factory),FIXME(factory)" {
		t.Fatalf("defaults: %+v", s)
	}
	repo := c.RepoNames()[0]
	if _, err := Parse([]byte(y + "\nsources:\n  sentry: {enabled: true, org: acme, projects: {" + repo + ": api, other/x: web}}\n")); err == nil {
		t.Fatal("accepted a project for a repo not in repos")
	}
	c, err = Parse([]byte(y + "\nsources:\n  daily_issue_cap: 1\n  sentry: {enabled: true, org: acme, projects: {" + repo + ": other/api}}\n" +
		"  dependabot: {enabled: true, min_severity: critical}\n  todos: {enabled: true, markers: [TODO, FIXME], every: 2h}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if o, p, ok := c.Sources.Sentry.SentryProject(repo); !ok || o != "other" || p != "api" {
		t.Fatalf("project %s %s %v", o, p, ok)
	}
	if !c.Sources.Any() || c.Sources.DailyIssueCap != 1 || c.Sources.TODOs.Every.Duration != 2*time.Hour || len(c.DependabotRepos()) != 1 {
		t.Fatalf("set: %+v", c.Sources)
	}
	if _, err := Parse([]byte(y + "\nsources: {sentry: {base_url: 'http://127.0.0.1:9000'}}\n")); err != nil {
		t.Fatalf("loopback http: %v", err)
	}
	c.Sources.Off()
	if c.Sources.Any() {
		t.Fatal("Off left a source on")
	}
	for name, bad := range map[string]string{
		"sentry too often":      "sources: {sentry: {every: 1m}}",
		"dependabot too often":  "sources: {dependabot: {every: 5m}}",
		"todos too often":       "sources: {todos: {every: 30m}}",
		"negative cap":          "sources: {daily_issue_cap: -1}",
		"unknown key":           "sources: {sentry: {token: abc}}",
		"unknown source":        "sources: {snyk: {enabled: true}}",
		"sentry no projects":    "sources: {sentry: {enabled: true, org: acme}}",
		"sentry no org":         "sources: {sentry: {projects: {" + repo + ": api}}}",
		"sentry bad slug":       "sources: {sentry: {org: acme, projects: {" + repo + ": 'a b'}}}",
		"sentry bad url":        "sources: {sentry: {base_url: 'ftp://x'}}",
		"sentry url with creds": "sources: {sentry: {base_url: 'https://u:p@sentry.example'}}",
		"sentry bad sort":       "sources: {sentry: {sort: date}}",
		"sentry plain http":     "sources: {sentry: {base_url: 'http://sentry.example.com'}}",
		"bad severity":          "sources: {dependabot: {min_severity: severe}}",
		"dependabot not a repo": "sources: {dependabot: {repos: [o/nope]}}",
		"empty markers":         "sources: {todos: {markers: []}}",
		"absolute skip":         "sources: {todos: {skip_paths: [/etc]}}",
	} {
		if _, err := Parse([]byte(y + "\n" + bad + "\n")); err == nil {
			t.Errorf("%s: accepted %s", name, bad)
		}
	}
}

func TestDependabotIsGitHubOnly(t *testing.T) {
	b, _ := exampleYAML()
	y := strings.Replace(string(b), "repo: claptimes5/factory", "repos: [o/r, gitlab.com/team/web]", 1)
	if _, err := Parse([]byte(y)); err != nil {
		t.Fatal(err)
	}
	_, err := Parse([]byte(y + "sources: {dependabot: {enabled: true}}\n"))
	if err == nil || !strings.Contains(err.Error(), "GitLab's dependency scanning isn't supported") {
		t.Fatalf("GitLab repo with dependabot: %v", err)
	}
	if _, err := Parse([]byte(y + "sources: {dependabot: {enabled: true, repos: [gitlab.com/team/web]}}\n")); err == nil {
		t.Fatal("accepted a GitLab repo in dependabot.repos")
	}
	c, err := Parse([]byte(y + "sources: {dependabot: {enabled: true, repos: [o/r]}}\n"))
	if err != nil || strings.Join(c.DependabotRepos(), ",") != "o/r" {
		t.Fatalf("GitHub repo listed: %v %v", c, err)
	}
}
