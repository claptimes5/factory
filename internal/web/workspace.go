package web

import (
	"net/http"
	"slices"
	"strings"

	"github.com/claptimes5/factory/internal/config"
)

// Workspace is one of the factory's workspaces, as the dashboard scopes
// its screens: a project and its repos (see config.Workspace).
type Workspace struct {
	Name  string
	Repos []string
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
