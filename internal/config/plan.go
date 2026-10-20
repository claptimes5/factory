package config

import (
	"fmt"
	"slices"
	"time"
)

// Plan is the plan check for big items: before the build, the builder's
// route for the item's size writes a short plan (approach, files, risks,
// tests), posted on the issue. An item of one of Sizes waits for a human
// to approve it, with the same factory:go that approves a cost estimate
// (one OK covers both); a medium item's plan can be checked by the critic
// instead, which approves it or sends it back once.
type Plan struct {
	// Enabled is on unless false.
	Enabled *bool `yaml:"enabled,omitempty"`
	// Sizes are the triage sizes whose plan waits for a human. Default
	// [L].
	Sizes []string `yaml:"sizes,omitempty"`
	// Medium is critic, for routing.critic to check an M item's plan
	// before the build goes on by itself, or off (the default: a plan and
	// a check cost two more runs on every M item).
	Medium string `yaml:"medium,omitempty"`
}

// Plan check modes for an item's size (Config.PlanFor).
const (
	PlanHuman  = "human"
	PlanCritic = "critic"
)

// On says whether the plan check runs at all.
func (p Plan) On() bool { return p.Enabled == nil || *p.Enabled }

// Off turns the plan check off, as eval and compare do: nobody is there
// to approve a plan.
func (p *Plan) Off() { p.Enabled = new(bool) }

// PlanFor is how an item of this size gets its plan approved: PlanHuman,
// PlanCritic, or "" for no plan.
func (c *Config) PlanFor(size string) string {
	switch {
	case !c.Plan.On() || size == "":
		return ""
	case slices.Contains(c.Plan.Sizes, size):
		return PlanHuman
	case size == "M" && c.Plan.Medium == PlanCritic:
		return PlanCritic
	}
	return ""
}

func (c *Config) planDefaults() {
	if c.Plan.Sizes == nil {
		c.Plan.Sizes = []string{"L"}
	}
	if c.Timeouts["plan"].Duration == 0 {
		c.Timeouts["plan"] = Duration{15 * time.Minute}
	}
}

func (c *Config) checkPlan() []error {
	var errs []error
	for _, s := range c.Plan.Sizes {
		if !slices.Contains([]string{"XS", "S", "M", "L"}, s) {
			errs = append(errs, fmt.Errorf("plan.sizes: %q isn't a size (XS, S, M or L)", s))
		}
	}
	if m := c.Plan.Medium; m != "" && m != PlanCritic && m != "off" {
		errs = append(errs, fmt.Errorf("plan.medium is %q: critic or off", m))
	}
	return errs
}

// Concise says whether plans and chat replies are written with
// prompts/concise.md.
func (c *Config) Concise() bool { return c.ConciseAsks == nil || *c.ConciseAsks }
