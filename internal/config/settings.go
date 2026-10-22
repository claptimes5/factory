package config

import (
	"errors"
	"fmt"
	"math"
	"reflect"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Setting is one value the dashboard's Settings screen may change. The
// rest of factory.yaml (the runner and its egress, gates and their
// commands, repos, env, services, files, the web listener and pricing)
// can run commands on the host or widen network access, so it is edited
// in the file only.
type Setting struct {
	Path  []string
	Label string
	Help  string
	// Kind is number, integer, text, bool, select, list or duration.
	Kind    string
	Options []string // a select's choices; "" means the default
	// Row groups the settings of one routing entry on one line.
	Row string
	// Value is what the file says, nil when it leaves the key out.
	// Default is what applies then, nil when the key is required.
	Value   any
	Default any
	// Min and Max bound a number: the config accepts values, such as no
	// jobs at once, that would quietly stop the factory. Max 0 is none.
	Min, Max float64
	// MinDuration is a duration's least value; 0 means 10s. Off says a
	// duration of 0 (or none) turns the thing off, so it is allowed
	// below that.
	MinDuration time.Duration
	Off         bool
}

// SettingGroup is one card of the Settings screen.
type SettingGroup struct {
	Name     string
	Help     string
	Settings []Setting
}

// Efforts are the reasoning efforts the harnesses take: Claude's low to
// max, Codex's minimal to xhigh. Validation passes any value through to
// the CLI, so a route's own value is offered too.
var Efforts = []string{"minimal", "low", "medium", "high", "xhigh", "max"}

// Settings lists what the dashboard may change, with the values in src, a
// factory.yaml. The Models group has a row per routing entry in the file.
func Settings(src []byte) ([]SettingGroup, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return nil, err
	}
	var root *yaml.Node
	if len(doc.Content) > 0 {
		root = doc.Content[0]
	}
	num := func(path, label, help string, def any) Setting {
		return Setting{Path: strings.Split(path, "."), Label: label, Help: help, Kind: "number", Default: def, Min: 0.01}
	}
	integer := func(path, label, help string, def any) Setting {
		s := num(path, label, help, def)
		s.Kind, s.Min = "integer", 1
		return s
	}
	bounded := func(s Setting, lo, hi float64) Setting {
		s.Min, s.Max = lo, hi
		return s
	}
	groups := []SettingGroup{
		{Name: "Budgets", Help: "Dollar caps and limits the daemon enforces.", Settings: []Setting{
			num("budgets.daily_total", "Daily cap", "Total dollars a day; once it's spent, no new jobs start until midnight UTC.", nil),
			integer("budgets.concurrent_jobs", "Jobs at once", "How many jobs run at the same time.", 3),
			integer("budgets.awaiting_human_max", "PRs waiting for you", "No new builds start while this many PRs wait for you.", 5),
			num("budgets.per_job.XS", "Per XS job", "The cap for one XS build; left out, S's applies.", nil),
			num("budgets.per_job.S", "Per S job", "The cap for one S build.", nil),
			num("budgets.per_job.M", "Per M job", "The cap for one M build.", nil),
			num("budgets.per_job.L", "Per L job", "The cap for one L build.", nil),
			num("budgets.triage", "Per triage", "The cap for one triage job.", nil),
			num("budgets.review", "Per review", "The cap for one review job.", nil),
			num("budgets.verify", "Per verifier run", "The cap for one check of a review's blocking findings; left out, triage's applies.", nil),
			bounded(num("budgets.pause_at_usage", "Pause at usage (%)", "Jobs wait once a subscription's usage window is this full.", 95), 1, 100),
			bounded(num("budgets.confirm_over", "Ask before over", "Items whose cost estimate (p75) is over this many dollars wait for your OK before building; 0 is off.", 0), 0, 0),
			integer("budgets.max_turns.XS", "Turns per XS build", "The most turns (model calls) one XS build takes. A build that runs out stops, keeps its work and waits for you to continue it.", DefaultMaxTurns["XS"]),
			integer("budgets.max_turns.S", "Turns per S build", "The most turns one S build takes.", DefaultMaxTurns["S"]),
			integer("budgets.max_turns.M", "Turns per M build", "The most turns one M build takes.", DefaultMaxTurns["M"]),
			integer("budgets.max_turns.L", "Turns per L build", "The most turns one L build takes; other steps, such as triage and review, keep 80.", DefaultMaxTurns["L"]),
			bounded(num("budgets.fast_multiplier", "Fast mode multiplier", "Scales a job's cap when it runs in fast mode.", 2), 1, 0),
		}},
		{Name: "Models", Help: "Which harness and model run each step. A reviewer runs in a fresh session of its own, so it may use the builder's model; another vendor's model, the default, catches more of its blind spots.", Settings: routes(root)},
		{Name: "Review", Help: "How review checks its own findings.", Settings: []Setting{
			{Path: []string{"review", "verify"}, Label: "Verify findings", Help: "A second model (routing.verify) checks each blocking finding; only confirmed ones block.", Kind: "bool", Default: true},
			{Path: []string{"review", "paired_high_risk"}, Label: "Pair high-risk reviews", Help: "The reviewer and the critic read a high-risk change side by side, instead of one after the other.", Kind: "bool", Default: false},
			{Path: []string{"review", "incremental"}, Label: "Re-review what changed", Help: "After round one, a reviewer reads the diff since its last review, with its open findings.", Kind: "bool", Default: true},
		}},
		{Name: "Escalation", Help: "What happens when a build fails its gates or review.", Settings: []Setting{
			{Path: []string{"escalation", "implement"}, Label: "Model tiers", Help: "Models a failing build moves up through, cheapest first.", Kind: "list"},
			integer("escalation.max_attempts_per_tier", "Attempts per tier", "Tries on each model before moving up a tier.", 2),
			bounded(integer("escalation.max_review_rounds", "Review rounds", "Times the reviewer may send a build back before it's parked.", 2), 0, 0),
		}},
		{Name: "Auto-merge", Help: "Small, low-risk PRs can merge themselves once CI passes. Repos' own settings are edited in factory.yaml.", Settings: []Setting{
			{Path: []string{"auto_merge", "up_to"}, Label: "Up to risk", Help: "The highest risk that merges on its own; none waits for you.", Kind: "select", Options: []string{"", "none", "low", "medium"}, Default: "none"},
			{Path: []string{"auto_merge", "max_size"}, Label: "Largest size", Help: "The largest triage size that qualifies.", Kind: "select", Options: []string{"", "XS", "S", "M", "L"}, Default: "S"},
			{Path: []string{"auto_merge", "method"}, Label: "Method", Help: "How the PR merges.", Kind: "select", Options: []string{"", "merge", "squash", "rebase"}, Default: "merge"},
		}},
		{Name: "Orchestrator", Help: "The agent that files and orders work on its own schedule.", Settings: []Setting{
			{Path: []string{"orchestrator", "enabled"}, Label: "On", Help: "It spends on its own schedule, so it's off unless you turn it on.", Kind: "bool", Default: false},
			num("orchestrator.run_budget", "Per run", "Dollars for one orchestrator run or chat reply.", 3),
			num("orchestrator.daily_budget", "Daily cap", "Dollars a day for the orchestrator, tuning and chat, apart from building.", 15),
			integer("orchestrator.daily_issue_cap", "Issues a day", "How many issues it may open a day.", 10),
			{Path: []string{"orchestrator", "interval"}, Label: "Every", Help: "How often it runs on schedule, such as 60m.", Kind: "duration", Default: "60m"},
		}},
		{Name: "Quiet hours", Help: "When only something wrong is sent: main broken, a revert, the budget or the disk run out. The rest waits for one catch-up when they end.", Settings: []Setting{
			{Path: []string{"notify", "quiet_hours", "from"}, Label: "From", Help: "A 24-hour time such as 19:00; empty is off.", Kind: "text"},
			{Path: []string{"notify", "quiet_hours", "until"}, Label: "Until", Help: "Such as 08:00; earlier than From spans midnight.", Kind: "text"},
			{Path: []string{"notify", "quiet_hours", "weekends"}, Label: "All weekend", Help: "Saturday and Sunday are quiet all day.", Kind: "bool", Default: false},
			{Path: []string{"notify", "quiet_hours", "timezone"}, Label: "Time zone", Help: "Such as Europe/Madrid; empty is the factory machine's.", Kind: "text"},
		}},
		{Name: "Asks", Help: "How the factory writes what you read and decide on.", Settings: []Setting{
			{Path: []string{"concise_asks"}, Label: "Concise", Help: "Plans and chat replies lead with what you decide, in short numbered steps (prompts/concise.md, from the i-have-adhd skill).", Kind: "bool", Default: true},
		}},
		{Name: "Tuning", Help: "How often the factory reads its metrics and proposes changes to these settings.", Settings: []Setting{
			{Path: []string{"tuning", "every"}, Label: "Every", Help: "How often it proposes changes, such as 720h, at least 1h; 0 turns it off.", Kind: "duration", Default: "720h", MinDuration: MinTuningEvery, Off: true},
			integer("tuning.min_items", "Fewest builds", "It skips a run when fewer items were built since the last one.", 10),
		}},
		{Name: "Intake", Help: "How the factory finds labeled issues.", Settings: []Setting{
			{Path: []string{"intake", "poll_interval"}, Label: "Poll every", Help: "How often it looks for labeled issues, such as 60s.", Kind: "duration", Default: "60s"},
		}},
	}
	for gi := range groups {
		for si := range groups[gi].Settings {
			s := &groups[gi].Settings[si]
			if n := lookup(root, s.Path); n != nil {
				s.Value = s.read(n)
			}
			if s.Kind == "select" && s.Value != nil && !slices.Contains(s.Options, fmt.Sprint(s.Value)) {
				s.Options = append(s.Options, fmt.Sprint(s.Value))
			}
		}
	}
	return groups, nil
}

