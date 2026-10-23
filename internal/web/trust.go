package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/claptimes5/factory/internal/config"
	"github.com/claptimes5/factory/internal/store"
)

// Trust: how much the factory may do on its own, for each workspace, on a
// ladder toward full automation. Levels 1 to 5 are names for auto_merge
// (up_to and max_size); 6 adds self_start (docs/plans/trust-level-6.md);
// 7 is the road ahead, not built yet. The
// factory suggests a level up when its record at that level is clean;
// only a person raises it. The breaker steps it back on its own.
// Background: docs/plans/design-system.md, "Trust".

// TrustLevel is one rung of the ladder.
type TrustLevel struct {
	Level  int    `json:"level"`
	Name   string `json:"name"`
	Detail string `json:"detail"`
	// UpTo and MaxSize are its auto_merge; UpTo "none" merges nothing.
	UpTo    string `json:"up_to"`
	MaxSize string `json:"max_size"`
	// Available: the factory can work at this level today.
	Available bool `json:"available"`
	// Needs is what a level not built yet would take.
	Needs string `json:"needs"`
	// Evidence is how many changes in the window it would have merged on
	// its own, with none gone wrong, before the factory suggests it.
	Evidence int `json:"evidence"`
}

// TrustLevels is the ladder, lowest first.
var TrustLevels = []TrustLevel{
	{Level: 1, Name: "Ask me about every change", Detail: "Every pull request waits for you. The factory keeps score of what it would have merged, so you can see when it's ready for more.", UpTo: "none", Available: true},
	{Level: 2, Name: "Merge tiny, low-risk changes", Detail: "Copy, config and one-line fixes (size XS) merge once the checks and the reviewer pass.", UpTo: "low", MaxSize: "XS", Available: true, Evidence: 5},
	{Level: 3, Name: "Merge small, low-risk changes", Detail: "Small fixes and changes (up to size S) merge on their own.", UpTo: "low", MaxSize: "S", Available: true, Evidence: 10},
	{Level: 4, Name: "Merge low-risk changes of any normal size", Detail: "Low-risk features and fixes (up to size M) merge on their own.", UpTo: "low", MaxSize: "M", Available: true, Evidence: 20},
	{Level: 5, Name: "Merge medium-risk changes too", Detail: "Medium-risk changes merge on their own as well. You're told each time, and each can be reverted.", UpTo: "medium", MaxSize: "M", Available: true, Evidence: 30},
	{Level: 6, Name: "Pick its own work", Detail: "What the factory finds itself starts without your label and merges itself under level 5's rules: failing checks on main and Dependabot alerts to begin with, up to 3 a day and a fifth of the daily budget. The orchestrator's issues merge themselves only from here.", UpTo: "medium", MaxSize: "M", Available: true, Evidence: 10},
	{Level: 7, Name: "Ship", Detail: "It releases and deploys what it merges, with a canary and an automatic rollback. You watch, and keep the off switch.", Needs: "Not built yet: the factory doesn't deploy."},
}

// trustWindow is how far back the record and practice runs look.
const trustWindow = 30 * 24 * time.Hour

// APITrust is the trust screen.
type APITrust struct {
	Levels     []TrustLevel        `json:"levels"`
	Workspaces []APIWorkspaceTrust `json:"workspaces"`
	// AlwaysAsks are the kinds of change that wait for a person at every level.
	AlwaysAsks []string `json:"always_asks"`
	WindowDays int      `json:"window_days"`
	// CanChange: this request may raise or lower a level (see settingsWritable); Reason says why not.
	CanChange bool   `json:"can_change"`
	Reason    string `json:"reason"`
	// Pending: factory.yaml changed since the factory started; a change
	// of level applies once it restarts.
	Pending bool `json:"pending"`
}

