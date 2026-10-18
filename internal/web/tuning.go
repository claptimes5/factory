package web

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/claptimes5/factory/internal/config"
	"github.com/claptimes5/factory/internal/store"
)

// The tuning loop's proposals show on the Settings screen. Applying one
// goes through the same save as the form: it re-applies the proposed
// changes to the file as it is now, rather than the stored diff, so edits
// made since are kept. For the same reason the diff shown is worked out
// afresh against the file on every read.

// APITuningChange is one proposed setting, with its evidence; Reason says
// why the daemon dropped it.
type APITuningChange struct {
	Path   []string `json:"path"`
	Value  any      `json:"value"`
	Why    string   `json:"why"`
	Reason string   `json:"reason"`
	// Now says why a kept change wouldn't apply to the file as it is now
	// (already made, say), or "" when it would.
	Now string `json:"now"`
}

// APITuningProposal is one proposal from the tuning loop.
type APITuningProposal struct {
	ID         int64             `json:"id"`
	Status     string            `json:"status"`
	CreatedAt  string            `json:"created_at"`
	PeriodFrom string            `json:"period_from"`
	PeriodTo   string            `json:"period_to"`
	Summary    string            `json:"summary"`
	Changes    []APITuningChange `json:"changes"`
	Dropped    []APITuningChange `json:"dropped"`
	Ideas      []string          `json:"ideas"`
	Diff       string            `json:"diff"`
	PRURL      string            `json:"pr_url"`
}

// APITuning is the proposal waiting for a human, if any.
type APITuning struct {
	Proposal *APITuningProposal `json:"proposal"`
}

func tuningChanges(cs []store.ProposedChange) []APITuningChange {
	out := make([]APITuningChange, len(cs))
	for i, c := range cs {
		out[i] = APITuningChange{Path: c.Path, Value: c.Value, Why: c.Why, Reason: c.Reason}
	}
	return out
}

func (s *Server) apiTuning(w http.ResponseWriter, r *http.Request) {
	p, err := s.Store.PendingProposal(r.Context())
	if err != nil {
		apiErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := APITuning{}
	if p != nil {
		ideas := p.Ideas
		if ideas == nil {
			ideas = []string{}
		}
		ap := &APITuningProposal{ID: p.ID, Status: p.Status, CreatedAt: p.CreatedAt, PeriodFrom: p.PeriodFrom, PeriodTo: p.PeriodTo,
			Summary: p.Summary, Changes: tuningChanges(p.Changes), Dropped: tuningChanges(p.Dropped), Ideas: ideas, Diff: p.Diff, PRURL: p.PRURL}
		if src, err := os.ReadFile(s.ConfigPath); err == nil && s.ConfigPath != "" {
			dir, _ := filepath.Abs(filepath.Dir(s.ConfigPath))
			edited, why := config.Proposed(src, dir, proposalChanges(p.Changes))
			for i := range ap.Changes {
				ap.Changes[i].Now = why[i]
			}
			ap.Diff = config.UnifiedDiff(filepath.Base(s.ConfigPath), src, edited)
		}
		out.Proposal = ap
	}
	writeJSON(w, http.StatusOK, out)
}

func proposalChanges(cs []store.ProposedChange) []config.Change {
	out := make([]config.Change, len(cs))
	for i, c := range cs {
		out[i] = config.Change{Path: c.Path, Value: c.Value}
	}
	return out
}

// pendingProposal reads the proposal a request names, refusing one a
// human or a newer proposal already settled.
func (s *Server) pendingProposal(w http.ResponseWriter, r *http.Request) (*store.Proposal, bool) {
	id, ok := pathID(w, r)
	if !ok || !jsonOnly(w, r) {
		return nil, false
	}
	if ok, why := s.settingsWritable(r); !ok {
		apiErr(w, http.StatusForbidden, why)
		return nil, false
	}
	p, err := s.Store.GetProposal(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		apiErr(w, http.StatusNotFound, "no such proposal")
		return nil, false
	}
	if err != nil {
		apiErr(w, http.StatusInternalServerError, err.Error())
		return nil, false
	}
	if p.Status != store.ProposalPending {
		apiErr(w, http.StatusConflict, "this proposal was already "+p.Status)
		return nil, false
	}
	return p, true
}

func (s *Server) apiApplyTuning(w http.ResponseWriter, r *http.Request) {
	if s.ConfigPath == "" {
		apiErr(w, http.StatusNotFound, "this control room has no config file to edit")
		return
	}
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	p, ok := s.pendingProposal(w, r)
	if !ok {
		return
	}
	var in struct {
		Version string `json:"version"`
		// Changes are the indexes of the changes to apply; all when left out.
		Changes *[]int `json:"changes"`
	}
	if err := s.readBody(r, &in); err != nil {
		apiErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(p.Changes) == 0 {
		apiErr(w, http.StatusBadRequest, "this proposal changes no settings; dismiss it instead")
		return
	}
	pick := make([]int, len(p.Changes))
	for i := range pick {
		pick[i] = i
	}
	if in.Changes != nil {
		pick = *in.Changes
	}
	if len(pick) == 0 {
		apiErr(w, http.StatusBadRequest, "pick at least one change to apply")
		return
	}
	src, err := os.ReadFile(s.ConfigPath)
	if err != nil {
		apiErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	dir, _ := filepath.Abs(filepath.Dir(s.ConfigPath))
	_, why := config.Proposed(src, dir, proposalChanges(p.Changes))
	var changes []config.Change
	seen := map[int]bool{}
	for _, i := range pick {
		if i < 0 || i >= len(p.Changes) || seen[i] {
			apiErr(w, http.StatusBadRequest, fmt.Sprintf("no change %d in this proposal", i))
			return
		}
		seen[i] = true
		c := p.Changes[i]
		if !config.Tunable(c.Path) {
			apiErr(w, http.StatusBadRequest, strings.Join(c.Path, ".")+" isn't a setting tuning may change")
			return
		}
		if why[i] == config.NoChange {
			continue // already made: nothing to write
		}
		changes = append(changes, config.Change{Path: c.Path, Value: c.Value})
	}
	if len(changes) == 0 {
		apiErr(w, http.StatusBadRequest, "factory.yaml already makes the changes you picked")
		return
	}
	res, code, err := s.saveChanges(r, changes, in.Version)
	if err != nil {
		apiErr(w, code, err.Error())
		return
	}
	// The file is written: a newer proposal that superseded this one
	// meanwhile doesn't make the save a failure.
	if err := s.Store.DecideProposal(r.Context(), p.ID, store.ProposalApplied, "human"); err != nil && !errors.Is(err, store.ErrNotPending) {
		apiErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) apiDismissTuning(w http.ResponseWriter, r *http.Request) {
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	p, ok := s.pendingProposal(w, r)
	if !ok {
		return
	}
	s.act(w, s.Store.DecideProposal(r.Context(), p.ID, store.ProposalDismissed, "human"), "Dismissed. The next tuning run proposes afresh.")
}