// routes are the Models rows: one per routing entry, in the file's order.
func routes(root *yaml.Node) []Setting {
	var out []Setting
	_, routing := findIn(root, "routing")
	if routing == nil || routing.Kind != yaml.MappingNode {
		return out
	}
	for i := 0; i+1 < len(routing.Content); i += 2 {
		k := routing.Content[i].Value
		p := func(f string) []string { return []string{"routing", k, f} }
		out = append(out,
			Setting{Path: p("harness"), Row: k, Label: "Harness", Help: "claude or codex.", Kind: "select", Options: []string{"claude", "codex"}},
			Setting{Path: p("model"), Row: k, Label: "Model", Help: "A Claude alias such as sonnet, or a model id; a Codex model needs a pricing entry.", Kind: "text"},
			Setting{Path: p("effort"), Row: k, Label: "Effort", Help: "Reasoning effort; left out, the CLI's default.", Kind: "select", Options: append([]string{""}, Efforts...), Default: ""},
			Setting{Path: p("fast"), Row: k, Label: "Fast", Help: "Quicker at a higher price per token; Claude on Opus only.", Kind: "bool", Default: false},
		)
	}
	return out
}

func findIn(m *yaml.Node, k string) (key, val *yaml.Node) {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil, nil
	}
	return find(m, k)
}