// APIWorkspaceTrust is one workspace's trust: one level for its repos,
// and the record of all of them.
type APIWorkspaceTrust struct {
	Workspace string   `json:"workspace"`
	Repos     []string `json:"repos"`
	Level     int      `json:"level"`
	// Custom: its auto_merge is set by hand to something between levels;
	// Level is the highest it covers.
	Custom bool `json:"custom"`
	// Mixed: it has no level of its own yet and its repos are at
	// different ones (set per repo before workspaces); Level is the
	// lowest, and setting one gives them all that.
	Mixed bool `json:"mixed"`
	// Stricter are its repos set lower than it by hand in factory.yaml,
	// which they stay at.
	Stricter []APIRepoLevel `json:"stricter"`
	// SteppedBack: the breaker turned auto-merge off, so it works at
	// level 1 until it's reset; Why says what tripped it.
	SteppedBack bool   `json:"stepped_back"`
	Why         string `json:"why"`
	// The record over the window.
	Merged    int `json:"merged"`
	Automatic int `json:"automatic"`
	Reverted  int `json:"reverted"`
	BrokeMain int `json:"broke_main"`
	// Practice is what each level would have merged on its own over the
	// window, and how many of those went wrong. Level 6's is of the
	// issues the factory filed itself: how many a person took on.
	Practice []APIPractice `json:"practice"`
	// Proposed and Accepted are level 6's record: the issues the factory
	// filed from its own findings, and how many of those a person took
	// on (or, at level 6, it started).
	Proposed int `json:"proposed"`
	Accepted int `json:"accepted"`
	// Sources are what starts on its own at level 6.
	Sources []string `json:"sources"`
	// Suggest is the level the record supports, above the current one; 0
	// for none. SuggestWhy says why, or why not yet.
	Suggest    int    `json:"suggest"`
	SuggestWhy string `json:"suggest_why"`
}

// APIRepoLevel is a repo's own trust level.
type APIRepoLevel struct {
	Repo  string `json:"repo"`
	Level int    `json:"level"`
}

// APIPractice is a practice run: one level against the window's merges.
type APIPractice struct {
	Level      int `json:"level"`
	WouldMerge int `json:"would_merge"`
	WentWrong  int `json:"went_wrong"`
}

func levelOf(a *config.AutoMerge, s *config.SelfStart) (int, bool) {
	if s != nil && len(s.Sources) > 0 {
		five := TrustLevels[4]
		return 6, !a.On() || a.UpTo != five.UpTo || a.MaxSize != five.MaxSize
	}
	if !a.On() {
		return 1, false
	}
	level := 1
	for _, l := range TrustLevels {
		if l.Available && l.Level > 1 && l.Level < 6 && a.Allows(l.UpTo, l.MaxSize) {
			level = l.Level
		}
	}
	l := TrustLevels[level-1]
	ms := a.MaxSize
	if ms == "" {
		ms = "S"
	}
	return level, level == 1 || a.UpTo != l.UpTo || ms != l.MaxSize
}

func (l TrustLevel) autoMerge() *config.AutoMerge {
	return &config.AutoMerge{UpTo: l.UpTo, MaxSize: l.MaxSize}
}

func (s *Server) trust(r *http.Request) (APITrust, error) {
	ctx := r.Context()
	out := APITrust{Levels: TrustLevels, Workspaces: []APIWorkspaceTrust{}, WindowDays: int(trustWindow.Hours() / 24)}
	out.CanChange, out.Reason = s.settingsWritable(r)
	if s.SetTrust == nil {
		out.CanChange, out.Reason = false, "This control room can't change trust."
	}
	var cfg *config.Config
	if s.ConfigPath != "" {
		b, err := os.ReadFile(s.ConfigPath)
		if err != nil {
			return out, err
		}
		out.Pending = s.StartedConfig != "" && ConfigVersion(b) != s.StartedConfig
		if cfg, err = config.Load(s.ConfigPath); err != nil {
			return out, err
		}
		out.AlwaysAsks = cfg.Risk.NeedsPerson
	}
	if out.AlwaysAsks == nil {
		out.AlwaysAsks = []string{}
	}
	var breakers []APIBreaker
	if s.Breakers != nil {
		breakers = s.Breakers(ctx)
	}
	since := time.Now().Add(-trustWindow).UTC().Format(store.TimeFormat)
	for _, w := range s.workspaces() {
		t := APIWorkspaceTrust{Workspace: w.Name, Repos: w.Repos, Level: 1, Stricter: []APIRepoLevel{}, Practice: []APIPractice{}, Sources: []string{}}
		if cfg != nil {
			workspaceLevel(cfg, w, &t)
		}
		for _, repo := range w.Repos {
			if i := slices.IndexFunc(breakers, func(b APIBreaker) bool { return strings.EqualFold(b.Repo, repo) }); i >= 0 && breakers[i].Tripped && !t.SteppedBack {
				t.SteppedBack, t.Why = true, breakers[i].Reason
			}
		}
		if err := s.practice(ctx, &t, since); err != nil {
			return out, err
		}
		out.Workspaces = append(out.Workspaces, t)
	}
	return out, nil
}

