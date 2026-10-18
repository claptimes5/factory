package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Grooming is backlog grooming: on a cadence, a read-only groom job reads
// each repo's open issues and flags likely duplicates, stale issues and
// ones too big for one change. The factory comments and labels; a human
// confirms with a label before anything is closed or split.
type Grooming struct {
	// EveryText is the cadence as written: a duration of at least 24h,
	// such as 168h for weekly, or 0 or none (the default) for off. Every
	// is it parsed.
	EveryText string        `yaml:"every"`
	Every     time.Duration `yaml:"-"`
	// StaleAfterText is how long an issue goes without activity before it
	// may be called stale: a duration such as 2160h, or days such as 90d
	// (the default). At least 7d.
	StaleAfterText string        `yaml:"stale_after,omitempty"`
	StaleAfter     time.Duration `yaml:"-"`
	// MaxIssues is how many issues one run reads; default 50, at most 200.
	MaxIssues int `yaml:"max_issues,omitempty"`
	// Budget is dollars per groom job; 0 uses budgets.triage.
	Budget float64 `yaml:"budget,omitempty"`
	// Repos limits it to these repos; default every repo.
	Repos []string `yaml:"repos,omitempty"`

	everyErr, staleErr error
}

// Limits on grooming.
const (
	MinGroomingEvery = 24 * time.Hour
	MinStaleAfter    = 7 * 24 * time.Hour
	MaxGroomIssues   = 200
)

// Off turns grooming's cadence off, for eval and compare.
func (g *Grooming) Off() { g.Every = 0 }

// GroomBudget is the dollar cap of a groom job.
func (c *Config) GroomBudget() float64 {
	if c.Grooming.Budget > 0 {
		return c.Grooming.Budget
	}
	return c.Budgets.Triage
}

// GroomRepos are the repos grooming reads.
func (c *Config) GroomRepos() []string {
	if len(c.Grooming.Repos) == 0 {
		return c.RepoNames()
	}
	var out []string
	for _, r := range c.Grooming.Repos {
		if rc, ok := c.RepoConf(r); ok {
			out = append(out, rc.Name)
		}
	}
	return out
}

// parseDays reads a duration that may also be whole days, such as 90d.
func parseDays(s string) (time.Duration, error) {
	if d, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(d)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("%q isn't a number of days", s)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	return time.ParseDuration(s)
}

func (c *Config) groomingDefaults() {
	g := &c.Grooming
	switch s := strings.TrimSpace(g.EveryText); s {
	case "", "none", "0":
		g.Every = 0
	default:
		g.Every, g.everyErr = time.ParseDuration(s)
	}
	if s := strings.TrimSpace(g.StaleAfterText); s == "" {
		g.StaleAfter = 90 * 24 * time.Hour
	} else {
		g.StaleAfter, g.staleErr = parseDays(s)
	}
	if g.MaxIssues == 0 {
		g.MaxIssues = 50
	}
	if c.Timeouts["groom"].Duration == 0 {
		c.Timeouts["groom"] = Duration{10 * time.Minute}
	}
	// It reads issues and writes a few sentences: like learn, on triage's
	// harness, with Sonnet for Claude.
	if _, ok := c.Routing["groom"]; !ok && c.Routing != nil {
		if t, ok := c.Routing["triage"]; ok {
			r := Route{Harness: t.Harness, Model: t.Model}
			if r.Harness == "claude" {
				r.Model = "sonnet"
			}
			c.Routing["groom"] = r
		}
	}
}

func (c *Config) checkGrooming() []error {
	var errs []error
	bad := func(f string, a ...any) { errs = append(errs, fmt.Errorf(f, a...)) }
	g := c.Grooming
	switch {
	case g.everyErr != nil || g.Every < 0:
		bad("grooming.every must be a duration such as 168h, or 0 or none to turn grooming off")
	case g.Every > 0 && g.Every < MinGroomingEvery:
		bad("grooming.every must be at least 24h, or 0 or none to turn grooming off")
	}
	if g.staleErr != nil || g.StaleAfter < MinStaleAfter {
		bad("grooming.stale_after must be a duration of at least 7 days, such as 90d or 2160h")
	}
	if g.MaxIssues < 1 || g.MaxIssues > MaxGroomIssues {
		bad("grooming.max_issues must be between 1 and %d", MaxGroomIssues)
	}
	if g.Budget < 0 {
		bad("grooming.budget can't be negative; leave it out, or 0, to use budgets.triage")
	}
	for _, r := range g.Repos {
		if _, ok := c.RepoConf(r); !ok {
			bad("grooming.repos: %s isn't in repos", r)
		}
	}
	return errs
}
