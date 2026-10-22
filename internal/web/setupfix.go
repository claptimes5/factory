package web

import (
	"context"
	"errors"
	"net/http"
	"os"
	"slices"
	"strings"

	"github.com/claptimes5/factory/internal/config"
	"github.com/claptimes5/factory/internal/gates"
	"github.com/claptimes5/factory/internal/stack"
	"gopkg.in/yaml.v3"
)

// APISetupFix is what Fix setup on a parked item would change: the
// problem it stopped on, and the change to its repo in factory.yaml that
// the repo's own files suggest. Changes is empty when they suggest none.
type APISetupFix struct {
	Repo    string `json:"repo"`
	Problem string `json:"problem"` // what the sandbox lacks, in a sentence
	Line    string `json:"line"`    // the output line that showed it
	// Changes says each change for people; Before and After are the
	// repo's entry in factory.yaml now and with them.
	Changes []string `json:"changes"`
	Before  string   `json:"before"`
	After   string   `json:"after"`
	Notes   []string `json:"notes"`
	// Version is factory.yaml's as read; applying sends it back, so it
	// can't overwrite changes made since.
	Version  string `json:"version"`
	Writable bool   `json:"writable"`
	Reason   string `json:"reason"` // why it can't be applied here, when it can't
}

// setupFix is the proposal for item id, the config it's read from, and
// its repo's index there.
func (s *Server) setupFix(ctx context.Context, id int64) (APISetupFix, *config.Config, int, config.Repo, int, error) {
	if s.ConfigPath == "" || s.Detect == nil || s.SetupPark == nil {
		return APISetupFix{}, nil, 0, config.Repo{}, http.StatusNotFound, errors.New("this control room can't change the setup")
	}
	it, err := s.Store.GetItem(ctx, id)
	if err != nil {
		return APISetupFix{}, nil, 0, config.Repo{}, http.StatusNotFound, err
	}
	pk, ok := s.SetupPark(ctx, it)
	if !ok {
		return APISetupFix{}, nil, 0, config.Repo{}, http.StatusConflict, errors.New("it isn't parked on a setup problem")
	}
	b, err := os.ReadFile(s.ConfigPath)
	if err != nil {
		return APISetupFix{}, nil, 0, config.Repo{}, http.StatusInternalServerError, err
	}
	cfg, err := config.Load(s.ConfigPath)
	if err != nil {
		return APISetupFix{}, nil, 0, config.Repo{}, http.StatusConflict, err
	}
	i := slices.IndexFunc(cfg.Repos, func(r config.Repo) bool { return strings.EqualFold(r.Name, it.Repo) })
	if i < 0 {
		return APISetupFix{}, nil, 0, config.Repo{}, http.StatusConflict, errors.New(it.Repo + " isn't in factory.yaml any more")
	}
	sug, err := s.Detect(ctx, cfg.Repos[i].Name)
	if err != nil {
		return APISetupFix{}, nil, 0, config.Repo{}, http.StatusBadGateway, errors.New("couldn't read the repo's files: " + err.Error())
	}
	fixed, changes := stack.Fix(cfg, cfg.Repos[i], sug)
	out := APISetupFix{Repo: cfg.Repos[i].Name, Problem: (&gates.Fault{Kind: pk.Setup, Line: pk.Line}).What(), Line: pk.Line,
		Changes: changes, Notes: []string{}, Version: ConfigVersion(b)}
	if out.Changes == nil {
		out.Changes = []string{}
	}
	if sug != nil {
		out.Notes = append(out.Notes, sug.Notes...)
	}
	out.Before, out.After = repoYAML(cfg.Repos[i]), repoYAML(fixed)
	return out, cfg, i, fixed, http.StatusOK, nil
}

func repoYAML(r config.Repo) string {
	b, _ := yaml.Marshal([]config.Repo{r})
	return string(b)
}

func (s *Server) apiSetupFix(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	out, _, _, _, code, err := s.setupFix(r.Context(), id)
	if err != nil {
		apiErr(w, code, err.Error())
		return
	}
	out.Writable, out.Reason = s.settingsWritable(r)
	writeJSON(w, http.StatusOK, out)
}

// apiApplySetupFix saves the proposal to factory.yaml, asks for a restart,
// which waits for running jobs and lets no new one start, and queues the
// item's retry, which so runs on the new setup: after a rebuild of its
// job image when that changed (see `factory images`).
func (s *Server) apiApplySetupFix(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok || !jsonOnly(w, r) {
		return
	}
	if ok, why := s.settingsWritable(r); !ok {
		apiErr(w, http.StatusForbidden, why)
		return
	}
	var in struct {
		Version string `json:"version"`
	}
	if err := s.readBody(r, &in); err != nil {
		apiErr(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx := r.Context()
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	fix, cfg, i, fixed, code, err := s.setupFix(ctx, id)
	switch {
	case err != nil:
		apiErr(w, code, err.Error())
		return
	case fix.Version != in.Version:
		apiErr(w, http.StatusConflict, "factory.yaml changed since this was shown; look again")
		return
	case len(fix.Changes) == 0:
		apiErr(w, http.StatusConflict, "the repo's files suggest no change to make; add what's missing to factory.yaml yourself")
		return
	}
	repos := slices.Clone(cfg.Repos)
	repos[i] = fixed
	if err := config.WriteRepos(s.ConfigPath, repos); err != nil {
		apiErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	msg := "Saved to factory.yaml."
	if s.Restarter != nil {
		n := s.Restarter.RequestRestart(ctx)
		msg += " " + RestartMessage(n, s.Restarter.RestartWaitsFor())
	} else {
		msg += " Restart the factory to apply it."
	}
	if job, _ := s.itemChoices(ctx, id); job != nil {
		if err := s.Actions.Retry(ctx, *job, false); err != nil {
			s.act(w, errors.New(msg+" Couldn't queue its retry: "+err.Error()+"; press Retry after the restart"), "")
			return
		}
		msg += " Its retry is queued and starts after that, once its job image is built."
	}
	writeJSON(w, http.StatusOK, APIActionResult{OK: true, Message: msg})
}
