package config

import (
	"fmt"
	"time"
)

// Reviews says how review checks itself: a verifier rules on each blocking
// finding before it blocks, a high-risk item can have its reviewer and
// critic read it side by side, and a re-review reads only what changed.
type Reviews struct {
	// Verify sends each blocking finding to routing.verify, which confirms
	// or refutes it. Only a confirmed finding blocks; a refuted one stays
	// as a note. When the verifier can't run (budget, an error, no
	// answer), the finding blocks. On unless false.
	Verify *bool `yaml:"verify,omitempty"`
	// PairedHighRisk reviews a high-risk item with its reviewer and its
	// critic at once, instead of the critic after the reviewer passes it,
	// and blocks on any finding of either that the verifier confirms.
	PairedHighRisk bool `yaml:"paired_high_risk,omitempty"`
	// Incremental gives a reviewer, after its first round, only the diff
	// since the commit it last reviewed, with its open findings. On unless
	// false.
	Incremental *bool `yaml:"incremental,omitempty"`
	// ContextBytes is the largest whole diff a re-review also gets as
	// context; a larger one is listed by file. Default 20000.
	ContextBytes int `yaml:"context_bytes,omitempty"`
	// Rethink gives an item that hits escalation.max_review_rounds one
	// more try before it parks: a fresh build from the base branch on the
	// next size's implement route, told the approach itself may be wrong.
	// On unless false.
	Rethink *bool `yaml:"rethink,omitempty"`
}

// VerifyOn says whether blocking findings go to the verifier.
func (r Reviews) VerifyOn() bool { return r.Verify == nil || *r.Verify }

// RethinkOn says whether the round cap earns one fresh attempt first.
func (r Reviews) RethinkOn() bool { return r.Rethink == nil || *r.Rethink }

// IncrementalOn says whether a re-review reads only what changed.
func (r Reviews) IncrementalOn() bool { return r.Incremental == nil || *r.Incremental }

func (c *Config) reviewDefaults() {
	if c.Reviews.ContextBytes == 0 {
		c.Reviews.ContextBytes = 20000
	}
	if c.Timeouts["verify"].Duration == 0 {
		c.Timeouts["verify"] = Duration{10 * time.Minute}
	}
}

func (c *Config) checkReview() []error {
	var errs []error
	if c.Reviews.ContextBytes < 0 {
		errs = append(errs, fmt.Errorf("review.context_bytes can't be negative"))
	}
	if c.Budgets.Verify < 0 {
		errs = append(errs, fmt.Errorf("budgets.verify must be > 0, or left out to use budgets.triage"))
	}
	return errs
}

// buildsWith says whether some implement route can build with this
// harness and model, at any tier it escalates to, and names its size.
func (c *Config) buildsWith(harness, model string) (string, bool) {
	for _, size := range []string{"XS", "S", "M", "L"} {
		b, ok := c.Routing["implement."+size]
		if !ok || b.Harness != harness {
			continue
		}
		for _, m := range c.builderModels(b) {
			if sameModel(harness, m, model) {
				return size, true
			}
		}
	}
	return "", false
}

// VerifyRoute is the verifier's route: routing.verify, else triage's
// harness and model (a cheap tier), unless a builder could run on those,
// when it is the reviewer's. The verifier never grades its own build.
func (c *Config) VerifyRoute() Route {
	if r, ok := c.Routing["verify"]; ok {
		return r
	}
	t := c.Routing["triage"]
	if _, builds := c.buildsWith(t.Harness, t.Model); !builds {
		return Route{Harness: t.Harness, Model: t.Model, Effort: t.Effort}
	}
	r := c.Routing["review"]
	return Route{Harness: r.Harness, Model: r.Model, Effort: r.Effort}
}

// CriticRoute is the high-risk critic's route: routing.critic, else the
// strongest Claude tier (implement.L) with only what makes it that model,
// not the builder's skills, instructions or fast mode.
func (c *Config) CriticRoute() Route {
	if r, ok := c.Routing["critic"]; ok {
		return r
	}
	r := c.Routing["implement.L"]
	r.Skills, r.Use, r.Instructions, r.InstructionsFile, r.Fast = "", nil, "", "", false
	return r
}
