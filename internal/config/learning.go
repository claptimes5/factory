package config

import (
	"fmt"
	"time"
)

// Learning is the learn loop: from the changes you push to a factory PR
// before merging it, and from findings reviewers repeat across items, a
// daily learn job proposes lines for the repo's AGENTS.md, which its
// CLAUDE.md loads, in a PR of its own.
type Learning struct {
	// Enabled is on unless set to false.
	Enabled *bool `yaml:"enabled,omitempty"`
	// MinFindings is how many alike findings, from at least two items,
	// start a learn job when no human edit does; default 2.
	MinFindings int `yaml:"min_findings,omitempty"`
	// Budget is dollars per learn job; 0 uses budgets.triage.
	Budget float64 `yaml:"budget,omitempty"`
}

// On says whether the learn loop runs.
func (l Learning) On() bool { return l.Enabled == nil || *l.Enabled }

// LearnBudget is the dollar cap of a learn job.
func (c *Config) LearnBudget() float64 {
	if c.Learning.Budget > 0 {
		return c.Learning.Budget
	}
	return c.Budgets.Triage
}

func (c *Config) learningDefaults() {
	if c.Learning.MinFindings == 0 {
		c.Learning.MinFindings = 2
	}
	if c.Timeouts["learn"].Duration == 0 {
		c.Timeouts["learn"] = Duration{10 * time.Minute}
	}
	// It reads diffs and writes a sentence or two: on triage's harness,
	// with Sonnet for Claude, since triage often runs on the cheapest model.
	if _, ok := c.Routing["learn"]; !ok && c.Routing != nil {
		if t, ok := c.Routing["triage"]; ok {
			r := Route{Harness: t.Harness, Model: t.Model}
			if r.Harness == "claude" {
				r.Model = "sonnet"
			}
			c.Routing["learn"] = r
		}
	}
}

func (c *Config) checkLearning() []error {
	var errs []error
	if c.Learning.MinFindings < 2 {
		errs = append(errs, fmt.Errorf("learning.min_findings must be at least 2: a repeat takes two findings"))
	}
	if c.Learning.Budget < 0 {
		errs = append(errs, fmt.Errorf("learning.budget must be > 0, or left out to use budgets.triage"))
	}
	return errs
}
