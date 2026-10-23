package web

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"

	"github.com/claptimes5/factory/internal/config"
	"github.com/claptimes5/factory/internal/store"
)

// Workspace is one of the factory's workspaces, as the dashboard scopes
// its screens: a project and its repos (see config.Workspace).
type Workspace struct {
	Name  string
	Repos []string
	// DailyCap is its own daily budget for builds and reviews, 0 for none.
	DailyCap float64
}

// APIWorkspace is a workspace for the dashboard's switcher.
type APIWorkspace struct {
	Name  string   `json:"name"`
	Repos []string `json:"repos"`
	// DailyCapUSD is its own daily budget (0 for none), and SpentTodayUSD
	// what its builds and reviews cost today.
	DailyCapUSD   float64 `json:"daily_cap_usd"`
	SpentTodayUSD float64 `json:"spent_today_usd"`
}

// eventWorkspace is the workspace an event is of: its item's, or the one
// its orchestrator or chat job ran for; "" for neither.
func (s *Server) eventWorkspace(ctx context.Context, e store.Event) string {
	if e.ItemID != nil {
		if it, err := s.Store.GetItem(ctx, *e.ItemID); err == nil {
			return s.workspaceOf(it.Repo)
		}
		return ""
	}
	if e.JobID != nil {
		if j, err := s.Store.GetJob(ctx, *e.JobID); err == nil {
			return jobWorkspace(j)
		}
	}
	return ""
}

// jobWorkspace is the workspace an orchestrator or chat job ran for, from
// its input; "" for any other job.
func jobWorkspace(j *store.Job) string {
	if j == nil || j.ItemID != nil {
		return ""
	}
	var in struct {
		Workspace string `json:"workspace"`
	}
	_ = json.Unmarshal([]byte(j.Input), &in)
	return in.Workspace
}

// workspaces are the factory's workspaces in order: Workspaces, or, when
// none were given, each repo's own, as factory.yaml with none makes them.
func (s *Server) workspaces() []Workspace {
	if s.Workspaces != nil {
		return s.Workspaces
	}
	c := &config.Config{}
	for _, r := range s.Repos {
		c.Repos = append(c.Repos, config.Repo{Name: r})
	}
	var out []Workspace
	for _, n := range c.WorkspaceNames() {
		out = append(out, Workspace{Name: n, Repos: c.WorkspaceRepos(n)})
	}
	return out
}

// workspaceOf is the workspace repo is in, or "".
func (s *Server) workspaceOf(repo string) string {
	for _, w := range s.workspaces() {
		if slices.ContainsFunc(w.Repos, func(r string) bool { return strings.EqualFold(r, repo) }) {
			return w.Name
		}
	}
	return ""
}

// hasWorkspace says whether name is one of the factory's workspaces.
func (s *Server) hasWorkspace(name string) bool {
	return slices.ContainsFunc(s.workspaces(), func(w Workspace) bool { return w.Name == name })
}

// soleWorkspace is the factory's workspace when it has only one, else "".
func (s *Server) soleWorkspace() string {
	if ws := s.workspaces(); len(ws) == 1 {
		return ws[0].Name
	}
	return ""
}

// reqWorkspace is the workspace a request asks for in ?workspace=: "" for
// every workspace, the factory's only one when it has one. An unknown one
// is answered with a 400, and ok is false.
func (s *Server) reqWorkspace(w http.ResponseWriter, r *http.Request) (string, bool) {
	ws := r.URL.Query().Get("workspace")
	if ws == "" {
		return s.soleWorkspace(), true
	}
	if !s.hasWorkspace(ws) {
		apiErr(w, http.StatusBadRequest, "no workspace "+ws)
		return "", false
	}
	return ws, true
}
