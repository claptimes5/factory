package config

import (
	"cmp"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Workspace is a project: the repos that belong together, and what the
// factory scopes its work to. The dashboard shows one at a time, each has
// its own orchestrator, and trust and an optional budget are its own.
// Every repo is in exactly one; a repo listed in none has one of its own
// (see WorkspaceOf). See docs/plans/workspaces.md.
type Workspace struct {
	Repos []string `yaml:"repos"`
	// ChangeTogether makes an issue in any of its repos one change across
	// all of them: one branch, a PR in each, one review. Repos are then
	// listed in the order their PRs merge.
	ChangeTogether bool `yaml:"change_together,omitempty"`
	// DailyTotal is dollars a day for its builds and reviews, under
	// budgets.daily_total; 0 for no cap of its own.
	DailyTotal float64 `yaml:"daily_total,omitempty"`
	// Orchestrator overrides the top-level orchestrator's settings for
	// this workspace's orchestrator.
	Orchestrator *WorkspaceOrchestrator `yaml:"orchestrator,omitempty"`
	// AutoMerge and SelfStart are its trust level, for every repo in it.
	// A repo's own may only be stricter.
	AutoMerge *AutoMerge `yaml:"auto_merge,omitempty"`
	SelfStart *SelfStart `yaml:"self_start,omitempty"`
}

// WorkspaceOrchestrator is what a workspace changes about its orchestrator;
// unset fields take the top-level orchestrator's.
type WorkspaceOrchestrator struct {
	On            *bool    `yaml:"enabled,omitempty"`
	Interval      Duration `yaml:"interval,omitempty"`
	DailyBudget   float64  `yaml:"daily_budget,omitempty"`
	DailyIssueCap int      `yaml:"daily_issue_cap,omitempty"`
}

// UnmarshalYAML also reads the older form, a list of repos that change
// together: `shop: [you/api, you/web]`.
func (w *Workspace) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.SequenceNode {
		var repos []string
		if err := n.Decode(&repos); err != nil {
			return err
		}
		*w = Workspace{Repos: repos, ChangeTogether: true}
		return nil
	}
	type plain Workspace
	return n.Decode((*plain)(w))
}

// MarshalYAML writes the older form back as it was, when that's all it says.
func (w Workspace) MarshalYAML() (any, error) {
	if w.ChangeTogether && w.DailyTotal == 0 && w.Orchestrator == nil && w.AutoMerge == nil && w.SelfStart == nil {
		return w.Repos, nil
	}
	type plain Workspace
	return plain(w), nil
}

