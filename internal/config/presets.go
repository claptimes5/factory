package config

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// A preset is how hard the factory works: which model each step runs on,
// how strictly review checks itself, and how far a failing build climbs.
// It's a set of changes to factory.yaml, made for the harness that builds
// and the one that reviews (the same one when there's only one), and
// applied like any edit on the Settings screen. Everything else is left as
// it is.

// Preset is one rung of the ladder.
type Preset struct {
	Key, Name, Summary string
}

// Presets are the ladder, least work first.
var Presets = []Preset{
	{"lean", "Lean", "Small models for most builds, one review round, findings not double-checked: the least usage per change."},
	{"balanced", "Balanced", "A small model for small builds, the big one for large builds and review, findings double-checked, two review rounds."},
	{"thorough", "Thorough", "Bigger models for most builds, the reviewer at high effort, high-risk changes reviewed in pairs, three review rounds."},
	{"maximum", "Maximum", "The biggest model at high effort for every build and review."},
}

// tier is a harness's small, middle or big model.
type tier int

const (
	small tier = iota
	mid
	big
)

// models are a harness's models by tier, smallest first.
type models [3]string

// Models are the harness's small, middle and big models. Claude's are its
// aliases. Codex's are the models the config prices or routes to it,
// cheapest first by output price: names differ by account, so they come
// from the config, not from here.
func (c *Config) models(harness string) models {
	if harness == "claude" {
		return models{"haiku", "sonnet", "opus"}
	}
	var ms []string
	add := func(m string) {
		if m != "" && !claudeAlias(m) && !slices.Contains(ms, m) {
			ms = append(ms, m)
		}
	}
	for _, r := range c.Routing {
		if r.Harness == harness {
			add(r.Model)
		}
	}
	for m := range c.Pricing {
		add(m)
	}
	if len(ms) == 0 {
		ms = []string{"gpt-6-luna", "gpt-6.1-sol"} // the docs' placeholders
	}
	sort.SliceStable(ms, func(i, j int) bool {
		pi, pj := c.Pricing[ms[i]].OutputPerMTok, c.Pricing[ms[j]].OutputPerMTok
		if pi != pj {
			return pi < pj
		}
		return ms[i] < ms[j]
	})
	return models{ms[0], ms[len(ms)/2], ms[len(ms)-1]}
}

// claudeAlias says whether m names a Claude model.
func claudeAlias(m string) bool {
	m = strings.ToLower(m)
	for _, a := range []string{"haiku", "sonnet", "opus", "fable", "claude"} {
		if strings.Contains(m, a) {
			return true
		}
	}
	return false
}

// pick is the route for harness h's model at tier t. With fewer models
// than tiers, effort tells them apart: the middle tier runs at medium,
// the big one at high.
func (c *Config) pick(h string, t tier, effort string) Route {
	ms := c.models(h)
	if effort == "" && ms[mid] == ms[big] && t != small {
		effort = map[tier]string{mid: "medium", big: "high"}[t]
	}
	return Route{Harness: h, Model: ms[t], Effort: effort}
}

