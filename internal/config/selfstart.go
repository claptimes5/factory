package config

import (
	"fmt"
	"slices"
)

// SelfStart is trust level 6 for a repo: the issues the factory files
// from its own findings start without a label, and merge themselves under
// the repo's auto_merge. Below it, they wait for a person
// (docs/plans/trust-level-6.md).
type SelfStart struct {
	// Sources are the findings that start on their own: main_health,
	// dependabot, todos, sentry, hunt, orchestrator.
	Sources []string `yaml:"sources"`
	// Daily is how many issues may start on their own a day (default 3).
	Daily int `yaml:"daily,omitempty"`
	// Budget is the dollars a day for work that started on its own
	// (default a fifth of budgets.daily_total).
	Budget *float64 `yaml:"budget,omitempty"`
}

// SelfStartSources are the findings level 6 may start, safest first.
var SelfStartSources = []string{"main_health", "dependabot", "todos", "sentry", "hunt", "orchestrator"}

// DefaultSelfStartSources are the ones the Trust screen turns on.
var DefaultSelfStartSources = []string{"main_health", "dependabot"}

// Allows says whether a finding from source starts on its own.
func (s *SelfStart) Allows(source string) bool { return s != nil && slices.Contains(s.Sources, source) }

// DailyCap is how many may start on their own a day.
func (s *SelfStart) DailyCap() int {
	if s == nil {
		return 0
	}
	if s.Daily > 0 {
		return s.Daily
	}
	return 3
}

// BudgetUSD is the dollars a day for work that started on its own.
func (s *SelfStart) BudgetUSD(dailyTotal float64) float64 {
	if s == nil {
		return 0
	}
	if s.Budget != nil {
		return *s.Budget
	}
	return dailyTotal / 5
}

func (s *SelfStart) check(repo string) error {
	if s == nil {
		return nil
	}
	for _, src := range s.Sources {
		if !slices.Contains(SelfStartSources, src) {
			return fmt.Errorf("repos[%s].self_start.sources: %q isn't one of %v", repo, src, SelfStartSources)
		}
	}
	if s.Daily < 0 || s.Budget != nil && *s.Budget < 0 {
		return fmt.Errorf("repos[%s].self_start: daily and budget can't be negative", repo)
	}
	return nil
}
