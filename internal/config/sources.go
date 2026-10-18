package config

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Sources are places the factory finds work on its own: Sentry errors,
// Dependabot alerts and TODO comments. Each files issues that wait for
// someone with write access to approve them; none is built on its own.
type Sources struct {
	// DailyIssueCap is how many issues all of them together may file per
	// repo a day; default 3.
	DailyIssueCap int              `yaml:"daily_issue_cap,omitempty"`
	Sentry        SentrySource     `yaml:"sentry"`
	Dependabot    DependabotSource `yaml:"dependabot"`
	TODOs         TODOSource       `yaml:"todos"`
}

// SentrySource files an issue for each unresolved Sentry issue over the
// thresholds. The token is SENTRY_AUTH_TOKEN, from factory.env.
type SentrySource struct {
	Enabled bool `yaml:"enabled"`
	// Every is how often it reads Sentry; default 15m, at least 5m.
	Every Duration `yaml:"every,omitempty"`
	// BaseURL is a self-hosted Sentry's address; default https://sentry.io.
	BaseURL string `yaml:"base_url,omitempty"`
	// Org is the organization slug.
	Org string `yaml:"org,omitempty"`
	// Projects maps a repo to its Sentry project slug, or org/project for
	// one in another organization.
	Projects map[string]string `yaml:"projects,omitempty"`
	// MinEvents and MinUsers are what a Sentry issue needs to be filed:
	// default 10 events, and any number of users.
	MinEvents int `yaml:"min_events,omitempty"`
	MinUsers  int `yaml:"min_users,omitempty"`
	// Sort is freq (the default: most events first) or user.
	Sort string `yaml:"sort,omitempty"`
}

// DependabotSource files an issue for each open Dependabot security alert
// at or above a severity. GitHub only.
type DependabotSource struct {
	Enabled bool `yaml:"enabled"`
	// Every is how often it reads the alerts; default 1h, at least 15m.
	Every Duration `yaml:"every,omitempty"`
	// MinSeverity is low, medium, high (the default) or critical.
	MinSeverity string `yaml:"min_severity,omitempty"`
	// Repos limits it to these repos; default every GitHub repo.
	Repos []string `yaml:"repos,omitempty"`
}

// TODOSource files an issue for each marked comment on a repo's base
// branch.
type TODOSource struct {
	Enabled bool `yaml:"enabled"`
	// Every is how often it scans; default 6h, at least 1h.
	Every Duration `yaml:"every,omitempty"`
	// Markers are what a comment must start with; by default the
	// factory's own, DefaultTODOMarkers. Add TODO and FIXME to file
	// every one.
	Markers []string `yaml:"markers,omitempty"`
	// MaxPerRun is how many it may file per scan; default 5.
	MaxPerRun int `yaml:"max_per_run,omitempty"`
	// SkipPaths are folders (vendor/legacy) or globs (*.gen.ts) to leave
	// out, on top of the vendored and generated ones always left out.
	SkipPaths []string `yaml:"skip_paths,omitempty"`
	// Repos limits it to these repos; default every repo.
	Repos []string `yaml:"repos,omitempty"`
}

// Shortest cadences: each reading is an API call or a clone per repo.
const (
	MinSentryEvery     = 5 * time.Minute
	MinDependabotEvery = 15 * time.Minute
	MinTODOEvery       = time.Hour
)

// Severities are Dependabot's, lowest first.
var Severities = []string{"low", "medium", "high", "critical"}

// DefaultTODOMarkers are matched unless markers says otherwise.
var DefaultTODOMarkers = []string{"TODO(factory)", "FIXME(factory)"}

// Off turns every source off, for runs that aren't the real daemon's.
func (s *Sources) Off() {
	s.Sentry.Enabled, s.Dependabot.Enabled, s.TODOs.Enabled = false, false, false
}

// Any says whether a source is on.
func (s *Sources) Any() bool { return s.Sentry.Enabled || s.Dependabot.Enabled || s.TODOs.Enabled }

func (c *Config) sourcesDefaults() {
	s := &c.Sources
	if s.DailyIssueCap == 0 {
		s.DailyIssueCap = 3
	}
	if s.Sentry.Every.Duration == 0 {
		s.Sentry.Every.Duration = 15 * time.Minute
	}
	if s.Sentry.BaseURL == "" {
		s.Sentry.BaseURL = "https://sentry.io"
	}
	if s.Sentry.MinEvents == 0 {
		s.Sentry.MinEvents = 10
	}
	if s.Sentry.Sort == "" {
		s.Sentry.Sort = "freq"
	}
	if s.Dependabot.Every.Duration == 0 {
		s.Dependabot.Every.Duration = time.Hour
	}
	if s.Dependabot.MinSeverity == "" {
		s.Dependabot.MinSeverity = "high"
	}
	if s.TODOs.Every.Duration == 0 {
		s.TODOs.Every.Duration = 6 * time.Hour
	}
	if s.TODOs.Markers == nil {
		s.TODOs.Markers = DefaultTODOMarkers
	}
	if s.TODOs.MaxPerRun == 0 {
		s.TODOs.MaxPerRun = 5
	}
}

