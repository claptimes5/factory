package config

import (
	"fmt"

	"github.com/claptimes5/factory/internal/pathglob"
)

// Security is the security pass: a review of its own, on routing.security
// with prompts/security.md, of each change that touches code where a
// mistake is an exploit. It reads the change beside the reviewer, and its
// findings block whatever the reviewer decides.
type Security struct {
	// Enabled is on unless set to false.
	Enabled *bool `yaml:"enabled,omitempty"`
	// Paths are the globs of files whose change starts a pass; left out,
	// every area of the sensitive list, which is the place to set them,
	// and [] for none, so only Content and the feature map start one.
	// Set, they replace the sensitive list for the pass. See
	// pathglob.Match for how they match.
	Paths []string `yaml:"paths,omitempty"`
	// ExtraPaths are added to Paths, or to the sensitive list.
	ExtraPaths []string `yaml:"extra_paths,omitempty"`
	// Content also starts a pass on code the change adds that runs a
	// command, builds SQL from strings, evaluates code, deserializes
	// unsafe formats, writes raw HTML or turns off TLS checks. On unless
	// false.
	Content *bool `yaml:"content,omitempty"`
}

// On says whether the security pass runs.
func (s Security) On() bool { return s.Enabled == nil || *s.Enabled }

// ContentOn says whether added code can start a pass.
func (s Security) ContentOn() bool { return s.Content == nil || *s.Content }

// SecurityMatch says which glob a changed file matches that starts a
// pass, if any: one of security.paths when set, or else of the sensitive
// list, as "crypto: *crypt*"; or one of security.extra_paths.
func (c *Config) SecurityMatch(file string) (string, bool) {
	s := c.Security
	if s.Paths != nil {
		if g, ok := pathglob.First(s.Paths, file); ok {
			return g, true
		}
	} else if g, ok := c.SensitiveMatch(file); ok {
		return g, true
	}
	return pathglob.First(s.ExtraPaths, file)
}

// reviewRoutes are the routing entries whose model reviews a change.
var reviewRoutes = []string{"review", "review.medium_risk", "review.high_risk"}

// SecurityRoute is the route the security pass runs on for an item built
// on the routes given (its implement jobs' harnesses and models): the
// first of routing.security, the critic's route and the implement routes,
// strongest first, that is neither a reviewer's model nor one that built
// the item; routing.security set on a reviewer's model is still used (see
// Warnings). So by default it is another vendor's model than the
// reviewer's, and never the model whose code it reads. With every one
// ruled out, it is the first that isn't a reviewer's.
func (c *Config) SecurityRoute(built ...Route) Route {
	var cands []Route
	if r, ok := c.Routing["security"]; ok {
		cands = append(cands, r)
	}
	cands = append(cands, c.CriticRoute())
	for _, size := range []string{"L", "M", "S", "XS"} {
		if r, ok := c.Routing["implement."+size]; ok {
			r.Skills, r.Use, r.Instructions, r.InstructionsFile, r.Fast = "", nil, "", "", false
			cands = append(cands, r)
		}
	}
	reviewers := c.reviewers()
	first := -1
	_, set := c.Routing["security"]
	for i, r := range cands {
		if sameAsAny(r, reviewers) && (!set || i > 0) {
			continue
		}
		if first < 0 {
			first = i
		}
		if !sameAsAny(r, built) {
			return r
		}
	}
	if first < 0 {
		return cands[0]
	}
	return cands[first]
}

// sameAsAny says whether r may be the same harness and model as one of
// routes.
func sameAsAny(r Route, routes []Route) bool {
	for _, o := range routes {
		if o.Harness == r.Harness && sameModel(r.Harness, o.Model, r.Model) {
			return true
		}
	}
	return false
}

func (c *Config) checkSecurity() []error {
	var errs []error
	for _, g := range append(append([]string{}, c.Security.Paths...), c.Security.ExtraPaths...) {
		if err := pathglob.Valid(g); err != nil {
			errs = append(errs, fmt.Errorf("security: glob %q: %v", g, err))
		}
	}
	// Left out, what it falls back to: never a reviewer's.
	if _, set := c.Routing["security"]; !set {
		if r := c.SecurityRoute(); sameAsAny(r, c.reviewers()) {
			errs = append(errs, fmt.Errorf("the security pass would run on %s/%s, a reviewer's model: set routing.security", r.Harness, r.Model))
		}
	}
	return errs
}

// Warnings are what loads but is worth a second look: routing.security on
// a reviewer's model. Each pass is a job of its own in a fresh session, so
// it still runs, but it shares that reviewer's blind spots.
func (c *Config) Warnings() []string {
	r, ok := c.Routing["security"]
	if !ok {
		return nil
	}
	for _, rk := range reviewRoutes {
		if rv, ok := c.Routing[rk]; ok && rv.Harness == r.Harness && sameModel(r.Harness, rv.Model, r.Model) {
			return []string{fmt.Sprintf("routing.security runs on %s/%s, as routing.%s does: each runs as a job of its own, but a second model would catch more of what the reviewer misses", r.Harness, r.Model, rk)}
		}
	}
	return nil
}

func (c *Config) reviewers() []Route {
	var out []Route
	for _, k := range reviewRoutes {
		if r, ok := c.Routing[k]; ok {
			out = append(out, r)
		}
	}
	return out
}