// workspaceLevel fills a workspace's level from factory.yaml: its own,
// with the repos set lower than it; or, with none of its own, the lowest
// of its repos'.
func workspaceLevel(cfg *config.Config, w Workspace, t *APIWorkspaceTrust) {
	own, _ := cfg.WorkspaceConf(w.Name)
	if own.AutoMerge != nil || own.SelfStart != nil {
		am := own.AutoMerge
		if am == nil {
			am = cfg.AutoMerge
		}
		t.Level, t.Custom = levelOf(am, own.SelfStart)
		if own.SelfStart != nil {
			t.Sources = own.SelfStart.Sources
		}
		for _, repo := range w.Repos {
			if rc, ok := cfg.RepoConf(repo); ok {
				if l, _ := levelOf(rc.AutoMerge, rc.SelfStart); l < t.Level {
					t.Stricter = append(t.Stricter, APIRepoLevel{Repo: repo, Level: l})
				}
			}
		}
		return
	}
	// Its repos merge at the lowest of their levels (see config's
	// resolve); mixed is whether the levels set on them differ.
	set := map[int]bool{}
	for i, repo := range w.Repos {
		rc, ok := cfg.RepoConf(repo)
		if !ok {
			continue
		}
		if i == 0 {
			t.Level, t.Custom = levelOf(rc.AutoMerge, rc.SelfStart)
			if rc.SelfStart != nil {
				t.Sources = rc.SelfStart.Sources
			}
		}
		for _, r := range cfg.Repos {
			if strings.EqualFold(r.Name, rc.Name) {
				am := r.AutoMerge
				if am == nil || am.UpTo == "" {
					am = cfg.AutoMerge
				}
				l, _ := levelOf(am, r.SelfStart)
				set[l] = true
			}
		}
	}
	t.Mixed = len(set) > 1
}

// proposals is level 6's practice run: the issues the factory filed from
// its own findings in the window (sources and the hunt) in the
// workspace's repos, how many became work (a person labeled it, or level
// 6 started it), and how many of the merges from those were reverted or
// broke main.
func (s *Server) proposals(ctx context.Context, t *APIWorkspaceTrust, since string, merges []store.FactoryMerge) error {
	taken := map[int64]bool{}
	for _, repo := range t.Repos {
		evs, err := s.Store.EventsFor(ctx, "source_filed", repo)
		if err != nil {
			return err
		}
		for _, e := range evs {
			if e.At < since {
				continue
			}
			var d struct {
				Issue int `json:"issue"`
			}
			if json.Unmarshal([]byte(e.Data), &d) != nil || d.Issue == 0 {
				continue
			}
			t.Proposed++
			if it, err := s.Store.ItemByIssue(ctx, repo, d.Issue); err == nil {
				t.Accepted++
				taken[it.ID] = true
			}
		}
	}
	p := APIPractice{Level: 6, WouldMerge: t.Accepted}
	for _, m := range merges {
		if taken[m.ItemID] && (m.RevertedAt != "" || m.RedAt != "") {
			p.WentWrong++
		}
	}
	t.Practice = append(t.Practice, p)
	return nil
}