// lookup is the node at path, or nil.
func lookup(n *yaml.Node, path []string) *yaml.Node {
	for _, k := range path {
		if _, n = findIn(n, k); n == nil {
			return nil
		}
	}
	if n.Kind == yaml.ScalarNode && n.Tag == "!!null" {
		return nil
	}
	return n
}

// read is a value from the file, as its kind has it.
func (s Setting) read(n *yaml.Node) any {
	var v any
	switch s.Kind {
	case "number":
		v = new(float64)
	case "integer":
		v = new(int)
	case "bool":
		v = new(bool)
	case "list":
		v = new([]string)
	default:
		return n.Value
	}
	if err := n.Decode(v); err != nil {
		return n.Value // validation says what's wrong
	}
	return reflect.ValueOf(v).Elem().Interface()
}

// FindSetting is the setting at path, if the dashboard may change it.
func FindSetting(groups []SettingGroup, path []string) (Setting, bool) {
	for _, g := range groups {
		for _, s := range g.Settings {
			if slices.Equal(s.Path, path) {
				return s, true
			}
		}
	}
	return Setting{}, false
}

// Coerce checks that v, as JSON decodes it, fits the setting and returns
// what Edit should write: nil for an empty value, which removes the key.
func (s Setting) Coerce(v any) (any, error) {
	name := strings.Join(s.Path, ".")
	if v == nil {
		return nil, nil
	}
	switch s.Kind {
	case "number", "integer":
		f, ok := v.(float64)
		if !ok || math.IsNaN(f) || math.IsInf(f, 0) {
			return nil, fmt.Errorf("%s must be a number", name)
		}
		if s.Kind == "integer" && f != math.Trunc(f) {
			return nil, fmt.Errorf("%s must be a whole number", name)
		}
		if f < s.Min || s.Max > 0 && f > s.Max {
			if s.Max > 0 {
				return nil, fmt.Errorf("%s must be from %g to %g", name, s.Min, s.Max)
			}
			return nil, fmt.Errorf("%s must be at least %g", name, s.Min)
		}
		return f, nil
	case "bool":
		b, ok := v.(bool)
		if !ok {
			return nil, fmt.Errorf("%s must be true or false", name)
		}
		if b == s.Default {
			return nil, nil // off is the default: leave it out
		}
		return b, nil
	case "list":
		l, ok := v.([]any)
		if !ok {
			return nil, fmt.Errorf("%s must be a list", name)
		}
		var out []any
		for _, e := range l {
			str, ok := e.(string)
			if !ok {
				return nil, fmt.Errorf("%s must be a list of names", name)
			}
			if str = strings.TrimSpace(str); str != "" {
				out = append(out, str)
			}
		}
		if len(out) == 0 {
			return nil, nil
		}
		return out, nil
	}
	str, ok := v.(string)
	if !ok {
		return nil, fmt.Errorf("%s must be text", name)
	}
	str = strings.TrimSpace(str)
	switch {
	case str == "":
		return nil, nil
	case s.Kind == "select" && !slices.Contains(s.Options, str):
		return nil, fmt.Errorf("%s must be one of %s", name, strings.Join(slices.DeleteFunc(slices.Clone(s.Options), func(o string) bool { return o == "" }), ", "))
	case s.Kind == "duration" && s.Off && str == "none":
	case s.Kind == "duration":
		d, err := time.ParseDuration(str)
		if err != nil {
			return nil, fmt.Errorf("%s: %v", name, err)
		}
		least := s.MinDuration
		if least == 0 {
			least = 10 * time.Second
		}
		if d < least && (!s.Off || d != 0) {
			return nil, fmt.Errorf("%s must be at least %s", name, shortDuration(least))
		}
	case strings.ContainsAny(str, "\n\r"):
		return nil, errors.New(name + " must be one line")
	}
	return str, nil
}

// shortDuration writes a duration as factory.yaml would: 1h, not 1h0m0s.
func shortDuration(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = s[:len(s)-2]
	}
	if strings.HasSuffix(s, "h0m") {
		s = s[:len(s)-2]
	}
	return s
}
