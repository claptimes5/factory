package config

import (
	"fmt"
	"time"
)

// Hunt is old-bug hunting: a low-priority periodic job that reviews one
// area of a repo's feature map at a time, on the base branch, read-only,
// and files what it finds as issues waiting for approval, as the sources
// do. It runs only when no other job is queued or running.
type Hunt struct {
	// Enabled turns it on; off by default, since it spends on its own.
	Enabled bool `yaml:"enabled"`
	// Every is how often a hunt may start; default 24h, at least 1h.
	Every Duration `yaml:"every,omitempty"`
	// Budget is dollars per hunt; 0 uses budgets.review.
	Budget float64 `yaml:"budget,omitempty"`
	// DailyBudget is what hunts may spend in all per day; 0 is one
	// hunt's budget. Hunts also wait while today's build budget is spent.
	DailyBudget float64 `yaml:"daily_budget,omitempty"`
	// MaxFindings is the most issues one hunt files; default 3, at most 10.
	MaxFindings int `yaml:"max_findings,omitempty"`
	// DailyIssueCap is how many issues hunts may file per repo a day,
	// apart from the sources' cap; default max_findings.
	DailyIssueCap int `yaml:"daily_issue_cap,omitempty"`
	// Repos limits it to these repos; default every repo.
	Repos []string `yaml:"repos,omitempty"`
}

// Limits on hunting.
const (
	MinHuntEvery    = time.Hour
	MaxHuntFindings = 10
)

// HuntBudget is the dollar cap of one hunt.
func (c *Config) HuntBudget() float64 {
	if c.Hunt.Budget > 0 {
		return c.Hunt.Budget
	}
	return c.Budgets.Review
}

// HuntDailyBudget is what hunts may spend per day.
func (c *Config) HuntDailyBudget() float64 {
	if c.Hunt.DailyBudget > 0 {
		return c.Hunt.DailyBudget
	}
	return c.HuntBudget()
}

// HuntRepos are the repos hunted.
func (c *Config) HuntRepos() []string {
	if len(c.Hunt.Repos) == 0 {
		return c.RepoNames()
	}
	var out []string
	for _, r := range c.Hunt.Repos {
		if rc, ok := c.RepoConf(r); ok {
			out = append(out, rc.Name)
		}
	}
	return out
}

func (c *Config) huntDefaults() {
	h := &c.Hunt
	if h.Every.Duration == 0 {
		h.Every.Duration = 24 * time.Hour
	}
	if h.MaxFindings == 0 {
		h.MaxFindings = 3
	}
	if h.DailyIssueCap == 0 {
		h.DailyIssueCap = h.MaxFindings
	}
	if c.Timeouts["hunt"].Duration == 0 {
		c.Timeouts["hunt"] = Duration{20 * time.Minute}
	}
	// It reads code for bugs, as review does: the reviewer's model.
	if _, ok := c.Routing["hunt"]; !ok && c.Routing != nil {
		if r, ok := c.Routing["review"]; ok {
			c.Routing["hunt"] = Route{Harness: r.Harness, Model: r.Model, Effort: r.Effort}
		}
	}
}

func (c *Config) checkHunt() []error {
	var errs []error
	bad := func(f string, a ...any) { errs = append(errs, fmt.Errorf(f, a...)) }
	h := c.Hunt
	if h.Every.Duration < MinHuntEvery {
		bad("hunt.every must be at least 1h")
	}
	if h.Budget < 0 || h.DailyBudget < 0 {
		bad("hunt.budget and hunt.daily_budget can't be negative; leave them out to use the defaults")
	}
	if h.MaxFindings < 1 || h.MaxFindings > MaxHuntFindings {
		bad("hunt.max_findings must be between 1 and %d", MaxHuntFindings)
	}
	if h.DailyIssueCap < 1 {
		bad("hunt.daily_issue_cap must be at least 1")
	}
	for _, r := range h.Repos {
		if _, ok := c.RepoConf(r); !ok {
			bad("hunt.repos: %s isn't in repos", r)
		}
	}
	if _, ok := c.Routing["hunt"]; h.Enabled && !ok {
		bad("routing.hunt is missing (it defaults from routing.review)")
	}
	return errs
}