// practice fills a workspace's record and practice runs from its repos'
// merged factory PRs, and the level its record supports.
func (s *Server) practice(ctx context.Context, t *APIWorkspaceTrust, since string) error {
	var merges []store.FactoryMerge
	for _, repo := range t.Repos {
		ms, err := s.Store.FactoryMerges(ctx, repo, since)
		if err != nil {
			return err
		}
		merges = append(merges, ms...)
	}
	type change struct {
		risk, size string
		wrong      bool
	}
	var changes []change
	for _, m := range merges {
		t.Merged++
		wrong := m.RevertedAt != "" || m.RedAt != ""
		if m.Auto {
			t.Automatic++
		}
		if m.RevertedAt != "" {
			t.Reverted++
		}
		if m.RedAt != "" {
			t.BrokeMain++
		}
		c := change{wrong: wrong}
		if m.ItemID > 0 {
			if it, err := s.Store.GetItem(ctx, m.ItemID); err == nil && it != nil {
				c.risk, c.size = it.Risk, it.Complexity
			}
		}
		changes = append(changes, c)
	}
	for _, l := range TrustLevels {
		if !l.Available || l.Level == 1 || l.Level == 6 {
			continue
		}
		p := APIPractice{Level: l.Level}
		for _, c := range changes {
			if l.autoMerge().Allows(c.risk, c.size) {
				p.WouldMerge++
				if c.wrong {
					p.WentWrong++
				}
			}
		}
		t.Practice = append(t.Practice, p)
	}
	if err := s.proposals(ctx, t, since, merges); err != nil {
		return err
	}
	next := t.Level + 1
	switch {
	case t.SteppedBack:
		t.SuggestWhy = "Auto-merge is off after what went wrong. Reset it first; the record starts again from there."
	case next > len(TrustLevels) || !TrustLevels[next-1].Available:
		t.SuggestWhy = "It's at the highest level the factory can work at today."
	default:
		p := t.Practice[slices.IndexFunc(t.Practice, func(p APIPractice) bool { return p.Level == next })]
		need := TrustLevels[next-1].Evidence
		switch {
		case next == 6 && p.WentWrong > 0:
			t.SuggestWhy = fmt.Sprintf("Not yet: %d of the changes from issues it filed itself went wrong.", p.WentWrong)
		case next == 6 && (p.WouldMerge < need || p.WouldMerge*5 < t.Proposed*4):
			t.SuggestWhy = fmt.Sprintf("Not yet: of the %d issues it filed from its own findings in %d days, you took on %d. Level 6 needs %d, and 4 in 5.", t.Proposed, int(trustWindow.Hours()/24), p.WouldMerge, need)
		case next == 6:
			t.Suggest = next
			t.SuggestWhy = fmt.Sprintf("You took on %d of the %d issues it filed from its own findings in the last %d days, and none of what merged from them went wrong.", p.WouldMerge, t.Proposed, int(trustWindow.Hours()/24))
		case p.WentWrong > 0:
			t.SuggestWhy = fmt.Sprintf("Not yet: %d of the %d changes level %d would have merged went wrong.", p.WentWrong, p.WouldMerge, next)
		case p.WouldMerge < need:
			t.SuggestWhy = fmt.Sprintf("Not yet: level %d would have merged %d changes in %d days, none gone wrong. It needs %d.", next, p.WouldMerge, int(trustWindow.Hours()/24), need)
		default:
			t.Suggest = next
			t.SuggestWhy = fmt.Sprintf("Level %d would have merged %d changes on its own in the last %d days, and none was reverted or broke main.", next, p.WouldMerge, int(trustWindow.Hours()/24))
		}
	}
	return nil
}

func (s *Server) apiTrust(w http.ResponseWriter, r *http.Request) {
	t, err := s.trust(r)
	if err != nil {
		apiErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, t)
}

// apiSetTrust moves a workspace to a level: its auto_merge and
// self_start in factory.yaml, applied once the factory restarts.
func (s *Server) apiSetTrust(w http.ResponseWriter, r *http.Request) {
	if !jsonOnly(w, r) {
		return
	}
	if ok, why := s.settingsWritable(r); !ok {
		apiErr(w, http.StatusForbidden, why)
		return
	}
	if s.SetTrust == nil {
		apiErr(w, http.StatusNotFound, "this control room can't change trust")
		return
	}
	var in struct {
		Workspace string `json:"workspace"`
		Level     int    `json:"level"`
	}
	if err := s.readBody(r, &in); err != nil {
		apiErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if in.Level < 1 || in.Level > len(TrustLevels) || !TrustLevels[in.Level-1].Available {
		apiErr(w, http.StatusBadRequest, fmt.Sprintf("level %d isn't one the factory can work at", in.Level))
		return
	}
	if !s.hasWorkspace(in.Workspace) {
		apiErr(w, http.StatusBadRequest, in.Workspace+" isn't one of the factory's workspaces")
		return
	}
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	l := TrustLevels[in.Level-1]
	var self *config.SelfStart
	if l.Level == 6 {
		self = &config.SelfStart{Sources: slices.Clone(config.DefaultSelfStartSources)}
	}
	if err := s.SetTrust(r.Context(), in.Workspace, l.autoMerge(), self); err != nil {
		apiErr(w, http.StatusBadRequest, err.Error())
		return
	}
	t, err := s.trust(r)
	if err != nil {
		apiErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Message string   `json:"message"`
		Trust   APITrust `json:"trust"`
	}{fmt.Sprintf("%s is set to level %d: %s. It applies once the factory restarts.", in.Workspace, l.Level, strings.ToLower(l.Name[:1])+l.Name[1:]), t})
}