var sentrySlug = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// SentryProject is the org and project a repo's errors are in, if any.
func (s SentrySource) SentryProject(repo string) (org, project string, ok bool) {
	for r, p := range s.Projects {
		if strings.EqualFold(r, repo) {
			if o, pr, cut := strings.Cut(p, "/"); cut {
				return o, pr, true
			}
			return s.Org, p, true
		}
	}
	return "", "", false
}

func (c *Config) checkSources() []error {
	var errs []error
	bad := func(f string, a ...any) { errs = append(errs, fmt.Errorf(f, a...)) }
	s := c.Sources
	if s.DailyIssueCap < 0 {
		bad("sources.daily_issue_cap must be at least 1")
	}
	for _, e := range []struct {
		name, min string
		got, at   time.Duration
	}{{"sentry", "5m", s.Sentry.Every.Duration, MinSentryEvery}, {"dependabot", "15m", s.Dependabot.Every.Duration, MinDependabotEvery}, {"todos", "1h", s.TODOs.Every.Duration, MinTODOEvery}} {
		if e.got < e.at {
			bad("sources.%s.every must be at least %s", e.name, e.min)
		}
	}
	se := s.Sentry
	if u, err := url.Parse(se.BaseURL); err != nil || u.Scheme != "https" && (u.Scheme != "http" || !loopback(u.Hostname())) || u.Host == "" || u.User != nil {
		bad("sources.sentry.base_url must be an https address such as https://sentry.io (http only on this machine): the token goes with every request")
	}
	if se.Org != "" && !sentrySlug.MatchString(se.Org) {
		bad("sources.sentry.org %q isn't an organization slug", se.Org)
	}
	for repo, p := range se.Projects {
		if _, ok := c.RepoConf(repo); !ok {
			bad("sources.sentry.projects: %s isn't in repos", repo)
		}
		o, pr, cut := strings.Cut(p, "/")
		if !cut {
			o, pr = se.Org, p
		}
		switch {
		case !sentrySlug.MatchString(pr) || cut && !sentrySlug.MatchString(o):
			bad("sources.sentry.projects: %s: %q isn't a project slug (or org/project)", repo, p)
		case o == "":
			bad("sources.sentry.projects: %s: set sources.sentry.org, or write the project as org/project", repo)
		}
	}
	if se.Enabled && len(se.Projects) == 0 {
		bad("sources.sentry.projects must map at least one repo to its Sentry project")
	}
	if se.MinEvents < 0 || se.MinUsers < 0 {
		bad("sources.sentry.min_events and min_users can't be negative")
	}
	if se.Sort != "freq" && se.Sort != "user" {
		bad("sources.sentry.sort must be freq or user")
	}
	d := s.Dependabot
	if !slices.Contains(Severities, d.MinSeverity) {
		bad("sources.dependabot.min_severity must be low, medium, high or critical")
	}
	for _, r := range d.Repos {
		if rc, ok := c.RepoConf(r); !ok {
			bad("sources.dependabot.repos: %s isn't in repos", r)
		} else if rc.GitLab() {
			bad("sources.dependabot.repos: %s is on GitLab; Dependabot is GitHub's, and GitLab's dependency scanning isn't supported", r)
		}
	}
	if d.Enabled && len(d.Repos) == 0 {
		var gl []string
		for _, r := range c.Repos {
			if r.GitLab() {
				gl = append(gl, r.Name)
			}
		}
		if len(gl) > 0 {
			bad("sources.dependabot is GitHub's, and GitLab's dependency scanning isn't supported: list the GitHub repos in sources.dependabot.repos (%s is on GitLab)", strings.Join(gl, ", "))
		}
	}
	t := s.TODOs
	if len(t.Markers) == 0 {
		bad("sources.todos.markers needs at least one marker, such as TODO(factory)")
	}
	for _, m := range t.Markers {
		if strings.TrimSpace(m) == "" || strings.ContainsAny(m, "\n\r") {
			bad("sources.todos.markers: %q isn't a marker", m)
		}
	}
	if t.MaxPerRun < 0 {
		bad("sources.todos.max_per_run must be at least 1")
	}
	for _, p := range t.SkipPaths {
		if strings.TrimSpace(p) == "" || strings.HasPrefix(p, "/") || strings.Contains(p, "..") {
			bad("sources.todos.skip_paths: %q must be a folder or glob relative to the repo", p)
		}
	}
	for _, r := range t.Repos {
		if _, ok := c.RepoConf(r); !ok {
			bad("sources.todos.repos: %s isn't in repos", r)
		}
	}
	return errs
}

// loopback says whether a host is this machine.
func loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// DependabotRepos are the repos Dependabot alerts are read for.
func (c *Config) DependabotRepos() []string {
	if len(c.Sources.Dependabot.Repos) > 0 {
		return c.Sources.Dependabot.Repos
	}
	var out []string
	for _, r := range c.Repos {
		if !r.GitLab() {
			out = append(out, r.Name)
		}
	}
	return out
}

// TODORepos are the repos scanned for TODOs.
func (c *Config) TODORepos() []string {
	if len(c.Sources.TODOs.Repos) > 0 {
		return c.Sources.TODOs.Repos
	}
	return c.RepoNames()
}

// SentryRepos are the repos with a Sentry project, in config order.
func (c *Config) SentryRepos() []string {
	var out []string
	for _, r := range c.RepoNames() {
		if _, _, ok := c.Sources.Sentry.SentryProject(r); ok {
			out = append(out, r)
		}
	}
	return out
}