// PresetChanges are the changes that make preset key true, with builder
// building and reviewer reviewing.
func (c *Config) PresetChanges(key, builder, reviewer string) ([]Change, error) {
	if !slices.ContainsFunc(Presets, func(p Preset) bool { return p.Key == key }) {
		return nil, fmt.Errorf("no preset %q", key)
	}
	one := builder == reviewer
	lvl := map[string]int{"lean": 0, "balanced": 1, "thorough": 2, "maximum": 3}[key]
	// The builds, by size: lean to maximum.
	build := map[string][4]tier{
		"triage":       {small, small, small, mid},
		"implement.XS": {small, small, small, mid},
		"implement.S":  {small, small, mid, big},
		"implement.M":  {mid, mid, big, big},
		"implement.L":  {mid, big, big, big},
		"orchestrate":  {mid, big, big, big},
	}
	// With one harness the reviewer is its biggest model, so a large
	// build stays a tier below it until the top rungs.
	if one {
		l := build["implement.L"]
		l[1] = mid
		build["implement.L"] = l
	}
	effort := func(route string) string {
		switch {
		case lvl == 3 && route != "triage":
			return "high"
		case lvl == 2 && (route == "implement.M" || route == "implement.L"):
			return "high"
		}
		return ""
	}
	routes := map[string]Route{}
	for route, ts := range build {
		routes[route] = c.pick(builder, ts[lvl], effort(route))
	}
	revEffort := map[int]string{0: "", 1: "", 2: "high", 3: "high"}[lvl]
	routes["review"] = c.pick(reviewer, [4]tier{mid, big, big, big}[lvl], revEffort)
	routes["review.high_risk"] = c.pick(reviewer, big, "high")
	// The verifier checks a finding, not a build: a small model will do,
	// but with one harness it's the reviewer's, at low effort, so it's no
	// weaker than what it checks.
	if one {
		routes["verify"] = c.pick(reviewer, big, "low")
	} else {
		routes["verify"] = c.pick(reviewer, [4]tier{small, small, mid, mid}[lvl], "")
	}
	var out []Change
	set := func(path string, v any) { out = append(out, Change{Path: strings.Split(path, "."), Value: v}) }
	keys := make([]string, 0, len(routes))
	for k := range routes {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		r := routes[k]
		p := []string{"routing", k}
		out = append(out, Change{Path: append(p, "harness"), Value: r.Harness}, Change{Path: append(slices.Clone(p), "model"), Value: r.Model})
		var e any
		if r.Effort != "" {
			e = r.Effort
		}
		out = append(out, Change{Path: append(slices.Clone(p), "effort"), Value: e})
	}
	// The security pass never runs on a reviewer's model: with two
	// harnesses it's the builder's biggest; with one, the biggest model
	// the reviewer doesn't use.
	sec := c.pick(builder, big, "high")
	if one {
		for _, t := range []tier{mid, small} {
			if s := c.pick(builder, t, "high"); s.Model != routes["review"].Model && s.Model != routes["review.high_risk"].Model {
				sec = s
				break
			}
		}
	}
	out = append(out, Change{Path: []string{"routing", "security", "harness"}, Value: sec.Harness},
		Change{Path: []string{"routing", "security", "model"}, Value: sec.Model},
		Change{Path: []string{"routing", "security", "effort"}, Value: sec.Effort})
	// A failing build climbs the builder's models it doesn't start on.
	var tiers []string
	for _, t := range []tier{small, mid, big} {
		if m := c.models(builder)[t]; (lvl < 2 || t != small) && (lvl < 3 || t == big) && !slices.Contains(tiers, m) {
			tiers = append(tiers, m)
		}
	}
	set("escalation.implement", tiers)
	set("escalation.max_review_rounds", [4]int{1, 2, 3, 3}[lvl])
	set("review.verify", lvl > 0)
	set("review.paired_high_risk", lvl > 1)
	return out, nil
}

// Harnesses are the harness that builds (implement.M's) and the one that
// reviews (review's), as the config routes them.
func (c *Config) Harnesses() (builder, reviewer string) {
	builder, reviewer = c.Routing["implement.M"].Harness, c.Routing["review"].Harness
	if builder == "" {
		builder = reviewer
	}
	if reviewer == "" {
		reviewer = builder
	}
	return builder, reviewer
}

// PresetNow is the preset src, a factory.yaml, matches as it stands, or ""
// when its settings were changed by hand.
func PresetNow(src []byte) (string, error) {
	c, err := Parse(src)
	if err != nil {
		return "", err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return "", err
	}
	root := doc.Content[0]
	b, r := c.Harnesses()
	for _, p := range Presets {
		changes, err := c.PresetChanges(p.Key, b, r)
		if err != nil {
			return "", err
		}
		if slices.IndexFunc(changes, func(ch Change) bool { return !holds(root, ch) }) < 0 {
			return p.Key, nil
		}
	}
	return "", nil
}

// holds says whether the file already says what change would.
func holds(root *yaml.Node, ch Change) bool {
	n := lookup(root, ch.Path)
	if ch.Value == nil {
		return n == nil
	}
	if n == nil {
		// A bool a preset sets to its default may be left out.
		return ch.Value == (map[string]any{"review.verify": true, "review.paired_high_risk": false, "escalation.max_review_rounds": 2})[strings.Join(ch.Path, ".")]
	}
	switch v := ch.Value.(type) {
	case []string:
		var got []string
		return n.Decode(&got) == nil && slices.Equal(got, v)
	case bool:
		var got bool
		return n.Decode(&got) == nil && got == v
	case int:
		var got int
		return n.Decode(&got) == nil && got == v
	}
	return n.Value == fmt.Sprint(ch.Value)
}
