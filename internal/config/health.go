package config

import (
	"fmt"
	"time"
)

// MainHealth is the loop that watches CI on each repo's base branch and,
// when a check goes red or flakes there, files a fix issue that goes to
// the front of the queue.
type MainHealth struct {
	// Enabled is off unless set: the issues it files spend on builds.
	Enabled bool `yaml:"enabled"`
	// Every is how often it reads CI; default 10m, at least 1m.
	Every Duration `yaml:"every,omitempty"`
	// DailyIssueCap is how many issues it may file per repo a day; 0 or
	// left out is 3.
	DailyIssueCap int `yaml:"daily_issue_cap,omitempty"`
	// RequireApproval files the issues without the intake label, so
	// they wait for someone to add it. Off, they're built at once.
	RequireApproval bool `yaml:"require_approval,omitempty"`
}

// MinHealthEvery is the shortest main health cadence: each reading is a
// few API calls per repo.
const MinHealthEvery = time.Minute

func (c *Config) healthDefaults() {
	h := &c.MainHealth
	if h.Every.Duration == 0 {
		h.Every.Duration = 10 * time.Minute
	}
	if h.DailyIssueCap == 0 {
		h.DailyIssueCap = 3
	}
}

func (c *Config) checkHealth() []error {
	var errs []error
	if c.MainHealth.Every.Duration < MinHealthEvery {
		errs = append(errs, fmt.Errorf("main_health.every must be at least 1m"))
	}
	if c.MainHealth.DailyIssueCap < 0 {
		errs = append(errs, fmt.Errorf("main_health.daily_issue_cap must be at least 1; 0 or left out is 3 (main_health.enabled: false turns it off)"))
	}
	return errs
}