// WorkspaceNames are every workspace, those factory.yaml names by name,
// then each repo's own in the order of repos.
func (c *Config) WorkspaceNames() []string {
	ix := c.workspaces()
	out := sortedKeys(c.Workspaces)
	for _, r := range c.Repos {
		if n := ix[strings.ToLower(r.Name)]; !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	return out
}

// WorkspaceOf is the workspace repo is in: the one factory.yaml puts it
// in, else its own, named for it. "" for a repo the config doesn't have.
func (c *Config) WorkspaceOf(repo string) string { return c.workspaces()[strings.ToLower(repo)] }

// WorkspaceRepos are a workspace's repos as configured, in its order: for
// one whose repos change together, the order their PRs merge in.
func (c *Config) WorkspaceRepos(name string) []string {
	if w, ok := c.Workspaces[name]; ok {
		out := make([]string, 0, len(w.Repos))
		for _, x := range w.Repos {
			if r, ok := c.RepoConf(x); ok {
				x = r.Name
			}
			out = append(out, x)
		}
		return out
	}
	var out []string
	for _, r := range c.Repos {
		if c.WorkspaceOf(r.Name) == name {
			out = append(out, r.Name)
		}
	}
	return out
}

// WorkspaceConf is a workspace's settings; a repo's own workspace has
// nothing set but its repo.
func (c *Config) WorkspaceConf(name string) (Workspace, bool) {
	if w, ok := c.Workspaces[name]; ok {
		return w, true
	}
	repos := c.WorkspaceRepos(name)
	return Workspace{Repos: repos}, len(repos) > 0
}

// ChangeTogether is the workspace repo is in and its repos, in merge order,
// when they change as one; "" and nil otherwise.
func (c *Config) ChangeTogether(repo string) (string, []string) {
	name := c.WorkspaceOf(repo)
	if w, ok := c.Workspaces[name]; ok && w.ChangeTogether {
		return name, c.WorkspaceRepos(name)
	}
	return "", nil
}

// workspaces maps each repo, lower-cased, to its workspace's name.
func (c *Config) workspaces() map[string]string {
	ix := map[string]string{}
	for _, name := range sortedKeys(c.Workspaces) {
		for _, m := range c.Workspaces[name].Repos {
			if _, ok := ix[strings.ToLower(m)]; !ok {
				ix[strings.ToLower(m)] = name
			}
		}
	}
	taken := map[string]bool{}
	for n := range c.Workspaces {
		taken[n] = true
	}
	// A repo in none is its own, named for it, and kept off any name in use.
	for _, r := range c.Repos {
		key := strings.ToLower(r.Name)
		if _, ok := ix[key]; ok {
			continue
		}
		base := ownName(r.Name)
		n := base
		for i := 2; taken[n]; i++ {
			n = fmt.Sprintf("%s-%d", base, i)
		}
		taken[n], ix[key] = true, n
	}
	return ix
}

var notName = regexp.MustCompile(`[^a-z0-9-]+`)

// ownName is the name of a repo's own workspace: its short name, as a
// workspace name may be.
func ownName(repo string) string {
	n := strings.Trim(notName.ReplaceAllString(ShortName(repo), "-"), "-")
	if n == "" || n[0] < 'a' || n[0] > 'z' {
		n = "repo-" + n
	}
	if len(n) > 56 {
		n = n[:56]
	}
	return strings.TrimRight(n, "-")
}

// checkWorkspaces reports what's wrong with the workspaces, given the
// repos configured (lower-cased).
func (c *Config) checkWorkspaces(seen map[string]bool, bad func(string, ...any), checkAutoMerge func(string, *AutoMerge)) {
	inWS := map[string]string{}
	for _, name := range sortedKeys(c.Workspaces) {
		w := c.Workspaces[name]
		if !wsName.MatchString(name) {
			bad("workspace %q: names are lowercase letters, digits and -", name)
		}
		switch {
		case len(w.Repos) == 0:
			bad("workspace %s needs at least one repo", name)
		case w.ChangeTogether && len(w.Repos) < 2:
			bad("workspace %s: change_together needs at least two repos", name)
		}
		short := map[string]bool{}
		for _, m := range w.Repos {
			key := strings.ToLower(m)
			switch {
			case !seen[key]:
				bad("workspace %s: %s isn't in repos", name, m)
			case inWS[key] != "":
				bad("workspace %s: %s is already in workspace %s", name, m, inWS[key])
			}
			inWS[key] = name
			if !w.ChangeTogether {
				continue
			}
			// Each repo of one change is checked out in a folder named for it.
			if sn := ShortName(m); short[sn] {
				bad("workspace %s: two repos are named %s; repos that change together need different names", name, sn)
			} else {
				short[sn] = true
			}
		}
		if w.DailyTotal < 0 {
			bad("workspace %s: daily_total is dollars, or 0 for none of its own", name)
		}
		if o := w.Orchestrator; o != nil && (o.DailyBudget < 0 || o.DailyIssueCap < 0 || o.Interval.Duration < 0) {
			bad("workspace %s: orchestrator budgets, caps and interval can't be negative", name)
		}
		checkAutoMerge("workspace "+name+": ", w.AutoMerge)
		if err := w.SelfStart.check("workspaces." + name); err != nil {
			bad("%v", err)
		}
	}
}

// Orchestration is a workspace's orchestrator: its own settings, with the
// top level's for what it leaves out.
type Orchestration struct {
	On            bool
	Interval      time.Duration
	RunBudget     float64 // dollars per run
	DailyBudget   float64 // dollars a day for it and the workspace's chats
	DailyIssueCap int     // issues it and the chats may open a day
}

// OrchestratorFor is workspace ws's orchestrator.
func (c *Config) OrchestratorFor(ws string) Orchestration {
	t := c.Orchestrator
	o := Orchestration{On: t.On && !c.noOrchestrator, Interval: t.Interval.Duration, RunBudget: t.RunBudget, DailyBudget: t.DailyBudget, DailyIssueCap: t.DailyIssueCap}
	w, _ := c.WorkspaceConf(ws)
	if x := w.Orchestrator; x != nil {
		if x.On != nil {
			o.On = *x.On && !c.noOrchestrator
		}
		if x.Interval.Duration > 0 {
			o.Interval = x.Interval.Duration
		}
		if x.DailyBudget > 0 {
			o.DailyBudget = min(x.DailyBudget, t.DailyBudget)
		}
		if x.DailyIssueCap > 0 {
			o.DailyIssueCap = min(x.DailyIssueCap, t.DailyIssueCap)
		}
	}
	return o
}

// AnyOrchestrator says whether some workspace's orchestrator is on.
func (c *Config) AnyOrchestrator() bool {
	return slices.ContainsFunc(c.WorkspaceNames(), func(ws string) bool { return c.OrchestratorFor(ws).On })
}

// DisableOrchestrator turns every workspace's orchestrator off, as on a
// host that can't run it.
func (c *Config) DisableOrchestrator() { c.noOrchestrator = true }

// trustOf is a repo's auto_merge, given its own, its workspace's and the
// top level's: the workspace's level, or the repo's where that's
// stricter; the merge method and breaker as the most specific one sets
// them.
func trustOf(repo, ws, top *AutoMerge) *AutoMerge {
	switch {
	case ws == nil && repo == nil:
		return top
	case ws == nil:
		return repo
	}
	out := *ws
	if repo != nil {
		if stricter(repo, ws) {
			out.UpTo, out.MaxSize = repo.UpTo, repo.MaxSize
		}
		out.Method, out.Breaker = cmp.Or(repo.Method, out.Method), cmp.Or(repo.Breaker, out.Breaker)
	}
	if top != nil {
		out.Method, out.Breaker = cmp.Or(out.Method, top.Method), cmp.Or(out.Breaker, top.Breaker)
	}
	return &out
}

// stricter says whether a merges less on its own than b.
func stricter(a, b *AutoMerge) bool {
	risks, sizes := []string{"none", "low", "medium"}, []string{"XS", "S", "M", "L"}
	size := func(x *AutoMerge) int { return slices.Index(sizes, cmp.Or(x.MaxSize, "S")) }
	ra, rb := slices.Index(risks, a.UpTo), slices.Index(risks, b.UpTo)
	if ra != rb {
		return ra < rb
	}
	return ra > 0 && size(a) < size(b)
}

// selfStartOf is a repo's self_start, given its own and its workspace's:
// with both, only the sources both allow, within the lower caps.
func selfStartOf(repo, ws *SelfStart) *SelfStart {
	switch {
	case ws == nil:
		return repo
	case repo == nil:
		return ws
	}
	out := SelfStart{Daily: min(repo.DailyCap(), ws.DailyCap())}
	for _, s := range repo.Sources {
		if slices.Contains(ws.Sources, s) {
			out.Sources = append(out.Sources, s)
		}
	}
	switch {
	case repo.Budget != nil && ws.Budget != nil:
		b := min(*repo.Budget, *ws.Budget)
		out.Budget = &b
	default:
		out.Budget = cmp.Or(repo.Budget, ws.Budget)
	}
	return &out
}
